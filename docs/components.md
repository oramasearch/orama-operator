# Component reconcilers

What the operator creates per component, and reconciler status. Update this as each
reconciler lands.

| Component | Reconciler | Creates | Status |
|---|---|---|---|
| **engine** | `internal/controller/engine.go` | writer `StatefulSet` (PVC) on the primary; readers `Deployment` (+Envoy sidecar); **shared `embeddings` Deployment** (ai-server :50051, +GPU when `writer.gpu`); Services; engine `ConfigMap` | ✅ **done** (validated on AKS) |
| **dashboard** | `internal/controller/app.go` | `Deployment` (+ `db-migrate` init: Drizzle, [#34](https://github.com/oramasearch/orama-cloud-v1-operator/issues/34)) + Service + `Ingress` | ✅ **done** (validated on AKS) |
| **docs** | `internal/controller/app.go` | `Deployment` + Service + `Ingress` (docs.orama.com, Next.js + Fumadocs, `:3000`) | ✅ **done** |
| **ingestion** | `internal/controller/ingestion.go` | index-builder `Deployment` + HARVEST master + Mongo `StatefulSet` + Selenium | ✅ **done** (validated on AKS) |
| **controlDB** | `internal/controller/controldb.go` | CloudNativePG `Cluster` (primary bootstrap; read-tier replica) | ✅ **done** (validated on AKS) |
| **analytics** | `internal/controller/datastore.go` | ClickHouse `StatefulSet` + Service (8123/9000) | ✅ **done** (validated on AKS) |
| **cache** | `internal/controller/datastore.go` | Redis `StatefulSet` + Service (6379) | ✅ **done** (validated on AKS) |
| **broker** | `internal/controller/datastore.go` | RabbitMQ `StatefulSet` + Service (5672/15672), central placement | ✅ **done** (validated on AKS) |
| **search** | `internal/controller/datastore.go` | Typesense `StatefulSet` + Service (8108) | ✅ **done** (validated on AKS) |
| **disco** | `internal/controller/disco.go` | `Deployment` (DISCO + embeddings sidecar) + Service | ✅ **done** (validated on AKS) |
| **workers** | `internal/controller/app.go` | one bundled-image `Deployment` + Service | ✅ **done** (validated on AKS) |
| **analytics-processor** | `internal/controller/gaps.go` | `Deployment` + Service — pulls `/v1/analytics` → ClickHouse | 🟡 **modeled** ([#5](https://github.com/oramasearch/orama-cloud-v1-operator/issues/5); image is a follow-up) |
| **oramasearch-app** | `internal/controller/gaps.go` | `Deployment` + Service + `Ingress` — api.askorama.ai (legacy) | 🟡 **modeled** ([#7](https://github.com/oramasearch/orama-cloud-v1-operator/issues/7); image is a follow-up) |
| **search-workers** | `internal/controller/gaps.go` | 4 CF-worker `Deployment`s (index-shared / large-index / openai-decrypter / telemetry) | 🟡 **modeled** ([#6](https://github.com/oramasearch/orama-cloud-v1-operator/issues/6); images + CF-binding wiring are a follow-up) |
| **cloudflare** | `internal/controller/cloudflare.go` | CF DNS record via the CF API (safe no-op until configured) | 🟡 **implemented** (not live-tested) |
| **cloudflare-lb** | `internal/controller/cloudflareloadbalancer_controller.go` | global **Load Balancer** + **Origin Pool(s)** + **Health Monitor** via the CF Load Balancing API, from an owned `CloudflareLoadBalancer` CRD (finalizer teardown) | 🟡 **implemented** ([#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73); not live-tested — needs LB enabled on the account; live-node origins are [#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74)) |
| **idp** (keycloak) | `internal/controller/idp.go` | Keycloak `Deployment` + Service (when `idp.provider=keycloak`) | ✅ **done** (validated on AKS) |
| **demoSeed** | `internal/controller/idp.go` | one-shot seed `Job` (placeholder) | ✅ **done** (validated on AKS) |
| **zetta-capacity** | `internal/controller/zetta_capacity.go` | Karpenter `NodePool` (unstructured, cluster-scoped) + a `PodDisruptionBudget`; pins the Zetta pods onto the pool (taint/toleration + nodeSelector + `do-not-disrupt`) | 🟡 **implemented** (opt-in, [#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69) first slice; not yet live-tested — needs an EKS/AKS-NAP cluster) |
| **zetta-capacity-install** | `internal/controller/zetta_capacity_install.go` | Operator-owned **Karpenter install** (cloud-gated): server-side-applies the pinned upstream controller (Deployment + ServiceAccount + RBAC) + Karpenter CRDs, as `unstructured` | 🟡 **implemented** (opt-in `install: true`, [#75](https://github.com/oramasearch/orama-cloud-v1-operator/issues/75); EKS self-install, no-op on AKS-NAP/bare-metal; not yet live-tested) |

## Bootstrap — config + per-component secrets (`internal/controller/config.go`)

Runs **first**. Makes the install self-bootstrapping (no manual secret pre-creation):

- **`<cr>-config` ConfigMap** (shared, non-sensitive) — service DNS for
  Postgres/ClickHouse/Redis/RabbitMQ/Typesense/DISCO/OramaCore, ports, Typesense +
  Keycloak non-secret config. Reconciled every pass.
- **Per-component secrets** `<cr>-<component>-secret` — each holds **only the dummy
  credentials that component needs** (`componentSecretData`). e.g. `search-secret` =
  `TYPESENSE_API_KEY`; `cache-secret` = empty (Redis is passwordless, #32); `engine-secret`
  = master key + JWT keys + LLM keys; `dashboard-secret` = ClickHouse/JWT/Stripe (its **Postgres
  password comes from the CNPG `<cr>-controldb-app` secret**, not here — #32). **No shared blob** —
  a pod never sees a credential it doesn't use. **Create-if-absent** — the operator
  never overwrites an existing secret, so real / rotated values are
  preserved. Components with no secret needs (selenium, mongo) get none.

Each component consumes `envFrom: [<cr>-config, <cr>-<component>-secret]` (its own
secret only). Everything is owner-referenced → GC'd with the CR.

**Disable ≠ leave running ([#23](https://github.com/oramasearch/orama-cloud-v1-operator/issues/23)).**
Owner-GC only fires on CR *deletion*; so when a `components.<x>.enabled` toggle flips off, the
reconciler actively **prunes** that component's owned resources by label
(`internal/controller/teardown.go` — `teardownByComponent`), covering Deployments/StatefulSets/
Services/ConfigMaps/Secrets/PDBs/Ingresses/Jobs (and the CNPG `Cluster` for `controlDB`). StatefulSet
PVCs are left behind (data preserved). Idempotent — `enable → disable → enable` round-trips cleanly
(validated on AKS: disabling `docs` pruned it, re-enabling restored it, `Ready 22/22`).

- **JWT keypair (real, generated)** — the operator generates a **real RSA-2048
  keypair** once (create-if-absent) into `<cr>-jwt` (private + public, for the
  **dashboard** to sign the OramaCore writer JWT) and `<cr>-jwt-pub` (public **only**,
  for the **engine** to verify). Least-privilege: the engine never receives the
  private key. `JWT_ISSUER` is non-secret (in the ConfigMap).

**Validated on AKS (2026-07-08):** per-component secrets are generated (the
`search-secret` holds only `TYPESENSE_API_KEY`, **not** the Cloudflare token); the
cache pod's `envFrom` is `[config, cache-secret]` only; Typesense + Keycloak boot
`1/1` off their scoped secrets.

> **Follow-ups:** edit the remaining dummy secret values in place for production
> (they're plain in-cluster K8s Secrets; the operator won't overwrite them). (The JWT
> keypair is already real + generated.)

## engine (`internal/controller/engine.go`)

Driven by `spec.region.role`:

- **Writer** (primary only) — a `StatefulSet` (1 replica) running `oramasearch/oramacore`,
  a `volumeClaimTemplate` for `/app/.data`, and the CR's nodeSelector/tolerations. Service on
  `:8080`. OramaCore itself is CPU; when `writer.gpu` is set the `nvidia.com/gpu: 1` limit goes
  on the **shared embeddings Deployment** (the model inference is the GPU consumer), not here.
- **Readers** (primary + read tiers) — a `Deployment` (`reader.replicas`) with the
  `oramacore` container **+ an Envoy sidecar** (`envoyproxy/envoy:v1.31.2`, `:80 →
  127.0.0.1:8080`, from the generated `envoy.yaml`) when `engine.envoy` is set.
  Service on `:80` (→ Envoy, or → `:8080` if Envoy is off).
- **Embeddings ai-server** — ONE **shared `<cr>-embeddings` Deployment** (gRPC on `:50051`) +
  Service that the writer and all readers reach via `ai_server.host: <cr>-embeddings` (#14) —
  **not** a per-pod sidecar, so the model loads into RAM once instead of once per engine pod. This
  matches OramaCore's reference architecture (`config-docker.yaml` points `ai_server.host` at a
  standalone `python-ai-server`). `embeddingsImage()` resolves the **published**
  `oramasearch/oramacore-ai-server:latest` (or `-cuda` for `writer.gpu`, which also puts the
  `nvidia.com/gpu: 1` limit + the writer's nodeSelector/tolerations on **this** Deployment — the
  embedding model, not OramaCore, is the GPU consumer). Auto-falls back to CPUExecutionProvider on
  a CPU node. **Not** `embeddings-server-py` (the separate HPES / index-manager path). Validated on
  AKS: shared `<cr>-embeddings` pod serving gRPC, writer **1/1** + readers **2/2** (sidecar gone).

  > **Deeper redundancy ([#39](https://github.com/oramasearch/orama-cloud-v1-operator/issues/39)):**
  > the `oramacore` image *also* embeds a Python interpreter and loads the embedding models
  > **in-process** per pod ("Building Python service" → "src.embeddings.models: Models loaded"), so
  > each writer/reader still carries a model copy independent of this shared ai-server. Whether to
  > offload those to the shared ai-server (or drop the redundant one) is tracked in #39.
- **Config** — a `ConfigMap` with `envoy.yaml` + a **full, valid** `config.yaml`
  (`http` / `writer_side` / `reader_side` / `ai_server` / `log`, matching
  `oramacore/config-docker.yaml`; `execution_providers: CUDA|CPU` from `writer.gpu`).
  The `config.yaml` is a **template** — `master_api_key` is a `__MASTER_API_KEY__`
  placeholder, so **no credential lives in the ConfigMap**. A `render-config` **init
  container** substitutes the real value from `<cr>-engine-secret` (via `envFrom`) into
  a shared `emptyDir`, and OramaCore reads it via `CONFIG_PATH=/etc/oramacore/config.yaml`
  (env override can't reach the nested `writer_side.master_api_key` — `config`'s
  `ORAMACORE_` provider splits on `_`). All children set an owner reference to the CR, so
  deleting the CR cascade-deletes them.
- **Graceful shutdown** — OramaCore handles SIGTERM (drains HTTP, then force-commits
  BOTH sides to disk — `oramacore/src/lib.rs`, `web_server/mod.rs`; `CMD ["./oramacore"]`
  = PID 1). Because the writer's commit flushes indexes, the writer pod gets
  `terminationGracePeriodSeconds: 300` (readers: 60) so k8s doesn't SIGKILL the flush
  at the default 30s.
- **Health probes** — startup / readiness / liveness all hit `GET /health` on `:8080`
  (`oramacore/src/web_server/api/mod.rs`; also the prod ALB path). The **startup probe**
  (`failureThreshold: 30 × 10s` ≈ 5 min) gates the others so a slow index-load boot
  isn't killed; readiness gates Service routing; liveness restarts a hung pod.
- **Disruption budgets** — the **writer** gets a PDB with `maxUnavailable: 0` (it's a
  singleton — voluntary disruptions like node drains/autoscaler are blocked so it
  isn't casually evicted; **node maintenance must consciously handle it**). **Readers**
  get `maxUnavailable: 1` (roll one at a time, keep the rest serving).

**Validated on AKS (2026-07-08):** applying a `primary` CR created the writer STS
(+ dynamically-provisioned PVC), the reader Deployment, both Services, and the
ConfigMap; the CR reaches **`Phase=Ready`** once every owned workload is ready — a status
roll-up over the live Deployment/StatefulSet + CNPG readiness (`internal/controller/status.go`,
[#37](https://github.com/oramasearch/orama-cloud-v1-operator/issues/37)). Phase is
`Progressing | Ready | Degraded` (Degraded = a component regressed after being Ready); it also
exposes `status.componentsReady` (`22/22`, a print column), a **per-component list**
(`status.components[]` — one entry per deployable component, e.g. `writer 1/1`, `broker 1/1` — not
per-pod), and a standard `Ready` condition. The status subresource is **written only on a real
transition** (deep-compared first), so a steady stack generates no control-plane churn. Deleting
the CR GC'd everything. The
`render-config` init container renders the config, and **OramaCore 1.2.28 parses it and
boots** (`Configuration loaded successfully` → `Starting web server on
http://0.0.0.0:8080`); the ConfigMap shows only `master_api_key: "__MASTER_API_KEY__"`
while the rendered file in the pod holds the real value.

> **Follow-ups:** the `embeddings` ai-server image is built by the bridge's
> [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images)
> tooling (`oramasearch/embeddings-server-py:{cpu,gpu}-latest`, private Docker Hub — the tag
> is auto-picked from `writer.gpu`; just set `spec.images.pullSecretName`); RabbitMQ
> writer→reader replication (currently in-memory transport, single-node); HPA + the
> reader→primary-writer endpoint on read tiers.

## controlDB (`internal/controller/controldb.go`)

The control-plane Postgres (PlanetScale in prod) as a **CloudNativePG `Cluster`**,
provisioned as an **unstructured** object so the operator stays decoupled from CNPG's
Go types/version. Requires the CNPG operator (installed via the helmfile).

- **Primary** — a fresh cluster, `spec.instances` from the profile (light 1 / medium
  2 / heavy 3), `10Gi` storage, `bootstrap.initdb` creating the `oramacore` database.
- **Read tier** (`role: read`, `replicate: true`) — a **replica cluster** streaming
  from the primary region's `postgresEndpoint` (`spec.replica` + `externalClusters`).
- `mode: external` skips creation (the app uses the external endpoint). Owner ref on
  the `Cluster` → deleting the CR cascade-deletes it (CNPG then removes the pods/PVCs).

**Validated on AKS (2026-07-07):** with the CNPG operator installed, a `medium`
primary CR produced a `Cluster` (instances=2, initdb=`oramacore`, owned by the CR);
CNPG bootstrapped Postgres; deleting the CR GC'd the cluster + its pods.

> **Follow-ups:** the replica cluster's `streaming_replica` credentials/secret wiring;
> exposing storage size + the DB name as CR fields; a Service alias the app can use.

## Data stores — cache / broker / analytics / search (`internal/controller/datastore.go`)

A single generic `reconcileDataStore` provisions each as a **single-node
`StatefulSet` + PVC + Service**, parameterized per store (image, ports, data path,
env). Honors the `DataStoreSpec`: `enabled`, `mode: external` (skip — the app uses
the endpoint), and `placement: central` (only the primary runs it; read tiers skip
and reference the primary's endpoint). Images are overridable via
`spec.images.overrides.<component>`.

| Store | Component | Image | Ports |
|---|---|---|---|
| Redis | `cache` | `redis:7-alpine` | 6379 |
| RabbitMQ | `broker` | `rabbitmq:3.13-management` | 5672, 15672 |
| ClickHouse | `analytics` | `clickhouse/clickhouse-server:24.8` | 8123, 9000 |
| Typesense | `search` | `typesense/typesense:0.25.2` | 8108 |

**Validated on AKS (2026-07-07):** a CR with all four enabled produced four
StatefulSets + Services + PVCs; **all four pods booted `1/1`**; deleting the CR
removed the workloads.

> **PVC retention:** StatefulSet PVCs are **retained** on delete (the k8s default —
> data is left in place, not auto-deleted). Orphaned PVCs are cleaned up manually.
> (A configurable `delete`-on-teardown option can be added later.)

> **Follow-ups:** these are single-node for light/medium. **Heavy/production** should
> swap in the dedicated operators (Altinity ClickHouse, RabbitMQ cluster-operator) and
> a clustered Typesense; wire credentials via the component secret (the Typesense API key
> is a dev placeholder today); add readiness probes per store.

## App compute — ingestion / workers / dashboard / docs (`app.go`, `ingestion.go`)

A generic `reconcileApp` provisions a stateless **Deployment + Service (+ Ingress if a
host is set)**.

- **ingestion** — `index-builder` Deployment (was Cloud Run); when `harvest` is on,
  also the HARVEST **master** Deployment, a **Mongo** StatefulSet (record store), and a
  **Selenium** grid Deployment.
- **workers** — the ported CF workers as **one bundled image** → a single Deployment
  (`workers.replicas`, image via `workers.image` or `spec.images.overrides.workers`).
- **dashboard** — a Deployment + Service + an **Ingress** on `dashboard.host`.

**Validated on AKS (2026-07-08):** a CR with these enabled produced the 5 Deployments,
the Mongo StatefulSet, the dashboard Ingress, and 6 Services; CR-delete GC'd them.

> **Follow-ups:** images for `index-builder` / `harvest` / `dashboard` / the `workers`
> bundle are **placeholders** (`oramasearch/<name>:latest`) pending published/built
> images — override via `spec.images.overrides`. HARVEST's Podman-per-job worker model
> needs porting to k8s Jobs (the master + Mongo + Selenium are here; the job spawner
> is a follow-up). Dashboard/index-builder need their env + secret wiring
> (control-plane DB URL, API keys) via the component secret.

## DISCO (`disco.go`)

A `Deployment` (in-memory → no PVC; restart cold-starts the cache) with two
containers: the Go **DISCO** service (`:8080`) + a co-located gRPC **embeddings**
sidecar (`:50051`), matching prod. Service on `:8080`. **Validated on AKS.**

## IdP + demoSeed (`idp.go`)

- **idp** — when `idp.provider=keycloak`, deploys **Keycloak** (`Deployment` + Service,
  `start-dev`). `none`/`firebase`/`clerk` are external (nothing deployed). Validated on
  AKS (Keycloak booted `1/1`). Admin creds are dev placeholders → edit the keycloak secret in prod.
- **demoSeed** — when `demoSeed: true`, creates a one-shot **Job** (create-if-absent).
  Today it's a **placeholder** (echo); the real seed will create a demo
  project+index+docs via the API once the engine is Ready. Validated on AKS (Job ran
  to `Complete`).

## Cloudflare (`cloudflare.go`) — 🟡 implemented, not live-tested

Cloudflare stays the permanent edge/geo-LB; the operator reconciles CF **DNS** via the
CF API (raw HTTP): it upserts a proxied record pointing `dashboard.host` at this
cluster's ingress address. **Safe no-op** unless fully configured — `enabled` +
`apiTokenSecretRef` → a Secret with keys `token` + `zoneID` + `accountID` + `manageDNS` + a resolvable
ingress target. The no-op path is validated on AKS (loop stays green with `enabled`
but no token).

> **Follow-ups:** validate against a live Cloudflare account; add **Load Balancing**
> (geo-steered pools + health checks across the regional clusters — the actual
> geo-LB); deprovision records on teardown; surface the managed hostnames beyond just
> the dashboard.

## Zetta elastic capacity (`zetta_capacity.go`) — 🟡 implemented, opt-in, not live-tested

The **first slice** of Zetta-driven elastic capacity ([operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69)):
`Zetta → operator → Karpenter → nodes → more Zetta nodes`. Opt-in via
`OramaZettaCluster.spec.capacity.karpenter.enabled` (default off). Reconciled from the
`OramaZettaCluster` controller (step 0, before the StatefulSet).

- **Owned `NodePool`** — the operator reconciles a Karpenter `NodePool` (`karpenter.sh/v1`)
  via **`unstructured`** (same decoupling as the CNPG `Cluster` — no dependency on Karpenter's
  Go types/version). It carries the cloud-neutral policy: a dedicated **taint**
  (`orama.cloud/zetta:NoSchedule`), the **pool label** (`orama.cloud/zetta-pool`) on the node
  template, `requirements` (os/arch/capacity-type + optional instance-type pin), the **cost cap**
  (`spec.limits` cpu/memory/nodes), and **stateful-safe disruption**
  (`consolidationPolicy: WhenEmpty`, a `nodes: "1"` budget, `expireAfter: Never`,
  `terminationGracePeriod: 1h`). CreateOrUpdate ⇒ the write is a no-op when nothing changed
  (anti-flap, principle #7).
- **Provider `NodeClass` is referenced, not owned** — `spec.capacity.karpenter.nodeClassRef`
  (`{group, kind, name}`) points at the platform's `EC2NodeClass` (EKS) / `AKSNodeClass`
  (AKS NAP). The NodeClass carries cluster-specific cloud details (AMI/subnet/IAM on AWS) the
  operator can't invent, so the platform owns it. `name` is required when enabled; missing ⇒
  `CapacityReady=False, Reason=Blocked`.
- **Scale trigger (this slice)** — pure **scheduling**: the Zetta pods get the matching
  toleration + `nodeSelector: {orama.cloud/zetta-pool: <name>}` (in `mutateZettaStatefulSet`),
  so raising `spec.replicas` past node capacity leaves pods **Pending → Karpenter provisions →**
  the pods schedule and join. The operator changes *desired replicas + NodePool limits*;
  Karpenter does the launch. A **Zetta-signal-driven** trigger (an `orama_control.scale_requests`
  row the operator polls) is the **next** slice — noted, not built.
- **Quorum-safety** — the Zetta pods carry `karpenter.sh/do-not-disrupt` (Karpenter never
  voluntarily evicts a live CRDT peer) + a `maxUnavailable: 1` **PDB**. Drain-safe,
  operator-driven **scale-down** (lower replicas, let Karpenter reclaim the emptied node) is a
  later slice.
- **Karpenter-absent = graceful no-op** — existence is checked via the **RESTMapper** (no
  informer started for a possibly-missing CRD). No `karpenter.sh/v1 NodePool` mapping ⇒
  `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. Covers bare-metal / k0s
  clusters with no node autoscaler.
- **Cluster-scoped cleanup** — a `NodePool` is **cluster-scoped**, so a namespaced CR can't own
  it via `ownerReferences` (k8s GC would treat the owner as absent and delete it). Instead the
  pool is label-identified and cleaned up by a **finalizer** (`orama.cloud/karpenter-capacity`),
  added while capacity is on and released on disable or CR-delete. The namespaced PDB is
  owner-GC'd normally.
- **Status** — `status.provisionedNodes` (count of nodes labeled
  `karpenter.sh/nodepool=<pool>`, read via the **uncached APIReader** so the operator holds no
  cluster-wide Node informer) + the `CapacityReady` condition, both written only on change
  (anti-flap).

> **RBAC:** `karpenter.sh/nodepools` (`*`), `karpenter.sh/nodeclaims` +
> `karpenter.k8s.aws/ec2nodeclasses` + `karpenter.azure.com/aksnodeclasses` +
> core `nodes` (`get;list;watch`), and `policy/poddisruptionbudgets` — mirrored into
> `chart/templates/rbac.yaml`. A grant for a non-existent apiGroup is harmless.

## Operator-owned Karpenter install (`zetta_capacity_install.go`) — 🟡 implemented, opt-in, not live-tested

The **second slice** of [operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69)
([operator#75](https://github.com/oramasearch/orama-cloud-v1-operator/issues/75)), extending the NodePool
work above: the NodePool reconcile *assumes* Karpenter is installed; this makes the operator **own the
install** too. Opt-in via `OramaZettaCluster.spec.capacity.karpenter.install` (default off). Reconciled as
**step 0** of the `OramaZettaCluster` controller, before the NodePool, so the CRDs exist first.

- **Vendored upstream manifests, server-side-applied** — the operator embeds (`go:embed`) the pinned
  **Karpenter v1.13.0** manifests under `internal/controller/assets/karpenter/v1.13.0/` — the CRD bundle
  (`NodePool`/`NodeClaim`/`EC2NodeClass`/`NodeOverlay`) and the controller bundle (ServiceAccount + RBAC +
  Deployment + Service + PDB) — and applies each document as an `unstructured` object via **server-side
  apply** (`client.Apply` + force-ownership). **No Karpenter Go types are vendored** (same decoupling as
  the NodePool reconcile). Re-vendor with [`hack/fetch-karpenter-manifests.sh`](../hack/fetch-karpenter-manifests.sh)
  (a reproducible `helm template` of the upstream OCI charts); two placeholders
  (`__CLUSTER_NAME__` / `__CONTROLLER_ROLE_ARN__`) are substituted per-CR at apply time.
- **Cloud-gated** — the cloud is detected from a node's `providerID`:
  - **bare-metal / k0s** (no cloud providerID) ⇒ no-op, `KarpenterInstalled=False, Reason=BareMetalNoCloud`.
  - **AKS** ⇒ no-op, `Reason=ManagedByAKSNAP` — Node Auto Provisioning already deploys and manages
    Karpenter; use `enabled` (not `install`) there.
  - **EKS** ⇒ the self-install path proceeds.
- **Cloud IAM prerequisite, surfaced not crashed** — Karpenter's controller needs a cloud identity to
  create VMs, which the in-cluster operator **cannot mint** (a cloud-account prereq, like the image pull
  secret). On EKS the reconciler checks `controllerRoleARN` (the IRSA role) + `installClusterName`
  **before** deploying the controller; if the identity is missing it reports
  `KarpenterInstalled=False, Reason=CloudIdentityMissing` and does **not** deploy (no `CrashLoopBackOff`).
  Full detail: [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md).
- **Cluster-singleton, ref-counted cleanup** — the controller install is shared by every CR that opts in.
  A namespaced CR can't own the cluster-scoped CRDs/ClusterRoles, so cleanup is a **finalizer**
  (`orama.cloud/karpenter-install`) that removes the operator-managed controller resources only when **no
  other** `OramaZettaCluster` still requests the install. **The CRDs are applied but never deleted** on
  teardown — deleting a Karpenter CRD cascades to every `NodePool`/`NodeClaim` in the cluster.
- **Status** — a `KarpenterInstalled` condition (`BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud`
  / `CloudIdentityMissing` / `InstallClusterNameRequired` → `InstallProgressing` → `Installed`), written
  only on change (anti-flap).

> **RBAC (install):** `apiextensions.k8s.io/customresourcedefinitions`
> (`get;list;watch;create;update;patch` — **no `delete`**, CRDs aren't GC'd) and
> `rbac.authorization.k8s.io` `clusterroles`/`clusterrolebindings`/`roles`/`rolebindings`
> (`…;delete;bind;escalate` — creating RBAC that grants Karpenter's broad permissions needs `bind` +
> `escalate`, since the operator doesn't itself carry every node/pod grant). ServiceAccounts, Deployments,
> Services and PDBs the install creates are already granted. Mirrored into `chart/templates/rbac.yaml`.

> **Follow-ups (later slices of #69):** the Zetta→operator signal channel
> (`orama_control.scale_requests`); drain-safe, quorum-aware scale-down; multi-cluster
> placement policy (per-operator identity already namespaces the state); consent-trail
> integration (`add_nodes` / `approve_consent`); live validation on an EKS + AKS-NAP cluster.
> **Follow-ups:** validate against a live Cloudflare account; deprovision records on
> teardown; surface the managed hostnames beyond just the dashboard.

## Cloudflare global Load Balancer (`cloudflareloadbalancer_controller.go`) — 🟡 implemented, not live-tested

The `CloudflareLoadBalancer` CRD ([#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73))
turns a single hostname into a **global load balancer** — origin pools of wan-accessible
nodes, health checks, geo/proximity steering, and failover — the actual geo-LB. It is the
same "external resource as an owned CRD" shape as `CloudflareDNSRecord`: the child
controller makes the CF API calls and holds a finalizer, so the main loop never mutates
Cloudflare directly. From **one** CR it reconciles three Cloudflare Load Balancing objects:

- a **Health Monitor** (account-scoped) — `type` http/https/tcp, `path` (default
  `/v1/server_info`), `interval`/`timeout`/`retries`, `expectedCodes`; drops unhealthy
  origins from rotation.
- one or more **Origin Pools** (account-scoped) — each a named set of origins
  (`{name, address, enabled, weight}`), `minimumOrigins`, the monitor attached. Pools that
  leave the spec are pruned after the LB stops referencing them.
- the **Load Balancer** (zone-scoped) — the `hostname`, `default_pools` (spec order),
  `fallbackPool`, `steeringPolicy` (`off｜geo｜dynamic｜proximity｜random`; `dynamic` maps to the
  API's `dynamic_latency`), optional `regionPools` (region code → pool names) for geo
  steering, and `proxied`.

**Idempotent + merge-not-clobber:** create-or-adopt (list by our `orama-operator:<ns>/<name>`
description tag / by pool name / by hostname), then **PATCH only the fields we manage** so
server-managed fields are left intact. **Anti-flap (principle #7):** the status subresource
(LB id, monitor id, per-pool ids + health, a `Ready` condition) is written **only on a real
transition** (deep-compare); a steady LB produces zero control-plane writes, refreshed on a
5-minute resync. **Graceful when Load Balancing is off:** a 403/entitlement error →
`Ready=False (LoadBalancingNotEnabled)` + a gentle requeue, never a crash-loop. **Safe no-op**
until the Secret (token+zoneID+accountID) exists → `Ready=False (WaitingForCredentials)`.

> **Follow-ups:** validate against a live Cloudflare account with Load Balancing enabled;
> auto-populate pool origins from the live wan-accessible node set
> ([#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74) /
> [zetta#739](https://github.com/oramasearch/zetta/issues/739)) so the main loop creates/updates
> the `CloudflareLoadBalancer` as nodes come and go.
**Zetta cluster reachability + the CF LB origin pool.** The `OramaZettaCluster` reconciler
(`internal/controller/oramazettacluster_controller.go`) classifies each slice by its **node-reachability
type** (`spec.reachability`, [operator#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74)):
a **`wan-accessible`** slice is a rendezvous whose routable address the operator advertises as
`.status.wanAddress` (from `spec.wanAddress` / `spec.hostNetwork` / `spec.exposePeerLoadBalancer`), while a
**`dial-out-only`** slice (the default, and any `spec.ephemeral` slice) dials OUT only and **never gets an
inbound peer LoadBalancer** — the operator ignores `spec.exposePeerLoadBalancer` on it (warns + tears down
any existing LB), fixing the earlier anti-pattern of fronting a private node with an inbound LB
([zetta#739](https://github.com/oramasearch/zetta/issues/739)). Two consumers read the wan-accessible set:
siblings copy `.status.wanAddress` into their `spec.externalPeers`, and the **Cloudflare LB origin pool**
([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73) / the
`CloudflareLoadBalancer` CRD, [operator#76](https://github.com/oramasearch/orama-cloud-v1-operator/pull/76))
is populated from those addresses (HTTP origin at the same host `:4800`) — the documented wiring hook, not
yet auto-populated here.
