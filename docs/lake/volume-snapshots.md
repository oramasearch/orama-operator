# Lake volume snapshots

Scheduled CSI snapshots of lake's store volume and of every workspace Zetta's volumes
([operator#260](https://github.com/oramasearch/orama-operator/issues/260), decision 54 S13 in
liquid-bridge `docs/decisions.md`). After lake#380 a workspace Zetta's volume holds that
workspace's repositories, Drive, models, datasets and tenant images, and on a one-node workspace it
is the only copy. Lake's store volume holds the transparency log, TUF metadata and the instance
registry, and nothing else backs it up. This is separate from
[operator#33](https://github.com/oramasearch/orama-operator/issues/33), the Cloud v1 dumps to
object storage, and from the control-plane Postgres backups ([backups.md](backups.md)).

## Desired state

Lake owns it, in its control Postgres (lake migration `0069_workspace_volumes`). The operator reads
it there and never talks to a workspace Zetta (decision 51).

| Table | Written by | Columns |
|---|---|---|
| `volume_snapshot_policies` | lake (`request_volume_snapshot`, the migration, a trigger on new workspaces) | `target` (`store` or a workspace id), `schedule` (five cron fields, UTC, default `0 3 * * *`), `keep` (default 7), `requested_at` |
| `volume_snapshot_status` | the operator | `target`, `snapshot_name`, `ready_to_use`, `snapshot_at`, `message` |

Every workspace gets the default policy when it is created, and the migration adds `store` and
every existing workspace. Lake's `request_volume_snapshot` stamps `requested_at` and may change
`schedule` and `keep`; `volume_snapshots` reads the policies with their status. Both are for the
operator credential or lake's instance administrator only (lake `docs/api/volumes.md`).

## What the operator does

Every Lake runs it; there is no switch ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)). Both fields are optional:

```yaml
spec:
  snapshots:
    className: lake-retain        # default
    driver: disk.csi.azure.com    # default
```

The `lake-volume-snapshots` controller wakes on `NOTIFY lake_volume_snapshots` and polls every 60
seconds (every 10 while a snapshot is not ready). On each pass:

1. **Class.** It creates the cluster-scoped `VolumeSnapshotClass` `className` for `driver` with
   `deletionPolicy: Retain` when it is absent. An existing class is used as it is, and refused
   unless its `deletionPolicy` is `Retain`: every target then reports the reason and nothing is
   taken. Without the `snapshot.storage.k8s.io` CRDs every target reports that instead.
2. **Claims.** `store` is the Lake's store claim (`lake-<name>-store` or `spec.store.existingClaim`)
   in `spec.namespace`. A workspace target is each `data-<cluster>-zetta-<n>` claim in the workspace
   namespace (running or stopped workspaces; a stopped workspace keeps its claims). A deleted
   workspace, or one lake no longer has, is left alone: nothing is taken or pruned, and its status
   row stays.
3. **Take.** Every time is judged by Postgres's `now()`, read with the policies, never the
   operator's clock, so clock skew cannot make a request look honored. A claim gets a `VolumeSnapshot` named `<claim>-<UTC timestamp>` (for example
   `data-prod-ws-6bb777ba498a-zetta-0-20261004t030000z`) in its namespace when it has none, when
   the schedule's next time after its last snapshot has come, or when `requested_at` is newer than
   its last snapshot. A schedule that never fires (February 31st) takes no scheduled snapshot and is
   reported in the status; a request still takes one. An existing object with the new snapshot's
   name is adopted only if it is this target's live snapshot of that claim; a terminating, hand-made
   or foreign one is reported and left alone. Each is labelled `orama.cloud/snapshot-target=<target>`,
   `orama.cloud/lake=<lake>` and `app.kubernetes.io/managed-by=orama-operator`, and annotated with
   the exact instant (`orama.cloud/snapshot-at`) and claim. It has no owner reference.
4. **Prune.** Only the operator's own snapshots of this target are considered (its labels); a
   hand-made snapshot, or another target's, in the same namespace is never adopted, counted or
   deleted. Only `readyToUse` snapshots count toward `keep`, and the newest ready one is never
   deleted, even at `keep: 1`. Of the failed ones (not ready, with `status.error`), the newest is
   kept for diagnosis and older ones are deleted; one still in progress is kept until a newer one is
   ready. Before a snapshot is deleted the operator reads its bound `VolumeSnapshotContent` and
   touches it only if it is bound back to that snapshot by the expected driver; it then patches the
   content to `deletionPolicy: Delete`, so the Azure snapshot is deleted with it and `keep` bounds
   what is really kept. A snapshot whose content is not bound yet, missing, of another driver or
   bound elsewhere is not pruned, and the status message says why. Snapshots of a deleted workspace
   are never pruned, so their contents keep the class's `Retain`: the class stays `Retain` as the
   safety net for workspace deletion.
5. **Label contents.** Each bound `VolumeSnapshotContent` is labelled with the target and annotated
   with its snapshot's namespace, name, claim and instant, so it can be found after its snapshot or
   namespace is gone.
6. **Report.** `volume_snapshot_status` gets the target's newest snapshot, `ready_to_use` (every
   claim's newest is `readyToUse`), its instant, and any `status.error.message`. It is written only
   when it changes.

Deleting a workspace deletes its namespace, and with it the `VolumeSnapshot` objects there. Their
contents and the Azure snapshots stay (`Retain`), labelled with the workspace id.

