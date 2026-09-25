# Lake workspaces

How lake gets one Zetta cluster per customer without Kubernetes access: lake appends a row to a
Zetta table on its own data plane, the operator creates a Lake-owned `OramaZettaCluster`, and the
credentials reach lake through one mounted Secret. Field definitions are in the
[`Lake` CRD reference](../crd-reference.md#lake-separate-crd-cluster-scoped--the-lake-service)
(`spec.workspaces`). The pattern copies [Lake deployment environments](environments.md).

## Why

Liquid-bridge decision 45: Zetta has no tenancy, so a customer using lake's Design, Train and Ship
pages needs a Zetta cluster of its own — a **workspace**. lake runs without any Kubernetes
permission. It already holds the admin bearer for its data plane (`LAKE_DATA_PLANE_URL`,
`LAKE_DATA_PLANE_TOKEN`), so the request travels through that Zetta cluster's
`orama_control.workspaces` table, the same way liquid requests deployments through
`orama_control.deployments`.

## What is created

With `spec.workspaces.enabled: true` (and the Zetta data plane enabled):

1. The Lake's data plane, `OramaZettaCluster lake-<lake>-zetta`, is rendered with
   `provisionOperatorSchema: true`. Its schema Job creates the operator databases, including
   `orama_control.workspaces`. The OramaZettaCluster reconciler then polls that table at the Lake's
   `spec.deploySync.interval` (default `15s`), from the same loop that polls deployments
   (`internal/controller/zetta_workspaces.go`). Operator-schema mode also turns on that cluster's
   coarse state sync and topology rows, exactly as on any operator-schema cluster.
2. Each distinct valid `workspace_id` becomes `OramaZettaCluster <lake>-<workspace_id>` in the Lake
   namespace, controlled by the Lake, with:
   - labels `orama.cloud/lake=<lake>`, `orama.cloud/workspace-id=<workspace_id>`,
     `orama.cloud/customer-id=<customer_id>` (a `sha256-<16 hex>` digest when the customer ID is not
     a valid label value), plus `app.kubernetes.io/name=lake-workspace`; the raw customer ID is
     also kept in annotation `orama.cloud/customer-id`;
   - `replicas: 1`, `ingressEnabled: false`, `provisionOperatorSchema: false`, the Lake's
     `pullSecretName`;
   - `image`, `storage` and `resources` from `spec.workspaces` (defaults: OramaZettaCluster image,
     `5Gi`, memory request `64Mi` and limit `1Gi`, no CPU request).
3. The Lake reconciler maintains Secret **`lake-<lake>-workspaces`** in the Lake namespace, owned
   by the Lake. Each key is a `workspace_id`; its value is that workspace's admin token, copied
   from `<lake>-<workspace_id>-zetta-admin` key `token` once the workspace controller mints it.
4. The lake Deployment mounts that Secret read-only at `/var/run/lake/workspaces` (`optional:
   true`, mode `0440`, readable through lake's fsGroup) and receives:

| Env | Value |
|---|---|
| `LAKE_WORKSPACE_DIR` | `/var/run/lake/workspaces` |
| `LAKE_WORKSPACE_URL_TEMPLATE` | `http://<lake>-{workspace}-zetta.<namespace>.svc.cluster.local:4800` |

lake replaces the literal text `{workspace}` with a `workspace_id` to get the workspace's
in-cluster client URL, and reads the bearer from `$LAKE_WORKSPACE_DIR/<workspace_id>`. For
`Lake/prod` in namespace `lake` the template is
`http://prod-{workspace}-zetta.lake.svc.cluster.local:4800`. The client Service name is
`<cluster>-zetta` because that is how the OramaZettaCluster reconciler names every client Service
(`zettaNames`); the Lake's own data plane follows the same rule, which is why its URL is
`lake-<lake>-zetta-zetta`.

The mount is a whole-directory Secret volume, not a `subPath`, so kubelet refreshes it in place:
adding a workspace key never restarts lake. The refresh follows kubelet's sync period (about a
minute), so a `Ready` row can arrive before the file does; lake should retry reading the file
rather than treat its absence as an error. The env vars and volume are rendered only while
workspaces are enabled; enabling or disabling them rolls the lake Deployment once.

## The table contract

Zetta is append-only on the deployed engine, so `orama_control.workspaces` is an event stream.
Every column is `TEXT`:

```sql
CREATE TABLE workspaces (workspace_id TEXT, record_type TEXT, lake TEXT, customer_id TEXT,
                         phase TEXT, url TEXT, message TEXT, updated_at TEXT)
```

| Column | `desired` row (lake writes) | `withdrawn` row (lake writes) | `status` row (operator writes) |
|---|---|---|---|
| `workspace_id` | the slug, `^ws-[0-9a-f]{12}$` | same | same |
| `record_type` | `desired` (empty is read as `desired`) | `withdrawn` | `status` |
| `lake` | the `Lake` resource name | same | same |
| `customer_id` | the owning customer; required | the owning customer; required | copied from the governing row |
| `phase` | empty | empty | `Provisioning`, `Ready`, `Failed` or `Deleted` |
| `url` | empty | empty | the workspace client URL (empty on `Failed` and `Deleted`) |
| `message` | empty | empty | human-readable detail |
| `updated_at` | RFC 3339 | RFC 3339 | RFC 3339 (nanosecond precision) |

Readers reduce each stream latest-wins by `workspace_id` and `updated_at`, per record type. A
workspace is **withdrawn** when its newest `withdrawn` row is newer than its newest `desired` row
(same ordering; on an identical `updated_at` the withdrawal wins). A `desired` row newer than the
withdrawal brings the workspace back. The operator processes a row only when all of these hold
(the locality rule shared with deployments):

- a cluster-scoped `Lake` named by `lake` exists locally;
- that Lake's `spec.namespace` is the namespace of the `OramaZettaCluster` being polled;
- that Lake has `spec.workspaces.enabled` and its Zetta data plane enabled.

Rows failing the rule are skipped without a status row, so a peer in another namespace or another
Kubernetes cluster never creates a second copy. Rows that pass it are then validated; a malformed
row gets one `Failed` status row and nothing is created:

| Check | `Failed` message |
|---|---|
| `workspace_id` does not match `^ws-[0-9a-f]{12}$` | `workspace_id must match ^ws-[0-9a-f]{12}$` |
| `customer_id` empty | `customer_id is required` |
| `<lake>-<workspace_id>-zetta-headless` longer than 63 characters (Lake names over 32 characters) | `lake name "<lake>" is too long for workspace cluster names` |
| a cluster of that name exists but is not this Lake's workspace | `OramaZettaCluster <ns>/<name> exists and is not this Lake's workspace` |
| the workspace already belongs to a different `customer_id` | `workspace_id is already bound to another customer` |
| (new workspace only) a live data PVC of that name exists without its cluster | `PersistentVolumeClaim <ns>/<pvc> exists and is not this Lake's workspace` |
| (new workspace only) the Lake already has `spec.workspaces.maxWorkspaces` live workspaces | `workspace quota reached (<n>)` |

The same checks apply to a `withdrawn` row (except the quota), and a refused withdrawal deletes
nothing.

Status rows are appended only when phase, URL or message differs from the newest status row
already in the table, so a steady workspace writes nothing and the anti-flap survives operator
restarts. The phases:

| Phase | When | Message |
|---|---|---|
| `Provisioning` | the cluster exists and its node is not ready | `waiting for the workspace Zetta node` |
| `Provisioning` | the node is ready but its token is not yet in `lake-<lake>-workspaces` | `waiting for the workspace credential` |
| `Provisioning` | a re-desired workspace's previous cluster is still terminating | `waiting for the previous workspace cluster to be deleted` |
| `Provisioning` | a re-desired workspace's previous data PVC is still terminating | `waiting for the previous workspace data volume to be deleted` |
| `Ready` | the node is ready and the token is in the Secret | `workspace ready` |
| `Failed` | validation, ownership or the quota refused the row | see the table above |
| `Deleted` | a withdrawal finished: no cluster, no data PVC, no credential key | `workspace deleted` |

Polling is idempotent. Re-reading the same rows, or a newer desired row for the same
`workspace_id`, creates nothing new: the cluster's name is derived from the Lake and the
`workspace_id`, and the operator writes only the spec fields it owns (`replicas`, `image`,
`pullSecretName`, `storage`, `resources`, `ingressEnabled`, `provisionOperatorSchema`), so
server-side defaults on the rest of the spec do not cause an update on every poll. Changing
`spec.workspaces.image`, `storage` or `resources` updates every existing workspace on its next poll.

## Withdrawal (deletion)

**Withdrawal deletes the workspace's data. It is irreversible:** a workspace has one node and no
backup, and its data PVC is deleted, so the storage class's reclaim policy decides whether the
underlying disk survives (with the usual `Delete` policy it does not). A later `desired` row
creates an empty workspace, never the old data.

lake withdraws a workspace by appending a row with `record_type = 'withdrawn'` and the same
columns as a desired row (`workspace_id`, `lake`, `customer_id`, `updated_at`; `phase`, `url` and
`message` empty):

```sql
INSERT INTO workspaces (workspace_id, record_type, lake, customer_id, phase, url, message, updated_at)
VALUES ('ws-0123456789ab', 'withdrawn', 'prod', 'cus_test', '', '', '', '2026-09-25T12:00:00Z')
```

When the withdrawal is newer than the newest desired row, each poll, before any desired row is
processed:

1. applies the locality rule and the validation checks above; a refused row gets one `Failed`
   status row and nothing is deleted;
2. reads `OramaZettaCluster <lake>-<workspace_id>`. If it exists it must be controlled by this Lake
   and labelled `orama.cloud/lake=<lake>` and `orama.cloud/workspace-id=<workspace_id>`, and its
   `orama.cloud/customer-id` annotation must equal the row's `customer_id`; otherwise the row is
   refused as above. Clusters of another Lake, unlabelled clusters, and clusters not created by
   the workspace reconciler are never touched;
3. deletes the cluster's data PVCs, **then** the cluster (background propagation, so its
   StatefulSet, pods, Services and Secrets are garbage-collected). The PVCs are the ones the
   cluster's StatefulSet created from its `data` volumeClaimTemplate: named
   **`data-<lake>-<workspace_id>-zetta-<ordinal>`** (a workspace has one replica, so
   `data-<lake>-<workspace_id>-zetta-0`) and carrying that StatefulSet's selector labels
   (`app.kubernetes.io/name=orama-zetta`, `app.kubernetes.io/instance=<lake>-<workspace_id>`).
   A StatefulSet never deletes these itself. Deleting them first means an interrupted pass never
   leaves a live volume behind a cluster that is already gone. If the cluster is already gone, a
   leftover live PVC is deleted only when the newest status row carries a URL (proof that this
   reconciler materialized the workspace); otherwise the row is refused;
