package plugin

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const testPermissionsImage = "docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092"

func imageConfig(profile string) Config {
	cfg := testConfig()
	cfg.SecurityProfile, cfg.PermissionsImage = profile, testPermissionsImage
	return cfg
}

func TestIdentityProfilePodPolicies(t *testing.T) {
	for _, profile := range []string{"", profileFixed, profileImage, profileImageCI} {
		for _, dind := range []bool{false, true} {
			t.Run(profile+map[bool]string{false: "/plain", true: "/dind"}[dind], func(t *testing.T) {
				cfg := testConfig()
				cfg.SecurityProfile = profile
				if cfg.securityProfile() != profileFixed {
					cfg.PermissionsImage = testPermissionsImage
				}
				if dind {
					cfg.DinD = dindConfig().DinD
				}
				cfg.AppArmorProfile = "unconfined"
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
				p := podSpec(podName("policy"), "policy", cfg.Image, cfg)
				pc, job := p.Spec.SecurityContext, p.Spec.Containers[0]
				sc := job.SecurityContext
				if pc.RunAsUser != nil || pc.RunAsGroup != nil || pc.RunAsNonRoot != nil || pc.FSGroup == nil || *pc.FSGroup != 10001 || pc.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault || pc.AppArmorProfile != nil {
					t.Fatal("Pod identity/security policy")
				}
				if sc.RunAsGroup != nil || *sc.AllowPrivilegeEscalation || sc.Privileged != nil || sc.SeccompProfile != nil || sc.AppArmorProfile.Type != core.AppArmorProfileTypeUnconfined || !slices.Equal(sc.Capabilities.Drop, []core.Capability{"ALL"}) {
					t.Fatal("job security policy")
				}
				if cfg.securityProfile() == profileFixed {
					if sc.RunAsUser == nil || *sc.RunAsUser != 10001 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || len(p.Spec.InitContainers) != 0 {
						t.Fatal("legacy identity/helper changed")
					}
				} else {
					if sc.RunAsUser != nil || sc.RunAsNonRoot != nil || len(p.Spec.InitContainers) != 1 {
						t.Fatal("image identity overridden")
					}
					h := p.Spec.InitContainers[0]
					if h.Image != testPermissionsImage || h.Image == job.Image || *h.SecurityContext.RunAsUser != 0 || *h.SecurityContext.RunAsGroup != 10001 || *h.SecurityContext.AllowPrivilegeEscalation || h.SecurityContext.Privileged != nil || len(h.SecurityContext.Capabilities.Add) != 0 || !slices.Equal(h.SecurityContext.Capabilities.Drop, []core.Capability{"ALL"}) || len(h.Resources.Limits) != 3 || len(h.Resources.Requests) != 3 {
						t.Fatal("helper not bounded/independent")
					}
					if !slices.Equal(h.Command, []string{"/bin/sh", "-ec", permissionsScript, "permissions"}) || !slices.Contains(h.Args, workspace+"/workdir") || !slices.Contains(h.Args, workspace+"/tmp") || slices.Contains(h.Args, dockerDataRoot) {
						t.Fatal("permission helper scope")
					}
					if dind != slices.Contains(h.Args, dockerSocketDir) {
						t.Fatal("socket permission setup")
					}
					if h.Resources.Requests.Cpu().String() != "10m" || h.Resources.Requests.Memory().String() != "16Mi" || h.Resources.Requests.StorageEphemeral().String() != "1Mi" || h.Resources.Limits.Cpu().String() != "100m" || h.Resources.Limits.Memory().String() != "64Mi" || h.Resources.Limits.StorageEphemeral().String() != "64Mi" {
						t.Fatal("helper budgets changed")
					}
					mutated := p.DeepCopy()
					mutated.Spec.InitContainers[0].Image = "other@sha256:" + strings.Repeat("f", 64)
					if sameEnvironment(mutated, p) {
						t.Fatal("mutated helper adopted despite old annotation")
					}
				}
				wantCaps := []core.Capability(nil)
				if profile == profileImageCI {
					wantCaps = []core.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID", "SETFCAP", "SYS_CHROOT", "KILL"}
				}
				if !slices.Equal(sc.Capabilities.Add, wantCaps) {
					t.Fatalf("capabilities %v", sc.Capabilities.Add)
				}
				for _, denied := range []core.Capability{"SYS_ADMIN", "NET_RAW", "MKNOD", "SETPCAP", "NET_ADMIN"} {
					if slices.Contains(sc.Capabilities.Add, denied) {
						t.Fatal("excess capability", denied)
					}
				}
				if *p.Spec.AutomountServiceAccountToken || p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC || p.Spec.ShareProcessNamespace != nil || len(p.Spec.ImagePullSecrets) != 0 || p.Spec.ServiceAccountName != "" || p.Spec.RestartPolicy != core.RestartPolicyNever {
					t.Fatal("isolation changed")
				}
				for _, v := range p.Spec.Volumes {
					if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || v.HostPath != nil || v.Projected != nil {
						t.Fatal("volume boundary")
					}
				}
				base := testConfig()
				if dind {
					base.DinD = cfg.DinD
				}
				old := podSpec(p.Name, "policy", cfg.Image, base)
				if !reflect.DeepEqual(job.Resources, old.Spec.Containers[0].Resources) || !reflect.DeepEqual(job.VolumeMounts, old.Spec.Containers[0].VolumeMounts) || !reflect.DeepEqual(p.Spec.Volumes, old.Spec.Volumes) || !reflect.DeepEqual(job.Command, old.Spec.Containers[0].Command) {
					t.Fatal("job resources/storage/command changed")
				}
				if dind && !reflect.DeepEqual(p.Spec.Containers[1], old.Spec.Containers[1]) {
					t.Fatal("fixed DinD changed")
				}
				if p.Annotations[jobTemplate] == "" || p.Annotations[profileAnnotation] != cfg.securityProfile() {
					t.Fatal("unidentified template")
				}
			})
		}
	}
	unset := testConfig()
	explicit := unset
	explicit.SecurityProfile = profileFixed
	if !reflect.DeepEqual(podSpec("id", "name", unset.Image, unset), podSpec("id", "name", unset.Image, explicit)) {
		t.Fatal("unset must equal explicit fixed")
	}
}

func TestIdentityInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ profile, helper string }{
		{"root", ""}, {"IMAGE", testPermissionsImage}, {"image ", testPermissionsImage},
		{profileImage, ""}, {profileImageCI, "busybox:1.37"}, {profileImage, "busybox@sha256:bad"},
		{profileImage, "busybox@sha512:" + strings.Repeat("a", 128)}, {profileFixed, testPermissionsImage}, {"", testPermissionsImage},
	} {
		cfg := testConfig()
		cfg.SecurityProfile, cfg.PermissionsImage = tc.profile, tc.helper
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %q %q", tc.profile, tc.helper)
		}
	}
}

func TestIdentityTemplateRetryAndServicesRejected(t *testing.T) {
	for _, profile := range []string{profileFixed, profileImage, profileImageCI} {
		t.Run(profile, func(t *testing.T) {
			cfg := testConfig()
			cfg.SecurityProfile = profile
			if profile != profileFixed {
				cfg.PermissionsImage = testPermissionsImage
			}
			kube := fake.NewSimpleClientset()
			s, _ := New(cfg, kube, &rest.Config{})
			r := &pb.CreateRequest{Name: "retry"}
			first, err := s.Create(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			again, err := s.Create(context.Background(), r)
			if err != nil || again.EnvironmentId != first.EnvironmentId {
				t.Fatal("idempotent retry", err)
			}
			for _, change := range []func(*Config){
				func(c *Config) { c.AppArmorProfile = "unconfined" },
				func(c *Config) { c.WorkspaceSizeLimit = "512Mi" },
				func(c *Config) {
					c.SecurityProfile, c.PermissionsImage = profileImageCI, testPermissionsImage
					if profile == profileImageCI {
						c.SecurityProfile = profileImage
					}
				},
			} {
				other := cfg
				change(&other)
				reload, _ := New(other, kube, &rest.Config{})
				if _, err := reload.Create(context.Background(), r); status.Code(err) != codes.AlreadyExists {
					t.Fatal("accepted changed template", err)
				}
			}
			if profile != profileFixed {
				other := cfg
				other.PermissionsImage = "busybox@sha256:" + strings.Repeat("a", 64)
				reload, _ := New(other, kube, &rest.Config{})
				if _, err := reload.Create(context.Background(), r); status.Code(err) != codes.AlreadyExists {
					t.Fatal("accepted changed helper", err)
				}
			}
			if _, err := s.Create(context.Background(), &pb.CreateRequest{Name: "service", Services: []*pb.ServiceContainer{{Name: "db", Image: "postgres:17"}}}); status.Code(err) != codes.InvalidArgument {
				t.Fatal("services accepted", err)
			}
			pods, _ := kube.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 1 {
				t.Fatal("unsupported service mutated Kubernetes")
			}
			if _, err := s.Remove(context.Background(), &pb.RemoveRequest{EnvironmentId: first.EnvironmentId}); err != nil {
				t.Fatal(err)
			}
			pods, _ = kube.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 0 {
				t.Fatal("cleanup leaked")
			}
		})
	}
}

func TestImageEnvironmentPrecedenceAndRestart(t *testing.T) {
	for _, home := range []string{"/home/node", ""} {
		cfg := imageConfig(profileImage)
		cfg.DinD = dindConfig().DinD
		p := podSpec(podName("env"), "env", cfg.Image, cfg)
		// Use a different current config: defaults/identity must follow stored Pod.
		s, _ := New(testConfig(), fake.NewSimpleClientset(p), &rest.Config{})
		calls := 0
		s.execFn = func(_ context.Context, _ string, args []string, _ io.Reader, out, _ io.Writer) error {
			if !slices.Equal(args, []string{"/usr/bin/env", "-0"}) {
				t.Fatal("shell/application discovery", args)
			}
			calls++
			_, err := io.WriteString(out, "HOME="+home+"\x00PATH=/image/bin\x00IMAGE_ONLY=one=two\nthree\x00INPUT_A-B=image\x00EMPTY=\x00DOCKER_HOST=wrong\x00TMPDIR=wrong\x00")
			return err
		}
		for i := 0; i < 2; i++ {
			defaults, err := s.executionDefaults(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if defaults["HOME"] != home || defaults["PATH"] != "/image/bin" || defaults["DOCKER_HOST"] != dockerHost || defaults["TMPDIR"] != "/shared/tmp" || defaults["IMAGE_ONLY"] != "one=two\nthree" {
				t.Fatal("defaults lost")
			}
			r := &pb.ExecRequest{Command: []string{"/usr/bin/env", "-0"}, Workdir: t.TempDir(), Env: map[string]string{"HOME": "", "PATH": "/runner/bin", "INPUT_A-B": "runner", "EMPTY": ""}}
			// Execute the full cwd-shell/final-env argv locally, using an allowed
			// workspace path substituted only after command construction.
			wd := r.Workdir
			r.Workdir = "/workspace"
			args, err := commandArgsWithDefaults(r, defaults)
			if err != nil {
				t.Fatal(err)
			}
			args[8] = wd
			cmd := exec.Command(args[0], args[1:]...)
			data, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			actual, err := parseImageEnv(data)
			if err != nil {
				t.Fatal(err)
			}
			if actual["HOME"] != "" || actual["PATH"] != "/runner/bin" || actual["INPUT_A-B"] != "runner" || actual["IMAGE_ONLY"] != defaults["IMAGE_ONLY"] {
				t.Fatal("Runner precedence/empty/non-shell env lost")
			}
			if _, present := actual["EMPTY"]; !present {
				t.Fatal("empty became absent")
			}
		}
		if calls != 2 {
			t.Fatal("defaults cached/persisted instead of rediscovered")
		}
	}
}

func TestImageEnvironmentBoundsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		transport  bool
		want       codes.Code
	}{
		{"empty", "", false, codes.OK}, {"absent HOME", "PATH=/bin\x00", false, codes.OK},
		{"no trailing NUL", "SECRET=sensitive", false, codes.FailedPrecondition},
		{"bad entry", "SECRET=sensitive\x00bad\x00", false, codes.FailedPrecondition},
		{"duplicate", "SECRET=sensitive\x00SECRET=other\x00", false, codes.FailedPrecondition},
		{"empty key", "=sensitive\x00", false, codes.FailedPrecondition},
		{"too big", "SECRET=" + strings.Repeat("x", maxImageEnvBytes) + "\x00", false, codes.ResourceExhausted},
		{"transport", "", true, codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := imageConfig(profileImage)
			p := podSpec(podName(tc.name), tc.name, cfg.Image, cfg)
			s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
			s.execFn = func(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) error {
				if tc.transport {
					return errors.New("sensitive transport detail")
				}
				_, err := io.WriteString(out, tc.data)
				return err
			}
			env, err := s.executionDefaults(context.Background(), p)
			if status.Code(err) != tc.want {
				t.Fatalf("want %v: %v", tc.want, err)
			}
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("secret in diagnostics")
			}
			if tc.want == codes.OK && (env["HOME"] != "/shared/workdir" || env["PATH"] == "") {
				t.Fatal("absent fallback")
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		cfg := imageConfig(profileImage)
		p := podSpec(podName("timeout"), "timeout", cfg.Image, cfg)
		s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
		s.execFn = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			<-ctx.Done()
			return ctx.Err()
		}
		begin := time.Now()
		if _, err := s.executionDefaults(context.Background(), p); status.Code(err) != codes.DeadlineExceeded || time.Since(begin) != imageEnvTimeout {
			t.Fatal("unbounded probe", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.executionDefaults(ctx, p); !errors.Is(err, context.Canceled) {
			t.Fatal("lost cancellation", err)
		}
	})
}

func TestNativeEmptyEnvironmentAndInvalidExec(t *testing.T) {
	cfg := imageConfig(profileImage)
	p := podSpec(podName("empty-env"), "empty-env", cfg.Image, cfg)
	s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
	calls := 0
	s.execFn = func(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) error {
		calls++
		_, err := io.WriteString(out, "PATH=\x00HOME=\x00EMPTY=\x00")
		return err
	}
	env, err := s.executionDefaults(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"PATH", "HOME", "EMPTY"} {
		if value, present := env[key]; !present || value != "" {
			t.Fatal("explicit empty became fallback/absent", key)
		}
	}
	for _, request := range []*pb.ExecRequest{
		{Command: []string{"env"}, User: ptr("root")},
		{Command: []string{"env"}, Env: map[string]string{"VALUE": "secret\x00invalid"}},
		{Command: []string{"env"}, Workdir: "/outside"},
		{},
	} {
		request.EnvironmentId = p.Name
		if err := s.Exec(request, &execCapture{ctx: context.Background()}); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid Exec accepted", err)
		}
	}
	if calls != 1 {
		t.Fatal("invalid request executed environment probe")
	}
}

