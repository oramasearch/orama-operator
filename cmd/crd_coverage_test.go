/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
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

// The cluster-management kinds are gated behind --enable-cluster-management and their CRDs live in
// chart/management-crds, not chart/crds: only the management cluster provisions other clusters
// (operator#351), and a leaf's operator must not watch or need them. Keep this list in step with
// the gated block in main.go.
var managementKinds = map[string]bool{
	"KubernetesCluster":         true,
	"KubernetesClusterProvider": true,
	"AksCluster":                true,
}

func registeredKinds(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var kinds []string
	for _, match := range reconcilerPattern.FindAllStringSubmatch(string(source), -1) {
		kind := match[1]
		// The row-intake reconcilers are additional controllers of Lake, not separate CRDs. The
		// volume snapshot reconciler reads snapshot.storage.k8s.io kinds uncached and watches only
		// Lake, so a cluster without the snapshot CRDs does not stop the manager.
		if kind == "LakeApps" || kind == "LakeWorkspaces" || kind == "LakeVolumeSnapshots" {
			kind = "Lake"
		}
		kinds = append(kinds, kind)
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

func managementChartKinds(t *testing.T) []string {
	return kindsIn(t, filepath.Join("..", "chart", "management-crds"))
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
	for _, kind := range managementChartKinds(t) {
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
	for _, kind := range append(append(chartKinds(t), e2eChartKinds(t)...), managementChartKinds(t)...) {
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

// A gated kind must live in its own folder, never in chart/crds: a management kind installed by
// the chart would be put on every leaf, and one missing from chart/management-crds would crash the
// management cluster's operator when the flag is on.
func TestManagementKindsLiveOnlyInManagementCRDs(t *testing.T) {
	for _, kind := range chartKinds(t) {
		if managementKinds[kind] {
			t.Errorf("%s is a cluster-management kind and must be in chart/management-crds, not chart/crds", kind)
		}
	}
	shipped := make(map[string]bool)
	for _, kind := range managementChartKinds(t) {
		shipped[kind] = true
		if !managementKinds[kind] {
			t.Errorf("chart/management-crds ships %s, which is not in managementKinds", kind)
		}
	}
	for kind := range managementKinds {
		if !shipped[kind] {
			t.Errorf("%s is a cluster-management kind with no CRD in chart/management-crds", kind)
		}
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	gate := strings.Index(string(source), "if enableClusterManagement {")
	if gate < 0 {
		t.Fatal("main.go no longer gates cluster management behind enableClusterManagement")
	}
	for kind := range managementKinds {
		registration := strings.Index(string(source), "controller."+kind+"Reconciler{")
		if registration < gate {
			t.Errorf("%sReconciler is registered outside the enableClusterManagement block", kind)
		}
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
	// The management kinds' RBAC is rendered only under clusterManagement.enabled; the resource
	// names are in the template either way, which is what this checks.
	management, err := filepath.Glob(filepath.Join("..", "chart", "management-crds", "*.yaml"))
	if err != nil {
		t.Fatalf("glob chart/management-crds: %v", err)
	}
	manifests = append(manifests, management...)
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

// The generated ClusterRole (config/rbac/role.yaml, from the +kubebuilder:rbac markers) is what the
// code needs; the chart's hand-maintained ClusterRole is what the operator gets. Every generated
// group/resource/verb must be granted by the chart, or the operator fails at runtime with a
// Forbidden that no unit test sees — the managed-services catalog's upstream kinds, for one.
// Template directives are dropped, so e2e-gated resources count as granted (their gating is
// checked above).
func TestChartRBACCoversGeneratedRole(t *testing.T) {
	generated := readClusterRoles(t, filepath.Join("..", "config", "rbac", "role.yaml"), false)
	chart := readClusterRoles(t, filepath.Join("..", "chart", "templates", "rbac.yaml"), true)
	if len(generated) != 1 || len(chart) == 0 {
		t.Fatalf("found %d generated and %d chart ClusterRoles", len(generated), len(chart))
	}
	var chartRules []rbacv1.PolicyRule
	for _, role := range chart {
		chartRules = append(chartRules, role.Rules...)
	}
	var missing []string
	for _, rule := range generated[0].Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					if !ruleGrants(chartRules, group, resource, verb) && !knownChartRBACGaps[group+"/"+resource+" "+verb] {
						missing = append(missing, fmt.Sprintf("%s/%s %s", group, resource, verb))
					}
				}
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("chart/templates/rbac.yaml does not grant what the code's RBAC markers require:\n  %s\n"+
			"Add them to the chart ClusterRole (make manifests regenerates only config/rbac).", strings.Join(missing, "\n  "))
	}
}

// knownChartRBACGaps are markers the chart deliberately does not grant. Pods: the
// OramaZettaCluster marker asks for watch, but nothing reads pods through the cache, and the chart
// grants get and list only (for the Zetta e2e log reader).
var knownChartRBACGaps = map[string]bool{
	"/pods watch": true,
}

func ruleGrants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	matches := func(values []string, want string) bool {
		for _, value := range values {
			if value == want || value == "*" {
				return true
			}
		}
		return false
	}
	for _, rule := range rules {
		if matches(rule.APIGroups, group) && matches(rule.Resources, resource) && matches(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func readClusterRoles(t *testing.T, path string, template bool) []rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if template {
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		raw = []byte(strings.Join(kept, "\n"))
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var roles []rbacv1.ClusterRole
	for {
		var role rbacv1.ClusterRole
		if err := decoder.Decode(&role); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		if role.Kind == "ClusterRole" {
			roles = append(roles, role)
		}
	}
	return roles
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
