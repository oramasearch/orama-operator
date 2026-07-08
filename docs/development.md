# Development

## Toolchain

- **Go** (`/opt/homebrew/bin/go`), **kubebuilder**, **helm**, **helmfile**, **kubectl**.
  ```sh
  export PATH="/opt/homebrew/bin:$(go env GOPATH)/bin:$PATH"
  ```
- `controller-gen` is fetched on demand by the Makefile (`go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest`).

## Codegen (after editing `api/v1alpha1/*.go`)

```sh
make generate      # DeepCopy (zz_generated.deepcopy.go)
make manifests     # CRD + RBAC into config/crd/bases + config/rbac
cp config/crd/bases/*.yaml chart/crds/    # the install chart carries the CRD
```
The generated DeepCopy + CRDs are committed. Then:

```sh
go build ./... && go vet ./...
helm lint chart
```

## Run locally (dev loop)

Reconciles against your current kube-context using your kubeconfig creds:

```sh
make run           # = go run ./cmd/main.go
```

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
