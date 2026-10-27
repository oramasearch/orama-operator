/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Monitoring of one Orama Cloud v1 stack (operator#15), on the platform layer's
// kube-prometheus-stack and Perses:
//
//   - scrape configuration: the OramaCore writer and readers' /metrics on :8080 (never on the public
//     hosts, engine_metrics.go) and the control database's CloudNativePG exporter (:9187);
//   - alert rules: a component not ready (from the stack's own status), the engine's metrics gone,
//     slow searches, the control database down or its replicas behind;
//   - dashboards: the whole stack on one page, and one per component.
//
// The stack's NetworkSandbox lets only the platform's Prometheus pods reach those two ports
// (oramaCloudScrapeAllowances). The broker is not scraped: RabbitMQ's Prometheus plugin is not
// enabled on the broker, and enabling it restarts the broker, so it waits for a planned window.

const (
	// prometheusPodsLabel selects the Prometheus pods prometheus-operator runs.
	prometheusPodsLabel = "app.kubernetes.io/name"
	prometheusPodsValue = "prometheus"
	cnpgMetricsPort     = 9187
)

// v1MonitoredComponents are the components that get a dashboard; the critical ones page when they
// are not ready.
var v1MonitoredComponents = []struct {
	Name     string
	Critical bool
}{
	{"writer", true},
	{"reader", true},
	{"broker", true},
	{"controldb", true},
	{"cache", true},
	{"analytics", false},
	{"dashboard", true},
	{"workers", false},
	{"search", false},
	{"disco", false},
}

func v1EngineMonitorName(oc *oramav1alpha1.OramaCloudV1) string    { return oc.Name + "-engine" }
func v1ControlDBMonitorName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-controldb" }

// oramaCloudScrapeAllowances let only the platform's Prometheus reach the engine's and the control
// database's metrics ports.
func oramaCloudScrapeAllowances(engine, cnpgPods metav1.LabelSelector) []oramav1alpha1.NetworkSandboxPolicy {
	prometheus := []networkingv1.NetworkPolicyPeer{namespacePodPeer(pixieMonitoringNamespace, map[string]string{prometheusPodsLabel: prometheusPodsValue})}
	return []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance("The platform's Prometheus scrapes the writer's and readers' /metrics on :8080 (operator#15).",
			networkPolicy("allow-prometheus-engine-scrape", engine, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: prometheus, Ports: tcpPorts(oramacorePort)}}, nil)),
		sandboxAllowance("The platform's Prometheus scrapes CloudNativePG's exporter on :9187 (operator#15).",
			networkPolicy("allow-prometheus-controldb-scrape", cnpgPods, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: prometheus, Ports: tcpPorts(cnpgMetricsPort)}}, nil)),
	}
}

func v1PodMonitor(oc *oramav1alpha1.OramaCloudV1, name string, selector map[string]any, endpoint map[string]any) *unstructured.Unstructured {
	monitor := &unstructured.Unstructured{}
	monitor.SetGroupVersionKind(podMonitorGVK)
	monitor.SetNamespace(oc.Namespace)
	monitor.SetName(name)
	monitor.SetLabels(labelsFor(oc, "monitoring"))
	monitor.Object["spec"] = map[string]any{
		"selector":            selector,
		"namespaceSelector":   map[string]any{"matchNames": []any{oc.Namespace}},
		"podMetricsEndpoints": []any{endpoint},
	}
	return monitor
}

