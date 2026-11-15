// SPDX-License-Identifier: MIT

package crdrollout

import (
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

func crd(t *testing.T, doc string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The Lake CRD before operator#542: spec.apps.enabled, spec.apps.image, a required spec.host.
const before = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: lakes.orama.cloud}
spec:
  group: orama.cloud
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            required: [host]
            properties:
              host: {type: string}
              mode: {type: string, enum: [a, b]}
              apps:
                type: object
                properties:
                  enabled: {type: boolean}
                  image: {type: string}
              list:
                type: array
                items:
                  type: object
                  properties:
                    old: {type: string}
`

// After: apps.enabled is gone, spec.clerk and a required spec.name are new, enum value b gone.
const after = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: lakes.orama.cloud}
spec:
  group: orama.cloud
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            required: [host, name]
            properties:
              host: {type: string}
              name: {type: string}
              mode: {type: string, enum: [a]}
              clerk: {type: object}
              apps:
                type: object
                properties:
                  image: {type: string}
              list:
                type: array
                items:
                  type: object
                  properties:
                    new: {type: string}
`

func spec(t *testing.T, c map[string]any) map[string]any {
	t.Helper()
	root := schemaOf(versions(c)[0])
	return root["properties"].(map[string]any)["spec"].(map[string]any)
}

func TestUnionKeepsEveryOldFieldAndAddsEveryNewOne(t *testing.T) {
	union := Union(crd(t, after), crd(t, before))
	s := spec(t, union)
	props := s["properties"].(map[string]any)
	for _, name := range []string{"host", "name", "mode", "clerk", "apps", "list"} {
		if _, ok := props[name]; !ok {
			t.Errorf("spec.%s is missing from the union", name)
		}
	}
	apps := props["apps"].(map[string]any)["properties"].(map[string]any)
	if _, ok := apps["enabled"]; !ok {
		t.Error("spec.apps.enabled, which the old operator reads, is missing from the union")
	}
	items := props["list"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if items["old"] == nil || items["new"] == nil {
		t.Errorf("array item properties were not merged: %v", items)
	}
	// A field only the new CRD requires is optional in the union, so the old operator's writes,
	// which do not set it, are not refused.
	if got := s["required"]; !reflect.DeepEqual(got, []any{"host"}) {
		t.Errorf("required = %v, want [host]", got)
	}
	if got := props["mode"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("enum = %v, want [a b]", got)
	}
	if !Wider(crd(t, after), crd(t, before)) {
		t.Error("Wider says the old CRD declares nothing the new one lacks")
	}
}

func TestUnionWithNothingOnTheClusterIsTheChart(t *testing.T) {
	if got, want := Union(crd(t, after), nil), normalize(crd(t, after)); !reflect.DeepEqual(got, want) {
		t.Errorf("union with a missing CRD differs from the chart's: %v", got)
	}
	if Wider(crd(t, after), nil) {
		t.Error("a CRD the cluster lacks is reported as wider")
	}
}

func TestAPurelyAdditiveChangeIsNotWider(t *testing.T) {
	// The cluster has the old CRD, the chart the old one plus a field: nothing to prune.
	next := crd(t, before)
	spec(t, next)["properties"].(map[string]any)["clerk"] = map[string]any{"type": "object"}
	if Wider(next, crd(t, before)) {
		t.Error("an additive change is reported as needing a prune")
	}
	// The cluster as the API server returns it (numbers as int64) equals the chart's YAML.
	same := crd(t, before)
	same["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["maxProperties"] = int64(3)
	chart := crd(t, before)
	chart["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["maxProperties"] = float64(3)
	if Wider(chart, same) {
		t.Error("number types alone make a CRD look wider")
	}
}

func TestAVersionOnlyTheClusterHasIsKeptButNeverStored(t *testing.T) {
	old := crd(t, before)
	next := crd(t, after)
	v := versions(next)[0]
	v["name"] = "v1"
	union := Union(next, old)
	got := versions(union)
	if len(got) != 2 || got[0]["name"] != "v1" || got[1]["name"] != "v1alpha1" {
		t.Fatalf("versions = %v", got)
	}
	if got[0]["storage"] != true || got[1]["storage"] != false || got[1]["served"] != true {
		t.Errorf("storage/served wrong: %v", got)
	}
}

func TestChangesListsRemovalsAndTypeChangesOnly(t *testing.T) {
	old := crd(t, before)
	next := crd(t, after)
	spec(t, next)["properties"].(map[string]any)["host"] = map[string]any{"type": "object"}
	var keys []string
	for _, c := range Changes(old, next) {
		keys = append(keys, c.Key())
	}
	want := []string{
		"lakes.orama.cloud v1alpha1 spec.apps.enabled removed",
		"lakes.orama.cloud v1alpha1 spec.host type changed from string to object",
		"lakes.orama.cloud v1alpha1 spec.list[].old removed",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("changes = %q, want %q", keys, want)
	}
	if got := Changes(old, old); len(got) != 0 {
		t.Errorf("a CRD compared with itself has changes: %v", got)
	}
}

func TestParseRefusesAnythingButACRD(t *testing.T) {
	if _, err := Parse("x", []byte("---\napiVersion: v1\nkind: Secret\n")); err == nil {
		t.Error("a Secret was accepted as a CRD")
	}
	got, err := Parse("x", []byte("---\n"+before+"\n---\n"+after))
	if err != nil || len(got) != 2 {
		t.Errorf("Parse = %d, %v", len(got), err)
	}
}
