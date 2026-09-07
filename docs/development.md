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

Without `KUBEBUILDER_ASSETS`, that case reports a skip while the handler, contract-digest, auth,
audit, secret-reference, REST, and MCP tests still run.

## Build targets

```sh
make manifests      # generate CRD + RBAC from the kubebuilder markers
make build          # generate + fmt + vet + compile the manager to bin/manager
make docker-build IMG=oramasearch/orama-operator:<tag>
make helm-package   # copy generated CRDs into the chart + package
```

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
