/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// v1HooksFixture is the hooks block both sides render for a CR named v1, written out by hand from
// Prod 1's configs (orama-deploy 655f1a2) and #306, not from the production slices.
const v1HooksFixture = `  hooks:
    denied_domains:
      - "127.*"
      - "localhost"
      - "::1"
      - "10.*"
      - "172.1[6-9].*"
      - "172.2[0-9].*"
      - "172.3[0-1].*"
      - "192.168.*"
      - "169.254.169.254"
      - "169.254.169.253"
      - "169.254.169.123"
      - "169.254.*"
      - "fd00:ec2::*"
      - "100.6[4-9].*"
      - "100.[7-9][0-9].*"
      - "100.1[0-1][0-9].*"
      - "100.12[0-7].*"
      - "22[4-9].*"
      - "23[0-9].*"
      - "24[0-9].*"
      - "25[0-5].*"
      - "255.255.255.255"
      - "0.0.0.0"
      - "192.0.0.*"
      - "192.0.2.*"
      - "198.51.100.*"
      - "203.0.113.*"
      - "198.1[8-9].*"
      - "fc00:*"
      - "fd00:*"
      - "fe80:*"
      - "ff00:*"
      - "127.**"
      - "10.**"
      - "172.1[6-9].**"
      - "172.2[0-9].**"
      - "172.3[0-1].**"
      - "192.168.**"
      - "169.254.**"
      - "100.6[4-9].**"
      - "100.[7-9][0-9].**"
      - "100.1[0-1][0-9].**"
      - "100.12[0-7].**"
      - "22[4-9].**"
      - "23[0-9].**"
      - "24[0-9].**"
      - "25[0-5].**"
      - "192.0.0.**"
      - "192.0.2.**"
      - "198.51.100.**"
      - "203.0.113.**"
      - "198.1[8-9].**"
      - "*.svc"
      - "*.svc.cluster.local"
      - "*.cluster.local"
      - "kubernetes*"
      - "*.internal"
      - "**.svc"
      - "**.cluster.local"
      - "kubernetes*.**"
      - "**.internal"
      - "::ffff:*"
      - "::"
      - "v1-*"
      - "v1-*.**"
    evaluation_timeout: 200ms
    execution_timeout: 1s
`

// The expected list in its four parts, independent of the production slices.
var (
	wantProd1Domains = []string{
		"127.*", "localhost", "::1",
		"10.*", "172.1[6-9].*", "172.2[0-9].*", "172.3[0-1].*", "192.168.*",
		"169.254.169.254", "169.254.169.253", "169.254.169.123", "169.254.*", "fd00:ec2::*",
		"100.6[4-9].*", "100.[7-9][0-9].*", "100.1[0-1][0-9].*", "100.12[0-7].*",
		"22[4-9].*", "23[0-9].*", "24[0-9].*", "25[0-5].*", "255.255.255.255", "0.0.0.0",
		"192.0.0.*", "192.0.2.*", "198.51.100.*", "203.0.113.*", "198.1[8-9].*",
		"fc00:*", "fd00:*", "fe80:*", "ff00:*",
	}
	wantDerivedDomains = []string{
		"127.**", "10.**", "172.1[6-9].**", "172.2[0-9].**", "172.3[0-1].**", "192.168.**", "169.254.**",
		"100.6[4-9].**", "100.[7-9][0-9].**", "100.1[0-1][0-9].**", "100.12[0-7].**",
		"22[4-9].**", "23[0-9].**", "24[0-9].**", "25[0-5].**",
		"192.0.0.**", "192.0.2.**", "198.51.100.**", "203.0.113.**", "198.1[8-9].**",
	}
	// The cluster entries after localhost, which Prod 1's list already holds.
	wantClusterDomains = []string{
		"*.svc", "*.svc.cluster.local", "*.cluster.local", "kubernetes*", "*.internal",
		"**.svc", "**.cluster.local", "kubernetes*.**", "**.internal",
		"::ffff:*", "::",
	}
)

func wantHookDomains(name string) []string {
	return slices.Concat(wantProd1Domains, wantDerivedDomains, wantClusterDomains, []string{name + "-*", name + "-*.**"})
}

func namedOC(name string, mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Name = name
		if mut != nil {
			mut(o)
		}
	})
}

