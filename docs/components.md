# Component reconcilers

What the operator creates per component, and reconciler status. Update this as each
reconciler lands.

| Component | Reconciler | Creates | Status |
|---|---|---|---|
| **engine** | `internal/controller/engine.go` | writer `StatefulSet` (+GPU, PVC) on the primary; readers `Deployment` (+Envoy sidecar); Services; engine `ConfigMap` | ✅ **done** (validated on AKS) |
| **dashboard** | `internal/controller/app.go` | `Deployment` + Service + `Ingress` | ✅ **done** (validated on AKS) |
| **ingestion** | `internal/controller/ingestion.go` | index-builder `Deployment` + HARVEST master + Mongo `StatefulSet` + Selenium | ✅ **done** (validated on AKS) |
| **controlDB** | `internal/controller/controldb.go` | CloudNativePG `Cluster` (primary bootstrap; read-tier replica) | ✅ **done** (validated on AKS) |
| **analytics** | `internal/controller/datastore.go` | ClickHouse `StatefulSet` + Service (8123/9000) | ✅ **done** (validated on AKS) |
| **cache** | `internal/controller/datastore.go` | Redis `StatefulSet` + Service (6379) | ✅ **done** (validated on AKS) |
| **broker** | `internal/controller/datastore.go` | RabbitMQ `StatefulSet` + Service (5672/15672), central placement | ✅ **done** (validated on AKS) |
| **search** | `internal/controller/datastore.go` | Typesense `StatefulSet` + Service (8108) | ✅ **done** (validated on AKS) |
| **disco** | `internal/controller/disco.go` | `Deployment` (DISCO + embeddings sidecar) + Service | ✅ **done** (validated on AKS) |
| **workers** | `internal/controller/app.go` | one bundled-image `Deployment` + Service | ✅ **done** (validated on AKS) |
| **cloudflare** | `internal/controller/cloudflare.go` | CF DNS record via the CF API (safe no-op until configured) | 🟡 **implemented** (not live-tested; LB pools are a follow-up) |
| **idp** (keycloak) | `internal/controller/idp.go` | Keycloak `Deployment` + Service (when `idp.provider=keycloak`) | ✅ **done** (validated on AKS) |
| **demoSeed** | `internal/controller/idp.go` | one-shot seed `Job` (placeholder) | ✅ **done** (validated on AKS) |

## engine (`internal/controller/engine.go`)

Driven by `spec.region.role`:

- **Writer** (primary only) — a `StatefulSet` (1 replica) running `oramasearch/oramacore`,
  a `volumeClaimTemplate` for `/app/.data`, and (when `writer.gpu`) an
  `nvidia.com/gpu: 1` limit + the CR's nodeSelector/tolerations. Service on `:8080`.
- **Readers** (primary + read tiers) — a `Deployment` (`reader.replicas`) with the
  `oramacore` container **+ an Envoy sidecar** (`envoyproxy/envoy:v1.31.2`, `:80 →
  127.0.0.1:8080`, from the generated `envoy.yaml`) when `engine.envoy` is set.
  Service on `:80` (→ Envoy, or → `:8080` if Envoy is off).
- **Config** — a `ConfigMap` with `envoy.yaml` + a minimal `config.yaml` (`provider:
  cpu|local` from `writer.gpu`). All children set an owner reference to the CR, so
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

**Validated on AKS (2026-07-07):** applying a `primary` CR created the writer STS
(+ dynamically-provisioned PVC), the 2-replica reader Deployment, both Services, and
the ConfigMap; the CR reached `Phase=Provisioning`; deleting the CR GC'd everything.

> **Follow-ups:** the `config.yaml` is minimal — full OramaCore config (cluster
> registry, API keys via ExternalSecrets, reader→writer wiring) is next, as are HPA +
> the reader→primary-writer endpoint on read tiers.

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
> a clustered Typesense; wire credentials via ExternalSecrets (the Typesense API key
> is a dev placeholder today); add readiness probes per store.

## App compute — ingestion / workers / dashboard (`app.go`, `ingestion.go`)

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
> (control-plane DB URL, API keys) via ExternalSecrets.

## DISCO (`disco.go`)

A `Deployment` (in-memory → no PVC; restart cold-starts the cache) with two
containers: the Go **DISCO** service (`:8080`) + a co-located gRPC **embeddings**
sidecar (`:50051`), matching prod. Service on `:8080`. **Validated on AKS.**

## IdP + demoSeed (`idp.go`)

- **idp** — when `idp.provider=keycloak`, deploys **Keycloak** (`Deployment` + Service,
  `start-dev`). `none`/`firebase`/`clerk` are external (nothing deployed). Validated on
  AKS (Keycloak booted `1/1`). Admin creds are dev placeholders → ExternalSecrets in prod.
- **demoSeed** — when `demoSeed: true`, creates a one-shot **Job** (create-if-absent).
  Today it's a **placeholder** (echo); the real seed will create a demo
  project+index+docs via the API once the engine is Ready. Validated on AKS (Job ran
  to `Complete`).

## Cloudflare (`cloudflare.go`) — 🟡 implemented, not live-tested

Cloudflare stays the permanent edge/geo-LB; the operator reconciles CF **DNS** via the
CF API (raw HTTP): it upserts a proxied record pointing `dashboard.host` at this
cluster's ingress address. **Safe no-op** unless fully configured — `enabled` +
`apiTokenSecretRef` (key `token`) present + `zoneID` + `manageDNS` + a resolvable
ingress target. The no-op path is validated on AKS (loop stays green with `enabled`
but no token).

> **Follow-ups:** validate against a live Cloudflare account; add **Load Balancing**
> (geo-steered pools + health checks across the regional clusters — the actual
> geo-LB); deprovision records on teardown; surface the managed hostnames beyond just
> the dashboard.
