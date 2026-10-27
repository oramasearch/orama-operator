# Observability

How the operator gives every cluster metrics, alerts, dashboards and live tracing, with nothing
outside our clusters and nothing that is not permissively licensed. This is
[operator#15](https://github.com/oramasearch/orama-operator/issues/15) as amended by
[#456](https://github.com/oramasearch/orama-operator/issues/456) (Pixie, implemented by this
operator, a `prod-support` cluster, nonprod isolated), [#455](https://github.com/oramasearch/orama-operator/issues/455)
(Pixie's images from our copy) and [#437](https://github.com/oramasearch/orama-operator/issues/437)
(the secure link between clusters).

## Decisions this follows

| Decision | Source |
|---|---|
| Monitoring is in-cluster Prometheus and Alertmanager, on every cluster identically. | Steve, #15, 2026-10-13 |
| Grafana, Loki, Mimir and Tempo are AGPL-3.0 and ruled out. Everything is permissive, for resale and for customers' own clusters. | Steve, #15 |
| Pixie for live tracing, installed by our operator: no Pixie operator, no OLM, versions pinned, auto-update off. | Steve, #15, #456 |
| Pixie Cloud is self-hosted on a `prod-support` cluster for production and on nonprod for nonprod; never Pixie's or New Relic's hosted service, never Auth0. | Steve, #456 |
| Production agents report to `prod-support`; nonprod agents and their hub stay on nonprod. The operator enforces it. | #456 |
| Agents reach a hub on another cluster only through #437's peer link (zetta-tunnel, mutual TLS 1.3, `PeerConnection` + `PeerLink`), with a deploy key of their own. | #456, #437 decision D6 |
| Everything runs on-prem and air-gapped: images from our private registry, nothing calls out at run time. | #456 |

## What runs where

| Cluster | Platform layer (every cluster) | From resources |
|---|---|---|
| Every cluster | kube-prometheus-stack (Prometheus, Alertmanager, kube-state-metrics, node-exporter) and Perses, from `helmfile.yaml` and `config/clusters/*/platform.yaml` | The rules, scrape configuration, routing and dashboards of what runs on it |
| A cluster with an `ObservabilityAgent` | | `PixieVizier` in namespace `pl`: Pixie's node agents and Vizier services |
| A cluster with an `ObservabilityHub` | | `PixieCloud` in namespace `plc`: self-hosted Pixie Cloud |

| Hub on | Serves agents on |
|---|---|
| a `prod-support` cluster | `production` and `prod-support` clusters |
| a `nonprod` cluster | `nonprod` clusters |

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

`KubernetesCluster` already carries a `purpose` tag. #456 adds a third value, **`prod-support`**: a
cluster that runs support services for production and no customer or test workload. It counts as
production for the cluster rules (`observability_purpose.go`).

| Rule | Enforced by |
|---|---|
| A hub runs only on a `prod-support` or `nonprod` cluster. | `ObservabilityHub` is `Refused`; nothing is written. |
| An agent reports only to a hub of its purpose: production and prod-support to prod-support, nonprod to nonprod. | `ObservabilityAgent` is `Refused` and anything it had placed is removed; the hub controller also leaves it out of the `PixieCloud`'s `agents`, so the hub never issues it a key. |
| No customer or test workload on `prod-support`. | A `LakeSet` or `OramaCloudV1Set` naming a prod-support cluster is refused (`SupportCluster`). |
| Production credentials never reach nonprod; they may reach prod-support. | The production-credential rules of the sets and `OramaOperator` ([`crd-reference.md`](../crd-reference.md#production-credentials-never-reach-a-nonprod-member)), applied to every Secret the observability controllers copy. |
| Nothing from production is reachable from nonprod. | A nonprod agent never gets a link to a prod-support hub, and every Pixie namespace is default-deny. |

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
| The bootstrap role | the hub's operator | `plc/pixie-db-bootstrap`: may only read and insert `orgs` and `users` in the profile database (granted by their owner when missing), two connections. |
| OpenSearch users | the hub's operator | `plc/pixie-search`: `pixie-indexer` writes the metadata indices, `pixie-api` only reads them; only bcrypt hashes reach OpenSearch. |
| The administrator's password | the hub's operator | `plc/pixie-admin`. Upstream creates `admin@default.com` with password `admin`; that container is removed. |
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
| PxL scripts from `artifacts.px.dev` and a GCS bucket | outside services | Served inside the cluster by `pixie-scripts` (image `script-bundle`): to the UI through the proxy at `/pxl_scripts/`, and to the script manager as a storage emulator (`STORAGE_EMULATOR_HOST`). |
| Auth0, Google sign-in | outside services | Not configured. Sign-in is Ory (Kratos, password), with the administrator the operator creates. Clerk as Kratos' OIDC provider is a later step and needs the internet, so the on-prem default stays Ory. |

### The bootstrap

What a person would do after installing Pixie Cloud, done by the hub cluster's operator
(`pixie_cloud_bootstrap.go`), once the hub is Ready:

1. **The metadata index**, created in OpenSearch with the indexer's own body (vendored from
   `src/cloud/indexer/md/mapping.o.go` as `assets/pixie/md-index.json`). The indexer compares its
   desired settings with the live index and reindexes on any difference, so the body must be its
   own.
2. **The administrator**, through Kratos' admin API.
3. **The organisation and the administrator's user.** Pixie has no API that creates the first
   organisation or user without someone signing in, so this one step is a schema adapter: it reads
   first and inserts only what is missing, as the dedicated `bootstrap` role, against the vendored
   profile migrations (`TestPixieBootstrapMatchesVendoredSchema` checks every column it writes and
   that nothing it leaves out is required). The organisation's id is a function of its name.
4. **Deploy keys, through Pixie's own API**: GraphQL `CreateDeploymentKey`, `deploymentKeys`,
   `DeleteDeploymentKey` and `clusters`, as the administrator, with a five-minute token the operator
   signs with the hub's signing key exactly as Pixie's auth service issues one
   (`TestPixieUserTokenIsWhatPixieAccepts`, `TestPixieAPIDeployKeyLifecycle`). Pixie generates
   every key; nothing writes Pixie's key tables.

### OpenSearch in place of Elasticsearch, checked against the sources

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
| `nats`, `curl`, `postgres` | mirrors of the images Pixie's manifests name (`ghcr.io/pixie-io/...`); `postgres` is a CloudNativePG operand image |
| `envoy`, `hydra`, `hydra-cli`, `kratos` | mirrors of `envoyproxy/envoy`, `oryd/hydra` (`-sqlite` and `-alpine`), `oryd/kratos` at the versions the manifests pin |
| `opensearch` | a mirror of `opensearchproject/opensearch` 2.19 |
| `script-bundle` | new in #455: a static, non-root image serving PxL's `bundle.json` built from the same commit on port 8080 at `/pixie-scripts/bundle.json`, and its storage metadata at `/storage/v1/b/pixie-scripts/o/bundle.json` |

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
- kube-state-metrics turns the operator's own status into metrics, so an alert says what the
  resource says: `orama_oramacloudv1_phase` and `orama_oramacloudv1_component_ready`,
  `orama_pixiecloud_phase` and `orama_pixiecloud_agent_connected`, `orama_pixievizier_phase`,
  `orama_peerconnection_phase`.

What the operator writes:

| Next to | Scrape | Rules | Dashboards |
|---|---|---|---|
| Each `OramaCloudV1` | PodMonitors: the writer's and readers' `/metrics` on :8080 (by number: a reader's Envoy also names a port `http`), and the control database's exporter (:9187). The stack's sandbox admits only the platform's Prometheus pods, on those two ports. | A critical component (writer, reader, broker, control database, cache, dashboard) not ready for 10 minutes pages; the rest after 30 minutes as a warning. The engine's or control database's metrics gone for 5 minutes; search p95 over 0.5 s for 15 minutes; replica lag over 5 minutes. | `<stack>-overview` (health, traffic, resources) and one per component, in the namespace's Perses project. |
| Each `PixieVizier` | | Agents not Ready for 15 minutes; the link to the hub down for 15 minutes (critical). Evaluated on the agent's cluster, so they fire when the hub is unreachable. | |
| Each `PixieCloud` | | The hub not Ready for 15 minutes; an agent cluster not heartbeating for 10 minutes (critical). A lost cluster cannot page about itself; this is the rule that notices. | |

Not scraped yet: the broker. RabbitMQ's Prometheus plugin is not enabled on the broker, and enabling
it restarts the broker, so it waits for a planned window; until then the broker is covered by its
readiness and the writer's enqueue latency.

Rolling the operator out changes nothing on a Cloud v1 stack until the platform layer is applied:
without the monitoring CRDs it writes no monitoring object, dashboard or sandbox allowance
(`TestOramaCloudV1MonitoringWaitsForThePlatformLayer`). A `PixieCloud` or `PixieVizier` then shows
`Alerting=False` (`MonitoringCRDsMissing`); every other resource still reconciles.

Rules on metrics that may be absent have an `absent()` companion, so a missing target is an alert,
never a silent pass: `OramaEngineTargetsMissing` and `OramaControlDBTargetsMissing` (critical),
`OramaStackStatusMetricsMissing`, and `ObservabilityAgentStatusMissing` on agents and hubs.

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
| An agent's cluster | `ObservabilityAgentDisconnected` fires on the hub's cluster. |
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

Nothing here creates infrastructure or changes a live cluster by itself. In order, each with Steve's
consent where the cluster rules ask for it:

1. **Images (#455).** Build every key above from `4b445e0b7907` into private `oramasearch/pixie-*`
   repositories and record the digests. Until then no provider is Ready and nothing Pixie runs
   anywhere; steps 2, 3 and the hubs' and agents' resources can go first, and give every cluster
   its alert routing (paging for `v1-eu` and `v1-us`) before Pixie exists.
2. **Operator.** Roll the release that carries these kinds onto every cluster
   ([`operations.md`](operations.md)). Its CRDs go on as usual
   ([`crd-rollout.md`](../operations/crd-rollout.md)). Nothing changes on a Cloud v1 stack yet:
   its monitoring waits for the monitoring CRDs of step 3. This must come before step 3: the
   `ClusterHelmRelease` CRD it carries adds `imagePullSecretRef`, which an older CRD would prune
   from the platform files.
3. **Platform layer.** Mirror every `platform` image of the inventory into its private
   repository (nothing installs until they exist). Then apply the new `ClusterHelmRelease`s in each
   `config/clusters/*/platform.yaml` (kube-prometheus-stack, then Perses); on the managing cluster,
   create the `monitoring` pull Secret ([`install.md`](../install.md)) and `helmfile apply` the
   merged file, with consent. kube-prometheus-stack adds the monitoring CRDs and Prometheus' 50 Gi
   and Alertmanager's 2 Gi volumes per cluster. Each Cloud v1 stack then gets its PodMonitors,
   rules, dashboards and two scrape allowances in its sandbox (additive: no pod restarts).
4. **Nonprod first** (#456's order): an `ObservabilityProvider`, an `ObservabilityHub` on
   `orama-nonprod-cluster-01` and an `ObservabilityAgent` for it; prove the node images (Ubuntu
   22.04/24.04, Azure Linux 3), the protocols (HTTP, gRPC, Postgres, Redis, Kafka, AMQP) and that
   nothing calls out. Samples: [`config/samples/observability.yaml`](../../config/samples/observability.yaml).
5. **prod-support.** Create the `KubernetesCluster` (`purpose: prod-support`; region and size are
   Steve's), its platform layer and operator, then its hub.
6. **Production agents** on `orama-production-cluster-02` and `-03`, each with its own pull Secret,
   reporting to the prod-support hub over their peer links. Committed under `config/clusters/` like
   every applied resource.

Rollback: delete the agents, then the hub (each withdraws what it placed); remove the platform
releases from both installation paths in one change; the CRDs stay, as always.

## Verify

Native tests (`go test ./internal/controller -run 'Pixie|Observability|OramaCloudV1Monitoring'`,
`go test ./cmd`):

- `TestRenderVizier`, `TestRenderPixieCloud`: exact objects, images only from the release, every
  container budgeted, auto-update off, no updater or certificate Job, no Elastic or BigQuery, one
  database and role per service, nothing fetched from outside, deterministic output.
- `TestPixieVizierWaitsForItsOwnCredentials`, `TestPixieVizierReconcileIsDeterministicAndKeepsRegistration`,
  `TestPixieVizierToleratesMissingMonitoringCRDs`.
- `TestPixieCloudBootstrapIssuesAndRevokesPerClusterKeys`: the bootstrap, a key per cluster, the
  peer allowance kept, revocation, a steady hub writing nothing.
- `TestObservabilityAgentsReportOnlyToTheirPurposesHub`, `TestObservabilityHubRefusedOnProduction`,
  `TestObservabilityProductionCredentialsNeverReachNonprod`, `TestObservabilityPurposeRules`.
- `TestOramaCloudV1MonitoringScrapesOnlyTheEngineAndControlDB`, `TestOramaCloudV1ScrapeAllowancesAdmitOnlyPrometheus`.
- `TestObservabilityCRDsRefuseMalformedResources` (envtest).
- `TestEveryClusterRunsPermissiveMetricsAlertingAndDashboards`, `TestHelmfileAndEveryLeafInstallTheSamePlatform`.

**Not yet qualified on a cluster**, because the images do not exist yet (#455): Pixie on the AKS node
images, the bootstrap against a live Pixie Cloud (the Kratos admin API and the SQL against Pixie's
migrated schema), OpenSearch with Pixie's indexer, the script bundle image, the kube-state-metrics
custom-resource metrics and the PodMonitors against live targets. Each is step 4 of the rollout.
