package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

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

// Config fixes the job namespace and supplies the default job image. Runner
// label arguments and explicit container.image may select another job image.
type Config struct {
	Namespace               string
	Image                   string
	Arch                    string
	StartupTimeout          time.Duration
	CleanupTimeout          time.Duration
	WorkspaceSizeLimit      string // Kubernetes quantities; empty means default.
	EphemeralStorageRequest string
	EphemeralStorageLimit   string
	AppArmorProfile         string // Empty leaves AppArmor unspecified; otherwise runtime-default, unconfined or localhost:<name>.
	SecurityProfile         string // Empty/fixed preserves legacy identity; image/image-ci opt into image USER and env.
	PermissionsImage        string // Operator-owned digest-pinned helper, required only for image/image-ci.
	DinD                    DinDConfig
}

func (c Config) Validate() error {
	if c.Namespace == "" || c.Image == "" || (c.Arch != "arm64" && c.Arch != "amd64") || c.StartupTimeout <= 0 || c.CleanupTimeout <= 0 {
		return fmt.Errorf("namespace, image, arm64/amd64 architecture and positive timeouts are required")
	}
	if _, _, _, err := c.storageQuantities(); err != nil {
		return err
	}
	if _, err := c.appArmorProfile(); err != nil {
		return err
	}
	if err := c.validateIdentity(); err != nil {
		return err
	}
	return c.DinD.validate()
}

