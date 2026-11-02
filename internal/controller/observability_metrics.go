/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The hub's metrics (operator#15, #456, #1059): production's metrics, dashboards and paging live on
// the managing cluster, not only on the leaves. Every cluster keeps its own Prometheus (the
// platform layer) for its own alerts; the hub runs one more Prometheus, in plc, that federates a
// narrow set of series from each cluster it serves and keeps them for metricsRetention.
//
//	leaf: platform Prometheus <- metrics-federation (pl, GET /federate only) <- PeerConnection export
//	      ==== #437 peer link, mutual TLS 1.3, the agent's own PeerLink ====
//	hub:  PeerConnection import peer-metrics-<agent> <- hub Prometheus (plc) -> hub Alertmanager
//
// The hub's own cluster is federated directly from its platform Prometheus. Every federated series
// gets the cluster it came from as a `cluster` label, so two clusters' stacks of the same namespace
// and name never mix. The hub's rules page when a cluster's metrics stop arriving, when its own
// alerting stops (its Watchdog is gone) and when its Alertmanager is down: a lost leaf cannot page
// about itself, so the hub does.

const (
	hubPrometheusName = "observability-hub"
	// hubPrometheusComponent is the StatefulSet prometheus-operator makes for it, and its pods'
	// component label, which the hub's sandbox allowances select (pixie_sandbox.go).
	hubPrometheusComponent = "prometheus-" + hubPrometheusName
	// hubPrometheusService is the Service prometheus-operator creates for every Prometheus.
	hubPrometheusService = "prometheus-operated"
	hubPrometheusConfig  = "hub-prometheus-config"
	hubPrometheusPort    = 9090
	// metricsScopeLabel marks the rules only the hub Prometheus evaluates; the platform Prometheus
	// excludes them (its ruleSelector, helmfile.yaml), since it has no federated series.
	metricsScopeLabel = "orama.cloud/metrics-scope"
	metricsScopeHub   = "hub"

	// The leaf's federation proxy, in Vizier's namespace.
	metricsFederationName = "metrics-federation"
	metricsFederationPort = 9090

	// platformPrometheusTarget is the platform layer's Prometheus on every cluster.
	platformPrometheusTarget = "kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"
	// platformAlertmanagerTarget is the platform layer's Alertmanager on every cluster.
	platformAlertmanagerTarget = "kube-prometheus-stack-alertmanager.monitoring.svc.cluster.local:9093"
	// platformAlertmanagerJob is the job its own up series carries.
	platformAlertmanagerJob = "kube-prometheus-stack-alertmanager"
	// platformPrometheusImage is the platform mirror of Prometheus (hack/observability-images.yaml),
	// the image the hub Prometheus runs: nothing falls back to a public registry.
	platformPrometheusImage = "docker.io/oramasearch/platform-prometheus:v3.15.0-distroless"
	// platformEnvoyImage is the platform mirror of the Envoy Pixie also pins (hack/observability-images.yaml,
	// `operator`), so a cluster's metrics reach the hub before any Pixie image exists.
	platformEnvoyImage = "docker.io/oramasearch/platform-envoy:v1.34.2"
)

var prometheusGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"}

// federationMatch are the series the hub federates from each cluster: health, the operator's own
// status, readiness and restarts, kube-prometheus-stack's per-container CPU and memory recording
// rules (not raw cAdvisor series), the engine's latency histograms, control database lag and volume
// use. Narrow on purpose: enough for the hub's dashboards and paging, not a copy of each Prometheus.
var federationMatch = []string{
	`{__name__=~"up|ALERTS|orama_.+"}`,
	`{__name__=~"kube_pod_status_ready|kube_pod_container_status_restarts_total|kube_node_status_condition|kube_deployment_status_replicas_available|kube_statefulset_status_replicas_ready"}`,
	`{__name__=~"node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m|node_namespace_pod_container:container_memory_working_set_bytes"}`,
	`{__name__=~"search_calculation_time_sec_(bucket|count|sum)|writer_doc_calculation_time_sec_(bucket|count|sum)|cnpg_pg_replication_lag|kubelet_volume_stats_(used|capacity)_bytes"}`,
}

// federatedCluster is one cluster the hub reads.
type federatedCluster struct {
	// Name is the cluster's KubernetesCluster, the cluster label of its series.
	Name string
	// Agent is the ObservabilityAgent, or "" for the hub's own cluster without one.
	Agent string
	// Target is host:port of the cluster's /federate.
	Target string
}

