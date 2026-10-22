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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/namespaceaccess"
	ctrl "sigs.k8s.io/controller-runtime"
)

func installManagerAdmission(t *testing.T, admin client.Client, operator client.Client) {
	t.Helper()
	raw, err := exec.Command("helm", "template", "orama-operator", "../../chart", "--namespace", "orama-system", "--kube-version", "1.35.0", "--show-only", "templates/manager-admission.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render admission: %v: %s", err, raw)
	}
	objects, err := decodeManifests(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if err := admin.Create(context.Background(), obj); err != nil {
			t.Fatalf("install %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	// Dry-run the manager request until the API server has observed the policies and bindings.
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := operator.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-denied-proof", Labels: map[string]string{namespaceaccess.ManagedLabel: namespaceaccess.ManagedValue}}}, client.DryRunAll)
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "reserved namespaces") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager admission never became active: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// RoleBinding grants reach the API server's RBAC informer asynchronously. A real
// controller retries reconciliation; bound that same retry in the positive API fixture.
func managerWriteAfterRolePropagation(t *testing.T, write func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := write()
		if err == nil {
			return
		}
		if !apierrors.IsForbidden(err) || time.Now().After(deadline) {
			t.Fatalf("write after RoleBinding propagation: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManagerNamespaceAuthorization(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS required")
	}
	server := &envtest.Environment{BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	server.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC").Set("enable-admission-plugins", "ValidatingAdmissionPolicy")
	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := newLakeScheme(t)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, name := range []string{"orama-system", "managed", "foreign"} {
		labels := map[string]string{}
		if name == "managed" {
			labels[namespaceaccess.ManagedLabel] = namespaceaccess.ManagedValue
		}
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := exec.Command("helm", "template", "orama-operator", "../../chart", "--namespace", "orama-system", "--kube-version", "1.35.0", "--show-only", "templates/rbac.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, raw)
	}
	objects, err := decodeManifests(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	// Impersonation here uses the envtest admin certificate, while every authorization and
	// admission check sees only the deployed manager identity.
	operatorConfig := rest.CopyConfig(config)
	operatorConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:orama-system:orama-operator", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:orama-system", "system:authenticated"}}
	operator, err := client.New(operatorConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	installManagerAdmission(t, admin, operator)
	manager := namespaceaccess.Wrap(operator, operator, "orama-system", "orama-operator")
	t.Run("authorization has no kube-system workload authority", func(t *testing.T) {
		cs, err := kubernetes.NewForConfig(operatorConfig)
		if err != nil {
			t.Fatal(err)
		}
		for _, attrs := range []authorizationv1.ResourceAttributes{
			{Namespace: "kube-system", Verb: "create", Group: "apps", Resource: "daemonsets"},
			{Namespace: "foreign", Verb: "patch", Group: "apps", Resource: "deployments", Name: "victim"},
			{Namespace: "foreign", Verb: "update", Resource: "secrets", Name: "victim"},
			{Namespace: "kube-system", Verb: "create", Resource: "pods"},
			{Namespace: "kube-system", Verb: "impersonate", Resource: "serviceaccounts", Name: "karpenter"},
		} {
			review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
			if err != nil || review.Status.Allowed {
				t.Errorf("can-i %+v = %+v, %v", attrs, review, err)
			}
		}
	})
	t.Run("managed namespace bootstrap and repair", func(t *testing.T) {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "proof", Namespace: "managed"}}
		managerWriteAfterRolePropagation(t, func() error { return manager.Create(ctx, sec) })
		if err := admin.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: namespaceaccess.RoleName, Namespace: "managed"}}); err != nil {
			t.Fatal(err)
		}
		sec.Labels = map[string]string{"rotation": "second"}
		managerWriteAfterRolePropagation(t, func() error { return manager.Update(ctx, sec) })
		var binding rbacv1.RoleBinding
		if err := admin.Get(ctx, client.ObjectKey{Name: namespaceaccess.RoleName, Namespace: "managed"}, &binding); err != nil {
			t.Fatal(err)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "orama-operator" {
			t.Fatal(binding.Subjects)
		}
	})
	t.Run("Lake and Cloud v1 workload reconcilers", func(t *testing.T) {
		lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "authorization-proof"}, Spec: oramav1alpha1.LakeSpec{Namespace: "managed", Image: "oramasearch/lake:test"}}
		if err := manager.Create(ctx, lake); err != nil {
			t.Fatal(err)
		}
		r := &LakeReconciler{Client: manager, APIReader: operator, Scheme: scheme}
		if _, err := r.reconcileLakeDeployment(ctx, lake, "managed", "lake-authorization-proof", lake.Spec.Image, "proof-store", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, lakeTemplateLabels(lake), 1, lakeDefaultPort, lakeSealingMaterial{}, lakeClerkState{}, lakePaymentsState{}); err != nil {
			t.Fatalf("Lake workload: %v", err)
		}
		oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "authorization-proof", Namespace: "managed"}, Spec: oramav1alpha1.OramaCloudV1Spec{IDP: oramav1alpha1.IDPSpec{Provider: "none"}, Region: oramav1alpha1.RegionSpec{Name: "test", Role: "primary"}}}
		if err := manager.Create(ctx, oc); err != nil {
			t.Fatal(err)
		}
		oc.Spec.Components.Cache.Enabled = true
		cloud := &OramaCloudV1Reconciler{Client: manager, APIReader: operator, Scheme: scheme}
		if err := cloud.reconcileBootstrap(ctx, oc); err != nil {
			t.Fatalf("Cloud v1 bootstrap: %v", err)
		}
		if err := cloud.reconcileCache(ctx, oc); err != nil {
			t.Fatalf("Cloud v1 StatefulSet: %v", err)
		}
		// Revocation leaves the existing workload intact and publishes the prerequisite as status.
		var foreign corev1.Namespace
		if err := operator.Get(ctx, client.ObjectKey{Name: "foreign"}, &foreign); err != nil {
			t.Fatal(err)
		}
		blocked := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled"}, Spec: oramav1alpha1.LakeSpec{Namespace: "foreign", Image: "oramasearch/lake:test"}}
		if err := manager.Create(ctx, blocked); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: blocked.Name}}); err != nil {
				t.Fatalf("blocked Lake: %v", err)
			}
		}
		if err := operator.Get(ctx, client.ObjectKey{Name: blocked.Name}, blocked); err != nil {
			t.Fatal(err)
		}
		if blocked.Status.Phase != "Degraded" {
			t.Fatalf("blocked Lake phase: %s", blocked.Status.Phase)
		}
	})
	t.Run("sets and leaf namespace bootstrap", func(t *testing.T) {
		for _, kind := range []string{"Lake", "OramaCloudV1"} {
			namespaced := ""
			if kind == "OramaCloudV1" {
				namespaced = "new-v1-member"
			}
			namespace := "new-lake-member"
			if namespaced != "" {
				namespace = namespaced
			}
			spec := map[string]interface{}{"image": "oramasearch/lake:test", "namespace": namespace}
			if namespaced != "" {
				spec = map[string]interface{}{"idp": map[string]interface{}{"provider": "none"}, "region": map[string]interface{}{"name": "test", "role": "primary"}}
			}

			managerWriteAfterRolePropagation(t, func() error {
				return applyNamespaceAndPullSecret(ctx, manager, namespace, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "member-pull"}, Type: corev1.SecretTypeOpaque})
			})
			var ns corev1.Namespace
			if err := operator.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
				t.Fatal(err)
			}
			if ns.Labels[namespaceaccess.ManagedLabel] != namespaceaccess.ManagedValue {
				t.Fatalf("%s member lost namespace opt-in", kind)
			}
			definition := lakeSetKind
			if namespaced != "" {
				definition = oramaCloudV1SetKind
			}
			request := memberRequest{Kind: definition.instance, CRD: definition.crd, Name: namespace, Namespace: namespace, ObjectNamespace: namespaced, Owner: "proof", Spec: spec}
			if state, err := applyMemberWith(ctx, manager, request); err != nil || state.Object == nil {
				t.Fatalf("%s set member: %v", kind, err)
			}
		}
	})

	t.Run("foreign namespace cannot be adopted or bound", func(t *testing.T) {
		var ns corev1.Namespace
		if err := admin.Get(ctx, client.ObjectKey{Name: "foreign"}, &ns); err != nil {
			t.Fatal(err)
		}
		ns.Labels = map[string]string{namespaceaccess.ManagedLabel: namespaceaccess.ManagedValue}
		if err := operator.Update(ctx, &ns); !apierrors.IsForbidden(err) {
			t.Fatalf("namespace adoption: %v", err)
		}
		indirect := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "indirect", Namespace: "foreign"}, Spec: oramav1alpha1.OramaCloudV1Spec{IDP: oramav1alpha1.IDPSpec{Provider: "none"}, Region: oramav1alpha1.RegionSpec{Name: "test", Role: "primary"}}}
		if err := operator.Create(ctx, indirect); !apierrors.IsForbidden(err) {
			t.Fatalf("foreign custom-resource workload path: %v", err)
		}

		binding := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: namespaceaccess.RoleName, Namespace: "foreign"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: namespaceaccess.RoleName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "orama-operator", Namespace: "orama-system"}}}
		if err := operator.Patch(ctx, binding, client.Apply, client.FieldOwner("proof")); !apierrors.IsForbidden(err) {
			t.Fatalf("foreign namespace grant: %v", err)
		}
		if err := manager.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "denied", Namespace: "foreign"}}); err == nil || !strings.Contains(err.Error(), "committed namespace manifest") {
			t.Fatalf("namespace explanation: %v", err)
		}
	})
	t.Run("namespace revocation denies writes despite old binding", func(t *testing.T) {
		var ns corev1.Namespace
		if err := admin.Get(ctx, client.ObjectKey{Name: "managed"}, &ns); err != nil {
			t.Fatal(err)
		}
		delete(ns.Labels, namespaceaccess.ManagedLabel)
		if err := admin.Update(ctx, &ns); err != nil {
			t.Fatal(err)
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "revoked", Namespace: "managed"}}
		if err := manager.Create(ctx, sec); err == nil {
			t.Fatal("manager's uncached namespace check permitted a revoked namespace")
		}
		// Native admission observes Namespace updates through the API server's informer.
		// Wait for that revision before asserting direct bearer requests are denied too.
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := operator.Create(ctx, sec, client.DryRunAll)
			if apierrors.IsForbidden(err) {
				break
			}
			if err != nil || time.Now().After(deadline) {
				t.Fatalf("admission did not observe namespace revocation: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := operator.Create(ctx, sec); !apierrors.IsForbidden(err) {
			t.Fatalf("revoked namespace write: %v", err)
		}
	})
	t.Run("named Karpenter roles cannot escalate or rebind", func(t *testing.T) {
		role := &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metav1.ObjectMeta{Name: "karpenter-core"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}}
		if err := operator.Patch(ctx, role, client.Apply, client.FieldOwner("proof")); !apierrors.IsForbidden(err) {
			t.Fatalf("wildcard Karpenter role: %v", err)
		}
		binding := &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "karpenter-core"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "karpenter-core"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "orama-operator", Namespace: "orama-system"}}}
		if err := operator.Patch(ctx, binding, client.Apply, client.FieldOwner("proof")); !apierrors.IsForbidden(err) {
			t.Fatalf("rebind Karpenter: %v", err)
		}
	})
	t.Logf("manager RBAC and admission verified with %d chart objects", len(objects))
}