// Use the container-level API field: it applies only to the job container
// into which Runner execs, without modifying Pod-wide security defaults.
func (c Config) appArmorProfile() (*core.AppArmorProfile, error) {
	switch c.AppArmorProfile {
	case "":
		return nil, nil
	case "runtime-default":
		return &core.AppArmorProfile{Type: core.AppArmorProfileTypeRuntimeDefault}, nil
	case "unconfined":
		return &core.AppArmorProfile{Type: core.AppArmorProfileTypeUnconfined}, nil
	}
	if strings.HasPrefix(c.AppArmorProfile, "localhost:") {
		name := strings.TrimPrefix(c.AppArmorProfile, "localhost:")
		if name != "" && name == strings.TrimSpace(name) && strings.IndexFunc(name, unicode.IsControl) == -1 {
			return &core.AppArmorProfile{Type: core.AppArmorProfileTypeLocalhost, LocalhostProfile: &name}, nil
		}
		return nil, fmt.Errorf("JOB_APPARMOR_PROFILE: localhost requires a non-empty profile name without surrounding whitespace or control characters")
	}
	return nil, fmt.Errorf("JOB_APPARMOR_PROFILE: invalid value %q; use runtime-default, unconfined or localhost:<name> (or leave unset)", c.AppArmorProfile)
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
	// Runner v13.2 sends image="" for jobs without container.image, leaving
	// the plugin label suffix in label_arg. An explicit container.image takes
	// precedence; the configured image is only the fallback for an empty label.
	image := r.GetImage()
	if image == "" {
		image = r.GetLabelArg()
	}
	if image == "" {
		image = s.cfg.Image
	}
	if strings.TrimSpace(r.GetName()) == "" || image != strings.TrimSpace(image) || strings.ContainsRune(image, 0) || len(r.GetServices()) != 0 || len(r.GetBackendOptions()) != 0 || len(r.GetCapAdd()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "name and valid image required; services, options and cap_add unsupported")
	}
	id := podName(r.GetName())
	unlock := s.lock(id)
	defer unlock()
	obj := podSpec(id, r.GetName(), image, s.cfg)
	if r.GetEnvironmentTimeout() != nil && r.GetEnvironmentTimeout().AsDuration() > 0 {
		seconds := int64((r.GetEnvironmentTimeout().AsDuration() + time.Second - 1) / time.Second)
		obj.Spec.ActiveDeadlineSeconds = &seconds
	}
	p, err := s.client.CoreV1().Pods(s.cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		p, err = s.get(ctx, id)
		if err == nil && !sameEnvironment(p, obj) {
			return nil, status.Error(codes.AlreadyExists, "environment name collision or job template configuration mismatch")
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
func sameEnvironment(p, expected *core.Pod) bool {
	if p.Annotations["forgejo.org/runner-name"] != expected.Annotations["forgejo.org/runner-name"] ||
		p.Annotations[jobTemplate] == "" || p.Annotations[jobTemplate] != expected.Annotations[jobTemplate] ||
		p.Annotations[profileAnnotation] != expected.Annotations[profileAnnotation] ||
		p.Annotations[dindTemplate] != expected.Annotations[dindTemplate] || len(p.Spec.Containers) != len(expected.Spec.Containers) || len(p.Spec.InitContainers) != len(expected.Spec.InitContainers) {
		return false
	}
	for i, c := range expected.Spec.InitContainers {
		if p.Spec.InitContainers[i].Name != c.Name || p.Spec.InitContainers[i].Image != c.Image {
			return false
		}
	}
	for i, c := range expected.Spec.Containers {
		if p.Spec.Containers[i].Name != c.Name || p.Spec.Containers[i].Image != c.Image {
			return false
		}
	}
	return true
}

func ptr(s string) *string { return &s }
func podSpec(id, name, image string, c Config) *core.Pod {
	workspaceSize, storageRequest, storageLimit, err := c.storageQuantities()
	if err != nil {
		panic(err) // New validates the config before any Pod can be created.
	}
	appArmor, err := c.appArmorProfile()
	if err != nil {
		panic(err) // New validates the config before any Pod can be created.
	}
	no := false
	never := core.RestartPolicyNever
	seccomp := core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}
	p := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: c.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "forgejo-runner-kubernetes", "forgejo.org/execution-id": id}, Annotations: map[string]string{"forgejo.org/runner-name": name}}, Spec: core.PodSpec{
		RestartPolicy: never, AutomountServiceAccountToken: &no, NodeSelector: map[string]string{"kubernetes.io/arch": c.Arch}, SecurityContext: &core.PodSecurityContext{SeccompProfile: &seccomp, FSGroup: ptrInt64(10001)},
		Volumes: []core.Volume{{Name: "workspace", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &workspaceSize}}}},
		Containers: []core.Container{{Name: "job", Image: image, ImagePullPolicy: core.PullIfNotPresent, Command: []string{"/bin/sh", "-c", "mkdir -p /shared/act /shared/toolcache /shared/workdir /shared/tmp && exec sleep infinity"}, VolumeMounts: []core.VolumeMount{{Name: "workspace", MountPath: workspace}, {Name: "workspace", MountPath: "/workspace"}},
			SecurityContext: &core.SecurityContext{AppArmorProfile: appArmor, AllowPrivilegeEscalation: &no, Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}}, Resources: core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("128Mi"), core.ResourceEphemeralStorage: storageRequest}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("1"), core.ResourceMemory: resource.MustParse("1Gi"), core.ResourceEphemeralStorage: storageLimit}}}},
	}}
	if c.DinD.Enabled {
		c.DinD.addToPod(p)
	}
	c.applyIdentity(p)
	// All profiles pin the full template before API defaulting and Runner's
	// remaining lifetime. Never adopt an old, unhashed Pod on a Create retry.
	encoded, err := json.Marshal(struct {
		Spec              core.PodSpec `json:"spec"`
		Profile           string       `json:"profile"`
		EnvironmentPolicy string       `json:"environmentPolicy"`
	}{p.Spec, c.securityProfile(), "job-env-v1"})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	p.Annotations[jobTemplate] = hex.EncodeToString(sum[:])
	if c.DinD.Enabled {
		p.Annotations[dindTemplate] = p.Annotations[jobTemplate]
	}
	return p
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
	waiting := "job readiness"
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		p, err := s.get(ctx, id)
		if err != nil {
			return false, err
		}
		if p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
			return false, status.Errorf(codes.FailedPrecondition, "pod terminated: %s", p.Status.Message)
		}
		permissionsReady := len(p.Spec.InitContainers) == 0
		for _, cs := range p.Status.InitContainerStatuses {
			if cs.Name == "permissions" && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
				permissionsReady = true
			}
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				return false, status.Errorf(codes.FailedPrecondition, "permissions init failed (exit %d)", cs.State.Terminated.ExitCode)
			}
			if cs.State.Waiting != nil {
				switch cs.State.Waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
					return false, status.Errorf(codes.FailedPrecondition, "permissions init: %s", cs.State.Waiting.Reason)
				}
			}
		}
		jobReady, daemonReady := false, false
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				return false, status.Errorf(codes.FailedPrecondition, "%s container terminated: %s (exit %d): %s", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
			}
			if cs.State.Waiting != nil {
				switch cs.State.Waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
					return false, status.Errorf(codes.FailedPrecondition, "%s container: %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
			}
			if cs.Name == "job" {
				jobReady = cs.Ready
			}
			if cs.Name == "dind" {
				daemonReady = cs.Ready // kubelet's Docker API exec probe, not just Running.
			}
		}
		if permissionsReady && jobReady && (!podHasDinD(p) || daemonReady) {
			return true, nil
		}
		waiting = "job readiness"
		if podHasDinD(p) && !daemonReady {
			waiting = "dind Docker API readiness (docker info probe); inspect dind container logs"
		}
		if !permissionsReady {
			waiting = "permissions init completion"
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
			return status.Errorf(codes.DeadlineExceeded, "pod startup timed out waiting for %s", waiting)
		}
		return err
	}
	p, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	env, err := s.executionDefaults(ctx, p)
	if err != nil {
		return err
	}
	if _, present := env["PATH"]; !present {
		env["PATH"] = defaultExecPath
	}
	return stream.Send(&pb.StartOutput{Output: &pb.StartOutput_StartComplete{StartComplete: &pb.StartComplete{ImageEnv: env}}})
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
