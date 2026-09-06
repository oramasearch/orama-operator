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
- `targetImage` for an upgrade is a published CloudNativePG-compatible PostgreSQL operand image.

Never run a rehearsal against `Lake/prod` on AKS unless that operation is deliberately approved.
Use the disposable k0s or `kind` cluster described in the work order when its R2 Secret is
available.

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

The controller creates `lake-rehearsal-<name>-<hash>`, copies the object-store and application
Secrets into it, and creates a one-instance CNPG `Cluster` named `postgres`. Recovery uses the
Lake's exact object-store destination, the original Barman server name, and the selected backup ID.
After CNPG is healthy, a `postgres:18-alpine` Job queries all schema guard tables and the newest
`schema_migrations` version in both live and recovered databases. Success marks both `backup` and
`restore` as `tested`. Either outcome removes every scratch object.

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
  targetImage: ghcr.io/cloudnative-pg/postgresql:18.2
```

Success marks only `upgrade` as `tested`; it does not change the live Cluster image. Apply a live
image change through the Lake specification only after the reviewed operator API exposes that
production control.

## Failover

Failover is available only when `spec.postgres.instances` is at least 2. The controller reads
CNPG's healthy instance list, selects a non-primary instance deterministically, and requests the
same status transition as `kubectl cnpg promote`. It waits until CNPG reports that target as the
healthy current primary, then runs a Job in the Lake namespace using the existing CNPG application
Secret and `-rw` Service. The query checks that the schema remains reachable through the
application connection contract.

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
