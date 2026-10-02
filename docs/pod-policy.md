# Generated CI Pod policy audit and operator contract

**Resource policy for v0.1.0-alpha.12, following alpha.11 (`30063dd`).** Publication was explicitly authorized after audit review; cluster deployment/acceptance remains separate. Scope: plugin-generated jobs, not the plugin Deployment's own security/resources. No Forgejo, application or cluster-IaC access. The CPU baseline below is operator-supplied evidence, not measured here.

## Review findings and decisions

Alpha.11 emits job requests **100m CPU / 128Mi memory / 256Mi ephemeral** and limits **1 CPU / 1Gi memory / 2Gi ephemeral**. Only the ephemeral values and workspace cap are configurable. None of these exact numbers is a Runner mechanic. The reported four-core job had cpu.max `100000 100000`, Node availableParallelism1, throttled periods37–39% and CPU PSI some avg10/60/300 about67/58/52%, with negligible I/O/memory PSI. This supports removing the arbitrary one-core ceiling, not diagnosing Vitest.

CPU is compressible: a request informs placement and contention weight, while a CPU limit throttles available bursts. Keep a modest **100m scheduling default** (not a reservation of one core and not a ceiling), expose its operator choice, and **omit CPU limit by default**. A contention-aware operator can raise requests without setting an equal limit. The same review finds fixed memory values deserve operator control: retain **128Mi request / 1Gi limit** as lightweight scheduling and finite node-memory protection defaults, not required workload sizes. Memory limits can cause OOM kills and unlimited memory exposes node pressure; explicit omission is an operator decision, not inferred from CPU's compressibility.

Disk ephemeral requests/limits/workspace caps remain configurable positive bounds: disk pressure/eviction is different from CPU throttling. Keep default256Mi/2Gi/1Gi and workspace<ephemeral limit validation. No new storage subsystem or generic resource language.

Remove explicit **job** IfNotPresent pull policy. There is no executor reason to suppress Kubernetes' normal :latest/untagged Always default. Pinned DinD IfNotPresent remains unchanged. No additional scheduling/DNS/security/lifecycle knobs are justified.

## Complete inventory

Classes: **1 mechanic**, **2 isolation**, **3 operator policy**, **4 image/workload**, **5 Kubernetes/runtime default**, **6 fixed-DinD requirement**, **7 historical assumption removed**. Mixed rows distinguish purpose from numeric policy. Values describe the resulting local implementation unless marked former.