func TestOldPodProfileAndRetry(t *testing.T) {
	cfg := imageConfig(profileImage)
	oldCfg := testConfig()
	p := podSpec(podName("old"), "old", oldCfg.Image, oldCfg)
	delete(p.Annotations, profileAnnotation)
	delete(p.Annotations, jobTemplate)
	// Approximate the actual pre-profile security placement, not current config.
	p.Spec.SecurityContext.RunAsUser, p.Spec.SecurityContext.RunAsNonRoot = ptrInt64(10001), ptrBool(true)
	p.Spec.Containers[0].SecurityContext.RunAsUser, p.Spec.Containers[0].SecurityContext.RunAsNonRoot = nil, nil
	s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
	s.execFn = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
		t.Fatal("legacy image env probed after config change")
		return nil
	}
	if _, err := s.executionDefaults(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), &pb.CreateRequest{Name: "old"}); status.Code(err) != codes.AlreadyExists {
		t.Fatal("adopted unhashed live Pod", err)
	}
	native := podSpec(podName("stripped"), "stripped", cfg.Image, cfg)
	delete(native.Annotations, profileAnnotation)
	if _, err := s.executionDefaults(context.Background(), native); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("stripped native profile interpreted as legacy", err)
	}
	p.Annotations[profileAnnotation] = "unknown"
	if _, err := s.executionDefaults(context.Background(), p); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("unknown profile accepted", err)
	}
}

