# The `Lake` CR

What the operator does for a `Lake`, how it is deployed, and how to run it. Field reference:
[`../crd-reference.md`](../crd-reference.md) (`Lake` section). lake's own docs (server, schema,
deployment state): https://github.com/oramasearch/lake/tree/main/docs.

## At a glance

The cluster-scoped `Lake` CR deploys the lake service with its control-plane Postgres, an owned
Zetta data plane, and a shared persistent libliquid store. The operator creates a `100Gi` RWX store claim by default or
mounts `spec.store.existingClaim`, sets `LAKE_STORE_ROOT=/var/lib/lake/store`, and accepts
additional lake runtime configuration through `spec.env`. The workload has resource bounds,
Lake-native health probes, non-root execution, RWO-safe replacement, and replicated-pod
disruption and placement controls. Each Lake also owns an X25519 sealing
key and publishes its recipient in status; namespaced `LakeSealedSecret` resources turn
cluster-bound ciphertext into owned, labelled Kubernetes Secrets without putting plaintext in the
authored resource ([sealed-secrets.md](sealed-secrets.md)). `kubectl get lake` reports managed and
unmanaged data-service counts, and operation-level status never implies untested protection;
`LakeRestoreRehearsal` automates Postgres restore, scratch upgrade and replicated failover proof
([data-services/README.md](data-services/README.md),
[data-services/postgres.md](data-services/postgres.md)).

Desired deployment events in the operator's Zetta control plane become Lake-owned
`LakeEnvironment` CRs: one restricted, quota-bound `untrusted` runtime namespace per named or
per-change environment, with anti-flap status returned to Zetta, wildcard DNS for previews, and
delivered `LakeSealedSecret` values copied into the isolated workload namespace before startup
([environments.md](environments.md)). Liquid reaches that intake through the operator's
authenticated REST `/v1` and MCP deployment surface; the contract, credentials, rollback, audit
and connection details are in the [deployment control-plane guide](../control-plane/README.md).

Namespaced `LakeApp` resources run customer apps from a pinned Lake repository revision or a
digest-pinned image. Each app has a restricted namespace, private-by-default ingress, scoped data
bindings, and live rollout events. The CR contract and resource lifecycle are in
[`apps.md`](apps.md). With `spec.apps.enabled`, a dedicated controller follows lake's `apps`
table using the Services DSN, `lake_apps` notifications and a 60-second fallback list. It registers
generated token hashes, returns current rollout status, handles stopped/deleted rows and orphans,
and owns one proxied apps wildcard DNS record. Builds remain separate in #195.

With `spec.builds.enabled`, lake writes a queued image-build row and the operator runs one isolated
rootless BuildKit Job for it in a per-space namespace. Source, push, and event credentials are
single-build scoped; the Job pushes through Lake's in-cluster registry, reuses a per-space registry
cache, and reports its digest, size, progress, and failures. See [`builds.md`](builds.md).

With `spec.services.enabled`, lake runs managed services for its customers — Zetta, Postgres,
the catalog of Kafka, ClickHouse, TiDB, Cassandra and Valkey (preview), and SQLite, DuckDB and
Polars: it writes the
`services` table in its control Postgres, and the operator creates one Lake-owned
`OramaZettaCluster`, CloudNativePG `Cluster`, upstream operator's resource (Strimzi, Altinity, TiDB
Operator, K8ssandra, valkey-operator), or StatefulSet of [lake-sqlite](lake-sqlite.md), [lake-duckdb](lake-duckdb.md) or
[lake-polars](lake-polars.md) per service, writes `service_status`, and hands lake each connection
through a mounted Secret ([services.md](services.md)). This supersedes the
decision-45 workspaces ([workspaces.md](workspaces.md)); a workspace is now the customer's default
Zetta service. The same poll runs a customer's **stores** (lake#196; phase 1: cluster volumes, one
Lake-owned PersistentVolumeClaim each, connection in the same Secret) — [stores.md](stores.md).

## What a Lake reconciles into