| Field / behavior | Class | Disposition and reason |
| --- | --- | --- |
| Pod metadata.name deterministic `fj-exec-<hash>`; namespace | 1 / 3 | Deterministic Runner environment identity and configured JOB_NAMESPACE; one Pod per job. |
| Managed-by/execution-id labels; Runner-name annotation | 1 | Ownership check, orphan tracing and safe idempotent Remove; no automatic orphan sweeper. |
| Job-template SHA256 annotation; DinD template annotation when enabled | 1 | Intended pre-defaulted PodSpec retry collision check, including resources; no secret/spec dump. |
| Container names `job`, optional `dind` | 1 / 6 | Stable exec/copy/readiness targets; no services or arbitrary sidecars/init containers. |
| Image selection | 1 / 4 / 3 | Create.image → label suffix → JOB_IMAGE fallback; image must supply existing execution tools and architecture. |
| Job imagePullPolicy | 5 / 7 | **Now omitted**; former hard-coded IfNotPresent incorrectly overrode latest/untagged defaults. |
| Job command | 1 | `/bin/sh -c 'mkdir -p /shared/{act,toolcache,workdir,tmp} && exec sleep infinity'` expressed as existing literal paths. Keeps an exec-able CI environment; not application ENTRYPOINT/CMD. |
| Job args / workingDir | 4 / 5 | Omitted. Runner argv/cwd for each Exec are implemented explicitly; do not promise full application WORKDIR/startup semantics. |
| Job env/envFrom without DinD | 4 / 5 | No Pod env injected. Effective image/runtime env discovered directly with env -0, not registry inspection or entrypoint execution. |
| Env discovery/Exec precedence | 1 | 30s/64KiB NUL probe at Start/every Exec; inherited env → stored daemon defaults → absent-only PATH/HOME fallback → explicit Runner Exec. Empty values reaching wire preserved; Runner's empty/PATH merge limitations remain. No persisted secret cache. |
| CPU request | 3 | JOB_CPU_REQUEST default100m; `none` omits. Scheduling/contention choice, not a CPU ceiling. |
| CPU limit | 3 / 7 | JOB_CPU_LIMIT **omitted by default**; positive value opts into throttling; `none` also omits. Former1CPU not required. |
| Memory request / limit | 3 (limit also2) | JOB_MEMORY_REQUEST/LIMIT default128Mi/1Gi; either accepts `none`. Memory placement/OOM safety, not CPU-style burst policy. |
| Ephemeral request / limit | 3 (limit also2) | Existing JOB_EPHEMERAL_STORAGE_REQUEST/LIMIT default256Mi/2Gi; positive quantities, request≤limit. Writable layers/logs and volume use affect eviction. |
| Pod-level resources / resize policy / hugepages / extended resources | 5 | Omitted; no generic field/resource passthrough. |
| Pod/job runAsUser, runAsGroup, runAsNonRoot | 4 / 5 | Omitted: image-native identity; no fixed UID, non-root enforcement or synthetic NSS records. |
| Pod supplementalGroups / supplementalGroupsPolicy / fsGroupChangePolicy | 5 | Omitted; runtime image memberships/default handling. No Strict feature assumption. |
| Pod fsGroup | 6 | 10001 **only with fixed DinD**, for0660 socket membership independent of image primary GID. Ordinary world-writable emptyDir access needs no forced group. |
| Job capabilities | 5 | Omitted: runtime/admission baseline, no add/drop list or security modes. |
| Job privileged:false | 2 | Explicit non-privileged job. Root image identity is not privileged execution. |
| Job allowPrivilegeEscalation:false | 2 | No-new-privileges prevents set-ID/file-cap exec gaining privileges beyond current process; ordinary root package/user operations passed locally. |
| Pod seccomp RuntimeDefault | 2 | Intentional syscall confinement with runtime-defined implementation; job inherits, not a custom filter or Unconfined job default. |
| Job AppArmor | 3 / 2 | Existing unset/runtime-default/unconfined/localhost operator mechanism retained, job-only. Accepted runtime signaling workaround not revisited or made default. |
| readOnlyRootFilesystem | 4 / 5 | Omitted (writable): ordinary image/package/tmp paths may need writes; no forced immutable rootfs. |
| SELinux, Windows, procMount, sysctls, user namespaces and other security options | 5 | Omitted; no generic securityContext configuration. |
| automountServiceAccountToken:false | 2 | No Kubernetes credential supplied to jobs/daemon; plugin credentials remain only in plugin Pod. |
| serviceAccountName / imagePullSecrets | 5 | Omitted; namespace defaults/admission may apply. No credential feature introduced. Token automount remains explicitly false. |
| restartPolicy:Never | 1 | Must not restart failed job/daemon into a changed execution environment or rerun CI startup against mutable state. |
| activeDeadlineSeconds | 1 | Positive Runner environment timeout rounded up to seconds; omitted if absent/nonpositive. Hash excludes varying remaining lifetime. Deadline kills workloads, not TTL deletion. |
| terminationGracePeriodSeconds / lifecycle hooks / TTL | 5 | Pod field/hooks omitted (normal API grace default); no Job object/TTL controller. Explicit Remove requests grace0 as proven cleanup mechanic. |
| Remove deletion / cleanup timeout | 1 / 2 | Explicit Runner Remove only; bounded30s API cleanup despite cancelled caller, idempotent, zero deletion grace. Do not delete on failed/cancelled streams. Deletion not watched to completion; crashes/API uncertainty can orphan Pods. |
| Start readiness / startup timeout | 1 / 6 | Job Ready; fixed daemon docker-info probe Ready; terminal/pull/scheduling errors detected;3min bound. No job probe invented, no service health or app supervision. |
| Init / ephemeral containers / readiness gates | 5 | Omitted; no permissions helper, startup probe, init daemon or generic injection API. |
| Node selector kubernetes.io/arch | 1 / 3 | Existing JOB_ARCH arm64(default)/amd64 ensures actual architecture matches advertised Runner environment/toolcache architecture. No silent removal of that functional contract. |
| affinity, anti-affinity, topology spread, tolerations, priorityClassName, schedulerName, nodeName | 5 | Omitted. Default scheduler/admission chooses placement; no new knobs for absent fields. |
| hostNetwork/hostPID/hostIPC/shareProcessNamespace | 2 / 5 | Not enabled (normal isolated defaults); never exposed as toggles. No hostPath/device mount. |
| DNSPolicy/DNSConfig/hostname/subdomain/hostAliases | 5 | Omitted: normal ClusterFirst/name behavior after defaulting. No workflow service aliases. |
| Job ports/hostPorts / Kubernetes Service | 5 / 2 | Omitted: exec uses Kubernetes API, no job listener publication/host exposure. Plugin's own50051 Service is separate. |
| enableServiceLinks | 5 | Omitted; normal Kubernetes service env may be discovered. Not an authentication feature. |
| Workspace volume | 1 / 3 / 2 | One disk-backed emptyDir (medium omitted), default1Gi cap/configurable. Ephemeral per-job, no PVC/host path/persistent cross-job state. |
| `/shared` and `/workspace` mounts | 1 | Two aliases of one capped volume, not two budgets; Runner staging, Actions/tar paths and existing workdir contract. Default writable mounts; no subPath/bidirectional propagation. |
| Temp paths | 1 / 6 | Runner TempPath/shared tmp staging. Private image /tmp stays private; TMPDIR=/shared/tmp only for DinD shared binds. No tmpfs/dev-shm policy invented. |
| EmptyDir mode/ACL/preparation | 5 / 4 | No helper/new hardening. Root0777; fsGroup can produce02777 with DinD. Accepted world-write inside private Pod, not host/cross-job sharing. Workload owns its child modes/umask. |

