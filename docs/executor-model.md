# Job-image identity and workflow services: research and proposed executor model

**Status: single ordinary image-native job model implemented locally, unreleased and awaiting review. Workflow services and volume hardening remain deferred.** Released baseline: `2cdde546dcaa77808c37099308a1d997a0ae9548` / v0.1.0-alpha.10. This is an improvement milestone, not recovery of a broken executor. Publication was stopped before any tag/release. The operator then removed backward compatibility as a requirement and directed KISS: no fixed UID, job security profiles, custom capability lists, permission helpers or NSS fixes. No Forgejo instance, application repository or cluster-IaC was accessed; pinned Runner source already available locally was inspected. No custom CI image was built.

**Implemented subset:** ordinary non-privileged job containers honor image USER and runtime capability defaults, with no runAsUser/runAsGroup/runAsNonRoot or capabilities field. Outer seccomp/AppArmor/no-new-privileges/token/host/resource/network boundaries remain. fsGroup10001 is used only for fixed DinD socket access, not ordinary workspace writes. Universal bounded NUL-framed env discovery preserves image HOME/PATH and Runner precedence. No modes, compatibility branches or profile annotations exist; new Pod templates are hashed. See **[job-security.md](job-security.md)** for the complete implemented operator/security/env/retry contract, tests and the narrow release-gated integration handoff. Sections 12–16 below remain **future research, not implemented features**.

Operator-supplied evidence now establishes ARM64 Actions/transfers, quiet Exec, per-job images, fixed DinD, native Buildx build/load, application smoke tests, registry publication and registry-backed BuildKit caching. The operator reports that the accepted Unconfined **job** AppArmor profile removes the explicit child-process `kill EACCES` failure and changes unchanged verification from 142/146 to 143/146 passing. That decision is accepted, not revisited. These are supplied integration observations, not measurements repeated here. Neither 143/146 nor successful image smoke tests establishes a passing full verification suite.

## 1. Exact Runner and protocol inspected

Local `/tmp/fj-runner-v13`: **Forgejo Runner v13.2.0**, commit `df6b843fb929bb04b933b09c6bf208774d42480b`. `act/plugin/proto/v1alpha/plugin.proto` SHA256:

```text
961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79
```

Source anchors at that commit (local files, no remote Forgejo requests):

- `act/plugin/proto/v1alpha/plugin.proto`: RPC/lifecycle contract 18–78; service/Create fields 92–124; Start/Exec fields 167–184.
- `act/runner/run_context.go`: `startPluginEnvironment` 1059–1161; `containerImage` 1308–1322; `getJobContext` 1498–1509. Docker-only service processing 557–689, job setup 698–744, startup 751–790 and service health polling 903–945 are useful contrasts, **not the plugin execution path**.
- `act/plugin/adapter.go`: `AddServiceContainerRaw` 79–86; `newCreateRequest` 178–187; Create 202–243; Start 245–300; Exec 310–340; image-env layering 550–575; no-op health and Remove 577–595.
- `act/runner/step.go`: `mergeEnv` 318–342 layers workflow container environment into step environment.
- `act/model/workflow.go`: `ContainerSpec`; `act/model/job_context.go`: service context shape. `act/plugin/proto/v1alpha/plugin_grpc.pb.go`: actual generated streaming API.

`jobs.<id>.container.image` is interpolated and sent as **Create.image**. Image selection remains explicit image → label suffix → operator fallback. The wire calls images **backend-defined opaque references**, not a mandatory OCI identity/security contract. There is no Create UID/GID, security-context, entrypoint or working-directory field. Cap additions/drops are advisory global adjustments, not a privilege request. `backend_options` comes from operator Runner plugin configuration, not Docker `container.options`.

Workflow container env reaches Exec through step merging; Exec has argv, env, optional user and workdir. A nonempty user can be sent by the adapter, but this plugin explicitly rejects it: Kubernetes `PodExecOptions` offers neither per-exec UID nor env/cwd switching. The plugin must own the environment's default identity. Workflow `container.options` (including `--user`), entrypoint/init/tty/volumes/pull credentials are **not translated by this pinned plugin startup path**. A plugin cannot reject or implement omitted settings it never receives; operator documentation must say this explicitly.

StartComplete.image_env exists so Runner can layer resolved image environment below workflow variables. Its adapter fills empty values and has special PATH composition; this is not a perfectly literal map override for explicitly empty strings. The released alpha.10 Start reported plugin Docker/TMPDIR defaults and a hard-coded PATH, not arbitrary image ENV. The simplified local implementation now captures effective inherited env with `/usr/bin/env -0` at Start and each Exec, without a registry lookup or secret cache; stored plugin defaults and explicit Runner env layer above it. Honoring USER alone would not preserve PATH/HOME/tool settings because Exec uses `env -i`.

Job entrypoint preservation is not a protocol requirement: Runner needs an execution environment, and its Docker path also normally substitutes a long-lived `tail -f /dev/null` entrypoint. Retain this plugin's explicit shell/mkdir/sleep keepalive command and Runner workspaces. “Image-native identity” does **not** mean running an image's arbitrary application entrypoint or using its WORKDIR instead of Runner's workdir.

