/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The manager registers a watch for every kind unconditionally, so a kind whose CRD is not on the
// cluster is not a degraded controller: the cache never syncs, the manager exits, and the pod
// crash-loops with every other controller down with it. That took the operator down for ~12 hours
// on 2026-09-09 (operator#166), twice in a row — lakee2etests, then clerkapplications.
//
// Fail-fast is deliberate; see docs/operations/crd-rollout.md. What makes it safe is that the two
// halves of the invariant are checked rather than remembered:
//
//   - a controller cannot ship without its CRD in the chart — this test, in CI, no cluster needed;
//   - a chart CRD cannot be missing from the cluster — hack/check-cluster-crds.sh, at rollout.
//
// This test covers the first half.

var (
	reconcilerPattern   = regexp.MustCompile(`controller\.([A-Za-z0-9]+)Reconciler\{`)
	crdKindPattern      = regexp.MustCompile(`(?m)^    kind: ([A-Za-z0-9]+)$`)
	crdPluralPattern    = regexp.MustCompile(`(?m)^  name: ([a-z0-9]+)\.orama\.cloud`)
	rbacResourcePattern = regexp.MustCompile(`(?m)^\s+- ([a-z0-9]+)$`)
)

// e2e kinds are gated behind --enable-e2e-controllers and their CRDs live in chart/e2e-crds, not
// chart/crds: production runs no e2e suites. Keep this list in step with the gated block in main.go.
var e2eKinds = map[string]bool{
	"OramaCloudV1E2ETest": true,
	"ZettaClusterE2ETest": true,
	"LakeE2ETest":         true,
}

func registeredKinds(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var kinds []string
	for _, match := range reconcilerPattern.FindAllStringSubmatch(string(source), -1) {
		kinds = append(kinds, match[1])
	}
	if len(kinds) == 0 {
		t.Fatal("no controller.XxxReconciler registrations found in main.go; the pattern needs updating")
	}
	sort.Strings(kinds)
	return kinds
}

func chartKinds(t *testing.T) []string { return kindsIn(t, filepath.Join("..", "chart", "crds")) }

func e2eChartKinds(t *testing.T) []string {
	return kindsIn(t, filepath.Join("..", "chart", "e2e-crds"))
}

func kindsIn(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s contains no CRDs", dir)
	}
	var kinds []string
	for _, file := range files {
		manifest, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		// spec.names.kind, the singular kind the controller watches. Indented four spaces under
		// spec.names, which distinguishes it from the top-level `kind: CustomResourceDefinition`.
		match := crdKindPattern.FindStringSubmatch(string(manifest))
		if match == nil {
			t.Fatalf("no spec.names.kind in %s", file)
		}
		kinds = append(kinds, match[1])
	}
	sort.Strings(kinds)
	return kinds
}

// Every controller the manager starts must have a CRD in the chart. Without this, adding a
// reconciler and forgetting its CRD produces a rollout that crash-loops the entire operator, and
// the first sign is production.
func TestEveryRegisteredControllerHasAChartCRD(t *testing.T) {
	chart := make(map[string]bool)
	for _, kind := range chartKinds(t) {
		chart[kind] = true
	}
	for _, kind := range e2eChartKinds(t) {
		chart[kind] = true
	}
	var missing []string
	for _, kind := range registeredKinds(t) {
		if !chart[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("main.go starts controllers for kinds the chart ships no CRD for: %s\n"+
			"Run `make helm-package` (or copy config/crd/bases/*.yaml into chart/crds/). "+
			"Rolling this would crash-loop the whole operator, not just these kinds.",
			strings.Join(missing, ", "))
	}
}

// And the reverse: a CRD in the chart that nothing reconciles is either a controller that was
// removed without its CRD, or a kind installed on every cluster for no reason. Both are worth
// noticing.
func TestEveryChartCRDHasARegisteredController(t *testing.T) {
	registered := make(map[string]bool)
	for _, kind := range registeredKinds(t) {
		registered[kind] = true
	}
	var orphaned []string
	for _, kind := range append(chartKinds(t), e2eChartKinds(t)...) {
		if !registered[kind] {
			orphaned = append(orphaned, kind)
		}
	}
	if len(orphaned) > 0 {
		t.Fatalf("chart/crds ships CRDs no controller reconciles: %s\n"+
			"Either the controller was dropped and its CRD left behind, or the CRD should not be "+
			"installed on every cluster.", strings.Join(orphaned, ", "))
	}
}

// The chart ClusterRole is hand-maintained, and a kind the manager watches without RBAC to list and
// watch it fails its cache sync exactly like a missing CRD does — same crash, same blast radius,
// third place to forget. CLAUDE.md warns about it; this makes the warning enforceable.
func TestEveryChartCRDIsCoveredByChartRBAC(t *testing.T) {
	manifests, err := filepath.Glob(filepath.Join("..", "chart", "crds", "*.yaml"))
	if err != nil {
		t.Fatalf("glob chart/crds: %v", err)
	}
	var plurals []string
	for _, file := range manifests {
		manifest, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		match := crdPluralPattern.FindStringSubmatch(string(manifest))
		if match == nil {
			t.Fatalf("no metadata.name of the form <plural>.orama.cloud in %s", file)
		}
		plurals = append(plurals, match[1])
	}
	if len(plurals) == 0 {
		t.Fatal("chart/crds contains no CRDs")
	}

	rbac, err := os.ReadFile(filepath.Join("..", "chart", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read chart/templates/rbac.yaml: %v", err)
	}
	granted := make(map[string]bool)
	for _, match := range rbacResourcePattern.FindAllStringSubmatch(string(rbac), -1) {
		granted[match[1]] = true
	}

	var missing []string
	for _, plural := range plurals {
		if !granted[plural] {
			missing = append(missing, plural)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("chart/templates/rbac.yaml grants nothing for: %s\n"+
			"The manager cannot list or watch these, so its cache never syncs and the whole "+
			"operator crash-loops. Add them to the ClusterRole rules.", strings.Join(missing, ", "))
	}
}

// Production runs no e2e suites (that is what a testing environment is for), so the install chart
// must not put an e2e CRD on every cluster. One of them — LakeE2ETest — is what crash-looped
// production for twelve hours in operator#166 while reconciling nothing anyone wanted.
func TestInstallChartShipsNoE2ECRD(t *testing.T) {
	var leaked []string
	for _, kind := range chartKinds(t) {
		if e2eKinds[kind] {
			leaked = append(leaked, kind)
		}
	}
	if len(leaked) > 0 {
		sort.Strings(leaked)
		t.Fatalf("chart/crds ships e2e kinds: %s\n"+
			"They belong in chart/e2e-crds, applied only by testing environments alongside "+
			"--set e2e.enabled=true.", strings.Join(leaked, ", "))
	}
}

// And the gate itself: every kind in chart/e2e-crds is one main.go registers behind the flag, so a
// kind cannot quietly move between the two sets.
func TestE2EChartHoldsExactlyTheGatedKinds(t *testing.T) {
	seen := make(map[string]bool)
	for _, kind := range e2eChartKinds(t) {
		if !e2eKinds[kind] {
			t.Errorf("chart/e2e-crds ships %s, which is not gated behind --enable-e2e-controllers", kind)
		}
		seen[kind] = true
	}
	for kind := range e2eKinds {
		if !seen[kind] {
			t.Errorf("%s is gated behind --enable-e2e-controllers but chart/e2e-crds ships no CRD for it", kind)
		}
	}
}
