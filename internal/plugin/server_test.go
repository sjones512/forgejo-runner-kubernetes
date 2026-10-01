package plugin

import (
	"context"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/api/resource"
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
	p := podSpec(id, "a job", c.Image, c)
	if p.Spec.RestartPolicy != core.RestartPolicyNever || *p.Spec.AutomountServiceAccountToken || p.Spec.NodeSelector["kubernetes.io/arch"] != "arm64" {
		t.Fatalf("isolation/scheduling: %+v", p.Spec)
	}
	if p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation == nil || *p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation enabled")
	}
	if p.Spec.Containers[0].SecurityContext.RunAsUser != nil || p.Spec.Containers[0].SecurityContext.RunAsNonRoot != nil || p.Spec.Containers[0].SecurityContext.Capabilities != nil || p.Spec.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod security context")
	}
	if len(p.Spec.Containers[0].Resources.Limits) != 3 || len(p.Spec.Containers[0].Resources.Requests) != 3 {
		t.Fatal("unbounded resources")
	}
	if p.Spec.Volumes[0].EmptyDir == nil || p.Spec.Volumes[0].EmptyDir.SizeLimit == nil || len(p.Spec.Containers[0].VolumeMounts) != 2 {
		t.Fatal("workspace not ephemeral")
	}
	storageRequest := p.Spec.Containers[0].Resources.Requests[core.ResourceEphemeralStorage]
	storageLimit := p.Spec.Containers[0].Resources.Limits[core.ResourceEphemeralStorage]
	if p.Spec.Volumes[0].EmptyDir.SizeLimit.Cmp(resource.MustParse("1Gi")) != 0 ||
		storageRequest.Cmp(resource.MustParse("256Mi")) != 0 || storageLimit.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatal("default storage budget changed")
	}
	if strings.Contains(p.Annotations["forgejo.org/runner-name"], "secret") {
		t.Fatal("unexpected annotation")
	}
	if podName("a job") != id || podName("another job") == id || !validID.MatchString(id) {
		t.Fatal("invalid deterministic name")
	}
}
func TestJobAppArmorPodSpec(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		wantType      core.AppArmorProfileType
		wantName      string
	}{
		{"unset", "", "", ""},
		{"runtime default", "runtime-default", core.AppArmorProfileTypeRuntimeDefault, ""},
		{"unconfined", "unconfined", core.AppArmorProfileTypeUnconfined, ""},
		{"localhost", "localhost:ci-jobs", core.AppArmorProfileTypeLocalhost, "ci-jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.AppArmorProfile = tc.setting
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			pod := podSpec(podName("app-armor"), "app-armor", cfg.Image, cfg)
			pc := pod.Spec.SecurityContext
			container := pod.Spec.Containers[0]
			security := container.SecurityContext
			if pc.AppArmorProfile != nil {
				t.Fatal("AppArmor must be set on the job container, not the Pod")
			}
			if tc.wantType == "" {
				if security.AppArmorProfile != nil {
					t.Fatalf("unset AppArmor emitted a profile: %+v", security.AppArmorProfile)
				}
				encoded, err := json.Marshal(pod)
				if err != nil || strings.Contains(string(encoded), "appArmorProfile") {
					t.Fatalf("unset AppArmor must not appear in serialized Pod: %s (%v)", encoded, err)
				}
			} else {
				profile := security.AppArmorProfile
				if profile == nil || profile.Type != tc.wantType {
					t.Fatalf("AppArmor type: %+v, want %s", profile, tc.wantType)
				}
				if tc.wantName == "" && profile.LocalhostProfile != nil || tc.wantName != "" && (profile.LocalhostProfile == nil || *profile.LocalhostProfile != tc.wantName) {
					t.Fatalf("AppArmor localhost profile: %+v, want %q", profile, tc.wantName)
				}
			}
			if pc.SeccompProfile == nil || pc.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault || security.SeccompProfile != nil ||
				pc.RunAsNonRoot != nil || pc.RunAsUser != nil || pc.RunAsGroup != nil || security.RunAsNonRoot != nil || security.RunAsUser != nil || security.RunAsGroup != nil || pc.FSGroup != nil ||
				security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Privileged == nil || *security.Privileged ||
				security.Capabilities != nil ||
				pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Fatalf("other security controls changed: pod=%+v container=%+v", pc, security)
			}
		})
	}
}

func TestJobAppArmorValidation(t *testing.T) {
	for _, tc := range []struct {
		value, want string
	}{
		{"", ""},
		{"runtime-default", ""},
		{"unconfined", ""},
		{"localhost:ci-jobs", ""},
		{"localhost:", "JOB_APPARMOR_PROFILE"},
		{"localhost:  ", "JOB_APPARMOR_PROFILE"},
		{"localhost: ci-jobs", "JOB_APPARMOR_PROFILE"},
		{"localhost:ci-jobs ", "JOB_APPARMOR_PROFILE"},
		{"localhost:ci\x00jobs", "JOB_APPARMOR_PROFILE"},
		{"localhost:ci\njobs", "JOB_APPARMOR_PROFILE"},
		{"RuntimeDefault", "JOB_APPARMOR_PROFILE"},
		{"runtime-default:ci-jobs", "JOB_APPARMOR_PROFILE"},
		{"unconfined:ci-jobs", "JOB_APPARMOR_PROFILE"},
		{"localhost", "JOB_APPARMOR_PROFILE"},
		{"bogus", "JOB_APPARMOR_PROFILE"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := testConfig()
			cfg.AppArmorProfile = tc.value
			_, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{Host: "https://example.invalid"})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("setting %q: want error containing %q, got %v", tc.value, tc.want, err)
			}
		})
	}
}