4. removes the `<workspace_id>` key from `lake-<lake>-workspaces` (the Lake reconciler skips
   clusters that are gone or being deleted, so it does not put the key back);
5. once the cluster and every data PVC are gone (the PVC waits for its pod to terminate), appends
   one `status` row with phase **`Deleted`**, message `workspace deleted` and an empty URL.

Every step is repeated on each poll until the `Deleted` row is written, and the transition rule
keeps a finished withdrawal silent, so the path is idempotent. A withdrawal of a workspace that
never existed writes `Deleted` at once.

**Re-desire.** A `desired` row newer than the withdrawal recreates the workspace from scratch: the
operator waits (`Provisioning`, see the phase table) while the old cluster or its data PVC is still
terminating, so the new StatefulSet never binds the old volume, then creates a fresh cluster whose
new admin token reaches `lake-<lake>-workspaces` as usual. lake must read the new token rather than
cache the old one.

**Other ways a workspace goes away.** Disabling `spec.workspaces` stops intake of both desired and
withdrawn rows and removes the env and volume from lake, but deletes nothing. Deleting the Lake
removes every workspace: the Lake finalizer deletes every OramaZettaCluster the Lake controls, and
Kubernetes garbage-collects the Secret (their data PVCs are not deleted by that path). The Lake's
`spec.zetta` reconcile skips workspace clusters, so turning the data plane off does not delete them.