// federationJob is the scrape job of one cluster; its up series is the cluster's reachability.
func federationJob(cluster string) string { return "federate-" + cluster }

// agentMetricsImport is the name an agent's metrics are exported under and imported as: unique per
// agent, so each becomes its own Service in plc.
func agentMetricsImport(agent string) string { return "metrics." + agent }

// hubFederatedClusters is every cluster the hub reads: its own, directly, and each accepted agent
// on another cluster, through that agent's peer link. Sorted.
func hubFederatedClusters(cloud *oramav1alpha1.PixieCloud) []federatedCluster {
	clusters := []federatedCluster{}
	own := firstNonEmpty(cloud.Spec.Cluster, cloud.Spec.Hub)
	clusters = append(clusters, federatedCluster{Name: own, Target: platformPrometheusTarget})
	for _, agent := range cloud.Spec.Agents {
		if agent.Cluster == own {
			continue
		}
		clusters = append(clusters, federatedCluster{
			Name: agent.Cluster, Agent: agent.Name,
			Target: fmt.Sprintf("%s.%s.svc.cluster.local:%d", peerImportServiceName(agentMetricsImport(agent.Name)), oramav1alpha1.PixieCloudNamespace, metricsFederationPort),
		})
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	return clusters
}

// hubScrapeConfig is the hub Prometheus' scrape jobs, Prometheus configuration YAML: one federate
// job per cluster, with the cluster's name as a label on every series it brings.
func hubScrapeConfig(cloud *oramav1alpha1.PixieCloud) ([]byte, error) {
	var jobs []map[string]any
	for _, cluster := range hubFederatedClusters(cloud) {
		jobs = append(jobs, map[string]any{
			"job_name":        federationJob(cluster.Name),
			"scrape_interval": "60s",
			"scrape_timeout":  "30s",
			"metrics_path":    "/federate",
			// Keep each series' own job and instance, so a leaf's up{job="...alertmanager"} stays
			// apart from this job's own up, the hub's reachability of the cluster.
			"honor_labels": true,
			"params":       map[string]any{"match[]": federationMatch},
			"static_configs": []any{map[string]any{
				"targets": []string{cluster.Target},
				"labels":  map[string]string{"cluster": cluster.Name},
			}},
			// After federation, never trust what the cluster says about itself. A federated up
			// series with a federate job would pass for the hub's own reachability series (which
			// no metric relabelling touches; a series without a job, such as a recording rule,
			// gets this job from honor_labels), so it is dropped. Every series' cluster label is
			// overwritten with the managing cluster's name for it.
			"metric_relabel_configs": []any{
				map[string]any{"source_labels": []string{"__name__", "job"}, "regex": "up;federate-.+", "action": "drop"},
				map[string]any{"target_label": "cluster", "replacement": cluster.Name, "action": "replace"},
			},
		})
	}
	return sigsyaml.Marshal(jobs)
}

// queryFederationUp asks the hub Prometheus at baseURL for up{job=~"federate-.+"}: per cluster's
// federate job, whether the hub's own last scrape of it worked. A leaf's own up series never carry
// a federate job (hubScrapeConfig drops them), so the answer is the hub's alone.
func queryFederationUp(ctx context.Context, httpClient *http.Client, baseURL string) (map[string]bool, error) {
	query := url.Values{"query": {`up{job=~"federate-.+"}`}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/query?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub Prometheus: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil || answer.Status != "success" {
		return nil, fmt.Errorf("hub Prometheus answered %s: %v", resp.Status, err)
	}
	up := map[string]bool{}
	for _, sample := range answer.Data.Result {
		if len(sample.Value) == 2 {
			up[sample.Metric["job"]] = sample.Value[1] == "1"
		}
	}
	return up, nil
}

// hubAlertmanagerConfig sends the hub's alerts to its cluster's Alertmanager, statically: the hub
// Prometheus needs no Kubernetes API access.
func hubAlertmanagerConfig() ([]byte, error) {
	return sigsyaml.Marshal([]any{map[string]any{
		"api_version":    "v2",
		"static_configs": []any{map[string]any{"targets": []string{platformAlertmanagerTarget}}},
	}})
}

// hubMetricsVolume is the hub Prometheus' volume, and the size it may fill: 90% of it.
func hubMetricsVolume(cloud *oramav1alpha1.PixieCloud) (resource.Quantity, string) {
	volume := quantityOr(cloud.Spec.Storage.Metrics, "100Gi")
	return volume, fmt.Sprintf("%dGB", volume.Value()/1_000_000_000*9/10)
}

// hubMetricsObjects is the hub Prometheus, its configuration and its rules.
func hubMetricsObjects(cloud *oramav1alpha1.PixieCloud) ([]*unstructured.Unstructured, error) {
	scrape, err := hubScrapeConfig(cloud)
	if err != nil {
		return nil, err
	}
	alertmanagers, err := hubAlertmanagerConfig()
	if err != nil {
		return nil, err
	}
	config := &unstructured.Unstructured{}
	config.SetAPIVersion("v1")
	config.SetKind("Secret")
	config.SetNamespace(cloud.Namespace)
	config.SetName(hubPrometheusConfig)
	config.SetLabels(map[string]string{pixieManagedLabel: pixieManagedMetrics})
	config.Object["type"] = string(corev1.SecretTypeOpaque)
	config.Object["stringData"] = map[string]any{"scrape.yaml": string(scrape), "alertmanagers.yaml": string(alertmanagers)}

	volume, size := hubMetricsVolume(cloud)
	claim := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": volume.String()}},
	}
	if class := cloud.Spec.Storage.StorageClassName; class != "" {
		claim["storageClassName"] = class
	}
	prometheus := &unstructured.Unstructured{}
	prometheus.SetGroupVersionKind(prometheusGVK)
	prometheus.SetNamespace(cloud.Namespace)
	prometheus.SetName(hubPrometheusName)
	prometheus.SetLabels(map[string]string{pixieManagedLabel: pixieManagedMetrics})
	prometheus.Object["spec"] = map[string]any{
		"image":            platformPrometheusImage,
		"imagePullSecrets": []any{map[string]any{"name": cloud.Spec.PullSecretName}},
		"replicas":         int64(1),
		"retention":        firstNonEmpty(cloud.Spec.MetricsRetention, "30d"),
		"retentionSize":    size,
		"storage":          map[string]any{"volumeClaimTemplate": map[string]any{"spec": claim}},
		// The volume goes with the hub (#1133).
		"persistentVolumeClaimRetentionPolicy": map[string]any{"whenDeleted": "Delete", "whenScaled": "Retain"},
		// The platform Prometheus's identity (kube-prometheus-stack's default). Without fsGroup the
		// volume stays root's and Prometheus, which runs as a non-root user, cannot write to it.
		"securityContext": map[string]any{
			"runAsNonRoot":   true,
			"runAsUser":      int64(1000),
			"runAsGroup":     int64(2000),
			"fsGroup":        int64(2000),
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "200m", "memory": "1Gi"},
			"limits":   map[string]any{"cpu": "2", "memory": "2Gi"},
		},
		"podMetadata":                   map[string]any{"labels": map[string]any{pixieManagedLabel: pixieManagedMetrics, pixieComponentLabel: hubPrometheusComponent}},
		"additionalScrapeConfigs":       map[string]any{"name": hubPrometheusConfig, "key": "scrape.yaml"},
		"additionalAlertManagerConfigs": map[string]any{"name": hubPrometheusConfig, "key": "alertmanagers.yaml"},
		"ruleSelector":                  map[string]any{"matchLabels": map[string]any{metricsScopeLabel: metricsScopeHub}},
		"ruleNamespaceSelector":         map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": cloud.Namespace}},
		// prometheus-operator writes no configuration at all, these additions and the rules included,
		// for a Prometheus that sets none of the monitor selectors. This one selects the hub's own
		// ScrapeConfigs, of which there are none: it is there so the configuration is written.
		"scrapeConfigSelector":          map[string]any{"matchLabels": map[string]any{metricsScopeLabel: metricsScopeHub}},
		"scrapeConfigNamespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": cloud.Namespace}},
	}
	rules := prometheusRuleObject(cloud.Namespace, "hub-cluster-alerts",
		map[string]string{metricsScopeLabel: metricsScopeHub}, "orama.observability.hub", hubClusterRules(cloud))
	dashboards, err := hubDashboards(cloud)
	if err != nil {
		return nil, err
	}
	return []*unstructured.Unstructured{config, prometheus, rules, dashboards}, nil
}

