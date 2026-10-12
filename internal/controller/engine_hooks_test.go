/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// withHooks sets spec.components.engine.hooks.
func withHooks(h oramav1alpha1.EngineHooksSpec) func(*oramav1alpha1.OramaCloudV1) {
	return func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Hooks = &h }
}

// hooksOf parses a config and returns the hooks block of one side section.
func hooksOf(t *testing.T, cfg, side string) map[string]any {
	t.Helper()
	sec, ok := parseEngineConfig(t, side, cfg)[side].(map[string]any)
	if !ok {
		t.Fatalf("config has no %s section", side)
	}
	hooks, ok := sec["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no hooks block", side)
	}
	return hooks
}

// deniedOf returns a hooks block's denied_domains.
func deniedOf(t *testing.T, hooks map[string]any) []string {
	t.Helper()
	var out []string
	for _, d := range hooks["denied_domains"].([]any) {
		out = append(out, d.(string))
	}
	return out
}

func TestHooksBlockIsTheSameOnWriterAndReader(t *testing.T) {
	combined := oramacoreConfigYAML(newOCV1(nil))
	sides := sideImagesOC(nil)
	pairs := map[string][2]map[string]any{
		"combined":  {hooksOf(t, combined, "writer_side"), hooksOf(t, combined, "reader_side")},
		"side-only": {hooksOf(t, engineSideFor(sides, "writer").template, "writer_side"), hooksOf(t, engineSideFor(sides, "reader").template, "reader_side")},
	}
	for name, p := range pairs {
		writer, reader := p[0], p[1]
		if !reflect.DeepEqual(writer, reader) {
			t.Errorf("%s: writer hooks %v differ from reader hooks %v", name, writer, reader)
		}
		if writer["evaluation_timeout"] != "200ms" || writer["execution_timeout"] != "1s" {
			t.Errorf("%s: timeouts = %v / %v, want Prod 1's 200ms / 1s", name, writer["evaluation_timeout"], writer["execution_timeout"])
		}
		if _, ok := writer["allowed_domains"]; ok {
			t.Errorf("%s: allowed_domains rendered; OramaCore refuses it with denied_domains", name)
		}
		denied := deniedOf(t, writer)
		want := append(slices.Clone(prod1HookDeniedDomains),
			"localhost", "*.svc", "*.svc.cluster.local", "*.cluster.local", "kubernetes*", "*.internal", "v1-*",
			"10.**", "127.**", "169.254.**")
		for _, d := range want {
			if !slices.Contains(denied, d) {
				t.Errorf("%s: denied_domains lacks %q", name, d)
			}
		}
	}
}

func TestHooksExtraDeniedDomainsAppendedOnce(t *testing.T) {
	extra := []string{"10.244.**", "203.0.113.*", "10.244.**", "metadata.example", "v1-*"}
	oc := newOCV1(withHooks(oramav1alpha1.EngineHooksSpec{ExtraDeniedDomains: extra}))
	denied := deniedOf(t, hooksOf(t, oramacoreConfigYAML(oc), "reader_side"))

	seen := map[string]bool{}
	for _, d := range denied {
		if seen[d] {
			t.Errorf("denied_domains lists %q twice", d)
		}
		seen[d] = true
	}
	for _, d := range extra {
		if !seen[d] {
			t.Errorf("denied_domains lacks extra entry %q", d)
		}
	}
	// The entries not already in the default list come last, in the order given.
	if got, want := denied[len(denied)-2:], []string{"10.244.**", "metadata.example"}; !slices.Equal(got, want) {
		t.Errorf("denied_domains ends with %v, want %v", got, want)
	}
}

func TestHooksTimeoutsFromSpec(t *testing.T) {
	oc := sideImagesOC(withHooks(oramav1alpha1.EngineHooksSpec{
		EvaluationTimeout: &metav1.Duration{Duration: 500 * time.Millisecond},
		ExecutionTimeout:  &metav1.Duration{Duration: 2 * time.Second},
	}))
	hooks := hooksOf(t, engineSideFor(oc, "writer").template, "writer_side")
	if hooks["evaluation_timeout"] != "500ms" || hooks["execution_timeout"] != "2s" {
		t.Errorf("timeouts = %v / %v, want 500ms / 2s", hooks["evaluation_timeout"], hooks["execution_timeout"])
	}
}

func TestEngineConfigsParseWithoutSelectEmbeddingsProperties(t *testing.T) {
	sides := sideImagesOC(nil)
	configs := map[string]string{
		combinedConfigKey: oramacoreConfigYAML(newOCV1(nil)),
		writerConfigKey:   engineSideFor(sides, "writer").template,
		readerConfigKey:   engineSideFor(sides, "reader").template,
	}
	for key, cfg := range configs {
		parseEngineConfig(t, key, cfg)
		if strings.Contains(cfg, "select_embeddings_properties") {
			t.Errorf("%s still renders select_embeddings_properties, which HooksConfig ignores", key)
		}
	}
}

func TestHooksExtraDeniedDomainsRollTheEngine(t *testing.T) {
	before := engineStatefulSets(t, fullStackOCV1(nil))
	after := engineStatefulSets(t, fullStackOCV1(withHooks(oramav1alpha1.EngineHooksSpec{ExtraDeniedDomains: []string{"10.244.**"}})))
	for side := range before {
		was := before[side].Spec.Template.Annotations["orama.cloud/config-hash"]
		now := after[side].Spec.Template.Annotations["orama.cloud/config-hash"]
		if was == "" || was == now {
			t.Errorf("%s config-hash %q did not change with extraDeniedDomains (now %q)", side, was, now)
		}
	}
}