## Quota

`spec.workspaces.maxWorkspaces` (`int32`, `0` or unset = unlimited) caps a Lake's live workspaces:
clusters controlled by the Lake and labelled as its workspaces, excluding those being deleted and
those withdrawn in the current stream. Withdrawals are processed before desired rows, so a
withdrawal frees its slot in the same poll. A desired row for a **new** workspace (no cluster yet)
past the cap creates nothing and gets a `Failed` status row `workspace quota reached (<n>)`, where
`<n>` is the cap; the row is written on transition only, and the workspace is created on a later
poll once a slot frees or the cap rises. Existing workspaces are never removed or refused by the
quota, including after the cap is lowered below the current count.

## Known limits

- lake receives each workspace's **admin** token. A per-service Zetta role for lake is the
  least-privilege follow-up once Zetta supports scoped users.
- The Zetta binary receives the memory limit only as a cgroup limit; there is no Zetta memory
  setting derived from `spec.workspaces.resources` yet.
- A workspace has one node and no backup; its data is recoverable only from its volume, and a
  withdrawal deletes that volume. There is no soft-delete or retention window.
- A withdrawal waits for the cluster's pod to terminate before its PVC disappears; a PVC stuck
  terminating holds the `Deleted` row (and a re-desire) until it clears.

## How to verify

