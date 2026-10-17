# Development

## Toolchain

- **Go** (`/opt/homebrew/bin/go`), **kubebuilder**, **helm**, **helmfile**, **kubectl**.
  ```sh
  export PATH="/opt/homebrew/bin:$(go env GOPATH)/bin:$PATH"
  ```
- `controller-gen` is fetched on demand by the Makefile at the pinned `v0.21.0` version.

## Repository layout

```
api/v1alpha1/        # the CRD Go types (OramaCloudV1, OramaZettaCluster, Lake, …): the design artifact
internal/controller/ # the reconcilers, one file per component or CRD (status: docs/components.md)
cmd/main.go          # the manager entrypoint
config/crd|samples/  # generated CRDs + example CRs (generated and committed)
chart/               # the install Helm chart (operator + CRDs + RBAC; the ClusterRole is hand-maintained)
helmfile.yaml        # composition: platform operators + this chart
docs/                # this guide, the CRD reference, the component reconcilers, the Lake CR
DESIGN.md            # architecture + decision log + open questions
CLAUDE.md            # working rules (incl. docs discipline)
Makefile             # standard kubebuilder targets (manifests/build/docker/helm)
```

## Codegen (after editing `api/v1alpha1/*.go`)

```sh
make generate      # DeepCopy (zz_generated.deepcopy.go)
make manifests     # CRD + RBAC into config/crd/bases + config/rbac
cp config/crd/bases/*.yaml chart/crds/    # the install chart carries the CRD
```
The generated DeepCopy + CRDs are committed. Then:

```sh
go build ./... && go vet ./...
go test ./...
helm lint chart
```

