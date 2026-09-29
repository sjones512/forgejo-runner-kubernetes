package plugin

import (
	"fmt"
	"maps"

	"github.com/distribution/reference"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	dockerSocketDir = "/run/forgejo-docker"
	dockerHost      = "unix://" + dockerSocketDir + "/docker.sock"
	dindTemplate    = "forgejo.org/dind-template-sha256"
)

// DinDConfig is operator-only, instance-wide configuration. No Runner field
// can enable it or change its image, privileges, endpoint or resource budgets.
type DinDConfig struct {
	Enabled                 bool
	Image                   string // Required when enabled; name@sha256 digest, never an unpinned tag.
	CPURequest              string
	CPULimit                string
	MemoryRequest           string
	MemoryLimit             string
	EphemeralStorageRequest string
	EphemeralStorageLimit   string
	DataSizeLimit           string
	StorageDriver           string // Empty uses image default; overlay2 or vfs are explicit alternatives.
}

func (c DinDConfig) resources() (core.ResourceRequirements, resource.Quantity, error) {
	result := core.ResourceRequirements{Requests: core.ResourceList{}, Limits: core.ResourceList{}}
	parse := func(name, raw, def string) (resource.Quantity, error) {
		if raw == "" {
			raw = def
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil || q.Sign() <= 0 {
			return q, fmt.Errorf("%s: positive Kubernetes quantity required, got %q", name, raw)
		}
		return q, nil
	}
	for _, pair := range []struct {
		key                          core.ResourceName
		requestName, limitName       string
		request, limit               string
		defaultRequest, defaultLimit string
	}{
		{core.ResourceCPU, "JOB_DIND_CPU_REQUEST", "JOB_DIND_CPU_LIMIT", c.CPURequest, c.CPULimit, "100m", "2"},
		{core.ResourceMemory, "JOB_DIND_MEMORY_REQUEST", "JOB_DIND_MEMORY_LIMIT", c.MemoryRequest, c.MemoryLimit, "128Mi", "2Gi"},
		{core.ResourceEphemeralStorage, "JOB_DIND_EPHEMERAL_STORAGE_REQUEST", "JOB_DIND_EPHEMERAL_STORAGE_LIMIT", c.EphemeralStorageRequest, c.EphemeralStorageLimit, "1Gi", "12Gi"},
	} {
		req, err := parse(pair.requestName, pair.request, pair.defaultRequest)
		if err != nil {
			return result, resource.Quantity{}, err
		}
		lim, err := parse(pair.limitName, pair.limit, pair.defaultLimit)
		if err != nil {
			return result, resource.Quantity{}, err
		}
		if req.Cmp(lim) > 0 {
			return result, resource.Quantity{}, fmt.Errorf("%s must not exceed %s", pair.requestName, pair.limitName)
		}
		result.Requests[pair.key], result.Limits[pair.key] = req, lim
	}
	data, err := parse("JOB_DIND_DATA_SIZE_LIMIT", c.DataSizeLimit, "10Gi")
	if err != nil {
		return result, data, err
	}
	// Both data and socket emptyDirs count toward the Pod's aggregate local
	// storage usage. Preserve positive headroom in the daemon's contribution
	// above their combined caps, in addition to the job's existing headroom.
	used := data.DeepCopy()
	used.Add(resource.MustParse("1Mi"))
	if used.Cmp(result.Limits[core.ResourceEphemeralStorage]) >= 0 {
		return result, data, fmt.Errorf("JOB_DIND_DATA_SIZE_LIMIT plus the 1Mi socket volume must be less than JOB_DIND_EPHEMERAL_STORAGE_LIMIT")
	}
	return result, data, nil
}

func (c DinDConfig) validate() error {
	if c.Enabled || c.Image != "" {
		name, err := reference.ParseNormalizedNamed(c.Image)
		if err != nil {
			return fmt.Errorf("JOB_DIND_IMAGE: valid image reference pinned by @sha256:<64 lowercase hex digits> required")
		}
		pinned, ok := name.(reference.Digested)
		if !ok || pinned.Digest().Algorithm().String() != "sha256" {
			return fmt.Errorf("JOB_DIND_IMAGE: image must be pinned by @sha256:<64 lowercase hex digits>")
		}
	}
	switch c.StorageDriver {
	case "", "overlay2", "vfs":
	default:
		return fmt.Errorf("JOB_DIND_STORAGE_DRIVER: use overlay2, vfs or leave unset for the image default")
	}
	_, _, err := c.resources()
	return err
}

func (c DinDConfig) addToPod(p *core.Pod) {
	resources, dataSize, err := c.resources()
	if err != nil {
		panic(err) // Config.Validate runs before provisioning.
	}
	socketSize := resource.MustParse("1Mi")
	p.Spec.Volumes = append(p.Spec.Volumes,
		core.Volume{Name: "docker-socket", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &socketSize}}},
		core.Volume{Name: "docker-data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: &dataSize}}},
	)
	job := &p.Spec.Containers[0]
	job.VolumeMounts = append(job.VolumeMounts, core.VolumeMount{Name: "docker-socket", MountPath: dockerSocketDir})
	job.Env = []core.EnvVar{{Name: "DOCKER_HOST", Value: dockerHost}, {Name: "TMPDIR", Value: workspace + "/tmp"}}
	// Explicit dockerd arguments avoid the official image's default wildcard
	// TCP listener. Keep its entrypoint for DinD initialization/reaping.
	args := []string{"dockerd", "--host=" + dockerHost, "--group=10001", "--data-root=/var/lib/docker", "--exec-root=/run/forgejo-docker-state"}
	if c.StorageDriver != "" {
		args = append(args, "--storage-driver="+c.StorageDriver)
	}
	p.Spec.Containers = append(p.Spec.Containers, core.Container{
		Name: "dind", Image: c.Image, ImagePullPolicy: core.PullIfNotPresent, Args: args,
		Env: []core.EnvVar{{Name: "DOCKER_TLS_CERTDIR", Value: ""}},
		SecurityContext: &core.SecurityContext{
			Privileged: ptrBool(true), RunAsUser: ptrInt64(0), RunAsGroup: ptrInt64(0), RunAsNonRoot: ptrBool(false), AllowPrivilegeEscalation: ptrBool(true),
			SeccompProfile:  &core.SeccompProfile{Type: core.SeccompProfileTypeUnconfined},
			AppArmorProfile: &core.AppArmorProfile{Type: core.AppArmorProfileTypeUnconfined},
		},
		Resources: resources,
		VolumeMounts: []core.VolumeMount{
			{Name: "workspace", MountPath: workspace}, {Name: "workspace", MountPath: "/workspace"},
			{Name: "docker-socket", MountPath: dockerSocketDir},
			// Covers /var/lib/docker and /var/lib/containerd with ONE data cap,
			// including images that default to the newer containerd image store.
			{Name: "docker-data", MountPath: "/var/lib"},
		},
		ReadinessProbe: &core.Probe{
			ProbeHandler:   core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"docker", "--host=" + dockerHost, "info"}}},
			TimeoutSeconds: 3, PeriodSeconds: 2, FailureThreshold: 3, SuccessThreshold: 1,
		},
	})
}

func podHasDinD(p *core.Pod) bool {
	for _, c := range p.Spec.Containers {
		if c.Name == "dind" {
			return true
		}
	}
	return false
}

// Kubernetes Pod env does not survive commandArgs' env -i. Return only the
// defaults emitted on the job container, and layer Runner's Exec env above
// them. Use the stored Pod so an existing environment survives a config reload.
func podExecDefaults(p *core.Pod) map[string]string {
	result := map[string]string{}
	for _, c := range p.Spec.Containers {
		if c.Name == "job" {
			for _, v := range c.Env {
				if v.Name == "DOCKER_HOST" || v.Name == "TMPDIR" {
					result[v.Name] = v.Value
				}
			}
		}
	}
	return result
}

func mergeExecDefaults(env, defaults map[string]string) map[string]string {
	result := maps.Clone(defaults)
	if result == nil {
		result = map[string]string{}
	}
	maps.Copy(result, env)
	return result
}
