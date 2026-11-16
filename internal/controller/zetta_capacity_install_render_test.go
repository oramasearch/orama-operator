// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// operator#79: the cluster name and role ARN were pasted into the YAML text before parsing, so these
// values injected a second document (a cluster-admin binding) that the operator then applied.
var hostileInstallValues = []string{
	"orama\"\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: pwn\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cluster-admin\nsubjects:\n- kind: Group\n  name: system:authenticated\n  apiGroup: rbac.authorization.k8s.io\n",
	"arn:aws:iam::123456789012:role/x\n---\nkind: ClusterRoleBinding\n",
	"x\"\n              valueFrom: {}\n",
	"x\n    eks.amazonaws.com/audience: evil",
	"{\"a\": 1}",
	"'quoted'",
	"__CLUSTER_NAME__",
	"__CONTROLLER_ROLE_ARN__",
}

func TestValidateKarpenterInstallValues(t *testing.T) {
	validNames := []string{"orama-eks-prod", "a", "A", "9cluster", "a_b-c", strings.Repeat("a", 100)}
	invalidNames := append([]string{
		"-leading-hyphen", "_leading-underscore", "has space", "has.dot", "has/slash", "has:colon",
		"line\nbreak", "trailing-newline\n", "quote\"", "tab\t", "nul\x00", "ünïcode",
		strings.Repeat("a", 101),
	}, hostileInstallValues...)
	validARNs := []string{
		"arn:aws:iam::123456789012:role/KarpenterController",
		"arn:aws-cn:iam::123456789012:role/KarpenterController",
		"arn:aws-us-gov:iam::123456789012:role/path/to/Role+=,.@_-",
		"arn:aws:iam::123456789012:role/" + strings.Repeat("r", 512),
	}
	invalidARNs := append([]string{
		"arn:aws:iam::12345678901:role/short-account",
		"arn:aws:iam::1234567890123:role/long-account",
		"arn:aws:iam::123456789012:user/not-a-role",
		"arn:aws:sts::123456789012:assumed-role/x/y",
		"arn:aws-foo:iam::123456789012:role/bad-partition",
		"arn:aws:iam::123456789012:role/",
		"ARN:aws:iam::123456789012:role/upper",
		"arn:aws:iam::123456789012:role/sp ace",
		"arn:aws:iam::123456789012:role/x\n",
		"arn:aws:iam::123456789012:role/x\"",
		"arn:aws:iam::123456789012:role/x#comment",
		"arn:aws:iam::123456789012:role/" + strings.Repeat("r", 513),
		"KarpenterController",
	}, hostileInstallValues...)

	for _, name := range validNames {
		if err := validateKarpenterInstallValues(name, ""); err != nil {
			t.Errorf("valid cluster name %q refused: %v", name, err)
		}
	}
	for _, arn := range validARNs {
		if err := validateKarpenterInstallValues("", arn); err != nil {
			t.Errorf("valid role ARN %q refused: %v", arn, err)
		}
	}
	for _, name := range invalidNames {
		err := validateKarpenterInstallValues(name, "")
		if err == nil {
			t.Errorf("invalid cluster name %q accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "installClusterName") || strings.Contains(err.Error(), name) {
			t.Errorf("cluster name error must name the field and not echo the value: %v", err)
		}
	}
	for _, arn := range invalidARNs {
		err := validateKarpenterInstallValues("", arn)
		if err == nil {
			t.Errorf("invalid role ARN %q accepted", arn)
			continue
		}
		if !strings.Contains(err.Error(), "controllerRoleARN") || strings.Contains(err.Error(), arn) {
			t.Errorf("role ARN error must name the field and not echo the value: %v", err)
		}
	}
	// Empty is a missing prerequisite (its own condition), not a format error.
	if err := validateKarpenterInstallValues("", ""); err != nil {
		t.Errorf("empty values must pass format validation: %v", err)
	}
}

// resetInstallValues puts the placeholders back in the two fields, so a rendered bundle can be compared
// with the template field by field.
func resetInstallValues(t *testing.T, objs []*unstructured.Unstructured) {
	t.Helper()
	for _, obj := range objs {
		if obj.GetNamespace() != karpenterNamespace || obj.GetName() != karpenterControllerName {
			continue
		}
		switch obj.GetKind() {
		case "ServiceAccount":
			annotations := obj.GetAnnotations()
			annotations[installRoleARNAnnotation] = installRoleARNPlaceholder
			obj.SetAnnotations(annotations)
		case "Deployment":
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			for _, raw := range containers {
				container := raw.(map[string]interface{})
				for _, rawVar := range container["env"].([]interface{}) {
					envVar := rawVar.(map[string]interface{})
					if envVar["name"] == installClusterNameEnv {
						envVar["value"] = installClusterNamePlaceholder
					}
				}
			}
			if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// installValuesOf reads back the two fields from a rendered bundle.
func installValuesOf(t *testing.T, objs []*unstructured.Unstructured) (clusterName, roleARN string) {
	t.Helper()
	for _, obj := range objs {
		if obj.GetNamespace() != karpenterNamespace || obj.GetName() != karpenterControllerName {
			continue
		}
		switch obj.GetKind() {
		case "ServiceAccount":
			roleARN = obj.GetAnnotations()[installRoleARNAnnotation]
		case "Deployment":
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			for _, raw := range containers {
				for _, rawVar := range raw.(map[string]interface{})["env"].([]interface{}) {
					envVar := rawVar.(map[string]interface{})
					if envVar["name"] == installClusterNameEnv {
						clusterName = envVar["value"].(string)
					}
				}
			}
		}
	}
	return clusterName, roleARN
}

// assertOnlyInstallValuesChanged checks a rendered bundle has the template's objects, carries the two
// values exactly, and equals the template everywhere else.
func assertOnlyInstallValuesChanged(t *testing.T, rendered []*unstructured.Unstructured, clusterName, roleARN string) {
	t.Helper()
	if err := requireExactObjects(rendered, karpenterControllerObjects); err != nil {
		t.Fatalf("rendered bundle object set: %v", err)
	}
	gotName, gotARN := installValuesOf(t, rendered)
	if gotName != clusterName || gotARN != roleARN {
		t.Fatalf("rendered values = (%q, %q), want (%q, %q)", gotName, gotARN, clusterName, roleARN)
	}
	// The apply body is JSON; a YAML round trip of every object must also give back the same objects.
	for _, obj := range rendered {
		raw, err := yaml.Marshal(obj.Object)
		if err != nil {
			t.Fatal(err)
		}
		again, err := decodeManifests(raw)
		if err != nil || len(again) != 1 || !equality.Semantic.DeepEqual(again[0].Object, obj.Object) {
			t.Fatalf("%s %s does not survive a YAML round trip as one object (err %v, %d objects)", obj.GetKind(), obj.GetName(), err, len(again))
		}
	}
	template, err := karpenterControllerTemplate()
	if err != nil {
		t.Fatal(err)
	}
	resetInstallValues(t, rendered)
	for i := range template {
		if !equality.Semantic.DeepEqual(template[i].Object, rendered[i].Object) {
			t.Fatalf("%s %s changed outside the two install fields", rendered[i].GetKind(), rendered[i].GetName())
		}
	}
}

func TestRenderKarpenterControllerSetsOnlyTheTwoFields(t *testing.T) {
	const name, arn = "orama-eks-prod", "arn:aws:iam::123456789012:role/KarpenterController"
	rendered, err := renderKarpenterController(name, arn)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyInstallValuesChanged(t, rendered, name, arn)
}

// Validation is the first line; this shows the second holds without it. Even an unvalidated hostile value
// is one string in one field: no extra object, no other field touched.
func TestKarpenterInstallValuesCannotInjectObjects(t *testing.T) {
	for _, value := range hostileInstallValues {
		objs, err := karpenterControllerTemplate()
		if err != nil {
			t.Fatal(err)
		}
		if err := setKarpenterInstallValues(objs, value, value); err != nil {
			t.Fatalf("set %q: %v", value, err)
		}
		assertOnlyInstallValuesChanged(t, objs, value, value)
	}
}

func TestRenderKarpenterControllerRefusesInvalidValues(t *testing.T) {
	const name, arn = "orama-eks-prod", "arn:aws:iam::123456789012:role/KarpenterController"
	for _, value := range hostileInstallValues {
		if objs, err := renderKarpenterController(value, arn); err == nil || objs != nil {
			t.Errorf("hostile cluster name %q rendered", value)
		}
		if objs, err := renderKarpenterController(name, value); err == nil || objs != nil {
			t.Errorf("hostile role ARN %q rendered", value)
		}
	}
	if _, err := renderKarpenterController("", arn); err == nil {
		t.Error("empty cluster name rendered")
	}
	if _, err := renderKarpenterController(name, ""); err == nil {
		t.Error("empty role ARN rendered")
	}
}

func TestRequireExactObjectsRefusesAnyOtherSet(t *testing.T) {
	extra := &unstructured.Unstructured{}
	extra.SetAPIVersion("rbac.authorization.k8s.io/v1")
	extra.SetKind("ClusterRoleBinding")
	extra.SetName("pwn")
	cases := map[string]func([]*unstructured.Unstructured) []*unstructured.Unstructured{
		"extra":     func(o []*unstructured.Unstructured) []*unstructured.Unstructured { return append(o, extra) },
		"duplicate": func(o []*unstructured.Unstructured) []*unstructured.Unstructured { return append(o, o[0]) },
		"missing":   func(o []*unstructured.Unstructured) []*unstructured.Unstructured { return o[1:] },
		"renamed": func(o []*unstructured.Unstructured) []*unstructured.Unstructured {
			o[0].SetName("karpenter-other")
			return o
		},
		"renamespaced": func(o []*unstructured.Unstructured) []*unstructured.Unstructured {
			o[0].SetNamespace("default")
			return o
		},
	}
	for name, mutate := range cases {
		objs, err := karpenterControllerTemplate()
		if err != nil {
			t.Fatal(err)
		}
		if err := requireExactObjects(mutate(objs), karpenterControllerObjects); err == nil {
			t.Errorf("%s bundle accepted", name)
		}
	}
}

func TestKarpenterCRDBundleIsExactlyTheExpectedCRDs(t *testing.T) {
	objs, err := karpenterCRDObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != len(karpenterCRDNames) {
		t.Fatalf("CRD bundle has %d objects, want %d", len(objs), len(karpenterCRDNames))
	}
}

// The reconciler's patterns must be the CRD's, in the generated CRD and in the chart copy.
func TestKarpenterInstallPatternsMatchCRD(t *testing.T) {
	for _, path := range []string{
		"../../config/crd/bases/orama.cloud_oramazettaclusters.yaml",
		"../../chart/crds/orama.cloud_oramazettaclusters.yaml",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		objs, err := decodeManifests(raw)
		if err != nil || len(objs) != 1 {
			t.Fatalf("decode %s: %v", path, err)
		}
		versions, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "versions")
		for _, rawVersion := range versions {
			props, found, err := unstructured.NestedMap(rawVersion.(map[string]interface{}),
				"schema", "openAPIV3Schema", "properties", "spec", "properties", "capacity", "properties", "karpenter", "properties")
			if err != nil || !found {
				t.Fatalf("%s: no spec.capacity.karpenter schema", path)
			}
			for field, want := range map[string]struct {
				pattern string
				max     int64
			}{
				"installClusterName": {installClusterNamePattern.String(), installClusterNameMaxLength},
				"controllerRoleARN":  {installRoleARNPattern.String(), installRoleARNMaxLength},
			} {
				schema := props[field].(map[string]interface{})
				if schema["pattern"] != want.pattern {
					t.Errorf("%s %s pattern = %v, reconciler uses %s", path, field, schema["pattern"], want.pattern)
				}
				if schema["maxLength"] != want.max {
					t.Errorf("%s %s maxLength = %v, reconciler uses %d", path, field, schema["maxLength"], want.max)
				}
			}
		}
	}
}

// The chart and generated ClusterRoles scope RBAC writes, bind and escalate to exactly the bundle's
// names, and grant none of those verbs without a name.
// TestChartKarpenterRBACMatchesBundle also covers the vendored Pixie bundles (operator#15), the
// other vendored RBAC the operator applies: the scoped names are exactly the two bundles' objects.
func TestChartKarpenterRBACMatchesBundle(t *testing.T) {
	want := map[string][]string{}
	pixie := []karpenterObjectKey{}
	for _, key := range append(append([]pixieObjectKey{}, vizierObjects...), pixieCloudObjects...) {
		pixie = append(pixie, karpenterObjectKey{Kind: key.Kind, Name: key.Name})
	}
	for _, key := range append(append([]karpenterObjectKey{}, karpenterControllerObjects...), pixie...) {
		switch key.Kind {
		case "ClusterRole":
			want["clusterroles"] = append(want["clusterroles"], key.Name)
		case "ClusterRoleBinding":
			want["clusterrolebindings"] = append(want["clusterrolebindings"], key.Name)
		case "Role":
			want["roles"] = append(want["roles"], key.Name)
		case "RoleBinding":
			want["rolebindings"] = append(want["rolebindings"], key.Name)
		}
	}
	want["customresourcedefinitions"] = append([]string(nil), karpenterCRDNames...)
	scoped := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "bind": true, "escalate": true, "*": true}

	for _, path := range []string{"../../chart/templates/rbac.yaml", "../../config/rbac/role.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		objs, err := decodeManifests([]byte(strings.Join(kept, "\n")))
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		got := map[string]map[string]bool{}
		for _, obj := range objs {
			if obj.GetKind() != "ClusterRole" {
				continue
			}
			var role rbacv1.ClusterRole
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role); err != nil {
				t.Fatal(err)
			}
			for _, rule := range role.Rules {
				for _, resource := range rule.Resources {
					if _, tracked := want[resource]; !tracked {
						continue
					}
					for _, verb := range rule.Verbs {
						if !scoped[verb] {
							continue
						}
						if len(rule.ResourceNames) == 0 {
							t.Errorf("%s grants %s on %s without resourceNames", path, verb, resource)
						}
						if got[resource] == nil {
							got[resource] = map[string]bool{}
						}
						for _, name := range rule.ResourceNames {
							got[resource][name] = true
						}
					}
				}
			}
		}
		for resource, names := range want {
			var gotNames []string
			for name := range got[resource] {
				gotNames = append(gotNames, name)
			}
			sort.Strings(gotNames)
			wantNames := append([]string(nil), names...)
			sort.Strings(wantNames)
			if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
				t.Errorf("%s scopes %s writes to %v, the bundle has %v", path, resource, gotNames, wantNames)
			}
		}
	}
}