The deployment control-plane suite includes an envtest case for create, list, rollback, and status.
Install the matching Kubernetes binaries and export their directory before `go test ./...`:

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path 1.30.x)"
go test ./...
```

The same assets run the workspace envtests. `TestWorkspaceDatastorePoliciesAgainstAPIServer`
(operator#268) checks that every workspace NetworkPolicy passes API validation, that the
Kubernetes API egress rule matches the test API server's own `default/kubernetes` Service and
Endpoints, and that a steady second pass rewrites no policy.

The same assets run `TestDashboardCronsAdmission` (operator#17): the generated `OramaCloudV1` CRD
refuses `dashboard.crons` without the dashboard, an `e2eCanary` without the crons, and a canary image
not pinned to a digest. The scripts the dashboard crons and the e2e canary render are run against a
local HTTP server by `TestDashboardCronScriptAgainstAServer` and `TestE2ECanaryScript`, under `sh` and
`dash` when present; they need `curl` (and `jq` for the canary) and skip without them.

The same assets run `TestKarpenterInstallEnvtest` (operator#79), with RBAC authorization on: the
`OramaZettaCluster` CRD schema refuses an injected, overlong or malformed `installClusterName` /
`controllerRoleARN`; the chart's own ServiceAccount and ClusterRole install and remove the Karpenter
bundle; and the same identity is forbidden to create or bind any role, binding or CRD outside it.

The same assets run the Zetta environment passthrough case. It verifies custom env reaches the
StatefulSet, reserved-name collisions appear in status, and an env change advances the StatefulSet
generation so Kubernetes rolls the pods.

The Lake volume snapshot envtests (`TestVolumeSnapshots*`, operator#260) also load the upstream
`snapshot.storage.k8s.io` CRDs from `internal/controller/testdata/snapshot-crds/` (external-snapshotter
v8.2.0, the version on the dev cluster; refresh from that project's `client/config/crd/` when the
cluster's version changes). The volume snapshot and workspace pgx stores run their SQL against
`testdata/lake-workspaces.sql` inside a rolled-back transaction when
`LAKE_WORKSPACES_TEST_POSTGRES_URI` is set.

The Lake schema proof also needs local Postgres and `psql`, `createdb`, and `dropdb` on `PATH`.
Set `LAKE_SCHEMA_TEST_POSTGRES=1` and `PGUSER` to your local role; the suite creates fresh databases
on `127.0.0.1:5432` and drops only those it created. With this opt-in, missing envtest assets are
a failure. See [the schema proof and vendoring commands](lake/schema.md#verification).

The Lake managed-services store runs its pgx SQL against the vendored schema with
`LAKE_SERVICES_TEST_POSTGRES=1` (connection `LAKE_SERVICES_TEST_POSTGRES_URI`, default
`postgres://127.0.0.1:5432/postgres`). It works in a scratch schema inside one transaction that is
rolled back, so it creates and drops nothing: `go test ./internal/controller/ -run PgxStore`. See
[services](lake/services.md#how-to-verify).

The apps sync uses fake Lake Postgres rows with native envtest (`TestLakeAppsEnvtestLifecycle`).
Its optional SQL adapter proof is `TestLakeAppsPgxStoreAgainstPostgres`, using
`LAKE_APPS_TEST_POSTGRES_URI` against a fresh local database. It creates only a temporary contract
table in a rolled-back transaction while lake#293 implements the upstream migration. Run native
tests with `GOMAXPROCS=6 GOFLAGS=-p=6`; no image build or live cluster is needed. See
[the apps contract and proof](lake/apps.md#verification).

The workspace sync's optional SQL proof is `TestLakeWorkspacesPgxStoreAgainstPostgres`, using
`LAKE_WORKSPACES_TEST_POSTGRES_URI` against a fresh local database. It applies only
`internal/controller/testdata/lake-workspaces.sql` inside a rolled-back transaction because
lake#265/lake#311 own the production migration. The deterministic namespace, policy, lifecycle and
legacy-cleanup cases run in the ordinary Go suite. A live CI or dev cluster runs the tokenless
network Job with `make verify-workspace-isolation`; its required variables and assertions are in
[`lake/workspaces.md`](lake/workspaces.md#verification-job).

lake-sqlite, lake-duckdb and lake-polars, the web services behind the `sqlite`, `duckdb` and
`polars` service kinds, are each their own private repository
([oramasearch/lake-sqlite](https://github.com/oramasearch/lake-sqlite),
[oramasearch/lake-duckdb](https://github.com/oramasearch/lake-duckdb),
[oramasearch/lake-polars](https://github.com/oramasearch/lake-polars)); this repository only runs
their images ([`lake/lake-sqlite.md`](lake/lake-sqlite.md), [`lake/lake-duckdb.md`](lake/lake-duckdb.md),
[`lake/lake-polars.md`](lake/lake-polars.md)).

The sealed-delivery admission suite also uses these assets and requires `helm` to render the
operator's actual RBAC. It installs both pinned k0s policies and the tenant grant, then proves namespace
selection, delivery, rotation/removal and authorization without using a live cluster. Run it with:

```sh
go test -count=1 -v ./internal/controller -run '^TestLakeEnvironmentSealedDeliveryAdmission$'
```

Fixture provenance and refresh instructions are in
[`lake/admission-fixtures.md`](lake/admission-fixtures.md).

Without `KUBEBUILDER_ASSETS`, the API-server cases report a skip while the handler, contract-digest, auth,
audit, secret-reference, REST, and MCP tests still run. The Lake schema proof fails rather than skips when
`LAKE_SCHEMA_TEST_POSTGRES=1` is set without the assets.

## Build targets

```sh
make manifests      # generate CRD + RBAC from the kubebuilder markers
make build          # generate + fmt + vet + compile the manager to bin/manager
make helm-package   # copy generated CRDs into the chart + package
```

### Obtain or publish the operator image

Installations need a **published immutable operator tag** compatible with this checkout's CRDs
and RBAC, plus permission to pull its private repository. Obtain both from the Orama release
maintainer before following [install.md](install.md). The chart's `v1-sandbox` default is mutable;
it is not a release recommendation. This guide does not assert that any new tag is published.

The development machine does not run Docker (bridge decision 33). Native build and verification:

```sh
go build ./...
go vet ./...
go test ./...
```

To publish an operator image, an authorized maintainer uses **cluster BuildKit through
`buildctl`**. Obtain the existing remote builder's endpoint, its TLS client configuration, and
a registry configuration with push permission from the build infrastructure owner. Set
`BUILDKIT_HOST`, `BUILDKIT_TLS_DIR` (containing `ca.pem`, `cert.pem`, and `key.pem`), and
`DOCKER_CONFIG` (the protected directory containing the registry `config.json`) to those supplied
locations. The configuration file format does not require a local Docker daemon. Keep credentials
outside the repository; the read-only installation token is not a publishing credential.

From this repository root, set `OPERATOR_REPOSITORY` to the authorized destination and
`OPERATOR_TAG` to a new immutable release tag approved by the maintainer. For Linux amd64 nodes:

```sh
: "${BUILDKIT_HOST:?Set the supplied remote BuildKit endpoint}"
: "${BUILDKIT_TLS_DIR:?Set the supplied BuildKit TLS directory}"
: "${DOCKER_CONFIG:?Set the protected registry configuration directory}"
: "${OPERATOR_REPOSITORY:?Set the authorized image repository}"
: "${OPERATOR_TAG:?Set a new immutable release tag}"
export DOCKER_CONFIG
buildctl --addr "$BUILDKIT_HOST" --tlsdir "$BUILDKIT_TLS_DIR" build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --opt platform=linux/amd64 \
  --opt build-arg:TARGETOS=linux \
  --opt build-arg:TARGETARCH=amd64 \
  --output "type=image,name=${OPERATOR_REPOSITORY}:${OPERATOR_TAG},push=true"
```

For arm64 nodes, use `linux/arm64` and `TARGETARCH=arm64` with a builder that supports that
platform. BuildKit uses this repo's `Dockerfile` on the remote builder and pushes directly to the
registry. Record the successful push's digest and confirm the installation identity can pull it
before distributing the tag. If builder or publishing access is unavailable, obtain the published
image from the release maintainer instead; a native `go build` alone does not publish an image.
The image carries two binaries: `/manager`, and `/mesh-projector` (`cmd/mesh-projector`), the
node-local half of zetta-tunnel's mesh that the mesh DaemonSet runs beside the agent
([`infrastructure/mesh.md`](infrastructure/mesh.md)).
Application component images are supplied by their owning projects, not built in this repository.

The manager and application pull-secret settings are described separately in
[install/credentials.md](install/credentials.md). No live build or cluster access is needed for
the local chart and Go proof.

## Run locally (dev loop)

The standard loop: run the manager **out-of-cluster** against your current kube-context,
using your kubeconfig creds (no in-cluster deploy needed to iterate):

```sh
make run           # = go run ./cmd/main.go
```

**Run only ONE manager at a time.** The metrics server binds `:8080` and the deployment control plane
binds `:8090`; a second manager (or a stale/wedged one left running) fails with
`bind: address already in use`, and two
managers reconciling the same CR will fight. If startup errors on `:8080`, an old manager
is still alive — find and stop it, then re-run:

```sh
lsof -nP -iTCP:8080 -sTCP:LISTEN     # the holder is the manager (COMMAND ocv1-mgr / main)
kill <pid>                            # killing the manager does NOT delete deployed pods
```

A wedged manager shows up as the CR's `.status.observedGeneration` lagging
`.metadata.generation` (it stopped reconciling) — restart it to catch up. To run a second
manager deliberately (e.g. debugging), override the ports:
`go run ./cmd/main.go --metrics-bind-address=:8180 --health-probe-bind-address=:8181 --control-plane-bind-address=:8190`.

## AKS smoke test

Context `orama-production-cluster-01` is a fresh, empty, single-node **sandbox**
(`Standard_D8as_v7`, **no GPU**). Steps that were verified:

```sh
kubectl create namespace orama-system
kubectl apply -f chart/crds/orama.cloud_oramacloudv1s.yaml     # install CRD (cluster-scoped)
kubectl apply -f config/samples/primary-us-east.yaml -n orama-system   # schema-validated server-side
kubectl get ocv1 -A                                            # REGION/ROLE/PROFILE printcolumns
make run                                                       # manager starts + reconciles the CR
```

Result: the CRD installs, the sample CR is accepted, and the manager logs
`reconciling OramaCloudV1 … region=us-east role=primary`.

> **No GPU node yet.** When testing the engine, set `components.engine.writer.gpu: false`
> or the writer pod stays `Pending` (no `nvidia.com/gpu` capacity). Add a GPU node pool
> to test GPU embeddings for real.

## Ship

Commit + push this repo → bump the pointer in `orama-cloud-v1-bridge` → bump the
pointer in the umbrella `orama-bridge` (fast-forward to `main`). No AI attribution.
