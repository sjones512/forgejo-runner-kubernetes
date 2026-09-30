# OCI image building with per-job Docker-in-Docker: research and proposal

**Status: the operator reports alpha.8's architecture validated on real ARM64 k3s (readiness, Unix-only API, native build/load/run, shared binds, isolation and cleanup); data-volume acceptance failed.** The narrowly scoped correction mounts the capped data `emptyDir` directly at `/var/lib/docker`; corrected storage enforcement remains for cluster validation. Protocol/DinD research below is retained. No cluster-IaC, pi-wright workflow or node configuration is changed. The shared CI namespace may use **Privileged PSA**, since verification also needs settings incompatible with Restricted; the prior Restricted namespace split is withdrawn. `JOB_DIND_ENABLED=true` now gives every job Pod a fixed operator-pinned daemon sidecar without making the job container privileged. This is not a workaround for containerd/containerd#12886. See [cluster-integration.md](cluster-integration.md#job-local-docker) for the implemented configuration, resource defaults and deployment gates.

## Recommendation in brief

The approved and implemented model is **one plugin instance, one shared CI namespace permitting Privileged PSA, and one fixed operator-controlled DinD sidecar in every generated job Pod** when explicitly enabled. Enable this globally for the homelab instance rather than introducing multiple plugin instances, labels or execution classes first. Keep it an explicit instance-wide opt-in for the public plugin, not a change to its default. The daemon belongs to that job only. Connect through a Unix socket on a bounded `emptyDir`; never mount a node runtime socket. Give the daemon its own bounded ephemeral data storage. Share workspace at identical `/shared` and `/workspace` paths in both containers, and provide `TMPDIR=/shared/tmp` for shared temporary fixtures.

**Forgejo Runner v13.2.0 does send services to plugins.** The protocol naturally permits a sidecar, but only sends a small subset of workflow service configuration. It does not send service privileges, daemon arguments, health options, volumes or registry credentials. These need an explicit operator policy, not a reinterpretation of arbitrary Docker `options:`.

This is analogous to ARC's explicit `containerMode: dind`: the **operator selects one uniform environment**, and workflows need not declare a service or select a build class. Generic Forgejo service support can remain rejected, as it is today. Keep the service/protocol findings below: workflow-declared DinD and selective classes are technically possible, but are not needed for this first design. At low homelab concurrency, simplicity plausibly outweighs unused-daemon cost; Section 6 quantifies the scheduling and privilege-exposure tradeoff without inventing idle benchmarks.

Privileged DinD is the boring, conventional solution for this controlled homelab. **It is not a strong boundary against malicious repository code.** Its daemon effectively loses seccomp/AppArmor/capability confinement even if the job container retains those settings. It needs PSA **Privileged**, not Baseline. If an enforceable guarantee of node isolation or resource containment against hostile code becomes a requirement, this design is insufficient; a separate disposable build machine/VM is a different milestone.

## 1. Exact Runner/protocol target and evidence

Source inspected locally at Runner **v13.2.0**, commit **`df6b843fb929bb04b933b09c6bf208774d42480b`**. The proto SHA256 is **`961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`**, matching this plugin's target. References below use that commit rather than upstream HEAD.

- [R1: `plugin.proto`](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/plugin/proto/v1alpha/plugin.proto): `BackendPlugin` lines 40–80; `ServiceContainer` lines 92–101; `CreateRequest` lines 103–124; `StartComplete` lines 167–171; `ExecRequest` lines 173–184.
- [R2: `act/plugin/adapter.go`](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/plugin/adapter.go): `AddServiceContainerRaw` lines 79–86; networking/container-action flags lines 92–102; `newCreateRequest`/`Create` lines 176–221; `Start` lines 245–298; `UpdateFromImageEnv`, `IsHealthy`, `Remove` lines 550–597. `adapter_test.go` also asserts the image/env/ports/backend-options mapping in `Create` tests.
- [R3: `act/runner/run_context.go`](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/runner/run_context.go): Docker service handling lines 557–689, Docker startup lines 748–790, service health/lifecycle lines 893–952; **plugin dispatch/startup** lines 1010–1161; `containerImage`/`runsOnImage` lines 1308–1350; `getJobContext` lines 1498–1509.
- [R4: `act/model/workflow.go`](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/model/workflow.go): `Job.Services` line 226, `Job.Container()` lines 318 onward, `ContainerSpec` lines 630–647. [JobContext](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/model/job_context.go) defines services but no dynamic port-result map.
- [R5: `internal/pkg/labels/labels.go`](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/internal/pkg/labels/labels.go): `Parse` lines 34–80, query-option validation lines 101–113, `PickPlatform` lines 169–190.
- [R6: operator plugin configuration](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/internal/pkg/config/config.go): `serializedPluginSettings` lines 419–422 (`address`, `options`); `validateLabelScheme` lines 582–593. [Runner mapping](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/internal/app/run/runner.go) lines 422–433 copies options verbatim. [Client](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/plugin/client.go) `validateCapabilities` only requires a nonempty backend name; it does not require that name to equal the configured scheme.
- [R7: cleanup](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/runner/job_executor.go) lines 153–200; [step environment](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/runner/step.go) `mergeEnv`, lines 318–330; [Docker health checks](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b/act/container/docker/run.go) lines 243–273.

### What actually crosses the plugin boundary

