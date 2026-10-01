package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
)

const (
	defaultExecPath  = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	maxImageEnvBytes = 64 * 1024
	imageEnvTimeout  = 30 * time.Second
)

type boundedEnvBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedEnvBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > maxImageEnvBytes-b.buffer.Len() {
		b.overflow = true
		b.cancel() // Don't leave SPDY waiting for EOF after a failed output copy.
		return 0, fmt.Errorf("image environment exceeds limit")
	}
	return b.buffer.Write(p)
}

func (b *boundedEnvBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes()), b.overflow
}

func parseImageEnv(data []byte) (map[string]string, error) {
	if len(data) > maxImageEnvBytes || len(data) > 0 && data[len(data)-1] != 0 {
		return nil, status.Error(codes.FailedPrecondition, "invalid job environment discovery framing")
	}
	result := map[string]string{}
	if len(data) == 0 {
		return result, nil
	}
	for _, entry := range bytes.Split(data[:len(data)-1], []byte{0}) {
		key, value, ok := strings.Cut(string(entry), "=")
		if !ok || key == "" {
			return nil, status.Error(codes.FailedPrecondition, "invalid job environment discovery entry")
		}
		if _, exists := result[key]; exists {
			return nil, status.Error(codes.FailedPrecondition, "duplicate job environment discovery key")
		}
		result[key] = value
	}
	return result, nil
}

// Collect inherited effective image/Pod environment, not a shell's filtered
// environment. No application entrypoint, registry inspection, secret logging
// or persistent env annotation/cache. Every job uses the same behavior.
func (s *Server) executionDefaults(ctx context.Context, p *core.Pod) (map[string]string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, imageEnvTimeout)
	defer cancel()
	out := &boundedEnvBuffer{cancel: cancel}
	err := s.execPod(probeCtx, p.Name, []string{"/usr/bin/env", "-0"}, nil, out, io.Discard)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	data, overflow := out.snapshot()
	if overflow {
		return nil, status.Error(codes.ResourceExhausted, "job environment discovery exceeds 64KiB limit")
	}
	if probeCtx.Err() != nil {
		return nil, status.Error(codes.DeadlineExceeded, "job environment discovery timed out")
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "job environment discovery failed; image must support /usr/bin/env -0")
	}
	inherited, err := parseImageEnv(data)
	if err != nil {
		return nil, err
	}
	// Stored plugin endpoint/TMPDIR defaults beat inherited image values;
	// explicit Runner Exec values win last, including empty values.
	defaults := mergeExecDefaults(podExecDefaults(p), inherited)
	if _, present := defaults["PATH"]; !present {
		defaults["PATH"] = defaultExecPath
	}
	if _, present := defaults["HOME"]; !present {
		defaults["HOME"] = workspace + "/workdir"
	}
	return defaults, nil
}