// v1MonitoringObjects is every monitoring object of one stack.
func v1MonitoringObjects(oc *oramav1alpha1.OramaCloudV1) ([]*unstructured.Unstructured, error) {
	ns := oc.Namespace
	engine := v1PodMonitor(oc, v1EngineMonitorName(oc), map[string]any{
		"matchLabels": map[string]any{"app.kubernetes.io/instance": oc.Name, "app.kubernetes.io/managed-by": "orama-cloud-v1-operator"},
		"matchExpressions": []any{map[string]any{
			"key": "app.kubernetes.io/component", "operator": "In", "values": []any{"writer", "reader"},
		}},
	}, map[string]any{
		// By number: a reader pod also has Envoy's port named http, which answers 404 here.
		"portNumber": int64(oramacorePort), "path": "/metrics", "interval": "30s",
	})
	controlDB := v1PodMonitor(oc, v1ControlDBMonitorName(oc), map[string]any{
		"matchLabels": map[string]any{"cnpg.io/cluster": oc.Name + "-controldb"},
	}, map[string]any{"port": "metrics", "interval": "30s"})

	stack := fmt.Sprintf(`namespace=%q, name=%q`, ns, oc.Name)
	var critical []string
	for _, component := range v1MonitoredComponents {
		if component.Critical {
			critical = append(critical, component.Name)
		}
	}
	rules := []alertRule{
		{
			Alert:    "OramaStackComponentDown",
			Expr:     fmt.Sprintf(`orama_oramacloudv1_component_ready{%s, component=~%q} == 0`, stack, joinAlternatives(critical)),
			For:      "10m",
			Severity: "critical",
			Summary:  "Orama Cloud v1 {{ $labels.name }}: {{ $labels.component }} has not been ready for 10 minutes.",
		},
		{
			Alert:    "OramaStackComponentNotReady",
			Expr:     fmt.Sprintf(`orama_oramacloudv1_component_ready{%s, component!~%q} == 0`, stack, joinAlternatives(critical)),
			For:      "30m",
			Severity: "warning",
			Summary:  "Orama Cloud v1 {{ $labels.name }}: {{ $labels.component }} has not been ready for 30 minutes.",
		},
		{
			Alert:    "OramaEngineMetricsDown",
			Expr:     fmt.Sprintf(`up{namespace=%q, job=%q} == 0`, ns, ns+"/"+v1EngineMonitorName(oc)),
			For:      "5m",
			Severity: "critical",
			Summary:  "The OramaCore {{ $labels.pod }} has not answered /metrics for 5 minutes.",
		},
		{
			Alert:    "OramaSearchSlow",
			Expr:     fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(search_calculation_time_sec_bucket{namespace=%q}[5m]))) > 0.5`, ns),
			For:      "15m",
			Severity: "warning",
			Summary:  "Orama Cloud v1 searches have taken over 0.5 s at the 95th percentile for 15 minutes.",
		},
		{
			Alert:    "OramaControlDBMetricsDown",
			Expr:     fmt.Sprintf(`up{namespace=%q, job=%q} == 0`, ns, ns+"/"+v1ControlDBMonitorName(oc)),
			For:      "5m",
			Severity: "critical",
			Summary:  "The control database instance {{ $labels.pod }} has not answered its exporter for 5 minutes.",
		},
		{
			Alert:    "OramaControlDBReplicationLag",
			Expr:     fmt.Sprintf(`max(cnpg_pg_replication_lag{namespace=%q, pod=~%q}) > 300`, ns, oc.Name+"-controldb-.*"),
			For:      "15m",
			Severity: "warning",
			Summary:  "A control database replica has been over 5 minutes behind for 15 minutes.",
		},
	}
	ruleObject := prometheusRuleObject(ns, oc.Name+"-alerts", labelsFor(oc, "monitoring"), "orama.cloud-v1."+oc.Name, rules)

	dashboards, err := v1Dashboards(oc)
	if err != nil {
		return nil, err
	}
	// The dashboards first: they need no CRD, so they are written even before the platform layer.
	return []*unstructured.Unstructured{dashboards, engine, controlDB, ruleObject}, nil
}

func joinAlternatives(names []string) string {
	out := ""
	for i, name := range names {
		if i > 0 {
			out += "|"
		}
		out += name
	}
	return out
}

// v1Dashboards is the stack's Perses Project, overview and per-component dashboards, as one
// ConfigMap (applied as unstructured with the rest).
func v1Dashboards(oc *oramav1alpha1.OramaCloudV1) (*unstructured.Unstructured, error) {
	ns := oc.Namespace
	stack := fmt.Sprintf(`namespace=%q, name=%q`, ns, oc.Name)
	overview := persesDashboard(ns, oc.Name+"-overview", "Orama Cloud v1 "+oc.Name+": overview", []persesRow{
		{Title: "Health", Panels: []persesPanel{
			{Title: "Components ready", Query: fmt.Sprintf(`orama_oramacloudv1_component_ready{%s}`, stack), Legend: "{{component}}"},
			{Title: "Stack phase", Query: fmt.Sprintf(`orama_oramacloudv1_phase{%s} == 1`, stack), Legend: "{{phase}}"},
			{Title: "Container restarts (1h)", Query: fmt.Sprintf(`sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace=%q}[1h])) > 0`, ns), Legend: "{{pod}}"},
			{Title: "Firing alerts", Query: fmt.Sprintf(`sum by (alertname) (ALERTS{namespace=%q, alertstate="firing"})`, ns), Legend: "{{alertname}}"},
		}},
		{Title: "Traffic", Panels: []persesPanel{
			{Title: "Searches per second", Query: fmt.Sprintf(`sum(rate(search_calculation_time_sec_count{namespace=%q}[5m]))`, ns), Legend: "searches"},
			{Title: "Search latency p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(search_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
			{Title: "Documents written per second", Query: fmt.Sprintf(`sum(rate(writer_doc_calculation_time_sec_count{namespace=%q}[5m]))`, ns), Legend: "documents"},
			{Title: "Engine scrape health", Query: fmt.Sprintf(`up{namespace=%q, job=%q}`, ns, ns+"/"+v1EngineMonitorName(oc)), Legend: "{{pod}}"},
		}},
		{Title: "Resources", Panels: []persesPanel{
			{Title: "CPU by pod (cores)", Query: fmt.Sprintf(`topk(15, sum by (pod) (rate(container_cpu_usage_seconds_total{namespace=%q, container!=""}[5m])))`, ns), Legend: "{{pod}}"},
			{Title: "Memory by pod", Query: fmt.Sprintf(`topk(15, sum by (pod) (container_memory_working_set_bytes{namespace=%q, container!=""}))`, ns), Legend: "{{pod}}", Unit: "bytes"},
			{Title: "Volume use", Query: fmt.Sprintf(`kubelet_volume_stats_used_bytes{namespace=%q} / kubelet_volume_stats_capacity_bytes{namespace=%q}`, ns, ns), Legend: "{{persistentvolumeclaim}}", Unit: "percent-decimal"},
		}},
	})
	dashboards := []map[string]any{overview}
	for _, component := range v1MonitoredComponents {
		rows := []persesRow{{Title: "Workload", Panels: workloadPanels(ns, oc.Name+"-"+component.Name+"-.*")}}
		switch component.Name {
		case "writer":
			rows = append(rows, persesRow{Title: "Writes", Panels: []persesPanel{
				{Title: "Documents per second", Query: fmt.Sprintf(`sum(rate(writer_doc_calculation_time_sec_count{namespace=%q}[5m]))`, ns), Legend: "documents"},
				{Title: "Document write p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(writer_doc_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
				{Title: "Commit p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(commit_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
				{Title: "Broker enqueue p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(rabbitmq_enqueue_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
			}})
		case "reader":
			rows = append(rows, persesRow{Title: "Searches", Panels: []persesPanel{
				{Title: "Searches per second", Query: fmt.Sprintf(`sum by (pod) (rate(search_calculation_time_sec_count{namespace=%q}[5m]))`, ns), Legend: "{{pod}}"},
				{Title: "Search p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(search_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
				{Title: "Embedding p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(embedding_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
				{Title: "Answer (chat) p95", Query: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(chat_calculation_time_sec_bucket{namespace=%q}[5m])))`, ns), Legend: "p95", Unit: "seconds"},
			}})
		case "controldb":
			rows = append(rows, persesRow{Title: "Postgres", Panels: []persesPanel{
				{Title: "Replication lag", Query: fmt.Sprintf(`cnpg_pg_replication_lag{namespace=%q}`, ns), Legend: "{{pod}}", Unit: "seconds"},
				{Title: "Backends", Query: fmt.Sprintf(`sum by (pod) (cnpg_backends_total{namespace=%q})`, ns), Legend: "{{pod}}"},
				{Title: "Database size", Query: fmt.Sprintf(`cnpg_pg_database_size_bytes{namespace=%q}`, ns), Legend: "{{datname}}", Unit: "bytes"},
				{Title: "Exporter health", Query: fmt.Sprintf(`up{namespace=%q, job=%q}`, ns, ns+"/"+v1ControlDBMonitorName(oc)), Legend: "{{pod}}"},
			}})
		}
		dashboards = append(dashboards, persesDashboard(ns, oc.Name+"-"+component.Name, "Orama Cloud v1 "+oc.Name+": "+component.Name, rows))
	}
	configMap, err := persesConfigMap(ns, oc.Name+"-dashboards", ns, labelsFor(oc, "monitoring"), dashboards...)
	if err != nil {
		return nil, err
	}
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

// reconcileMonitoring writes the stack's scrape configuration, rules and dashboards. Without the
// platform layer's monitoring CRDs it writes nothing and says so in the log; the next pass after
// they are installed writes them.
func (r *OramaCloudV1Reconciler) reconcileMonitoring(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	objects, err := v1MonitoringObjects(oc)
	if err != nil {
		return err
	}
	if err := applyMonitoringObjects(ctx, r.Client, r.Scheme, oc, objects); err != nil {
		if errors.Is(err, errMonitoringCRDsMissing) {
			logf.FromContext(ctx).Info("monitoring not written: " + err.Error())
			return nil
		}
		return err
	}
	return nil
}
