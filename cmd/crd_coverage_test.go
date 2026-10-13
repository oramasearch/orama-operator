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

	"sigs.k8s.io/yaml"

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

// The e2e test kinds. Their CRDs are in chart/crds and their controllers run on every cluster,
// like every other kind (Steve, 2026-10-12, "no differences").
var e2eKinds = map[string]bool{
	"OramaCloudV1E2ETest": true,
	"ZettaClusterE2ETest": true,
	"LakeE2ETest":         true,
}

// The cluster-management kinds ship in chart/crds and their controllers run on every cluster:
// there is no difference between a management cluster's operator and a leaf's, and any cluster
// can manage others (Steve, 2026-10-12; operator#351). This list holds them to that.
var managementKinds = map[string]bool{
	"KubernetesCluster":         true,
	"KubernetesClusterProvider": true,
	"AksCluster":                true,
	"ClusterHelmRelease":        true,
	"OramaOperator":             true,
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
	for _, kind := range chartKinds(t) {
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

// The operator is the same on every cluster. The management kinds are in the install chart, their
// controllers are registered unconditionally, and neither the manager nor the chart has a switch
// that would make one cluster's operator differ from another's in what it can manage.
func TestManagementKindsShipAndRunOnEveryCluster(t *testing.T) {
	shipped := make(map[string]bool)
	for _, kind := range chartKinds(t) {
		shipped[kind] = true
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for kind := range managementKinds {
		if !shipped[kind] {
			t.Errorf("%s has no CRD in chart/crds, so a cluster installed from the chart could not manage others", kind)
		}
		if !strings.Contains(string(source), "controller."+kind+"Reconciler{") {
			t.Errorf("%sReconciler is not registered", kind)
		}
	}
	if strings.Contains(string(source), "enable-cluster-management") {
		t.Error("main.go has an --enable-cluster-management flag again: the operator must not differ between clusters")
	}
	for _, file := range []string{"values.yaml", "templates/rbac.yaml", "templates/operator-deployment.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "chart", file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "clusterManagement.enabled") || regexp.MustCompile(`(?m)^clusterManagement:\n  enabled:`).Match(data) {
			t.Errorf("chart/%s switches cluster management on or off: the operator must not differ between clusters", file)
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

// Every cluster gets exactly the same CRDs: one chart, one directory. A second directory of CRDs
// applied only to some clusters is how clusters come to differ, and it has been removed twice
// (chart/e2e-crds and chart/management-crds, 2026-10-12).
func TestTheChartHasOneCRDDirectoryWithEveryKind(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "chart"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "crds") && entry.Name() != "crds" {
			t.Errorf("chart/%s exists: every CRD belongs in chart/crds, installed on every cluster", entry.Name())
		}
	}
	shipped := make(map[string]bool)
	for _, kind := range chartKinds(t) {
		shipped[kind] = true
	}
	for kind := range e2eKinds {
		if !shipped[kind] {
			t.Errorf("%s has no CRD in chart/crds", kind)
		}
	}
}

// THE RULE (CLAUDE.md, "One operator: no differences between clusters"): the operator is the same
// on every cluster. Nothing in the manager or the chart may make one cluster's operator differ
// from another's in which kinds it has, which controllers it runs, or what it is allowed to do.
// Steve said it three times on 2026-10-12 before it was done; this test is why it stays done.
func TestNoClusterDiffersFromAnother(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Every controller is registered at the top level of main, never inside a condition.
	registration := regexp.MustCompile(`(?m)^(\t+)if err = \(&controller\.([A-Za-z0-9]+)Reconciler\{`)
	matches := registration.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no controller registrations in main.go; the pattern this test relies on changed")
	}
	for _, match := range matches {
		if match[1] != "\t" {
			t.Errorf("%sReconciler is registered inside a condition: every controller runs on every cluster", match[2])
		}
	}
	for _, kind := range registeredKinds(t) {
		found := false
		for _, match := range matches {
			found = found || match[2] == kind
		}
		if !found {
			t.Errorf("%sReconciler is registered in a form this test cannot see; register it like the others", kind)
		}
	}
	// No flag turns a set of controllers on or off.
	if flagPattern := regexp.MustCompile(`"enable-[a-z0-9-]*(controllers?|management)[a-z0-9-]*"`); flagPattern.Match(source) {
		t.Errorf("main.go has a flag that turns controllers on or off (%s): no cluster's operator may differ from another's", flagPattern.Find(source))
	}
	// The chart's RBAC and manager arguments do not depend on values.
	for _, file := range []string{"templates/rbac.yaml", "templates/operator-deployment.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "chart", file))
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "{{- if") {
				continue
			}
			// Allowed: where images are pulled from and how the pod calls a cloud. These are a
			// cluster's configuration, not a different operator.
			if strings.Contains(line, "imagePullSecrets") || strings.Contains(line, "azureWorkloadIdentityClientID") {
				continue
			}
			t.Errorf("chart/%s:%d renders conditionally (%s): RBAC and manager arguments are the same on every cluster", file, number+1, strings.TrimSpace(line))
		}
	}
}

// A CRD the API server will refuse must not ship. gofmt treats a "// +kubebuilder" marker as
// comment text and rewrites two single quotes in it into a typographic quote, which is not valid
// CEL; the cluster refused ClusterHelmRelease for an empty-string literal in a validation rule on
// 2026-10-12 (write size(x) > 0 instead). A validation rule in a shipped CRD is plain ASCII.
func TestShippedCRDValidationRulesArePlainASCII(t *testing.T) {
	for _, dir := range []string{"../chart/crds"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			// Parsed, because YAML wraps a long rule over several lines.
			var crd any
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("%s/%s: %v", dir, entry.Name(), err)
			}
			var walk func(node any)
			walk = func(node any) {
				switch typed := node.(type) {
				case map[string]any:
					for key, value := range typed {
						if rule, ok := value.(string); ok && key == "rule" {
							for _, r := range rule {
								if r > 126 {
									t.Errorf("%s/%s: a validation rule has the non-ASCII character %q: %s", dir, entry.Name(), r, rule)
									break
								}
							}
						}
						walk(value)
					}
				case []any:
					for _, value := range typed {
						walk(value)
					}
				}
			}
			walk(crd)
		}
	}
}
