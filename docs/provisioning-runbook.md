# Provisioning runbook — external dependencies for a working v1 self-host

Walks through provisioning **every external dependency** so the full Orama Cloud v1
stack runs and *works* in-cluster from one `OramaCloudV1` CR — not just boots. Companion
to [`../README.md`](../README.md) (what the operator is) and the bridge's
[`build/images/RUNBOOK.md`](https://github.com/oramasearch/orama-cloud-v1-bridge/blob/main/build/images/RUNBOOK.md)
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

For each, mint the token at the provider, then drop it into the component's Secret. The
Secret name is `v1-<component>` (create-if-absent — the operator never overwrites a Secret
that already exists, so **set these before first reconcile or delete+recreate to update**).

| Service | Component(s) | Env key | Get it from |
|---|---|---|---|
| **OpenAI** | engine, workers, openai-decrypter | `OPENAI_API_KEY` | platform.openai.com → API keys |
| **Groq** | engine, workers | `GROQ_API_KEY` | console.groq.com |
| **Anthropic** | engine, workers | `ANTHROPIC_API_KEY` | console.anthropic.com |
| **Google (Gemini)** | engine | `GOOGLE_API_KEY` | aistudio.google.com → API keys |
| **HuggingFace** | engine (model pulls) | `HF_TOKEN` | huggingface.co → settings → tokens |
| **Apify** | index-builder (crawler) | `APIFY_TOKEN` | console.apify.com → integrations |
| **Ably** | harvest, index-builder (live progress) | `ABLY_API_KEY` | ably.com → app → API keys |
| **AWS S3** | harvest (crawl exports) | `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | IAM user w/ a scoped bucket policy |
| **Firebase** | dashboard, oramasearch-app (auth) | `FIREBASE_SERVICE_ACCOUNT` (+ the app's `NEXT_PUBLIC_FIREBASE_*` at build) | Firebase console → service accounts. **Temporary** — see the auth-provider decision ([operator#19](https://github.com/oramasearch/orama-operator/issues/19)). |
| **Stripe** | dashboard (billing) | `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` | dashboard.stripe.com → API keys / webhooks |
| **Resend** | dashboard (transactional email) | `RESEND_API_KEY` | resend.com → API keys |
| **Slack** | dashboard (ops alerts) | `SLACK_TOKEN` | api.slack.com → your app |
| **Sentry** | dashboard, oramasearch-app, index-builder, workers (error/APM) | `SENTRY_DSN` | sentry.io → project → Client Keys (DSN). **Optional** — an empty DSN disables it. |

Real keys reach a member as Secrets on the managing cluster, copied through its set's `secrets`
list, and the member names them: a provider's key through `spec.llm.providers[].apiKeySecretRef`
([crd-reference.md](crd-reference.md#specllm)), the master key through `spec.masterAPIKeySecretRef`
([crd-reference.md](crd-reference.md#specmasterapikeysecretref)). The operator watches every Secret
a member reads and rolls its consumers when a value changes; nothing is restarted, patched or
exec'd by hand.

> **QStash / Upstash:** the operator points `QSTASH_URL` at the **in-cluster RabbitMQ**
> (`v1-broker:5672`) as a shim, so no Upstash account is needed for the sandbox. Supply a
> real `QSTASH_TOKEN` only if you switch back to hosted QStash.

---

## Step 3 — In-cluster / operator-generated `[GEN]` (usually nothing to do)

The operator generates these and points components at in-cluster service DNS. Override only
for a prod-grade deployment.

- **Generated secrets:** the RSA **JWT keypair** (dashboard signs, engine verifies), the broker
  admin password (`<cr>-broker-admin`), the Redis password (`<cr>-cache-auth`) and the analytics key
  the readers and analytics-processor share (`<cr>-analytics-api-key`), generated once with random
  values and read by every client by reference
  ([components.md](components.md#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)).
  `DISCO_SECRET`, `HARVEST_SIGN` (job signing, shared master↔worker↔index-builder),
  `DECRYPTER_API_SECRET_BASE64` and the ClickHouse/Typesense passwords are still seeded `dev-*`
  placeholders.
- **Supplied, never generated:** `MASTER_API_KEY`, through `spec.masterAPIKeySecretRef`. It must
  equal the target control database's `oramacore_clusters.master_api_key`; without a real one the
  engine is held at zero and `Ready` names the cause.
  > **✅ Resolved (operator#32):** the **Postgres** password is generated by CloudNativePG (in
  > `<cr>-controldb-app`); DB clients now source it from there — analytics-processor (`PG_PASSWORD`),
  > the dashboard (`POSTGRES_PASSWORD` ← `.password`), and index-builder (`DATABASE_URL` ← `.uri`).
  > **Redis** runs with `--requirepass` from `<cr>-cache-auth`, and harvest-master and index-builder
  > connect with the same password ([#307](https://github.com/oramasearch/orama-operator/issues/307)).
- **In-cluster endpoints** (no external service): `ORAMACORE_WRITER_URL`/`READER_URL`,
  `DISCO_URL`, `EMBEDDINGS_URL` (→ ai-server `:50051`), `HARVEST_MASTER_ENDPOINT` (→ the
  harvest-master service), `DECRYPTER_BASE_URL` (→ the `openai-decrypter` worker),
  `REDIS_URL` (→ cache), `AIRBYTE_URL` (→ the in-cluster Airbyte Helm release).

---

## Dependency matrix

Per component: its external deps and the tier. `[CF]` operator-provisions on Cloudflare ·
`[EXT]` you supply a token · `[GEN]` operator generates / in-cluster.

| Component | Dependencies |
|---|---|
| **engine** (OramaCore) | `[EXT]` OpenAI, Groq, Anthropic, HuggingFace, MASTER_API_KEY · `[GEN]` JWT-pub, broker admin, analytics key (readers) |
| **workers** (RAG bundle) | `[EXT]` OpenAI, Groq, Anthropic, Sentry(opt), MASTER_API_KEY · `[GEN]` ClickHouse, DISCO_SECRET, QStash→broker |
| **dashboard** | `[EXT]` Firebase, Stripe, Resend, Slack · `[GEN]` Postgres/ClickHouse/Redis, JWT, Airbyte(in-cluster) |
| **oramasearch-app** | `[EXT]` Firebase · `[GEN]` DB/JWT |
| **index-builder** | `[CF]` KV namespaces, CF token/account · `[EXT]` Apify, Ably, MASTER_API_KEY · `[GEN]` Typesense, embeddings-bearer, decrypter, HARVEST_MASTER_ENDPOINT+SIGN, DISCO_SECRET_KEY, Redis |
| **harvest** (master+worker) | `[EXT]` AWS S3, Ably · `[GEN]` HARVEST_SIGN, Mongo(in-cluster), RabbitMQ |
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
   index-builder `DATABASE_URL`, analytics-processor `PG_PASSWORD`); the broker and Redis
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
   live progress needs a real Ably key (or an in-cluster replacement,
   [operator#28](https://github.com/oramasearch/orama-operator/issues/28)); auth needs
   the Firebase replacement ([operator#19](https://github.com/oramasearch/orama-operator/issues/19)).
