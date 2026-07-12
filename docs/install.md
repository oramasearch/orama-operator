# Install

How to install the operator and stand up an Orama Cloud v1 stack in a cluster.

## Prerequisites

- A Kubernetes cluster (1.29+) and `kubectl` context pointing at it.
- **helm** 3.x (and **helmfile** if you want the one-shot platform install).
- A default **StorageClass** with `ReadWriteOnce` (for the StatefulSet PVCs).
- An **ingress controller** if you enable the dashboard (for its Ingress).
- *(Optional)* a **GPU node pool** (`nvidia.com/gpu`) for the OramaCore writer's
  built-in embeddings. **No GPU?** set `components.engine.writer.gpu: false` (CPU
  embeddings) or the writer pod stays `Pending`.

## 1. Platform operators

The operator reconciles some components into upstream operators' CRs, so install those
first. Easiest is the repo's **helmfile** (toggle off anything you already have):

```sh
helmfile apply        # CloudNativePG, RabbitMQ operator
```

Or install just what you need directly, e.g. CloudNativePG (required for `controlDB`):

```sh
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace
```

*(ClickHouse's Altinity operator isn't wired into the helmfile yet — `analytics` runs
as a single-node StatefulSet without it. See [components.md](components.md).)*

## 2. Install the operator

```sh
helm install orama-cloud-v1-operator ./chart -n orama-system --create-namespace
```

This installs the **CRD**, **RBAC**, and the **operator Deployment**. Confirm it's up:

```sh
kubectl -n orama-system get deploy orama-cloud-v1-operator
kubectl get crd oramacloudv1s.orama.cloud
```

> The chart uses the operator image `oramasearch/orama-cloud-v1-operator:latest`
> (override with `--set operator.image.tag=...`). Build/push it with
> `make docker-build docker-push IMG=...` (see [development.md](development.md)).

### Gotchas (dev sandbox)

- **Stale mutable-tag image.** A mutable tag (e.g. `:v1-sandbox`) + the default `imagePullPolicy:
  IfNotPresent` means a node that already cached the tag keeps running the **old** image after a
  rebuild+push. This silently ran a pre-`#92` operator (still rendering `--cluster-plaintext`) and a
  pre-`#745` Zetta binary (crashing on `--require-auth`). **Pin by digest** to force the exact build:
  `kubectl -n orama-system set image deploy/orama-cloud-v1-operator '*=<repo>@sha256:…'` and the OZC
  `spec.image: <repo>@sha256:…`; **verify the running pod's `imageID` after any rebuild.**
  `imagePullPolicy: Always` is the real fix.
- **CRDs already present → `helm install` conflict.** If the CRDs were already `kubectl`-applied, use
  `helm install --skip-crds` + `kubectl apply -f chart/crds/`. Helm never upgrades `crds/` on `helm
  upgrade`, so after adding/altering a CRD, **`kubectl apply -f chart/crds/`** to the cluster.