```sh
# The data plane carries the operator schema and the workspaces table.
kubectl get oramazettacluster lake-prod-zetta -n lake -o jsonpath='{.spec.provisionOperatorSchema}'
kubectl logs -n lake job/lake-prod-zetta-zetta-schema | grep workspaces

# Append a desired row through the data plane (token from lake-prod-zetta-zetta-admin).
TOKEN=$(kubectl get secret lake-prod-zetta-zetta-admin -n lake -o jsonpath='{.data.token}' | base64 -d)
kubectl run -n lake ws-probe --rm -i --restart=Never --image=curlimages/curl:8.10.1 -- \
  curl -sG -H "Authorization: Bearer $TOKEN" \
  http://lake-prod-zetta-zetta.lake.svc.cluster.local:4800/v1/sql \
  --data-urlencode database=orama_control \
  --data-urlencode "statement=INSERT INTO workspaces (workspace_id, record_type, lake, customer_id, updated_at) VALUES ('ws-0123456789ab', 'desired', 'prod', 'cus_test', '$(date -u +%Y-%m-%dT%H:%M:%SZ)')"

# Within one poll interval: the cluster, then the credential, then a Ready status row.
kubectl get oramazettacluster -n lake -l orama.cloud/lake=prod
kubectl get secret lake-prod-workspaces -n lake -o jsonpath='{.data}' | jq 'keys'
kubectl exec -n lake deploy/lake-prod -- ls /var/run/lake/workspaces
```

Query the status stream with `SELECT workspace_id, phase, url, message, updated_at FROM workspaces
WHERE record_type = 'status'` through the same `/v1/sql` call.

Withdrawal (destroys the probe workspace's data) uses the same call with `record_type` `withdrawn`
and a newer `updated_at`; within a poll or two the cluster, `data-prod-ws-0123456789ab-zetta-0`
and the Secret key are gone and a `Deleted` status row appears:

```sh
kubectl get oramazettacluster,pvc -n lake -l app.kubernetes.io/instance=prod-ws-0123456789ab
kubectl get oramazettacluster -n lake prod-ws-0123456789ab   # NotFound
```

The unit tests are `internal/controller/zetta_workspaces_test.go`: table DDL, SQL quoting, the
materialization lifecycle (spec, owner, labels, idempotent re-poll, `Provisioning` then `Ready`
only on transition), the locality rule, invalid rows, customer rebinding, Secret aggregation, the
lake Deployment env and volume appearing only while enabled, withdrawal ordering in the reducer,
withdrawal deleting the cluster, data PVC and Secret key and writing `Deleted` once, re-desire
after withdrawal waiting for the terminating cluster and volume and then recreating, withdrawal
leaving another Lake's, unlabelled or unowned clusters and unproven PVCs alone, and the quota
refusing the n+1th workspace while a lowered quota keeps existing ones.

## How to change

- **Row contract.** Adding a column needs a new table (Zetta has no `ALTER TABLE`); change
  `operatorZettaSchema` in `internal/controller/zetta_controlplane.go`, the reader in
  `zetta_workspaces.go`, lake's writer, and this page together.
- **Workspace cluster shape.** Edit the mutate function in `reconcileZettaWorkspace`; keep writing
  only named spec fields so re-polls stay no-ops.
- **Credentials or URL.** The Secret name, key format, mount path and URL template are a contract
  with lake; change them only together with lake's reader.
- **Deletion.** `withdrawZettaWorkspace` in `zetta_workspaces.go`. Keep the ownership check in
  front of every delete, PVCs before the cluster, and `Deleted` only once nothing remains. A
  retention window (keep the volume for N days) would need a new phase and a decision with lake.
- **PVC naming.** `workspaceDataClaims` derives the claim names from the OramaZettaCluster
  StatefulSet name (`zettaNames`) and its `data` volumeClaimTemplate; renaming either changes
  which volumes a withdrawal finds.
