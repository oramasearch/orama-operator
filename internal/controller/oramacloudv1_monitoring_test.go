// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
		if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != platformMonitoringNamespace || peer.PodSelector.MatchLabels[prometheusPodsLabel] != prometheusPodsValue {
			t.Errorf("%s admits %+v, not only the platform's Prometheus", allowance.Name, peer)
		}
		if len(rule.Ports) != 1 {
			t.Errorf("%s opens %d ports", allowance.Name, len(rule.Ports))
		}
	}
}

// Rolling the operator out changes nothing on a stack until the platform layer is applied: without
// the monitoring CRDs no monitoring object is written; with them, every one is.
func TestOramaCloudV1MonitoringWaitsForThePlatformLayer(t *testing.T) {
	scheme := newLakeScheme(t)
	oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "v1-eu", Namespace: "v1-eu", UID: "uid-1"}}
	count := func(c client.Client) int {
		var configMaps corev1.ConfigMapList
		if err := c.List(context.Background(), &configMaps, client.InNamespace("v1-eu")); err != nil {
			t.Fatal(err)
		}
		return len(configMaps.Items)
	}
	without := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc).Build()
	if monitoringInstalled(without) {
		t.Fatal("the fake cluster reports monitoring CRDs")
	}
	if err := (&OramaCloudV1Reconciler{Client: without, Scheme: scheme}).reconcileMonitoring(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if n := count(without); n != 0 {
		t.Fatalf("%d monitoring objects written before the platform layer", n)
	}
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{podMonitorGVK, prometheusRuleGVK, {Version: "v1", Kind: "ConfigMap"}} {
		mapper.Add(gvk, apimeta.RESTScopeNamespace)
	}
	with := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc).WithRESTMapper(mapper).Build()
	if err := (&OramaCloudV1Reconciler{Client: with, Scheme: scheme}).reconcileMonitoring(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if n := count(with); n != 1 {
		t.Fatalf("dashboards ConfigMaps written: %d, want 1", n)
	}
	monitor := &unstructured.Unstructured{}
	monitor.SetGroupVersionKind(podMonitorGVK)
	if err := with.Get(context.Background(), client.ObjectKey{Namespace: "v1-eu", Name: "v1-eu-engine"}, monitor); err != nil {
		t.Fatalf("no engine PodMonitor once the CRDs exist: %v", err)
	}
}

// stackRules is the alerting rules of oc's <cr>-alerts PrometheusRule, by alert name.
func stackRules(t *testing.T, oc *oramav1alpha1.OramaCloudV1) (*unstructured.Unstructured, map[string]map[string]any) {
	t.Helper()
	objects, err := v1MonitoringObjects(oc)
	if err != nil {
		t.Fatal(err)
	}
	rule := hubObject(t, objects, "PrometheusRule")
	groups, _, _ := unstructured.NestedSlice(rule.Object, "spec", "groups")
	byAlert := map[string]map[string]any{}
	for _, r := range groups[0].(map[string]any)["rules"].([]any) {
		byAlert[r.(map[string]any)["alert"].(string)] = r.(map[string]any)
	}
	return rule, byAlert
}

func analyticsImportStack(processor, imports bool) *oramav1alpha1.OramaCloudV1 {
	oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "v1-eu", Namespace: "v1-eu"}}
	oc.Spec.Components.AnalyticsProcessor.Enabled = processor
	oc.Spec.Components.AnalyticsProcessor.Import.Enabled = imports
	return oc
}

