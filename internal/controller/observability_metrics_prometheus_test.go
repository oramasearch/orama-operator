/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// These tests run the hub's rendered configuration and rules in real Prometheus 3.15 (the version
// the platform mirror holds), not against strings. They need PROMETHEUS_BINARY_DIR, a directory with
// prometheus and promtool (the release tarball, sha256-checked); without it they are skipped, as the
// envtests are without KUBEBUILDER_ASSETS.

func prometheusBinaries(t *testing.T) (prometheus, promtool string) {
	t.Helper()
	dir := os.Getenv("PROMETHEUS_BINARY_DIR")
	if dir == "" {
		t.Skip("PROMETHEUS_BINARY_DIR is required to run the hub's metrics in real Prometheus")
	}
	return filepath.Join(dir, "prometheus"), filepath.Join(dir, "promtool")
}

// testHubCloud is a prod-support hub with two production agents on other clusters and one whose
// metrics never arrive.
func testHubCloud() *oramav1alpha1.PixieCloud {
	cloud := testPixieCloud()
	cloud.Spec.Cluster = "support"
	cloud.Spec.Agents = []oramav1alpha1.PixieCloudAgent{
		{Name: "eu", Cluster: "prod-eu"}, {Name: "us", Cluster: "prod-us"}, {Name: "gone", Cluster: "prod-gone"},
	}
	return cloud
}

// rulesFile is a PrometheusRule's groups as a Prometheus rule file.
func rulesFile(t *testing.T, rule *unstructured.Unstructured) []byte {
	t.Helper()
	groups, _, _ := unstructured.NestedSlice(rule.Object, "spec", "groups")
	data, err := sigsyaml.Marshal(map[string]any{"groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hubObject(t *testing.T, objects []*unstructured.Unstructured, kind string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind {
			return object
		}
	}
	t.Fatalf("no %s among the hub's metrics objects", kind)
	return nil
}

