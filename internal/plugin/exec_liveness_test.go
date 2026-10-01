package plugin

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kexec "k8s.io/client-go/util/exec"
)

// Exercise real gRPC framing/keepalive and the production Exec/chunkWriter path,
// replacing only Kubernetes execution. Virtual time crosses the Runner's 30s
// ping interval and the old server's ping-strike threshold without real sleeps.
func TestExecOutputLiveness(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		start, periodic, delayed, cancel, deadline, legacy bool
		image                                              bool
		exit                                               int
		duration                                           time.Duration
	}{
		{name: "quiet success", start: true},
		{name: "fully silent success", duration: 10 * time.Minute},
		{name: "periodic output", start: true, periodic: true},
		{name: "quiet nonzero", exit: 23},
		{name: "delayed stdout and stderr", delayed: true},
		{name: "explicit cancellation", cancel: true},
		{name: "explicit deadline", deadline: true},
		{name: "image quiet success", image: true, start: true},
		{name: "image fully silent success", image: true, duration: 10 * time.Minute},
		{name: "image quiet nonzero", image: true, exit: 23},
		{name: "image cancellation", image: true, cancel: true},
		{name: "image deadline", image: true, deadline: true},
		{name: "legacy default rejects quiet stream", start: true, legacy: true},
		{name: "legacy default rejects fully silent success", duration: 10 * time.Minute, legacy: true},
		{name: "legacy default rejects quiet nonzero", exit: 23, legacy: true},
		{name: "legacy default rejects delayed output", delayed: true, legacy: true},
		{name: "legacy default masked by periodic output", start: true, periodic: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := testConfig()
				if tc.image {
					cfg = imageConfig(profileImage)
				}
				s, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{})
				if err != nil {
					t.Fatal(err)
				}
				duration := tc.duration
				if duration == 0 {
					duration = 180 * time.Second
				}
				started, finished := make(chan struct{}), make(chan error, 1)
				s.execFn = func(ctx context.Context, _ string, args []string, stdin io.Reader, stdout, stderr io.Writer) (result error) {
					if slices.Equal(args, []string{"/usr/bin/env", "-0"}) {
						_, err := io.WriteString(stdout, "HOME=/home/node\x00PATH=/image/bin\x00")
						return err
					}
					defer func() { finished <- result }()
					if stdin != nil {
						return errors.New("unexpected stdin")
					}
					close(started)
					if tc.start {
						if _, err := io.WriteString(stdout, "before\n"); err != nil {
							return err
						}
					}
					for i := 0; i < int(duration/(15*time.Second)); i++ {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(15 * time.Second):
						}
						if tc.periodic {
							if _, err := io.WriteString(stdout, "process output\n"); err != nil {
								return err
							}
						}
					}
					if tc.start || tc.delayed {
						if _, err := io.WriteString(stdout, "after\n"); err != nil {
							return err
						}
					}
					if tc.delayed {
						if _, err := io.WriteString(stderr, "delayed stderr\n"); err != nil {
							return err
						}
					}
					if tc.exit != 0 {
						return kexec.CodeExitError{Err: errors.New("nonzero exit"), Code: tc.exit}
					}
					return nil
				}
				listener := bufconn.Listen(1 << 20)
				g := NewGRPCServer()
				if tc.legacy {
					g = grpc.NewServer()
				}
				pb.RegisterBackendPluginServer(g, s)
				go g.Serve(listener)
				defer g.Stop()
				defer listener.Close()
				conn, err := grpc.NewClient("passthrough:///bufnet",
					grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
					// Exact defaults from pinned Runner act/plugin/client.go.
					grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: false}),
				)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				client := pb.NewBackendPluginClient(conn)
				created, err := client.Create(context.Background(), &pb.CreateRequest{Name: "liveness"})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				if tc.deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 45*time.Second)
				}
				defer cancel()
				if tc.cancel {
					go func() { <-started; time.Sleep(45 * time.Second); cancel() }()
				}
				begin := time.Now()
				stream, err := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: created.EnvironmentId, Command: []string{"quiet-command"}})
				if err != nil {
					t.Fatal(err)
				}
				var out, errout strings.Builder
				var complete *pb.ExecComplete
				for {
					message, recvErr := stream.Recv()
					if recvErr != nil {
						if tc.cancel || tc.deadline {
							want := codes.Canceled
							if tc.deadline {
								want = codes.DeadlineExceeded
							}
							if status.Code(recvErr) != want {
								t.Fatalf("want %s, got %v", want, recvErr)
							}
						} else if tc.legacy && !tc.periodic {
							if status.Code(recvErr) != codes.Unavailable || !strings.Contains(recvErr.Error(), "too_many_pings") {
								t.Fatalf("expected legacy keepalive rejection, got %v", recvErr)
							}
							t.Logf("legacy control: %v", recvErr)
						} else if recvErr != io.EOF {
							t.Fatalf("quiet/open RPC failed after %s: %v", time.Since(begin), recvErr)
						}
						break
					}
					if data := message.GetData(); data != nil {
						if data.Stream == pb.DataChunk_STDERR {
							errout.Write(data.Data)
						} else {
							out.Write(data.Data)
						}
					}
					if end := message.GetExecComplete(); end != nil {
						complete = end
					}
					if fail := message.GetExecFailed(); fail != nil {
						t.Fatalf("ExecFailed: %s", fail.ErrorMessage)
					}
				}
				if tc.cancel || tc.deadline || (tc.legacy && !tc.periodic) {
					if tc.legacy && time.Since(begin) >= duration {
						t.Fatal("legacy policy must disconnect before process completion")
					}
					if complete != nil {
						t.Fatal("cancelled execution reported completion")
					}
					if err := <-finished; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("executor cancellation: %v", err)
					}
				} else {
					if complete == nil || complete.ExitCode != int32(tc.exit) {
						t.Fatalf("completion: %v", complete)
					}
					if time.Since(begin) < duration {
						t.Fatal("execution ended before simulated process finished")
					}
					if tc.start && !strings.Contains(out.String(), "before\nafter\n") && !tc.periodic {
						t.Fatal(out.String())
					}
					if tc.periodic && strings.Count(out.String(), "process output\n") != 12 {
						t.Fatal(out.String())
					}
					if tc.delayed && (out.String() != "after\n" || errout.String() != "delayed stderr\n") {
						t.Fatalf("streams: %q %q", out.String(), errout.String())
					}
					<-finished
				}
				// Stream cancellation alone never removes an environment.
				if _, err := client.Remove(context.Background(), &pb.RemoveRequest{EnvironmentId: created.EnvironmentId}); err != nil {
					t.Fatal(err)
				}
				t.Logf("simulated duration %s; exit=%v", time.Since(begin), complete)
			})
		})
	}
}

