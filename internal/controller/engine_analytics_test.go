/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// prod1AnalyticsHeaders is reader_side.analytics.metadata_from_headers on Prod 1's readers
// (oramacore-aws prod-config-yaml/reader-*-config.yaml).
var prod1AnalyticsHeaders = []any{
	map[string]any{"header": "user-agent", "metadata_key": "ua"},
	map[string]any{"header": "CF-Connecting-IP", "metadata_key": "ip"},
	map[string]any{"header": "CF-IPCountry", "metadata_key": "country"},
	map[string]any{"header": "continent", "metadata_key": "continent"},
	map[string]any{"header": "colo", "metadata_key": "colo"},
	map[string]any{"header": "latitude", "metadata_key": "latitude"},
	map[string]any{"header": "longitude", "metadata_key": "longitude"},
}

// readsAnalyticsKey reports whether a container's env, envFrom or command names the analytics key.
func readsAnalyticsKey(c corev1.Container) bool {
	b, _ := json.Marshal(struct {
		Env     []corev1.EnvVar
		EnvFrom []corev1.EnvFromSource
		Command []string
	}{c.Env, c.EnvFrom, c.Command})
	return strings.Contains(string(b), "v1-analytics-api-key") || strings.Contains(string(b), analyticsKeyEnvName)
}

