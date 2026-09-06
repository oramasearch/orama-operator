# The `Lake` CR

What the operator does for a `Lake`, how it is deployed, and how to run it. Field reference:
[`../crd-reference.md`](../crd-reference.md) (`Lake` section). lake's own docs (server, schema,
deployment state): https://github.com/oramasearch/lake/tree/main/docs.

## What a Lake reconciles into

In order, every reconcile (`internal/controller/lake_controller.go`):

1. **Namespace** `spec.namespace` — created with a blind `Create` (AlreadyExists ignored), never
   owned, never deleted: the operator holds `namespaces: get, create` only (operator#106: no
   Namespace informer).
2. **Admin token** — Secret `lake-<name>-admin` (key `token`, 64 hex), minted once, owned;
   injected as `LAKE_ADMIN_TOKEN`. lake's `/v1` capabilities and MCP tool calls require it as a
   bearer.
3. **Instance KEK** — Secret `lake-<name>-instance-kek` (key `key`, 64 hex), minted once and owned
   while Postgres is enabled; injected as `LAKE_INSTANCE_KEK`. It wraps lake's persisted identity
   key and the per-customer KEKs used by static secrets and encryption. It is distinct from the
   admin bearer and is never sent to clients.
4. **Cluster sealing key** — Secret `lake-<name>-sealing-key`, an X25519 keypair minted once and
   owned. The public key and `kid` are published in `status.sealing` and injected into lake as
   `LAKE_SEALING_PUBLIC_KEY` / `LAKE_SEALING_KID`; the private key remains in the operator-owned
   Secret. An explicit `spec.sealing.generation` increase rotates it while records still using old
   keys keep those private keys available. See [sealed-secrets.md](sealed-secrets.md).
5. **Store claim** — either `spec.store.existingClaim` or an owned PVC
   `lake-<name>-store`, created once with `spec.store.size` (default `100Gi`),
   `storageClassName`, and `accessModes` (default `ReadWriteMany`). The selected claim is mounted
   read-write at `/var/lib/lake/store` in every pod. The pod runs with `fsGroup: 10001` (lake's
   uid/gid in the image) so a freshly provisioned volume is writable by lake on first mount. Operator-created claims are never replaced,
   resized, or deleted because the store spec changes.
6. **Deployment** `lake-<name>` — `spec.image` run as built, `spec.args`, port `spec.port`
   (default 4900), readiness `GET /`, `spec.replicas`, `imagePullSecrets` from
   `spec.pullSecretName`; operator env including `LAKE_STORE_ROOT=/var/lib/lake/store` plus
   `spec.env` entries whose names do not conflict with operator-owned variables.
7. **Service** `lake-<name>` — ClusterIP 80 → `spec.port`.
8. **Control-plane Postgres** — CloudNativePG `Cluster` `lake-<name>-controldb`
   (`spec.postgres`: enabled by default, instances, storageSize 50Gi, storageClassName), the same
   unstructured path `OramaCloudV1` uses (`controldb.go`). Disabling tears it down **with its
   data**.
9. **Schema** — ConfigMap `lake-<name>-controldb-migrations` + `PostgresSchema`
   `lake-<name>-controldb-schema` applying `assets/lake-schema.sql` (vendored byte-identical from
   `oramasearch/lake` `db/schema.sql`), guard table = the table the newest migration creates
   (`agent_guard` for `0017_agent_sessions`), version = content
   hash. The file is idempotent, so a new migration = a new guard, and the next Job re-runs the
   file adding only what is missing; on an existing Lake, delete the `PostgresSchema` (owned, it
   is recreated) to trigger that Job. Proper incremental migrations are #126.
10. **Control-plane backup** (opt-in: `spec.postgres.backup.enabled`) — CNPG Barman object-store
   configuration on `lake-<name>-controldb` plus an owned `ScheduledBackup`
   `lake-<name>-controldb-backup`. The R2/S3 Secret lives in `spec.namespace`; CNPG owns the
   individual `Backup` resources. See [backups.md](backups.md) for R2 setup, verification,
   changing the schedule or retention, and recovery into a new Cluster.
11. **Ingress + DNS** (opt-in: `ingressEnabled` + `host`, `dnsSecretRef`) — nginx Ingress for the
   whole host and an owned `CloudflareDNSRecord`, the `OramaZettaCluster` contract; gating off
   tears both down.
