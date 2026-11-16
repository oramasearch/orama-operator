# Restore rehearsals: lake-prod's control plane and store

How a restore of lake.example.com's data is rehearsed, what backups exist to restore from, and what to
record afterwards ([#127](https://github.com/oramasearch/orama-operator/issues/127), launch
checklist [lake#1158](https://github.com/oramasearch/lake/issues/1158)). A rehearsal restores into
throwaway resources and reads them; it never touches the live database, the live store, their
backups or their snapshots.

## What exists to restore from (read-only, 2026-10-24)

lake.example.com is the `LakeSet` `lake-prod` on the managing cluster, with two members:

| | EU | US |
|---|---|---|
| Cluster | `example-production-eu` (`swedencentral`) | `example-production-us` (`eastus`) |
| Lake, namespace | `shared`, `lake-shared` | `shared-us`, `lake-shared-us` |
| Control database | CNPG `lake-shared-controldb`, 1 instance, 10Gi | CNPG `lake-shared-us-controldb`, 1 instance, 10Gi |
| Control database backups | **None**: no `spec.backup`, no `ScheduledBackup`, no CNPG `Backup`; `BackupReady=False` (`BackupDisabled`) | **None**, the same |
| Store | claim `lake-shared-store`, 20Gi, `managed-csi` | claim `lake-shared-us-store`, 20Gi, `managed-csi` |
| Store snapshots | 7 `VolumeSnapshot`s, daily at 03:00 UTC, 2026-10-18 to 2026-10-24, all `readyToUse`, class `lake-retain` (`Retain`) | 7, the same dates, all `readyToUse` |
| Workspace Zetta snapshots | none (no workspace) | 1, `prod-ws-6d3936330133/data-shared-us-ws-6d3936330133-zetta-0-20261024t045018z` |
| Lake's own Zetta volume | `data-lake-shared-zetta-zetta-0`, no backup or snapshot | `data-lake-shared-us-zetta-zetta-0`, the same |
| Operator | v0.9.5 | v0.9.5 |

The operator's own `Backup`/`BackupSchedule` kinds ([`../backups.md`](../backups.md)) are not used
on either cluster, and no `LakeRestoreRehearsal` has run on either.

The store snapshots are Azure disk snapshots in the cluster's own region and subscription, not a
copy elsewhere. Read the current state with:

```sh
# On the managing cluster: the leaf's kubeconfig, into a private file outside any repository.
kubectl --context example-management -n orama-system get secret example-production-eu-kubeconfig \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > "$KC/example-production-eu"; chmod 600 "$KC/example-production-eu"
export KUBECONFIG="$KC/example-production-eu"
kubectl get lake shared
kubectl get clusters.postgresql.cnpg.io,scheduledbackups.postgresql.cnpg.io,backups.postgresql.cnpg.io -n lake-shared
kubectl get volumesnapshots -n lake-shared -l orama.cloud/snapshot-target=store
```

## What is missing, and filed

| Gap | Issue | Effect on the rehearsal |
|---|---|---|
| No control-plane Postgres backup in either region | [#957](https://github.com/oramasearch/orama-operator/issues/957) (tool choice: [#731](https://github.com/oramasearch/orama-operator/issues/731)) | The Postgres rehearsal fails at once: *control-plane object-store backups are not enabled*. |
| The control databases run CNPG's default image by tag | [#958](https://github.com/oramasearch/orama-operator/issues/958) | Once backups exist, the Postgres rehearsal still refuses: *source Postgres image must use an immutable SHA-256 digest*. |
| The store's snapshots never leave the cluster's region and account | [#738](https://github.com/oramasearch/orama-operator/issues/738) | None: the store rehearsal restores from them. It does not protect against losing the region. |
| Lake's own Zetta volume has no backup or snapshot | [#959](https://github.com/oramasearch/orama-operator/issues/959) | Nothing to rehearse. |
| The instance key is not escrowed | [#739](https://github.com/oramasearch/orama-operator/issues/739) | A restored database is useless for sealed secrets if the key is lost; not rehearsed here. |
| The store verifier checks readability, not lake's own integrity checks | [#960](https://github.com/oramasearch/orama-operator/issues/960) | The store rehearsal proves the files restore and read, not that the transparency log and TUF metadata verify. |

Fixed in the same change as this page: the Lake's network sandbox now admits the rehearsal
verifier to the control database (`allow-rehearsal-ingress-to-controldb`,
[network sandbox](../infrastructure/network-sandbox.md)). Before it, every Postgres rehearsal of a
sandboxed Lake, which lake-prod's are, failed at *read-only rehearsal login is unavailable*.

## How a rehearsal is declared

Every file in [`config/clusters/`](../../config/clusters/README.md) is applied to the managing
cluster, but a `LakeRestoreRehearsal` must run on the cluster of the Lake it rehearses. So it is an
entry in the set file, under `spec.restoreRehearsals` of
[`sets/lake-prod.yaml`](../../config/clusters/sets/lake-prod.yaml), and the managing operator applies
it on the member's cluster with `spec.lakeRef` set to the member's Lake. The entries are in the
file, commented out:

```yaml
  restoreRehearsals:
  - name: shared-store-restore-20261024
    member: shared
    spec: {service: store, operation: restore, from: latest}
  - name: shared-us-store-restore-20261024
    member: shared-us
    spec: {service: store, operation: restore, from: latest}
```

A rehearsal's spec is immutable. To rehearse again, add an entry with a new name (the date is the
convention); keep the old entries as the record until they are no longer wanted.

### Store restore (`service: store`)

On the member's cluster, the `LakeRestoreRehearsal` controller:

1. Picks the newest `readyToUse` `VolumeSnapshot` the operator took of `lake-<member>-store`
   (`from: latest`), or the one named in `from`, and pins it in `status.selectedSnapshot`. A
   snapshot the operator did not take of this store is refused.
2. Creates the scratch namespace `lake-rehearsal-<name>-<hash>`.
3. Creates a cluster-scoped `VolumeSnapshotContent` `lake-rehearsal-store-<hash>` bound to the
   same Azure snapshot handle, with `deletionPolicy: Retain`, and a `VolumeSnapshot` `store` in the
   scratch namespace bound to it.
4. Restores a claim `store` from that snapshot (the store's storage class, at least its size), and
   runs a Job `verify` that mounts it **read-only**, as UID 10001 (Lake's), with no service account
   token, lists every file and reads each one in full (one hour at most).
5. Records the outcome and the verifier's report (`status.evidence`:
   `files=<n> kib=<n> top=<entries>`), then deletes the Job, the claim (and with it the restored
   disk), the snapshot, the namespace and the content. The content is `Retain`, so deleting it
   leaves the Azure snapshot, which the live snapshot is still bound to.

It never reads or writes the live claim, and it never creates, changes or deletes the live
`VolumeSnapshot`s or their contents.

### Control database restore (`service: postgres`)

Unchanged from [Postgres rehearsals](data-services/postgres.md): it recovers the selected CNPG
`Backup` from the object store into a scratch CNPG `Cluster` named `postgres`, then a read-only Job
compares every guard-table count, the missing migrations and the schema version of the restored and
the live database. The scratch cluster has no `backup` section, so it never writes to the live
cluster's backup path. It needs #957 and #958 first; until then the entries stay commented out (or,
if applied, fail at once and create nothing).

## What the cluster owner applies

Nothing in this change is applied. In order:

1. **An operator release containing this change**, rolled per
   [`../infrastructure/operations.md`](../infrastructure/operations.md): to the leaves through each
   `config/clusters/<cluster>/operator.yaml` (they run the rehearsal), and to the managing cluster
   (it applies `spec.restoreRehearsals`). The CRDs widen before the roll, as
   [`../operations/crd-rollout.md`](../operations/crd-rollout.md) says. Through nonprod first: the
   same entries for a `lake-nonprod` member are the dev proof.
2. **Uncomment the store entries** in `config/clusters/sets/lake-prod.yaml` and apply that file:

   ```sh
   kubectl --context example-management apply -f config/clusters/sets/lake-prod.yaml
   ```

   Commit the uncommented file with the apply. The members' Lakes are not changed by it: the only
   new thing on each leaf is the rehearsal and its scratch resources.
3. **For the control database**, after #957 and #958 are done and a CNPG `Backup` is `completed` in
   each region: add the Postgres entries under new names and apply the same way.

## Reading the result

On the managing cluster, for both regions at once:

```sh
kubectl --context example-management get lakeset lake-prod \
  -o jsonpath='{range .status.restoreRehearsals[*]}{.name}{"\t"}{.phase}{"\t"}{.selected}{"\t"}{.evidence}{"\t"}{.message}{"\n"}{end}'
```

On a leaf, the rehearsal itself (`kubectl get lrr` shows Lake, Service, Operation, Phase,
Completed):

```sh
kubectl get lrr
kubectl get lrr shared-store-restore-20261024 -o yaml
kubectl get ns,volumesnapshotcontents -l app.kubernetes.io/name=lake-postgres-rehearsal   # while it runs
```

| Phase | Means |
|---|---|
| `Pending` (set) | The member is not applied yet, or its cluster's operator predates the kind. |
| `Running` | Selected, restoring or verifying. `message` says the step. |
| `Succeeded` | Store: the snapshot restored and every file read; `evidence` has the count and size. Postgres: the restored database matches the live one; the Lake's `status.dataServices` postgres row shows `backup`/`restore` `tested`. |
| `Failed` | `message` (and `evidence` for the store) says why: no ready snapshot, a snapshot that failed to bind, the verifier's own words. Scratch resources are deleted all the same. |
| `Blocked` (set) | The set refused the entry (another Lake in `lakeRef`, or a changed spec). |

A pass is `Succeeded` in both regions. The controller fails a store with no files; a file count
far below the previous rehearsal's is also a fail, judged by whoever reads it, so compare `evidence`
with the last one you recorded.

## Cleaning up

The controller deletes every scratch resource when the rehearsal finishes, success or failure.
Check that nothing is left:

```sh
kubectl get ns -l app.kubernetes.io/name=lake-postgres-rehearsal
kubectl get volumesnapshotcontents -l app.kubernetes.io/name=lake-postgres-rehearsal
```

Both must be empty. The `LakeRestoreRehearsal` itself stays as the record. To remove it, delete its
entry from the set file and apply the file; the set deletes it on the leaf. If a scratch namespace
or content is left behind, say so and file it: deleting it by hand is a change to a production
cluster and needs the cluster owner's consent.

## What to record afterwards

Record each rehearsal in your own operations log: the date, the operator version, the commit of the
set file, which rehearsals ran in which region, their phase, the selected snapshot or Backup and the
evidence, and who approved the apply. The rehearsal is evidence of recovery testing; keep it with your
change records.

