# Lake managed services

How a lake customer gets its own Zetta, Postgres, Kafka, ClickHouse, TiDB, Cassandra, Valkey,
SQLite, DuckDB and Polars services without lake having any Kubernetes access: lake writes the
`services` table in its control Postgres, the operator creates one Lake-owned resource per service
(for Kafka through Valkey, the resource of that kind's upstream operator —
[the catalog](#the-catalog-decision-47); for SQLite, DuckDB and Polars, a StatefulSet of
[lake-sqlite](lake-sqlite.md), [lake-duckdb](lake-duckdb.md) or [lake-polars](lake-polars.md), which
the operator runs itself — [SQLite](#sqlite-decision-47-amended),
[DuckDB and Polars](#duckdb-and-polars-lake195)), writes what it observed to `service_status`, and
hands lake each service's connection through one mounted Secret. The exception is the Zetta named
by a workspace's `zetta_service_id`: the workspace reconciler owns that cluster and its
`service_status`, while this poll only delivers its connection into the services Secret. The authoritative contract is
lake's
[`docs/control-plane/services.md`](https://github.com/oramasearch/lake/blob/aee5cdada63cb6fed886c67a6fff86123b742bf5/docs/control-plane/services.md)
(liquid-bridge decisions 46 and 47, amended; lake#195; lake branch `feat/duckdb-polars-kinds`); this page is the
operator's side of it. Field definitions are in the
[`Lake` CRD reference](../crd-reference.md#lake-separate-crd-cluster-scoped--the-lake-service)
(`spec.services`). Workspace namespaces are reconciled separately from the
[`workspaces` table](workspaces.md). Every Lake runs this services poll; there is no switch ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)). A
customer's **stores** (volumes, the customer's own buckets, and Lake-run buckets and filesystems;
lake#196, lake#664) ride the same poll, connection and Secret: see [Stores](#stores-lake196).

## A service's pods must meet its namespace's Pod Security level

A service created in a workspace runs in that workspace's namespace, which enforces the
`restricted` Pod Security Standard ([`workspaces.md`](workspaces.md)). The API server refuses a pod
that does not meet it, so the upstream operator behind a kind must produce `restricted` pods.

| Kind | How its pods meet `restricted` |
|---|---|
| Postgres | CloudNativePG's pods do, as shipped. |
| SQLite, DuckDB, Polars | The operator runs them and sets the security context itself. |
| Kafka | **The operator writes the security context itself**, on the `KafkaNodePool`'s pod and broker container and on the `Kafka`'s entity operator. Strimzi is also installed with `STRIMZI_POD_SECURITY_PROVIDER_CLASS=restricted` (`helmfile.yaml`, and every cluster's `ClusterHelmRelease`) ([#428](https://github.com/oramasearch/orama-operator/issues/428)). |
| Cassandra | The operator writes `spec.cassandra.podSecurityContext` and a security context on each of the four containers cass-operator creates (`cassandra`, `server-system-logger`, and the init containers `server-config-init-base` and `server-config-init`) ([#435](https://github.com/oramasearch/orama-operator/issues/435)). |
| ClickHouse | The operator writes a pod template (`restricted`) used by every ClickHouse and Keeper pod, running as the image's `clickhouse` user (101). The image is pinned to `25.3`: a pod template must name it, and the Altinity operator's default is `latest`. |
| TiDB | The operator writes `spec.podSecurityContext` (UID 1000) and a container security context on the discovery service, PD, TiKV, TiDB and TiDB's slow-log tailer. |
| Valkey | The operator writes `spec.podSecurityContext`, running as the image's own `valkey` user (999:1000) because the image declares no numeric user, and a security context on the server and metrics exporter containers. |

### What a service costs a workspace's quota

A workspace namespace has a ResourceQuota and a LimitRange that gives any container without
resources a limit of one CPU and 1Gi. **The quota no longer caps the sum of CPU limits**
([#435](https://github.com/oramasearch/orama-operator/issues/435)): a limit is only the ceiling a
container may burst to, and with a default of one CPU a workspace of nine pods using a third of a
CPU between them was reported full at "8 of 8". What a workspace may reserve is capped by
`requests.cpu` (4), and its memory by `requests.memory` (8Gi) and `limits.memory` (32Gi). It may
hold 100 Service objects and 200 Secrets: a Postgres is three Services and a Cassandra five, and at
the earlier limit of 20 a workspace with five services could not create a sixth. Every
upstream-operator kind states its containers' sizes, so none takes the default:

| Kind | Containers | CPU limits charged |
|---|---|---|
| Postgres, Zetta, SQLite, DuckDB, Polars | 1 per instance | 1 per instance (the namespace default) |
| Kafka | broker, plus the entity operator's topic and user operators | **1 for a one-broker Kafka**, stated by the operator: 500m for the broker and 250m for each operator. Before 2026-10-13 nothing was stated, a Kafka cost 3, and the entity operator's pod was refused in a full workspace ([#428](https://github.com/oramasearch/orama-operator/issues/428)). |
| Cassandra | `cassandra` (500m, 2Gi, heap fixed at 1Gi) and the system logger (100m, 128M) | 0.6 |
| ClickHouse | `clickhouse` (500m, 1Gi) per replica; a Keeper (250m, 512Mi) when replicated | 0.5 per replica |
| TiDB | discovery (100m), PD (250m), TiKV (500m, 4Gi, block cache capped at 512MB), TiDB (250m) and its slow-log tailer (50m) | 1.15 per instance |
| Valkey | server (250m, 256Mi) and the exporter (50m, 64Mi) | 0.3 per node |

A service that is not ready says why. Every kind's `service_status.message` carries Kubernetes'
last Warning about the service's objects (a pod the API server refused under Pod Security or a
quota, an image that cannot be pulled, a container that keeps exiting); a Kafka's carries what
Strimzi itself is stuck on first.

## Configuration

Every Lake runs managed services. Every field is optional; empty images use the operator's defaults
(`internal/controller/lake_defaults.go`):

```yaml
spec:
  services:
    maxServices: 20          # 0 = no limit
    zettaImage: oramasearch/zetta:<tag>
    zettaEnv:                # copied to every Zetta service, including the workspace-owned cluster; the
                             # operator adds ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864 and
                             # MALLOC_ARENA_MAX=2 for each name the list does not set
      - { name: ZETTA_MALLOC_TRIM_THRESHOLD_BYTES, value: "134217728" }
      - { name: MALLOC_ARENA_MAX, value: "2" }
    sqliteImage: oramasearch/lake-sqlite:<tag>   # optional; the chart's lakeServices.sqliteImage otherwise
    duckdbImage: oramasearch/lake-duckdb:<tag>   # optional; the chart's lakeServices.duckdbImage otherwise
    polarsImage: oramasearch/lake-polars:<tag>   # optional; the chart's lakeServices.polarsImage otherwise
```

Services need the control-plane Postgres (`spec.postgres.enabled`, the default; without it the
Lake's `FeaturesReady` condition says so); they do not need the Zetta data plane.
`spec.workspaces.maxWorkspaces` does not cap them. For compatibility, `workspaces.image`,
`workspaces.resources` and `workspaces.storage.storageClassName` remain the shared fallbacks for
managed Zetta service pods; set `services.zettaImage` when the images differ.

## The poll

The Lake reconciler polls, once the Lake's schema has applied (the tables come with migration
`0024_services`; `0025_service_catalog` widens `kind` to the catalog, `0026_sqlite_services` adds
`sqlite` and `0027_analytics_services` adds `duckdb` and `polars`), at the Lake's `spec.deploySync.interval` (default `15s`, floor `1s`) — the
cadence the workspace poll used. Each poll (`internal/controller/lake_services.go`):

1. reads Secret `lake-<lake>-controldb-app` key `uri` (the CloudNativePG application Secret of
   the Lake's control DB). A missing Secret or an empty `uri` means "not yet": it is logged and the
   poll ends. It connects with pgx (one connection per poll, closed at the end, 30-second bound on
   the whole pass).
2. reads every service joined to its workspace and status, oldest first, leaving out history
   (`desired = 'absent'` with phase `Deleted`). The joined `w.zetta_service_id` marks the one Zetta
   row this poll must skip.
3. handles **absent** services first, so a deletion frees its quota slot in the same poll;
4. counts the live services for the quota;
5. creates or updates each **present** service;
6. writes the credentials Secret;
7. upserts `service_status` for each service whose phase, message, endpoint or observed
   generation changed. A steady service writes nothing.

Between 5 and 6 the same pass reconciles the customer's stores ([below](#stores-lake196)), so the
Secret is written once with both.

Nothing in the poll fails the Lake reconcile: an unreachable database or a failed Kubernetes call
is logged and retried on the next poll. The store is the Go interface `LakeServiceStore` (read
services, upsert status); `lake_services_pgx.go` is the pgx implementation, and tests substitute
an in-memory fake through `LakeReconciler.ServiceStores`.

### Starting fast (lake#197)

Three delays used to sit between *Create* and a Ready row on lake's Services page, each up to a
poll or more. Each now has a mechanism:

| Delay | Before | Now |
|---|---|---|
| Seeing a new or deleted service | the next poll (up to 15 s) | lake sends `NOTIFY lake_services` when it writes a service; `LakeServicesListener` (`lake_services_notify.go`, a leader-only manager Runnable) keeps one `LISTEN lake_services` connection per services-enabled Lake and sends that Lake to the reconciler through `LakeReconciler.ServiceEvents` (a `source.Channel` watch). The poll remains the fallback for a missed notification |
| Following a starting service | the poll (15 s) | while any service is Provisioning or being deleted, the Lake requeues every 2 s (`lakeServicesChangingPoll`); a settled Lake goes back to the poll |
| lake seeing the credential | kubelet refreshes a mounted Secret only when it syncs the pod, about once a minute | when the services Secret changes, the operator patches `orama.cloud/services-secret-version: <resourceVersion>` onto every lake pod (listed uncached through `LakeReconciler.APIReader`, so no pod informer); an annotation change makes kubelet sync the pod, and its volumes, now. Nothing restarts. RBAC: `pods` `get`, `list`, `patch` |

## Present services

Every service row has a required immutable `workspace_id`. The operator joins it to the
operator-written `workspaces.namespace`; a missing namespace fails closed. The service becomes
**`<lake>-<slug>`** in that workspace namespace, controlled by the Lake, with
labels `orama.cloud/lake=<lake>`, `orama.cloud/service-id=<id>`, `orama.cloud/service-kind=<kind>`,
`orama.cloud/workspace=<workspace_id>`, `orama.cloud/customer-id` (a `sha256-…` digest when the id is not a valid label value) and
`app.kubernetes.io/name=lake-service`, and annotation `orama.cloud/customer-id=<customer_id>`.
For a Zetta service, the reconciler carries the Lake/service/kind/customer identity labels from the
child `OramaZettaCluster` onto its pods without changing the StatefulSet's immutable selector.
`LakeApp` NetworkPolicies use those labels to open workspace and explicit service egress to that
target only. The workspace namespace is default-deny, so the workspace reconciler also writes the
narrow flows each datastore operator needs to reach its pods, and the API egress that CNPG,
Strimzi's entity operator and TiDB discovery need
([workspaces.md](workspaces.md#datastore-operator-flows-operator268), operator#268). This section
describes rows other than the workspace-owned Zetta.

| | Zetta (`kind = 'zetta'`) | Postgres (`kind = 'postgres'`) |
|---|---|---|
| Resource | `OramaZettaCluster` | CloudNativePG `Cluster` (`postgresql.cnpg.io/v1`, unstructured, as `controldb.go` does) |
| From `spec.instances` | `replicas` | `instances` |
| From `spec.storage_gi` | `storage.size` `<n>Gi`, class from `spec.workspaces.storage.storageClassName` | `storage.size` `<n>Gi`, class from `spec.postgres.storageClassName` |
| Other fields | image `spec.services.zettaImage` (then `spec.workspaces.image`), `env` from `spec.services.zettaEnv`, the Lake's `pullSecretName`, `resources` from `spec.workspaces.resources` (default memory `64Mi`/`1Gi`, no CPU request), no ingress, no operator schema | `bootstrap.initdb` database `app`, owner `app` |
| Ready when | status phase `Ready` and `readyNodes` ≥ instances | `status.readyInstances` = instances |
| Credential source | `<cluster>-zetta-admin` key `token` | the whole CloudNativePG `<cluster>-app` Secret |
| `endpoint` | `http://<cluster>-zetta.<ns>.svc.cluster.local:4800` | `<cluster>-rw.<ns>.svc.cluster.local:5432` |

`spec` defaults are `instances: 1` and `storage_gi: 10`; values outside 1–3 and 1–100 fail the
service. The data volume and the Postgres bootstrap are written only when the resource is created:
a StatefulSet's volume template is immutable and the contract has no resize. Only named fields are
written, so server-side defaults never turn a poll into an update.

Changing `spec.services.zettaEnv` updates every Zetta child through its single owner: the workspace
reconciler for `zetta_service_id`, and this services reconciler for all other Zetta rows. The Zetta
controller then changes the StatefulSet pod template and rolls the nodes. Operator-managed
environment names remain authoritative; each child reports ignored collisions through
`status.ignoredEnvVars`.

### Workspace-owned Zetta exclusion

`listServicesSQL` selects `w.zetta_service_id` beside every service row. When a Zetta service ID
matches, the services poll does not create, update, delete or write status for that row, and does
not count its cluster against `spec.services.maxServices`. It does write that Zetta's connection
(slug key and ID alias) into the services Secret, read from the workspace cluster and its admin
Secret once the cluster is Ready, because this poll is the Secret's one writer; the workspace
reconciler reports Ready only once the connection is there
([operator#264](https://github.com/oramasearch/orama-operator/issues/264);
[`workspaces.md`](workspaces.md#connection-delivery-operator264)). The workspace reconciler owns
the child spec, creation-time storage and comparison-gated `service_status` endpoint.

Anything else already holding the name fails the service and is never modified or deleted:

| Case | `Failed` message |
|---|---|
| a cluster of that name that is not this Lake's service (another controller, another Lake or another service id) | `OramaZettaCluster <ns>/<name> exists and is not this Lake's service`, or `CloudNativePG Cluster <ns>/<name> exists and is not this Lake's service` |
| a new Zetta service whose data PVC `data-<cluster>-zetta-<n>` already exists without its cluster | `PersistentVolumeClaim <ns>/<pvc> exists and is not this Lake's service` |
| a malformed row (slug, default on a non-Zetta service, missing customer, a Lake name too long for the kind's derived names, bad spec) | the specific reason, e.g. `spec.instances must be between 1 and 3` |
| a kind this operator does not run | `unknown service kind "<kind>": this operator runs zetta, postgres, kafka, clickhouse, tidb, cassandra, valkey, sqlite, duckdb, polars` |
| a sqlite service whose `spec.instances` is not 1 | `a sqlite service has exactly one instance, because SQLite has one writer; spec.instances is <n>` |
| a duckdb service whose `spec.instances` is not 1 | `a duckdb service has exactly one instance, because one DuckDB process owns its database file; spec.instances is <n>` |

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
  object named `dc1`, and that object is namespaced: a second Cassandra service in the workspace's
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
(`valkey-<name>-2-2` + hash), ClickHouse 26 (`chk-<name>-keeper-0-0` + hash), SQLite, DuckDB and
Polars 11 (the StatefulSet `<name>`'s revision hash, so Zetta's 15 is the bound). Cassandra's derived names come
from the slug alone. With a 16-character slug, ClickHouse allows Lake names up to 20
characters and every other kind at least 24.

## SQLite (decision 47, amended)

SQLite has no upstream operator, so the Orama operator runs it directly behind
**[lake-sqlite](lake-sqlite.md)**, a small web service in its own private repository (`oramasearch/lake-sqlite`,
image `oramasearch/lake-sqlite`) over one SQLite database. The driver is
[`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) — pure Go, no cgo, **BSD-3-Clause**. The
kind goes through the catalog machinery (`internal/controller/lake_service_wrapper.go`, with
SQLite's values in `lake_service_sqlite.go`), so
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

## DuckDB and Polars (lake#195)

DuckDB and Polars have no upstream operator we would use either (lake `/lake-service` skill, step
1: the commercial Polars On-Prem product and its operator are excluded by their terms), so the
Orama operator runs each behind a small web service of ours, in its own private repository and
private image, exactly as it runs lake-sqlite:

- **[lake-duckdb](lake-duckdb.md)** (`oramasearch/lake-duckdb`), *powered by DuckDB*: lake-sqlite's
  API over one DuckDB file, in Go with `github.com/duckdb/duckdb-go/v2` (MIT; DuckDB 1.5.5, MIT,
  linked statically). The engine is locked down before it answers: `enable_external_access=false`,
  no extension install or autoload, no community extensions, memory and thread limits, then
  `lock_configuration=true`; the request timeout interrupts a statement.
- **[lake-polars](lake-polars.md)** (`oramasearch/lake-polars`), *powered by Polars*: analytics
  compute, in Rust with the open-source `polars` crate (MIT). `POST /v1/datasets/<name>` uploads
  CSV, JSONL or Parquet (stored as Parquet), `GET /v1/datasets` lists them, `DELETE
  /v1/datasets/<name>` removes one, and `POST /v1/query` `{sql}` runs Polars SQL over them
  (`{columns, rows}`, 10,000-row cap, `truncated`). Table functions that read files are refused
  before Polars sees the query.

Both go through the same wrapper machinery as SQLite (`internal/controller/lake_service_wrapper.go`;
their values in `lake_service_duckdb.go` and `lake_service_polars.go`): the same ownership, labels,
refusal of foreign objects and Secrets, quota, old-volume check, status, deletion order, pod
security and applied-hash template writes. What differs:

| | DuckDB | Polars |
|---|---|---|
| Resources written | `StatefulSet` `<name>`, ClusterIP `Service` `<name>` (8080 → `http`), Secret `<name>-credentials` | the same |
| `instances` → | must be 1 (the default); anything else fails the service — one DuckDB process owns its file | the StatefulSet's `replicas` (1–3): workers sharing nothing, each with its own volume, behind the one Service. Datasets are per worker until shared storage exists (lake#196), so 1 is the useful value. A StatefulSet, not a Deployment, so a volume stays with its pod |
| `storage_gi` → | `volumeClaimTemplates` `data` (`ReadWriteOnce`), mounted at `/data`; the database is `/data/main.duckdb` (WAL and spill files beside it) | the same claim template, one `data-<name>-<i>` per worker, at `/data`; each dataset is `/data/<name>.parquet` |
| Pod | container `lake-duckdb`, image `spec.services.duckdbImage`, then `--lake-duckdb-image` (the chart's `lakeServices.duckdbImage`), then `oramasearch/lake-duckdb:0.1.0`; requests `50m` CPU, `256Mi` memory, limit `2Gi`, handed over as `LAKE_DUCKDB_MEMORY_LIMIT` (DuckDB's `memory_limit` is half of it, the Go heap a quarter); `LAKE_DUCKDB_THREADS=2` (no CPU limit, so DuckDB would otherwise start a thread per node CPU); `LAKE_DUCKDB_TOKEN` from the Secret | container `lake-polars`, image `spec.services.polarsImage`, then `--lake-polars-image` (`lakeServices.polarsImage`), then `oramasearch/lake-polars:0.1.0`; requests `50m` CPU, `256Mi` memory, limit `2Gi`, handed over as `LAKE_POLARS_MEMORY_LIMIT` (an upload is capped at a quarter of it); `LAKE_POLARS_THREADS=2`; `TMPDIR=/tmp`; `LAKE_POLARS_ADDR=0.0.0.0:8080`; `LAKE_POLARS_TOKEN` from the Secret |
| Both | the Lake's `pullSecretName`; startup, readiness and liveness probes on `GET /healthz`; uid/gid/fsGroup `65532`, `runAsNonRoot`, read-only root filesystem, no privilege escalation, every capability dropped, `RuntimeDefault` seccomp, no service-account token; a `1Gi` `emptyDir` at `/tmp`; `terminationGracePeriodSeconds` 45 | |
| Ready when | `status.readyReplicas` is 1 (`/healthz` runs `SELECT 1`) and the credential is written | `status.readyReplicas` equals `instances` (every worker's `/healthz` lists and writes its directory) and the credential is written |
| Credential | `{"kind":"duckdb","endpoint":…,"token":…,"database":"main"}` | `{"kind":"polars","endpoint":…,"token":…}` |
| `endpoint` | `http://<name>.<ns>.svc.cluster.local:8080` | the same |
| PVCs deleted after the StatefulSet, selected by | `app.kubernetes.io/name=lake-duckdb`, `app.kubernetes.io/instance=<name>`, `orama.cloud/service-id=<id>` (`data-<name>-0`) | `app.kubernetes.io/name=lake-polars`, the same instance and service id (every `data-<name>-<i>`, including a worker's claim left behind after `instances` went down) |

Changing a Polars service's `instances` scales the StatefulSet in place (the claim template is
immutable, `storage_gi` never resizes). Scaling down keeps the removed workers' claims, and their
datasets, until the service is deleted or scaled back up. A new image rolls every pod of that kind
once, as for SQLite.

**Where this departs from the contract or adds to it:** the same as SQLite (the ClusterIP Service is
the StatefulSet's governing Service; the token is key `token` of `<name>-credentials`). lake-duckdb's
`/v1/exec` answers `last_insert_id: null` (DuckDB has no rowid to report), binds a JSON boolean as
`BOOLEAN` rather than 1/0, and adds `403 forbidden` for a refused file, extension or network access
and `409 conflict` for a transaction conflict. The operator does not bound CPU (neither does it for
SQLite); both binaries bound their own thread pools to 2.

## Credentials

Secret **`lake-<lake>-services`** (controlled by the Lake) holds one key per Ready service,
**`<slug>.json`**:

```json
{"kind":"zetta","endpoint":"http://prod-svc-0123456789ab-zetta.prod-ws-123e4567e89b.svc.cluster.local:4800","token":"<admin token>"}
{"kind":"postgres","host":"prod-svc-0123456789ab-rw.prod-ws-123e4567e89b.svc.cluster.local","port":5432,"database":"app","user":"app","password":"…","uri":"postgresql://app:…@prod-svc-0123456789ab-rw.prod-ws-123e4567e89b:5432/app"}
{"kind":"kafka","endpoint":"prod-svc-0123456789ab-kafka-bootstrap.prod-ws-123e4567e89b.svc.cluster.local:9092","bootstrap":"prod-svc-0123456789ab-kafka-bootstrap.prod-ws-123e4567e89b.svc.cluster.local:9092","username":"prod-svc-0123456789ab-app","password":"…","sasl_mechanism":"SCRAM-SHA-512","security_protocol":"SASL_PLAINTEXT"}
{"kind":"clickhouse","endpoint":"clickhouse-prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local:8123","host":"clickhouse-prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local","http_port":8123,"native_port":9000,"user":"app","password":"…","database":"default"}
{"kind":"tidb","endpoint":"prod-svc-0123456789ab-tidb.prod-ws-123e4567e89b.svc.cluster.local:4000","host":"prod-svc-0123456789ab-tidb.prod-ws-123e4567e89b.svc.cluster.local","port":4000,"user":"root","password":"…","uri":"mysql://root:…@prod-svc-0123456789ab-tidb.prod-ws-123e4567e89b.svc.cluster.local:4000"}
{"kind":"cassandra","endpoint":"svc-0123456789ab-svc-0123456789ab-service.prod-ws-123e4567e89b.svc.cluster.local:9042","host":"svc-0123456789ab-svc-0123456789ab-service.prod-ws-123e4567e89b.svc.cluster.local","port":9042,"datacenter":"dc1","user":"svc-0123456789ab-superuser","password":"…"}
{"kind":"valkey","endpoint":"valkey-prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local:6379","host":"valkey-prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local","port":6379,"user":"app","password":"…","cluster":true}
{"kind":"sqlite","endpoint":"http://prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"<64 hex>","database":"main"}
{"kind":"duckdb","endpoint":"http://prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"<64 hex>","database":"main"}
{"kind":"polars","endpoint":"http://prod-svc-0123456789ab.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"<64 hex>"}
```

`uri` is CloudNativePG's own, copied unchanged; TiDB's is built from the host and the generated
password. A present service keeps its key while its
connection cannot be read, so a transient failure never removes a file lake depends on; the key
of an absent service, or of a row that no longer exists, is dropped.

For [Apps](apps.md), every present service with a connection also has a key equal to its service
ID in this same Secret. It is an exact alias of `<slug>.json`, retained across temporary credential
read failures and removed on withdrawal or row removal. Store connections keep only their existing
`<slug>.json` keys. Apps resolve same-workspace service IDs and mount only requested connections,
using the service name as the filename. Existing Lake consumers continue using slug keys.

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
| `Provisioning` | the resource exists and is not ready | `waiting for the Zetta nodes` / `waiting for the Postgres instances` / `waiting for the Kafka cluster` / `waiting for the ClickHouse installation` / `waiting for the ClickHouse Keeper` / `waiting for TiDB <pd\|tikv\|tidb>` / `waiting for the TiDB root password` / `waiting for the Cassandra datacenter` / `waiting for the Valkey cluster` / `waiting for the lake-sqlite pod` / `waiting for the lake-duckdb pod` / `waiting for the lake-polars pods` |
| `Provisioning` | the resource is ready but its credential is not yet in the Secret | `delivering the connection` |
| `Provisioning` | a previous resource or data volume of that name is still terminating | `waiting for the previous … to be deleted` |
| `Ready` | the resource is ready **and** its credential was written in this poll | `service ready` |
| `Failed` | ownership, validation, a missing upstream operator or the quota refused the service | see above; quota: exactly `service quota reached (<n>)`; `the <kind> operator is not installed on this cluster` |
| `Deleting` | an absent service's resources are still going | `deleting the Zetta cluster`, `deleting the Zetta data volumes`, `deleting the Postgres cluster`, `deleting the Postgres data volumes`, and for the catalog `deleting the <Kafka\|ClickHouse\|TiDB\|Cassandra\|Valkey\|SQLite\|DuckDB\|Polars> cluster`, then `… data volumes` |
| `Deleted` | nothing remains | `service deleted` |

`endpoint` is written once the resource exists and is this service's (it is empty on `Failed` and
`Deleted`, and kept while `Deleting`); that is also the proof the deletion path uses for leftover
volumes. `observed_generation` is the `generation` of the row the phase describes.

## Absent services (deletion)

**Deleting a service deletes its data, irreversibly.** For each absent service not yet `Deleted`:

- **Zetta:** check ownership; delete the data PVCs `data-<cluster>-zetta-<n>`, **then** the cluster
  (background propagation). If the cluster is already gone, a leftover live PVC is deleted only
  when the service's status carries an endpoint.
- **Postgres:** check ownership; delete the CloudNativePG `Cluster`. Its PVCs (label
  `cnpg.io/cluster=<cluster>`) are owned by it and garbage-collected.
- **Catalog kinds, SQLite, DuckDB and Polars:** check the ownership of every object the kind can
  write, and delete them all in one pass, primary first (`Kafka`, `KafkaNodePool`, `KafkaUser`;
  `ClickHouseInstallation`, `ClickHouseKeeperInstallation`; a wrapper kind's `StatefulSet`, then its
  `Service`), with background propagation. Only once every one is gone — so nothing can recreate a
  claim — delete the PVCs matching the kind's selector above (for SQLite and DuckDB,
  `data-<name>-0`; for Polars every worker's `data-<name>-<i>`; and only when they carry the
  StatefulSet's labels); only once those are gone too, delete the generated
  password or token Secret (when this service owns it), so the credential is the last thing to go.
  A PVC is deleted only when the service's status carries an endpoint, which the operator writes
  for a resource it owns; the first deletion pass writes it too. A kind whose operator is not
  installed has nothing to delete but its leftover PVCs.
- remove `<slug>.json` from `lake-<lake>-services`;
- write `Deleted` only once the resource and every data PVC are gone; until then `Deleting`, and
  the next poll repeats the steps.

A resource that fails the ownership rule is never deleted; the service is `Failed` instead.

## Quota

`spec.services.maxServices` (`0` = no limit) caps
the live services: Lake-controlled `OramaZettaCluster`s, CloudNativePG `Cluster`s and the primary
resource of each catalog kind (`Kafka`, `ClickHouseInstallation`, `TidbCluster`,
`K8ssandraCluster`, `ValkeyCluster`, and the `StatefulSet` of SQLite, DuckDB and Polars; a kind that is not installed counts
none) labelled
`orama.cloud/lake=<lake>` with a service-id, not being
deleted and not absent in the current read. A **new** service over the cap creates nothing and is
`Failed` / `service quota reached (<n>)`; it starts on a later poll once there is room. An existing
service is never refused or removed, even after the cap is lowered. The workspace-owned Zetta is
excluded from this count; `spec.workspaces.maxWorkspaces` governs workspace admission. lake's own
per-customer cap (`LAKE_SERVICES_PER_CUSTOMER`) is separate.

## Other ways a service goes away

Disabling services stops the poll and removes the mount and env; it deletes nothing. Deleting the
Lake deletes every Lake-controlled `OramaZettaCluster` through the Lake finalizer and
garbage-collects the CloudNativePG Clusters, the catalog kinds' upstream resources, the
StatefulSets and Services of SQLite, DuckDB and Polars, and the Secrets; Zetta data PVCs, the wrapper
kinds' `data-<name>-<i>`, and the PVCs
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
manager to start. SQLite, DuckDB and Polars need nothing new: the manager already manages `statefulsets`,
`services`, `secrets` and `persistentvolumeclaims`.

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
  The same holds for DuckDB, and for each Polars worker's datasets.
- **Polars datasets are per worker** until shared storage (lake#196): with `instances` above 1 an
  upload lands on one worker and a query on any, so 1 is the only useful value.
- The upstream operators themselves (and cert-manager, which K8ssandra needs) are installed by the
  `helmfile.yaml`; until they are, each kind's services report the not-installed message.
- The catalog has been exercised against the fake client and validated against the pinned CRD
  schemas only, not yet against the running upstream operators. The SQLite kind has been exercised
  against the fake client only; the lake-sqlite image has not yet been built or run on a cluster.
  The DuckDB and Polars kinds have been exercised against the fake client only; their images
  (`oramasearch/lake-duckdb:0.1.0`, `oramasearch/lake-polars:0.1.0`) must be built and pushed
  (private) before either kind can start.

## Stores (lake#196)

Migration `0028_stores` adds lake's `stores` / `store_status` tables (lake
`docs/control-plane/storage.md`). The services poll reads them on the same connection, after the
services, and `LakeServiceStore` carries `ListStores` and `UpsertStoreStatus` beside the service
methods. It runs the **cluster volume** (a Lake-controlled PersistentVolumeClaim
`<lake>-<st-slug>` labelled `orama.cloud/store-id`, on the services' StorageClass, Ready as soon as
the claim exists and its `<slug>.json` connection is in `lake-<lake>-services`), the customer's own
**buckets** (`r2`, `s3`, `b2`, `gcs`, `azure`: a `LakeSealedSecret` delivering the workspace's
credential, lake#664) and the **Lake-run** pairs (`zetta`, `lake`: a connection only); the network
filesystems are `Failed`. The wrapper kinds (`sqlite`, `duckdb`, `polars`) mount the stores lake's
`store_mounts` names for them (lake#666): a volume at its path, a bucket as `STORE_<NAME>_*`
variables, both in the StatefulSet's pod template, so a change rolls the pods; the poll also
reports each mounted volume's used space. A schema without the `stores` table is logged and skipped; the services
carry on. The Secret keeps a store key by the same present-set rule as a service key, and keeps
every `st-…` key when the stores read failed. Absent and provisioning stores keep the 2-second
poll. The whole of it is [stores.md](stores.md).

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
kubectl -n prod-ws-123e4567e89b get statefulset,service,pvc -l 'orama.cloud/service-kind in (duckdb,polars)'
kubectl -n lake get secret lake-prod-services -o jsonpath='{.data}' | jq 'keys'
kubectl -n lake exec deploy/lake-prod -- ls /var/run/lake/services
```

The unit tests (`lake_services_test.go`) cover Zetta and Postgres creation, Ready gated on the
credential, exclusion of a workspace-owned Zetta while other services continue, refusal of foreign resources and
unproven volumes, the deletion order and `Deleted`, the quota message and the fact that it never
refuses existing services, status upserts only on change, the "not yet" app Secret, and the lake
Deployment's mount and env. `lake_service_catalog_test.go` runs every catalog kind, SQLite, DuckDB and Polars included, through: the
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
and the quota. `lake_service_analytics_test.go` adds DuckDB's and Polars': the StatefulSet (one
pod, or `instances` workers), pod, probes, resources, security context, env, Service and token;
DuckDB's `instances` other than 1 failing; Polars scaling with `instances` and Ready only when
every worker is; every worker's claim deleted with the service; the image order; and an image
change rewriting the template once. The pgx test also shows the vendored schema
(`0025_service_catalog`, `0026_sqlite_services` and `0027_analytics_services`) accepts a row of
every catalog kind, `sqlite`, `duckdb` and `polars`, which reads back as that kind, and still
refuses any other.

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
- **A new wrapper kind** (a service of ours rather than an upstream operator's): a
  `wrapperServiceKind` value in its own `lake_service_<kind>.go`, registered like a catalog kind,
  plus its image in `serviceImage`, a Lake CRD field, a flag and a chart value.
- **lake-sqlite, lake-duckdb, lake-polars themselves**: see [lake-sqlite.md](lake-sqlite.md),
  [lake-duckdb.md](lake-duckdb.md) and [lake-polars.md](lake-polars.md); their APIs are part of the
  contract.
- **Schema**: the tables arrive with lake's `db/schema.sql`; see [schema.md](schema.md).
