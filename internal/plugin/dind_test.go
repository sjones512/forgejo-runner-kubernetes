package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const testDinDImage = "docker.io/library/docker@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0"

func dindConfig() Config {
	c := testConfig()
	c.DinD = DinDConfig{Enabled: true, Image: testDinDImage}
	return c
}

func TestDinDDisabledPodUnchanged(t *testing.T) {
	cfg := testConfig()
	before, _ := json.Marshal(podSpec(podName("unchanged"), "unchanged", cfg.Image, cfg))
	cfg.DinD = dindConfig().DinD
	cfg.DinD.Enabled = false
	after, _ := json.Marshal(podSpec(podName("unchanged"), "unchanged", cfg.Image, cfg))
	if string(before) != string(after) {
		t.Fatalf("disabled feature changed Pod: %s", after)
	}
	if strings.Contains(string(after), "docker-socket") || strings.Contains(string(after), "DOCKER_HOST") {
		t.Fatal("disabled feature leaked DinD state")
	}
}

func hasMount(mounts []core.VolumeMount, want core.VolumeMount) bool {
	return slices.ContainsFunc(mounts, func(m core.VolumeMount) bool { return reflect.DeepEqual(m, want) })
}

func TestDinDPodIsolationAndMounts(t *testing.T) {
	for _, armor := range []string{"", "runtime-default", "unconfined", "localhost:ci-jobs"} {
		t.Run(armor, func(t *testing.T) {
			cfg := dindConfig()
			cfg.AppArmorProfile = armor
			p := podSpec(podName("dind"), "dind", "node:24-bookworm", cfg)
			baseline := cfg
			baseline.DinD.Enabled = false
			old := podSpec(p.Name, "dind", "node:24-bookworm", baseline)
			if len(p.Spec.Containers) != 2 || p.Spec.Containers[0].Name != "job" || p.Spec.Containers[1].Name != "dind" || p.Spec.Containers[1].Image != testDinDImage {
				t.Fatalf("containers: %+v", p.Spec.Containers)
			}
			job, daemon := p.Spec.Containers[0], p.Spec.Containers[1]
			if !reflect.DeepEqual(job.SecurityContext, old.Spec.Containers[0].SecurityContext) || !reflect.DeepEqual(p.Spec.SecurityContext, old.Spec.SecurityContext) || !reflect.DeepEqual(job.Resources, old.Spec.Containers[0].Resources) {
				t.Fatal("DinD altered job security/resources")
			}
			sc := daemon.SecurityContext
			if !*sc.Privileged || *sc.RunAsUser != 0 || *sc.RunAsGroup != 0 || *sc.RunAsNonRoot || !*sc.AllowPrivilegeEscalation || sc.Capabilities != nil || sc.SeccompProfile.Type != core.SeccompProfileTypeUnconfined || sc.AppArmorProfile.Type != core.AppArmorProfileTypeUnconfined {
				t.Fatalf("daemon context: %+v", sc)
			}
			if p.Spec.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault || p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC || p.Spec.ShareProcessNamespace != nil || *p.Spec.AutomountServiceAccountToken || p.Spec.ServiceAccountName != "" || len(p.Spec.InitContainers) != 0 || len(p.Spec.ImagePullSecrets) != 0 || p.Spec.RestartPolicy != core.RestartPolicyNever {
				t.Fatalf("host/token/lifecycle boundary: %+v", p.Spec)
			}
			for _, c := range p.Spec.Containers {
				if len(c.Ports) != 0 {
					t.Fatal("unexpected exposed ports")
				}
				for _, m := range c.VolumeMounts {
					if m.MountPropagation != nil {
						t.Fatal("unexpected mount propagation")
					}
				}
			}
			if len(p.Spec.Volumes) != 3 {
				t.Fatal(p.Spec.Volumes)
			}
			for _, v := range p.Spec.Volumes {
				if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || v.EmptyDir.Medium != "" || v.HostPath != nil || v.Projected != nil || v.Secret != nil {
					t.Fatalf("non-ephemeral/unbounded volume: %+v", v)
				}
			}
			for _, m := range []core.VolumeMount{{Name: "workspace", MountPath: "/shared"}, {Name: "workspace", MountPath: "/workspace"}, {Name: "docker-socket", MountPath: dockerSocketDir}} {
				if !hasMount(job.VolumeMounts, m) || !hasMount(daemon.VolumeMounts, m) {
					t.Fatalf("missing common bind/socket path %+v", m)
				}
			}
			if !hasMount(daemon.VolumeMounts, core.VolumeMount{Name: "docker-data", MountPath: "/var/lib"}) || hasMount(job.VolumeMounts, core.VolumeMount{Name: "docker-data", MountPath: "/var/lib"}) {
				t.Fatal("daemon state must not be mounted in job")
			}
			if !slices.Contains(job.Env, core.EnvVar{Name: "DOCKER_HOST", Value: dockerHost}) || !slices.Contains(job.Env, core.EnvVar{Name: "TMPDIR", Value: "/shared/tmp"}) {
				t.Fatal("job endpoint/fixture directory defaults missing")
			}
			if !slices.Equal(daemon.Args, []string{"dockerd", "--host=" + dockerHost, "--group=10001", "--data-root=/var/lib/docker", "--exec-root=/run/forgejo-docker-state"}) || len(daemon.Command) != 0 {
				t.Fatalf("must retain image entrypoint with fixed Unix-only args: %+v", daemon)
			}
			probe := daemon.ReadinessProbe
			if probe == nil || !slices.Equal(probe.Exec.Command, []string{"docker", "--host=" + dockerHost, "info"}) || probe.TimeoutSeconds <= 0 || probe.PeriodSeconds <= 0 {
				t.Fatal("missing bounded Docker API probe")
			}
		})
	}
}

