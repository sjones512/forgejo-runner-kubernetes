# Ordinary image-native jobs

One job model: **run the selected image as an ordinary non-privileged Kubernetes container**. This pre-alpha plugin has no compatibility commitment to the former fixed UID. There are no job security profiles, custom capability lists, permissions images/init helpers, or NSS/passwd fixes. The identity simplification was published in alpha.11; alpha.12 published CPU/pull-policy cleanup. The subsequent **alpha.13** follow-up removes all remaining default resource/storage caps; the operator/namespace chooses limits. No cluster deployment is performed here. Full generated-field inventory, resource defaults/configuration and CPU experiment: **[pod-policy.md](pod-policy.md)**. [Executor-model research](executor-model.md) records the source evidence and deferred service work.

## Identity, capabilities and outer boundaries

The plugin emits **no runAsUser, runAsGroup, runAsNonRoot or capabilities field** on the job or Pod. Image USER/default-root identity and the container runtime/admission's ordinary capability baseline apply. Root identity does not imply privileged execution. There is no instance security mode or workflow privilege/securityContext passthrough; per-exec user and workflow cap_add remain rejected.

Retained boundaries:

- Explicit job `privileged: false` and `allowPrivilegeEscalation: false` (no-new-privileges).
- Pod RuntimeDefault seccomp and existing operator-selected **job-only** AppArmor; unset AppArmor remains unspecified. The supplied trusted environment's accepted Unconfined override is not a plugin default.
- No host namespaces, host paths/runtime sockets, shared process namespace or ServiceAccount token.
- Operator-opt-in resource/storage limits (alpha.13: **no default CPU/memory/ephemeral or emptyDir size caps**, for jobs). Scheduling requests remain unchanged; restartPolicy Never and Runner lifetime deadline remain executor mechanics. See [resource semantics/configuration](pod-policy.md#operator-resource-contract).
- Required NetworkPolicy isolation: only the trusted Runner may reach unauthenticated plugin gRPC. Ordinary Pod network access still needs operator policy.

Ordinary workspace emptyDir writes do not require a forced group. No fsGroup is emitted.

No plugin-defined capability baseline is imposed or promised. Runtime defaults can include capabilities such as NET_RAW or MKNOD that the rejected custom profile excluded; admission/runtime policy remains authoritative. Ordinary non-privileged execution is not a sandbox against untrusted images or kernel vulnerabilities. Restricted PSA can reject root/unset non-root identity, default capabilities or other fields; this plugin does not evade admission.

Local standard Debian/Node tests used **no cap-add/cap-drop**, no-new-privileges and Docker default seccomp. Apt update/install `hello`, chown/chmod, switching to node, chroot and signalling a switched-UID child passed. Observed root CapEff/CapBnd `00000000a80425fb`, NoNewPrivs 1, Seccomp 2. This records that Docker runtime, **not a contractual Kubernetes capability list** or proof of the target RuntimeDefault seccomp/capability policy. Package/daemon operations requiring unavailable privileges can still fail; do not add speculative profiles to work around them.

## Image environment and HOME

Because Exec uses `env -i`, retaining image identity alone would lose meaningful image settings. Start and every Exec therefore directly execute **`/usr/bin/env -0`** in `job` to capture its effective inherited image/Pod environment, without running application ENTRYPOINT or querying registries. Discovery is bounded to **30 seconds / 64KiB**, validates NUL framing/entries/duplicate keys, discards probe stderr and reports no raw environment/transport detail in errors. Images must provide an env utility supporting `-0`, in addition to existing shell/tar/mkdir/sleep requirements.

Order:

1. Effective inherited image/runtime environment.
2. Conventional PATH and HOME=/shared/workdir fallbacks **only when absent**.
3. Explicit Runner Exec env wins, including empty values.

Start advertises base defaults in StartComplete.image_env. Exec rediscovers them so plugin restart does not require a volatile cache or secret annotation. The plugin does not print/persist the environment; it sends it over the trusted Runner RPC contract. Kubernetes exec argv/environment can be visible to cluster administrators.

Pinned Runner's image-env merge treats existing **empty** workflow values as unset and composes PATH specially. The plugin preserves empty intent where it reaches Exec but cannot recover intent Runner already discarded before serializing that request.

HOME does **not** create a passwd/NSS account. Registered root/named/numeric image accounts resolve their passwd home when a child removes HOME. An image's deliberately unregistered numeric UID can still produce `uv_os_homedir ENOENT` in a sanitized child. Absent HOME gets the writable shared fallback for normal commands; explicit empty HOME remains empty. Images must make their declared HOME usable. No fabricated account, application workaround or promised fix for the specific supplied failure is introduced.

The executor still replaces image ENTRYPOINT/CMD with its long-lived CI command and uses Runner argv/workdir under /shared or /workspace. It does not implement full application startup/WORKDIR semantics, missing tools, arbitrary Docker options, container actions or per-exec user switching.

## Existing volumes

No permission preparation/hardening is added. Kubernetes emptyDir starts with a broad **0777** root. These volumes are private to one ephemeral job Pod, not shared across jobs or mounted from the host. World-write is a known within-Pod security characteristic to revisit separately if warranted, not group-private hardening.

The unchanged job startup creates /shared/{act,toolcache,workdir,tmp} as its effective identity, which also runs Exec and tar transfers. Both aliases share one disk emptyDir, with an optional operator size cap. Workload umask/chmod and ownership govern children; arbitrary cross-UID writes into another process's restrictive directories are not guaranteed.

Image building uses [operator-owned remote BuildKit](oci-building-design.md), not a job sidecar.

## Templates, restart and upgrade

New Pods have a deterministic full **job-template** SHA256 over their intended PodSpec, before API defaulting/remaining Runner lifetime. Job image/security/resources/storage/AppArmor participate. There is no profile annotation, env-policy mode/version or compatibility branch. Conflicting/unhashed Create retries fail with a generic template conflict, without secret/spec dumps; the plugin does not patch/delete a conflicting live environment.

This is a deliberate pre-alpha default change, not an upgrade preserving UID 10001. Drain/remove active old jobs before deployment; do not depend on old live environments being continued compatibly. Later RPCs use the actual stored Pod and one universal environment behavior. No fixed-UID legacy handler or automatic NSS repair exists.

## Validation and separate acceptance

Unit/protocol tests cover ordinary Pod fields, environment precedence and
bounds, template conflicts, service rejection, streaming transfers and cleanup.
The opt-in local container diagnostic exercises image identities and HOME/NSS:

```sh
EXECUTOR_IDENTITY_CONTAINER_TESTS=1 go test -race ./internal/plugin -run '^TestIdentityContainers$' -count=1 -v
```

Docker named volumes start 0755/root:root, unlike kubelet emptyDir. Test-only
setup models a 0777 root before workloads; it does not add production permission
preparation. Local Docker diagnostics do not prove target Kubernetes image USER
behavior, ARM64 execution or admission policy.

After upgrading, verify ordinary job execution, Actions, environment propagation,
file transfers and cleanup on the real runner. Preserve plugin ingress isolation.
No application test changes or new security modes are part of the builder migration.
