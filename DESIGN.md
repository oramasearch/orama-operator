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
  Services / Secrets / subchart releases).
- **Install chart** (`chart/`) — self-contained; its *only* job is to install the
  **operator + CRDs + RBAC**. It does **not** deploy the app; the operator does.
- **`helmfile.yaml`** (repo root) — the **composition layer**: installs the upstream
  **platform operators** (CloudNativePG, RabbitMQ operator, …) +
  this operator chart, each toggleable (`installed:`) so a cluster that already has
  them skips them. This is the "reuse OSS charts / helmfile" decision.

```
helmfile apply  ─▶  platform operators (CNPG, RabbitMQ, …) + the v1 operator chart
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
| 5 | **Cloud functions** | **Plain Deployments** (no Knative for now). The ported Cloudflare Workers are **bundled into ONE image** run as a single Deployment; `index-builder` (was GCP Cloud Run) is its own Deployment. |
| 6 | **Edge / geo-LB** | **Cloudflare is permanent** — **DNS + Load Balancing (geo-steering)** always live there, and the **operator reconciles them via the Cloudflare API** (`spec.cloudflare`, default on). Each CF resource is an **owned child CRD** (child controller = the CF API call + a finalizer; the main loop never mutates CF directly): `CloudflareDNSRecord`/`CloudflareKVNamespace`/`CloudflareR2Bucket`, and — for the geo-LB itself — **`CloudflareLoadBalancer`** ([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73)): one CR → a health **Monitor** + origin **Pool(s)** + the zone **Load Balancer**, with geo/proximity steering + failover. Auto-populating pool origins from the live wan-accessible node set is [operator#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74). Worker *logic* runs in-cluster. |
| 7 | **Managed SaaS** | Brought **in-cluster** via upstream operators/charts: Postgres (CloudNativePG), ClickHouse (Altinity operator), Redis, RabbitMQ (cluster-operator), Typesense, Mongo (HARVEST). Each is toggleable to `external` (point at real SaaS). |
| 8 | **Auth / IdP** | **Configurable**: `spec.idp: none \| keycloak \| firebase \| clerk`. |
| 9 | **Secrets** | **In-cluster Kubernetes Secrets** — the operator generates per-component secrets (dummy values, create-if-absent); edit them for real values. **No External Secrets Operator.** No plaintext in the CR. A config **file** that must embed a secret (OramaCore's `master_api_key`) keeps a `__PLACEHOLDER__` in the ConfigMap and is rendered by a **`render-config` init container** from the component Secret into an `emptyDir` at pod start — so the credential lives only in the Secret, never the ConfigMap. |
| 10 | **Images** | **Reuse** published `oramasearch/*` images where they exist (`oramacore`). Components with **no** published image (`embeddings-server-py`, `dashboard`, `docs`, `orama-workers`, `harvest`, `disco`, `index-builder`) are built by the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images) tooling — local `docker build` from submodule source (**can't trigger prod CI**), pushed to **private** Docker Hub repos under `oramasearch` (the embeddings image as `cpu-latest`/`gpu-latest`). The operator pulls them via `spec.images.tag` + `spec.images.pullSecretName`. Still no build-from-source *in the operator repo*. |
| 11 | **Cross-cluster net** | **No service mesh** for now — read tiers reach the primary via configured endpoints (`spec.peers`) over public ingress + mTLS. |
| 12 | **"Working" bar** | Operator optionally **seeds a demo project+index** (`spec.demoSeed`) so a fresh install answers queries out of the box. |
| 13 | **GPU / LLM** | The **writer** requests `nvidia.com/gpu` only for its **built-in embeddings** (ONNX+CUDA; set `writer.gpu: false` for CPU on a non-GPU node). The *other* GPU consumer is an optional in-cluster **vLLM** (`spec.llm.gpu`). LLM is a **list** — `spec.llm.providers[]` (multiple; users pick per query) + `spec.llm.default`; `vllm` runs in-cluster. |
| 14 | **Profiles / replication** | `spec.profile: light \| medium \| heavy` presets replication + sizing (light=dev/no-repl, medium=single-tenant prod, heavy=multi-tenant). Per-store override: `placement: local \| central` (e.g. RabbitMQ **central** = one cluster hosts it) + `replicate` (e.g. Postgres read-replica on read tiers). |
| 15 | **e2e suite** | The `OramaCloudV1E2ETest` runs a **comprehensive functional suite** (`internal/controller/assets/e2e-functional.sh`) embedded via `go:embed` and shipped in a ConfigMap; the Job's runner (default `alpine:3.20`, `spec.image`) installs curl+jq and runs it. Chosen over the prior single-purpose `oramacore-test-e2e` image so coverage lives **in-repo**, is editable without an image rebuild, and can assert **every** critical function (~70 assertions in variations) against the live engine. The suite self-bootstraps a clean-slate collection each run for determinism; commit-dependent or unwired functions (batch-get, analytics) SKIP-and-track rather than fail. Coverage backlog: [operator#44](https://github.com/oramasearch/orama-cloud-v1-operator/issues/44). |
| 16 | **Zetta CRD** | `OramaZettaCluster` (separate top-level CRD) deploys a **headless Orama Zetta cluster** — a StatefulSet of equal-peer CRDT nodes + a headless Service (peer DNS 7777) + a client Service (HTTP API 4800), each node a per-node PVC at `--data-dir`. **Step 1 of One Orama** ([orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)): the operator only *runs* Zetta headless — no operator↔Zetta symbiosis, no Zetta MCP changes. `node-0` bootstraps (mints its own mTLS token); peers join via a pod-ordinal wrapper. The image is built from the Zetta source in `zetta-bridge/build/images/zetta-headless` (C++26, clang-19, cmake ≥3.30 for `cxx_std_26`). |
| 17 | **Zetta e2e CRD** | `ZettaClusterE2ETest` (separate top-level CRD) runs the **live-cluster zetta e2e suite** as a Job against a deployed `OramaZettaCluster` and reflects pass/fail in status — mirroring `OramaCloudV1E2ETest`, but with the **opposite script-location choice**. The v1 suite is **embedded in the operator** (`go:embed` of `assets/e2e-functional.sh`) because it targets OramaCore, a stable dependency the operator already models. The Zetta suite instead **lives in the zetta repo** (`tests/e2e/zetta-e2e.sh`) and is **baked into the `oramasearch/zetta-e2e` image** (its `ENTRYPOINT` runs it) — the CRD only *triggers* it and passes the cluster addressing as `ZETTA_E2E_*` env (base URL pinned to node-0, peer URLs for ordinals 1..N-1, levels, optional MCP token). **Why:** the Zetta engine is under rapid, coupled development (SQL/MCP/serving/CRDT surfaces all moving) — the test must **evolve with the engine repo** and its L2 `zetta::testkit` catalog, so co-locating it with the engine (script → zetta-bridge image → operator CRD) keeps coverage honest without an operator recompile for every engine change. Runs L2 (in-app testkit) + L3 (black-box integration); the forcing-function rule holds (never soften an assertion). |
| 18 | **Elastic capacity (Zetta pool)** | **First slice of [operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69).** Opt-in `spec.capacity.karpenter` on `OramaZettaCluster`. The operator **owns a Karpenter `NodePool`** (modelled as **`unstructured`** — no dependency on Karpenter's Go types, matching the CNPG `Cluster` pattern) and **references** a platform-owned provider `NodeClass` (`EC2NodeClass`/`AKSNodeClass` via `nodeClassRef` — the NodeClass carries cloud specifics the operator can't invent). Scale is **scheduling-driven**: the Zetta pods are pinned to the pool (dedicated taint/toleration + pool nodeSelector), so raising `spec.replicas` past node capacity ⇒ Pending pods ⇒ Karpenter provisions ⇒ pods join. The NodePool is **cluster-scoped** so it can't be owner-GC'd by a namespaced CR — cleaned up via a **finalizer** + label identity instead. Stateful-safe disruption (`WhenEmpty`, `nodes:"1"` budget, `expireAfter: Never`, pod `do-not-disrupt` + a PDB). **Karpenter-absent (bare metal/k0s) = graceful no-op** (RESTMapper check ⇒ `CapacityReady=KarpenterNotInstalled`). The **Zetta-signal-driven** trigger (`orama_control.scale_requests`) + drain-safe scale-down are **later** slices. |
| 19 | **Operator owns the Karpenter install (cloud-gated)** | **Second slice of [operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69) ([operator#75](https://github.com/oramasearch/orama-cloud-v1-operator/issues/75)), extending #18.** #18 reconciles a `NodePool` *assuming Karpenter is installed*; opt-in `spec.capacity.karpenter.install` makes the operator **own the install** too — it **server-side-applies the pinned upstream Karpenter manifests** (controller Deployment + ServiceAccount + RBAC + the CRDs) as **`unstructured`** objects (vendored via `go:embed` under `assets/karpenter/<version>/`, regenerated by `hack/fetch-karpenter-manifests.sh`; **still no Karpenter Go types**). **Cloud-gated** via the node `providerID`: no-op on **bare-metal/k0s** (`BareMetalNoCloud`) and on **AKS** (`ManagedByAKSNAP` — Node Auto Provisioning already manages Karpenter); the self-install path targets **EKS**. **Boundary (documented, not owned):** Karpenter's **cloud IAM identity** (the controller's IRSA role / the nodes' instance profile / an Azure role assignment) is a **cloud-account prerequisite the in-cluster operator cannot mint** — same class as the image pull secret. The reconciler **surfaces `KarpenterInstalled=CloudIdentityMissing` and refuses to deploy the controller (no crash-loop)** rather than fail. Cluster-singleton, ref-counted across CRs via a finalizer; **CRDs are applied but never deleted** (deleting one cascades to every NodePool/NodeClaim cluster-wide). Not live-tested — needs an EKS/AKS-NAP cluster. Full detail: [`docs/karpenter-cloud-iam-prerequisite.md`](docs/karpenter-cloud-iam-prerequisite.md). |

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
| index-builder (Cloud Run) | **Deployment** | our operator |
| Cloudflare Workers | **one bundled image** → single `Deployment` | our operator |
| Cloudflare DNS + Load Balancing | reconciled via the **CF API** (not in-cluster) | our operator |
| HPES embeddings | `Deployment` (GPU optional) | our operator |
| DISCO semantic cache | `StatefulSet` + gRPC embeddings | our operator |
| Dashboard (Vercel/Next.js) | `Deployment` + Ingress | our operator |
| docs.orama.com (Vercel/Next.js+Fumadocs) | `Deployment` + Ingress | our operator |
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
3. **Worker bundle internals** — the decision is **one Node image, one Deployment**
   for all ported workers. Open: how the bundle multiplexes the worker routes
   (a single Node/Hono server with per-worker handlers) and which workers are
   trivial ports vs need real logic.
4. **KV / R2 / Queues / Durable-Objects shims** — back the CF bindings the ported
   workers used with in-cluster equivalents (KV→Redis, R2→MinIO/S3, Queues→RabbitMQ,
   Durable Objects→a stateful shim).
5. **Cross-cluster replication transport** — Postgres streaming + OramaCore's
   RabbitMQ-stream replication over public ingress + mTLS (no mesh); confirm ports.

## Status

🟢 **All component reconcilers implemented + validated on AKS.** API types, generated
CRD + DeepCopy, manager, install chart, helmfile, samples in place (`go build` /
`go vet` green). Every component reconciles into the right topology, sets `Phase`, and
owner-GC cleans up on CR delete — verified on the live cluster:

✅ engine (writer/readers/Envoy) · ✅ controlDB (CloudNativePG) · ✅ cache/broker/
analytics/search (Redis/RabbitMQ/ClickHouse/Typesense) · ✅ ingestion (index-builder +
HARVEST + Mongo + Selenium) · ✅ workers · ✅ dashboard (+ Ingress) · ✅ disco ·
✅ idp (Keycloak) · ✅ demoSeed · 🟡 cloudflare (DNS via the CF API — implemented, not
yet live-tested; LB pools a follow-up).

Per-component detail + follow-ups: [`docs/components.md`](docs/components.md).
**Remaining work is hardening, not new components:** real app images (most are
placeholders), full config + real in-cluster secret values per component, readiness/HPA
across the board, the Cloudflare live validation + Load Balancing, and the HARVEST
job-spawner + replica-DB credentials.
