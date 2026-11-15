# Provisioning runbook — external dependencies for a working v1 self-host

Walks through provisioning **every external dependency** so the full Orama Cloud v1
stack runs and *works* in-cluster from one `OramaCloudV1` CR — not just boots. Companion
to [`../README.md`](../README.md) (what the operator is) and the bridge's
`build/images/RUNBOOK.md`
(build → push → pull-secret → deploy). Read this once top-to-bottom, then use the
[dependency matrix](#dependency-matrix) as the checklist.

> The operator seeds **dummy dev values** for every secret so the stack *boots* without
> any of this. This runbook is how you replace the dummies with real credentials so each
> component actually *functions* (crawls, embeds, bills, emails, answers).

## The three tiers

| Tier | Who provisions it | Action needed |
|---|---|---|
| **[CF] Cloudflare** | The **operator**, using its API token (`spec.cloudflare.apiTokenSecretRef`) | Give the operator a scoped token once → it creates DNS, load-balancers, worker routes, **KV namespaces**, R2, and injects the resulting IDs into component config. |
| **[GEN] In-cluster / generated** | The **operator** (generates secrets / points at in-cluster service DNS) | **None** by default. Override only if you want real values (e.g. a real ClickHouse password). |
| **[EXT] External SaaS** | **You** — sign up / mint a token, hand it to the operator as a Secret | One step per service (below). These are the only things the operator genuinely cannot create for you. |

## Prerequisites

- The cluster + `kubectl`/`helm` context, the private image pull secret
  (`oramasearch-dockerhub`) in the CR namespace, and the operator running (see the build
  RUNBOOK). All commands below assume namespace `orama-system` and CR name `v1`.

---

## Step 1 — Cloudflare API token (the linchpin)

The operator provisions **everything Cloudflare** from one token, so this unblocks the
whole `[CF]` tier at once.

**1a. Mint a scoped token** at Cloudflare → My Profile → API Tokens → Create Token
(Custom). Grant, for the Orama account + zone(s):

| Permission | Why |
|---|---|
| Zone → DNS → Edit | DNS records for each app host (`manageDNS`) |
| Account → Workers KV Storage → Edit | **KV namespaces** for index-builder (dumps + api-keys) |
| Account → Workers R2 Storage → Edit | R2 buckets (legacy indexes) |
| Account → Workers Scripts → Edit | worker routes (`manageWorkerRoutes`) |
| Account → Load Balancing → Edit + Zone → Load Balancers → Edit | geo load-balancer (`manageLoadBalancers`) |
| Zone → Health Checks → Edit | LB origin health checks |

**1b. Store it as a Secret and wire it into the CR:**

**Two Secrets** — the control-plane API token, and the R2 (S3-compatible) data-plane keys.
Nothing Cloudflare-related lives in the CR; it just points at the two Secrets.

```bash
# 1) Control plane — DNS / Load Balancing / Workers KV / R2 bucket provisioning
kubectl create secret generic cloudflare-api-token -n orama-system \
  --from-literal=token='<CF_API_TOKEN>' \
  --from-literal=zoneID='<zone-id>' \
  --from-literal=accountID='<account-id>'

# 2) Data plane — R2 object read/write via the S3-compatible API (distinct credential type)
kubectl create secret generic cloudflare-r2 -n orama-system \
  --from-literal=s3ApiEndpoint='https://<account-id>.r2.cloudflarestorage.com' \
  --from-literal=accessKeyId='<r2-access-key-id>' \
  --from-literal=secretAccessKey='<r2-secret-access-key>'
```
```yaml
# in the OramaCloudV1 CR — point at both Secrets
spec:
  cloudflare:
    enabled: true
    apiTokenSecretRef: { name: cloudflare-api-token }   # token + zoneID + accountID (control plane)
    r2SecretRef:       { name: cloudflare-r2 }          # s3ApiEndpoint + accessKeyId + secretAccessKey (R2 data plane)
    manageDNS: true
    manageLoadBalancers: true
    manageWorkerRoutes: true
```

**1c. What the operator does with it:** it models each external Cloudflare resource as an
**owned CRD** — `CloudflareDNSRecord`, `CloudflareKVNamespace`, `CloudflareR2Bucket` — each
with a finalizer that removes the real resource from Cloudflare when the CR is deleted. It
reconciles the dashboard **DNS** record and, when ingestion is enabled, **provisions the KV
namespaces** (`<cr>-dumps`, `<cr>-api-keys`) + the legacy-indexes **R2 bucket**, then injects
the KV IDs into index-builder's config (`CLOUDFLARE_DUMPS_KV_NAMESPACE_ID`,
`CLOUDFLARE_API_KEYS_KV_NAMESPACE_ID`) from the KV CRs' `.status` — so you never copy a
namespace ID by hand. Inspect:
`kubectl get cloudflarednsrecord,cloudflarekvnamespace,cloudflarer2bucket -n orama-system`.

