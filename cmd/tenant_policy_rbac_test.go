/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

type policyRBACObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Rules             []rbacv1.PolicyRule `json:"rules,omitempty"`
	RoleRef           rbacv1.RoleRef      `json:"roleRef,omitempty"`
	Subjects          []rbacv1.Subject    `json:"subjects,omitempty"`
	source            string
}

type protectedPolicyResource struct {
	group    string
	resource string
}

var protectedPolicyResources = []protectedPolicyResource{
	{group: "networking.k8s.io", resource: "networkpolicies"},
	{group: "cilium.io", resource: "ciliumnetworkpolicies"},
	{group: "orama.cloud", resource: "networksandboxes"},
	// The remaining secure-networking kinds are reserved by the decided design. Keeping them in
	// this test before their controllers ship prevents a tenant wildcard from silently acquiring
	// write access when their CRDs are added.
	{group: "orama.cloud", resource: "connections"},
	{group: "orama.cloud", resource: "egressrules"},
	{group: "orama.cloud", resource: "tunnels"},
	{group: "orama.cloud", resource: "peerconnections"},
	{group: "orama.cloud", resource: "zettatunnels"},
	{group: "orama.cloud", resource: "cloudflaretunnels"},
}

var (
	rbacKindPattern  = regexp.MustCompile(`(?m)^kind:\s*(?:ClusterRole|Role|ClusterRoleBinding|RoleBinding)\s*$`)
	helmValuePattern = regexp.MustCompile(`{{[^{}]+}}`)
)

// Only the operator manager may write a policy resource. Kubernetes policy is additive: a tenant
// allow would reopen a destination that the operator's default deny closes. This test reads every
// checked-in Role and ClusterRole manifest that the operator, its chart, its generated install
// manifests, or its pinned delivered fixtures can create. It also follows bindings so the manager
// role cannot be handed to a workload identity.
func TestOnlyOperatorRBACCanWriteNetworkPolicy(t *testing.T) {
	objects := readRepositoryRBAC(t)
	roles := make(map[string]policyRBACObject)
	writers := make(map[string]bool)
	roleCount := 0
	for _, object := range objects {
		if object.Kind != "Role" && object.Kind != "ClusterRole" {
			continue
		}
		roleCount++
		key := policyRoleKey(object.Kind, object.Namespace, object.Name)
		roles[key] = object
		if !roleWritesProtectedPolicy(object.Rules) {
			continue
		}
		writers[key] = true
		if !isOperatorManagerRole(object) {
			t.Errorf("%s %s in %s can write protected network policy; only the operator manager may do so",
				object.Kind, object.Name, object.source)
		}
	}
	if roleCount == 0 {
		t.Fatal("found no Role or ClusterRole manifests; the repository RBAC scan is incomplete")
	}

	for _, object := range objects {
		if object.Kind != "RoleBinding" && object.Kind != "ClusterRoleBinding" {
			continue
		}
		key := policyRoleKey(object.RoleRef.Kind, object.Namespace, object.RoleRef.Name)
		_, roleIsKnown := roles[key]
		refIsProtected := writers[key]
		if !roleIsKnown && isBuiltInPolicyWriter(object.RoleRef.Name) {
			refIsProtected = true
		}
		if !refIsProtected {
			continue
		}
		if !isOperatorManagerBinding(object) {
			t.Errorf("%s %s in %s binds policy-writer %s/%s to a non-operator identity: %+v",
				object.Kind, object.Name, object.source, object.RoleRef.Kind, object.RoleRef.Name, object.Subjects)
		}
	}
}

