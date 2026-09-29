# Cluster integration contract (first experimental test)

This is a deployment/test handoff, **not** a compatibility claim. Plugin target: Forgejo Runner **v13.2.0**, commit `df6b843fb929bb04b933b09c6bf208774d42480b`, `plugin.v1alpha` proto SHA256 `961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`. Use a real Runner of that version. Details of upstream discovery: [research.md](research.md). A first real Forgejo + ARM64 k3s test on v0.1.0-alpha.1 reached Pod creation, ARM64 exec and stdout, but failed after the step due to premature Pod deletion on a cancelled CopyOut. This lifecycle defect was fixed in v0.1.0-alpha.2, which subsequently **passed the minimal ARM64 shell workflow** (multiple steps, stdout, success and exit 42, Pod deletion on both outcomes). JavaScript Actions have not yet been demonstrated.

## Cluster-supplied components

| Item | Contract |
| --- | --- |
| Plugin image | Public `ghcr.io/sjones512/forgejo-runner-kubernetes` multiarch OCI image; **pin by published `@sha256:…` manifest-list digest**, not a mutable tag or `latest`. Do not deploy until a digest is actually published. |
| Entrypoint / arguments | Image `ENTRYPOINT ["/plugin"]`; no args. Go binary requires in-cluster ServiceAccount environment and token. |
| Environment | `PLUGIN_LISTEN_ADDR` (default `0.0.0.0:50051`), `JOB_NAMESPACE` (default `forgejo-jobs`), `JOB_IMAGE` (default `ubuntu:24.04`), `JOB_ARCH` (default `arm64`; `amd64` only for disposable local testing). Timeouts are built in: startup 3 min, cleanup API calls 30 sec. No kubeconfig. |
| Ports / health | Plaintext HTTP/2 gRPC over TCP port **50051** by default, `plugin.v1alpha.BackendPlugin` and `grpc.health.v1` (SERVING once listening; NOT_SERVING on graceful shutdown). No HTTP readiness/liveness endpoint; optional gRPC probes only, with no implication that Kubernetes API or job Pods are healthy. |
| Service / exposure | Internal ClusterIP (or similarly private reachability) pointing to the plugin Pod's gRPC port; **no external Ingress, Gateway, HTTPRoute or LoadBalancer**. Runner resolves/reaches the Service. |
| Namespaces / identity | Plugin and trusted Runner in a trusted namespace; job Pods in a **separate, fixed** `JOB_NAMESPACE`. Plugin Pod uses its dedicated ServiceAccount in the trusted namespace with an automatically mounted in-cluster token. Namespace-scoped Role + RoleBinding **in the job namespace**, binding that SA across namespaces. Job Pod uses the namespace's default ServiceAccount name but has `automountServiceAccountToken: false`; do not propagate plugin credentials or volumes. |
| Minimum RBAC | Core API `pods`: **create, get, delete** in `JOB_NAMESPACE` only; `pods/exec`: **create** in that namespace only (POST/SPDY). No list/watch, secrets, configmaps, service resources, roles, cluster-scoped privileges or cluster-admin. `pods/exec` authorization applies to all Pods in the job namespace, not only this plugin's Pods; keep unrelated privileged Pods out of it. |
| Selectors | Service selector must match plugin Deployment Pod labels (example: `app: forgejo-runner-kubernetes`); NetworkPolicy must select the same plugin Pods. Its ingress peer selector must match **only trusted Runner Pods**. Job Pods carry `app.kubernetes.io/managed-by=forgejo-runner-kubernetes`, `forgejo.org/execution-id=<deterministic name>` and annotation `forgejo.org/runner-name=<Runner name>`. Job Pods have node selector `kubernetes.io/arch=arm64`. |
| Network security | **Required invariant: Forgejo Runner may reach plugin gRPC; untrusted CI job Pods may NOT reach plugin gRPC.** Runner v13.2.0 does not authenticate plugin RPCs. Enforce an ingress NetworkPolicy for plugin Pods allowing ONLY trusted Runner Pod traffic on TCP 50051, and verify the cluster CNI actually enforces it. If Runner is in another namespace, match both trusted namespace and trusted pod labels; do not accidentally broaden with separate rules. Default-deny / egress rules for jobs and cluster-wide network access are administrator choices beyond this plugin, but preventing job-to-plugin reachability is mandatory. [`deploy/plugin-network-policy.yaml`](../deploy/plugin-network-policy.yaml) is a selector example, not proof of enforcement. |
| Plugin Pod resources / secrets | Example plugin container requests 50m CPU / 128Mi RAM, limits 1 CPU / 512Mi RAM; deployment policy may adjust. No application secrets, Forgejo token, kubeconfig, Docker socket, privileged sidecar or image-pull secret is required for public images. The plugin SA projected token is **required inside the plugin Pod only**. Runner separately needs its normal Forgejo registration/connection credentials, managed by the Runner deployment. |
| Job image / volumes | The only intended first-test job image is multiarch `ubuntu:24.04` (tag not digest-pinned here); must supply `/bin/sh`, `/usr/bin/env`, `mkdir`, GNU `tar`, `sleep`, and default `bash` for a Runner shell step. The plugin runs `mkdir … && exec sleep infinity` as nonroot UID/GID 10001, then execs commands into that container. A **single `emptyDir`** (1Gi limit) is mounted at `/shared` and `/workspace` in the job Pod; no PVC or hostPath. Runner's `container.workdir_parent` must remain `workspace`. The root filesystem is writable. No extra job secrets/ServiceAccount token are mounted by the plugin. |
| Job Pod limits | Requests: CPU **100m**, memory **128Mi**, ephemeral-storage **256Mi**. Limits: CPU **1**, memory **1Gi**, ephemeral-storage **2Gi**; `emptyDir` size limit **1Gi**. `restartPolicy: Never`, RuntimeDefault seccomp, nonroot, capabilities drop ALL, no privilege escalation, `automountServiceAccountToken: false`. The Runner-provided environment timeout sets `activeDeadlineSeconds` when positive, but this does **not** delete a Pod. |

