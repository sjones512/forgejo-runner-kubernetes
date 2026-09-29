package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kexec "k8s.io/client-go/util/exec"
)

func TestStreamingProtocol(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	s, e := New(cfg, fake.NewSimpleClientset(), &rest.Config{Host: "https://example.invalid"})
	if e != nil {
		t.Fatal(e)
	}
	var uploaded []byte
	s.execFn = func(_ context.Context, _ string, args []string, in io.Reader, out, errout io.Writer) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "tar -xpf"):
			uploaded, e = io.ReadAll(in)
			return e
		case strings.Contains(cmd, "tar -cf"):
			tw := tar.NewWriter(out)
			data := []byte("KEY=value\n")
			if e := tw.WriteHeader(&tar.Header{Name: "env", Mode: 0600, Size: int64(len(data))}); e != nil {
				return e
			}
			if _, e := tw.Write(data); e != nil {
				return e
			}
			return tw.Close()
		default:
			out.Write([]byte("hello\n"))
			errout.Write([]byte("warning\n"))
			return kexec.CodeExitError{Err: errors.New("exit status 42"), Code: 42}
		}
	}
	listener := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer()
	pb.RegisterBackendPluginServer(g, s)
	go g.Serve(listener)
	defer g.Stop()
	conn, e := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	client := pb.NewBackendPluginClient(conn)
	create, e := client.Create(ctx, &pb.CreateRequest{Name: "job", LabelArg: cfg.Image})
	if e != nil {
		t.Fatal(e)
	}
	id := create.EnvironmentId
	copyIn, e := client.CopyIn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dest := "/shared/act"
	if e = copyIn.Send(&pb.CopyInChunk{EnvironmentId: &id, DestPath: &dest}); e != nil {
		t.Fatal(e)
	}
	if e = copyIn.Send(&pb.CopyInChunk{Data: []byte("example archive")}); e != nil {
		t.Fatal(e)
	}
	if _, e = copyIn.CloseAndRecv(); e != nil {
		t.Fatal(e)
	}
	if string(uploaded) != "example archive" {
		t.Fatalf("upload %q", uploaded)
	}
	exec, e := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: []string{"sh", "step.sh"}, Workdir: "/workspace/org/repo"})
	if e != nil {
		t.Fatal(e)
	}
	got := map[pb.DataChunk_Stream]string{}
	var exit int32
	for {
		msg, e := exec.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if d := msg.GetData(); d != nil {
			got[d.Stream] += string(d.Data)
		}
		if end := msg.GetExecComplete(); end != nil {
			exit = end.ExitCode
		}
	}
	if exit != 42 || got[pb.DataChunk_STDOUT] != "hello\n" || got[pb.DataChunk_STDERR] != "warning\n" {
		t.Fatalf("exit %d streams %v", exit, got)
	}
	copyOut, e := client.CopyOut(ctx, &pb.CopyOutRequest{EnvironmentId: id, SrcPath: "/shared/act/env"})
	if e != nil {
		t.Fatal(e)
	}
	var archive bytes.Buffer
	for {
		msg, e := copyOut.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		archive.Write(msg.Data)
	}
	tr := tar.NewReader(&archive)
	hdr, e := tr.Next()
	if e != nil || hdr.Name != "env" {
		t.Fatalf("tar: %v %v", hdr, e)
	}
	b, e := io.ReadAll(tr)
	if e != nil || string(b) != "KEY=value\n" {
		t.Fatalf("env: %q %v", b, e)
	}
	if _, e = client.Remove(ctx, &pb.RemoveRequest{EnvironmentId: id}); e != nil {
		t.Fatal(e)
	}
}
