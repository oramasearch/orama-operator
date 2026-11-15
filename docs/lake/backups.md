# Lake control-plane backups and recovery

Each `Lake` can opt into scheduled backups of its CNPG control-plane Postgres to an
S3-compatible object store. The operator writes a Barman Cloud Plugin `ObjectStore`
(`barmancloud.cnpg.io`) `lake-<name>-controldb-archive`, names it as the Cluster's WAL archiver in
`spec.plugins`, and owns a `ScheduledBackup` with `method: plugin`; CNPG creates the individual
`Backup` resources. Until [operator#731](https://github.com/oramasearch/orama-operator/issues/731)
this used CNPG's in-tree `spec.backup.barmanObjectStore`, which CloudNativePG removes in 1.31.0; the
`ObjectStore` has the same destination, so backups taken the old way stay readable. The plugin must
be installed on the cluster ([`../backups.md`](../backups.md#platform)); without it the Lake reports
`BackupReady=False`, `ArchivePluginNotInstalled`. This protects lake's users,
organizations, customers, and repositories. It does not back up the Zetta data plane or any
other datastore.

## Configure Cloudflare R2

For `Lake/prod`, create a dedicated Cloudflare R2 bucket, such as `lake-prod-backups`. Create an
R2 API token scoped to that bucket with **Object Read & Write** permission, then create the
credentials Secret in the Lake namespace. The endpoint is the account's S3 API endpoint.

```sh
kubectl create secret generic lake-prod-backups-r2 -n lake \
  --from-literal=s3ApiEndpoint='https://<ACCOUNT_ID>.r2.cloudflarestorage.com' \
  --from-literal=accessKeyId='<R2_ACCESS_KEY_ID>' \
  --from-literal=secretAccessKey='<R2_SECRET_ACCESS_KEY>'
```

Keep this Secret out of Git. The operator reads `s3ApiEndpoint` only to render the ObjectStore's
endpoint; the plugin references `accessKeyId` and `secretAccessKey` directly from the Secret.

Enable the backup in the `Lake` after the Secret exists:

```yaml
spec:
  postgres:
    backup:
      enabled: true
      destination:
        bucket: lake-prod-backups
      secretRef:
        name: lake-prod-backups-r2
```

The default six-field CNPG schedule is `0 0 3 * * *` (03:00:00 daily), the retention policy is
`30d`, and the default prefix is `lake-<name>/controldb`. The resulting object-store path for
`Lake/prod` is `s3://lake-prod-backups/lake-prod/controldb`; WAL files are gzip-compressed.

## Verify and change a backup

The operator creates `lake-<name>-controldb-backup`. CNPG creates a `Backup` immediately when
that schedule is first applied, then according to the schedule.

```sh
kubectl get scheduledbackups,backups -n lake
kubectl get lake prod
```

`Lake.status.backupReady` is true only when the ScheduledBackup exists and the newest CNPG
Backup is `completed`; `lastBackupTime` is displayed in the `Backup` column. To confirm objects
landed in R2, use an S3-compatible client configured with the R2 token:

```sh
aws s3 ls s3://lake-prod-backups/lake-prod/controldb/ --recursive \
  --endpoint-url 'https://<ACCOUNT_ID>.r2.cloudflarestorage.com'
```

Change `schedule`, `retention`, `destination.path`, or `destination.endpointURL` in the Lake
manifest and apply it again. `endpointURL` overrides the Secret's `s3ApiEndpoint`; leaving it
empty uses that Secret value. Set `enabled: false` to remove the plugin from the Cluster and delete
the ObjectStore and the ScheduledBackup. Existing R2 objects are retained.

If enabled backups have no bucket, no Secret reference, a missing Secret, or a Secret missing one
of the three required keys, the operator reports `BackupReady=False` with the cause in the Lake
condition and sets the Lake phase to `Degraded`. It does not create a ScheduledBackup or configure
the object store until the configuration is corrected.

## Rehearse recovery

Use a `LakeRestoreRehearsal`; do not point the running Lake at a recovered Cluster or replace
`lake-prod-controldb` during an investigation. The controller uses the same R2 Secret and exact
bucket/prefix from the Lake backup configuration in an isolated scratch namespace.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: LakeRestoreRehearsal
metadata:
  name: prod-postgres-restore-20260905
spec:
  lakeRef: prod
  service: postgres
  operation: restore
  from: latest
```

Apply it and inspect the durable result:

```sh
kubectl apply -f lake-prod-restore-rehearsal.yaml
kubectl get lrr prod-postgres-restore-20260905 -w
kubectl get lake prod -o jsonpath='{.status.dataServices}' | jq
```

The controller pins the selected Barman backup ID, recovers it through the Barman Cloud Plugin (an
`ObjectStore` `source-archive` in the scratch namespace, with the copied credentials), compares guard-table counts, the
migrations missing from `schema_migrations` and the schema version with the live database, fails if
either side lacks a migration the vendored lake schema records, records success or failure, and deletes the scratch
resources. See [Postgres data-service rehearsals](data-services/postgres.md) for restore, upgrade,
and failover details.

The live recovery rehearsal remains deferred until backups are enabled. On lake.live the Lakes are
`shared` (EU) and `shared-us` (US) on the production leaf clusters, and on 2026-10-24 neither had
`spec.postgres.backup` enabled: no Barman object store, no `ScheduledBackup`, no CNPG `Backup`.
What exists, what is missing and how the rehearsal is declared from the `lake-prod` set file are in
[restore rehearsals](restore-rehearsals.md). Do not run it as part of an operator code change.

## Managed services

[operator#740](https://github.com/oramasearch/orama-operator/issues/740). The control-plane backup
above is the Lake's own database. The data in its managed services goes through the operator's
backup system ([`../backups.md`](../backups.md)) once the Lake has `spec.backups`:

```yaml
spec:
  backups:
    cloudflareSecretRef: {name: cloudflare-r2}   # token, accountID: may create R2 buckets and API tokens
    schedule: "0 3 * * *"                        # default
    keep: 7                                      # default
```

- The Lake owns `CloudflareR2Bucket` `lake-<name>-backups` in its namespace, `deletionPolicy:
  Retain`: removing `spec.backups` or deleting the Lake deletes the resource, never the bucket or a
  backup, and setting it again adopts the bucket.
- Each ready managed service gets a `BackupSchedule` `<datastore>-backup` in its workspace
  namespace, where `<datastore>` is the service's resource (`<lake>-<slug>`, or the workspace's
  Zetta cluster). For Zetta, ClickHouse and Valkey the Lake also writes `<datastore>-backup-connection`
  (`host`, `port`, `username`, `password`, `database`) from the service's connection. The workspace
  namespace is added to the bucket's `allowedNamespaces`.
- **A late or failed backup is shown**: appended to the service's `service_status` message (for
  example `service ready; backup late: no backup has succeeded since ...`), and summarised in the
  Lake's condition `BackupsOnSchedule`. A workspace's own Zetta shows only on the condition, since
  its status belongs to the workspace reconciler.
- Deleting a service deletes its schedule and connection Secret. Its `Backup`s are only labelled,
  so they and their objects stay.

Nothing of this is applied to any cluster: `spec.backups` is set in a set file, with Steve's consent
for the apply, and the account token is Steve's to issue.

