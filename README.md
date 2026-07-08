# orama-cloud-v1-operator

A **Kubernetes operator** (kubebuilder / Go) plus a thin **install Helm chart** that
deploys an **entire, self-contained Orama Cloud v1 stack in-cluster** — the whole
product surface (engine, dashboard, control-plane DB, analytics, cache, broker,
ingestion, the cloud functions, and the edge workers) — driven by a single
`OramaCloudV1` custom resource.

It is the **k8s-native re-packaging** of the production system documented in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1).
Read [`DESIGN.md`](DESIGN.md) for the architecture + the full decision log.

> **Status: 🟢 all component reconcilers implemented + validated on AKS.** Every
> component (engine, control-plane Postgres, the four data stores, ingestion, workers,
> dashboard, DISCO, IdP, demoSeed) reconciles into the right topology and owner-GCs on
> delete; the Cloudflare DNS reconciler is implemented but not yet live-tested.
> Remaining work is **hardening** (real images, full config/secrets, HPA, Cloudflare
> LB) — see [`docs/components.md`](docs/components.md).

## How it works

```
helmfile apply                                      # platform operators + the v1 operator
# ...or just the operator if the platform bits already exist:
helm install orama-cloud-v1-operator ./chart -n orama-system --create-namespace
kubectl apply -f config/samples/primary-us-east.yaml   # one OramaCloudV1 per cluster
kubectl get ocv1 -A -w                              # operator reconciles the whole stack
```

**Full install guide:** [`docs/install.md`](docs/install.md) (prereqs, platform
operators, the CR, multi-region, overriding dummy secrets/images, uninstall).

- **`helmfile.yaml`** composes the upstream **platform operators** the stack builds on
  (CloudNativePG, RabbitMQ operator, …) + this operator chart —
  each toggleable so you skip whatever your cluster already has.
- The **chart** installs only the **operator + CRDs + RBAC** (self-contained).
- The **operator** reconciles one `OramaCloudV1` CR into every child workload
  (Deployments / StatefulSets / Secrets / dependency CRs) **and** the
  Cloudflare DNS + Load Balancing (via the CF API).

## The `OramaCloudV1` CR (one per cluster)

Everything is configured on one resource. Highlights (full schema in
[`api/v1alpha1/oramacloudv1_types.go`](api/v1alpha1/oramacloudv1_types.go)):

| Field | Purpose |
|---|---|
| `spec.region.role` | `primary` (write path + stateful stores) or `read` (regional read tier) |
| `spec.profile` | `light \| medium \| heavy` — presets replication + sizing |
| `spec.idp.provider` | `none \| keycloak \| firebase \| clerk` |
| `spec.llm.providers[]` / `spec.llm.default` | **multiple** LLM providers (users pick per query); defaults to groq+openai+anthropic |
| `spec.cloudflare` | operator manages CF **DNS + Load Balancing** via the CF API (default on) |
| `spec.components.*` | per-component enable + `in-cluster \| external`, `placement: local \| central`, `replicate` |
| `spec.components.engine.writer.gpu` | request `nvidia.com/gpu` for the writer's built-in embeddings (prod: L40S) |
| `spec.components.workers` | all ported CF workers bundled into one image → one Deployment |
| `spec.peers` | endpoints of the other regions (read tiers → primary); no mesh |
| `spec.demoSeed` | seed a demo project+index so a fresh install answers queries |

Samples: [`config/samples/primary-us-east.yaml`](config/samples/primary-us-east.yaml)
(primary region) and [`config/samples/read-eu.yaml`](config/samples/read-eu.yaml)
(a read tier pointing at the primary).

## Multi-region

Install one release + one CR per cluster (`us-east` = `primary`, `eu`/`ap` = `read`).
**Cloudflare** provides the geo load-balancing (DNS steering) in front of each
cluster's ingress. Read tiers reach the primary's writer + Postgres via `spec.peers`
(public ingress + mTLS — **no service mesh** for now).

## Repo layout

```
api/v1alpha1/        # the OramaCloudV1 CRD Go types (the design artifact)
internal/controller/ # the reconcilers (engine in progress; others todo)
cmd/main.go          # the manager entrypoint
config/crd|samples/  # generated CRDs + example CRs
chart/               # the install Helm chart (operator + CRDs + RBAC)
helmfile.yaml        # composition: platform operators + this chart
docs/                # dev guide, CRD reference, component reconcilers
DESIGN.md            # architecture + decision log + open questions
CLAUDE.md            # working rules (incl. docs discipline)
Makefile             # standard kubebuilder targets (manifests/build/docker/helm)
```

Docs are kept in lockstep with the source — see [`CLAUDE.md`](CLAUDE.md) and
[`docs/`](docs/).

## Build (needs Go + kubebuilder toolchain)

```
make manifests   # generate CRD + RBAC from the kubebuilder markers
make build       # compile the manager
make docker-build IMG=oramasearch/orama-cloud-v1-operator:0.1.0
make helm-package # copy generated CRDs into the chart + package
```

## Conventions

No AI attribution in commits/PRs. Reuse published `oramasearch/*` images. Keep
[`DESIGN.md`](DESIGN.md) in sync with decisions; keep the ops truth in
[`techops-docs`](https://github.com/oramasearch/techops-docs).
