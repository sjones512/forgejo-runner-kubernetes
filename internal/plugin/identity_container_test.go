package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const identityNodeImage = "node@sha256:367679cf9792759492a486e4aa4b421764d71a9546a6dae8aab81a99eb797b3e"
const identityDinDImage = "docker.io/library/docker@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0"

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

// Local Docker models kubelet's existing emptyDir+fsGroup modes and runtime
// credentials; it does NOT prove real Kubernetes image USER/fsGroup behavior.
// Only the daemon is privileged; no host socket/path/token is mounted.
func TestIdentityContainers(t *testing.T) {
	if os.Getenv("EXECUTOR_IDENTITY_CONTAINER_TESTS") != "1" {
		t.Skip("opt-in local Docker identity diagnostics")
	}
	docker := func(args ...string) []byte { return identityDocker(t, args...) }
	if !strings.HasPrefix(strings.TrimSpace(string(docker("context", "inspect", "default", "--format", "{{.Endpoints.docker.Host}}"))), "unix://") {
		t.Fatal("requires local Unix Docker endpoint")
	}
	docker("pull", identityNodeImage)
	docker("pull", identityDinDImage)
	probe, err := filepath.Abs("../../examples/homedir-probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, dindEnabled := range []bool{false, true} {
		for _, tc := range []struct{ name, user, home, expect string }{
			{"root", "", "/root", "known-passwd"},
			{"named", "node", "/home/node", "known-passwd"},
			{"numeric-account", "1000:1000", "/home/node", "known-passwd"},
			{"arbitrary-numeric", "23456:34567", "/shared/workdir", "missing-passwd"},
		} {
			t.Run(tc.name+map[bool]string{false: "/no-dind", true: "/dind"}[dindEnabled], func(t *testing.T) {
				docker := func(args ...string) []byte { return identityDocker(t, args...) }
				cfg := testConfig()
				if dindEnabled {
					cfg.DinD = dindConfig().DinD
					cfg.DinD.Image = identityDinDImage
				}
				p := podSpec(podName(tc.name), tc.name, identityNodeImage, cfg)
				if len(p.Spec.InitContainers) != 0 {
					t.Fatal("job added init machinery")
				}
				volumes := []string{}
				for range 3 {
					volumes = append(volumes, strings.TrimSpace(string(docker("volume", "create"))))
				}
				t.Cleanup(func() { docker(append([]string{"volume", "rm"}, volumes...)...) })
				mounts := []string{"--mount", "type=volume,src=" + volumes[0] + ",dst=/shared", "--mount", "type=volume,src=" + volumes[0] + ",dst=/workspace", "--mount", "type=volume,src=" + volumes[1] + ",dst=" + dockerSocketDir}
				common := []string{"--network", "none", "--cpus", "1", "--memory", "256m", "--security-opt", "no-new-privileges"}
				if dindEnabled {
					common = append(common, "--group-add", "10001")
				}
				mode, group := "0777", "0"
				if dindEnabled {
					mode, group = "2777", "10001"
				}
				if !dindEnabled {
					mounts = mounts[:4]
				}
				// Docker named volumes start 0755/root:root, unlike Kubernetes emptyDir.
				// This disposable diagnostic setup emulates kubelet's pre-container 02777
				// roots only: no 2770 conversion, helper command or job staging directories.
				args := append([]string{"run", "--rm"}, common...)
				args = append(args, mounts...)
				args = append(args, "--entrypoint", "/bin/sh", identityNodeImage, "-ec", "chgrp "+group+" /shared; chmod "+mode+" /shared"+map[bool]string{false: "", true: "; chgrp 10001 /run/forgejo-docker; chmod 2777 /run/forgejo-docker"}[dindEnabled])
				docker(args...)

				if dindEnabled {
					daemon := p.Spec.Containers[1]
					args = []string{"run", "-d", "--network", "none", "--privileged", "--cpus", "2", "--memory", "2g", "-e", "DOCKER_TLS_CERTDIR="}
					args = append(args, mounts...)
					args = append(args, "--mount", "type=volume,src="+volumes[2]+",dst="+dockerDataRoot, daemon.Image)
					args = append(args, daemon.Args...)
					dind := strings.TrimSpace(string(docker(args...)))
					t.Cleanup(func() { docker("rm", "-f", dind) })
					docker("exec", dind, "/bin/sh", "-ec", `for i in $(seq 1 60); do docker --host=unix:///run/forgejo-docker/docker.sock info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1`)
					socket := strings.TrimSpace(string(docker("exec", dind, "stat", "-c", "%u:%g:%a", dockerSocketDir+"/docker.sock")))
					if socket != "0:10001:660" {
						t.Fatal("unexpected actual dockerd socket", socket)
					}
				}

				args = append([]string{"run", "-d"}, common...)
				if tc.user != "" {
					args = append(args, "--user", tc.user)
				}
				args = append(args, mounts...)
				args = append(args, "--mount", "type=bind,src="+probe+",dst=/probe.cjs,readonly", "-e", "HOME="+tc.home, "-e", "PATH=/image-tool/bin:/usr/local/bin:/usr/bin:/bin", "-e", "IMAGE_VALUE=preserved", "--entrypoint", p.Spec.Containers[0].Command[0], identityNodeImage)
				args = append(args, p.Spec.Containers[0].Command[1:]...)
				job := strings.TrimSpace(string(docker(args...)))
				t.Cleanup(func() { docker("rm", "-f", job) })
				docker("exec", job, "/bin/sh", "-ec", `for i in 1 2 3 4 5; do [ -d /shared/workdir ] && exit 0; sleep 1; done; exit 1`)
				s, err := New(cfg, fake.NewSimpleClientset(p), &rest.Config{})
				if err != nil {
					t.Fatal(err)
				}
				s.execFn = func(ctx context.Context, _ string, argv []string, in io.Reader, out, stderr io.Writer) error {
					args := []string{"--context", "default", "exec"}
					if in != nil {
						args = append(args, "-i")
					}
					args = append(args, job)
					args = append(args, argv...)
					cmd := exec.CommandContext(ctx, "docker", args...)
					cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
					return cmd.Run()
				}
				defaults, err := s.executionDefaults(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				if defaults["HOME"] != tc.home || defaults["PATH"] != "/image-tool/bin:/usr/local/bin:/usr/bin:/bin" || defaults["IMAGE_VALUE"] != "preserved" {
					t.Fatal("image env not preserved")
				}
				run := func(command ...string) string {
					t.Helper()
					argv, err := commandArgsWithDefaults(&pb.ExecRequest{Command: command, Workdir: "/workspace"}, defaults)
					if err != nil {
						t.Fatal(err)
					}
					var stdout, stderr strings.Builder
					if err := s.execPod(context.Background(), p.Name, argv, nil, &stdout, &stderr); err != nil {
						t.Fatalf("generated Exec argv: %v %s", err, stderr.String())
					}
					return stdout.String()
				}
				homedir := run("/usr/local/bin/node", "/probe.cjs", tc.expect)
				t.Log(homedir)
				run("/usr/local/bin/node", "-e", `const a=require('assert/strict'),fs=require('fs');fs.writeFileSync(process.env.HOME+'/ci-probe','ok');fs.writeFileSync('/workspace/test','ok');a.equal(fs.readFileSync('/shared/test','utf8'),'ok');fs.writeFileSync('/shared/other','ok');a.equal(fs.readFileSync('/workspace/other','utf8'),'ok');const s=fs.statSync('/shared');a.equal(s.mode&0o7777,parseInt(process.argv[1],8));a.equal(s.gid,Number(process.argv[2]));`, mode, group)
				if dindEnabled {
					run("/usr/local/bin/node", "-e", `const a=require('assert/strict'),http=require('http');const req=http.get({socketPath:process.env.DOCKER_HOST.slice(7),path:'/info'},r=>{a.equal(r.statusCode,200);let v='';r.on('data',d=>v+=d);r.on('end',()=>{const info=JSON.parse(v);a.equal(info.ServerVersion,'29.8.1');a.equal(info.DockerRootDir,'/var/lib/docker')})});req.on('error',e=>{throw e});req.setTimeout(5000,()=>req.destroy(new Error('socket timeout')));`)
				}
				// Explicit empty HOME remains empty, not silently replaced.
				emptyArgs, err := commandArgsWithDefaults(&pb.ExecRequest{Command: []string{"/usr/local/bin/node", "/probe.cjs", tc.expect}, Env: map[string]string{"HOME": ""}}, defaults)
				if err != nil {
					t.Fatal(err)
				}
				var emptyOut strings.Builder
				if err := s.execPod(context.Background(), p.Name, emptyArgs, nil, &emptyOut, io.Discard); err != nil || !strings.Contains(emptyOut.String(), `"home": ""`) {
					t.Fatal("empty HOME/NSS semantics", err)
				}
				// Missing inherited HOME gets the shared writable fallback, no NSS fix.
				nativeExec := s.execFn
				s.execFn = func(ctx context.Context, id string, argv []string, in io.Reader, out, stderr io.Writer) error {
					if strings.Join(argv, " ") == "/usr/bin/env -0" {
						argv = []string{"/usr/bin/env", "-u", "HOME", "/usr/bin/env", "-0"}
					}
					return nativeExec(ctx, id, argv, in, out, stderr)
				}
				absent, err := s.executionDefaults(context.Background(), p)
				s.execFn = nativeExec
				if err != nil || absent["HOME"] != "/shared/workdir" {
					t.Fatal("absent HOME fallback", err)
				}
				fallbackArgs, err := commandArgsWithDefaults(&pb.ExecRequest{Command: []string{"/usr/local/bin/node", "/probe.cjs", tc.expect}}, absent)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.execPod(context.Background(), p.Name, fallbackArgs, nil, io.Discard, io.Discard); err != nil {
					t.Fatal("fallback HOME/NSS", err)
				}

				// Exercise the actual streaming CopyIn/CopyOut RPCs, not just tar argv.
				listener := bufconn.Listen(1024 * 1024)
				g := NewGRPCServer()
				pb.RegisterBackendPluginServer(g, s)
				go g.Serve(listener)
				t.Cleanup(g.Stop)
				conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				client := pb.NewBackendPluginClient(conn)
				for _, dest := range []string{"/shared/act/copy", "/workspace/copy"} {
					var archive bytes.Buffer
					tw := tar.NewWriter(&archive)
					payload := []byte("identity transfer\n")
					if err := tw.WriteHeader(&tar.Header{Name: "payload", Mode: 0600, Uid: 9876, Gid: 8765, Size: int64(len(payload))}); err != nil {
						t.Fatal(err)
					}
					if _, err := tw.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := tw.Close(); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					upload, err := client.CopyIn(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := upload.Send(&pb.CopyInChunk{EnvironmentId: &p.Name, DestPath: &dest}); err != nil {
						t.Fatal(err)
					}
					if err := upload.Send(&pb.CopyInChunk{Data: archive.Bytes()}); err != nil {
						t.Fatal(err)
					}
					if _, err := upload.CloseAndRecv(); err != nil {
						t.Fatal(err)
					}
					run("/usr/local/bin/node", "-e", `const a=require('assert/strict'),fs=require('fs');a.equal(fs.statSync(process.argv[1]).uid,process.geteuid());a.equal(fs.readFileSync(process.argv[1],'utf8'),'identity transfer\n')`, dest+"/payload")
					alias := "/shared" + strings.TrimPrefix(dest, "/workspace")
					if strings.HasPrefix(dest, "/shared") {
						alias = "/workspace" + strings.TrimPrefix(dest, "/shared")
					}
					download, err := client.CopyOut(ctx, &pb.CopyOutRequest{EnvironmentId: p.Name, SrcPath: alias + "/payload"})
					if err != nil {
						t.Fatal(err)
					}
					archive.Reset()
					for {
						chunk, err := download.Recv()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						archive.Write(chunk.Data)
					}
					tr := tar.NewReader(&archive)
					hdr, err := tr.Next()
					if err != nil || hdr.Name != "payload" {
						t.Fatal("CopyOut header", hdr, err)
					}
					got, err := io.ReadAll(tr)
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatal("CopyOut payload", err)
					}
				}
				t.Log("ordinary runtime: HOME/NSS, both aliases and CopyIn/CopyOut passed; dind socket tested when enabled")
			})
		}
	}
	t.Run("ordinary-root-package-setup", func(t *testing.T) {
		args := []string{"run", "--rm", "--cpus", "1", "--memory", "256m", "--security-opt", "no-new-privileges"}
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
