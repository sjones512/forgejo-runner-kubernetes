# forgejo-runner-kubernetes (experimental)

Proof-of-concept **execution plugin**, not a new CI engine. Licensed under [MIT](LICENSE). Forgejo Runner handles workflows; this gRPC server uses its in-cluster Kubernetes identity to create one ephemeral Pod per job and forward file transfers, stdout/stderr and process exit status through Kubernetes `pods/exec`. **Not production-ready. No end-to-end Forgejo workflow or ARM64 k3s run has yet been verified.**

Target: **Forgejo Runner v13.2.0** (`plugin.v1alpha`, commit `df6b843fb929bb04b933b09c6bf208774d42480b`). See [research / protocol mapping / tradeoffs](docs/research.md) and the [cluster integration contract](docs/cluster-integration.md). Protocol is alpha and may change without notice. Implemented RPCs: Capabilities, Create, Start, Exec, CopyIn, CopyOut, Remove, plus gRPC health. **Unit and protocol-streaming tests, race tests and `go vet` pass; an ARM64 Go build succeeds. First real Forgejo + ARM64 k3s test reached Pod creation/exec/stdout but failed during post-step CopyOut on v0.1.0-alpha.1; a successful end-to-end run has NOT been demonstrated. Production use is not recommended.**

## Installation experiment

Build the OCI image for your architecture (`docker buildx build --platform linux/arm64 ...`), substitute its name in [`deploy/example.yaml`](deploy/example.yaml), create `forgejo-jobs` and `forgejo-runner` namespaces, apply [`deploy/plugin-network-policy.yaml`](deploy/plugin-network-policy.yaml) **first** (edit its trusted Runner pod selector), then deploy the example. The example expects the trusted Runner in `forgejo-runner`; if yours is elsewhere, change the manifest namespaces, RoleBinding subject and NetworkPolicy selectors accordingly. Verify the CNI enforces NetworkPolicy and that untrusted job Pods cannot connect to port 50051. Alternatively, co-locate plugin and Runner in one Pod using a UNIX socket (not covered by this example). This is an example, **not** Flux configuration. The plugin requires `rest.InClusterConfig()` and a ServiceAccount; it cannot run outside a cluster without code changes. Its namespace Role needs only `pods` create/get/delete and `pods/exec` create; no cluster-admin, secret access or node socket. Run the Runner where it can reach the plugin Service DNS. Runner config (Runner v13.2.0, default `container.workdir_parent: workspace`):

```yaml
plugins:
  kubernetes:
    address: forgejo-runner-kubernetes.forgejo-runner.svc.cluster.local:50051
runner:
  labels:
    - 'k3s:kubernetes://ubuntu:24.04'
```

Use `runs-on: k3s`, for example:

```yaml
on: push
jobs:
  hello:
    runs-on: k3s
    steps:
      - run: |
          uname -a
          echo 'hello from Kubernetes'
```

This is a **proposed test**, not a verified passing workflow. `JOB_NAMESPACE` defaults to `forgejo-jobs`, `JOB_IMAGE` to `ubuntu:24.04`, `JOB_ARCH` to `arm64` (`amd64` for disposable kind tests), `PLUGIN_LISTEN_ADDR` to `0.0.0.0:50051`. The label suffix must equal `JOB_IMAGE` for plain jobs (`Create.image` is empty without `container:` in Runner v13.2.0); other images, services, per-command user, backend options, capability additions and container actions are **unsupported**. Runner `container.workdir_parent` must be `workspace`. The job image must contain `/bin/sh`, `/usr/bin/env`, `mkdir`, GNU `tar`, `sleep`, and (for default shell steps) `bash`. Tar archives are streamed by the plugin but extracted/created using `tar` *inside the job image*; this is not independent of image utilities. The `ubuntu:24.04` multiarch image is the *only* image this milestone is intended to use. Runner can override the label image with `jobs.<id>.container.image`; an incompatible override is rejected. No helper/privileged container, Docker/Podman, kubectl, persistent workspace, arbitrary image ENV discovery, or Docker actions.

`Start` waits up to 3 minutes for readiness and diagnoses some unschedulable/image-pull failures. Job Pod has `restartPolicy: Never`, bounded CPU/memory/ephemeral storage, nonroot UID, dropped capabilities, RuntimeDefault seccomp, `allowPrivilegeEscalation: false`, and **no ServiceAccount token**. One `emptyDir` is mounted at both `/shared` (runner staging) and `/workspace` (default job working directory). Container root filesystem is not read-only because `/tmp` and other image paths can be needed. The Runner's requested environment lifetime becomes the Pod active deadline; it is not an automatic TTL deletion.

The plugin is **trusted infrastructure**: Kubernetes RBAC does not restrict the *contents* of Pods it creates. **There is no gRPC authentication in Runner v13.2's plaintext TCP plugin connection. Without enforced ingress isolation, a job Pod can call the plugin directly and ask it to provision Pods using the plugin's Kubernetes privileges.** Treat the example's ingress NetworkPolicy (or a co-located private UNIX socket) as mandatory, not optional. Isolate the Service from all untrusted workloads and do not expose it outside the trusted cluster. A job Pod receives no Kubernetes API credentials but still has ordinary Pod network access by default, and can access whatever the network allows. Configure NetworkPolicy, namespace quota and other admission controls separately as appropriate. This is Kubernetes container isolation, **not** a VM security boundary. `pods/exec` arguments/environment can be visible to cluster administrators.

## Development

Go 1.26+: `go test -race ./...`, `go vet ./...`, `go build ./cmd/forgejo-runner-kubernetes`; `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/forgejo-runner-kubernetes` for ARM64. GitHub Actions is the bootstrap CI; do not move this project's own CI onto the plugin. Pull requests and manual dispatches validate development; **a pushed `v*` tag runs validation and publishes in one workflow run** (pushing `main` does not launch a duplicate run). Tag CI validates before building/publishing a versioned `linux/amd64` + `linux/arm64` image to GHCR, and publishes binary artifacts, checksums and the **immutable OCI digest** in the GitHub prerelease. Do not use `latest` or deploy a tag when a digest is available. Next: integration test with a disposable kind/k3d cluster (amd64), then an actual v13.2.0 Runner and ARM64 k3s workflow. Verify both stdout/stderr content, nonzero exits, missing env files, and deletion.

Known gaps: no Runner end-to-end test or Kubernetes exec test yet; tar copy is dependent on GNU tar and has not been hardened against malicious archives; no orphan sweeper after plugin crash or API uncertainty, and pod deletion is requested but not watched to completion; only explicit Remove deletes the Pod (a cancelled Kubernetes exec stream may not reliably kill its remote command before Runner removes the environment); startup diagnostics are limited; image ENV is not inspected; no generalized filesystem helper. Pod identity is deterministic from Runner's per-task name, with labels and an annotation for later orphan tracing. **Do not deploy this to run untrusted public repositories without further security review.** Stop after the first real successful workflow and reassess protocol fit, risk and cleanup before expanding scope.
