# orama-cloud-v1-operator — design

A Kubernetes **operator** (kubebuilder / Go) plus a thin **install Helm chart** that
stands up an **entire, self-contained Orama Cloud v1 stack in-cluster** — the whole
product surface, not just the engine — driven by a single `OramaCloudV1` custom
resource.

Prod v1 is documented in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1);
this repo is the **k8s-native re-packaging** of that system.

## Deliverable shape

- **Operator** (`orama-cloud-v1-operator`) — reconciles one umbrella CRD,
  `OramaCloudV1`, into every child workload (Deployments / StatefulSets / Knative
  Services / ExternalSecrets / subchart releases).
- **Install chart** (`chart/`) — self-contained; its *only* job is to install the
  **operator + CRDs + RBAC**. It does **not** deploy the app; the operator does.
- **`helmfile.yaml`** (repo root) — the **composition layer**: installs the upstream
  **platform operators** (Knative, External Secrets, CloudNativePG, RabbitMQ, …) +
  this operator chart, each toggleable (`installed:`) so a cluster that already has
  them skips them. This is the "reuse OSS charts / helmfile" decision.

```
helmfile apply  ─▶  platform operators (Knative, ESO, CNPG, …) + the v1 operator chart
                       │
helm install ./chart ─▶  operator + CRDs + RBAC   (if you skip helmfile)
                       │
kubectl apply OramaCloudV1 (one per cluster)  ─▶  operator reconciles the whole stack
```

## Decisions (from the 2026-07-07 design grill)

| # | Decision | Choice |
|---|---|---|
| 1 | **Scope** | The **entire** v1 surface, everything in-cluster (engine, dashboard, control-plane DB, analytics, cache, broker, ingestion, the cloud functions, workers). |
| 2 | **Deliverable** | A **kubebuilder operator** (Go) + a thin **install Helm chart** that launches it. |
| 3 | **CRD granularity** | **One umbrella CRD** `OramaCloudV1` (finer per-component CRDs may come later). |
| 4 | **Region model** | **Configurable** per cluster via `spec.region.role: primary \| read`. Multiple releases across `us-east / eu / ap` that talk to each other. |
| 5 | **Cloud functions** | **Knative Serving** — `index-builder` (was GCP Cloud Run) + the ported Cloudflare Workers run as Knative Services (scale-to-zero). |
| 6 | **Edge / geo-LB** | **Cloudflare** stays as the **geo load-balancer** (DNS + steering) in front of each cluster's ingress. Worker *logic* moves in-cluster. |
| 7 | **Managed SaaS** | Brought **in-cluster** via upstream operators/charts: Postgres (CloudNativePG), ClickHouse (Altinity operator), Redis, RabbitMQ (cluster-operator), Typesense, Mongo (HARVEST). Each is toggleable to `external` (point at real SaaS). |
| 8 | **Auth / IdP** | **Configurable**: `spec.idp: none \| keycloak \| firebase \| clerk`. |
| 9 | **Secrets** | **External Secrets Operator** (`spec.secrets.backend`). No plaintext in the CR. |
| 10 | **Images** | **Reuse** published `oramasearch/*` images (no build-from-source in this repo). |
| 11 | **Cross-cluster net** | **No service mesh** for now — read tiers reach the primary via configured endpoints (`spec.peers`) over public ingress + mTLS. |
| 12 | **"Working" bar** | Operator optionally **seeds a demo project+index** (`spec.demoSeed`) so a fresh install answers queries out of the box. |
| 13 | **GPU / LLM** | Writer requests `nvidia.com/gpu` (nodeSelector+toleration; CPU-embeddings toggle). LLM `spec.llm.provider: vllm \| groq \| openai \| anthropic` (in-cluster vLLM optional). |

## Region topology (mirror-prod default)

