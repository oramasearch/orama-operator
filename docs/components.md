# Component reconcilers

What the operator creates per component, and reconciler status. Update this as each
reconciler lands.

| Component | Reconciler | Creates | Status |
|---|---|---|---|
| **engine** | `internal/controller/engine.go` | writer `StatefulSet` (+GPU, PVC) on the primary; readers `Deployment` (+Envoy sidecar); Services; engine `ConfigMap` | ✅ **done** (validated on AKS) |
| dashboard | — | `Deployment` + `Ingress` | ⬜ todo |
| ingestion | — | index-builder `Deployment` + HARVEST (+ Mongo, Selenium) | ⬜ todo |
| **controlDB** | `internal/controller/controldb.go` | CloudNativePG `Cluster` (primary bootstrap; read-tier replica) | ✅ **done** (validated on AKS) |
| analytics | — | ClickHouse installation | ⬜ todo |
| cache | — | Redis | ⬜ todo |
| broker | — | RabbitMQ (central placement) | ⬜ todo |
| search | — | Typesense cluster | ⬜ todo |
| disco | — | `StatefulSet` + embeddings sidecar | ⬜ todo |
| workers | — | one bundled-image `Deployment` | ⬜ todo |
| cloudflare | — | CF DNS + Load Balancing via the CF API (external) | ⬜ todo |

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
