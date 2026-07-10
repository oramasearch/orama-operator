# `OramaCloudV1` reference

One umbrella CR per cluster describes an entire regional v1 deployment. Source of
truth: [`../api/v1alpha1/oramacloudv1_types.go`](../api/v1alpha1/oramacloudv1_types.go)
(regenerate this doc's tables when the types change).

## Top-level `spec`

| Field | Type | Default | Purpose |
|---|---|---|---|
| `region.name` | string | — | Region label (`us-east`, `eu`, `ap`). |
| `region.role` | enum | `primary` | `primary` (write path + stateful stores) or `read` (regional read tier). |
| `profile` | enum | `light` | `light` (dev/no-repl) · `medium` (single-tenant prod) · `heavy` (multi-tenant). Presets replication + sizing. |
| `idp.provider` | enum | `none` | `none \| keycloak \| firebase \| clerk`. |
| `images.registry/tag/overrides` | — | `oramasearch` / `latest` | Image resolution (overrides win). **`tag` applies to the operator's OWN built images only.** Reused published images resolve independently (off `:latest`, not the sandbox tag): `oramacore` → `oramasearch/oramacore:latest`; `embeddings` (the engine ai-server :50051) → `oramasearch/oramacore-ai-server:latest`, or `-cuda` when `writer.gpu`. Override either via `overrides.oramacore` / `overrides.embeddings`. |
| `images.pullSecretName` | string | — | docker-registry Secret (CR namespace) for **private** images — e.g. the private Docker Hub repos built by the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images) tooling. Attached to every pod. |
| `llm.providers[]` | list | groq+openai+anthropic | **Multiple** LLM providers (`name`, `models[]`, `apiKeySecretRef`, `gpu`) — end users pick per query. Empty ⇒ the prod set. |
| `llm.default` | enum | `groq` | Provider used when a request doesn't name one. |
| `cloudflare.*` | object | enabled | Operator manages CF **DNS + Load Balancing + R2/KV** via the CF API. **Two Secrets:** `apiTokenSecretRef` → `token`+`zoneID`+`accountID` (control plane); `r2SecretRef` → `s3ApiEndpoint`+`accessKeyId`+`secretAccessKey` (R2 S3 data plane). |
| `peers[]` | list | — | Other regions (read tiers → primary `writerEndpoint`/`postgresEndpoint`); no mesh. |
| `demoSeed` | bool | false | Seed a demo project+index so a fresh install answers queries. |

## `spec.components`

Every component has `enabled`. Data stores also have `mode: in-cluster|external`,
`placement: local|central`, `replicate`, and `endpoint`/`credentialsSecretRef`.

| Component | Extra fields | Maps to (prod) |
|---|---|---|
| `engine` | `writer.gpu`, `writer.{resources,nodeSelector,tolerations}`, `reader.{replicas,hpa,resources}`, `envoy` | OramaCore writer + readers + Envoy |
| `dashboard` | `host` | app.orama.com (Next.js) |
| `docs` | `host` | docs.orama.com (Next.js + Fumadocs) |
| `ingestion` | `harvest` | index-builder (Deployment) + HARVEST |
| `controlDB` | data-store | PlanetScale → CloudNativePG |
| `analytics` | data-store | ClickHouse |
| `cache` | data-store | Upstash Redis |
| `broker` | data-store (`placement: central`) | RabbitMQ |
| `search` | data-store | Typesense hosted-search |
| `disco` | `enabled` | DISCO semantic cache |
| `workers` | `image`, `replicas` | the CF workers, bundled into one image |

## `OramaCloudV1E2ETest` (separate CRD)

