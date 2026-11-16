# Lake control-plane backups and recovery

Each `Lake` can opt into scheduled backups of its CNPG control-plane Postgres to an
S3-compatible object store. The operator configures CNPG's Barman object store and owns a
`ScheduledBackup`; CNPG creates the individual `Backup` resources. This protects lake's users,
organizations, customers, and repositories. It does not back up the Zetta data plane or any
other datastore.

## Configure Cloudflare R2

For each Lake, create a dedicated Cloudflare R2 bucket, such as `lake-prod-backups`. Create an
R2 API token scoped to that bucket with **Object Read & Write** permission, then create the
credentials Secret in the Lake namespace. The endpoint is the account's S3 API endpoint.

```sh
kubectl create secret generic lake-prod-backups-r2 -n lake \
  --from-literal=s3ApiEndpoint='https://<ACCOUNT_ID>.r2.cloudflarestorage.com' \
  --from-literal=accessKeyId='<R2_ACCESS_KEY_ID>' \
  --from-literal=secretAccessKey='<R2_SECRET_ACCESS_KEY>'
```

Keep this Secret out of Git. The operator reads `s3ApiEndpoint` only to render CNPG's public
endpoint setting; CNPG references `accessKeyId` and `secretAccessKey` directly from the Secret.

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
a development Lake is `s3://lake-prod-backups/lake-prod/controldb`; WAL files are gzip-compressed.

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
empty uses that Secret value. Set `enabled: false` to remove the Barman configuration and delete
the ScheduledBackup. Existing R2 objects are retained.

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

The controller pins the selected Barman backup ID, recovers it, compares guard-table counts, the
migrations missing from `schema_migrations` and the schema version with the live database, fails if
either side lacks a migration the vendored lake schema records, records success or failure, and deletes the scratch
resources. See [Postgres data-service rehearsals](data-services/postgres.md) for restore, upgrade,
and failover details.

The live recovery rehearsal remains deferred until backups are enabled. On lake.example.com the Lakes are
`shared` (EU) and `shared-us` (US) on the production leaf clusters, and on 2026-10-24 neither had
`spec.postgres.backup` enabled: no Barman object store, no `ScheduledBackup`, no CNPG `Backup`.
What exists, what is missing and how the rehearsal is declared from the `lake-prod` set file are in
[restore rehearsals](restore-rehearsals.md). Do not run it as part of an operator code change.
