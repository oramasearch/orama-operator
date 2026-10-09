/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestKarpenterInstallEnvtest runs a real API server with RBAC authorization (operator#79):
//
//   - the OramaZettaCluster CRD schema refuses an injected, overlong or malformed cluster name or role ARN;
//   - the operator's ServiceAccount, holding exactly the chart's ClusterRole, can install and remove the
//     Karpenter bundle — the name-scoped bind and escalate are enough;
//   - the same identity cannot create or bind any other role, binding or CRD.
func TestKarpenterInstallEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the Karpenter install envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := newLakeScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("crd_schema_refuses_invalid_values", func(t *testing.T) {
		testKarpenterInstallCRDSchema(ctx, t, admin)
	})
	t.Run("operator_rbac_is_scoped_to_the_bundle", func(t *testing.T) {
		testKarpenterInstallRBAC(ctx, t, admin, config, scheme)
	})
}

func testKarpenterInstallCRDSchema(ctx context.Context, t *testing.T, admin client.Client) {
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}); err != nil {
		t.Fatal(err)
	}
	const validName, validARN = "orama-eks-prod", "arn:aws:iam::123456789012:role/KarpenterController"
	newCR := func(name, clusterName, roleARN string) *oramav1alpha1.OramaZettaCluster {
		return &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"},
			Spec: oramav1alpha1.OramaZettaClusterSpec{Capacity: &oramav1alpha1.OramaZettaCapacity{
				Karpenter: &oramav1alpha1.KarpenterCapacity{Install: true, InstallClusterName: clusterName, ControllerRoleARN: roleARN},
			}},
		}
	}
	refused := map[string]*oramav1alpha1.OramaZettaCluster{
		"injected-name":  newCR("injected-name", hostileInstallValues[0], validARN),
		"quoted-name":    newCR("quoted-name", "orama\"", validARN),
		"newline-name":   newCR("newline-name", "orama\n---", validARN),
		"overlong-name":  newCR("overlong-name", strings.Repeat("a", 101), validARN),
		"injected-arn":   newCR("injected-arn", validName, hostileInstallValues[1]),
		"user-arn":       newCR("user-arn", validName, "arn:aws:iam::123456789012:user/KarpenterController"),
		"short-account":  newCR("short-account", validName, "arn:aws:iam::12345:role/KarpenterController"),
		"overlong-arn":   newCR("overlong-arn", validName, "arn:aws:iam::123456789012:role/"+strings.Repeat("r", 513)),
		"not-an-arn-arn": newCR("not-an-arn-arn", validName, "KarpenterController"),
	}
	for name, cr := range refused {
		err := admin.Create(ctx, cr)
		if !apierrors.IsInvalid(err) {
			t.Errorf("%s: create = %v, want Invalid from the CRD schema", name, err)
			continue
		}
		t.Logf("%s: refused by the API server: %v", name, firstLine(err.Error()))
	}
	for _, cr := range []*oramav1alpha1.OramaZettaCluster{
		newCR("valid", validName, validARN),
		newCR("valid-gov", "a_b-9", "arn:aws-us-gov:iam::123456789012:role/path/Karpenter+=,.@_-"),
		newCR("unset", "", ""),
	} {
		if err := admin.Create(ctx, cr); err != nil {
			t.Errorf("%s: valid CR refused: %v", cr.Name, err)
		}
	}
}