RBAC: `volumesnapshotclasses` get/list/watch/create, `volumesnapshots` get/list/watch/create/delete,
`volumesnapshotcontents` get/list/watch/patch (the label and the `deletionPolicy: Delete` switch of a
pruned snapshot's content), and `storageclasses` get/list/watch for volume growth. The operator never
deletes a class or a content; the snapshot controller deletes a pruned content once it is `Delete`.

## Finding a snapshot

```sh
# A live workspace or the store
kubectl -n <namespace> get volumesnapshot -l orama.cloud/snapshot-target=<workspace id or store>

# A deleted workspace: its retained contents
kubectl get volumesnapshotcontent -l orama.cloud/snapshot-target=<workspace id> \
  -o custom-columns=NAME:.metadata.name,HANDLE:.status.snapshotHandle,SIZE:.status.restoreSize,AT:.metadata.annotations.orama\.cloud/snapshot-at
```

Or ask lake: `volume_snapshots` with `target`.

## Restore runbook (manual)

Nothing here is automated. On a real production cluster, ask before step 4 of B: it deletes a
claim. **Never delete or replace `lake-prod-store`**; restore the store only into a scratch claim.

### A. Restore into a scratch claim and read it

1. Pick a snapshot that is `readyToUse`. If its `VolumeSnapshot` is gone (deleted workspace), bind
   the retained content to a new one first:

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshotContent
   metadata:
     name: restore-<target>-<timestamp>
   spec:
     deletionPolicy: Retain
     driver: disk.csi.azure.com
     volumeSnapshotClassName: lake-retain
     source:
       snapshotHandle: <status.snapshotHandle of the retained content>
     volumeSnapshotRef:
       name: restore-<timestamp>
       namespace: <scratch namespace>
   ---
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata:
     name: restore-<timestamp>
     namespace: <scratch namespace>
   spec:
     volumeSnapshotClassName: lake-retain
     source:
       volumeSnapshotContentName: restore-<target>-<timestamp>
   ```

2. Create the claim from the snapshot, in the snapshot's namespace, with the source's storage
   class and at least its `restoreSize`:

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: restore-scratch
     namespace: <namespace>
   spec:
     accessModes: [ReadWriteOnce]
     storageClassName: default
     resources:
       requests:
         storage: 20Gi
     dataSource:
       apiGroup: snapshot.storage.k8s.io
       kind: VolumeSnapshot
       name: <snapshot name>
   ```

3. Mount it read-only in a pod that passes the namespace's restricted Pod Security profile, and
   read the Zetta data directory (`/data` in a Zetta pod; here `/restore`):

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     name: restore-reader
     namespace: <namespace>
   spec:
     automountServiceAccountToken: false
     securityContext:
       runAsNonRoot: true
       runAsUser: 10001
       runAsGroup: 10001
       fsGroup: 10001
       seccompProfile: {type: RuntimeDefault}
     containers:
       - name: reader
         image: busybox:1.36
         command: ["sh", "-c", "ls -la /restore && du -sh /restore && sleep 3600"]
         securityContext:
           allowPrivilegeEscalation: false
           capabilities: {drop: [ALL]}
         volumeMounts:
           - {name: restore, mountPath: /restore, readOnly: true}
     volumes:
       - name: restore
         persistentVolumeClaim: {claimName: restore-scratch, readOnly: true}
   ```

   A workspace namespace denies egress by default; the pull needs the image in a registry the
   namespace can reach (or a node that already has it). `kubectl -n <namespace> logs restore-reader`
   shows the directory. Delete the pod and the scratch claim when done.

### B. Swap a snapshot into a workspace Zetta

1. Take a fresh snapshot of the current volume first (`request_volume_snapshot`) and wait for
   `ready_to_use`, so the swap is reversible.
2. Stop the workspace in lake (`update_workspace` with `desired: stopped`). The operator deletes the
   workspace's `OramaZettaCluster`; the StatefulSet goes and the claim `data-<cluster>-zetta-0`
   stays.
3. Keep the old disk: `kubectl patch pv <old claim's volume> -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'`.
4. Delete the old claim, then create a claim with the **same name** `data-<cluster>-zetta-0` from
   the snapshot (step A.2, same class, at least the workspace's `storage_gi`).
5. Start the workspace (`desired: running`). The operator recreates the cluster, and its
   StatefulSet adopts the claim by name. The workspace's storage reconcile grows it if
   `storage_gi` is larger.
6. Once the workspace is verified, delete the retained old PV and its Azure disk, or keep them as a
   fallback.

### Removing retained snapshots

The operator never does this. To remove a deleted workspace's snapshots, set each content's
`spec.deletionPolicy` to `Delete` and delete it; the snapshot controller then deletes the Azure
snapshot. Deleting a content with `Retain` removes only the Kubernetes object and leaves the Azure
snapshot billed.

## Verify on dev

The envtests (`TestVolumeSnapshots*` in `internal/controller/lake_volume_snapshots_envtest_test.go`)
run against the upstream snapshot CRDs (external-snapshotter v8.2.0, the version on the dev
cluster). They cover class creation, scheduled creation, pruning to `keep`, one-off requests, status
reporting, a refused `Delete` class and snapshots outliving their workspace. On dev, after the
operator and lake `0069` are deployed on `Lake/prod`:

1. `request_volume_snapshot` for `store` and for one workspace; `volume_snapshots` until both are
   `ready_to_use` (`kubectl get volumesnapshot -A -l app.kubernetes.io/managed-by=orama-operator`).
2. Restore the workspace snapshot into a scratch claim (A), mount it, and read the Zetta data
   directory. Delete the scratch claim.
3. Record the snapshot names, the times to `readyToUse` and the restore in the deployment record.