func TestProtectedPolicyGrantDetection(t *testing.T) {
	tests := []struct {
		name  string
		rule  rbacv1.PolicyRule
		write bool
	}{
		{
			name: "read is allowed",
			rule: rbacv1.PolicyRule{
				APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list", "watch"},
			},
		},
		{
			name: "exact network policy write",
			rule: rbacv1.PolicyRule{
				APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"create"},
			},
			write: true,
		},
		{
			name: "wildcard API group",
			rule: rbacv1.PolicyRule{
				APIGroups: []string{"*"}, Resources: []string{"ciliumnetworkpolicies"}, Verbs: []string{"update"},
			},
			write: true,
		},
		{
			name: "wildcard resource",
			rule: rbacv1.PolicyRule{
				APIGroups: []string{"orama.cloud"}, Resources: []string{"*"}, Verbs: []string{"patch"},
			},
			write: true,
		},
		{
			name: "wildcard verb",
			rule: rbacv1.PolicyRule{
				APIGroups: []string{"orama.cloud"}, Resources: []string{"networksandboxes"}, Verbs: []string{"*"},
			},
			write: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := roleWritesProtectedPolicy([]rbacv1.PolicyRule{test.rule}); got != test.write {
				t.Errorf("roleWritesProtectedPolicy() = %v, want %v", got, test.write)
			}
		})
	}
}

func readRepositoryRBAC(t *testing.T) []policyRBACObject {
	t.Helper()
	root := ".."
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || path == filepath.Join(root, "chart", "crds") || path == filepath.Join(root, "config", "crd") {
				return filepath.SkipDir
			}
			return nil
		}
		if extension := filepath.Ext(path); extension != ".yaml" && extension != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rbacKindPattern.Match(raw) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover RBAC manifests: %v", err)
	}
	sort.Strings(paths)
	var objects []policyRBACObject
	for _, path := range paths {
		objects = append(objects, decodePolicyRBAC(t, path)...)
	}
	return objects
}

func decodePolicyRBAC(t *testing.T, path string) []policyRBACObject {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(raw), "{{") {
		raw = sanitizeHelmRBAC(raw)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var objects []policyRBACObject
	for {
		var object policyRBACObject
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		switch object.Kind {
		case "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding":
			object.source = filepath.ToSlash(strings.TrimPrefix(path, "../"))
			objects = append(objects, object)
		}
	}
	return objects
}

func sanitizeHelmRBAC(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{{") {
			continue
		}
		if strings.Contains(line, "{{") {
			prefix := line[:strings.Index(line, "{{")]
			if strings.HasSuffix(strings.TrimSpace(prefix), "labels:") {
				line = prefix + "{}"
			} else {
				line = helmValuePattern.ReplaceAllString(line, "template")
			}
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}

func policyRoleKey(kind, namespace, name string) string {
	if kind == "ClusterRole" {
		return kind + "/" + name
	}
	return kind + "/" + namespace + "/" + name
}

func roleWritesProtectedPolicy(rules []rbacv1.PolicyRule) bool {
	for _, protected := range protectedPolicyResources {
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			if policyRuleGrants(rules, protected.group, protected.resource, verb) {
				return true
			}
		}
	}
	return false
}

func policyRuleGrants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	matches := func(values []string, wanted string) bool {
		for _, value := range values {
			if value == wanted || value == "*" {
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

func isOperatorManagerRole(object policyRBACObject) bool {
	return object.Kind == "ClusterRole" &&
		(object.source == "config/rbac/role.yaml" && object.Name == "manager-role" ||
			object.source == "chart/templates/rbac.yaml" && object.Name == "template-manager")
}

func isBuiltInPolicyWriter(name string) bool {
	return name == "cluster-admin" || name == "admin" || name == "edit"
}

func isOperatorManagerBinding(object policyRBACObject) bool {
	if object.Kind != "ClusterRoleBinding" || object.source != "chart/templates/rbac.yaml" ||
		object.RoleRef.Kind != "ClusterRole" || object.RoleRef.Name != "template-manager" ||
		len(object.Subjects) != 1 {
		return false
	}
	subject := object.Subjects[0]
	return subject.Kind == "ServiceAccount" && subject.Name == "template" && subject.Namespace == "template"
}
