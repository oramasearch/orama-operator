# Security deep search: workspace data services, methodology runs and sealed secret delivery

Issue [#714](https://github.com/oramasearch/orama-operator/issues/714), part of the multitenancy
security review [lake#873](https://github.com/oramasearch/lake/issues/873). Reviewed 2026-10-21.

**Question.** Can one workspace reach another workspace's data, credentials or model through its
managed data services, its methodology runs, or the delivery of its secrets into Kubernetes; and can
it reach more of its own than it was given?

**Answer.** No cross-workspace path was found that works from inside a workspace sandbox today.
One defect let untrusted methodology code escape its sandbox into the credential-carrying collect
pod; it is **fixed in this change** (operator side) and filed for the runtime. The other findings are
missing defence in depth, admin credentials handed to tenants, and one live leftover, each filed.

## How

- **Code read**, at `origin/dev` of the operator (66d838f) and lake (3b436919), and
  `oramasearch/methodology-runtime` at `main`. Paths below are relative to `internal/controller/`
  for the operator unless they start with `lake:` or `runtime:`.
- **Regression tests written** for every isolation rule this review relies on in the operator
  (listed under each part), and a failing-then-passing test for the fix.
- **Live reads**, read-only, on the managing cluster (`kubectl` context
  `orama-production-cluster-01`): namespaces and their labels, objects in the three workspace
  namespaces (names only), NetworkPolicies, CNPG clusters, `LakeSealedSecret`s, the Lake's sealing
  status, the CNI, Strimzi `Kafka` objects. No Secret value was read.
- **Not run:** a live network probe. It creates a pod, so it is a change to a cluster and needs
  Steve's consent. It is written out in [The live probe](#the-live-probe-needs-steves-consent).

## Summary

| # | Check | Verdict | Issue |
|---|---|---|---|
| 1.1 | Service credentials generated per service, no static or empty password | **Pass**, two upstream defaults unknown, one short TiDB window | [#772](https://github.com/oramasearch/orama-operator/issues/772), [#774](https://github.com/oramasearch/orama-operator/issues/774) |
| 1.2 | Service reachable only from its own workspace and its Lake | **Pass**, except **Kafka: fail** | [#770](https://github.com/oramasearch/orama-operator/issues/770) |
| 1.3 | No tenant shares a datastore server with another tenant or with Lake | **Pass**; one live tenant Postgres left in the Lake namespace | [#778](https://github.com/oramasearch/orama-operator/issues/778) |
| 1.4 | Superuser Secrets never reach tenant code; operator access path not usable by tenants | **Pass** | [#775](https://github.com/oramasearch/orama-operator/issues/775) (TLS of the operator's sign-in) |
| 1.5 | The credential handed to a tenant is least privilege | **Fail** (admin for TiDB, Cassandra, Valkey, Kafka, ClickHouse) | [#771](https://github.com/oramasearch/orama-operator/issues/771) (lake-services half of [#99](https://github.com/oramasearch/orama-operator/issues/99)) |
| 1.6 | Lake returns credentials only to the owning workspace, at the right privilege | **Fail** (bind is customer-write and crosses workspaces) | [lake#945](https://github.com/oramasearch/lake/issues/945) |
| 1.7 | All credentials not concentrated where one compromise reads them all | **Fail** (one aggregate Secret in every Lake pod) | [#773](https://github.com/oramasearch/orama-operator/issues/773) |
| 1.8 | Snapshots and backups restorable only into the same workspace | **Pass** (no tenant restore exists); data services have no backups | backups: [#731](https://github.com/oramasearch/orama-operator/issues/731)–[#737](https://github.com/oramasearch/orama-operator/issues/737), PR [#742](https://github.com/oramasearch/orama-operator/pull/742) |
| 2.1 | Run pods: no token, locked security context, limits, deadlines | **Pass** | — |
| 2.2 | Run (sandbox) pod: no credential | **Pass** by the pod spec; **failed in practice** through the shared volume | **fixed here**; [methodology-runtime#4](https://github.com/oramasearch/methodology-runtime/issues/4) |
| 2.3 | Network: none unless granted; granted means public internet only | **Pass** | — |
| 2.4 | One workspace's run cannot read another's inputs, outputs or model | **Pass**; one registry hardening gap | [lake#952](https://github.com/oramasearch/lake/issues/952) |
| 2.5 | Collect: what it reads back, size limits | **Partial** (follows symlinks) | [methodology-runtime#5](https://github.com/oramasearch/methodology-runtime/issues/5) |
| 2.6 | Serve pods: reachable only by Lake, no credential, own model only | **Pass** | — |
| 3.1 | A delivered Secret lands only in its own namespace | **Pass** | — |
| 3.2 | Ciphertext for one cluster or namespace cannot be replayed into another | **Pass**; **workspace not bound at delivery** | [lake#950](https://github.com/oramasearch/lake/issues/950) |
| 3.3 | Rotation rewrites the Secret | **Pass**; a revoked secret is **not** removed | [#777](https://github.com/oramasearch/orama-operator/issues/777) |
| 3.4 | The operator never logs or stores a plaintext outside the Secret | **Pass** | — |
| 3.5 | `lake-secret://` references resolved only within the owning workspace | **Pass at write time, not checked at delivery** | [lake#950](https://github.com/oramasearch/lake/issues/950), PR [#709](https://github.com/oramasearch/orama-operator/pull/709) |

Every security finding above is enforced by NetworkPolicy, which needs the CNI to enforce it. The
managing cluster runs Cilium (`kube-system/cilium` DaemonSet, live); requiring it everywhere is
[#699](https://github.com/oramasearch/orama-operator/issues/699) (PR
[#717](https://github.com/oramasearch/orama-operator/pull/717)).

## 1. Workspace data services

Lake's `services` rows become one upstream cluster per service, named `<lake>-<slug>`
(`lake_services.go:221`, `933-953`; `lake_service_catalog.go:209-211`), in the workspace namespace
`<env>-ws-<12 hex>` (`lake_workspaces.go:381-382`; services placed at `lake_services.go:333-336`,
`610`). Each kind has one cluster-wide upstream operator (`helmfile.yaml`: CNPG, Strimzi
`watchAnyNamespace` `:114-122`, Altinity `:136-151`, TiDB `:153-170`, K8ssandra `:172-183`, Valkey
`:187-193`).

### 1.1 Credentials

| Kind | Credential | Verdict |
|---|---|---|
| Postgres (CNPG) | CNPG-generated `<cluster>-app`: owner `app` of database `app` (`lake_services.go:976-978`, read `:995-1021`) | Pass |
| Catalog kinds | `ensureServicePassword`: 16 random bytes hex into `<name>-credentials` in the workspace namespace (`lake_service_catalog.go:60-62`, `436-469`); never rotated (`:435`) | Pass |
| ClickHouse | `app` with a generated password, only its SHA-256 in the spec (`lake_service_clickhouse.go:103-110`). Built-in `default` and Altinity's `clickhouse_operator` are left to upstream defaults (`:104-111`, `helmfile.yaml:136-151`) | Pass for `app`; **unknown** for built-ins: [#772](https://github.com/oramasearch/orama-operator/issues/772) |
| TiDB | root password generated into `<name>-init`, applied by the initializer (`lake_service_tidb.go:27-33`, `100`, `116-118`); not Ready before `passwordInitialized` (`:142-144`) | Pass; empty-password window: [#774](https://github.com/oramasearch/orama-operator/issues/774) |
| Valkey | built-in `default` user disabled; `app` with the generated password (`lake_service_valkey.go:44-55`, `81-83`) | Pass |
| Cassandra | K8ssandra-generated `<slug>-superuser` (`lake_service_cassandra.go:124-153`); stock `cassandra/cassandra` not verified gone | Pass; **unknown**: [#772](https://github.com/oramasearch/orama-operator/issues/772) |
| Kafka | SCRAM-SHA-512 user made by Strimzi's User Operator, owner UID checked (`lake_service_kafka.go:241-247`); `tls: false` (`:74-77`), `SASL_PLAINTEXT` (`:43`) | Partial: [#770](https://github.com/oramasearch/orama-operator/issues/770) |
| SQLite, DuckDB, Polars | 32-byte bearer token in `<name>-credentials`, by `SecretKeyRef` (`lake_service_wrapper.go:44`, `132-133`, `236-237`) | Pass |

### 1.2 Who can reach a service

The workspace `NetworkSandbox` puts `default-deny` (selector `{}`, both directions) on the
namespace (`lake_workspaces.go:814-905`). Ingress to service pods is allowed only from: the owning
Lake's pods on client ports (`lake_workspace_service_policies.go:17-87`); each upstream operator on
its management ports (`lake_workspace_datastore_policies.go:90-175`, `223-247`); the Orama operator
on 5432 (`lake_workspaces.go:1133-1156`); same-namespace pods until the workspace's connections are
active (`:839-844`, `1082-1093`); and declared `Connection` paths, same namespace only
(`connection_controller.go:455`, `464`). A workspace's egress is DNS, its Lake, its connections and,
if turned on, public addresses (`lake_workspaces.go:72`, `825-838`); an app's binding egress names
its own workspace only (`lakeapp_controller.go:524-533`).

Live, `prod-ws-6bb777ba498a`: `default-deny`, `allow-lake-service-postgres-ingress` (from
`namespace=lake`, `app.kubernetes.io/name=lake`, `instance=prod`, TCP 5432 to `cnpg.io/cluster`
pods), `allow-operator-to-postgres` (from `orama-system`, `app.kubernetes.io/name=orama-operator`,
5432), the upstream operator allowances, per-app and per-connection policies. No policy names
another workspace namespace.

**Kafka fails.** Strimzi writes its own `<cluster>-network-policy-kafka`, which admits the client
listener 9092 from every source because the listener sets no `networkPolicyPeers`
(`lake_service_kafka.go:74-77`); the repository already documents it (`docs/lake/workspaces.md:229-233`)
and `helmfile.yaml:114-129` does not turn the generation off. Policies are a union, so this undoes
`default-deny` for Kafka. There is no authorization either. No `Kafka` exists live today. Filed
[#770](https://github.com/oramasearch/orama-operator/issues/770) (p2).

### 1.3 Sharing

Every service is its own upstream cluster; none shares a server with another tenant or with Lake's
control DB, which is its own CNPG cluster in the Lake namespace (`lake_services.go:384`). Upstream
instance labels are per object, so two services of one kind never select each other's pods
(`lake_service_catalog.go:213-226`). The upstream operators are shared and cluster-wide by design.

**Live leftover.** `lake/prod-svc-5d97f8ed8de9`, a tenant Postgres (customer `787217d6-…`, created
2026-09-26, controller `Lake/prod`, one pod running 25 days) with its Services and Secrets, still
runs in the Lake namespace; the same service also runs in `prod-ws-787217d6ec44` since 2026-10-04.
Nothing removes the old copy. Filed [#778](https://github.com/oramasearch/orama-operator/issues/778)
(p3); deleting it is a database deletion and needs Steve's yes.

### 1.4 Superuser and the operator's own access

- Every tenant Postgres has `enableSuperuserAccess: true` (`lake_services.go:962-967`; live `true` on
  all three workspace clusters). `<cluster>-superuser` is read only by the `Connection` reconciler to
  manage scoped roles (`connection_credentials.go:139-158`). No code mounts or copies it into a
  workload; the operator creates no Role or RoleBinding for tenants in workspace namespaces (live:
  only the CNPG and TiDB instance service accounts are bound), and tenant pods carry no token
  (`lake_workspaces.go:805`, the app pod spec). **Pass.**
- `workspaceOperatorPostgresName` (`allow-operator-to-postgres`) admits only the operator pod in its
  namespace (`lake_workspaces.go:1133-1156`, `851-856`). A tenant would need to run a pod with that
  label in `orama-system`. **Pass.** The operator's superuser sign-in uses `sslmode=prefer`
  (`connection_postgres_roles.go:153`): [#775](https://github.com/oramasearch/orama-operator/issues/775) (p4).

### 1.5 Privilege of the tenant's credential

Credential-scoped `Connection`s get a `lake_conn_*` role with `NOSUPERUSER NOCREATEDB NOCREATEROLE
NOREPLICATION NOBYPASSRLS` (`connection_postgres_roles.go:248`). Every other path hands over the
service's owner or administrator: Postgres the database owner; TiDB `root` (`lake_service_tidb.go:27`);
Cassandra the K8ssandra superuser (`lake_service_cassandra.go:132`); Valkey `app` with `+@all`
(`lake_service_valkey.go:50`); ClickHouse `app` with the `default` profile; Kafka the only user with no
ACLs. Contained by one-cluster-per-service, but not least privilege. Filed
[#771](https://github.com/oramasearch/orama-operator/issues/771) (p3), the lake-services half of
[#99](https://github.com/oramasearch/orama-operator/issues/99) (in progress, which today touches Cloud
v1 credentials).

### 1.6 What lake returns, and to whom

- `service_connection` returns the full connection, password included, to customer **admin**
  (`lake:src/api/workspace_capabilities.cpp:1400-1447`), service looked up by customer
  (`lake:src/control/zetta_workspace.cpp:288-295`).
- `bind_service_to_app` needs only customer **write** and matches the service by customer, not
  workspace (`lake:src/api/web_app_capabilities.cpp:1173-1211`, `385-405`); the operator copies the
  owner credential from `lake-<lake>-services[service_id]` into the app's bindings Secret without a
  workspace check (`lakeapp_tokens.go:107-121`). A write-only member can read TiDB `root` from an
  app, and across workspaces of the customer. Filed [lake#945](https://github.com/oramasearch/lake/issues/945) (p3).
- `connect_service` (Postgres) has the same customer-write pattern
  (`lake:src/api/workspace_capabilities.cpp:2588-2666`); covered by the same issue.

### 1.7 Concentration

Every Ready credential of every customer is copied into `lake-<lake>-services` in the Lake namespace
(`lake_services.go:1187-1212`) and mounted into every Lake pod (`lake_controller.go:730-744`). Filed
[#773](https://github.com/oramasearch/orama-operator/issues/773) (p3).

### 1.8 Snapshots and backups

`request_volume_snapshot`, `volume_snapshots` and `set_workspace_storage` are limited to the
operator or the instance administrator (`lake:src/api/volume_capabilities.cpp:14-24`, `127-203`) and
target only the store and workspace Zetta claims (`lake_volume_snapshots.go:33-42`, `247-290`). Lake
has no restore capability; restore is a manual runbook (`docs/lake/volume-snapshots.md`). No tenant
can restore into another workspace. **Pass.** Managed data services have no backup at all; that is
the backup work in [#731](https://github.com/oramasearch/orama-operator/issues/731)–[#737](https://github.com/oramasearch/orama-operator/issues/737)
and PR [#742](https://github.com/oramasearch/orama-operator/pull/742). When #742 lands, its restore
must refuse a target namespace other than the source's workspace, and its dump must not use TiDB
`root`, the Cassandra superuser or the CNPG superuser.

## 2. Methodology runs

Every methodology pod of a Lake runs in one namespace, `lake-<lake>-methodology-runs`
(`lake_methodology_runs.go:167-174`), shared by every workspace of that Lake, with Pod Security
`restricted` enforced (`:232-244`).

### 2.1 Pod hardening — pass

One builder, `methodologyPod` (`:382-436`): no service-account token, no service links (`:407-408`),
uid/gid/fsGroup 10001, `runAsNonRoot`, `RuntimeDefault` seccomp, no privilege escalation, read-only
root, all capabilities dropped (`:410-431`), a 1Gi `/tmp` emptyDir. Stage and collect: 100m–1 CPU,
256Mi–1Gi (`:372-377`), 1800 s deadline. The run's resources and timeout come from the row (lake caps
Validate at 900 s, `lake:src/api/methodology_capabilities.cpp:752-755`); the operator has no ceiling of
its own, which belongs to op#713. Jobs: `backoffLimit` 0. Tests:
`TestTheSandboxCarriesNoCredentialAndRunsLocked`, `TestTheServePodHoldsNoCredentialAndRunsLocked`.
Untrusted code shares nodes and kernel with other tenants under runc (no RuntimeClass, no tainted
pool): filed by the app review as [#763](https://github.com/oramasearch/orama-operator/issues/763).

### 2.2 The credential and the shared volume — fixed here

- Stage and collect mount the per-run `lmr.<run id>.<secret>` credential as a file, mode 0440
  (`:392-399`), live until the run is terminal; lake accepts it only for
  `methodology_run_input`/`_output`/`_artifact` of that run (`lake:src/api/registry.cpp:646-660`,
  `own_run` at `lake:src/api/methodology_capabilities.cpp:2150-2161`). The run pod mounts no Secret
  (`:596-605`).
- **Defect.** The runtime starts with `python3 -s -m orama_methodology` (`runtime:bin/methodology:8`),
  and `-m` puts the working directory first on `sys.path`. The image's working directory is `/work`
  (`runtime:Dockerfile:31`), and the operator set no `workingDir`, so stage and collect started in
  `/work`: the volume the untrusted run pod writes. A run that wrote `/work/orama_methodology/__init__.py`
  or `/work/json.py` had its code imported by the collect pod, with the credential and the transfer
  network (DNS and the Lake's port; DNS tunnelling alone breaks "no network unless granted", and with
  PR [#749](https://github.com/oramasearch/orama-operator/pull/749) the Lake's internal API is
  reachable).
- **Fix (this change).** `methodologyPod` gives every pod that mounts a credential (stage, collect,
  model stage) `workingDir: /tmp`, its own emptyDir, and `PYTHONSAFEPATH=1` so Python adds no working
  directory at all (`lake_methodology_runs.go`, `methodologySafeWorkingDir`). Test
  `TestMethodologyCredentialPodsNeverStartInTheSandboxVolume` (fails on `origin/dev`: working
  directory `""`). The runtime's own half is
  [methodology-runtime#4](https://github.com/oramasearch/methodology-runtime/issues/4) (p2).

### 2.3 Network — pass

`methodology-default-deny` (`:295-303`). `methodology-transfer` lets stage and collect reach kube-dns
on 53 and this Lake's pods on the Lake's container port only (`:310-324`). The run pod without a
grant has no egress, DNS included. With a grant, `methodology-sandbox-internet` allows DNS and
`0.0.0.0/0` minus private, CGNAT, loopback, link-local and reserved ranges (`:329-357`): no cluster
pod, no other workspace, no metadata service. Serve pods accept ingress only from this Lake's pods on
8080 (`lake_methodology_deployments.go:161-183`). PR #749 adds the Lake-side halves
(`allow-methodology-ingress-to-lake` for stage and collect only; `allow-lake-to-methodology-serve`)
and is narrowly scoped; it is why 2.2 had to be fixed before it merges.

### 2.4 Cross-workspace — pass

The operator passes only the row's run or deployment id to the pod (`:443`,
`lake_methodology_deployments.go:193`); the workspace id is used only for the queue (`:549-579`).
Lake never takes a workspace from the pod: input, output, artifact and model-stage reads use the
run's or deployment's own `workspace_id` and `customer_id`
(`lake:src/api/methodology_capabilities.cpp:2172-2252`, `2293-2420`, `2511-2556`, `2730-2795`), and a
deployment may only name a training run of its own workspace (`:2659`). The registry lets a run
credential pull its build image when the build is the customer's, without comparing workspaces
(`lake:src/api/http_server.cpp:10940-10951`): [lake#952](https://github.com/oramasearch/lake/issues/952) (p4).

### 2.5 Collect and size limits — partial

Collect reads `result.json`, `artifact.bin` (only when the result says `ok`) and the last 64 KiB of
`log.txt` from `/work/out` (`runtime:orama_methodology/transfer.py:200-235`); lake bounds the inline
artifact at 24 MiB, a staged artifact at 1 GiB, the log at 64 KiB and `error`, `predictions_sample`,
`findings` at 64 KiB each (`lake:src/api/methodology_capabilities.cpp:2302-2353`), the rest by the
64 MiB request limit. Collect follows symbolic links the sandbox can plant:
[methodology-runtime#5](https://github.com/oramasearch/methodology-runtime/issues/5) (p3). The result
is self-reported by the untrusted code, by design.

### 2.6 Served models — pass

ClusterIP on 80→8080, reachable only by the Lake; no credential in the serve pod
(`lake_methodology_deployments.go:212-230`, model volume read-only); lake calls the deployment of the
caller's own customer, workspace and model (`lake:src/api/methodology_capabilities.cpp:2928`) and
stages weights from the deployment's own workspace (`:2771-2779`).

## 3. Sealed secret delivery

### 3.1 Own namespace only — pass

The produced Secret is always `resource.Namespace`/`spec.template.name`
(`lakesealedsecret_controller.go:106`, `262-263`); a Secret of that name the resource does not control
is refused (`:116`); stale owned Secrets are deleted only in the same namespace (`:564-582`). An
environment delivery is accepted only when its environment still names it, in the environment's own
namespace (`lakeenvironment_delivery.go:111-125`). App and store projections are produced in the Lake
namespace and copied into the app's workspace namespace by the LakeApp controller
(`lakeapp_controller.go:934-1000`). Tests: `TestLakeSealedSecretNeverOverwritesASecretItDoesNotControl`,
`TestSealedSecretRecordIsRefusedOutsideItsBinding`.

### 3.2 Replay — pass for cluster and namespace; workspace not bound

The envelope's AAD is `{cluster, namespace, name}` (`sealedsecret_envelope.go:25-29`), compared exactly
(`:115-126`) and authenticated by XChaCha20-Poly1305 (`:146-152`); the key derivation binds the
recipient public key (`:159-176`); one X25519 key per Lake, cluster id the Lake's UID (live
`status.sealing.cluster` is a UUID). A literal record moved to another namespace, cluster or Secret
name is refused and writes nothing (test `TestSealedSecretRecordIsRefusedOutsideItsBinding`). The
operator asks lake to seal only to its own namespace, name and key, and refuses a record lake sealed
for another namespace (test `TestLakeSourceSealsOnlyToItsOwnNamespace`).

**Not bound: the workspace.** App and store projections share the Lake namespace, so the AAD does not
name the workspace, and `mint_sealed_secret` reads any mount as `admin` without checking that it
belongs to the projection's workspace (`lake:src/api/sealed_delivery.cpp:449-488`). The check exists
only when the reference is written (`lake:src/api/web_app_capabilities.cpp:333-381`,
`lake:src/api/workspace_capabilities.cpp:1577-1593`). Lake also seals to any key when
`LAKE_SEALING_PUBLIC_KEY` is unset (`sealed_delivery.cpp:442`; the operator always sets it,
`lake_controller.go:1173`). Filed [lake#950](https://github.com/oramasearch/lake/issues/950) (p3).

`mint_sealed_secret` is reachable only with the Lake's admin bearer (`resolve_object = operator_only`,
`sealed_delivery.cpp:687`; admin bypass at `lake:src/api/registry.cpp:846-851`); the body
carries no `attestation` since [#791](https://github.com/oramasearch/orama-operator/issues/791)
(LSR-042): it used to carry the operator's own Kubernetes service-account token, unverified by lake. Creating a `LakeSealedSecret` needs Kubernetes RBAC that only the operator
and cluster administrators hold (no aggregation to `edit`/`admin` in `config/rbac` or the chart); live,
no `LakeSealedSecret` exists on the managing cluster.

### 3.3 Rotation — pass; revocation not handled

An unpinned lake source is minted again every five minutes (`lakesealedsecret_controller.go:220-229`)
and `CreateOrUpdate` rewrites the data (`:261-275`); a dynamic lease renews at `renewAt` (`:216`); a
sealing-key change re-mints (`:215`); drift is detected by a private-keyed content MAC (`:213`,
`387-424`); LakeApp copies carry a digest that rolls the pods. Tests:
`TestLakeSourceFollowsTheSecretsCurrentVersion`, `TestDynamicSourceRenewsBeforeTTL`,
`TestLakeSealingRotationRetainsUsedKey`. When lake refuses (secret deleted or destroyed), the last
Secret is kept (`:240-246`): [#777](https://github.com/oramasearch/orama-operator/issues/777) (p3).

### 3.4 No plaintext logged — pass

Neither `lakesealedsecret_controller.go` nor `sealedsecret_envelope.go` logs; status messages and
returned errors carry decode, AAD (names only), authentication-failure or lake error text, never a
value; the content MAC is an HMAC under the private key. Test
`TestLakeSealedSecretStatusNeverCarriesThePlaintext` (status, conditions and the reconcile error,
for a successful and a refused delivery).

### 3.5 `lake-secret://` references

Lake accepts only `lake-secret://` references in an app's secret environment and checks each against
the app's workspace when written (`lake:src/api/web_app_capabilities.cpp:266-290`, `333-381`, at
`:757` and `:978`). The operator still accepted a plain Kubernetes Secret name from a row, which
would copy any Lake-namespace Secret into the app: [#492](https://github.com/oramasearch/orama-operator/issues/492),
fixed by PR [#709](https://github.com/oramasearch/orama-operator/pull/709) (open). Delivery-time
binding is lake#950.

## Overlaps with the other deep searches

- [#711](https://github.com/oramasearch/orama-operator/issues/711) (apps): node and kernel sharing
  (#763) also covers methodology and serve pods; app bindings carry service credentials (lake#945).
- [#712](https://github.com/oramasearch/orama-operator/issues/712) (builds): builds produce the
  methodology image; PR #749 puts builder log words in build status.
- [#713](https://github.com/oramasearch/orama-operator/issues/713) (operator trust in rows): the
  methodology row's `image`, `runtime_image` (not digest-pinned), resources, GPU and timeout are used
  without operator ceilings; the app row's secret references (#492/#709); the mount ids lake#950 would
  check at delivery.

## The live probe (needs Steve's consent)

**Not run.** It creates a pod and is therefore a change to a cluster; it needs Steve's explicit yes for
this action. Preferred path: run it as a methodology with `network: false` and as an app on
`lake-testing` through lake's API, so the operator renders exactly the policies a tenant gets. Only if
Steve chooses it: the pod below by hand in one `lake-testing` workspace namespace, then deleted.

```sh
#!/bin/sh
# Probe what a tenant pod can reach of other workspaces' data services. Prints OPEN or closed; writes
# nothing. Fill in from read-only kubectl reads first:
#   OTHER_WS   another workspace namespace    SVC_PG  a Postgres service there (<lake>-svc-<id>)
#   SVC_KAFKA  a Kafka service there, if any  SVC_*   the other kinds, if any
t() { if nc -z -w 3 "$1" "$2" 2>/dev/null; then echo "OPEN   $1:$2 $3"; else echo "closed $1:$2 $3"; fi; }
t "${SVC_PG}-rw.${OTHER_WS}.svc.cluster.local"            5432 "other workspace Postgres"
t "${SVC_KAFKA}-kafka-bootstrap.${OTHER_WS}.svc.cluster.local" 9092 "other workspace Kafka (#770)"
t "${SVC_CH}.${OTHER_WS}.svc.cluster.local"               9000 "other workspace ClickHouse"
t "${SVC_TIDB}-tidb.${OTHER_WS}.svc.cluster.local"        4000 "other workspace TiDB"
t "${SVC_VALKEY}.${OTHER_WS}.svc.cluster.local"           6379 "other workspace Valkey"
t "${SVC_CASS}-service.${OTHER_WS}.svc.cluster.local"     9042 "other workspace Cassandra"
# As a methodology run without a grant, every line above and this one must be closed:
t kube-dns.kube-system.svc.cluster.local 53 "cluster DNS (closed in a sandbox with no grant)"
```

Expected: every line `closed`, from a workspace pod and from a methodology sandbox. Kafka is expected
`OPEN` from a namespace without its own egress rules until #770 is fixed; check it from a platform
namespace only with Steve's yes.

## Issues filed

| Repo | Issue | Priority |
|---|---|---|
| methodology-runtime | [#4](https://github.com/oramasearch/methodology-runtime/issues/4) collect and stage import modules the sandbox wrote | p2 |
| methodology-runtime | [#5](https://github.com/oramasearch/methodology-runtime/issues/5) collect follows symbolic links in `/work/out` | p3 |
| orama-operator | [#770](https://github.com/oramasearch/orama-operator/issues/770) Kafka listener open to every namespace, plaintext, no ACLs | p2 |
| orama-operator | [#771](https://github.com/oramasearch/orama-operator/issues/771) tenants receive administrator credentials | p3 |
| orama-operator | [#772](https://github.com/oramasearch/orama-operator/issues/772) ClickHouse and Cassandra built-in users left to defaults | p3 |
| orama-operator | [#773](https://github.com/oramasearch/orama-operator/issues/773) one Secret holds every tenant credential, in every Lake pod | p3 |
| orama-operator | [#777](https://github.com/oramasearch/orama-operator/issues/777) a revoked lake secret stays in its projection | p3 |
| orama-operator | [#778](https://github.com/oramasearch/orama-operator/issues/778) tenant Postgres left in the Lake namespace (live) | p3 |
| orama-operator | [#774](https://github.com/oramasearch/orama-operator/issues/774) TiDB root empty until the initializer runs | p4 |
| orama-operator | [#775](https://github.com/oramasearch/orama-operator/issues/775) operator superuser sign-in with `sslmode=prefer` | p4 |
| lake | [#945](https://github.com/oramasearch/lake/issues/945) `bind_service_to_app` gives the owner credential to write, across workspaces | p3 |
| lake | [#950](https://github.com/oramasearch/lake/issues/950) `mint_sealed_secret` does not bind the workspace | p3 |
| lake | [#952](https://github.com/oramasearch/lake/issues/952) run credential pulls any workspace's build image of the customer | p4 |