// runRender runs a render-config container's script with its /tmpl/ input and output directory
// pointed at a temp dir, and returns the rendered file.
func runRender(t *testing.T, c corev1.Container, tmplName, tmpl, outDir string, env map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"tmpl", "out"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tmpl", tmplName), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/tmpl/", dir+"/tmpl/", outDir, dir+"/out/").Replace(c.Command[2])
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render-config: %v: %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("render-config executed a value")
	}
	b, err := os.ReadFile(filepath.Join(dir, "out", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Only the reader-only config holds reader_side.analytics, with the analytics key placeholder and
// Prod 1's seven header mappings; only the readers' render-config reads <cr>-analytics-api-key.
func TestReaderConfigRendersAnalytics(t *testing.T) {
	h := newEngineHarness(t)
	h.reconcile(engineOC(withSideImages))
	cm := h.engineConfig()

	rs, _ := parseYAML(t, cm.Data[readerConfigKey])["reader_side"].(map[string]any)
	analytics, ok := rs["analytics"].(map[string]any)
	if !ok {
		t.Fatalf("reader.yaml has no reader_side.analytics:\n%s", cm.Data[readerConfigKey])
	}
	if got := analytics["api_key"]; got != "__ANALYTICS_API_KEY__" {
		t.Errorf("reader_side.analytics.api_key = %v, want the analytics key placeholder", got)
	}
	if got := analytics["metadata_from_headers"]; !reflect.DeepEqual(got, prod1AnalyticsHeaders) {
		t.Errorf("metadata_from_headers = %v, want Prod 1's %v", got, prod1AnalyticsHeaders)
	}
	if strings.Contains(cm.Data[readerConfigKey], "__MASTER_API_KEY__") {
		t.Error("reader.yaml holds the master key placeholder")
	}

	if _, ok := parseYAML(t, cm.Data[writerConfigKey])["reader_side"]; ok {
		t.Error("writer.yaml has a reader_side")
	}
	init := renderInit(t, h.reader().Spec.Template.Spec)
	if !slices.ContainsFunc(init.Env, func(e corev1.EnvVar) bool {
		return reflect.DeepEqual(e, secretKeyEnv("ANALYTICS_API_KEY", "v1-analytics-api-key", "ANALYTICS_API_KEY"))
	}) {
		t.Errorf("reader render-config does not take ANALYTICS_API_KEY from v1-analytics-api-key: %+v", init.Env)
	}
	if readsAnalyticsKey(renderInit(t, h.writer().Spec.Template.Spec)) {
		t.Error("the writer's render-config reads the analytics key")
	}

	// The combined config has no analytics block, and neither side's render-config reads the key.
	h = newEngineHarness(t)
	h.reconcile(engineOC(nil))
	cm = h.engineConfig()
	if strings.Contains(cm.Data[combinedConfigKey], "analytics:") || strings.Contains(cm.Data[combinedConfigKey], "__ANALYTICS_API_KEY__") {
		t.Errorf("the combined config has an analytics block:\n%s", cm.Data[combinedConfigKey])
	}
	for side, spec := range h.podTemplates() {
		if readsAnalyticsKey(renderInit(t, spec.Spec)) {
			t.Errorf("combined %s render-config reads the analytics key", side)
		}
	}
}

// The readers' render-config fills reader_side.analytics.api_key with ANALYTICS_API_KEY, never the
// master key.
func TestReaderRenderFillsAnalyticsKey(t *testing.T) {
	h := newEngineHarness(t)
	h.reconcile(engineOC(withSideImages))
	env := map[string]string{"ANALYTICS_API_KEY": "test-analytics-key", "MASTER_API_KEY": "test-master-key",
		"BROKER_USERNAME": "orama", "BROKER_PASSWORD": "broker-pass"}
	for _, p := range llmProviders(engineOC(withSideImages)) {
		env[llmKeyEnv(p.Name)] = "llm-key"
	}
	out := runRender(t, renderInit(t, h.reader().Spec.Template.Spec), readerConfigKey, h.engineConfig().Data[readerConfigKey], "/etc/oramacore/", env)
	rs, _ := parseYAML(t, out)["reader_side"].(map[string]any)
	analytics, _ := rs["analytics"].(map[string]any)
	if got := analytics["api_key"]; got != "test-analytics-key" {
		t.Errorf("rendered reader_side.analytics.api_key = %v, want ANALYTICS_API_KEY", got)
	}
	if strings.Contains(out, "__ANALYTICS_API_KEY__") || strings.Contains(out, "test-master-key") {
		t.Error("the rendered reader config holds the placeholder or the master key")
	}
}

// The processor's render-config writes the analytics key into every api_key and keeps every byte
// of the ClickHouse password and the Postgres URI; the master key in its environment reaches no output.
func TestAnalyticsProcessorRendersAnalyticsKey(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	cm := &corev1.ConfigMap{}
	h.get("v1-analytics-processor-config", cm)
	env := map[string]string{"ANALYTICS_API_KEY": "test-analytics-key", "MASTER_API_KEY": "test-master-key",
		"CLICKHOUSE_PASSWORD": `c'h&#\$(touch pwned)`, "PG_URI": "postgresql://app:pg'pass@v1-controldb-rw:5432/oramacore"}
	init := containerIn(t, h.templates()["analytics-processor"].Spec, "render-config")
	out := runRender(t, init, "config.yaml", cm.Data["config.yaml"], "/etc/analytics-processor/", env)
	doc := parseYAML(t, out)
	for _, src := range doc["oramacores"].([]any) {
		if got := src.(map[string]any)["api_key"]; got != "test-analytics-key" {
			t.Errorf("oramacores api_key = %v, want ANALYTICS_API_KEY", got)
		}
	}
	if strings.Contains(out, "__ANALYTICS_API_KEY__") || strings.Contains(out, "test-master-key") {
		t.Error("the rendered processor config holds the placeholder or the master key")
	}
	if got := doc["clickhouse"].(map[string]any)["password"]; got != env["CLICKHOUSE_PASSWORD"] {
		t.Errorf("clickhouse.password = %q", got)
	}
	if got := doc["postgres"].(map[string]any)["url"]; got != env["PG_URI"] {
		t.Errorf("postgres.url = %q", got)
	}
}

// Neither processor container reads the master key or loads its component Secret whole, even when
// an older operator left MASTER_API_KEY in that Secret, with the default or an alternate reference.
// The render init takes each value by secretKeyRef, and the pod's hash covers those Secrets.
func TestAnalyticsProcessorHoldsNoMasterKey(t *testing.T) {
	for name, ref := range map[string]*oramav1alpha1.MasterAPIKeySecretRef{
		"default reference":   {Name: testMasterSecret},
		"alternate reference": {Name: "shared-master", Key: "token"},
	} {
		t.Run(name, func(t *testing.T) {
			old := secretOf("v1-analytics-processor-secret", map[string]string{"CLICKHOUSE_PASSWORD": "ch-pass", "MASTER_API_KEY": "sentinel-master-7777"})
			oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.MasterAPIKeySecretRef = ref })
			h := newEngineHarness(t, oc, engineSecret(), old, secretOf("shared-master", map[string]string{"token": "shared-master-8888"}))
			h.mustRun()
			tmpl := h.templates()["analytics-processor"]
			shared := []corev1.EnvFromSource{sharedConfigEnvFrom(oc)}
			for _, c := range append(slices.Clone(tmpl.Spec.InitContainers), tmpl.Spec.Containers...) {
				if !reflect.DeepEqual(c.EnvFrom, shared) {
					t.Errorf("%s envFrom = %+v, want the shared ConfigMap only", c.Name, c.EnvFrom)
				}
				for _, e := range c.Env {
					if e.Name == "MASTER_API_KEY" {
						t.Errorf("%s has a MASTER_API_KEY env entry", c.Name)
					}
				}
			}
			b, _ := json.Marshal(tmpl)
			for _, s := range []string{ref.Name, "sentinel-master-7777", "MASTER_API_KEY"} {
				if strings.Contains(string(b), s) {
					t.Errorf("the processor pod template names %q", s)
				}
			}
			for _, v := range tmpl.Spec.Volumes {
				if v.Secret != nil || v.Projected != nil {
					t.Errorf("volume %s mounts a Secret", v.Name)
				}
			}
			init := containerIn(t, tmpl.Spec, "render-config")
			want := []corev1.EnvVar{secretKeyEnv("ANALYTICS_API_KEY", "v1-analytics-api-key", "ANALYTICS_API_KEY"),
				secretKeyEnv("CLICKHOUSE_PASSWORD", "v1-analytics-processor-secret", "CLICKHOUSE_PASSWORD"),
				secretKeyEnv("PG_URI", "v1-controldb-app", "uri")}
			if !reflect.DeepEqual(init.Env, want) {
				t.Errorf("render-config env = %+v, want %+v", init.Env, want)
			}
			main := containerIn(t, tmpl.Spec, "analytics-processor")
			if !reflect.DeepEqual(main.Env, []corev1.EnvVar{{Name: "CONFIG_PATH", Value: "/etc/analytics-processor/config.yaml"}}) {
				t.Errorf("main container env = %+v", main.Env)
			}
			if got, want := tmpl.Annotations[credentialsHashKey], secretDataHash(h.secret("v1-analytics-api-key"), h.secret("v1-analytics-processor-secret")); got != want {
				t.Errorf("credentials hash = %q, want the analytics key and component Secrets' %q", got, want)
			}
		})
	}
}

// The control DB's app Secret is in the processor's hash once CNPG creates it, so a changed
// Postgres password rolls the processor.
func TestAnalyticsProcessorHashCoversPostgresPassword(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret(), secretOf("v1-controldb-app", map[string]string{"password": "pg-1"}))
	h.mustRun()
	before := h.templates()["analytics-processor"].Annotations[credentialsHashKey]
	h.setSecret("v1-controldb-app", "password", "pg-2")
	h.mustRun()
	if h.templates()["analytics-processor"].Annotations[credentialsHashKey] == before {
		t.Error("a changed Postgres password kept the processor's credentials hash")
	}
	if reqs := h.r.requestsForSecret(context.Background(), h.secret("v1-controldb-app")); len(reqs) != 1 || reqs[0].Name != "v1" {
		t.Errorf("a v1-controldb-app event mapped to %v, want v1", reqs)
	}
}
