package plugin

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	kexec "k8s.io/client-go/util/exec"
)

func testConfig() Config {
	return Config{Namespace: "jobs", Image: "ubuntu:24.04", Arch: "arm64", StartupTimeout: time.Minute, CleanupTimeout: time.Second}
}
func TestPodSpec(t *testing.T) {
	c := testConfig()
	id := podName("a job")
	p := podSpec(id, "a job", c)
	if p.Spec.RestartPolicy != core.RestartPolicyNever || *p.Spec.AutomountServiceAccountToken || p.Spec.NodeSelector["kubernetes.io/arch"] != "arm64" {
		t.Fatalf("isolation/scheduling: %+v", p.Spec)
	}
	if p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation == nil || *p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation enabled")
	}
	if *p.Spec.SecurityContext.RunAsUser == 0 || p.Spec.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod security context")
	}
	if len(p.Spec.Containers[0].Resources.Limits) != 3 || len(p.Spec.Containers[0].Resources.Requests) != 3 {
		t.Fatal("unbounded resources")
	}
	if p.Spec.Volumes[0].EmptyDir == nil || p.Spec.Volumes[0].EmptyDir.SizeLimit == nil || len(p.Spec.Containers[0].VolumeMounts) != 2 {
		t.Fatal("workspace not ephemeral")
	}
	if strings.Contains(p.Annotations["forgejo.org/runner-name"], "secret") {
		t.Fatal("unexpected annotation")
	}
	if podName("a job") != id || podName("another job") == id || !validID.MatchString(id) {
		t.Fatal("invalid deterministic name")
	}
}
func TestLifecycleCreateRemove(t *testing.T) {
	ctx := context.Background()
	c := testConfig()
	client := fake.NewSimpleClientset()
	s, err := New(c, client, &rest.Config{Host: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	req := &pb.CreateRequest{Name: "job 1", Image: c.Image}
	res, err := s.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Arch != "aarch64" || res.ActPath != "/shared/act" {
		t.Fatal(res)
	}
	// Repeated create must not create another Pod.
	_, err = s.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := client.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatalf("pods: %d", len(list.Items))
	}
	for i := 0; i < 2; i++ {
		if _, err = s.Remove(ctx, &pb.RemoveRequest{EnvironmentId: res.EnvironmentId}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ = client.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatal("pod leaked")
	}
	if _, err = s.Create(ctx, &pb.CreateRequest{Name: "bad", Image: c.Image, Services: []*pb.ServiceContainer{{Name: "db"}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("service should fail closed: %v", err)
	}
}
func TestCleanupFailure(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	client := fake.NewSimpleClientset()
	s, err := New(cfg, client, &rest.Config{Host: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(ctx, &pb.CreateRequest{Name: "delete-error", Image: cfg.Image})
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
	_, err = s.Remove(ctx, &pb.RemoveRequest{EnvironmentId: created.EnvironmentId})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected cleanup error, got %v", err)
	}
}

func TestExitTranslation(t *testing.T) {
	for _, tc := range []struct {
		err     error
		code    int32
		message string
	}{
		{nil, 0, ""},
		{kexec.CodeExitError{Err: errors.New("exit status 23"), Code: 23}, 23, ""},
		{errors.New("API interrupted"), 0, "API interrupted"},
	} {
		code, msg := exitResult(tc.err)
		if code != tc.code || msg != tc.message {
			t.Fatalf("%v: code %d message %q", tc.err, code, msg)
		}
	}
}

func TestExecTranslation(t *testing.T) {
	r := &pb.ExecRequest{Command: []string{"sh", "-e", "script.sh"}, Workdir: "/workspace/org/repo", Env: map[string]string{"MY_VAR": "a b; $(id)", "PATH": "/bin"}}
	args, err := commandArgs(r)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "|")
	if !strings.Contains(joined, "MY_VAR=a b; $(id)") || !strings.Contains(joined, "forgejo|/workspace/org/repo|PATH=") || !strings.HasSuffix(joined, "|sh|-e|script.sh") {
		t.Fatal(args)
	}
	if !safePath("/shared/act/") {
		t.Fatal("Runner CopyIn uses trailing slash")
	}
	for _, p := range []string{"/etc", "/shared/../etc", "/workspace/../etc", "/workspace/abc/../../etc"} {
		if safePath(p) {
			t.Fatalf("accepted %q", p)
		}
	}
	r.Workdir = "/etc"
	if _, err := commandArgs(r); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	r.Workdir = "/shared"
	for _, env := range []map[string]string{
		{"": "value"}, {"A=B": "value"}, {"BAD\x00NAME": "value"}, {"GOOD": "bad\x00value"},
	} {
		r.Env = env
		if _, err := commandArgs(r); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("env %q: expected InvalidArgument, got %v", env, err)
		}
	}
	r.Env = nil
	r.User = new(string)
	*r.User = "root"
	if _, err := commandArgs(r); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
}

func TestExecEnvironmentArgv(t *testing.T) {
	r := &pb.ExecRequest{Command: []string{"/usr/bin/env"}, Workdir: "/workspace", Env: map[string]string{
		"INPUT_FETCH-DEPTH": "0", "INPUT_NODE-VERSION-FILE": ".node-version", "A.B": "dot", "--help": "not an env option", "VALUE": "a=b",
	}}
	args, err := commandArgs(r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args[:3], []string{"/usr/bin/env", "-i", "--"}) {
		t.Fatalf("env option boundary: %q", args[:3])
	}
	// Execute the constructed argv through both env calls and the cwd shell.
	// Substitute only the workdir to avoid writing /workspace on the test host.
	wd := slices.Index(args, "forgejo") + 1
	if wd == 0 || wd >= len(args) {
		t.Fatal(args)
	}
	args[wd] = t.TempDir()
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	for k, v := range r.Env {
		if !slices.Contains(lines, k+"="+v) {
			t.Fatalf("lost %q in child environment: %q", k, out)
		}
	}
}