func TestConfiguredStoragePod(t *testing.T) {
	cfg := testConfig()
	cfg.WorkspaceSizeLimit = "5Gi"
	cfg.EphemeralStorageRequest = "3Gi"
	cfg.EphemeralStorageLimit = "7Gi"
	client := fake.NewSimpleClientset()
	s, err := New(cfg, client, &rest.Config{Host: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), &pb.CreateRequest{Name: "storage", Image: cfg.Image}); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods: %d, %v", len(pods.Items), err)
	}
	pod := pods.Items[0]
	storageRequest := pod.Spec.Containers[0].Resources.Requests[core.ResourceEphemeralStorage]
	storageLimit := pod.Spec.Containers[0].Resources.Limits[core.ResourceEphemeralStorage]
	if pod.Spec.Volumes[0].EmptyDir.SizeLimit.Cmp(resource.MustParse("5Gi")) != 0 ||
		storageRequest.Cmp(resource.MustParse("3Gi")) != 0 || storageLimit.Cmp(resource.MustParse("7Gi")) != 0 {
		t.Fatalf("configured storage not reflected in Pod: %+v", pod.Spec)
	}
}

func TestStorageConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, request, limit, want string
	}{
		{"default", "", "", "", ""},
		{"custom", "5Gi", "3Gi", "7Gi", ""},
		{"request equals limit", "1Gi", "2Gi", "2Gi", ""},
		{"invalid workspace", "bogus", "", "", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"negative workspace", "-1Gi", "", "", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"zero workspace", "0", "", "", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"invalid request", "", "1GiB", "", "JOB_EPHEMERAL_STORAGE_REQUEST"},
		{"zero request", "", "0", "", "JOB_EPHEMERAL_STORAGE_REQUEST"},
		{"invalid limit", "", "", "2Gii", "JOB_EPHEMERAL_STORAGE_LIMIT"},
		{"negative limit", "", "", "-1Gi", "JOB_EPHEMERAL_STORAGE_LIMIT"},
		{"workspace exceeds default limit", "5Gi", "", "", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"workspace equals limit", "2Gi", "", "2Gi", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"request exceeds limit", "", "3Gi", "2Gi", "JOB_EPHEMERAL_STORAGE_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.WorkspaceSizeLimit, cfg.EphemeralStorageRequest, cfg.EphemeralStorageLimit = tc.workspace, tc.request, tc.limit
			_, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{Host: "https://example.invalid"})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCreateImageSelection(t *testing.T) {
	for _, tc := range []struct {
		name, labelArg, image, want string
	}{
		{"label supplies image", "node:24-bookworm", "", "node:24-bookworm"},
		{"container image overrides label", "node:24-bookworm", "node:26-bookworm", "node:26-bookworm"},
		{"explicit image without label", "", "node:26-bookworm", "node:26-bookworm"},
		{"operator fallback", "", "", "ubuntu:24.04"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			kube := fake.NewSimpleClientset()
			s, err := New(cfg, kube, &rest.Config{Host: "https://example.invalid"})
			if err != nil {
				t.Fatal(err)
			}
			req := &pb.CreateRequest{Name: "image-selection", Image: tc.image, LabelArg: tc.labelArg}
			if _, err := s.Create(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			p, err := kube.CoreV1().Pods(cfg.Namespace).Get(context.Background(), podName(req.Name), metav1.GetOptions{})
			if err != nil || p.Spec.Containers[0].Image != tc.want {
				t.Fatalf("image: Pod %v, error %v; want %q", p, err, tc.want)
			}
		})
	}
}

func TestCreateImageValidationAndRetry(t *testing.T) {
	cfg := testConfig()
	kube := fake.NewSimpleClientset()
	s, err := New(cfg, kube, &rest.Config{Host: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, image := range []string{" ", " node:24-bookworm", "node:24-bookworm\x00other"} {
		if _, err := s.Create(ctx, &pb.CreateRequest{Name: "bad-image", Image: image}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("accepted image %q: %v", image, err)
		}
	}
	req := &pb.CreateRequest{Name: "same-name", LabelArg: "node:24-bookworm"}
	if _, err := s.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, req); err != nil {
		t.Fatalf("same image retry: %v", err)
	}
	if _, err := s.Create(ctx, &pb.CreateRequest{Name: req.Name, LabelArg: "node:26-bookworm"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("silent retry with different image: %v", err)
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
	if !slices.Contains(lines, "HOME=/shared/workdir") {
		t.Fatalf("no writable HOME for unlisted numeric UID: %q", out)
	}
	for k, v := range r.Env {
		if !slices.Contains(lines, k+"="+v) {
			t.Fatalf("lost %q in child environment: %q", k, out)
		}
	}
	r.Env["HOME"] = "/workspace/custom-home"
	args, err = commandArgs(r)
	if err != nil {
		t.Fatal(err)
	}
	wd = slices.Index(args, "forgejo") + 1
	args[wd] = t.TempDir()
	out, err = exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(string(out), "\n")
	if !slices.Contains(lines, "HOME=/workspace/custom-home") || slices.Contains(lines, "HOME=/shared/workdir") {
		t.Fatalf("Runner HOME override not preserved: %q", out)
	}
}