| Workflow/operator input | Plugin path in v13.2.0 |
| --- | --- |
| `jobs.<id>.container.image` | Interpolated by `containerImage()` → `CreateRequest.image` **field 1**. Empty if no job container. |
| `container.env` | Merged into step environments → `ExecRequest.env` **field 3**, not container startup env in `Create`. |
| `container.options`, `volumes`, `ports`, `credentials`, `entrypoint`, `cmd`, `init`, `tty` | Not forwarded as such by `startPluginEnvironment`; do not assume Docker executor handling applies. |
| `jobs.<id>.services.<service-id>.image` | Interpolated, skipped if empty; → `CreateRequest.services` **field 7**, containing `ServiceContainer.image` **field 2**. |
| Service ID | `ServiceContainer.name` **field 1**. Not a Pod/container ID assigned by Runner. |
| Service env | Each value interpolated → `ServiceContainer.env` **field 3**, map of strings. |
| Service ports | Each string interpolated → `ServiceContainer.ports` **field 4**. Proto calls these host:container mappings; plugin startup does **not** parse/resolve them like Docker startup. |
| Service `options`, health flags, `credentials`, `volumes`, entrypoint/cmd/init/tty | **Absent from service proto and omitted by plugin startup.** In particular `options: --privileged` does not enable anything here. |
| Runner global cap add/drop | `CreateRequest.cap_add` **5** / `cap_drop` **6**; advisory, not per service. Not a `privileged` field. |
| Runner `plugins.<scheme>.options` | `CreateRequest.backend_options` **field 8**; operator configuration. Despite proto commentary saying per-label, actual config is per configured plugin key, not a workflow job field. |
| Selected label suffix | `CreateRequest.label_arg` **field 9**. Human `runs-on` label name is **not** sent. |
| Lifetime | `CreateRequest.environment_timeout` **field 10**, shortened to the job deadline by the adapter when applicable. |

The job/service model is richer than the plugin wire model. A plugin cannot reject service options it never receives or honor `services.credentials` it never sees. Documentation must explicitly identify these unsupported semantics; do not claim full service-container parity.

## 2. Service lifecycle, health, credentials and networking

**Plugin path (R1–R3, R7):** Runner selects a plugin, connects, checks plugin gRPC health, asks `Capabilities`, builds **one environment**, accumulates services with `AddServiceContainerRaw`, then issues **one `Create`** containing all services. It calls `Start` for that environment and expects `StartComplete`, followed by job `CopyIn`/`Exec`/`CopyOut` and one `Remove` at cleanup (also following cancellation/startup failure where an environment was created). There are **no separate service Create/Start/Exec/Remove RPCs**, service identifiers for later RPCs, per-service status events, or service health RPCs. `CapabilitiesResponse` only supplies the backend name, with no service-support negotiation flag. `Exec` addresses an environment, not a named service.

A backend decides whether these become containers in one Pod, separate Pods, VM processes, etc. One Pod with job + DinD fits **naturally**; it is not prohibited by the single environment ID. Kubernetes deleting that Pod removes both containers and their `emptyDir` storage. Cleanup must still handle partial provisioning and startup failures. The current plugin does not watch deletion to completion or sweep orphaned Pods; privileged orphan exposure deserves explicit operator attention in later validation.

**Health:** `pluginEnvironment.IsHealthy()` is a no-op. Plugin gRPC health measures plugin availability, not daemon readiness. The plugin's `Start` must ensure the daemon API is usable before `StartComplete`; the implementation uses kubelet's bounded `docker --host=<socket> info` readiness probe and waits for both containers' Ready status; merely seeing a Running Pod or a started `dockerd` process is insufficient. Do not depend on OCI `HEALTHCHECK` being translated by Kubernetes. No workflow health flags reach this plugin path.

**Networking:** `ManagesOwnNetworking()` is true; Runner does not attach a Docker network. In one Kubernetes Pod, containers share the Pod network namespace/localhost. They do **not** get service-ID DNS names just from Kubernetes container names. A plugin must define any aliases and port semantics; no protocol response reports dynamic mapped service ports. `getJobContext()` returns status only, so do not rely on `${{ job.services.docker.ports[...] }}`. The recommended Unix endpoint avoids service-name and port-mapping dependencies altogether.

**Pull credentials:** The plugin path never calls Docker-path `handleServiceCredentials()`. Neither service nor job pull credentials are in `Create`. Initially use public, immutable ARM64-capable images. Operator-managed Kubernetes `imagePullSecrets` could be a later explicit capability (kubelet uses those without mounting them in repository containers), but workflow `services.credentials` cannot be transparently implemented on this wire contract. Registry push/login credentials supplied to the workflow are separate from kubelet image-pull credentials and from Kubernetes API credentials.

**Docker path contrast:** `prepareJobContainer` separately creates Docker service containers, parses ports, uses service credentials/volumes/commands/options, assigns network aliases; `startJobContainer` creates a Docker bridge, starts services and job, then polls `IsHealthy`; cleanup removes those containers/network. These paths are **not used for a plugin job**. Also, `SupportsDockerContainerActions()` stays false: a shell step using Docker CLI is possible without making `uses: docker://...` or Docker-based Actions supported.

## 3. Viable execution shapes

