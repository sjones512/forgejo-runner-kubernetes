# Cluster integration contract (first experimental test)

This is a deployment/test handoff, **not** a compatibility claim. Plugin target: Forgejo Runner **v13.2.0**, commit `df6b843fb929bb04b933b09c6bf208774d42480b`, `plugin.v1alpha` proto SHA256 `961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`. Use a real Runner of that version. Details of upstream discovery: [research.md](research.md). A first real Forgejo + ARM64 k3s test on v0.1.0-alpha.1 reached Pod creation, ARM64 exec and stdout, but failed after the step due to premature Pod deletion on a cancelled CopyOut. This lifecycle defect was fixed in v0.1.0-alpha.2, which subsequently **passed the minimal ARM64 shell workflow** (multiple steps, stdout, success and exit 42, Pod deletion on both outcomes). Pinned checkout/setup-node Actions subsequently ran on ARM64 k3s, but the first real `pi-wright` verify workload was evicted while installing dependencies because the shared workspace `emptyDir` exceeded its original **1Gi** limit. Kubelet reported successful cleanup and no orphaned job resources; this was **not** a confirmed memory OOM. No passing full verify result is claimed.

Resource and volume-size limits are operator opt-in. See [pod-policy.md](pod-policy.md).

## Cluster-supplied components

