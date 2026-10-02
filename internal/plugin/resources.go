package plugin

import (
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Ordinary job scheduling/limits are operator choices, not Runner mechanics.
// CPU has no default limit: requests affect placement/contention, not bursts.
// Memory retains a finite default; explicit "none" delegates to cluster policy.
func (c Config) jobResources() (core.ResourceRequirements, resource.Quantity, error) {
	workspaceSize, storageRequest, storageLimit, err := c.storageQuantities()
	if err != nil {
		return core.ResourceRequirements{}, workspaceSize, err
	}
	result := core.ResourceRequirements{
		Requests: core.ResourceList{core.ResourceEphemeralStorage: storageRequest},
		Limits:   core.ResourceList{core.ResourceEphemeralStorage: storageLimit},
	}
	parse := func(name, raw, def string, cpu bool) (*resource.Quantity, error) {
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
	for _, pair := range []struct {
		key                                                                  core.ResourceName
		requestName, limitName, request, limit, defaultRequest, defaultLimit string
	}{
		{core.ResourceCPU, "JOB_CPU_REQUEST", "JOB_CPU_LIMIT", c.CPURequest, c.CPULimit, "100m", ""},
		{core.ResourceMemory, "JOB_MEMORY_REQUEST", "JOB_MEMORY_LIMIT", c.MemoryRequest, c.MemoryLimit, "128Mi", "1Gi"},
	} {
		req, err := parse(pair.requestName, pair.request, pair.defaultRequest, pair.key == core.ResourceCPU)
		if err != nil {
			return result, workspaceSize, err
		}
		lim, err := parse(pair.limitName, pair.limit, pair.defaultLimit, pair.key == core.ResourceCPU)
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
	return result, workspaceSize, nil
}