// The pinned Runner disables client pings without active RPCs. The server's
// unchanged false policy must not penalize silence between RPCs. grpc-go's
// normal 30-minute channel idling may close an unused transport; the next RPC
// reconnects normally, rather than surfacing a keepalive rejection.
func TestRunnerIdleConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(testConfig(), fake.NewSimpleClientset(), &rest.Config{})
		if err != nil {
			t.Fatal(err)
		}
		listener := bufconn.Listen(1 << 20)
		g := NewGRPCServer()
		pb.RegisterBackendPluginServer(g, s)
		go g.Serve(listener)
		defer g.Stop()
		defer listener.Close()
		var dials atomic.Int32
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { dials.Add(1); return listener.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: false}),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		client := pb.NewBackendPluginClient(conn)
		check := func(wantDials int32) {
			t.Helper()
			caps, err := client.Capabilities(context.Background(), &pb.CapabilitiesRequest{})
			if err != nil || caps.GetName() != "kubernetes" {
				t.Fatalf("idle connection RPC: %v %v", caps, err)
			}
			synctest.Wait()
			if got := dials.Load(); got != wantDials {
				t.Fatalf("expected %d connections, got %d", wantDials, got)
			}
		}
		check(1)
		time.Sleep(10 * time.Minute)
		check(1)
		time.Sleep(35 * time.Minute)
		check(2)
	})
}
