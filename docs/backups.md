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
| **Postgres** mechanism (Lake's control plane, Cloud v1's control DB, Lake `postgres` services) | **Not built: waiting on Steve's choice** of tool ([research](research/backups/README.md#postgres-the-decision-that-is-open)). A Postgres `BackupSchedule` reports `Ready=False`, `MechanismNotAvailable`. |
| Zetta, Kafka, Cassandra, TiDB, Valkey, SQLite, DuckDB, Polars | Not built; each is filed (see [Follow-ups](#follow-ups)). Refused the same way. |
| Owners creating schedules (a Lake for its services, an `OramaCloudV1` for its datastores) and showing a late backup on themselves | Not built; filed. Today a schedule is a resource you write. |
| Restore and the scheduled restore rehearsal | Not built; their kinds are settled after these two (issue body). |
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

When the Backup has succeeded or failed, the credentials copy and the network policy are deleted.

### Status

| Resource | Says |
|---|---|
| `Backup` | `phase` (`Pending`, `Running`, `Succeeded`, `Failed`), `location` (`s3://<bucket>/<path>/<name>/`), `startTime`, `completionTime`, `sizeBytes`, `message`, and condition `Complete` with the reason it is waiting or failed: `BucketNotFound`, `BucketHasNoCredentials`, `BucketNotAllowed`, `BucketNotReady`, `OperatorImageUnknown`, `MechanismNotAvailable`, `BackupFailed`, `JobFailed`, `NoReport`, `JobLost`. |
| `BackupSchedule` | `lastScheduleTime`, `nextScheduleTime`, `lastSuccessfulBackup`, `lastSuccessTime`, and conditions `Ready` (`Scheduled`, `Suspended`, `InvalidSchedule`, `MechanismNotAvailable`), `LastBackupSucceeded` (the newest finished Backup's outcome and message) and `OnSchedule` (`False`, `BackupLate`, once a whole tick has passed after the tick that should have followed the last success). |

Status is written only when it changes; a steady schedule causes no writes. The schedule wakes for
its next tick and for the moment it would turn late.

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
| `Postgres` | Open: see the research. | — | Not built. |
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

## Platform

Nothing new is installed: no chart, no CRD from another project. The two new CRDs are in
`chart/crds` with every other kind, every controller runs on every cluster (principle 11), and
`helmfile.yaml` and `config/clusters/*/platform.yaml` are unchanged.

## Follow-ups

Filed from [#453](https://github.com/oramasearch/orama-operator/issues/453), each `p1`:

| Issue | What |
|---|---|
| [#731](https://github.com/oramasearch/orama-operator/issues/731) | **Postgres** through CNPG-I, and Lake and Cloud v1 off `barmanObjectStore` before CloudNativePG 1.31. Waits on Steve's tool choice. |
| [#732](https://github.com/oramasearch/orama-operator/issues/732) | Zetta, built into the engine. |
| [#733](https://github.com/oramasearch/orama-operator/issues/733) | Kafka: mirroring or no backup, stated on the kind. Waits on Steve. |
| [#734](https://github.com/oramasearch/orama-operator/issues/734) | Cassandra through Medusa. |
| [#735](https://github.com/oramasearch/orama-operator/issues/735) | TiDB through BR. |
| [#736](https://github.com/oramasearch/orama-operator/issues/736) | Valkey RDB snapshots. |
| [#737](https://github.com/oramasearch/orama-operator/issues/737) | SQLite, DuckDB and Polars. |
| [#738](https://github.com/oramasearch/orama-operator/issues/738) | The Lake store volume to a bucket. |
| [#739](https://github.com/oramasearch/orama-operator/issues/739) | Escrow of the Lake instance key. Waits on Steve. |
| [#740](https://github.com/oramasearch/orama-operator/issues/740) | Owners create schedules and show a late or failed backup on themselves; the datastore's egress to the bucket. |
| [#741](https://github.com/oramasearch/orama-operator/issues/741) | Restore and the scheduled restore rehearsal. |