```
   Cloudflare geo-LB  (DNS steering to nearest cluster ingress)
        │                        │                         │
   ┌────▼─────────┐        ┌─────▼──────┐            ┌──────▼─────┐
   │ us-east      │        │ eu         │            │ ap         │
   │ role=primary │        │ role=read  │            │ role=read  │
   ├──────────────┤        ├────────────┤            ├────────────┤
   │ writer(GPU)  │        │ readers    │            │ readers    │
   │ pg PRIMARY   │──repl─▶│ pg replica │            │ pg replica │
   │ clickhouse   │        │ envoy      │            │ envoy      │
   │ rabbitmq     │        │ workers RO │            │ workers RO │
   │ index-builder│        └────────────┘            └────────────┘
   │ HARVEST      │           ▲                         ▲
   │ dashboard    │           └──── spec.peers ─────────┘
   │ workers RW   │        (read tiers point at primary writer/pg)
   └──────────────┘
```

`role=primary` runs the write path + stateful stores; `role=read` runs the
regional read path and points at the primary via `spec.peers`. A single-cluster dev
install uses `role=primary` with everything enabled.

## Component → k8s mapping

| v1 component (prod) | In-cluster form | Chart/operator |
|---|---|---|
| OramaCore writer (GPU L40S) | `StatefulSet` + `nvidia.com/gpu`, PVC | our operator |
| OramaCore readers ×N | `Deployment` (+HPA) behind Envoy | our operator |
| Envoy (per-reader :80→:8080) | sidecar or `DaemonSet`/sidecar | our operator |
| PlanetScale control plane | Postgres | **CloudNativePG** |
| ClickHouse Cloud | ClickHouse | **Altinity ClickHouse operator** |
| Upstash Redis + QStash | Redis | bitnami/redis (+ a queue shim) |
| RabbitMQ | RabbitMQ | **rabbitmq-cluster-operator** |
| Typesense hosted-search | Typesense | community chart |
| HARVEST (crawler + Mongo) | Deploy + Mongo + Selenium | our operator + bitnami/mongodb |
| index-builder (Cloud Run) | **Knative Service** | our operator + Knative |
| Cloudflare Workers | **Knative Services** (ported) | our operator + Knative |
| HPES embeddings | `Deployment` (GPU optional) | our operator |
| DISCO semantic cache | `StatefulSet` + gRPC embeddings | our operator |
| Dashboard (Vercel/Next.js) | `Deployment` + Ingress | our operator |
| Firebase auth | IdP shim (`none/keycloak/firebase/clerk`) | our operator (+ keycloak chart) |
| Grafana Cloud observ. | out of scope (point at existing) | — |
| Groq/OpenAI/Anthropic | remote, or in-cluster vLLM | our operator |

## Open questions (resolve as we build — do NOT block the scaffold)

1. **Index target — OramaCore vs Typesense** ([techops-docs#16](https://github.com/oramasearch/techops-docs/issues/16)):
   the `index-builder` write path speaks the Typesense admin API while the answer
   path reads OramaCore. The operator must model **both** runtimes until #16 settles
   which the boxes actually run.
2. **Embeddings placement** — writer built-in GPU embeddings vs a separate HPES
   Deployment vs remote. Default: built-in on the writer; HPES optional.
3. **Worker port strategy** — run the Cloudflare Workers on `workerd` containers
   (faithful, needs KV/R2/DO shims) vs port each to a plain Node Knative Service
   (portable). Default per-worker; DISCO/telemetry/segmenter port cleanly.
4. **KV / R2 / Queues / Durable-Objects shims** — back CF bindings with in-cluster
   equivalents (KV→Redis, R2→MinIO, Queues→RabbitMQ, DO→a stateful shim).
5. **Cross-cluster replication transport** — Postgres streaming + OramaCore's
   RabbitMQ-stream replication over public ingress + mTLS (no mesh); confirm ports.

## Status

🚧 **Scaffold** — API types + install chart + samples + this design. The controller
reconcile logic and CRD codegen (`make manifests`) are built next (needs Go +
kubebuilder + controller-gen on the build machine / CI).