Every reconcile manages the following resources; the Deployment and disruption budget wait for
step 10 to finish applying the desired schema (`internal/controller/lake_controller.go`):

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
   uid/gid in the image) so a freshly provisioned volume is writable by lake on first mount, and
   `fsGroupChangePolicy: OnRootMismatch` avoids recursively changing every stored object's ownership
   after a node move when the volume root is already correct. Operator-created claims are never
   replaced, resized, or deleted because the store spec changes.
6. **Deployment + disruption budget** `lake-<name>` — `spec.image` run as built, `spec.args`, port
   `spec.port` (default 4900), readiness `GET /readyz`, liveness `GET /healthz`,
   `spec.replicas`, resource defaults or `spec.resources`, `imagePullSecrets` from
   `spec.pullSecretName`, and operator env including `LAKE_STORE_ROOT=/var/lib/lake/store`,
   `LAKE_APPS_WILDCARD` when apps are enabled, and `LAKE_INTERNAL_URL` when builds or agents need
   lake's in-cluster address, plus `spec.env` entries whose names do not conflict with
   operator-owned variables. Pods run as
   uid/gid 10001 with the restricted container baseline. The Deployment uses `Recreate`, so an
   old pod releases a ReadWriteOnce store before its replacement can attach the volume on another
   node. Replicated Deployments get a
   `maxUnavailable: 1` PodDisruptionBudget and the default soft hostname spread, or
   `spec.topologySpreadConstraints` when supplied. Scaling to one replica removes the budget.
7. **Service** `lake-<name>` — ClusterIP 80 → `spec.port`.
8. **Zetta data plane** — owned `OramaZettaCluster` `lake-<name>-zetta` in the Lake namespace,
   configured from `spec.zetta` (enabled, image, replicas, per-node storage, env, and reachability).
   It inherits `spec.pullSecretName` and has operator-schema provisioning disabled because it is
   Lake's data store, not the operator's control store. Its client Service and seeded admin-token
   Secret reach the lake pod as `LAKE_DATA_PLANE_URL` and `LAKE_DATA_PLANE_TOKEN`. Deleting the
   Lake starts deletion of this child and waits for its finalizers before releasing the Lake
   finalizer; disabling `spec.zetta` tears the child down with its per-node volumes.
9. **Control-plane Postgres** — CloudNativePG `Cluster` `lake-<name>-controldb`
   (`spec.postgres`: enabled by default, instances, storageSize 50Gi, storageClassName), the same
   unstructured path `OramaCloudV1` uses (`controldb.go`). Disabling tears it down **with its
   data**.
10. **Schema** — ConfigMap `lake-<name>-controldb-migrations` + `PostgresSchema`
   `lake-<name>-controldb-schema` applying `assets/lake-schema.sql` (vendored byte-identical from
   `oramasearch/lake` `db/schema.sql`), guard table = `stores_guard` from
   `0028_stores`, version = hash of the SQL plus guard. A changed hash automatically
   replaces the obsolete migration Job, waiting for its pods to stop. Deployment creation and
   updates wait for Applied at the desired version and generation; existing pods retain their
   previous workload spec while the schema applies. Re-vendoring and native envtest proof are in
   [schema.md](schema.md). Proper incremental migrations are #126.
11. **Control-plane backup** (opt-in: `spec.postgres.backup.enabled`) — CNPG Barman object-store
   configuration on `lake-<name>-controldb` plus an owned `ScheduledBackup`
   `lake-<name>-controldb-backup`. The R2/S3 Secret lives in `spec.namespace`; CNPG owns the
   individual `Backup` resources. See [backups.md](backups.md) for R2 setup, verification,
   changing the schedule or retention, and recovery into a new Cluster.
12. **Ingress + TLS + DNS** (opt-in: `ingressEnabled` + `host`, `tls.secretName`,
   `dnsSecretRef`) — nginx Ingress for the whole host and an owned `CloudflareDNSRecord`, the
   `OramaZettaCluster` contract; gating off tears both down. The Ingress accepts Lake's 64 MiB
   request bodies without buffering streaming requests or responses. `tls.secretName` adds the
   host certificate and HTTPS redirect. When `previewWildcard` is set, the Lake also owns one
   wildcard record against the same ingress address; individual previews do not create
   Cloudflare records.
