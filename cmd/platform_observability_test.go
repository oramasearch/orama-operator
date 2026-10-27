/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
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

// observabilityInventory is hack/observability-images.yaml.
type observabilityInventory struct {
	Charts []struct {
		Chart, Version, File, SHA256 string
	} `json:"charts"`
	Platform []struct {
		Mirror, Upstream, Licence string
	} `json:"platform"`
}

// The platform charts, rendered offline from the vendored tarballs with exactly the values both
// installation paths install, pull only Orama's mirrors listed in the inventory, and every pod and
// every Prometheus and Alertmanager carries the cluster's pull Secret. A public image anywhere
// fails: air-gapped means no fallback.
func TestPlatformChartsRenderOfflineWithOnlyMirroredImages(t *testing.T) {
	var inventory observabilityInventory
	readYAML(t, filepath.Join("..", "hack", "observability-images.yaml"), &inventory)
	mirrors := map[string]bool{}
	for _, image := range inventory.Platform {
		if !strings.HasPrefix(image.Mirror, "docker.io/oramasearch/platform-") {
			t.Errorf("inventory mirror %s is not one of Orama's private repositories", image.Mirror)
		}
		mirrors[image.Mirror] = true
	}
	releases := helmfilePlatform(t)
	seen := map[string]bool{}
	for _, chartEntry := range inventory.Charts {
		data, err := os.ReadFile(filepath.Join("..", chartEntry.File))
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != chartEntry.SHA256 {
			t.Fatalf("%s does not match its recorded sha256", chartEntry.File)
		}
		release, ok := releases[chartEntry.Chart]
		if !ok || release.Version != chartEntry.Version {
			t.Fatalf("helmfile.yaml installs %s %s, the vendored chart is %s", chartEntry.Chart, release.Version, chartEntry.Version)
		}
		chart, err := loader.LoadArchive(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		install := action.NewInstall(&action.Configuration{})
		install.DryRun, install.ClientOnly, install.Replace = true, true, true
		install.ReleaseName, install.Namespace = release.Release, release.Namespace
		// Every cluster runs Kubernetes 1.35 (config/clusters/*/cluster.yaml).
		install.KubeVersion = &chartutil.KubeVersion{Version: "v1.35.0", Major: "1", Minor: "35"}
		rendered, err := install.Run(chart, release.Values)
		if err != nil {
			t.Fatalf("render %s: %v", chartEntry.Chart, err)
		}
		decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered.Manifest), 4096)
		for {
			var object map[string]any
			if err := decoder.Decode(&object); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if object == nil {
				continue
			}
			kind, _ := object["kind"].(string)
			metadata, _ := object["metadata"].(map[string]any)
			name, _ := metadata["name"].(string)
			spec, _ := object["spec"].(map[string]any)
			var pod map[string]any
			switch kind {
			case "Deployment", "DaemonSet", "StatefulSet", "Job":
				template, _ := spec["template"].(map[string]any)
				pod, _ = template["spec"].(map[string]any)
			case "Prometheus", "Alertmanager":
				pod = spec
				if image, _ := spec["image"].(string); image != "" {
					seen[image] = true
				}
				if kind == "Prometheus" && !excludesHubRules(spec) {
					t.Errorf("the platform Prometheus %s would evaluate the hub's rules, which read series it does not have", name)
				}
			default:
				continue
			}
			if secrets, _ := pod["imagePullSecrets"].([]any); len(secrets) == 0 {
				t.Errorf("%s %s %s pulls without the cluster's pull Secret", chartEntry.Chart, kind, name)
			}
			for _, key := range []string{"initContainers", "containers"} {
				containers, _ := pod[key].([]any)
				for _, c := range containers {
					container, _ := c.(map[string]any)
					if image, _ := container["image"].(string); image != "" {
						seen[image] = true
					}
					args, _ := container["args"].([]any)
					for _, arg := range args {
						text, _ := arg.(string)
						for _, flag := range []string{"--prometheus-config-reloader=", "--thanos-default-base-image="} {
							if strings.HasPrefix(text, flag) {
								seen[strings.TrimPrefix(text, flag)] = true
							}
						}
					}
				}
			}
		}
	}
	for image := range seen {
		if !mirrors[image] {
			t.Errorf("the platform renders %s, which is not an inventoried mirror", image)
		}
	}
	for image := range mirrors {
		if !seen[image] {
			t.Errorf("the inventory lists %s, which nothing renders", image)
		}
	}
}

// excludesHubRules reports whether a Prometheus' ruleSelector leaves out rules labelled
// orama.cloud/metrics-scope=hub (internal/controller/observability_metrics.go).
func excludesHubRules(spec map[string]any) bool {
	selector, _ := spec["ruleSelector"].(map[string]any)
	expressions, _ := selector["matchExpressions"].([]any)
	for _, e := range expressions {
		expression, _ := e.(map[string]any)
		values, _ := expression["values"].([]any)
		if expression["key"] == "orama.cloud/metrics-scope" && expression["operator"] == "NotIn" && len(values) == 1 && values[0] == "hub" {
			return true
		}
	}
	return false
}