Use the example Role/RoleBinding, Deployment and Service in [`deploy/example.yaml`](../deploy/example.yaml) as a starting point, **not** a prescribed homelab layout. No Kubernetes Operator, CRD, external DNS or public endpoint is involved.

Runner config (the key `kubernetes` must match the label's URI scheme; `ubuntu:24.04` must match `JOB_IMAGE`):

```yaml
plugins:
  kubernetes:
    address: forgejo-runner-kubernetes.<trusted-namespace>.svc.cluster.local:50051
runner:
  labels:
    - 'k3s:kubernetes://ubuntu:24.04'
```

`runs-on: k3s` selects this plugin. Runner v13.2.0 sends an empty Create `image` for a job without `container:`; `label_arg` is `ubuntu:24.04`. Image overrides other than the configured image are rejected. This plugin does not provide Forgejo credentials to jobs; Runner/Forgejo handle workflow/job traffic as usual.

## Connections and lifecycle

1. **Forgejo ↔ Runner:** normal Forgejo Actions job dispatch, logs and status over the Runner's existing connection (managed by Runner; plugin never contacts Forgejo).
2. **Runner → plugin:** TCP/50051 internal unauthenticated gRPC via the Service; capabilities/health on connection; Create, Start, CopyIn, Exec, CopyOut, Remove per job. No plugin → Runner connection.
3. **plugin → Kubernetes API:** HTTPS using the mounted plugin ServiceAccount token (in-cluster config) for Pod create/get/delete and Pod exec streams, in `JOB_NAMESPACE`. It polls Pod status via GET (no watch/list). The API server/kubelet transports exec to the Pod; plugin does **not** open a direct network connection to the job Pod. Plugin does not contact the registry directly: nodes pull images via their runtime.
4. **job Pod → network:** command-dependent; hello-world needs no egress to Forgejo or plugin. No direct connection from job Pod to plugin is required or permitted. Forgejo/Runner may need ordinary connectivity to each other and to Git/Actions sources as part of existing Runner behavior. DNS, image registry and Kubernetes control-plane connectivity are cluster dependencies, not plugin-created networking.
5. Runner copies scripts as tar; plugin invokes **tar inside the job container** via `pods/exec` for extraction, and invokes tar again to stream files back. The plugin streams tar bytes but does *not* parse/extract/create tar archives itself. No `kubectl`, helper container or general arbitrary-image file transport. Exec uses Kubernetes exec with TTY disabled; separate stdout/stderr chunks go back over gRPC. Kubernetes exec exit errors become `ExecComplete(exit_code)` (including nonzero); infrastructure failures become `ExecFailed`. Completion depends on actual API/Runner behavior, not yet verified in-cluster.
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

Check Forgejo dispatch, plugin RPC calls, creation of the `fj-exec-…` Pod in the configured namespace, successful ARM64 placement (`uname -m` / node architecture), both stdout and stderr content in Forgejo Actions (Runner v13.2.0 routes both into its job log; use distinct marker lines to identify each, not separate UI channels), correct success **and** nonzero-exit failure status (test a separate failing command, e.g. `exit 23`), and eventual Pod deletion after both jobs. These shell-only outcomes have been reported as verified on ARM64 k3s with v0.1.0-alpha.2; the pinned checkout/setup-node test is **not** verified. If a new job fails or leaves a Pod, capture Runner/plugin logs and Pod events/status before considering changes. No cluster changes are performed by this repository.

Known limitations: alpha protocol, only the tested Ubuntu image and bash/sh run steps targeted; image tag pulls can drift, `/tmp` writable, tar/tooling required in the job image, archive extraction not hardened for hostile tar contents, no service/OCI container actions, image ENV introspection, generic user override, caching, automated orphan sweep or verified pinned JavaScript Actions integration. Cancelling a Kubernetes exec stream is not a guaranteed remote process kill; Runner Remove ends the Pod at job teardown. The job container is isolated by Kubernetes container security, not by a VM boundary.
