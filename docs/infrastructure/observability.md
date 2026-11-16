# Observability

How every cluster gets metrics, alerts and dashboards, with nothing outside our clusters and
nothing that is not permissively licensed
([operator#15](https://github.com/oramasearch/orama-operator/issues/15)).

The operator has no observability kinds of its own today. Pixie, and the `ObservabilityProvider`,
`ObservabilityHub` and `ObservabilityAgent` kinds and the hub metrics federation built on it, were
removed in [operator#1192](https://github.com/oramasearch/orama-operator/issues/1192) (see
[Removed: Pixie and the observability kinds](#removed-pixie-and-the-observability-kinds)). What is
left is the platform layer's monitoring on every cluster, and the monitoring each `OramaCloudV1`
stack writes for itself. Lake's workspace metrics are not here: they live in each workspace's Zetta.

## Decisions this follows

| Decision | Source |
|---|---|
| Monitoring is in-cluster Prometheus and Alertmanager, on every cluster identically. | #15 |
| Grafana, Loki, Mimir and Tempo are AGPL-3.0 and ruled out. Everything is permissive, for resale and for customers' own clusters. | #15 |
| Pixie is removed and not replaced for now; an observability design for Lake comes later. The platform Prometheus stays, for Orama Cloud v1. | #1192 |
| Everything runs on-prem and air-gapped: images from a mirror, nothing calls out at run time. | #15 |

## What runs where

| Cluster | Platform layer | From resources |
|---|---|---|
| Every cluster | kube-prometheus-stack (Prometheus, Alertmanager, kube-state-metrics, node-exporter) and Perses, from `helmfile.yaml` and `config/clusters/*/platform.yaml` | The scrape configuration, rules and dashboards each `OramaCloudV1` writes next to itself (`oramacloudv1_monitoring.go`) |

Prometheus, Alertmanager and Perses are the same release with the same values everywhere (the *two
installation paths* rule; `cmd/platform_sync_test.go`).

## Licences of what is installed

| Component | Licence |
|---|---|
| Prometheus, Alertmanager, prometheus-operator, kube-state-metrics, node-exporter | Apache-2.0 |
| Perses | Apache-2.0 |
| k8s-sidecar (Perses' provisioning sidecar) | MIT |

`TestEveryClusterRunsPermissiveMetricsAlertingAndDashboards` fails if either installation path
installs a Grafana, Loki, Mimir, Tempo, Elasticsearch, ECK or Kibana chart, or leaves
kube-prometheus-stack's Grafana subchart on.

## Images

[`hack/observability-images.yaml`](../../hack/observability-images.yaml) lists every image and chart
the platform layer's monitoring runs, with the upstream it is mirrored from and its licence:

- **The platform layer**: every image the two charts render is overridden in their values to a
  mirror, `docker.io/oramasearch/platform-<name>` at the upstream tag, pulled with the cluster's own
  Secret (`ClusterHelmRelease` `imagePullSecretRef`; on the managing cluster,
  [`install.md`](../install.md)). A mirror that does not exist fails the pull: there is no fallback
  to a public registry. Perses' helm test pod, the one public image the chart renders, is off.
- **The charts** are vendored in `hack/charts/` and checked against their recorded sha256.
  `TestPlatformChartsRenderOfflineWithOnlyMirroredImages` renders them offline with the exact
  values and fails on any image outside the inventory or any pod without the pull Secret.

The charts themselves are fetched from their public repositories by `helmfile` and
`ClusterHelmRelease`, as every platform chart is; an install without internet pulls the vendored
tarballs from its own chart repository (OCI, `spec.chart.credentialsSecretRef`).

## Budgets

| Per cluster | Requests | Limits |
|---|---|---|
| Prometheus stack and Perses | about 2.5 GiB and 0.7 CPU, plus node-exporter per node; 52 Gi of volumes | about 5.2 GiB |

## Metrics, alerting and dashboards

- **Prometheus** keeps 15 days or 45 GB on a 50 Gi volume, and selects PodMonitors and
  PrometheusRules in every namespace. kube-state-metrics, node-exporter and the kubelet cover every
  pod, node and volume.
- **Alertmanager** evaluates every cluster's alerts. **No receiver is configured**: nothing in the
  operator writes an `AlertmanagerConfig` since Pixie's removal, so alerts are visible in each
  cluster's Alertmanager and Prometheus but are not delivered to Slack or a pager (see
  [Alert delivery](#alert-delivery)).
- **Perses** (read-only) shows every ConfigMap labelled `perses.dev/resource: "true"`.
- **Rules** are kube-prometheus-stack's defaults, without those for what a managed control plane
  runs itself (etcd, the controller manager, the scheduler; Cilium replaces kube-proxy) and without
  `KubeCPUOvercommit`, `KubeMemoryOvercommit`, `KubeCPUQuotaOvercommit` and
  `KubeMemoryQuotaOvercommit`. The first two fire whenever the pods' requests would not fit with the
  largest node gone, which on a cluster sized to what it runs is nearly always; every cluster
  autoscales its pools instead, and a pod that cannot be placed pages as `KubePodNotReady`. The
  quota pair fires when namespace quotas add up to more than the nodes, and Lake's workspace quotas
  are ceilings meant to; a workspace that reaches its own quota still alerts as `KubeQuotaExceeded`.
- kube-state-metrics turns the operator's own status into metrics, so an alert says what the
  resource says: `orama_oramacloudv1_phase`, `orama_oramacloudv1_component_ready` and
  `orama_peerconnection_phase`.

What the operator writes:

| Next to | Scrape | Rules | Dashboards |
|---|---|---|---|
| Each `OramaCloudV1` | PodMonitors: the writer's and readers' `/metrics` on :8080 (by number: a reader's Envoy also names a port `http`), and the control database's exporter (:9187). The stack's sandbox admits only the platform's Prometheus pods, on those two ports. | A critical component (writer, reader, broker, control database, cache, dashboard) not ready for 10 minutes; the rest after 30 minutes as a warning. The engine's or control database's metrics gone for 5 minutes; search p95 over 0.5 s for 15 minutes; replica lag over 5 minutes. | `<stack>-overview` (health, traffic, resources) and one per component, in the namespace's Perses project. |

Not scraped yet: the broker. RabbitMQ's Prometheus plugin is not enabled on the broker, and enabling
it restarts the broker, so it waits for a planned window; until then the broker is covered by its
readiness and the writer's enqueue latency.

Rolling the operator out changes nothing on a Cloud v1 stack until the platform layer is applied:
without the monitoring CRDs it writes no monitoring object, dashboard or sandbox allowance
(`TestOramaCloudV1MonitoringWaitsForThePlatformLayer`). Writes are all or nothing:
`applyMonitoringObjects` (`platform_monitoring.go`) looks up every kind first, so a platform layer
installed in part gets nothing written.

Rules on metrics that may be absent have an `absent()` companion, so a missing target is an alert,
never a silent pass: `OramaEngineTargetsMissing` and `OramaControlDBTargetsMissing` (critical) and
`OramaStackStatusMetricsMissing`.

### Alert delivery

Until #1192, each cluster's `AlertmanagerConfig` (every alert of the cluster to its hub's Slack or
webhook receivers) was written through `PixieVizier` or `PixieCloud`, and a hub on the managing
cluster federated every cluster's metrics and paged when one went silent. Both went with Pixie.
Delivering alerts again, and paging about a cluster that cannot page about itself, is for the
observability design that replaces it.

## Reaching the UIs

Nothing is exposed publicly: no Ingress, no DNS record. People reach each UI with `kubectl
port-forward` on the cluster it runs on, so access is exactly who holds that cluster's credentials.
Perses (read-only), Prometheus and Alertmanager have no sign-in of their own.

```sh
kubectl -n monitoring port-forward svc/perses 8080:8080                            # http://localhost:8080
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090    # http://localhost:9090
kubectl -n monitoring port-forward svc/kube-prometheus-stack-alertmanager 9093:9093  # http://localhost:9093
```

A Cloud v1 stack's dashboards are in its namespace's Perses project, on its own cluster.

## Removed: Pixie and the observability kinds

[operator#1192](https://github.com/oramasearch/orama-operator/issues/1192) removed, after v0.11.12:

- the kinds `ObservabilityProvider`, `ObservabilityHub`, `ObservabilityAgent`, `PixieCloud` and
  `PixieVizier`, their controllers, CRDs and RBAC;
- the vendored Pixie manifests (`internal/controller/assets/pixie/`), the Pixie build material
  (`hack/pixie-patches/`, `hack/pixie-script-bundle/`, `hack/fetch-pixie-manifests.sh`) and the
  Pixie and federation images from `hack/observability-images.yaml`;
- the hub Prometheus, its federation from every cluster, its dashboards and its cluster-lost rules,
  and the per-cluster alert routing;
- kube-state-metrics' `orama_pixiecloud_*` and `orama_pixievizier_*` metrics, from `helmfile.yaml`
  and every `platform.yaml`.

**Rolling it out.** An object of a removed kind has no controller once the operator without it runs,
and the hubs and agents carry the `orama.cloud/observability` finalizer, so they must be gone first:

1. With the operator that still ships them (v0.11.12 or an earlier `dev` build), delete the
   observability resources from their committed files (`config/clusters/observability-hub.yaml`,
   `observability-provider.yaml` and each `<cluster>/observability.yaml`, as they were before
   #1192). The managing operator tears down what each wrote on its clusters: the `PixieCloud` and
   `PixieVizier`, their workloads, links and copied Secrets.
2. `make check-crds CONTEXT=<context>` on every cluster. It fails while any object of a kind listed
   in [`hack/retired-crds.txt`](../../hack/retired-crds.txt) remains.
3. Roll the operator, and apply the platform layer (the kube-state-metrics change) by both paths.

The CRDs stay on each cluster, empty, until they are deleted; deleting them is a change to the
cluster like any other. The namespaces `plc` and `pl` are emptied by step 1 and left behind
(`cmd/crd_field_removal_test.go` acknowledges the CRD removals; `docs/operations/crd-rollout.md`).