// hubClusterRules page from the hub about every cluster it reads, so a cluster that cannot page
// about itself (gone, cut off, or with its own alerting down) still pages.
//
// Every rule names only the clusters the hub reads now: a removed cluster's last series stay
// queryable for a few minutes, and must not page.
func hubClusterRules(cloud *oramav1alpha1.PixieCloud) []alertRule {
	clusters := hubFederatedClusters(cloud)
	names := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		names = append(names, regexp.QuoteMeta(cluster.Name))
	}
	expected := fmt.Sprintf(`cluster=~%q`, strings.Join(names, "|"))
	rules := []alertRule{
		{
			Alert:    "ObservabilityClusterUnreachable",
			Expr:     fmt.Sprintf(`up{job=~"federate-.+", %s} == 0`, expected),
			For:      "5m",
			Severity: "critical",
			Summary:  "The hub cannot read cluster {{ $labels.cluster }}'s metrics: the cluster, its link to the hub or its Prometheus is down.",
		},
		{
			// A cluster's own critical alerts, while its Alertmanager cannot deliver them: the hub
			// delivers them instead, so the problem itself pages.
			Alert: "ObservabilityRelayedCriticalAlert",
			Expr: fmt.Sprintf(`max without (alertstate, alertname) (label_replace(ALERTS{alertstate="firing", severity="critical", %s, alertname!~"Observability.+"}, "relayed_alert", "$1", "alertname", "(.*)")) and on(cluster) (up{job=%q} == 0)`,
				expected, platformAlertmanagerJob),
			For:      "1m",
			Severity: "critical",
			Summary:  "Cluster {{ $labels.cluster }}: {{ $labels.relayed_alert }} is firing in {{ $labels.namespace }}, and that cluster's Alertmanager cannot deliver it.",
		},
		{
			Alert:    "ObservabilityClusterAlertmanagerDown",
			Expr:     fmt.Sprintf(`up{job=%q, %s} == 0`, platformAlertmanagerJob, expected),
			For:      "5m",
			Severity: "critical",
			Summary:  "Cluster {{ $labels.cluster }}'s Alertmanager is down: its own alerts are not delivered.",
		},
	}
	for _, cluster := range clusters {
		job := federationJob(cluster.Name)
		reachable := fmt.Sprintf(`on() (up{job=%q} == 1)`, job)
		rules = append(rules,
			alertRule{
				Alert:    "ObservabilityClusterMetricsMissing",
				Expr:     fmt.Sprintf(`absent(up{job=%q, cluster=%q})`, job, cluster.Name),
				For:      "10m",
				Severity: "critical",
				Summary:  "The hub has no metrics job for cluster " + cluster.Name + ": its metrics are not collected.",
			},
			alertRule{
				// kube-prometheus-stack's Watchdog always fires; its absence from a reachable
				// cluster means that cluster's rules no longer run.
				Alert:    "ObservabilityClusterAlertingDown",
				Expr:     fmt.Sprintf(`absent(ALERTS{alertname="Watchdog", cluster=%q}) and %s`, cluster.Name, reachable),
				For:      "15m",
				Severity: "critical",
				Summary:  "Cluster " + cluster.Name + " is reachable but its Watchdog alert is gone: its own alert rules are not running.",
			},
			alertRule{
				Alert:    "ObservabilityClusterAlertmanagerMissing",
				Expr:     fmt.Sprintf(`absent(up{job=%q, cluster=%q}) and %s`, platformAlertmanagerJob, cluster.Name, reachable),
				For:      "15m",
				Severity: "critical",
				Summary:  "Cluster " + cluster.Name + " is reachable but reports no Alertmanager: its own alerts are not delivered.",
			},
		)
	}
	return rules
}

