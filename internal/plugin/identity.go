package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	profileAnnotation = "forgejo.org/job-security-profile"
	jobTemplate       = "forgejo.org/job-template-sha256"
	profileFixed      = "fixed"
	profileImage      = "image"
	profileImageCI    = "image-ci"
	defaultExecPath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	maxImageEnvBytes  = 64 * 1024
	imageEnvTimeout   = 30 * time.Second
	// The helper acts only on fresh, root-owned emptyDirs before job/daemon
	// startup. fsGroup/runAsGroup supplies membership for chgrp without CHOWN.
	permissionsScript = `umask 007; for dir in "$@"; do mkdir -p -- "$dir"; chgrp 10001 "$dir"; chmod 2770 "$dir"; done`
)

// Explicit subset of conventional OCI/Docker defaults for package ownership,
// user switching/supervision, file capabilities and dpkg chroots. Never inferred from USER.
func ciCapabilities() []core.Capability {
	return []core.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID", "SETFCAP", "SYS_CHROOT", "KILL"}
}

func (c Config) securityProfile() string {
	if c.SecurityProfile == "" {
		return profileFixed
	}
	return c.SecurityProfile
}

func (c Config) validateIdentity() error {
	switch c.securityProfile() {
	case profileFixed:
		if c.PermissionsImage != "" {
			return fmt.Errorf("JOB_PERMISSIONS_IMAGE requires JOB_SECURITY_PROFILE=image or image-ci")
		}
	case profileImage, profileImageCI:
		name, err := reference.ParseNormalizedNamed(c.PermissionsImage)
		if err != nil {
			return fmt.Errorf("JOB_PERMISSIONS_IMAGE: an operator-owned image pinned by @sha256:<64 lowercase hex digits> is required")
		}
		pinned, ok := name.(reference.Digested)
		if !ok || pinned.Digest().Algorithm().String() != "sha256" {
			return fmt.Errorf("JOB_PERMISSIONS_IMAGE: an operator-owned image pinned by @sha256:<64 lowercase hex digits> is required")
		}
	default:
		return fmt.Errorf("JOB_SECURITY_PROFILE: use fixed (default), image or image-ci")
	}
	return nil
}

func (c Config) applyIdentity(p *core.Pod) {
	profile := c.securityProfile()
	p.Annotations[profileAnnotation] = profile
	job := &p.Spec.Containers[0]
	if profile == profileFixed {
		// Preserve effective legacy identity without imposing it on other
		// containers. Primary GID remains unspecified, exactly as before.
		job.SecurityContext.RunAsUser = ptrInt64(10001)
		job.SecurityContext.RunAsNonRoot = ptrBool(true)
		return
	}
	if profile == profileImageCI {
		job.SecurityContext.Capabilities.Add = ciCapabilities()
	}
	dirs := []string{workspace, workspace + "/act", workspace + "/toolcache", workspace + "/workdir", workspace + "/tmp"}
	mounts := []core.VolumeMount{{Name: "workspace", MountPath: workspace}}
	if podHasDinD(p) {
		dirs = append(dirs, dockerSocketDir)
		mounts = append(mounts, core.VolumeMount{Name: "docker-socket", MountPath: dockerSocketDir})
	}
	p.Spec.InitContainers = []core.Container{{
		Name: "permissions", Image: c.PermissionsImage, ImagePullPolicy: core.PullIfNotPresent,
		Command: []string{"/bin/sh", "-ec", permissionsScript, "permissions"}, Args: dirs, VolumeMounts: mounts,
		SecurityContext: &core.SecurityContext{
			RunAsUser: ptrInt64(0), RunAsGroup: ptrInt64(10001), RunAsNonRoot: ptrBool(false),
			AllowPrivilegeEscalation: ptrBool(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}},
		},
		Resources: core.ResourceRequirements{
			Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("10m"), core.ResourceMemory: resource.MustParse("16Mi"), core.ResourceEphemeralStorage: resource.MustParse("1Mi")},
			Limits:   core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("64Mi"), core.ResourceEphemeralStorage: resource.MustParse("64Mi")},
		},
	}}
}

// An unannotated verified fixed-UID Pod is a pre-profile legacy environment.
// Later RPCs follow the stored Pod, never reinterpret using current config.
func podSecurityProfile(p *core.Pod) (string, error) {
	switch v := p.Annotations[profileAnnotation]; v {
	case profileFixed:
		return profileFixed, nil
	case "":
		// Recognize the actual old fixed-UID shape, not any owned Pod with
		// missing annotations (e.g. an accidentally stripped native Pod).
		for _, job := range p.Spec.Containers {
			if job.Name != "job" {
				continue
			}
			var uid *int64
			var nonroot *bool
			if p.Spec.SecurityContext != nil {
				uid, nonroot = p.Spec.SecurityContext.RunAsUser, p.Spec.SecurityContext.RunAsNonRoot
			}
			if job.SecurityContext != nil {
				if job.SecurityContext.RunAsUser != nil {
					uid = job.SecurityContext.RunAsUser
				}
				if job.SecurityContext.RunAsNonRoot != nil {
					nonroot = job.SecurityContext.RunAsNonRoot
				}
			}
			if uid != nil && *uid == 10001 && nonroot != nil && *nonroot {
				return profileFixed, nil
			}
		}
		return "", status.Error(codes.FailedPrecondition, "missing stored job security profile; legacy identity cannot be verified")
	case profileImage, profileImageCI:
		return v, nil
	default:
		return "", status.Error(codes.FailedPrecondition, "unknown stored job security profile")
	}
}

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
// environment. No application entrypoint, image inspection, secret logging or
// persistent env annotation/cache. Rediscovery supports plugin restart safely.
func (s *Server) executionDefaults(ctx context.Context, p *core.Pod) (map[string]string, error) {
	profile, err := podSecurityProfile(p)
	if err != nil {
		return nil, err
	}
	defaults := podExecDefaults(p)
	if profile == profileFixed {
		return defaults, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, imageEnvTimeout)
	defer cancel()
	out := &boundedEnvBuffer{cancel: cancel}
	err = s.execPod(probeCtx, p.Name, []string{"/usr/bin/env", "-0"}, nil, out, io.Discard)
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
	// Image/Pod PATH and explicit empty values beat fallback. Stored plugin
	// endpoint/TMPDIR defaults beat inherited image values; Runner Exec wins last.
	defaults = mergeExecDefaults(defaults, inherited)
	if _, present := defaults["PATH"]; !present {
		defaults["PATH"] = defaultExecPath
	}
	if _, present := defaults["HOME"]; !present {
		defaults["HOME"] = workspace + "/workdir"
	}
	return defaults, nil
}