13. **Deployment environments** — desired rows in the operator's Zetta
   `orama_control.deployments` table become Lake-owned `LakeEnvironment` CRs in this namespace.
   Each CR owns its profiled workload Namespace through a verified finalizer. Referenced
   `LakeSealedSecret` resources must be current and Ready; their produced Secrets are copied into the
   isolated Namespace before the workload starts. See
   [`environments.md`](environments.md). Liquid creates, lists, rolls back, and reads these deployments
   through the operator's authenticated [deployment control plane](../control-plane/README.md).
14. **Data-service lifecycle evidence** — `status.dataServices` reports Postgres and Zetta backup,
    restore, upgrade, and failover evidence. A separate cluster-scoped `LakeRestoreRehearsal`
    controller runs Postgres procedures and updates the row; see
    [data services](data-services/README.md).
15. **Managed services** (opt-in: `spec.services.enabled`, or the deprecated
    `spec.workspaces.enabled`) — once the schema has applied, the operator polls the `services`
    table in the control Postgres (pgx, Secret `lake-<name>-controldb-app` key `uri`) every
    `spec.deploySync.interval`. Each present service becomes `<name>-<slug>`: an `OramaZettaCluster`,
    a CloudNativePG `Cluster`, or, for the catalog kinds (liquid-bridge decision 47), a Strimzi
    `Kafka`, Altinity `ClickHouseInstallation`, `TidbCluster`, `K8ssandraCluster` or `ValkeyCluster`,
    or, for `sqlite`, `duckdb` and `polars`, a `StatefulSet` and `Service` of
    [lake-sqlite](lake-sqlite.md), [lake-duckdb](lake-duckdb.md) or [lake-polars](lake-polars.md),
    controlled by the Lake; a kind whose upstream operator is not installed fails only its own
    services. Decision-45 `<name>-ws-…` workspace clusters are adopted by the customer's default
    Zetta service. Ready services' connections go to
    Secret `lake-<name>-services` (key `<slug>.json`), mounted at `/var/run/lake/services` with
    `LAKE_SERVICE_DIR`; `service_status` is upserted on change. An absent service is deleted with its
    data (irreversibly); `spec.services.maxServices` caps how many exist. The poll never fails the
    Lake. See [`services.md`](services.md).
16. **In-cluster builds** (opt-in: `spec.builds.enabled`) — queued rows in the Lake control
    Postgres become one `oramasearch/lake-builder` Job each in a deterministic per-space namespace,
    with scoped credentials, network isolation, resource/deadline/retention bounds, per-Lake and
    per-space concurrency, registry cache, progress events, and terminal digest/size status. See
    [`builds.md`](builds.md).

**Status** reports overall service readiness through `phase` and `conditions[Ready]`, alongside
Deployment replica counts, store binding, Postgres, schema, Zetta, backup, sealing, and data-service
observations. `Ready=True` (`LakeReady`) requires all of the following:

- The Deployment controller has observed its current generation, the desired replica count is
  positive, and at least that many replicas are ready, updated, and available.
- The selected store PVC is Bound.
- When Postgres is enabled, CNPG reports at least `spec.postgres.instances` ready instances
  (default 1), and the owned `PostgresSchema` reports `Applied` for its current generation with
  `appliedVersion` matching the current vendored schema hash.
- When Zetta is enabled, the owned cluster reports `Ready` with every desired node ready.
- Enabled backup configuration is valid. Backup completion remains a separate `BackupReady`
  observation.

An unfinished rollout or dependency reports `Progressing` with an explanatory `Ready=False`
condition. Schema failure, Deployment replica failure or an exceeded progress deadline, a degraded
Zetta child, or invalid backup configuration reports `Degraded`. Failures take precedence over
pending work. The condition records the Lake generation; unchanged observations preserve its
transition time and produce zero status writes, including after an operator restart.

CNPG is polled every 20 seconds for as long as Postgres is enabled, including after success and
with backups disabled. Losing database readiness therefore removes `Ready` on the next poll;
recovery restores it when every requirement is satisfied. Owned Deployment and PostgresSchema
changes also enqueue reconciliation. Pending store and Zetta observations retain their existing
20-second polls. `kubectl get lake` columns: Namespace, Host, Ready, DB, Zetta, Schema, Store,
Backup, Data, Phase. The Ready column is a replica count; use `PHASE=Ready` or the `Ready=True`
condition to confirm overall readiness. The Data column shows managed and unmanaged service
counts; see [data services](data-services/README.md) for the operation-level labels and customer
boundary.