| Shape | Fit and tradeoffs |
| --- | --- |
| **Job + rootful DinD in the same Pod** | Recommended. Conventional GitLab/ARC pattern. One environment, one Pod delete, shared loopback, job keeps its direct security settings; privileged daemon gets container-specific overrides. Unix `emptyDir` socket is job-local, not a node socket. |
| Workflow-declared DinD service | Fits `Create.services`. Needs an operator-approved policy that knows which service gets privilege, how to run it, required shared mounts and readiness. A narrowly supported/allowlisted DinD service is feasible; generic services, port translation, private pulls and Docker options are unnecessary extra scope for the first build milestone. |
| **Operator-managed fixed DinD sidecar** | Recommended smallest initial feature. An explicitly enabled plugin instance injects it into **every generated job Pod**, with operator-pinned image/arguments/storage. Like ARC's explicit DinD mode, not covert behavior. Disabling the instance-wide feature preserves today's single-container default. Keep rejecting general services rather than silently ignoring them. A second conflicting workflow DinD declaration should fail, not start two daemons. |
| Same-container rootful DinD | Technically viable but a worse default. Every job command shares the privileged/root-capable outer container. Current plugin overrides image entrypoint with `mkdir … && exec sleep infinity`, so just selecting `docker:dind` **does not start Docker**, and its UID 10001/capability settings also prevent ordinary rootful daemon startup. Requires a dedicated privileged startup policy or a workflow startup step/background daemon and a suitable image. Readiness, PID 1/reaping and shutdown still need handling. Subsequent `Exec`s can use the same persistent daemon; shell exports alone do not persist between steps. Less Pod wiring, but more job/image coupling and broader direct privilege. |
| Rootless DinD sidecar | Official `docker:dind-rootless` is **still conventionally privileged** when nested; Docker and ARC explicitly say so. Requires subordinate UID/GID mappings, user namespaces, writable HOME/runtime directory and rootless networking; cgroup resource controls have limitations. It is not a way to retain Restricted PSA with unchanged Docker behavior. Not recommended as the smaller initial solution. |
| Separate per-job daemon Pod | Possible by plugin choice, but unnecessarily awkward for these tests: TCP/TLS, separate lifetime, remote bind-path visibility, and loopback/host-network assumptions. Woodpecker uses this general pattern, but it does not automatically fit pi-wright. |

A build-only daemonless exporter/direct BuildKit arrangement does not by itself provide Docker `run`, `exec`, named volumes, image inspect and restart behavior needed by the existing smoke tests. Replacing Docker is not the smaller answer for this milestone; no general OCI-builder survey is needed.

## 4. Proposed Pod-level architecture

```text
trusted Runner -> one operator-configured plugin -> shared CI namespace Pod
  job: retain explicit settings unless separately reconsidered;
       DinD itself does not require changing the job's direct security context
    docker CLI + buildx + Node + required job utilities
    DOCKER_HOST=unix:///run/forgejo-docker/docker.sock
  dind: pinned Docker daemon, rootful, privileged
    only the job-local Unix endpoint; daemon persists across steps
  bounded ephemeral volumes:
    shared workspace at the same /shared and /workspace paths
    common TMPDIR=/shared/tmp inside that existing workspace
    shared socket directory (not all of /var/run)
    daemon state (not shared into job container)
  no hostPath, host runtime sockets, host PID/network, or SA token
```

The daemon sidecar uses its real image entrypoint with **explicit `dockerd` arguments for Unix-only listening**. The official entrypoint prepends wildcard TCP 2375/2376 when invoked with no daemon arguments or only flags; merely omitting a Kubernetes Service is not enough to make that endpoint private. Prefer `dockerd --host=unix:///run/forgejo-docker/docker.sock` and arrange the socket group/mode so UID 10001's available group can connect (for example root:10001, 0660). Do not make it world-writable or expose a Service/hostPort. If TCP is chosen instead, bind loopback only or use per-job mutually authenticated TLS; never an unauthenticated wildcard endpoint. ARC demonstrates the Unix-only approach.

A daemon socket grants control of **that daemon**, so its restricted client is not a security barrier against abuse of the privileged sidecar. Retaining client hardening is still useful least privilege for ordinary processes, not a promise that client code cannot obtain daemon privilege.

`CopyIn`, `CopyOut` and Runner `Exec` stay directed at container **`job`**, as today. Advertise the Docker endpoint/defaults through the plugin's environment contract (`StartComplete.image_env` is available) or explicit instance-wide DinD defaults; do not rely on a one-time shell export or arbitrary job-image ENV discovery. Give **every job** its own daemon and state—no sharing across jobs or a persistent daemon service. The daemon image supplies the CLI used for its readiness probe; verification job images need not acquire Docker CLI merely because they get the sidecar. Publishing images still need Docker CLI/buildx for workflow commands.

Use an ordinary sidecar under Pod `restartPolicy: Never` for the smallest fail-fast design, with explicit daemon readiness in `Start`. A Kubernetes native restartable sidecar with startup probe is also conventional (ARC uses it on Kubernetes >=1.29), but automatic daemon restart can lose build state; decide lifecycle intentionally rather than copying ARC wholesale. Readiness/death detection must cover the daemon too, not only the current `job` container status.

## 5. Actual security and host-facility requirements