// checkHubRules loads cloud's rendered scrape and Alertmanager configuration in promtool, then runs
// promtool's rule tests on its rendered rules.
func checkHubRules(t *testing.T, cloud *oramav1alpha1.PixieCloud, tests string) {
	t.Helper()
	_, promtool := prometheusBinaries(t)
	dir := t.TempDir()
	objects, err := hubMetricsObjects(cloud)
	if err != nil {
		t.Fatal(err)
	}
	secret := hubObject(t, objects, "Secret")
	data, _, _ := unstructured.NestedStringMap(secret.Object, "stringData")
	var scrape, alertmanagers []any
	if err := sigsyaml.Unmarshal([]byte(data["scrape.yaml"]), &scrape); err != nil {
		t.Fatal(err)
	}
	if err := sigsyaml.Unmarshal([]byte(data["alertmanagers.yaml"]), &alertmanagers); err != nil {
		t.Fatal(err)
	}
	// prometheus-operator puts additionalScrapeConfigs and additionalAlertManagerConfigs into the
	// generated configuration as they are.
	config, err := sigsyaml.Marshal(map[string]any{
		"global":         map[string]any{"scrape_interval": "60s"},
		"scrape_configs": scrape,
		"alerting":       map[string]any{"alertmanagers": alertmanagers},
		"rule_files":     []string{"rules.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("rules.yaml", rulesFile(t, hubObject(t, objects, "PrometheusRule")))
	if out, err := exec.Command(promtool, "check", "config", write("prometheus.yml", config)).CombinedOutput(); err != nil {
		t.Fatalf("promtool check config: %v\n%s", err, out)
	}
	if out, err := exec.Command(promtool, "test", "rules", write("tests.yaml", []byte(tests))).CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules: %v\n%s", err, out)
	}
}

// The rules page as intended: a cluster lost entirely, a cluster whose metrics job is missing, a
// reachable cluster whose own rules or Alertmanager stopped, that cluster's critical alerts relayed;
// and nothing for a healthy cluster.
func TestHubMetricsRulesInPrometheus(t *testing.T) {
	checkHubRules(t, testHubCloud(), `rule_files: [rules.yaml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      # prod-eu: healthy.
      - {series: 'up{job="federate-prod-eu", cluster="prod-eu"}', values: '1x40'}
      - {series: 'ALERTS{alertname="Watchdog", alertstate="firing", severity="none", cluster="prod-eu"}', values: '1x40'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="prod-eu"}', values: '1x40'}
      # prod-us: the whole cluster is lost at minute 10; its last federated series go stale.
      - {series: 'up{job="federate-prod-us", cluster="prod-us"}', values: '1x10 0x30'}
      - {series: 'ALERTS{alertname="Watchdog", alertstate="firing", severity="none", cluster="prod-us"}', values: '1x10'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="prod-us"}', values: '1x10'}
      # support (the hub's own cluster): reachable, but its rules stopped and its Alertmanager is down.
      - {series: 'up{job="federate-support", cluster="support"}', values: '1x40'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="support"}', values: '0x40'}
      - {series: 'ALERTS{alertname="OramaStackComponentDown", alertstate="firing", severity="critical", namespace="v1", component="writer", cluster="support"}', values: '1x40'}
      # The same alert on healthy prod-eu is delivered there, and not relayed.
      - {series: 'ALERTS{alertname="OramaStackComponentDown", alertstate="firing", severity="critical", namespace="v1", component="writer", cluster="prod-eu"}', values: '1x40'}
      # prod-gone: no metrics job at all.
    alert_rule_test:
      - eval_time: 12m
        alertname: ObservabilityClusterUnreachable
        exp_alerts: []
      - eval_time: 20m
        alertname: ObservabilityClusterUnreachable
        exp_alerts:
          - exp_labels: {severity: critical, job: federate-prod-us, cluster: prod-us}
            exp_annotations: {summary: "The hub cannot read cluster prod-us's metrics: the cluster, its link to the hub or its Prometheus is down."}
      - eval_time: 20m
        alertname: ObservabilityClusterMetricsMissing
        exp_alerts:
          - exp_labels: {severity: critical, job: federate-prod-gone, cluster: prod-gone}
            exp_annotations: {summary: "The hub has no metrics job for cluster prod-gone: its metrics are not collected."}
      - eval_time: 20m
        alertname: ObservabilityClusterAlertingDown
        exp_alerts:
          - exp_labels: {severity: critical, cluster: support}
            exp_annotations: {summary: "Cluster support is reachable but its Watchdog alert is gone: its own alert rules are not running."}
      - eval_time: 20m
        alertname: ObservabilityClusterAlertmanagerDown
        exp_alerts:
          - exp_labels: {severity: critical, job: kube-prometheus-stack-alertmanager, cluster: support}
            exp_annotations: {summary: "Cluster support's Alertmanager is down: its own alerts are not delivered."}
      - eval_time: 20m
        alertname: ObservabilityRelayedCriticalAlert
        exp_alerts:
          - exp_labels: {severity: critical, relayed_alert: OramaStackComponentDown, namespace: v1, component: writer, cluster: support}
            exp_annotations: {summary: "Cluster support: OramaStackComponentDown is firing in v1, and that cluster's Alertmanager cannot deliver it."}
      - eval_time: 35m
        alertname: ObservabilityClusterAlertmanagerMissing
        exp_alerts: []
`)
}

// What must not page, and a leaf that loses only its Alertmanager:
//   - the hub restarts (every series missing for eight minutes, its own up included): nothing;
//   - prod-us's agent was removed, and its last series (a dead cluster, a dead Alertmanager, a
//     critical alert) are still within the lookback: nothing, since no rule expects prod-us;
//   - prod-eu's Alertmanager goes down after the restart, the cluster still reachable: it pages,
//     and prod-eu's critical alert is relayed by the hub.
func TestHubMetricsRulesIgnoreRestartsAndRemovedClusters(t *testing.T) {
	cloud := testHubCloud()
	cloud.Spec.Agents = cloud.Spec.Agents[:1]
	checkHubRules(t, cloud, `rule_files: [rules.yaml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - {series: 'up{job="federate-support", cluster="support"}', values: '1x10 _x8 1x21'}
      - {series: 'ALERTS{alertname="Watchdog", alertstate="firing", severity="none", cluster="support"}', values: '1x10 _x8 1x21'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="support"}', values: '1x10 _x8 1x21'}
      - {series: 'up{job="federate-prod-eu", cluster="prod-eu"}', values: '1x10 _x8 1x21'}
      - {series: 'ALERTS{alertname="Watchdog", alertstate="firing", severity="none", cluster="prod-eu"}', values: '1x10 _x8 1x21'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="prod-eu"}', values: '1x10 _x8 0x21'}
      - {series: 'ALERTS{alertname="OramaStackComponentDown", alertstate="firing", severity="critical", namespace="v1", component="writer", cluster="prod-eu"}', values: '1x10 _x8 1x21'}
      - {series: 'up{job="federate-prod-us", cluster="prod-us"}', values: '0x40'}
      - {series: 'up{job="kube-prometheus-stack-alertmanager", cluster="prod-us"}', values: '0x40'}
      - {series: 'ALERTS{alertname="OramaStackComponentDown", alertstate="firing", severity="critical", namespace="v1", component="writer", cluster="prod-us"}', values: '1x40'}
    alert_rule_test:
      - {eval_time: 16m, alertname: ObservabilityClusterMetricsMissing, exp_alerts: []}
      - {eval_time: 16m, alertname: ObservabilityClusterAlertingDown, exp_alerts: []}
      - {eval_time: 35m, alertname: ObservabilityClusterMetricsMissing, exp_alerts: []}
      - {eval_time: 35m, alertname: ObservabilityClusterAlertingDown, exp_alerts: []}
      - {eval_time: 35m, alertname: ObservabilityClusterUnreachable, exp_alerts: []}
      - {eval_time: 35m, alertname: ObservabilityClusterAlertmanagerMissing, exp_alerts: []}
      - eval_time: 35m
        alertname: ObservabilityClusterAlertmanagerDown
        exp_alerts:
          - exp_labels: {severity: critical, job: kube-prometheus-stack-alertmanager, cluster: prod-eu}
            exp_annotations: {summary: "Cluster prod-eu's Alertmanager is down: its own alerts are not delivered."}
      - eval_time: 35m
        alertname: ObservabilityRelayedCriticalAlert
        exp_alerts:
          - exp_labels: {severity: critical, relayed_alert: OramaStackComponentDown, namespace: v1, component: writer, cluster: prod-eu}
            exp_annotations: {summary: "Cluster prod-eu: OramaStackComponentDown is firing in v1, and that cluster's Alertmanager cannot deliver it."}
`)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// runPrometheus starts Prometheus with config, its data and configuration in dir, and returns its
// address and a stop function. Starting it again in the same dir is a restart that keeps its data.
func runPrometheus(t *testing.T, binary, dir string, config map[string]any, rules []byte) (string, func()) {
	t.Helper()
	if rules != nil {
		if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), rules, 0o600); err != nil {
			t.Fatal(err)
		}
		config["rule_files"] = []string{filepath.Join(dir, "rules.yaml")}
	}
	data, err := sigsyaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prometheus.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	address := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cmd := exec.Command(binary, "--config.file="+filepath.Join(dir, "prometheus.yml"), "--storage.tsdb.path="+filepath.Join(dir, "data"),
		"--web.listen-address="+address, "--web.enable-lifecycle", "--log.level=warn")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	t.Cleanup(stop)
	return address, stop
}

// query is an instant query's samples, label sets with values.
func query(t *testing.T, address, expr string) []map[string]string {
	t.Helper()
	resp, err := http.Get("http://" + address + "/api/v1/query?" + url.Values{"query": {expr}}.Encode())
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var answer struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil
	}
	var out []map[string]string
	for _, sample := range answer.Data.Result {
		sample.Metric["__value__"] = fmt.Sprint(sample.Value[1])
		out = append(out, sample.Metric)
	}
	return out
}

