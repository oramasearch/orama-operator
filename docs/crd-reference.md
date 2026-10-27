# `OramaCloudV1` reference

One umbrella CR per cluster describes an entire regional v1 deployment. Source of
truth: [`../api/v1alpha1/oramacloudv1_types.go`](../api/v1alpha1/oramacloudv1_types.go)
(regenerate this doc's tables when the types change). Samples:
[`../config/samples/primary-us-east.yaml`](../config/samples/primary-us-east.yaml) (a primary
region) and [`../config/samples/read-eu.yaml`](../config/samples/read-eu.yaml) (a read tier
pointing at the primary); the full sample list is in
[install.md](install.md#3-deploy-a-v1-stack-single-cluster-quickstart).

## Top-level `spec`

| Field | Type | Default | Purpose |
|---|---|---|---|
| `region.name` | string | — | Region label (`us-east`, `eu`, `ap`). |
| `region.role` | enum | `primary` | `primary` (write path + stateful stores) or `read` (regional read tier). |
| `profile` | enum | `light` | `light` (dev/no-repl) · `medium` (single-tenant prod) · `heavy` (multi-tenant). Presets replication + sizing. |
| `idp.provider` | enum | `none` | `none \| keycloak \| firebase \| clerk`. |
| `jwtIssuer` | string | — | The issuer of the stack's write tokens: the shared ConfigMap's `JWT_ISSUER`, which the dashboard signs with, and the only issuer the writer accepts ([below](#speccomponentsenginewriterjwt)). At most 128 characters. No CRD default, because a production issuer is chosen on purpose: `engine.writer.jwt` refuses an empty one. With `engine.writer.jwt` unset an empty value gives `JWT_ISSUER: orama-cloud`. A change rolls the dashboard and, with `engine.writer.jwt` set, the writer. |
| `images.registry/tag/overrides` | — | `oramasearch` / `latest` | Image resolution (overrides win). **`tag` applies to the operator's OWN built images only.** Reused published images resolve independently (off `:latest`, not the sandbox tag): `oramacore` → `oramasearch/oramacore:latest`; `embeddings` (the engine ai-server :50051) → `oramasearch/oramacore-ai-server:latest`, or `-cuda` when `writer.gpu`. Override either via `overrides.oramacore` / `overrides.embeddings`. `overrides.analytics-import` replaces the analytics import's `curlimages/curl:8.10.1` ([`analyticsProcessor.import`](#analyticsprocessorimport)). `overrides.oramacore-writer` and `overrides.oramacore-reader` (set both or neither; one alone fails the reconcile while the engine is enabled) run the writer-only and reader-only OramaCore builds with their own `writer.yaml` and `reader.yaml`, and win over `overrides.oramacore`; unset, both pods run the combined image, so every reader also runs a writer ([components.md](components.md#engine-internalcontrollerenginego)). `overrides.postgres-client` is the psql image of the control DB's `PostgresSchema` Job and its cluster-row Job, default `postgres:16-alpine` ([components.md](components.md#controldb-internalcontrollercontroldbgo)); additional helper override keys are `busybox`, `envoy`, `selenium`, `keycloak`, `pgadmin`, `demo-seed`, and `controldb` (CNPG `imageName`). Fixed upstream defaults are pinned by manifest digest; see [workload image integrity](components.md#workload-image-integrity-lsr-214). |
| `images.requireDigests` | bool | `false` | Reject mutable images on rendered workload writes. With enforcement off, `ImagesPinned=False` / `ImagesNotPinned` and a Warning event report unpinned selections without blocking live production. Enable after configuring reviewed component and helper digests. Existing immutable tagged Jobs remain reported until their normal replacement. |
| `images.pullSecretName` | string | — | docker-registry Secret (CR namespace) for **private** images — e.g. the private Docker Hub repos built by the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images) tooling. Attached to every pod. |
| `llm.providers[]` | list | Prod 1's four ([below](#specllm)) | **Multiple** LLM providers (`name`: `groq \| openai \| anthropic \| google \| vllm`, `models[]`, `apiKeySecretRef: {name}`, `gpu`) — end users pick per query. The first model is the provider's default model. |
| `llm.default` | string | `groq` | Provider used when a request doesn't name one; must be one of the providers. |
| `llm.automaticEmbeddingsSelector` | `{provider, model}` | unset | Provider and model that choose the properties to embed for collections with automatic embeddings. Unset renders no selector. |
| `masterAPIKeySecretRef` | `{name, key}` | unset | The Secret, in the CR's namespace, holding the engine master API key under `key` (default `MASTER_API_KEY`). Required while the engine or another master key consumer is enabled; there is no default Secret and no generated key ([below](#specmasterapikeysecretref)). |
| `cloudflare.*` | object | enabled | Operator manages CF **DNS + Load Balancing + R2/KV** via the CF API. **Two Secrets:** `apiTokenSecretRef` → `token`+`zoneID`+`accountID` (control plane; `token` is also the DNS-01 credential of the stack's Let's Encrypt `Issuer`, so a stack with any host needs it, and index-builder reads its `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` from `token` and `accountID`, [#571](components.md#app-compute--ingestion--workers--dashboard--docs-appgo-ingestiongo)); `r2SecretRef` → `s3ApiEndpoint`+`accessKeyId`+`secretAccessKey` (R2 S3 data plane). `manageDNS` gives the dashboard, docs, writer and reader hosts their DNS records, all in the zone of the token Secret's `zoneID` ([public hosts](#public-writer-and-reader-hosts-enginewriterhost-enginereaderhost)). |
| `components.workers.sentryDSNSecretRef` | `{name, key}` | unset | The workers' `SENTRY_DSN` from a key of a Secret in the CR's namespace instead of `<cr>-workers-secret`; read, never created or changed. A missing Secret or key fails the workers step. The workers' `OPENAI_API_KEY`, `GROQ_API_KEY` and `ANTHROPIC_API_KEY` come from the same providers' `llm.providers[].apiKeySecretRef` as the engine's ([#571](components.md#app-compute--ingestion--workers--dashboard--docs-appgo-ingestiongo)). |
| `components.ingestion.ablyKeySecretRef` | `{name, key}` | unset | Index-builder's `ABLY_API_KEY` for deployment progress, from a Secret in the CR's namespace. An explicit required `secretKeyRef` wins over component `envFrom`; no new dummy is seeded. The Secret is read, never written; missing Secret, key or empty value fails ingestion before changing the builder. Its watch and credentials hash roll the builder on rotation. Unset keeps historical sandbox defaults. HARVEST's separate seed is unchanged while its worker launcher is missing (#12); this field does not activate crawling. [Decision and limits](provisioning-runbook.md#ably-deployment-progress-747). |
| `peers[]` | list | — | Other regions (read tiers → primary `writerEndpoint`/`postgresEndpoint`); no mesh. |
| `demoSeed` | bool | false | Seed a demo project+index so a fresh install answers queries. |

## `spec.llm`

The engine's LLM settings, rendered into the `ai_server` section of every engine config (the
combined `config.yaml` and `combined-reader.yaml`, or both `writer.yaml` and `reader.yaml`):

- `ai_server.llm` is the `default` provider as an OpenAI-compatible remote (`local: false`): its
  built-in base URL, its first model and its key. A request that names no provider goes there.
- `ai_server.remote_llms` has one entry per provider: `provider`, its key and `default_model` (the
  first model). No `url` is rendered, so OramaCore uses its built-in URL. The model list does not
  restrict the model a request names.
- `ai_server.embeddings.automatic_embeddings_selector` is `automaticEmbeddingsSelector`, only when
  it is set.

| Provider | Base URL (OramaCore v1.2.39 `src/ai/llms.rs`) |
|---|---|
| `groq` | `https://api.groq.com/openai/v1` |
| `openai` | `https://api.openai.com/v1` |
| `google` | `https://generativelanguage.googleapis.com/v1beta/openai` |
| `anthropic` | `https://api.anthropic.com/v1` |

**Defaults.** An omitted or empty `providers` list is Prod 1's set, and an empty `default` is
`groq`; the shared `LLM_PROVIDERS` value lists the same names. The CR's spec is not filled in.

```yaml
llm:
  default: groq
  providers:
    - { name: openai,    models: [gpt-5.2] }
    - { name: google,    models: [gemini-2.0-flash-001] }
    - { name: groq,      models: [openai/gpt-oss-120b] }
    - { name: anthropic, models: [claude-sonnet-4-5] }
  automaticEmbeddingsSelector: { provider: openai, model: gpt-5.2 }
```

`automaticEmbeddingsSelector` has no default: without it no selector is rendered, even on a CR
with no `spec.llm`, so the defaults are Prod 1's providers and models but not its whole block.
Production sets the block above explicitly (`config/clusters/sets/v1-prod.yaml`).

**Keys.** A provider's key comes from `<cr>-engine-secret` under `<PROVIDER>_API_KEY`
(`GROQ_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY`; the operator seeds missing
ones with `dev-*` placeholders and never replaces a value), or, with `apiKeySecretRef`, from that
Secret in the CR's namespace under the same `<PROVIDER>_API_KEY`. The operator only reads
a referenced Secret. The ConfigMap holds `__<PROVIDER>_API_KEY__` placeholders; the
`render-config` init container fills them at pod start.

**Rotation rolls the engine.** The writer and reader pod templates carry
`orama.cloud/config-hash` (the side's template, so an LLM setting change rolls them) and
`orama.cloud/llm-secret-hash`, a SHA-256 of the data of `<cr>-engine-secret` and every referenced
key Secret. The operator watches those Secrets, including ones it does not own, so changing a key
value rolls the writer and readers with no restart by hand; a metadata-only edit does not. A read
region has no writer and rolls its readers.

**Validation.** Before any engine object is written, the reconcile fails with `Ready=False`
(`ReconcileError`) and leaves the running engine as it is when: the default or selector provider
is not one of the providers; a provider is listed twice, is `vllm` (in-cluster vLLM is not
implemented), is unsupported, or has no first model; the selector's model is empty; an
`apiKeySecretRef.name` is empty; or a key Secret or key is missing, empty or holds a line break.
Messages name the field or Secret and key, never a value. Fixing the spec or (re)creating the
Secret recovers on the next reconcile, which the Secret watch triggers.

## `spec.masterAPIKeySecretRef`

The one source of `MASTER_API_KEY`
([#307](https://github.com/oramasearch/orama-operator/issues/307)). `name` is required and
nonempty; `key` defaults to `MASTER_API_KEY`, so `key: token` reads `Data["token"]` while every
consumer still receives the variable `MASTER_API_KEY`. The Secret is supplied, normally through the
set's `secrets` list, and the operator only reads it:

```yaml
spec:
  secrets:
    - { name: v1-dev-master, namespace: orama-system, as: v1-master-api-key }
  template:
    masterAPIKeySecretRef:
      name: v1-master-api-key
      key: MASTER_API_KEY
```

Consumers: the writer's render-config (`writer_side.master_api_key`), index-builder, workers, the
dashboard and the
`OramaCloudV1E2ETest` runner; never a reader, in either image mode
([#795](https://github.com/oramasearch/orama-operator/issues/795): the combined image's readers
generate their own writer's key at pod start). Each receives an explicit `secretKeyRef`, which wins over a
`MASTER_API_KEY` an older `<cr>-<component>-secret` still holds. The value must equal the target
control database's `oramacore_clusters.master_api_key`, which the dashboard uses for administrative
calls. On a control DB with no row the operator inserts the `c1` row with this key once, and never
changes it ([`ClusterRowReady`](#statusconditionsclusterrowready)).

**Unset means generated (#615).** With no reference, or one with an empty name, the operator creates
`<cr>-master-api-key` (`MASTER_API_KEY`, 48 random hex characters) once and never replaces it, and
every consumer reads it. Name a Secret only when the stack must carry an existing key, as production
carries Prod 1's: the `c1` cluster row keeps the key it was inserted with.

**The engine needs a real key.** While `components.engine.enabled` is true the operator checks the
master key Secret on every reconcile, before it writes a writer or reader template. On a failure the writer
and readers are not created, or a running stack's are scaled to zero with their volumes and
credentials kept, the phase is `Degraded`, and `Ready=False` names the namespace, Secret and key,
never the value:

| Failure | `Ready` reason |
|---|---|
| the Secret does not exist | `MasterAPIKeySecretMissing` |
| the key does not exist | `MasterAPIKeyKeyMissing` |
| the value is empty | `MasterAPIKeyEmpty` |
| the value starts with `dev-` | `MasterAPIKeyPlaceholder` |
| the value holds a line break | `MasterAPIKeyInvalid` |

The CR is checked again every 20 seconds and on every event of the referenced Secret, and the
configured replica counts return once the key is valid, with no spec edit. With the engine
disabled only the name is required, and only when ingestion, workers or the dashboard is enabled
(analytics-processor holds the generated analytics key instead, #386); those consumers are not created or changed without it.

**A changed key rolls its consumers.** Each consumer's pod template carries
`orama.cloud/credentials-hash` over the Secrets it reads, so a changed value, or a reference to
another Secret, rolls exactly them ([components.md](components.md#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)).
It does not change the control database row.

## `spec.components`

Every component has `enabled`. Data stores also have `mode: in-cluster|external`,
`placement: local|central`, `replicate`, and `endpoint`/`credentialsSecretRef`.
The control DB is always in-cluster: `controlDB.mode` supports only `in-cluster`
(the existing default when omitted). Admission refuses `external` even when
`enabled: false`; the reconcile also refuses resources stored before this rule.
`controlDB.endpoint` and `controlDB.credentialsSecretRef` do not select a different
control database. Other data stores, including the broker, still support external mode.
`controlDB.stripePrices` holds this stack's Stripe price IDs for the dashboard reference rows the
operator seeds ([`components.md`](components.md#the-control-dbs-schema)): `plans[]` is
`{ id, monthly, yearly }` (sets `subscription_plans.stripe_price_id_monthly`/`_yearly`; a plan not
listed has both NULL) and `addons[]` is `{ id, priceID }` (sets `stripe_addon_prices.stripe_price_id`;
an add-on price row is seeded only when listed). `id` is the reference row's UUID; an ID that is not
a reference row fails the seed. Empty seeds every plan without Stripe prices and no add-on price.
Every control DB also declares the CNPG managed role `migration`, which signs in only while an
[`OramaCloudV1Migration`](#oramacloudv1migration-separate-crd-namespaced--one-prod-1-team-copied-into-a-stack)
targets the stack; it has no field ([`cloud-v1-migration.md`](cloud-v1-migration.md#what-the-stack-provides)).
`spec.components.ingressClassName` (`nginx`, the default, or `zetta-tunnel`) picks the ingress that
serves the stack's Ingresses; DNS follows ([`infrastructure/ingress.md`](infrastructure/ingress.md)).
Every host (`dashboard.host`, `docs.host`, `engine.writer.host`, `engine.reader.host`, and each one's
`extraHosts`, [below](#extra-hosts-extrahosts)) gets origin TLS: the stack owns cert-manager `Issuer`
`<cr>-letsencrypt` (Let's Encrypt, DNS-01 with `cloudflare.apiTokenSecretRef` key `token`), and each
host's certificate lives in Secret `<host>-tls` beside the Ingress. There is no per-host TLS field and
no way to turn it off; without the token the reconcile fails rather than serving HTTP
([`components.md`](components.md#origin-tls-ingress_tlsgo-457)).

| Component | Extra fields | Maps to (prod) |
|---|---|---|
| `engine` | `writer.gpu`, `writer.{resources,nodeSelector,tolerations,storage}`, `writer.jwt`, `writer.{host,extraHosts}`, `reader.{replicas,resources,storage}`, `reader.{host,extraHosts}`, `envoy` | OramaCore writer + readers + Envoy |
| `dashboard` | `host`, `extraHosts`, `envFrom`, `env` ([below](#speccomponentsdashboard)) | app.orama.com (Next.js) |
| `docs` | `host`, `extraHosts` | docs.orama.com (Next.js + Fumadocs) |
| `ingestion` | `harvest`, `ablyKeySecretRef`, `resources`, `hpesResources` | index-builder (Deployment) + HARVEST + HPES, the embeddings server index-builder calls (`hpesResources`: default 7Gi and 2 CPUs requested, 10Gi limit; [components](components.md#hpes-index-builders-embeddings-hpesgo-574)) |
| `controlDB` | data-store (`mode: in-cluster` only; `external` refused even when disabled), `stripePrices` | Always in-cluster CloudNativePG |
| `analytics` | data-store | ClickHouse |
| `cache` | data-store | Upstash Redis; its REST API through the `redis-http` shim the cache brings ([components](components.md#the-upstash-rest-shim-redis_httpgo-562)) |
| `broker` | data-store (`placement: central`) | RabbitMQ |
| `search` | data-store | Typesense hosted-search |
| `disco` | `enabled` | DISCO semantic cache |
| `workers` | `image`, `replicas` | the CF workers, bundled into one image |
| `analyticsProcessor` | `resources`, `import.{enabled,suspend}` | analytics-processor (readers' search events → ClickHouse) + its scheduled import |
| `importConsumer` | `enabled`, `image`, `concurrency`, `resources` | the import consumer, listing the stack's R2 uploads bucket ([below](#speccomponentsimportconsumer)) |

`engine.reader.replicas` is the fixed number of readers (0 or unset → 3), each with its own data
volume (`10Gi` unless `engine.reader.storage` says otherwise, [below](#component-storage)). There is no autoscaler and no autoscaling field: `engine.reader.hpa`
was removed in #606. `engine.writer.gpu` requests `nvidia.com/gpu` for the writer's built-in embeddings (prod runs an
L40S). `workers` bundles all ported CF workers into one image → one Deployment.

Engine startup budgets are fixed: **20 minutes for the writer** (`failureThreshold: 120`)
and **40 minutes for readers** (`failureThreshold: 240`), both at a 10s period on
`GET /health`, numeric port 8080. Startup gates readiness and liveness until it succeeds;
these budgets add no mandatory delay. There are no per-side startup timeout fields.
Readiness remains every 10s and liveness every 15s, both with a 3s timeout and failure
threshold 3. See [engine health probes](components.md#engine-internalcontrollerenginego).

### `analyticsProcessor.import`

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `false` | Owns the CronJob `<cr>-analytics-import`, which sends `POST /import` and then `POST /aggregate2` to the processor every five minutes (UTC), as Prod 1's importer and aggregator timers do. Its curl image is `curlimages/curl:8.10.1`, or `spec.images.overrides["analytics-import"]`. Requires `engine`, `analytics` (the in-cluster ClickHouse) and `controlDB` enabled and `engine.reader.replicas` of zero (→ 3) or more. |
| `suspend` | `false` | Copied to the CronJob's `spec.suspend`. The CronJob, the readers' headless Service and the processor's sources stay in place; no new Job starts. A Job already running finishes, and the readers keep the events logged meanwhile: the first import after resuming takes them. |

The schedule is fixed; there is no per-stack schedule. The processor's sources are the same whether
the import is enabled, disabled or suspended: each reader ordinal once, through
`<cr>-reader-headless` ([components.md](components.md#analytics-import)). A missing prerequisite or
a negative reader count removes the CronJob (suspended or not) and fails the reconcile with
`Ready=False` (`ReconcileError`) naming it; this check runs before every other step, so an earlier
component's failure cannot leave an old schedule running. `import.enabled: false` removes the
CronJob; `analyticsProcessor.enabled: false` removes the CronJob and the processor's Deployment,
Service, ConfigMap and retention Job, and reports no import error.

### `spec.components.importConsumer`

One Deployment, `<cr>-import-consumer`, runs `deno run -A src/bucket-worker.ts` from the configured
`oramasearch/airbyte-fargate-consumer` image. It lists the stack's R2 uploads bucket every ten seconds.
Bucket provisioning, credential mappings and cutover: [cloud-v1-import-consumer.md](cloud-v1-import-consumer.md).

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `false` | Runs the consumer. Requires Cloudflare and the dashboard enabled. `false` removes the Deployment, Service and PDB. |
| `image` | none | Full image reference containing `src/bucket-worker.ts`. Required when enabled. |
| `concurrency` | `2` (1–16) | Objects the single worker imports at once, each in its own process (`WORKER_CONCURRENCY`). |
| `resources` | requests 256Mi / 100m, memory limit 1536Mi | Each import's heap (`IMPORT_MAX_OLD_SPACE_MB`) is three quarters of the memory limit less 128Mi, divided by concurrency, at least 64Mi. |

The Deployment always has one replica and uses `Recreate`: the worker's duplicate-import protection
is local to one process. Its bucket connection uses `S3_ENDPOINT`, `S3_BUCKET`, `AWS_REGION`,
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, all from `<cr>-uploads-r2` by `secretKeyRef`.
`DASHBOARD_URL=http://<cr>-dashboard:3000`; `PRIVATE_API_KEY` comes from the same Secret key the
dashboard reads (`dashboard.env` wins over `dashboard.envFrom`). The remaining environment is
`WORKER_CONCURRENCY`, `IMPORT_MAX_OLD_SPACE_MB`, `POLL_INTERVAL_SECONDS=10`, `PORT=8080` and
`NODE_ENV=production`, without `envFrom`. TCP probes check 8080; termination grace is 600 seconds.
Bucket credential or API key rotation rolls the worker.

Admission refuses an enabled consumer without an image, Cloudflare or the dashboard, and concurrency
outside 1–16. Reconciliation repeats the dependency checks, requires all five bucket Secret keys and
a Secret-backed dashboard API key, and reports configuration errors through `Ready=False`
(`ReconcileError`) naming `import-consumer`. This last step leaves other components able to reconcile.

### Extra hosts (`extraHosts`)

`dashboard`, `docs`, `oramasearchApp`, `engine.writer` and `engine.reader` each take `extraHosts`
beside `host` ([#313](https://github.com/oramasearch/orama-operator/issues/313)): further names the
component's Ingress answers, each routed to the same backend as `host`. At the cutover
`app.orama.com` is repointed to Prod 2 in Cloudflare while `app.orama.ing` keeps working, and an
Ingress that answers only one name returns ingress-nginx's 404 for the other:

```yaml
spec:
  components:
    dashboard:
      enabled: true
      host: app.orama.ing
      extraHosts:
        - app.orama.com
```

For CR `v1-eu`, Ingress `v1-eu-dashboard` gets two Prefix `/` rules, `app.orama.ing` then
`app.orama.com`, each to Service `v1-eu-dashboard` port 3000, and origin TLS for both (one TLS entry
and Secret `<host>-tls` each). The rules are rebuilt from `host` then `extraHosts` in order on every
reconcile, so removing an extra removes its rule and its certificate request. The writer's extras go
to `<cr>-writer`:8080 and the readers' to `<cr>-reader`:80.

- **No DNS.** Only `host` gets a `CloudflareDNSRecord`; an extra host never does, under any
  setting, and adding or removing one changes no record. Pointing an extra name at the cluster is a
  reviewed Cloudflare change (the cutover runbook). Its certificate still needs DNS-01, so the name
  must be in a zone the stack's Cloudflare token can edit.
- **Same lifecycle as `host`.** Extras are served only while the component is: a disabled
  component, a disabled engine or a read region's writer gets no Ingress. They do not widen
  `writer.jwt.audiences`; a caller using a writer alias still needs a token with an accepted audience.
- **`oramasearchApp`** shares the type, so it accepts the field and its names take part in the
  checks below, but the component stays unwired: no Ingress is created for it.
- **Checks.** Each entry is 1 to 253 characters and a lowercase DNS name with at least one dot,
  the same pattern as `engine.writer.host` (`listType: set`); the CRD refuses anything else, so
  `App.Orama.com` or `app.orama.com:443` never reaches the reconcile, which checks the pattern again. Before writing any child
  resource the reconcile refuses, with phase `Degraded` and `Ready=False` reason
  `InvalidIngressHosts`: `extraHosts` without `host`; an empty or over-long entry; and any name,
  compared case-insensitively, that appears twice across the `host` and `extraHosts` of all five
  components, enabled or not. The message names both fields, for example
  `spec.components.dashboard.extraHosts[0] and spec.components.docs.host are both "app.orama.com"`.
  Only the status is written; existing children keep their last valid state until the spec is
  fixed.

### Public writer and reader hosts (`engine.writer.host`, `engine.reader.host`)

Customers' SDKs and the search worker reach the engine through these names
([#385](https://github.com/oramasearch/orama-operator/issues/385)). Both are optional bare DNS
names (lowercase, at least one dot, at most 253 characters; no scheme, path or port) with no
default: empty keeps the component internal, so an existing CR gets no engine Ingress or record
until a host is set. Production uses `writer.orama.ing` and `reader.orama.ing`, beside the
dashboard's `app.orama.ing`.

| Field | Published while | Ingress | Backend | Sandbox rule |
|---|---|---|---|---|
| `writer.host` | engine enabled, `region.role: primary` | `<cr>-writer` | Service `<cr>-writer`:8080 | `allow-writer-ingress-nginx`: writer pods, TCP 8080 |
| `reader.host` | engine enabled | `<cr>-reader` | Service `<cr>-reader`:80 (Envoy when `envoy`, else OramaCore) | `allow-reader-ingress-nginx`: reader pods, TCP 80 with Envoy, 8080 without |

Each Ingress routes `/` on its host (Prefix) to its Service on the stack's ingress class, with origin
TLS as above, and is controlled by the CR. The sandbox rule admits only ingress-nginx (never
zetta-tunnel's ingress gateway, bridge decision 19 of 2026-10-17), only to this stack's pods; the internal-client rules and every egress
rule are unchanged. A read region never publishes its writer (it runs none). When an endpoint stops
being published (host cleared, engine disabled, or the writer's region turned `read`), its Ingress and
rule are removed and its DNS record is deleted, including on the disabled-engine teardown. An object
at `<cr>-writer` or `<cr>-reader` that the CR does not control is never adopted, changed or deleted;
publishing over it fails the reconcile with a conflict naming the kind, namespace and name.

**`/metrics` is refused on both hosts**
([#314](https://github.com/oramasearch/orama-operator/issues/314)). OramaCore serves Prometheus
metrics on `:8080/metrics`. Each public Ingress has a companion, `<cr>-writer-metrics` or
`<cr>-reader-metrics`, with the same hosts (extra hosts included), class, TLS Secrets, Service and
port, one `Exact` `/metrics` path per host, and `nginx.ingress.kubernetes.io/denylist-source-range:
"0.0.0.0/0,::/0"`, so ingress-nginx answers `/metrics` (with any query string) 403 on every host.
It appears, changes and goes away with the Ingress it shadows, and an object at its name the CR
does not control blocks publishing that endpoint. Readers with Envoy also answer `/metrics` 404 in
Envoy. Inside the cluster it stays on each engine pod's `:8080`. The refusal is ingress-nginx
behaviour: zetta-tunnel's gateway cannot refuse a source and so serves none of these hosts
([ingress](infrastructure/ingress.md#what-it-serves)). For the worked example below:

| Ingress | Host | Path | Backend |
|---|---|---|---|
| `v1-dev-writer` | `writer-dev.oramadev.net` | `/`, Prefix | `v1-dev-writer:8080` |
| `v1-dev-writer-metrics` | `writer-dev.oramadev.net` | `/metrics`, Exact, deny all | `v1-dev-writer:8080` |
| `v1-dev-reader` | `reader-dev.oramadev.net` | `/`, Prefix | `v1-dev-reader:80` |
| `v1-dev-reader-metrics` | `reader-dev.oramadev.net` | `/metrics`, Exact, deny all | `v1-dev-reader:80` |

With `writer.jwt` set, `https://<writer.host>` must be one of `writer.jwt.audiences`
([below](#speccomponentsenginewriterjwt)).

**DNS.** With `cloudflare.enabled`, `cloudflare.manageDNS` and a non-empty
`cloudflare.apiTokenSecretRef.name`, each published endpoint (the dashboard with a host, the writer,
the readers) has one owned, proxied `CloudflareDNSRecord`: `<cr>-dashboard-dns`, `<cr>-writer-dns`,
`<cr>-reader-dns`. Its `spec.name` is the host, `spec.content` the first load-balancer address of the
endpoint's own Ingress (hostname before IP), `spec.secretRef` the token Secret, and `spec.type` empty
(inferred). The record is created only once that Ingress has an address; an existing record keeps its
content while the address is temporarily empty. Every host must be in the zone of the Secret's
`zoneID`: there is no per-host zone selection yet. The parent never writes a record's status or calls
Cloudflare; the [`CloudflareDNSRecord`](#cloudflarednsrecord-separate-crd) controller creates the
Cloudflare record with its ownership comment and never touches one it did not create.

| Change | What happens to the record |
|---|---|
| Ingress address changes | The same child's `spec.content` is updated; its record ID and finalizer stay. |
| Host or Secret name changes before `status.recordID` is set | The same child is updated. |
| Host or Secret name changes after `status.recordID` is set | The child is deleted with its old spec; its finalizer removes the old Cloudflare record with the old Secret; a later reconcile creates a fresh child for the new values. Keep the old Secret, with valid credentials, until that child is gone. |
| Host cleared, component disabled, writer's region turned `read`, `cloudflare.enabled` or `manageDNS` off, token reference removed or emptied | The child is deleted; its finalizer cleans up the Cloudflare record. |
| Child is being deleted | Left untouched until it is gone, even if the endpoint is enabled again meanwhile. |
| The Secret's `zoneID` changes | Nothing from the parent: moving a record between zones is the `CloudflareDNSRecord` controller's concern. |

A child at the record's name that the CR does not control is left untouched and reported as a
conflict. Deleting the CR removes its Ingresses and records through garbage collection. An
endpoint's [`extraHosts`](#extra-hosts-extrahosts) get no record.

With the worked example below (CR `v1-dev`, writer Ingress address `192.0.2.10`, reader `192.0.2.11`),
the writer's Ingress routes `writer-dev.oramadev.net/` to `v1-dev-writer:8080`, the reader's
`reader-dev.oramadev.net/` to `v1-dev-reader:80`, and `v1-dev-writer-dns` / `v1-dev-reader-dns`
point the hosts at `192.0.2.10` / `192.0.2.11`, proxied, with `secretRef: cloudflare-api-token`:

```yaml
spec:
  region: {name: eu, role: primary}
  jwtIssuer: https://app-dev.oramadev.net
  components:
    engine:
      enabled: true
      envoy: true
      writer:
        host: writer-dev.oramadev.net
        jwt:
          audiences: [https://writer-dev.oramadev.net]
      reader:
        host: reader-dev.oramadev.net
    dashboard: {enabled: true, host: app-dev.oramadev.net}
  cloudflare:
    enabled: true
    manageDNS: true
    apiTokenSecretRef: {name: cloudflare-api-token}
```

### `spec.components.engine.writer.jwt`

Makes the writer verify the write tokens the dashboard signs (`POST /api/user/jwt`: issuer
`JWT_ISSUER`, audience the project's `oramacore_clusters.writer_url`). It renders one OramaCore JWKS
provider under `writer_side.jwt` in the combined and writer-only configs; the reader-only config
never has one. Unset renders no `jwt` block, and a write token gets HTTP 500 from the writer
(`JwtError::NotConfigured`).

| Field | Default | Purpose |
|---|---|---|
| `jwksURL` | in-cluster dashboard | The dashboard's JWKS endpoint, an `http` or `https` URL with a host and no credentials. Unset renders `http://<cr>-dashboard:3000/api/.well-known/jwks.json`, so the writer fetches keys from the dashboard Service in its own namespace, not through public DNS and Cloudflare. |
| `audiences[]` | — | Required, at least one. Every accepted audience: each exact `oramacore_clusters.writer_url` in the control database. Each entry 1 to 128 characters. Rendered in order with duplicates removed and otherwise as written: a trailing slash, scheme or case change is a different audience. |

The provider's `issuers` is `[jwtIssuer]`, the same value as the dashboard's `JWT_ISSUER`, and
`refresh_interval` is `1h`. OramaCore matches issuer and audience exactly, so a configuration that
would reject every write fails the reconcile instead (`phase: Degraded`, `Ready=False`, reason
`ReconcileError`, the message naming the field) before any engine object changes:

- `spec.jwtIssuer` is empty, or longer than 128 characters;
- `audiences` is absent or empty, or an entry is empty or longer than 128 characters;
- `engine.writer.host` is set and `https://<host>` is not exactly one of `audiences` (a trailing
  slash, `http` scheme or other host does not count), because the dashboard signs for the writer
  URL and every write would get a 401;
- `jwksURL` is unset and the dashboard is disabled;
- `jwksURL` does not parse, is not `http` or `https`, has no host name (`http://:3000/jwks`), or
  carries a user or password.

The operator checks these strings only: it does not read the control database or fetch the JWKS
during reconcile. A disabled engine is torn down without the check. A change to the issuer, the JWKS
URL, the audiences, or setting or unsetting the block rolls the writer through
`orama.cloud/config-hash`; a reader-only config is unaffected.

Example for a CR named `v1-dev`:

```yaml
jwtIssuer: https://app-dev.oramadev.net
components:
  dashboard:
    enabled: true
  engine:
    enabled: true
    writer:
      jwt:
        audiences:
          - https://writer.a.example
          - https://writer.b.example
```

renders

```yaml
writer_side:
  jwt:
    providers:
      - name: dashboard
        jwks_url: "http://v1-dev-dashboard:3000/api/.well-known/jwks.json"
        issuers: ["https://app-dev.oramadev.net"]
        audiences: ["https://writer.a.example", "https://writer.b.example"]
        refresh_interval: 1h
```

### `spec.components.dashboard`

The dashboard's toggle and hosts (`enabled`, `host`, `extraHosts`, `resources`, as for `docs`), plus
the rest of its environment from Secrets and ConfigMaps in the instance's namespace
([operator#311](https://github.com/oramasearch/orama-operator/issues/311)).

| Field | Type | Meaning |
|---|---|---|
| `envFrom` | `[]EnvFromSource` | Secrets (`secretRef`) and ConfigMaps (`configMapRef`), each with an optional `prefix` and `optional`. Appended after the operator's own sources (`<cr>-config`, `<cr>-dashboard-secret`, `<cr>-jwt`), so a key here overrides the shared ConfigMap and the seeded dashboard Secret, and a later entry overrides an earlier one. |
| `env` | `[]EnvVar` | Variables as in a container: a `value`, or `valueFrom` with `secretKeyRef`, `configMapKeyRef`, `fieldRef` or `resourceFieldRef`. Appended after the operator's explicit env; an entry with the same name as an operator variable replaces it, and for a name listed twice the last entry wins, so each name appears once. |
| `stripeSecretKeySecretRef` | `{name, key}` | The dashboard's `STRIPE_SECRET_KEY` from a key of a Secret in the instance's namespace: a Stripe **restricted key** with only the permissions the dashboard uses ([components.md](components.md#the-dashboards-stripe-key-a-restricted-key-of-its-own-802)), never the account's full secret key. Read, never created or changed; an explicit `secretKeyRef` env entry, listed in the credentials hash, so a changed value rolls the dashboard. A missing Secret, key or value fails the dashboard step. While it is set, `STRIPE_SECRET_KEY` is refused in `env` and in every `envFrom` source. **Required on a production instance** (one its set labels `orama.cloud/purpose: production`), holding a live-mode restricted key (`rk_live_`); a full key (`sk_`) or a test-mode key is refused ([#802](https://github.com/oramasearch/orama-operator/issues/802)). |

```yaml
spec:
  components:
    dashboard:
      enabled: true
      envFrom:
        - secretRef:
            name: dashboard-extra
      env:
        - name: STRIPE_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: stripe-test
              key: api-key
```

**Precedence**, lowest to highest: the operator's envFrom, `envFrom`, the operator's explicit env
(`MASTER_API_KEY` from `spec.masterAPIKeySecretRef`, `POSTGRES_PASSWORD`, `DATABASE_URL`), `env`.

**Refused variables.** `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`,
`POSTGRES_PASSWORD`, `JWT_PRIVATE_KEY`, `JWT_PUBLIC_KEY`,
`JWT_ISSUER` and `MASTER_API_KEY` belong to the operator (the control DB, the generated `<cr>-jwt`, the
operator's JWT issuer, and `spec.masterAPIKeySecretRef`), and so does `STRIPE_SECRET_KEY` while
`stripeSecretKeySecretRef` is set. Naming one in `env`, or carrying one as a key of an `envFrom` source after
its `prefix` (`prefix: JWT_` with key `PRIVATE_KEY` is `JWT_PRIVATE_KEY`), fails the reconcile, even
when a later entry would shadow it and even when the source is `optional`.

**References.** Each `envFrom` entry sets exactly one of `secretRef` and `configMapRef`, with a name;
each key selector in `env` sets a name and a key. References are local to the instance's namespace.
A missing object, or a missing key behind a `secretKeyRef`/`configMapKeyRef`, fails the reconcile
unless the reference is `optional: true`; `optional` covers only a missing object or key, never a read
error or a refused variable. An empty value is a present value. Optional references stay in the
container as written, so Kubernetes applies its usual semantics. ConfigMap `binaryData` never reaches
the environment and is neither read nor hashed.

Every failure sets `Ready=False` (`ReconcileError`) with a message such as `dashboard:
dashboard.envFrom[0] (Secret extra) sets MASTER_API_KEY, which the operator owns (it comes from
spec.masterAPIKeySecretRef); remove it` or `dashboard:
dashboard.envFrom[0]: Secret orama-system/extra does not exist`, naming the field index, the object and
the variable, never a value. The check runs before the dashboard Deployment is written, so the
running dashboard keeps its last valid template. Fixing the spec or (re)creating the object recovers
on the next reconcile, which the watch below triggers. A disabled dashboard is torn down without
reading or checking either field.

**Rolling on change.** The pod template carries `orama.cloud/dashboard-env-hash`: the SHA-256 (64
lowercase hex characters) of the data of every `envFrom` source and of the selected key behind every
`env` key selector, with each object's kind, name, order, prefix and selected key, and whether an
optional object or key is present. The controller watches the referenced Secrets and ConfigMaps,
owned or not, so a changed value, a created or deleted optional source, or an added or removed
selected key rolls the dashboard. Metadata changes and unselected keys of a selector-only object do
not. The annotation holds a hash only; values are written nowhere else. With no Secret or ConfigMap
reference the annotation is absent, so a dashboard with neither field set keeps the template it had
before these fields existed.

**On a production instance** the dashboard also needs `CRON_SECRET` and `STRIPE_WEBHOOK_SECRET` from
the user's own Secrets and `stripeSecretKeySecretRef` with a live restricted key, or it is not
rendered ([components.md](components.md#a-production-dashboard-needs-its-production-authentication-settings-794)).

How the environment Secret reaches each cluster is in
[components.md](components.md#the-dashboards-environment).

#### `spec.components.dashboard.crons`

Prod 1's scheduled jobs as CronJobs in the stack
([operator#17](https://github.com/oramasearch/orama-operator/issues/17)): the dashboard's three
Vercel crons and, optionally, the e2e canary from Prod 1's AWS crons host. Omitted, it creates
nothing. `dashboard_crons.go`; behaviour and the Prod 1 inventory are in
[components.md](components.md#the-dashboards-scheduled-jobs).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | `bool` | `false` | Creates the three dashboard CronJobs; `false` deletes them, and the canary with them. |
| `suspend` | `*bool` | `true` | Copied to the `spec.suspend` of the three CronJobs and `<cr>-e2e-canary`. Stops new runs; a run in progress finishes. |
| `e2eCanary.image` | `string` | required | The canary image, pinned to a digest (`name@sha256:<64 hex>`; the CRD refuses anything else). It runs `monitor-test` and provides `/bin/sh`, `curl` and `jq` for the prune. |
| `e2eCanary.secretRef.name` | `string` | required | A Secret in the instance's namespace with non-empty keys `collectionID`, `indexID` and `apiKey` (the canary project's private API key), delivered through the set's `secrets` list. |

The schedules are Prod 1's and fixed, in UTC:

| CronJob | Calls | Schedule |
|---|---|---|
| `<cr>-dashboard-cron-crawler` | `GET http://<cr>-dashboard:3000/api/private/v1/run-crawler-cron` | `*/1 * * * *` |
| `<cr>-dashboard-cron-index-gc` | `GET http://<cr>-dashboard:3000/api/private/v1/run-index-gc-cron` | `0 * * * *` |
| `<cr>-dashboard-cron-consumption-sync` | `GET http://<cr>-dashboard:3000/api/billing/queue-consumption-sync` | `0 */6 * * *` |
| `<cr>-e2e-canary` | `monitor-test`, then the collection prune once a day after 04:30 | `*/5 * * * *` |

```yaml
spec:
  components:
    dashboard:
      enabled: true
      envFrom:
        - secretRef:
            name: v1-prod-dashboard-env   # holds CRON_SECRET
      crons:
        enabled: true
        suspend: true
        e2eCanary:
          image: oramasearch/oramacore-test-e2e@sha256:<digest>
          secretRef:
            name: v1-prod-e2e-canary
```

No set file carries `e2eCanary` yet: the canary image is not built, and the CRD requires it.

**Admission.** `crons.enabled: true` needs `dashboard.enabled: true` (`dashboard.crons requires the
dashboard enabled`), and `e2eCanary` needs `crons.enabled: true`. To turn a running dashboard off,
commit `dashboard.enabled: false`, `crons.enabled: false` and no `e2eCanary` together. The reconcile
repeats the first rule for objects that bypassed admission: it deletes the crons and the dashboard,
then reports the error.

**`CRON_SECRET`.** Each dashboard CronJob reads `CRON_SECRET` through a `secretKeyRef` to the Secret
key the dashboard itself reads it from, so the two cannot drift. When `dashboard.env` names
`CRON_SECRET` (its last entry wins), that entry must be a `secretKeyRef` and the CronJobs use the same
Secret and key. Otherwise the last `envFrom` source holding `CRON_SECRET` after its `prefix` wins, and it
must be a Secret: with `prefix: CRON_`, key `SECRET` is selected. A literal, a ConfigMap or a `fieldRef`
winning is an error, even if an earlier Secret also holds the key. The selected Secret must exist and
the key must be non-empty, even when the dashboard's reference is `optional`.

**Errors.** A missing source, Secret or key, a canary Secret without its three keys, or a same-name
CronJob or claim this CR does not control fails the `dashboard` step with `Ready=False`
(`ReconcileError`) and a message naming the field and the object, never a value. The dashboard crons'
inputs are checked before the first of them is written; the canary's Secret is checked after the three
are written, so a canary problem never keeps the dashboard crons from running. A foreign object is
left unchanged. Run outcomes appear in Job status and logs only.

## `status`

`phase` (`Progressing | Ready | Degraded`), `componentsReady`, the per-component list and the
`Ready` condition are described in [components.md](components.md#engine-internalcontrollerenginego).

### `status.conditions[EngineSidesSeparated]`

Present while the engine is enabled and its image selection is valid; removed when the engine is
disabled. `True` (`SideImages`) when `spec.images.overrides` sets both `oramacore-writer` and
`oramacore-reader`: the writer renders `writer.yaml` and the readers `reader.yaml`. `False`
(`CombinedImage`) while both run the combined `oramacore` image, the writer with `config.yaml` and
the readers with `combined-reader.yaml`, so every reader also runs a writer (one with a key of its
own that produces to `oramacore-reader-local`, never the writer's master key). A single side override is reported by `Ready=False` (`ReconcileError`) and
leaves this condition as it was. It does not change the stack's `phase`, and is written only when
it changes.

### Component storage

The shared `VolumeSpec` is available at `spec.components.engine.writer.storage`, `spec.components.engine.reader.storage`,
`spec.components.{analytics,cache,broker,search,controlDB}.storage` and `spec.components.ingestion.harvestMongoStorage` (HARVEST's Mongo).

| Field | Type | Default | Behavior |
|---|---|---|---|
| `storage.size` | positive Kubernetes quantity | `10Gi` | Omitted storage, `{}`, and omitted size all resolve to the default, including objects constructed directly in Go. Admission rejects malformed, zero and negative quantities. Quantities compare numerically: `10240Mi` equals `10Gi`. |
| `storage.storageClassName` | nonempty string, optional | cluster default on creation | Selects the new PVC's class, or CNPG's `spec.storage.storageClass`. Omission on existing resources retains their class, including a class populated by default admission. A later cluster-default change does not move existing storage. |
| `storage.approveDataDeletion` | bool | `false` | `false`: a volume the operator can't change in place (another class, or larger than a declared `size`) is left as it is, and `StorageApplied` says so. `true`: the operator replaces it, erasing its data ([below](#replacing-a-volume-storageapprovedatadeletion)). |

For example, `engine.writer.storage: {size: 50Gi, storageClassName: managed-csi}` and
`cache.storage: {size: 20Gi}` set independent sizes. These are examples, not production sizing.
For an `OramaCloudV1Set`, put the component settings under `spec.template.components`.
`reader.storage` applies to every reader's claim, `data-<cr>-reader-<n>`; it defaults to `10Gi`, and production's
size is explained in [components.md](components.md#engine-internalcontrollerenginego). `ingestion.harvestMongoStorage` works like
every other store's `storage`, and is managed while ingestion and HARVEST are enabled.

New StatefulSets receive a `data` claim template at the resolved size/class. Existing templates
remain intact because Kubernetes makes them immutable. The operator grows each matching claim's
storage request itself, including retained claims and claims created later from an old template.
The template's old size does not cause a storage mismatch. For controlDB it grows the CNPG
Cluster's configured size; CNPG owns expansion of its PVCs. Other CNPG storage keys remain intact.

An existing volume grows in place. A different class or a smaller size cannot be applied in place:
the operator replaces the volume when its `storage.approveDataDeletion` is `true`
([below](#replacing-a-volume-storageapprovedatadeletion)), and otherwise reports the difference. Removing `storage` after growth requests the
`10Gi` default and can therefore report an unsupported shrink. Restore the grown request to clear
that rejection. A larger reported capacity alone is not a shrink: provisioners can round capacity
above the request. A claim with a class mismatch receives no size update, even if growth was also
requested. Other claims/components continue reconciling.

`status.conditions[type=StorageApplied]` is independent of `Ready`, phase, and workload readiness:

| Status / reason | Meaning |
|---|---|
| `True / Applied` | Every applicable existing PVC request and CNPG storage configuration accepts the desired size/class, or there is no locally managed storage. It does not prove disk or filesystem expansion has completed. |
| `False / NotApplied` | At least one resource has a problem. The message lists each resource in stable sorted order with one of: `ShrinkNotSupported` (desired size below an existing claim request or CNPG configured size), `StorageClassChangeNotSupported` (an explicit class differs from an existing resource's class, including a classless existing CNPG Cluster), `StorageUpdateFailed` (a storage read or write failed; the API error is included) or `ClaimsPending` (a required claim has not appeared yet; the operator does not create it). |

The condition observes the current generation and recovers after accepted settings or successful
retries. A rejected setting may coexist with `Ready=True`. The operator polls every 20 seconds
while locally managed storage exists, even with Ready workloads, and writes status only on change.
See [component reconciliation](components.md#component-storage) for scope and expansion ownership.

### Replacing a volume (`storage.approveDataDeletion`)

A volume's class can't change, and a StatefulSet's claims outlive it, so a volume on another class, or larger
than its declared `size`, can only reach its declared storage by being replaced
([#915](https://github.com/oramasearch/orama-operator/issues/915)). With `approveDataDeletion: true` the operator
does that, and the volume's data is lost:

```yaml
engine:
  writer:
    storage:
      size: 512Gi
      storageClassName: managed-csi-premium
      approveDataDeletion: true
```

- **A StatefulSet's volume** (writer, readers, analytics, broker, cache, search, HARVEST's Mongo): the claims
  that differ, and the StatefulSet, are deleted and created again. Claims that already match are kept.
- **The control DB**: the CNPG Cluster is deleted and created again, and its schemas, reference rows and `c1`
  row run again on the new database.
- **Analytics** also restarts analytics-processor, which creates ClickHouse's tables when it starts.

It is desired state, not a one-time request: every pass compares the spec with the cluster, leaves a matching
volume alone, and handles a later change the same way. Only declared values count, so `true` with no `size`
never erases a volume that grew past `10Gi`. While a replacement runs, no component is reconciled, and `Ready`
and `StorageApplied` are `False / ReplacingStorage` with what is left. A claim another controller owns is
never replaced. Copying the data to the new volume is not supported yet.

### `status.conditions[ClusterRowReady]`

Present while the stack inserts its own `oramacore_clusters` row: an enabled in-cluster control DB,
outside a read region, with the engine enabled. `True` (`RowPresent`) once the Job
`<cr>-controldb-cluster-row` succeeded, meaning the control DB has a row: the Job inserted the `c1`
row into an empty table, or found a row and left it unchanged (its log says which). `False` with
`WaitingForPublicHosts` (`engine.writer.host` or `engine.reader.host` is unset; the row records only
public URLs, so it waits), `WaitingForSchema` (the control DB schema is not applied yet),
`WaitingForMasterAPIKey` (the master key is missing or a placeholder; the row is written once, so it
waits), `Inserting` (the Job is running) or `JobFailed` (the message gives the time the operator
replaces the Job). `True` reports the Job, not the table: a succeeded Job is never re-run, so a
control DB emptied later (a restore) still shows `True`, and the row keeps the master key it was
inserted with even after `spec.masterAPIKeySecretRef` changes. It does not
change the stack's `phase`; while it is not `True` the CR keeps its 20-second readiness requeue. See
[components.md](components.md#controldb-internalcontrollercontroldbgo).

## `OramaCloudV1E2ETest` (separate CRD)

Runs the **comprehensive functional e2e suite** as a Job against a deployed stack — a test is a
first-class, repeatable resource (the operator never runs suites during normal reconcile). The suite
(`internal/controller/assets/e2e-functional.sh`, embedded via `go:embed` and shipped in a ConfigMap)
is a curl+jq script that self-bootstraps a clean-slate collection+index and asserts **every critical
OramaCore function in variations** — liveness, auth (keys + negatives), collection/index lifecycle +
document CRUD/upsert/delete, the write→searchable replication canary, fulltext (typo tolerance/exact/
threshold/boost/pagination), vector + hybrid, filters (number/date/bool/string/geo + and/or/not),
facets, sort, groupBy, batch-get, KV, hooks, merchandising, MCP, and answer/RAG. It never calls the
analytics export, which erases the readers' search events: only the analytics processor pulls them
([#315](https://github.com/oramasearch/orama-operator/issues/315)). It is the
forcing function (see the production-migration principle in `CLAUDE.md`) — never softened to pass.
Coverage is tracked by [operator#44](https://github.com/oramasearch/orama-operator/issues/44).
Sample: [`../config/samples/e2e-test.yaml`](../config/samples/e2e-test.yaml).

The ConfigMap carries three scripts: `functional.sh` (the engine sections), `lib.sh` (the HTTP and
assertion helpers both use) and `control-plane.sh` (sections 21-24, the control plane around the
engine), plus `config.json`, which names the target's writer and reader, its in-cluster dashboard
(`http://<target>-dashboard:3000`, or none), whether its writer verifies dashboard write tokens
(`engine.writer.jwt`), and its edge worker Services (the workers bundle and the four hosted-search
workers, each only when enabled). The runner's pod carries the target's labels with component `e2e`,
so the stack's network sandbox applies to it and admits it to exactly those Services
([`network-sandbox.md`](infrastructure/network-sandbox.md)).

The control-plane sections ([#61](https://github.com/oramasearch/orama-operator/issues/61),
[#63](https://github.com/oramasearch/orama-operator/issues/63)):

- **21. Dashboard.** The JWKS holds RSA public keys and no private member. `/api/user/jwt` refuses a
  scope other than `write`, a missing or wrong private API key, and a project's key for another
  tenant's collection, never with a token and never with a 2xx. Session routes refuse a caller with no
  session or a forged `_orama_session` cookie (including `indexes/<id>/update` and `airbyte/*`,
  [dashboard#106](https://github.com/oramasearch/dashboard/issues/106)); `/api/private/v1/get-job`
  refuses a caller without `PRIVATE_API_KEY`; the CSRF guard refuses a foreign `Origin` (403) and a
  non-JSON body (415); the Stripe webhook refuses an unsigned or badly signed event.
- **22. Write tokens** (needs the fixture). A token for the fixture project is RS256, has `sub` = the
  project, `scope` `write`, the plan cap `lmt.max_doc_count`, at most ten minutes of life, an issuer,
  the project's writer URL as audience and a `kid` in the JWKS. On the writer it is accepted on its
  own collection, refused on another tenant's collection with nothing landing there, and refused when
  its payload is tampered with, when unsigned (`alg: none`) and when garbage; one document past the
  plan cap is refused with 402 (skipped when the cap needs more than 2000 inserts). With
  `readerSessionCookie`, a member with read access only gets no private API key, is refused on a
  write route, and is refused on another tenant's project.
- **23. Edge workers.** Each in-cluster worker answers `/health` and refuses a call without its key
  and with a wrong one (segmenter `X-API-KEY`, openai-decrypter `x-api-key`, telemetry, large-index and
  index-shared `api-key`). A 2xx always fails, so the decrypter's `200 {}` on every failure and the
  segmenter's fail-open are caught once the routes run in-cluster; the scaffold's explicit 501 "not yet
  wired in-cluster" is skipped and names [#6](https://github.com/oramasearch/orama-operator/issues/6).
  secure-proxy, worker-mcp and worker-telemetry run on Cloudflare, not in the stack, and are listed as
  skipped.
- **24. Expired write token** (needs the fixture). The token minted right after liveness is refused
  with 401 and an expiry message once its `exp` plus the writer's 60-second leeway has passed. The
  suite waits for that, at most `E2E_JWT_EXPIRY_MAX_WAIT` seconds (default 900).

Every credential stays in a variable; failure messages replace `privateApiKey`, `apiKey`,
`readerApiKey`, `writerApiKey` and `jwt` values before logging a body.

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaCloudV1` to test (same namespace); the Job points at `<targetRef>-writer` / `-reader`, and its runner takes `MASTER_API_KEY` from the target's `spec.masterAPIKeySecretRef` when the Job is created. Until the target exists and sets that reference the test stays `Pending` and is checked again every 20 seconds. An existing Job is immutable and keeps the reference it was created with. |
| `spec.image` | **Runner base image** (default `alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc`). New Jobs also honor the target stack's `images.requireDigests`. Must provide `sh` + a package manager — the Job installs curl+jq into it, then runs the embedded suite. Override only with another curl/jq-capable base. |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `spec.dashboardFixtureSecretRef.name` | Optional. A Secret in the test's namespace for the write-token sections (22, 24): `collectionID`, `indexID` and `apiKey` (a dashboard project of the target that the suite may write to, and its private API key, as in the e2e canary's Secret), required and non-empty; `readerSessionCookie`, optional, the `_orama_session` cookie of a member with read access only to that project (it lasts seven days). The runner reads each key through a `secretKeyRef`; none reaches the ConfigMap. Until the Secret exists with the three keys the test stays `Pending`. Omitted, sections 22 and 24 are skipped with the reason. |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ocv1e2e`. |

## `OramaCloudV1Migration` (separate CRD, namespaced) — one Prod 1 team copied into a stack

Copies one Prod 1 team into the `OramaCloudV1` named by `spec.targetRef`, in the same namespace
([#653](https://github.com/oramasearch/orama-operator/issues/653)). Each step runs the
`cloud-v1-migration` image's `run-step` as an owned Job, once per `(step, apply, attempt)`, and the
tool's `status --json` becomes `status.result`. What it checks before a Job, the Job's environment,
the migration role, the network paths and deletion: [`cloud-v1-migration.md`](cloud-v1-migration.md).
Short name `ocv1m`. Sample: [`../config/samples/oramacloudv1migration.yaml`](../config/samples/oramacloudv1migration.yaml).

`targetRef`, `teamID`, `projectIDs`, `runID` and `source` cannot change after creation (CEL: *the
target, team, projects, run and source are immutable; declare a new migration instead*).

On a managed cluster it is declared in the stack's `OramaCloudV1Set` (`spec.migrations[]`, below),
which applies it labelled `orama.cloud/managed-by-set` with `targetRef` set to the member.

| Field | Purpose |
|---|---|
| `spec.targetRef` | The `OramaCloudV1` in this namespace that receives the team. |
| `spec.teamID` | The Prod 1 team, a lowercase UUID. |
| `spec.projectIDs[]` | The team's complete, reviewed project set (1 to 1000 UUIDs, a set). The tool refuses a list that differs from the team's projects. Passed sorted and comma-joined. |
| `spec.runID` | The tool's durable run, resumed by every step (DNS label, at most 40 characters). |
| `spec.step` | `plan`, `seed`, `catchUp` or `verify`: how far the migration should go. Each change runs that step once. |
| `spec.apply` | Lets `seed` and `catchUp` write to the target (`--apply`); without it they run the tool's dry run. Ignored by `plan` and `verify`. |
| `spec.attempt` | Runs the current step again when raised (Job `<name>-<step>-<attempt>`); during deletion, retries a removal that did not finish. Minimum 0. |
| `spec.evidenceSecretRef` | `{name, key}` of the reviewed isolation receipt, mounted read-only at `/evidence/isolation.json` for `seed` and `catchUp`, which refuse to run without it. The operator never creates it. |
| `spec.image` | The `cloud-v1-migration` image, pinned by digest (`name@sha256:<64 hex>`). |
| `spec.pullSecretName` | Pull Secret for `image`. The pod also gets the stack's `spec.images.pullSecretName` for its postgres-client init container. |
| `spec.runVolume` | `{size, storageClassName}` of the run volume `<name>-runs` (`ReadWriteOnce`, default 20Gi on the cluster's default class; a larger size grows it when the next Job starts). It holds customer payloads. |
| `spec.removeOnDelete` | On deletion, runs `run-step remove-target --apply` when an applied seed or catch-up started, before deleting the run volume. The tool refuses (exit 3) once the team was switched, and the resource then stays. |
| `spec.source.databaseSecretRef` | `{name, key}` of the SELECT-only PlanetScale URL (`planetscale-dump-ro`). |
| `spec.source.databaseHost` | The host in that URL, used only to name the Prod 1 egress; the URL stays in the Secret. |
| `spec.source.writerURL`, `.readerURL` | Prod 1's engine origins, `https://<host>` with no port or path. |
| `spec.source.masterKeySecretRef` | `{name, key}` of Prod 1's engine master key. |
| `spec.source.deploymentRevision`, `.schemaRevision` | The reviewed Prod 1 revisions (`v1.2.39`, `890a7e97` on 2026-10-19). |
| `spec.source.settings` | Where the writer's `info.json` is read over SSM: `region` (`us-east-2`), `instanceID` (`i-` and 8 to 17 hex), `document` (the SSM document's name), `dataDir` (the writer's `writer_side.collections.data_dir`, absolute), and `credentialsSecretRef.name`, a Secret with `access_key_id` and `secret_access_key` for an IAM user that may run only that document on that instance. |
| `status.phase` | `Pending` (waiting for an earlier Job, the target or a Secret) \| `Running` \| `Succeeded` (exit 0) \| `Blocked` (refused by the operator, or by the tool with exit 3) \| `Failed` \| `Removing`. |
| `status.step`, `.attempt`, `.apply`, `.jobName` | What the last Job ran (`step` is `remove` for the removal). |
| `status.result` | The tool's `status --json` from the Job's termination message, as the API server stores it. Absent when the message is not a JSON object. |
| `status.message`, `.startedAt`, `.completedAt`, `.observedGeneration` | The last transition's explanation and the last Job's times. |
| `status.conditions[Seeded]` | `True` once an applied seed succeeded under this resource; `catchUp` and `verify` need it. Never cleared. |
| `status.conditions[TargetWritten]` | `True` once an applied seed or catch-up started; deletion with `removeOnDelete` then removes the team. Never cleared. |
| `status.conditions[Removed]` | `True` once the removal on deletion succeeded; it never runs again, even once its Job is gone. Never cleared. |

Print columns: Target, Step, Apply, Phase, Age. Status is written only on a real transition.

## `OramaZettaCluster` (separate CRD) — step 1 of One Orama

Deploys a **headless Orama Zetta cluster** (a StatefulSet of equal-peer CRDT nodes) from one CR — the
first step of the [One Orama](https://github.com/oramasearch/orama-bridge/tree/main/docs/projects/one-orama)
convergence ([orama-bridge#130](https://github.com/oramasearch/orama-bridge/issues/130)). It creates a
**headless Service** (peer DNS on 7777), a **client Service** (the HTTP API on 4800 — MCP + REST +
serving), and the **StatefulSet** (each node a per-node PVC at `/data`; `node-0` bootstraps, peers join
it). With `spec.host` set it also fronts the host with an **nginx Ingress** and — when `spec.dnsSecretRef`
is given — an **owned `CloudflareDNSRecord`** (a proxied A/CNAME record → the ingress LoadBalancer),
reusing the same Cloudflare child-controller path as the dashboard DNS. A `CloudflareDNSRecord` whose
name (or type) changes deletes the record it made before (`status.recordID`) once the new one exists,
so a host change never leaves the old name live (#529), but only while that record is still its own
(see the `CloudflareDNSRecord` section); a record already gone counts as deleted.
A `CloudflareDNSRecord` takes `type` `A`, `CNAME` or `TXT`; `proxied` is always written (default
`true` only when left out), so a controller's `false` is kept; and `deletionPolicy: Retain` leaves the
Cloudflare record when the resource is deleted (`Delete`, the default, removes it), which a Lake's Clerk
records use because every Lake sharing the instance writes them (operator#541). The Ingress proxies the **whole
host** to the Zetta node's own HTTP server on `:4800`: the bare root (`/`) is the node's **web dashboard**
([zetta#647](https://github.com/oramasearch/zetta/issues/647)) and every other path is MCP/REST/serving.
(An earlier operator version parked a static `zetta` placeholder at `/` while the node HTTP server was
unreliable; that shadowed the working dashboard and has been removed — the operator now also tears down
any leftover `<name>-zetta-landing` Deployment/Service/ConfigMap.)
This stage only **runs** Zetta headless — no operator↔Zetta symbiosis and **no** changes to Zetta's MCP
service. Sample: [`../config/samples/oramazettacluster.yaml`](../config/samples/oramazettacluster.yaml).

| Field | Purpose |
|---|---|
| `spec.replicas` | Number of equal-peer headless zetta nodes (default 1). |
| `spec.image` | Zetta-headless image (default `oramasearch/zetta:headless-v1-sandbox`), built by [`zetta-bridge/build/images/zetta-headless`](https://github.com/oramasearch/zetta-bridge/tree/main/build/images/zetta-headless). |
| `spec.pullSecretName` | Pull secret for the private zetta image. |
| `spec.storage.size` / `.storageClassName` | Per-node PVC at `--data-dir` (`/data`) — the CRDT doc, `node_id`, certs, JSONL/large-object data. They shape the StatefulSet's claim template only when the StatefulSet is created: the template is immutable, so a later change is kept in the spec and never rewrites it. Whoever raises the size grows the existing claims in place (the Lake workspace reconciler does, operator#259); a replica added later starts at the template's size. While spec.storage differs from the template, `status.conditions[StorageTemplateCurrent]` is `False` with reason `ClaimTemplateUnchanged`, saying who grows the claims (the workspace reconciler, or nobody for any other cluster). |
| _(pod security and volume ownership — automatic)_ | Zetta pods mount no ServiceAccount token, require non-root uid/gid/fsGroup 10001, use `RuntimeDefault` seccomp, disallow privilege escalation and drop all capabilities. `fsGroupChangePolicy: OnRootMismatch` avoids a recursive ownership change when a `/data` volume already has the correct group. This satisfies restricted Pod Security Admission when host networking is off. |
| `spec.resources` | Per-node mem/cpu caps (right-sizing). |
| `spec.env[]` | Kubernetes `EnvVar` entries appended to the Zetta container after operator-managed variables; `value` and `valueFrom` are supported. Changes update the StatefulSet pod template and roll the nodes. `POD_NAME`, `HOST_IP`, `ZETTA_CLUSTER_TOKEN`, `ZETTA_ADMIN_TOKEN`, `ZETTA_HOST_NETWORK`, `ZETTA_ADVERTISE_MAP`, `ORAMA_ZETTA_SEED`, and the engine's own provisioning variables `ZETTA_SEED_PEER` and `ZETTA_DATA_DIR` (an unvalidated peer, or a data dir off the PVC — [operator#279](https://github.com/oramasearch/orama-operator/issues/279)) are reserved: operator values win, and collisions appear in `status.ignoredEnvVars`. Use this for engine settings such as `ZETTA_MALLOC_TRIM_THRESHOLD_BYTES` and `MALLOC_ARENA_MAX`. |
| _(cluster join token — automatic)_ | Inter-node peer traffic is **always mTLS**. The operator mints one shared cluster join token per cluster into Secret **`<name>-zetta-cluster`** (`token` = 64-hex, minted once, never rotated) and injects it into **every** node as the read-only file `/run/secrets/zetta-cluster/token` (`ZETTA_CLUSTER_TOKEN_FILE`, [zetta#770](https://github.com/oramasearch/zetta/issues/770)) and, for images that predate it, `ZETTA_CLUSTER_TOKEN`; Zetta derives the cluster CA deterministically from it, so all nodes trust each other and form **one** cluster ([operator#92](https://github.com/oramasearch/orama-operator/issues/92)). There is no plaintext-peer knob — an unencrypted mode was removed as a security hole. See [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md). |
| `spec.clusterName` | The **GLOBAL** Zetta cluster name. CRs (in the same or other k8s clusters) sharing it are **siblings** — equal peers of one multi-leader cluster ([operator#71](https://github.com/oramasearch/orama-operator/issues/71)); this CR is a **slice**. Empty = a standalone cluster named after the CR. |
| `spec.externalPeers[]` | Route table ([operator#70](https://github.com/oramasearch/orama-operator/issues/70)) — `host:port` addresses of **wan-accessible** siblings, each appended as `--peer` so this slice's nodes dial OUT and join the shared cluster. Copy a sibling's `.status.wanAddress` here. Zetta is the mesh (gossip relays transitively), so a connected dial-graph is enough. **Each entry must be exactly `host:port`** — a DNS name, an IPv4 address or a bracketed IPv6 address (`[fd00::1]:7777`), port 1–65535; at most 64 entries ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). The API server refuses anything else (CRD pattern). A CR stored before that rule with an invalid entry is **refused by the controller**: the StatefulSet is left unchanged, `status.phase` is `Degraded` and `PeerAddressesValid=False` (`InvalidSpecPeerAddress`) names the bad entries. Peers reach Zetta as process arguments, never as shell text. |
| `spec.reachability` | **Default `dial-out-only`.** The node-reachability type ([zetta#739](https://github.com/oramasearch/zetta/issues/739)): `dial-out-only` (behind NAT / a private VNet — dials OUT only, **never gets an inbound LoadBalancer**) or `wan-accessible` (a routable rendezvous others dial out to — the operator advertises its address as `.status.wanAddress`, which siblings put in `externalPeers` and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) consumes). Also rendered INTO the engine: a `dial-out-only` slice gets `--dial-out-only` ([zetta#803](https://github.com/oramasearch/zetta/issues/803)) so its nodes advertise no dialable address and `system.nodes.reachability` reflects it. |
| `spec.ephemeral` | **Default `false`.** Marks the slice as burst/autoscaled capacity expected to churn ([operator#69](https://github.com/oramasearch/orama-operator/issues/69)). **Always forced to `dial-out-only`** (an ephemeral node is never advertised as a rendezvous — its address would flap); its membership blips aren't treated as a cluster fault. Rendered into the engine as `--node-type ephemeral` (else `--node-type permanent`, [zetta#810](https://github.com/oramasearch/zetta/issues/810)) → `system.nodes.node_type`; permanent nodes carry the durability-bearing replication factor, ephemeral ones don't. |
| `spec.wanAddress` | For a `wan-accessible` slice, the routable address (`host` or `host:port`; `:7777` assumed) siblings use — a mesh-VPN address, a stable public IP, or a DNS name. **Authoritative** when set (advertised verbatim as `.status.wanAddress`); when empty the operator derives it from `spec.hostNetwork` (node host IP) or `spec.exposePeerLoadBalancer` (cloud LB IP). Ignored unless `wan-accessible`. Validated like `externalPeers` (DNS name, IPv4, `[IPv6]` or a bare IPv6 literal, optional port 1–65535) by the CRD pattern and the controller; an invalid stored value is refused the same way (`PeerAddressesValid=False`), whatever the reachability ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). |
| `spec.hostNetwork` | **Default `false`.** Run node pods on the host network so `:7777`/`:4800` are reachable at the k8s node's own address — the simplest routable address for a **wan-accessible** slice (esp. single-node). Packs ordinals onto per-ordinal ports when `replicas>1`. |
| `spec.exposePeerLoadBalancer` | **Default `false`.** A cloud `LoadBalancer` fronting the peer port `:7777`. **Valid ONLY for a `wan-accessible` slice** — on a `dial-out-only`/ephemeral slice it is **ignored** (a warning is logged, any existing LB torn down) because a private node must dial OUT, not accept inbound (the [zetta#739](https://github.com/oramasearch/zetta/issues/739) anti-pattern). Even for wan-accessible it's only one option (a host with a public IP or a `wanAddress` needs none). Peer traffic is **mTLS** on the wire (the shared cluster join token derives the CA, [operator#92](https://github.com/oramasearch/orama-operator/issues/92)), so keep the `<name>-zetta-cluster` Secret protected. |
| `spec.provisionOperatorSchema` | **Default `true`.** Makes this cluster the operator's own datastore (One Orama symbiosis v0): once the node is up, an in-cluster Job applies the operator's schema via Zetta's `/v1/sql` — the **`orama_control`** database (`components`, `reconcile_events`, `topology`, `deployments`, `deployment_records`, and lake's `environment_env` and `domains`) and the **`orama_data`** database (`documents`, the app-data landing zone). Idempotent (tolerates "already exists"; Zetta has no `IF NOT EXISTS`). A rendered-schema hash automatically replaces the immutable Job when `operatorZettaSchema` changes. Set `false` to opt out. |
| _(readiness probe — disabled)_ | The pod's readiness probe is **off** ([operator#68](https://github.com/oramasearch/orama-operator/issues/68)) because the node's flaky HTTP server ([zetta#731](https://github.com/oramasearch/zetta/issues/731)) made it flap out of the Service. Liveness + startup (peer port) remain. Restore `GET /mcp.md` readiness once zetta#731 lands. |
| `spec.mcpTokenAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed the `--mcp-token` flag (passing it now crashes the node), so the operator no longer renders it or provisions a `<name>-zetta-mcp` Secret. Auth is enforced with the seeded admin token instead (see `spec.requireAuth`). Retained for API compatibility; setting it has no effect. |
| `spec.requireAuth` | **DEPRECATED / no-op** as of [zetta#799](https://github.com/oramasearch/zetta/issues/799). The engine made auth **mandatory** and removed `--require-auth` — every node ALWAYS gates its HTTP/MCP/SQL surface on a bearer that resolves to a user; there is no unauthenticated/plaintext/legacy-shared-secret mode. The operator now **ALWAYS** mints a 64-hex admin token into Secret `<name>-zetta-admin` (key `token`, mint-once), injects it as env `ZETTA_ADMIN_TOKEN` on every node, from which the engine seeds the owner (never as `--admin-token`: argv is readable by every process on the node, [operator#279](https://github.com/oramasearch/orama-operator/issues/279); redacted to `****` in the pod log), authenticates its **own** Zetta SQL client + schema-provisioning Job with it, and the `ZettaClusterE2ETest` always runs its authz assertions (`ZETTA_E2E_REQUIRE_AUTH=1`). **Image requirement:** the target's image MUST be an **auth-capable build** ([zetta#799](https://github.com/oramasearch/zetta/issues/799)) — the pre-#799 sandbox image rejects it. The field is retained for API compatibility; setting it has no effect (auth is enforced regardless). Sample: [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml). |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** When `true` **and** `spec.host` is set, the operator provisions the nginx Ingress (+ the Cloudflare DNS record below if `dnsSecretRef` is set), proxying the whole host to `:4800`. When `false` (or `host` empty) **nothing external is created and any previously created Ingress/DNS are torn down** — even if `host` is still set. Lets you keep `host` recorded while toggling exposure off, instead of overloading "empty host" as the off switch. |
| `spec.host` | The **domain name** the API/UI (:4800) is exposed at (e.g. `zetta.oramadev.net`) when `ingressEnabled` is `true` — the whole host proxies to the node's `:4800`, so root `/` is the node's **web dashboard** and other paths are MCP/REST/serving. Requires an ingress controller. Names the endpoint; `ingressEnabled` is the on/off switch. |
| `spec.ingressClassName` | `nginx` (default) or `zetta-tunnel`: which ingress serves the cluster's Ingress; DNS follows ([`infrastructure/ingress.md`](infrastructure/ingress.md)). |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `spec.host`, create a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord` CR, using the named Secret (keys `token`/`zoneID`/`accountID`) — normally the same `cloudflare-api-token` Secret the `OramaCloudV1` uses. |
| `spec.capacity.karpenter.enabled` | **Default `false`. Opt-in elastic node capacity** ([operator#69](https://github.com/oramasearch/orama-operator/issues/69), first slice). When `true`, the operator owns a Karpenter **`NodePool`** and pins the Zetta pods onto it (a dedicated taint `orama.cloud/zetta:NoSchedule` + toleration and the `orama.cloud/zetta-pool` nodeSelector), so raising `spec.replicas` past current node capacity leaves pods **Pending → Karpenter provisions matching nodes just-in-time →** the new pods schedule and join the CRDT cluster. Scheduling-driven scale-up; the Zetta-signal-driven trigger (`orama_control.scale_requests`) is a later slice. |
| `spec.capacity.karpenter.nodeClassRef` | The provider **NodeClass** Karpenter launches nodes from — `{group, kind, name}` (the Karpenter v1 `nodeClassRef` shape). Defaults `group: karpenter.k8s.aws`, `kind: EC2NodeClass` (EKS); on AKS NAP set `group: karpenter.azure.com`, `kind: AKSNodeClass`. **`name` is REQUIRED when enabled** — the operator owns only the cloud-neutral NodePool; the NodeClass (AMI/subnet/IAM on AWS) is platform-owned. Missing name ⇒ `CapacityReady=False, Reason=Blocked`. |
| `spec.capacity.karpenter.capacityType` / `.arch` | `on-demand` (**default**, stateful-safe) \| `spot`; `amd64` (**default**) \| `arm64`. Spot only with enough replicas to tolerate an interruption (a reclaim evicts a peer). |
| `spec.capacity.karpenter.instanceTypes[]` | Optional pin via the portable `node.kubernetes.io/instance-type` label. Empty (recommended) ⇒ Karpenter chooses the cheapest fitting type from the NodeClass offerings. |
| `spec.capacity.karpenter.limits` | `{cpu, memory, nodes}` — the **cost/scale guardrail**; Karpenter stops provisioning once any cap is hit. Defaults `cpu: "16"`, `memory: 64Gi`, `nodes: 3`. |
| _(disruption / quorum-safety)_ | The generated NodePool is **stateful-safe**: `consolidationPolicy: WhenEmpty` (never evict a live peer for underutilization), a `nodes: "1"` disruption budget (cap churn), `expireAfter: Never`, `terminationGracePeriod: 1h`; the Zetta pods carry `karpenter.sh/do-not-disrupt` and a `maxUnavailable: 1` PodDisruptionBudget. Operator-driven drain-safe **scale-down** is a later slice. |
| _(Karpenter absent)_ | If the `karpenter.sh/v1 NodePool` CRD isn't installed (bare metal / no autoscaler), enabling capacity is a **graceful no-op**: `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. `spec.replicas` still packs more pods onto the fixed host until full. Or set `install: true` (below) to have the operator install Karpenter itself. |
| `spec.capacity.karpenter.install` | **Default `false`. Opt-in — make the operator OWN the Karpenter install** ([operator#75](https://github.com/oramasearch/orama-operator/issues/75)). When `true`, the operator server-side-applies the pinned upstream Karpenter controller (Deployment + ServiceAccount + RBAC) and the Karpenter CRDs (`NodePool`/`NodeClaim`/`EC2NodeClass`) as owned resources (modelled as `unstructured` — **no Karpenter Go types vendored**; version pinned to **v1.13.0** under `internal/controller/assets/karpenter/`). **Cloud-gated:** a clean no-op on bare-metal/k0s (`Reason=BareMetalNoCloud`) and on AKS, where **Node Auto Provisioning already manages Karpenter** (`Reason=ManagedByAKSNAP` — use `enabled`, not `install`); the self-install path targets **EKS**. Surfaced via the `KarpenterInstalled` condition. **Only a CR in the operator's own namespace may install** (the controller is one per cluster and its IRSA role decides the cloud account it acts in); a CR elsewhere gets `Reason=InstallNamespaceNotAllowed`, and a second CR there with different values gets `Reason=ConflictingInstallSpec` ([operator#280](https://github.com/oramasearch/orama-operator/issues/280)). |
| `spec.capacity.karpenter.controllerRoleARN` | **The cloud IAM identity the operator CANNOT mint** — a cloud-account prerequisite, same class as the image pull secret. The AWS IAM role ARN Karpenter's **controller** assumes (via IRSA — annotated onto its ServiceAccount) to create/terminate EC2 instances. **Required when `install` on EKS** (unless wired out-of-band, e.g. EKS Pod Identity); empty ⇒ `KarpenterInstalled=False, Reason=CloudIdentityMissing` and **the controller is not deployed** (no crash-loop). This is the *controller* identity; the launched *nodes* get their own instance profile via the platform-owned `nodeClassRef`. See [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md). **Validated** ([operator#79](https://github.com/oramasearch/orama-operator/issues/79)): pattern `^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_\-/]{1,512}$`, `maxLength` 550 — the API server refuses anything else, and the reconciler re-checks (`KarpenterInstalled=False, Reason=InvalidInstallSpec`). Set on the parsed ServiceAccount as a structured annotation, never pasted into YAML. |
| `spec.capacity.karpenter.installClusterName` | The AWS EKS cluster name Karpenter's controller uses (`settings.clusterName`) to discover the cluster's tagged subnets/security-groups/interruption queue. **Required when `install` on EKS**; empty ⇒ `KarpenterInstalled=False, Reason=InstallClusterNameRequired`. **Validated** (operator#79): the EKS cluster-name rule `^[0-9A-Za-z][A-Za-z0-9\-_]{0,99}$`, `maxLength` 100 — refused at admission, and re-checked by the reconciler (`InvalidInstallSpec`). Set on the parsed Deployment as the controller's `CLUSTER_NAME` env value, never pasted into YAML. |
| `KarpenterInstalled` condition | Present only when `install` is set. `InvalidInstallSpec` (a stored `installClusterName` or `controllerRoleARN` fails the pattern; nothing is applied, on any cloud) / `InstallNamespaceNotAllowed` / `ConflictingInstallSpec` (operator#280) / `BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud` / `CloudIdentityMissing` / `InstallClusterNameRequired` (gated or blocked) → `InstallProgressing` (applied, controller not yet Available) → `Installed` (controller available in `kube-system`). Written **only on change** (anti-flap). |
| `status.phase` / `.readyNodes` / `.totalNodes` | Readiness roll-up. `kubectl get ozc`. |
| `status.provisionedNodes` / `CapacityReady` condition | Number of nodes Karpenter has provisioned for this cluster (`karpenter.sh/nodepool=<pool>`), plus a `CapacityReady` condition (`Disabled`-dropped / `Blocked` / `KarpenterNotInstalled` / `NodePoolProgressing` / `NodePoolReady`). Written **only on change** (anti-flap). |
| `status.reachability` / `.ephemeral` | The **effective** reachability type of the slice (a print column) + whether it's ephemeral — echoed from spec (`reachability` forced to `dial-out-only` when `ephemeral`). |
| `status.wanAddress` | The routable rendezvous address (`host:port`) siblings dial OUT to when this slice is **wan-accessible** — copy it into a sibling's `spec.externalPeers`, and the Cloudflare LB origin pool ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)) is populated from the wan-accessible set (its HTTP origin is the same host at `:4800`). Empty for a `dial-out-only` slice or until the address is known. Shown by `kubectl get ozc -o wide`. |
| `status.ignoredEnvVars[]` | Sorted, de-duplicated names from `spec.env` that collided with operator-managed variables and were not added to the container. This observation does not degrade cluster readiness and is written only when it changes. |
| `PeerAddressesValid` condition | Whether every peer address the slice uses is a valid `host:port` ([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). `True`/`Valid` normally. `False`/`InvalidSpecPeerAddress`: an invalid `spec.externalPeers` or `spec.wanAddress` — the StatefulSet is not updated until the spec is fixed and `status.phase` is `Degraded`. `False`/`InvalidTopologyPeerDropped`: rows read from the shared `orama_control.topology` table carried invalid addresses; they were dropped (quoted in the message) and the cluster reconciles with the remaining peers. Left unchanged on a round where the topology could not be read, so a flaky read does not flap it — except a stale `InvalidSpecPeerAddress`, which clears as soon as the spec is valid. Values in the message are Go-quoted (no raw newlines). |

**Live (`oramadev.net` sandbox):** `GET https://zetta.oramadev.net/` returns the node's **web dashboard**,
and `GET https://zetta.oramadev.net/v1/server_info` returns the node's server info from the Zetta API —
both through Cloudflare (proxied), the same reachability path as `app.oramadev.net` → the dashboard.

## `ZettaClusterE2ETest` (separate CRD)

Runs the **live-cluster zetta e2e suite** as a Job against a deployed `OramaZettaCluster` — a test is a
first-class, repeatable resource (the operator never runs suites during normal reconcile). **Unlike the
`OramaCloudV1E2ETest` suite** — which is embedded in the operator via `go:embed` — the Zetta suite lives
in the **zetta repo** (`tests/e2e/zetta-e2e.sh`) and is **baked into the `oramasearch/zetta-e2e` image**
(its `ENTRYPOINT` runs the script) so it evolves in lockstep with the engine; this CRD only **triggers**
it. The reconciler reads the target cluster's `spec.replicas` and wires the cluster addressing to the
suite via env (the exact contract the script reads — **do not rename**):

| Env var | Value |
|---|---|
| `ZETTA_E2E_BASE_URL` | `http://<targetRef>-zetta-0.<targetRef>-zetta-headless.<ns>.svc.cluster.local:4800` — the writer, pinned to node-0 via per-pod DNS. |
| `ZETTA_E2E_PEER_URLS` | Space-joined per-pod DNS of ordinals `1..replicas-1` (`<targetRef>-zetta-<i>.<targetRef>-zetta-headless.<ns>.svc.cluster.local:4800`); empty for a single node. |
| `ZETTA_E2E_LEVELS` | `spec.levels` (default `2,3`). The suite runs L2 (`zetta::testkit`) + L3 (black-box integration over the deployed surfaces). |
| `ZETTA_E2E_REQUIRE_AUTH` | **Always `1`** — auth is mandatory ([zetta#799](https://github.com/oramasearch/zetta/issues/799)), so the suite always runs its **authz-enforcement assertions** (an unauthenticated request must be refused; the full RLS/masking/scoped-grants/token-lifecycle matrix lands as the authz system does). |
| `ZETTA_E2E_TOKEN` | **Always** a `SecretKeyRef` to `<targetRef>-zetta-admin` key `token` (the **seeded admin** principal, always provisioned, so authenticated calls pass). |

If the target `OramaZettaCluster` doesn't exist yet, the test parks in `Pending`
(`waiting for target OramaZettaCluster <ref>`) and requeues — it never creates a Job without a target.
A change to Zetta coverage touches **three** repos: zetta (the script) → zetta-bridge (the image) →
this operator (the CRD). Sample: [`../config/samples/zetta-e2e-test.yaml`](../config/samples/zetta-e2e-test.yaml).

**Authz coverage (always on):** auth is mandatory ([zetta#799](https://github.com/oramasearch/zetta/issues/799),
[issue #88](https://github.com/oramasearch/orama-operator/issues/88)), so this test **always** runs
the suite's authz-enforcement assertions — it sets `ZETTA_E2E_REQUIRE_AUTH=1` and hands the suite the
target's **seeded admin token** (`<targetRef>-zetta-admin`, always provisioned). **Multi-node** authz
coverage needs no extra field: the reconciler already builds `ZETTA_E2E_PEER_URLS` from the target's
replicas, so pointing this at a `replicas>=2` cluster exercises the authenticated path across every node.
End-to-end sample (a 3-replica target + its test): [`../config/samples/zetta-require-auth.yaml`](../config/samples/zetta-require-auth.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the `OramaZettaCluster` to test (same namespace). Required. |
| `spec.image` | **Runner image** (default `oramasearch/zetta-e2e:v1-sandbox`) that bakes the suite as its `ENTRYPOINT` — the operator sets only `Env`, never a `Command`. |
| `spec.levels` | Which test levels to run (`ZETTA_E2E_LEVELS`, default `2,3`). |
| `spec.pullSecretName` | Pull secret authenticating image pulls (also avoids Docker Hub anon rate limits). |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed` (from the Job). `kubectl get ozce2e`; `kubectl logs job/<name>-e2e`. |

## `CloudflareDNSRecord` (separate CRD)

Creates new DNS records and reconciles only records it owns. Foreign A, AAAA or
CNAME records at the desired name are conflicts, including records with identical
settings, and so is a foreign TXT record when `spec.type` is `TXT`. The controller lists
every result page before any mutation; other TXT/MX records are ignored. Conflicts and ambiguous ownership set `ready: false` and retry in five
minutes without changing ownership status or external records.

| Field | Meaning |
|---|---|
| `spec.secretRef` | Same-namespace Secret with `token` and `zoneID`. Missing either reports waiting and retries in five minutes without an API call. |
| `spec.name` | Record FQDN. |
| `spec.type` | Explicit record type wins. Empty infers A for an IP (including the existing IPv6 inference), CNAME otherwise. |
| `spec.content` | Record target. |
| `spec.proxied` | Proxy setting, default `true`; explicit `false` is preserved. TTL is automatic (`1`). |
| `spec.deletionPolicy` | `Delete` (default) deletes the owned record with the resource. `Retain` makes no Cloudflare call on deletion or rename; the record stays. |
| `spec.sharedOwner` | Optional owner of a record several resources keep, e.g. `clerk/<instance ID>`. The record is created with comment `orama-operator:<sharedOwner>`, and every resource with the same value recognises it as its own. Requires `deletionPolicy: Retain` (the API server refuses it otherwise). |
| `status.recordID` | Created or recovered record ID, or a pre-upgrade legacy ID. |
| `status.ownerComment` | Optional exact comment `orama-operator:<namespace>/<name>` (or `orama-operator:<sharedOwner>`), saved on creation or marker recovery. Empty with a populated ID means legacy compatibility. |
| `status.ready` | True after a successful create or confirmed owned reconciliation. |
| `status.message` | `reconciled` on success; otherwise the waiting/API diagnostic, conflicting record's name, type, content and ID, or ownership ambiguity. |

Creation rejects comments over 100 characters (namespace/name over 85) with a
five-minute retry and no mutation; it never truncates or hashes them. This limit
never blocks legacy reads or deletion. Exact markers recover a record after a
failed status update, without a second create. Owned records matching type, content,
proxying and TTL receive only GETs; changed settings receive one PUT preserving the
current comment. Legacy IDs never gain a comment. A previously marked record whose
comment changes remains marked in status and cannot become legacy.

A changed name or type creates the new record, then deletes the previous ID through the
cleanup check below; a previous record that lost its comment is left, reported with
`RecordNotOwned` and no longer tracked. With `Retain` the previous record is left.

With `Retain`, the finalizer is released without a Cloudflare call. Otherwise the cleanup
finalizer fetches marked records by ID and deletes only if their stored
comment still matches. Otherwise it releases the finalizer, leaves the record and emits
Warning `RecordNotOwned`. Legacy IDs retain deletion by ID. A 404 on GET or DELETE
means cleanup succeeded; other failures retain the finalizer and return the error.
A changed `spec.name` (or type) is a new record at the new name: once it is created, the
previous `status.recordID` goes through the same owned delete (marked records only while
their comment still matches, legacy IDs by ID), so a rename never leaves the old name live
([#529](https://github.com/oramasearch/orama-operator/issues/529)) and never deletes a record
the operator did not create. A failed delete keeps the old ID in status and retries.
No ID means no Cloudflare calls. Missing credentials with an ID retain the finalizer
and retry in five minutes. Install this CRD update through the approved rollout
mechanism before the controller, so Kubernetes preserves `ownerComment`.

The `app.orama.com` cutover repoint is a reviewed Cloudflare change in the bridge
[cutover runbook](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/cutover-runbook.md),
listed in [production changes](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/production-changes.md).
The operator cannot adopt that existing record.

## `CloudflareR2Bucket` (separate CRD, namespaced) — an R2 bucket, its deletion policy and scoped credentials

| Field | Meaning |
|---|---|
| `spec.secretRef` | Same-namespace Secret with the account `token` and `accountID`. Missing reports nothing and waits. |
| `spec.name` | The R2 bucket name. An existing bucket is adopted. |
| `spec.deletionPolicy` | **Required, no default.** `Delete` deletes the bucket from Cloudflare with the resource; `Retain` keeps the bucket and every object in it. Backup buckets use `Retain` ([`backups.md`](backups.md#the-bucket)). |
| `spec.credentials.secretName` | When set, the operator mints an account-owned API token granting *Workers R2 Storage Bucket Item Write* on this bucket alone and writes its S3 credentials here (keys `accessKeyId`, `secretAccessKey`, `endpoint`, `bucket`, `region`), owned by this resource. A lost Secret is replaced by a new token; the old one is revoked. Removing `spec.credentials` revokes the token and deletes the Secret. |
| `spec.credentials.allowedNamespaces` | Other namespaces whose `Backup`s may use the bucket. |
| `spec.cors` | Optional list of `{allowedOrigins, allowedMethods, allowedHeaders?, maxAgeSeconds?}`. Replaces the whole bucket CORS configuration when set; omission leaves it alone. Methods: GET, PUT, POST, DELETE, HEAD; max age 0–86400 seconds. Compared by effective settings before writing. |
| `spec.expireAfterDays` | Optional integer, minimum 1. Owns lifecycle rule `orama-expire`, deleting objects after this many days; other lifecycle rules are preserved. Omission leaves lifecycle alone. |
| `status.created`, `status.ready`, `status.message` | Bucket exists; everything reconciled; otherwise Cloudflare's refusal (written only on change). |
| `status.credentialsTokenID`, `status.credentialsSecret` | The bucket token's id (also the S3 access key id, not a secret) and the Secret holding the credentials. |
| `spec.customDomain` | Optional `{domain, zoneID, minTLS?}`. Serves the bucket's objects **publicly** on `domain` through Cloudflare (an R2 custom domain; Cloudflare creates the DNS record and certificate). `zoneID` is the zone the host is in (not a secret); `minTLS` is `1.2` (default) or `1.3`. Kept enabled at that TLS version; removing it, or changing the domain, detaches the old host. Set it only on a bucket that holds public content, such as a [status page](infrastructure/status-pages.md). The account token must be allowed to attach custom domains in that zone. |
| `status.customDomain`, `status.customDomainStatus` | The attached host and Cloudflare's verdict, `ownership=<state> ssl=<state>`; the resource is looked at again every minute until both are `active`. |

A Cloudflare-enabled stack with the dashboard enabled owns `<cr>-uploads`, with credentials in
`<cr>-uploads-r2`, PUT CORS for the dashboard host and extra hosts, seven-day expiry, and deletion
policy `Delete` because these objects are import staging. The dashboard, crawler and import consumer
share the one bucket-scoped token. The dashboard's references are optional to preserve startup before
provisioning succeeds; when the Secret appears its credential hash rolls the dashboard onto R2. See [cloud-v1-import-consumer.md](cloud-v1-import-consumer.md).

On deletion the finalizer revokes the bucket token whatever the policy, and deletes the bucket only
with `Delete`.

## `StatusPage` (separate CRD, namespaced) — external checks, alerts and a public status page

Checks public endpoints from the operator that holds the resource, alerts when a component changes
state, and writes a static page to a bucket. Runbook and the lake.live page:
[`infrastructure/status-pages.md`](infrastructure/status-pages.md).

| Field | Meaning |
|---|---|
| `spec.title` | The page heading and the alerts' prefix. |
| `spec.intervalSeconds` | How often every check runs. Default 60, minimum 30. |
| `spec.timeoutSeconds` | Per-check timeout, 1–25. Default 10. |
| `spec.failureThreshold` | Consecutive failed runs that make a check failing, 1–10. Default 3. One pass clears it. |
| `spec.components[]` | `{name, title, group?, checks[]}`, in page order; `group` is a heading (`Europe`). A component is `Operational` when every check that ran passes, `Outage` when every one is failing, `Degraded` in between, `Unknown` when none could run. |
| `spec.components[].checks[]` | `{name, url, method? (GET\|POST), headers?, body?, secretRef?, bearerTokenKey?, expect}`. `$(KEY)` in `url`, header values and `body` is replaced from the same-namespace Secret `secretRef`; `bearerTokenKey` sends that key as `Authorization: Bearer`. A missing Secret or key leaves the check **not checked** (`uncheckedChecks`), never failing. |
| `…checks[].expect` | Every field set must hold: `status` (default 200), `bodyContains[]`, `json[]` of `{path, equals?}` (dotted path, numbers index arrays; `equals` compares a string as written and anything else as its JSON; unset requires a non-null value), `minCertificateDays` (fails when the server's certificate expires sooner). |
| `spec.publish` | Optional `{bucketSecretRef, publicURL?}`. `bucketSecretRef` is a Secret with S3 credentials (`accessKeyId`, `secretAccessKey`, `endpoint`, `bucket`, `region`), as a `CloudflareR2Bucket`'s `spec.credentials` writes. `index.html` is written when it changes, `status.json` on every run. |
| `spec.alerts.webhook.secretRef` | A Secret with key `url`. Each change of state is POSTed as JSON with `text` and `content` (Slack, Discord, Google Chat and Mattermost read it as is), `component`, `from`, `to`, `at`. |
| `spec.alerts.email.secretRef` | A Secret with `host`, `port` (465 implicit TLS; anything else requires STARTTLS), `username`, `password`, `from`, `to` (comma-separated). |
| `status.phase`, `status.operational` | The worst component state; `n/total` components operational. |
| `status.components[]` | `{name, state, since, failingChecks, uncheckedChecks, message, history}`. `message` says why a check fails or did not run, with the check's URL as written (Secret values never appear). `history` is the worst state of each of the last 90 UTC days. |
| `status.conditions` | `ChecksConfigured` (False lists the checks whose Secret or key is missing), `Published` (False carries the bucket's refusal), `AlertsDelivered` (False: `NoChannel`, or `DeliveryFailed` with the reason; undelivered alerts are retried every run). |

The status subresource is written only when a component changes state, a new day starts in its
history, or a condition changes: a steady page writes nothing. Failure counts are kept in memory and
seeded from `status.components[].failingChecks` after a restart, so a failing check stays failing and
its recovery is a real one. `status.json` is public and carries only titles, states, `since`, the
history and its `generated` time, never a URL or a failure message. The page marks itself
**Unknown** when `generated` is older than three intervals plus a minute, so a stopped prober never
leaves a stale Operational page.

## `BackupSchedule` (separate CRD, namespaced) — backups of one datastore on a schedule

[`backups.md`](backups.md). Lives in the datastore's namespace. Short name `bsched`.

| Field | Meaning |
|---|---|
| `spec.datastore.kind` | `Postgres`, `ClickHouse`, `Zetta`, `Kafka`, `Cassandra`, `TiDB`, `Valkey`, `SQLite`, `DuckDB` or `Polars`. Selects the mechanism; a kind with none yet makes the schedule `Ready=False`, `MechanismNotAvailable`, and it creates nothing. Today only `ClickHouse` has one. |
| `spec.datastore.name` | The datastore's name in the namespace; part of the default path and the `orama.cloud/backup-datastore` label. |
| `spec.datastore.connectionSecretRef.name` | Secret with `host`, `username`, `password`, optional `port` and `database`, read by the backup Job as environment variables. |
| `spec.bucket` | `kind` (`CloudflareR2Bucket`, the default), `name`, optional `namespace` (another namespace must list this one in the bucket's `spec.credentials.allowedNamespaces`). |
| `spec.path` | Prefix in the bucket; default `<namespace>/<kind>/<name>` in lower case. Each Backup writes under `<path>/<backup>/`. |
| `spec.schedule` | Five-field cron, UTC. Default `0 3 * * *`. |
| `spec.keep` | Succeeded Backups kept, minimum 1, default 7. The newest succeeded one is never deleted; failed ones are kept to the same number; running ones are never deleted. |
| `spec.suspend` | No new Backups; none deleted. |
| `status.lastScheduleTime`, `status.nextScheduleTime` | The newest Backup's tick (taken from the Backups, so a restart neither skips nor repeats); the next tick. |
| `status.lastSuccessfulBackup`, `status.lastSuccessTime` | The newest succeeded Backup and when it completed. |
| `status.conditions` | `Ready` (`Scheduled`, `Suspended`, `InvalidSchedule`, `MechanismNotAvailable`); `LastBackupSucceeded` (the newest finished Backup's outcome, its message in the words of what refused); `OnSchedule` (`False`, `BackupLate`, once a whole tick has passed after the one that should have followed the last success). |

Missed ticks become one Backup for the latest tick. No new Backup starts while one is running.
Deleting the schedule keeps its Backups.

## `Backup` (separate CRD, namespaced) — one backup

[`backups.md`](backups.md#how-one-backup-runs). Short name `bkp`. **The spec is immutable.**

| Field | Meaning |
|---|---|
| `spec.datastore`, `spec.bucket`, `spec.path` | As on `BackupSchedule`. |
| `status.phase` | `Pending` (waiting, reason in `Complete`), `Running`, `Succeeded`, `Failed`. |
| `status.location` | `s3://<bucket>/<path>/<name>/`. |
| `status.jobName`, `status.startTime`, `status.completionTime` | The Job (`<name>-backup`, shortened with a hash past 56 characters) and when it ran. |
| `status.sizeBytes`, `status.message` | What the mechanism reported. |
| `status.conditions[Complete]` | `True`/`BackupSucceeded`, or `False` with `BucketNotFound`, `BucketHasNoCredentials`, `BucketNotAllowed`, `BucketNotReady`, `OperatorImageUnknown`, `Running`, `MechanismNotAvailable`, `BackupFailed`, `JobFailed`, `NoReport` or `JobLost`. |

The Job runs the operator's own image as `/manager backup --datastore <kind> --backup <name>
--prefix <path>/<name>`, restricted, with no service account token, next to an owned
`NetworkPolicy` and an owned copy of the bucket's scoped credentials (`<job>-bucket`), both removed
when the Backup finishes. Deleting a Backup deletes its objects from the bucket; if the bucket
resource is gone the objects stay.

## `EdgeProvider` (separate CRD, namespaced) — the vendor, account and credentials for edge resources

Principle 10's provider resource for the edge kinds (operator#578). A Lake with a `dnsSecretRef` owns
one, `lake-<name>-edge`.

| Field | Meaning |
|---|---|
| `spec.type` | `cloudflare` (the only type). |
| `spec.cloudflare.accountId` | Optional Cloudflare account ID; zone policies do not need it. |
| `spec.cloudflare.apiTokenSecretRef` | `{name, key}` of the token in this namespace. For an `EdgeZonePolicy`: Zone > Zone Settings > Edit, Zone > DNS > Read, Zone > Zone > Read and Zone > SSL and Certificates > Edit on the zone. For an `EdgeHostPolicy`, on each host's zone: Zone > Zone > Read, **Zone > Config Rules > Edit** (API permission group `Config Settings Write`, for `originTLS`) and **Zone > Single Redirect > Edit** (`Dynamic URL Redirects Write`, for `redirectToHTTPS`). Never written or logged. |
| `status.ready`, `status.message` | Whether the Secret and key exist. Whether the token is allowed what a kind needs is reported by that kind. |

## `EdgeZonePolicy` (separate CRD, namespaced) — secure a DNS zone's edge

The provider-neutral request "secure this zone's edge" (operator#578). It has no settings and no off
switch: it always means origin encryption Full, then Full (strict), authenticated origin pulls, an
edge certificate for every proxied host more than one level below the apex (Cloudflare: an Advanced
Certificate, lake#798), and Always Use HTTPS, each done only when its check passes, and never
undone. Short name
`edgezp`; `kubectl get edgezp -A` shows `Origin`, `AOP`, `HTTPS`, `Phase`. A Lake with a
`dnsSecretRef` owns one, `lake-<name>-edge`. The full contract, the checks and the order are in
[`lake/origin-tls.md`](lake/origin-tls.md).

| Field | Meaning |
|---|---|
| `spec.providerRef.name` | An `EdgeProvider` in this namespace. |
| `spec.zoneID` | The provider's zone identifier (Cloudflare: the zone ID). |
| `status.phase` | `Ready` (every setting on), `Progressing` (a setting waits on a check), `Failed` (the zone cannot be read or changed; the message names the missing permission). |
| `status.originEncryption` | The zone's observed mode: `off`, `flexible`, `full` or `strict`. Lake Ingresses redirect HTTP only at `full` or `strict`. |
| `status.authenticatedOriginPulls`, `status.originClientCAPEM` | Whether the edge presents its client certificate to origins, and the CA that signs it. Lake Ingresses require the certificate once this is on. |
| `status.alwaysUseHTTPS` | Whether the edge redirects plain HTTP. |
| `status.zone` | The zone's name (`lake.live`), read with Zone > Zone > Read. The owning Lake's `Issuer` solves DNS-01 for this zone and every name under it, and HTTP-01 for any other name; empty keeps DNS-01 for every name ([operator#632](https://github.com/oramasearch/orama-operator/issues/632)). A refused read keeps the last name. |
| `status.proxiedHosts`, `status.hosts[]` | How many proxied hosts the zone has, and up to 50 that fail a check: `{name, origin, edge}` with the reason for each leg. |
| `status.conditions` | `Credentials`, `OriginCertificates`, `EdgeCertificates`, `OriginTLSStrict`, `AuthenticatedOriginPulls`, `EdgeCertificateOrders` (the edge certificates ordered, pending, or why an order is refused), `AlwaysUseHTTPS`, `ZoneName` (False names the missing Zone > Zone > Read; it does not change the phase). |

Several policies may name one zone (one per Lake); they ask for the same settings. Deleting a policy
changes nothing in the zone.

## `CloudflareZonePolicy` (separate CRD, namespaced) — an `EdgeZonePolicy` applied to Cloudflare

Created and owned by an `EdgeZonePolicy` whose provider is `cloudflare`, with the same name; a person
never writes one. The only kind that changes a Cloudflare zone's settings (`ssl`,
`ssl_automatic_mode`, `tls_client_auth`, `always_use_https`). Short name `cfzp`.

| Field | Meaning |
|---|---|
| `spec.zoneID` | The Cloudflare zone. |
| `spec.apiTokenSecretRef` | The provider's token Secret and key. |
| `status` | The same status as `EdgeZonePolicy`, which copies it. |

## `EdgeHostPolicy` (separate CRD, namespaced) — per-host Full (strict) and HTTPS redirect

The provider-neutral request "these hosts get authenticated origin TLS and/or an HTTPS redirect"
([operator#527](https://github.com/oramasearch/orama-operator/issues/527)). It changes only the hosts
it names, as declared rules; a zone-wide setting is never touched, because the same zones carry hosts
the operator does not run (Prod 1's). Short name `edgehp`; `kubectl get edgehp -A` shows `OriginTLS`,
`Redirect`, `Ready`.

**Order with certificates:** Full (strict) on a host whose origin certificate is not valid yet takes
that host down. The policy does not check certificates: whatever creates an `EdgeHostPolicy` adds a
host only once that host's origin certificate is ready.

| Field | Meaning |
|---|---|
| `spec.providerRef.name` | An `EdgeProvider` in this namespace (its token needs the permissions listed there). |
| `spec.hosts[]` | 1 to 100 exact host names, lowercase, no wildcards. Each must be in a zone the token can read. |
| `spec.originTLS` | `strict`: origin encryption Full (strict) for these hosts. Empty leaves the zone's mode in force. |
| `spec.redirectToHTTPS` | `true`: plain-HTTP requests for these hosts are answered with a 301 to HTTPS (path and query kept). |
| `status.ready`, `status.message` | Copied from the `CloudflareHostPolicy`: ready when every rule is in place in every zone and no other rule overrides it. |

Deleting the policy deletes the `CloudflareHostPolicy`, whose finalizer removes the rules.

## `CloudflareHostPolicy` (separate CRD, namespaced) — an `EdgeHostPolicy` applied as Cloudflare rules

Created and owned by an `EdgeHostPolicy` whose provider is `cloudflare`, with the same name; a person
never writes one. The only kind that calls the Cloudflare Rulesets API. Short name `cfhp`.

| Field | Meaning |
|---|---|
| `spec.accountId` | The provider's account; narrows the zone lookup when set. |
| `spec.apiTokenSecretRef` | The provider's token Secret and key. |
| `spec.hosts`, `spec.originTLS`, `spec.redirectToHTTPS` | The parent's. |
| `spec.ruleRefPrefix` | `orama-operator-<namespace>-<name>` (dots as dashes; a longer one is cut to 64 characters ending in 12 hex characters of its SHA-256). Our rules have the refs `<prefix>-tls` and `<prefix>-https`; no other rule is ever changed or deleted. |
| `status.zones[]` | `{zoneID, zoneName, configRuleID, redirectRuleID, ready, message}` per zone holding our rules. |
| `status.ready`, `status.message`, `status.observedGeneration` | Every zone ready, else what is wrong (a missing zone, a refused call with the permission it needs, or another rule that overrides ours). |

What it does in each zone (hosts grouped by zone, the zone being the longest suffix of the host that is
a zone the token can read):

| Phase | Ref | Rule | Position |
|---|---|---|---|
| `http_config_settings` | `<prefix>-tls` | `http.host in {<hosts, sorted>}`, `set_config`, `{"ssl": "strict"}` | Appended last: every matching set_config rule applies and the last change wins |
| `http_request_dynamic_redirect` | `<prefix>-https` | `(http.request.scheme eq "http" and http.host in {…})`, `redirect`, 301 to `concat("https://", http.host, http.request.uri.path)`, query kept | Inserted first: a redirect stops evaluation, the first match wins |

- A phase with no entrypoint gets one holding only our rule (`PUT .../phases/<phase>/entrypoint`). An
  existing entrypoint is never `PUT`, which would replace every rule in it: our rule is added with
  `POST .../rulesets/<id>/rules`, changed with `PATCH .../rules/<rule id>` only when it differs, and
  left alone when equal (a steady policy only reads).
- Another enabled rule that may match one of our hosts and would override ours (a later set_config
  rule setting a different `ssl`, an earlier redirect with other parameters) makes the zone not ready,
  naming that rule's ID and description. It is never moved or changed.
- A flag turned off, or a zone with no host left, deletes our rule there by ID, only while its ref is
  still ours. Deletion (finalizer `orama.cloud/cloudflare-hostpolicy-cleanup`) does the same in every
  zone; a `404` counts as done, any other error keeps the finalizer.
- It rechecks every ten minutes, so a rule edited by hand is put back.

## `PaymentProvider` (separate CRD, namespaced) — the vendor, account and key for payment resources

Principle 10's provider resource for the payment kinds (operator#637, the Stripe epic
orama-bridge#199). It checks its key against the vendor and changes nothing there. Short name
`payprov`; `kubectl get payprov -A` shows `Type`, `Account`, `Live`, `Credentials`, `Reason`.
The check, its reasons and the Secret it reads are in
[`infrastructure/payments.md`](infrastructure/payments.md).

| Field | Meaning |
|---|---|
| `spec.type` | `stripe` (the only type). |
| `spec.stripe.accountId` | The Stripe account or sandbox, `acct_...`. A key of another account is refused. |
| `spec.stripe.livemode` | Whether this is live mode. The key's prefix must agree: `sk_live_`/`rk_live_` for `true`, `sk_test_`/`rk_test_` for `false`. Required, so a provider always says which mode it is. |
| `spec.stripe.apiKeySecretRef` | `{name, key}` of the operator's restricted key in this namespace. Never written to status or logged. |
| `status.conditions[Credentials]` | `True` with reason `Verified` (the key's mode and account match) or `ModeVerified` (a restricted key that may not read the account; its mode matches, the account is not checked). `False` with `SecretMissing`, `ModeMismatch`, `AccountMismatch` or `StripeAPIError`. |

The key is checked every hour, and a minute after a Stripe error or while the Secret is missing.
Status is written only when the condition changes.

## `PaymentWebhookEndpoint` (separate CRD, namespaced) — deliver an app's payment events to a URL

Principle 10's abstract kind for webhook endpoints (operator#637). For a `stripe` provider it keeps
an owned `StripeWebhookEndpoint` of the same name, with the same spec and the provider's key, and
copies back its `Ready` condition and endpoint ID; it calls no vendor itself. One per app per host:
each endpoint has its own signing secret. Short name `paywh`; `kubectl get paywh -A` shows `App`,
`URL`, `Endpoint`, `Ready`, `Reason`. The lifecycle is in
[`infrastructure/payments.md`](infrastructure/payments.md#webhook-endpoints).

| Field | Meaning |
|---|---|
| `spec.providerRef.name` | A `PaymentProvider` in this namespace. |
| `spec.url` | Where the events go. `https://` only. |
| `spec.events` | The event types the app handles (`invoice.paid`, ...), at least one. Sent sorted and deduplicated. |
| `spec.apiVersion` | The payload version the app parses (Lake: `2024-06-20`). Stripe cannot change it on an endpoint, so a new value replaces the endpoint and its signing secret. |
| `spec.app` | `cloud_v1` or `lake`: the endpoint's `metadata[orama_app]`. |
| `spec.signingSecretName` | The Secret, in this namespace, that receives the signing secret under key `signingSecret`. The operator owns it. |
| `status.endpointId` | The vendor's endpoint ID (`we_...`). |
| `status.conditions[Ready]` | Copied from the concrete kind: `True` with reason `Configured`; `False` with `ProviderNotReady`, `URLTaken`, `SigningSecretTaken` or `StripeAPIError`. `False`/`Progressing` until the concrete kind has reported on its current spec. |

## `StripeWebhookEndpoint` (separate CRD, namespaced) — one Stripe webhook endpoint and its signing secret

Created and owned by a `PaymentWebhookEndpoint` of a `stripe` provider; a person never writes one.
The only kind that calls Stripe's webhook API. Short name `stripewh`, with the same columns as
`paywh`. Finalizer `orama.cloud/payment-webhook-cleanup` tries to delete the Stripe endpoint,
retrying failures for five minutes from `metadata.deletionTimestamp`. A Stripe 401/403 or missing
provider/key Secret releases it immediately. Cleanup failures emit `EndpointLeftBehind` with the
endpoint ID for deletion in the Stripe Dashboard. With an empty ID, a working key still cleans up
the endpoint matching our owner tag and URL; cleanup failures are logged and release it immediately
without an event, as do unavailable credentials. Kubernetes read errors still retry.

| Field | Meaning |
|---|---|
| `spec.*` | The `PaymentWebhookEndpoint`'s spec, unchanged. |
| `spec.apiKeySecretRef` | The provider's key Secret and key, copied from `PaymentProvider.spec.stripe.apiKeySecretRef`. |
| `status.endpointId` | The Stripe endpoint (`we_...`). |
| `status.writes` | How many creates and updates Stripe has answered for this object, a 5xx included: the attempt suffix of the next idempotency key. |
| `status.conditions[Ready]` | `True`/`Configured`; `False` with `ProviderNotReady` (the provider's `Credentials` are not `True` for its current spec, or its key is missing), `URLTaken` (another endpoint delivers to the URL; its ID is in the message), `SigningSecretTaken` (another controller owns the signing Secret) or `StripeAPIError`. |

Stripe is called every hour, and a minute after a failure; a changed spec or key, or a lost signing
Secret, is acted on at once. Status is written only when it changes.

## `PaymentCatalog` (separate CRD, namespaced) — an app's products and prices in a payment account

Principle 10's abstract kind for catalogs (operator#638). For a `stripe` provider it keeps an owned
`StripeCatalog` of the same name, with the same spec and the provider's key, and copies back its
`Ready` condition and prices; it calls no vendor itself. **Deleting it changes nothing in the
account**: there is no finalizer. Short name `paycat`; `kubectl get paycat -A` shows `App`,
`Version`, `Ready`, `Reason`. The lifecycle is in
[`infrastructure/payments.md`](infrastructure/payments.md#catalogs).

| Field | Meaning |
|---|---|
| `spec.providerRef.name` | A `PaymentProvider` in this namespace. |
| `spec.app` | `cloud_v1` or `lake`: `metadata[orama_app]` on every product and price. |
| `spec.version` | The definition's version, at least 1, bumped on every change. Stamped on every product as `metadata[orama_catalog_version]`; a catalog never writes a product that carries a higher one (`Superseded`). |
| `spec.products[].id` | The product's fixed ID (`lake_usage`): `^[a-z0-9_]{3,64}$`, once per catalog. A product this catalog created that the list no longer has is archived. |
| `spec.products[].name` | The product's name. |
| `spec.products[].description` | Optional; empty means none. |
| `spec.products[].metadata` | Added to the product's metadata. A key starting with `orama_` is refused (`InvalidSpec`). |
| `spec.products[].prices[].lookupKey` | Names the price across accounts and over time (`cloud_v1.plan.<uuid>.monthly`): letters, digits, `_ . : -`, at most 128, once in the whole catalog. |
| `spec.products[].prices[].currency` | Three lowercase letters (`usd`). |
| `spec.products[].prices[].unitAmount` | In the currency's minor unit (cents), 0 or more. |
| `spec.products[].prices[].interval` | `month` or `year` for a recurring price; empty for one-time. |
| `spec.products[].prices[].metadata` | Added to the price's metadata, as for products. |
| `status.prices` | Lookup key → the price ID now holding it, as of the last pass that brought the whole catalog to its spec. |
| `status.conditions[Ready]` | Copied from the concrete kind: `True` with reason `Configured` or `Superseded`; `False` with `ProviderNotReady`, `InvalidSpec`, `ProductTaken`, `PriceTaken` or `StripeAPIError`. `False`/`Progressing` until the concrete kind has reported on its current spec. |

## `StripeCatalog` (separate CRD, namespaced) — one catalog's Stripe products and prices

Created and owned by a `PaymentCatalog` of a `stripe` provider; a person never writes one. The only
kind that writes Stripe products and prices. It archives and never deletes, and has no finalizer.
Short name `stripecat`, with the same columns as `paycat`.

| Field | Meaning |
|---|---|
| `spec.*` | The `PaymentCatalog`'s spec, unchanged. |
| `spec.apiKeySecretRef` | The provider's key Secret and key, copied from `PaymentProvider.spec.stripe.apiKeySecretRef`. |
| `status.prices` | Lookup key → price ID (`price_...`). |
| `status.writes` | How many creates and updates Stripe has answered for this object, a 5xx included: the attempt suffix of the next idempotency key. |
| `status.conditions[Ready]` | `True`/`Configured`; `True`/`Superseded` (a product carries a newer catalog version, and nothing is written); `False` with `ProviderNotReady`, `InvalidSpec` (a lookup key under two products, or an `orama_` metadata key), `ProductTaken` (a product with that ID belongs to another app or was made by hand), `PriceTaken` (a price that is not this catalog's holds a lookup key; its ID is in the message) or `StripeAPIError`. |

Stripe is called every six hours, and a minute after a failure; a changed spec or key is acted on at
once. Status is written only when it changes.

## `CloudflareLoadBalancer` (separate CRD) — the global geo-LB

Owns a Cloudflare **global Load Balancer** ([operator#73](https://github.com/oramasearch/orama-operator/issues/73)):
a single hostname served across the cluster's wan-accessible nodes with origin pools, health checks,
geo/proximity steering, and failover. Same "external resource as an owned CRD" pattern as
`CloudflareDNSRecord` — the child controller (`cloudflareloadbalancer_controller.go`) makes the CF
Load Balancing API calls and holds a finalizer, so the main loop never mutates Cloudflare directly.
From one CR it reconciles an account-scoped **Monitor**, one or more account-scoped **Pools**, and the
zone-scoped **Load Balancer**; teardown deletes them in dependency order (LB → pools → monitor). All
credentials come from the referenced Secret (`token`/`zoneID`/`accountID`).

**Cloudflare Load Balancing is a PAID add-on** — enable it on the account first
(dashboard: Delivery & performance → Load Balancing). Until then the reconciler is a safe no-op that
reports `Ready=False (LoadBalancingNotEnabled)` rather than crash-looping. Populating pool origins from
the **live** wan-accessible node set is tracked separately
([operator#74](https://github.com/oramasearch/orama-operator/issues/74) /
[zetta#739](https://github.com/oramasearch/zetta/issues/739)).
Sample: [`../config/samples/cloudflareloadbalancer.yaml`](../config/samples/cloudflareloadbalancer.yaml).

| Field | Purpose |
|---|---|
| `spec.secretRef` | Secret with the Cloudflare credentials (keys `token`/`zoneID`/`accountID`) — normally the `cloudflare-api-token` Secret. |
| `spec.hostname` | The FQDN the load balancer answers on (e.g. `zetta.oramadev.net`). Cloudflare owns this DNS entry — do **not** also manage it with a `CloudflareDNSRecord`. |
| `spec.steeringPolicy` | `off` (failover order) `\| geo` (by region via `regionPools`) `\| dynamic` (by measured latency; sent as `dynamic_latency`) `\| proximity` (nearest origin) `\| random`. Default `geo`. |
| `spec.fallbackPool` | Pool name (`pools[].name`) used as the last resort when all default pools are unhealthy. Empty = the last declared pool. |
| `spec.regionPools` | Map of Cloudflare region code (e.g. `ENAM`, `WEUR`) → ordered list of pool names — for `geo` steering. |
| `spec.proxied` | Route the hostname through Cloudflare's proxy (orange-cloud). Default `true`. |
| `spec.sessionAffinity` | `none \| cookie \| ip_cookie \| header`. Default `none`. |
| `spec.monitor.*` | The health monitor: `type` (`http\|https\|tcp`, default `https`), `method` (default `GET`), `path` (default `/v1/server_info`), `port`, `expectedCodes` (default `200`), `expectedBody`, `interval` (60), `timeout` (5), `retries` (2), `followRedirects`. |
| `spec.pools[]` | Origin pools (≥1). Each: `name`, `origins[]`, `minimumOrigins` (1), `enabled` (true), `useMonitor` (true = attach the monitor). Become the LB's `default_pools` in declared order. |
| `spec.pools[].origins[]` | `{ name, address, enabled (true), weight ("1", a decimal 0..1) }`. `address` is a node's public endpoint; the monitor probes it. |
| `status.loadBalancerID` / `.monitorID` / `.pools[]` | The provisioned Cloudflare IDs + per-pool `id` and `health` (`healthy\|unhealthy\|unknown`). `kubectl get cflb`. |
| `status.conditions[Ready]` | `Reconciled` \| `WaitingForCredentials` \| `LoadBalancingNotEnabled` \| `ReconcileError`. Written only on a real transition (anti-flap). |
| `status.pass` / `.fail` / `.skip` | **Best-effort** assertion counts, parsed from the Job's pod logs (`Zetta e2e: pass=N fail=N skip=N`) once the Job is terminal, shown as `kubectl get ozce2e` print columns. `0` until the summary is readable (the operator reads pod logs via a clientset — RBAC `pods/log get`); a crash before the summary line leaves them at their last value. |

## `RabbitMQTopology` (separate CRD) — the broker's declared topology

Models the RabbitMQ broker's **vhosts / users / permissions / streams** as an owned, reconciled CRD
([operator#40](https://github.com/oramasearch/orama-operator/issues/40) /
[#41](https://github.com/oramasearch/orama-operator/issues/41)) — declarative topology
instead of OramaCore creating the replication stream as an app-boot side effect and every service
sharing the admin user. Same "external resource as an owned CRD" pattern as `CloudflareDNSRecord`: the
child controller (`rabbitmqtopology_controller.go`) drives the broker's **management HTTP API**
(`:15672`) and holds a finalizer, so the main loop never mutates the broker directly. This is the
operator's **own** topology CRD — the bitnami RabbitMQ Cluster / Messaging-Topology operators were
dropped (their images 404'd), so the broker runs as a plain StatefulSet and the operator owns its
topology (principle #3). See [`datastore-crds.md`](datastore-crds.md) for the full design.

`OramaCloudV1` creates + owns one `<cr>-broker` instance (in `reconcileBrokerTopology`) declaring the
`/` vhost, the least-privilege per-service users `oramacore-writer` / `oramacore-reader` (passwords
generated into `<cr>-broker-users`) and `harvest` (`<cr>-broker-harvest`), their scoped permissions,
the `oramacore-operations` replication stream and the combined readers' `oramacore-reader-local`
stream. The engine and harvest-master sign in as those users and are written only once this resource
reports `Ready` for its generation with every user applied
([#99](https://github.com/oramasearch/orama-operator/issues/99),
[#795](https://github.com/oramasearch/orama-operator/issues/795),
[#824](https://github.com/oramasearch/orama-operator/issues/824)). **Ordered readiness gating:** until the broker (and its `<cr>-broker-admin`
Secret) is reachable the reconciler is a safe no-op reporting `Ready=False (BrokerUnavailable /
WaitingForCredentials)`, not an error. Sample:
[`../config/samples/rabbitmqtopology.yaml`](../config/samples/rabbitmqtopology.yaml).

| Field | Purpose |
|---|---|
| `spec.brokerService` | Broker Service DNS name whose management API is reconciled (e.g. `<cr>-broker`); reached at `http://<brokerService>.<ns>.svc.cluster.local:<managementPort>`. |
| `spec.managementPort` | RabbitMQ management HTTP API port. Default `15672`. |
| `spec.adminSecretRef` | Secret with the broker admin creds (keys `username`+`password`). Missing = safe no-op (waits). |
| `spec.vhosts[]` | Virtual hosts to ensure (idempotent). `/` always exists but may be listed for intent. |
| `spec.users[]` | Per-service accounts (least-privilege, principle #4): `{ name, passwordSecretRef {name,key}, tags }`. Password is sourced from a Secret — never in spec; service accounts stay untagged. |
| `spec.permissions[]` | `{ user, vhost (`/`), configure, write, read }` regexes. Scope to the exact stream (`^oramacore-operations$`) — a producer needs `write`, a consumer `read`, a declarer `configure`. Default regex `^$` (none). |
| `spec.streams[]` | Stream queues (x-queue-type=stream): `{ name, vhost, maxAge, maxLengthBytes, maxSegmentSizeBytes }`. **Retention is declaration-time only** (RabbitMQ cannot change it later via policy) and must MATCH any native stream-protocol client that also `Create`s the stream, or the client's Create conflicts — leave retention unset unless you know the client's exact create args. |
| `status.vhosts` / `.users` / `.permissions` / `.streams[]` | Applied counts + per-stream `present`. `kubectl get rmqtopo`. |
| `status.conditions[Ready]` | `Reconciled` \| `WaitingForCredentials` \| `BrokerUnavailable` \| `ReconcileError`. Written only on a real transition (anti-flap, principle #7). |

## `PostgresSchema` (separate CRD) — a versioned, tracked SQL schema

Applies + tracks a **versioned Postgres schema**
([operator#40](https://github.com/oramasearch/orama-operator/issues/40)) — superseding the
bare idempotent migration Job ([#34](https://github.com/oramasearch/orama-operator/issues/34))
with a reconciled, auditable CRD that records the applied version in `.status`. The controller
(`postgresschema_controller.go`) **owns** an idempotent migration Job (wait-for-DB → guard →
apply-in-one-transaction) and reflects `phase` + `appliedVersion` (read back from the Job, so a bumped
desired version never misreports). It carries **no finalizer** — deleting the CR never drops the
schema/data, it only stops managing the migration.

`OramaCloudV1` creates + owns four instances, in order (`reconcileControlDBMigrations`):
`<cr>-controldb-schema` (dashboard migrations 0000 to 0007), `<cr>-controldb-schema-0008` (0008 with
the reference rows), `<cr>-controldb-reference-data` (the reference rows with the stack's Stripe
prices, `applyEveryVersion`) and `<cr>-controldb-migration-grants` (the `migration` role's grants,
`applyEveryVersion`, [#653](https://github.com/oramasearch/orama-operator/issues/653)); see
[`components.md`](components.md#the-control-dbs-schema). See
[`datastore-crds.md`](datastore-crds.md) for the full design. Sample:
[`../config/samples/postgresschema.yaml`](../config/samples/postgresschema.yaml).

| Field | Purpose |
|---|---|
| `spec.connectionSecretRef` | `{ name, key }` — Secret + key holding the Postgres connection URI (the CNPG `<cr>-controldb-app` Secret, key `uri`). Missing = safe no-op (waits). |
| `spec.schemaConfigMapRef` | `{ name, key }` — ConfigMap + key holding the schema SQL (applied all-or-nothing in one transaction). Required unless `spec.migrate` is set (a CEL rule refuses a PostgresSchema with neither). |
| `spec.migrate` | Runs the image's own migration command instead of a schema file ([#936](https://github.com/oramasearch/orama-operator/issues/936)): `args` (passed to the entrypoint of `spec.image`), `databaseEnv` (the variable set from `connectionSecretRef`), `failOnExitCodes` (fail the Job at once through a pod failure policy; other exits retry up to the backoff limit of 20), `runAsUser`/`runAsGroup`. No ConfigMap, no guard check by the operator: the command does its own. A Lake on an image with `lake manifest` sets `args: [migrate, --wait-seconds, "600"]`, `databaseEnv: LAKE_CONTROL_DB_URI`, `failOnExitCodes: [1, 2]`, uid/gid 10001, `spec.image` = the image by digest and `spec.version` = `image:<digest>`. |
| `spec.version` | Schema version recorded in `.status.appliedVersion` once applied. A change automatically replaces the prior Job with foreground deletion, waiting for its pods before starting the guarded apply. Lake hashes SQL plus guard; the dashboard hashes SQL. |
| `spec.guardTable` | A `public` table that must exist for the Job to skip the apply. Default `crawler_configs`. When the schema file records migrations (`INSERT INTO schema_migrations (version) VALUES ('NNNN_…')`, with or without a space before `(version)`, [#948](https://github.com/oramasearch/orama-operator/issues/948)), the Job also requires every one of them in the database's `schema_migrations`, and otherwise applies the whole idempotent file ([#271](https://github.com/oramasearch/orama-operator/issues/271)). With no `schema_migrations` table, or a file that records none, the guard alone decides. |
| `spec.applyEveryVersion` | Default `false`. When `true` the Job has no guard: each new `spec.version` applies the file once, for SQL that is safe to apply again (the control DB's reference-row upsert). `guardTable` is then ignored. |
| `spec.image` | psql-carrying image for the migration Job, or with `spec.migrate` the image that runs the command. Default `postgres:16-alpine@sha256:deb28ee471183cb0c303113dac9b2433c51eeeeb678def7af9a9ec5820367815`. `OramaCloudV1` sets `spec.images.overrides.postgres-client` here when present. |
| `spec.imagePullSecrets[]` | Pull Secrets for the migration Job's pod, so `spec.image` can be private. `OramaCloudV1` sets its `spec.images.pullSecretName`. Changing either field does not re-run an applied schema. |
| `status.phase` / `.appliedVersion` | `Pending \| Applying \| Applied \| Failed` + the version actually applied. `kubectl get pgschema`. |
| `status.conditions[Applied]` | `Applied` \| `Applying` \| `ReplacingMigrationJob` \| `WaitingForDatabase` \| `MigrationFailed`. Written only on a real transition (anti-flap). |

## `NetworkSandbox` (separate CRD, namespaced) — default-deny with explicit paths

Short name: `netsandbox`. One resource describes the network boundary of its owner's pods.
Required `spec.podSelector` identifies those pods; explicit `{}` is appropriate only in a fully
owned workspace namespace. The controller always renders `NetworkPolicy/default-deny`, selecting
only those pods for both ingress and egress with no rules. It also always renders the owner-scoped
`CiliumNetworkPolicy/deny-cloud-metadata`, denying all egress to `169.254.169.254/32`,
`168.63.129.16/32` and `fd00:ec2::254/128`, regardless of other allows. Cilium v2 API and dataplane
support are required. Each allowance selector is intersected with the owner selector, including
Cilium endpoint selectors. `spec.policies` lists exceptions expressible as Kubernetes NetworkPolicy;
each item has a unique `name`, a required human `comment` identifying
the component and reason, and the complete `networking.k8s.io/v1` policy `spec`. The comment is
copied to the rendered policy as `orama.cloud/network-policy-comment`.

`spec.ciliumPolicies` has the same name/comment contract and a Cilium v2 `spec`. It is only for a
path Kubernetes NetworkPolicy cannot express and is empty for ordinary pod, namespace, CIDR and
port rules. The mandatory metadata deny is generated even when this field is empty, and its name
is reserved: declarations cannot override it. `spec.policyLabels` is copied to all rendered policies
so a parent can preserve its existing ownership labels while the sandbox adopts live policies.
`spec.mesh.enrolled` enrols every selected pod in zetta-tunnel's mesh: each declared Connection
allowance (`connection-…`) then renders only TCP 15008, the mesh's mutual-TLS port, and the node
agent captures the pods ([`infrastructure/mesh.md`](infrastructure/mesh.md)). A Lake workspace's
comes from `spec.workspaces.overrides.<id>.mesh`.

The controller owns the rendered policies, removes an owned policy no longer declared, and refuses
to take over a policy controlled by another resource. It reports `status.observedGeneration`, the
ordinary and Cilium policy counts (including mandatory denies), and `conditions[Ready]`. Stable
reconciliation writes no status. Owner watches repair both kinds; owner references garbage-collect
them with the sandbox. Details and the AKS live observation are in
[`infrastructure/network-sandbox.md`](infrastructure/network-sandbox.md#mandatory-cloud-metadata-deny).

## `Connection` (separate CRD, namespaced) — one declared workload path

Short name: `conn`. Lake's guarded Postgres intake creates one resource in the owning workspace
namespace for each `network_connections` row. Users and tenant workloads do not write this kind.
The controller resolves Lake IDs to operator-owned pod labels, writes paired ingress and egress
allowances into that workspace's `NetworkSandbox`, and renders them synchronously. The sandbox,
not the `Connection`, owns the resulting `NetworkPolicy` objects. Details:
[`infrastructure/connections.md`](infrastructure/connections.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Cluster-scoped Lake that owns and materialized the resource. Immutable. |
| `spec.connectionId` | Immutable UUID of the Lake `network_connections` row. |
| `spec.customerId` / `.workspaceId` | Immutable Lake ownership UUIDs. The resource must be in the reconciled namespace of that workspace. |
| `spec.sourceKind` / `.sourceId` | Source identity: `app \| service \| agent \| tunnel` plus its Lake UUID. Apps resolve through their existing `LakeApp` workload identity; services and agents use operator-owned ID labels. |
| `spec.targetKind` / `.targetId` | Destination identity: `service \| app` plus its Lake UUID. Selection is restricted to the CR's namespace. |
| `spec.ports[]` | Destination TCP ports, each 1–65535. Empty resolves to the target's Kubernetes Service target ports, including named ports; it never means all pod ports. |
| `spec.credentialScope` | Optional, app to service only: `{access: read \| write, schema?}`. The operator keeps one role for this connection in the target's datastore (Postgres first) with exactly these grants and writes its credential as a workspace secret ([operator#469](https://github.com/oramasearch/orama-operator/issues/469)). |
| `spec.desired` | `present \| absent`. Absent removes both rendered sides, and drops any credential role, before status becomes `Deleted`. |
| `spec.generation` | Lake row generation, minimum 1. A new generation of a credential-scoped connection rotates its credential. |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted` and a stable explanation. |
| `status.observedGeneration` | Lake generation represented by the status and returned to Postgres. |
| `status.credentialSecret` | `lake-secret://<mount id>/connection-<id>`: the workspace secret holding the issued credential, returned to Postgres as `credential_secret`. Never the credential. |
| `status.credentialRole` / `.credentialGeneration` | The datastore role issued for this connection and the generation it was issued for. |
| `status.conditions[Ready]` | True only after the paired policies have been rendered. Stable reconciliation does not rewrite it. |

## `EgressRule` (separate CRD, namespaced) — one outside destination

Short name: `egr`. Lake's guarded Postgres intake creates one resource per `network_egress_rules`
row in the owning workspace namespace (`egress-<row id>`); the workspace reconciler keeps one more,
`internet-https`, for a workspace whose `internetEgress` override is on. Users and tenant workloads
do not write this kind. The controller adds the rule's allowances to the workspace `NetworkSandbox`
and, for a DNS name, keeps the workspace's zetta-tunnel egress gateway. Details:
[`infrastructure/egress-rules.md`](infrastructure/egress-rules.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Cluster-scoped Lake that owns the resource. Immutable. |
| `spec.ruleId` | Immutable UUID of the Lake row. Empty only for the operator's `internet-https` rule. |
| `spec.customerId` / `.workspaceId` | Lake ownership UUIDs. |
| `spec.sourceKind` / `.sourceId` | `app \| service \| agent` and its Lake UUID, set together. Unset: every workload in the workspace. |
| `spec.cidr` | An outside range, enforced by policy. Loopback, link-local and cluster addresses inside it are always excepted; a range wholly inside them is refused. |
| `spec.dnsName` | An exact lower-case host name, enforced by the egress gateway; the source may reach only the gateway. |
| `spec.publicInternet` | Every public address, as `internetEgress` has always allowed. Only the operator's own rule. |
| `spec.ports[]` | 1 to 64 distinct TCP ports. |
| `spec.desired` | `present \| absent`. Absent removes every allowance and re-renders the gateway snapshot before `Deleted`. |
| `spec.generation` | Lake row generation, minimum 1. |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted`; a Ready name rule's message names the gateway address. |
| `status.observedGeneration` | Lake generation represented by the status and returned to Postgres. |

Exactly one of `cidr`, `dnsName` and `publicInternet` is set (a CEL rule on the CRD).

## `Tunnel` (separate CRD, namespaced) — one person, one target, for a time

Short name: `tun`. The abstract kind (principle 10): Lake's guarded intake creates one per
`network_tunnel_grants` row in the workspace namespace, named `tunnel-<grant id>`. Users and tenant
workloads do not write it. The controller picks the concrete kind from `spec.providerRef`, keeps
exactly that child, mirrors its status, and on expiry or revocation deletes the child before
reporting `Expired` or `Revoked`. Details: [`infrastructure/tunnels.md`](infrastructure/tunnels.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` / `.grantId` | The Lake and the grant row id. Immutable. The grant id is the identity in the developer's certificate. |
| `spec.customerId` / `.workspaceId` | Immutable Lake ownership UUIDs. |
| `spec.userId` | The Lake user the grant is for. |
| `spec.targetKind` / `.targetId` / `.port` | `service \| app`, its Lake UUID and the target Service's port (1–65535). Nothing else is reachable. |
| `spec.expiresAt` / `.revokedAt` | The grant ends at `expiresAt`, or at once when `revokedAt` is set. |
| `spec.clientCAPEM` | Lake's per-customer CA (public), from `client_ca_pem` (Lake 0085). Required by `ZettaTunnel`. |
| `spec.subject` | The person's e-mail address, set only for a `cloudflare` provider (Cloudflare Access). |
| `spec.providerRef` | A `TunnelProvider` in the namespace. Empty: Orama's own `ZettaTunnel`. |
| `spec.generation` | Lake row generation, minimum 1. |
| `status.phase` / `.message` | `Pending \| Ready \| Expired \| Revoked \| Failed`. |
| `status.provider` | `ZettaTunnel` or `CloudflareTunnel`. |
| `status.endpoint` / `.relayCAPEM` | What the client connects to, and the relay's CA, only while `Ready`; returned to `network_tunnel_grant_status`. |
| `status.observedGeneration` / `.conditions[Ready]` | Lake generation acknowledged; comparison-gated. |

## `TunnelProvider` (separate CRD, namespaced) — the vendor, account and credentials behind tunnels

Short name: `tunprov`. A `TunnelProvider` named `default` in a workspace namespace is the provider
for that workspace's Lake-granted tunnels. CEL rules require `spec.cloudflare` for type `cloudflare`
and forbid it for `zetta`. The controller reports `conditions[Ready]` (the token Secret and key
exist); it never calls the vendor.

| Field | Meaning |
|---|---|
| `spec.type` | `zetta` (Orama's own zetta-tunnel) or `cloudflare` (the customer's own account only). |
| `spec.customerId` | The Lake customer it belongs to. A `Tunnel` of another customer is refused. |
| `spec.cloudflare.accountId` / `.zoneId` / `.domain` | The customer's account, zone and the domain under which hostnames `tunnel-<grant id>.<domain>` are made. |
| `spec.cloudflare.apiTokenSecretRef` | `{name, key}` of a Secret in the namespace: the customer's API token (Cloudflare Tunnel edit, Access apps and policies edit, DNS edit). Never written or logged by the operator. |

## `ZettaTunnel` (separate CRD, namespaced) — a tunnel carried by zetta-tunnel

Short name: `ztun`. Created and owned by a `Tunnel`. Runs the grant's zetta-tunnel endpoint in the
workspace (Secret, Service and Deployment `tunnel-<grant id>`), the generated
`Connection/connection-tunnel-<grant id>` from the endpoint to the target, and the workspace sandbox
allowance admitting only the relay. Every `ZettaTunnel` feeds the cluster's one relay in namespace
`zetta-tunnel`. A finalizer removes the allowance, the endpoint and the Connection before it goes.

| Field | Meaning |
|---|---|
| `spec.lakeRef`, `.grantId`, `.customerId`, `.workspaceId`, `.targetKind`, `.targetId`, `.port`, `.expiresAt` | The grant, copied from the `Tunnel`. |
| `spec.clientCAPEM` | The CA(s) the relay trusts for this grant's developer certificates. |
| `status.phase` / `.message` | `Pending \| Ready \| Expired \| Failed`. |
| `status.endpoint` / `.relayCAPEM` | The relay's public `host:443` and its server CA, while `Ready`. |
| `status.target` | The in-cluster `host:port` the endpoint is fixed to. |

## `CloudflareTunnel` (separate CRD, namespaced) — a tunnel carried by the customer's Cloudflare account

Short name: `cftun`. Created and owned by a `Tunnel` whose provider is of type `cloudflare`. Makes a
remotely managed tunnel, a CNAME and an Access application admitting only `spec.subject` in the
customer's account, and runs `cloudflared` in the workspace with the generated Connection to the
target. A finalizer deletes those Cloudflare objects with the customer's token.

| Field | Meaning |
|---|---|
| grant fields | As on `ZettaTunnel`. |
| `spec.providerRef` | The `cloudflare` `TunnelProvider`. |
| `spec.subject` | The one e-mail address Cloudflare Access admits. |
| `status.endpoint` | The hostname `tunnel-<grant id>.<domain>`. |
| `status.tunnelId` / `.dnsRecordId` / `.accessApplicationId` | The Cloudflare objects made, deleted exactly at teardown. |

## `PeerConnection` (separate CRD, namespaced) — one end of a peer

Short name: `peer`. Named services exchanged with another cluster or network through zetta-tunnel's
peer gateway (decision D6); no address is routed. Lake's guarded intake creates one per
`network_peers` row (`peer-<row id>`); Orama's own stacks write their own. Details:
[`infrastructure/peer-connections.md`](infrastructure/peer-connections.md).

| Field | Meaning |
|---|---|
| `spec.peerId` | The identity both ends share (the Lake row id). Immutable. |
| `spec.lakeRef` / `.customerId` / `.workspaceId` | Set for a Lake peer. |
| `spec.remote` | The other gateway's `host:port`. Empty: the address the `PeerLink` passed in the credentials Secret. |
| `spec.exports[]` | `{name?, targetKind: service \| app \| kubernetesService, targetId \| serviceName, port}`. A CEL rule requires `serviceName` for `kubernetesService`, else `targetId`. Empty `name`: the target Service's name. |
| `spec.imports[]` | `{name, port}`: the other end's export names (lowercase DNS-style, never an address). Each becomes Service `peer-<name>` here. |
| `spec.credentialsSecretName` | Default `<name>-credentials`; written only by a `PeerLink`. |
| `spec.pullSecretName` | The gateway's image pull secret; a Lake peer uses the Lake's. |
| `spec.desired` / `.generation` | `present \| absent` (default `present`) and the row generation (default 1). |
| `status.phase` / `.message` | `Pending \| Ready \| Failed \| Deleted`; the message lists where each import is reachable. |
| `status.address` / `.identity` | This gateway's public `host:7446` and certificate identity. |
| `status.imports[]` / `.exports[]` | Resolved Services and targets. |

## `PeerLink` (separate CRD, cluster-scoped) — credentials for a pair, through the managing cluster

Short name: `peerlink`. Lives on the managing cluster. Mints the pair's CA once, one certificate per
end (`<end>.<peerId>.peer.orama.internal`, client and server), and writes each end's credentials
Secret on its cluster with the other end's identity and address. Deleting it deletes both Secrets.

| Field | Meaning |
|---|---|
| `spec.peerId` | The pair's peer id. Immutable. |
| `spec.ends[2]` | `{name, clusterRef?, namespace, peerConnection}`; `clusterRef` empty is this cluster. The two names differ (CEL). |
| `status.phase` | `Ready` once both Secrets are current and both ends report an address. |
| `status.ends[]` | `{name, identity, written, address, message}`. |

## `ObservabilityProvider` (separate CRD, cluster-scoped) — an observability vendor's release

Short name `obsprovider`. Lives on the managing cluster. Type `pixie` is self-hosted Pixie Cloud and
Vizier built from our private copy `oramasearch/pixie` ([#455](https://github.com/oramasearch/orama-operator/issues/455)).
Hubs and agents point at it, so every cluster runs the same build. Details:
[`infrastructure/observability.md`](infrastructure/observability.md).

| Field | Meaning |
|---|---|
| `spec.type` | `pixie`. Immutable. |
| `spec.pixie.commit` | The `oramasearch/pixie` commit the images were built from. Must equal the commit the operator vendors (`internal/controller/assets/pixie/COMMIT`), or the provider is not Ready. |
| `spec.pixie.vizierVersion` / `.cliVersion` | The versions the hub's artifact tracker reports; pinned, so Pixie never offers another. |
| `spec.pixie.images` | Every image the operator renders, by key, as `repository@sha256:<digest>` (CEL). A missing, unknown or tag-only image makes the provider not Ready. The key set is in [`observability.md`](infrastructure/observability.md#images). |
| `status.ready` / `.message` | Whether the release can be installed, and why not. |
| `status.hubs` / `.agents` | How many use it. |

## `ObservabilityHub` (separate CRD, cluster-scoped) — one purpose's observability hub

Short name `obshub`. Lives on the managing cluster, which writes a `PixieCloud` onto the hub's
cluster. A hub runs only on a cluster tagged `purpose: prod-support` (serving production) or
`purpose: nonprod` (serving nonprod); on any other cluster it is `Refused`.

| Field | Meaning |
|---|---|
| `spec.providerRef` | The `ObservabilityProvider`. |
| `spec.clusterRef` | The hub's `KubernetesCluster`. Immutable. |
| `spec.domain` | The host name people reach the UI under (and `work.<domain>`). No DNS record or public exposure is created. |
| `spec.organization` / `.adminEmail` | The one organisation every agent's cluster is registered under, and its administrator. The password is generated into Secret `plc/pixie-admin` on the hub's cluster. |
| `spec.imagePullSecretRef` | Registry credentials on the managing cluster, copied as `plc/pixie-pull` under the production-credential rules. |
| `spec.storage` | `storageClassName`, `postgres` (default 20Gi), `search` (20Gi), `messaging` (10Gi). |
| `spec.alerting.receivers[]` | `{name, severities?, slack: {urlSecretRef, channel?} \| webhook: {urlSecretRef}}`: where every alert of the hub's cluster and of every agent's cluster goes. |
| `status.phase` | `Pending \| Progressing \| Ready \| Degraded \| Refused`, from the `PixieCloud`. |
| `status.purpose` / `.concreteRef` / `.ready` / `.components[]` | The hub cluster's purpose, the concrete resource, `n/total` and one entry per component. |
| `status.agents[]` | `{name, cluster, connected}` for each accepted agent. |

## `ObservabilityAgent` (separate CRD, cluster-scoped) — one cluster's agents

Short name `obsagent`. Lives on the managing cluster, which writes a `PixieVizier` onto the agent's
cluster, links it to its hub (a `PeerConnection` pair and a `PeerLink`), and delivers the deploy key
the hub issued for that cluster alone. An agent whose cluster's purpose may not report to its hub
(production and prod-support report only to a prod-support hub, nonprod only to a nonprod hub) is
`Refused`, and nothing of it is left on any cluster.

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The agent's `KubernetesCluster`. Immutable. |
| `spec.hubRef` | The `ObservabilityHub`. |
| `spec.imagePullSecretRef` | Registry credentials, copied as `pl/pixie-pull` under the production-credential rules. |
| `spec.nodeMemory` | Each node agent's memory (default 2Gi, at least 1Gi); its data store is 60% of it. |
| `status.phase` | `Pending \| Progressing \| Ready \| Degraded \| Refused`. `Ready` needs the agents ready and the hub seeing the cluster heartbeat. |
| `status.purpose` / `.hubPurpose` / `.link` | Both clusters' purposes, and `local` or the `PeerLink` that carries the link. |
| `status.ready` / `.components[]` / `.connected` | From the `PixieVizier` and the hub. |

## `PixieCloud` (separate CRD, namespaced) — self-hosted Pixie Cloud

Short name `pxcloud`. Written by an `ObservabilityHub` into namespace `plc` of the hub's cluster;
nobody writes it by hand. The cluster's own operator runs Pixie Cloud from the vendored manifests,
with OpenSearch instead of Elastic's, a CloudNativePG database with one role per service, the PxL
script server, minted certificates and keys, a default-deny sandbox, and the bootstrap
(administrator, organisation, one deploy key per agent). `spec` mirrors the hub (`release`, `hub`,
`purpose`, `domain`, `organization`, `adminEmail`, `pullSecretName`, `storage`), plus
`agents[] {name, cluster}` (only agents the managing cluster accepted) and `alertRoutes[]`.
`status` has `phase`, `ready`, `components[]`, `agents[] {name, deployKeySecret, registered,
connected}`, `caCertificate` (the hub's public trust root) and the `Ready` and `Alerting` conditions.

## `PixieVizier` (separate CRD, namespaced) — Pixie's agents on a cluster

Short name `pxvizier`. Written by an `ObservabilityAgent` into namespace `pl` of the agent's cluster.
The cluster's own operator runs Vizier (node agents, Kelvin, query broker, metadata, cloud connector,
NATS) with auto-update off and no updater, its own CA and signing key, and a default-deny sandbox.
`spec`: `release` (empty while the provider is not installable: the alert routing and rules are
written anyway and Vizier waits), `agent`, `purpose`, `clusterName`, `cloudAddress` (the hub's vzconn, never under
`.svc.cluster.local`), `cloudCASecretName`, `deployKeySecretName`, `pullSecretName`, `nodeMemory`,
`alertRoutes[]`. `status`: `phase`, `ready`, `components[]`, `clusterID` (what the hub registered),
and the `Ready` and `Alerting` conditions.

## `ClerkApplication` (separate CRD) — Clerk auth per environment

Provisions a **Clerk application per environment** (dev/testing/staging/prod) via the Clerk
**Platform API** (`POST /v1/platform/applications`), so Studio auth ([zetta#889](https://github.com/oramasearch/zetta/issues/889))
gets its issuer + keys with no manual dashboard work. One CR per environment; the resulting
publishable/secret keys + `CLERK_ISSUER` land in an owned Secret. Template: `CloudflareKVNamespace`.
Design: [orama-operator#116](https://github.com/oramasearch/orama-operator/issues/116).

| Field | Meaning |
|---|---|
| `spec.secretRef` | Secret holding the Clerk **Platform API token** (key `platformToken`). You create it by hand — the operator never sets or logs it. |
| `spec.applicationName` | Clerk app display name; the **idempotency key** — the reconciler adopts an existing app of this name instead of creating a duplicate. |
| `spec.environment` | `dev \| testing \| staging \| prod`. Informational + the default for `instanceType`. |
| `spec.instanceType` | `development \| production`. Default: `prod` → production (needs `spec.domain`), else development (free `clerk.accounts.dev`). |
| `spec.template` / `spec.domain` | optional Clerk template (`b2c-saas`…) / custom domain for a production instance. |
| `spec.keysSecretName` | where keys + issuer are written. Default `<name>-clerk-keys` (owner-ref'd → GC'd on delete). Keys: `publishableKey`, `secretKey`, `issuer`. |
| `spec.deletionPolicy` | `Retain` (default) orphans the Clerk app on CR delete — a user pool is **never destroyed by accident**; `Delete` removes it via the Platform API. |
| `status.applicationID` / `.instanceID` | the Clerk `app_…` / `ins_…` ids. `kubectl get clerkapp`. |
| `status.issuer` | the Frontend API URL → `CLERK_ISSUER` on the node (derived from the publishable key). |
| `status.publishableKey` | non-secret, surfaced in status; the **secret key lives only in the keys Secret**, never in status or a log. |

Wire the node: set `CLERK_ISSUER` on the `OramaZettaCluster` from the keys Secret's `issuer` — the
node then verifies Clerk session JWTs via JWKS ([zetta#889](https://github.com/oramasearch/zetta/issues/889)),
with the native bearer/session token still the on-prem fallback. A node with no `CLERK_ISSUER` is
bearer-only.
## `Lake` (separate CRD, cluster-scoped) — the lake service

Deploys **lake** — the hosted service behind `lake.live` (control plane on Postgres via CNPG, data plane on Zetta) (forge, drive, platform, identity, secrets,
PKI, registry and search over one content-addressed store; specified in
[`oramasearch/liquid`](https://github.com/oramasearch/liquid) `docs/features/18-lake-service.md`) — from
one **cluster-scoped** CR. It creates the target **Namespace** (if missing; not owned, so deleting a
Lake never takes a namespace with it), a hardened **Deployment** (`lake-<name>`, running
`spec.image`), a ClusterIP **Service** (`lake-<name>`, port 80), and an owned
**`OramaZettaCluster`** (`lake-<name>-zetta`) for the data plane. Replicated workloads also get a
PodDisruptionBudget and topology spread. A spec change rolls the Deployment once. A ReadWriteMany
store surges with `maxUnavailable: 0`. A single-replica Lake on a ReadWriteOnce store recreates by
default and surges onto the running pod's node only with `spec.rollout.overlapOnSharedStore` (see
`docs/lake/README.md`, Rollouts). A Lake finalizer starts and waits for Zetta's
ordered cleanup before releasing the parent. With `spec.ingressEnabled` and `spec.host` it fronts the
host with an **nginx Ingress** (the whole host → the lake Service), origin TLS on every host
([`lake/origin-tls.md`](lake/origin-tls.md), operator#578), and, when
`spec.dnsSecretRef` is given, an **owned `CloudflareDNSRecord`** (proxied, host → the ingress
LoadBalancer) — the same contract and child-controller path as `OramaZettaCluster`. Gating off
tears the Ingress and the record down and leaves the workloads. Sample:
[`../config/samples/orama_v1alpha1_lake.yaml`](../config/samples/orama_v1alpha1_lake.yaml).

| Field | Purpose |
|---|---|
| `spec.namespace` | Namespace the workloads land in (default `lake-dev`). Created if missing; never deleted by the operator. |
| `spec.image` | The lake image, run as built. **Required, no default**; the sample uses the private `oramasearch/lake:v1-sandbox`. |
| `spec.pullSecretName` | imagePullSecret attached to the pods, for a private `spec.image`. Must exist in `spec.namespace`. |
| `spec.port` | Container port the Service (port 80) and probes target. Default 4900, lake's `--listen` default. Readiness is `GET /readyz`; liveness is `GET /healthz`. |
| `spec.args` | Args for the image's entrypoint; empty runs the image as built (lake listens on `0.0.0.0:4900`). |
| `spec.replicas` | Deployment replicas (default 1). |
| `spec.resources` | Complete Kubernetes container resource override. Default requests: CPU `50m`, memory `128Mi`; default limit: memory `512Mi`, with no CPU limit. |
| `spec.topologySpreadConstraints[]` | Complete pod topology-spread override. When omitted with more than one replica, defaults to `maxSkew: 1`, `topologyKey: kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting this Lake's pods. An explicit empty list disables the default. At one replica no default is rendered. |
| _(workload hardening — automatic)_ | The pod and container require non-root uid/gid 10001, use fsGroup 10001 with `fsGroupChangePolicy: OnRootMismatch`, disallow privilege escalation, drop all Linux capabilities, and use `RuntimeDefault` seccomp. Readiness is `GET /readyz` every 5 s, a startup probe on `GET /healthz` allows five minutes before liveness applies, the container gets a 15-second kubelet `sleep` pre-stop drain, and the pod gets a 45-second termination grace period. A ReadWriteMany store or a multi-replica Lake rolls with `maxSurge: 1`, `maxUnavailable: 0`. A single-replica Lake on any other store uses `Recreate` unless `spec.rollout.overlapOnSharedStore` is set; a ReadWriteOncePod claim always uses `Recreate`. More than one replica creates `PodDisruptionBudget lake-<name>` with `maxUnavailable: 1`; scaling to one deletes it. |
| `spec.store` | Persistent libliquid customer store mounted read-write at `/var/lib/lake/store`, with `LAKE_STORE_ROOT` fixed to that path. Without `existingClaim`, the operator creates and owns `lake-<name>-store` once. Later store spec changes never mutate, replace, or delete it. |
| `spec.store.size` / `.storageClassName` / `.accessModes` | Creation-time settings for the generated claim: quantity `100Gi` by default, cluster-default storage class, and `[ReadWriteMany]`. RWX is the multi-replica default; `ReadWriteOnce` requires `spec.replicas: 1`. |
| `spec.store.existingClaim` | PVC in `spec.namespace` to mount instead of creating one. It is referenced without adoption, mutation, or deletion. |
| `spec.rollout.overlapOnSharedStore` | Default `false`. A single-replica Lake on a ReadWriteOnce claim (or a claim that does not exist yet) rolls with `Recreate`, which leaves a restart gap. `true` makes it surge instead (`maxSurge: 1`, `maxUnavailable: 0`) and adds required pod affinity on `kubernetes.io/hostname` to its own pods, so the new pod mounts the volume beside the old one. During that overlap two lake processes open one store root, so set it only once that has been proven safe (see `docs/lake/README.md`, Rollouts). It has no effect on a ReadWriteMany claim or a multi-replica Lake, which always surge, or on a ReadWriteOncePod claim, which always recreates. Toggling it changes the pod template once. |
| `spec.env[]` | Kubernetes `EnvVar` entries appended after operator env. `value` and `valueFrom` are supported. Conflicts with `LAKE_ADMIN_TOKEN`, `LAKE_APPS_RUNTIME_IMAGE`, `LAKE_METHODOLOGY_RUNTIME_IMAGE`, `LAKE_INSTANCE_KEK`, `LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_DATA_PLANE_URL`, `LAKE_DATA_PLANE_TOKEN`, `LAKE_INTERNAL_URL`, `LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, `LAKE_SERVICE_DIR`, or `LAKE_STORE_ROOT` are ignored so operator values win. `LAKE_APPS_WILDCARD` is a compatibility exception: the last explicit entry wins over `spec.apps.wildcard` and is rendered once. Use `spec.env` for Lake runtime settings such as `LAKE_LOG_MODE`, `LAKE_LOG_WITNESSES`, and `LAKE_PUBLIC_HOST`. |
| `spec.sealing.generation` | Desired X25519 sealing-key generation (default 1). Increasing it rotates; decreasing it is refused. Old private keys remain while any `LakeSealedSecret` record names them. |
| `spec.postgres` | **lake's control plane**: a CloudNativePG `Cluster` named `lake-<name>-controldb` in `spec.namespace`, the same path `OramaCloudV1` uses for its control DB. `enabled` (default `true`; `false` tears it down, deleting its data), `instances` (default 1), `storageSize` (default `50Gi`; CNPG grows the PVC in place when the StorageClass allows expansion), `storageClassName`. CNPG generates the `lake-<name>-controldb-app` Secret; the lake container gets `LAKE_CONTROL_DB_URI` and `LAKE_CONTROL_DB_PASSWORD` from it (optional refs, so the pod starts before the DB does). Requires the CNPG operator (helmfile). |
| `spec.zetta` | **lake's data plane**: an owned `OramaZettaCluster` named `lake-<name>-zetta` in `spec.namespace`. `enabled` defaults to `true`; `false` deletes the cluster and its per-node data volumes. `image` (empty uses the operator default in `internal/controller/lake_defaults.go`), `replicas` (default 1), `storage.size` (Zetta default `10Gi`), `storage.storageClassName`, `env` (followed by the operator's default Zetta env for each name it does not set: `ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864`, `MALLOC_ARENA_MAX=2`, and an empty `ZETTA_EGRESS_ALLOW`, which denies all egress as the Lake namespace's NetworkSandbox does; [egress rules](infrastructure/egress-rules.md#a-lake-zettas-own-allowlist-defense-in-depth)), and `reachability` (`dial-out-only` by default or `wan-accessible`) map to the child CR. The child inherits the Lake's `pullSecretName`. It sets `provisionOperatorSchema: true`: the cluster is also the Lake's deployment control plane, the one operator-schema Zetta in the Lake namespace (operator#557; managed services are still requested through the control Postgres). The lake container gets `LAKE_DATA_PLANE_URL=http://lake-<name>-zetta-zetta.<namespace>.svc.cluster.local:4800` and `LAKE_DATA_PLANE_TOKEN` from Secret `lake-<name>-zetta-zetta-admin`, key `token`; the token reference is required, so kubelet waits for the Zetta controller to mint it. With a public host (`spec.host`, or `LAKE_PUBLIC_HOST` in `spec.env`) it also gets the same URL and Secret reference as `LAKE_OPERATOR_ZETTA_URL` and `LAKE_OPERATOR_ZETTA_TOKEN`, and `LAKE_PUBLIC_HOST=<spec.host>` when `spec.env` does not state it; all three are reserved. See [Lake deployment environments](lake/environments.md#every-lakes-control-plane-is-its-own-zetta). |
| `spec.postgres.image` | Optional CNPG-compatible operand image pinned by SHA-256 digest. Unset keeps CNPG's default. Set a reviewed pin before restore or upgrade rehearsals; changing it requests CNPG's normal database rollout. The Lake also declares a separate `lake_rehearsal` read-only login for verification. |
| `spec.postgres.backup` | Opt-in (`enabled`, default `false`) CNPG Barman object-store backup. `schedule` defaults to six-field `0 0 3 * * *`; `retention` defaults to `30d`; `destination.bucket` and `secretRef.name` are required when enabled. `destination.path` defaults to `lake-<name>/controldb`; `endpointURL` defaults to the Secret's `s3ApiEndpoint`. The Secret must be in `spec.namespace` and contain `s3ApiEndpoint`, `accessKeyId`, and `secretAccessKey`. The operator configures gzip WAL compression and creates owned `lake-<name>-controldb-backup` `ScheduledBackup` resources. See [`lake/backups.md`](lake/backups.md). |
| _(admin token — automatic)_ | Secret **`lake-<name>-admin`** (key `token`, 64 hex), minted once and preserved, owned by the Lake; injected as `LAKE_ADMIN_TOKEN`. lake requires it as `Authorization: Bearer` on every `/v1` capability except `server_info` and on MCP tool calls. Retrieve: `kubectl get secret lake-<name>-admin -n <ns> -o jsonpath='{.data.token}' \| base64 -d`. |
| _(sealing key — automatic)_ | Secret **`lake-<name>-sealing-key`** holds the current and still-needed prior X25519 private keys. Only the active public key and `kid` are injected into lake. See [`lake/sealed-secrets.md`](lake/sealed-secrets.md). |
| _(schema — automatic)_ | With Postgres on and an image that has `lake manifest` (lake#1147 onward), the Lake reads the image's manifest once (Job `lake-<name>-manifest`, cached in `status.imageManifest`) and owns a `lake-<name>-controldb-schema` **`PostgresSchema`** with `spec.migrate` running `lake migrate --wait-seconds 600` from the image by digest, versioned `image:<digest>`; see [the schema guide](lake/schema.md#a-lake-release-is-only-its-image-orama-operator936) ([#936](https://github.com/oramasearch/orama-operator/issues/936)). For an older image, the Lake owns a `lake-<name>-controldb-migrations` ConfigMap holding lake's control-plane schema (vendored byte-identical from `oramasearch/lake` `db/schema.sql`) and a `lake-<name>-controldb-schema` **`PostgresSchema`** that applies it in one transaction, guarded on the newest migration table (`workspace_volumes_guard` for `0069_workspace_volumes`) plus every migration the file records being present in `schema_migrations`, and versioned by a hash of SQL plus guard. Schema completion gates Deployment creation and updates. Re-vendor lake `db/schema.sql` unchanged, update the pinned source and guard, and run the checks in [the schema guide](lake/schema.md). Incremental migrations remain #126. |
| `spec.ingressEnabled` | **Default `false`. The explicit external-exposure gate.** With `spec.host`, provisions the nginx Ingress (+ the DNS record below if `dnsSecretRef` is set). `false` tears both down even if `host` is still set. The Ingress permits `64m` request bodies and disables request/response buffering for Lake's streaming transfers. |
| `spec.tls.secretName` | Optional hand-supplied `kubernetes.io/tls` Secret in `spec.namespace` for `host` and `apexHost`. Empty (the normal case) uses the certificates the operator issues: every Lake Ingress serves HTTPS either way. Without it, the Lake's `Issuer` issues every host: DNS-01 in the `dnsSecretRef` zone, HTTP-01 for any other host and for a Lake with no `dnsSecretRef` ([operator#632](https://github.com/oramasearch/orama-operator/issues/632)). See [`lake/origin-tls.md`](lake/origin-tls.md). |
| `spec.host` | Domain the lake is exposed at (e.g. `hello.lake.live`) when `ingressEnabled` is `true`. |
| `spec.ingressClassName` | `nginx` (default) or `zetta-tunnel`: which ingress serves the Lake's Ingress and its environments'. DNS follows, because it is written from the Ingress's address. The per-cluster cutover sets it in the cluster's set member ([`infrastructure/ingress.md`](infrastructure/ingress.md)). |
| `spec.dnsSecretRef.name` | With `ingressEnabled` + `host`, creates a **proxied Cloudflare DNS record** (host → ingress LB) as an owned `CloudflareDNSRecord`, once the ingress controller has assigned an address (the owned Ingress's status change re-queues the Lake). The Secret (keys `token`/`zoneID`) must live in `spec.namespace` and hold the zone of `host` — a `lake.live` host needs a `lake.live`-zone Secret, not the `oramadev.net` one the Zetta clusters use. It is also origin TLS's credential: the Lake's Let's Encrypt `Issuer` `lake-<name>-letsencrypt` (DNS-01, key `token`, for names in the zone; other names and a Lake without `dnsSecretRef` use HTTP-01), and its zone policy, an owned `EdgeProvider` and `EdgeZonePolicy` `lake-<name>-edge` for `zoneID` ([below](#edgezonepolicy-separate-crd-namespaced--secure-a-dns-zones-edge)). The token needs Zone > DNS > Edit, **Zone > Zone Settings > Edit**, Zone > Zone > Read and **Zone > SSL and Certificates > Edit** (edge certificates for app hosts, lake#798). |
| `spec.region` | `{name, label}`: where the Lake runs, for its region picker (decision 64). Rendered as `LAKE_REGION` and `LAKE_REGION_LABEL`. In a LakeSet, `name` comes from the member's cluster ([`lake/README.md#regions`](lake/README.md#regions)). |
| `spec.regions` | The deployment's regions as the picker offers them, `[{name, label, host, latitude, longitude}]`, rendered as `LAKE_REGIONS` (JSON). A LakeSet computes it from its members. |
| `spec.apexHost` | A second host the Lake answers (the region picker, e.g. `lake.live`). With `ingressEnabled` + `host`, adds an Ingress rule (and TLS host) and, with `dnsSecretRef`, an owned proxied `<lake>-apex-dns` record; rendered as `LAKE_APEX_HOST`. Removing it removes both. |
| `spec.customDomain.host` | A host in a zone the **customer** owns (white label, [operator#947](https://github.com/oramasearch/orama-operator/issues/947)). Needs `host` and `ingressEnabled`; must differ from `host` and `apexHost`; `spec.env` may not then set `LAKE_PUBLIC_HOST`, `LAKE_HOST_ALIASES` or `LAKE_CONTROL_HOST` (CEL). The operator writes no record in the customer's zone: it publishes a routing CNAME (to the DNS-only origin record `<host label>.origin.<zone>` it keeps as an owned `CloudflareDNSRecord` `lake-<name>-custom-domain-origin`, or to the load balancer without `dnsSecretRef`) and an ownership TXT `_orama-verification.<host>` in `status.customDomain.records`, looks both up, and once both are found creates the Ingress `lake-<name>-custom-domain` (no Cloudflare edge settings) and a `Certificate` `<host>-tls` solved with HTTP-01. Once that is `Ready` it renders `LAKE_PUBLIC_HOST=<customer host>`, `LAKE_HOST_ALIASES=<host>` and `LAKE_CONTROL_HOST=<host>`. Clerk allows the customer origin once verified, and a Clerk production instance's records in the customer's zone are published, not written. Condition `CustomDomainReady`. Removing it removes the Ingress, the certificate, its Secret and the origin record. See [`lake/custom-domains.md`](lake/custom-domains.md). |
| `spec.clerk.secretKeySecretRef.name` | A Secret in `spec.namespace` holding a Clerk instance's Backend API secret key (key `secretKey`, optional `publishableKey`). The operator keeps the instance configured for lake through the Backend API (the `lake` JWT template, the host and apex as allowed origins, a webhook endpoint at `https://<host>/identity/clerk-webhook`), writes a production instance's DNS records as owned, unproxied, `Retain` `CloudflareDNSRecord`s with `dnsSecretRef`, and renders the reserved `LAKE_CLERK_PUBLISHABLE_KEY`, `LAKE_CLERK_SECRET_KEY` (from this Secret's `secretKey`) and `LAKE_CLERK_WEBHOOK_SECRET` (from the owned Secret `lake-<name>-clerk`). Condition `ClerkReady`. Without `spec.clerk` nothing changes. See [`lake/clerk.md`](lake/clerk.md) ([operator#541](https://github.com/oramasearch/orama-operator/issues/541)). |
| `spec.payments` | `{mode, secretRef}`: the Stripe account the Lake bills through ([operator#639](https://github.com/oramasearch/orama-operator/issues/639)). `mode` is `test` or `live`; `secretRef.name` is a Secret in `spec.namespace` with `accountID` (`acct_…`), `operatorKey` and `appKey`, both keys of `mode`. Requires `spec.host` and `spec.billing.enabled: true` (the API server refuses it otherwise). The Lake owns a `PaymentProvider` and a `PaymentWebhookEndpoint` `lake-<name>-payments` (`https://<host>/billing/stripe/webhook`, Lake's invoice and Checkout events, `apiVersion: 2024-06-20`, signing secret in `lake-<name>-payments-webhook`) and a `PaymentCatalog` `lake-<name>-catalog` (`lake_usage`, `lake_private_warehouse`, `lake_dedicated_cluster`, no prices), checks that `appKey` sees `lake_usage`, and once all of that is in place renders the reserved `LAKE_STRIPE_SECRET_KEY` (`appKey`) and `LAKE_STRIPE_WEBHOOK_SECRET` (`signingSecret`), with pod annotation `orama.cloud/payments-secret-hash`. Condition `PaymentsReady`; a payments failure never fails the Lake. Removing it deletes the endpoint before the provider. Without `spec.payments` nothing changes. See [`lake/payments.md`](lake/payments.md). |
| `spec.billing` | `{enabled}`: whether the Lake bills ([operator#662](https://github.com/oramasearch/orama-operator/issues/662)). `enabled: true` renders the reserved `LAKE_BILLING=on`; `enabled: false` renders no `LAKE_BILLING`, so billing is off. Set either way, it replaces a `spec.env` `LAKE_BILLING` entry, and the operator logs that the entry is ignored. Unset (on-prem and enterprise installs), the operator renders nothing and `spec.env` keeps whatever it says. `spec.payments` requires `enabled: true` (CEL); billing without payments uses lake's manual provider. See [`lake/payments.md`](lake/payments.md#billing-specbilling). |
| `spec.previewWildcard` | DNS suffix for preview environments. `preview.example.com` yields `<change-short>.preview.example.com`; with Lake ingress and DNS enabled, the Lake owns one `*.preview.example.com` Cloudflare record and previews do not create records per change. |
| _(every feature — no switch)_ | **Every `Lake` runs every feature** ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)): apps, builds, methodology runs and deployments, agents (the whole Agent Library and user-created launches and schedules), managed services and stores, workspaces, connections, egress rules, tunnels, peers and volume snapshots. There is no per-Lake `enabled` field; the old `apps`, `builds`, `services`, `workspaces` and `snapshots` `enabled` fields were removed and the CRD prunes stored values. Every image has an operator default, one table in `internal/controller/lake_defaults.go` (listed in [`lake/README.md`](lake/README.md#default-images)); a spec image overrides its default. The one prerequisite is the control Postgres (`spec.postgres.enabled`, default `true`): without it, condition `FeaturesReady` is `False` with reason `PostgresDisabled` and names the features that cannot run. |
| `spec.apps.runtimeImage` | Private app-runtime image used by `LakeApp` resources in runtime mode. Empty uses the operator default (`internal/controller/lake_defaults.go`). Injected into lake as the reserved `LAKE_APPS_RUNTIME_IMAGE`, which lake uses as the base image for generated app builds. Apps-table synchronization from Lake control Postgres uses `lake_apps` notifications and a 60-second fallback list. |
| `spec.apps.wildcard` | DNS suffix for apps. An app without `spec.host` uses `<app>.<wildcard>`. **Empty uses the Lake's `spec.host`**, so an app is `<app>.<lake host>` (`myapp.lake-dev.oramadev.net`); HTTPS on such two-level names needs a certificate per zone ([#515](https://github.com/oramasearch/orama-operator/issues/515)). The operator injects it into lake as `LAKE_APPS_WILDCARD`; an explicit `spec.env` entry wins and is rendered once. With `dnsSecretRef`, the shared `*.<wildcard>` DNS record is owned by the apps-table sync path. |
| `spec.apps.ingressClass` | Ingress class for apps; empty defaults to `nginx`. |
| `spec.builds` | Every Lake reconciles lake's in-cluster build rows; lake gets its Service address as `LAKE_INTERNAL_URL`, which it uses as the registry authority. Each queued row becomes one isolated rootless BuildKit Job. See [`lake/builds.md`](lake/builds.md). |
| `spec.builds.builderImage` | Private `oramasearch/lake-builder:<tag>` image used for every build. Empty uses the operator default (`internal/controller/lake_defaults.go`). When `spec.pullSecretName` is set, the operator copies that Secret from the Lake namespace to each build namespace as `builder-pull`; the Job references the local copy because image pull Secrets cannot cross namespaces, mounts its `.dockerconfigjson` read-only at `/etc/lake-builder/pull/.dockerconfigjson`, and sets `BASE_REGISTRY_AUTH` to that path for private base-image pulls. With no Lake pull Secret, the auth mount and environment variable are absent. |
| `spec.builds.resources` | Builder container resource override, merged over bounded defaults for CPU, memory, and ephemeral storage. |
| `spec.builds.concurrency.perLake` / `.perSpace` | Active Job caps for the whole Lake and one target registry space. Defaults are 4 and 2; both must be at least 1. |
| `spec.methodologies.runtimeImage` | Private `oramasearch/methodology-runtime` image. Injected into lake as the reserved `LAKE_METHODOLOGY_RUNTIME_IMAGE` (the base of every methodology image build) and run by each methodology run's stage and collect pods. Empty uses the operator default (`internal/controller/lake_defaults.go`). See [`lake/methodology-runs.md`](lake/methodology-runs.md). |
| `spec.methodologies.workVolumeSize` / `.storageClassName` | Each methodology run's work volume: size (default `10Gi`) and storage class (default the cluster's). |
| `spec.methodologies.namespaceQuota` | Hard limit of the ResourceQuota `methodology-ceiling` on the methodology-runs namespace: a ceiling on every workspace's runs and served models together. Each run and served model also counts against its own workspace's quota through a never-scheduled reservation pod in the workspace's namespace ([orama-operator#799](https://github.com/oramasearch/orama-operator/issues/799)). Empty uses the default `requests.cpu` 64, `requests.memory` and `limits.memory` 256Gi, `requests.nvidia.com/gpu` 8, `pods` 500, `persistentvolumeclaims` 200. See [`lake/methodology-runs.md`](lake/methodology-runs.md#counted-against-the-workspaces-quota). |
| `spec.builds.cacheEnabled` | Enables a per-space BuildKit registry cache (`<registry>/<space>/cache/<image>`). Defaults to `true`. |
| `spec.deploySync.interval` | How often the operator reads desired deployment events from its Zetta `orama_control.deployments` table and polls every Lake's control rows (`services`, stores, agents, and `builds`). Default `15s`; positive values below `1s` are clamped to `1s`. Workspaces have a fixed 60-second recovery list in addition to `lake_workspaces` notifications. |
| `spec.agents.runtimeImage` | `oramasearch/agent-runtime:<tag>`, the image every launch of a user-created Python agent runs on ([`lake/agents.md`](lake/agents.md), lake#231). Empty uses the operator default (`internal/controller/lake_defaults.go`). Each row requires `workspace_id`; a present launch becomes a Job (`job`) or a Deployment + Service (`service`) `<name>-<slug>` in the joined workspace namespace, with a local credential Secret `<name>-<slug>-credential` whose SHA-256 only is written to `agent_launch_status`. |
| `spec.agents.library[]` | `{key, image}`: overrides the image of one of the Agent Library's specialized agents. **Every Lake runs all nine** on the operator's default images (`internal/controller/lake_defaults.go`); an entry replaces the image of its key and cannot remove an agent. The agents (`agent-ask-orama`, `agent-project`, `agent-autosuggest`, `agent-creator`, and since lake#263 `agent-eval-generator`, `agent-autosuggest-corpus`, `agent-project-corpus`, and since lake#296 `agent-application`, which builds and deploys apps only through lake's capabilities and needs no cluster privileges, and since lake#304 `agent-experiments`, which runs AutoML studies the same way), each a Deployment + Service `<name>-<key>` on 8080; lake gets `LAKE_AGENT_ENDPOINTS`, `LAKE_INTERNAL_URL`, `LAKE_AGENT_MODEL`, `LAKE_AGENT_SERVICE_TOKEN` (Secret `lake-<name>-agent-service-token`, minted once) and `LAKE_AGENT_BUS_DIR` (Secret `lake-<name>-agent-bus`, one agent-bus credential per agent; each agent gets only its own as `AGENT_BUS_TOKEN`). A library agent whose key the operator no longer runs is deleted. |
| `spec.agents.openaiSecretName` | A Secret in `spec.namespace` with `OPENAI_API_KEY`, the general Orama key; people provide it. Only the library agents in `spec.namespace` get it: it is never copied into a workspace namespace or given to a launch or schedule pod, and copies an earlier operator made are deleted ([orama-operator#779](https://github.com/oramasearch/orama-operator/issues/779), `docs/lake/agents.md`). |
| `spec.agents.model` | The frontier model agents default to (default `gpt-6.1-sol`). |
| `spec.agents.jobDeadlineSeconds` | `activeDeadlineSeconds` of a job launch (default 3600, minimum 60). |
| `spec.services` | Managed services, run by every Lake ([`lake/services.md`](lake/services.md), liquid-bridge decision 46); needs `spec.postgres` enabled. Every row requires `workspace_id` and joins the operator-written workspace namespace; there is no Lake-namespace fallback. The operator creates the selected Zetta, Postgres, catalog, SQLite, DuckDB or Polars resource there with Lake, workspace, customer, service and kind labels. The Zetta named by `workspaces.zetta_service_id` is excluded because the workspace reconciler owns its cluster and status. Connections are projected into Lake's trusted `lake-<name>-services` Secret for the control plane. Status is generation guarded. An `absent` row irreversibly removes only resources that pass ownership checks. See the service guide for catalog resources, readiness, credentials and deletion order. |
| `spec.services.maxServices` | Cap on live services for this Lake (`int32`, minimum `0`). `0` is unlimited. A **new** service past the cap creates nothing and is `Failed` with message `service quota reached (<n>)`; it starts on a later poll once there is room. Existing services are never refused or removed. Workspace count has its own `spec.workspaces.maxWorkspaces`. |
| `spec.services.zettaImage` | Image for Zetta services and workspace Zettas. Empty falls back to `spec.workspaces.image`, then the operator default (`internal/controller/lake_defaults.go`). |
| `spec.services.zettaEnv[]` | Kubernetes `EnvVar` entries copied to `spec.env` on every managed Zetta service, including the workspace-owned cluster through the workspace reconciler, followed by the operator's default Zetta env for each name the list does not set (`ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864`, `MALLOC_ARENA_MAX=2`, and `ZETTA_EGRESS_ALLOW` rendered for each Zetta from the EgressRules, Connections and `internetEgress` its workspace NetworkSandbox carries for it; see [egress rules](infrastructure/egress-rules.md#a-lake-zettas-own-allowlist-defense-in-depth)). Last comes `ZETTA_CONNECTOR_ALLOW_DESTINATIONS`, always the operator's (a stated value is dropped): the exact `host:port` of each postgres, tidb, sqlite, duckdb and polars service in the Zetta's workspace namespace, empty when there are none ([connector destinations](infrastructure/egress-rules.md#a-workspace-zettas-approved-connector-destinations), [#882](https://github.com/oramasearch/orama-operator/issues/882)). Changes roll each service cluster's StatefulSet. Reserved-name handling and collision status follow the `OramaZettaCluster` contract. |
| `spec.services.sqliteImage` | Image for SQLite services: [lake-sqlite](lake/lake-sqlite.md), which the operator runs itself. Empty falls back to the operator's `--lake-sqlite-image` flag, which the chart sets from `lakeServices.sqliteImage` (default `oramasearch/lake-sqlite:0.1.1`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every SQLite service's pod template, rolling each pod once. |
| `spec.services.duckdbImage` | Image for DuckDB services: [lake-duckdb](lake/lake-duckdb.md), powered by DuckDB, which the operator runs itself (one StatefulSet of one pod per service). Empty falls back to the operator's `--lake-duckdb-image` flag, which the chart sets from `lakeServices.duckdbImage` (default `oramasearch/lake-duckdb:0.1.0`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every DuckDB service's pod template, rolling each pod once. |
| `spec.services.polarsImage` | Image for Polars services: [lake-polars](lake/lake-polars.md), powered by Polars, which the operator runs itself (one StatefulSet of `instances` workers per service). Empty falls back to the operator's `--lake-polars-image` flag, which the chart sets from `lakeServices.polarsImage` (default `oramasearch/lake-polars:0.1.0`). The pods pull with the Lake's `pullSecretName`. Changing it rewrites every Polars service's pod template, rolling each pod once. |
| `spec.workspaces` | Every Lake reconciles lake's control-Postgres `workspaces` rows into isolated `<environment>-ws-<first 12 UUID hex>` namespaces. `lake_workspaces` notifications wake the controller; a complete list every 60 seconds recovers missed notifications. Needs Postgres and waits for the current Lake schema. See [`lake/workspaces.md`](lake/workspaces.md). |
| `spec.workspaces.image` / `.storage` / `.resources` | Workspace Zetta settings. `spec.services.zettaImage` takes image precedence, then `workspaces.image`, then the operator default (`internal/controller/lake_defaults.go`). `storage.storageClassName` is the class of every workspace volume. The size comes from lake: each workspace Zetta is created at its row's `storage_gi` (lake migration `0069`, default 5 GiB), and `storage.size` (default `5Gi`) is **deprecated and ignored** with lake's 0069 schema: it applies only to a row without `storage_gi`, and new workspaces take lake's column default. Change a workspace's size with lake's `set_workspace_storage`. The workspace namespace's ResourceQuota `requests.storage` is the plan's value (headroom for the namespace's other claims) plus `storage_gi` × replicas, so growth is not capped by the plan. When `storage_gi` grows, the operator patches `spec.resources.requests.storage` of each claim `data-<cluster>-zetta-<n>` in place (the StatefulSet's claim template is immutable and is left as created), waits for `status.capacity`, raises the child's `spec.storage.size` to match, and writes `storage_gi_observed`, `storage_phase` (`Pending \| Resizing \| Ready \| Failed`) and `storage_message` on the workspace row. A size below a claim's is ignored and reported; volumes never shrink. A class without `allowVolumeExpansion`, or an API refusal such as a quota, is `Failed` with the reason and does not fail the workspace. See [`lake/workspaces.md`](lake/workspaces.md#workspace-volume-size-operator259). Resources default to a `64Mi` memory request and `1Gi` memory limit. The Lake pull Secret is copied into each workspace namespace. |
| `spec.snapshots.className` / `.driver` | Every Lake takes scheduled CSI snapshots of lake's store volume and of every workspace Zetta's volumes, from lake's `volume_snapshot_policies` (lake migration `0069`, operator#260). The operator creates the `VolumeSnapshotClass` `className` (default `lake-retain`) for `driver` (default `disk.csi.azure.com`) with `deletionPolicy: Retain` when it is absent, and refuses an existing one that is not `Retain`. Each target's claims get a `VolumeSnapshot` `<claim>-<UTC timestamp>` labelled `orama.cloud/snapshot-target`, when the schedule comes due, when there is none yet and when lake's `requested_at` is newer than the last; only `readyToUse` snapshots count toward `keep` and the newest ready one is never deleted; the newest failed one is kept for diagnosis and older failed ones deleted; before a live target's snapshot is pruned its bound content (checked for the driver and its binding) is switched to `deletionPolicy: Delete` so the cloud snapshot goes too; hand-made and other targets' snapshots are never touched; bound contents are labelled with the target; requests and schedules are judged by Postgres's `now()`; a schedule that never fires is reported and takes nothing (a request still works); and `volume_snapshot_status` reports the newest. `lake_volume_snapshots` notifications wake the controller; it also polls every 60 seconds. Requires Postgres and the `snapshot.storage.k8s.io` CRDs. See [`lake/volume-snapshots.md`](lake/volume-snapshots.md). |
| `spec.workspaces.maxWorkspaces` | Cap on running or stopped workspace namespaces. `0` is unlimited. Lowering it never removes an existing namespace. A new row over the cap is `Failed` with `workspace quota reached (<n>)`. |
| `spec.workspaces.defaultPlan` / `.plans` | Named workspace ResourceQuota and LimitRange profiles. Empty `defaultPlan` selects the built-in `default` plan. A custom plan provides `quota`, `defaultRequest` and `default` ResourceLists; missing or incomplete plans fail closed. |
| `spec.workspaces.overrides[uuid]` | Per-workspace `{plan, internetEgress, mesh}`. `mesh: true` enrols the workspace's sandbox in zetta-tunnel's mesh (default `false`; [`infrastructure/mesh.md`](infrastructure/mesh.md)). A missing override uses the default plan and denies internet access. `internetEgress: true` allows only public TCP 443 while excluding private/reserved ranges, metadata, Kubernetes API Service and endpoints, and current Node addresses and Pod CIDRs; the operator carries it as the workspace's `EgressRule/internet-https`. |
| `status.readyReplicas` / `.phase` | Replica count and overall service state: `Progressing \| Ready \| Degraded`. `Ready` requires the current Deployment generation with the desired positive ready/updated/available replica counts, a Bound store, enabled Postgres readiness and current applied schema, enabled Zetta readiness, and valid enabled backup configuration. Replica count alone does not establish readiness. Written only on a real transition. |
| `status.conditions[Ready]` | `LakeReady` on success. Pending work reports `Progressing` with `DeploymentProgressing`, `PostgresNotReady`, `SchemaPending`, `SchemaNotCurrent`, or the store/Zetta reason. Failed schema (`SchemaFailed`), Deployment replica failure (`ReplicaFailure`) or progress deadline (`ProgressDeadlineExceeded`), degraded Zetta, and invalid enabled backup configuration report `Degraded`. Failures take precedence over pending work; messages explain the blocker. Unchanged observations preserve transition times and write no status. |
| `status.conditions[FeaturesReady]` | `True` (`AllFeatures`) when the Lake can run every feature. `False` (`PostgresDisabled`) when `spec.postgres.enabled` is `false`, naming the features that cannot run without the control Postgres. It does not change `phase`. |
| _(`LAKE_MCP_RUNNER_IMAGE`)_ | The image lake runs MCP servers on. The operator injects its default (`internal/controller/lake_defaults.go`) unless `spec.env` sets the variable, whose value is then rendered once. |
| `status.store` / `conditions[StoreReady]` | Selected `claimName`, PVC `phase`, and bound `capacity`. `StoreReady=False` reports a missing or unbound claim; an unbound store keeps the overall phase out of `Ready`. |
| `status.schemaPhase` / `.schemaVersion` | Mirrors the owned PostgresSchema: `Pending \| Applying \| Applied \| Failed` and the applied `sha256:<12>` (vendored schema) or `image:<digest>` (an image that migrates itself). Overall readiness requires the schema controller to observe the current generation, `Applied=True`, and the applied version to match the desired version (the SQL-plus-guard hash, or the image digest). A stale `Applied` value keeps the Lake `Progressing` and cannot release a rollout. |
| `status.imageManifest` | What the image reported through `lake manifest`, read once per `spec.image` by Job `lake-<name>-manifest` ([#936](https://github.com/oramasearch/orama-operator/issues/936)): `image`, `imageRef` (by digest, from the pod's `imageID`), `supported` (`false` for an image that predates the command: vendored schema and Go Stripe lists), `version`, `revision`, `schemaVersion`, `latestMigration`, `guardTable`, `guardTables`, `migrations`, `migrateArgs`, `databaseEnv`, `stripeWebhookEvents`, `stripeProducts`. Its `version` and `revision` are the `Version` and `Revision` columns of `kubectl get lake` (and Lens's list), beside `Image` (`spec.image`) ([#983](https://github.com/oramasearch/orama-operator/issues/983)). While a new image's is read the previous image's stays and the rollout is held; a Job that cannot run gives Ready reason `ImageManifestFailed`, untrusted output `ImageManifestInvalid` (Degraded). |
| `status.postgresReady` / `.postgresInstances` | True when CNPG reports at least the configured `spec.postgres.instances` ready instances (default 1). The count mirrors CNPG `readyInstances`/`instances`, e.g. `1/1`; missing/unreadable status is unready. Polled every 20s whenever Postgres is enabled, including after success and with backups disabled. Readiness loss removes overall `Ready` on the next poll. Disabled Postgres reports false with an empty count and does not gate readiness or schema. |
| `status.zettaReady` / `conditions[ZettaReady]` | True only when the owned `OramaZettaCluster` reports `Ready` with every desired node ready. `ZettaProgressing`, `ZettaDegraded`, and `ZettaDisabled` explain false states. The Lake owns and watches the child and also polls every 20s while it is enabled but unready. |
| `status.backupReady` / `.lastBackupTime` | True only when the ScheduledBackup exists and the newest CNPG Backup labelled `cnpg.io/cluster=lake-<name>-controldb` is `completed`; completion time comes from that Backup. The `Backup` print column shows `lastBackupTime`. Invalid enabled configuration reports `BackupReady=False` with a condition and `Phase=Degraded`. |
| `status.dataServices[]` | One row per associated data service: `name`, `kind` (`postgres \| zetta`), `managed`, and `backup`, `restore`, `upgrade`, `failover` evidence (`tested \| untested \| none`), plus optional `lastVerified` and `notes`. `managed` is true only when all operations required by the current configuration are tested. Single-instance Postgres reports `failover: none`; owned Zetta remains unmanaged until its lifecycle procedures are rehearsed. The `Data` print column uses `dataServicesSummary` to show managed/unmanaged counts. |
| `status.customDomain` / `conditions[CustomDomainReady]` | With `spec.customDomain`: `{host, phase (Pending, Verified, Routed), records[] {purpose, type, name, value, found}, verifiedTime, certificateReady, certificateNotAfter, message}`. Verification is kept once reached; `records[].found` is the latest lookup. Written only on a change. Does not affect the Lake's `Ready`. See [`lake/custom-domains.md`](lake/custom-domains.md). |
| `status.sealing` | Active `{cluster, generation, kid, publicKey}` recipient plus `previousKeys[] {kid, remaining}` and `unresolvedRecords`. `cluster` is the Lake UID and is part of envelope AAD. `remaining` counts records still requiring an old private key; malformed records retain all old keys until corrected. |

## `LakeE2ETest` (separate CRD, cluster-scoped)

Runs the lake service exit suite as a Job against a cluster-scoped `Lake`. The suite remains in a
dedicated image so its signup and liquid protocol coverage evolves with lake and liquid; the
operator schedules it and reports the Job result. The controller waits until the target reports a
ready replica, ready control-plane Postgres, and an applied schema. It then creates `<name>-e2e` in
the Lake namespace with service-account token mounting disabled. Sample:
[`../config/samples/lake-e2e-test.yaml`](../config/samples/lake-e2e-test.yaml).

| Field | Purpose |
|---|---|
| `spec.targetRef` | Name of the cluster-scoped `Lake`. Required. The Job runs in that Lake's `spec.namespace`, or `lake-dev` when omitted. |
| `spec.image` | Required runner image. Its entrypoint signs up a fresh user, creates a repository, uses the real liquid client to push and pull through the Lake sync routes, and verifies the pulled file bytes. Use an immutable published tag. |
| `spec.pullSecretName` | Optional runner image pull Secret in the Lake namespace. Empty reuses the target Lake's `spec.pullSecretName`. |
| `status.phase` | `Pending \| Running \| Succeeded \| Failed`, written only when the phase, message, or Job name changes. `Pending` creates no Job while the target is absent or not ready. |
| `status.message` / `.jobName` | Current explanation and the retained Job name. Logs are available with `kubectl logs -n <lake-namespace> job/<name>-e2e`. |

The runner receives exactly two values from the controller: `LAKE_E2E_BASE_URL` is
`http://lake-<target>.<namespace>.svc.cluster.local`, and `LAKE_E2E_RUN_ID` is the test resource UID
for collision-free account and repository names. Authentication begins with the suite's public
signup path; the operator does not inject the Lake admin bearer.

## `LakeApp` (separate CRD, namespaced) — one deployed app

A `LakeApp` is one Lake-owned application desired state. It lives in the referenced Lake's
namespace. Synchronized workloads run in their workspace namespace; standalone workloads use a
private namespace. See the operational contract in
[`lake/apps.md`](lake/apps.md).

| Field | Meaning |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`; the `LakeApp` must live in that Lake's `spec.namespace`. |
| `spec.mode` | `runtime` (default) or `image`. Runtime requires `source`; image requires a digest-pinned `image`. |
| `spec.source.repository` | Lake repository identifier loaded by app-runtime. |
| `spec.source.revision` | Explicit pinned repository revision. It becomes `status.observedRevision` after reconciliation. |
| `spec.source.path` | Application directory within the repository; empty means its root. |
| `spec.source.entrypoint` | Optional framework entrypoint override. Empty omits `APP_ENTRYPOINT`, letting app-runtime read `app.toml`. |
| `spec.source.framework` | `fastapi`, `streamlit`, or `static`. |
| `spec.source.tokenSecretRef` | Standalone app's `{name, key}` selecting its repository read token in the Lake namespace. Synchronized apps use their uniquely named generated token Secret in the workspace namespace instead. |
| `spec.sync` | Operator-managed apps-row identity: immutable `id`, `customerId` and `workspaceId`; the operator-written `workspaceNamespace`; desired `generation`; optional `restartGeneration`, the generation of Lake's last `restart_app`, written on the pod template as `orama.cloud/lake-app-restart` so a restart replaces the pods (lake#1228); and optional `tokenHashes` (`source`, nullable `zetta`, `events`, `pull` SHA-256 digests). Requires Lake ownership; token hashes acknowledge successful registration in Postgres before workload startup. Plaintext is never in this field. |
| `spec.image` | Image-mode OCI reference, required to match `name@sha256:<64 hex>`. |
| `spec.pullSecretName` | Image-mode override for the Lake's private-image pull Secret. Synchronized apps merge its Docker auth entries with generated Lake-registry credentials in the workload namespace; standalone apps copy it. |
| `spec.port` | App container port, default `8080`. The Service exposes it on port 80. |
| `spec.replicas` | Desired replicas, default `1`. Zero stops pods while retaining the app resources. |
| `spec.resources` | App-container requests/limits; defaults to `100m`/`128Mi` requests and `1`/`1Gi` limits. |
| `spec.env[]` | Literal `{name, value}` entries. Operator-managed runtime variables take precedence. |
| `spec.secretEnv[]` | `{name, secretKeyRef: {name, key}, lakeSecret?: {mount, name, field}}` entries. Each source Secret is read from the Lake namespace, only selected keys are copied into the isolated app namespace, and the Deployment uses `env[].valueFrom.secretKeyRef`. With `lakeSecret` (lake#625) the controller owns a `LakeSealedSecret` with a lake source that produces `secretKeyRef.name`, so the value comes from lake's secret store and follows its new versions ([apps](lake/apps.md#lake-secret-references-lake625)). Operator-managed names take precedence; secret-backed values take precedence over literal `spec.env`. |
| `spec.secretRefs[]` | One-release legacy input: Lake-namespace whole Secrets copied into the app namespace and exposed through `envFrom`. New apps use `spec.secretEnv`. |
| `spec.host` | Exact hostname. Empty derives `<app>.<Lake spec.apps.wildcard>`. The Ingress serves it over HTTPS: a host one label under the apps wildcard uses the Lake's wildcard certificate, any other its own `Certificate` in the Lake namespace, copied next to the Ingress ([`lake/origin-tls.md`](lake/origin-tls.md)). |
| `spec.auth` | `lake` (default) uses Lake's session authorization through nginx `auth-url`; `public` omits that gate. |
| `spec.bindings.zetta` | `{customerId, scopes}` for scoped read-only workspace access. Synchronized apps use an operator-generated token validated by Lake; standalone apps supply the token through their binding Secret. |
| `spec.bindings.services[]` | Lake service identifiers whose connection documents are delivered. |
| `spec.bindings.serviceNames` | Service-ID-to-filename map for `/var/run/orama/bindings/services/<name>`. The sync resolves same-workspace service names; standalone apps can omit it to retain ID filenames. |
| `spec.bindings.secretRef` | Standalone app's Lake-namespace source Secret. Key `zetta` contains the workspace token; each requested service identifier names its connection. Required for standalone data bindings; synchronized apps use generated tokens and the Lake services Secret. |
| `spec.eventsSecretRef` | Lake-namespace Secret with `url` and `token`; copied to the app and used for best-effort publication to `app-events.<app>`. |
| `spec.stores[]` | Written by the apps sync from lake's `store_mounts` (lake#666); only a synchronized app may carry it (max 32). Each `{id, name, slug, type: volume\|object, provider, claimName?, mountPath?, readOnly?, bucket?, endpoint?, region?, prefix?, secretName?}`. A volume's claim is mounted at `mountPath` (pod `fsGroup` 10001); a bucket's connection is `STORE_<NAME>_BUCKET`, `_ENDPOINT`, `_REGION`, `_PREFIX` and optional `secretKeyRef`s for `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`, `_SESSION_TOKEN`, `_ACCOUNT_KEY`, `_SAS_TOKEN`, read from a copy of the Lake-namespace `secretName` ([stores](lake/stores.md#mounts-lake666)). |
| `status.phase` | `Progressing`, `Ready`, `Degraded`, or `TornDown`. |
| `status.url` | `https://<host>` when ingress is configured. |
| `status.ready` | True only when every desired replica is updated and available at the current revision. |
| `status.observedRevision` | Runtime source revision or image digest observed by the controller. |
| `status.message` | Stable rollout summary or highest-priority pod failure. |
| `status.conditions` | Standard `Ready` condition, deep-compared before status writes. |

## `LakeEnvironment` (separate CRD, namespaced) — one desired deployment

A `LakeEnvironment` is the Kubernetes desired-state record materialized from one deployment in the
operator's Zetta control plane. It lives in the referenced Lake's `spec.namespace` and is owned by the
cluster-scoped `Lake`. The controller gives it a dedicated profiled Namespace, Deployment, Service and
optional Ingress. See [`lake/environments.md`](lake/environments.md) for the event model, lifecycle and
verification. Sample:
[`../config/samples/orama_v1alpha1_lakeenvironment.yaml`](../config/samples/orama_v1alpha1_lakeenvironment.yaml).

| Field | Purpose |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`; the `LakeEnvironment` must live in that Lake's `spec.namespace`. |
| `spec.repository` / `.environment` | Stable Lake repository identifier and human-facing environment name. |
| `spec.kind` | `named` or `preview`. Preview lifecycle also ends when its change is closed. |
| `spec.change` | Immutable liquid change identifier. The DNS-safe short form prefixes a preview hostname. |
| `spec.image` | Required immutable OCI digest reference (`...@sha256:<64 hex>`). Rollback is another deployment event naming an older change and image. |
| `spec.replicas` / `.port` | Deployment replicas (default `1`) and required container port exposed by Service port 80. |
| `spec.env[]` | Literal `{name,value}` workload environment variables: the deployment row's own, then lake's `orama_control.environment_env` values, which win for the same name. |
| `spec.secretRefs[]` | Names or `secret://<name>` references for `LakeSealedSecret` resources in the Lake namespace. Every reference must belong to the same Lake and have delivered its current generation. The produced Secrets are copied into the isolated workload namespace and imported with `envFrom`; unresolved references report `Degraded` and remove the workload. |
| `spec.host` | Exact hostname for a named environment. Preview hosts are derived from the Lake's `previewWildcard`. |
| `spec.domains[]` | Custom domains `{host, tls}` (`tls` `acme` or `provided`), from lake's `orama_control.domains` requests. Each is a rule of the environment's Ingress and, with the Lake's `dnsSecretRef`, a proxied `CloudflareDNSRecord` owned by the environment; removing it removes both. Keyed by `host`. See [the row contract](lake/environments.md#orama_controldomains). Every host of the environment's Ingress (its own and each domain) gets origin TLS: a preview host the preview wildcard's certificate, any other its own `Certificate` from the Lake's `Issuer` ([`lake/origin-tls.md`](lake/origin-tls.md)). |
| `status.phase` | `Progressing \| Ready \| Degraded \| TornDown`, written only on a real transition. |
| `status.url` / `.readyReplicas` | Public URL when ingress is configured, and observed Deployment readiness. |
| `status.deploymentId` | Stable deployment-event identity copied from the `orama.cloud/deployment-id` annotation. |
| `status.domains[]` | Per custom domain: `host`, `status` (`pending \| active \| failed`) and `message`. `active` once its DNS record is Ready; the operator copies it into lake's `domains` rows. |
| `status.conditions[]` | Standard `Ready` condition with transition reason and message. |

## `LakeRestoreRehearsal` (separate CRD, cluster-scoped)

Runs and records a Lake data-service lifecycle rehearsal. The spec is immutable. Restore and
upgrade recover a selected CNPG object-store backup into an isolated scratch namespace, compare
guard-table row counts, the migrations missing from `schema_migrations` and the newest schema migration
with the live database, fail when either side lacks a migration the vendored lake schema records, and delete every
scratch object after success or failure. Upgrade changes the scratch Cluster image before
verification. Failover requests a CNPG switchover on the live replicated control DB and verifies
the dedicated read-only rehearsal role connects through the write Service and that the database
holds every migration the vendored lake schema records. A store restore (`service: store`) binds
the cloud snapshot of a selected operator-taken `VolumeSnapshot` of the Lake's store claim to a new
`Retain` `VolumeSnapshotContent`, restores it into a claim in the scratch namespace, reads every
file read-only, records the verifier's report and deletes everything it made; the live claim,
snapshots and contents are never changed ([`lake/restore-rehearsals.md`](lake/restore-rehearsals.md)).
Verifier pods carry `orama.cloud/rehearsal-lake=<lake>`, as does the scratch namespace, which is
what the Lake's `allow-rehearsal-ingress-to-controldb` sandbox policy admits to the control database.

| Field | Purpose |
|---|---|
| `spec.lakeRef` | Name of the cluster-scoped `Lake`. |
| `spec.service` | `postgres` (the CNPG control database) or `store` (the Lake's store volume, from its CSI snapshots). CEL: the store supports only `restore`. |
| `spec.operation` | `restore \| upgrade \| failover`. |
| `spec.from` | `latest` by default: the newest completed CNPG `Backup` (postgres) or the newest `readyToUse` operator-taken store `VolumeSnapshot` (store). Otherwise the name of one, in the Lake's namespace; a store snapshot the operator did not take of this Lake's store claim is refused. Restore and upgrade pin a Backup's Barman `status.backupId`. |
| `spec.targetImage` | Required for `upgrade`: the CloudNativePG-compatible PostgreSQL operand image pinned by SHA-256 digest and tested on the scratch clone. Restore/upgrade also require a pinned source image before credentials are copied. |
| `status.phase` / `.message` | `Pending \| Running \| Succeeded \| Failed` and the current or terminal explanation. |
| `status.scratchNamespace` / `.selectedBackup` | Deterministic scratch location and the pinned CNPG Backup object for restore and upgrade. |
| `status.selectedSnapshot` | The pinned store `VolumeSnapshot` of a store restore. |
| `status.evidence` | What the verifier reported in its termination message; for a store restore `files=<n> kib=<n> top=<entries>`, or the reason it failed. Kept after the scratch resources are deleted. |
| `status.previousPrimary` / `.targetPrimary` | The primary transition selected by a failover rehearsal. |
| `status.startedAt` / `.completedAt` / `.observedGeneration` | Durable audit timestamps and the represented immutable spec generation. |

`kubectl get lrr` shows Lake, Service, Operation, Phase, and completion time. Detailed procedures:
[`lake/data-services/postgres.md`](lake/data-services/postgres.md).

## `LakeSealedSecret` (separate CRD, namespaced) — one-way Secret delivery

Produces one owned Kubernetes Secret from authored ciphertext or a lake reference. The referenced
cluster-scoped Lake owns the `LakeSealedSecret`; the resource owns the produced Secret. Literal
records open only with the target Lake's private key and require exact AAD for the Lake UID,
resource namespace, and `spec.template.name`. The output is labelled
`lake.orama.cloud/sealed: "true"`. Complete envelope and rotation rules:
[`lake/sealed-secrets.md`](lake/sealed-secrets.md). Sample:
[`../config/samples/lake-sealed-secret.yaml`](../config/samples/lake-sealed-secret.yaml).

| Field | Purpose |
|---|---|
| `spec.lakeRef.name` | Cluster-scoped `Lake` that owns the recipient key and lake service endpoint. |
| `spec.template.name` / `.type` | Produced Secret name and optional Kubernetes Secret type (`Opaque` when omitted). The name is bound into envelope AAD. |
| `spec.data` | Map of Secret keys to standard-base64 canonical-CBOR records. Required only for `literal`. All entries must use one `kid`. |
| `spec.source.type` | `literal \| lake \| dynamic \| delivery`. `delivery` is created by the environment controller. |
| `spec.source.lake` | Static lake reference `{mount, name, version?}`. It contains no value. |
| `spec.source.dynamic` | Dynamic request `{engine, role, ttl}`. It contains no credential. |
| `spec.source.delivery` | Operator-managed `{environment, name}` reference to a LakeEnvironment and original LakeSealedSecret in the Lake namespace. Owns the copied Secret in the workload namespace; contains no ciphertext. See [environment ownership and upgrade order](lake/environments.md#sealed-secrets). |
| `status.phase` | `Pending \| Ready \| Error`. Written only on a real transition. |
| `status.unsealedAt` / `.kid` | Last successful produced-Secret update and the key that opened its record set. |
| `status.lease` | Dynamic lease `{id, expiresAt, renewAt}`; no credential value. |
| `status.mintedAt` / `.version` | For a lake source: the last delivery from lake and the secret version the Secret holds. An unpinned lake source is delivered again every five minutes. |
| `status.conditions[Ready]` | `Delivered` or a stable refusal reason such as `UnsealFailed`, `SecretOwnershipConflict`, or `MintFailed`. |

## `KubernetesCluster` (separate CRD, cluster-scoped) — a cluster the operator provisions

The abstract kind of the infrastructure work ([#351](https://github.com/oramasearch/orama-operator/issues/351),
[#354](https://github.com/oramasearch/orama-operator/issues/354); design in
[`infrastructure/README.md`](infrastructure/README.md)). It says what cluster is wanted in
provider-neutral terms and names a `KubernetesClusterProvider`. That provider's type decides the
concrete kind the operator creates for it: `AksCluster` for type `aks`. The controller never calls
a vendor's API.

**On every cluster.** These kinds ship in `chart/crds` and their controllers run in every
cluster's operator: there is no difference between a management cluster's operator and a leaf's
([`infrastructure/README.md`](infrastructure/README.md#one-operator-on-every-cluster)). A cluster
manages others by holding these resources.

| Field | Meaning |
|---|---|
| `spec.providerRef` | Name of the `KubernetesClusterProvider`. Immutable. |
| `spec.region` | The provider's name for the region, such as `westus2`. Immutable. |
| `spec.kubernetesVersion` | `1.35` or `1.35.8`. |
| `spec.nodePools[]` | `name`, `role` (`system` or `user`), `machineSize` (the provider's word, such as `Standard_D8as_v7`), and either `count` or `minCount` with `maxCount` (which turns on the provider's autoscaling between the two; `count` is then the starting size). Optional `zones`, `labels`, `taints`, `sysctls`. At least one pool must be `system`, with at least one node. |
| `spec.nodePools[].sysctls` | Optional map of Linux kernel names to integer values or string ranges, for example `{vm.max_map_count: 4194304}`. Boolean settings use integer `0` or `1`. The provider validates names, types and values; AKS refuses unsupported settings before provisioning. Only explicitly configured settings are checked for drift. See [node sysctls](infrastructure/node-sysctls.md) for supported settings and rollout limits. |
| `spec.deletionPolicy` | **Required, no default.** `Delete` destroys the real cluster when the resource is deleted; `Retain` leaves it running. Clusters are ephemeral, so whoever writes the resource chooses. |
| `spec.kubeconfigSecretRef` | Where the admin kubeconfig is written (key `kubeconfig`). Default: `<name>-kubeconfig` in the namespace of the provider's credentials Secret. |
| `spec.tags` | Attached to the cluster where the provider supports them. **For an AKS provider a tag name cannot contain `<`, `>`, `%`, `&`, `\`, `?` or `/`**, so a Kubernetes-style name such as `orama.cloud/role` is refused: the cluster is `Degraded` with the tag named, and Azure is never called. At most 50 tags, names up to 512 characters, values up to 256. |
| `status.phase` | `Pending`, `Provisioning`, `Ready`, `Degraded` or `Deleting`, summarized from the concrete resource. `kubectl get kcluster` (the short name `kc` belongs to Strimzi's `KafkaConnect`). |
| `status.concreteRef` | The concrete resource, as `AksCluster/<name>`. |
| `status.endpoint` / `.kubernetesVersion` / `.nodePools[]` | The API server's address, the version running, and each pool's state, count and the provider's `error` for it. |
| `status.message` | Why the cluster is not `Ready`, in the provider's words, copied from the concrete resource. A column of `kubectl get kcluster`. |
| `status.kubeconfigSecretRef` | Set once the kubeconfig Secret holds a credential. |
| `Ready` / `InSync` conditions | Carried across from the concrete resource. |

A cluster whose spec is invalid in a way the schema cannot express (no system pool, `minCount`
above `maxCount`) is `Degraded` with the reason, and nothing is provisioned. A cluster whose
provider does not exist is `Pending` until it does.

Sample: [`config/samples/kubernetescluster-nonprod.yaml`](../config/samples/kubernetescluster-nonprod.yaml).

## `KubernetesClusterProvider` (separate CRD, cluster-scoped) — a vendor, an account, its credentials

Written once per account and shared by every cluster in it. Swapping a provider for a client that
requires a different one means pointing a `KubernetesCluster` at a different provider resource.

| Field | Meaning |
|---|---|
| `spec.type` | `aks`. Immutable. |
| `spec.credentialsSecretRef` | `{name, namespace}` of the Secret the operator authenticates with. For `aks`: keys `tenantID` and `clientID`, plus `clientSecret` for a service principal. **Without `clientSecret` the operator uses workload identity**: it exchanges its projected service-account token for an Azure token, so no long-lived secret exists. |
| `spec.aks.subscriptionID` | The Azure subscription clusters are created in. |
| `spec.aks.resourceGroupPrefix` / `.resourceGroupSuffix` | Each cluster's resource group is `<prefix><cluster name><suffix>`. The suffix defaults to `_group`. |
| `spec.aks.skuTier` | `Free`, `Standard` (default) or `Premium`. |
| `spec.aks.network` | `plugin` (`azure`), `pluginMode` (`overlay`), `dataplane` (`cilium`), `policy` (`cilium`), `podCIDR`, `serviceCIDR`, `dnsServiceIP`. The defaults are the existing cluster's. |
| `spec.aks.upgradeChannel` / `.nodeOSUpgradeChannel` | Defaults `patch` and `NodeImage`. |
| `spec.aks.osDiskSizeGB` | Default 256. |
| `spec.aks.workloadIdentity` | Default `true`: turns on the OIDC issuer and workload identity in each cluster. |
| `status.ready` | True once one cheap call (reading the subscription) succeeded with the credentials. |
| `status.authMethod` | `workload-identity` or `service-principal`, as read from the Secret. |
| `status.clusters` | How many `KubernetesCluster` resources point at this provider. **A provider cannot be deleted while that is above zero**; the delete is held and `status.message` says why. |

## `AksCluster` (separate CRD, cluster-scoped) — one AKS managed cluster

The concrete kind, and the only one whose controller calls Azure. A `KubernetesCluster` creates
and owns it; its spec is complete on its own. It calls the Azure Resource Manager REST API
directly, as the Cloudflare and Clerk controllers call theirs: there is no Azure SDK dependency.

| Field | Meaning |
|---|---|
| `spec.subscriptionID` / `.resourceGroup` / `.location` | Where the cluster lives. Immutable. |
| `spec.clusterName` | The managed cluster's name in Azure. Default: the resource's name. |
| `spec.credentialsSecretRef` | The Azure credentials Secret, as on the provider. |
| `spec.kubernetesVersion`, `.skuTier`, `.network`, `.upgradeChannel`, `.nodeOSUpgradeChannel`, `.workloadIdentity` | As on the provider and the abstract kind. |
| `spec.nodePools[]` | `name`, `mode` (`System` or `User`), `vmSize`, `count`, `enableAutoScaling` with `minCount` and `maxCount`, `osDiskSizeGB`, `availabilityZones`, `nodeLabels`, `nodeTaints`, `sysctls`. Sysctls use the same Linux names and value types as `KubernetesCluster`; they map to Azure `linuxOSConfig.sysctls` on creation. Unsupported settings produce `Degraded` / `InvalidSysctls` and `InSync=False` before any Azure request. |
| `spec.deletionPolicy` | Required. `Delete` deletes the managed cluster, then the resource group **if the operator created it**; `Retain` deletes nothing. |
| `spec.kubeconfigSecretRef` | Where the admin kubeconfig is written. The Secret is owned by the `AksCluster`. |
| `status.phase` / `.provisioningState` / `.powerState` | The operator's phase, and Azure's own words. `kubectl get aks`. |
| `status.resourceID`, `.fqdn`, `.kubernetesVersion`, `.nodeResourceGroup`, `.oidcIssuerURL`, `.nodePools[]` | What Azure reports. |
| `status.operation` | The latest operation Azure ran on the cluster, read from `operations/latest` on every reconcile: `id` (to quote to Azure support), `status` (Azure's progress line while it runs, such as `CreatingAgentPools: 0/1 nodes completed`, then `Succeeded`, `Failed` or `Canceled`), `startTime`, `endTime`, and `error` (`code: message`, followed by the causes Azure nests under it). The `Operation` column of `kubectl get aks`. |
| `status.provisioningError`, `.nodePools[].error` | The provisioning error Azure reports on the cluster and on each pool. |
| `status.message` | Why the cluster is not `Ready`, in Azure's words: the provisioning state, the operation's progress, and every error Azure reports on the operation, the cluster and each pool. The same text is the `Ready` condition's message and is copied to the `KubernetesCluster`. `kubectl get aks -o wide`, `kubectl get kcluster`. A failure is also logged by the operator as an error, once per distinct failure. |
| `status.createdResourceGroup` | True when the operator created the resource group, and so may delete it. |
| `status.kubeconfigWritten` | True once the kubeconfig Secret holds a credential. |
| `InSync` condition | False when the spec asks for something the cluster does not have. **This version reports the difference and does not act on it**: a cluster is not changed after it is created. Configured sysctls report the pool, kernel name and desired/reported values; an omitted Azure value is reported as unknown, without guessing an OS default. `Ready` still describes provisioning. Removing a sysctl from the resource stops managing it and does not reset the live setting. |

What a reconcile does: register the finalizer; ensure the resource group; create the managed
cluster if it is absent; poll every 30 seconds until Azure reports `Succeeded`; fetch the admin
credential once and write the kubeconfig Secret; then re-read the cluster every five minutes.
Status is written only when it changes. On deletion with `Delete`, the resource is held until
Azure reports the cluster gone; if the credentials are missing, it is held and says so, so a
running cluster is never orphaned silently.

## `ClusterHelmRelease` (separate CRD, cluster-scoped) — one chart release on a managed cluster

Short name `chr`. Reconciled wherever it is created; the cluster holding it is managing the cluster it names.
How it fits: [`infrastructure/README.md`](infrastructure/README.md#installing-it-clusterhelmrelease-and-oramaoperator).

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The `KubernetesCluster` to install onto. Immutable. The release waits while the cluster is not `Ready`. |
| `spec.releaseName`, `spec.namespace` | The Helm release and its namespace on that cluster. Immutable. The namespace is created when absent. |
| `spec.chart.repository`, `.name`, `.version` | A chart repository (`https://...`) with the chart's `name`, or an OCI repository (`oci://...`) holding the chart. `version` is required; changing it upgrades the release. |
| `spec.chart.credentialsSecretRef` | For a private chart: a `kubernetes.io/dockerconfigjson` Secret with an entry for the repository's host, or a Secret with `username` and `password`. |
| `spec.values` | The chart's values. |
| `spec.dependsOn[]` | Other `ClusterHelmRelease` names, on the same cluster, that must be `Ready` first. A release others depend on is not uninstalled until they are deleted. |
| `spec.requiredCRDs[]` | CRDs that must exist on the cluster before this chart is installed or upgraded. |
| `spec.crdURLs[]` | `https://` URLs of manifests holding CRDs the chart ships apart from itself. Applied server-side before the chart on every install and upgrade; a manifest holding anything but CustomResourceDefinitions is refused. |
| `status.phase` | `Pending` (waiting for the cluster, a dependency or a CRD), `Installing`, `Ready`, `Degraded` or `Deleting`. |
| `status.chartVersion`, `.appVersion`, `.revision`, `.releaseStatus` | The release on the cluster, in Helm's words. |
| `status.workloads`, `.readyWorkloads` | The release's Deployments, StatefulSets and DaemonSets, and how many are rolled out. |
| `status.message` | Why it is not `Ready`: each workload that is not rolled out with what its pods report, or what the chart repository, Helm or the cluster refused. |

## `OramaOperator` (separate CRD, cluster-scoped) — a managed cluster's own operator, at a version

Short name `ooperator`. Reconciled wherever it is created.

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The `KubernetesCluster` to install onto. Immutable. |
| `spec.version` | The operator version the cluster runs: the image tag, and the chart published with it. Changing it upgrades the cluster's operator, CRDs first. |
| `spec.namespace` | Default `orama-system`. Immutable. |
| `spec.imagePullSecretRef` | A `kubernetes.io/dockerconfigjson` Secret on the management cluster. Copied to the leaf for the image pull, and used to pull the chart. A Secret production uses is never copied to a cluster that is not `production` ([below](#production-credentials-never-reach-a-nonprod-member)). |
| `spec.imagePullSecretAs` | The name of the copy on the leaf, which the leaf's operator pulls with. Default: `imagePullSecretRef`'s name. Lets a cluster get its own credential under the name of the one it replaces, so the old copy is overwritten in place. |
| `spec.imageRepository`, `spec.chartRepository` | Defaults `oramasearch/orama-operator` and `registry-1.docker.io/oramasearch/orama-operator-chart`. |
| `spec.values` | Chart values for the cluster's operator. The image and its pull secret are set by the controller and win; nothing else differs from any other cluster's operator. |
| `status.phase` | `Pending` (waiting for the cluster, or for prerequisites), `Installing`, `Ready`, `Degraded` or `Deleting`. |
| `status.installedVersion`, `.chartVersion`, `.revision`, `.releaseStatus` | The Helm release on the leaf. |
| `status.image`, `.replicas`, `.readyReplicas` | The operator's Deployment on the leaf. `Ready` needs every replica ready on the image asked for. |
| `status.message` | Why it is not `Ready`. When prerequisites are missing it names the CRDs and says nothing was installed; when pods are failing it carries the kubelet's reason; when the pull Secret is refused as a production credential (reason `ProductionCredential`) it names what makes it production. |

## `LakeSet` and `OramaCloudV1Set` (separate CRDs, cluster-scoped) — instances on managed clusters

Short names `lset` and `ocv1set`. One shape for both; how it fits:
[`infrastructure/README.md`](infrastructure/README.md#workloads-on-managed-clusters-lakeset-and-oramacloudv1set).

| Field | Meaning |
|---|---|
| `spec.template` | The instance's spec (a `Lake`'s, an `OramaCloudV1`'s), the default for every member. Validated by each member's cluster when applied. A `LakeSet` template does not set `namespace`; an `OramaCloudV1Set` template does not set `region.name`. |
| `spec.secrets[]` | `name` and `namespace` of a Secret on this cluster, copied into every member's namespace before its instance; `as` renames it there. A production credential is never copied into a member whose cluster is not `production` ([below](#production-credentials-never-reach-a-nonprod-member)). |
| `spec.clusters[]` | The members, 1 to 64. Independent: none waits for another. `clusterRef` (a `KubernetesCluster`), `name` (the instance's name there), optional `namespace` (default `lake-<name>` or `v1-<name>`), optional `spec` (merged over the template; lists replace), optional `secrets` (replace the set's of the same name for this member). The cap of 64 holds for both kinds because they share this shape: the `OramaCloudV1Set` rule that each migration names one member loops over the members, and the API server refuses a CEL rule over an unbounded list. |
| `spec.deletionPolicy` | `Retain` (default): removing a member or deleting the set leaves the instance running. `Delete`: the instance is deleted from its cluster. |
| `spec.migrations[]` | `OramaCloudV1Set` only, at most 256, keyed by `name`: the Prod 1 teams copied into the members ([`cloud-v1-migration.md`](cloud-v1-migration.md#declared-in-the-set-oramacloudv1set)). `name` is the `OramaCloudV1Migration`'s name (DNS label, at most 63). `member` names exactly one `clusters[].name` (CEL: *every migrations[].member must name exactly one clusters[].name*) and cannot change once applied. `secrets` are copied into the member's namespace before it, like a member's, refused when the stack gets a Secret of that name from the set or the member, and deleted once the migration is gone. `spec` is the `OramaCloudV1Migration`'s spec without `targetRef`, which is the member; it is validated by the member's cluster when applied. Removing an entry deletes the migration there; removing its member or deleting the set waits for its migrations to be gone. |
| `spec.e2eTests[]` | `LakeSet` only, at most 16, keyed by `name`: lake's end-to-end suite run against a member, the testing → staging gate of a substantial lake release ([#589](https://github.com/oramasearch/orama-operator/issues/589); [`lake/README.md#service-e2e`](lake/README.md#service-e2e)). Each becomes a cluster-scoped [`LakeE2ETest`](#lakee2etest-separate-crd-cluster-scoped) named `name` (DNS label, at most 52) on the member's cluster, labelled `orama.cloud/managed-by-set=LakeSet.<set>`. `member` names exactly one `clusters[].name` (CEL: *every e2eTests[].member must name exactly one clusters[].name*). `spec` is the `LakeE2ETest`'s spec without `member` and `targetRef`: the set fills in `member` as `{lakeSet: <set>, name: <member>}`, and refuses a `targetRef` or a different `member`. A Secret it names (`LAKE_E2E_CLERK_SECRET_KEY` from `clerk-secret-key`) must be in the member's namespace, which the set's or the member's `secrets` provide; the entry copies none of its own. Applying the entry starts a run; changing its `spec` (`runID`) starts another; removing it deletes the test and its Job there; deleting the set deletes its tests first. |
| `status.phase` | `Ready` (every member `Ready`), `Progressing`, or `Degraded` (the set was refused, or a member failed). |
| `status.purpose` | The `purpose` tag all the members' clusters share. A set whose clusters differ in purpose, or lack one, applies nothing. |
| `status.members`, `.readyMembers` | Counts. Columns of `kubectl get lset`. |
| `status.memberStatuses[]` | Per member: `cluster`, `name`, `namespace`, `phase` (`Pending`, `Progressing`, `Ready`, `Degraded`, or `Deleting` for a removed member waiting for its migrations), `version` (a Lake's image, a v1's image tag) and `message`. |
| `status.versions` | Each member and what it runs, shortened to the image tag (`dev=dev-aa9fefd5, testing=v0.7.0, staging=v0.7.0`); members with no version yet are left out. The `Versions` column of `kubectl get lset` and Lens's list ([#983](https://github.com/oramasearch/orama-operator/issues/983)). |
| `status.migrations[]` | `OramaCloudV1Set` only. Per migration, the declared ones in order and then those still being removed: `name`, `member`, `cluster` and `namespace` (where it was applied), `step`, `phase` and `message` as the member's cluster reports them (`Pending` while the set waits to apply it, `Blocked` when the set refuses to), and `secrets` (the Secrets copied for it). The set's phase does not depend on them. |
| `status.migrationSummary` | `OramaCloudV1Set` only: how many declared migrations succeeded, `1/2 Succeeded`; empty with none. The `Migrations` column of `kubectl get ocv1set`. |
| `spec.restoreRehearsals[]` | `LakeSet` only, at most 16, keyed by `name` (DNS label, at most 63): a restore rehearsal of a member's data into throwaway resources ([#127](https://github.com/oramasearch/orama-operator/issues/127); [`lake/restore-rehearsals.md`](lake/restore-rehearsals.md)). Each becomes a cluster-scoped [`LakeRestoreRehearsal`](#lakerestorerehearsal-separate-crd-cluster-scoped) named `name` on the member's cluster, labelled `orama.cloud/managed-by-set=LakeSet.<set>`. `member` names exactly one `clusters[].name` (CEL: *every restoreRehearsals[].member must name exactly one clusters[].name*). `spec` is the rehearsal's spec without `lakeRef`, which the set fills in as the member's Lake, refusing a different one. A rehearsal's spec is immutable: an entry changed after it was applied is reported with what the cluster has and the message *a rehearsal's spec cannot change*, and is not applied; add an entry under a new name instead. Removing the entry deletes the rehearsal there (its controller deletes any scratch resources); deleting the set deletes its rehearsals first. |
| `status.restoreRehearsals[]` | `LakeSet` only. Per rehearsal, the declared ones in order and then those still being removed: `name`, `member`, `cluster`, and as the member's cluster reports it `phase` (`Pending`, `Running`, `Succeeded`, `Failed`; `Pending` while the set waits; `Blocked` when the set refuses it; `Removing`), `message`, `selected` (the Backup or VolumeSnapshot restored), `evidence`, `completedAt`. The set's phase does not depend on them. |
| `status.e2eTests[]` | `LakeSet` only. Per e2e test, the declared ones in order and then those still being removed: `name`, `member`, `cluster` (where it was applied), and the latest run as the member's cluster reports it: `phase` (`Pending`, `Running`, `Succeeded`, `Failed`; `Pending` while the set waits to apply it or until the cluster starts the run for the latest `spec`, so an earlier run's result is never reported as this one's; `Blocked` when the set refuses it; `Removing`), `message`, `jobName`, `runID`, `lakeRevision`, `pass`, `fail`, `skip`. The failed and skipped steps' reasons stay on the `LakeE2ETest` on the member's cluster. The set's phase does not depend on them. |
| `status.e2eSummary` | `LakeSet` only: each declared test's phase and, once finished, `pass/fail/skip`, e.g. `testing-gate Failed 311/22/3`; empty with none. The `E2E` column of `kubectl get lset`. |
| `status.message` | Which members are not `Ready`, and what each one's cluster says about it; for an `OramaCloudV1Set`, also what the set could not do with a migration, and for a `LakeSet` with an e2e test. |

### Production credentials never reach a nonprod member

[orama-operator#787](https://github.com/oramasearch/orama-operator/issues/787) and
[#789](https://github.com/oramasearch/orama-operator/issues/789) (findings LSR-038 and LSR-040 of the
Lake pre-launch security review), and [#790](https://github.com/oramasearch/orama-operator/issues/790)
(LSR-041: lake-nonprod copied lake-prod's Stripe and OpenAI Secrets). Before copying a Secret into a member (as a set, member or
migration Secret), the operator refuses it, and copies nothing for that member or migration, when:

| The Secret on the managing cluster | Refused for a member whose cluster is |
|---|---|
| is labelled `orama.cloud/purpose: <purpose>` | any other purpose, in either direction |
| is listed by a production set: its `spec.secrets`, a member's `secrets` or a migration's `secrets`, in any `LakeSet` or `OramaCloudV1Set` with a member on a cluster tagged `purpose: production` | not `production` |
| is in the `memberSecrets` of a `LakeOrderSource` whose `lakeSetRef` is a production set: it is copied into every Lake ordered from that set, so it is production before the first order | not `production` |
| is named by the migration's `spec.source.databaseSecretRef`, `masterKeySecretRef` or `settings.credentialsSecretRef`: a Prod 1 credential, which is production whatever the Secret is called | not `production`, unless the Secret is labelled `orama.cloud/purpose` with that cluster's purpose (a fixture standing in for Prod 1) |

What is production comes only from resources: each member's cluster's `purpose` tag, the set files,
the order sources, and the Secret's label. An ordered `LakeSet` is a set like any other: one made from
a production set onto a cluster that is not production gets none of that set's Secrets. A refused member is `Degraded`, with the reason on it; a refused migration is
`Blocked` (or keeps its last phase if it was already applied) and is not updated. Neither is retried
until an edit: give the nonprod set its own Secret, copied under the name its stack mounts with `as`,
so the earlier copy is overwritten in place. A production set is never refused by this rule for a
Secret it shares, so production keeps running while nonprod is split off.
**`OramaOperator` keeps the same rule** for the pull Secret it copies to the cluster it installs onto
([orama-operator#832](https://github.com/oramasearch/orama-operator/issues/832)). The Secret is
refused when it is labelled `orama.cloud/purpose` with another purpose than the cluster's, or, for a
cluster that is not `production` (including one with no `purpose` tag), when a production set lists
it or another `OramaOperator` names it for a cluster tagged `production`. The refused resource is
`Degraded` with reason `ProductionCredential`, nothing reaches its cluster (no pull Secret, no chart
pull, no release change), and it is looked at again every five minutes. Give the cluster its own
Secret in `spec.imagePullSecretRef`, copied under the old name with `spec.imagePullSecretAs`. The
sets count a production `OramaOperator`'s pull Secret as production too.
**`ObservabilityHub` and `ObservabilityAgent` keep the same rule** (operator#15) for the pull Secret
and the alert receivers they copy; a refused one is `Refused` and nothing reaches its cluster. A
production-tier agent's pull Secret and a prod-support hub's pull Secret and receivers count as
production. **`prod-support` is production tier** (operator#456): a Secret production uses, or one
labelled `orama.cloud/purpose: production`, may be copied to a `prod-support` cluster, and never to
`nonprod`. A `LakeSet` or `OramaCloudV1Set` naming a `prod-support` cluster is refused
(`SupportCluster`): it runs no customer or test workload. Nothing else copies a Secret from the
managing cluster to a managed one: a `ClusterHelmRelease`'s
`chart.credentialsSecretRef` and a `KubernetesClusterProvider`'s `credentialsSecretRef` are read on
the managing cluster only, and a `KubernetesCluster`'s kubeconfig is written there.

`TestCommittedSetsAndOperatorsNeverShareASecretAcrossPurposes` checks the files in
`config/clusters/sets/` and `config/clusters/<cluster>/operator.yaml`: no Secret source is named by
sets or `OramaOperator`s of two purposes.

## `LakeOrderSource` (separate CRD, cluster-scoped) — paid lake.live orders

Short name `lorders`. Provisions the paid orders a `LakeSet`'s Lakes record in lake's `cloud_orders`
([operator#634](https://github.com/oramasearch/orama-operator/issues/634), lake#831). Design:
[`infrastructure/orders.md`](infrastructure/orders.md).

| Field | Meaning |
|---|---|
| `metadata.name` | At most 224 characters: `claimed_by` is `orama-operator/LakeOrderSource/<name>` and lake allows 255. A longer name is `Degraded` and claims nothing. |
| `spec.lakeSetRef` | The `LakeSet` whose members' control Postgres hold the orders. Every ordered Lake copies its template and secrets. |
| `spec.hostDomain` | The DNS zone ordered Lakes are served in: `<lake name>.<hostDomain>`. Must be in the template's `dnsSecretRef` zone. At most 234 characters, so the host fits lake's 253. |
| `spec.member` | Every ordered Lake's own settings, as a set member's `spec`. Must not set `host`, `args`, `apexHost`, `region`, `regions`, `namespace` or `LAKE_PUBLIC_HOST`, which the operator sets per order. |
| `spec.memberSecrets[]` | Copied into every ordered Lake's namespace, as a set member's `secrets`. |
| `spec.dedicatedCluster` | `providerRef` (the `KubernetesClusterProvider`: the cloud identity) and `templateClusterRef` (the `KubernetesCluster` whose version, node pools, `ClusterHelmRelease`s and `OramaOperator` a dedicated cluster copies). Without it every `dedicated_cluster` order fails. |
| `spec.pollInterval` | How often the orders are read. Default `30s`. |
| `status.phase` | `Ready`, `Progressing` (orders provisioning), `Pending` (the set or lake's table is missing) or `Degraded` (a member cannot be read, or the spec is refused). |
| `status.provisioning`, `.readyOrders`, `.failed` | This source's orders in each state. Columns of `kubectl get lorders`. |
| `status.members[]` | Per member: `cluster`, `name`, `reachable`, and the `message` when its orders were not read. |
| `status.orders[]` | Each order being provisioned: `id`, `member`, `offer`, `region`, `lakeSet`, and the `message` saying what it waits for. |

Every resource it makes is named `order-<first 12 hex digits of the id>`, labelled
`orama.cloud/order-id` and `orama.cloud/order-source`, and annotated `orama.cloud/order-owner`.
