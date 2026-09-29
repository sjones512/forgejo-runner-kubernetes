package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const prefix = "fj-exec-"
const workspace = "/shared"

const (
	defaultWorkspaceSizeLimit      = "1Gi"
	defaultEphemeralStorageRequest = "256Mi"
	defaultEphemeralStorageLimit   = "2Gi"
)

// Config intentionally fixes the job image and namespace: workflow inputs cannot
// widen either trust boundary in this first milestone.
type Config struct {
	Namespace               string
	Image                   string
	Arch                    string
	StartupTimeout          time.Duration
	CleanupTimeout          time.Duration
	WorkspaceSizeLimit      string // Kubernetes quantities; empty means default.
	EphemeralStorageRequest string
	EphemeralStorageLimit   string
}

func (c Config) Validate() error {
	if c.Namespace == "" || c.Image == "" || (c.Arch != "arm64" && c.Arch != "amd64") || c.StartupTimeout <= 0 || c.CleanupTimeout <= 0 {
		return fmt.Errorf("namespace, image, arm64/amd64 architecture and positive timeouts are required")
	}
	_, _, _, err := c.storageQuantities()
	return err
}

// The disk-backed emptyDir is part of the Pod's local ephemeral-storage usage,
// not extra capacity. Leave room above its cap for writable layers and logs.
func (c Config) storageQuantities() (workspaceSize, request, limit resource.Quantity, err error) {
	parse := func(name, raw, def string) (resource.Quantity, error) {
		if raw == "" {
			raw = def
		}
		q, e := resource.ParseQuantity(raw)
		if e != nil {
			return q, fmt.Errorf("%s: invalid Kubernetes quantity %q: %w", name, raw, e)
		}
		if q.Sign() <= 0 {
			return q, fmt.Errorf("%s must be positive: %q", name, raw)
		}
		return q, nil
	}
	if workspaceSize, err = parse("JOB_WORKSPACE_SIZE_LIMIT", c.WorkspaceSizeLimit, defaultWorkspaceSizeLimit); err != nil {
		return
	}
	if request, err = parse("JOB_EPHEMERAL_STORAGE_REQUEST", c.EphemeralStorageRequest, defaultEphemeralStorageRequest); err != nil {
		return
	}
	if limit, err = parse("JOB_EPHEMERAL_STORAGE_LIMIT", c.EphemeralStorageLimit, defaultEphemeralStorageLimit); err != nil {
		return
	}
	if request.Cmp(limit) > 0 {
		err = fmt.Errorf("JOB_EPHEMERAL_STORAGE_REQUEST must not exceed JOB_EPHEMERAL_STORAGE_LIMIT")
	} else if workspaceSize.Cmp(limit) >= 0 {
		err = fmt.Errorf("JOB_WORKSPACE_SIZE_LIMIT must be less than JOB_EPHEMERAL_STORAGE_LIMIT to leave room for container layers and logs")
	}
	return
}

type Server struct {
	pb.UnimplementedBackendPluginServer
	cfg    Config
	client kubernetes.Interface
	rest   *rest.Config
	locks  sync.Map                                                                       // *sync.Mutex per deterministic pod name; no volatile env registry
	execFn func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error // test transport seam
}

