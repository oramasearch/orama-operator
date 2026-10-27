/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Perses dashboards (operator#15). Perses (Apache-2.0) runs on every cluster from the platform
// layer, read-only, and loads every ConfigMap labelled perses.dev/resource=true through its
// provisioning sidecar. The operator writes one such ConfigMap next to each thing it runs: a
// Project, an overview dashboard and one dashboard per component. Each dashboard carries its own
// Prometheus datasource, the platform's Prometheus, so nothing else has to exist first.

const (
	persesResourceLabel = "perses.dev/resource"
	// platformPrometheusURL is kube-prometheus-stack's Prometheus Service in the platform layer.
	platformPrometheusURL = "http://kube-prometheus-stack-prometheus.monitoring.svc:9090"
)

// persesPanel is one time-series chart.
type persesPanel struct {
	Title, Query, Legend string
	// Unit is a Perses format unit: "percent-decimal", "bytes", "seconds", "decimal".
	Unit string
}

// persesRow is a titled band of panels, laid out two per line.
type persesRow struct {
	Title  string
	Panels []persesPanel
}

// persesDashboard is a Perses Dashboard resource on the cluster's platform Prometheus.
func persesDashboard(project, name, title string, rows []persesRow) map[string]any {
	return persesDashboardOn(platformPrometheusURL, project, name, title, rows)
}

// persesDashboardOn is a Perses Dashboard resource whose datasource is the Prometheus at url.
func persesDashboardOn(url, project, name, title string, rows []persesRow) map[string]any {
	panels := map[string]any{}
	layouts := []any{}
	index := 0
	for _, row := range rows {
		items := []any{}
		for i, panel := range row.Panels {
			key := fmt.Sprintf("p%02d", index)
			index++
			unit := panel.Unit
			if unit == "" {
				unit = "decimal"
			}
			panels[key] = map[string]any{
				"kind": "Panel",
				"spec": map[string]any{
					"display": map[string]any{"name": panel.Title},
					"plugin": map[string]any{"kind": "TimeSeriesChart", "spec": map[string]any{
						"yAxis": map[string]any{"format": map[string]any{"unit": unit}},
					}},
					"queries": []any{map[string]any{
						"kind": "TimeSeriesQuery",
						"spec": map[string]any{"plugin": map[string]any{
							"kind": "PrometheusTimeSeriesQuery",
							"spec": map[string]any{"query": panel.Query, "seriesNameFormat": panel.Legend},
						}},
					}},
				},
			}
			items = append(items, map[string]any{
				"x": (i % 2) * 12, "y": (i / 2) * 7, "width": 12, "height": 7,
				"content": map[string]any{"$ref": "#/spec/panels/" + key},
			})
		}
		layouts = append(layouts, map[string]any{
			"kind": "Grid",
			"spec": map[string]any{"display": map[string]any{"title": row.Title}, "items": items},
		})
	}
	return map[string]any{
		"kind":     "Dashboard",
		"metadata": map[string]any{"name": name, "project": project},
		"spec": map[string]any{
			"display":  map[string]any{"name": title},
			"duration": "1h",
			"datasources": map[string]any{"prometheus": map[string]any{
				"default": true,
				"plugin": map[string]any{"kind": "PrometheusDatasource", "spec": map[string]any{
					"proxy": map[string]any{"kind": "HTTPProxy", "spec": map[string]any{"url": url}},
				}},
			}},
			"panels":  panels,
			"layouts": layouts,
		},
	}
}

// persesConfigMap holds a Project and its dashboards, one file each, for the provisioning sidecar.
func persesConfigMap(namespace, name, project string, labels map[string]string, dashboards ...map[string]any) (*corev1.ConfigMap, error) {
	data := map[string]string{}
	projectJSON, err := json.Marshal(map[string]any{"kind": "Project", "metadata": map[string]any{"name": project}, "spec": map[string]any{}})
	if err != nil {
		return nil, err
	}
	data["project.json"] = string(projectJSON)
	for _, dashboard := range dashboards {
		encoded, err := json.Marshal(dashboard)
		if err != nil {
			return nil, err
		}
		name := dashboard["metadata"].(map[string]any)["name"].(string)
		data[name+".json"] = string(encoded)
	}
	all := copyStringMap(labels)
	all[persesResourceLabel] = "true"
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: all}, Data: data}, nil
}

// workloadPanels are the panels every component gets: availability, restarts, CPU and memory,
// from kube-state-metrics and the kubelet, which need nothing from the component itself.
func workloadPanels(namespace, podRegex string) []persesPanel {
	pods := fmt.Sprintf(`namespace=%q, pod=~%q`, namespace, podRegex)
	return []persesPanel{
		{Title: "Ready pods", Query: fmt.Sprintf(`sum(kube_pod_status_ready{%s, condition="true"})`, pods), Legend: "ready"},
		{Title: "Container restarts (1h)", Query: fmt.Sprintf(`sum by (pod) (increase(kube_pod_container_status_restarts_total{%s}[1h]))`, pods), Legend: "{{pod}}"},
		{Title: "CPU (cores)", Query: fmt.Sprintf(`sum by (pod) (rate(container_cpu_usage_seconds_total{%s, container!=""}[5m]))`, pods), Legend: "{{pod}}"},
		{Title: "Memory (working set)", Query: fmt.Sprintf(`sum by (pod) (container_memory_working_set_bytes{%s, container!=""})`, pods), Legend: "{{pod}}", Unit: "bytes"},
	}
}
