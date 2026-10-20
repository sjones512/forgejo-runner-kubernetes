# Executor model

One environment is one disposable Kubernetes Pod with one ordinary job
container. The plugin implements Runner's Create, Start, Exec, CopyIn, CopyOut
and Remove contract; it is not a scheduler or application controller.

Image identity and runtime capability defaults apply, with explicit
non-privilege/no-new-privileges and RuntimeDefault seccomp. The plugin does not
invent security profiles, accounts, permissions helpers or compatibility modes.
See [job security](job-security.md) for the identity/environment boundary and
[pod policy](pod-policy.md) for operator resource choices.

Runner owns workflow environment and job credentials. The plugin forwards
execution and file transfers; it does not fetch image metadata from registries
or inspect workflow event types to grant permissions.

Only explicit Remove deletes the owned job Pod. Cancellation of a transfer can
be normal Runner step processing and must not destroy the environment. A
remaining Pod after a plugin crash or uncertain API response needs operator
cleanup; there is no automatic orphan controller.

Workflow services, capability additions, backend options, per-command users and
container Actions remain unsupported. Image building uses the
[operator-managed remote BuildKit path](oci-building-design.md), not sidecars.
No future Docker service-profile framework is planned as part of this migration.
