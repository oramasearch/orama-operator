// SPDX-License-Identifier: MIT

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"
)

// monitoringObject is the first object of kind among rendered monitoring objects.
func monitoringObject(t *testing.T, objects []*unstructured.Unstructured, kind string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind {
			return object
		}
	}
	t.Fatalf("no %s among the monitoring objects", kind)
	return nil
}

// prometheusBinaries are the prometheus and promtool of the release tarball in
// PROMETHEUS_BINARY_DIR (docs/development.md); without it the test is skipped.
func prometheusBinaries(t *testing.T) (prometheus, promtool string) {
	t.Helper()
	dir := os.Getenv("PROMETHEUS_BINARY_DIR")
	if dir == "" {
		t.Skip("PROMETHEUS_BINARY_DIR is required to run the rules in real Prometheus")
	}
	return filepath.Join(dir, "prometheus"), filepath.Join(dir, "promtool")
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