// A CR stored before the CRD pattern existed reaches the reconciler: it is refused with a condition, on
// any cloud, before the finalizer or any object is written.
func TestReconcileKarpenterInstallRefusesInvalidSpec(t *testing.T) {
	scheme := newLakeScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kc := range []oramav1alpha1.KarpenterCapacity{
		{Install: true, InstallClusterName: hostileInstallValues[0], ControllerRoleARN: "arn:aws:iam::123456789012:role/KarpenterController"},
		{Install: true, InstallClusterName: "orama-eks-prod", ControllerRoleARN: hostileInstallValues[1]},
	} {
		z := &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "zetta", Namespace: "tenant"},
			Spec:       oramav1alpha1.OramaZettaClusterSpec{Capacity: &oramav1alpha1.OramaZettaCapacity{Karpenter: &kc}},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0123"}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z, node).Build()
		r := &OramaZettaClusterReconciler{Client: c, Scheme: scheme}
		cond, err := r.reconcileKarpenterInstall(context.Background(), z)
		if err != nil {
			t.Fatal(err)
		}
		if cond == nil || cond.Type != installConditionType || cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidInstallSpec" {
			t.Fatalf("condition = %+v, want KarpenterInstalled=False InvalidInstallSpec", cond)
		}
		if controllerutil.ContainsFinalizer(z, installFinalizer) {
			t.Error("install finalizer added for a refused spec")
		}
		var bindings rbacv1.ClusterRoleBindingList
		if err := c.List(context.Background(), &bindings); err != nil && !apimeta.IsNoMatchError(err) {
			t.Fatal(err)
		}
		var accounts corev1.ServiceAccountList
		if err := c.List(context.Background(), &accounts, client.InNamespace(karpenterNamespace)); err != nil {
			t.Fatal(err)
		}
		if len(bindings.Items) != 0 || len(accounts.Items) != 0 {
			t.Errorf("refused spec still wrote objects: %d bindings, %d service accounts", len(bindings.Items), len(accounts.Items))
		}
	}
}