12. **Data-service lifecycle evidence** — `status.dataServices` reports Postgres and Zetta backup,
    restore, upgrade, and failover evidence. A separate cluster-scoped `LakeRestoreRehearsal`
    controller runs Postgres procedures and updates the row; see
    [data services](data-services/README.md).

**Status** mirrors the Deployment (`readyReplicas`, `phase`), publishes the active sealing
recipient and old-key record counts (`sealing`), selected store claim
(`store.claimName`, `store.phase`, `store.capacity`, `StoreReady` condition), the CNPG cluster
(`postgresReady`, `postgresInstances`), the PostgresSchema (`schemaPhase`, `schemaVersion`), and
the backup (`backupReady`, `lastBackupTime`, `BackupReady` condition), and lifecycle evidence for
Postgres and the pending Zetta data plane (`dataServices`); written only on a real
transition. An unbound store keeps the Lake out of `Ready`. The Lake polls the claim and CNPG every
20 s until both storage and the DB are ready and the schema is Applied, and while enabled backups
observe new CNPG `Backup` resources. `kubectl get lake` columns: Namespace, Host, Ready, DB,
Schema, Store, Backup, Data, Phase. The Data column shows managed and unmanaged service counts;
see [data services](data-services/README.md) for the operation-level labels and customer boundary.

**Owned kinds** (`SetupWithManager`): PersistentVolumeClaim (only the operator-created store
claim), Deployment, Service, Ingress, CloudflareDNSRecord, ConfigMap, Secret, LakeSealedSecret,
PostgresSchema, CNPG
ScheduledBackup. Not owned: an existing store claim, the Namespace, the CNPG Cluster (polled), and
CNPG Backup resources (observed by cluster label). A `LakeRestoreRehearsal` temporarily owns a
scratch Namespace, CNPG Cluster, copied Secrets, and verification Job; terminal cleanup deletes
them explicitly.

**RBAC.** The chart ClusterRole is hand-maintained (`chart/templates/rbac.yaml`); a Lake needs
`lakes`, `lakes/status`, `lakes/finalizers`, `namespaces: create`, plus the shared grants for
deployments, persistentvolumeclaims, services, configmaps, ingresses, cloudflarednsrecords,
postgresschemas, lakerestorerehearsals, `lakesealedsecrets` (including status and finalizers),
namespace-wide Secrets and `postgresql.cnpg.io/clusters`, `clusters/status`, `scheduledbackups`, and
`backups`. Scratch Namespace ownership is checked through the direct API reader, so Namespace RBAC
remains `get`, `create`, and `delete` without a cluster-wide informer. Keep it in sync with
`config/rbac/role.yaml` or the manager
CrashLoops on cache sync.

## Deploying the operator with Lake support

The Lake CR lives on PR #118 (branch `feat/lake-crd`) until merged; production AKS runs that
branch's image. Build, push, refresh CRDs, upgrade:

```sh
docker buildx build --platform linux/amd64 -t oramasearch/orama-operator:lake-crd-<sha> --load .
docker push oramasearch/orama-operator:lake-crd-<sha>
kubectl apply -f config/crd/bases/orama.cloud_lakes.yaml        # helm never upgrades crds/
helm upgrade orama-cloud-v1-operator ./chart -n orama-system \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag=lake-crd-<sha> \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub'
```

Use immutable tags (the `v1-sandbox` + `IfNotPresent` stale-cache failure is documented in
[`../install.md`](../install.md)). The first upgrade after a `kubectl set image` digest pin needs
`--force-conflicts`: helm 4 applies server-side and the `image` field is owned by `kubectl-set`.

## Running a Lake

### Store volume

The default store creates `lake-<name>-store` at `100Gi` with `ReadWriteMany`, because every
Deployment replica mounts the same libliquid customer root. The storage class must actually
support RWX. `ReadWriteOnce` is valid only with `spec.replicas: 1`; otherwise pods scheduled on
different nodes cannot mount the shared claim. To use a pre-provisioned claim in the Lake
namespace, set `spec.store.existingClaim`; the operator mounts it without adopting, changing, or
deleting it.