### Fixed DinD inventory (unchanged)

| Field | Class | Disposition |
| --- | --- | --- |
| Operator enabled flag; required digest-pinned image | 3 / 2 / 6 | Default-off JOB_DIND_ENABLED; job requests cannot enable/replace daemon. Existing digest validator retained. |
| CPU request/limit | 3 / 6 | Existing JOB_DIND_CPU_REQUEST/LIMIT default100m/2; both positive/configurable. Not changed by job CPU omission. |
| Memory request/limit | 3 / 6 | Existing JOB_DIND_MEMORY_REQUEST/LIMIT128Mi/2Gi. |
| Ephemeral request/limit | 3 / 2 / 6 | Existing JOB_DIND_EPHEMERAL_STORAGE_REQUEST/LIMIT1Gi/12Gi. |
| Data emptyDir cap / exact mount | 3 / 2 / 6 | JOB_DIND_DATA_SIZE_LIMIT10Gi; exact /var/lib/docker defeats image VOLUME shadowing. Data+socket cap<daemon ephemeral limit. |
| Socket emptyDir cap / mounts | 2 / 6 | Fixed1Mi, /run/forgejo-docker in job+daemon, no host socket; finite socket filesystem budget not a traffic-volume limit. No demonstrated need for another setting. |
| Shared workspace aliases | 1 / 6 | Same volume/mount paths for daemon-visible binds; private /tmp not shared. |
| Daemon args | 2 / 6 | Unix-only host, group10001, data root/exec root fixed; optional existing default/overlay2/vfs storage driver. No wildcard TCP listener. |
| Daemon ENTRYPOINT/command/workdir/pull policy | 4 / 6 | Command/workdir omitted, image entrypoint retained; Args configure dockerd; IfNotPresent matches immutable digest's Kubernetes default. |
| DOCKER_TLS_CERTDIR empty | 6 | Disables official entrypoint TLS/TCP behavior; fixed private Unix API. |
| Job DOCKER_HOST/TMPDIR defaults | 6 | Stored Pod defaults survive plugin restart; explicit Runner Exec env can override endpoint/temp choice but not daemon provisioning. |
| privileged/root/GID0/non-root:false/escalation:true | 2 exception / 6 | Nested Docker functional requirement; daemon alone, no host mounts/namespaces/token. Socket control is a powerful trusted-CI surface. |
| Daemon seccomp/AppArmor Unconfined | 6 | Established daemon-only overrides, no ordinary job policy change. |
| Capabilities/read-only rootfs/other security fields | 5 / 6 | Unset; privileged runtime and writable daemon data requirements. |
| Readiness exec docker info | 1 / 6 | Existing timeout3s/period2s/failure3/success1, Start gate. No liveness/startup probe/restart redesign. |

