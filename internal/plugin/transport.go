package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"
)

// Kubernetes exec has no env or cwd fields; the constrained ubuntu image
// supplies /usr/bin/env and /bin/sh. Values are passed as argv, never shell-expanded.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func commandArgs(r *pb.ExecRequest) ([]string, error) {
	if len(r.GetCommand()) == 0 || r.GetCommand()[0] == "" || r.GetUser() != "" {
		return nil, status.Error(codes.InvalidArgument, "command required; per-exec user unsupported")
	}
	wd := r.GetWorkdir()
	if wd == "" {
		wd = workspace + "/workdir"
	}
	if !safePath(wd) {
		return nil, status.Error(codes.InvalidArgument, "workdir outside workspace")
	}
	args := []string{"/usr/bin/env", "-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	keys := make([]string, 0, len(r.GetEnv()))
	for k := range r.GetEnv() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envName.MatchString(k) || strings.ContainsRune(r.GetEnv()[k], 0) {
			return nil, status.Error(codes.InvalidArgument, "invalid environment variable")
		}
		args = append(args, k+"="+r.GetEnv()[k])
	}
	args = append(args, "/bin/sh", "-c", `mkdir -p -- "$1" && cd -- "$1" && shift && exec "$@"`, "forgejo", wd)
	args = append(args, r.GetCommand()...)
	return args, nil
}
func safePath(p string) bool {
	return (p == workspace || strings.HasPrefix(p, workspace+"/") || p == "/workspace" || strings.HasPrefix(p, "/workspace/")) && path.Clean(p) == strings.TrimSuffix(p, "/") && !strings.ContainsRune(p, 0)
}

// execPod is deliberately a method boundary so the streaming interface can
// later be tested against a disposable API server without mocking SPDY frames.
func (s *Server) execPod(ctx context.Context, id string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.execFn != nil {
		return s.execFn(ctx, id, argv, stdin, stdout, stderr)
	}
	req := s.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(s.cfg.Namespace).Name(id).SubResource("exec")
	req.VersionedParams(&core.PodExecOptions{Container: "job", Command: argv, Stdin: stdin != nil, Stdout: stdout != nil, Stderr: stderr != nil, TTY: false}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(s.rest, "POST", req.URL())
	if err != nil {
		return err
	}
	return ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Tty: false})
}
func exitResult(err error) (int32, string) {
	if err == nil {
		return 0, ""
	}
	var ex kexec.ExitError
	if errors.As(err, &ex) {
		return int32(ex.ExitStatus()), ""
	}
	return 0, err.Error()
}

// chunkWriter sends are serialized: stdout and stderr may arrive on separate
// SPDY goroutines, but a grpc server stream must not have concurrent writers.
type chunkWriter struct{ send func([]byte) error }

func (w chunkWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if err := w.send(append([]byte(nil), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (s *Server) Exec(r *pb.ExecRequest, stream grpc.ServerStreamingServer[pb.ExecOutput]) error {
	id := r.GetEnvironmentId()
	if err := s.checkID(id); err != nil {
		return err
	}
	unlock := s.lock(id)
	defer unlock()
	if _, err := s.get(stream.Context(), id); err != nil {
		return err
	}
	argv, err := commandArgs(r)
	if err != nil {
		return err
	}
	var mu sync.Mutex // serializes stdout/stderr sends
	send := func(t pb.DataChunk_Stream, b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_Data{Data: &pb.DataChunk{Stream: t, Data: b}}})
	}
	err = s.execPod(stream.Context(), id, argv, nil, chunkWriter{send: func(b []byte) error { return send(pb.DataChunk_STDOUT, b) }}, chunkWriter{send: func(b []byte) error { return send(pb.DataChunk_STDERR, b) }})
	if stream.Context().Err() != nil {
		return stream.Context().Err()
	}
	code, failed := exitResult(err)
	if failed != "" {
		return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_ExecFailed{ExecFailed: &pb.ExecFailed{ErrorMessage: failed}}})
	}
	return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_ExecComplete{ExecComplete: &pb.ExecComplete{ExitCode: code}}})
}

func (s *Server) CopyIn(stream grpc.ClientStreamingServer[pb.CopyInChunk, pb.CopyInResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "missing copy header: %v", err)
	}
	if first.EnvironmentId == nil || first.DestPath == nil || !safePath(first.GetDestPath()) {
		return status.Error(codes.InvalidArgument, "invalid copy destination or environment ID")
	}
	id := first.GetEnvironmentId()
	if err := s.checkID(id); err != nil {
		return err
	}
	unlock := s.lock(id)
	defer unlock()
	if _, err := s.get(stream.Context(), id); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	received := make(chan error, 1)
	go func() {
		if _, e := pw.Write(first.Data); e != nil {
			received <- e
			return
		}
		for {
			chunk, e := stream.Recv()
			if e == io.EOF {
				received <- pw.Close()
				return
			}
			if e != nil {
				pw.CloseWithError(e)
				received <- e
				return
			}
			if chunk.EnvironmentId != nil || chunk.DestPath != nil {
				e = status.Error(codes.InvalidArgument, "copy header on later chunk")
				pw.CloseWithError(e)
				received <- e
				return
			}
			if _, e = pw.Write(chunk.Data); e != nil {
				received <- e
				return
			}
		}
	}()
	var stderr bytes.Buffer
	err = s.execPod(stream.Context(), id, []string{"/bin/sh", "-c", `mkdir -p -- "$1" && exec tar -xpf - -C "$1" --no-same-owner --no-same-permissions`, "forgejo", first.GetDestPath()}, pr, nil, &stderr)
	if err != nil {
		pr.CloseWithError(err)
	}
	recvErr := <-received
	if stream.Context().Err() != nil {
		return stream.Context().Err()
	}
	if recvErr != nil {
		return status.Errorf(codes.InvalidArgument, "copy stream: %v", recvErr)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "extract archive: %v: %s", err, stderr.String())
	}
	return stream.SendAndClose(&pb.CopyInResponse{})
}

func (s *Server) CopyOut(r *pb.CopyOutRequest, stream grpc.ServerStreamingServer[pb.CopyOutChunk]) error {
	id := r.GetEnvironmentId()
	if err := s.checkID(id); err != nil {
		return err
	}
	if !safePath(r.GetSrcPath()) || r.GetSrcPath() == workspace {
		return status.Error(codes.InvalidArgument, "invalid copy source")
	}
	unlock := s.lock(id)
	defer unlock()
	if _, err := s.get(stream.Context(), id); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		// Missing step env files are normal. Runner treats an empty archive as no values.
		e := s.execPod(stream.Context(), id, []string{"/bin/sh", "-c", `[ ! -e "$1" ] || exec tar -cf - -C "$2" -- "$3"`, "forgejo", r.GetSrcPath(), path.Dir(r.GetSrcPath()), path.Base(r.GetSrcPath())}, nil, pw, &stderr)
		pw.CloseWithError(e)
		done <- e
	}()
	buf := make([]byte, 32*1024)
	for {
		n, e := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.CopyOutChunk{Data: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				pr.CloseWithError(sendErr)
				<-done
				return sendErr
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			<-done
			if stream.Context().Err() != nil {
				return stream.Context().Err()
			}
			return status.Errorf(codes.Internal, "archive read: %v: %s", e, stderr.String())
		}
	}
	err := <-done
	if stream.Context().Err() != nil {
		return stream.Context().Err()
	}
	if err != nil {
		return status.Errorf(codes.Internal, "archive: %v: %s", err, stderr.String())
	}
	return nil
}
