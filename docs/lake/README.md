# The `Lake` CR

What the operator does for a `Lake`, how it is deployed, and how to run it. Field reference:
[`../crd-reference.md`](../crd-reference.md) (`Lake` section). lake's own docs (server, schema,
deployment state): https://github.com/oramasearch/lake/tree/main/docs.

## At a glance

The cluster-scoped `Lake` CR deploys the lake service with its control-plane Postgres, an owned
Zetta data plane, and a shared persistent libliquid store. The operator creates a `100Gi` RWX store claim by default or
mounts `spec.store.existingClaim`, sets `LAKE_STORE_ROOT=/var/lib/lake/store`, and accepts
additional lake runtime configuration through `spec.env`. The workload has resource bounds,
Lake-native health probes, non-root execution, RWO-safe replacement, and replicated-pod
disruption and placement controls. Each Lake also owns an X25519 sealing
key and publishes its recipient in status; namespaced `LakeSealedSecret` resources turn
cluster-bound ciphertext into owned, labelled Kubernetes Secrets without putting plaintext in the
authored resource ([sealed-secrets.md](sealed-secrets.md)). `kubectl get lake` reports managed and
unmanaged data-service counts, and operation-level status never implies untested protection;
`LakeRestoreRehearsal` automates Postgres restore, scratch upgrade and replicated failover proof
([data-services/README.md](data-services/README.md),
[data-services/postgres.md](data-services/postgres.md)).

Desired deployment events in the operator's Zetta control plane become Lake-owned
`LakeEnvironment` CRs: one restricted, quota-bound `untrusted` runtime namespace per named or
per-change environment, with anti-flap status returned to Zetta, wildcard DNS for previews, and
delivered `LakeSealedSecret` values copied into the isolated workload namespace before startup
([environments.md](environments.md)). Liquid reaches that intake through the operator's
authenticated REST `/v1` and MCP deployment surface; the contract, credentials, rollback, audit
and connection details are in the [deployment control-plane guide](../control-plane/README.md).

Namespaced `LakeApp` resources run customer apps from a pinned Lake repository revision or a
digest-pinned image. Synchronized apps share their restricted workspace namespace with
collision-free resource names; standalone CRs retain a private namespace. Apps have
private-by-default ingress, scoped data bindings, and live rollout events. The CR contract and resource lifecycle are in
[`apps.md`](apps.md). On every Lake, a dedicated controller follows lake's `apps`
table using the Services DSN, `lake_apps` notifications and a 60-second fallback list. It registers
generated source, Zetta, event and registry-pull token hashes, merges the pull token with the Lake's
existing Docker config for each app pod, returns current rollout status, handles stopped/deleted rows
and orphans, and owns one proxied apps wildcard DNS record. Builds remain separate in #195.

On every Lake, lake writes a queued image-build row and the operator runs one isolated
rootless BuildKit Job for it in a per-space namespace. Source, push, and event credentials are
single-build scoped; the Job pushes through Lake's in-cluster registry, reuses a per-space registry
cache, and reports its digest, size, progress, and failures. See [`builds.md`](builds.md).