All other generated PodSpec/container fields remain unset/zero unless listed. API/admission may default or mutate them; the pre-default inventory is not a promise of the final admitted Pod.

## Operator resource contract

New **four** settings, instance-wide like existing JOB_* settings, never per-workflow:

| Setting | Empty/unset | Explicit choice |
| --- | --- | --- |
| JOB_CPU_REQUEST |100m| Positive CPU quantity, or exact `none` to omit |
| JOB_CPU_LIMIT |**omitted**| Positive CPU quantity, or `none` to omit |
| JOB_MEMORY_REQUEST |128Mi| Positive byte quantity, or `none` to omit |
| JOB_MEMORY_LIMIT |1Gi| Positive byte quantity, or `none` to omit (operator accepts node-memory risk) |

Requests and limits are independent. Validate quantities/positive values, CPU precision≥1m, and request≤limit **where both exist**. Empty selects defaults; `none` removes the field, not a zero limit. Existing storage values remain positive and bounded; no `none` storage feature added. DinD configuration/positive validation is unchanged. Namespace LimitRange/ResourceQuota/admission can supply defaults or reject omitted limits; when a limit exists with no request, Kubernetes can copy the limit to the request. Inspect admitted Pods, not only plugin output. Parent cgroup limits/cpusets and runtime placement may still constrain bursts. No CPU request or omitted limit promises four available cores.

Example for a job scheduling estimate without a CPU cap:

```yaml
- name: JOB_CPU_REQUEST
  value: '500m'
# JOB_CPU_LIMIT omitted: no plugin CPU limit
- name: JOB_MEMORY_REQUEST
  value: '512Mi'
- name: JOB_MEMORY_LIMIT
  value: '2Gi'
```

These are **job** settings, not the plugin server Deployment or fixed daemon budgets. Burstable jobs still compete for CPU; higher concurrency/more workers can increase memory use, so measure rather than equating requests/limits or raising every budget.

## Research evidence and limits