This table describes **conventional rootful Docker DinD**, not a claimed minimal custom-capability sandbox. Its job-column settings are the current plugin baseline, **not** a requirement that future verification remain Restricted; the operator's separate verification-capability decisions are not designed here. GitLab, ARC and the official DinD wrapper all prescribe privileged mode. Kubernetes API types already used by this plugin (`k8s.io/api v0.37.1`, `core/v1.SecurityContext`) support per-container overrides of Pod UID/nonroot/seccomp/AppArmor defaults.

| Setting/facility | Job container | Rootful DinD daemon sidecar |
| --- | --- | --- |
| `privileged` | false/unset | **true** for the conventional recipe. |
| UID / nonroot | Preserve job UID 10001 and nonroot; retain FSGroup behavior. | Override inherited `runAsUser` to **0**, `runAsNonRoot` to **false**; use an appropriate root group. Do not set the whole Pod to root. |
| Capabilities | Keep drop ALL. | Privileged grants **all capabilities**. Do not pretend drop ALL or a handpicked additional `CAP_KILL`/`SYS_ADMIN` is equivalent to tested DinD support. No minimal-capability set was established here. |
| Privilege escalation | Keep false. | Cannot retain false with privileged mode; use true/omit. Kubernetes documents the incompatibility (and all-capability access is already present). |
| seccomp | Keep **RuntimeDefault**. Pod default can stay RuntimeDefault. | **Effectively Unconfined** because privileged overrides configured seccomp. Explicit daemon-container Unconfined can document intent but is not an extra requirement once privileged. A manifest-level inherited RuntimeDefault does not prove daemon syscall confinement. |
| AppArmor | Preserve independent job/operator choice; this task does not address the existing exec issue. | **No effective AppArmor confinement** in privileged mode. Explicit daemon-container Unconfined is documentary/redundant, not the rootless/exec-issue workaround. |
| `/dev/fuse` | Not required. | No dedicated FUSE device requirement for the conventional rootful kernel-backed storage path. FUSE may matter for alternative/rootless drivers; do not import Podman requirements. Privileged mode nevertheless exposes broad device access. |
| User namespaces | No new requirement. | No rootless/userns-remap requirement for default rootful DinD. Rootless alternatives need subordinate IDs and permitted user namespace creation, not merely UID 10001. |
| Filesystem/mounts | Writable shared workspace/tmp/socket; no daemon data mount needed. | Writable daemon runtime/data, nested mount support, suitable kernel storage driver and filesystem. Bounded disk `emptyDir` data; no PVC/cache or hostPath needed. |
| cgroups | Retain job limits. | Needs working writable/nestable cgroup setup. Official DinD wrapper enables cgroup-v2 subtree controllers and moves initial processes into an `init` cgroup. Validate the actual CRI cgroup namespace/subtree rather than adding a node `/sys/fs/cgroup` hostPath. |
| Networking | Pod network, not host network. | Needs nested bridge/veth/firewall support, generally iptables/netfilter kernel support. Daemon changes operate in the Pod network namespace; no Kubernetes hostNetwork is required. |
| Tokens / host sockets / host namespaces | None. | None. Do not mount the plugin's credentials, node Docker/containerd sockets, host PID/IPC/network or arbitrary node paths. No bidirectional node mount-propagation setup is proposed. |

**Important host exposure:** conventional privileged mode undoes mount masks and grants device/administrative access; absence of explicit hostPath does **not** guarantee the container cannot reach node devices, mount filesystems or affect kernel interfaces. The official DinD wrapper can mount `securityfs` and make its mount namespace shared. These are meaningful risks of privilege itself, not permission to introduce a node AppArmor change or an extra host mount in this project. Keep them visible in review; an actual pinned-image/runtime compatibility test is necessary before approval.

