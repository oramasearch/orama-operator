/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// The chart runs the manager only by its image's digest (orama-operator#808): a tag alone, which a
// publisher can move, is refused when the chart is rendered.
func TestTheChartRunsTheOperatorImageOnlyByItsDigest(t *testing.T) {
	chart, err := loader.Load(filepath.Join("..", "chart"))
	if err != nil {
		t.Fatal(err)
	}
	render := func(tag string) (string, error) {
		overrides := map[string]any{}
		if tag != "" {
			overrides["operator"] = map[string]any{"image": map[string]any{"tag": tag}}
		}
		values, err := chartutil.ToRenderValues(chart, overrides,
			chartutil.ReleaseOptions{Name: "orama-operator", Namespace: "orama-system"}, chartutil.DefaultCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := engine.Render(chart, values)
		if err != nil {
			return "", err
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal([]byte(rendered["orama-operator/templates/operator-deployment.yaml"]), &deployment); err != nil {
			t.Fatal(err)
		}
		return deployment.Spec.Template.Spec.Containers[0].Image, nil
	}

	if image, err := render(""); err != nil || !strings.Contains(image, "@sha256:") {
		t.Fatalf("the default image %q (%v) is not pinned by its digest", image, err)
	}
	pinned := "dev-83242f9@sha256:" + strings.Repeat("a", 64)
	if image, err := render(pinned); err != nil || image != "oramasearch/orama-operator:"+pinned {
		t.Fatalf("a pinned tag renders %q (%v)", image, err)
	}
	for _, tag := range []string{"v0.7.3", "v0.7.3@sha256:abc", "latest"} {
		if _, err := render(tag); err == nil || !strings.Contains(err.Error(), "@sha256:<64 hex digits>") {
			t.Errorf("tag %q without a digest was rendered: %v", tag, err)
		}
	}
}
