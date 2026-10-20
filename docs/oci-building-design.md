# Image building ownership

The supported image-build path is Buildx's remote driver connected to an
operator-managed persistent BuildKit service. Registry layer caches alone do not
retain writable Cargo cache mounts; preserving the daemon's state does.

The operator owns the Deployment, Service, PVC, privileges, resources, garbage
collection and network access. The plugin owns only disposable execution Pods.
Do not add builder lifecycle management, endpoint selection or credential APIs
to the plugin. Workflow environment and ordinary job-scoped credentials suffice.

Jobs provide Docker CLI/Buildx, choose the remote endpoint and use `--push`.
A local Docker daemon is unnecessary. Registry credentials belong to the
disposable client session, not permanent BuildKit configuration. Writable cache
mounts must be scoped to the project and use locking where concurrent writers
are unsafe.

DinD sidecars, Docker socket/data volumes and `JOB_DIND_*` settings have been
removed. There is no fallback or builder-selection framework. This is a breaking
change for workflows using `docker run`, `--load`, Compose or daemon-local image
inspection: remote BuildKit builds images; it is not a container execution API.
Workflow services and container Actions remain unsupported.

Deploy and verify the remote path before upgrading the plugin. Drain active jobs
before the upgrade; do not migrate live environments across template changes.
Validate separate job Pods, source-only recompilation, builder replacement,
concurrent clients and cleanup independently of application publication CI.
