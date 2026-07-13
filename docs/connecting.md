# Connecting to a deployed OramaZettaCluster

How to reach a Zetta cluster the operator deployed, across **every** surface it exposes:
the REST/SQL API, MCP (for agents like Claude), the self-describing `mcp.md` / `llms.txt`
pages, and the web portal. All of them are served by the **same node HTTP server on port
4800**; the peer/replication port `7777` is internal and never a client surface.

Examples use a cluster named `e2e` in namespace `orama-system` (Services `e2e-zetta` /
`e2e-zetta-headless`). Substitute your `OramaZettaCluster` name.

## Addressing — one client port, three ways to reach it

The operator creates two Services:

| Service | Type | Ports | Purpose |
|---|---|---|---|
| `<name>-zetta` | ClusterIP | **4800** | **client surface** — REST · SQL · MCP · `mcp.md` · `llms.txt` · web portal. Round-robins across nodes. |
| `<name>-zetta-headless` | ClusterIP (headless) | 7777, 4800 | per-pod DNS; `7777` is inter-node **mTLS** peering (not a client surface). |

Reach `:4800` one of three ways:

- **In-cluster** — any pod: `http://<name>-zetta.<ns>.svc.cluster.local:4800`.
- **From your laptop** — port-forward:
  ```bash
  kubectl port-forward svc/e2e-zetta -n orama-system 4800:4800
  # everything below is then at http://127.0.0.1:4800
  ```
  Pin to one node instead of round-robin with `kubectl port-forward pod/e2e-zetta-0 …`.
- **Public** — if the CR sets `spec.host`, the operator provisions an Ingress at that host:
  `/` serves a landing placeholder; **every other path proxies to `:4800`**. With
  `spec.dnsSecretRef` it also creates the Cloudflare DNS record.

## Authentication

Peer traffic is always mTLS (a shared cluster join token, `<name>-zetta-cluster`). The **client**
surface auth is **mandatory** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)) — every
HTTP/MCP/SQL call needs a bearer that resolves to a user; there is no unauthenticated mode.

- **Admin token** — Secret `<name>-zetta-admin` (key `token`), ALWAYS minted by the operator and
  seeded on the node as the owner principal. All API/SQL/MCP calls need
  `Authorization: Bearer <token>`. Retrieve it:
  ```bash
  kubectl get secret <name>-zetta-admin -n orama-system -o jsonpath='{.data.token}' | base64 -d
  ```
- `spec.requireAuth` / `spec.mcpTokenAuth` are **deprecated no-ops** — there is no separate
  `<name>-zetta-mcp` token or unauthenticated mode anymore.

## 1. REST + SQL API (`/v1/*`)

Every response carries `ok`, a `trace_id`/`span_id`, and `duration_ms` (observability is
built in). SQL rows are **objects** keyed by column.

```bash
B=http://127.0.0.1:4800
# auth is mandatory (zetta#799) — send the seeded admin bearer on every call
TOKEN=$(kubectl get secret <name>-zetta-admin -n orama-system -o jsonpath='{.data.token}' | base64 -d)
A=(-H "Authorization: Bearer $TOKEN")

# health + cluster membership
curl -s "${A[@]}" "$B/v1/server_info"
#   {"ok":true,...,"value":{"cluster_members":[<4 node uuids>],"databases":2,"node_id":"…"}}

# run SQL (append-only Postgres subset on the deployed engine)
curl -sG "${A[@]}" "$B/v1/sql" --data-urlencode "statement=SELECT name FROM system.tables"

# the capability registry as OpenAPI + per-capability specs
curl -s "${A[@]}" "$B/v1/specs"          # list
curl -s "${A[@]}" "$B/openapi.json"      # full OpenAPI
```

`server_info.cluster_members` is the quickest "is the cluster formed?" check — it lists
every peer's node id (four, for a 4-node cluster).

## 2. MCP — for Claude and other agents

The node speaks **Model Context Protocol over streamable HTTP** — plain JSON-RPC
request/response over `POST /mcp` (**no SSE**), as its own `/mcp.md` documents.

```bash
curl -s -X POST "$B/mcp" \
  -H 'content-type: application/json' \
  -H 'accept: application/json' \
  -H "authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},
                 "clientInfo":{"name":"me","version":"0"}}}'
#   {"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"zetta",…},"capabilities":{"tools":{}}}}
# then: {"method":"tools/list"}  ·  {"method":"tools/call","params":{"name":"sql","arguments":{…}}}
```

**Point Claude Code at it** — `.mcp.json` (or `claude mcp add`):

```json
{
  "mcpServers": {
    "zetta": {
      "type": "http",
      "url": "http://127.0.0.1:4800/mcp",
      "headers": { "Authorization": "Bearer <admin-token from <name>-zetta-admin>" }
    }
  }
}
```

(For a public cluster use the `spec.host` URL instead of the port-forward.)

## 3. Self-describing pages — `mcp.md` + `llms.txt`

The node generates its own connection + capability docs live from the registry, so they're
always current — hand either to a human or an LLM:

- **`GET /mcp.md`** — human/agent guide to connecting over MCP (endpoint, auth, the tool
  list). This is the canonical "how do I talk to this node" page.
- **`GET /llms.txt`** — a live one-pager of node facts (version, cluster member count,
  databases, indexes) for an LLM; **`/llms-full.txt`** and `/llms.txt/<topic>` go deeper.

```bash
curl -s "$B/mcp.md" | head    # "# Connect to this Zetta node over MCP …"
curl -s "$B/llms.txt" | head  # "# Orama Zetta node <id> … 4 cluster member(s) · 2 database(s)"
```

## 4. Web portal

**`GET /`** serves the Zetta web dashboard (`Content-Type: text/html`, `<title>Zetta</title>`).
Open it in a browser:

```bash
kubectl port-forward svc/e2e-zetta -n orama-system 4800:4800
open http://127.0.0.1:4800/      # or browse the spec.host URL for a public cluster
```

Through the Ingress, the host root shows the landing placeholder and the dashboard lives
under the proxied paths; via port-forward you hit the node directly and `/` is the dashboard.

## Quick reference

| Want | Where |
|---|---|
| Is the cluster formed? | `GET /v1/server_info` → `value.cluster_members` (one id per node) |
| Run SQL | `GET /v1/sql?statement=…` (`Bearer` required) |
| Connect an agent | `POST /mcp` (JSON-RPC, `Bearer` required — auth is mandatory) |
| How to connect (self-doc) | `GET /mcp.md` |
| Facts for an LLM | `GET /llms.txt` · `/llms-full.txt` |
| Web portal | `GET /` (dashboard) |
| Retrieve admin token | `kubectl get secret <name>-zetta-admin -o jsonpath='{.data.token}' \| base64 -d` |

## See also
- [`zetta-datastore.md`](zetta-datastore.md) — how the **operator itself** connects to this
  cluster as its datastore (One Orama).
- [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md) — the peer (`:7777`) mTLS / join-token model.