// The analytics alerts exist only for a stack whose processor runs the import CronJob, and read
// that CronJob's kube-state-metrics series.
func TestOramaCloudV1AnalyticsAlertsFollowTheImport(t *testing.T) {
	analyticsAlerts := []string{"OramaAnalyticsStale", "OramaAnalyticsImportMetricsMissing"}
	_, rules := stackRules(t, analyticsImportStack(true, true))
	for _, alert := range analyticsAlerts {
		rule := rules[alert]
		if rule == nil {
			t.Fatalf("no %s with the import enabled", alert)
		}
		if expr := rule["expr"].(string); !strings.Contains(expr, `cronjob="v1-eu-analytics-import"`) || !strings.Contains(expr, `namespace="v1-eu"`) {
			t.Errorf("%s does not name the stack's import CronJob: %s", alert, expr)
		}
	}
	for _, off := range []*oramav1alpha1.OramaCloudV1{analyticsImportStack(true, false), analyticsImportStack(false, true)} {
		_, rules := stackRules(t, off)
		for _, alert := range analyticsAlerts {
			if rules[alert] != nil {
				t.Errorf("%s rendered with processor=%v import=%v", alert,
					off.Spec.Components.AnalyticsProcessor.Enabled, off.Spec.Components.AnalyticsProcessor.Import.Enabled)
			}
		}
	}
}

// In real Prometheus: a healthy import is silent, one whose last success is over 30 minutes old
// fires, the same import suspended does not, and a CronJob kube-state-metrics never saw succeed
// fires the missing-metrics alert.
func TestOramaCloudV1AnalyticsAlertsInPrometheus(t *testing.T) {
	_, promtool := prometheusBinaries(t)
	rule, _ := stackRules(t, analyticsImportStack(true, true))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), rulesFile(t, rule), 0o600); err != nil {
		t.Fatal(err)
	}
	// series is one input series of v1-eu's import CronJob, as kube-state-metrics labels it.
	series := func(metric, values string) string {
		return fmt.Sprintf("      - {series: '%s{namespace=\"v1-eu\", cronjob=\"v1-eu-analytics-import\"}', values: '%s'}\n", metric, values)
	}
	tests := `rule_files: [rules.yaml]
evaluation_interval: 1m
tests:
  # Healthy: the last success follows the clock.
  - interval: 1m
    input_series:
` + series("kube_cronjob_status_last_successful_time", "0+60x60") +
		series("kube_cronjob_spec_suspend", "0x60") + `
    alert_rule_test:
      - {eval_time: 60m, alertname: OramaAnalyticsStale, exp_alerts: []}
      - {eval_time: 60m, alertname: OramaAnalyticsImportMetricsMissing, exp_alerts: []}
  # Stalled: the last success stays at 0 s; it is 30 minutes old at 30m and pending for 5 more.
  - interval: 1m
    input_series:
` + series("kube_cronjob_status_last_successful_time", "0x60") +
		series("kube_cronjob_spec_suspend", "0x60") + `
    alert_rule_test:
      - {eval_time: 34m, alertname: OramaAnalyticsStale, exp_alerts: []}
      - eval_time: 37m
        alertname: OramaAnalyticsStale
        exp_alerts:
          - exp_labels: {severity: warning, namespace: v1-eu, cronjob: v1-eu-analytics-import}
            exp_annotations: {summary: "Orama Cloud v1 v1-eu: analytics have not been imported and aggregated for 30 minutes; the dashboard's analytics are going stale."}
  # Stalled but suspended: expected, silent.
  - interval: 1m
    input_series:
` + series("kube_cronjob_status_last_successful_time", "0x60") +
		series("kube_cronjob_spec_suspend", "1x60") + `
    alert_rule_test:
      - {eval_time: 60m, alertname: OramaAnalyticsStale, exp_alerts: []}
  # Never succeeded: no last-success series at all.
  - interval: 1m
    input_series:
` + series("kube_cronjob_spec_suspend", "0x60") + `
    alert_rule_test:
      - {eval_time: 29m, alertname: OramaAnalyticsImportMetricsMissing, exp_alerts: []}
      - eval_time: 32m
        alertname: OramaAnalyticsImportMetricsMissing
        exp_alerts:
          - exp_labels: {severity: warning, namespace: v1-eu, cronjob: v1-eu-analytics-import}
            exp_annotations: {summary: "kube-state-metrics reports no successful run of v1-eu-analytics-import: OramaAnalyticsStale cannot fire."}
`
	if err := os.WriteFile(filepath.Join(dir, "tests.yaml"), []byte(tests), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(promtool, "test", "rules", filepath.Join(dir, "tests.yaml")).CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules: %v\n%s", err, out)
	}
}
