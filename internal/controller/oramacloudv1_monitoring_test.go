/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestOramaCloudV1MonitoringScrapesOnlyTheEngineAndControlDB(t *testing.T) {
	oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "v1-eu", Namespace: "v1-eu"}}
	objects, err := v1MonitoringObjects(oc)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*unstructured.Unstructured{}
	for _, object := range objects {
		byName[object.GetKind()+"/"+object.GetName()] = object
	}
	engine := byName["PodMonitor/v1-eu-engine"]
	if engine == nil {
		t.Fatal("no engine PodMonitor")
	}
	endpoints, _, _ := unstructured.NestedSlice(engine.Object, "spec", "podMetricsEndpoints")
	endpoint := endpoints[0].(map[string]any)
	// A reader pod also names Envoy's port http, and Envoy answers 404 on /metrics.
	if endpoint["portNumber"] != int64(oramacorePort) || endpoint["port"] != nil || endpoint["path"] != "/metrics" {
		t.Errorf("engine endpoint %v: want port number %d, /metrics", endpoint, oramacorePort)
	}
	expressions, _, _ := unstructured.NestedSlice(engine.Object, "spec", "selector", "matchExpressions")
	if values := expressions[0].(map[string]any)["values"]; len(values.([]any)) != 2 {
		t.Errorf("the engine PodMonitor selects %v, want the writer and readers", values)
	}
	if byName["PodMonitor/v1-eu-controldb"] == nil || byName["PrometheusRule/v1-eu-alerts"] == nil {
		t.Fatalf("missing objects: %v", byName)
	}
	groups, _, _ := unstructured.NestedSlice(byName["PrometheusRule/v1-eu-alerts"].Object, "spec", "groups")
	rules := groups[0].(map[string]any)["rules"].([]any)
	for _, rule := range rules {
		expr := rule.(map[string]any)["expr"].(string)
		if !strings.Contains(expr, `"v1-eu"`) {
			t.Errorf("rule %s is not scoped to the stack's namespace: %s", rule.(map[string]any)["alert"], expr)
		}
	}
	dashboards := byName["ConfigMap/v1-eu-dashboards"]
	if dashboards.GetLabels()[persesResourceLabel] != "true" {
		t.Error("the dashboards are not labelled for Perses' provisioning sidecar")
	}
	data, _, _ := unstructured.NestedStringMap(dashboards.Object, "data")
	if _, ok := data["v1-eu-overview.json"]; !ok || data["project.json"] == "" {
		t.Fatalf("no overview or project in %d files", len(data))
	}
	for _, component := range v1MonitoredComponents {
		raw, ok := data["v1-eu-"+component.Name+".json"]
		if !ok {
			t.Errorf("no dashboard for %s", component.Name)
			continue
		}
		var dashboard map[string]any
		if err := json.Unmarshal([]byte(raw), &dashboard); err != nil || dashboard["kind"] != "Dashboard" {
			t.Errorf("%s dashboard is not a Perses Dashboard: %v", component.Name, err)
		}
	}
	if objects[0].GetKind() != "ConfigMap" {
		t.Error("dashboards are not written first, before the objects that need the monitoring CRDs")
	}
}

func TestOramaCloudV1ScrapeAllowancesAdmitOnlyPrometheus(t *testing.T) {
	engine := metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "writer"}}
	cnpg := metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": "v1-eu-controldb"}}
	allowances := oramaCloudScrapeAllowances(engine, cnpg)
	if len(allowances) != 2 {
		t.Fatalf("allowances %v", allowances)
	}
	for _, allowance := range allowances {
		if len(allowance.Spec.Egress) != 0 || len(allowance.Spec.Ingress) != 1 {
			t.Fatalf("%s is not one ingress rule", allowance.Name)
		}
		rule := allowance.Spec.Ingress[0]
		peer := rule.From[0]
		if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != pixieMonitoringNamespace || peer.PodSelector.MatchLabels[prometheusPodsLabel] != prometheusPodsValue {
			t.Errorf("%s admits %+v, not only the platform's Prometheus", allowance.Name, peer)
		}
		if len(rule.Ports) != 1 {
			t.Errorf("%s opens %d ports", allowance.Name, len(rule.Ports))
		}
	}
}
