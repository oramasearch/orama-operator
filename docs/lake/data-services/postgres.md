# Postgres data-service rehearsals

`LakeRestoreRehearsal` proves the Lake control-plane Postgres lifecycle without replacing the live
database. The CR is cluster-scoped, like `Lake`. Its spec is immutable after creation, and its
terminal `Succeeded` or `Failed` status is the durable audit result. A failed result leaves the
corresponding Lake evidence `untested`, invalidates an older successful result for that operation,
and writes the failure message and completion timestamp to the Postgres row.

## Prerequisites

- CloudNativePG is installed and the source Cluster is healthy.
- `spec.postgres.backup.enabled` is true, its R2/S3 Secret is valid, and at least one CNPG Backup is
  `completed`.
- The operator ClusterRole includes `lakerestorerehearsals`, scratch Namespace lifecycle, Jobs,
  Secrets, CNPG `clusters`, `clusters/status`, and `backups`.
- Restore and upgrade require the source Cluster's running image to use a reviewed SHA-256 digest.
  Set `Lake.spec.postgres.image` to the approved CNPG-compatible image before rehearsing. Empty
  keeps CNPG's default for compatibility; changing the field requests CNPG's normal image rollout.
- `targetImage` for an upgrade is a published CNPG-compatible image pinned by SHA-256 digest.

- The Lake's network sandbox admits the verifier to the control database through
  `allow-rehearsal-ingress-to-controldb` (operator versions after v0.9.5). Under an older operator
  the verifier of a sandboxed Lake cannot connect and the rehearsal fails at *read-only rehearsal
  login is unavailable*.

For lake-prod, the rehearsal is declared in its set file and applied on the member's cluster by the
managing operator; the runbook, with what is missing today, is
[restore rehearsals](../restore-rehearsals.md).

Never run a rehearsal against `Lake/prod` on AKS unless that operation is deliberately approved.
Use the disposable k0s or `kind` cluster described in the work order when its R2 Secret is
available.

## Verification credential

The Lake manages `lake_rehearsal` through CNPG's `managed.roles`, with membership only in
`pg_read_all_data`, no administrative or write privileges, no RLS bypass, and four connections.
Its password is generated once in `<control-db>-rehearsal`. Scratch recovery declares the same
role with the copied credential, so a backup taken before the role existed can still be verified.
All verification Jobs use this role and transaction read-only mode, and wait for CNPG to apply
the login before querying. A missing Secret or role
leaves the rehearsal pending or fails its Job; the controller never falls back to the owner.
Restore/upgrade validate source and target pins before copying credentials. The verifier image
itself is pinned in `internal/controller/helper_images.go`.

## Restore

The default `from: latest` selects the newest completed `Backup` and pins its
`status.backupId`. Set `from` to a CNPG `Backup` object name to select a specific completed backup.

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

The controller creates `lake-rehearsal-<name>-<hash>`, copies the object-store Secret and dedicated read-only rehearsal credential
into it, and creates a one-instance CNPG `Cluster` named `postgres`. Recovery uses the
Lake's exact object-store destination, the original Barman server name, and the selected backup ID.
After CNPG is healthy, a digest-pinned `postgres:18-alpine` Job queries all schema guard tables and the newest
`schema_migrations` version in both live and recovered databases, including
`services_guard` from `0024_services`, `service_catalog_guard` from `0025_service_catalog`,
`sqlite_services_guard` from `0026_sqlite_services`, `analytics_services_guard` from
`0027_analytics_services` and `stores_guard` from `0028_stores`. The same query lists, as
`missing_migrations=`, every migration recorded in the vendored lake schema
(`INSERT INTO schema_migrations (version) VALUES ('NNNN_…')`) that the database's `schema_migrations`
lacks. For a Lake whose image has `lake manifest`, the guard tables and the migrations are the
image's own (`status.imageManifest.guardTables` and `.migrations`,
[#936](https://github.com/oramasearch/orama-operator/issues/936)). The Job fails if that list is non-empty on either side, before comparing. Comparison alone
cannot see a gap present in both databases, and neither the guard counts nor the newest version show
an earlier-numbered migration that never applied: on 2026-10-05, `0067_workspace_store` merged after
`0068` and `0069` had applied, and the newest version stayed `0069`
([#271](https://github.com/oramasearch/orama-operator/issues/271)). Run a rehearsal after the Lake
schema reports `Applied` at the operator's vendored version, or the check reports the migrations the
pending Job is about to apply. Success marks both `backup` and `restore` as `tested`. Either outcome
removes every scratch object.

## Upgrade

Upgrade starts from the same isolated recovery path. Once the scratch Cluster is healthy on the
source image reported by CNPG, the controller changes its `spec.imageName` to `targetImage` with an
unsupervised primary update. A multi-instance clone uses the `switchover` update method; a
single-instance clone uses `restart`. CNPG must report the target image and a healthy Cluster before
the same data verification runs.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: LakeRestoreRehearsal
metadata:
  name: prod-postgres-upgrade-18-2
spec:
  lakeRef: prod
  service: postgres
  operation: upgrade
  from: latest
  targetImage: ghcr.io/cloudnative-pg/postgresql:18.2@sha256:<reviewed-64-hex-digest>
```

Success marks only `upgrade` as `tested`; it does not change the live Cluster image. Apply a reviewed live image change through `Lake.spec.postgres.image`, with the usual production approval.

## Failover

Failover is available only when `spec.postgres.instances` is at least 2. The controller reads
CNPG's healthy instance list, selects a non-primary instance deterministically, and requests the
same status transition as `kubectl cnpg promote`. It waits until CNPG reports that target as the
healthy current primary, then runs a Job in the Lake namespace using the dedicated read-only rehearsal
Secret and `-rw` Service. The query checks that the schema remains reachable through the
read-only connection contract and that `schema_migrations` holds every migration the vendored lake
schema records.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: LakeRestoreRehearsal
metadata:
  name: prod-postgres-failover-20260905
spec:
  lakeRef: prod
  service: postgres
  operation: failover
```

A single-instance Lake always reports `failover: none` with an explanatory note. Creating a
failover rehearsal for it records `Failed` without changing the Cluster.

## Observe results

```sh
kubectl get lrr
kubectl get lrr <rehearsal-name> -o yaml
kubectl get lake <lake-name> -o jsonpath='{.status.dataServices}' | jq
```

Keep successful and failed rehearsal CRs as audit records. Deleting one does not erase the outcome
already recorded on the Lake; a later rehearsal updates the operation evidence and `lastVerified`.
