# Ordinary image-native jobs

One job model: **run the selected image as an ordinary non-privileged Kubernetes container**. This pre-alpha plugin has no compatibility commitment to the former fixed UID. There are no job security profiles, custom capability lists, permissions images/init helpers, or NSS/passwd fixes. The identity simplification was published in alpha.11; the subsequent resource/pull-policy cleanup targets alpha.12 after explicit review/publication authorization. No cluster deployment is performed here. Full generated-field inventory, resource defaults/configuration and CPU experiment: **[pod-policy.md](pod-policy.md)**. [Executor-model research](executor-model.md) records the source evidence and deferred service work.

## Identity, capabilities and outer boundaries

The plugin emits **no runAsUser, runAsGroup, runAsNonRoot or capabilities field** on the job or Pod. Image USER/default-root identity and the container runtime/admission's ordinary capability baseline apply. Root identity does not imply privileged execution. There is no instance security mode or workflow privilege/securityContext passthrough; per-exec user and workflow cap_add remain rejected.

Retained boundaries:

- Explicit job `privileged: false` and `allowPrivilegeEscalation: false` (no-new-privileges).
- Pod RuntimeDefault seccomp and existing operator-selected **job-only** AppArmor; unset AppArmor remains unspecified. The supplied trusted environment's accepted Unconfined override is not a plugin default.
- No host namespaces, host paths/runtime sockets, shared process namespace or ServiceAccount token.
- Operator resource policy (alpha.12: CPU100m request/no default limit, memory128Mi/1Gi, existing bounded ephemeral storage), capped disk emptyDirs, restartPolicy Never and Runner lifetime deadline. See [resource semantics/configuration](pod-policy.md#operator-resource-contract), distinct from daemon budgets.
- Required NetworkPolicy isolation: only the trusted Runner may reach unauthenticated plugin gRPC. Ordinary Pod network access still needs operator policy.

**fsGroup 10001 is emitted only with fixed DinD**, for its group-accessible Unix socket; it does not change the image's primary GID or create an account. Ordinary workspace emptyDir writes do not require a forced group. Fixed DinD retains its existing daemon-only privileged/root/Unconfined fields, image pin, args, probes and budgets.

No plugin-defined capability baseline is imposed or promised. Runtime defaults can include capabilities such as NET_RAW or MKNOD that the rejected custom profile excluded; admission/runtime policy remains authoritative. Ordinary non-privileged execution is not a sandbox against untrusted images or kernel vulnerabilities. Restricted PSA can reject root/unset non-root identity, default capabilities or other fields; this plugin does not evade admission.

Local standard Debian/Node tests used **no cap-add/cap-drop**, no-new-privileges and Docker default seccomp. Apt update/install `hello`, chown/chmod, switching to node, chroot and signalling a switched-UID child passed. Observed root CapEff/CapBnd `00000000a80425fb`, NoNewPrivs 1, Seccomp 2. This records that Docker runtime, **not a contractual Kubernetes capability list** or proof of the target RuntimeDefault seccomp/capability policy. Package/daemon operations requiring unavailable privileges can still fail; do not add speculative profiles to work around them.

## Image environment and HOME

Because Exec uses `env -i`, retaining image identity alone would lose meaningful image settings. Start and every Exec therefore directly execute **`/usr/bin/env -0`** in `job` to capture its effective inherited image/Pod environment, without running application ENTRYPOINT or querying registries. Discovery is bounded to **30 seconds / 64KiB**, validates NUL framing/entries/duplicate keys, discards probe stderr and reports no raw environment/transport detail in errors. Images must provide an env utility supporting `-0`, in addition to existing shell/tar/mkdir/sleep requirements.

Order:

1. Effective inherited image/runtime environment.
2. Stored plugin-owned DOCKER_HOST/TMPDIR defaults when fixed DinD is present.
3. Conventional PATH and HOME=/shared/workdir fallbacks **only when absent**.
4. Explicit Runner Exec env wins, including empty values.

Start advertises base defaults in StartComplete.image_env. Exec rediscovers them so plugin restart does not require a volatile cache or secret annotation. The plugin does not print/persist the environment; it sends it over the trusted Runner RPC contract. Kubernetes exec argv/environment can be visible to cluster administrators. Stored Pod daemon defaults, not the current server config, govern a live environment.

Pinned Runner's image-env merge treats existing **empty** workflow values as unset and composes PATH specially. The plugin preserves empty intent where it reaches Exec but cannot recover intent Runner already discarded before serializing that request.

HOME does **not** create a passwd/NSS account. Registered root/named/numeric image accounts resolve their passwd home when a child removes HOME. An image's deliberately unregistered numeric UID can still produce `uv_os_homedir ENOENT` in a sanitized child. Absent HOME gets the writable shared fallback for normal commands; explicit empty HOME remains empty. Images must make their declared HOME usable. No fabricated account, application workaround or promised fix for the specific supplied failure is introduced.

The executor still replaces image ENTRYPOINT/CMD with its long-lived CI command and uses Runner argv/workdir under /shared or /workspace. It does not implement full application startup/WORKDIR semantics, missing tools, arbitrary Docker options, container actions or per-exec user switching.

## Existing volumes and fixed DinD

No permission preparation/hardening is added. Kubernetes emptyDir starts with a broad **0777** root. With fixed DinD, fsGroup supplies group ownership/permissions and setgid, so roots can be root:10001 **02777**; fsGroup does not remove world-write. These volumes are private to one ephemeral job Pod, not shared across jobs or mounted from the host. World-write is a known within-Pod security characteristic to revisit separately if warranted, not group-private hardening.

The unchanged job startup creates /shared/{act,toolcache,workdir,tmp} as its effective identity, which also runs Exec and tar transfers. Both aliases share one capped disk emptyDir. Workload umask/chmod and ownership govern children; arbitrary cross-UID writes into another process's restrictive directories are not guaranteed.

Fixed `JOB_DIND_ENABLED` remains operator-only/default-off: same pinned daemon, private socket root:10001 **0660**, data root /var/lib/docker, bounded resources/storage/readiness, shared binds/private-/tmp semantics, DOCKER_HOST/TMPDIR defaults and explicit Runner override precedence. A non-privileged job accesses the daemon via supplemental group 10001. Deliberately dropping supplemental groups can lose socket access. Controlling the privileged nested daemon still creates an elevated kernel/runtime attack surface. No workflow services or DinD migration.

## Templates, restart and upgrade

New Pods have a deterministic full **job-template** SHA256 over their intended PodSpec, before API defaulting/remaining Runner lifetime. Job image/security/resources/storage/AppArmor, conditional fsGroup and daemon policy participate. There is no profile annotation, env-policy mode/version or compatibility branch. Conflicting/unhashed Create retries fail with a generic template conflict, without secret/spec dumps; the plugin does not patch/delete a conflicting live environment.

This is a deliberate pre-alpha default change, not an upgrade preserving UID 10001. Drain/remove active old jobs before deployment; do not depend on old live environments being continued compatibly. Later RPCs use the actual stored Pod and one universal environment behavior. No fixed-UID legacy handler or automatic NSS repair exists.

## Validation and separate acceptance

Full `go test ./...`, `go test -race ./...`, `go vet ./...`, twenty focused race repetitions, formatting/diff and diagnostic syntax checks passed, as did the local container suite and a static Linux ARM64 cross-build. Runner v13.2.0 and the imported/generated protocol are unchanged (proto SHA256 `961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`).

Unit tests cover intended ordinary Pod fields, conditional socket group, no init/helper, preserved resources/mounts/daemon, env precedence/framing/non-shell keys/empty values/redaction/limits/cancellation, stored daemon defaults/restart, template conflicts and unchanged service rejection. Real-gRPC/bufconn virtual-time quiet/silent/nonzero/cancellation/deadline tests retain old **transport-policy** negative controls, not old identity modes.

Optional local diagnostic:

```sh
EXECUTOR_IDENTITY_CONTAINER_TESTS=1 go test -race ./internal/plugin -run '^TestIdentityContainers$' -count=1 -v
```

Passed root, named node, registered numeric1000 and arbitrary UID23456/GID34567, **each with DinD off and on**. No job cap-add/cap-drop or forced group without DinD. Tests run generated long-lived job/Exec argv and streaming CopyIn/CopyOut RPCs (bufconn → Docker exec), checking archive payload/ownership and opposite /shared↔/workspace aliases; HOME present/absent/empty and sanitized-child NSS; and actual pinned Docker29.8.1 `/info` through its observed root:10001 mode-0660 socket. A separate ordinary root package/chown/user-switch/chroot/supervision case passed with runtime defaults. Disposable containers/volumes are cleaned up.

Docker named volumes start 0755/root:root, unlike kubelet emptyDir, so test-only setup models existing **0777**, or **02777/root:10001 with DinD**, roots before workloads. It does not create staging directories or implement production permission preparation. Docker --user and --group-add model image-resolved credentials/fsGroup, **not real Kubernetes image USER/fsGroup**, ARM64 execution, target runtime policy, storage-cap accounting or build/cache/publication. No custom CI image is built. The specific supplied homedir failure remains unattributed pending real integration.

**The identity model was introduced in alpha.11; alpha.12 adds reviewed resource-policy changes. Publication is not cluster acceptance.** After separately authorized deployment of an immutable release digest:

1. Drain active old jobs and deploy the newly authorized immutable plugin digest. Preserve namespace/storage/fixed DinD and accepted job AppArmor settings otherwise; there is no security-profile/helper configuration.
2. On ARM64, exercise images declaring root/default USER, named USER, registered numeric USER and arbitrary numeric USER. Capture actual euid/egid/groups, capabilities/no-new-privileges/seccomp, selected image/runtime digest, HOME presence and NSS result; verify no privileged job/init/token/host access. Check fsGroup absent without DinD and 10001 with it, recording actual emptyDir/socket modes.
3. Confirm HOME present/absent/empty, sanitized-child NSS limitations, both workspace aliases, CopyIn/CopyOut, Actions, quiet/nonzero/cancellation and explicit cleanup. Test restart/template conflicts without attempting to migrate live old jobs.
4. Rerun existing pi-wright `ci.yaml` **unchanged**; capture the previously failing child's Node/libuv/euid/HOME/NSS/error. Only real integration may attribute or declare that homedir failure fixed. No application test edits, synthetic passwd or heartbeat wrappers.
5. Separately install a small ordinary OS package, chown and switch users under the actual runtime baseline with unchanged no-new-privileges/seccomp/AppArmor. Record failures; do not infer a need for modes/custom capabilities from speculative compatibility.
6. Reconfirm existing fixed-DinD socket/readiness/build/load/exact-image smoke/cache/publication/storage accounting and cleanup; preserve job-to-plugin ingress denial. Services remain rejected and deferred. Publication alone is not integration acceptance.
