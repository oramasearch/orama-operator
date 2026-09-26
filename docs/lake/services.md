# Lake managed services

How a lake customer gets its own Zetta, Postgres, Kafka, ClickHouse, TiDB, Cassandra, Valkey and
SQLite services without lake having any Kubernetes access: lake writes the `services` table in its
control Postgres, the operator creates one Lake-owned resource per service (for Kafka through
Valkey, the resource of that kind's upstream operator — [the catalog](#the-catalog-decision-47);
for SQLite, a StatefulSet of [lake-sqlite](lake-sqlite.md), which the operator runs itself —
[SQLite](#sqlite-decision-47-amended)), writes what it observed to `service_status`, and hands lake
each service's connection through one mounted Secret. The authoritative contract is lake's
[`docs/control-plane/services.md`](https://github.com/oramasearch/lake/blob/17b4ad57f2571c30516f7ecb85c03a4165e32e59/docs/control-plane/services.md)
(liquid-bridge decisions 46 and 47, amended; lake branch `feat/sqlite-service`); this page is the
operator's side of it. Field definitions are in the
[`Lake` CRD reference](../crd-reference.md#lake-separate-crd-cluster-scoped--the-lake-service)
(`spec.services`). It supersedes [workspaces](workspaces.md): a customer's workspace is now its
**default Zetta service**.

## Turning it on

```yaml
spec:
  services:
    enabled: true
    maxServices: 20          # 0 = no limit
    zettaImage: oramasearch/zetta:<tag>
    sqliteImage: oramasearch/lake-sqlite:<tag>   # optional; the chart's lakeServices.sqliteImage otherwise
```

Services need the control-plane Postgres (`spec.postgres.enabled`, the default); they do not need
the Zetta data plane. `spec.workspaces` is a deprecated alias: `workspaces.enabled` turns services
on, `workspaces.image` is the Zetta image when `services.zettaImage` is empty, and
`workspaces.maxWorkspaces` is the cap when `services.maxServices` is `0`. `workspaces.resources`
and `workspaces.storage.storageClassName` still shape every Zetta service's nodes.

## The poll

The Lake reconciler polls, once the Lake's schema has applied (the tables come with migration
`0024_services`; `0025_service_catalog` widens `kind` to the catalog and `0026_sqlite_services` adds
`sqlite`), at the Lake's `spec.deploySync.interval` (default `15s`, floor `1s`) — the
cadence the workspace poll used. Each poll (`internal/controller/lake_services.go`):

1. reads Secret `lake-<lake>-controldb-app` key `uri` (the CloudNativePG application Secret of
   the Lake's control DB). A missing Secret or an empty `uri` means "not yet": it is logged and the
   poll ends. It connects with pgx (one connection per poll, closed at the end, 30-second bound on
   the whole pass).
2. reads every service joined to its status, oldest first, leaving out history (`desired =
   'absent'` with phase `Deleted`).
3. handles **absent** services first, so a deletion frees its quota slot in the same poll;
4. counts the live services for the quota;
5. creates or updates each **present** service;
6. writes the credentials Secret;
7. upserts `service_status` for each service whose phase, message, endpoint or observed
   generation changed. A steady service writes nothing.

Nothing in the poll fails the Lake reconcile: an unreachable database or a failed Kubernetes call
is logged and retried on the next poll. The store is the Go interface `LakeServiceStore` (read
services, upsert status); `lake_services_pgx.go` is the pgx implementation, and tests substitute
an in-memory fake through `LakeReconciler.ServiceStores`.

## Present services

Every service becomes **`<lake>-<slug>`** in the Lake namespace, controlled by the Lake, with
labels `orama.cloud/lake=<lake>`, `orama.cloud/service-id=<id>`, `orama.cloud/service-kind=<kind>`,
`orama.cloud/customer-id` (a `sha256-…` digest when the id is not a valid label value) and
`app.kubernetes.io/name=lake-service`, and annotation `orama.cloud/customer-id=<customer_id>`.

| | Zetta (`kind = 'zetta'`) | Postgres (`kind = 'postgres'`) |
|---|---|---|
| Resource | `OramaZettaCluster` | CloudNativePG `Cluster` (`postgresql.cnpg.io/v1`, unstructured, as `controldb.go` does) |
| From `spec.instances` | `replicas` | `instances` |
| From `spec.storage_gi` | `storage.size` `<n>Gi`, class from `spec.workspaces.storage.storageClassName` | `storage.size` `<n>Gi`, class from `spec.postgres.storageClassName` |
| Other fields | image `spec.services.zettaImage` (then `spec.workspaces.image`), the Lake's `pullSecretName`, `resources` from `spec.workspaces.resources` (default memory `64Mi`/`1Gi`, no CPU request), no ingress, no operator schema | `bootstrap.initdb` database `app`, owner `app` |
| Ready when | status phase `Ready` and `readyNodes` ≥ instances | `status.readyInstances` = instances |
| Credential source | `<cluster>-zetta-admin` key `token` | the whole CloudNativePG `<cluster>-app` Secret |
| `endpoint` | `http://<cluster>-zetta.<ns>.svc.cluster.local:4800` | `<cluster>-rw.<ns>.svc.cluster.local:5432` |

`spec` defaults are `instances: 1` and `storage_gi: 10`; values outside 1–3 and 1–100 fail the
service. The data volume and the Postgres bootstrap are written only when the resource is created:
a StatefulSet's volume template is immutable and the contract has no resize. Only named fields are
written, so server-side defaults never turn a poll into an update.

### Adoption of workspace clusters

A decision-45 workspace cluster — `OramaZettaCluster <lake>-ws-<12 hex>`, controlled by the Lake,
labelled `orama.cloud/lake=<lake>` and `orama.cloud/workspace-id=<slug>`, with no service-id label,
and whose customer annotation (if any) matches — is **adopted** by the customer's default Zetta
service with that slug. Adoption adds the service labels (the `workspace-id` label stays), keeps the
existing data volume and its size, and is not subject to the quota, so an existing workspace keeps
its data.

Anything else already holding the name fails the service and is never modified or deleted:

| Case | `Failed` message |
|---|---|
| a cluster of that name that is not this Lake's service (another controller, another Lake, another service id, or a workspace cluster claimed by a non-default service) | `OramaZettaCluster <ns>/<name> exists and is not this Lake's service`, or `CloudNativePG Cluster <ns>/<name> exists and is not this Lake's service` |
| a new Zetta service whose data PVC `data-<cluster>-zetta-<n>` already exists without its cluster | `PersistentVolumeClaim <ns>/<pvc> exists and is not this Lake's service` |
| a malformed row (slug, default on a non-Zetta service, missing customer, a Lake name too long for the kind's derived names, bad spec) | the specific reason, e.g. `spec.instances must be between 1 and 3` |
| a kind this operator does not run | `unknown service kind "<kind>": this operator runs zetta, postgres, kafka, clickhouse, tidb, cassandra, valkey, sqlite` |
| a sqlite service whose `spec.instances` is not 1 | `a sqlite service has exactly one instance, because SQLite has one writer; spec.instances is <n>` |

## The catalog (decision 47)

Kafka, ClickHouse, TiDB, Cassandra and Valkey are each run by their own upstream operator, which the
operator's `helmfile.yaml` installs cluster-wide. The Orama operator writes that operator's
resources and never runs the database itself (`internal/controller/lake_service_catalog.go` for
what every kind shares, `lake_service_<kind>.go` for each kind). The resources are
**unstructured** objects with a GVK constant, as CloudNativePG's are: the operator does not import
the upstream Go modules. Every field name below was read from the upstream CRD at the pinned
version (and, where the CRD does not say, from that version's source); every built object was
validated against those CRD schemas.

Every upstream object is named from `<lake>-<slug>`, controlled by the Lake, and carries the
service labels and annotation above — except `app.kubernetes.io/instance`, which is the object's own
name: upstream operators read that label as their cluster's instance (TiDB Operator derives every
selector from it), so a Lake-wide value would make two services of one kind select each other's
pods. Only the fields the table lists are written, individually, so the upstream API server's
defaults survive and a steady service issues no update; storage, and arrays whose items the CRD
defaults, are written once at creation.

| | Kafka | ClickHouse | TiDB | Cassandra | Valkey (**preview**) |
|---|---|---|---|---|---|
| Upstream operator | Strimzi 1.2.0 | Altinity 0.27.4 | TiDB Operator v1.6.6 (stable 1.x) | K8ssandra Operator 1.33.0 (drives cass-operator v1.32.0) | valkey-io/valkey-operator 0.6.0 |
| Resources written | `kafka.strimzi.io/v1` `Kafka` `<name>`; `KafkaNodePool` `<name>-pool`; `KafkaUser` `<name>-app` (both labelled `strimzi.io/cluster=<name>`) | `clickhouse.altinity.com/v1` `ClickHouseInstallation` `<name>`; with `instances` > 1, `clickhouse-keeper.altinity.com/v1` `ClickHouseKeeperInstallation` `<name>` | `pingcap.com/v1alpha1` `TidbCluster` `<name>` | `k8ssandra.io/v1alpha1` `K8ssandraCluster` `<name>` | `valkey.io/v1alpha1` `ValkeyCluster` `<name>` |
| `instances` → | node pool `replicas`, roles `controller` and `broker` (KRaft) | `configuration.clusters[main].layout`: `shardsCount: 1`, `replicasCount` | `pd`, `tikv`, `tidb` `replicas` each, `maxFailoverCount: 0` | the one datacenter's `size` | `shards: 3`, `replicas: instances - 1` |
| `storage_gi` → | node pool `storage`: `persistent-claim`, `size`, `deleteClaim: false` | volume claim template `data` (`defaults.templates.dataVolumeClaimTemplate`); the Keeper gets `1Gi` | `pd.requests.storage`, `tikv.requests.storage` | datacenter `storageConfig.cassandraDataVolumeClaimSpec` | `persistence.size` |
| Other fields | listener `scram` on 9092, `internal`, no TLS, `scram-sha-512` authentication; entity operator with topic and user operators; broker `config` replication factors = `instances`, min ISR 1 (2 for three brokers); `KafkaUser` `authentication: scram-sha-512` | `configuration.users`: `app/password_sha256_hex`, `app/networks/ip: ["::/0"]`, `app/profile`, `app/quota` `default`; `configuration.zookeeper.keeper.name` when replicated; Keeper: cluster `keeper`, one replica | `version: v8.5.7`, `pvReclaimPolicy: Delete`, `discovery: {}`, `pd.config.replication.max-replicas: instances`, `tikv.config.storage.reserve-space` a tenth of the volume, `tidb.service.type: ClusterIP`, `tidb.initializer.createPassword: true` | `serverType: cassandra`, `serverVersion: 5.0.4`, `clusterName: <slug>`, datacenter `metadata.name: <slug>`, `datacenterName: dc1` | users: `default` disabled; `app` with `passwordSecret` `<name>-credentials`/`password`, `commands.allow: ["@all"]`, `keys.readWrite: ["*"]`, `channels.patterns: ["*"]` |
| Ready when | `Kafka` condition `Ready=True`, and the `KafkaUser` Secret exists | `status.status` = `Completed` (and the Keeper's, when there is one) | `status.{pd,tikv,tidb}.phase` = `Normal` with `statefulSet.readyReplicas` = `instances`, and `status.tidb.passwordInitialized` | `status.datacenters.<slug>.cassandra`: `cassandraOperatorProgress: Ready` and condition `Ready=True` | condition `Ready=True` |
| Credential source | Secret named by the `KafkaUser`'s `status.secret`, key `password`, owned by that `KafkaUser` | Secret `<name>-credentials`, key `password`, generated by the Orama operator | Secret `<name>-init`, key `root`, generated by the Orama operator | Secret `<slug>-superuser`, keys `username`, `password`, controlled by the `K8ssandraCluster` | Secret `<name>-credentials`, key `password`, generated by the Orama operator |
| `endpoint` | `<name>-kafka-bootstrap.<ns>.svc.cluster.local:9092` | `clickhouse-<name>.<ns>.svc.cluster.local:8123` | `<name>-tidb.<ns>.svc.cluster.local:4000` | `<slug>-<slug>-service.<ns>.svc.cluster.local:9042` | `valkey-<name>.<ns>.svc.cluster.local:6379` |
| PVCs deleted after the resources, selected by | `strimzi.io/cluster=<name>`, `strimzi.io/kind=Kafka` | `clickhouse.altinity.com/chi=<name>` or `clickhouse-keeper.altinity.com/chk=<name>`, `…/app=chop` (Altinity normally deletes them itself) | `app.kubernetes.io/instance=<name>`, `app.kubernetes.io/managed-by=tidb-operator` | `cassandra.datastax.com/cluster=<slug>` | `valkey.io/cluster=<name>`, `app.kubernetes.io/managed-by=valkey-operator` |

`<name>` is `<lake>-<slug>`. A password the Orama operator generates is 32 hex characters, stored
once in a Secret controlled by the Lake and labelled with the service
(`app.kubernetes.io/component=credentials`), created before the upstream resource and never
rotated; a Secret of that name the Lake does not own fails the service. A Secret an upstream
operator writes is trusted only when it is owned by the upstream object it belongs to; otherwise
the service fails with `Secret <ns>/<name> exists and is not this Lake's service`.

**Where this departs from the contract table** (the CRD wins, per the contract):

- **Kafka user name.** The contract names the `KafkaUser` `app`. A `KafkaUser`'s name is its
  SCRAM username and is unique per namespace, so two Kafka services of one Lake would collide; it
  is `<name>-app`, and the credential's `username` says so. The API is `kafka.strimzi.io/v1`, the
  only version Strimzi 1.2.0 serves; KRaft is its only mode, so there is no KRaft annotation to set.
- **ClickHouse endpoint.** The table gives `<name>-clickhouse…` and names the installation's own
  Service `clickhouse-<name>` in the same cell; Altinity creates only the latter, so that is the
  host. The Keeper is referenced through `configuration.zookeeper.keeper.name`, the 0.27.4 CRD's
  field for it, and readiness also waits for the Keeper.
- **TiDB password Secret.** The v1.6.6 CRD's `tidb.initializer` has one field, `createPassword`;
  with it, TiDB Operator sets the root password from Secret `<cluster>-init`, key `root`, when that
  Secret exists. So the Orama operator generates `<name>-init` rather than `<name>-credentials`,
  and readiness also waits for `status.tidb.passwordInitialized`. TiDB Operator keeps PD and TiKV
  claims after the `TidbCluster` is deleted, so deletion removes them as it does for Kafka,
  Cassandra and Valkey (the contract lists only those three).
- **Cassandra names.** The contract's endpoint `<name>-dc1-service` needs a `CassandraDatacenter`
  object named `dc1`, and that object is namespaced: a second Cassandra service in the Lake's
  namespace would collide with the first. The datacenter object is named after the slug, and so is
  the Cassandra cluster, which keeps cass-operator's StatefulSet `<slug>-<slug>-default-sts` at 45
  characters whatever the Lake is called (cass-operator's webhook rejects more than 60, and the pod
  revision-hash label allows 52). `datacenterName: dc1` keeps the Cassandra datacenter itself named
  `dc1`, which is what clients route by and what the credential says. The superuser Secret follows
  the cluster name: `<slug>-superuser`.
- **Valkey default user.** Not in the contract: Valkey's built-in `default` user is switched off,
  since left unmanaged it accepts unauthenticated clients. The operator's probes and replication
  use its own system users.
- **Additions** the contract does not mention: every catalog credential carries `endpoint` besides
  `kind` (the contract lists it as common to all); Kafka's internal-topic replication factors
  follow the broker count (Kafka's default of three leaves consumer groups unavailable on fewer
  brokers); TiDB runs with `maxFailoverCount: 0` so it never exceeds `instances` pods; and, as for
  Zetta, a new service whose selector already matches a PVC does not start.

**A kind whose upstream operator is not installed** (its CRD is not served, so the API returns a
no-match error) fails that service with `the <kind> operator is not installed on this cluster`, for
example `the kafka operator is not installed on this cluster`. Nothing else is affected: the rest
of the Lake's services, the quota count (a kind that is not installed has no live resources) and
the Lake reconcile carry on; the service starts on a later poll once the operator is installed. The
upstream resources are read uncached, so a missing CRD never blocks the manager's cache.

**Names.** Each kind adds its longest derived name to `<lake>-<slug>`, counting a StatefulSet's
11-character revision-hash suffix, and a Lake name that would push it past 63 characters fails the
service with `lake name "<lake>" is too long for service resource names`: Zetta and Postgres 15
characters, Kafka 16 (`-kafka-bootstrap`), TiDB 16 (`<name>-tikv` + hash), Valkey 22
(`valkey-<name>-2-2` + hash), ClickHouse 26 (`chk-<name>-keeper-0-0` + hash), SQLite 11 (the
StatefulSet `<name>`'s revision hash, so Zetta's 15 is the bound). Cassandra's derived names come
from the slug alone. With a 16-character slug, ClickHouse allows Lake names up to 20
characters and every other kind at least 24.

## SQLite (decision 47, amended)

SQLite has no upstream operator, so the Orama operator runs it directly behind
**[lake-sqlite](lake-sqlite.md)**, a small web service in its own private repository (`oramasearch/lake-sqlite`,
image `oramasearch/lake-sqlite`) over one SQLite database. The driver is
[`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) — pure Go, no cgo, **BSD-3-Clause**. The
kind goes through the catalog machinery (`internal/controller/lake_service_sqlite.go`), so
ownership, labels, the refusal of foreign objects and Secrets, the quota, the old-volume check,
status and the deletion order are exactly the catalog's; only the objects differ, and they are
built-in Kubernetes kinds rather than an upstream operator's.

| | SQLite |
|---|---|
| Resources written | `apps/v1` `StatefulSet` `<name>` and `v1` `Service` `<name>` (ClusterIP, port `8080` → the pod's `http` port), plus Secret `<name>-credentials` |
| `instances` → | must be 1 (the default); anything else fails the service (SQLite has one writer; lake enforces the same) |
| `storage_gi` → | the StatefulSet's `volumeClaimTemplates` `data` (`ReadWriteOnce`, the default StorageClass), mounted at `/data`; the database is `/data/main.db`, in WAL mode |
| Pod | one container `lake-sqlite`, image `spec.services.sqliteImage`, then the operator's `--lake-sqlite-image` (the chart's `lakeServices.sqliteImage`), then `oramasearch/lake-sqlite:0.1.1`; the Lake's `pullSecretName`; requests `50m` CPU and `64Mi` memory, limit `512Mi` memory, handed to the binary as `LAKE_SQLITE_MEMORY_LIMIT` (it holds the Go heap to three quarters and SQLite's soft heap limit to a quarter); startup, readiness and liveness probes on `GET /healthz`; uid/gid/fsGroup `65532`, `runAsNonRoot`, read-only root filesystem, no privilege escalation, every capability dropped, `RuntimeDefault` seccomp, no service-account token; a `1Gi` `emptyDir` at `/tmp` for SQLite's temporary files; `LAKE_SQLITE_TOKEN` from `<name>-credentials` key `token` |
| Ready when | the StatefulSet's `status.readyReplicas` is 1 — its readiness probe passes only while `/healthz` opens the database and answers `SELECT 1` — and the credential is written |
| Credential source | Secret `<name>-credentials`, key `token`: 32 random bytes as 64 hex characters, generated by the Orama operator, never rotated |
| `endpoint` | `http://<name>.<ns>.svc.cluster.local:8080` |
| PVCs deleted after the StatefulSet, selected by | the StatefulSet's selector, which the StatefulSet controller copies onto `data-<name>-0`: `app.kubernetes.io/name=lake-sqlite`, `app.kubernetes.io/instance=<name>`, `orama.cloud/service-id=<id>` |

The pod template is written when the StatefulSet is created and again only when it changes: its
hash is kept in the annotation `orama.cloud/applied-spec-hash`, since the API server's many pod
defaults would otherwise make every poll an update. A new image (a changed `sqliteImage` or chart
default) therefore rolls every SQLite service's pod once. The selector, the governing Service name
and the claim template are written only at creation (they are immutable), so `storage_gi` never
resizes a volume, as for every kind.

The lake-sqlite API (the contract gives the outline; [lake-sqlite.md](lake-sqlite.md) has the whole
of it):

- `POST /v1/query` `{sql, params}` answers `{columns, rows}` with JSON integers, floats, strings
  and `null`, a blob as `{"$base64": "…"}`, at most 10,000 rows and `"truncated": true` when cut;
- `POST /v1/exec` `{sql, params}` answers `{rows_affected, last_insert_id}`;
- `POST /v1/batch` `{statements: [{sql, params}]}` runs every statement in one transaction and
  answers `{results: [...]}`, one per statement; any error rolls the whole batch back;
- `GET /healthz` answers 200 only when the database opens and `SELECT 1` works;
- every route but `/healthz` needs `Authorization: Bearer <token>`, compared in constant time;
  errors are always `{error: {code, message}}` (400 for SQL errors, 401 for a bad token, 413 for a
  body over 16 MiB).

**Where this departs from the contract or adds to it:** the Service is named `<name>`, exactly as
the contract's endpoint says, and there is no headless Service (the StatefulSet's `serviceName` is
that ClusterIP Service; nothing resolves the pod by name). The generated token lives in the same
`<name>-credentials` Secret name as the other generated passwords, keyed `token`. There is no
replication and no backup: **the volume is the only copy** of a SQLite service's data, and deleting
the service deletes it.

## Credentials

Secret **`lake-<lake>-services`** (controlled by the Lake) holds one key per Ready service,
**`<slug>.json`**:

```json
{"kind":"zetta","endpoint":"http://prod-svc-0123456789ab-zetta.lake.svc.cluster.local:4800","token":"<admin token>"}
{"kind":"postgres","host":"prod-svc-0123456789ab-rw.lake.svc.cluster.local","port":5432,"database":"app","user":"app","password":"…","uri":"postgresql://app:…@prod-svc-0123456789ab-rw.lake:5432/app"}
{"kind":"kafka","endpoint":"prod-svc-0123456789ab-kafka-bootstrap.lake.svc.cluster.local:9092","bootstrap":"prod-svc-0123456789ab-kafka-bootstrap.lake.svc.cluster.local:9092","username":"prod-svc-0123456789ab-app","password":"…","sasl_mechanism":"SCRAM-SHA-512","security_protocol":"SASL_PLAINTEXT"}
{"kind":"clickhouse","endpoint":"clickhouse-prod-svc-0123456789ab.lake.svc.cluster.local:8123","host":"clickhouse-prod-svc-0123456789ab.lake.svc.cluster.local","http_port":8123,"native_port":9000,"user":"app","password":"…","database":"default"}
{"kind":"tidb","endpoint":"prod-svc-0123456789ab-tidb.lake.svc.cluster.local:4000","host":"prod-svc-0123456789ab-tidb.lake.svc.cluster.local","port":4000,"user":"root","password":"…","uri":"mysql://root:…@prod-svc-0123456789ab-tidb.lake.svc.cluster.local:4000"}
{"kind":"cassandra","endpoint":"svc-0123456789ab-svc-0123456789ab-service.lake.svc.cluster.local:9042","host":"svc-0123456789ab-svc-0123456789ab-service.lake.svc.cluster.local","port":9042,"datacenter":"dc1","user":"svc-0123456789ab-superuser","password":"…"}
{"kind":"valkey","endpoint":"valkey-prod-svc-0123456789ab.lake.svc.cluster.local:6379","host":"valkey-prod-svc-0123456789ab.lake.svc.cluster.local","port":6379,"user":"app","password":"…","cluster":true}
{"kind":"sqlite","endpoint":"http://prod-svc-0123456789ab.lake.svc.cluster.local:8080","token":"<64 hex>","database":"main"}
```

`uri` is CloudNativePG's own, copied unchanged; TiDB's is built from the host and the generated
password. A present service keeps its key while its
connection cannot be read, so a transient failure never removes a file lake depends on; the key
of an absent service, or of a row that no longer exists, is dropped.

The lake Deployment mounts the Secret read-only at **`/var/run/lake/services`** (volume
`services`, `optional: true`, mode `0440`) and gets **`LAKE_SERVICE_DIR=/var/run/lake/services`**.
It no longer gets `LAKE_WORKSPACE_DIR` or `LAKE_WORKSPACE_URL_TEMPLATE`: lake reads endpoints from
`service_status`. `LAKE_DATA_PLANE_URL` and `LAKE_DATA_PLANE_TOKEN` are unchanged; they are lake's
own data plane. kubelet refreshes a whole-directory Secret volume in place (about a minute), so a
new key never restarts lake, and lake should retry a missing file rather than fail. The superseded
Secret `lake-<lake>-workspaces` is deleted once services run.

## Status

| Phase | When | Message |
|---|---|---|
| `Provisioning` | the resource exists and is not ready | `waiting for the Zetta nodes` / `waiting for the Postgres instances` / `waiting for the Kafka cluster` / `waiting for the ClickHouse installation` / `waiting for the ClickHouse Keeper` / `waiting for TiDB <pd\|tikv\|tidb>` / `waiting for the TiDB root password` / `waiting for the Cassandra datacenter` / `waiting for the Valkey cluster` / `waiting for the lake-sqlite pod` |
| `Provisioning` | the resource is ready but its credential is not yet in the Secret | `waiting for the service credential` |
| `Provisioning` | a previous resource or data volume of that name is still terminating | `waiting for the previous … to be deleted` |
| `Ready` | the resource is ready **and** its credential was written in this poll | `service ready` |
| `Failed` | ownership, validation, a missing upstream operator or the quota refused the service | see above; quota: exactly `service quota reached (<n>)`; `the <kind> operator is not installed on this cluster` |
| `Deleting` | an absent service's resources are still going | `deleting the Zetta cluster`, `deleting the Zetta data volumes`, `deleting the Postgres cluster`, `deleting the Postgres data volumes`, and for the catalog `deleting the <Kafka\|ClickHouse\|TiDB\|Cassandra\|Valkey\|SQLite> cluster`, then `… data volumes` |
| `Deleted` | nothing remains | `service deleted` |

`endpoint` is written once the resource exists and is this service's (it is empty on `Failed` and
`Deleted`, and kept while `Deleting`); that is also the proof the deletion path uses for leftover
volumes. `observed_generation` is the `generation` of the row the phase describes.

## Absent services (deletion)

**Deleting a service deletes its data, irreversibly.** For each absent service not yet `Deleted`:

- **Zetta:** check ownership (as above, a not-yet-adopted workspace cluster of a default service
  counts as the service's); delete the data PVCs `data-<cluster>-zetta-<n>`, **then** the cluster
  (background propagation). If the cluster is already gone, a leftover live PVC is deleted only
  when the service's status carries an endpoint.
- **Postgres:** check ownership; delete the CloudNativePG `Cluster`. Its PVCs (label
  `cnpg.io/cluster=<cluster>`) are owned by it and garbage-collected.
- **Catalog kinds and SQLite:** check the ownership of every object the kind can write, and
  delete them all in one pass, primary first (`Kafka`, `KafkaNodePool`, `KafkaUser`;
  `ClickHouseInstallation`, `ClickHouseKeeperInstallation`; SQLite's `StatefulSet`, then its
  `Service`), with background propagation. Only once every one is gone — so nothing can recreate a
  claim — delete the PVCs matching the kind's selector above (for SQLite, `data-<name>-0`, and only
  when it carries the StatefulSet's labels); only once those are gone too, delete the generated
  password or token Secret (when this service owns it), so the credential is the last thing to go.
  A PVC is deleted only when the service's status carries an endpoint, which the operator writes
  for a resource it owns; the first deletion pass writes it too. A kind whose operator is not
  installed has nothing to delete but its leftover PVCs.
- remove `<slug>.json` from `lake-<lake>-services`;
- write `Deleted` only once the resource and every data PVC are gone; until then `Deleting`, and
  the next poll repeats the steps.

A resource that fails the ownership rule is never deleted; the service is `Failed` instead.

## Quota

`spec.services.maxServices` (falling back to `spec.workspaces.maxWorkspaces`; `0` = no limit) caps
the live services: Lake-controlled `OramaZettaCluster`s, CloudNativePG `Cluster`s and the primary
resource of each catalog kind (`Kafka`, `ClickHouseInstallation`, `TidbCluster`,
`K8ssandraCluster`, `ValkeyCluster`, and SQLite's `StatefulSet`; a kind that is not installed counts
none) labelled
`orama.cloud/lake=<lake>` with a service-id (or a workspace-id, for unadopted workspaces), not being
deleted and not absent in the current read. A **new** service over the cap creates nothing and is
`Failed` / `service quota reached (<n>)`; it starts on a later poll once there is room. An existing
service — including an adopted workspace — is never refused or removed, even after the cap is
lowered. lake's own per-customer cap (`LAKE_SERVICES_PER_CUSTOMER`) is separate.

## Other ways a service goes away

Disabling services stops the poll and removes the mount and env; it deletes nothing. Deleting the
Lake deletes every Lake-controlled `OramaZettaCluster` through the Lake finalizer and
garbage-collects the CloudNativePG Clusters, the catalog kinds' upstream resources, SQLite's
StatefulSets and Services, and the Secrets; Zetta data PVCs, SQLite's `data-<name>-0`, and the PVCs
that Strimzi, TiDB Operator, cass-operator and the Valkey operator keep, are not deleted by that
path. The Lake's `spec.zetta` reconcile skips service
clusters.

## RBAC

Zetta and Postgres need nothing beyond what the manager already had (`postgresql.cnpg.io/clusters`,
`secrets`, `persistentvolumeclaims` including `delete`, `oramazettaclusters`). The catalog adds
`get, list, watch, create, update, patch, delete` on `kafka.strimzi.io` `kafkas`, `kafkanodepools`,
`kafkausers`; `clickhouse.altinity.com` `clickhouseinstallations`; `clickhouse-keeper.altinity.com`
`clickhousekeeperinstallations`; `pingcap.com` `tidbclusters`; `k8ssandra.io` `k8ssandraclusters`;
and `valkey.io` `valkeyclusters` — as kubebuilder markers in `lake_service_catalog.go`
(`make manifests` regenerates `config/rbac/role.yaml`) and in the chart's hand-maintained
ClusterRole. `go test ./cmd/...` (`TestChartRBACCoversGeneratedRole`) fails when the chart grants
less than the markers ask for. None of these kinds is watched, so no CRD of theirs is needed for the
manager to start. SQLite needs nothing new: the manager already manages `statefulsets`, `services`,
`secrets` and `persistentvolumeclaims`.

## Known limits

- lake receives each Zetta service's **admin** token, and the operator connects to lake's control
  Postgres with lake's own owner credential (decision 46 explains why a narrower role was not worth
  it yet).
- Postgres services get CloudNativePG's default resources and no backup; Zetta services have no
  backup either. Endpoints are in-cluster only.
- `spec` cannot change after creation.
- Services whose rows disappear without passing through `absent` (lake's `services.customer_id`
  cascades on customer deletion) lose their credential key but their resources are not deleted:
  the operator deletes only what a row marked absent.
- The catalog kinds run with their upstream operators' default container resources and binary
  memory settings, and on the cluster's default StorageClass: none of them is right-sized yet
  (CLAUDE.md principle 8), and none has backups.
- **Valkey is a preview**: valkey-operator 0.6.0's README says it is not ready for production.
- **SQLite has one copy.** One pod, one `ReadWriteOnce` volume, no replication and no backup yet:
  a lost volume loses the database, and the service is unavailable while its pod restarts or moves.
- The upstream operators themselves (and cert-manager, which K8ssandra needs) are installed by the
  `helmfile.yaml`; until they are, each kind's services report the not-installed message.
- The catalog has been exercised against the fake client and validated against the pinned CRD
  schemas only, not yet against the running upstream operators. The SQLite kind has been exercised
  against the fake client only; the lake-sqlite image has not yet been built or run on a cluster.

## How to verify

```sh
go test ./internal/controller/ -run Services
go test ./cmd/...        # the chart ClusterRole grants every RBAC marker
# The pgx SQL against the vendored schema on a local Postgres, inside a rolled-back transaction:
LAKE_SERVICES_TEST_POSTGRES=1 go test ./internal/controller/ -run PgxStore

# On a cluster, after creating a service on lake's /services page:
kubectl -n lake get oramazettacluster,clusters.postgresql.cnpg.io -l orama.cloud/lake=prod
kubectl -n lake get kafka,kafkanodepool,kafkauser,chi,chk,tidbcluster,k8ssandracluster,valkeycluster -l orama.cloud/lake=prod
kubectl -n lake get statefulset,service,pvc -l orama.cloud/service-kind=sqlite
kubectl -n lake get secret lake-prod-services -o jsonpath='{.data}' | jq 'keys'
kubectl -n lake exec deploy/lake-prod -- ls /var/run/lake/services
```

The unit tests (`lake_services_test.go`) cover Zetta and Postgres creation, Ready gated on the
credential, adoption of a `ws-` cluster keeping its data, refusal of foreign resources and
unproven volumes, the deletion order and `Deleted`, the quota message and the fact that it never
refuses existing services, status upserts only on change, the "not yet" app Secret, and the lake
Deployment's mount and env. `lake_service_catalog_test.go` runs every catalog kind, SQLite included, through: the
resources built with the right GVK, replicas and storage, labels and ownership; no update on a
steady poll and no password rotation; Ready gated on the upstream status (and a partly ready
status) plus the credential; the exact credential fields; the deletion order (upstream objects,
then the PVCs, then the generated Secret, leaving an unrelated PVC alone) through `Deleting` to
`Deleted`; refusal of a foreign object, a foreign password Secret and an upstream Secret its
object does not own, with nothing deleted; the not-installed message while a Postgres service in
the same poll carries on; an old volume blocking creation; the quota; the unknown-kind message;
and the per-kind name length. `lake_service_sqlite_test.go` adds SQLite's own: the StatefulSet,
pod, probes, resources, security context, Service and 64-hex token; `instances` other than 1
failing; the image order (Lake, flag, default); an image change rewriting the template once and a
steady pass after it writing nothing; a claim without the StatefulSet's labels surviving deletion;
and the quota. The pgx test also shows the vendored schema (`0025_service_catalog` and
`0026_sqlite_services`) accepts a row of every catalog kind and `sqlite`, which reads back as that
kind, and still refuses any other.

## How to change

- **The contract** (tables, phases, messages, Secret name and key format, labels): change it in
  lake's `docs/control-plane/services.md` first, then here and in `lake_services.go` together.
- **Resource shape**: the mutate functions in `reconcileZettaService` and
  `reconcilePostgresService`, and each catalog kind's `build` in `lake_service_<kind>.go`; keep
  writing named fields only, mark storage and defaulted arrays `createOnly`, and read field names
  from the upstream CRD at the pinned version.
- **A new catalog kind**: implement `catalogKind` in its own `lake_service_<kind>.go`, register it
  in `catalogKinds` and `serviceKindNames`, add its RBAC marker and the chart ClusterRole rule,
  and add its case to `catalogCases()` in the tests. lake's schema must allow the kind first.
- **Deletion**: `deleteLakeService` and `deleteCatalogService`; keep the ownership check before
  every delete, Zetta PVCs before the cluster, a catalog kind's PVCs only after its resources and
  its generated Secret only after its PVCs, and `Deleted` only once nothing remains.
- **lake-sqlite itself**: see [lake-sqlite.md](lake-sqlite.md); its API is part of the contract.
- **Schema**: the tables arrive with lake's `db/schema.sql`; see [schema.md](schema.md).