func testKarpenterInstallRBAC(ctx context.Context, t *testing.T, admin client.Client, config *rest.Config, scheme *runtime.Scheme) {
	// The operator identity is the chart's ServiceAccount with the chart's ClusterRole, nothing more.
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}}); err != nil {
		t.Fatal(err)
	}
	chart, err := exec.Command("helm", "template", "orama-operator", "../../chart", "--namespace", "orama-system", "--show-only", "templates/rbac.yaml").Output()
	if err != nil {
		t.Fatalf("render operator RBAC: %v", err)
	}
	objs, err := decodeManifests(chart)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("install chart %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	token, err := clientset.CoreV1().ServiceAccounts("orama-system").CreateToken(ctx, "orama-operator", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	operatorConfig := rest.AnonymousClientConfig(config)
	operatorConfig.BearerToken = token.Status.Token
	operator, err := client.New(operatorConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	r := &OramaZettaClusterReconciler{Client: operator, Scheme: scheme}

	// Install, twice (create, then the steady-state apply), as the operator. Each pass renders afresh,
	// as each reconcile does: an apply decodes the server's response into the object it sent.
	const clusterName, roleARN = "orama-eks-prod", "arn:aws:iam::123456789012:role/KarpenterController"
	for pass := 0; pass < 2; pass++ {
		crds, err := karpenterCRDObjects()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.applyKarpenterObjects(ctx, crds); err != nil {
			t.Fatalf("pass %d: apply Karpenter CRDs as the operator: %v", pass, err)
		}
		bundle, err := renderKarpenterController(clusterName, roleARN)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.applyKarpenterObjects(ctx, bundle); err != nil {
			t.Fatalf("pass %d: apply Karpenter controller as the operator: %v", pass, err)
		}
	}
	var account corev1.ServiceAccount
	if err := admin.Get(ctx, client.ObjectKey{Namespace: karpenterNamespace, Name: karpenterControllerName}, &account); err != nil {
		t.Fatal(err)
	}
	if account.Annotations[installRoleARNAnnotation] != roleARN {
		t.Errorf("installed ServiceAccount role ARN = %q", account.Annotations[installRoleARNAnnotation])
	}
	t.Log("operator identity installed the Karpenter CRDs and controller bundle with name-scoped bind/escalate")

	// Anything outside the bundle is forbidden to the same identity.
	adminRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"}
	everyone := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:authenticated"}}
	evilRole := &unstructured.Unstructured{}
	evilRole.SetAPIVersion("rbac.authorization.k8s.io/v1")
	evilRole.SetKind("ClusterRole")
	evilRole.SetName("karpenter-evil")
	_ = unstructured.SetNestedSlice(evilRole.Object, []interface{}{map[string]interface{}{
		"apiGroups": []interface{}{"*"}, "resources": []interface{}{"*"}, "verbs": []interface{}{"*"},
	}}, "rules")
	evilCRD := &unstructured.Unstructured{}
	evilCRD.SetAPIVersion("apiextensions.k8s.io/v1")
	evilCRD.SetKind("CustomResourceDefinition")
	evilCRD.SetName("widgets.example.com")
	_ = unstructured.SetNestedMap(evilCRD.Object, map[string]interface{}{
		"group": "example.com", "scope": "Namespaced",
		"names": map[string]interface{}{"plural": "widgets", "singular": "widget", "kind": "Widget", "listKind": "WidgetList"},
		"versions": []interface{}{map[string]interface{}{
			"name": "v1", "served": true, "storage": true,
			"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{"type": "object"}},
		}},
	}, "spec")
	for name, attempt := range map[string]func() error{
		"ClusterRoleBinding pwn -> cluster-admin": func() error {
			return operator.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "pwn"}, RoleRef: adminRef, Subjects: everyone})
		},
		"RoleBinding kube-system/pwn -> cluster-admin": func() error {
			return operator.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "pwn", Namespace: "kube-system"}, RoleRef: adminRef, Subjects: everyone})
		},
		// An allowed binding name still cannot bind a role outside the bundle.
		"RoleBinding tenant/karpenter -> cluster-admin": func() error {
			return operator.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "karpenter", Namespace: "tenant"}, RoleRef: adminRef, Subjects: everyone})
		},
		"apply ClusterRole karpenter-evil": func() error {
			return operator.Patch(ctx, evilRole, client.Apply, client.FieldOwner(karpenterFieldOwner), client.ForceOwnership)
		},
		"apply CRD widgets.example.com": func() error {
			return operator.Patch(ctx, evilCRD, client.Apply, client.FieldOwner(karpenterFieldOwner), client.ForceOwnership)
		},
		"delete ClusterRole cluster-admin": func() error {
			return operator.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"}})
		},
	} {
		err := attempt()
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: err = %v, want Forbidden", name, err)
			continue
		}
		t.Logf("%s: forbidden", name)
	}

	// Teardown as the operator.
	if err := r.deleteKarpenterController(ctx); err != nil {
		t.Fatalf("delete Karpenter controller as the operator: %v", err)
	}
	var bindings rbacv1.ClusterRoleBindingList
	if err := admin.List(ctx, &bindings); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings.Items {
		if strings.HasPrefix(binding.Name, "karpenter") || binding.Name == "pwn" {
			t.Errorf("ClusterRoleBinding %s remains after teardown", binding.Name)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
