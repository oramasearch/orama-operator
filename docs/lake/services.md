# Lake managed services

How a lake customer gets its own Zetta and Postgres clusters without lake having any Kubernetes
access: lake writes the `services` table in its control Postgres, the operator creates one
Lake-owned resource per service, writes what it observed to `service_status`, and hands lake each
service's connection through one mounted Secret. The authoritative contract is lake's
[`docs/control-plane/services.md`](https://github.com/oramasearch/lake/blob/a8d09dda036a7af9857cfa4f4b21ef9ab57b7a3d/docs/control-plane/services.md)
(liquid-bridge decision 46); this page is the operator's side of it. Field definitions are in the
[`Lake` CRD reference](../crd-reference.md#lake-separate-crd-cluster-scoped--the-lake-service)
(`spec.services`). It supersedes [workspaces](workspaces.md): a customer's workspace is now its
**default Zetta service**.

## Turning it on

```yaml
spec:
  services:
    enabled: true
    maxServices: 20          # 0 = no limit
    zettaImage: oramasearch/zetta:<tag>
```

Services need the control-plane Postgres (`spec.postgres.enabled`, the default); they do not need
the Zetta data plane. `spec.workspaces` is a deprecated alias: `workspaces.enabled` turns services
on, `workspaces.image` is the Zetta image when `services.zettaImage` is empty, and
`workspaces.maxWorkspaces` is the cap when `services.maxServices` is `0`. `workspaces.resources`
and `workspaces.storage.storageClassName` still shape every Zetta service's nodes.

## The poll

The Lake reconciler polls, once the Lake's schema has applied (the tables come with migration
`0024_services`), at the Lake's `spec.deploySync.interval` (default `15s`, floor `1s`) — the
cadence the workspace poll used. Each poll (`internal/controller/lake_services.go`):

1. reads Secret `lake-<lake>-controldb-app` key `uri` (the CloudNativePG application Secret of
   the Lake's control DB). A missing Secret or an empty `uri` means "not yet": it is logged and the
   poll ends. It connects with pgx (one connection per poll, closed at the end, 30-second bound on
   the whole pass).
2. reads every service joined to its status, oldest first, leaving out history (`desired =
   'absent'` with phase `Deleted`).
3. handles **absent** services first, so a deletion frees its quota slot in the same poll;
4. counts the live services for the quota;
5. creates or updates each **present** service;
6. writes the credentials Secret;
7. upserts `service_status` for each service whose phase, message, endpoint or observed
   generation changed. A steady service writes nothing.

Nothing in the poll fails the Lake reconcile: an unreachable database or a failed Kubernetes call
is logged and retried on the next poll. The store is the Go interface `LakeServiceStore` (read
services, upsert status); `lake_services_pgx.go` is the pgx implementation, and tests substitute
an in-memory fake through `LakeReconciler.ServiceStores`.

## Present services

Every service becomes **`<lake>-<slug>`** in the Lake namespace, controlled by the Lake, with
labels `orama.cloud/lake=<lake>`, `orama.cloud/service-id=<id>`, `orama.cloud/service-kind=<kind>`,
`orama.cloud/customer-id` (a `sha256-…` digest when the id is not a valid label value) and
`app.kubernetes.io/name=lake-service`, and annotation `orama.cloud/customer-id=<customer_id>`.

| | Zetta (`kind = 'zetta'`) | Postgres (`kind = 'postgres'`) |
|---|---|---|
| Resource | `OramaZettaCluster` | CloudNativePG `Cluster` (`postgresql.cnpg.io/v1`, unstructured, as `controldb.go` does) |
| From `spec.instances` | `replicas` | `instances` |
| From `spec.storage_gi` | `storage.size` `<n>Gi`, class from `spec.workspaces.storage.storageClassName` | `storage.size` `<n>Gi`, class from `spec.postgres.storageClassName` |
| Other fields | image `spec.services.zettaImage` (then `spec.workspaces.image`), the Lake's `pullSecretName`, `resources` from `spec.workspaces.resources` (default memory `64Mi`/`1Gi`, no CPU request), no ingress, no operator schema | `bootstrap.initdb` database `app`, owner `app` |
| Ready when | status phase `Ready` and `readyNodes` ≥ instances | `status.readyInstances` = instances |
| Credential source | `<cluster>-zetta-admin` key `token` | the whole CloudNativePG `<cluster>-app` Secret |
| `endpoint` | `http://<cluster>-zetta.<ns>.svc.cluster.local:4800` | `<cluster>-rw.<ns>.svc.cluster.local:5432` |

`spec` defaults are `instances: 1` and `storage_gi: 10`; values outside 1–3 and 1–100 fail the
service. The data volume and the Postgres bootstrap are written only when the resource is created:
a StatefulSet's volume template is immutable and the contract has no resize. Only named fields are
written, so server-side defaults never turn a poll into an update.

### Adoption of workspace clusters

A decision-45 workspace cluster — `OramaZettaCluster <lake>-ws-<12 hex>`, controlled by the Lake,
labelled `orama.cloud/lake=<lake>` and `orama.cloud/workspace-id=<slug>`, with no service-id label,
and whose customer annotation (if any) matches — is **adopted** by the customer's default Zetta
service with that slug. Adoption adds the service labels (the `workspace-id` label stays), keeps the
existing data volume and its size, and is not subject to the quota, so an existing workspace keeps
its data.

Anything else already holding the name fails the service and is never modified or deleted:

| Case | `Failed` message |
|---|---|
| a cluster of that name that is not this Lake's service (another controller, another Lake, another service id, or a workspace cluster claimed by a non-default service) | `OramaZettaCluster <ns>/<name> exists and is not this Lake's service`, or `CloudNativePG Cluster <ns>/<name> exists and is not this Lake's service` |
| a new Zetta service whose data PVC `data-<cluster>-zetta-<n>` already exists without its cluster | `PersistentVolumeClaim <ns>/<pvc> exists and is not this Lake's service` |
| a malformed row (slug, kind, default on Postgres, missing customer, Lake name over 31 characters, bad spec) | the specific reason, e.g. `spec.instances must be between 1 and 3` |

## Credentials

Secret **`lake-<lake>-services`** (controlled by the Lake) holds one key per Ready service,
**`<slug>.json`**:

```json
{"kind":"zetta","endpoint":"http://prod-svc-0123456789ab-zetta.lake.svc.cluster.local:4800","token":"<admin token>"}
{"kind":"postgres","host":"prod-svc-0123456789ab-rw.lake.svc.cluster.local","port":5432,"database":"app","user":"app","password":"…","uri":"postgresql://app:…@prod-svc-0123456789ab-rw.lake:5432/app"}
```

`uri` is CloudNativePG's own, copied unchanged. A present service keeps its key while its
connection cannot be read, so a transient failure never removes a file lake depends on; the key
of an absent service, or of a row that no longer exists, is dropped.

The lake Deployment mounts the Secret read-only at **`/var/run/lake/services`** (volume
`services`, `optional: true`, mode `0440`) and gets **`LAKE_SERVICE_DIR=/var/run/lake/services`**.
It no longer gets `LAKE_WORKSPACE_DIR` or `LAKE_WORKSPACE_URL_TEMPLATE`: lake reads endpoints from
`service_status`. `LAKE_DATA_PLANE_URL` and `LAKE_DATA_PLANE_TOKEN` are unchanged; they are lake's
own data plane. kubelet refreshes a whole-directory Secret volume in place (about a minute), so a
new key never restarts lake, and lake should retry a missing file rather than fail. The superseded
Secret `lake-<lake>-workspaces` is deleted once services run.

## Status

| Phase | When | Message |
|---|---|---|
| `Provisioning` | the resource exists and is not ready | `waiting for the Zetta nodes` / `waiting for the Postgres instances` |
| `Provisioning` | the resource is ready but its credential is not yet in the Secret | `waiting for the service credential` |
| `Provisioning` | a previous resource or data volume of that name is still terminating | `waiting for the previous … to be deleted` |
| `Ready` | the resource is ready **and** its credential was written in this poll | `service ready` |
| `Failed` | ownership, validation or the quota refused the service | see above; quota: exactly `service quota reached (<n>)` |
| `Deleting` | an absent service's resources are still going | `deleting the Zetta cluster`, `deleting the Zetta data volumes`, `deleting the Postgres cluster`, `deleting the Postgres data volumes` |
| `Deleted` | nothing remains | `service deleted` |

`endpoint` is written once the resource exists and is this service's (it is empty on `Failed` and
`Deleted`, and kept while `Deleting`); that is also the proof the deletion path uses for leftover
volumes. `observed_generation` is the `generation` of the row the phase describes.

## Absent services (deletion)

**Deleting a service deletes its data, irreversibly.** For each absent service not yet `Deleted`:

- **Zetta:** check ownership (as above, a not-yet-adopted workspace cluster of a default service
  counts as the service's); delete the data PVCs `data-<cluster>-zetta-<n>`, **then** the cluster
  (background propagation). If the cluster is already gone, a leftover live PVC is deleted only
  when the service's status carries an endpoint.
- **Postgres:** check ownership; delete the CloudNativePG `Cluster`. Its PVCs (label
  `cnpg.io/cluster=<cluster>`) are owned by it and garbage-collected.
- remove `<slug>.json` from `lake-<lake>-services`;
- write `Deleted` only once the resource and every data PVC are gone; until then `Deleting`, and
  the next poll repeats the steps.

A resource that fails the ownership rule is never deleted; the service is `Failed` instead.

## Quota

`spec.services.maxServices` (falling back to `spec.workspaces.maxWorkspaces`; `0` = no limit) caps
the live services: Lake-controlled `OramaZettaCluster`s and CloudNativePG `Cluster`s labelled
`orama.cloud/lake=<lake>` with a service-id (or a workspace-id, for unadopted workspaces), not being
deleted and not absent in the current read. A **new** service over the cap creates nothing and is
`Failed` / `service quota reached (<n>)`; it starts on a later poll once there is room. An existing
service — including an adopted workspace — is never refused or removed, even after the cap is
lowered. lake's own per-customer cap (`LAKE_SERVICES_PER_CUSTOMER`) is separate.

## Other ways a service goes away

Disabling services stops the poll and removes the mount and env; it deletes nothing. Deleting the
Lake deletes every Lake-controlled `OramaZettaCluster` through the Lake finalizer and
garbage-collects the CloudNativePG Clusters and the Secret; Zetta data PVCs are not deleted by that
path. The Lake's `spec.zetta` reconcile skips service clusters.

## RBAC

Nothing new: the manager already has `postgresql.cnpg.io/clusters`, `secrets`,
`persistentvolumeclaims` (including `delete`) and `oramazettaclusters`, both as kubebuilder markers
and in the chart's hand-maintained ClusterRole.

## Known limits

- lake receives each Zetta service's **admin** token, and the operator connects to lake's control
  Postgres with lake's own owner credential (decision 46 explains why a narrower role was not worth
  it yet).
- Postgres services get CloudNativePG's default resources and no backup; Zetta services have no
  backup either. Endpoints are in-cluster only.
- `spec` cannot change after creation.
- Services whose rows disappear without passing through `absent` (lake's `services.customer_id`
  cascades on customer deletion) lose their credential key but their resources are not deleted:
  the operator deletes only what a row marked absent.

## How to verify

```sh
go test ./internal/controller/ -run Services
# The pgx SQL against the vendored schema on a local Postgres, inside a rolled-back transaction:
LAKE_SERVICES_TEST_POSTGRES=1 go test ./internal/controller/ -run PgxStore

# On a cluster, after creating a service on lake's /services page:
kubectl -n lake get oramazettacluster,clusters.postgresql.cnpg.io -l orama.cloud/lake=prod
kubectl -n lake get secret lake-prod-services -o jsonpath='{.data}' | jq 'keys'
kubectl -n lake exec deploy/lake-prod -- ls /var/run/lake/services
```

The unit tests (`lake_services_test.go`) cover Zetta and Postgres creation, Ready gated on the
credential, adoption of a `ws-` cluster keeping its data, refusal of foreign resources and
unproven volumes, the deletion order and `Deleted`, the quota message and the fact that it never
refuses existing services, status upserts only on change, the "not yet" app Secret, and the lake
Deployment's mount and env.

## How to change

- **The contract** (tables, phases, messages, Secret name and key format, labels): change it in
  lake's `docs/control-plane/services.md` first, then here and in `lake_services.go` together.
- **Resource shape**: the mutate functions in `reconcileZettaService` and
  `reconcilePostgresService`; keep writing named fields only.
- **Deletion**: `deleteLakeService`; keep the ownership check before every delete, Zetta PVCs
  before the cluster, and `Deleted` only once nothing remains.
- **Schema**: the tables arrive with lake's `db/schema.sql`; see [schema.md](schema.md).
