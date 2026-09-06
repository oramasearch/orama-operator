# Zetta as the operator's datastore (One Orama symbiosis v0)

The operator uses an **Orama Zetta** node as its own datastore. Once an `OramaZettaCluster` is up, the
operator provisions its **control-plane** and **data-plane** databases + tables on that node and can
read/write them over Zetta's SQL surface.

This is **stage 2** of [One Orama](https://github.com/oramasearch/orama-bridge/tree/main/docs/projects/one-orama)
(epic [orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)) — the *symbiotic*
half of "operator ↔ Zetta": the operator provisions Zetta ([`OramaZettaCluster`](crd-reference.md)),
and Zetta becomes the operator's store. The endgame is to retire v1's Postgres / ClickHouse / Typesense
and keep everything in Zetta.

## The schema

Provisioned automatically when `spec.provisionOperatorSchema` is `true` (the default):

| database | table | purpose |
|---|---|---|
| `orama_control` | `components` | the operator's registry — one row per managed component (`name, kind, image, status, node, updated_at`). |
| `orama_control` | `reconcile_events` | an audit of operator actions (`ts, cluster, action, detail`). |
| `orama_control` | `topology` | the cross-cluster Zetta node topology ([operator#105](https://github.com/oramasearch/orama-cloud-v1-operator/issues/105)) — one row per wan-accessible node (`operator, cluster, node, address, reachability, node_type, status, updated_at`). Each operator WRITES its slice's wan endpoints; Zetta CRDT-syncs the table; each operator READS it back to derive cross-cluster peering. See [Cross-cluster topology through Zetta](#cross-cluster-topology-through-zetta). |
| `orama_control` | `deployments` | append-only desired deployment and observed status events ([operator#124](https://github.com/oramasearch/orama-cloud-v1-operator/issues/124)). A desired event becomes a Lake-owned `LakeEnvironment`; status events carry phase, URL and message only on transition. See [Lake deployment environments](lake/environments.md). |
| `orama_control` | `deployment_records` | immutable records for liquid's public create/list/rollback/status contract, including result, rollback origin and sealed-secret references. Kept separate so existing append-only `deployments` tables need no unsupported column migration. See the [deployment control plane](control-plane/README.md). |
| `orama_data` | `documents` | the application-data landing zone (`id, collection, body, updated_at`) — where v1's served data migrates to. |

The definitive list is `operatorZettaSchema` in
[`internal/controller/zetta_controlplane.go`](../internal/controller/zetta_controlplane.go).

## How the operator provisions it

The operator runs **locally** (against the kube-context), so it can't reach the node's ClusterIP
directly. It uses the same mechanism as the control-DB migration ([`controldb.go`](../internal/controller/controldb.go)):
an **in-cluster Job** (`<name>-zetta-schema`, a `curl` image) that

1. waits for the node (`GET /v1/server_info`),
2. applies each statement idempotently to **`GET /v1/sql`** (see [Zetta SQL notes](#zetta-sql-notes)),
3. completes.

The Job is owner-ref'd to the `OramaZettaCluster` (GC'd with it) and carries a content hash of the
rendered schema. When `operatorZettaSchema` changes, the controller deletes the immutable old Job and
recreates it automatically; existing tables return "already exists" and only new statements apply.

```
OramaZettaCluster ──owns──▶ StatefulSet (zetta node, :4800 /v1/sql)
        │
        └──owns──▶ ConfigMap <name>-zetta-schema (apply.sh)
        └──owns──▶ Job       <name>-zetta-schema  ──GET /v1/sql──▶ orama_control, orama_data
```

## Zetta SQL notes

Zetta speaks a **Postgres subset** over `/v1/sql`. What the operator relies on:

- **Supported statements: `CREATE` / `DROP` / `INSERT` / `SELECT` only** — **no `UPDATE`, no `DELETE`.**
  Row data is **append-only**. Keeping a record "in sync" therefore means **append a new row + read the
  latest** (event-sourcing / latest-wins), not update-in-place — which suits Zetta's CRDT model. (An
  `UPDATE`/`DELETE` subset is future engine work.)
- **No `IF NOT EXISTS`.** Re-creating errors with `… already exists …`, so the apply script runs each
  statement and **treats "already exists" as success** (that's the idempotency contract).
- **One statement per call.** The `sql` capability runs a single statement; the script applies them one
  at a time, each with an optional `database` (default database for unqualified names).
- **`system.*` views** are read-only observability (`system.datasets`, `system.tables`, `system.nodes`,
  `system.traces`, …). `SELECT database, dataset, column_count FROM system.datasets` lists what exists.
- `SELECT <literal>` (e.g. `SELECT 1`) is **not** supported — SELECT wants identifiers/columns.

## Querying it

All three reach the same node; the data is one CRDT store, replicated across the cluster.

Auth is **always enforced** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)) — every request
needs `Authorization: Bearer <admin-token>` (Secret `<name>-zetta-admin`, key `token`, minted by the
operator). Retrieve it with `kubectl get secret <name>-zetta-admin -o jsonpath='{.data.token}' | base64 -d`.

```sh
# Browser / HTTP (GET; auth is mandatory — send the admin bearer):
curl -sG -H "Authorization: Bearer $TOKEN" https://zetta.oramadev.net/v1/sql \
  --data-urlencode "statement=select * from orama_control.components"

# In-cluster (from any pod):
curl -sG -H "Authorization: Bearer $TOKEN" http://<name>-zetta:4800/v1/sql \
  --data-urlencode "statement=SELECT * FROM system.datasets"

# Over MCP (Claude, or any MCP client):
#   tool: sql   { "statement": "SELECT name, status FROM components", "database": "orama_control" }
```

Example row (`orama_control.components`):

```json
{"image":"oramasearch/zetta:headless-v1-sandbox","kind":"OramaZettaCluster",
 "name":"zetta-0","status":"Ready","updated_at":"2026-07-09"}
```

## Source of truth: desired vs observed (what actually lives here)

Two different kinds of state, two homes — this is the key design point:

- **Reconciled desired state = the CRs** (`OramaCloudV1`, `OramaZettaCluster`, `LakeEnvironment`, …).
  Kubernetes remains the reconcile API and source of truth after materialization. Deployment intake is
  the explicit exception from liquid-bridge decision 22: liquid appends an external desired event to
  `orama_control.deployments`, and the operator deterministically creates or updates the corresponding
  `LakeEnvironment` CR. All subsequent workload reconciliation derives from that CR.
- **Observed / derived state** — what is actually running, per-component status, reconcile history,
  config snapshots. **This is what belongs in Zetta and should stay in sync**, giving a durable,
  SQL-queryable, replicated record independent of etcd. That is the real symbiosis.

The operator **continuously syncs** its observed state (`zetta_statesync.go`): on each reconcile it reads
the coarse, component-granular CR statuses it already maintains (`OramaCloudV1.status.components[]` +
each `OramaZettaCluster` phase) and writes the **changed** rows to `orama_control.components` — **same
anti-flap discipline as the status subresource** (a steady stack produces zero Zetta writes). The
deployed engine is append-only (no `UPDATE` yet — [zetta#735](https://github.com/oramasearch/zetta/issues/735)),
so a change **appends** a row; read **latest-wins** by `updated_at`. Best-effort: the flaky node
([zetta#731](https://github.com/oramasearch/zetta/issues/731)) just retries next reconcile. A 60 s
idle requeue keeps it tracking the rest of the operator's CRs; when the namespace has Lakes, their
shortest deployment-sync interval drives the same reconcile loop.

Deployment events use the same append-only constraint but keep `desired` and `status` event streams
separate under one `deployment_id`, so a status append cannot erase configuration. The operator polls
at the shortest Lake `spec.deploySync.interval` in the Zetta namespace (default `15s`), processes only
rows naming a local Lake in that namespace, and appends status only when phase, URL or message changes.
See [`docs/lake/environments.md`](lake/environments.md) for the complete row contract and lifecycle.

## Many operators, one Zetta (multi-cluster)

A Zetta cluster is a set of **equal CRDT peers joined on :7777** — it is **not bound to one k8s cluster**.
Nodes can live in different k8s clusters (or on bare hosts) and still form **one logical datastore**, as
long as they can reach each other's peer port. So the target topology is **N k8s clusters → 1 Zetta**:

- **One operator per k8s cluster** (they don't share an operator), all using the **same** Zetta as their
  store.
- **Per-operator identity.** Because they share one store, every synced row is namespaced by an
  `operator` id so states never collide (two clusters each with a `v1/writer` stay distinct). The id
  defaults to the **`kube-system` Namespace UID** (stable + unique per cluster); override with the
  `ORAMA_OPERATOR_ID` env var. The UID is read via a single **uncached GET** (operator#106) — not a
  cluster-wide Namespace informer — so the operator needs only `namespaces: [get]`, never list/watch.
  Query one operator's view with `… WHERE operator = '<id>'`.
- **Peering across clusters** is the networking piece, and it follows the **node-reachability model**
  (see the Zetta [`docs/cluster/cluster.md`](https://github.com/oramasearch/zetta/blob/dev/docs/cluster/cluster.md)
  "Node reachability + the route table"), **not** an inbound endpoint on every node:
  - **Each slice DECLARES its reachability type** (`spec.reachability`, [operator#74](https://github.com/oramasearch/orama-cloud-v1-operator/issues/74)):
    - **`wan-accessible`** — a routable address (a rendezvous others dial **out** to). The operator
      advertises that address as **`.status.wanAddress`** — derived from `spec.wanAddress` (a mesh-VPN /
      stable public IP / DNS name), else `spec.hostNetwork` (the node's host IP), else the
      `spec.exposePeerLoadBalancer` cloud-LB IP.
    - **`dial-out-only`** (the **default**) — behind NAT or a private VNet (a home node, an AKS pod on a
      private IP). It dials **outbound** only and accepts **no inbound**; `.status.wanAddress` is empty.
    - **`spec.ephemeral`** — burst/autoscaled capacity ([operator#69](https://github.com/oramasearch/orama-cloud-v1-operator/issues/69))
      expected to churn; **always forced to dial-out-only** (its address would flap, so it is never a
      rendezvous) and its membership blips are not treated as a fault.
  - **The mesh self-forms — coordinated THROUGH Zetta** ([operator#105](https://github.com/oramasearch/orama-cloud-v1-operator/issues/105)):
    each operator writes its slice's wan endpoints into `orama_control.topology`, Zetta CRDT-syncs the
    table, and each operator reads it back to **derive** its siblings' `--peer` set automatically — no
    admin hand-copies `.status.wanAddress` into `spec.externalPeers` anymore (though setting
    `spec.externalPeers` still works and **wins** as an explicit override). Dial-out-only siblings reach
    out to the derived wan rendezvous; Zetta relays transitively + computes the route table
    ([zetta#739](https://github.com/oramasearch/zetta/issues/739)). A private node is **always the
    initiator**. The same wan-accessible set feeds the **Cloudflare LB origin pool**
    ([operator#73](https://github.com/oramasearch/orama-cloud-v1-operator/issues/73) — HTTP origin at the
    same host `:4800`). Full mechanism: [Cross-cluster topology through Zetta](#cross-cluster-topology-through-zetta).
  - **Anti-pattern — do NOT** provision an inbound `LoadBalancer` (or, worse, one source-locked to a
    specific peer's IP) to "reach" a `dial-out-only` node — that **inverts** the model. The operator
    enforces this: `spec.exposePeerLoadBalancer` is **honored only on a `wan-accessible` slice**; on a
    dial-out-only/ephemeral slice it is **ignored** (a warning is logged and any existing peer LB torn
    down). And even for a wan-accessible node a cloud LB is only one option — a node whose host already
    has a public IP (`hostNetwork`) or a mesh-VPN address (`wanAddress`) needs none.
  - **Advertised address vs bound address:** Zetta now has `--advertise HOST:PORT[,…]`, and the operator
    gives each **wan-accessible** node its OWN routable address via `--advertise` — a **per-node peer LB**
    per permanent node, so every node advertises its own IP and cross-cluster load spreads across all their
    uplinks (aggregate bandwidth, [operator#104](https://github.com/oramasearch/orama-cloud-v1-operator/issues/104)).
    `.status.wanAddress` is the joined per-node list a sibling copies into `spec.externalPeers`.
  - **A `dial-out-only` node must advertise NO dialable address, and peers must never dial it**
    ([zetta#803](https://github.com/oramasearch/zetta/issues/803)): it has no routable address, so
    gossiping its private/bound pod IP makes peers try to dial it — and when two clusters **share a pod
    CIDR** (k0s + AKS both `10.244.0.x`) that dial reaches the WRONG node and undercounts membership
    (peripheral nodes showed `cluster: 7 node`, not 9). It is a pure initiator; its one outbound link
    carries CRDT sync both ways, and full membership rides the CRDT node-list
    ([zetta#800](https://github.com/oramasearch/zetta/issues/800)). The operator passes `spec.reachability`
    to the node so a dial-out slice never advertises its pod IP.
  - Inter-node peer traffic is **always mTLS**: the operator mints one shared cluster join token per
    cluster (Secret `<name>-zetta-cluster`) and injects it into every node as `ZETTA_CLUSTER_TOKEN`, so
    all nodes derive the same cluster CA and form one cluster ([operator#92](https://github.com/oramasearch/orama-cloud-v1-operator/issues/92),
    see [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md)). The earlier `clusterPlaintext` escape hatch was
    **removed** as a security hole once real mTLS clustering worked. NOTE: a *multi-node* cluster only
    fully converges once the engine's DNS-dial + mTLS-sync fixes land ([zetta#785](https://github.com/oramasearch/zetta/issues/785),
    [zetta#786](https://github.com/oramasearch/zetta/issues/786)) — the token seeding is necessary but not
    sufficient on its own.

## Cross-cluster topology through Zetta

Cross-cluster peering ([operator#105](https://github.com/oramasearch/orama-cloud-v1-operator/issues/105))
used to be hand-wired: an admin copied each slice's `.status.wanAddress` into its siblings'
`spec.externalPeers`. It is now **coordinated through Zetta itself** — the operator uses its own datastore
(`orama_control.topology`) as the rendezvous directory, and the mesh self-forms. The loop, per reconcile
(`internal/controller/zetta_topology.go`, gated on the datastore symbiosis, best-effort):

1. **Write.** The operator appends this slice's **wan-accessible** node endpoints to `orama_control.topology`
   — one row per node: `operator` (this operator's id — the per-k8s-cluster namespacing), `cluster` (the
   **global** name = `spec.clusterName`, or the CR name when unset), `node` (the pod name, stable per node),
   `address` (the routable `host:port`, from `zettaWANAddresses` — `spec.wanAddress` / per-node peer-LB IP /
   host IP), `reachability`, `node_type` (`permanent`/`ephemeral` — the coarse role the operator knows),
   `status`, `updated_at`. **Same anti-flap discipline as `components`:** a row is appended **only when its
   semantic content changes** (a steady wan slice writes nothing). A **dial-out-only** slice has no routable
   address, so it writes nothing.
2. **Sync.** Zetta **CRDT-syncs** `orama_control.topology` across every cluster for free — it is a table on
   the one shared store. No operator work is needed for the sync itself.
3. **Read + derive.** The operator `SELECT`s the topology back (scoped to its `cluster`), takes **latest-wins
   per node by `updated_at`**, and **derives** its `--peer` set from the **wan-accessible active** rows of the
   **other** operators sharing this cluster name — excluding its own nodes and its own advertised addresses.
   The StatefulSet's `--peer` flags are the **union** of `spec.externalPeers` (explicit override — wins) and
   this derived set (deduped). Setting `spec.externalPeers` by hand still works; the derived set fills the gap.
4. **Failover.** When a wan node drops out — this operator scales the slice down, or it flips to
   `dial-out-only` — the operator appends a **`status='gone'` marker row** for that node (there is no
   `DELETE`; the engine is append-only, [zetta#735](https://github.com/oramasearch/zetta/issues/735)). On the
   next read, latest-wins picks the `gone` row and the node **falls out of every sibling's derived `--peer`
   set**. A node whose address is only momentarily pending (an LB IP not yet assigned) is **not** marked gone
   — its last active row stands, so a transient blip never flaps the mesh.

```
operator A (cluster-X)                Zetta (one CRDT store)                operator B (cluster-X)
  write A's wan nodes ───INSERT──▶  orama_control.topology  ◀──INSERT─── write B's wan nodes
  read topology       ◀──SELECT───   (CRDT-synced across    ───SELECT──▶ read topology
  derive --peer = B's wan            all clusters)                       derive --peer = A's wan
```

Reads use `zettaSQLQuery` (in `zetta_statesync.go`) — the authenticated sibling of `zettaSQL` that parses
`.value.rows` out of the `/v1/sql` success envelope (`zettaSQL` only checks `ok:true`). Both send the admin
bearer (auth is mandatory — [zetta#799](https://github.com/oramasearch/zetta/issues/799)).

### Blocked: engine-side node-list GC (out of scope)

The `gone` marker only affects the **operator's derived `--peer` set**. It does **NOT** remove the departed
node from **Zetta's own membership CRDT** (the engine's `node:<id>` node-list). That GC **cannot be done over
SQL** (append-only — no `DELETE`) and **there is no node-removal API yet**, so a `gone` marker row is the most
the operator can express. Removing a node from the engine's node-list is future engine work; the operator
does **not** attempt any engine-side removal. Until then a departed node lingers in Zetta's membership view
even though no sibling dials it.

## Evolving the schema

1. Edit `operatorZettaSchema` in [`zetta_controlplane.go`](../internal/controller/zetta_controlplane.go).
2. Rebuild + restart the operator.
3. The operator detects the changed schema hash, replaces the immutable `<name>-zetta-schema` Job and
   re-applies it (existing objects are skipped via "already exists").

There is no `DROP` in the reconcile path — tearing the schema down is a deliberate, separate action, so a
reconcile can never destroy data. Turning `spec.provisionOperatorSchema` off removes the Job + ConfigMap
but **leaves the databases intact**.

## Caveats (current)

- **Node HTTP is flaky** — the node drops ~⅔ of back-to-back requests
  ([zetta#731](https://github.com/oramasearch/zetta/issues/731)); the schema Job retries hard, and the
  pod's **readiness probe is disabled** ([operator#68](https://github.com/oramasearch/orama-cloud-v1-operator/issues/68))
  so it doesn't flap out of the Service.
- **Auth** — the engine's auth is **mandatory** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)):
  every HTTP/MCP/SQL call must carry a bearer that resolves to a user. The operator ALWAYS seeds a known
  admin token (Secret `<name>-zetta-admin`) and authenticates BOTH its state-sync SQL client
  (`zetta_statesync.go`) and its schema-provisioning curl Job (`zetta_controlplane.go`) with it. The
  `spec.requireAuth` / `spec.mcpTokenAuth` fields are **deprecated no-ops** (there is no unauthenticated
  or `--mcp-token` mode anymore).
- **One deployment store per Lake namespace** — the public deployment control plane refuses an ambiguous
  namespace containing more than one operator-schema Zetta cluster. Other operator state-sync paths still
  run per enabled cluster.
