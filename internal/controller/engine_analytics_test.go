/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const analyticsKeyPlaceholderLine = `    api_key: "__MASTER_API_KEY__"`

// TestEngineConfigEnablesReaderAnalytics covers #55: the engine config template carries a
// reader_side.analytics block whose api_key is the master-key placeholder (never a value), and
// the render-config init container fills it from the engine Secret's MASTER_API_KEY.
func TestEngineConfigEnablesReaderAnalytics(t *testing.T) {
	oc := newOCV1(nil)
	tmpl := oramacoreConfigYAML(oc)

	_, reader, ok := strings.Cut(tmpl, "\nreader_side:\n")
	if !ok {
		t.Fatal("config has no reader_side block")
	}
	reader, _, _ = strings.Cut(reader, "\nai_server:\n")
	_, analytics, ok := strings.Cut(reader, "\n  analytics:\n")
	if !ok {
		t.Fatalf("reader_side has no analytics block:\n%s", reader)
	}
	if !strings.HasPrefix(analytics, analyticsKeyPlaceholderLine+"\n") {
		t.Errorf("reader_side.analytics.api_key must be the master-key placeholder, got:\n%s", analytics)
	}
	// OramaCore's OramaCoreAnalyticConfig has no serde default for this field.
	if !strings.Contains(analytics, "    metadata_from_headers:\n") {
		t.Errorf("reader_side.analytics needs metadata_from_headers, got:\n%s", analytics)
	}

	// The placeholder must come from a key the engine Secret actually carries.
	if _, ok := componentSecretData("engine")["MASTER_API_KEY"]; !ok {
		t.Fatal("engine Secret seeds no MASTER_API_KEY")
	}
	render := renderConfigInit(oc)
	engineSecret := componentSecretName(oc, "engine")
	if !slices.ContainsFunc(render.EnvFrom, func(e corev1.EnvFromSource) bool {
		return e.SecretRef != nil && e.SecretRef.Name == engineSecret
	}) {
		t.Errorf("render-config does not envFrom the engine Secret %s: %+v", engineSecret, render.EnvFrom)
	}

	// Run the init container's real command against the template, with its mount paths
	// pointed at a temp dir, and check the rendered file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-config.yaml"), []byte(tmpl), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/tmpl/", dir+"/in-", "/etc/oramacore/", dir+"/out-").Replace(render.Command[2])
	cmd := exec.Command(render.Command[0], render.Command[1], script)
	cmd.Env = append(os.Environ(), "MASTER_API_KEY=test-master-key", "GROQ_API_KEY=test-groq-key")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render command: %v\n%s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "out-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	if strings.Contains(got, "__MASTER_API_KEY__") {
		t.Error("rendered config still holds the __MASTER_API_KEY__ placeholder")
	}
	if !strings.Contains(got, "\n  analytics:\n    api_key: \"test-master-key\"\n") {
		t.Errorf("rendered reader_side.analytics.api_key is not the Secret's MASTER_API_KEY:\n%s", got)
	}
}

// TestAnalyticsProcessorPollsReaderWithEngineKey covers the other half of #55: the processor
// drains the reader Service (where searches land) and authenticates with MASTER_API_KEY from
// its own Secret, the same key the engine renders as reader_side.analytics.api_key.
func TestAnalyticsProcessorPollsReaderWithEngineKey(t *testing.T) {
	oc := newOCV1(nil)
	cfg := analyticsProcessorConfigYAML(oc)

	for _, url := range []string{
		"api_url: http://" + oc.Name + "-reader/v1/analytics\n    api_key: __MASTER_API_KEY__\n",
		"api_url: http://" + oc.Name + "-writer:8080/v1/analytics\n    api_key: __MASTER_API_KEY__\n",
	} {
		if !strings.Contains(cfg, url) {
			t.Errorf("processor config lacks oramacores entry %q:\n%s", url, cfg)
		}
	}
	if _, ok := componentSecretData("analytics-processor")["MASTER_API_KEY"]; !ok {
		t.Error("analytics-processor Secret seeds no MASTER_API_KEY")
	}
}