func TestDinDResourcesAndValidation(t *testing.T) {
	defaults := dindConfig()
	p := podSpec(podName("resources"), "resources", defaults.Image, defaults)
	if p.Spec.Volumes[2].EmptyDir.SizeLimit.String() != "10Gi" || p.Spec.Containers[1].Resources.Limits.StorageEphemeral().String() != "12Gi" {
		t.Fatal("default data/headroom budget")
	}
	for _, tc := range []struct {
		name string
		set  func(*DinDConfig)
		want string
	}{
		{"default", func(*DinDConfig) {}, ""},
		{"overlay2", func(d *DinDConfig) { d.StorageDriver = "overlay2" }, ""},
		{"tag plus digest", func(d *DinDConfig) { d.Image = strings.Replace(testDinDImage, "@", ":29-dind@", 1) }, ""},
		{"missing image", func(d *DinDConfig) { d.Image = "" }, "JOB_DIND_IMAGE"},
		{"mutable image", func(d *DinDConfig) { d.Image = "docker:29-dind" }, "JOB_DIND_IMAGE"},
		{"short digest", func(d *DinDConfig) { d.Image = "docker@sha256:abc" }, "JOB_DIND_IMAGE"},
		{"invalid name", func(d *DinDConfig) { d.Image = "https://" + testDinDImage }, "JOB_DIND_IMAGE"},
		{"bad disabled image", func(d *DinDConfig) { d.Enabled = false; d.Image = "docker:latest" }, "JOB_DIND_IMAGE"},
		{"cpu zero", func(d *DinDConfig) { d.CPURequest = "0" }, "JOB_DIND_CPU_REQUEST"},
		{"cpu reversed", func(d *DinDConfig) { d.CPURequest = "3" }, "JOB_DIND_CPU_REQUEST"},
		{"cpu limit invalid", func(d *DinDConfig) { d.CPULimit = "bogus" }, "JOB_DIND_CPU_LIMIT"},
		{"memory reversed", func(d *DinDConfig) { d.MemoryRequest = "3Gi" }, "JOB_DIND_MEMORY_REQUEST"},
		{"memory negative", func(d *DinDConfig) { d.MemoryLimit = "-1Gi" }, "JOB_DIND_MEMORY_LIMIT"},
		{"storage reversed", func(d *DinDConfig) { d.EphemeralStorageRequest = "13Gi" }, "JOB_DIND_EPHEMERAL_STORAGE_REQUEST"},
		{"storage zero", func(d *DinDConfig) { d.EphemeralStorageLimit = "0" }, "JOB_DIND_EPHEMERAL_STORAGE_LIMIT"},
		{"data invalid", func(d *DinDConfig) { d.DataSizeLimit = "10GiB" }, "JOB_DIND_DATA_SIZE_LIMIT"},
		{"data zero", func(d *DinDConfig) { d.DataSizeLimit = "0" }, "JOB_DIND_DATA_SIZE_LIMIT"},
		{"data exceeds budget", func(d *DinDConfig) { d.DataSizeLimit = "12Gi" }, "JOB_DIND_DATA_SIZE_LIMIT"},
		{"data socket equals budget", func(d *DinDConfig) { d.EphemeralStorageLimit = "10241Mi" }, "JOB_DIND_DATA_SIZE_LIMIT"},
		{"driver invalid", func(d *DinDConfig) { d.StorageDriver = "--host=tcp://0.0.0.0:2375" }, "JOB_DIND_STORAGE_DRIVER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults
			tc.set(&cfg.DinD)
			_, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{Host: "https://example.invalid"})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	cfg := dindConfig()
	cfg.DinD = DinDConfig{Enabled: true, Image: testDinDImage, CPURequest: "500m", CPULimit: "4", MemoryRequest: "1Gi", MemoryLimit: "4Gi", DataSizeLimit: "20Gi", EphemeralStorageRequest: "3Gi", EphemeralStorageLimit: "25Gi", StorageDriver: "vfs"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p = podSpec(p.Name, "resources", cfg.Image, cfg)
	r := p.Spec.Containers[1].Resources
	if r.Requests.Cpu().String() != "500m" || r.Limits.Cpu().String() != "4" || r.Requests.Memory().String() != "1Gi" || r.Limits.Memory().String() != "4Gi" || r.Requests.StorageEphemeral().String() != "3Gi" || r.Limits.StorageEphemeral().String() != "25Gi" || p.Spec.Volumes[2].EmptyDir.SizeLimit.String() != "20Gi" || !slices.Contains(p.Spec.Containers[1].Args, "--storage-driver=vfs") {
		t.Fatal("custom daemon resources/driver not applied")
	}
}

func TestDinDImageSelectionRetryAndRemove(t *testing.T) {
	for _, tc := range []struct{ image, label, want string }{
		{"node:26-bookworm", "node:24-bookworm", "node:26-bookworm"},
		{"", "node:24-bookworm", "node:24-bookworm"},
		{"", "", "ubuntu:24.04"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			cfg := dindConfig()
			kube := fake.NewSimpleClientset()
			s, _ := New(cfg, kube, &rest.Config{})
			req := &pb.CreateRequest{Name: "retry", Image: tc.image, LabelArg: tc.label}
			created, err := s.Create(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), created.EnvironmentId, metav1.GetOptions{})
			if p.Spec.Containers[0].Image != tc.want || p.Spec.Containers[1].Image != cfg.DinD.Image {
				t.Fatal("job image selection coupled to daemon")
			}
			if _, err := s.Create(context.Background(), req); err != nil {
				t.Fatal("same configuration retry failed", err)
			}
			for _, change := range []func(*Config){
				func(c *Config) { c.DinD.Enabled = false },
				func(c *Config) { c.DinD.Image = "docker@sha256:" + strings.Repeat("a", 64) },
				func(c *Config) { c.DinD.MemoryLimit = "4Gi" },
				func(c *Config) { c.DinD.StorageDriver = "vfs" },
				func(c *Config) { c.WorkspaceSizeLimit = "512Mi" },
			} {
				other := cfg
				change(&other)
				restarted, _ := New(other, kube, &rest.Config{})
				if _, err := restarted.Create(context.Background(), req); status.Code(err) != codes.AlreadyExists {
					t.Fatalf("different configuration retry accepted: %v", err)
				}
			}
			if _, err := s.Create(context.Background(), &pb.CreateRequest{Name: "services", Services: []*pb.ServiceContainer{{Name: "docker", Image: cfg.DinD.Image}}}); status.Code(err) != codes.InvalidArgument {
				t.Fatal("generic workflow services must remain rejected")
			}
			for i := 0; i < 2; i++ {
				if _, err := s.Remove(context.Background(), &pb.RemoveRequest{EnvironmentId: created.EnvironmentId}); err != nil {
					t.Fatal(err)
				}
			}
			pods, _ := kube.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 0 {
				t.Fatal("daemon/job Pod leaked")
			}
		})
	}
}