Size, class, and access modes are creation-time settings for an operator-created claim. Later spec
changes do not mutate or replace the PVC, including when switching to or from `existingClaim`.
Deleting the Lake still garbage-collects the operator-owned claim, while a referenced existing
claim survives.

Lake opens one libliquid store per customer below
`/var/lib/lake/store/<customer-id>/store`. `LAKE_STORE_ROOT` is always set to the mounted root.

### Lake environment

`spec.env` accepts Kubernetes `EnvVar` entries, including `valueFrom` references. Entries follow
the operator's variables in the container spec. The operator ignores custom entries named
`LAKE_ADMIN_TOKEN`, `LAKE_INSTANCE_KEK`, `LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`,
`LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, or `LAKE_STORE_ROOT`, so the reconciled Secret and
volume contracts always win.

Lake reads these configurable variables:

| Variables | Purpose |
|---|---|
| `LAKE_LOG_MODE`, `LAKE_LOG_WITNESSES`, `LAKE_LOG_WITNESS_THRESHOLD`, `LAKE_LOG_AGGREGATOR` | Transparency log mode (`air-gapped`, `witnessed`, or `anchored`), JSON witness set, quorum, and anchored-mode endpoint. The runtime defaults to `witnessed`, which requires configured witnesses when a store root is present; use `air-gapped` until witnesses exist. |
| `LAKE_PUBLIC_HOST` | Public host used when Lake constructs externally visible URLs. |
| `LAKE_REPLICATION_TOKEN` | Bearer presented to peer Lake instances; use a Secret reference. |
| `LAKE_IDENTITY_SEED` | Optional base64 Ed25519 identity seed; use a Secret reference. |
| `LAKE_TSA_URL`, `LAKE_TSA_TRUST_CERT`, `LAKE_TSA_RENEWAL_SECONDS`, `LAKE_TSA_RENEW_BEFORE_SECONDS` | RFC 3161 timestamp authority, optional PEM trust roots, and renewal policy. |
| `LAKE_PRICE_CURRENCY`, `LAKE_STORAGE_QUOTA_BYTES`, `LAKE_PRICE_STORAGE_GIB_HOUR_MICROS`, `LAKE_PRICE_TRANSFER_IN_GIB_MICROS`, `LAKE_PRICE_TRANSFER_OUT_GIB_MICROS`, `LAKE_PRICE_COMPUTE_SECOND_MICROS` | Metering currency, storage quota, and unit prices. |

The operator additionally supplies `LAKE_ADMIN_TOKEN`, `LAKE_INSTANCE_KEK`,
`LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_SEALING_PUBLIC_KEY`,
`LAKE_SEALING_KID`, and `LAKE_STORE_ROOT` from managed resources.

```sh
kubectl create namespace lake
# copy the pull Secret and the Cloudflare zone token into it, then:
kubectl apply -f - <<'Y'
apiVersion: orama.cloud/v1alpha1
kind: Lake
metadata: { name: prod }
spec:
  namespace: lake
  image: oramasearch/lake:<tag>
  pullSecretName: oramasearch-dockerhub
  args: ["--listen", "0.0.0.0:4900", "--node", "prod"]
  replicas: 3
  store:
    size: 100Gi
    accessModes: [ReadWriteMany]
  env:
    - name: LAKE_LOG_MODE
      value: air-gapped
    - name: LAKE_PUBLIC_HOST
      value: lake.oramadev.net
  postgres: { instances: 1, storageSize: 50Gi }
  ingressEnabled: true
  host: lake.oramadev.net
  dnsSecretRef: { name: cloudflare-api-token }
Y
kubectl wait lake/prod --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl get lake prod
```

`kubectl delete lake <name>` removes everything owned, including the database.

## Current production instance

`Lake/prod`, namespace `lake`, 3 replicas, 50Gi Postgres, schema `0001_identity_and_customers`
applied, https://lake.oramadev.net; operator helm revision 8, `lake-crd-8058ab5` (2026-09-02).
Details and the not-yet list: https://github.com/oramasearch/lake/blob/main/docs/deployment/aks.md.

## Open

- #125 Zetta data plane (owned `OramaZettaCluster` per Lake).
- #126 incremental migrations in `PostgresSchema`.
- #122 hardening: resources, liveness, PDB, TLS.
- #119 land PR #118; #120 module/chart/release rename.