func New(cfg Config, client kubernetes.Interface, rc *rest.Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil || rc == nil {
		return nil, fmt.Errorf("Kubernetes client and REST config required")
	}
	return &Server{cfg: cfg, client: client, rest: rc}, nil
}
func (s *Server) lock(id string) func() {
	v, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

var validID = regexp.MustCompile(`^fj-exec-[a-f0-9]{32}$`)

func podName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return prefix + hex.EncodeToString(sum[:16])
}
func (s *Server) checkID(id string) error {
	if !validID.MatchString(id) {
		return status.Error(codes.NotFound, "unknown environment ID")
	}
	return nil
}
func (s *Server) get(ctx context.Context, id string) (*core.Pod, error) {
	if err := s.checkID(id); err != nil {
		return nil, err
	}
	p, err := s.client.CoreV1().Pods(s.cfg.Namespace).Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "environment not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "get pod: %v", err)
	}
	if p.Labels["app.kubernetes.io/managed-by"] != "forgejo-runner-kubernetes" || p.Labels["forgejo.org/execution-id"] != id {
		return nil, status.Error(codes.NotFound, "environment not owned by plugin")
	}
	return p, nil
}
func (s *Server) Capabilities(context.Context, *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	return &pb.CapabilitiesResponse{Name: "kubernetes"}, nil
}
func (s *Server) Create(ctx context.Context, r *pb.CreateRequest) (*pb.CreateResponse, error) {
	// v13.2 sends image="" when jobs.<id>.container is absent; the label
	// suffix is the only image hint for a plain `run:` job. Reject overrides.
	if strings.TrimSpace(r.GetName()) == "" || (r.GetImage() != "" && r.GetImage() != s.cfg.Image) || (r.GetImage() == "" && r.GetLabelArg() != s.cfg.Image) || len(r.GetServices()) != 0 || len(r.GetBackendOptions()) != 0 || (r.GetLabelArg() != "" && r.GetLabelArg() != s.cfg.Image) || len(r.GetCapAdd()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "name and configured image required; services, options, alternate label suffix and cap_add unsupported")
	}
	id := podName(r.GetName())
	unlock := s.lock(id)
	defer unlock()
	obj := podSpec(id, r.GetName(), s.cfg)
	if r.GetEnvironmentTimeout() != nil && r.GetEnvironmentTimeout().AsDuration() > 0 {
		seconds := int64((r.GetEnvironmentTimeout().AsDuration() + time.Second - 1) / time.Second)
		obj.Spec.ActiveDeadlineSeconds = &seconds
	}
	p, err := s.client.CoreV1().Pods(s.cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		p, err = s.get(ctx, id)
		if err == nil && p.Annotations["forgejo.org/runner-name"] != r.GetName() {
			return nil, status.Error(codes.AlreadyExists, "environment name collision")
		}
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "create pod: %v", err)
	}
	_ = p
	arch := "aarch64"
	if s.cfg.Arch == "amd64" {
		arch = "x86_64"
	}
	return &pb.CreateResponse{EnvironmentId: id, RootPath: workspace, ActPath: workspace + "/act", ToolCachePath: workspace + "/toolcache", TempPath: workspace + "/tmp", Os: "Linux", Arch: arch, DefaultPathVariable: ptr("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")}, nil
}
func ptr(s string) *string { return &s }
func podSpec(id, name string, c Config) *core.Pod {
	workspaceSize, storageRequest, storageLimit, err := c.storageQuantities()
	if err != nil {
		panic(err) // New validates the config before any Pod can be created.
	}
	no := false
	never := core.RestartPolicyNever
	seccomp := core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}
	return &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: c.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "forgejo-runner-kubernetes", "forgejo.org/execution-id": id}, Annotations: map[string]string{"forgejo.org/runner-name": name}}, Spec: core.PodSpec{
		RestartPolicy: never, AutomountServiceAccountToken: &no, NodeSelector: map[string]string{"kubernetes.io/arch": c.Arch}, SecurityContext: &core.PodSecurityContext{SeccompProfile: &seccomp, RunAsNonRoot: ptrBool(true), RunAsUser: ptrInt64(10001), FSGroup: ptrInt64(10001)},
		Volumes: []core.Volume{{Name: "workspace", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &workspaceSize}}}},
		Containers: []core.Container{{Name: "job", Image: c.Image, ImagePullPolicy: core.PullIfNotPresent, Command: []string{"/bin/sh", "-c", "mkdir -p /shared/act /shared/toolcache /shared/workdir /shared/tmp && exec sleep infinity"}, VolumeMounts: []core.VolumeMount{{Name: "workspace", MountPath: workspace}, {Name: "workspace", MountPath: "/workspace"}},
			SecurityContext: &core.SecurityContext{AllowPrivilegeEscalation: &no, Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}}, Resources: core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("128Mi"), core.ResourceEphemeralStorage: storageRequest}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("1"), core.ResourceMemory: resource.MustParse("1Gi"), core.ResourceEphemeralStorage: storageLimit}}}},
	}}
}
func ptrBool(v bool) *bool    { return &v }
func ptrInt64(v int64) *int64 { return &v }
func (s *Server) Start(r *pb.StartRequest, stream grpc.ServerStreamingServer[pb.StartOutput]) error {
	id := r.GetEnvironmentId()
	if err := s.checkID(id); err != nil {
		return err
	}
	unlock := s.lock(id)
	defer unlock()
	ctx, cancel := context.WithTimeout(stream.Context(), s.cfg.StartupTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		p, err := s.get(ctx, id)
		if err != nil {
			return false, err
		}
		if p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
			return false, status.Errorf(codes.FailedPrecondition, "pod terminated: %s", p.Status.Message)
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "job" && cs.Ready {
				return true, nil
			}
			if cs.State.Waiting != nil && (cs.State.Waiting.Reason == "ErrImagePull" || cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "CreateContainerConfigError") {
				return false, status.Errorf(codes.FailedPrecondition, "job container: %s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == core.PodScheduled && cond.Status == core.ConditionFalse && cond.Reason == "Unschedulable" {
				return false, status.Errorf(codes.FailedPrecondition, "pod unschedulable: %s", cond.Message)
			}
		}
		return false, nil
	})
	if err != nil {
		if stream.Context().Err() != nil {
			return stream.Context().Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return status.Error(codes.DeadlineExceeded, "pod startup timed out")
		}
		return err
	}
	return stream.Send(&pb.StartOutput{Output: &pb.StartOutput_StartComplete{StartComplete: &pb.StartComplete{ImageEnv: map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}}}})
}
func (s *Server) Remove(_ context.Context, r *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	id := r.GetEnvironmentId()
	if err := s.checkID(id); err != nil {
		return nil, err
	}
	unlock := s.lock(id)
	defer unlock()
	// Remove may arrive with an already-cancelled Runner context. Complete the
	// bounded deletion anyway; the Pod must not outlive a cancelled job.
	timeout, cancel := context.WithTimeout(context.Background(), s.cfg.CleanupTimeout)
	defer cancel()
	if _, err := s.get(timeout, id); err != nil {
		if status.Code(err) == codes.NotFound {
			return &pb.RemoveResponse{}, nil
		}
		return nil, err
	}
	grace := int64(0)
	err := s.client.CoreV1().Pods(s.cfg.Namespace).Delete(timeout, id, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Unavailable, "delete pod: %v", err)
	}
	// Deletion may finish asynchronously; deterministic ID prevents mistaking a
	// different pod for this execution. Future orphan sweep can use the labels.
	return &pb.RemoveResponse{}, nil
}
