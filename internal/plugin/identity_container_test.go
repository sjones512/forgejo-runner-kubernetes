package plugin

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const identityNodeImage = "node@sha256:367679cf9792759492a486e4aa4b421764d71a9546a6dae8aab81a99eb797b3e"

func identityDocker(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"--context", "default"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local Docker diagnostic failed: %v\n%s", err, out)
	}
	return out
}

// Explicit opt-in: local standard images only, never cluster/application tests.
// Docker --user models runtime-resolved image credentials, NOT proof that a real
// kubelet honors an image's USER. Pod-field tests prove we leave those unset;
// real named/numeric image-USER acceptance remains the separate cluster handoff.
func TestIdentityContainers(t *testing.T) {
	if os.Getenv("EXECUTOR_IDENTITY_CONTAINER_TESTS") != "1" {
		t.Skip("opt-in local Docker identity diagnostics")
	}
	docker := func(args ...string) []byte { return identityDocker(t, args...) }
	if !strings.HasPrefix(strings.TrimSpace(string(docker("context", "inspect", "default", "--format", "{{.Endpoints.docker.Host}}"))), "unix://") {
		t.Fatal("requires local Unix Docker endpoint")
	}
	docker("pull", testPermissionsImage)
	docker("pull", identityNodeImage)
	probe, err := filepath.Abs("../../examples/homedir-probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, profile, user, home, expect string }{
		{"legacy", profileFixed, "10001", "/root", "missing-passwd"},
		{"native-root", profileImage, "", "/root", "known-passwd"},
		{"native-named", profileImage, "node", "/home/node", "known-passwd"},
		{"native-numeric-account", profileImage, "1000:1000", "/home/node", "known-passwd"},
		{"native-unregistered", profileImage, "23456:34567", "/shared/workdir", "missing-passwd"},
		{"ci-root", profileImageCI, "", "/root", "known-passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := func(args ...string) []byte { return identityDocker(t, args...) }
			cfg := dindConfig()
			cfg.SecurityProfile = tc.profile
			if tc.profile != profileFixed {
				cfg.PermissionsImage = testPermissionsImage
			}
			p := podSpec(podName(tc.name), tc.name, identityNodeImage, cfg)
			vol := strings.TrimSpace(string(docker("volume", "create")))
			sockvol := strings.TrimSpace(string(docker("volume", "create")))
			t.Cleanup(func() { docker("volume", "rm", vol, sockvol) })
			mounts := []string{"--mount", "type=volume,src=" + vol + ",dst=/shared", "--mount", "type=volume,src=" + vol + ",dst=/workspace", "--mount", "type=volume,src=" + sockvol + ",dst=" + dockerSocketDir}
			common := []string{"--network", "none", "--cpus", "1", "--memory", "256m", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--group-add", "10001"}
			if tc.profile != profileFixed {
				h := p.Spec.InitContainers[0]
				args := append([]string{"run", "--rm"}, common...)
				args = append(args, "--user", "0:10001")
				args = append(args, mounts...)
				args = append(args, "--entrypoint", h.Command[0], h.Image)
				args = append(args, h.Command[1:]...)
				args = append(args, h.Args...)
				docker(args...)
			} else {
				// Emulate existing kubelet emptyDir+fsGroup root mode, not a new
				// world-write policy. Legacy has no permission helper.
				args := append([]string{"run", "--rm"}, common...)
				args = append(args, mounts...)
				args = append(args, "--entrypoint", "/bin/sh", testPermissionsImage, "-ec", "chgrp 10001 /shared /run/forgejo-docker; chmod 2777 /shared /run/forgejo-docker")
				docker(args...)
				args = append([]string{"run", "--rm"}, common...)
				args = append(args, "--user", "10001")
				args = append(args, mounts...)
				args = append(args, "--entrypoint", "/bin/sh", identityNodeImage, "-ec", "mkdir -p /shared/act /shared/toolcache /shared/workdir /shared/tmp")
				docker(args...)
			}
			// Fake daemon socket, not dockerd, privileged execution or host socket.
			args := append([]string{"run", "-d", "--rm"}, common...)
			args = append(args, mounts...)
			args = append(args, "--entrypoint", "/usr/local/bin/node", identityNodeImage, "-e", `const fs=require('fs');require('net').createServer(c=>c.end('ok')).listen('/run/forgejo-docker/docker.sock',()=>fs.chmodSync('/run/forgejo-docker/docker.sock',0o660));`)
			server := strings.TrimSpace(string(docker(args...)))
			t.Cleanup(func() { docker("rm", "-f", server) })
			docker("exec", server, "/bin/sh", "-ec", `for i in 1 2 3 4 5; do [ -S /run/forgejo-docker/docker.sock ] && [ "$(stat -c %a /run/forgejo-docker/docker.sock)" = 660 ] && exit 0; sleep 1; done; exit 1`)
			s, _ := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
			s.execFn = func(ctx context.Context, _ string, argv []string, _ io.Reader, out, stderr io.Writer) error {
				args := append([]string{"--context", "default", "run", "--rm"}, common...)
				if tc.user != "" {
					args = append(args, "--user", tc.user)
				}
				for _, cap := range p.Spec.Containers[0].SecurityContext.Capabilities.Add {
					args = append(args, "--cap-add", string(cap))
				}
				args = append(args, mounts...)
				args = append(args, "--mount", "type=bind,src="+probe+",dst=/probe.cjs,readonly", "-e", "HOME="+tc.home, "-e", "PATH=/image-tool/bin:/usr/local/bin:/usr/bin:/bin", "-e", "IMAGE_VALUE=preserved", "--entrypoint", argv[0], identityNodeImage)
				args = append(args, argv[1:]...)
				cmd := exec.CommandContext(ctx, "docker", args...)
				cmd.Stdout = out
				cmd.Stderr = stderr
				return cmd.Run()
			}
			defaults, err := s.executionDefaults(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if tc.profile != profileFixed && (defaults["HOME"] != tc.home || defaults["PATH"] != "/image-tool/bin:/usr/local/bin:/usr/bin:/bin" || defaults["IMAGE_VALUE"] != "preserved") {
				t.Fatal("image env not preserved")
			}
			// Full production argv (including cwd shell and both env invocations).
			r := &pb.ExecRequest{Command: []string{"/usr/local/bin/node", "/probe.cjs", tc.expect}, Workdir: "/workspace"}
			argv, err := commandArgsWithDefaults(r, defaults)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr strings.Builder
			if err := s.execPod(context.Background(), p.Name, argv, nil, &stdout, &stderr); err != nil {
				t.Fatalf("homedir diagnostic: %v %s %s", err, stdout.String(), stderr.String())
			}
			t.Log(stdout.String())
			if tc.profile == profileFixed && !strings.Contains(stdout.String(), `"home": "/shared/workdir"`) {
				t.Fatal("legacy fallback changed")
			}
			if !strings.Contains(stdout.String(), `"libuv": "1.52.1"`) {
				t.Fatal("unexpected libuv")
			}
			ioTest := `const a=require('assert/strict'),fs=require('fs'),net=require('net');fs.writeFileSync(process.env.HOME+'/ci-probe','ok');fs.writeFileSync('/workspace/test-'+process.geteuid(),'ok');a.equal(fs.readFileSync('/shared/test-'+process.geteuid(),'utf8'),'ok');fs.writeFileSync('/shared/other-'+process.geteuid(),'ok');a.equal(fs.readFileSync('/workspace/other-'+process.geteuid(),'utf8'),'ok');a.equal(fs.statSync('/shared').mode&0o7777,MODE);const c=net.connect(process.env.DOCKER_HOST.slice(7));let v='';c.on('data',d=>v+=d);c.on('end',()=>a.equal(v,'ok'));c.on('error',e=>{throw e});c.setTimeout(5000,()=>{c.destroy();process.exitCode=1});`
			mode := "0o2770"
			if tc.profile == profileFixed {
				mode = "0o2777"
			}
			ioTest = strings.ReplaceAll(ioTest, "MODE", mode)
			r.Command = []string{"/usr/local/bin/node", "-e", ioTest}
			argv, err = commandArgsWithDefaults(r, defaults)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(argv, "DOCKER_HOST="+dockerHost) {
				t.Fatal("Docker default absent")
			}
			stdout.Reset()
			stderr.Reset()
			if err := s.execPod(context.Background(), p.Name, argv, nil, &stdout, &stderr); err != nil {
				t.Fatalf("workspace/socket: %v %s", err, stderr.String())
			}
			if p.Spec.Containers[0].SecurityContext.Privileged != nil || p.Spec.SecurityContext.FSGroup == nil || *p.Spec.SecurityContext.FSGroup != 10001 {
				t.Fatal("socket required privilege or lost fsGroup")
			}
			for _, v := range p.Spec.Volumes {
				if v.EmptyDir == nil || v.HostPath != nil {
					t.Fatal("volume boundary")
				}
			}
			if p.Spec.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
				t.Fatal("seccomp lost")
			}
		})
	}
	t.Run("bounded-package-setup", func(t *testing.T) {
		args := []string{"run", "--rm", "--cpus", "1", "--memory", "256m", "--security-opt", "no-new-privileges", "--cap-drop", "ALL"}
		for _, cap := range ciCapabilities() {
			args = append(args, "--cap-add", string(cap))
		}
		// Public apt mirrors only, no app checkout or image build. Logs remain in
		// this disposable container, not dumped as environment diagnostics.
		args = append(args, "--entrypoint", "/bin/sh", identityNodeImage, "-ec", `
apt-get update >/tmp/apt-log 2>&1
apt-get install --no-install-recommends -y hello >>/tmp/apt-log 2>&1
hello
 touch /tmp/ownership; chown node:node /tmp/ownership; chmod 0640 /tmp/ownership
setpriv --reuid=node --regid=node --init-groups id
chroot / /usr/bin/id
node -e 'const c=require("child_process").spawn("/bin/sleep",["10"],{uid:1000,gid:1000});c.on("spawn",()=>process.kill(c.pid,"SIGTERM"));c.on("exit",(_,signal)=>{if(signal!=="SIGTERM")process.exitCode=1})'
grep -E "^(CapEff|CapBnd|NoNewPrivs|Seccomp):" /proc/self/status
`)
		out := string(identityDocker(t, args...))
		if !strings.Contains(out, "Hello, world!") || !strings.Contains(out, "NoNewPrivs:\t1") || !strings.Contains(out, "Seccomp:\t2") {
			t.Fatal("ordinary package setup or security flags failed")
		}
		t.Log(out)
	})
}