// hubDashboards is the hub's Perses project: every cluster on one page, one page per cluster, and
// Orama Cloud v1 across clusters, every legend naming the cluster.
func hubDashboards(cloud *oramav1alpha1.PixieCloud) (*unstructured.Unstructured, error) {
	url := fmt.Sprintf("http://%s.%s.svc:%d", hubPrometheusService, cloud.Namespace, hubPrometheusPort)
	const project = "observability-hub"
	dashboards := []map[string]any{
		persesDashboardOn(url, project, "clusters", "Observability hub: every cluster", []persesRow{
			{Title: "Reachability", Panels: []persesPanel{
				{Title: "Metrics arriving (1 = yes)", Query: `up{job=~"federate-.+"}`, Legend: "{{cluster}}"},
				{Title: "Firing alerts", Query: `sum by (cluster, alertname) (ALERTS{alertstate="firing", alertname!="Watchdog"})`, Legend: "{{cluster}} {{alertname}}"},
				{Title: "Components not ready", Query: `sum by (cluster) (orama_oramacloudv1_component_ready == 0)`, Legend: "{{cluster}}"},
				{Title: "Ready nodes", Query: `sum by (cluster) (kube_node_status_condition{condition="Ready", status="true"})`, Legend: "{{cluster}}"},
			}},
		}),
		persesDashboardOn(url, project, "cloud-v1", "Orama Cloud v1 across clusters", []persesRow{
			{Title: "Health", Panels: []persesPanel{
				{Title: "Components ready", Query: `orama_oramacloudv1_component_ready`, Legend: "{{cluster}} {{namespace}}/{{name}} {{component}}"},
				{Title: "Stack phase", Query: `orama_oramacloudv1_phase == 1`, Legend: "{{cluster}} {{namespace}}/{{name}} {{phase}}"},
			}},
			{Title: "Traffic", Panels: []persesPanel{
				{Title: "Searches per second", Query: `sum by (cluster, namespace) (rate(search_calculation_time_sec_count[5m]))`, Legend: "{{cluster}} {{namespace}}"},
				{Title: "Search latency p95", Query: `histogram_quantile(0.95, sum by (cluster, namespace, le) (rate(search_calculation_time_sec_bucket[5m])))`, Legend: "{{cluster}} {{namespace}}", Unit: "seconds"},
				{Title: "Documents written per second", Query: `sum by (cluster, namespace) (rate(writer_doc_calculation_time_sec_count[5m]))`, Legend: "{{cluster}} {{namespace}}"},
				{Title: "Control database replica lag", Query: `max by (cluster, namespace) (cnpg_pg_replication_lag)`, Legend: "{{cluster}} {{namespace}}", Unit: "seconds"},
			}},
		}),
	}
	for _, cluster := range hubFederatedClusters(cloud) {
		c := fmt.Sprintf(`cluster=%q`, cluster.Name)
		dashboards = append(dashboards, persesDashboardOn(url, project, "cluster-"+cluster.Name, "Cluster "+cluster.Name, []persesRow{
			{Title: "Health", Panels: []persesPanel{
				{Title: "Metrics arriving (1 = yes)", Query: fmt.Sprintf(`up{job=%q}`, federationJob(cluster.Name)), Legend: "{{cluster}}"},
				{Title: "Components ready", Query: fmt.Sprintf(`orama_oramacloudv1_component_ready{%s}`, c), Legend: "{{namespace}}/{{name}} {{component}}"},
				{Title: "Firing alerts", Query: fmt.Sprintf(`sum by (alertname, namespace) (ALERTS{%s, alertstate="firing", alertname!="Watchdog"})`, c), Legend: "{{namespace}} {{alertname}}"},
				{Title: "Container restarts (1h)", Query: fmt.Sprintf(`sum by (namespace) (increase(kube_pod_container_status_restarts_total{%s}[1h]))`, c), Legend: "{{namespace}}"},
			}},
			{Title: "Resources", Panels: []persesPanel{
				{Title: "CPU by namespace (cores)", Query: fmt.Sprintf(`sum by (namespace) (node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m{%s})`, c), Legend: "{{namespace}}"},
				{Title: "Memory by namespace", Query: fmt.Sprintf(`sum by (namespace) (node_namespace_pod_container:container_memory_working_set_bytes{%s})`, c), Legend: "{{namespace}}", Unit: "bytes"},
				{Title: "Volume use", Query: fmt.Sprintf(`kubelet_volume_stats_used_bytes{%s} / kubelet_volume_stats_capacity_bytes{%s}`, c, c), Legend: "{{namespace}}/{{persistentvolumeclaim}}", Unit: "percent-decimal"},
				{Title: "Search latency p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (namespace, le) (rate(search_calculation_time_sec_bucket{%s}[5m])))`, c), Legend: "{{namespace}}", Unit: "seconds"},
			}},
		}))
	}
	configMap, err := persesConfigMap(cloud.Namespace, "hub-dashboards", project, map[string]string{pixieManagedLabel: pixieManagedMetrics}, dashboards...)
	if err != nil {
		return nil, err
	}
	return configMapToUnstructured(configMap)
}

// metricsFederationObjects is the leaf's read-only window onto its platform Prometheus, which the
// agent's PeerConnection exports to the hub: Envoy (the platform mirror) answering GET /federate
// from the platform Prometheus and 404 to everything else, so the hub can read series and nothing
// of Prometheus' API or admin endpoints. It needs nothing of Pixie.
func metricsFederationObjects() ([]client.Object, error) {
	labels := map[string]string{pixieManagedLabel: pixieManagedMetrics, pixieComponentLabel: metricsFederationName}
	config, err := json.Marshal(metricsFederationEnvoyConfig())
	if err != nil {
		return nil, err
	}
	budget := pixieBudget{CPURequest: "20m", CPULimit: "500m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: metricsFederationName},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"orama.cloud/envoy-config": shortHash(config)}},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(101),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "envoy",
						Image:           platformEnvoyImage,
						Command:         []string{"envoy"},
						Args:            []string{"-c", "/etc/envoy/envoy.json", "--log-level", "warn"},
						Ports:           []corev1.ContainerPort{{Name: "federate", ContainerPort: metricsFederationPort}},
						Resources:       budget.requirements(),
						SecurityContext: hardenedSecurityContext(),
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(metricsFederationPort)},
						}},
						VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/envoy", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: metricsFederationName}},
					}}},
				},
			},
		},
	}
	return []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: metricsFederationName}, Data: map[string]string{"envoy.json": string(config)}},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: metricsFederationName},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    []corev1.ServicePort{{Name: "federate", Port: metricsFederationPort, TargetPort: intstr.FromInt32(metricsFederationPort)}},
			},
		},
		deployment,
	}, nil
}