// sideHooks is the parsed hooks mapping of one side of a config, and whether the side is present.
func sideHooks(t *testing.T, doc, side string) (map[string]any, bool) {
	t.Helper()
	s, ok := parseYAML(t, doc)[side].(map[string]any)
	if !ok {
		return nil, false
	}
	h, ok := s["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no hooks mapping", side)
	}
	return h, true
}

func TestHookDeniedDomains(t *testing.T) {
	if n := len(wantProd1Domains) + len(wantDerivedDomains) + len(wantClusterDomains) + 2; n != 65 {
		t.Fatalf("expected list has %d entries, want 65", n)
	}
	got := hookDeniedDomains(namedOC("v1-dev", nil))
	if want := wantHookDomains("v1-dev"); !reflect.DeepEqual(got, want) {
		t.Fatalf("hookDeniedDomains =\n%q\nwant\n%q", got, want)
	}
	seen := map[string]int{}
	for _, d := range got {
		seen[d]++
		if seen[d] > 1 {
			t.Errorf("%q appears more than once", d)
		}
	}
	for _, d := range []string{"localhost", "127.**", "10.**", "169.254.**"} {
		if seen[d] != 1 {
			t.Errorf("%q appears %d times, want once", d, seen[d])
		}
	}
	if tail := got[len(got)-2:]; !reflect.DeepEqual(tail, []string{"v1-dev-*", "v1-dev-*.**"}) {
		t.Errorf("last two entries = %q", tail)
	}
}

// Every render path carries the same hooks block, and a side-only config leaves the other side out.
func TestHooksRenderedOnEverySide(t *testing.T) {
	oc := namedOC("v1", nil)
	if got := hooksYAML(oc); got != v1HooksFixture {
		t.Fatalf("hooksYAML(v1) =\n%s\nwant\n%s", got, v1HooksFixture)
	}
	want, _ := sideHooks(t, "writer_side:\n"+v1HooksFixture, "writer_side")

	configs := []struct {
		name, doc string
		sides     map[string]bool
	}{
		{"combined", oramacoreConfigYAML(oc), map[string]bool{"writer_side": true, "reader_side": true}},
		{"writer-only", oramacoreWriterConfigYAML(oc), map[string]bool{"writer_side": true, "reader_side": false}},
		{"reader-only", oramacoreReaderConfigYAML(oc), map[string]bool{"writer_side": false, "reader_side": true}},
	}
	for _, c := range configs {
		for side, present := range c.sides {
			got, ok := sideHooks(t, c.doc, side)
			if ok != present {
				t.Errorf("%s config: %s present = %v, want %v", c.name, side, ok, present)
				continue
			}
			if present && !reflect.DeepEqual(got, want) {
				t.Errorf("%s config: %s hooks = %v, want %v", c.name, side, got, want)
			}
		}
	}

	h := want
	if h["evaluation_timeout"] != "200ms" || h["execution_timeout"] != "1s" {
		t.Errorf("timeouts = %v / %v, want 200ms / 1s", h["evaluation_timeout"], h["execution_timeout"])
	}
	var domains []string
	for _, d := range h["denied_domains"].([]any) {
		s, ok := d.(string)
		if !ok {
			t.Fatalf("denied_domains entry %v parsed as %T, want a string", d, d)
		}
		domains = append(domains, s)
	}
	if !reflect.DeepEqual(domains, wantHookDomains("v1")) {
		t.Errorf("parsed denied_domains =\n%q\nwant\n%q", domains, wantHookDomains("v1"))
	}
}

// OramaCore refuses allowed_domains next to denied_domains, and select_embeddings_properties is not
// one of its hooks keys; neither is rendered in either image mode.
func TestHooksRenderOnlyDeniedDomainsAndTimeouts(t *testing.T) {
	oc := namedOC("v1", nil)
	for name, doc := range map[string]string{
		"combined":    oramacoreConfigYAML(oc),
		"writer-only": oramacoreWriterConfigYAML(oc),
		"reader-only": oramacoreReaderConfigYAML(oc),
	} {
		for _, key := range []string{"allowed_domains", "select_embeddings_properties"} {
			if strings.Contains(doc, key) {
				t.Errorf("%s config renders %s", name, key)
			}
		}
		for _, side := range []string{"writer_side", "reader_side"} {
			h, ok := sideHooks(t, doc, side)
			if !ok {
				continue
			}
			keys := slices.Sorted(maps.Keys(h))
			if want := []string{"denied_domains", "evaluation_timeout", "execution_timeout"}; !reflect.DeepEqual(keys, want) {
				t.Errorf("%s config %s hooks keys = %v, want %v", name, side, keys, want)
			}
			list := h["denied_domains"].([]any)
			for _, p := range []string{"*.svc", "172.1[6-9].**", "**.cluster.local", "::"} {
				if !slices.Contains(list, any(p)) {
					t.Errorf("%s config %s denied_domains lacks the string %q", name, side, p)
				}
			}
		}
	}
}