**Resources:** retain explicit CPU/memory/ephemeral requests and limits for **both** containers and bounded workspace/tmp/daemon state volumes. Limits for the current single job container do not budget a second daemon. Account for intermediate build layers, loaded images, smoke-test volumes and nested BuildKit state. Mount the capped volume **directly at `/var/lib/docker`**, not merely `/var/lib`: the official image's declared child `VOLUME /var/lib/docker` shadowed the parent mount in alpha.8. The fixed daemon data-root and mount now share one code constant. Docker 29.8.1's managed containerd defaults to `<Root>/containerd/daemon`; externally configured stores (including `/var/lib/containerd`) are not automatically covered. See the [source evidence and accounting limitations](cluster-integration.md#workspace-storage-and-readiness).

Do not claim these limits constitute a hard resource sandbox for a privileged daemon. GitLab explicitly warns that DinD builds can see full node capacity and Pod limits may not constrain nested builds as expected. Check actual cgroup ancestry and controller accounting for daemon, BuildKit and smoke containers, and exercise limit behavior in a later integration milestone. Namespace quotas/concurrency and a dedicated build node are useful operational controls; they do not prevent a malicious privileged process from attacking the node. If bounded resources means a hostile-code-proof guarantee rather than configured/validated normal-operation limits, privileged DinD does not satisfy it.

## 6. One shared Privileged-PSA namespace: simplicity and cost

**Accepted policy:** use the shared CI namespace with `pod-security.kubernetes.io/enforce: privileged`. Both Restricted and Baseline forbid the conventional privileged daemon; verification's additional capability needs independently make retaining Restricted inappropriate. Privileged PSA permits elevated Pods but does **not** set `privileged: true` on containers. Keep explicit container settings rather than discarding them wholesale. Namespace policy and runtime settings are separate decisions. No namespace labels are changed here.

Use **one plugin Deployment/endpoint and one fixed job namespace**, with namespace-scoped create/get/delete Pods and create pods/exec rights. Keep the plugin itself in the trusted namespace, nonprivileged and reachable only by the trusted Runner. Existing RBAC can manage a multi-container Pod without Kubernetes Services or cluster-scoped rights. RBAC does not restrict Pod contents; a compromised plugin credential in this namespace remains sensitive with or without DinD. No PSA exemptions or new class routing are needed.

### Quantified scheduling comparison (not idle measurements)

**Historical planning model retained below:** the implemented defaults are now daemon requests **100m CPU / 128Mi memory / 1Gi ephemeral storage**, not the 50m/128Mi/256Mi illustrative inputs. Daemon limits are 2 CPU / 2Gi memory / 12Gi storage, data cap 10Gi. Substitute those values into the same equations for deployment planning; actual idle usage is still unmeasured.

There is no dependable portable value for modern ARM64 `dockerd` + its `containerd` idle CPU/memory footprint. **No daemon was launched and no cluster benchmark was performed.** CPU requests are not idle CPU consumption; memory requests are not measured RSS/working set; `emptyDir.sizeLimit` is not disk preallocation.

For transparent planning, suppose each daemon has requests of **50m CPU, 128Mi memory and 256Mi ephemeral storage**. These are **illustrative arithmetic inputs, not validated settings or recommended build limits**. The existing plugin Deployment example requests **50m CPU and 128Mi memory**; a second identically configured single-replica plugin reserves that continuously. Its actual usage is also unmeasured. Shared job-container requests and genuine build resources cancel out of the comparison.

Let `N` be active jobs, `B` the subset needing Docker, and `D` the daemon request vector:

- Every-job sidecar: additional requests = `N × D`.
- Two instances with selective sidecars: additional requests = `B × D + extra plugin requests`.
- Every-job minus selective = `(N − B) × D − extra plugin requests`.
- One instance with selective classes: additional requests = `B × D`; avoids the second Deployment but adds policy/routing complexity.

| Active jobs `N` / Docker jobs `B` | Every-job daemon CPU / memory requests | Selective daemons + extra plugin CPU / memory requests | Every-job minus selective |
| --- | --- | --- | --- |
| 0 / 0 | 0 / 0 | 50m / 128Mi | −50m / −128Mi |
| 1 / 0 | 50m / 128Mi | 50m / 128Mi | Equal requests, **not** equal actual usage |
| 2 / 0 | 100m / 256Mi | 50m / 128Mi | +50m / +128Mi |
| 4 / 1 | 200m / 512Mi | 100m / 256Mi | +100m / +256Mi |
| 4 / 4 | 200m / 512Mi | 250m / 640Mi | −50m / −128Mi |

With these inputs, scheduling break-even is **one active non-Docker job**. Extra daemon ephemeral-storage requests are `(N − B) × 256Mi` before subtracting extra plugin storage requests (none is explicit in the Deployment example). If daemon **memory requests** instead need 256Mi or 512Mi, four non-Docker jobs reserve 1Gi or 2Gi respectively; subtract the extra plugin's 128Mi for the two-instance comparison. Actual memory can be above/below requests; peak builds require separate sizing.

For `H_nonDocker` non-Docker job-hours over `T` hours, extra CPU-request-hours versus two always-running instances are `0.05 × H_nonDocker − 0.05 × T`; extra memory-request-hours are `128Mi × H_nonDocker − 128Mi × T` with the example inputs. These quantify scheduled capacity, **not consumed resources**. Every-job sidecars disappear between jobs; a second plugin usually stays up. Actual-runtime break-even depends on measured usage, not requests.

### Concrete idle/startup overhead

- **Per running job:** an extra outer container/shim, `dockerd`, nested `containerd`, housekeeping, sockets, cgroup/network initialization and logs. A job that never invokes Docker should not pull build bases, run nested containers or proactively bootstrap a `buildx docker-container` builder. It still runs a privileged daemon. Idle CPU/memory and initialized data-directory size are **unmeasured**; no universal 50Mi/100Mi footprint is claimed.
- **Cold image pull:** read-only Docker Registry inspection of `docker:29-dind` found ARM64 compressed layers totaling **124,809,281 bytes (119.03 MiB), 16 layers**. Observed index `sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0`; ARM64 manifest `sha256:2aece977596803b4174bb35171b63007f554867e9931fef6c3b97a10d9517226`. This is pinned observational evidence, **not a chosen deployment image**. Cached layers need no download per job; shared layers reduce incremental cold transfer. Unpacked node image storage and per-container writes are different, unmeasured quantities. Both architectures need this image on Docker nodes; every-job injection broadens which nodes/jobs depend on it.
- **Startup/failure coupling:** every verification job waits for daemon readiness and can fail on image pulls, daemon startup, storage drivers or cgroups even without Docker calls. Numerical latency needs measurement. Unix-only arguments avoid unnecessary TCP/TLS certificate generation but not mount/network/cgroup initialization. Uniform readiness is simpler than lazy startup or per-job fallback.
- **Storage:** unused daemons have initialization/log writes, not active-build layers/test volumes. Volume caps do not reserve full capacity. Node image cache is separate from job `emptyDir`; job workspace limits do not bound additional daemon storage. Keep explicit budgets for both containers and volumes.

A later authorized prototype should measure cgroup working set/CPU after startup and after 5–20 idle minutes, data/log bytes, cold/cached latency, and the second plugin's footprint. Include verification and builds, not only summed process RSS with shared-page double counting. This is a **measurement gate**, not authorization to launch privileged Pods now.

### Security exposure and operational complexity

At concurrency `N/B`, every-job injection creates **N privileged daemons instead of B**: `N − B` extra, even when idle. Verification dependencies can control their own daemon socket and request privileged nested execution. Missing Docker CLI is not a boundary: code can speak the socket API.

Additional privilege exposure over time is the **sum of non-Docker job durations**. Example: one 20-minute verification job and two 10-minute publishing jobs give 20 selective daemon-minutes versus 40 every-job daemon-minutes, **2×**. Peak count remains two if publishing runs in parallel after verify. This measures opportunity/duration, **not** breach probability. If verification is already explicitly privileged, incremental privilege is smaller; incompatible with Restricted does not itself mean full privilege.

Separate instances/classes can reduce unused resources, Docker failure coupling and privilege opportunity. They **do not** restore a Restricted-PSA boundary under the accepted shared-namespace policy or isolate a shared kernel. Two instances add one Deployment, endpoint/Service, configuration/upgrade target, credentials/RBAC bindings, NetworkPolicy selectors and label routing. Selective classes on one instance avoid that infrastructure but add request-policy validation, routing and Pod-spec/testing branches. Every-job injection keeps one configuration, endpoint, namespace and readiness/security template. Retain hard host/socket/token/network boundaries in all designs.

**Revised recommendation:** start with instance-wide fixed DinD for this trusted homelab and bounded concurrency. Do not build multiple instances/classes for a hypothetical PSA split. Reconsider selective injection only if measured pressure, Docker-independent startup failures or changed trust requirements justify it. No lazy/eager hybrid or general service implementation is needed initially.

### Optional selective labels remain protocol-compatible (not the recommendation)

Retained **Runner configuration example** for a possible later selective design, not a deployment proposal for the current one-instance recommendation (R3, R5, R6):

```yaml
plugins:
  kubernetes:
    address: ordinary-plugin.<trusted-namespace>.svc.cluster.local:50051
  kubernetes-build:
    address: build-plugin.<trusted-namespace>.svc.cluster.local:50051
runner:
  labels:
    - 'k3s:kubernetes://<ordinary-job-image>'
    - 'k3s-build:kubernetes-build://<job-image-with-docker-cli-and-buildx>'
```

`runs-on: k3s` routes to the first endpoint; `runs-on: k3s-build` routes to the second. Custom schemes are allowed when their plugin key exists. A nonempty suffix is required by the label parser. The response backend name may remain `kubernetes`; client validation does not require scheme equality.

Crucially, if both labels instead use the same `kubernetes://<same-image>` endpoint, **the plugin cannot tell their human label names apart**. It only sees `image`, `label_arg`, configured `backend_options`, etc. Do not encode privilege implicitly in the image. Keep current selection `Create.image → label_arg → JOB_IMAGE` independent of the endpoint's execution policy, including when `container.image` overrides the default image. Use an operator allowlist if the build policy needs image restrictions.

An alternative is two configured plugin keys pointing at one server with distinct `options` maps; Runner forwards those as `backend_options`. That server would need a new strict allowlisted policy implementation (the current plugin rejects all backend options); classes can use the same accepted CI namespace. This works on the wire but adds a policy/configuration branch that the every-job architecture avoids. There is no supported arbitrary `?privileged=true` label query: label validation rejects unknown options, and `platform` query support is Docker-only. Workflow `container.options: --privileged` also cannot select the plugin class.

## 7. Preserve pi-wright's exact-artifact publishing sequence

Read-only inspection of the local pi-wright checkout at **`2ce6bf7ab359787c03b713e1f06b4b167bc79187`**: `.github/workflows/control-plane.yml` lines 68–169, `scripts/smoke-image.ts`, `scripts/smoke-agent-image.mjs`. The publishing jobs depend on `verify` and build native ARM64 with `docker buildx build --platform linux/arm64 --load`, test the loaded image, then `docker push` **that same tag**, inspect its digest and log out. No workflow/script was changed.

**This ordering can remain.** Use the same per-job daemon and Docker context throughout build, smoke and push. `--load` exports the single-platform result into that daemon's image store; the smoke-test Docker calls and later `push` use the same store. Do not replace it with early `buildx --push`, a registry-first test, or a second rebuild. The two publish jobs should have separate Pods/daemons. Native ARM64 needs an ARM64-capable daemon/job image and ARM64 node placement, not QEMU/binfmt registration on the node. CLI/buildx/daemon versions need to be deliberately compatible and pinned.

The Docker commands can remain **substantially unchanged**, with instance-wide daemon configuration and environment/tool provisioning added later; a separate build-class label is not required. Concrete compatibility requirements from the existing tests:

- Control-plane smoke uses nested named Docker volumes, `run`, `exec`, `inspect`, `stop/start`, and loopback-published ephemeral ports. DinD provides these conventional Docker operations; a mere image exporter does not.
- Agent smoke creates `mkdtemp(path.join(os.tmpdir(), ...))`, binds those fixture paths into nested containers, and checks ownership/group permissions. **Docker bind mounts resolve on the daemon side, not the client.** The implementation sets a common `TMPDIR=/shared/tmp` under identically mounted shared storage; it does **not** share private `/tmp` paths. Sharing only `/workspace` would fail. Paths and UID/GID/group access must agree; do not redesign the tests to hide this.
- Agent smoke starts a TLS server on job `127.0.0.1` and calls nested `docker run --network host`. With a same-Pod **rootful** daemon, Docker's “host” is the daemon's **Pod network namespace**, shared with the job—not the k3s node. This satisfies the ban on Kubernetes `hostNetwork`; do not translate nested `--network host` into that field. Separate daemon Pods or rootless extra network namespaces would not preserve this loopback behavior automatically.
- Agent smoke exercises sudo inside the **nested image**, not the outer job container. Outer job `allowPrivilegeEscalation: false` need not be relaxed merely to run that test: DinD launches nested processes independently. Exact nested behavior remains a later validation gate.
- Registry publication still needs appropriately scoped workflow credentials. GitHub's `GITHUB_TOKEN` package semantics are not automatically portable to Forgejo/GHCR; credential provisioning is separate from this execution design. No Kubernetes credential is needed by repository code.

None of these source-based compatibility arguments proves either image's full build/smoke/push passes through this plugin yet.

## 8. Homelab threat model

Operator-controlled repositories justify choosing the conventional privileged CI pattern over engineering a hostile-public-code sandbox. They do not eliminate compromised dependencies, malicious Dockerfiles or accidental destructive commands.

- **Better than a node runtime socket:** the job daemon owns only nested job containers/images/volumes. It cannot simply enumerate/delete k3s containers via the node's Docker/containerd API because those sockets are absent. Docker bind paths start in the sidecar's filesystem/mount namespace, not an intentionally exported node filesystem. This is materially different from handing over a host runtime socket.
- **Still high privilege:** all capabilities, broad devices and mount/kernel interfaces increase attack surface; kernel/runtime vulnerabilities or administrative device access can compromise the node. The restricted Docker client can ask its daemon to create privileged nested containers or mount daemon-visible paths. Do not market the sidecar as containing hostile code.
- **Cluster credentials:** no SA token in job or daemon, no plugin filesystem/credentials shared with either, no host PID/network/paths. This removes easy authenticated cluster API access; a node compromise can still recover node credentials or affect production workloads. Namespace separation is not kernel isolation.
- **NetworkPolicy:** retain job/build-to-plugin gRPC denial and isolate build Pod ingress/egress as appropriate. A Unix socket avoids accidentally opening a daemon port to other Pods. NetworkPolicy operates on Pod traffic, not between job and sidecar or each nested Docker container; nested traffic needs CNI validation and no bypass claim after a node escape.
- **Ephemerality/resources:** cleanup removes job daemon state and avoids cross-job daemon/cache contamination; quotas, resource limits and concurrency reduce normal damage. They do not undo already-exfiltrated registry secrets or make privileged execution a hard resource/security boundary. Failed cleanup leaves a more sensitive orphan than an ordinary Pod.

For this homelab, the operator is considering privileged DinD **for every job**, including verification, under one explicitly enabled instance-wide policy. Prefer nodes without sensitive production co-tenants where practical. Keep the job container's direct settings independent of DinD; code with daemon socket access is elevated even if its job container is nonprivileged. No requirement in this proposal involves solving the separate AppArmor exec issue, node policy changes, host sockets or cluster-IaC edits.

## 9. First implementation and remaining validation gates

The first focused slice implements:

1. **Operator-only `JOB_DIND_ENABLED` (default false)** and required digest-pinned `JOB_DIND_IMAGE` when enabled. No extra instance, classes, new Runner labels or workflow-controlled privilege.
2. One fixed rootful privileged sidecar with explicit Unconfined sidecar AppArmor/seccomp, dedicated Unix socket, shared workspace/TMPDIR and a single bounded data volume at the exact fixed `/var/lib/docker` data root. Requests/limits and data cap are operator-configurable; optional `JOB_DIND_STORAGE_DRIVER` selects `overlay2`/`vfs`, otherwise the image default. Mandatory socket/group/data/runtime args are fixed, not arbitrary operator/workflow command strings.
3. Probe-backed daemon readiness plus job readiness before `StartComplete`, bounded by startup timeout/cancellation. `DOCKER_HOST` and `TMPDIR` propagate through Start and workflow Exec despite `env -i`; Runner explicit env overrides remain supported. `Exec` and copy still target `job`, with no Docker client on the plugin host.
4. Same-name retries compare container names/images and, when DinD is enabled, an annotation hashing the generated template before API defaulting/lifetime. A changed daemon/security/resource template is not silently reused. One explicit `Remove` deletes the whole Pod; streaming cancellation does not independently delete it.
5. Tests cover disabled/enabled shape, security/host/token boundaries, mounts/defaults, pinned-image/quantity/driver validation, image selection, retry conflicts, readiness success/deadline/terminal failure, cleanup and environment propagation. The existing full Runner step-file lifecycle regression also runs with DinD enabled (fake Pod status/exec).
6. Generic workflow `services`, backend options, per-exec users, added capabilities and container actions remain rejected. The public default is still the original single-container environment. No registry credentials, host sockets, persistence/cache or publishing redesign.

These source/unit guarantees do not prove storage enforcement, nested cgroup limits or publishing success. The operator's separate alpha.8 ARM64 k3s evidence validates the architecture and native build/load/run but identifies the image-volume shadowing defect; the corrected mount is not yet cluster-tested. The exact operator contract is in [cluster-integration.md](cluster-integration.md#job-local-docker).

If a later decision selects workflow-declared DinD instead of instance-wide fixed injection, also parse/validate `Create.services` name/image/env/ports against an operator allowlist, materialize only the supported daemon service, fail unsupported requested semantics visible on the wire, and document omitted options/credentials/health/volume semantics. This still requires the same privilege/readiness/mount policy; it does not require a protocol change.

The original broad cluster acceptance plan is superseded for this milestone by the [storage-focused handoff](cluster-integration.md#workspace-storage-and-readiness): report Docker root, inspect the exact capped volume backing it, repeat native build/load/run, measure data and kubelet/container accounting, safely exercise the bound if practical, and verify cleanup. Do not re-prove the validated architecture unless this correction changes it; inspect the real pi-wright publishing workflow only after storage acceptance.

## 10. External primary references

- [GitLab Runner Kubernetes executor](https://docs.gitlab.com/runner/executors/kubernetes/#using-dockerdind): documents DinD as a **privileged service container in the same Pod**, daemon endpoint/TLS requirements, and the **“Prevent host kernel exposure” resource warning**. [Runner security](https://docs.gitlab.com/runner/security/) discusses trust and privileged execution.
- [GitHub ARC runner scale sets, DinD mode](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets#using-docker-in-docker-mode): privileged rootful DinD, shared Unix socket/work volumes, startup readiness, and rootless still requiring privilege. Its example is a pattern, not a reason to adopt mutable image tags or its exact lifecycle.
- [Woodpecker Kubernetes backend](https://woodpecker-ci.org/docs/administration/configuration/backends/kubernetes): per-step security contexts, privileged detached DinD and TLS/service networking. Its separate-step Pod/service model is not evidence of Forgejo plugin semantics.
- [Docker rootless DinD tips](https://docs.docker.com/engine/security/rootless/tips/#rootless-docker-in-docker): explicitly says `--privileged` is needed to disable seccomp, AppArmor and mount masks, despite nonroot daemon UID. [Rootless requirements](https://docs.docker.com/engine/security/rootless/) and tips describe subordinate IDs and cgroup limitations.
- [Official Docker DinD entrypoint, pinned](https://github.com/docker-library/docker/blob/868418aadf5d09cf67ddd5e11ba01b16b7fe0f53/dockerd-entrypoint.sh); [generated DinD Dockerfile at that commit](https://github.com/docker-library/docker/blob/868418aadf5d09cf67ddd5e11ba01b16b7fe0f53/29/dind/Dockerfile). Its `DIND_COMMIT` points to [this exact wrapper](https://github.com/moby/moby/blob/8d9e3502aba39127e4d12196dae16d306f76993d/hack/dind), which documents privileged mode and implements nested cgroups/mount setup. These are source observations, not a chosen release image digest.
- [Kubernetes Linux security constraints](https://kubernetes.io/docs/concepts/security/linux-kernel-security-constraints/#kernel-level-security-features-and-privileged-containers): privileged overrides seccomp/AppArmor, grants all capabilities. [SecurityContext](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/): container overrides and privilege-escalation semantics. [Pinned API type](https://github.com/kubernetes/api/blob/v0.37.1/core/v1/types.go): `SecurityContext`, including privileged, UID/nonroot, escalation and per-container seccomp/AppArmor fields.
- [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/) and [Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/): namespace levels, Baseline privilege prohibition, Restricted additions, audit/warn and exemptions. Use the policy appropriate to the deployed Kubernetes version; no exemption or label was applied here.
- [Kubernetes resource management](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/): container requests contribute to Pod scheduling; requests and limits are distinct from measured consumption. [Docker runtime metrics](https://docs.docker.com/engine/containers/runmetrics/): cgroup-based CPU/memory accounting. The numerical request model in Section 6 is an explicitly hypothetical comparison, not a benchmark from these sources. Cold-pull bytes were summed directly from `layers[].size` in the public [Docker registry](https://registry-1.docker.io/v2/) ARM64 manifest pinned above; no image layers were pulled or container executed.
- [Docker Buildx build `--load`](https://docs.docker.com/reference/cli/docker/buildx/build/#load): load single-platform result into the Docker image store. [Bind mounts](https://docs.docker.com/engine/storage/bind-mounts/#considerations-and-constraints): source paths are daemon-side. [Host networking](https://docs.docker.com/engine/network/drivers/host/): nested `host` refers to the Docker host's network namespace. [Docker containerd image store](https://docs.docker.com/engine/storage/containerd/): state-directory/version considerations.

**Approved decision / next milestone:** one shared Privileged-PSA CI namespace and one operator-enabled plugin instance adding fixed rootful DinD to every generated job Pod. Defaults remain disabled until the cluster operator opts in. The separate cluster agent must deploy the corrected immutable plugin digest and repeat only the storage-focused acceptance before inspecting the unchanged pi-wright publishing flow. ARM64 architectural compatibility has been reported; corrected data-volume enforcement, actual idle footprint and complete nested resource accounting remain unverified. The preserved resource/exposure models are planning aids, not integration claims.