// Unit tests simulate kubelet probe results in ContainerStatus. They validate
// Start's gate, not a real dockerd/API exec probe on Kubernetes.
type startCapture struct {
	grpc.ServerStream
	ctx context.Context
	out []*pb.StartOutput
}

func (s *startCapture) Context() context.Context       { return s.ctx }
func (s *startCapture) Send(out *pb.StartOutput) error { s.out = append(s.out, out); return nil }

func TestDinDStartReadinessAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   core.ContainerStatus
		want    codes.Code
		message string
	}{
		{"ready", core.ContainerStatus{Name: "dind", Ready: true, State: core.ContainerState{Running: &core.ContainerStateRunning{}}}, codes.OK, ""},
		{"running but probe fails", core.ContainerStatus{Name: "dind", State: core.ContainerState{Running: &core.ContainerStateRunning{}}}, codes.DeadlineExceeded, "dind Docker API readiness"},
		{"missing status", core.ContainerStatus{}, codes.DeadlineExceeded, "dind Docker API readiness"},
		{"job not ready", core.ContainerStatus{Name: "dind", Ready: true}, codes.DeadlineExceeded, "job readiness"},
		{"image pull failure", core.ContainerStatus{Name: "dind", State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "image unavailable"}}}, codes.FailedPrecondition, "dind container: ImagePullBackOff"},
		{"daemon exit", core.ContainerStatus{Name: "dind", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "cgroup setup failed"}}}, codes.FailedPrecondition, "dind container terminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dindConfig()
			cfg.StartupTimeout = 30 * time.Millisecond
			kube := fake.NewSimpleClientset()
			s, _ := New(cfg, kube, &rest.Config{})
			created, err := s.Create(context.Background(), &pb.CreateRequest{Name: "start"})
			if err != nil {
				t.Fatal(err)
			}
			p, _ := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), created.EnvironmentId, metav1.GetOptions{})
			p.Status.Phase = core.PodRunning
			// Job status FIRST must not bypass daemon readiness/failure.
			p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "job", Ready: tc.name != "job not ready"}, tc.state}
			kube.CoreV1().Pods(cfg.Namespace).UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
			stream := &startCapture{ctx: context.Background()}
			err = s.Start(&pb.StartRequest{EnvironmentId: p.Name}, stream)
			if status.Code(err) != tc.want || tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("start: %v, want %v %q", err, tc.want, tc.message)
			}
			if tc.want == codes.OK {
				if len(stream.out) != 1 || stream.out[0].GetStartComplete().ImageEnv["DOCKER_HOST"] != dockerHost || stream.out[0].GetStartComplete().ImageEnv["TMPDIR"] != "/shared/tmp" {
					t.Fatal("missing completion/defaults")
				}
			} else if len(stream.out) != 0 {
				t.Fatal("reported StartComplete before daemon ready")
			}
			if _, err := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{}); err != nil {
				t.Fatal("Start must leave cleanup to Runner Remove")
			}
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := s.Remove(cancelled, &pb.RemoveRequest{EnvironmentId: p.Name}); err != nil {
				t.Fatal(err)
			}
			pods, _ := kube.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 0 {
				t.Fatal("cleanup leaked daemon")
			}
		})
	}
}

