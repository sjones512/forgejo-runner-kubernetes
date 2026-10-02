package plugin

import (
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Empty selects def; empty/none after defaulting means omit, never a zero limit.
func parseResourceQuantity(name, raw, def string, cpu bool) (*resource.Quantity, error) {
	if raw == "" {
		raw = def
	}
	if raw == "" || raw == "none" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil || q.Sign() <= 0 {
		return nil, fmt.Errorf("%s: positive Kubernetes quantity or none required", name)
	}
	if cpu && q.Cmp(*resource.NewMilliQuantity(q.MilliValue(), resource.DecimalSI)) != 0 {
		return nil, fmt.Errorf("%s: CPU precision finer than 1m is unsupported", name)
	}
	return &q, nil
}

// Requests guide scheduling; every resource/storage limit is operator opt-in.
func (c Config) jobResources() (core.ResourceRequirements, *resource.Quantity, error) {
	workspaceSize, err := parseResourceQuantity("JOB_WORKSPACE_SIZE_LIMIT", c.WorkspaceSizeLimit, "", false)
	if err != nil {
		return core.ResourceRequirements{}, nil, err
	}
	result := core.ResourceRequirements{Requests: core.ResourceList{}, Limits: core.ResourceList{}}
	for _, pair := range []struct {
		key                                                    core.ResourceName
		requestName, limitName, request, limit, defaultRequest string
	}{
		{core.ResourceCPU, "JOB_CPU_REQUEST", "JOB_CPU_LIMIT", c.CPURequest, c.CPULimit, "100m"},
		{core.ResourceMemory, "JOB_MEMORY_REQUEST", "JOB_MEMORY_LIMIT", c.MemoryRequest, c.MemoryLimit, "128Mi"},
		{core.ResourceEphemeralStorage, "JOB_EPHEMERAL_STORAGE_REQUEST", "JOB_EPHEMERAL_STORAGE_LIMIT", c.EphemeralStorageRequest, c.EphemeralStorageLimit, "256Mi"},
	} {
		req, err := parseResourceQuantity(pair.requestName, pair.request, pair.defaultRequest, pair.key == core.ResourceCPU)
		if err != nil {
			return result, workspaceSize, err
		}
		lim, err := parseResourceQuantity(pair.limitName, pair.limit, "", pair.key == core.ResourceCPU)
		if err != nil {
			return result, workspaceSize, err
		}
		if req != nil && lim != nil && req.Cmp(*lim) > 0 {
			return result, workspaceSize, fmt.Errorf("%s must not exceed %s", pair.requestName, pair.limitName)
		}
		if req != nil {
			result.Requests[pair.key] = *req
		}
		if lim != nil {
			result.Limits[pair.key] = *lim
		}
	}
	// Compare only explicit bounds; no implicit workspace/container cap.
	if lim, ok := result.Limits[core.ResourceEphemeralStorage]; ok && workspaceSize != nil && workspaceSize.Cmp(lim) >= 0 {
		return result, workspaceSize, fmt.Errorf("JOB_WORKSPACE_SIZE_LIMIT must be less than JOB_EPHEMERAL_STORAGE_LIMIT to leave room for container layers and logs")
	}
	return result, workspaceSize, nil
}