// operator#280: only a CR in the operator's namespace may own the cluster-wide Karpenter install. A
// tenant's CR is refused before the finalizer or any object is written, and so is every CR when the
// operator's namespace is unknown.
func TestReconcileKarpenterInstallRefusesCRsOutsideTheOperatorNamespace(t *testing.T) {
	scheme := newLakeScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kc := oramav1alpha1.KarpenterCapacity{Install: true, InstallClusterName: "orama-eks-prod", ControllerRoleARN: "arn:aws:iam::210987654321:role/TenantRole"}
	for _, operatorNamespace := range []string{"orama-system", ""} {
		z := &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "zetta", Namespace: "prod-ws-0123456789ab"},
			Spec:       oramav1alpha1.OramaZettaClusterSpec{Capacity: &oramav1alpha1.OramaZettaCapacity{Karpenter: &kc}},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0123"}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z, node).Build()
		r := &OramaZettaClusterReconciler{Client: c, Scheme: scheme, OperatorNamespace: operatorNamespace}
		cond, err := r.reconcileKarpenterInstall(context.Background(), z)
		if err != nil {
			t.Fatal(err)
		}
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InstallNamespaceNotAllowed" {
			t.Fatalf("operator namespace %q: condition = %+v, want KarpenterInstalled=False InstallNamespaceNotAllowed", operatorNamespace, cond)
		}
		if controllerutil.ContainsFinalizer(z, installFinalizer) {
			t.Errorf("operator namespace %q: install finalizer added for a refused CR", operatorNamespace)
		}
		var accounts corev1.ServiceAccountList
		if err := c.List(context.Background(), &accounts, client.InNamespace(karpenterNamespace)); err != nil {
			t.Fatal(err)
		}
		if len(accounts.Items) != 0 {
			t.Errorf("operator namespace %q: refused CR still wrote %d service accounts", operatorNamespace, len(accounts.Items))
		}
	}
}