Native fake-client regression coverage in `internal/controller/lake_readiness_test.go` exercises
failed schema with an unavailable database, old ready pods during rollout, recovery, later database
readiness loss, stale schema versions/generations, and zero status writes in steady state.

**Owned kinds** (`SetupWithManager`): PersistentVolumeClaim (only the operator-created store
claim), Deployment, PodDisruptionBudget, Service, OramaZettaCluster, Ingress, CloudflareDNSRecord, ConfigMap, Secret, LakeEnvironment, LakeSealedSecret,
PostgresSchema, CNPG
ScheduledBackup. Not owned: an existing store claim, the Namespace, the CNPG Cluster (polled), and
CNPG Backup resources (observed by cluster label). A `LakeRestoreRehearsal` temporarily owns a
scratch Namespace, CNPG Cluster, copied Secrets, and verification Job; terminal cleanup deletes
them explicitly.

**RBAC.** The chart ClusterRole is hand-maintained (`chart/templates/rbac.yaml`); a Lake needs
`lakes`, `lakes/status`, `lakes/finalizers`, `oramazettaclusters`, `namespaces: create`, plus the shared grants for
deployments, poddisruptionbudgets, persistentvolumeclaims, services, configmaps, ingresses, cloudflarednsrecords,
postgresschemas, lakerestorerehearsals, `lakesealedsecrets` (including status and finalizers),
namespace-wide Secrets and `postgresql.cnpg.io/clusters`, `clusters/status`, `scheduledbackups`, and
`backups`. Scratch Namespace ownership is checked through the direct API reader, so Namespace RBAC
remains `get`, `create`, and `delete` without a cluster-wide informer. Keep it in sync with
`config/rbac/role.yaml` or the manager
CrashLoops on cache sync.

## Observed state in Zetta

When an `OramaZettaCluster` with `spec.provisionOperatorSchema` enabled runs in the Lake's
`spec.namespace`, the existing operator state-sync loop projects the cluster-scoped Lake into
`orama_control.components`. The row is keyed by the operator identity, Lake owner, `kind=Lake`, and
Lake name. Its `status` is the Lake phase; `detail` is deterministic JSON containing the desired
replica count and the complete anti-flapped `Lake.status`, including store, Postgres, schema, backup,
data-service, sealing, and condition observations.

Each changed row also appends an `orama_control.reconcile_events` entry with action
`component.state_changed`. The event's `cluster` is the operator identity and its JSON detail names
the row and new state. The component append and event use the same authenticated, best-effort retry
path as every other operator state row. A successful pair is fingerprinted, so an unchanged Lake
writes nothing. If the component append succeeds but the event fails, the next Zetta reconcile
retries only the event.

Lake is cluster-scoped, so association is explicit: a datastore observes only Lakes whose effective
workload namespace matches its namespace (`lake-dev` when `spec.namespace` is empty). The deployment
REST/MCP contract does not read this projection; it continues to use the separate append-only
`deployments` and `deployment_records` tables described in the
[deployment control-plane guide](../control-plane/README.md).

The `LakeEnvironment` controller additionally needs `lakeenvironments` (including status and
finalizers), verified Namespace update/delete, and Deployments, Services, Ingresses,
NetworkPolicies, ResourceQuotas and LimitRanges across environment namespaces. The chart grants
these cluster-wide, while the controller deletes only a Namespace bearing its exact ownership
annotation.

## Service e2e

`LakeE2ETest` is a cluster-scoped, explicit test trigger; ordinary Lake reconciliation never runs
the suite. It waits for the target Lake to have a ready replica, ready control-plane Postgres, and
an applied schema, then creates Job `<test-name>-e2e` in `Lake.spec.namespace`. The required runner
image owns the cross-repository workflow: sign up a fresh user, create a repository, invoke the real
liquid client to push it through Lake's sync endpoint, pull into an empty checkout, and compare the
file bytes. The operator supplies only the in-cluster base URL and resource UID as
`LAKE_E2E_BASE_URL` and `LAKE_E2E_RUN_ID`; it does not give the suite the Lake admin bearer.

