# Research: backups for every datastore

Step 1 of [operator#453](https://github.com/oramasearch/orama-operator/issues/453): for each
datastore the operator runs, what backs it up today, the mechanism to use, its consistency, its
licence for resale, and how a restore is proven. Research, not a design: the design and what is
built are in [`../../backups.md`](../../backups.md). Licences were checked through Perplexity on
2026-10-21 against each project's licence file (cited below); nothing here is legal advice.

## What backs up today (at `origin/dev` `66d838f`, 2026-10-21)

| Datastore | Where the operator runs it | Backed up today | Notes |
|---|---|---|---|
| Lake control-plane Postgres | CNPG `Cluster` `lake-<name>-controldb` (`internal/controller/lake_controller.go`) | **Opt-in**: `spec.postgres.backup` sets CNPG `spec.backup.barmanObjectStore` and a `ScheduledBackup`; `status.backupReady`, `status.lastBackupTime`, condition `BackupReady` | `barmanObjectStore` is deprecated since CloudNativePG 1.26; removal is now scheduled for **1.31.0**, postponed twice (1.28.2 moved it to 1.30.0, 1.28.4 to 1.31.0; [1.30 notes](https://cloudnative-pg.io/docs/1.30/release_notes/v1.30/), [1.28 notes](https://cloudnative-pg.io/docs/1.30/release_notes/old/v1.28/)). `LakeRestoreRehearsal` restores from these CNPG `Backup`s ([`../../lake/backups.md`](../../lake/backups.md)). |
| Cloud v1 control DB | CNPG `Cluster` `<oc>-controldb` (`controldb.go`) | **No** | [#399](https://github.com/oramasearch/orama-operator/pull/399) (barmanObjectStore) was rejected for the reason above. |
| Lake `postgres` services | CNPG `Cluster` in the workspace namespace (`lake_services.go`) | **No** | |
| Lake store volume | the Lake's PVC | **CSI snapshots**, from lake's `volume_snapshot_policies` ([operator#260](https://github.com/oramasearch/orama-operator/issues/260), [`../../lake/volume-snapshots.md`](../../lake/volume-snapshots.md)) | Disk snapshots through a `Retain` `VolumeSnapshotClass`: in the cloud's snapshot storage in the same region, not in a bucket; restore is a runbook. |
| Workspace Zetta volumes | `OramaZettaCluster` PVCs | **CSI snapshots**, same mechanism | Volume-level, not Zetta-consistent. |
| Lake instance key (`lake-prod-instance-kek` Secret) | Kubernetes Secret | **No** | Not backed up anywhere else (umbrella `CLAUDE.md`). Losing it loses every sealed workspace secret. |
| Kafka, ClickHouse, TiDB, Cassandra, Valkey (Lake catalog kinds) | their upstream operators' resources (decision 31) | **No** | |
| SQLite, DuckDB, Polars (Lake wrapper kinds) | StatefulSet + PVC (decision 32) | **No** | "The volume is the only copy" (decision 32). |
| Cloud v1 engine (OramaCore writer and readers) | StatefulSets | **No** | [#404](https://github.com/oramasearch/orama-operator/pull/404) (scheduled CSI snapshots) was rejected in favour of this system; its rules are kept below. |
| Cloud v1 ClickHouse, Redis, RabbitMQ | in-cluster | **No** | |
| Buckets | `CloudflareR2Bucket` (only the Cloud v1 index-builder bucket used it) | — | Before #453: an account-wide token only, and the finalizer always deleted the bucket. |

## Mechanism per datastore

| Datastore | Mechanism | Consistency | Licence (SPDX) | Tool in the operator image? | Restore proof |
|---|---|---|---|---|---|
| **Postgres** | Continuous WAL archiving + base backups through CloudNativePG's plugin interface (CNPG-I); tool **open** (next section) | Point-in-time, crash-consistent base backup plus WAL | CloudNativePG Apache-2.0 ([licence](https://github.com/cloudnative-pg/cloudnative-pg/blob/main/LICENSE)) | No: the plugin runs as its own sidecar | CNPG `bootstrap.recovery` into a scratch `Cluster`, then a row check (`LakeRestoreRehearsal` does this today) |
| **ClickHouse** | `BACKUP ... TO S3(...)` / `RESTORE ... FROM S3(...)`, built into the server | Per-table snapshot at backup time | Apache-2.0 ([licence](https://github.com/ClickHouse/ClickHouse/blob/master/LICENSE)) | No: SQL over HTTP from Go. **Built.** | `RESTORE` into a scratch server, compare row counts per table |
| **Zetta** | Our own, built into Zetta (zetta work) | To be defined by Zetta | Ours | No | Restore into a scratch `OramaZettaCluster`, run the Zetta e2e reads |
| **TiDB** | BR through TiDB Operator's `Backup` resource (the operator already runs TiDB Operator, decision 31) | Snapshot at a TSO; log backup for PITR | Apache-2.0 ([licence](https://github.com/pingcap/tidb/blob/master/LICENSE)) | No: TiDB Operator runs BR | TiDB Operator `Restore` into a scratch cluster |
| **Cassandra** | Medusa through K8ssandra (`MedusaBackupJob`), already the operator for Cassandra | Per-node snapshot, cluster-wide at one time | Apache-2.0 ([licence](https://github.com/thelastpickle/cassandra-medusa/blob/master/LICENSE)) | No: K8ssandra runs Medusa | `MedusaRestoreJob` into a scratch datacenter |
| **Valkey** | RDB snapshot (`BGSAVE`, or a replica's `SYNC` stream) uploaded to the bucket | Point-in-time fork snapshot | BSD-3-Clause ([licence](https://github.com/valkey-io/valkey/blob/unstable/COPYING)) | Possibly none: the replication protocol from Go | Load the RDB into a scratch server, compare `DBSIZE` and sampled keys |
| **Kafka** | No snapshot exists. Choose: MirrorMaker 2 (Apache-2.0, part of Kafka) to another cluster, or **no backup, stated on the kind** | — | Apache-2.0 | — | Consume from the mirror |
| **SQLite** | The wrapper's own online backup (`VACUUM INTO` or the backup API) to a file, uploaded | Transactionally consistent | SQLite public domain; `modernc.org/sqlite` BSD-3-Clause | No: the wrapper service does it | Open the file, `PRAGMA integrity_check` |
| **DuckDB, Polars** | `EXPORT DATABASE` (DuckDB) or the wrapper's file snapshot, uploaded | Consistent per export | DuckDB MIT; Polars MIT | No | Import into a scratch instance |
| **Lake store volume** | Today CSI snapshots; to a bucket would need a file-level copy (restic-style) or lake's own export | Crash-consistent at best | — | — | Mount the restored volume in a scratch Lake |
| **Lake instance key** | Escrow, not backup: sealed to a recovery key held off-cluster (decision for Steve) | — | — | — | Unseal in a scratch namespace and decrypt a known secret |

Rules kept from the rejected #404 for any volume mechanism: daily by default keeping 7; only ready
snapshots count and the newest ready one is never deleted; snapshots outlive the stack through a
`Retain` class in the platform layer; timing from the newest existing snapshot. The
`BackupSchedule` controller applies the same rules to `Backup`s.

## Postgres: the decision that is open

Postgres is first in the issue's order, and it is not built, because the tool is Steve's choice:

| Option | Licence | What it means |
|---|---|---|
| **Barman Cloud Plugin** (`cloudnative-pg/plugin-barman-cloud`) | The plugin is Apache-2.0 ([licence](https://github.com/cloudnative-pg/plugin-barman-cloud/blob/main/LICENSE)); it installs and runs **Barman, GPL-3.0** ([licence](https://github.com/EnterpriseDB/barman/blob/master/LICENSE)) | CloudNativePG's own replacement for `barmanObjectStore`, maintained upstream. Shipping its image to self-hosting customers means meeting GPL-3.0 for Barman (licence text, notices, corresponding source). A platform chart in `helmfile.yaml` and every `platform.yaml`. |
| **Our own CNPG-I plugin on pgBackRest** | pgBackRest is **MIT** ([licence](https://github.com/pgbackrest/pgbackrest/blob/main/LICENSE)) | No official plugin exists; we write and maintain one (a sidecar image of our own, private repo per the org rule). |
| **Our own CNPG-I plugin on WAL-G** | WAL-G is **Apache-2.0** ([licence](https://github.com/wal-g/wal-g/blob/master/LICENSE)) | Same as above, on WAL-G. |
| **Base backups only, from a Job** | — | No point-in-time restore. A physical base backup over the replication protocol needs a replication role and a `pg_hba` rule on every cluster, and CloudNativePG has no way to bootstrap from such a tarball, so restore would be ours too. Not recommended. |

Continuous archiving needs every WAL segment shipped as it is written, which a scheduled Job does
not do; a CNPG-I plugin runs beside Postgres and does. So whichever plugin is chosen, the Postgres
`Backup` would ask CloudNativePG for a backup (`method: plugin`) rather than run the tool itself, and
the `BackupSchedule` would own the plugin's archive configuration. Lake's existing
`spec.postgres.backup` moves to it before the operator's CloudNativePG pin reaches 1.31.

**Decided under Steve's 2026-11-14 delegation: the Barman Cloud Plugin**
([#731](https://github.com/oramasearch/orama-operator/issues/731)): upstream-maintained, reads the
existing backups, no new repository or image of ours. Built as described in
[`../../backups.md`](../../backups.md#postgres).

## Sources

- The issue and its comments: https://github.com/oramasearch/orama-operator/issues/453
- Licence and removal-version checks (Perplexity, 2026-10-21): the licence files linked in the
  tables; [CloudNativePG 1.30 release notes](https://cloudnative-pg.io/docs/1.30/release_notes/v1.30/).
- R2 bucket-scoped tokens and the derived S3 credentials:
  https://developers.cloudflare.com/r2/api/tokens/
