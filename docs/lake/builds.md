# Lake in-cluster builds

Lake records an image build in its control Postgres and never talks to Kubernetes. With
`Lake.spec.builds.enabled`, the operator turns each queued `builds` row into one short-lived,
lake-owned Kubernetes Job running the private `oramasearch/lake-builder` image. There is no
shared BuildKit daemon and no developer-facing build path.

## Lake configuration

```yaml
spec:
  pullSecretName: oramasearch-dockerhub
  builds:
    enabled: true
    builderImage: oramasearch/lake-builder:<immutable-tag>
    cacheEnabled: true
    concurrency:
      perLake: 4
      perSpace: 2
    resources:
      requests:
        cpu: 500m
        memory: 1Gi
        ephemeral-storage: 2Gi
      limits:
        cpu: "2"
        memory: 4Gi
        ephemeral-storage: 20Gi
```

`builderImage` is required when builds are enabled. The builder Job inherits the Lake's image pull
Secret. Resource entries override the bounded defaults shown above one key at a time, so CPU,
memory, and ephemeral-storage remain bounded when a partial override is supplied. Concurrency
defaults to four active Jobs for the Lake and two for one registry space. Both configured limits
must be positive. Registry cache import/export defaults on and can be disabled explicitly.

Turning builds off stops the table poll. It does not kill a Job already created and does not remove
the per-space namespace, because doing either would make an in-flight build's outcome ambiguous.

## Row and credential contract

Lake owns the `builds` table and sends `NOTIFY lake_builds` after a request change. The operator
reads these fields:

`id, customer_id, repository, revision, context, dockerfile, target, build_args_json, desired,
phase, digest, size, message, token_hashes_json, created_at, updated_at`.

`desired` is `queued` or `cancelled`. A valid target has the form
`<registry-host>/<space>/<name>:<tag>`. The operator derives both isolation and cache identity from
that target rather than accepting a second, potentially conflicting space value.

Before creating a Job, the operator mints three independent credentials once:

- source read token scoped to the requested repository;
- registry push token scoped to the target `<space>/` prefix;
- event token scoped to `build-events.<build-id>`.

Their plaintext exists only in Secret `<job>-tokens` in the build namespace. The row receives a
canonical JSON object containing the three SHA-256 hashes; Lake resolves presented credentials by
hash and never stores their plaintext. A retry after an operator restart reuses the Secret and the
same hashes.

The Job receives the lake-builder contract exactly as environment variables:

| Variable | Value |
|---|---|
| `LAKE_URL` | Lake's in-cluster Service URL |
| `BUILD_ID` | row UUID |
| `REPOSITORY` / `REVISION` | pinned source identity |
| `CONTEXT` / `DOCKERFILE` | source-relative build inputs |
| `TARGET` | in-cluster Lake registry target |
| `BUILD_ARGS` | the row's JSON object |
| `BUILD_ARG_ALLOW` | `PYTHON_VERSION,APP_DEPS,APP_RUNTIME_IMAGE` |
| `SOURCE_TOKEN` / `PUSH_TOKEN` / `EVENTS_TOKEN` | Secret key references |
| `CACHE_REF` | `<registry>/<space>/cache/<name>`, or empty when caching is disabled |
| `DOCKER_CONFIG` | writable `/home/user/.docker` |

lake-builder fetches the repository tree and files from Lake REST at the pinned revision, validates
every build argument against `BUILD_ARG_ALLOW`, runs rootless BuildKit through
`buildctl-daemonless.sh` for `linux/amd64`, and pushes the image and optional registry cache through
Lake's in-cluster Service. Its final stdout line is JSON `{"digest":"sha256:…","size":N}`.

## Isolation and lifecycle

Each target space has a deterministic namespace `lake-<lake>-builds-<hash>`. The exact space is in
an ownership annotation; a hash collision or a namespace created by another owner fails the row
instead of adopting it. The namespace uses Pod Security `restricted` audit/warn and `privileged`
enforcement. Kubernetes Pod Security Admission has no per-pod exception: both restricted and
baseline reject the unconfined seccomp and AppArmor profiles rootless BuildKit requires. The
namespace is not tenant-writable, and only the builder container receives those two exceptions; it
still runs as non-root uid/gid 1000, cannot escalate privileges, drops every Linux capability, has
no service-account token, and receives no host mounts.

The namespace has default-deny ingress and egress. A second NetworkPolicy permits:

- cluster DNS on TCP/UDP 53;
- Lake pods in `spec.namespace` on TCP 80 for source, registry, and event REST;
- public TCP 80/443 for package indexes, excluding private, loopback, link-local, and metadata
  address ranges. Kubernetes NetworkPolicy cannot select public endpoints by DNS name, so the
  public rule is the narrow portable expression of package-index access.

The builder uses size-limited `emptyDir` volumes for source/workspace, BuildKit state, temporary
files, and Docker configuration. It has `backoffLimit: 0`, a one-hour
`activeDeadlineSeconds`, and `ttlSecondsAfterFinished: 3600`.

The operator enforces both concurrency caps before it creates a Job. Rows remain `Queued` with a
stable explanatory message while a cap is full. Cancellation deletes only a Job and token Secret
whose build-id label and Lake controller reference match; the row becomes `Cancelled` after those
objects are absent.

## Status, progress, and recovery

While the Job is pending or active, the row is `Queued` or `Running`. A succeeded Job is not marked
`Succeeded` until the operator parses a valid digest and size from the final non-empty stdout line.
Malformed or unavailable final output remains non-terminal and is retried while the retained Job
and pod exist. A failed Job records its Kubernetes condition or container termination reason. The
terminal row keeps the digest, size, and message after Job garbage collection; its event topic
`build-events.<build-id>` is the durable logs reference.

lake-builder posts phase, batched BuildKit log, digest, and error events to
`POST /v1/publish_build_event` with its event token. Event delivery is best-effort and never decides
build success. The operator additionally posts transitions for pod scheduling, OOM termination,
and Job deadline expiry. It records each distinct pod-level event on the Job before moving on, so a
steady reconciliation does not publish duplicates.

Status writes are comparison-gated. An unchanged row produces no Postgres update, and an unchanged
Lake produces no Kubernetes update. `NOTIFY lake_builds` wakes the controller promptly; the normal
poll remains the recovery path for lost notifications and restarts. A Lake schema without the
external `builds` table is logged and skipped so operator rollout can precede the Lake migration
that adds it.

## Verify

```sh
go test ./internal/controller -run 'LakeBuild|BuildStore|BuildListener'
```

The native envtest covers queued row to namespace, policies, Secret, and Job, then simulated Job
success to digest/size status. The fake-client cases cover validation, both concurrency caps,
cancellation, failure diagnosis, credential reuse, and no-write steady reconciliation. The pgx
case runs the list and update SQL against a scratch `builds` table when the local Postgres proof is
enabled.

The dev-cluster build-and-pull proof needs a published lake-builder image plus the matching Lake
registry and token endpoints. It is not replaced by a local container build.
