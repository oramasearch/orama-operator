/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Observability on every cluster (operator#15): kube-prometheus-stack and Perses, on both
// installation paths, and nothing AGPL or Elastic-licensed. Grafana, Loki, Mimir and Tempo are
// AGPL-3.0, and kube-prometheus-stack installs Grafana as a subchart unless told not to.
func TestEveryClusterRunsPermissiveMetricsAlertingAndDashboards(t *testing.T) {
	paths := map[string]map[string]platformRelease{"helmfile.yaml": helmfilePlatform(t)}
	leaves, err := filepath.Glob(filepath.Join("..", "config", "clusters", "*", "platform.yaml"))
	if err != nil || len(leaves) == 0 {
		t.Fatalf("found no config/clusters/*/platform.yaml: %v", err)
	}
	for _, path := range leaves {
		paths[path] = leafPlatform(t, path)
	}
	forbidden := []string{"grafana", "loki", "mimir", "tempo", "elasticsearch", "eck-operator", "kibana", "opensearch-dashboards"}
	for path, releases := range paths {
		for chart := range releases {
			for _, name := range forbidden {
				if strings.Contains(chart, name) {
					t.Errorf("%s installs %s, which is not permissively licensed for resale", path, chart)
				}
			}
		}
		stack, ok := releases["kube-prometheus-stack"]
		if !ok {
			t.Errorf("%s does not install kube-prometheus-stack (Prometheus, Alertmanager, exporters)", path)
			continue
		}
		grafana, _ := stack.Values["grafana"].(map[string]any)
		if enabled, set := grafana["enabled"].(bool); !set || enabled {
			t.Errorf("%s: kube-prometheus-stack's Grafana subchart is not disabled (grafana.enabled: false)", path)
		}
		alertmanager, _ := stack.Values["alertmanager"].(map[string]any)
		spec, _ := alertmanager["alertmanagerSpec"].(map[string]any)
		selector, _ := spec["alertmanagerConfigSelector"].(map[string]any)
		labels, _ := selector["matchLabels"].(map[string]any)
		if labels["orama.cloud/alerting"] != "true" {
			t.Errorf("%s: Alertmanager does not select the operator's alert routing (orama.cloud/alerting)", path)
		}
		if _, ok := releases["perses"]; !ok {
			t.Errorf("%s does not install Perses (dashboards)", path)
		}
	}
}