- [Kubernetes resource semantics](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/): CPU request placement/relative weight, CPU-limit throttling, memory OOM handling, request copying from limit, CPU milliprecision, disk ephemeral eviction/accounting.
- [Image pull defaults](https://kubernetes.io/docs/concepts/containers/images/), [pinned API defaulting v1.37.1](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/apis/core/v1/defaults.go): latest/untagged Always vs other tags/digests IfNotPresent, DNSClusterFirst, scheduler default, normal30s termination grace, service links and request defaulting.
- [Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/): restart policies, grace/deletion and terminal states; deadline termination is not garbage collection.
- Pinned Runner v13.2.0 locally available source: plugin.proto lifecycle/Create/Exec fields, adapter.go/startPluginEnvironment as recorded in [executor-model.md](executor-model.md). No per-workflow resource field/API is invented.
- All producer code audited: server.go Config/Create/podSpec/Start/Remove; dind.go; environment.go/transport.go; main.jobConfig. API/client-go v0.37.1 and Kubernetes source v1.37.1 are research versions, not assumed cluster version/features.

## Implementation and local validation

Implemented `resources.go` ordinary-job quantity/omission policy and four main.jobConfig bindings; podSpec now uses validated ResourceRequirements and omits job pull policy. No production DinD/transport/cleanup/identity/security changes. Resource maps remain part of deterministic pre-default template hashing.

Tests in `resources_test.go` cover default/custom/none CPU and memory pairs, JSON CPU-limit omission, positive quantities/milliprecision/relationships rejected at startup, resource retry conflicts with/without DinD, equivalent absent policy hashes, independent job/daemon budgets, pull-policy omission for latest/untagged/tag/digest and unchanged daemon policy, plus absence of unnecessary lifecycle/scheduling/DNS/host overrides. Main env tests and existing Pod/identity specs were updated for two default limits instead of three.

Passed `go test ./...`, `go test -race ./...`, `go vet ./...`, twenty focused race repetitions including resources/liveness/lifecycle/transfers/DinD, existing opt-in local Docker identity/transfer/actual-fixed-dockerd/package suite, formatting/diff, diagnostic syntax and static Linux ARM64 cross-build. Pinned/imported Runner proto hashes still match `961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`; no go.mod/go.sum/generated/protocol change. Local Docker diagnostics are not the CPU acceptance experiment or proof of target Kubernetes enforcement. The audit was reviewed and prerelease publication explicitly authorized. Local validation does not establish cluster success.

## Breaking changes and separate acceptance

**Alpha.12:** job CPU limit defaults to absent, four CPU/memory controls added, job pull policy now omitted (latest/untagged can pull anew). No AppArmor/DinD/lifecycle/workspace/helper/service redesign. Drain old jobs before deploying: templates change and retries correctly conflict; do not migrate live Pods; deploy only a reviewed immutable release digest after separate deployment authorization.

After separately authorized deployment of the reviewed immutable prerelease:

1. Retain accepted job AppArmor, fixed DinD, namespace/storage and job-to-plugin ingress isolation. Leave JOB_CPU_LIMIT unset and start with default requests; check LimitRange/Quota/mutating admission for CPU-limit injection. No application test/worker/Firefox/Vitest changes.
2. Run **the exact same Vitest workflow** used for alpha.11 on the same four-core node/image/runtime/workload, controlling competing node load, inputs and caches. Capture admitted job resources and image/plugin digests, CPU/memory limits/requests, ancestor cpu.max/cpuset.cpus.effective, Node os.cpus().length and os.availableParallelism(). The ordinary job's cpu.max should no longer show the plugin's100000/100000 ceiling; expected availableParallelism depends on actual affinity/parent constraints.
3. At comparable workload phases and full duration, sample cpu.stat (delta nr_throttled/nr_periods, throttled_usec), CPU PSI some avg10/60/300 and total delta, wall time/test outcomes; record memory/I/O PSI, memory.current/peak/events and node competition. Compare with supplied alpha.11 baseline37–39% throttled periods, PSI67/58/52%, availableParallelism1. Do not claim improvement merely from an omitted YAML key or different runtime/cache state.
4. Separately exercise explicit CPU request and a deliberate CPU limit (e.g.1) to verify scheduling/expected throttling, memory override behavior and validation/conflict handling. Node may still report differently on other libuv/runtime versions; record versions, not assumed values.
5. Confirm Actions/transfers/aliases, root/named/numeric identities, quiet/nonzero/cancel/deadline and explicit cleanup remain; verify existing fixed-DinD readiness/socket/build/cache/publication/capped-data accounting/cleanup independently. No additional privileges, credentials, host namespaces or services.
6. Review results before rollout. Publication is not deployment or a real cluster experiment; defaults do not solve node pressure, concurrency policy, missing NSS or full workflow-service support.