## 2. Exact service fields and lifecycle

| ServiceContainer field | Number/type | Exact meaning |
| --- | --- | --- |
| `name` | 1, string | Service ID from workflow `services:` block. |
| `image` | 2, string | Backend-defined image reference. |
| `env` | 3, map<string,string> | Service environment. |
| `ports` | 4, repeated string | Requested **host:container port mappings**, not just informational ports. |

This is the complete service message. **No privileged flag**, user/group, command/entrypoint, volumes, health options, registry credentials, dependencies, or resource limits. Runner interpolates image/env/ports, skips services with empty interpolated images, and appends them to the single Create request. Service `options: --privileged` and Docker health options do not arrive at this plugin.

Runner connects/checks plugin health, calls Capabilities, then one environment Create → Start → CopyIn/Exec/CopyOut → Remove. Services are part of that environment; no separate service RPCs or service IDs usable by Exec. StartComplete gates the workflow, so the backend should finish its documented startup/readiness checks there. The plugin path does **not** populate Docker ServiceContainers or run the Docker service-health pipeline; adapter IsHealthy is a no-op. No ongoing service-health notification RPC exists. gRPC health is plugin health, not database health.

ManagesOwnNetworking is true: Runner does not create/attach a Docker network. CreateResponse/StartComplete contain no service endpoints or mapped-port results. `getJobContext()` returns status only: `${{ job.services.db.ports[...] }}` cannot be made reliable just by creating a Kubernetes Service. Dynamic port results would require Runner/protocol work outside this repository.

Remove owns the whole environment, including services. Runner installs cleanup before Create/Start; when Create succeeded, later startup failure/cancellation proceeds to Remove. Preserve explicit Remove-only deletion, cancellation-aware startup, environment mutex serialization and bounded idempotent cleanup. API uncertainty/plugin crashes can still orphan workloads; service support is not a new orphan sweeper.

## 3. Former fixed-UID behavior inspected (v0.1.0-alpha.10)

`internal/plugin/server.go:podSpec` emits:

- Pod: `runAsUser: 10001`, `runAsNonRoot: true`, `fsGroup: 10001`, RuntimeDefault seccomp.
- Job container: `allowPrivilegeEscalation: false`, capabilities drop ALL, optional operator job AppArmor profile, writable rootfs, bounded CPU/memory/ephemeral storage.
- **No runAsGroup is actually set**, at either level. Do not describe the existing primary GID as guaranteed 10001. It is runtime/image-derived; 10001 is the explicit supplemental volume group. The local numeric-only reproduction has primary GID 0 and supplemental GID 10001.
- One capped disk emptyDir, mounted twice at `/shared` and `/workspace`; no hostPath, host namespaces, service exposure or job API token. Only trusted Runner may reach unauthenticated plugin gRPC.
- Job image entrypoint replaced by shell setup plus sleep. Transfers/Exec always target `job`. Exec supplies HOME=/shared/workdir **only if absent**, preserving even an explicitly empty HOME.

Fixed DinD remains special operator policy: explicit root/privileged daemon overrides the Pod identity, keeps the accepted daemon-only Unconfined profiles, exact capped `/var/lib/docker`, shared workspace and separate Unix socket group 10001. Services are currently rejected before Pod creation. Nothing here changes AppArmor, seccomp, DinD, lifecycle or resource settings.

## 4. Linux, Node and libuv homedir resolution

Relevant known runtime from earlier integration: **Node v26.8.1**. Its vendored **libuv 1.52.1** was inspected and the local container reproduces exactly those versions. The supplied failing-child excerpt itself does not identify its Node version: the separate agent must capture `process.version`/`process.versions.uv` there rather than treating the local version as an independently observed failing-child version.

Pinned upstream sources:

- [Node v26.8.1 lib/os.js](https://github.com/nodejs/node/blob/v26.8.1/lib/os.js): homedir exports getCheckedFunction(getHomeDirectory), throwing ERR_SYSTEM_ERROR when native binding returns undefined.
- [src/node_os.cc](https://github.com/nodejs/node/blob/v26.8.1/src/node_os.cc), GetHomeDirectory: calls uv_os_homedir; records its error/syscall in the exception context.
- [vendored libuv unix/core.c](https://github.com/nodejs/node/blob/v26.8.1/deps/uv/src/unix/core.c): uv_os_homedir 1265–1304; uv__getpwuid_r 1352 onward; uv_os_get_passwd 1508; uv_os_getenv 1580 onward.
- [getpwuid_r Linux manual](https://man7.org/linux/man-pages/man3/getpwuid.3.html); [Node os.homedir documentation](https://nodejs.org/download/release/v26.8.1/docs/api/os.html#oshomedir).

Linux chain:

1. uv_os_homedir first calls uv_os_getenv("HOME"). If HOME exists, it returns that value (or an actual buffer/error result); **an empty value is still set**, not an NSS fallback.
2. Only HOME-not-found (`UV_ENOENT`) triggers uv_os_get_passwd.
3. That function calls getpwuid_r for **geteuid()**, through libc's configured passwd/NSS sources. `/etc/passwd` is a common source, not necessarily the only one; absence from that file alone does not prove NSS lookup fails.
4. getpwuid_r can successfully report no matching record: return 0 with result NULL. Libuv explicitly translates that to **UV_ENOENT**. NSS errors can also propagate. Node reports ERR_SYSTEM_ERROR with syscall `uv_os_homedir`, uv code ENOENT.
5. No directory-existence check is performed. HOME pointing to a nonexistent directory still resolves; a directory named /shared/workdir does not create a passwd account.

An arbitrary Kubernetes numeric runAsUser changes process credentials, **not the image passwd database/NSS configuration**. “Non-root” validation also does not create a user. Overriding a perfectly valid root/node image to unregistered UID 10001 can therefore break homedir calls whenever HOME is absent. `os.userInfo()` demonstrates the underlying passwd failure independently of HOME.

Default child-process environment inheritance would retain HOME. A child created with an explicit sanitized environment (or a later HOME deletion) does not. The plugin's top-level HOME default cannot restore a variable intentionally removed inside a job. Do not modify application tests or insert passwd entries/NSS shims as the default fix.

## 5. Local reproduction: verified mechanism, not attribution

Standard upstream image, no build or application files:

```text
node@sha256:367679cf9792759492a486e4aa4b421764d71a9546a6dae8aab81a99eb797b3e
```

Local Docker default endpoint was checked as `unix:///var/run/docker.sock`; recorded platform Linux **amd64**, Node 26.8.1/libuv 1.52.1. These are container-level measurements, **not ARM64/Kubernetes integration measurements**. Containers used `--rm --network none --cap-drop ALL --security-opt no-new-privileges`; only the standalone probe was mounted read-only. No node runtime socket was mounted in a probe container.

Run from this repository:

```sh
image=node@sha256:367679cf9792759492a486e4aa4b421764d71a9546a6dae8aab81a99eb797b3e
docker --context default run --rm --network none --user 10001 --group-add 10001 \
  --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,src=$PWD/examples/homedir-probe.cjs,dst=/probe.cjs,readonly" \
  "$image" /usr/bin/env HOME=/shared/workdir node /probe.cjs missing-passwd
# For absent HOME, replace the final env invocation with:
# /usr/bin/env -u HOME node /probe.cjs missing-passwd
```

`examples/homedir-probe.cjs` records only identity, HOME, matching passwd lines and homedir/userInfo results; it starts the same Node binary with `env: {}` and asserts the expected outcome.

| User | HOME absent | HOME=/shared/workdir | HOME empty | Sanitized child's homedir |
| --- | --- | --- | --- | --- |
| 10001:10001, no passwd record | uv_os_homedir ENOENT | /shared/workdir | empty string | ENOENT in all cases |
| 0:0, root passwd record | /root | /shared/workdir | empty string | /root |
| named node (UID/GID 1000) | /home/node | /shared/workdir | empty string | /home/node |
| numeric 1000:1000, same passwd record | /home/node | /shared/workdir | empty string | /home/node |

All 12 matrix cases passed assertions. Two further controls passed: the image's default user (no --user, HOME absent) resolves /root, and numeric-only 10001 plus supplemental group 10001 has GID 0, parent homedir /shared/workdir and sanitized-child ENOENT. Root/nonroot and capabilities do not themselves repair missing NSS records.

**Conclusion:** fixed UID 10001 **can** cause this failure in otherwise valid images; parent HOME does not refute it. The supplied integration failure is consistent with this reproduced mechanism, not yet definitively attributed. A deliberately numeric image USER without NSS support can still have the same failure in image-native mode.

## 6. Kubernetes USER and group semantics

Inspected client/API **v0.37.1** locally, plus upstream Kubernetes **v1.37.1** source. These are source versions, not a claim about the actual cluster's version/configuration:

- [kuberuntime/security_context.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/kuberuntime/security_context.go): effective container-over-Pod settings; image UID/username used when RunAsUser is absent; FSGroup appended to supplemental groups.
- [security_context_others.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/kuberuntime/security_context_others.go): verifyRunAsNonRoot.
- [helpers.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/kuberuntime/helpers.go): getImageUser; absent user metadata treated as root.
- [volume_linux.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/volume/volume_linux.go) and [empty_dir.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/volume/emptydir/empty_dir.go): ownership/modes.
- [Security-context documentation](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/); local core/v1/types.go PodSecurityContext, SecurityContext, EmptyDirVolumeSource and PodExecOptions.

| Setting | Result |
| --- | --- |
| No runAsUser at either level | Runtime uses image USER; named users are resolved in the image; no USER ordinarily means root. |
| Container/Pod runAsUser set | Explicit UID overrides image USER; container setting wins; no passwd entry is synthesized. |
| runAsNonRoot unset/false | No kubelet non-root validation; not permission to be privileged. Admission still applies. |
| runAsNonRoot true + explicit nonzero UID | Accepted identity check even if the UID has no passwd record. UID 0 fails. |
| runAsNonRoot true + numeric image UID | Nonzero valid numeric UID passes; UID 0/no USER fails. |
| runAsNonRoot true + named image USER | Kubelet cannot verify non-root from the name and rejects, even if the image's account is non-root. An explicit UID avoids that ambiguity but overrides image identity. |
| runAsGroup | Explicit primary GID; otherwise runtime default, potentially 0 for an arbitrary UID. Do not force the workspace group as image primary group. |
| fsGroup | Supplemental process group and supported-volume ownership/permission adjustment; not UID, primary GID, passwd account or HOME. |

Supplemental groups default to Merge, which can include image /etc/group memberships; Strict excludes implicit memberships but has feature/runtime compatibility requirements. Do not introduce Strict blindly or assume a node supports it. Images/entrypoints that call setgroups can discard supplemental groups themselves.

Privileged PSA in the supplied trusted CI namespace allows a wider security envelope; it does not force privilege, override runAsUser, or supply NSS entries.

## 7. Mature executor comparison

Snapshots inspected (pinned source commits; current docs are background, not timeless defaults):

| Executor | Identity/security model | Workspace implications |
| --- | --- | --- |
| GitLab Runner Kubernetes, `896d1e6abc0a5868e2612dce45793ffc2f282668` | No unconditional job UID. Optional admin container/pod security contexts; current job-level numeric user/group with allowlists. Admin container > admin Pod > job configuration; job-requested root requires explicit allowance. Helper settings are independent. | Configurable fsGroup, helper and init-permissions contexts. Some permission setup uses broad modes; not a reason to copy those modes. |
| Actions container hooks, `d4731a43a2fe1a4d4f7ea24dc515a1a120a065c8` | createContainerSpec does not force job runAsUser; operator hook templates can override $job securityContext. Job entrypoint normally replaced; services use their entrypoints. ARC runner-container identity is not the job-container identity. | createJobPod sets fsGroup 1001 and a separate UID/GID-1001 fs-init helper. It does **not** force job UID 1001. |
| Woodpecker, `25018577086396561a4f60711ce4ae267639f561` | Default nonroot policy false; no blanket UID. Configurable Pod/container contexts. Explicit runAsUser 0 is restricted unless privileged/userns permitted, distinct from leaving image USER unspecified. | Default fsGroup 1000; permission init helper for certain non-root workdirs. Separate-step/service Pod model is not directly copied. |
| Tekton, `61e02215132ce7bef054ff7e1fb074f217716c1a` | Step/image identity and operator/task security contexts; no universal fixed step UID. Optional hardened contexts for its helpers are not a blanket step UID. | Shared HOME/workspaces can conflict across identities. Its docs explicitly warn that SSH ignores HOME and requires a valid passwd home; HOME substitution is not NSS identity repair. |

Exact source links:

- GitLab [common/config.go](https://github.com/gitlabhq/gitlab-runner/blob/896d1e6abc0a5868e2612dce45793ffc2f282668/common/config.go), GetContainerSecurityContext/GetPodSecurityContext; [executors/kubernetes/kubernetes.go](https://github.com/gitlabhq/gitlab-runner/blob/896d1e6abc0a5868e2612dce45793ffc2f282668/executors/kubernetes/kubernetes.go), security precedence and init-permissions; [executor documentation](https://docs.gitlab.com/runner/executors/kubernetes/).
- Hooks [prepare-job.ts](https://github.com/actions/runner-container-hooks/blob/d4731a43a2fe1a4d4f7ea24dc515a1a120a065c8/packages/k8s/src/hooks/prepare-job.ts), createContainerSpec; [k8s/index.ts](https://github.com/actions/runner-container-hooks/blob/d4731a43a2fe1a4d4f7ea24dc515a1a120a065c8/packages/k8s/src/k8s/index.ts), createJobPod; [ARC hook-template documentation](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets#configuring-hook-extensions).
- Woodpecker [pod.go](https://github.com/woodpecker-ci/woodpecker/blob/25018577086396561a4f60711ce4ae267639f561/pipeline/backend/kubernetes/pod.go), podSecurityContext/containerSecurityContext/podInitContainer; [kubernetes.go](https://github.com/woodpecker-ci/woodpecker/blob/25018577086396561a4f60711ce4ae267639f561/pipeline/backend/kubernetes/kubernetes.go), default config; [flags.go](https://github.com/woodpecker-ci/woodpecker/blob/25018577086396561a4f60711ce4ae267639f561/pipeline/backend/kubernetes/flags.go).
- Tekton [pod.go](https://github.com/tektoncd/pipeline/blob/61e02215132ce7bef054ff7e1fb074f217716c1a/pkg/pod/pod.go), step/Pod construction; [security_context_config.go](https://github.com/tektoncd/pipeline/blob/61e02215132ce7bef054ff7e1fb074f217716c1a/pkg/pod/security_context_config.go); [auth/NSS warning](https://tekton.dev/docs/pipelines/auth/#using-secrets-as-a-non-root-user).

Conventional model: selected image identity plus explicit operator security policy, independently managed helpers/volumes. There is no universal convention that arbitrary images must all become UID 10001, nor a guarantee that permitting image root means permitting privileged execution.

## 8. Final reviewed direction: one ordinary image-native model

The initial compatibility/profile proposal was rejected. This pre-alpha plugin has no compatibility obligation justifying retention of UID10001. There is no demonstrated need for multiple modes: ordinary runtime defaults passed the required root/non-root paths, transfers, DinD socket and package/user operations.

The job/Pod omit UID/GID/non-root overrides and custom capability policy. Jobs explicitly remain non-privileged with no-new-privileges, RuntimeDefault seccomp, configured job AppArmor, resource/storage bounds, no host access/token and trusted-Runner-only gRPC ingress. fsGroup10001 is added only when the fixed daemon socket requires it. Runtime/admission determines ordinary capabilities; this is not the prior custom reduced set.

Every job uses direct `/usr/bin/env -0` discovery (30s/64KiB). Stored plugin defaults overlay inherited env; explicit Runner Exec env wins. Present/empty HOME is preserved, absent HOME gets a shared writable fallback, and no NSS entry is fabricated. Runner's empty/PATH merge limits remain documented. No cache/secret annotation/registry inspection or full image application startup semantics is introduced.

Continue explicit Runner argv/cwd and long-lived job command; per-exec user remains unsupported. Image selection alone cannot make distroless/tool-deficient images valid for existing tar/shell/Actions requirements.

## 9. No compatibility requirement; retain safe lifecycle/retry behavior

This deliberately changes default execution from arbitrary UID10001/drop-ALL to image identity/runtime defaults. No legacy handler or security mode is retained. Drain/remove old active jobs before deploying; do not depend on compatible continuation of old environments. Fixed DinD and configured AppArmor remain independent.

New job templates are hashed over intended PodSpec before API defaulting/remaining Runner lifetime, with and without DinD. Different image/security/resources/storage/group/daemon settings reject Create conflicts; no secret/spec dump or live patch/deletion occurs. Later RPCs use the actual stored Pod and one universal env behavior, including stored daemon defaults rather than current server config. Future service ordering/env/ports hashing remains separately scoped work.

## 10. Shared-volume permission strategy

Use **fsGroup 10001 only with fixed DinD, independent of image UID/primary GID**, to add its socket group. Ordinary emptyDir workspace writes require no forced group; without DinD the root remains broad 0777. Only job and approved Docker profile mount workspace/socket by default; ordinary services get neither.

Important source finding: current emptyDir setup defaults to **0777**, and fsGroup **ORs** group permissions/setgid into existing modes. It does not remove world permissions: the current mounted volume root can be 02777. Do not claim fsGroup alone makes the existing root group-private. This is confined to the private ephemeral Pod volume, not a host/shared cross-job workspace, but must be documented rather than made the new identity solution.

An initial proposal used an operator-pinned helper to change fresh roots to 2770. **Scope review rejected this additional hardening:** it was not needed to make image identities work. Helper-free tests of root, named, registered numeric and arbitrary numeric users with ordinary runtime defaults passed workspace writes/transfers both without and with fsGroup, and actual fixed-dockerd socket access with fsGroup. The helper and its configuration were removed before release. Existing emptyDir semantics remain; the job startup creates staging paths as the same identity used for Exec/tar. World-write within the private ephemeral Pod volume is a documented security characteristic to revisit separately, not an identity fix.

The inspected API has EmptyDir.mode, but it is **alpha/feature-gated**. Do not assume deployment support or modify node gates to use it. fsGroupChangePolicy has no effect on emptyDir (source passes nil); “OnRootMismatch solves this” is incorrect. No native mode feature or permissions subsystem is introduced in this job-only milestone.

Setgid ensures inherited **group**, not group-write under every umask. This job needs no precreated cross-UID staging paths: its own startup/Exec/tar share one identity. Files private to that UID can remain private; any future deliberate multi-user sharing would need separate validation. Workload chmod/umask and nested container users still matter. Services that drop groups via gosu/setgroups should not receive shared workspace by default; profile-specific sharing needs its own test.

Initial `bash examples/identity-volume-probe.sh` research tested a proposed 2770 root:10001 directory. Following scope review it now models the **existing 02777/root:10001** roots with Docker `--group-add 10001`, both aliases and a **fake** root:10001 mode-0660 socket. Root, named node (1000), numeric 10001 (primary GID 0), and numeric 23456:34567 write/connect without capabilities. Volumes/containers are removed. This standalone probe verifies Linux permission mechanics, not kubelet fsGroup or actual dockerd; the expanded opt-in Go suite below additionally tests real fixed dockerd and streaming transfers.

## 11. Ordinary OS package installation

**Yes**, a root image should permit normal package setup if the operator/cluster approves ordinary runtime execution. It need not be a privileged container. Writable rootfs, network access, image package tooling and the requisite ordinary capabilities must all be present.

Local control: UID 0, no-new-privileges, drop ALL cannot `chown 1000:1000 /tmp/owner-probe` (EPERM); the same standard image with ordinary Docker runtime capabilities succeeds. Thus “omit runAsUser, leave drop ALL” is not sufficient to promise apt/dpkg/post-install account switching. The simplified implementation removes drop ALL and **does not add a custom list**. Ordinary Docker runtime capabilities passed apt installation, ownership changes, user switching/chroot and cross-UID supervision with no-new-privileges/default seccomp. Recorded root CapEff/CapBnd was `00000000a80425fb`; target Kubernetes runtime/admission may differ. KILL within that runtime baseline enables cross-UID supervision, not an AppArmor workaround. See the actual contract/limits in [job-security.md](job-security.md). Services remain unimplemented. No Firefox/Playwright-specific implementation or custom CI image. A preinstalled standard browser image can be faster but does not replace correct root/user semantics; it must also support the selected architecture.

## 12. Generic services → Kubernetes

Recommended smallest topology **within a documented protocol subset**:

```text
one ephemeral Pod (one lifecycle/deadline/resource envelope)
  job                 Runner exec/copy target
  svc-db              native image ENTRYPOINT/CMD, private writable layer
  svc-cache           native image ENTRYPOINT/CMD, private writable layer
  dind                existing fixed daemon OR one approved requested Docker profile
```

No Kubernetes Service, hostPort, hostNetwork, PVC or new external endpoint. Containers share Pod networking but not filesystems or automatically their volume mounts.

Translate image and literal env to Kubernetes fields; sort/validate names/env, reject NUL/unsupported port forms and preserve empty values. **Kubernetes EnvVar.value itself expands `$(NAME)` and collapses `$$`**, unlike a Docker env assignment: escape each supplied `$` as `$$` before placing a literal Runner service value in Pod Env, and test `$`, `$$`, `$(NAME)`, newlines and empty values after kubelet processing. No shell evaluation. See core/v1/types.go EnvVar and [kubelet_pods.go makeEnvironmentVariables](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/kubelet_pods.go), `expansion.Expand`. Generic Command/Args/WorkingDir unset preserves the service image's startup semantics. Assign deterministic collision-free container names and validate service hostname compatibility; reject invalid/reserved names with actionable errors, never silently rewrite aliases. Cap service count and assign operator-controlled CPU/memory/ephemeral requests/limits to **every** service. Ordinary services remain **unprivileged**, but root/default capabilities are a separate operator policy, not synonymous with privileged.

Service storage is ephemeral. Image-declared volumes require explicit accounting tests: after the demonstrated Docker VOLUME shadowing defect, do not assume arbitrary runtime image-volume paths are bounded by writable-layer limits. If important data paths need separate capped emptyDirs, use explicit operator profiles for exact destinations, not a guessed universal database path. No persistent-state guarantee or private-image credential feature is added on this wire contract.

## 13. Networking, ports, readiness, failures and logs

- Kubernetes container names do **not** create service DNS. Add validated service-ID HostAliases pointing to **127.0.0.1** inside this Pod; service ID and localhost then reach the shared listener. No Service object is needed for intra-Pod access. Invalid/duplicate/reserved aliases fail before creation.
- ContainerPort is metadata, not forwarding. A conservative first subset accepts `N`, `N/tcp`, `N:N[/tcp]` (and explicitly documented UDP support if chosen). **Reject**, never silently discard, unequal host/container mappings, host-IP binds, random/range mappings and unsupported transports. Don't map them to hostPort. Distinct Pod-IP Services/proxies would be a different, larger model. Colliding declared listener ports/protocols fail early; undeclared/image-default collisions can only be discovered at runtime. Multiple default Postgres listeners on port 5432 are not transparently supported by one Pod.
- Start waits for required job, all required services and any Docker profile readiness under existing cancellation/startup timeout. Detect missing/not-ready status, scheduling/image/config failures, init-helper errors and any terminated required service (including exit 0). Never treat job Ready alone as sufficient; never delete on Start failure instead of awaiting Remove.
- Proposed baseline: native entrypoints with a TCP readiness probe on a documented primary declared TCP port, while operator-known service profiles may supply application-level probes (Redis ping/Postgres readiness/Docker info). TCP acceptance is **not application health**. Ports absent/UDP-only cannot justify strong readiness; choose/document running-only versus reject/require a profile before implementation. Don't claim omitted Docker health options or image HEALTHCHECK are honored.
- Kubelet TCP probes originate outside the Pod and default to PodIP ([prober.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/prober/prober.go), TCPSocket case). `host: 127.0.0.1` targets the node, **not Pod loopback**. Loopback-only services require an in-container exec probe with image-supplied tools or an explicit supported profile; never fabricate readiness by probing node localhost. Kubernetes has one readiness probe per container, so multiple-port/all-port health needs a separate contract.
- After Start, v1alpha offers no asynchronous health channel. Initially report subsequent known terminal service states at an appropriate RPC boundary without corrupting command exit/copy semantics; document the lack of continuous service supervision. Adding watches/stream cancellation is a separate behavior/RBAC decision, not silently bundled here.
- Service stdout/stderr goes to normal Kubernetes container logs, not mixed into job Exec output. Start failures identify service/container and reason without dumping env. Automatic log excerpts require new `pods/log get` permission, bounded/redacted handling and a stream policy; not assumed under current minimal RBAC. Operators can inspect container logs separately.

These wire limitations prevent claiming full Docker service parity. Supporting conventional one-Postgres/one-Redis services is feasible, but port/readiness compatibility must be agreed before code, rather than transformed silently.

## 14. Privileged-service policy

No workflow privilege flag exists. Use a **small operator-owned mapping** of a reserved service role/name plus normalized immutable image reference to a built-in approved profile; not a generic PodSpec patch language.

Example behavior, not config syntax: role `docker` + operator-approved `docker.io/library/docker@sha256:<index digest>` → fixed Docker profile. Both name and digest must match; matching an arbitrary image basename/tag or service name alone never grants privilege. Reserved-role mismatch fails before Pod creation; non-profile ordinary services never get Privileged=true. Unsupported requested profile env/ports must fail rather than override fixed daemon settings. Immutable multiarch index references are supported; no tag-to-digest trust shortcut.

The approved Docker profile owns privilege/root/daemon-only profiles, fixed Unix-only args, protected daemon env, data/socket caps, resource policy, mounts and readiness. Deny environment that can change those protected settings; do not allow arbitrary workflow flags/command or LD_PRELOAD/PATH injection into the privileged process through a broad env passthrough. Generic service env remains ordinary env, not a security-context request.

Allowlisting a daemon is **not** a sandbox against code controlling its Docker API: that job can launch powerful nested containers and exploit the elevated kernel surface. The operator must explicitly trust that role/repository set. No host runtime socket/paths/namespaces/token is added, but privilege still needs trusted-CI admission/isolation. AppArmor's existing job decision is unchanged.

## 15. DinD migration

Keep fixed `JOB_DIND_ENABLED` and all existing behavior/code while developing requested profiles. Ordinary services may coexist with fixed DinD under the eventual model. Do not launch a second daemon: explicitly requesting the reserved Docker role while fixed injection is enabled should fail clearly, not silently shadow sockets/defaults or reinterpret a request.

Validate requested Docker services with fixed injection disabled **only in a separately authorized validation deployment**, retaining the proven fixed mode for rollback. Once real ARM64 build/load/smoke/cache/push/storage/cleanup succeeds, the operator can migrate the same plugin instance to workflow-selected Docker services: publish job requests it; verification job requests none and receives no daemon. No permanent selective classes/multiple instances or forced application heartbeat. Do not remove fixed mode until replacement has real-cluster acceptance.

## 16. Docker endpoint ergonomics

A built-in approved Docker profile wires the same private Unix socket and shared bind paths, supplies default DOCKER_HOST to job Pod env, Start image/default environment and Exec defaults (which must survive env -i), and preserves Runner explicit overrides as a documented precedence choice. Use stored Pod defaults across config reload/restart. Job workflows need Docker CLI/buildx, not knowledge of `/run/forgejo-docker/docker.sock`; no TCP port/service hostname requirement.

Only this profile gets Docker-specific socket/workspace/TMPDIR wiring. Generic databases do not. Shared-path/private-/tmp semantics remain documented; endpoint ergonomics cannot make arbitrary private bind paths daemon-visible.

## 17. Implementation gate decision

The operator authorized job identity only, removed volume hardening, then explicitly removed compatibility/multiple-mode requirements. Required operations passed under ordinary runtime defaults, so the implementation now has **one normal image-native model**, no fixed UID/profile/capability-list/helper/NSS machinery. Outer boundaries, bounded universal env discovery, template/lifecycle safety and existing fixed daemon/service rejection remain. fsGroup is conditional on socket access. This default change is deliberate and reviewed as local implementation, not published or deployed. Configuration details: [job-security.md](job-security.md).

## 18. Tests and validation performed

Initial local research passed 14 homedir controls, four group/dual-path/fake-socket cases and a capability ownership contrast. Implementation tests cover ordinary Pod fields/runtime-policy omission, conditional fsGroup/no init, env framing/precedence/non-shell keys/empty values/redaction/limits/cancellation, stored daemon defaults/restart, deterministic retries and unchanged service rejection. Real-gRPC virtual-time quiet/silent/nonzero/cancellation/deadline cases include env discovery and retain old transport-policy negative controls.

Opt-in local container tests cover root, named, numeric-account and arbitrary-numeric credentials with **DinD off/on**, no job cap-add/cap-drop, and no forced group without DinD. Generated job startup/Exec and streaming CopyIn/CopyOut across aliases pass; HOME present/empty/absent plus sanitized-child NSS are checked; actual pinned fixed-DinD `/info` access passes via root:10001 mode-0660 socket. A separate package/chown/user-switch/chroot/supervision case uses ordinary runtime defaults. Test-only setup models kubelet 0777 or fsGroup02777 roots, not a production helper or directory-hardening step. Go ordinary/race tests and vet pass; formatting/diff, generated/protocol and ARM64 cross-build checks are retained. These do **not** prove real Kubernetes image USER/fsGroup, ARM64 runtime enforcement, DinD build/publication/storage enforcement or the specific application failure. Service tests/implementation remain future work.

## 19. Remaining uncertainties

The exact failing child image/digest, effective UID/GID, HOME state, Node/libuv version and NSS result remain uncaptured. This mechanism is proven but that application's failure is not declared fixed. Image-native numeric users without passwd support can still fail.

Universal image-env framing/limits and ordinary shared-path/transfers/socket/package access have local coverage, but actual kubelet USER/group/mode behavior, target RuntimeDefault enforcement, existing fixed dockerd compatibility and the application's child require separate validation. Commands that deliberately drop supplemental groups can lose shared/socket access. Runner can discard explicit-empty intent before the Exec wire map; the plugin cannot recover it. Image HOME may itself be unwritable and unregistered image UIDs remain honest NSS limitations. Per-Exec rediscovery adds a bounded exec round trip. Runtime capabilities are not plugin-defined and may differ from local Docker. No old-identity compatibility is promised; drain/remove old jobs before deployment.

Services' port/readiness/storage/privilege/logging questions remain future design, not implemented recovery mechanisms. Missing wire fields cannot be recovered by Kubernetes objects alone.

## 20. Integration acceptance: current subset versus future services

**For this job-only implementation, use the narrow six-step plan in [job-security.md](job-security.md#validation-and-separate-acceptance): authorized immutable release, retained accepted AppArmor/fixed DinD, actual credentials/HOME/NSS, unchanged verification rerun, separate package check and cleanup/isolation. No services or DinD migration.**

The original broader acceptance list below is retained as **future executor-model research**. Its service/requested-Docker items are explicitly deferred, not authorized by this milestone. After any future full model is reviewed/separately authorized:

1. First capture failing-child Node/libuv, exact selected image and runtime digest, euid/egid/groups, HOME **presence** (not a full secret env dump), libc/NSS/getent result, relevant passwd entry and complete error. Run the standalone probe under the actual job identity, then compare intended image-native identity. Do not modify application tests. Re-run unchanged verification; only that agent may attribute or declare the homedir failure fixed.
2. Ordinary jobs must retain outer isolation/resource/transport/lifecycle behavior, Actions/transfers/nonzero/quiet success, fixed DinD build/load/smoke/cache/push and cleanup, not the old forced UID policy. Keep accepted Unconfined job AppArmor configuration; no new signal investigation.
3. Image-native jobs: standard root image with no USER, a standard named-user image, a declared numeric-user image with passwd entry, and a deliberately unregistered numeric UID. Verify actual credentials, native group memberships, HOME and sanitized-child homedir behavior, `/workspace`↔`/shared` writes, safe modes and CopyIn/CopyOut. The unregistered UID/no-HOME case should remain an honest expected image limitation, not an injected passwd fix.
4. Ordinary runtime root jobs perform a small OS package install/ownership/user-switch setup with Privileged=false, no host access/token, runtime baseline/no-new-privileges and unchanged seccomp/AppArmor. Workflow cap_add remains unsupported; no custom job profile is introduced. This is not a Firefox workaround or custom-image build.
5. No services, one real Postgres, then Postgres+Redis: inspect images/entrypoints/env, aliases/localhost, exact declared ports, readiness-before-first-command, per-container limits and persistent-path ephemerality/accounting. Test slow startup, loopback-only probe behavior, invalid image/env/ports/aliases, conflicting listeners, process exit 0/nonzero, startup timeout/cancellation and failure during a job. Capture container logs/status without exposing secrets.
6. Retry same name with shuffled service/env ordering must reuse only the same template; changed image/env/ports/profile/identity/resources must reject conflicts. Test plugin restart/config reload with stored Pod defaults and explicit old-Pod behavior. Explicit Remove deletes job/services/storage on success, failure and cancellation; observe disappearance and no leftovers rather than assuming deletion request proves completion.
7. Approved requested Docker role, fixed mode disabled only for authorized validation: reject wrong role/digest/protected env/ports and duplicate providers. Verify only the approved daemon is privileged, socket group/default DOCKER_HOST, exact capped data root and shared binds/private tmp. Repeat native ARM64 Buildx build/load, direct smoke exact-image checks, cache/push and storage/cleanup. A verification job without Docker service has no daemon. Only after success migrate instance defaults; retain fixed-mode rollback.
8. Reconfirm ordinary services/jobs cannot reach unauthenticated plugin gRPC or gain plugin credentials/host runtime mounts. Service aliases must not imply cross-job connectivity. No real integration is performed by this repository task.