Apply [`../../config/samples/lake-e2e-test.yaml`](../../config/samples/lake-e2e-test.yaml) after
replacing its runner image placeholder with an immutable published tag. `spec.pullSecretName`
overrides image authentication for the Job; when omitted, the controller reuses the target Lake's
pull Secret. Inspect it with:

```sh
kubectl get lakee2e hello
kubectl logs -n lake-dev job/hello-e2e
```

Deleting and recreating the test resource starts a new run with a new UID and garbage-collects the
old Job. The k0s and air-gapped cluster executions for issue #123 remain deferred; the controller's
resource wiring and status transitions are covered with the fake client.

## Deploying the operator with Lake support

The Lake CR is on `main`. Build and publish an immutable operator image, refresh changed CRDs,
then upgrade the current release:

```sh
docker buildx build --platform linux/amd64 -t oramasearch/orama-operator:lake-crd-<sha> --load .
docker push oramasearch/orama-operator:lake-crd-<sha>
kubectl apply -f config/crd/bases/orama.cloud_lakes.yaml        # helm never upgrades crds/
helm upgrade orama-operator ./chart -n orama-system \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag=lake-crd-<sha> \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub'
```

An AKS installation still using the former Helm release must first follow the scheduled
[rename cutover runbook](../operations/rename-cutover.md). Do not change the release name with an
ordinary in-place upgrade.

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
`LAKE_DATA_PLANE_URL`, `LAKE_DATA_PLANE_TOKEN`, `LAKE_INTERNAL_URL`,
`LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, or `LAKE_STORE_ROOT`, so the reconciled Secret,
Service, and volume contracts always win. `LAKE_APPS_WILDCARD` is the exception: an explicit entry
wins over `spec.apps.wildcard`, and the operator renders it only once.

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
`LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_DATA_PLANE_URL`,
`LAKE_DATA_PLANE_TOKEN`, `LAKE_SEALING_PUBLIC_KEY`,
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
  resources:
    requests: { cpu: 250m, memory: 512Mi }
    limits: { memory: 2Gi }
  store:
    size: 100Gi
    accessModes: [ReadWriteMany]
  env:
    - name: LAKE_LOG_MODE
      value: air-gapped
    - name: LAKE_PUBLIC_HOST
      value: lake.oramadev.net
  postgres: { instances: 1, storageSize: 50Gi }
  zetta:
    replicas: 1
    image: oramasearch/zetta:<tag>
    storage: { size: 10Gi }
    env:
      - { name: ZETTA_MALLOC_TRIM_THRESHOLD_BYTES, value: "134217728" }
      - { name: MALLOC_ARENA_MAX, value: "2" }
    reachability: dial-out-only
  ingressEnabled: true
  host: lake.oramadev.net
  tls: { secretName: lake-origin-tls }
  previewWildcard: preview.lake.oramadev.net
  deploySync: { interval: 15s }
  dnsSecretRef: { name: cloudflare-api-token }
Y
kubectl wait lake/prod --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl get lake prod
```

`kubectl delete lake <name>` removes everything owned, including both databases. The Lake
finalizer waits for the Zetta child controller to finish its ordered cleanup before the parent is
released.

### AKS Zetta data-plane verification

After deploying the CRD and controller change, verify `Lake/hello` has both its Postgres control
plane and live Zetta data plane:

```sh
kubectl wait lake/hello --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl wait lake/hello --for=jsonpath='{.status.zettaReady}'=true --timeout=600s
kubectl get lake hello
kubectl get oramazettacluster lake-hello-zetta -n lake-dev
kubectl get cluster lake-hello-controldb -n lake-dev
kubectl exec -n lake-dev deploy/lake-hello -- sh -c \
  'test "$LAKE_DATA_PLANE_URL" = "http://lake-hello-zetta-zetta.lake-dev.svc.cluster.local:4800" && test -n "$LAKE_DATA_PLANE_TOKEN"'
kubectl port-forward -n lake-dev service/lake-hello-zetta-zetta 4800:4800
```