// operator#280: two CRs in the operator's namespace that disagree are not last-writer-wins: the older
// one holds and the newer is refused; one that agrees is not.
func TestReconcileKarpenterInstallRefusesAConflictingSecondCR(t *testing.T) {
	scheme := newLakeScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cr := func(name string, created time.Time, roleARN string) *oramav1alpha1.OramaZettaCluster {
		return &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system", UID: types.UID(name), CreationTimestamp: metav1.NewTime(created)},
			Spec: oramav1alpha1.OramaZettaClusterSpec{Capacity: &oramav1alpha1.OramaZettaCapacity{Karpenter: &oramav1alpha1.KarpenterCapacity{
				Install: true, InstallClusterName: "orama-eks-prod", ControllerRoleARN: roleARN,
			}}},
		}
	}
	now := time.Now().Truncate(time.Second)
	first := cr("first", now.Add(-time.Hour), "arn:aws:iam::123456789012:role/KarpenterController")
	agreeing := cr("agreeing", now, "arn:aws:iam::123456789012:role/KarpenterController")
	conflicting := cr("conflicting", now, "arn:aws:iam::210987654321:role/Other")
	tenant := cr("tenant", now.Add(-2*time.Hour), "arn:aws:iam::210987654321:role/Tenant")
	tenant.Namespace = "prod-ws-0123456789ab"
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0123"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, agreeing, conflicting, tenant, node).Build()
	r := &OramaZettaClusterReconciler{Client: c, Scheme: scheme, OperatorNamespace: "orama-system"}

	cond, err := r.reconcileKarpenterInstall(context.Background(), conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if cond == nil || cond.Reason != "ConflictingInstallSpec" || !strings.Contains(cond.Message, "orama-system/first") {
		t.Fatalf("conflicting CR: condition = %+v, want ConflictingInstallSpec naming orama-system/first", cond)
	}
	if controllerutil.ContainsFinalizer(conflicting, installFinalizer) {
		t.Error("install finalizer added for a conflicting CR")
	}
	for _, z := range []*oramav1alpha1.OramaZettaCluster{first, agreeing} {
		owner, err := r.conflictingInstallOwner(context.Background(), z)
		if err != nil {
			t.Fatal(err)
		}
		if owner != nil {
			t.Errorf("%s: conflicting owner = %s, want none (the tenant CR is older but not allowed)", z.Name, owner.Name)
		}
	}
	// The tenant's CR does not keep the shared install alive.
	others, err := r.otherClustersRequestInstall(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if !others {
		t.Error("the agreeing CR in the operator namespace should count as another requester")
	}
	onlyTenant := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, tenant).Build()
	r.Client = onlyTenant
	if others, err := r.otherClustersRequestInstall(context.Background(), first); err != nil || others {
		t.Errorf("tenant CR counted as a requester of the shared install (others=%v, err=%v)", others, err)
	}
}
