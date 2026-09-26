# Lake customer stores (lake#196)

A lake customer's **stores** — buckets, filesystems, network filesystems and volumes — follow the
[managed-services](services.md) model: lake writes the `stores` table in its control Postgres and
never talks to Kubernetes; the operator makes each store exist, hands lake its connection through
the same mounted Secret as the services, and writes what it observed to `store_status`. The
authoritative contract is lake's
[`docs/control-plane/storage.md`](https://github.com/oramasearch/lake/blob/9aa61410749a092b468b78abb8e5b1aa3f2aa0cb/docs/control-plane/storage.md)
(migration `0028_stores`; lake branch `feat/storage`); this page is the operator's side of it.

These are a customer's stores. The Lake's own libliquid store root (`spec.store`, claim
`lake-<lake>-store`) is a different thing and is not touched here.

**Phase 1 runs one type × provider pair: the cluster volume** (`volume` / `cluster`). Every other
pair is `Failed` with `<provider> <type> stores are not available on this lake yet`; lake does not
offer them, so that only guards a row written some other way. Phases 2–4 of lake#196 add them.

## The pass

Stores need nothing of their own to turn on: they ride the services poll
(`spec.services.enabled`, [the poll](services.md#the-poll)), on the same pgx connection, and the
same `NOTIFY lake_services` wakes the Lake (lake notifies that channel on every store insert and
withdrawal, so `LakeServicesListener` is unchanged). In `syncLakeServices`, after the services
(`internal/controller/lake_stores.go`):

1. the stores are read joined to `store_status`, oldest first, leaving out history (`desired =
   'absent'` with phase `Deleted`). **A schema before `0028_stores`** (no `stores` table,
   SQLSTATE `42P01`) is logged as `stores skipped` and the services carry on; any other read
   failure is logged and the pass keeps every `st-…` key the Secret has, since a failed read must
   never remove a connection lake depends on;
2. **absent** stores are deleted, then **present** stores are created;
3. the services Secret is written once, with the services' and the stores' connections;
4. `store_status` is upserted for each store whose phase, message, endpoint or observed generation
   changed. A steady store writes nothing.

An absent store not yet `Deleted`, and a `Provisioning` store, keep the Lake on the 2-second poll
(`lakeServicesChangingPoll`), exactly as a service does.

## Cluster volume

A present `volume` / `cluster` store becomes a **PersistentVolumeClaim `<lake>-<slug>`** in the
Lake's namespace (`<slug>` is `st-` plus 12 hex digits, so it never collides with a service's
`svc-…` or `ws-…`):

| Field | Value |
|---|---|
| access mode | `ReadWriteOnce` |
| size | `spec.size_gi` GiB, default `10`, `1`–`1024` (anything else is `Failed`) |
| StorageClass | the Lake's services' class, `spec.workspaces.storage.storageClassName` (the one every Zetta service's data volumes use); empty leaves it to the cluster default |
| labels | the Lake's labels (`app.kubernetes.io/name: lake`, `instance`, `managed-by`) plus `orama.cloud/lake: <lake>` and **`orama.cloud/store-id: <id>`** |
| annotation | `orama.cloud/customer-id: <customer id>` |
| owner | the Lake, as controller |

The claim is created once and never rewritten: a later spec never shrinks it (or grows it; lake
has no resize in phase 1). A claim of that name that is not controlled by the Lake or does not carry
this store's id is refused: `Failed`, `PersistentVolumeClaim <ns>/<name> exists and is not this
Lake's store`. `spec.region` and `spec.public` mean nothing to a cluster volume.

The store is **Ready as soon as the claim exists and its connection is in the Secret**. AKS's
`managed-csi` is `WaitForFirstConsumer`, so the claim stays `Pending` until a service mounts it;
that is the expected state, and the message says so.

## The connection

`<slug>.json` in Secret `lake-<lake>-services`, beside the services' connections, written by the
same writer (`reconcileLakeServicesSecret`) before the store is `Ready`:

```json
{"type":"volume","provider":"cluster","claim":"prod-st-0123456789ab","namespace":"lake","size_gi":10,"storage_class":"managed-csi"}
```

`size_gi` and `storage_class` are read from the claim itself: its requested size, and the class it
carries — the one asked for, or the default the API server's admission chose. `storage_class` is
`""` only when the cluster has no default class.

## Status

| Phase | When | Message |
|---|---|---|
| `Provisioning` | the claim exists but its connection is not yet in the Secret | `delivering the connection` |
| `Provisioning` | a previous claim of that name is still terminating | `waiting for the previous volume to be deleted` |
| `Ready` | the claim exists and is `Pending` (not yet mounted) | `created; binds when a service mounts it` |
| `Ready` | the claim is `Bound` | `bound` |
| `Failed` | a foreign claim, an invalid row, or an unavailable pair | see above |
| `Deleting` | an absent store's claim is still going | `deleting the volume` |
| `Deleted` | the claim is gone | `store deleted` |

`endpoint` is the claim as `<namespace>/<name>` (a volume has no network address; the claim is how
a service mounts it), empty on `Failed` and `Deleted`. `observed_generation` is the row's
`generation`.

## Absent stores (deletion)

**Deleting a volume store deletes its data, irreversibly** (lake asks for the store's name as a
typed confirmation first). For each absent store not yet `Deleted`: delete the claim if it is this
Lake's for this store, drop `<slug>.json`, and write `Deleting` while the claim remains (a mounted
claim is held by `kubernetes.io/pvc-protection` until its pod goes), then `Deleted`. A claim of that
name that is not the store's is never deleted: the store is `Deleted` with the message
`store deleted; PersistentVolumeClaim <ns>/<name> exists and is not this Lake's store and was left
in place`. A store of an unavailable pair has nothing to delete but a connection. A `Deleted` store
is history and is skipped. Deleting the Lake garbage-collects the claims it controls.

## RBAC

Nothing new: the manager already has `persistentvolumeclaims` `get, list, watch, create, update,
patch, delete` (the kubebuilder marker in `lake_controller.go` and the chart ClusterRole).

## Schema

The vendored schema is lake `db/schema.sql` at `9aa6141`, and the Lake's PostgresSchema guard is
`stores_guard`, the completion table of `0028_stores` ([schema.md](schema.md)).

## How to verify

```sh
go test ./internal/controller/ -run Stores
# The pgx SQL against the vendored schema on a local Postgres, inside a rolled-back transaction:
LAKE_SERVICES_TEST_POSTGRES=1 go test ./internal/controller/ -run PgxStore

# On a cluster, after creating a volume on lake's Train → Data → Storage page:
kubectl -n lake get pvc -l orama.cloud/store-id
kubectl -n lake get secret lake-prod-services -o jsonpath='{.data}' | jq 'keys'
```

`lake_stores_test.go` covers a volume going `Provisioning` (connection not deliverable) to `Ready`
with its exact connection, the claim's size, access mode, class, labels and owner, the default
size and cluster-default class, a smaller spec never shrinking the claim, `bound`, a steady pass
writing nothing, a foreign claim refused and left in place on deletion, an unavailable pair's
message, deletion through `Deleting` (claim held by its finalizer) to `Deleted` and then skipped,
the fast poll while provisioning or deleting and not once settled, services and stores sharing the
Secret, a failed stores read keeping store keys, a missing `stores` table leaving services working,
and invalid rows. `lake_stores_pgx_test.go` (opt-in) runs the SQL, including the missing-table
error, on a real Postgres.

## How to change

- **The contract**: change lake's `docs/control-plane/storage.md` first, then here and
  `lake_stores.go` together.
- **A new pair** (phases 2–4): extend `storeAvailable`, add its reconcile and delete beside
  `reconcileVolumeStore` / `deleteCustomerStore`, its connection shape, and its RBAC.
