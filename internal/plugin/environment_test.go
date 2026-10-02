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

func TestOrdinaryImageJobPolicy(t *testing.T) {
	for _, dind := range []bool{false, true} {
		cfg := testConfig()
		if dind {
			cfg.DinD = dindConfig().DinD
		}
		p := podSpec(podName("ordinary"), "ordinary", cfg.Image, cfg)
		pc, job := p.Spec.SecurityContext, p.Spec.Containers[0]
		sc := job.SecurityContext
		if pc.RunAsUser != nil || pc.RunAsGroup != nil || pc.RunAsNonRoot != nil || sc.RunAsUser != nil || sc.RunAsGroup != nil || sc.RunAsNonRoot != nil || sc.Capabilities != nil {
			t.Fatal("image identity/runtime capabilities overridden")
		}
		if sc.Privileged == nil || *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || pc.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
			t.Fatal("job outer security boundary")
		}
		if dind {
			if pc.FSGroup == nil || *pc.FSGroup != 10001 {
				t.Fatal("socket group missing")
			}
		} else if pc.FSGroup != nil {
			t.Fatal("unnecessary group on ordinary workspace")
		}
		if len(p.Spec.InitContainers) != 0 || *p.Spec.AutomountServiceAccountToken || p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC || p.Spec.ShareProcessNamespace != nil {
			t.Fatal("job isolation/init boundary")
		}
		for _, v := range p.Spec.Volumes {
			if v.EmptyDir == nil || v.EmptyDir.SizeLimit != nil || v.HostPath != nil || v.Projected != nil {
				t.Fatal("volume boundary")
			}
		}
		if len(job.Resources.Limits) != 0 || len(job.Resources.Requests) != 3 || len(job.VolumeMounts) != 2+map[bool]int{false: 0, true: 1}[dind] {
			t.Fatal("storage/resources/mounts")
		}
		if dind {
			expected := &core.Pod{Spec: core.PodSpec{Containers: []core.Container{{Name: "job"}}}}
			cfg.DinD.addToPod(expected)
			if !reflect.DeepEqual(p.Spec.Containers[1], expected.Spec.Containers[1]) {
				t.Fatal("fixed daemon changed")
			}
		}
	}
}

func TestJobTemplateRetryAndServicesRejected(t *testing.T) {
	cfg := testConfig()
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
		func(c *Config) { c.DinD = dindConfig().DinD },
	} {
		other := cfg
		change(&other)
		reload, _ := New(other, kube, &rest.Config{})
		if _, err := reload.Create(context.Background(), r); status.Code(err) != codes.AlreadyExists {
			t.Fatal("accepted changed template", err)
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
}

func TestImageEnvironmentPrecedenceAndRestart(t *testing.T) {
	for _, home := range []string{"/home/node", ""} {
		cfg := dindConfig()
		p := podSpec(podName("env"), "env", cfg.Image, cfg)
		// A different current config cannot remove stored daemon defaults.
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
		for range 2 {
			defaults, err := s.executionDefaults(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if defaults["HOME"] != home || defaults["PATH"] != "/image/bin" || defaults["DOCKER_HOST"] != dockerHost || defaults["TMPDIR"] != "/shared/tmp" || defaults["IMAGE_ONLY"] != "one=two\nthree" {
				t.Fatal("defaults lost")
			}
			r := &pb.ExecRequest{Command: []string{"/usr/bin/env", "-0"}, Workdir: "/workspace", Env: map[string]string{"HOME": "", "PATH": "/runner/bin", "INPUT_A-B": "runner", "EMPTY": ""}}
			args, err := commandArgsWithDefaults(r, defaults)
			if err != nil {
				t.Fatal(err)
			}
			args[8] = t.TempDir() // substitute an allowed cwd only after argv construction
			data, err := exec.Command(args[0], args[1:]...).Output()
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
			cfg := testConfig()
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
		cfg := testConfig()
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

func TestEmptyEnvironmentAndInvalidExec(t *testing.T) {
	cfg := testConfig()
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
	for _, r := range []*pb.ExecRequest{
		{Command: []string{"env"}, User: ptr("root")},
		{Command: []string{"env"}, Env: map[string]string{"VALUE": "secret\x00invalid"}},
		{Command: []string{"env"}, Workdir: "/outside"}, {},
	} {
		r.EnvironmentId = p.Name
		if err := s.Exec(r, &execCapture{ctx: context.Background()}); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid Exec accepted", err)
		}
	}
	if calls != 1 {
		t.Fatal("invalid request executed environment probe")
	}
}

func TestStartImageEnvironment(t *testing.T) {
	cfg := testConfig()
	p := podSpec(podName("start"), "start", cfg.Image, cfg)
	p.Status.Phase = core.PodRunning
	p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "job", Ready: true}}
	s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
	s.execFn = func(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) error {
		_, err := io.WriteString(out, "PATH=/image/bin\x00HOME=/root\x00")
		return err
	}
	stream := &startCapture{ctx: context.Background()}
	if err := s.Start(&pb.StartRequest{EnvironmentId: p.Name}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.out) != 1 || stream.out[0].GetStartComplete().ImageEnv["HOME"] != "/root" {
		t.Fatal("missing Start env")
	}
}
