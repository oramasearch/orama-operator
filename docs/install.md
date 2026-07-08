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
helmfile apply        # External Secrets, CloudNativePG, RabbitMQ operator
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

The full field reference is in [crd-reference.md](crd-reference.md); more samples in
[`../config/samples/`](../config/samples/).

## 4. Override the dummy secrets / images (for real use)

- **Secrets** — each component has its own `v1-<component>-secret` (e.g.
  `v1-engine-secret`, `v1-dashboard-secret`) seeded with dummy values. Edit them in
  place (the operator won't overwrite an existing secret), or point
  `spec.secrets.backend` at ExternalSecrets to source real values.
- **Images** — most app images are placeholders (`oramasearch/<name>:latest`).
  Override per component:
  ```yaml
  spec:
    images:
      overrides:
        dashboard: myregistry/dashboard:v1.2.3
        index-builder: myregistry/index-builder:v1.2.3
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
