# Paid lake.live orders: `LakeOrderSource`

Steve, 2026-10-18: *"this should be hooked into stripe and will deploy with the mgmt server
(operator) upon successful purchase"*
([operator#634](https://github.com/oramasearch/orama-operator/issues/634), part of
[lake#831](https://github.com/oramasearch/lake/issues/831), the lake.live order sheet).

When a customer pays for a **private warehouse** or a **dedicated cluster** on lake.live, the
management cluster's operator provisions it, with no step done by hand. This page is the design:
where the orders are read, how they reach the management cluster, what each offer becomes, how an
order is provisioned exactly once, and what is still open.

Status: implemented and tested locally (envtest, a real Postgres for the SQL). **Not yet run on a
cluster**: there is no `LakeOrderSource` in [`config/clusters/`](../../config/clusters/README.md),
lake#831's migration has not landed, and the port-forward path below has not been exercised against
a real leaf. The open questions are at the end.

## Where the orders are

Lake records each order in its control-plane PostgreSQL, in `cloud_orders` (lake's migration,
lake#831), and each change of state in `cloud_order_events`. Orders are multitenancy and billing, so
they live in the control plane (bridge decision 56), and the operator talks only to the control
plane, never to a workspace Zetta (decision 51): the same rule the managed-services poll follows
(`ServiceStores` in `lake_controller.go`).

The contract, as the operator uses it:

| Column | Operator |
|---|---|
| `id` (uuid), `offer` (`private_warehouse`, `dedicated_cluster`, `on_prem`, `shared`), `cloud_provider`, `region` | reads |
| `state` (`submitted`, `pending_payment`, `paid`, `provisioning`, `ready`, `failed`, `cancelled`) | reads; writes `provisioning`, `ready`, `failed` only |
| `host`, `failure_reason`, `claimed_by`, `claimed_at` | writes |
| everything else (contact, company, Stripe ids, free text, `path`) | never reads or writes |
| `cloud_order_events` (`order_id`, `at`, `from_state`, `to_state`, `actor`, `detail`) | inserts one row per change it makes, with `actor = 'operator'` |

The exact columns are lake's `docs/control-plane/schema.md` once lake#831's pull request lands; both
pull requests use the names above. Until then the operator's test fixture
[`internal/controller/testdata/lake-cloud-orders.sql`](../../internal/controller/testdata/lake-cloud-orders.sql)
holds the contract. While a member's control plane has no `cloud_orders` table, the source says so
and claims nothing.

## How the management cluster's operator reaches a leaf's orders

lake.live runs as the `lake-prod` `LakeSet`: member `shared` on `orama-production-cluster-02`
(`eu.lake.live`) and `shared-us` on `orama-production-cluster-03` (`us.lake.live`). **Each member is
its own Lake with its own control Postgres**, so an order is in whichever region's Lake the customer
used. The management cluster (`orama-production-cluster-01`) runs neither.

The management cluster's operator already holds each leaf's admin kubeconfig: the
`KubernetesCluster` controller writes it to `orama-system/<cluster>-kubeconfig`, and the set and
`OramaOperator` controllers use it to apply resources there. The order poll uses the same kubeconfig:

1. reads the member Lake's control DB Secret on the leaf, `lake-<member>-controldb-app` in the
   member's namespace (its `uri`; the value is used in memory and never logged or written);
2. finds the running primary pod of the Lake's CloudNativePG cluster (`cnpg.io/instanceRole=primary`);
3. opens a **port-forward through the leaf's API server** to that pod's port 5432, on `127.0.0.1` and
   an unused port in the operator's pod: the path `kubectl port-forward` takes;
4. connects with pgx to the forwarded port, with the `uri`'s credentials, and TLS (when the `uri`
   asks for it) checking the Service name on the server's certificate;
5. closes the connection and the forward at the end of the pass.

Why this way:

- **Nothing new is exposed.** The control Postgres keeps no address outside its cluster: no
  `LoadBalancer`, Ingress, firewall rule or peering is added for the operator. The forward rides the
  API server's authenticated TLS connection that the management cluster already uses.
- **The credential is the one already trusted.** The operator uses no Postgres role of its own and
  copies no Secret off the leaf.
- **It is the same code on every cluster.** The poll runs wherever a `LakeOrderSource` exists, and
  only the management cluster has one, because only it holds the kubeconfigs and the cloud identity.

The alternatives considered:

| Alternative | Why not |
|---|---|
| Each leaf's operator polls its own Lake (it can reach Postgres directly, like `ServiceStores`) | A leaf has no cloud identity, so it could never create a dedicated cluster, and an order placed in Europe for a private warehouse in the United States would need the other leaf. The management cluster must act; this would only move the hand-off. |
| The leaf's operator mirrors orders into a resource on the leaf that the management operator reads | Two hops and a second copy of the order state; the atomic claim still has to be in Postgres. |
| Expose each control Postgres to the management cluster (a private endpoint or a peering) | New network surface on a production database for one poll, and per-cluster network setup that a recreated cluster must repeat. |
| Lake calls the management cluster when an order is paid | The operator learns desired state from the control plane, not from calls (decision 51), and a lost call would lose a paid order. |

The cost of the port-forward: a pass depends on the leaf's API server and the kubelet that serves
the primary. When either is down, that member's orders wait (the source reports the member as not
reachable and why); no order is lost, because the row stays `paid`.

## The kind

`LakeOrderSource` is cluster-scoped and exists only on the management cluster. It is configuration,
like a set: the controller is registered on every cluster (principle 11), and a cluster's role is
decided by which resources exist on it.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: LakeOrderSource
metadata:
  name: lake-live
spec:
  lakeSetRef: lake-prod            # read cloud_orders from every member; ordered Lakes copy its template and secrets
  hostDomain: lake.live            # an ordered Lake is served at <lake name>.lake.live
  member:                          # every ordered Lake's own settings, as a set member's spec
    clerk:
      secretKeySecretRef: {name: clerk-secret-key}
    env:
      - {name: LAKE_LOG_MODE, value: air-gapped}
  memberSecrets:
    - {name: clerk-lake-prod, namespace: lake, as: clerk-secret-key}
  dedicatedCluster:
    providerRef: azure-orama                         # the cloud identity
    templateClusterRef: orama-production-cluster-03  # node pools, version, platform and operator to copy
  pollInterval: 30s
```

`member` may not set `host`, `args`, `apexHost`, `region`, `regions`, `namespace` or
`LAKE_PUBLIC_HOST`: the operator sets those for each order. A source that does is `Degraded` and
reads no order. Lake's column limits bound the rest: the source's name is at most 224 characters
(`claimed_by` is at most 255), `hostDomain` at most 234 (`host` is at most 253), and a
`failure_reason` or event detail over 2000 characters is cut to 2000. `kubectl get lorders` shows the counts and the message; `status.orders` lists every
order being provisioned and what it waits for; `status.members` says whether each member's orders
were read.

An ordered Lake's Secrets are its source set's and `memberSecrets`. When the source set is
production, both count as production credentials from the start, before any order: the set copy
never puts one into a Lake on a cluster that is not production, ordered or not
([the rule](../crd-reference.md#production-credentials-never-reach-a-nonprod-member), #790).

## What each offer becomes

Every resource below is made on the management cluster, named `order-<the first 12 hex digits of the
order id>`, labelled `orama.cloud/order-id: <id>` and `orama.cloud/order-source: <source>`, and
annotated `orama.cloud/order-owner`. The existing controllers do the rest.

| Offer | Resources | Ready when |
|---|---|---|
| `private_warehouse` | A `LakeSet` with one member, on the set's cluster whose region is the order's `region` (a cloud region name, such as `swedencentral`). Template and secrets are the source set's; the member spec is `spec.member` plus `host: <name>.<hostDomain>` and `args: [--listen, 0.0.0.0:4900, --node, <name>]`. Its own namespace `lake-<name>`, its own control plane, store and instance key, as every Lake has. | the `LakeSet` is `Ready` |
| `dedicated_cluster` | A `KubernetesCluster` copied from `templateClusterRef` (version, node pools, tags) with the order's region, `providerRef`, `deletionPolicy: Retain` and the tag `order-id`; a copy of every `ClusterHelmRelease` on the template cluster (named `<name>-<releaseName>`, `dependsOn` renamed with them); a copy of its `OramaOperator`; and the `LakeSet` as above, on the new cluster. | the `LakeSet` is `Ready`, which needs the cluster, its platform, its operator and the Lake |
| `on_prem`, `shared`, anything else | nothing | the order fails: lake sends those through the order form |

When the order's `LakeSet` is `Ready`, the order moves to `ready` with `host`. Copying the template
cluster is what makes an ordered cluster's platform and operator identical to every other leaf's
when it is created (the two-installation-paths rule, `cmd/platform_sync_test.go`, holds the template
in step).

**Placed and copied once.** Two things are decided only when an order's resources are first made,
and never again:

- **A dedicated cluster is copied from the template once.** The `ClusterHelmRelease`s and the
  `OramaOperator` are made first and the `KubernetesCluster` last; once the `KubernetesCluster`
  exists, the order is not compared with the template again. A change to the template cluster (an
  operator version, a Kubernetes version, a node pool, a chart) therefore never reaches customers'
  clusters, at once and ungated, through this controller. Moving a dedicated cluster on is a change
  to its own resources, rolled as its own stage (open question 7).
- **A private warehouse stays on the cluster it was placed on.** Its `LakeSet`'s member keeps its
  `clusterRef`. A cluster added to the source set later, in the same region, places only new orders;
  choosing again would give a ready customer a new, empty Lake on another cluster and leave the old
  one, with their data, running unmanaged under `Retain`.

A missing resource of a `provisioning` or `ready` order is made again, and an ordered `LakeSet`
follows the source set's template on every pass: a change to it (a new lake image) reaches ordered
Lakes too, the way it reaches the set's own members (open question 7).

## When an order fails

An order moves to `failed`, with `failure_reason` and an event, only when it cannot be provisioned
as written:

- it names no region, or a `private_warehouse` region where the set has no cluster;
- a `dedicated_cluster` with a cloud provider other than Azure, or a region the operator has no
  location for (`cloudRegionLocations`);
- **this cluster has no cloud identity**: the source names no `dedicatedCluster`, its
  `KubernetesClusterProvider` does not exist here or is of another type, or it was checked and its
  credentials could not be read. So the same operator on a leaf, or on a management cluster whose
  provider was never applied, fails a dedicated-cluster order with that reason rather than skipping
  it;
- a resource with the order's name exists and was made for something else;
- the offer is not one the operator provisions.

Anything that a fix on our side resolves is **not** a failure: the template cluster or its operator
missing, a provider whose credentials are being checked or whose subscription is unreachable, an API
error, a slow cluster. The order stays `provisioning`, and `status.orders[].message` says what it
waits for.

## Exactly once

- **The claim is one statement**: `UPDATE cloud_orders SET state = 'provisioning', claimed_by = …,
  claimed_at = now() WHERE id = … AND state = 'paid'`, with its event row inserted by the same
  statement. Of any number of pollers, only one finds the row still `paid`.
- **The moves on are guarded the same way**: `… WHERE id = … AND state = 'provisioning' AND
  claimed_by = <this source>`. An order another claimant holds is never touched.
- **`claimed_by` names the source, not the pod or the cluster**:
  `orama-operator/LakeOrderSource/<name>`. After a restart, or on a recreated management cluster
  with the same file, the source lists the orders it holds and resumes them.
- **Creation is idempotent**: every resource's name comes from the order id, and one that exists
  with the order's label is never made twice. An ordered `LakeSet` is brought to the described spec
  (keeping its cluster); a dedicated cluster's copies are made once and left as they are.

A consequence to keep: **one management cluster has a given `LakeOrderSource`**. Two management
clusters with the same file would both act on the orders they hold, as two management clusters with
the same `LakeSet` would both manage its Lakes.

## Reproducibility: these resources are not in `config/clusters/`

`config/clusters/` is the record of what a person decided to run. Ordered resources are decided by a
customer's payment, and **their source of truth is the `cloud_orders` row**: the order id, offer,
provider and region, together with the committed `LakeOrderSource` and the template set and cluster
it names. From those alone the operator makes the same resources again, so a recreated management
cluster provisions the same Lakes and clusters from the same rows. Writing each order into a file
would put a second copy of the order state in the repository and a person between payment and
provisioning, which is what lake#831 asks to remove.

So `kubectl diff -R -f config/clusters/` stays empty with ordered resources present: they are not in
those files, and they are found by their label:

```sh
kubectl --context orama-production-cluster-01 get lset,kcluster,chr,ooperator -l orama.cloud/order-id
```

**Nothing is deleted.** The operator never deletes an ordered resource, and the `LakeSet` and
`KubernetesCluster` it makes say `Retain`, so deleting either resource leaves the customer's Lake
and cluster running. Removal is only ever a change of the order's state, and the operator does not
act on `cancelled` yet (below).

## Open questions for Steve

1. **Cancellation.** What a `cancelled` order (or a lapsed Stripe subscription) does to a running
   private warehouse or dedicated cluster: delete it at once, after a grace period, after a final
   backup, or only on a person's approval. Today nothing is removed; the resources stay, labelled.
2. **The host.** Ordered Lakes are served at `order-<id prefix>.lake.live`. Should the customer
   choose a name (`acme.lake.live`), and does a dedicated cluster in a region get a regional name?
3. **Stuck orders.** An order waiting on our side (a cluster that never becomes `Ready`) stays
   `provisioning` indefinitely, with the reason in the source's status. Should it be failed after a
   bound, and should support be told while it waits (lake#831 says a failure goes to support)?
4. **Which cluster a private warehouse lands on** when a region has more than one: today a new order
   goes to the set member's cluster in that region, the first in the set's order. An order already
   placed stays where it is.
5. **Regions for dedicated clusters.** The operator creates them only in regions it has a location
   for (`cloudRegionLocations`: North America and Europe today). Asia, named on the order sheet,
   needs entries there, and a quota check in each region first.
6. **Clerk and billing for ordered Lakes.** `spec.member` gives every ordered Lake lake.live's own
   Clerk instance and the set's Stripe and model-provider keys. Whether a customer's private
   warehouse should share lake.live's sign-in and Orama's Stripe account, or get its own, is open.
7. **Upgrades.** An ordered Lake follows the source set's template on every pass, so a lake release
   reaches it when it reaches lake.live. A dedicated cluster's operator, Kubernetes version, node
   pools and platform do not follow the template after it is made, so nothing upgrades them yet.
   Should customers' instances and clusters follow the dev, testing, staging, prod pipeline as a
   stage of their own, or be pinned per customer?
8. **Size of a dedicated cluster.** It is a copy of the template cluster's node pools. Whether an
   order can choose a size (or accelerators) is for lake#831's order form.

## Code and tests

- Kind: [`api/v1alpha1/lakeordersource_types.go`](../../api/v1alpha1/lakeordersource_types.go).
- Reconciler and planning: [`internal/controller/lake_orders.go`](../../internal/controller/lake_orders.go).
- SQL, the port-forward and the pgx store: [`internal/controller/lake_orders_pgx.go`](../../internal/controller/lake_orders_pgx.go).
- Tests: [`lake_orders_test.go`](../../internal/controller/lake_orders_test.go) runs every scenario
  (each offer `paid` → `provisioning` → `ready` with its host; each failure; no cloud identity;
  four concurrent pollers and a restart; a missing table; a refused source; a ready dedicated
  cluster that does not follow a changed template; a ready warehouse that stays on its cluster when
  the set gains one in its region) against the fake client
  and, with `KUBEBUILDER_ASSETS`, against envtest's API server, which checks every object made
  against its CRD. [`lake_orders_pgx_test.go`](../../internal/controller/lake_orders_pgx_test.go)
  runs the SQL on a real Postgres in a database it creates and drops (`LAKE_ORDERS_TEST_POSTGRES=1`):
  eight connections claiming one order at once, a reconnect, only the holder moving it on, and no
  column but the operator's written.
  Its schema, `testdata/lake-cloud-orders.sql`, is a verbatim copy of lake's 0089_cloud_orders
  migration (the lake commit is named in its header);
  [`lake_orders_contract_test.go`](../../internal/controller/lake_orders_contract_test.go) checks,
  with no database, that every table and column the store's SQL names is in it.