// Rendering is a pure function of the CR: interleaving another CR changes nothing, names do not leak
// between CRs, and the package-level source lists are never modified.
func TestHooksRenderingIsStablePerCR(t *testing.T) {
	prod1, cluster := slices.Clone(prod1HookDeniedDomains), slices.Clone(clusterHookDeniedDomains)
	dev, other := namedOC("v1-dev", nil), namedOC("other", nil)

	first := oramacoreConfigYAML(dev)
	firstHooks := hooksYAML(dev)
	otherHooks := hooksYAML(other)
	returned := hookDeniedDomains(other)
	returned[0] = "mutated"
	_ = append(returned[:1], "appended")

	for i := range 3 {
		if got := oramacoreConfigYAML(dev); got != first {
			t.Fatalf("render %d of v1-dev differs from the first", i)
		}
		if got := hooksYAML(dev); got != firstHooks {
			t.Fatalf("hooks render %d of v1-dev differs from the first", i)
		}
		if got := hooksYAML(other); got != otherHooks {
			t.Fatalf("hooks render %d of other differs from the first", i)
		}
	}
	if !reflect.DeepEqual(prod1HookDeniedDomains, prod1) || !reflect.DeepEqual(clusterHookDeniedDomains, cluster) {
		t.Fatal("rendering modified the source lists")
	}
	if got := hookDeniedDomains(other); !reflect.DeepEqual(got, wantHookDomains("other")) {
		t.Errorf("hookDeniedDomains(other) after mutating an earlier result = %q", got)
	}
	if strings.Contains(firstHooks, `"other-`) || strings.Contains(otherHooks, `"v1-dev-`) {
		t.Error("one CR's Service patterns appear in the other's hooks block")
	}
	if !strings.HasSuffix(firstHooks, "      - \"v1-dev-*\"\n      - \"v1-dev-*.**\"\n    evaluation_timeout: 200ms\n    execution_timeout: 1s\n") {
		t.Errorf("v1-dev hooks block does not end with its Service patterns and timeouts:\n%s", firstHooks)
	}
}

// The writer and reader pod templates carry the hash of the template they render, in both image
// modes, so a hooks change rolls the engine and an unchanged template does not.
func TestHooksCoveredByConfigHash(t *testing.T) {
	for mode, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		t.Run(mode, func(t *testing.T) {
			h := newEngineHarness(t)
			oc := engineOC(mut)
			h.reconcile(oc)
			cm := h.engineConfig()
			for side, hash := range map[string]string{"writer": h.writer().Spec.Template.Annotations[configHashKey],
				"reader": h.reader().Spec.Template.Annotations[configHashKey]} {
				s := engineSideFor(oc, side)
				if !strings.Contains(cm.Data[s.configKey], v1HooksFixture) {
					t.Errorf("%s template %s lacks the hooks block", side, s.configKey)
				}
				if want := templateSHA256(cm.Data[s.configKey]); hash != want {
					t.Errorf("%s config hash = %q, want SHA-256 of %s %q", side, hash, s.configKey, want)
				}

				same := engineConfigAnnotations(engineSideFor(oc, side), "", "")[configHashKey]
				if same != hash {
					t.Errorf("%s: an identical template hashed to %q, want %q", side, same, hash)
				}
				changed := s
				changed.template = strings.Replace(s.template, `      - "::"`+"\n", "", 1)
				if changed.template == s.template {
					t.Fatalf("%s: test edit did not change the hooks text", side)
				}
				if got := engineConfigAnnotations(changed, "", "")[configHashKey]; got == hash {
					t.Errorf("%s: a hooks-only change kept config hash %q", side, got)
				}
			}
		})
	}
}