func TestPermissionsStartGate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state core.ContainerState
		want  codes.Code
	}{
		{"complete", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}, codes.OK},
		{"failed", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, Message: "SECRET=sensitive"}}, codes.FailedPrecondition},
		{"pull", core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "SECRET=sensitive"}}, codes.FailedPrecondition},
		{"running", core.ContainerState{Running: &core.ContainerStateRunning{}}, codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := imageConfig(profileImage)
			cfg.StartupTimeout = 20 * time.Millisecond
			p := podSpec(podName(tc.name), tc.name, cfg.Image, cfg)
			p.Status.Phase = core.PodRunning
			p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "job", Ready: true}}
			p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "permissions", State: tc.state}}
			s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
			s.execFn = func(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) error {
				_, err := io.WriteString(out, "PATH=/image/bin\x00HOME=/root\x00")
				return err
			}
			stream := &startCapture{ctx: context.Background()}
			err := s.Start(&pb.StartRequest{EnvironmentId: p.Name}, stream)
			if status.Code(err) != tc.want || err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("init readiness", err)
			}
			if tc.want == codes.OK && (len(stream.out) != 1 || stream.out[0].GetStartComplete().ImageEnv["HOME"] != "/root") {
				t.Fatal("missing image env")
			}
			if tc.want != codes.OK && len(stream.out) != 0 {
				t.Fatal("premature StartComplete")
			}
			if _, err := s.get(context.Background(), p.Name); err != nil {
				t.Fatal("Start deleted Pod")
			}
		})
	}
}
