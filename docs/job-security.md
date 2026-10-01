# Operator job identity/security profiles (v0.1.0-alpha.11)

This prerelease implements **job profiles only**, not workflow services. The existing default remains compatible. Publication was separately authorized after local implementation review; deployment and real-cluster acceptance remain pending. Review [executor-model research](executor-model.md) for pinned Runner/Kubernetes/Node sources and supplied integration evidence.

## Configuration

| Plugin Deployment setting | Behavior |
| --- | --- |
| `JOB_SECURITY_PROFILE` unset/empty or `fixed` | Default compatibility profile: job UID 10001, runAsNonRoot true, unspecified primary GID, capabilities drop ALL. No permission helper or image-env discovery. |
| `JOB_SECURITY_PROFILE=image` | Honor selected image USER/default UID and primary GID: no runAsUser/runAsGroup/runAsNonRoot on Pod or job container. Keep drop ALL. Root identity is allowed if image/admission allow it, but ordinary package-manager capabilities are **not** granted. |
| `JOB_SECURITY_PROFILE=image-ci` | Same image identity, but operator explicitly grants the bounded CI capability set below. It does not force UID 0, infer policy from USER, or make the job privileged. Use a deliberately root-capable image for root package setup. |
| `JOB_PERMISSIONS_IMAGE` | Required for `image`/`image-ci`: operator-owned standard Linux helper pinned as `name@sha256:<64 lowercase hex>`. Must provide `/bin/sh`, mkdir, chgrp and chmod and support the job architecture. No default/mutable helper image. Forbidden with the fixed profile to avoid silently ignored configuration. |

Unknown/case-mismatched/whitespace profile values, missing/unpinned/invalid helper images and contradictory fixed+helper configuration fail plugin startup. Profiles are instance-wide operator policy; workflow image selection, env, container options and backend options cannot change them. There is no Kubernetes securityContext passthrough.

Example opt-in (helper index verified to contain amd64 and ARM64; local execution tested on amd64 only):

```yaml
env:
  - name: JOB_SECURITY_PROFILE
    value: image
  - name: JOB_PERMISSIONS_IMAGE
    value: docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092
  # Only where the operator has already accepted this job-container override:
  - name: JOB_APPARMOR_PROFILE
    value: unconfined
```

To preserve the default, leave **both** new settings unset. To opt into bounded package/user setup, explicitly choose `image-ci`, not just an image with USER=root. For a non-root image needing only its correct identity, choose `image`; it receives no extra capabilities. Selecting `image-ci` grants its policy regardless of the declared UID and can confer user-switching powers even with a non-root image, subject to runtime capability handling. Do not treat it as an identity-only switch.

## Security and compatibility

All profiles retain Pod fsGroup **10001**, RuntimeDefault seccomp, existing job AppArmor selection, job resource/storage budgets, ephemeral volumes, restartPolicy Never, no ServiceAccount token, no hostPath/host namespaces and no privileged job. The compatibility UID/non-root restrictions moved from Pod to **job-container** securityContext with the same effective job identity. Neither legacy nor image profiles impose a job primary GID; fsGroup is a supplemental storage/socket group.

AppArmor remains unspecified unless configured. The supplied trusted environment's accepted Unconfined job override is honored, **not made a default**. Helper AppArmor is unspecified and helper seccomp inherits Pod RuntimeDefault. Fixed DinD retains its existing explicit privileged/root/daemon-only Unconfined profiles. No profile relaxes job seccomp or the trusted-Runner-only unauthenticated gRPC boundary.

The native profiles introduce a UID-0 permission init helper, even for a non-root job image; therefore they require admission permitting that helper. Restricted PSA can reject them. The supplied Privileged-PSA trusted CI namespace admits a wider envelope; it does not automatically make ordinary jobs privileged. Admission may impose additional identity/security constraints: validate actual runtime credentials, not just intended fields.

### Bounded `image-ci` capabilities

Drop **ALL**, then add exactly:

```text
CHOWN DAC_OVERRIDE FOWNER FSETID SETUID SETGID SETFCAP SYS_CHROOT KILL
```