- **RBAC must match what the image watches.** The manager CrashLoops (*"failed to wait for `<kind>` caches
  to sync"*) if the image watches a CRD/resource the ClusterRole doesn't grant. The `ORAMA_OPERATOR_ID`
  read no longer needs a cluster-wide `namespaces` **watch** (operator#106): it is a single **uncached
  GET** of the `kube-system` Namespace, so the grant is `namespaces: [get]` only — no list/watch informer.
  The chart ClusterRole is hand-maintained; keep it in sync with the generated `config/rbac/role.yaml`.
- **Multi-cluster:** each k8s cluster runs its **own** in-cluster operator; they share one Zetta and
  coordinate through it. See [zetta-datastore.md](zetta-datastore.md) + [zetta-cluster-mtls.md](zetta-cluster-mtls.md).

## 3. Deploy a v1 stack (single-cluster quickstart)

Apply one `OramaCloudV1` — the operator stands up the whole stack, **self-bootstrapped
with per-component dummy secrets** (no manual secret creation):

```sh
kubectl apply -n orama-system -f - <<'EOF'
apiVersion: orama.cloud/v1alpha1
kind: OramaCloudV1
metadata:
  name: v1
spec:
  region: { name: local, role: primary }
  profile: light
  demoSeed: true
  components:
    engine: { enabled: true, writer: { gpu: false } }   # gpu:false on a non-GPU cluster
EOF
```

Watch it reconcile:

```sh
kubectl get ocv1 -A -w
kubectl -n orama-system get pods,statefulsets,deployments,svc
```

To deploy from the **private component images** (built by `build/images`), use
[`../config/samples/sandbox.yaml`](../config/samples/sandbox.yaml) — a light, CPU-only,
single-node CR **validated on AKS**: it sets `images.{registry,tag,pullSecretName}` and needs no
per-image overrides (`oramacore` resolves to its published `:latest`, `embeddings` to
`:cpu-latest`/`:gpu-latest` from `writer.gpu` — the global `tag` applies only to the operator's
own built images). First create the pull secret and build/push the images (§4).

The full field reference is in [crd-reference.md](crd-reference.md); more samples in
[`../config/samples/`](../config/samples/) (`sandbox.yaml` = single-node, `primary-us-east.yaml`
= prod-like, `read-eu.yaml` = read tier).

## 4. Override the dummy secrets / images (for real use)

- **Secrets** — each component has its own `v1-<component>-secret` (e.g.
  `v1-engine-secret`, `v1-dashboard-secret`) seeded with dummy values. Edit them in
  place (the operator won't overwrite an existing secret), the operator
  won't overwrite them once created.
- **Images** — `oramacore` reuses its published image; the components with **no** published
  image (`embeddings-server-py`, `dashboard`, `docs`, `orama-workers`, `harvest`, `disco`,
  `index-builder`) are built from source by the bridge's
  [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images)
  tooling and pushed to **private Docker Hub** (`oramasearch/<name>:<tag>`). Build + push them
  first — full procedure in the
  [RUNBOOK](https://github.com/oramasearch/orama-cloud-v1-bridge/blob/main/build/images/RUNBOOK.md):
  ```sh
  REGISTRY=oramasearch TAG=v1-sandbox build/images/build.sh --push
  ```
  The images match the `oramasearch/<name>` default, so point the CR at them by **tag**, and —
  because the repos are private — give it a pull secret:
  ```sh
  # create the pull secret once (read-only Docker Hub token)
  kubectl create secret docker-registry oramasearch-dockerhub -n orama-system \
    --docker-server=https://index.docker.io/v1/ \
    --docker-username=<user> --docker-password=<read-only-token>
  ```
  ```yaml
  spec:
    images:
      registry: oramasearch
      tag: v1-sandbox
      pullSecretName: oramasearch-dockerhub   # attached to every pod so it can pull the private images
  ```
  A per-component `overrides` entry still wins over `registry`/`tag`:
  ```yaml
  spec:
    images:
      overrides:
        dashboard: myregistry/dashboard:v1.2.3
  ```

## 5. Multi-region

Install the operator + apply one CR **per cluster**. Make one region `primary` and the
others `read`, and give the read tiers the primary's endpoints via `spec.peers`.
Cloudflare provides the geo-LB — set `spec.cloudflare.apiTokenSecretRef` to a Secret
holding a **real** CF token (key `token`); the operator upserts the DNS record. See
[`../config/samples/read-eu.yaml`](../config/samples/read-eu.yaml).

## Uninstall

```sh
kubectl delete ocv1 v1 -n orama-system        # cascade-deletes the stack (PVCs RETAINED)
kubectl -n orama-system delete pvc -l app.kubernetes.io/managed-by=orama-cloud-v1-operator  # optional: drop the data
helm uninstall orama-cloud-v1-operator -n orama-system
```

> **PVCs are retained on CR delete** (the data-safe default). Delete them explicitly
> to reclaim storage.