func TestDinDStartCancellation(t *testing.T) {
	cfg := dindConfig()
	kube := fake.NewSimpleClientset()
	s, err := New(cfg, kube, &rest.Config{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(context.Background(), &pb.CreateRequest{Name: "cancelled-start"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &startCapture{ctx: ctx}
	if err := s.Start(&pb.StartRequest{EnvironmentId: created.EnvironmentId}, stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled Start, got %v", err)
	}
	if len(stream.out) != 0 {
		t.Fatal("cancelled startup reported complete")
	}
	if _, err := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), created.EnvironmentId, metav1.GetOptions{}); err != nil {
		t.Fatal("cancellation must not delete before Remove", err)
	}
	if _, err := s.Remove(ctx, &pb.RemoveRequest{EnvironmentId: created.EnvironmentId}); err != nil {
		t.Fatal(err)
	}
	pods, _ := kube.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatal("cancelled startup leaked Pod")
	}
}

func TestDinDExecEnvironmentDefaults(t *testing.T) {
	cfg := dindConfig()
	kube := fake.NewSimpleClientset()
	s, _ := New(cfg, kube, &rest.Config{})
	created, _ := s.Create(context.Background(), &pb.CreateRequest{Name: "exec"})
	// A config reload must not strip defaults from an existing DinD Pod.
	s.cfg.DinD.Enabled = false
	s.execFn = func(_ context.Context, _ string, args []string, _ io.Reader, _, _ io.Writer) error {
		if !slices.Contains(args, "DOCKER_HOST="+dockerHost) || !slices.Contains(args, "TMPDIR=/shared/tmp") || !slices.Contains(args, "HOME=/shared/workdir") || !slices.Contains(args, "INPUT_FETCH-DEPTH=0") {
			t.Errorf("lost Exec defaults/Action env: %q", args)
		}
		return nil
	}
	// No stdout is written; capture the final typed Exec message.
	stream := &execCapture{ctx: context.Background()}
	r := &pb.ExecRequest{EnvironmentId: created.EnvironmentId, Command: []string{"docker", "info"}, Env: map[string]string{"INPUT_FETCH-DEPTH": "0"}}
	if err := s.Exec(r, stream); err != nil {
		t.Fatal(err)
	}
	if len(r.Env) != 1 {
		t.Fatal("mutated Runner request env")
	}
	p, _ := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), created.EnvironmentId, metav1.GetOptions{})
	args, err := commandArgsWithDefaults(&pb.ExecRequest{Command: []string{"env"}, Env: map[string]string{"DOCKER_HOST": "unix:///custom.sock", "TMPDIR": "/workspace/custom", "HOME": "/workspace/home"}}, podExecDefaults(p))
	if err != nil || !slices.Contains(args, "DOCKER_HOST=unix:///custom.sock") || slices.Contains(args, "DOCKER_HOST="+dockerHost) || !slices.Contains(args, "TMPDIR=/workspace/custom") || !slices.Contains(args, "HOME=/workspace/home") {
		t.Fatalf("explicit Runner env override lost: %q %v", args, err)
	}
}

type execCapture struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *execCapture) Context() context.Context  { return s.ctx }
func (s *execCapture) Send(*pb.ExecOutput) error { return nil }
