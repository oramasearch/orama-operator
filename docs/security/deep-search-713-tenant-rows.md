# Deep search: the operator's trust in rows tenants can influence

Issue: [operator#713](https://github.com/oramasearch/orama-operator/issues/713), one of the silo-breach
deep searches of the multitenancy security review
[lake#873](https://github.com/oramasearch/lake/issues/873). Reviewed on 2026-10-21 against operator
`dev` at `66d838f` and lake `dev` at `3b436919`, with read-only reads of the managing cluster
`orama-production-cluster-01`.

**The question.** The operator turns desired state that tenants write through lake into Kubernetes
objects: rows of lake's control Postgres (apps, services, stores, store mounts, workspaces, agent
launches and schedules, builds, methodology runs and deployments, network connections, egress rules,
tunnel grants, peers, volume snapshot policies, cloud orders) and rows of the lake Zetta's
`orama_control` tables (deployments, environment values, domains). For every field a tenant can
influence: what does the operator do with it, does it check the field itself, and could it name
another tenant's namespace, Secret, host or network?

**Verdict.** No row field lets one tenant place a workload in another tenant's namespace, mount
another tenant's volume, or copy another tenant's Secret **today**. The namespace always comes from
`workspaces.namespace`, which only the operator writes, object names are derived from row UUIDs and
operator-generated slugs, images for services, agents and builders come from the Lake resource, and
NetworkPolicy selectors never take a row value. Two paths are real cross-tenant defects: **hosts**
(custom domains, and app hosts that are platform names) and **an Orama secret handed to tenant
pods**. Several isolation rules hold only because lake checks them at write time; the operator does
not refuse on its own, which the issue requires.

| # | Defect | Severity | Issue |
|---|---|---|---|
| 1 | A tenant can take another tenant's custom domain or a platform host: `planLakeDomains` gives a contested host to the lowest repository id, not the first claim, and refuses no platform zone; a held certificate is shared across owners | p2 | [operator#776](https://github.com/oramasearch/orama-operator/issues/776) |
| 2 | `create_domain` routes any host with no ownership proof, no cross-tenant uniqueness and no platform-zone refusal | p2 | [lake#949](https://github.com/oramasearch/lake/issues/949) |
| 3 | An app's explicit host bypasses lake's reserved names (`app.<wildcard>`, `login.<wildcard>`, the Lake's own host) | p2 | [lake#947](https://github.com/oramasearch/lake/issues/947) (operator half fixed here) |
| 4 | Orama's OpenAI key is copied into every workspace namespace and given to tenant agent pods | p2 | [operator#779](https://github.com/oramasearch/orama-operator/issues/779) |
| 5 | A tenant egress rule's CIDR may name private address space beyond the cluster | p3 | [operator#780](https://github.com/oramasearch/orama-operator/issues/780) |
| 6 | Row intakes trust lake's workspace routing: no `w.customer_id` comparison, no namespace ownership check | p3 | [operator#781](https://github.com/oramasearch/orama-operator/issues/781) (apps fixed here) |
| 7 | `mint_sealed_secret` delivers any mount's secret to the operator without binding it to the requesting object's workspace | p3 | [lake#971](https://github.com/oramasearch/lake/issues/971) |
| 8 | Row sizes, methodology images and row counts are bounded only by lake | p4 | [operator#782](https://github.com/oramasearch/orama-operator/issues/782) (app replicas fixed here) |
| 9 | An IP-literal peer remote opens a `/32` with no cluster or private exceptions | p4 | [operator#783](https://github.com/oramasearch/orama-operator/issues/783) |
| 10 | The apps wildcard certificate's private key is copied into every app's workspace namespace | p4 | [operator#784](https://github.com/oramasearch/orama-operator/issues/784) |

Already filed elsewhere and only cross-referenced here: plain Kubernetes Secret names in an app row's
`secret_env_refs` ([operator#492](https://github.com/oramasearch/orama-operator/issues/492), fixed by
the open [PR #709](https://github.com/oramasearch/orama-operator/pull/709)); the shared
`AGENT_SERVICE_TOKEN` in every service launch (open [PR #710](https://github.com/oramasearch/orama-operator/pull/710));
tenant builds reading Orama's Docker Hub credential ([operator#758](https://github.com/oramasearch/orama-operator/issues/758),
op#712's area); the apps wildcard defaulting to the Lake host ([operator#762](https://github.com/oramasearch/orama-operator/issues/762),
op#711); outside-destination exceptions on AKS overlay ([operator#764](https://github.com/oramasearch/orama-operator/issues/764)).

**Fixed in the same pull request as this report** (each with a test that fails on `dev`):

- an app row whose host is the Lake's own host, its public host, its preview wildcard's base, or a
  reserved platform label is `Failed` (`lakeAppHostNotPlatform`, `internal/controller/lake_apps.go:399-414`);
- more than 100 replicas is refused, lake's own ceiling (`lake_apps.go:339`, `:386`);
- a Zetta binding whose scopes are not exactly `["read"]` is refused (`lake_apps.go:470-474`);
- a synchronized LakeApp is placed only in a namespace labelled with the row's workspace **and
  customer**, and never in the Lake namespace (`lakeapp_controller.go:328-341`).

## How it was checked

- **Code read**: every `lake*_pgx.go` reader and the reconciler that renders its rows (apps, services
  and every `lake_service_*.go` kind, stores and buckets, store mounts, workspaces and their policies
  and storage, volume snapshots, store usage, workload resources, agents and schedules, builds,
  methodology runs and deployments, connections, egress rules, tunnels, peers, the network sandbox,
  orders, origin TLS), `lakeapp_controller.go`, `lakeenvironment_controller.go`,
  `lakeenvironment_delivery.go`, `zetta_deployments.go`, `deployment_controlplane.go`,
  `lakesealedsecret_controller.go`; lake's `db/schema.sql` (tables, `CHECK`s, unique indexes, the
  `enforce_workspace_routing` trigger at `:1748-1764`) and the capabilities that write each column
  (`src/api/web_app_capabilities.cpp`, `platform_capabilities.cpp`, `workspace_capabilities.cpp`,
  `build_capabilities.cpp`, `methodology_capabilities.cpp`, `network_capabilities.cpp`,
  `volume_capabilities.cpp`, `sealed_delivery.cpp`; `src/control/platform.cpp`, `network.cpp`,
  `postgres_store.cpp`). Operator line numbers are at this branch; lake's at `3b436919`.
- **Tests written**: `internal/controller/lake_apps_row_trust_test.go` (four tests, below). All four
  fail on `dev` without the fix (`host app.example.test under wildcard example.test was accepted`,
  `replicas above lake's ceiling accepted`, `bindings {"zetta":{"scopes":["read","write"]}} accepted`,
  `another customer's namespace: err = <nil>`).
- **Live, read-only** on `orama-production-cluster-01` (`kubectl get` only): `Lake/prod`'s host and
  wildcards (`lake.oramadev.net`, apps wildcard `oramadev.net`, no preview wildcard, DNS secret
  `cloudflare-api-token`); every Ingress host (`lake.oramadev.net`, `*.oramadev.net`,
  `app.oramadev.net` for Cloud v1's dashboard, two app hosts); the three LakeApps' hosts, secret
  references (none plain, none legacy), replicas and bindings; LakeEnvironments (none); workspace
  namespaces and their `orama.cloud/workspace` and `orama.cloud/customer` labels (all three carry
  both); the `ingress-nginx-admission` validating webhook.
- **Not read**: the leaf clusters, and lake's control Postgres (no read-only credential on this host
  for it). The `SELECT`s to run are in [Live probes](#live-probes).
- **Not run**: any write. The probes that create an app or a domain need Steve's consent and are
  written out below.

## 1. Names and namespaces

| Row source | What decides the namespace | Object names | Verdict |
|---|---|---|---|
| apps | `workspaces.namespace` (`lake_apps_pgx.go:71-81`), checked as a DNS label (`lake_apps.go:331-334`); the LakeApp controller now also requires the namespace's workspace and customer labels and refuses the Lake namespace (`lakeapp_controller.go:328-341`) | LakeApp `app-<sha8(lake)>-<row uuid>` (`lake_apps.go:182-185`), canonical UUID required (`:189`); children `app-<12 hex of the uuid>-…` (`lakeapp_controller.go:240-253`); an occupied name of another owner or app is `Failed` (`lake_apps.go:200-204`) | PASS (was PASS with a dependency on lake; now checked by the operator) |
| services, stores | `w.namespace` (`lake_services_pgx.go:23-33`, `lake_stores_pgx.go:20-29`) | `<lake>-<slug>`, slug `^(svc\|ws)-[0-9a-f]{12}$` / `^st-[0-9a-f]{12}$`, unique in lake (`db/schema.sql:1035,1141`) | PASS with a dependency: no `w.customer_id` comparison and no namespace label check ([#781](https://github.com/oramasearch/orama-operator/issues/781)); `services` has no routing trigger in lake |
| workspaces | `<environment>-ws-<12 hex of the id>`, derived by the operator (`lake_workspaces.go:364-387`), adopted or deleted only when its labels match (`:481-491`) | the Namespace itself (cluster-scoped) | PASS |
| agent launches, schedules | `w.namespace` (`lake_agents_pgx.go:23-37,105-120`) | `<lake>-al-<12 hex>`, slug regex | PASS with a dependency ([#781](https://github.com/oramasearch/orama-operator/issues/781)) |
| builds | `lake-<lake>-builds-<sha12(space)>`, space from `target`, which lake composes as `<authority>/<customer name>/<target>` (`build_capabilities.cpp:221`); annotation checked (`lake_builds.go:251`) | per build id | PASS; target not re-derived by the operator ([#782](https://github.com/oramasearch/orama-operator/issues/782)) |
| methodology runs, deployments | one shared `lake-<lake>-methodology-runs` (`lake_methodology_runs.go:167-174`), deny-all with per-role allows | `mr-…` slug, DB `CHECK` | PASS |
| connections, egress rules, tunnels, peers | `w.namespace` (`lake_connections_pgx.go:22-28` and siblings) | `<prefix>-<row uuid>`, refused if not controlled (`lake_connections.go:201-210`, `lake_egress_rules.go:160-164`, `lake_tunnels.go:148-156`, `lake_peers.go:217`) | PASS with a dependency: the namespace compare at `lake_connections.go:117` reads one column twice ([#781](https://github.com/oramasearch/orama-operator/issues/781)) |
| cloud orders | cluster-scoped `LakeSet`, `KubernetesCluster`, `ClusterHelmRelease`, `OramaOperator` named `order-<12 hex>` (`lake_orders.go:99-107`); offer, provider, region from fixed maps, host from the operator-owned source, never the row's `host`, gated on Stripe's paid webhook | — | PASS (the only row path to cluster-scoped objects) |
| orama_control deployments | `lake-<lake>-env-<sha10(lake, repository, environment)>` (`lakeenvironment_controller.go:152-167`); rows written only through the deployment control plane with the Lake admin token (`deployment_controlplane.go:287-353`) | LakeEnvironment `<environment>-<sha10>` (`zetta_deployments.go:535-547`) | PASS; the rows carry no customer or workspace, so a tenant-facing caller added later would lose tenancy |

**Is the namespace ever a tenant-chosen field?** No. `workspaces.namespace` is written by the operator
(`lake_workspaces_pgx.go:157`) and never by lake (no `UPDATE` of it in `postgres_store.cpp`).
`workspace_id` and `customer_id` are immutable and matched by lake's `enforce_workspace_routing`
trigger on most tables. The operator should still check, which #781 asks for and this PR does for
apps.

## 2. Images

| Source | Image | Verdict |
|---|---|---|
| apps, image mode | row `image`, must be `<ref>@sha256:<64 hex>` (`lake_apps.go:444-448`), any registry; runs restricted, non-root, no token (`lakeapp_controller.go:1357-1361`) | PASS (digest-pinned; registry choice is the tenant's own code, confined as op#711 found) |
| apps, runtime mode | the Lake's app-runtime image; tenant supplies source repository and revision | PASS (mutable default tag is [#765](https://github.com/oramasearch/orama-operator/issues/765)) |
| services, stores, agents, builders | from the Lake resource or operator constants (`lake_services.go:241-260`, `lake_agents.go:518`, `lake_builds.go:477`) | PASS |
| methodology runs | `runtime_image` (stage and collect, which hold the run credential) and sandbox `image` taken from the row; lake sets both | UNKNOWN for a lake bug: the operator does not pin them ([#782](https://github.com/oramasearch/orama-operator/issues/782)) |
| orama_control deployments | digest-pinned (`zetta_deployments.go:371`, `deployment_controlplane.go:461-476`) | PASS |

## 3. Env, args and commands

App `env_json` names are checked with `IsEnvVarName` and values are passed as plain env
(`lake_apps.go:497-514`); agent `input` becomes `AGENT_INPUT` (`lake_agents.go:505-509`); build
arguments are env read by the builder (op#712); no row value reaches `sh -c` or a command line
anywhere in scope. **PASS.** The Zetta peers shell-injection class ([#279](https://github.com/oramasearch/orama-operator/issues/279))
does not recur in row paths.

## 4. Hosts and domains

- **App hosts — FAIL, fixed in the operator, lake half filed.** Lake allows one label beneath the apps
  wildcard and unique among apps (`web_app_capabilities.cpp:205-225`, `db/schema.sql` `apps_host_idx`),
  and reserves platform names **for the app's name only** (`reserved_name`, `:135-168`, checked at
  `:714`). An explicit `host` skips it. On the managing cluster the wildcard is `oramadev.net`, next
  to `lake.oramadev.net` and `app.oramadev.net` (Cloud v1's dashboard). The operator accepted any such
  label (`lake_apps.go:359-365`); it now refuses the Lake's hosts and lake's built-in reserved labels
  (`lake_apps.go:366`, `:399-414`). Lake must refuse its configured list too
  ([lake#947](https://github.com/oramasearch/lake/issues/947)). Whether ingress-nginx's admission
  webhook refuses a second Ingress for an existing host and path is **UNKNOWN** (probe 2 below); if it
  does, the attack becomes squatting a name before the platform uses it rather than overlaying it.
- **Custom domains — FAIL** ([operator#776](https://github.com/oramasearch/orama-operator/issues/776),
  [lake#949](https://github.com/oramasearch/lake/issues/949)). `create_domain` checks only
  `valid_domain` (`platform_capabilities.cpp:513-579`); `planLakeDomains` refuses only the Lake's exact
  host and keeps a contested host for the first key in sort order, so the lowest repository id wins
  (`zetta_deployments.go:749-788`); the environment Ingress, a proxied Cloudflare record in the Lake's
  zone and a certificate follow (`lakeenvironment_controller.go:642-797`), and an existing owner's
  certificate is shared (`lake_origin_tls.go:327-329`). The Cloudflare controller never takes over an
  existing record (`cloudflarednsrecord_controller.go:34-36`), so existing platform records are safe;
  free names in the zone are not. No `LakeEnvironment` exists live, so nothing is exposed today.
- **Preview hosts**: `<dnsLabel(change)[:12]>.<previewWildcard>` (`lakeenvironment_controller.go:617-635`);
  `change` must be an algorithm-prefixed digest (`deployment_controlplane.go:43,465`), so a tenant
  cannot aim at another's preview host. PASS.
- **Named environment `host`**: taken from the deployment row without a suffix check
  (`lakeenvironment_controller.go:634`), but only the admin-token control plane writes it. PASS today;
  covered by #776's "refuse platform hosts".

## 5. Resource sizes

| Field | Operator bound | Verdict |
|---|---|---|
| app replicas | was `>= 0` only; now `<= 100` (`lake_apps.go:339`); app resources are the operator's fixed defaults (`:377-378`) | PASS (fixed here) |
| service instances, storage | 1-3 and 1-100 in the operator (`lake_services.go:293-318`) | PASS |
| store `size_gi` | 1-1024 in the operator (`lake_stores.go:144`); lake 1-100 | PASS |
| workspace `storage_gi` | none in the operator; lake allows it to instance administrators only (`volume_capabilities.cpp:104-110`) | PASS (not tenant-set) |
| methodology cpu, memory, gpus, timeout | positive only (`lake_methodology_runs.go:360-369,840`) | FAIL, defence in depth ([#782](https://github.com/oramasearch/orama-operator/issues/782)) |
| agent pods | no CPU limit (`lake_agents.go:296-297`) | FAIL, availability ([#782](https://github.com/oramasearch/orama-operator/issues/782)) |
| network rows, tunnel grants, Lake-wide service and workspace quotas | no per-workspace or per-customer cap | FAIL, availability ([#782](https://github.com/oramasearch/orama-operator/issues/782)) |

## 6. Secret and credential references

| Reference | How it resolves | Verdict |
|---|---|---|
| app `secret_env_refs`, `lake-secret://<mount>/<name>#<field>` | a LakeSealedSecret per app and secret in the Lake namespace (`lakeapp_controller.go:939-1000`), minted with the Lake admin bearer naming only `{mount, name}` (`lakesealedsecret_controller.go:474-514`); lake checks the mount is the app's workspace's at write time only (`web_app_capabilities.cpp:333-380`) | PASS with a dependency ([lake#971](https://github.com/oramasearch/lake/issues/971)) |
| app `secret_env_refs`, plain Secret name or legacy array | read from the **Lake namespace** (`lakeapp_controller.go:1070-1086`) and copied into the app's namespace; lake no longer writes them (`web_app_capabilities.cpp:266-290`); live: none | FAIL on `dev`, already [#492](https://github.com/oramasearch/orama-operator/issues/492) / [PR #709](https://github.com/oramasearch/orama-operator/pull/709); not changed here |
| bucket store credential | `spec.credential.{mount_id,name}` to the same mint (`lake_stores_buckets.go:111-112`); lake checks at `create_store` only (`workspace_capabilities.cpp:1588`) | PASS with a dependency ([lake#971](https://github.com/oramasearch/lake/issues/971)) |
| store mounts | the operator itself requires the store's workspace and customer to equal the workload's (`lake_store_mounts.go:135`), and reads the credential only when it is controlled by a Lake-owned LakeSealedSecret carrying the store id (`:274-297`) | PASS (the model the other references should follow) |
| environment `secretRefs` | any original LakeSealedSecret of the same Lake in the Lake namespace (`lakeenvironment_controller.go:489-545`); only the admin-token control plane writes them | PASS today; no tenant binding if a tenant-facing caller is added |
| Orama's own credentials in tenant pods | OpenAI key in every agent pod (`lake_agents.go:255-277,430-440`) | FAIL ([#779](https://github.com/oramasearch/orama-operator/issues/779)); service token is PR #710; Docker Hub credential in builds is #758 |

## 7. Labels, annotations and NetworkPolicy selectors

No row value is used as a label a policy selects on across namespaces. Workspace namespaces are
labelled by the operator with the row's UUIDs (`lake_workspaces.go:471-479`); app and connection
selectors are pod selectors in the row's own namespace, never a `namespaceSelector`
(`connection_controller.go:448-469`); the agent-id label is the row's UUID, resolved in the namespace.
Egress rule `dns_name` reaches only the gateway, whose egress is public-only
(`egress_rules.go:230-250`). **PASS**, except where a row adds an address block: CIDR egress rules
([#780](https://github.com/oramasearch/orama-operator/issues/780)) and IP-literal peer remotes
([#783](https://github.com/oramasearch/orama-operator/issues/783)).

## 8. Cluster-scoped objects

Reached from rows: the workspace `Namespace` (name derived by the operator), `VolumeSnapshotContent`
labels (targets written by administrators only), and the cloud-order kinds (fixed maps, paid only).
No row reaches a ClusterRole, ClusterRoleBinding, PersistentVolume, StorageClass or CRD. **PASS.**

## 9. Cross-row references

| Reference | Checked by the operator itself | Verdict |
|---|---|---|
| app → service binding | same customer, workspace, present (`lake_apps.go:477-492`) | PASS |
| app or service → store mount | same customer and workspace (`lake_store_mounts.go:135`) | PASS |
| app → Zetta binding | customer from the row; scopes now must be `["read"]` (`lake_apps.go:470-474`) | PASS (fixed here) |
| connection, egress, tunnel, peer → workload | resolved only in the row's namespace (`service_targets.go:67`, `connection_controller.go:343`) | PASS |
| methodology run → build | `LEFT JOIN builds` with no customer comparison; lake checks it (`methodology_capabilities.cpp:662-666`) | PASS with a dependency ([#781](https://github.com/oramasearch/orama-operator/issues/781)) |
| app or store → secret mount | lake only, at write time | PASS with a dependency ([lake#971](https://github.com/oramasearch/lake/issues/971)) |
| domain → host owned by another environment or app | key order, not first claim | FAIL ([#776](https://github.com/oramasearch/orama-operator/issues/776)) |

## Regression tests this review relies on

New, in `internal/controller/lake_apps_row_trust_test.go`:

- `TestLakeAppRowRefusesPlatformHosts` — the Lake's host, public host and preview base, and lake's
  reserved labels, are refused as app hosts; an ordinary host is accepted.
- `TestLakeAppRowRefusesReplicasAboveLakesCeiling` — 100 accepted, 101 refused.
- `TestLakeAppRowZettaBindingIsReadOnly` — only `["read"]` reaches the LakeApp.
- `TestSyncedLakeAppNamespaceMustBeItsWorkspaceAndCustomer` — a namespace with only the workspace
  label, with another customer's label, or the Lake namespace, is refused.

Existing, relied on: `lake_apps_test.go` (occupied names, foreign apps, service bindings in the same
workspace), `lake_store_mounts_test.go` (another workspace's store), `lake_workspaces_test.go`
(namespace ownership labels), `lakeapp_isolation_test.go` from op#711. Rules still without a test are
listed on the issues that will add them: a store of another **customer** in the same workspace id
(#781), a row whose namespace is labelled for another workspace for each non-app kind (#781), CIDR
rules over private ranges (#780, whose current test asserts the opposite), first-claim domain
ownership (#776).

## Live probes

Read-only, may be run without consent (each Lake's control Postgres, with a `SELECT`-only DSN):

```sql
-- app hosts that are platform names (lake#947)
SELECT id, customer_id, host FROM apps
 WHERE desired <> 'deleted'
   AND (split_part(host, '.', 1) IN ('lake','www','api','app','apps','admin','mcp','docs','status','mail',
       'smtp','auth','login','sso','id','cdn','static','assets','grafana','prometheus','argocd','registry',
       'git','dev','staging','prod','test')
    OR split_part(host, '.', 1) LIKE 'lake-%' OR split_part(host, '.', 1) LIKE 'orama%');
-- rows whose workspace belongs to another customer (#781)
SELECT 'services', s.id FROM services s JOIN workspaces w ON w.id = s.workspace_id WHERE w.customer_id <> s.customer_id
UNION ALL SELECT 'apps', a.id FROM apps a JOIN workspaces w ON w.id = a.workspace_id WHERE w.customer_id <> a.customer_id
UNION ALL SELECT 'stores', t.id FROM stores t JOIN workspaces w ON w.id = t.workspace_id WHERE w.customer_id <> t.customer_id;
-- egress rules over private space (#780)
SELECT id, workspace_id, cidr FROM network_egress_rules
 WHERE cidr IS NOT NULL AND (cidr <<= '10.0.0.0/8' OR cidr <<= '172.16.0.0/12' OR cidr <<= '192.168.0.0/16'
       OR cidr >>= '10.0.0.0/8' OR cidr <<= '100.64.0.0/10');
```

### Probes that need Steve's consent

They write to lake on `lake-dev` (nonprod), as a throwaway test user, and are removed afterwards.

1. **Reserved host through an explicit host** (lake#947): `create_app {name: "probe713", mode: "image",
   image: "<any>@sha256:<digest>", port: 8080, host: "app.<apps wildcard>"}`. Expected after lake's
   fix: `invalid_input` at `/host`. Before: lake accepts, and with this PR the operator marks the row
   `Failed` with `app host label "app" is reserved for the platform`.
2. **Host overlap in ingress-nginx**: with consent, on `lake-dev`, a server dry-run of an Ingress for
   an existing app's host and path `/` in another workspace namespace
   (`kubectl --dry-run=server apply`) shows whether the admission webhook refuses duplicates.
   A server dry-run writes nothing, but it is listed here because the manifest is hand-written.
3. **Domain claim order** (#776): two test tenants, each with a deployed environment; tenant A claims
   `probe713.<test zone>`, then tenant B (whose repository id sorts lower) claims the same host. Before
   the fix B's environment serves it on the next poll; after, B's row is `failed`.
