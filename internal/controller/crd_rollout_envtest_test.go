// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const rolloutTestCRD = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.rollout.test
spec:
  group: rollout.test
  names: {kind: Widget, listKind: WidgetList, plural: widgets, singular: widget}
  scope: Cluster
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
%s
`

// The previous release reads spec.enabled; the next one drops it and reads spec.clerk.
var (
	previousWidgetCRD = []byte(fmt.Sprintf(rolloutTestCRD, "              host: {type: string}\n              enabled: {type: boolean}"))
	nextWidgetCRD     = []byte(fmt.Sprintf(rolloutTestCRD, "              host: {type: string}\n              clerk: {type: string}"))
)

func widgetSpec(t *testing.T, c client.Client, name string) map[string]any {
	t.Helper()
	w := &unstructured.Unstructured{}
	w.SetAPIVersion("rollout.test/v1")
	w.SetKind("Widget")
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, w); err != nil {
		t.Fatal(err)
	}
	spec, _ := w.Object["spec"].(map[string]any)
	return spec
}

func eventuallySpec(t *testing.T, c client.Client, name string, want map[string]any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := widgetSpec(t, c, name)
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("widget %s spec = %v, want %v", name, got, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func createWidget(t *testing.T, c client.Client, name string, spec map[string]any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rollout.test/v1", "kind": "Widget",
			"metadata": map[string]any{"name": name}, "spec": spec,
		}}
		err := c.Create(context.Background(), w)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond) // the CRD is not established yet
	}
}

// TestRemovingACRDFieldIsInvisibleToTheOperatorStillRunning is operator#543 against a real API
// server: the operator's CRDs are applied widened before the roll, so a stored object keeps the
// field the old operator reads, and the new operator's field is accepted at once; only pruneCRDs,
// after the roll, drops the old field.
func TestRemovingACRDFieldIsInvisibleToTheOperatorStillRunning(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the CRD rollout envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The cluster as the previous release left it.
	var previous map[string]any
	if err := yaml.Unmarshal(previousWidgetCRD, &previous); err != nil {
		t.Fatal(err)
	}
	if err := applyCRD(ctx, c, previous); err != nil {
		t.Fatal(err)
	}
	createWidget(t, c, "prod", map[string]any{"host": "lake.example", "enabled": true})

	next := &chart.Chart{
		Metadata: &chart.Metadata{Name: "orama-operator", Version: "0.0.0-test"},
		Files:    []*chart.File{{Name: "crds/widgets.yaml", Data: nextWidgetCRD}},
	}

	// Before the roll: the widened CRDs.
	if err := applyCRDs(ctx, c, next, true); err != nil {
		t.Fatal(err)
	}
	// The old operator still reads spec.enabled ...
	eventuallySpec(t, c, "prod", map[string]any{"host": "lake.example", "enabled": true})
	// ... and the new one's field is already stored.
	createWidget(t, c, "staging", map[string]any{"host": "staging.example", "clerk": "lake-staging"})
	eventuallySpec(t, c, "staging", map[string]any{"host": "staging.example", "clerk": "lake-staging"})

	// After the roll: exactly the chart's CRDs, which prune spec.enabled.
	pruned, err := pruneCRDs(ctx, c, next)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pruned, []string{"widgets.rollout.test"}) {
		t.Fatalf("pruned %v, want the widget CRD", pruned)
	}
	eventuallySpec(t, c, "prod", map[string]any{"host": "lake.example"})
	eventuallySpec(t, c, "staging", map[string]any{"host": "staging.example", "clerk": "lake-staging"})

	// A steady cluster is not written again.
	if pruned, err := pruneCRDs(ctx, c, next); err != nil || len(pruned) != 0 {
		t.Fatalf("second prune wrote %v (%v); a CRD equal to the chart's must not be written", pruned, err)
	}
	// And widening a cluster that already equals the chart changes nothing either.
	if err := applyCRDs(ctx, c, next, true); err != nil {
		t.Fatal(err)
	}
	if pruned, err := pruneCRDs(ctx, c, next); err != nil || len(pruned) != 0 {
		t.Fatalf("widening an up-to-date CRD left something to prune: %v (%v)", pruned, err)
	}
}

func TestTheOperatorCRDsArePrunedOnlyOnceTheNewVersionRunsAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state leafOperatorState
		want  bool
	}{
		{"rolled out", leafOperatorState{Image: "docker.io/oramasearch/orama-operator:dev-new", Replicas: 1, ReadyReplicas: 1}, true},
		{"an old pod still runs", leafOperatorState{Image: "docker.io/oramasearch/orama-operator:dev-new", Replicas: 1, ReadyReplicas: 1, oldPods: 1}, false},
		{"not ready", leafOperatorState{Image: "docker.io/oramasearch/orama-operator:dev-new", Replicas: 1}, false},
		{"another version", leafOperatorState{Image: "docker.io/oramasearch/orama-operator:dev-old", Replicas: 1, ReadyReplicas: 1}, false},
		{"no Deployment", leafOperatorState{}, false},
	} {
		if got := tc.state.rolledOut("dev-new"); got != tc.want {
			t.Errorf("%s: rolledOut = %t, want %t", tc.name, got, tc.want)
		}
	}
}
