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
	dockerDataRoot  = "/var/lib/docker"
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
	DataSizeLimit           string // Empty/none omits the data-volume cap.
	SocketSizeLimit         string // Empty/none omits the socket-volume cap.
	StorageDriver           string // Empty uses image default; overlay2 or vfs are explicit alternatives.
}

func (c DinDConfig) resources() (core.ResourceRequirements, *resource.Quantity, *resource.Quantity, error) {
	result := core.ResourceRequirements{Requests: core.ResourceList{}, Limits: core.ResourceList{}}
	for _, pair := range []struct {
		key                    core.ResourceName
		requestName, limitName string
		request, limit         string
		defaultRequest         string
	}{
		{core.ResourceCPU, "JOB_DIND_CPU_REQUEST", "JOB_DIND_CPU_LIMIT", c.CPURequest, c.CPULimit, "100m"},
		{core.ResourceMemory, "JOB_DIND_MEMORY_REQUEST", "JOB_DIND_MEMORY_LIMIT", c.MemoryRequest, c.MemoryLimit, "128Mi"},
		{core.ResourceEphemeralStorage, "JOB_DIND_EPHEMERAL_STORAGE_REQUEST", "JOB_DIND_EPHEMERAL_STORAGE_LIMIT", c.EphemeralStorageRequest, c.EphemeralStorageLimit, "1Gi"},
	} {
		req, err := parseResourceQuantity(pair.requestName, pair.request, pair.defaultRequest, pair.key == core.ResourceCPU)
		if err != nil {
			return result, nil, nil, err
		}
		lim, err := parseResourceQuantity(pair.limitName, pair.limit, "", pair.key == core.ResourceCPU)
		if err != nil {
			return result, nil, nil, err
		}
		if req != nil && lim != nil && req.Cmp(*lim) > 0 {
			return result, nil, nil, fmt.Errorf("%s must not exceed %s", pair.requestName, pair.limitName)
		}
		if req != nil {
			result.Requests[pair.key] = *req
		}
		if lim != nil {
			result.Limits[pair.key] = *lim
		}
	}
	data, err := parseResourceQuantity("JOB_DIND_DATA_SIZE_LIMIT", c.DataSizeLimit, "", false)
	if err != nil {
		return result, nil, nil, err
	}
	socket, err := parseResourceQuantity("JOB_DIND_SOCKET_SIZE_LIMIT", c.SocketSizeLimit, "", false)
	if err != nil {
		return result, nil, nil, err
	}
	// Validate headroom only when all corresponding caps are selected.
	// Uncapped volumes never imply a bound on aggregate local storage usage.
	if limit, ok := result.Limits[core.ResourceEphemeralStorage]; ok && data != nil && socket != nil {
		used := data.DeepCopy()
		used.Add(*socket)
		if used.Cmp(limit) >= 0 {
			return result, nil, nil, fmt.Errorf("JOB_DIND_DATA_SIZE_LIMIT plus JOB_DIND_SOCKET_SIZE_LIMIT must be less than JOB_DIND_EPHEMERAL_STORAGE_LIMIT")
		}
	}
	return result, data, socket, nil
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
	_, _, _, err := c.resources()
	return err
}

func (c DinDConfig) addToPod(p *core.Pod) {
	resources, dataSize, socketSize, err := c.resources()
	if err != nil {
		panic(err) // Config.Validate runs before provisioning.
	}
	p.Spec.Volumes = append(p.Spec.Volumes,
		core.Volume{Name: "docker-socket", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: socketSize}}},
		core.Volume{Name: "docker-data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: dataSize}}},
	)
	job := &p.Spec.Containers[0]
	job.VolumeMounts = append(job.VolumeMounts, core.VolumeMount{Name: "docker-socket", MountPath: dockerSocketDir})
	job.Env = []core.EnvVar{{Name: "DOCKER_HOST", Value: dockerHost}, {Name: "TMPDIR", Value: workspace + "/tmp"}}
	// Explicit dockerd arguments avoid the official image's default wildcard
	// TCP listener. Keep its entrypoint for DinD initialization/reaping.
	args := []string{"dockerd", "--host=" + dockerHost, "--group=10001", "--data-root=" + dockerDataRoot, "--exec-root=/run/forgejo-docker-state"}
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
			// Mount at the exact data root, not its parent: official DinD declares
			// VOLUME /var/lib/docker, which can shadow a parent /var/lib mount.
			// Share the fixed path with --data-root so the two cannot drift.
			{Name: "docker-data", MountPath: dockerDataRoot},
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
