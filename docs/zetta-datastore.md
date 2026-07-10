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

The Job is owner-ref'd to the `OramaZettaCluster` (GC'd with it) and is created once — **to re-apply an
edited schema, `kubectl delete job <name>-zetta-schema`** and the operator recreates it.

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

```sh
# Browser / HTTP (GET; the node is unauthenticated by default):
https://zetta.oramadev.net/v1/sql?statement=select%20*%20from%20orama_control.components

# In-cluster (from any pod):
curl -sG http://<name>-zetta:4800/v1/sql --data-urlencode "statement=SELECT * FROM system.datasets"

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

- **Desired state = the CRs** (`OramaCloudV1`, `OramaZettaCluster`, …). For a kubebuilder operator,
  **Kubernetes (etcd) is necessarily the source of truth** — the CRs *are* the reconcile API. This does
  **not** move to Zetta while the operator is a k8s operator; a reconcile always derives from the CRs.
- **Observed / derived state** — what is actually running, per-component status, reconcile history,
  config snapshots. **This is what belongs in Zetta and should stay in sync**, giving a durable,
  SQL-queryable, replicated record independent of etcd. That is the real symbiosis.

**Status today (be honest):** the operator provisions the **schema** (the tables above) but does **not
yet continuously write its observed state into them** — the sample `components` row was seeded manually.
Closing that gap is the next step: on each reconcile, **append** a `components` row (current image/status
per component) and a `reconcile_events` row, and read **latest-wins** (append-only, because Zetta has no
`UPDATE`). Tracked in [orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130) (stage 2).
The operator reaching the node to write is the same constraint as the schema Job — trivial in-cluster,
via the ingress/MCP when running locally.

## Evolving the schema

1. Edit `operatorZettaSchema` in [`zetta_controlplane.go`](../internal/controller/zetta_controlplane.go).
2. Rebuild + restart the operator.
3. `kubectl delete job <name>-zetta-schema` — the operator recreates it and re-applies (existing objects
   are skipped via "already exists").

There is no `DROP` in the reconcile path — tearing the schema down is a deliberate, separate action, so a
reconcile can never destroy data. Turning `spec.provisionOperatorSchema` off removes the Job + ConfigMap
but **leaves the databases intact**.

## Caveats (current)

- **Node HTTP is flaky** — the node drops ~⅔ of back-to-back requests
  ([zetta#731](https://github.com/oramasearch/zetta/issues/731)); the schema Job retries hard, and the
  pod's **readiness probe is disabled** ([operator#68](https://github.com/oramasearch/orama-cloud-v1-operator/issues/68))
  so it doesn't flap out of the Service.
- **Auth** — provisioning assumes the node is unauthenticated (the functional default). `spec.mcpTokenAuth`
  is opt-in and currently breaks capability access until the node supports an owner-key bootstrap
  ([zetta#733](https://github.com/oramasearch/zetta/issues/733)); leave it off for now.
- **One node** — with a single `zetta-0` this is straightforward; picking which cluster is *the* operator
  datastore when there are several is future work.