Rationale, based on [Moby's pinned conventional defaults](https://github.com/moby/moby/blob/464cd50c3d9e92877d56940ea160de6fca7bea23/daemon/pkg/oci/caps/defaults.go), [Linux capabilities](https://man7.org/linux/man-pages/man7/capabilities.7.html) and [dpkg's chroot behavior](https://manpages.debian.org/bookworm/dpkg/dpkg.1.en.html):

- CHOWN/DAC_OVERRIDE/FOWNER/FSETID: package file ownership, access, modes and set-ID preservation.
- SETUID/SETGID: ordinary account switching, including package-manager helper users.
- SETFCAP: package-installed file capability metadata. No-new-privileges/bounding policy still constrain later execution.
- SYS_CHROOT: package-maintenance chroots; no host root is mounted.
- KILL: ordinary **cross-UID child supervision** after switching users. A local control gets EPERM without it and succeeds with it. This is not a containerd/AppArmor workaround; it neither disables nor overrides an LSM denial, and is not added to `fixed` or `image`.

No SYS_ADMIN, NET_ADMIN, SYS_PTRACE, NET_RAW, MKNOD, SETPCAP, AUDIT_WRITE, NET_BIND_SERVICE, wildcard/default capability inheritance or privileged job. Existing no-new-privileges remains in force. This is a bounded conventional CI profile, not a guarantee that every package, daemon or file-capability binary works. Do not broaden it speculatively when a package requests unavailable operations.

Local standard Debian/Node testing under the **exact generated set**, no-new-privileges and Docker default seccomp passed: apt update/install `hello`, ownership/mode changes, switching to node, chroot, and signalling a switched-UID child. Recorded CapEff/CapBnd `00000000800400fb`, NoNewPrivs 1, Seccomp 2. This does not prove enforcement under the actual cluster's runtime/seccomp configuration. No application tests or CI image were changed/built.

## Image environment and HOME

The fixed profile keeps previous behavior: Start reports only plugin defaults and fixed PATH; Exec clears inherited env and supplies HOME=/shared/workdir only when absent. Explicit Runner HOME, including an empty value, is preserved. No image-env probe is introduced for this profile.

For `image`/`image-ci`, Start executes **only `/usr/bin/env -0`** in `job` after helper/job/DinD readiness. It does not run the image application entrypoint or query registries. It discovers the effective inherited image/Pod environment with NUL framing, including non-shell keys and values containing newlines/equals. Images need an env utility supporting `-0`. Discovery is bounded to **30 seconds / 64KiB**, rejects malformed/duplicate entries, discards probe stderr and reports no raw environment/transport detail in errors.

Order for command defaults:

1. Captured image/runtime environment.
2. Stored plugin-owned fixed-DinD DOCKER_HOST/TMPDIR defaults, if present.
3. PATH fallback only if absent; HOME=/shared/workdir only if absent.
4. Explicit Runner Exec env wins, **including empty values**.

Native Start advertises those base defaults in StartComplete.image_env. Exec rediscovers them (one bounded probe per call) so clearing env does not lose meaningful image settings, and plugin restart/config change does not depend on a volatile cache or secret Pod annotation. Copy operations remain on the same job identity and existing image tools. Env is not printed or persisted by the plugin; it is sent over the existing trusted Runner RPC contract. Image HOME may point outside shared storage and is the image's responsibility to make usable.

Pinned Runner's image-env merge treats an existing **empty** workflow value as unset and composes PATH specially. The plugin honors empty values where they reach Exec, but cannot reconstruct intent that Runner already discarded before serializing that request. Do not claim perfect workflow-empty precedence beyond the wire.

HOME does **not** create a passwd/NSS account. Registered root/named/numeric image identities can resolve their passwd home when a child removes HOME. Legacy UID 10001 or an image's deliberately unregistered numeric UID can still produce `uv_os_homedir ENOENT` in a sanitized child. That is an honest identity/image limitation, not hidden by a fabricated account. Local diagnostics prove this; the specific supplied application failure is **not declared fixed** until separate integration confirms it.

The executor still replaces ENTRYPOINT/CMD with the long-lived CI command and uses Runner argv/workdir under /shared or /workspace. It does not provide full image application startup/WORKDIR semantics, per-exec user switching, missing tools, arbitrary Docker options or container actions.

## Shared workspace and fixed DinD

Legacy gets the same capped disk emptyDir shape and no new helper/mode changes. Its volume root can remain kubelet's broad 02777; fsGroup does not remove world permissions.

Native profiles run a **separate operator-pinned** `permissions` init container before workloads. It mounts only workspace at /shared and, if fixed DinD is enabled, its existing socket volume at /run/forgejo-docker. It normalizes these fresh roots and /shared/{act,toolcache,workdir,tmp} to root:10001 **2770**, using group membership rather than capabilities. It does not recursively chmod job data, modify passwd, touch Docker data or use the workflow-selected image as helper.

Helper: UID 0, primary GID 10001, no escalation, drop ALL, no privilege/token/host access. Requests **10m CPU / 16Mi memory / 1Mi ephemeral**, limits **100m / 64Mi / 64Mi**. It shares Pod deadline/startup timeout and ephemeral cleanup. Pull/config/exit failures block Start; required helper success precedes StartComplete. No alpha EmptyDir mode feature is required.

Both aliases remain one capped storage volume. Group 10001 enables root/named/arbitrary numeric users to write without world-write and reach fixed DinD's root:10001 mode-0660 socket. Setgid supplies group inheritance, not universal group-write despite arbitrary workload umasks/chmod. A process that explicitly drops supplemental groups may lose socket/shared-group access; normal plugin-controlled job startup does not drop them. New package/user-switching commands must retain suitable groups when they need shared paths.

`JOB_DIND_ENABLED` remains unchanged: one fixed daemon per job when enabled, same image pin, args, resource/data caps, probes, exact data root, shared binds/private-/tmp contract, default DOCKER_HOST and explicit Runner override precedence. Only the job/helper profile changes. Ordinary job privilege is never needed to access its socket. No requested services or DinD migration.

## Retry and restart

Every newly created Pod records the normalized profile and a deterministic full **job-template** SHA256; fixed DinD retains its annotation too. Normalized profile and explicit env-policy version, identity, capability set, helper image/command/resources/mounts, job AppArmor/storage/resources and daemon policy participate. The hash excludes API defaulting and remaining Runner lifetime; unset and explicit `fixed` produce identical templates.

Conflicting retries return AlreadyExists with a generic template conflict, not a secret/spec dump. Pre-profile Pods have no new hash: **Create retries fail safe**, including under the fixed profile; drain outstanding jobs before upgrade/reconfiguration where possible. Their later Start/Exec/copy/Remove RPCs remain supported as legacy (Start/Exec verify the missing-profile Pod's actual fixed-UID/non-root shape rather than treating a stripped native Pod as legacy). New Pods' later RPCs use the **stored profile and defaults**, never current config to reinterpret identity. Unknown stored profiles block Start/Exec; explicit Remove remains available. The plugin does not patch/delete a live conflicting environment.

## Local validation and separate acceptance

Unit/protocol tests cover default and all profiles, exact fields/caps, helper bounds/gates, env precedence/framing/cancellation/timeout/size limits, stored-profile restart behavior, retry conflicts, existing DinD and service rejection. Full `go test ./...`, `go test -race ./...`, `go vet ./...`, twenty focused race repetitions, formatting/diff and standalone diagnostic syntax checks passed, as did the optional local Docker suite and a static Linux ARM64 cross-build. The imported/generated pinned Runner protocol is unchanged (proto SHA256 `961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`). Native quiet success/silence/nonzero/cancellation/deadline uses real gRPC/bufconn and virtual time, alongside unchanged legacy controls.

Optional local diagnostics (Docker default context must be a local Unix endpoint):

```sh
EXECUTOR_IDENTITY_CONTAINER_TESTS=1 go test -race ./internal/plugin -run '^TestIdentityContainers$' -count=1 -v
```

This runs generated helper/Exec argv against standard pinned BusyBox/Node images: legacy UID, native root, named/numeric registered accounts, unregistered numeric UID, and CI-root. Each checks HOME/sanitized-child NSS, HOME writability, both aliases and a **fake** Docker socket. Named/numeric Docker --user emulates runtime credentials, not proof of actual Kubernetes image USER. A separate local package subtest uses the generated capability list and public apt mirrors. No Kubernetes or actual dockerd validation occurs here.

**Deployment requires separate authorization.** Use the immutable v0.1.0-alpha.11 image digest recorded in its GitHub prerelease, then perform the following acceptance plan:

1. Drain/reconcile old environments; deploy the exact new plugin digest. Keep namespace, storage, fixed DinD and accepted `JOB_APPARMOR_PROFILE=unconfined` settings otherwise unchanged.
2. Choose `image` plus pinned helper for the first identity test; use `image-ci` only when its elevated policy is intended. Capture actual Pod/job/helper UID/GID/groups, capability/no-new-privileges/seccomp state, selected image/digest, HOME presence and passwd/NSS result. Confirm no token/host access/privileged job and helper 2770 modes.
3. Rerun existing pi-wright `ci.yaml` **unchanged**, capture the previously failing child's Node/libuv/euid/HOME/NSS and complete error. Determine whether `uv_os_homedir ENOENT` disappears and record remaining process-test failures. Do not change application tests, fabricate passwd or add output heartbeats.
4. Separately exercise standard images with root, named USER, numeric USER with account, and unregistered numeric USER on ARM64. Confirm native USER is actually honored, both aliases/HOME behavior, normal Actions/transfers/quiet/nonzero/cancellation and cleanup. Missing-account/no-HOME failure should remain an explicit image limitation.
5. If enabling `image-ci`, separately install a small ordinary OS package and test ownership/user switching/supervision with the exact bounded set and unchanged security profiles. No application/browser-specific workaround. Capture failures before contemplating any capability change.
6. Confirm publication jobs' **existing fixed DinD** readiness/socket/defaults, native build/load and direct exact-image smoke, registry cache/publication and capped data-root accounting still work. Check cleanup on success/failure/cancellation; test profile/helper conflicts and plugin restart. Do not implement/request workflow services.
7. Preserve and test job-to-plugin ingress denial. Only after review and separate acceptance decide any rollout; publication does not establish real integration success.
