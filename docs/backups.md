# Backups: one system for every datastore

[operator#453](https://github.com/oramasearch/orama-operator/issues/453). Every datastore the
operator runs is backed up through **one backup system in the operator**, with one shape for every
datastore, to a bucket the operator provisions itself. Steve, 2026-10-13: *"build an in-operator,
native backup system for each store"*, *"create a backup schedule crd and a backup crd"*, *"the
operator can spin up its own job for each backup type (still use the operator image, but different
invocation method)"* and *"we should be using the cloudflare s3 bucket cr too"*.

What backs up today, per datastore, and the research behind each mechanism, are in
[`research/backups/`](research/backups/README.md).

## What is built, and what is not

| Part | State |
|---|---|
| `BackupSchedule` and `Backup` kinds, their controllers, retention, status | **Implemented**, unit- and envtest-tested. Not yet applied to any cluster. |
| Backup Jobs from the operator's own image (`/manager backup ...`) behind one Go interface | **Implemented.** |
| `CloudflareR2Bucket.spec.deletionPolicy` (`Retain` keeps the bucket) | **Implemented.** Required, no default. |
| `CloudflareR2Bucket.spec.credentials` (a token scoped to the bucket, its S3 keys in a Secret) | **Implemented.** Needs an account token that may create API tokens (below). |
| **ClickHouse** mechanism (`BACKUP ... TO S3`) | **Implemented.** The first datastore with a mechanism. |
| **Postgres** mechanism (Lake's control plane, Cloud v1's control DB, Lake `postgres` services) through CloudNativePG's plugin interface and the **Barman Cloud Plugin** ([#731](https://github.com/oramasearch/orama-operator/issues/731), [Postgres](#postgres)) | **Implemented.** Needs the plugin's chart on every cluster (not installed: [Platform](#platform)). Lake's control plane is off `barmanObjectStore`. |
| Zetta, Kafka, Cassandra, TiDB, Valkey, SQLite, DuckDB, Polars | See the [Mechanisms](#mechanisms) table. |
| Owners creating schedules (a Lake for its services, an `OramaCloudV1` for its datastores) and showing a late or failed backup on themselves | **Implemented** ([#740](https://github.com/oramasearch/orama-operator/issues/740), [Owners](#owners)). Not yet applied to any cluster. |
| The datastore's own network access during a run (accepting the run's pod; egress to the bucket for a server that writes it itself) | **Implemented** ([#740](https://github.com/oramasearch/orama-operator/issues/740), [How one backup runs](#how-one-backup-runs)). |
| `BackupRestore` and `RestoreRehearsal` kinds, `/manager restore`, the backup manifest and verification against it | **Implemented** ([#741](https://github.com/oramasearch/orama-operator/issues/741), [Restore](#restore)). Not yet applied to any cluster. |
| ClickHouse restore (`RESTORE ... FROM S3`, row counts per table before and after the snapshot) | **Implemented** ([#741](https://github.com/oramasearch/orama-operator/issues/741)). |
| Usage metering of backup storage | Not built; [#435](https://github.com/oramasearch/orama-operator/issues/435). |

## The kinds

**`BackupSchedule`** (namespaced, short name `bsched`) says which datastore, which bucket, when, and
how many to keep. On each cron tick (UTC, default `0 3 * * *`) it creates a `Backup`
`<schedule>-<UTC timestamp>`, and it deletes Backups past retention.

- **Timing comes from the newest Backup it made** (annotation `orama.cloud/scheduled-at`), so an
  operator restart neither skips nor repeats a tick. Missed ticks become **one** backup, for the
  latest tick, not a backlog.
- **One at a time.** No new Backup starts while one is still `Pending` or `Running`.
- **Retention** (`keep`, default 7): only `Succeeded` Backups count, newest first, so the newest
  succeeded backup is never deleted. `Failed` Backups are kept to the same number, for diagnosis.
  A running Backup is never deleted.
- **Backups outlive the schedule and the datastore.** They are labelled
  (`orama.cloud/backup-schedule`, `orama.cloud/backup-datastore`), not owned, so deleting either
  keeps them.
- `suspend: true` stops new backups and deletes none.

**`Backup`** (namespaced, short name `bkp`) is one backup. Its spec is immutable. Creating one by
hand takes a backup now. Deleting one deletes its objects from the bucket (its finalizer lists and
deletes `<path>/<name>/`); if the bucket resource is gone, the objects stay and the Backup is
released.

Both live in the **datastore's namespace** (for a Lake service, its workspace namespace). The full
field list is in [`crd-reference.md`](crd-reference.md#backupschedule-separate-crd-namespaced--backups-of-one-datastore-on-a-schedule).

## How one backup runs

The Backup controller creates, in the Backup's namespace, all owned by the Backup:

1. **`<job>-bucket`**, a copy of the bucket's scoped credentials (`accessKeyId`,
   `secretAccessKey`, `endpoint`, `bucket`, `region`). Never the account token.
2. **A `NetworkPolicy`** selecting the backup pod: no ingress; egress only to DNS, to pods in its
   own namespace (the datastore), and to public addresses on 443 (the object store), with private,
   link-local and carrier-grade ranges excepted.
3. **A `Job`** from the **operator's own image** (read from the manager pod, with its pull secrets
   copied into the namespace), running `/manager backup --datastore <kind> --backup <name>
   --prefix <path>/<name>`. One image and one version to build, sign, scan and pin, and a backup
   always runs the code of the operator that scheduled it. The pod runs at the `restricted` Pod
   Security level (non-root UID 65532, no privilege escalation, every capability dropped, read-only
   root filesystem, `RuntimeDefault` seccomp), **with no service account token**, `backoffLimit: 0`,
   a six-hour deadline, and is kept seven days after it finishes.

The Job reads the datastore's connection (`spec.datastore.connectionSecretRef`: `host`, `username`,
`password`, optional `port` and `database`) and the bucket copy as environment variables. It selects
the mechanism for the datastore kind through one Go interface, `backup.Mechanism`
(`internal/backup/`), and **reports through its termination message**: a JSON document with the
outcome, the size and a message in the words of whatever refused. The controller writes that to the
Backup's status. The issue's design had the Job write the status itself; reading the termination
message instead means the backup pod holds no Kubernetes credential at all.

4. **A second `NetworkPolicy`, `<job>-ds`, on the datastore's pods** (`spec.datastore.podSelector`,
   or the kind's default selector): they accept connections from the backup pod, which the
   namespace's default-deny would refuse, and, for a kind whose server writes the backup to the
   bucket itself (ClickHouse's `BACKUP ... TO S3`, TiDB's BR on TiKV, Cassandra's Medusa), they may
   reach public addresses on 443 with the same exceptions. It exists only while the run does
   ([#740](https://github.com/oramasearch/orama-operator/issues/740)).

When the Backup has succeeded or failed, the credentials copy and both network policies are deleted.

### Status

| Resource | Says |
|---|---|
| `Backup` | `phase` (`Pending`, `Running`, `Succeeded`, `Failed`), `location` (`s3://<bucket>/<path>/<name>/`), `startTime`, `completionTime`, `sizeBytes`, `message`, and condition `Complete` with the reason it is waiting or failed: `BucketNotFound`, `BucketHasNoCredentials`, `BucketNotAllowed`, `BucketNotReady`, `OperatorImageUnknown`, `MechanismNotAvailable`, `BackupFailed`, `JobFailed`, `NoReport`, `JobLost`. |
| `BackupSchedule` | `lastScheduleTime`, `nextScheduleTime`, `lastSuccessfulBackup`, `lastSuccessTime`, and conditions `Ready` (`Scheduled`, `Suspended`, `InvalidSchedule`, `MechanismNotAvailable`), `LastBackupSucceeded` (the newest finished Backup's outcome and message) and `OnSchedule` (`False`, `BackupLate`, once a whole tick has passed after the tick that should have followed the last success). |

Status is written only when it changes; a steady schedule causes no writes. The schedule wakes for
its next tick and for the moment it would turn late.

## Postgres

[#731](https://github.com/oramasearch/orama-operator/issues/731). **Decided under Steve's 2026-11-14
delegation: the Barman Cloud Plugin** (`cloudnative-pg/plugin-barman-cloud`), because it is
CloudNativePG's own maintained replacement for the in-tree `barmanObjectStore` that 1.31.0 removes,
it reads every backup taken the old way (the same Barman layout), and our own CNPG-I plugin on
pgBackRest or WAL-G would be a new repository, image and maintenance burden for the same result. The
plugin is Apache-2.0; it runs Barman (GPL-3.0) in its own sidecar image, which we ship unmodified. A
distribution to self-hosting customers carries Barman's licence text, notices and a pointer to its
corresponding source; nothing of ours links to it.

- **The schedule owns the archive.** A Postgres `BackupSchedule` writes, owned by it: a copy of the
  bucket's scoped keys `<schedule>-archive-s3`, an `ObjectStore` `<schedule>-archive` pointing at
  `s3://<bucket>/<path>/` (gzip WAL and data, `retentionPolicy` = `keep` ticks of its cron in whole
  days), and a `NetworkPolicy` `<schedule>-archive-egress` letting the Cluster's pods (`cnpg.io/cluster`)
  reach public HTTPS, because the sidecar ships each WAL segment as it is written. Without the
  plugin on the cluster the schedule is `Ready=False`, `ArchivePluginNotInstalled`.
- **The owner names the archive in its Cluster**, in `spec.plugins` (`barman-cloud.cloudnative-pg.io`,
  `isWALArchiver: true`, `barmanObjectName: <schedule>-archive`), once the plugin is installed: a
  Lake's `postgres` services (with `spec.backups`), an `OramaCloudV1`'s control DB (with its backup
  bucket). Any other plugin entry is left alone. Adding the plugin restarts the instances once,
  which is how CloudNativePG adds a sidecar.
- **A `Backup` is a CloudNativePG `Backup`** (`method: plugin`), owned by it. The Backup succeeds
  when CloudNativePG says `completed`, records the backup id, the archive and the server name in
  annotations (`orama.cloud/postgres-*`), and its location is
  `s3://<bucket>/<path>/<cluster>/base/<id>/`. It waits while the Cluster does not archive through the
  plugin. Deleting it deletes the CNPG `Backup`, never Barman's files: Barman's retention owns them.
- **A `BackupRestore` creates `target.name`**, a new one-instance Cluster bootstrapped by
  `recovery` from that backup id (`targetImmediate`) through an external cluster that reads the same
  archive, with no `spec.plugins`, so it never writes into the source's archive. It refuses an
  existing Cluster. It is verified when CloudNativePG has read back and replayed the base backup and
  every WAL segment to its end and the instance is ready; there is no row-level comparison for
  Postgres (a server that keeps writing has no count to compare with). A scratch one is deleted
  afterwards.
- **Lake's control plane** keeps `spec.postgres.backup`: the Lake writes `lake-<name>-controldb-archive`
  from it, names it in the Cluster, and its `ScheduledBackup` uses `method: plugin`
  ([`lake/backups.md`](lake/backups.md)). `LakeRestoreRehearsal` reads the archive through an
  `ObjectStore` in its scratch namespace.

## Kafka

[#733](https://github.com/oramasearch/orama-operator/issues/733). **Decided under Steve's 2026-11-14
delegation: no backup, stated on the kind.** Kafka has no snapshot. The alternative, MirrorMaker 2 to
another cluster, is replication rather than backup: it copies deletes, compactions and bad writes as
faithfully as good ones, and it needs a second Kafka per Kafka. A Lake keeps durable data in its
workspace Zetta, and a Kafka service's durability is its own replication (three brokers,
`min.insync.replicas` 2). So a `BackupSchedule` or `Backup` of kind `Kafka` is refused with reason
`NoBackupByDecision` and the decision in its message (`backup.Refusals`), never run. A Lake writes a
schedule for each Kafka service anyway, so the decision is said on the service's `service_status`
message; it does not count against the Lake's `BackupsOnSchedule`.

## Restore

[#741](https://github.com/oramasearch/orama-operator/issues/741). *A backup that has never been
restored is not counted as working* (#453), so restoring is part of the system, with one shape for
every kind.

**The manifest.** Every backup a Job takes ends by writing `<path>/<backup>/orama-backup.json`: the
kind, the Backup, when it completed, its size, and a **summary** of what the mechanism counted (for
ClickHouse, the rows of every table, counted before and after the snapshot, so a server that keeps
taking writes still gives a range the restored count must fall in). A backup whose manifest cannot be
written is a failed backup.

**`BackupRestore`** (short name `brestore`, spec immutable) restores one succeeded `Backup` of its
namespace into `spec.target`, a datastore of the same kind:

- It refuses the datastore the backup was taken of unless `spec.replaceSource` is set (recovery in
  place), and never restores over content: the mechanism refuses a target that already has any of
  the backup's tables, keys or files, and a kind whose restore creates the datastore creates
  `target.name`, which must not exist yet.
- It runs `/manager restore --datastore <kind> --restore <name> --prefix <path>/<backup>
  --verify=<bool> --cleanup=<scratch>` as a Job exactly like a backup's (restricted, no service
  account token, the bucket copy `<job>-bucket`, the run's policy and the target's `<job>-ds`, all
  owned by the restore and deleted when it finishes), or, for a kind whose own operator restores,
  that operator's resources.
- **Verification** (`spec.verify`, default true) reads the manifest, counts the same things in the
  restored datastore and passes only if every one is present and within its recorded range, and
  nothing else appeared. `status.verified` and `status.evidence` say what was compared;
  `status.phase` is `Succeeded` only when restored and, when asked, verified.
- `spec.scratch` marks a rehearsal target: afterwards, what was restored is removed (the restored
  ClickHouse databases, never `system` or `default`), so the next rehearsal finds it empty.

**`RestoreRehearsal`** (short name `rrehearsal`) does that on a cron schedule (UTC, default weekly,
Sunday 05:00): each tick creates a `BackupRestore` of the newest succeeded `Backup` of
`spec.backupScheduleName` into `spec.target` with `scratch: true`, owned by the rehearsal, and keeps
the newest `keep` (default 4) finished ones. It refuses a target that is the schedule's own
datastore or of another kind. Its timing comes from its newest restore, like a schedule's. Status:
`lastSuccessfulRestore`, `lastSuccessTime`, and conditions `Ready` (`Scheduled`, `Suspended`,
`InvalidSchedule`, `Refused`), `LastRehearsalSucceeded` (`RestoredAndVerified`, or the restore's
reason) and `OnSchedule` (`False`, `RehearsalLate`, once a whole tick has passed without a verified
restore).

`LakeRestoreRehearsal` stays as it is: it rehearses a Lake's control database and store from their
CNPG backups and CSI snapshots and writes the Lake's data-service evidence
([`lake/restore-rehearsals.md`](lake/restore-rehearsals.md)). The new kinds rehearse anything the
backup system backs up.

## The bucket

The destination is a **`CloudflareR2Bucket`** the operator provisions. Two things were added for
backups:

- **`spec.deletionPolicy`**, required, no default, like `KubernetesCluster`'s. `Retain` keeps the
  bucket and every object in it when the resource is deleted, and re-creating the resource adopts
  the existing bucket. `Delete` deletes the bucket with the resource, as every R2 bucket did before.
  Backup buckets use `Retain`. The operator's own Cloud v1 index-builder bucket
  (`<oc>-legacy-indexes`) is written with `Delete`, its behaviour until now.
- **`spec.credentials`**. The operator creates an account-owned API token
  (`POST /accounts/{account}/tokens`) whose one policy grants **Workers R2 Storage Bucket Item
  Write** (looked up by name in the account's permission groups) on
  `com.cloudflare.edge.r2.bucket.<account>_default_<bucket>` only, and writes the S3 credentials R2
  derives from it to `spec.credentials.secretName`, owned by the bucket resource: access key id =
  the token id, secret = SHA-256 of the token value
  ([Cloudflare](https://developers.cloudflare.com/r2/api/tokens/)). The token value is shown once,
  so a lost Secret means a new token, and the old one is revoked first. Removing `spec.credentials`,
  or deleting the resource, revokes the token whatever the deletion policy.
  `spec.credentials.allowedNamespaces` lists the other namespaces whose backups may use the
  bucket; the operator copies the scoped Secret into a backup's namespace only then.

**What the account token needs.** Creating the bucket needs *Workers R2 Storage: Edit* (as before).
`spec.credentials` also needs the token to be allowed to create account API tokens (*Account API
Tokens: Edit*) and to grant the R2 bucket permission group. Without it the bucket reports
`Ready=false` with Cloudflare's refusal. Issuing or widening that token is Steve's.

A second provider (Azure Blob through the cluster's identity, S3) would be another concrete bucket
kind named by `spec.bucket.kind`; only `CloudflareR2Bucket` exists.

## Mechanisms

| Datastore kind | Mechanism | Tool in the operator image | State |
|---|---|---|---|
| `ClickHouse` | The server's own `BACKUP ALL EXCEPT DATABASES system, information_schema, INFORMATION_SCHEMA TO S3(...)` (or `BACKUP DATABASE <database>` when the connection names one), sent over the HTTP interface (default port 8123) as the request body, never the URL. The server writes a consistent per-table snapshot straight to the bucket; `RESTORE ... FROM S3(...)` reads it back. Size from `system.backups`. The bucket secret is redacted from any reported error. | None (SQL over HTTP from Go). ClickHouse is Apache-2.0. | Implemented. The ClickHouse server needs egress to the bucket endpoint, which its own workspace policy must allow (filed). |
| `Postgres` | Continuous WAL archiving by the Barman Cloud Plugin's sidecar beside every instance, to the `ObjectStore` the schedule owns, plus a CloudNativePG base backup (`Backup`, `method: plugin`) per tick. Restore bootstraps a **new** Cluster from the base backup and the WAL up to its consistent point. | None in the operator image: the plugin's own sidecar (Barman, GPL-3.0, unmodified). | Implemented ([#731](https://github.com/oramasearch/orama-operator/issues/731)). |
| `Valkey` | A point-in-time RDB of every primary (`CLUSTER SHARDS`; a server without cluster mode is its own primary) over the replication protocol (`PSYNC ? -1`), streamed as a multipart upload to `shard-<n>.rdb` while its keys are counted per database. Restore: into a server or cluster with no key, every key written with `RESTORE ... ABSTTL` (the value's own RDB encoding as the DUMP payload) to the primary owning its slot, so the topology may differ; keys expired or expiring within a minute are counted, not written; each file's SHA-256 is checked. Verified: keys per database. A scratch target is emptied with `FLUSHALL`. | None: RESP and the RDB format from Go (`internal/backup/valkey.go`, `rdb.go`). Valkey is BSD-3-Clause. | Implemented ([#736](https://github.com/oramasearch/orama-operator/issues/736)). Module values and value types the reader does not know are refused with the key's name. |
| `Kafka` | None, by decision ([Kafka](#kafka)): refused with `NoBackupByDecision`. | — | Decided ([#733](https://github.com/oramasearch/orama-operator/issues/733)). |
| Every other kind | See the research. | — | Not built. |

## An example

```yaml
apiVersion: orama.cloud/v1alpha1
kind: CloudflareR2Bucket
metadata: {name: lake-backups, namespace: lake}
spec:
  secretRef: cloudflare-api-token      # the account token: token, accountID
  name: lake-prod-backups
  deletionPolicy: Retain
  credentials:
    secretName: lake-backups-s3
    allowedNamespaces: [ws-0f3c]
---
apiVersion: orama.cloud/v1alpha1
kind: BackupSchedule
metadata: {name: events, namespace: ws-0f3c}
spec:
  datastore:
    kind: ClickHouse
    name: events
    connectionSecretRef: {name: events-backup-connection}
  bucket: {name: lake-backups, namespace: lake}
  schedule: "0 3 * * *"
  keep: 7
```

Nothing like this is applied to any cluster yet: applying a resource is Steve's consent.

## Owners

[#740](https://github.com/oramasearch/orama-operator/issues/740). The resource that owns a datastore
writes its schedules, so nobody writes one by hand, and a late or failed backup is visible on that
owner.

**A `Lake` with `spec.backups`** owns a `CloudflareR2Bucket` `lake-<name>-backups` in its namespace
(`deletionPolicy: Retain`, made with the account token in `spec.backups.cloudflareSecretRef`, its
credentials allowed for every workspace namespace that has a schedule) and, for each ready managed
service, a `BackupSchedule` `<datastore>-backup` in the service's workspace namespace, with a
connection Secret `<datastore>-backup-connection` for the kinds whose Job talks to the server. The
connection comes from the service's connection document, which the Lake already reads from lake's
control Postgres (decision 51). A schedule that is not ready, a failed newest backup or a late one is
appended to the service's `service_status` message (nothing is added while all is well, so a steady
service writes nothing) and summarised in the Lake's `BackupsOnSchedule` condition. Deleting a
service deletes its schedule and connection; its Backups stay. Details:
[`lake/backups.md`](lake/backups.md#managed-services).

**An `OramaCloudV1`** with Cloudflare on and a token owns `<stack>-backups` (`Retain`) and a daily
schedule for its control DB (`Postgres`, `<stack>-controldb`) and in-cluster analytics
(`ClickHouse`, `<stack>-analytics`, with `<stack>-analytics-backup-connection` from its generated
password). The cache (Redis) holds what the stack rebuilds, the broker (RabbitMQ) is a transport, and
the engine is rebuilt from its sources, so none gets a schedule. Its `BackupsOnSchedule` condition
says what is late or failed, without changing the stack's phase.

## Platform

The operator's own CRDs are in `chart/crds` with every other kind and every controller runs on every
cluster (principle 11). **What the backup system needs from the platform layer is not in this change**
(cluster files are moving to `orama-infra`, operator#1149), and must be made in `helmfile.yaml` and
every `config/clusters/*/platform.yaml` together, in one pull request, before an operator with this
code is relied on for these kinds:

| Release | Chart | Namespace | For | Notes |
|---|---|---|---|---|
| `plugin-barman-cloud` | `cnpg/plugin-barman-cloud` (the `cnpg` repository already in the helmfile, `https://cloudnative-pg.github.io/charts`), pinned to the release that supports the CNPG pin (0.29.0, operator 1.30.0) | `cnpg-system`, with the CNPG operator | Postgres (#731) | Needs cert-manager (installed) for its TLS with CNPG. Its images (`plugin-barman-cloud`, `barman-cloud` sidecar) are mirrored and pinned by digest like every platform image. `needs: [cnpg-system/cnpg, cert-manager/cert-manager]`. |

Until it is installed, a Postgres schedule reports `ArchivePluginNotInstalled` and owners leave their
Clusters unchanged, so rolling the operator first changes nothing on a running database.

## Follow-ups

Filed from [#453](https://github.com/oramasearch/orama-operator/issues/453), each `p1`:

| Issue | What |
|---|---|
| [#731](https://github.com/oramasearch/orama-operator/issues/731) | **Postgres** through CNPG-I, and Lake and Cloud v1 off `barmanObjectStore` before CloudNativePG 1.31. Built: the Barman Cloud Plugin. |
| [#732](https://github.com/oramasearch/orama-operator/issues/732) | Zetta, built into the engine. |
| [#733](https://github.com/oramasearch/orama-operator/issues/733) | Kafka: decided, no backup, stated on the kind ([Kafka](#kafka)). |
| [#734](https://github.com/oramasearch/orama-operator/issues/734) | Cassandra through Medusa. |
| [#735](https://github.com/oramasearch/orama-operator/issues/735) | TiDB through BR. |
| [#736](https://github.com/oramasearch/orama-operator/issues/736) | Valkey RDB snapshots. Built. |
| [#737](https://github.com/oramasearch/orama-operator/issues/737) | SQLite, DuckDB and Polars. |
| [#738](https://github.com/oramasearch/orama-operator/issues/738) | The Lake store volume to a bucket. |
| [#739](https://github.com/oramasearch/orama-operator/issues/739) | Escrow of the Lake instance key. Waits on Steve. |
| [#740](https://github.com/oramasearch/orama-operator/issues/740) | Owners create schedules and show a late or failed backup on themselves; the datastore's egress to the bucket. |
| [#741](https://github.com/oramasearch/orama-operator/issues/741) | Restore and the scheduled restore rehearsal. |
