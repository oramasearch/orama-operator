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
| `images.registry/tag/overrides` | — | `oramasearch` / `latest` | Image resolution (overrides win). |
| `images.pullSecretName` | string | — | docker-registry Secret (CR namespace) for **private** images — e.g. the private Docker Hub repos built by the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images) tooling. Attached to every pod. |
| `llm.providers[]` | list | groq+openai+anthropic | **Multiple** LLM providers (`name`, `models[]`, `apiKeySecretRef`, `gpu`) — end users pick per query. Empty ⇒ the prod set. |
| `llm.default` | enum | `groq` | Provider used when a request doesn't name one. |
| `cloudflare.*` | object | enabled | Operator manages CF **DNS + Load Balancing** via the CF API (`manageDNS`, `manageLoadBalancers`, `apiTokenSecretRef`, `zoneID`). |
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
