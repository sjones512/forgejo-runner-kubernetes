# Generated CI Pod policy and operator contract

Capacity policy belongs to the operator, not the executor. CPU throttling,
memory OOM and disk eviction are different mechanisms. Requests are scheduling
estimates, not ceilings or guaranteed available capacity.

## Operator resource contract

Settings are instance-wide, not per-workflow.

| Setting | Empty/unset | Explicit choice |
| --- | --- | --- |
| JOB_CPU_REQUEST | 100m | Positive CPU quantity, or exact `none` to omit |
| JOB_CPU_LIMIT | omitted | Positive CPU quantity, or `none` to omit |
| JOB_MEMORY_REQUEST | 128Mi | Positive byte quantity, or `none` to omit |
| JOB_MEMORY_LIMIT | omitted | Positive byte quantity, or `none` to omit |
| JOB_EPHEMERAL_STORAGE_REQUEST | 256Mi | Positive byte quantity, or `none` to omit |
| JOB_EPHEMERAL_STORAGE_LIMIT | omitted | Positive byte quantity, or `none` to omit |
| JOB_WORKSPACE_SIZE_LIMIT | omitted | Positive byte quantity, or `none` to omit |

Requests and limits are independent. Quantities must be positive; CPU precision
must be at least 1m. Request≤limit is checked only when both exist. When both
workspace and ephemeral-storage caps exist, workspace<ephemeral limit leaves
room for writable layers and logs. A missing cap is never silently filled in.
All resource and volume-size limits are omitted by default.

Namespace admission can add defaults or reject omitted limits. Kubernetes can
copy a supplied limit into a missing request. Inspect admitted Pods, not only
plugin output. Parent cgroups and node contention can still constrain bursts;
omitting a CPU limit does not promise four available cores.

The workspace is one disk-backed emptyDir mounted at both /shared and /workspace.
The aliases do not have separate budgets. An emptyDir size limit is measured by
kubelet for eviction, not a synchronous filesystem quota or a space reservation.
Node DiskPressure can evict uncapped jobs too.

Image pull policy is left to Kubernetes defaults. Scheduling/DNS/lifecycle fields
are omitted unless required by the executor: architecture selection,
restartPolicy Never, Runner lifetime deadline and explicit bounded Remove.
No new general Pod configuration API is provided.

These settings govern jobs, not the plugin Deployment or the external BuildKit
server. Builder storage/resources/GC are entirely operator-owned.

## Breaking changes and separate acceptance

DinD configuration and sidecar/data/socket provisioning are removed. Follow the
[image-building migration](oci-building-design.md) before upgrading. Drain active
jobs: template hashes intentionally reject retries against changed Pod specs.

Verify normal Actions, file transfers, nonzero exits and cleanup after an
upgrade. Use focused plugin tests for Pod shape, resource validation and
lifecycle behavior; application publication workflows need no additional tests.

Sources: [Kubernetes resources](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/),
[image pull defaults](https://kubernetes.io/docs/concepts/containers/images/),
[Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