// reloadPrometheus rewrites the configuration of the Prometheus in dir and reloads it, as
// prometheus-operator's config reloader does when the hub's Secret changes.
func reloadPrometheus(t *testing.T, address, dir string, config map[string]any) {
	t.Helper()
	data, err := sigsyaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prometheus.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+address+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload answered %s", resp.Status)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// The hub's rendered federation jobs, run in a real hub Prometheus against two real leaf
// Prometheus servers whose stacks share a namespace and a name:
//   - every federated series arrives with its cluster, and the two clusters' series stay apart;
//   - a leaf cannot claim another cluster's name, nor forge the hub's own reachability series;
//   - series outside the match list do not arrive;
//   - a lost leaf turns its job's up to 0 on the hub (ObservabilityClusterUnreachable's signal),
//     which is what the operator's status reads (queryFederationUp);
//   - a restarted hub keeps what it had and resumes;
//   - a removed cluster is no longer scraped once the configuration reloads.
func TestHubFederatesEachLeafLive(t *testing.T) {
	prometheus, _ := prometheusBinaries(t)
	exporter := func(ready string, spoof bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "orama_oramacloudv1_component_ready{namespace=\"v1\",name=\"v1\",component=\"writer\"} %s\n", ready)
			fmt.Fprintln(w, "kube_pod_status_ready{namespace=\"v1\",pod=\"v1-writer-0\",condition=\"true\"} 1")
			fmt.Fprintln(w, "not_federated_total 7")
			if spoof {
				// A series that claims to come from the other cluster.
				fmt.Fprintln(w, "orama_oramacloudv1_component_ready{namespace=\"v1\",name=\"v1\",component=\"spoof\",cluster=\"prod-eu\"} 1")
			}
		}))
	}
	leaf := func(exporter *httptest.Server, rules string) (string, func()) {
		target := strings.TrimPrefix(exporter.URL, "http://")
		return runPrometheus(t, prometheus, t.TempDir(), map[string]any{
			"global":         map[string]any{"scrape_interval": "1s", "evaluation_interval": "1s"},
			"scrape_configs": []any{map[string]any{"job_name": "kube-state-metrics", "static_configs": []any{map[string]any{"targets": []string{target}}}}},
		}, []byte("groups:\n- name: leaf\n  rules:\n  - alert: Watchdog\n    expr: vector(1)\n    labels: {severity: none}\n"+rules))
	}
	euExporter, usExporter := exporter("1", false), exporter("0", true)
	defer euExporter.Close()
	defer usExporter.Close()
	euAddress, _ := leaf(euExporter, "")
	// prod-us also forges the hub's reachability series for prod-eu.
	usAddress, stopUS := leaf(usExporter, "  - record: up\n    expr: vector(1)\n    labels: {job: federate-prod-eu}\n")

	hubConfig := func(cloud *oramav1alpha1.PixieCloud) map[string]any {
		scrape, err := hubScrapeConfig(cloud)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []map[string]any
		if err := sigsyaml.Unmarshal(scrape, &jobs); err != nil {
			t.Fatal(err)
		}
		// Point each rendered job at a local server in place of its peer import Service, and
		// scrape fast; everything else (path, match[], honor_labels, the cluster label and its
		// relabelling) is as rendered.
		addresses := map[string]string{"federate-prod-eu": euAddress, "federate-prod-us": usAddress, "federate-support": euAddress}
		for _, job := range jobs {
			name := job["job_name"].(string)
			configs := job["static_configs"].([]any)
			configs[0].(map[string]any)["targets"] = []string{addresses[name]}
			job["scrape_interval"], job["scrape_timeout"] = "1s", "1s"
		}
		return map[string]any{"global": map[string]any{"scrape_interval": "1s"}, "scrape_configs": jobs}
	}
	cloud := testHubCloud()
	cloud.Spec.Agents = cloud.Spec.Agents[:2]
	hubDir := t.TempDir()
	hub, stopHub := runPrometheus(t, prometheus, hubDir, hubConfig(cloud), nil)
	federationUp := func() map[string]bool {
		up, err := queryFederationUp(context.Background(), http.DefaultClient, "http://"+hub)
		if err != nil {
			return nil
		}
		return up
	}

	eventually(t, "both clusters' component status on the hub", func() bool {
		return len(query(t, hub, `orama_oramacloudv1_component_ready{component="writer", cluster=~"prod-eu|prod-us"}`)) == 2
	})
	for _, sample := range query(t, hub, `orama_oramacloudv1_component_ready{component="writer", cluster=~"prod-eu|prod-us"}`) {
		want := map[string]string{"prod-eu": "1", "prod-us": "0"}[sample["cluster"]]
		if sample["__value__"] != want || sample["namespace"] != "v1" || sample["name"] != "v1" || sample["job"] != "kube-state-metrics" {
			t.Errorf("federated series %v: the two clusters' identical stacks are mixed or relabelled", sample)
		}
	}
	if spoofed := query(t, hub, `orama_oramacloudv1_component_ready{component="spoof"}`); len(spoofed) != 1 || spoofed[0]["cluster"] != "prod-us" {
		t.Errorf("a series prod-us labelled as prod-eu arrived as %v, not as prod-us's", spoofed)
	}
	if forged := query(t, hub, `up{job="federate-prod-eu"}`); len(forged) != 1 || forged[0]["cluster"] != "prod-eu" || forged[0]["instance"] != euAddress {
		t.Errorf("prod-eu's reachability on the hub is %v: a leaf forged it", forged)
	}
	eventually(t, "each leaf's Watchdog on the hub", func() bool {
		return len(query(t, hub, `ALERTS{alertname="Watchdog", cluster="prod-us"}`)) == 1
	})
	if got := query(t, hub, `not_federated_total`); len(got) != 0 {
		t.Errorf("a series outside the match list was federated: %v", got)
	}
	eventually(t, "the operator reading every cluster reachable", func() bool {
		up := federationUp()
		return up["federate-prod-eu"] && up["federate-prod-us"] && up["federate-support"]
	})

	stopUS()
	eventually(t, "the lost leaf's federate job at up=0", func() bool {
		up := query(t, hub, `up{job="federate-prod-us", cluster="prod-us"}`)
		return len(up) == 1 && up[0]["__value__"] == "0"
	})
	if up := federationUp(); !up["federate-prod-eu"] || up["federate-prod-us"] {
		t.Errorf("the operator reads %v with prod-us lost", up)
	}

	// A hub restart keeps the lost leaf's last metrics and resumes reading the others.
	stopHub()
	hub, _ = runPrometheus(t, prometheus, hubDir, hubConfig(cloud), nil)
	eventually(t, "the restarted hub serving what it kept", func() bool {
		return len(query(t, hub, `count_over_time(orama_oramacloudv1_component_ready{component="writer", cluster="prod-us"}[10m])`)) == 1
	})
	eventually(t, "the restarted hub reading prod-eu again", func() bool {
		up := query(t, hub, `up{job="federate-prod-eu"}`)
		return len(up) == 1 && up[0]["__value__"] == "1"
	})

	// prod-us's agent is removed: its job goes on reload. Prometheus writes no staleness marker for
	// a removed job, so its last up=0 stays queryable for the lookback; that is why every hub rule
	// names only the clusters it expects (TestHubMetricsRulesIgnoreRestartsAndRemovedClusters).
	eventually(t, "the restarted hub scraping lost prod-us", func() bool {
		return len(query(t, hub, `up{job="federate-prod-us"} == 0`)) == 1
	})
	cloud.Spec.Agents = cloud.Spec.Agents[:1]
	reloadPrometheus(t, hub, hubDir, hubConfig(cloud))
	scrapedJobs := func() map[string]bool {
		resp, err := http.Get("http://" + hub + "/api/v1/targets?state=active")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var answer struct {
			Data struct {
				ActiveTargets []struct {
					ScrapePool string `json:"scrapePool"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
			return nil
		}
		jobs := map[string]bool{}
		for _, target := range answer.Data.ActiveTargets {
			jobs[target.ScrapePool] = true
		}
		return jobs
	}
	eventually(t, "the removed cluster no longer scraped", func() bool {
		jobs := scrapedJobs()
		return !jobs["federate-prod-us"] && jobs["federate-prod-eu"] && len(query(t, hub, `up{job="federate-prod-eu"} == 1`)) == 1
	})
}

// The hub's dashboards read the hub Prometheus and name the cluster in every cross-cluster legend.
func TestHubDashboardsReadTheHubAndNameTheCluster(t *testing.T) {
	cloud := testHubCloud()
	dashboards, err := hubDashboards(cloud)
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(dashboards.Object, "data")
	for _, cluster := range hubFederatedClusters(cloud) {
		if _, ok := data["cluster-"+cluster.Name+".json"]; !ok {
			t.Errorf("no dashboard for cluster %s", cluster.Name)
		}
	}
	for file, raw := range data {
		if file == "project.json" {
			continue
		}
		var dashboard struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Datasources map[string]struct {
					Plugin struct {
						Spec struct {
							Proxy struct {
								Spec struct{ URL string } `json:"spec"`
							} `json:"proxy"`
						} `json:"spec"`
					} `json:"plugin"`
				} `json:"datasources"`
				Panels map[string]struct {
					Spec struct {
						Queries []struct {
							Spec struct {
								Plugin struct {
									Spec struct {
										Query            string `json:"query"`
										SeriesNameFormat string `json:"seriesNameFormat"`
									} `json:"spec"`
								} `json:"plugin"`
							} `json:"spec"`
						} `json:"queries"`
					} `json:"spec"`
				} `json:"panels"`
			} `json:"spec"`
		}
		if err := json.Unmarshal([]byte(raw), &dashboard); err != nil {
			t.Fatal(err)
		}
		if url := dashboard.Spec.Datasources["prometheus"].Plugin.Spec.Proxy.Spec.URL; url != "http://prometheus-operated.plc.svc:9090" {
			t.Errorf("%s reads %s, not the hub Prometheus", file, url)
		}
		perCluster := strings.HasPrefix(dashboard.Metadata.Name, "cluster-")
		for _, panel := range dashboard.Spec.Panels {
			q := panel.Spec.Queries[0].Spec.Plugin.Spec
			if perCluster && !strings.Contains(q.Query, `cluster="`) && !strings.Contains(q.Query, `job="federate-`) {
				t.Errorf("%s: %s is not filtered to its cluster", file, q.Query)
			}
			if !perCluster && !strings.Contains(q.SeriesNameFormat, "{{cluster}}") {
				t.Errorf("%s: %s does not name the cluster", file, q.Query)
			}
		}
	}
}

// The leaf's federation proxy answers only GET /federate, from the platform Prometheus.
func TestMetricsFederationProxyIsReadOnly(t *testing.T) {
	objects, err := metricsFederationObjects()
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	for _, object := range objects {
		switch o := object.(type) {
		case *corev1.ConfigMap:
			if err := json.Unmarshal([]byte(o.Data["envoy.json"]), &config); err != nil {
				t.Fatal(err)
			}
		case *appsv1.Deployment:
			if image := o.Spec.Template.Spec.Containers[0].Image; image != platformEnvoyImage {
				t.Errorf("the proxy runs %s, not the platform's Envoy", image)
			}
		}
	}
	if config == nil {
		t.Fatal("no Envoy configuration rendered")
	}
	listener := config["static_resources"].(map[string]any)["listeners"].([]any)[0].(map[string]any)
	manager := listener["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	routes := manager["route_config"].(map[string]any)["virtual_hosts"].([]any)[0].(map[string]any)["routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("routes %v", routes)
	}
	first := routes[0].(map[string]any)
	match := first["match"].(map[string]any)
	if match["path"] != "/federate" || match["headers"].([]any)[0].(map[string]any)["string_match"].(map[string]any)["exact"] != "GET" ||
		first["route"].(map[string]any)["cluster"] != "prometheus" {
		t.Errorf("the first route is not GET /federate to Prometheus: %v", first)
	}
	if routes[1].(map[string]any)["direct_response"].(map[string]any)["status"] != float64(404) {
		t.Errorf("anything else is not refused: %v", routes[1])
	}
	cluster := config["static_resources"].(map[string]any)["clusters"].([]any)[0].(map[string]any)
	address := cluster["load_assignment"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["lb_endpoints"].([]any)[0].(map[string]any)["endpoint"].(map[string]any)["address"].(map[string]any)["socket_address"].(map[string]any)
	if address["address"] != "kube-prometheus-stack-prometheus.monitoring.svc.cluster.local" || address["port_value"] != float64(9090) {
		t.Errorf("the proxy reads %v, not the platform Prometheus", address)
	}
}

// With the platform layer installed in part (rules served, Prometheus not), nothing of the hub's
// metrics is written: not the Secret, not the rules.
func TestMonitoringObjectsAreAllOrNothing(t *testing.T) {
	scheme := newPixieScheme(t)
	cloud := testHubCloud()
	cloud.Name = pixieConcreteName
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{prometheusRuleGVK, {Version: "v1", Kind: "Secret"}, {Version: "v1", Kind: "ConfigMap"}} {
		mapper.Add(gvk, apimeta.RESTScopeNamespace)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cloud).WithRESTMapper(mapper).Build()
	objects, err := hubMetricsObjects(cloud)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMonitoringObjects(context.Background(), kube, scheme, cloud, objects); !errors.Is(err, errMonitoringCRDsMissing) {
		t.Fatalf("want errMonitoringCRDsMissing, got %v", err)
	}
	var secrets corev1.SecretList
	if err := kube.List(context.Background(), &secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("a partial platform layer got %d objects written", len(secrets.Items))
	}
	rule := &unstructured.Unstructured{}
	rule.SetGroupVersionKind(prometheusRuleGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "plc", Name: "hub-cluster-alerts"}, rule); !apierrors.IsNotFound(err) {
		t.Fatalf("the hub's rules were written without its Prometheus: %v", err)
	}
}

// The hub Prometheus runs the platform mirror's image, keeps metricsRetention on its own volume,
// evaluates only hub-scoped rules in plc, and the platform mirror is the inventoried one.
func TestHubPrometheusIsPinnedAndScoped(t *testing.T) {
	cloud := testHubCloud()
	cloud.Spec.MetricsRetention = "90d"
	objects, err := hubMetricsObjects(cloud)
	if err != nil {
		t.Fatal(err)
	}
	prometheus := hubObject(t, objects, "Prometheus")
	spec, _, _ := unstructured.NestedMap(prometheus.Object, "spec")
	if spec["image"] != platformPrometheusImage || spec["retention"] != "90d" || spec["retentionSize"] != "96GB" {
		t.Errorf("hub Prometheus spec: image %v retention %v size %v", spec["image"], spec["retention"], spec["retentionSize"])
	}
	// The pods carry the label the hub's sandbox allowances select.
	podLabels, _, _ := unstructured.NestedStringMap(prometheus.Object, "spec", "podMetadata", "labels")
	if podLabels[pixieComponentLabel] != hubPrometheusComponent {
		t.Errorf("hub Prometheus pods are labelled %v; the sandbox selects %s=%s", podLabels, pixieComponentLabel, hubPrometheusComponent)
	}
	selector, _, _ := unstructured.NestedStringMap(prometheus.Object, "spec", "ruleSelector", "matchLabels")
	if selector[metricsScopeLabel] != metricsScopeHub {
		t.Errorf("the hub Prometheus selects rules %v", selector)
	}
	rules := hubObject(t, objects, "PrometheusRule")
	if rules.GetLabels()[metricsScopeLabel] != metricsScopeHub {
		t.Error("the hub's rules are not hub-scoped, so the platform Prometheus would evaluate them too")
	}
	inventory, err := os.ReadFile("../../hack/observability-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{platformPrometheusImage, platformEnvoyImage} {
		if !strings.Contains(string(inventory), "mirror: "+image) {
			t.Errorf("%s is not an inventoried mirror", image)
		}
	}
}