| Item | Contract |
| --- | --- |
| Plugin image | Public `ghcr.io/sjones512/forgejo-runner-kubernetes` multiarch OCI image; **pin by published `@sha256:…` manifest-list digest**, not a mutable tag or `latest`. Do not deploy until a digest is actually published. |
| Entrypoint / arguments | Image `ENTRYPOINT ["/plugin"]`; no args. Go binary requires in-cluster ServiceAccount environment and token. |
| Environment | `PLUGIN_LISTEN_ADDR` (default `0.0.0.0:50051`), `JOB_NAMESPACE` (default `forgejo-jobs`), `JOB_IMAGE` (default `ubuntu:24.04`), `JOB_ARCH` (default `arm64`; `amd64` only for disposable local testing), `JOB_CPU_REQUEST` (default100m), `JOB_CPU_LIMIT` (default omitted), `JOB_MEMORY_REQUEST` (default128Mi), `JOB_MEMORY_LIMIT` (default omitted), `JOB_WORKSPACE_SIZE_LIMIT` (default omitted), `JOB_EPHEMERAL_STORAGE_REQUEST` (default `256Mi`), `JOB_EPHEMERAL_STORAGE_LIMIT` (default omitted)—all resource/storage quantities accept positive values or exact `none` to omit— optional `JOB_APPARMOR_PROFILE` (unset by default; see [Job AppArmor](#job-apparmor)). Timeouts are built in: startup 3 min, cleanup API calls 30 sec. No kubeconfig. |
| Ports / health | Plaintext HTTP/2 gRPC over TCP port **50051** by default, `plugin.v1alpha.BackendPlugin` and `grpc.health.v1` (SERVING once listening; NOT_SERVING on graceful shutdown). No HTTP readiness/liveness endpoint; optional gRPC probes only, with no implication that Kubernetes API or job Pods are healthy. |
| Service / exposure | Internal ClusterIP (or similarly private reachability) pointing to the plugin Pod's gRPC port; **no external Ingress, Gateway, HTTPRoute or LoadBalancer**. Runner resolves/reaches the Service. |
| Namespaces / identity | Plugin and trusted Runner in a trusted namespace; job Pods in a **separate, fixed** `JOB_NAMESPACE`. Plugin Pod uses its dedicated ServiceAccount in the trusted namespace with an automatically mounted in-cluster token. Namespace-scoped Role + RoleBinding **in the job namespace**, binding that SA across namespaces. Job Pod uses the namespace's default ServiceAccount name but has `automountServiceAccountToken: false`; do not propagate plugin credentials or volumes. |
| Minimum RBAC | Core API `pods`: **create, get, delete** in `JOB_NAMESPACE` only; `pods/exec`: **create** in that namespace only (POST/SPDY). No list/watch, secrets, configmaps, service resources, roles, cluster-scoped privileges or cluster-admin. `pods/exec` authorization applies to all Pods in the job namespace, not only this plugin's Pods; keep unrelated privileged Pods out of it. |
| Selectors | Service selector must match plugin Deployment Pod labels (example: `app: forgejo-runner-kubernetes`); NetworkPolicy must select the same plugin Pods. Its ingress peer selector must match **only trusted Runner Pods**. Job Pods carry `app.kubernetes.io/managed-by=forgejo-runner-kubernetes`, `forgejo.org/execution-id=<deterministic name>` and annotation `forgejo.org/runner-name=<Runner name>`. Job Pods have node selector `kubernetes.io/arch=arm64`. |
| Network security | **Required invariant: Forgejo Runner may reach plugin gRPC; untrusted CI job Pods may NOT reach plugin gRPC.** Runner v13.2.0 does not authenticate plugin RPCs. Enforce an ingress NetworkPolicy for plugin Pods allowing ONLY trusted Runner Pod traffic on TCP 50051, and verify the cluster CNI actually enforces it. If Runner is in another namespace, match both trusted namespace and trusted pod labels; do not accidentally broaden with separate rules. Default-deny / egress rules for jobs and cluster-wide network access are administrator choices beyond this plugin, but preventing job-to-plugin reachability is mandatory. [`deploy/plugin-network-policy.yaml`](../deploy/plugin-network-policy.yaml) is a selector example, not proof of enforcement. |
| Plugin Pod resources / secrets | Example plugin container requests 50m CPU / 128Mi RAM with no limits; deployment policy may set limits explicitly, separate from generated job resources. No application secrets, Forgejo token, kubeconfig, Docker socket, privileged sidecar or image-pull secret is required for public images. The plugin SA projected token is **required inside the plugin Pod only**. Runner separately needs its normal Forgejo registration/connection credentials, managed by the Runner deployment. |
| Job image / volumes | The original shell-only test image was multiarch `ubuntu:24.04` (tag not digest-pinned here); a pinned JavaScript Action needs a Node 24 bootstrap runtime. `Create.image` overrides the label suffix; otherwise the suffix selects the image, and `JOB_IMAGE` is the last-resort default. All selected job images must supply `/bin/sh`, `/usr/bin/env`, `mkdir`, GNU `tar`, `sleep`, and default `bash` for a Runner shell step. The plugin runs `mkdir … && exec sleep infinity`, then execs commands into that container. Jobs honor image USER/runtime capabilities with no UID/GID/non-root overrides. A **single disk-backed `emptyDir`** (optional operator size limit, omitted by default) is mounted at both `/shared` and `/workspace` in the job Pod; the mounts share one budget, not two. No PVC or hostPath. Runner's `container.workdir_parent` must remain `workspace`. The root filesystem is writable. No extra job secrets/ServiceAccount token are mounted by the plugin. |
| Job Pod limits | Requests: CPU **100m**, memory **128Mi**, ephemeral-storage **256Mi** by default. All CPU/memory/ephemeral limits and disk `emptyDir` size caps are **omitted by default** in alpha.13. Alpha.12 still imposes memory1Gi/ephemeral2Gi/workspace1Gi. All quantities are operator controlled, accept `none` and are validated at startup. Requests are scheduling choices, not limits; namespace defaults/admission can affect actual values. See [Pod/resource policy](pod-policy.md). `restartPolicy: Never`; job-container controls remain RuntimeDefault seccomp, optional AppArmor (unset by default), no privilege escalation; ordinary jobs explicitly remain non-privileged, with image identity and runtime capabilities (no forced UID/non-root/custom capability policy); see [job execution](job-security.md). Pod `automountServiceAccountToken: false`. The Runner-provided environment timeout sets `activeDeadlineSeconds` when positive, but this does **not** delete a Pod. |

## Quiet workflow commands

Commands may remain completely silent until normal completion or a real timeout/cancellation; application heartbeat output is **not required** by `plugin.v1alpha`. The plugin's gRPC server explicitly uses `KeepaliveEnforcementPolicy{MinTime: 30s, PermitWithoutStream: false}` to accept supported Runner v13.2.0's 30-second client PINGs during active RPCs, including quiet Exec. No other server keepalive override or operator setting is needed. Runner stops its own PINGs with no active RPC; normal idle connections/reconnection are supported. This changes only transport-policy compatibility, not Exec, output/exit handling, Pod lifecycle or security. See [exec-liveness.md](exec-liveness.md) for source evidence, deterministic old/new-policy tests and the separate direct-smoke handoff. Deploy the new immutable plugin digest with the existing alpha.9 settings otherwise unchanged; retain exact-image checks around direct smoke execution without a heartbeat wrapper.

## Ordinary image-native jobs

**Identity model introduced in alpha.11; resource/pull-policy cleanup in alpha.12:** one ordinary non-privileged job model honors image USER and runtime capability defaults. No UID/GID/non-root override, drop/add capability list, job security mode, helper or NSS fix. RuntimeDefault seccomp, configured job AppArmor, no-new-privileges, host/token/NetworkPolicy isolation remains; capacity limits are operator policy. Ordinary workspace access needs no forced group. Existing private-ephemeral emptyDir roots remain broad 0777; volume hardening is deferred. Universal bounded env discovery preserves image HOME/PATH, absent/empty distinction and Runner precedence where wire values permit. See **[job-security.md](job-security.md)** for runtime/admission limitations, env/volume/hash/upgrade semantics, local evidence and the separate integration plan. This pre-alpha default deliberately does not preserve forced-UID compatibility: drain old jobs before deploying. Workflow services still fail before Pod creation.

## Job AppArmor

The plugin leaves AppArmor **unspecified by default**: if `JOB_APPARMOR_PROFILE` is absent or empty, it emits **no** `appArmorProfile` on either the Pod or its **job** container. Kubernetes, the node and the container runtime determine the effective profile. Set this plugin Deployment environment variable only when the operator explicitly wants to override that behavior on newly created job Pods:

| `JOB_APPARMOR_PROFILE` | `spec.containers[name=job].securityContext.appArmorProfile` |
| --- | --- |
| unset / empty (default) | Omitted (not the same as explicitly requesting `RuntimeDefault`) |
| `runtime-default` | `type: RuntimeDefault` |
| `unconfined` | `type: Unconfined` |
| `localhost:<name>` | `type: Localhost`, `localhostProfile: <name>` |

Values are case-sensitive and validated when the plugin starts. A Localhost profile name must be nonempty, have no surrounding whitespace or control characters and be **pre-loaded on nodes that run the job**. The plugin does not install profiles or verify their presence across nodes; Kubernetes/node admission and runtime checks decide whether a named profile is available. Invalid values, bare `localhost:` and profile names attached to other types prevent startup. The override is intentionally on the **job container**, where Runner's `pods/exec` processes run, not the plugin container or all containers in the Pod.

`unconfined` disables **AppArmor** confinement for that job container: this removes one layer of defense-in-depth and must be an explicit operator decision, never the public-plugin default. It does **not** disable seccomp: generated Pods still set **Pod `seccompProfile: RuntimeDefault`**. Image-native identity/runtime capabilities, no privilege escalation, no job ServiceAccount token and namespace/network isolation are unchanged. Cluster admission policy may restrict Unconfined AppArmor.

Our ARM64 k3s test cluster reproduced [containerd/containerd#12886](https://github.com/containerd/containerd/issues/12886): CRI exec processes acquired stacked AppArmor labels, and even same-job Node and shell child signals were denied (`kill EACCES` / `Permission denied`), with matching `apparmor="DENIED" operation="signal"` node audit events. **This does not imply other clusters need `unconfined`.** On this affected cluster the operator has chosen a temporary job-container override; remove it when the runtime no longer exhibits the issue. A successful full workflow or real workaround verification has **not** yet been reported for this image. See [process-signaling.md](process-signaling.md) for the evidence and separate cluster validation plan.

## Image building

Use an operator-owned persistent BuildKit service and Buildx's remote driver.
Jobs supply the endpoint and job-scoped registry authentication through ordinary
workflow configuration. The plugin does not create or manage builders, PVCs,
secrets or services. DinD settings, sidecars and Docker data/socket volumes are
removed; no job-local Docker runtime is provided.
See [ownership and migration](oci-building-design.md).

## Job storage budget

The plugin reads three Kubernetes **byte quantity** environment variables at startup; positive quantities opt in, exact `none` omits. Invalid, zero or negative values prevent startup:

| Plugin environment variable | Default | Generated job Pod field |
| --- | --- | --- |
| `JOB_WORKSPACE_SIZE_LIMIT` | Omitted | `spec.volumes[name=workspace].emptyDir.sizeLimit` |
| `JOB_EPHEMERAL_STORAGE_REQUEST` | `256Mi` | `spec.containers[name=job].resources.requests.ephemeral-storage` |
| `JOB_EPHEMERAL_STORAGE_LIMIT` | Omitted | `spec.containers[name=job].resources.limits.ephemeral-storage` |

When request and limit both exist, request≤limit is required. When both workspace and container caps exist, workspace<ephemeral limit leaves room for writable layers/logs. A workspace cap alone or container limit alone is valid; no missing cap is silently filled in. The plugin does not enforce a fixed overhead beyond a positive difference: select sufficient headroom for the actual workload. For example, an operator *might* set `JOB_WORKSPACE_SIZE_LIMIT=5Gi`, `JOB_EPHEMERAL_STORAGE_REQUEST=3Gi`, and `JOB_EPHEMERAL_STORAGE_LIMIT=7Gi` if the node's allocatable storage and namespace policy permit it; these are **not** prescribed values for `pi-wright`. Change the plugin Deployment environment, not the workflow or image; new job Pods use the values after the plugin restarts. Keep job concurrency, namespace quotas and node capacity in mind.

Per [Kubernetes' local ephemeral-storage accounting](https://kubernetes.io/docs/concepts/storage/ephemeral-storage/#resource-emphemeralstorage-consumption), a disk-backed `emptyDir` counts toward **overall Pod ephemeral-storage usage** together with writable container layers and logs. Its `sizeLimit` caps the volume; it is **not additional to** the container/Pod ephemeral-storage limit and does not reserve node capacity. The request informs scheduling and should reflect realistic expected usage, but does not guarantee space; node DiskPressure can still evict a Pod. The two mount points `/shared` and `/workspace` expose the **same** volume, so data is not budgeted twice. The volume is created for the job Pod and [deleted when the Pod is removed](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir); no persistence/caching. The plugin uses the default disk-backed `emptyDir`, **not tmpfs**: the kubelet's eviction was an `emptyDir` limit breach, not proof of a memory OOM. During the first `pi-wright` verify workload, checkout, setup-node, pnpm and dependency downloads completed before native package postinstall caused usage to exceed **1Gi**; node-3 had neither MemoryPressure nor DiskPressure. No orphaned resources were reported.

Use the example Role/RoleBinding, Deployment and Service in [`deploy/example.yaml`](../deploy/example.yaml) as a starting point, **not** a prescribed homelab layout. No Kubernetes Operator, CRD, external DNS or public endpoint is involved.

Runner config (the key `kubernetes` must match the label's URI scheme; the label suffix supplies the default job image):

```yaml
plugins:
  kubernetes:
    address: forgejo-runner-kubernetes.<trusted-namespace>.svc.cluster.local:50051
runner:
  labels:
    - 'k3s:kubernetes://ubuntu:24.04'
```

`runs-on: k3s` selects this plugin. Runner v13.2.0 sends an empty `Create.image` for a job without `container:`; `label_arg` is `ubuntu:24.04`. The plugin uses nonempty `Create.image` (from `jobs.<id>.container.image`) first, then nonempty `label_arg`, then `JOB_IMAGE` only if both are empty. It uses that resolved image as `spec.containers[name=job].image`. The plugin does not validate that the selected image supplies shell/tar/Node or limit registries; cluster admission controls can restrict image choice. This plugin does not provide Forgejo credentials to jobs; Runner/Forgejo handle workflow/job traffic as usual.

## Connections and lifecycle

1. **Forgejo ↔ Runner:** normal Forgejo Actions job dispatch, logs and status over the Runner's existing connection (managed by Runner; plugin never contacts Forgejo).
2. **Runner → plugin:** TCP/50051 internal unauthenticated gRPC via the Service; capabilities/health on connection; Create, Start, CopyIn, Exec, CopyOut, Remove per job. No plugin → Runner connection.
3. **plugin → Kubernetes API:** HTTPS using the mounted plugin ServiceAccount token (in-cluster config) for Pod create/get/delete and Pod exec streams, in `JOB_NAMESPACE`. It polls Pod status via GET (no watch/list). The API server/kubelet transports exec to the Pod; plugin does **not** open a direct network connection to the job Pod. Plugin does not contact the registry directly: nodes pull images via their runtime.
4. **job Pod → network:** command-dependent; hello-world needs no egress to Forgejo or plugin. No direct connection from job Pod to plugin is required or permitted. Forgejo/Runner may need ordinary connectivity to each other and to Git/Actions sources as part of existing Runner behavior. DNS, image registry and Kubernetes control-plane connectivity are cluster dependencies, not plugin-created networking.
5. Runner copies scripts as tar; plugin invokes **tar inside the job container** via `pods/exec` for extraction, and invokes tar again to stream files back. The plugin streams tar bytes but does *not* parse/extract/create tar archives itself. No `kubectl`, helper file-transport container or general arbitrary-image file transport. Exec uses Kubernetes exec with TTY disabled; separate stdout/stderr chunks go back over gRPC. Kubernetes exec exit errors become `ExecComplete(exit_code)` (including nonzero); infrastructure failures become `ExecFailed`. This behavior was exercised in the real ARM64 shell workflow; the larger verify job still needs a passing retest.
6. Runner's explicit `Remove` triggers a zero-grace Pod DELETE (missing Pod is success). Stream cancellation alone does **not** delete the environment: Runner may cancel a CopyOut reader during ordinary post-step processing. A successful DELETE request is **not** a wait for actual disappearance. A plugin/Runner crash after Create, lost API response, interrupted deletion or Pod stuck terminating can leave an orphan. Search job namespace for `app.kubernetes.io/managed-by=forgejo-runner-kubernetes`; arrange manual cleanup after confirming ownership. No automated reaper/TTL controller.

## Minimum integration test / gates

**Before running arbitrary repository code**, verify with cluster-side probes and RBAC checks (the deployment agent supplies these; this repository does not access the cluster):

- Trusted Runner Pod can reach the plugin's internal gRPC health endpoint; ordinary untrusted job Pods **cannot** connect to its port. Confirm actual NetworkPolicy enforcement, not just resource presence.
- Plugin SA has only the namespace Role above and no cluster-wide binding. Job Pod has no projected ServiceAccount token, no plugin SA token/mount or other plugin Kubernetes credentials. Check spec and actual filesystem/environment in a job Pod. Keep unrelated privileged Pods out of the job namespace.

Then execute a disposable Forgejo Actions workflow through Runner v13.2.0 with `runs-on: k3s` and a plain shell step containing:

```yaml
steps:
  - run: |
      uname -a
      id
      echo "hello from Kubernetes"
```

Check Forgejo dispatch, plugin RPC calls, creation of the `fj-exec-…` Pod in the configured namespace, successful ARM64 placement (`uname -m` / node architecture), both stdout and stderr content in Forgejo Actions (Runner v13.2.0 routes both into its job log; use distinct marker lines to identify each, not separate UI channels), correct success **and** nonzero-exit failure status (test a separate failing command, e.g. `exit 23`), and eventual Pod deletion after both jobs. These shell-only outcomes were reported as verified on ARM64 k3s with v0.1.0-alpha.2; checkout and setup-node later executed, but the first larger verify workload exceeded the original 1Gi workspace limit. If a new job fails or leaves a Pod, capture Runner/plugin logs and Pod events/status before considering changes. No cluster changes are performed by this repository.

Known limitations: alpha protocol, shell steps and pinned JavaScript Actions tested on ARM64, but no full verify success; image tag pulls can drift, `/tmp` writable, tar/tooling required in the job image, archive extraction not hardened for hostile tar contents, no service/OCI container actions, image ENV introspection, generic user override, caching, automated orphan sweep or verified pinned JavaScript Actions integration. Cancelling a Kubernetes exec stream is not a guaranteed remote process kill; Runner Remove ends the Pod at job teardown. The job container is isolated by Kubernetes container security, not by a VM boundary.
