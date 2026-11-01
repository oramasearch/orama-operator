# Observability

How the operator gives every cluster metrics, alerts, dashboards and live tracing, with nothing
outside our clusters and nothing that is not permissively licensed. This is
[operator#15](https://github.com/oramasearch/orama-operator/issues/15) as amended by
[#456](https://github.com/oramasearch/orama-operator/issues/456) (Pixie, implemented by this
operator, nonprod isolated), [#1059](https://github.com/oramasearch/orama-operator/issues/1059)
(production's hub on the managing cluster), [#455](https://github.com/oramasearch/orama-operator/issues/455)
(Pixie's images from our copy) and [#437](https://github.com/oramasearch/orama-operator/issues/437)
(the secure link between clusters).

## Decisions this follows

| Decision | Source |
|---|---|
| Monitoring is in-cluster Prometheus and Alertmanager, on every cluster identically. | Steve, #15, 2026-10-13 |
| Grafana, Loki, Mimir and Tempo are AGPL-3.0 and ruled out. Everything is permissive, for resale and for customers' own clusters. | Steve, #15 |
| Pixie for live tracing, installed by our operator: no Pixie operator, no OLM, versions pinned, auto-update off. | Steve, #15, #456 |
| Pixie Cloud is self-hosted, for production and on nonprod for nonprod; never Pixie's or New Relic's hosted service, never Auth0. | Steve, #456 |
| Production's hub (its Prometheus, Alertmanager routing, dashboards, Pixie Cloud and their databases) runs on the managing cluster. No separate support cluster is provisioned: *"let's put the observability databases and functionality on the management cluster instead of provisioning a prod support cluster"*. | Steve, 2026-10-29, #1059 |
| Production agents report to the managing cluster's hub; nonprod agents and their hub stay on nonprod. The operator enforces it. | #456, #1059 |
| Agents reach a hub on another cluster only through #437's peer link (zetta-tunnel, mutual TLS 1.3, `PeerConnection` + `PeerLink`), with a deploy key of their own. | #456, #437 decision D6 |
| Everything runs on-prem and air-gapped: images from our private registry, nothing calls out at run time. | #456 |

## What runs where

| Cluster | Platform layer (every cluster) | From resources |
|---|---|---|
| Every cluster | kube-prometheus-stack (Prometheus, Alertmanager, kube-state-metrics, node-exporter) and Perses, from `helmfile.yaml` and `config/clusters/*/platform.yaml` | The rules, scrape configuration, routing and dashboards of what runs on it |
| A cluster with an `ObservabilityAgent` | | `PixieVizier` in namespace `pl`: Pixie's node agents and Vizier services, and (for a hub on another cluster) the read-only metrics federation proxy |
| A cluster with an `ObservabilityHub` | | `PixieCloud` in namespace `plc`: self-hosted Pixie Cloud, and the hub Prometheus that keeps every cluster's metrics, its dashboards and the rules that page when a cluster is lost |

| Hub on | Serves agents on |
|---|---|
| the managing cluster (no `clusterRef`) | `production` clusters |
| a `nonprod` cluster | `nonprod` clusters |

An agent with no `clusterRef` runs on the managing cluster itself, like production's hub: it is
production, reaches the hub without a link, and is registered under the hub's name
(`orama-production-cluster-01`), the same name the hub already gives its own metrics. A hub's own
cluster has an agent on both hubs: nonprod's names `orama-nonprod-cluster-01`, production's has no
`clusterRef` (`config/clusters/observability-hub.yaml`).

Every controller runs on every cluster, from the same chart; a cluster is a hub or has agents only
because the resources say so (the *one operator* rule). Prometheus, Alertmanager and Perses are the
same release with the same values everywhere (the *two installation paths* rule;
`cmd/platform_sync_test.go`).

Lake's workspace metrics are not here: they live in each workspace's Zetta. This stack is for
operating the clusters and the services Orama runs.

## The kinds

Principle 10: the abstract kinds name the thing, a provider resource names the vendor's release,
and the provider's type decides the concrete kinds.

```
managing cluster                         hub's cluster (plc)        agent's cluster (pl)
ObservabilityProvider (type: pixie) ──┐
ObservabilityHub ─────────────────────┼─▶ PixieCloud
ObservabilityAgent ───────────────────┴──────────────────────────▶ PixieVizier
   └─ PeerLink ────────────────────────▶ PeerConnection ◀──mTLS──▶ PeerConnection
```

| Kind | Where | What |
|---|---|---|
| `ObservabilityProvider` | managing cluster, cluster-scoped | The release: the `oramasearch/pixie` commit and every image by digest. |
| `ObservabilityHub` | managing cluster | One purpose's hub: its cluster, domain, organisation, administrator, storage, alert receivers. |
| `ObservabilityAgent` | managing cluster | One cluster's agents and the hub they report to. |
| `PixieCloud` | hub's cluster, `plc` | Written by the hub controller; reconciled by that cluster's operator. |
| `PixieVizier` | agent's cluster, `pl` | Written by the agent controller; reconciled by that cluster's operator. |

The managing cluster does what needs more than one cluster: the purpose check, the link, copying
each cluster its own credentials. Each cluster's own operator runs what is on it (the management and
leaf split in [`README.md`](README.md#who-owns-what)). Field reference: [`crd-reference.md`](../crd-reference.md#observabilityprovider-separate-crd-cluster-scoped--an-observability-vendors-release).

Pixie is a vendor's software, so it gets a provider kind; Prometheus, Alertmanager and Perses are
the platform layer and are the same whichever tracing provider is used.

Why each kind exists, and why none is a copy of another:

| Kind | The need it owns that nothing existing does |
|---|---|
| `ObservabilityProvider` | One pinned release (commit and digests) shared by every hub and agent, so all clusters run the same build and a commit mismatch with the vendored manifests is refused in one place. Existing provider kinds hold an account's credentials; this holds a release. |
| `ObservabilityHub`, `ObservabilityAgent` | Decisions that need more than one cluster: the purpose check from two `KubernetesCluster`s, the link (a `PeerLink` and two `PeerConnection`s), and copying a key minted on one cluster to another. They live where the kubeconfigs are. |
| `PixieCloud`, `PixieVizier` | What a cluster's own operator reconciles there: Deployments, minted secrets, the bootstrap, status from the cluster's workloads. A managing cluster cannot reconcile pods on a leaf; this is the `KubernetesCluster` → `AksCluster` split across clusters. |

The concrete spec is not a second copy to edit: only the managing cluster writes it, whole, from the
abstract resource on every pass (server-side apply), so a change made to it on the cluster is undone.
Nothing owns anything across a cluster boundary: copies and concrete resources carry the hub's or
agent's label, and the managing cluster deletes them itself when the hub or agent goes.

## Purpose isolation

`KubernetesCluster` carries a `purpose` tag, `nonprod` or `production`. The managing cluster has no
`KubernetesCluster` of its own: a hub with no `clusterRef` runs there, and the managing cluster counts
as production, because it already holds every production credential and manages the production
clusters (`observability_purpose.go`). The rule is read from the resources, so it is the same on
every cluster's operator: a cluster that manages no production cluster has no production agent that
could report to a hub without a `clusterRef`.

| Rule | Enforced by |
|---|---|
| A hub runs only on the managing cluster (no `clusterRef`) or on a `nonprod` cluster, never on a production cluster beside customer workloads. | `ObservabilityHub` is `Refused`; nothing is written. |
| An agent reports only to a hub of its purpose: production to the managing cluster's hub, nonprod to a nonprod cluster's hub. | `ObservabilityAgent` is `Refused` and anything it had placed is removed; the hub controller also leaves it out of the `PixieCloud`'s `agents`, so the hub never issues it a key. |
| Production credentials never reach nonprod. The managing cluster's hub copies its Secrets within the managing cluster. | The production-credential rules of the sets and `OramaOperator` ([`crd-reference.md`](../crd-reference.md#production-credentials-never-reach-a-nonprod-member)), applied to every Secret the observability controllers copy. |
| Nothing from production is reachable from nonprod. | A nonprod agent never gets a link to production's hub, and every Pixie namespace is default-deny. |

## The link to the hub

An agent on the hub's own cluster dials vzconn directly (`vzconn-service.plc.svc:51600`), allowed by
both namespaces' sandboxes. An agent on another cluster gets #437's peer link:

| Object | Cluster | What |
|---|---|---|
| `PeerConnection plc/pixie-agent-<agent>` | hub's | Exports only `vzconn-service:51600`, as `pixie-cloud`. |
| `PeerConnection pl/pixie-hub` | agent's | Imports only `pixie-cloud`, as Service `peer-pixie-cloud`. |
| `PeerLink pixie-<agent>` | managing | The pair's trust root and one certificate per end; peer id is the agent's UID. |

The gateways speak mutual TLS 1.3 and admit only the other end's exact identity
([`peer-connections.md`](peer-connections.md)). Inside that, Vizier's own connection is TLS to the
hub, verified: Pixie's cloud connector checks the hub's certificate against the system roots and has
no option for a private CA, and it skips verification entirely for an address under
`.svc.cluster.local`. So the operator gives it the hub's CA as Go's root file (`SSL_CERT_FILE`),
issues the hub's proxy certificate for `peer-pixie-cloud.pl.svc` and `vzconn-service.plc.svc`, and
renders no other address (`checkVizierCloudAddress`). Pixie dials gRPC, and gRPC v1.43.0 (the version
Pixie's `go.mod` replaces in) sets the TLS server name to the dial address's host
(`credentials/tls.go`), so Go checks both the hub's CA and that the certificate names that host.
`TestHubCertificateIsVerifiedForTheDialedAddress` runs that handshake with the issued certificates:
it succeeds for the two addresses and fails for another name or another CA.

## Credentials, one set per cluster

| Credential | Minted by | Lives |
|---|---|---|
| Deploy key | Pixie Cloud's API (`CreateDeploymentKey`), called by the hub's operator, one per accepted agent | `plc/pixie-deploy-key-<agent>` (`deploy-key`, `id`) on the hub; copied only to that agent's cluster as `pl/pl-deploy-secrets`. Kept while Pixie still lists its id; a rebuilt hub gets a new one. Removing the agent calls `DeleteDeploymentKey`, then deletes the Secret. |
| Vizier's CA, service certificates, signing key | the agent's operator | `pl/pixie-vizier-ca`, `pl/service-tls-certs`, `pl/pl-cluster-secrets` (the cloud connector adds `cluster-id` and `cluster-name` there after it registers; the operator never changes them). |
| The hub's two CAs (services; what agents and people reach), certificates, JWT, session and Hydra secrets | the hub's operator | `plc/pixie-service-ca`, `plc/pixie-cloud-ca`, `plc/service-tls-*`, `plc/cloud-proxy-tls-certs`, `plc/cloud-auth-secrets`, `plc/cloud-session-secrets`, `plc/pl-hydra-secrets`. The public trust root is `PixieCloud.status.caCertificate`, copied to each agent as `pl/pixie-cloud-ca`. |
| One Postgres role and database per Pixie service, each with its own encryption key | the hub's operator | `plc/pixie-db-<service>`; CloudNativePG managed roles, never superuser, 30 connections each. vzconn and the artifact tracker never open the database and get no credentials. |
| The bootstrap role | the hub's operator | `plc/pixie-db-bootstrap`: may only read and insert `orgs`, `users`, `user_attributes` and `user_settings` in the profile database (granted by their owner when missing), two connections. |
| OpenSearch users | the hub's operator | `plc/pixie-search`: `pixie-indexer` writes the metadata indices, `pixie-api` only reads them; only bcrypt hashes reach OpenSearch. |
| The administrator's password | the hub's operator | `plc/pixie-admin`. Upstream creates `admin@default.com` with password `admin`; that container is removed. |
| Each user's password | the hub's operator | `plc/pixie-user-<email>`, keys `email` and `password`, one per email in the hub's `users`. |
| Registry credentials and alert receiver URLs | people, on the managing cluster | Copied to each cluster under the production-credential rules. Give each cluster its own pull Secret. |

Copies on other clusters carry the hub's or agent's label, never an owner reference across
clusters; they are re-applied every pass (a deleted copy comes back) and all of them are deleted
when the hub or agent is (`deleteLabelledSecrets`). Generated values are minted once and never
rewritten. Certificates live 90 days and are reissued 30
days before they expire; each pod template carries a hash of what it reads, so a reissue rolls the
pods that use it (Pixie's services read certificates only at start).

## How Pixie is installed

`hack/fetch-pixie-manifests.sh` renders, with `kubectl kustomize`, the manifests of one pinned commit
of `oramasearch/pixie` (`4b445e0b7907`, upstream `main` on 2026-10-13, which has the ClickHouse
export of upstream #2395) into `internal/controller/assets/pixie/`. The operator embeds them,
refuses a bundle that is not exactly the expected objects (`vizierObjects`, `pixieCloudObjects`), and
sets every value on the decoded objects, never as text. `TestPixieManifestCommitMatchesAssets` and
`TestPixieBundlesUseOnlyMappedImages` keep the commit and the image map honest.

**Vizier**: Vizier with persistent metadata, x86-64, and its NATS. Not applied: the updater (it would
change versions on its own), the cert provisioner Job (the operator mints the certificates), and the
RBAC of Pixie's own `Vizier` resource (without Pixie's operator the cloud connector reads its address
from `pl-cloud-config`). `PL_DISABLE_AUTO_UPDATE` is `true`.

**Cloud**, kept from upstream: the services and Ory Kratos and Hydra. Replaced or removed:

| Upstream | Licence | Here |
|---|---|---|
| Elasticsearch 7 through Elastic's ECK operator, and Kibana (`k8s/cloud_deps`) | Elastic License / SSPL / AGPL depending on version and build | **OpenSearch 2.19** (Apache-2.0), a single node run by the operator, with its security plugin on (TLS on both layers, two least-privilege users). The indexer runs with manual index management, so it never calls Elastic's ILM API, and the operator creates its index. OpenSearch reports Elasticsearch 7.10.2 to Pixie's client. |
| One Postgres Deployment shared by every service | PostgreSQL | CloudNativePG, two instances, one database and role per service. |
| The metrics server | Apache-2.0 | Not run: it only exports usage to Google BigQuery. |
| The plugin loader Job | Apache-2.0 | Not run: it downloads plugin definitions from GitHub (`pixie-io/pixie-plugin`) at run time. Export plugins (the ClickHouse retention path) wait for a mirrored plugin artifact from #455. |
| Sentry, Segment, LaunchDarkly, announcements, contact, support access | outside services | Off; the keys exist, empty. |
| The artifact tracker's public manifest and Google Cloud Storage | outside services | Versions pinned to the release (`vizierVersion`, `cliVersion`), so it never polls; no credentials mounted. |
| PxL scripts from `artifacts.px.dev` and a GCS bucket | outside services | Served inside the cluster by `pixie-scripts` (image `script-bundle`): to the UI through the proxy at `/pxl_scripts/` on the page's own origin (`SCRIPT_BUNDLE_URLS` is the path `/pxl_scripts/bundle.json`: the UI runs on `work.<domain>`, and a bundle on `<domain>` is cross-origin, which the browser refuses without CORS headers), to the API server at `http://pixie-scripts.plc.svc:8080/pixie-scripts/bundle.json` (its own `SCRIPT_BUNDLE_URLS`; the hub's domain does not resolve in the cluster), and to the script manager through Google's storage client, whose v1.10.0 sends object reads to `http://$STORAGE_EMULATOR_HOST/<bucket>/<object>`. Its once-a-minute refresh asks Google's metadata API, which the sandbox blocks (an error logged each minute), so the script manager reads the bundle at start and rolls when the bundle image changes. The API server's own copy needs `hack/pixie-patches/0001`. |
| Auth0, Google sign-in | outside services | Not configured. Sign-in is Ory (Kratos, password), with the administrator the operator creates. Clerk as Kratos' OIDC provider is a later step and needs the internet, so the on-prem default stays Ory. |

### The bootstrap

What a person would do after installing Pixie Cloud, done by the hub cluster's operator
(`pixie_cloud_bootstrap.go`). The index comes first, as soon as OpenSearch is ready; the rest once
the hub is Ready:

1. **The metadata index**, created in OpenSearch with the indexer's own body (vendored from
   `src/cloud/indexer/md/mapping.o.go` as `assets/pixie/md-index.json`). The indexer compares its
   desired settings with the live index and reindexes on any difference, so the body must be its
   own. It is created before the hub is Ready because the indexer, with manual index management,
   exits when the index is missing, and the hub is not Ready without the indexer.
2. **The administrator and the users**, through Kratos' admin API: `adminEmail` with the password in
   `plc/pixie-admin`, and each email of `users` with its own generated password in
   `plc/pixie-user-<email>` (every character a Secret name cannot hold becomes `-`, so
   `steve@orama.com` is `pixie-user-steve-orama.com`). An email removed from `users` loses its Kratos
   identity, so its sign-in, and then its Secret.
3. **The organisation, and everyone's user in it.** Pixie has no API that creates the first
   organisation or user without someone signing in, so this one step is a schema adapter: it reads
   first and writes only what is missing, as the dedicated `bootstrap` role, against the vendored
   profile migrations (`TestPixieBootstrapMatchesVendoredSchema` checks every column it writes and
   that nothing it leaves out is required). The organisation's id is a function of its name.
   **The organisation's domain is the administrator's email domain.** Pixie's sign-in refuses a member
   of an organisation that has no domain unless the organisation is named after that member's email
   (`src/cloud/auth/controllers/login.go`, `loginSimple`), with *"Our system found an issue with your
   account"*. Hubs bootstrapped before v0.11.8 had none and turned everyone away; the bootstrap sets
   it on them (the role may update `orgs.domain_name` and nothing else of an existing row).
   **Each user also gets its `user_attributes` and `user_settings` rows**, which Pixie's own
   `CreateUser` writes with every user. Pixie only ever updates them, so without them a setting saved
   in the UI matches no row and is lost: the welcome tour came back on every page load. Users created
   before v0.11.12 get them on the next reconcile.
4. **Deploy keys, through Pixie's own API**: GraphQL `CreateDeploymentKey`, `deploymentKeys`,
   `DeleteDeploymentKey` and `clusters`, as the administrator, with a five-minute token the operator
   signs with the hub's signing key exactly as Pixie's auth service issues one
   (`TestPixieUserTokenIsWhatPixieAccepts`, `TestPixieAPIDeployKeyLifecycle`). Pixie generates
   every key; nothing writes Pixie's key tables.

### OpenSearch in place of Elasticsearch, checked against the sources

This is a reading of the pinned sources, not a run: it shows nothing in Pixie's use is known to be
missing from OpenSearch 2.19, and it does not prove the indexer and autocomplete work end to end.
That is step 4 of the rollout.

| Question | Evidence (pinned versions) |
|---|---|
| Does Pixie's client accept OpenSearch? | Pixie uses `olivere/elastic` v7.0.12. Its `NewClient` runs a `HEAD /` healthcheck with basic auth and checks neither the product header nor the version. OpenSearch also reports 7.10.2 with `compatibility.override_main_response_version` (present in 2.19.6, `TransportMainAction`). |
| Does the index definition load? | The mapping uses the camelCase `nGram` tokenizer. OpenSearch 2.19.6 accepts it with a deprecation warning (`CommonAnalysisPlugin`); **OpenSearch 3.x refuses it** for new indices. So the provider's `opensearch` image must be 2.19.x until Pixie's mapping says `ngram`. `custom_token_chars`, `char_group`, `word_delimiter`, `max_ngram_diff` and `index.store.preload` all exist in 2.19. |
| Does anything call Elastic-only APIs? | Only managed index creation (ILM policies, rollover aliases, `esutils/managed_index.go`), which `PL_MD_MANUAL_INDEX_MANAGEMENT=true` skips. With it the indexer calls only index exists, put mapping, get and put settings, and document writes; the API server only searches. The `scripts` index it names is never queried. |
| What needs a source change? | The API server's two hard-coded public script-bundle URLs: `hack/pixie-patches/0001-api-script-bundle-urls.patch` for #455's `orama` branch. Nothing in the OpenSearch path needs a patch. |

### Licences of what is installed

| Component | Licence |
|---|---|
| Pixie (Vizier, Cloud, UI), from `oramasearch/pixie` | Apache-2.0 (`assets/pixie/LICENSE` is kept beside the vendored manifests) |
| Ory Kratos, Ory Hydra | Apache-2.0 |
| NATS | Apache-2.0 |
| OpenSearch | Apache-2.0 |
| Envoy | Apache-2.0 |
| CloudNativePG, PostgreSQL | Apache-2.0, PostgreSQL License |
| Prometheus, Alertmanager, prometheus-operator, kube-state-metrics, node-exporter | Apache-2.0 |
| Perses | Apache-2.0 |
| k8s-sidecar (Perses' provisioning sidecar) | MIT |

`TestEveryClusterRunsPermissiveMetricsAlertingAndDashboards` fails if either installation path
installs a Grafana, Loki, Mimir, Tempo, Elasticsearch, ECK or Kibana chart, or leaves
kube-prometheus-stack's Grafana subchart on; `TestRenderVizier` and `TestRenderPixieCloud` fail if a
rendered pod runs an image that is not the release's (and so any public or Elastic image).

### Images

Every image is the release's, by digest, from our private registry (#455). The release must name
exactly these keys, or the provider is not Ready:

| Keys | Built from |
|---|---|
| `vizier-pem`, `vizier-kelvin`, `vizier-metadata`, `vizier-query-broker`, `vizier-cloud-connector` | `//k8s/vizier:vizier_images_push` (the cert provisioner and updater are not needed) |
| `cloud-api`, `cloud-artifact-tracker`, `cloud-auth`, `cloud-config-manager`, `cloud-cron-script`, `cloud-indexer`, `cloud-plugin`, `cloud-profile`, `cloud-project-manager`, `cloud-proxy`, `cloud-scriptmgr`, `cloud-vzconn`, `cloud-vzmgr` | `//k8s/cloud:cloud_images_push` (the metrics server and plugin loader are not needed) |
| `nats`, `curl`, `postgres` | mirrors of the images Pixie's manifests name (`ghcr.io/pixie-io/...`); `postgres` is a CloudNativePG operand image, named by tag and digest (`:16.15@sha256:…`) because CloudNativePG reads the version from the tag |
| `envoy`, `hydra`, `hydra-cli`, `kratos` | mirrors of `envoyproxy/envoy`, `oryd/hydra` (`-sqlite` and `-alpine`), `oryd/kratos` at the versions the manifests pin |
| `opensearch` | a mirror of `opensearchproject/opensearch` 2.19 |
| `script-bundle` | new in #455: `hack/pixie-script-bundle` (nginx, non-root) serving `bundle-oss.json`, built with `px create-bundle` at the same commit, on port 8080 at `/pixie-scripts/bundle.json` |

Pixie's operator image and OLM are not needed.

### Air-gapped: every image and artifact inventoried, mirrored, and nothing public

[`hack/observability-images.yaml`](../../hack/observability-images.yaml) lists every image and chart
this work runs, with the upstream it is mirrored from and its licence:

- **Pixie**: the release keys above, each a private `oramasearch/pixie-*` repository, by digest.
  A missing or tag-only key keeps the provider not Ready; nothing Pixie runs.
- **The platform layer**: every image the two charts render is overridden in their values to a
  private mirror, `docker.io/oramasearch/platform-<name>` at the upstream tag, pulled with the
  cluster's own Secret (`ClusterHelmRelease` `imagePullSecretRef`; on the managing cluster,
  [`install.md`](../install.md)). A mirror that does not exist fails the pull: there is no fallback
  to a public registry. Perses' helm test pod, the one public image the chart renders, is off.
- **The charts** are vendored in `hack/charts/` and checked against their recorded sha256.
  `TestPlatformChartsRenderOfflineWithOnlyMirroredImages` renders them offline with the exact
  values and fails on any image outside the inventory or any pod without the pull Secret;
  `TestPixieInventoryMatchesReleaseKeys` keeps the Pixie list equal to what the operator renders.
- **The PxL script bundle** is the `script-bundle` image, served inside the hub.
- **What the operator runs for the hub's metrics** (`operator`): the hub Prometheus uses the
  platform's Prometheus mirror, and each leaf's federation proxy `platform-envoy`, a mirror of the
  Envoy the Pixie `envoy` key pins, so the hub's metrics need no Pixie image.
  `TestHubPrometheusIsPinnedAndScoped` checks both are inventoried.

The charts themselves are still fetched from their public repositories by `helmfile` and
`ClusterHelmRelease`, as every platform chart is today; an on-prem install without internet pulls
the vendored tarballs from its own chart repository (OCI, `spec.chart.credentialsSecretRef`).

### Budgets

Every container has requests and limits, and every binary is held to its limit by its own setting:
Go services get `GOMEMLIMIT` at 90% of their memory limit; the node agent's table store
(`PL_TABLE_STORE_DATA_LIMIT_MB`) is 60% of `nodeMemory`, Pixie's own ratio; OpenSearch's heap is
1 GiB of its 2 GiB; Postgres' `shared_buffers` is 256 MB of 1 GiB; NATS JetStream keeps at most a
quarter of its memory limit in memory and 90% of its volume (`spec.storage.messaging`) on disk. The tables are
`vizierBudgets` and `pixieCloudBudgets`; a vendored container without one fails rendering.

| Per cluster | Requests | Limits |
|---|---|---|
| Vizier (2 GiB node agents) | about 2 GiB and 0.5 CPU, plus 2 GiB and 0.2 CPU per node; a 16 Gi metadata volume | about 4 GiB, plus 2 GiB per node |
| Pixie Cloud (hub) | about 6.5 GiB and 2 CPU; 90 Gi of volumes by default (Postgres 2 × 20 Gi, search 20 Gi, JetStream 3 × 10 Gi) | about 11 GiB |
| Prometheus stack and Perses (every cluster) | about 2.5 GiB and 0.7 CPU, plus node-exporter per node; 52 Gi of volumes | about 5.2 GiB |

## Metrics, alerting and dashboards

Every cluster runs kube-prometheus-stack and Perses from the platform layer:

- **Prometheus** keeps 15 days or 45 GB on a 50 Gi volume, and selects PodMonitors and
  PrometheusRules in every namespace. kube-state-metrics, node-exporter and the kubelet cover every
  pod, node and volume.
- **Alertmanager** routes through one `AlertmanagerConfig` per cluster, labelled
  `orama.cloud/alerting`, which the operator writes in `pl` (or `plc` on a hub cluster with no
  agent): every alert of the cluster, of the severities a receiver takes, to that receiver, titled
  with the cluster's name. It does not wait for Pixie: the agent controller writes the
  `PixieVizier`, with the routes, as soon as the purposes match, and the cluster's operator writes
  the routing first; Vizier itself waits for the release, its key and the hub's CA.
- **Perses** (read-only) shows every ConfigMap labelled `perses.dev/resource: "true"`.
- **Rules** are kube-prometheus-stack's defaults, without those for what AKS runs itself (etcd, the
  controller manager, the scheduler; Cilium replaces kube-proxy) and without `KubeCPUOvercommit` and
  `KubeMemoryOvercommit`. Those two fire whenever the pods' requests would not fit with the largest
  node gone, which on a cluster sized to what it runs is nearly always. Every cluster autoscales its
  pools instead of holding a spare node, so a lost node is replaced, and a pod that cannot be placed
  meanwhile pages as `KubePodNotReady` (Pending for 15 minutes).
  `KubeCPUQuotaOvercommit` and `KubeMemoryQuotaOvercommit` are off too. They fire when the namespaces'
  quotas add up to more than 1.5 times the nodes, and Lake's workspace quotas (`workspace-defaults`, 4 CPU
  and 8 GiB each) are ceilings, meant to add up to more than the cluster. A workspace that reaches its own
  quota still pages, as `KubeQuotaExceeded`.
- kube-state-metrics turns the operator's own status into metrics, so an alert says what the
  resource says: `orama_oramacloudv1_phase` and `orama_oramacloudv1_component_ready`,
  `orama_pixiecloud_phase` and `orama_pixiecloud_agent_connected`, `orama_pixievizier_phase`,
  `orama_peerconnection_phase`.

What the operator writes:

| Next to | Scrape | Rules | Dashboards |
|---|---|---|---|
| Each `OramaCloudV1` | PodMonitors: the writer's and readers' `/metrics` on :8080 (by number: a reader's Envoy also names a port `http`), and the control database's exporter (:9187). The stack's sandbox admits only the platform's Prometheus pods, on those two ports. | A critical component (writer, reader, broker, control database, cache, dashboard) not ready for 10 minutes pages; the rest after 30 minutes as a warning. The engine's or control database's metrics gone for 5 minutes; search p95 over 0.5 s for 15 minutes; replica lag over 5 minutes. | `<stack>-overview` (health, traffic, resources) and one per component, in the namespace's Perses project. |
| Each `PixieVizier` | | Agents not Ready for 15 minutes; the link to the hub down for 15 minutes (critical). Evaluated on the agent's cluster, so they fire when the hub is unreachable. | |
| Each `PixieCloud` | Federates every cluster it serves into the hub Prometheus (below). | The hub not Ready for 15 minutes; an agent cluster's Vizier not heartbeating for 10 minutes. The hub-scoped rules of the next section. | The hub's Perses project (next section). |

Not scraped yet: the broker. RabbitMQ's Prometheus plugin is not enabled on the broker, and enabling
it restarts the broker, so it waits for a planned window; until then the broker is covered by its
readiness and the writer's enqueue latency.

Rolling the operator out changes nothing on a Cloud v1 stack until the platform layer is applied:
without the monitoring CRDs it writes no monitoring object, dashboard or sandbox allowance
(`TestOramaCloudV1MonitoringWaitsForThePlatformLayer`). A `PixieCloud` or `PixieVizier` then shows
`Alerting=False` (`MonitoringCRDsMissing`); every other resource still reconciles.

Writes are all or nothing: `applyMonitoringObjects` looks up every kind first, so a platform layer
installed in part gets nothing written and the condition says so (`TestMonitoringObjectsAreAllOrNothing`).

Rules on metrics that may be absent have an `absent()` companion, so a missing target is an alert,
never a silent pass: `OramaEngineTargetsMissing` and `OramaControlDBTargetsMissing` (critical),
`OramaStackStatusMetricsMissing`, and `ObservabilityAgentStatusMissing` on agents and hubs.

## Reaching the UIs

Nothing is exposed publicly: no Ingress, no DNS record. People reach each UI with `kubectl
port-forward` on the cluster it runs on, so access is exactly who holds that cluster's credentials.
Perses (read-only), Prometheus and Alertmanager have no sign-in of their own.

```sh
C=orama-production-cluster-01   # or orama-production-cluster-02, -03, orama-nonprod-cluster-01
kubectl --context $C -n monitoring port-forward svc/perses 8080:8080                            # http://localhost:8080
kubectl --context $C -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090    # http://localhost:9090
kubectl --context $C -n monitoring port-forward svc/kube-prometheus-stack-alertmanager 9093:9093  # http://localhost:9093
```

A Cloud v1 stack's dashboards are in its namespace's Perses project, on its own cluster. The hub's
dashboards (project `observability-hub`) are in the Perses of the hub's cluster: the managing cluster
for production, `orama-nonprod-cluster-01` for nonprod.

The Pixie UI answers on the hub's `domain` and `work.<domain>` through `cloud-proxy-service` in `plc`
(port 443). Script runs go to the proxy's Envoy on port **4444** (gRPC-web, from `work.<domain>`), whose CORS allows the hub's `domain` and its subdomains (upstream allows only `dev.withpixie.dev`, which refused every script run), so forward 4444 too. The browser must reach both names **on port 443**: the proxy redirects `<domain>` to
`https://work.<domain>/` without the port, so a forward on any other local port breaks at the first
redirect. Each person signs in with their own email: the hub's `adminEmail` (password in
`plc/pixie-admin`) or one of its `users` (password in `plc/pixie-user-<email>`). The browser does not
trust the hub's CA until `PixieCloud.status.caCertificate` is imported. The step-by-step, for every
UI on every cluster, is techops-docs'
[observability runbook](https://github.com/oramasearch/techops-docs/blob/main/docs/platform/observability-runbook.md).

## The hub's metrics: every cluster of its purpose, kept and paged on the hub

Each cluster keeps its own Prometheus and Alertmanager for its own alerts. That cannot cover the
cluster itself: a lost leaf takes both with it. So the hub runs one more Prometheus, in `plc`, that
federates every cluster it serves, keeps the series for `metricsRetention` (default 30 days) on its
own volume (`storage.metrics`, default 100 Gi), shows them in the hub's dashboards, and pages from the
hub (`observability_metrics.go`).

**None of it waits for Pixie.** The hub writes its Prometheus, rules and dashboards, and each leaf its
proxy, before anything of Pixie: no Pixie image, deploy key, registration or heartbeat is needed, so
a cluster's metrics reach the hub and its loss pages while Pixie is still missing or broken. They need
the platform layer's monitoring CRDs, the cluster's pull Secret, and two platform mirrors: the
Prometheus the platform already runs and `platform-envoy`, the Envoy Pixie also pins
(`hack/observability-images.yaml`, `operator`). The hub's objects carry `orama.cloud/pixie: metrics`,
so Pixie's own apply and prune never touch them.

**The data path, for a leaf on another cluster:**

```
leaf: platform Prometheus (monitoring) <- metrics-federation (pl: Envoy, GET /federate only, else 404)
        <- PeerConnection pl/pixie-hub export metrics.<agent>
      ======= the agent's PeerLink: zetta-tunnel gateways, mutual TLS 1.3, exact peer identities =======
hub:  PeerConnection plc/pixie-agent-<agent> import metrics.<agent> = Service peer-metrics-<agent>
        <- hub Prometheus (plc), job federate-<cluster>, every 60 s -> hub cluster's Alertmanager
```

- **The same link as Pixie's**: one `PeerLink` per agent carries vzconn one way and the metrics the
  other. No new endpoint is exposed; nothing but the read-only proxy is exported; the leaf's
  Prometheus API, admin endpoints and remote write are not reachable from the hub. The hub's own
  cluster is federated directly from its platform Prometheus.
- **Only accepted clusters**: the jobs are built from the `PixieCloud`'s `agents`, which only the
  managing cluster writes after the purpose check, so a production hub never reads nonprod and the
  reverse (`TestObservabilityAgentsReportOnlyToTheirPurposesHub`).
- **Narrow**: `match[]` brings health (`up`, `ALERTS`), the operator's status (`orama_*`),
  readiness, restarts and node conditions, kube-prometheus-stack's per-container CPU and memory
  recording rules, the engine's latency histograms, control database lag and volume use; not raw
  cAdvisor series.
- **Labelled by the hub, not by the cluster**: each series keeps its own job and instance
  (`honor_labels`), and its `cluster` label is overwritten after federation with the managing
  cluster's name for the job it came through, so `v1` in EU and `v1` in US are two series and a
  cluster cannot claim another's name. A federated `up` with a `federate-*` job is dropped, so a
  cluster cannot forge the hub's reachability series either: `up{job="federate-<cluster>"}` is the
  hub's own scrape of that cluster, always distinct from that cluster's own `up` series (its
  Alertmanager's, say), which keep their jobs. `TestHubFederatesEachLeafLive` runs this in real
  Prometheus with two leaves of identical stacks, one of which forges both.

**What pages from the hub**, evaluated only by the hub Prometheus (rules labelled
`orama.cloud/metrics-scope: hub`; the platform Prometheus' `ruleSelector` excludes them, on both
installation paths), delivered by the hub cluster's Alertmanager to the hub's receivers. The hub
Prometheus sets a `scrapeConfigSelector` (for the hub's own ScrapeConfigs, none today): without any
monitor selector, prometheus-operator writes none of its configuration, so neither the federation
jobs nor these rules would load.

| Alert | Fires when |
|---|---|
| `ObservabilityClusterUnreachable` | a cluster's federate job has been down for 5 minutes: the cluster, its link, its proxy or its Prometheus is gone |
| `ObservabilityClusterMetricsMissing` | an expected cluster has no federate job at all for 10 minutes |
| `ObservabilityClusterAlertingDown` | a reachable cluster's Watchdog has been absent for 15 minutes: its own rules are not running |
| `ObservabilityClusterAlertmanagerDown`, `…Missing` | a cluster's Alertmanager is down, or reports nothing while the cluster is reachable |
| `ObservabilityRelayedCriticalAlert` | a cluster's own critical alert is firing while its Alertmanager is down: the hub delivers it instead |

Every rule names only the clusters the hub reads now (`cluster=~"…"`, rendered from the agents).
Prometheus writes no staleness marker for a job a reload removes, so a removed cluster's last series
stay queryable for five minutes; scoped this way, they page nothing.

`TestHubMetricsRulesInPrometheus` checks the rendered configuration with `promtool check config` and
the rules with `promtool test rules` on synthetic series: a lost cluster, a missing job, a cluster
whose rules or Alertmanager stopped, a relayed alert, and a healthy cluster that fires nothing.
`TestHubMetricsRulesIgnoreRestartsAndRemovedClusters` covers what must not page: the hub down for
eight minutes, and a removed cluster whose dead series are still in the lookback; and a leaf that
loses only its Alertmanager, which pages and has its critical alert relayed.

**Dashboards** (`hub-dashboards` in `plc`, Perses project `observability-hub`, reading the hub
Prometheus): every cluster on one page (reachability, firing alerts, components not ready, nodes),
one page per cluster (components, alerts, restarts, CPU and memory by namespace, volumes, search
latency), and Orama Cloud v1 across clusters, every legend naming the cluster.

**Status and lifecycle**: each agent's `metricsConnected` (on the `PixieCloud`, the hub and the
agent) is the hub Prometheus' own `up` for that cluster's job, reported while Pixie still waits. A
hub Prometheus that does not answer reads false for every cluster. Enrolling an agent adds its
export, import, job and rules; removing it removes all of them, and its series age out of the hub
with `metricsRetention`. A withdrawn link (credentials gone, `PeerLink` deleted) or a lost import
Service stops the scrape: the status says false and `ObservabilityClusterUnreachable` pages. A
restarted hub keeps its data on its volume and resumes. Without the monitoring CRDs nothing of this is
written, and `Alerting=False`.

**Budget**: the hub Prometheus requests 1 GiB and 0.2 CPU (limits 2 GiB, 2 CPU) and may fill 90% of
its volume (`retentionSize`); each leaf's proxy requests 64 MiB.

## Status

Each kind reports `Ready | Progressing | Pending | Degraded | Refused`, `n/total`, one entry per
component (each Deployment, StatefulSet or DaemonSet, and the database), and a `Ready` condition
whose reason is the phase. Messages are coarse ("rolling out", "not running on every node") so a
replica count never changes status. Status is written only when it differs (deep-compared); a steady
hub or agent writes nothing (`TestPixieCloudBootstrapIssuesAndRevokesPerClusterKeys`,
`TestPixieVizierReconcileIsDeterministicAndKeepsRegistration`,
`TestObservabilityAgentsReportOnlyToTheirPurposesHub`).

## Failure and recovery

| What fails | What happens |
|---|---|
| The hub's cluster or the link | Vizier keeps collecting on its nodes (hours, in memory); `ObservabilityHubUnreachable` fires on the agent's cluster. Nothing on the agent's cluster depends on the hub to run. |
| An agent's whole cluster | On the hub: `ObservabilityClusterUnreachable` (its federate job down), and `ObservabilityAgentDisconnected` once Pixie runs. Neither needs anything on the lost cluster. |
| An agent's Alertmanager | On the hub: `ObservabilityClusterAlertmanagerDown`, and each of that cluster's critical alerts relayed (`ObservabilityRelayedCriticalAlert`). |
| Pixie missing or broken (no release, a key not issued, a cluster not registered) | The hub's metrics, dashboards and cluster-loss paging are unaffected: they are written before anything of Pixie and read only the platform's Prometheus. |
| An agent removed | Its job and its rules go with it; its last series, still queryable for five minutes, page nothing. |
| A Pixie credential deleted | Minted again. |
| The hub's database lost | The organisation (its id is a function of its name) and the administrator are created again; every key Pixie no longer lists is issued again and delivered to its cluster, whose Vizier then registers anew. What is lost is people's saved scripts and settings, and the old cluster registrations. No backup is configured for it yet. |
| A deploy key deleted in Pixie's UI | Issued again on the next pass and delivered to its cluster. |
| Pixie's API or migrations not ready | The hub stays `Progressing` with the reason; nothing is written. |
| A certificate near expiry | Reissued 30 days before, which rolls the pods that read it. |
| Monitoring CRDs missing | See above: dashboards only, `Alerting=False`, nothing else blocked. |
| A refused hub or agent | Nothing written, or everything withdrawn; the reason on the resource until an edit. |

Deleting an `ObservabilityAgent` removes its `PixieVizier`, both link ends, the `PeerLink` and its
delivered key, and the hub revokes the key. Deleting an `ObservabilityHub` removes its `PixieCloud`,
which removes the hub's data with it (its database volumes).

## Upgrading Pixie

Versions change only with a release; nothing updates itself.

1. Pick the commit in `oramasearch/pixie`; build and push every image by digest (#455).
2. `PIXIE_COMMIT=<sha> ./hack/fetch-pixie-manifests.sh`, update `pixieManifestCommit`, and fix
   whatever `go test ./internal/controller -run Pixie` says changed (new objects, images, containers
   without budgets).
3. Release the operator; then change the `ObservabilityProvider`'s `commit`, versions and images.
   Hubs and agents roll with it. A provider whose commit is not the operator's is not Ready, so the
   two cannot drift.

## Rolling it out

Nothing here creates infrastructure or changes a live cluster by itself. Each step needs what the
one before it published, and a missing piece is a status that says so, never a fallback. With
Steve's consent where the cluster rules ask for it:

1. **Publish the artifacts and the credentials they need.**
   - Pixie (#455): every release key from `4b445e0b7907` plus `hack/pixie-patches`, and the
     `script-bundle` image (`hack/pixie-patches/README.md`, "Building"), each digest recorded.
     Missing: the `ObservabilityProvider` is not Ready and names what is missing; no Pixie pod is
     created.
   - The platform mirrors: every `platform` and `operator` image of
     `hack/observability-images.yaml`. Missing: the chart's pods cannot pull, and the
     `ClusterHelmRelease` reports each workload that is not rolled out with its pods' pull error;
     without `platform-envoy` a leaf's `metrics-federation` cannot pull and the hub reports that
     cluster `metricsConnected: false` and pages `ObservabilityClusterUnreachable`.
   - The pull Secrets: the existing per-cluster registry Secrets on the managing cluster, and on the
     managing cluster itself one in `monitoring` ([`install.md`](../install.md)). Missing: the
     release is `Degraded` (`PullSecretUnavailable`).
2. **CRDs, then the operator.** Roll the release that carries these kinds onto every cluster,
   CRDs widened then exact ([`crd-rollout.md`](../operations/crd-rollout.md)). Everything is
   additive: new kinds, new optional `ClusterHelmRelease` fields, a `PixieVizier` field made
   optional. Nothing changes on a Cloud v1 stack yet: its monitoring waits for step 3's CRDs. This
   precedes step 3 because an older `ClusterHelmRelease` CRD would prune `imagePullSecretRef` from
   the platform files.
3. **The platform charts.** Apply the new `ClusterHelmRelease`s in each
   `config/clusters/*/platform.yaml` (kube-prometheus-stack, then Perses), and on the managing
   cluster `helmfile apply` the merged file, with consent. kube-prometheus-stack adds the
   monitoring CRDs and Prometheus' 50 Gi and Alertmanager's 2 Gi volumes per cluster. Each Cloud v1
   stack then gets its PodMonitors, rules, dashboards and two scrape allowances in its sandbox
   (additive: no pod restarts).
4. **Nonprod first** (#456's order): an `ObservabilityProvider`, an `ObservabilityHub` on
   `orama-nonprod-cluster-01` and an `ObservabilityAgent` for it
   ([`config/samples/observability.yaml`](../../config/samples/observability.yaml)). The agent's alert
   routing is written at once; the rest follows the artifacts. Qualify here what is listed under
   *Not yet qualified* below.
5. **Production's hub** on the managing cluster: an `ObservabilityHub` with no `clusterRef`, named
   after the managing cluster (its own metrics carry the hub's name as their `cluster` label). It
   needs the managing cluster's platform layer from step 3, and adds the hub's volumes there
   (`storage`: the hub Prometheus, OpenSearch, CloudNativePG and NATS).
6. **Production agents** on `orama-production-cluster-02` and `-03`, each with its own pull Secret,
   reporting to the managing cluster's hub over their peer links, committed under `config/clusters/`
   like every applied resource. `config/clusters/sets/v1-prod.yaml` is not part of this.

Rollback: delete the agents, then the hub (each withdraws what it placed); remove the platform
releases from both installation paths in one change; the CRDs stay, as always.

## Verify

What has been checked, and how strongly. Of all this, only the platform charts have run on a cluster: kube-prometheus-stack and
Perses installed from the mirrors on all four clusters on 2026-10-30, every leaf scrape target up.

| Evidence | What it shows | Tests |
|---|---|---|
| Controller tests (fake clients) | Rendering, reconciliation, idempotence, purpose isolation, credentials, teardown, status, the all-or-nothing monitoring writes | `TestRenderVizier`, `TestRenderPixieCloud`, `TestPixieVizier*`, `TestPixieCloudBootstrapIssuesAndRevokesPerClusterKeys`, `TestObservability*`, `TestOramaCloudV1Monitoring*`, `TestMonitoringObjectsAreAllOrNothing`, `TestHubPrometheusIsPinnedAndScoped`, `TestHubDashboardsReadTheHubAndNameTheCluster`, `TestMetricsFederationProxyIsReadOnly`, `TestClusterHelmReleaseCopiesItsPullSecretUnderTheProductionRule` |
| envtest (a real API server) | The CRDs install and refuse malformed resources; defaults apply | `TestObservabilityCRDsRefuseMalformedResources` |
| Offline Helm render of the vendored charts | Only inventoried mirrors render, every pod has the pull Secret, the platform Prometheus excludes hub rules, both paths install the same values | `TestPlatformChartsRenderOfflineWithOnlyMirroredImages`, `TestEveryClusterRunsPermissiveMetricsAlertingAndDashboards`, `TestHelmfileAndEveryLeafInstallTheSamePlatform` |
| Real Prometheus 3.15 (`PROMETHEUS_BINARY_DIR`) | The hub's rendered configuration loads. The hub rules page on synthetic series for a lost leaf, a lost Alertmanager alone, a missing job and stopped rules, and stay silent through a hub restart and for a removed cluster. Real federation from two leaves with identical stacks keeps them apart by cluster, overrides a forged cluster label, drops a forged reachability series, and brings only the match list. It turns a killed leaf's job to `up=0`, which the operator's status query reads; a restarted hub keeps its data and resumes; a removed cluster stops being scraped on reload | `TestHubMetricsRulesInPrometheus`, `TestHubMetricsRulesIgnoreRestartsAndRemovedClusters`, `TestHubFederatesEachLeafLive` |
| Go TLS with the issued certificates | A Vizier's dial verifies the hub's CA and name for both allowed addresses | `TestHubCertificateIsVerifiedForTheDialedAddress` |
| Reading the pinned sources | OpenSearch 2.19 accepts Pixie's mapping and client; the API's GraphQL key calls and token; gRPC's server name; the storage client's emulator reads; the patch applies at the pin | the tables above; `TestPixieBootstrapMatchesVendoredSchema`, `TestPixieUserTokenIsWhatPixieAccepts`, `TestPixieAPI*` against a fake API |

**Not run anywhere yet**, because no hub has been created; the images and mirrors exist since
2026-10-31 (#455, [`observability-provider.yaml`](../../config/clusters/observability-provider.yaml)): Pixie on the AKS node images (Ubuntu 22.04/24.04, Azure Linux 3) and the
protocols traced; the bootstrap against a live Pixie Cloud (Kratos' admin API, the adapter against the
migrated profile database, the GraphQL key calls); OpenSearch 2.19 with Pixie's indexer and
autocomplete; the script bundle through the proxy and the script manager; the Envoy federation proxy
(the `platform-envoy` mirror) and the metrics crossing a real peer link; the hub Prometheus under
prometheus-operator and its sandbox allowances; the kube-state-metrics custom-resource metrics; the
PodMonitors and alerts against live targets.
Each is step 4 of the rollout, on nonprod first.