> Geo load-balancer + worker-route CRDs are still to come
> ([operator#31](https://github.com/oramasearch/orama-operator/issues/31)); until
> then `manageLoadBalancers`/`manageWorkerRoutes` are inline no-ops.

---

## Step 2 — External SaaS (the only manual tokens)

For each, mint the token at the provider and put it in a Secret on the managing cluster. Nothing
is created or edited by hand on the stack's own cluster: the `OramaCloudV1Set` copies each Secret
in its `secrets` list into the member's namespace and keeps it in step, and the stack's spec
references it (for the LLM providers, `spec.llm.providers[].apiKeySecretRef`). Creating a Secret
on the managing cluster is a cluster write and needs the cluster owner's consent.

| Service | Component(s) | Env key | Get it from |
|---|---|---|---|
| **OpenAI** | engine, workers, openai-decrypter | `OPENAI_API_KEY` | platform.openai.com → API keys |
| **Groq** | engine, workers | `GROQ_API_KEY` | console.groq.com |
| **Anthropic** | engine, workers | `ANTHROPIC_API_KEY` | console.anthropic.com |
| **Google (Gemini)** | engine | `GOOGLE_API_KEY` | aistudio.google.com → API keys |
| **HuggingFace** | engine (model pulls) | `HF_TOKEN` | huggingface.co → settings → tokens |
| **Apify** | index-builder (crawler) | `APIFY_TOKEN` | console.apify.com → integrations |
| **Ably** | index-builder (deployment progress); HARVEST worker wiring remains #12 | `ABLY_API_KEY` | `components.ingestion.ablyKeySecretRef`; [decision and limits](#ably-deployment-progress-747) |
| **AWS S3** | harvest (crawl exports) | `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | IAM user w/ a scoped bucket policy |
| **Firebase** | dashboard, oramasearch-app (auth) | `FIREBASE_SERVICE_ACCOUNT` (+ the app's `NEXT_PUBLIC_FIREBASE_*` at build) | Firebase console → service accounts. **Temporary** — see the auth-provider decision ([operator#19](https://github.com/oramasearch/orama-operator/issues/19)). |
| **Stripe** | dashboard (billing) | `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` | dashboard.stripe.com → API keys / webhooks |
| **Resend** | dashboard (transactional email) | `RESEND_API_KEY` | resend.com → API keys |
| **Slack** | dashboard (ops alerts) | `SLACK_TOKEN` | api.slack.com → your app |
| **Sentry** | dashboard, oramasearch-app, index-builder, workers (error/APM) | `SENTRY_DSN` | sentry.io → project → Client Keys (DSN). **Optional** — an empty DSN disables it. |

Example: the engine's LLM keys. One Secret on the managing cluster, `orama-system/v1-nonprod-llm-keys`
for `v1-nonprod` (`v1-prod-llm-keys` for `v1-prod`), holds `OPENAI_API_KEY`, `GOOGLE_API_KEY`,
`GROQ_API_KEY` and `ANTHROPIC_API_KEY`. The set file lists it and every provider points at it:

```yaml
spec:
  secrets:
  - name: v1-nonprod-llm-keys
    namespace: orama-system
  template:
    llm:
      providers:
      - name: openai
        models: [gpt-5.2]
        apiKeySecretRef: { name: v1-nonprod-llm-keys }
      # google, groq and anthropic the same way
```

To rotate a key, change the value in the managing cluster's Secret. The set copies it to the
member, the operator watches the engine Secret and every `apiKeySecretRef` Secret, and it rolls the
writer and readers when a value changes; no restart is needed
([crd-reference.md](crd-reference.md#specllm)).

The master key arrives the same way, through `spec.masterAPIKeySecretRef`
([crd-reference.md](crd-reference.md#specmasterapikeysecretref)). The operator watches every Secret
a member reads and rolls its consumers when a value changes; nothing is restarted, patched or
exec'd by hand.

### Ably deployment progress (#747)

Ably is optional. A stack without `components.ingestion.ablyKeySecretRef` keeps its placeholder,
and index-builder's deployment progress is not published.

The operator supports a future `components.ingestion.ablyKeySecretRef: {name, key}` for
index-builder's deployment progress. It supplies a required explicit `ABLY_API_KEY` environment
entry that wins over an older component Secret's `envFrom`; new component Secrets omit the Ably
seed when referenced. The operator validates a nonempty value before writing the builder, watches
the Secret and hashes its data to roll the builder on rotation. It never writes the source Secret.
Unset preserves the historical sandbox seed.

Future activation needs a usable source credential, a declared set Secret
mirror and ingestion reference, and the operator with the new CRDs deployed through
[the CRD rollout runbook](operations/crd-rollout.md). The coordinator's earlier validation of the
recovered Prod 1 key returned HTTP 403 / Ably code 40300, `Account disabled`; that candidate is not
usable. If shared Prod 1 credentials are reconsidered, a separate Kubernetes Secret name would
provide no isolation of the Ably account, channels, permissions, quota or billing. No credential-copy
guard is relaxed by this optional capability.

This covers the builder only. At the current operator base, HARVEST worker spawning is still
unimplemented (#12); there is no dynamically rendered worker Job to wire or validate. HARVEST's
source master forwards worker environment through its Docker-compatible launcher, but the operator
configures no pool. Its separate worker Ably seed remains unchanged until that real consumer is
implemented. The pinned nonprod dashboard source (`56f355da`) has no legacy Ably token handler, so
the change adds no unused dashboard or master key and keeps dashboard `envFrom` unchanged.

The reference does not prove live Ably access or a complete index build. Builder/dashboard callback
compatibility, internal authentication and an owned fixture still gate #883's build, persistence
and readback proof. Apify and harvest-master's GitHub-token decisions remain open under #747.

> **QStash / Upstash:** the operator points `QSTASH_URL` at the **in-cluster RabbitMQ**
> (`v1-broker:5672`) as a shim, so no Upstash account is needed for the sandbox. Supply a
> real `QSTASH_TOKEN` only if you switch back to hosted QStash.

---

## Step 3 — In-cluster / operator-generated `[GEN]` (usually nothing to do)

The operator generates these and points components at in-cluster service DNS.

- **Generated secrets:** the RSA **JWT keypair** (dashboard signs, engine verifies), the broker
  admin password (`<cr>-broker-admin`), the Redis password (`<cr>-cache-auth`), the analytics key
  the readers and analytics-processor share (`<cr>-analytics-api-key`), HARVEST's control key
  (`<cr>-harvest-sign`: index-builder's `HARVEST_SIGN`, harvest-master's `HARVEST_SIGNATURE`,
  dashboard's `SCRAPER_SIGNATURE`) and
  harvest-master's notification signing key (`<cr>-harvest-notification`), generated once with
  random values and read by every client by reference
  ([components.md](components.md#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)).
  ClickHouse, Typesense, cron and decrypter credentials, plus index-builder's internal API keys,
  are also generated once and shared through explicit Secret references. `DISCO_SECRET` and
  `QSTASH_TOKEN` remain unresolved; neither is generated by this change.
- **Generated unless supplied:** `MASTER_API_KEY`. With no `spec.masterAPIKeySecretRef` the operator
  generates `<cr>-master-api-key` once (#615), which suits a fresh stack. A stack carrying copied data
  names a supplied Secret instead (production: Prod 1's key), since the key must equal the control
  database's `oramacore_clusters.master_api_key`; a named Secret that is missing or not real holds
  the engine at zero and `Ready` names the cause.
- **The `oramacore_clusters` row:** cluster rows are never copied from Prod 1. On an in-cluster
  control DB with no row, the operator inserts the stack's only row once both public hosts are set,
  all three schemas apply and the master key is valid: prefix `c1`, `writer_url`
  `https://<engine.writer.host>`, `reader_url` `https://<engine.reader.host>` and the master key.
  Without both hosts it waits (`ClusterRowReady=False`, `WaitingForPublicHosts`) rather than record
  an in-cluster URL. It inserts nothing when the table already has any row, for an external control
  DB or a read region, or without the engine. The row is written once and never updated, so:
  - **Rotating the master key does not update the row.** The row keeps the key it was inserted with.
    After changing the Secret behind `spec.masterAPIKeySecretRef`, the row's `master_api_key` still
    holds the old value; updating it is a database write that needs the cluster owner's consent, and the
    dashboard and engine must agree on the key.
  - **`ClusterRowReady=True` reports the Job, not the table.** A succeeded Job is never re-run, so
    after a restore that empties `oramacore_clusters` it still shows `True` with no row. Read the
    table to know.

  A failed insert is retried by the operator. See
  [components.md](components.md#controldb-internalcontrollercontroldbgo).
  > **✅ Resolved (operator#32):** the **Postgres** password is generated by CloudNativePG (in
  > `<cr>-controldb-app`); DB clients now source it from there — analytics-processor (`PG_URI` ← `.uri`),
  > the dashboard (`POSTGRES_PASSWORD` ← `.password`), and index-builder (`DATABASE_URL` ← `.uri`).
  > **Redis** runs with `--requirepass` from `<cr>-cache-auth`, and harvest-master and index-builder
  > connect with the same password ([#307](https://github.com/oramasearch/orama-operator/issues/307)).
- **In-cluster endpoints** (no external service): `ORAMACORE_WRITER_URL`/`READER_URL`,
  `DISCO_URL`, `EMBEDDINGS_URL` (→ ai-server `:50051`), `HARVEST_MASTER_ENDPOINT` (→ the
  harvest-master service), `DECRYPTER_BASE_URL` (→ the `openai-decrypter` worker),
  `REDIS_URL` (→ cache).

---

## Dependency matrix

Per component: its external deps and the tier. `[CF]` operator-provisions on Cloudflare ·
`[EXT]` you supply a token · `[GEN]` operator generates / in-cluster.

| Component | Dependencies |
|---|---|
| **engine** (OramaCore) | `[EXT]` OpenAI, Groq, Anthropic, HuggingFace, MASTER_API_KEY · `[GEN]` JWT-pub, broker admin, analytics key (readers) |
| **workers** (RAG bundle) | `[EXT]` OpenAI, Groq, Anthropic, Sentry(opt), MASTER_API_KEY · `[GEN]` ClickHouse, DISCO_SECRET, QStash→broker |
| **dashboard** | `[EXT]` Firebase, Stripe, Resend, Slack · `[GEN]` Postgres/ClickHouse/Redis, JWT |
| **oramasearch-app** | `[EXT]` Firebase · `[GEN]` DB/JWT |
| **index-builder** | `[CF]` KV namespaces, CF token/account · `[EXT]` Apify, Ably, MASTER_API_KEY · `[GEN]` Typesense, embeddings-bearer, decrypter, HARVEST_MASTER_ENDPOINT+SIGN, DISCO_SECRET_KEY, Redis |
| **harvest** (master+worker) | `[EXT]` AWS S3, Ably · `[GEN]` HARVEST_SIGNATURE (= index-builder's HARVEST_SIGN), NOTIFICATION_SIGNATURE, Mongo(in-cluster), RabbitMQ · no Podman API yet ([#12](https://github.com/oramasearch/orama-operator/issues/12)) |
| **disco** | `[GEN]` DISCO_SECRET, ClickHouse |
| **analytics-processor** | `[GEN]` analytics key (`<cr>-analytics-api-key`), ClickHouse |
| **search** (Typesense) | `[GEN]` Typesense key |
| **search-workers** (index-shared, large-index, openai-decrypter, telemetry) | `[EXT]` OpenAI (decrypter) · `[GEN]` decrypter secret, ClickHouse (telemetry) |

---

## Verification

- **Cloudflare:** `kubectl logs deploy/orama-operator -n orama-system | grep -i cloudflare` shows DNS/LB/KV reconcile; the index-builder pod should have non-empty `CLOUDFLARE_*_KV_NAMESPACE_ID`.
- **Per component:** `kubectl get pods -n orama-system` all `Running`/ready; then exercise the path — dashboard login (Firebase), a crawl (Apify→harvest→index-builder→Typesense), a RAG query (engine+workers+LLM).
- **A dummy still in place** surfaces as an auth/401 or connection error in that component's logs — grep for the provider name.

---

## Operator work still needed to make every service *fully* work

Every component now **boots healthy** (verified 21/21 in-cluster). ✅ Done since the first
draft: harvest-**master** is the reconciled service, Cloudflare KV/R2 are provisioned as
owned CRDs with ID-injection, and index-builder's full env is wired. These remaining items
make specific *paths* function (tracked as issues):

1. **harvest worker-spawning** — the master runs, but it spawns crawl workers via **Podman**
   (`WORKER_IMAGE`); that needs a port to **k8s Jobs** to actually crawl in-cluster
   ([operator#12](https://github.com/oramasearch/orama-operator/issues/12)).
2. **✅ Datastore credentials (operator#32, resolved)** — Postgres clients now source the
   CNPG-generated password/URI from `<cr>-controldb-app` (dashboard `POSTGRES_PASSWORD`,
   index-builder `DATABASE_URL`, analytics-processor `PG_URI`); the broker and Redis
   passwords are generated into `<cr>-broker-admin` and `<cr>-cache-auth` and every client reads
   them by reference (#307). Audited the rest: ClickHouse/Typesense honor their password env (the
   images read it) and Mongo is passwordless both sides — all consistent. _(The unmounted
   index-builder REST-API JWT key is separate; see the provisioning steps above.)_
3. **Cloudflare LB + Worker-Route CRDs** — DNS/KV/R2 are done as owned CRDs; load-balancer +
   worker-route CRDs are the remaining epic items
   ([operator#31](https://github.com/oramasearch/orama-operator/issues/31)).
4. **Teardown-on-disable** — delete a component's children when its toggle flips off (today
   owner-GC only fires on CR deletion)
   ([operator#23](https://github.com/oramasearch/orama-operator/issues/23)).
5. **Real external functionality** — crawling needs a real Apify token (or the HARVEST
   consolidation, [operator#29](https://github.com/oramasearch/orama-operator/issues/29));
   live progress keeps its placeholder by decision and remains deferred
   ([current decision and remaining blockers](#ably-deployment-progress-747)); auth needs
   the Firebase replacement ([operator#19](https://github.com/oramasearch/orama-operator/issues/19)).