// metricsFederationEnvoyConfig routes exactly GET /federate (with its match[] query) to the
// platform Prometheus.
func metricsFederationEnvoyConfig() map[string]any {
	host, port, _ := strings.Cut(platformPrometheusTarget, ":")
	portNumber, _ := strconv.Atoi(port)
	return map[string]any{
		"static_resources": map[string]any{
			"listeners": []any{map[string]any{
				"name":    "federate",
				"address": map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": metricsFederationPort}},
				"filter_chains": []any{map[string]any{"filters": []any{map[string]any{
					"name": "envoy.filters.network.http_connection_manager",
					"typed_config": map[string]any{
						"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
						"stat_prefix": "federate",
						"route_config": map[string]any{"virtual_hosts": []any{map[string]any{
							"name":    "federate",
							"domains": []string{"*"},
							"routes": []any{
								map[string]any{
									"match": map[string]any{
										"path":    "/federate",
										"headers": []any{map[string]any{"name": ":method", "string_match": map[string]any{"exact": "GET"}}},
									},
									"route": map[string]any{"cluster": "prometheus", "timeout": "30s"},
								},
								map[string]any{"match": map[string]any{"prefix": "/"}, "direct_response": map[string]any{"status": 404}},
							},
						}}},
						"http_filters": []any{map[string]any{
							"name":         "envoy.filters.http.router",
							"typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"},
						}},
					},
				}}}},
			}},
			"clusters": []any{map[string]any{
				"name":            "prometheus",
				"type":            "STRICT_DNS",
				"connect_timeout": "5s",
				"load_assignment": map[string]any{
					"cluster_name": "prometheus",
					"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{"endpoint": map[string]any{
						"address": map[string]any{"socket_address": map[string]any{"address": host, "port_value": portNumber}},
					}}}}},
				},
			}},
		},
	}
}

func shortHash(data []byte) string {
	return secretsHash(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Data: map[string][]byte{"x": data}})
}

// configMapToUnstructured is a ConfigMap as an apply body.
func configMapToUnstructured(configMap *corev1.ConfigMap) (*unstructured.Unstructured, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMap)
	if err != nil {
		return nil, err
	}
	object := &unstructured.Unstructured{Object: data}
	object.SetAPIVersion("v1")
	object.SetKind("ConfigMap")
	unstructured.RemoveNestedField(object.Object, "metadata", "creationTimestamp")
	return object, nil
}