Runs the **comprehensive functional e2e suite** as a Job against a deployed stack — a test is a
first-class, repeatable resource (the operator never runs suites during normal reconcile). The suite
(`internal/controller/assets/e2e-functional.sh`, embedded via `go:embed` and shipped in a ConfigMap)
is a curl+jq script that self-bootstraps a clean-slate collection+index and asserts **every critical
OramaCore function in variations** — liveness, auth (keys + negatives), collection/index lifecycle +
document CRUD/upsert/delete, the write→searchable replication canary, fulltext (typo tolerance/exact/
threshold/boost/pagination), vector + hybrid, filters (number/date/bool/string/geo + and/or/not),
facets, sort, groupBy, batch-get, KV, hooks, merchandising, MCP, analytics, and answer/RAG. It is the
forcing function (see the production-migration principle in `CLAUDE.md`) — never softened to pass.
Coverage is tracked by [operator#44](https://github.com/oramasearch/orama-cloud-v1-operator/issues/44).
Sample: [`../config/samples/e2e-test.yaml`](../config/samples/e2e-test.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaCloudV1` to test (same namespace); the Job points at `<targetRef>-writer` / `-reader`. |
| `spec.image` | **Runner base image** (default `alpine:3.20`). Must provide `sh` + a package manager — the Job installs curl+jq into it, then runs the embedded suite. Override only with another curl/jq-capable base. |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ocv1e2e`. |

## `OramaZettaCluster` (separate CRD) — step 1 of One Orama

Deploys a **headless Orama Zetta cluster** (a StatefulSet of equal-peer CRDT nodes) from one CR — the
first step of the [One Orama](https://github.com/oramasearch/orama-bridge/tree/main/docs/projects/one-orama)
convergence ([orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)). It creates a
**headless Service** (peer DNS on 7777), a **client Service** (the HTTP API on 4800 — MCP + REST +
serving), and the **StatefulSet** (each node a per-node PVC at `/data`; `node-0` bootstraps, peers join
it). With `spec.host` set it also fronts the host with an **nginx Ingress** and — when `spec.dnsSecretRef`
is given — an **owned `CloudflareDNSRecord`** (a proxied A/CNAME record → the ingress LoadBalancer),
reusing the same Cloudflare child-controller path as the dashboard DNS. The Ingress is **dual-path**: the
bare root (`/`, nginx `location = /`) serves a static **`zetta` landing placeholder** from a tiny
operator-owned nginx pod (ConfigMap + Deployment + Service), while every other path proxies to the Zetta
HTTP API/UI (`:4800`). The placeholder is **decoupled from Zetta's own HTTP server** (currently
unreliable — [zetta#700](https://github.com/oramasearch/zetta/issues/700) /
[zetta#695](https://github.com/oramasearch/zetta/issues/695)) so the host always resolves to something;
swap it for the real Zetta web UI when [zetta#647](https://github.com/oramasearch/zetta/issues/647) ships.
This stage only **runs** Zetta headless — no operator↔Zetta symbiosis and **no** changes to Zetta's MCP
service. Sample: [`../config/samples/oramazettacluster.yaml`](../config/samples/oramazettacluster.yaml).

| Field | Purpose |
|---|---|
| `spec.replicas` | Number of equal-peer headless zetta nodes (default 1). |
| `spec.image` | Zetta-headless image (default `oramasearch/zetta:headless-v1-sandbox`), built by [`zetta-bridge/build/images/zetta-headless`](https://github.com/oramasearch/zetta-bridge/tree/main/build/images/zetta-headless). |
| `spec.pullSecretName` | Pull secret for the private zetta image. |
| `spec.storage.size` / `.storageClassName` | Per-node PVC at `--data-dir` (`/data`) — the CRDT doc, `node_id`, certs, JSONL/large-object data. |
| `spec.resources` | Per-node mem/cpu caps (right-sizing). |
| `spec.clusterPlaintext` | Disable inter-node mTLS (single node mints its own token; set `true` for a multi-node cluster until mTLS token-sharing lands). |
| `spec.provisionOperatorSchema` | **Default `true`.** Makes this cluster the operator's own datastore (One Orama symbiosis v0): once the node is up, an in-cluster Job applies the operator's schema via Zetta's `/v1/sql` — the **`orama_control`** database (`components`, `reconcile_events`) and the **`orama_data`** database (`documents`, the app-data landing zone). Idempotent (tolerates "already exists"; Zetta has no `IF NOT EXISTS`). Edit `operatorZettaSchema` + delete the `<name>-zetta-schema` Job to re-apply. Set `false` to opt out. |
| _(readiness probe — disabled)_ | The pod's readiness probe is **off** ([operator#68](https://github.com/oramasearch/orama-cloud-v1-operator/issues/68)) because the node's flaky HTTP server ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) made it flap out of the Service. Liveness + startup (peer port) remain. Restore `GET /mcp.md` readiness once zetta#731 lands. |
| `spec.mcpTokenAuth` | **Default `false`.** When `true`, mints a 64-char bearer token into Secret `<name>-zetta-mcp` (key `token`), injects it as `--mcp-token` (never logged — entrypoint echoes `****`), and switches readiness to a TCP check. **Leave off:** Zetta's `--mcp-token` treats the token as an *access-key id* that must already be a provisioned principal, so a bare token authenticates but is then denied every capability (`unknown access key`) on both `/mcp` and `/v1/*` — the node authenticates but does nothing. Default (`false`) runs the functional unauthenticated-local session. A working authenticated mode needs an owner-key bootstrap in the node ([zetta#733](https://github.com/oramasearch/zetta/issues/733)). Connect instructions: `GET /mcp.md` (public). |
| `spec.host` | Hostname to expose at (e.g. `zetta.oramadev.net`) via an nginx Ingress — root `/` serves the `zetta` landing placeholder, other paths proxy to the API/UI (:4800). Empty = no external exposure (the Ingress/DNS/landing are torn down if the field is later cleared). |
| `spec.dnsSecretRef.name` | With `spec.host`, create a **proxied Cloudflare DNS record** (host → ingress LB) using the named Secret (keys `token`/`zoneID`/`accountID`) — normally the same `cloudflare-api-token` Secret the `OramaCloudV1` uses. |
| `status.phase` / `.readyNodes` / `.totalNodes` | Readiness roll-up. `kubectl get ozc`. |

**Live (`oramadev.net` sandbox):** `GET https://zetta.oramadev.net/` returns `zetta` (the landing pod),
and `GET https://zetta.oramadev.net/v1/server_info` returns the node's server info from the Zetta API —
both through Cloudflare (proxied), the same reachability path as `app.oramadev.net` → the dashboard.