On every Lake, the operator follows lake's `workspaces` table and creates one
restricted namespace and Zetta data plane per workspace. An owned `NetworkSandbox` renders
default-deny network policy, restricted
Pod Security Admission, tokenless service accounts, plan quotas and local credentials make the
namespace the trust boundary; see [`workspaces.md`](workspaces.md). Narrow per-kind policies let
the datastore operators reach their pods there, and let CNPG, Strimzi's entity operator and TiDB
discovery reach the Kubernetes API
([workspaces.md](workspaces.md#datastore-operator-flows-operator268), operator#268). Each workspace Zetta's
volumes grow in place from lake's `workspaces.storage_gi` and never shrink; the operator reports
the observed size on the workspace row ([workspaces.md](workspaces.md#workspace-volume-size-operator259),
operator#259).

On every Lake, the operator follows lake's `volume_snapshot_policies` and takes
scheduled CSI snapshots of lake's store volume and of every workspace Zetta's volumes through a
`VolumeSnapshotClass` with `deletionPolicy: Retain`, keeps the newest `keep` of each, honors
one-off requests and reports each target's newest snapshot in `volume_snapshot_status`.
Snapshots outlive their workspace, and restoring one is a documented runbook, not automated
([volume-snapshots.md](volume-snapshots.md), operator#260).

On every Lake, lake runs managed services for its customers — Zetta, Postgres,
the catalog of Kafka, ClickHouse, TiDB, Cassandra and Valkey (preview), and SQLite, DuckDB and
Polars: it writes the
`services` table in its control Postgres, and the operator creates one Lake-owned
`OramaZettaCluster`, CloudNativePG `Cluster`, upstream operator's resource (Strimzi, Altinity, TiDB
Operator, K8ssandra, valkey-operator), or StatefulSet of [lake-sqlite](lake-sqlite.md), [lake-duckdb](lake-duckdb.md) or
[lake-polars](lake-polars.md) per service, writes `service_status`, and hands lake each connection
through a mounted Secret ([services.md](services.md)). The same poll runs a customer's **stores**
(lake#196, lake#664): cluster volumes (one Lake-owned PersistentVolumeClaim each), the customer's
own buckets on R2, S3, B2, GCS and Azure (one `LakeSealedSecret` delivering the workspace's
credential each; the bucket itself is never touched) and Lake-run Zetta and Lake stores, each
connection in the same Secret. Services and apps mount them (lake#666): a volume at a path in the
pods the operator builds (apps and the sqlite, duckdb and polars services), a bucket as
`STORE_<NAME>_*` variables, and the operator reports each volume's used space —
[stores.md](stores.md).

## Clerk sign-in

With `spec.clerk` (a Clerk instance's secret key), the operator keeps that instance configured for
lake through Clerk's Backend API, writes a production instance's DNS records, and gives the Lake
`LAKE_CLERK_PUBLISHABLE_KEY` and `LAKE_CLERK_WEBHOOK_SECRET`. Two Clerk applications serve the four
environments. See [`clerk.md`](clerk.md) (operator#541), which also lists the one-time dashboard
steps and the Secrets to create.

## Every Lake runs every feature

Steve, 2026-10-17: *"all features should be enabled on all environments"* ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)).
A `Lake` has **no feature switch**: apps, builds, methodology runs and deployments, the whole
Agent Library and user-created agents, managed services and stores, workspaces, connections,
egress rules, tunnels, peers and volume snapshots are reconciled on every Lake, dev to prod. The
`enabled` fields of `spec.apps`, `spec.builds`, `spec.services`, `spec.workspaces` and
`spec.snapshots` were removed; the CRD prunes the values Lakes had stored.

Every image a feature runs has an operator default, so a `LakeSet` member states only what differs
per environment: host, args, env and secrets. A spec image still overrides its default, and moving
a default is a change to one file, rolled through the pipeline dev, testing, staging, prod.

The one prerequisite is the control Postgres (`spec.postgres.enabled`, default `true`). Without it
the features cannot run, and the Lake's `FeaturesReady` condition is `False` with reason
`PostgresDisabled`, naming them; a feature is never skipped silently. Deployments also need the
Lake's Zetta (`spec.zetta.enabled`, default `true`; otherwise reason `ZettaDisabled`) and a public
host (`spec.host` or `LAKE_PUBLIC_HOST` in `spec.env`; otherwise reason `NoPublicHost`), see
[environments](environments.md#every-lakes-control-plane-is-its-own-zetta). `ingressEnabled` stays: it
decides whether the Lake is exposed outside the cluster, not which features it runs.

### Default images

`internal/controller/lake_defaults.go`, pinned to what the managing cluster's `Lake/prod` ran on
2026-10-17:

| What | Spec field that overrides it | Default |
|---|---|---|
| Zetta: the Lake's data plane | `spec.zetta.image` | `oramasearch/zetta:main-4015ef76` |
| Zetta: every workspace and managed Zetta service | `spec.services.zettaImage`, then `spec.workspaces.image` | `oramasearch/zetta:main-4015ef76` |
| Runtime-mode apps (`LAKE_APPS_RUNTIME_IMAGE`) | `spec.apps.runtimeImage` | `oramasearch/app-runtime:976e6a8` |
| Image builds | `spec.builds.builderImage` | `oramasearch/lake-builder:31a48d9` |
| Methodologies (`LAKE_METHODOLOGY_RUNTIME_IMAGE`) | `spec.methodologies.runtimeImage` | `oramasearch/methodology-runtime:ac58062` |
| User-created agents | `spec.agents.runtimeImage` | `oramasearch/agent-runtime:f8feb14` |
| MCP servers (`LAKE_MCP_RUNNER_IMAGE`) | `LAKE_MCP_RUNNER_IMAGE` in `spec.env` | `oramasearch/lake-mcp-runner:d98451a` |
| `agent-ask-orama` | `spec.agents.library[key]` | `oramasearch/agent-ask-orama:591e662` |
| `agent-project` | `spec.agents.library[key]` | `oramasearch/agent-project:16e17d0` |
| `agent-autosuggest` | `spec.agents.library[key]` | `oramasearch/agent-autosuggest:3d9acf7` |
| `agent-creator` | `spec.agents.library[key]` | `oramasearch/agent-creator:6b1cc36-f8feb14` |
| `agent-eval-generator` | `spec.agents.library[key]` | `oramasearch/agent-eval-generator:e2a683c` |
| `agent-project-corpus` | `spec.agents.library[key]` | `oramasearch/agent-project-corpus:4537bcb` |
| `agent-autosuggest-corpus` | `spec.agents.library[key]` | `oramasearch/agent-autosuggest-corpus:a882954` |
| `agent-application` | `spec.agents.library[key]` | `oramasearch/agent-application:3a38da8` |
| `agent-experiments` | `spec.agents.library[key]` | `oramasearch/agent-experiments:62a4d64` |

Every Zetta a Lake owns also gets `ZETTA_MALLOC_TRIM_THRESHOLD_BYTES=67108864` and
`MALLOC_ARENA_MAX=2` unless its env (`spec.zetta.env`, `spec.services.zettaEnv`) names the
variable. The agent model defaults to `gpt-5.5` (`spec.agents.model`). An app without its own host
is `<app>.<spec.apps.wildcard>`, and an empty wildcard uses the Lake's `spec.host`
(`myapp.lake-dev.oramadev.net`); HTTPS on those two-level names needs a certificate per zone
([#515](https://github.com/oramasearch/orama-operator/issues/515)).

## What a Lake reconciles into

Every reconcile manages the following resources; the Deployment and disruption budget wait for
step 10 to finish applying the desired schema (`internal/controller/lake_controller.go`):

1. **Namespace** `spec.namespace` — created with a blind `Create` (AlreadyExists ignored), never
   owned, never deleted: the operator holds `namespaces: get, create` only (operator#106: no
   Namespace informer).
2. **Admin token** — Secret `lake-<name>-admin` (key `token`, 64 hex), minted once, owned;
   injected as `LAKE_ADMIN_TOKEN`. lake's `/v1` capabilities and MCP tool calls require it as a
   bearer.
3. **Instance KEK** — Secret `lake-<name>-instance-kek` (key `key`, 64 hex), minted once and owned
   while Postgres is enabled; injected as `LAKE_INSTANCE_KEK`. It wraps lake's persisted identity
   key and the per-customer KEKs used by static secrets and encryption. It is distinct from the
   admin bearer and is never sent to clients.
4. **Cluster sealing key** — Secret `lake-<name>-sealing-key`, an X25519 keypair minted once and
   owned. The public key and `kid` are published in `status.sealing` and injected into lake as
   `LAKE_SEALING_PUBLIC_KEY` / `LAKE_SEALING_KID`; the private key remains in the operator-owned
   Secret. An explicit `spec.sealing.generation` increase rotates it while records still using old
   keys keep those private keys available. See [sealed-secrets.md](sealed-secrets.md).
5. **Store claim** — either `spec.store.existingClaim` or an owned PVC
   `lake-<name>-store`, created once with `spec.store.size` (default `100Gi`),
   `storageClassName`, and `accessModes` (default `ReadWriteMany`). The selected claim is mounted
   read-write at `/var/lib/lake/store` in every pod. The pod runs with `fsGroup: 10001` (lake's
   uid/gid in the image) so a freshly provisioned volume is writable by lake on first mount, and
   `fsGroupChangePolicy: OnRootMismatch` avoids recursively changing every stored object's ownership
   after a node move when the volume root is already correct. Operator-created claims are never
   replaced, resized, or deleted because the store spec changes.
6. **Deployment + disruption budget** `lake-<name>` — `spec.image` run as built, `spec.args`, port
   `spec.port` (default 4900), readiness `GET /readyz`, liveness `GET /healthz`,
   `spec.replicas`, resource defaults or `spec.resources`, `imagePullSecrets` from
   `spec.pullSecretName`, and operator env including `LAKE_STORE_ROOT=/var/lib/lake/store`,
   `LAKE_APPS_WILDCARD`, `LAKE_APPS_RUNTIME_IMAGE`, `LAKE_METHODOLOGY_RUNTIME_IMAGE`,
   `LAKE_MCP_RUNNER_IMAGE` and the agents' variables (including `LAKE_INTERNAL_URL`, lake's
   in-cluster address) on every Lake, `LAKE_OPERATOR_ZETTA_URL` and `LAKE_OPERATOR_ZETTA_TOKEN` (item 8),
   `LAKE_PUBLIC_HOST` from `spec.host` when `spec.env` does not state it, plus `spec.env` entries whose
   names do not conflict with operator-owned variables. Pods run as
   uid/gid 10001 with the restricted container baseline. A spec change rolls the Deployment once;
   how the pods are replaced depends on the store's access modes, see [Rollouts](#rollouts). Replicated Deployments get a
   `maxUnavailable: 1` PodDisruptionBudget and the default soft hostname spread, or
   `spec.topologySpreadConstraints` when supplied. Scaling to one replica removes the budget.
7. **Service** `lake-<name>` — ClusterIP 80 → `spec.port`.
8. **Zetta data plane** — owned `OramaZettaCluster` `lake-<name>-zetta` in the Lake namespace,
   configured from `spec.zetta` (enabled, image, replicas, per-node storage, env, and reachability).
   It inherits `spec.pullSecretName` and has operator-schema provisioning **on**: it is also the
   Lake's deployment control plane, the one operator-schema Zetta in the Lake namespace
   ([operator#557](https://github.com/oramasearch/orama-operator/issues/557),
   [environments](environments.md#every-lakes-control-plane-is-its-own-zetta)). Its client Service and
   seeded admin-token Secret reach the lake pod as `LAKE_DATA_PLANE_URL` and `LAKE_DATA_PLANE_TOKEN`,
   and again as `LAKE_OPERATOR_ZETTA_URL` and `LAKE_OPERATOR_ZETTA_TOKEN`, which turn lake's
   Deployments on. Deleting the
   Lake starts deletion of this child and waits for its finalizers before releasing the Lake
   finalizer; disabling `spec.zetta` tears the child down with its per-node volumes.
9. **Control-plane Postgres** — CloudNativePG `Cluster` `lake-<name>-controldb`
   (`spec.postgres`: enabled by default, instances, storageSize 50Gi, storageClassName), the same
   unstructured path `OramaCloudV1` uses (`controldb.go`). Disabling tears it down **with its
   data**.
10. **Schema** — ConfigMap `lake-<name>-controldb-migrations` + `PostgresSchema`
   `lake-<name>-controldb-schema` applying `assets/lake-schema.sql` (vendored byte-identical from
   `oramasearch/lake` `db/schema.sql`), guard table = `methodology_deployments_guard` from
   `0083_methodology_deployments`, version = hash of the SQL plus guard. The Job skips the file only when
   the guard exists **and** every `INSERT INTO schema_migrations (version) VALUES ('NNNN_…')` in it
   is present in the database's `schema_migrations`; otherwise it applies the whole idempotent file,
   so a migration that merges out of order (0067 after 0068 and 0069, #271) still applies. A changed hash automatically
   replaces the obsolete migration Job, waiting for its pods to stop. Deployment creation and
   updates wait for Applied at the desired version and generation; existing pods retain their
   previous workload spec while the schema applies. Re-vendoring and native envtest proof are in
   [schema.md](schema.md). Proper incremental migrations are #126.
11. **Control-plane backup** (opt-in: `spec.postgres.backup.enabled`) — CNPG Barman object-store
   configuration on `lake-<name>-controldb` plus an owned `ScheduledBackup`
   `lake-<name>-controldb-backup`. The R2/S3 Secret lives in `spec.namespace`; CNPG owns the
   individual `Backup` resources. See [backups.md](backups.md) for R2 setup, verification,
   changing the schedule or retention, and recovery into a new Cluster.
12. **Ingress + TLS + DNS** (opt-in: `ingressEnabled` + `host`, `tls.secretName`,
   `dnsSecretRef`) — nginx Ingress for the whole host and an owned `CloudflareDNSRecord`, the
   `OramaZettaCluster` contract; gating off tears both down. The Ingress accepts Lake's 64 MiB
   request bodies without buffering streaming requests or responses. `tls.secretName` adds the
   host certificate and HTTPS redirect. When `previewWildcard` is set, the Lake also owns one
   wildcard record against the same ingress address; individual previews do not create
   Cloudflare records. `apexHost` adds a second rule to the same Ingress (and to its TLS hosts)
   and an owned `<lake>-apex-dns` record, for the region picker in front of a deployment's
   regions ([regions](#regions)); removing it removes both.
13. **Deployment environments** — desired rows in the operator's Zetta
   `orama_control.deployments` table, with lake's environment values and custom domains, become
   Lake-owned `LakeEnvironment` CRs in this namespace; every row is in lake's shape, keyed by the
   Lake's public host ([the row contract](environments.md#the-row-contract-lakes-format-the-operator-conforms)).
   Each CR owns its profiled workload Namespace through a verified finalizer. Referenced
   `LakeSealedSecret` resources must be current and Ready; their produced Secrets are copied into the
   isolated Namespace before the workload starts. See
   [`environments.md`](environments.md). Liquid creates, lists, rolls back, and reads these deployments
   through the operator's authenticated [deployment control plane](../control-plane/README.md).
14. **Data-service lifecycle evidence** — `status.dataServices` reports Postgres and Zetta backup,
    restore, upgrade, and failover evidence. A separate cluster-scoped `LakeRestoreRehearsal`
    controller runs Postgres procedures and updates the row; see
    [data services](data-services/README.md).
15. **Managed services** — once the schema has applied, the operator polls the `services`
    table in the control Postgres (pgx, Secret `lake-<name>-controldb-app` key `uri`) every
    `spec.deploySync.interval`. Each present service becomes `<name>-<slug>`: an `OramaZettaCluster`,
    a CloudNativePG `Cluster`, or, for the catalog kinds (liquid-bridge decision 47), a Strimzi
    `Kafka`, Altinity `ClickHouseInstallation`, `TidbCluster`, `K8ssandraCluster` or `ValkeyCluster`,
    or, for `sqlite`, `duckdb` and `polars`, a `StatefulSet` and `Service` of
    [lake-sqlite](lake-sqlite.md), [lake-duckdb](lake-duckdb.md) or [lake-polars](lake-polars.md),
    controlled by the Lake; a kind whose upstream operator is not installed fails only its own
    services. The poll excludes the workspace's own `zetta_service_id`, whose cluster and
    `service_status` are owned by the workspace reconciler, except that it writes that Zetta's
    connection too (operator#264). Ready services' connections go to
    Secret `lake-<name>-services` (key `<slug>.json`), mounted at `/var/run/lake/services` with
    `LAKE_SERVICE_DIR`; data-plane resources live in the row's joined workspace namespace and
    `service_status` is upserted on change. An absent service is deleted with its
    data (irreversibly); `spec.services.maxServices` caps how many exist. The poll never fails the
    Lake. See [`services.md`](services.md).
16. **Workspace namespaces** — `workspaces` rows become
    restricted `<environment>-ws-<id>` namespaces with quota, explicit network flows, localized
    pull credentials and one workspace Zetta. The workspace reconciler is that Zetta's only spec
    writer, uses the services image fallback, creates it at lake's `storage_gi` and grows its
    claims in place (never shrinking; operator#259), and writes its
    `service_status`, Ready only once the services poll has delivered its connection. Services, stores, user agents and synchronized apps with that required
    `workspace_id` also run there. Status writes are generation guarded; stopped rows
    retain data and deleted rows remove the namespace. See [`workspaces.md`](workspaces.md).
17. **In-cluster builds** — queued rows in the Lake control
    Postgres become one `oramasearch/lake-builder` Job each in a deterministic per-space namespace,
    with scoped credentials, the Lake pull Secret mounted read-only for private base images,
    network isolation, resource/deadline/retention bounds, per-Lake and
    per-space concurrency, registry cache, progress events, and terminal digest/size status. See
    [`builds.md`](builds.md).
18. **Volume snapshots** — `volume_snapshot_policies` rows become
    `VolumeSnapshot`s of the store claim and each workspace Zetta's claims through a `Retain`
    `VolumeSnapshotClass` the operator creates when absent: on the cron schedule, at first sight and
    on `requested_at`, pruned beyond `keep`, contents labelled with their target, and the newest
    reported in `volume_snapshot_status`. See [`volume-snapshots.md`](volume-snapshots.md).
19. **Workload resources** (with the control-rows poll) — one `workload_resources` row per
    workspace engine, managed service, app, build and agent launch: the CPU, memory, GPUs, GPU class
    and VRAM its scheduled pods were given, which lake prices (lake#643). Written only on change;
    a finished build or job keeps its values. See [`workload-resources.md`](workload-resources.md).
20. **Methodology runs** — each
    `methodology_runs` row waits for its image build, then runs a stage, a sandbox and a collect pod
    in `lake-<name>-methodology-runs`: the sandbox carries no credential, no token and no network
    unless granted. A workspace runs at most `spec.methodologies.maxConcurrentRunsPerWorkspace`
    (default 2) at once and the rest wait `Queued`, oldest first; a GPU run's stage pod, and so its
    work volume, goes in a zone where its GPU class has nodes, or it waits `Queued` (orama-operator#293).
    Phases and the sandbox's start and finish go to `methodology_run_status`. A trained model a
    person publishes (`methodology_deployments`, lake#690) is staged by a Job with its own
    credential and served by a `serve` Deployment of the methodology's image behind a Service that
    only lake's pods can reach. See [`methodology-runs.md`](methodology-runs.md).

**Status** reports overall service readiness through `phase` and `conditions[Ready]`, alongside
Deployment replica counts, store binding, Postgres, schema, Zetta, backup, sealing, and data-service
observations. `Ready=True` (`LakeReady`) requires all of the following:

- The Deployment controller has observed its current generation, the desired replica count is
  positive, and at least that many replicas are ready, updated, and available.
- The selected store PVC is Bound.
- When Postgres is enabled, CNPG reports at least `spec.postgres.instances` ready instances
  (default 1), and the owned `PostgresSchema` reports `Applied` for its current generation with
  `appliedVersion` matching the current vendored schema hash.
- When Zetta is enabled, the owned cluster reports `Ready` with every desired node ready.
- Enabled backup configuration is valid. Backup completion remains a separate `BackupReady`
  observation.

An unfinished rollout or dependency reports `Progressing` with an explanatory `Ready=False`
condition. Schema failure, Deployment replica failure or an exceeded progress deadline, a degraded
Zetta child, or invalid backup configuration reports `Degraded`. Failures take precedence over
pending work. The condition records the Lake generation; unchanged observations preserve its
transition time and produce zero status writes, including after an operator restart.

CNPG is polled every 20 seconds for as long as Postgres is enabled, including after success and
with backups disabled. Losing database readiness therefore removes `Ready` on the next poll;
recovery restores it when every requirement is satisfied. Owned Deployment and PostgresSchema
changes also enqueue reconciliation. Pending store and Zetta observations retain their existing
20-second polls. `kubectl get lake` columns: Namespace, Host, Ready, DB, Zetta, Schema, Store,
Backup, Data, Phase. The Ready column is a replica count; use `PHASE=Ready` or the `Ready=True`
condition to confirm overall readiness. The Data column shows managed and unmanaged service
counts; see [data services](data-services/README.md) for the operation-level labels and customer
boundary.

Native fake-client regression coverage in `internal/controller/lake_readiness_test.go` exercises
failed schema with an unavailable database, old ready pods during rollout, recovery, later database
readiness loss, stale schema versions/generations, and zero status writes in steady state.

**Owned kinds** (`SetupWithManager`): PersistentVolumeClaim (only the operator-created store
claim), Deployment, PodDisruptionBudget, Service, OramaZettaCluster, Ingress, CloudflareDNSRecord, ConfigMap, Secret, LakeEnvironment, LakeSealedSecret,
PostgresSchema, CNPG
ScheduledBackup. Not owned: an existing store claim, the Namespace, the CNPG Cluster (polled), and
CNPG Backup resources (observed by cluster label). A `LakeRestoreRehearsal` temporarily owns a
scratch Namespace, CNPG Cluster, copied Secrets, and verification Job; terminal cleanup deletes
them explicitly.

**RBAC.** The chart ClusterRole is hand-maintained (`chart/templates/rbac.yaml`); a Lake needs
`lakes`, `lakes/status`, `lakes/finalizers`, `oramazettaclusters`, `namespaces: create`, plus the shared grants for
deployments, poddisruptionbudgets, persistentvolumeclaims, services, configmaps, ingresses, cloudflarednsrecords,
postgresschemas, lakerestorerehearsals, `lakesealedsecrets` (including status and finalizers),
namespace-wide Secrets and `postgresql.cnpg.io/clusters`, `clusters/status`, `scheduledbackups`, and
`backups`. Scratch Namespace ownership is checked through the direct API reader, so Namespace RBAC
remains `get`, `create`, and `delete` without a cluster-wide informer. Keep it in sync with
`config/rbac/role.yaml` or the manager
CrashLoops on cache sync.

## Observed state in Zetta

When an `OramaZettaCluster` with `spec.provisionOperatorSchema` enabled runs in the Lake's
`spec.namespace`, the existing operator state-sync loop projects the cluster-scoped Lake into
`orama_control.components`. The row is keyed by the operator identity, Lake owner, `kind=Lake`, and
Lake name. Its `status` is the Lake phase; `detail` is deterministic JSON containing the desired
replica count and the complete anti-flapped `Lake.status`, including store, Postgres, schema, backup,
data-service, sealing, and condition observations.

Each changed row also appends an `orama_control.reconcile_events` entry with action
`component.state_changed`. The event's `cluster` is the operator identity and its JSON detail names
the row and new state. The component append and event use the same authenticated, best-effort retry
path as every other operator state row. A successful pair is fingerprinted, so an unchanged Lake
writes nothing. If the component append succeeds but the event fails, the next Zetta reconcile
retries only the event.

Lake is cluster-scoped, so association is explicit: a datastore observes only Lakes whose effective
workload namespace matches its namespace (`lake-dev` when `spec.namespace` is empty). The deployment
REST/MCP contract does not read this projection; it continues to use the separate append-only
`deployments` and `deployment_records` tables described in the
[deployment control-plane guide](../control-plane/README.md).

The `LakeEnvironment` controller additionally needs `lakeenvironments` (including status and
finalizers), verified Namespace update/delete, and Deployments, Services, Ingresses,
NetworkPolicies, ResourceQuotas and LimitRanges across environment namespaces. The chart grants
these cluster-wide, while the controller deletes only a Namespace bearing its exact ownership
annotation.

## Service e2e

`LakeE2ETest` is a cluster-scoped, explicit test trigger; ordinary Lake reconciliation never runs
the suite. It waits for the target Lake to have a ready replica, ready control-plane Postgres, and
an applied schema, then creates Job `<test-name>-e2e` in `Lake.spec.namespace`. The required runner
image owns the cross-repository workflow: sign up a fresh user, create a repository, invoke the real
liquid client to push it through Lake's sync endpoint, pull into an empty checkout, and compare the
file bytes. The operator supplies only the in-cluster base URL and resource UID as
`LAKE_E2E_BASE_URL` and `LAKE_E2E_RUN_ID`; it does not give the suite the Lake admin bearer.

Apply [`../../config/samples/lake-e2e-test.yaml`](../../config/samples/lake-e2e-test.yaml) after
replacing its runner image placeholder with an immutable published tag. `spec.pullSecretName`
overrides image authentication for the Job; when omitted, the controller reuses the target Lake's
pull Secret. Inspect it with:

```sh
kubectl get lakee2e hello
kubectl logs -n lake-dev job/hello-e2e
```

Deleting and recreating the test resource starts a new run with a new UID and garbage-collects the
old Job. The k0s and air-gapped cluster executions for issue #123 remain deferred; the controller's
resource wiring and status transitions are covered with the fake client.

## Deploying the operator with Lake support

The Lake CR is on `main`. Build and publish an immutable operator image, refresh changed CRDs,
then upgrade the current release:

```sh
docker buildx build --platform linux/amd64 -t oramasearch/orama-operator:lake-crd-<sha> --load .
docker push oramasearch/orama-operator:lake-crd-<sha>
kubectl apply -f config/crd/bases/orama.cloud_lakes.yaml        # helm never upgrades crds/
helm upgrade orama-operator ./chart -n orama-system \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag=lake-crd-<sha> \
  --set 'operator.imagePullSecrets[0].name=oramasearch-dockerhub'
```

An AKS installation still using the former Helm release must first follow the scheduled
[rename cutover runbook](../operations/rename-cutover.md). Do not change the release name with an
ordinary in-place upgrade.

Use immutable tags (the `v1-sandbox` + `IfNotPresent` stale-cache failure is documented in
[`../install.md`](../install.md)). The first upgrade after a `kubectl set image` digest pin needs
`--force-conflicts`: helm 4 applies server-side and the `image` field is owned by `kubectl-set`.

## Running a Lake

### Store volume

The default store creates `lake-<name>-store` at `100Gi` with `ReadWriteMany`, because every
Deployment replica mounts the same libliquid customer root. The storage class must actually
support RWX. `ReadWriteOnce` is valid only with `spec.replicas: 1`; otherwise pods scheduled on
different nodes cannot mount the shared claim. To use a pre-provisioned claim in the Lake
namespace, set `spec.store.existingClaim`; the operator mounts it without adopting, changing, or
deleting it.

Size, class, and access modes are creation-time settings for an operator-created claim. Later spec
changes do not mutate or replace the PVC, including when switching to or from `existingClaim`.
Deleting the Lake still garbage-collects the operator-owned claim, while a referenced existing
claim survives.

Lake opens one libliquid store per customer below
`/var/lib/lake/store/<customer-id>/store`. `LAKE_STORE_ROOT` is always set to the mounted root.

### Lake environment

`spec.env` accepts Kubernetes `EnvVar` entries, including `valueFrom` references. Entries follow
the operator's variables in the container spec. The operator ignores custom entries named
`LAKE_ADMIN_TOKEN`, `LAKE_INSTANCE_KEK`, `LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`,
`LAKE_DATA_PLANE_URL`, `LAKE_DATA_PLANE_TOKEN`, `LAKE_INTERNAL_URL`,
`LAKE_SEALING_PUBLIC_KEY`, `LAKE_SEALING_KID`, or `LAKE_STORE_ROOT`, so the reconciled Secret,
Service, and volume contracts always win. `LAKE_APPS_WILDCARD` is the exception: an explicit entry
wins over `spec.apps.wildcard`, and the operator renders it only once.

Lake reads these configurable variables:

| Variables | Purpose |
|---|---|
| `LAKE_LOG_MODE`, `LAKE_LOG_WITNESSES`, `LAKE_LOG_WITNESS_THRESHOLD`, `LAKE_LOG_AGGREGATOR` | Transparency log mode (`air-gapped`, `witnessed`, or `anchored`), JSON witness set, quorum, and anchored-mode endpoint. The runtime defaults to `witnessed`, which requires configured witnesses when a store root is present; use `air-gapped` until witnesses exist. |
| `LAKE_PUBLIC_HOST` | Public host used when Lake constructs externally visible URLs. |
| `LAKE_REPLICATION_TOKEN` | Bearer presented to peer Lake instances; use a Secret reference. |
| `LAKE_IDENTITY_SEED` | Optional base64 Ed25519 identity seed; use a Secret reference. |
| `LAKE_TSA_URL`, `LAKE_TSA_TRUST_CERT`, `LAKE_TSA_RENEWAL_SECONDS`, `LAKE_TSA_RENEW_BEFORE_SECONDS` | RFC 3161 timestamp authority, optional PEM trust roots, and renewal policy. |
| `LAKE_PRICE_CURRENCY`, `LAKE_STORAGE_QUOTA_BYTES`, `LAKE_PRICE_STORAGE_GIB_HOUR_MICROS`, `LAKE_PRICE_TRANSFER_IN_GIB_MICROS`, `LAKE_PRICE_TRANSFER_OUT_GIB_MICROS`, `LAKE_PRICE_COMPUTE_SECOND_MICROS` | Metering currency, storage quota, and unit prices. |

The operator additionally supplies `LAKE_ADMIN_TOKEN`, `LAKE_INSTANCE_KEK`,
`LAKE_CONTROL_DB_URI`, `LAKE_CONTROL_DB_PASSWORD`, `LAKE_DATA_PLANE_URL`,
`LAKE_DATA_PLANE_TOKEN`, `LAKE_SEALING_PUBLIC_KEY`,
`LAKE_SEALING_KID`, and `LAKE_STORE_ROOT` from managed resources.

```sh
kubectl create namespace lake
# copy the pull Secret and the Cloudflare zone token into it, then:
kubectl apply -f - <<'Y'
apiVersion: orama.cloud/v1alpha1
kind: Lake
metadata: { name: prod }
spec:
  namespace: lake
  image: oramasearch/lake:<tag>
  pullSecretName: oramasearch-dockerhub
  args: ["--listen", "0.0.0.0:4900", "--node", "prod"]
  replicas: 3
  resources:
    requests: { cpu: 250m, memory: 512Mi }
    limits: { memory: 2Gi }
  store:
    size: 100Gi
    accessModes: [ReadWriteMany]
  env:
    - name: LAKE_LOG_MODE
      value: air-gapped
    - name: LAKE_PUBLIC_HOST
      value: lake.oramadev.net
  postgres: { instances: 1, storageSize: 50Gi }
  zetta:
    replicas: 1
    image: oramasearch/zetta:<tag>
    storage: { size: 10Gi }
    env:
      - { name: ZETTA_MALLOC_TRIM_THRESHOLD_BYTES, value: "134217728" }
      - { name: MALLOC_ARENA_MAX, value: "2" }
    reachability: dial-out-only
  ingressEnabled: true
  host: lake.oramadev.net
  tls: { secretName: lake-origin-tls }
  previewWildcard: preview.lake.oramadev.net
  deploySync: { interval: 15s }
  dnsSecretRef: { name: cloudflare-api-token }
Y
kubectl wait lake/prod --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl get lake prod
```

`kubectl delete lake <name>` removes everything owned, including both databases. The Lake
finalizer waits for the Zetta child controller to finish its ordered cleanup before the parent is
released.

### AKS Zetta data-plane verification

After deploying the CRD and controller change, verify `Lake/hello` has both its Postgres control
plane and live Zetta data plane:

```sh
kubectl wait lake/hello --for=jsonpath='{.status.schemaPhase}'=Applied --timeout=600s
kubectl wait lake/hello --for=jsonpath='{.status.zettaReady}'=true --timeout=600s
kubectl get lake hello
kubectl get oramazettacluster lake-hello-zetta -n lake-dev
kubectl get cluster lake-hello-controldb -n lake-dev
kubectl exec -n lake-dev deploy/lake-hello -- sh -c \
  'test "$LAKE_DATA_PLANE_URL" = "http://lake-hello-zetta-zetta.lake-dev.svc.cluster.local:4800" && test -n "$LAKE_DATA_PLANE_TOKEN"'
kubectl port-forward -n lake-dev service/lake-hello-zetta-zetta 4800:4800
```

In another shell, authenticate a Zetta server-info request with the owned cluster's seeded token:

```sh
ZETTA_TOKEN=$(kubectl get secret lake-hello-zetta-zetta-admin -n lake-dev -o jsonpath='{.data.token}' | base64 -d)
curl --fail --silent --show-error -H "Authorization: Bearer $ZETTA_TOKEN" \
  http://127.0.0.1:4800/v1/server_info | jq
```

The AKS `Lake/hello` verification for issue #125 is deferred because this work order forbids
cluster changes.

## Workload hardening

The Lake Deployment defaults to CPU and memory requests of `50m` and `128Mi`, with a `512Mi`
memory limit. Set `spec.resources` to replace that complete Kubernetes resource block for a
production size. The default intentionally leaves the CPU limit unset so bursts are not throttled;
the scheduler still reserves CPU through the request.

Lake's unauthenticated operational routes drive distinct probes: `GET /healthz` is liveness and
`GET /readyz` is readiness, including control-store availability. The pod and container require
non-root uid/gid 10001, disable privilege escalation, drop every Linux capability, and use the
runtime-default seccomp profile. Rollout behavior, probes and the drain are in
[Rollouts](#rollouts).
The pod's `fsGroupChangePolicy: OnRootMismatch` applies the store's group ownership only when its
root does not already match gid 10001, avoiding a full recursive ownership pass on every mount.

### DNS: ndots 2 on lake, agent and app pods

Lake's pod, every agent pod (library Deployments, user-agent services, launch Jobs and scheduled
runs) and every app pod carry `dnsConfig.options: [{name: ndots, value: "2"}]`, keeping the
`ClusterFirst` nameservers and search list ([operator#276](https://github.com/oramasearch/orama-operator/issues/276)).
The kubelet default is `ndots:5`, so a Service's full name, `<svc>.<ns>.svc.cluster.local` (four
dots), was first tried under every search domain, A and AAAA each, including the node's cloud host
domain that CoreDNS forwards off the cluster. On dev that cost about 18 ms per lookup (5–32 ms)
against 2–7 ms for the absolute name. With `ndots:2` a name with two or more dots is tried as given
first: a Service's full name resolves on the first query, and an external name such as
`api.openai.com` no longer walks the search list. Short names (`<svc>`, `<svc>.<ns>`) still use
the search list as before.

Because a two-dot name is now tried as given first, every in-cluster URL the operator hands these
pods is the Service's full name: `LAKE_INTERNAL_URL`, an agent's `LAKE_URL`,
`LAKE_AGENT_ENDPOINTS` and a user agent's endpoint are `<svc>.<ns>.svc.cluster.local`, no longer
`<svc>.<ns>.svc`. The data plane URL, workspace Zetta connections, service connections and the
apps' auth and event URLs were already full names. Because `LAKE_INTERNAL_URL` is the build
registry authority, a build requested after this change targets
`lake-<name>.<ns>.svc.cluster.local/...`; rows already written keep their target, which still
resolves.

The other choice, writing every URL as an absolute name with a trailing dot, was rejected: it
covers only the names the operator writes (not CloudNativePG's `uri`, names lake derives itself,
or what an agent or app builds), and a trailing dot changes the `Host` header and the TLS
server name, which lake's request-origin and app-host derivation, libpq's `verify-full` host check
and the catalog clients would each have to accept.

To measure from inside lake's pod (`getent` resolves through the pod's own resolver; each run
includes starting `getent`):

```sh
NS=lake; POD=$(kubectl -n $NS get pod -l app.kubernetes.io/name=lake -o name | head -1)
kubectl -n $NS exec $POD -- cat /etc/resolv.conf   # options ndots:2 after the change
for name in lake-prod-zetta-zetta.lake.svc.cluster.local lake-prod-zetta-zetta.lake.svc.cluster.local.; do
  kubectl -n $NS exec $POD -- sh -c "for i in 1 2 3 4 5 6 7 8 9 10; do
    s=\$(date +%s%N); getent ahosts $name >/dev/null; e=\$(date +%s%N); echo \$(( (e-s)/1000 ))us; done" | sed "s|^|$name |"
done
```

Before the change the first name takes about 18 ms and the trailing-dot name 2–7 ms; after it
both take the trailing-dot time.

When `spec.replicas` is greater than one, the operator creates `PodDisruptionBudget lake-<name>`
with `maxUnavailable: 1`. It also defaults `spec.topologySpreadConstraints` to a soft
`kubernetes.io/hostname` spread. Supply the field to replace that list; an explicit empty list
disables the default. At one replica the operator removes the PDB and renders no default spread.

The nginx Ingress always sets `proxy-body-size: 64m` and disables request and response buffering
for Lake's bounded streaming transfers. Set `spec.tls.secretName` to a `kubernetes.io/tls` Secret
in the Lake namespace to render `spec.tls`, enable HTTPS redirect, and terminate origin TLS at
nginx. For Cloudflare Origin CA, switch the zone to Full (strict) only after the origin certificate
is present and the Ingress answers HTTPS.

### Rollouts

One Lake spec change produces exactly one Deployment rollout, and lake keeps answering while it
runs ([operator#243](https://github.com/oramasearch/orama-operator/issues/243)).

**One rollout per change.** The pod template is computed from the Lake spec, the sealing key and the
store claim's access modes only. Nothing in it comes from status, time, observed pods or a hash of
a Secret that changes later, and claim access modes are immutable, so a reconcile after the change
renders the same template and the Deployment generation moves once. Schema gating holds the whole
update until the schema has applied; it never applies half of it. Mounted Secrets (services,
agent bus) are refreshed by kubelet in place and never roll lake. The envtest
`TestLakeSpecChangeRollsOnce` patches `spec.apps.runtimeImage` and checks that the generation steps
once and that three more reconciles leave the template and generation unchanged.

**Pod replacement.**

| Store | Strategy |
|---|---|
| ReadWriteMany (the default), or more than one replica | `RollingUpdate`, `maxSurge: 1`, `maxUnavailable: 0`: the new pod starts beside the old one, and the old pod stops only after the new one is ready. No gap. |
| ReadWriteOnce, one replica (the AKS `Lake/prod` store, `managed-csi`) | `Recreate` by default: the old pod stops, then the new one starts. There is a restart gap. With `spec.rollout.overlapOnSharedStore: true` the Lake surges like the row above, with the co-location described below. |
| ReadWriteOncePod | Always `Recreate`. A second pod can never mount the claim, so a surge would never finish. |

**Why `overlapOnSharedStore` is an opt-in, and when it is safe.** A surge on a single-replica
ReadWriteOnce store starts a second lake process on the same store root while the first drains.
Two processes writing one liquid store corrupt it (lake#467). Since lake#479 (lake PR #480, lake
`c7d24d6` and later) lake never lets that happen: the new process binds and reports Ready while it
waits (bounded, `LAKE_STORE_OWNER_WAIT_SECONDS`, default 90 s) for the old one to release
`.lake-owner.lock` and liquid's `store.lock` (liquid#321), holding store-touching requests rather
than answering 503; the old process drains (`LAKE_DRAIN_SECONDS`, default 20 s) and releases both.
Older lake images refuse to start beside a running lake, so the surge would never finish: set the
opt-in only on a Lake whose image includes lake#479. It stays opt-in so a Lake on an older image keeps
`Recreate`. Dev (`Lake/prod`) runs with it on since 2026-10-03; a rollout restart there answered 129
of 129 one-second polls with 200 (slowest held request 29.5 s; the successor waited 23 s and opened
the store in 13 s).

With `Recreate`, the probes and drain below still shorten the gap, and Lake `Ready` stays honest.

**Probes and drain.**

| Setting | Value | Why |
|---|---|---|
| Readiness | `GET /readyz`, every 5 s, timeout 2 s, 3 failures | lake answers 200 when its control store answers and its store root is open, or while a live predecessor holds the store (it then holds store-touching requests until the hand-off); 503 on a cold open without a predecessor, after a failure, and after SIGTERM. |
| Startup | `GET /healthz`, every 5 s, 60 failures | Holds liveness off for up to five minutes while a cold start opens the store. |
| Liveness | `GET /healthz`, every 20 s, timeout 3 s, 3 failures | Process liveness only. |
| preStop | kubelet `sleep` action, 15 s | Lets the endpoint controller and nginx drop the pod before lake gets SIGTERM; lake then reports 503 on `/readyz`, drains in-flight requests for up to `LAKE_DRAIN_SECONDS` (20 s), closes its stores and releases the store locks. 15 + 20 s fits the 45 s grace period. The built-in action needs no shell in the image. |
| `terminationGracePeriodSeconds` | 45 | Covers the drain plus lake's shutdown. |

**Co-location with the opt-in.** A ReadWriteOnce volume can be mounted by several pods only on the
node it is attached to. With `overlapOnSharedStore`, a single-replica Lake whose claim is not
ReadWriteMany gets required pod affinity to its own pods on `kubernetes.io/hostname`. The surge pod
schedules onto the node of the running lake pod and mounts the already-attached volume. The first pod matches its own affinity term, so it schedules anywhere
when no lake pod exists. If that node cannot fit the surge pod, the new pod stays Pending, the old
pod keeps serving, and the Deployment reports `ProgressDeadlineExceeded` after 600 s (the Lake shows
`Degraded`); free capacity on that node or move the Lake by deleting its pod. A ReadWriteMany claim
(the default) and multi-replica Lakes surge anywhere with no affinity, and the field has no effect
on them. Toggling the field changes the pod template, so it causes one rollout.

**Readiness of the Lake.** The Lake's `Ready` condition also waits until no old replica remains
(`status.replicas` equals `status.updatedReplicas`), as `kubectl rollout status` does, so automation
that waits on the Lake does not report a rollout complete while the old pod is still draining. A
change made before the operator has reconciled it is still invisible to the Lake's status; compare
the `Ready` condition's `observedGeneration` with `metadata.generation` before trusting it.

### Rollout check

Run this after deploying the controller change to AKS. The Lake must be Ready and its public
hostname must already be healthy.

```sh
kubectl get deployment,poddisruptionbudget -n <namespace> lake-<name>
kubectl get pods -n <namespace> -l app.kubernetes.io/name=lake -o wide
```

Record the current pod and node, then restart and wait for the Deployment:

```sh
kubectl rollout restart deployment/lake-<name> -n <namespace>
kubectl rollout status deployment/lake-<name> -n <namespace>
kubectl get pods -n <namespace> -l app.kubernetes.io/name=lake
```

Poll the public hostname for the whole restart, for example
`while true; do curl -s -o /dev/null -w '%{http_code}\n' https://<host>/readyz; sleep 1; done`.
In every mode, `kubectl get rs -n <namespace>` must show exactly one new ReplicaSet for the change,
and the replacement must mount the store without a persistent `Multi-Attach` error.

- `Recreate` (the single-replica ReadWriteOnce default): the old pod terminates before the
  replacement is created, and the poll shows one short run of 503s for the restart gap.
- Surge (ReadWriteMany, or with `overlapOnSharedStore`): the replacement becomes Ready before the old
  pod starts terminating, and the poll shows no 503. On a ReadWriteOnce store it starts on the old
  pod's node. The PDB still has `maxUnavailable: 1` when the Lake has multiple
replicas.

## Current production instance

`Lake/prod`, namespace `lake`, 3 replicas, 50Gi Postgres, schema `0001_identity_and_customers`
applied, https://lake.oramadev.net; operator helm revision 8, `lake-crd-8058ab5` (2026-09-02).
Details and the not-yet list: https://github.com/oramasearch/lake/blob/main/docs/deployment/aks.md.

## Open

- #126 incremental migrations in `PostgresSchema`.
- [`app-transfer.md`](app-transfer.md) — the `app_transfer` rows lake meters app traffic from:
  each app's bytes in and out through ingress-nginx's per-Ingress counters, by hour; enabling the
  controller's metrics, the counter arithmetic across restarts, and what is not counted (lake#661).
- [`workload-resources.md`](workload-resources.md) — the `workload_resources` contract lake meters compute from: what each kind maps to, the effective-request arithmetic, GPU class and VRAM, and when a row is written, kept and deleted (lake#643).
- [`methodology-runs.md`](methodology-runs.md) — a workspace methodology's runs as untrusted code: the stage, sandbox and collect pods, their namespace and policies, phases, the per-workspace queue, GPU placement and metering (lake#655, orama-operator#293), and serving a trained model from Ship → Models (lake#690).
- [`agents.md`](agents.md) — the Agent Library's agents, the launches of user-created agents on agent-runtime, their cron schedules as CronJobs, and each library agent's own agent-bus credential (lake#228, lake#231, lake#244, lake#269).

## Regions

Decision 64 ([liquid-bridge#39](https://github.com/oramasearch/liquid-bridge/issues/39),
[operator#512](https://github.com/oramasearch/orama-operator/issues/512)): production Lake runs one
Lake per region, each at its own host (`eu.lake.live`, `us.lake.live`), with a region picker at the
apex (`lake.live`). A workspace belongs to the region it was created in; nothing joins the regions
yet. One `LakeSet` manages them all, and the operator supplies everything lake's picker needs:

| Field | Env var lake reads | Set by |
|---|---|---|
| `spec.region.name` | `LAKE_REGION` | the LakeSet, from the member's `KubernetesCluster` region; a different stated name is refused |
| `spec.region.label` | `LAKE_REGION_LABEL` | stated on the member ("Europe") |
| `spec.regions` | `LAKE_REGIONS` (JSON `[{name, label, host, latitude, longitude}]`) | the LakeSet: one entry per member with a region label, written into each such member; stating it in a set is refused |
| `spec.apexHost` | `LAKE_APEX_HOST` | stated on one member; a second is refused. Adds the Ingress rule and DNS record above |

**One environment per picker** (Steve, 2026-10-17: *"pickers only work within a single
environment, not across multiple environments"*). The regions a picker offers are the regions of
one environment: production's `eu.lake.live` and `us.lake.live`. Nonprod is one region and has no
picker. A set whose labelled members name the same cloud region twice is refused (#532), because
two members in one region are two environments, not two regions.

The four env names are reserved: the same names in `spec.env` are ignored. A Lake with none of these
fields (nonprod, a dedicated instance) gets none of the variables and shows no picker.

Region locations come from `cloudRegionLocations` in `internal/controller/lake_regions.go`, copied
from `az account list-locations`. A member in a region missing there is refused with a message
naming the region; add the region's latitude and longitude to the table to fix it.

**Verify:** `kubectl get lake <name> -n <ns> -o jsonpath='{.spec.regions}'` on the member's
cluster, and `kubectl get deploy lake-<name> -n <ns> -o yaml | grep -A1 LAKE_REGION`.