In another shell, authenticate a Zetta server-info request with the owned cluster's seeded token:

```sh
ZETTA_TOKEN=$(kubectl get secret lake-hello-zetta-zetta-admin -n lake-dev -o jsonpath='{.data.token}' | base64 -d)
curl --fail --silent --show-error -H "Authorization: Bearer $ZETTA_TOKEN" \
  http://127.0.0.1:4800/v1/server_info | jq
```

The AKS `Lake/hello` verification for issue #125 is deferred because this work order forbids
cluster changes.

## Workload hardening

The Lake Deployment defaults to CPU and memory requests of `50m` and `128Mi`, with a `512Mi`
memory limit. Set `spec.resources` to replace that complete Kubernetes resource block for a
production size. The default intentionally leaves the CPU limit unset so bursts are not throttled;
the scheduler still reserves CPU through the request.

Lake's unauthenticated operational routes drive distinct probes: `GET /healthz` is liveness and
`GET /readyz` is readiness, including control-store availability. The pod and container require
non-root uid/gid 10001, disable privilege escalation, drop every Linux capability, and use the
runtime-default seccomp profile. A five-second pre-stop drain and 30-second termination grace
period let lake finish in-flight work. The Deployment uses `Recreate`: Kubernetes stops every old
pod before starting replacements, so an RWO store can detach from the old node and attach to a new
one without a surge pod deadlock. A single-replica Lake is unavailable during this handoff;
readiness and rollout automation must allow for that planned restart gap.
The pod's `fsGroupChangePolicy: OnRootMismatch` applies the store's group ownership only when its
root does not already match gid 10001, avoiding a full recursive ownership pass on every mount.

When `spec.replicas` is greater than one, the operator creates `PodDisruptionBudget lake-<name>`
with `maxUnavailable: 1`. It also defaults `spec.topologySpreadConstraints` to a soft
`kubernetes.io/hostname` spread. Supply the field to replace that list; an explicit empty list
disables the default. At one replica the operator removes the PDB and renders no default spread.

The nginx Ingress always sets `proxy-body-size: 64m` and disables request and response buffering
for Lake's bounded streaming transfers. Set `spec.tls.secretName` to a `kubernetes.io/tls` Secret
in the Lake namespace to render `spec.tls`, enable HTTPS redirect, and terminate origin TLS at
nginx. For Cloudflare Origin CA, switch the zone to Full (strict) only after the origin certificate
is present and the Ingress answers HTTPS.

### Rollout-restart check

Run this after deploying the controller change to AKS. The Lake must be Ready and its public
hostname must already be healthy.

```sh
kubectl get deployment,poddisruptionbudget -n <namespace> lake-<name>
kubectl get pods -n <namespace> -l app.kubernetes.io/name=lake -o wide
```

Record the current pod and node, then restart and wait for the Deployment:

```sh
kubectl rollout restart deployment/lake-<name> -n <namespace>
kubectl rollout status deployment/lake-<name> -n <namespace>
kubectl get pods -n <namespace> -l app.kubernetes.io/name=lake
```

Watch events during the restart. The check passes when every old pod terminates before a replacement
is created, the replacement attaches the store without a persistent `Multi-Attach` error, and the
Lake returns to Ready. A short readiness failure is expected for one replica. The PDB still has
`maxUnavailable: 1` when the Lake has multiple replicas.

The earlier issue #122 zero-failure rolling-restart check is superseded by this RWO-safe restart
check: `Recreate` deliberately allows a readiness gap instead of starting a surge pod.

## Current production instance

`Lake/prod`, namespace `lake`, 3 replicas, 50Gi Postgres, schema `0001_identity_and_customers`
applied, https://lake.oramadev.net; operator helm revision 8, `lake-crd-8058ab5` (2026-09-02).
Details and the not-yet list: https://github.com/oramasearch/lake/blob/main/docs/deployment/aks.md.

## Open

- #126 incremental migrations in `PostgresSchema`.
- [`agents.md`](agents.md) — the Agent Library's agents, the launches of user-created agents on agent-runtime, their cron schedules as CronJobs, and each library agent's own agent-bus credential (lake#228, lake#231, lake#244, lake#269).
