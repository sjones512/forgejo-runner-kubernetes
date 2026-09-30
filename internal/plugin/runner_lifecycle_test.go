package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// Runner v13.2.0's runStepExecutor calls CopyIn (step files), Exec, then
// CopyOut for env/state/output/summary/path. GetContainerArchive pipes the
// tar stream to its reader; closing the reader can cancel a still-active
// CopyOut RPC even though the job and environment are still alive.
type observedRunnerServer struct {
	*Server
	firstCopyOutDone chan struct{}
	once             sync.Once
}

func (s *observedRunnerServer) CopyOut(r *pb.CopyOutRequest, stream grpc.ServerStreamingServer[pb.CopyOutChunk]) error {
	err := s.Server.CopyOut(r, stream)
	s.once.Do(func() { close(s.firstCopyOutDone) })
	return err
}

func TestRunnerEnvironmentLivesThroughStepFileCommands(t *testing.T) {
	t.Run("single container", func(t *testing.T) { testRunnerStepLifecycle(t, testConfig()) })
	t.Run("fixed DinD sidecar", func(t *testing.T) { testRunnerStepLifecycle(t, dindConfig()) })
}

func testRunnerStepLifecycle(t *testing.T, cfg Config) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	s, err := New(cfg, kube, &rest.Config{Host: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	var copyOutCount atomic.Int32
	s.execFn = func(ctx context.Context, _ string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "tar -xpf"):
			_, err := io.Copy(io.Discard, stdin)
			return err
		case strings.Contains(cmd, "tar -cf"):
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			name := args[len(args)-1] // tar -C <dir> -- <basename>
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 0}); err != nil {
				return err
			}
			if err := tw.Close(); err != nil {
				return err
			}
			if _, err := stdout.Write(b.Bytes()); err != nil {
				return err
			}
			if copyOutCount.Add(1) == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		default:
			if stdout != nil {
				_, err := stdout.Write([]byte("hello from Kubernetes\n"))
				return err
			}
			return nil
		}
	}
	obs := &observedRunnerServer{Server: s, firstCopyOutDone: make(chan struct{})}
	l := bufconn.Listen(1 << 20)
	grpcServer := NewGRPCServer()
	pb.RegisterBackendPluginServer(grpcServer, obs)
	go grpcServer.Serve(l)
	defer grpcServer.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := pb.NewBackendPluginClient(conn)
	created, err := p.Create(ctx, &pb.CreateRequest{Name: "task-123_WORKFLOW-abc_JOB-hello", LabelArg: cfg.Image})
	if err != nil {
		t.Fatal(err)
	}
	id := created.EnvironmentId
	pod, err := kube.CoreV1().Pods(cfg.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = core.PodRunning
	pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: "job", Ready: true}}
	if cfg.DinD.Enabled {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, core.ContainerStatus{Name: "dind", Ready: true})
	}
	if _, err = kube.CoreV1().Pods(cfg.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	started, err := p.Start(ctx, &pb.StartRequest{EnvironmentId: id})
	if err != nil {
		t.Fatal(err)
	}
	if message, err := started.Recv(); err != nil || message.GetStartComplete() == nil {
		t.Fatalf("start: %v %v", message, err)
	}
	// Runner copies event.json/envs.txt, then per-step file-command placeholders.
	// With no explicit shell, interpretShell probes for bash via rc.sh: another
	// CopyIn + Exec, before it uploads and executes the actual step script.
	copyIn := func(dest string) {
		t.Helper()
		stream, err := p.CopyIn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(&pb.CopyInChunk{EnvironmentId: &id, DestPath: &dest}); err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(&pb.CopyInChunk{Data: []byte("tar payload")}); err != nil {
			t.Fatal(err)
		}
		if _, err = stream.CloseAndRecv(); err != nil {
			t.Fatal(err)
		}
	}
	copyIn("/shared/act/")
	copyIn("/shared/act")
	copyIn("/shared/act") // rc.sh shell-detection script
	probe, err := p.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: []string{"sh", "/shared/act/probe.sh"}, Workdir: "/workspace/owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = probe.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	copyIn("/shared/act") // first user script
	execute := func() {
		t.Helper()
		stream, err := p.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/shared/act/workflow/step.sh"}, Workdir: "/workspace/owner/repo"})
		if err != nil {
			t.Fatal(err)
		}
		var complete bool
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if msg.GetExecComplete() != nil {
				complete = true
			}
		}
		if !complete {
			t.Fatal("Exec did not complete")
		}
	}
	execute()
	// Mirror Runner v13.2.0 GetContainerArchive: a gRPC Recv goroutine
	// copies the tar stream to an io.Pipe and cancels ONLY this CopyOut
	// context if its reader closes early. ParseEnvFile reads the first tar
	// entry but not the full archive padding/trailer, then closes the pipe.
	streamCtx, cancel := context.WithCancel(ctx)
	first, err := p.CopyOut(streamCtx, &pb.CopyOutRequest{EnvironmentId: id, SrcPath: "/shared/act/workflow/envs.txt"})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		for {
			chunk, err := first.Recv()
			if errors.Is(err, io.EOF) {
				_ = pw.Close()
				return
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err = pw.Write(chunk.Data); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()
	archive := tar.NewReader(pr)
	if _, err = archive.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(archive); err != nil {
		t.Fatal(err)
	}
	_ = pr.Close()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Runner CopyOut reader did not finish")
	}
	select {
	case <-obs.firstCopyOutDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled CopyOut did not finish")
	}
	if _, err = kube.CoreV1().Pods(cfg.Namespace).Get(ctx, id, metav1.GetOptions{}); err != nil {
		t.Fatalf("environment deleted before Runner Remove (after first CopyOut): %v", err)
	}
	for _, file := range []string{"statecmd.txt", "outputcmd.txt", "SUMMARY.md", "pathcmd.txt"} {
		stream, err := p.CopyOut(ctx, &pb.CopyOutRequest{EnvironmentId: id, SrcPath: "/shared/act/workflow/" + file})
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err = stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("CopyOut(%s) before Remove: %v", file, err)
			}
		}
	}
	copyIn("/shared/act")
	execute() // next step still uses the same environment ID
	if _, err = p.Remove(ctx, &pb.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err = kube.CoreV1().Pods(cfg.Namespace).Get(ctx, id, metav1.GetOptions{}); err == nil {
		t.Fatal("Remove did not delete Pod")
	}
}
