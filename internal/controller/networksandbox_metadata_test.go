// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func metadataDenyForTest(namespace string) *unstructured.Unstructured {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	policy.SetName(networkSandboxMetadataDenyName)
	policy.SetNamespace(namespace)
	return policy
}

func assertMetadataDeny(t *testing.T, kube client.Client, sandbox *oramav1alpha1.NetworkSandbox) *unstructured.Unstructured {
	t.Helper()
	policy := metadataDenyForTest(sandbox.Namespace)
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	selector := sandbox.Spec.PodSelector.DeepCopy()
	selector.MatchLabels = map[string]string{}
	for key, value := range sandbox.Spec.PodSelector.MatchLabels {
		selector.MatchLabels["k8s:"+key] = value
	}
	for i := range selector.MatchExpressions {
		selector.MatchExpressions[i].Key = "k8s:" + selector.MatchExpressions[i].Key
	}
	endpointSelector, err := runtime.DefaultUnstructuredConverter.ToUnstructured(selector)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{
		"endpointSelector": endpointSelector,
		"egressDeny": []interface{}{
			map[string]interface{}{
				"toCIDR": []interface{}{"169.254.169.254/32", "168.63.129.16/32", "fd00:ec2::254/128"},
			},
		},
	}
	if !reflect.DeepEqual(policy.Object["spec"], want) {
		t.Fatalf("metadata deny spec = %#v, want %#v", policy.Object["spec"], want)
	}
	if _, found := policy.Object["specs"]; found {
		t.Fatal("metadata deny has unexpected additional rules")
	}
	owner := metav1.GetControllerOf(policy)
	if owner == nil || owner.APIVersion != oramav1alpha1.GroupVersion.String() || owner.Kind != "NetworkSandbox" ||
		owner.Name != sandbox.Name || owner.UID != sandbox.UID || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		t.Fatalf("metadata deny lacks the sandbox garbage-collection owner: %+v", owner)
	}
	if !reflect.DeepEqual(policy.GetLabels(), sandbox.Spec.PolicyLabels) || policy.GetAnnotations()[networkSandboxCommentAnnotation] == "" {
		t.Fatalf("metadata deny labels or comment = %+v", policy.Object["metadata"])
	}
	return policy
}

func TestNetworkSandboxMetadataDenyForEveryProfile(t *testing.T) {
	for _, profile := range []string{"Lake", "workspace", "OramaCloudV1"} {
		t.Run(profile, func(t *testing.T) {
			ctx := context.Background()
			scheme := newLakeScheme(t)
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandboxDiscoveryObjects()...).Build()
			lake := testWorkspaceLake()
			namespace := lakeNamespace(lake)
			var owned, foreign map[string]string
			switch profile {
			case "Lake":
				r := &LakeReconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
				if err := r.reconcileLakeNetworkSandbox(ctx, lake, namespace, nil); err != nil {
					t.Fatal(err)
				}
				owned = map[string]string{lakeInstanceLabel: lake.Name}
				foreign = map[string]string{lakeInstanceLabel: "another-lake"}
			case "workspace":
				namespace = testWorkspaceName
				r := &LakeWorkspacesReconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
				if err := r.reconcileWorkspaceNetworkPolicies(ctx, lake, namespace, nil, true); err != nil {
					t.Fatal(err)
				}
			case "OramaCloudV1":
				stack := newOCV1(nil)
				namespace = stack.Namespace
				r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
				if err := r.reconcileNetworkSandbox(ctx, stack); err != nil {
					t.Fatal(err)
				}
				owned = labelsFor(stack, "reader")
				other := newOCV1(nil)
				other.Name = "another-stack"
				foreign = labelsFor(other, "reader")
			}
			var sandbox oramav1alpha1.NetworkSandbox
			if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
				t.Fatal(err)
			}
			assertMetadataDeny(t, kube, &sandbox)
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(ciliumNetworkPolicyGVK.GroupVersion().WithKind("CiliumNetworkPolicyList"))
			if err := kube.List(ctx, list); err != nil || len(list.Items) != 1 || list.Items[0].GetNamespace() != namespace {
				t.Fatalf("expected one namespaced metadata deny, got %v, error %v", list.Items, err)
			}
			if !selectorMatches(t, sandbox.Spec.PodSelector, owned) {
				t.Fatal("metadata deny misses owner pods")
			}
			if profile == "workspace" {
				if !reflect.DeepEqual(sandbox.Spec.PodSelector, metav1.LabelSelector{}) {
					t.Fatal("workspace deny must cover every pod in its owned namespace")
				}
				return
			}
			assertSandboxExcludes(t, kube, namespace, nil, foreign, map[string]string{"app.kubernetes.io/name": "orama-operator"})
		})
	}
}

func metadataSandboxForTest() *oramav1alpha1.NetworkSandbox {
	return &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: "isolated", UID: "sandbox-uid", Generation: 1},
		Spec: oramav1alpha1.NetworkSandboxSpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"owner": "ours"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "managed", Operator: metav1.LabelSelectorOpExists},
				},
			},
			PolicyLabels: map[string]string{"owner": "ours"},
		},
	}
}

func reconcileMetadataSandbox(t *testing.T, kube client.Client, sandbox *oramav1alpha1.NetworkSandbox) {
	t.Helper()
	r := &NetworkSandboxReconciler{Client: kube, Scheme: kube.Scheme()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.Status.NetworkPolicies != 1 || sandbox.Status.CiliumNetworkPolicies != 1 ||
		!apimeta.IsStatusConditionTrue(sandbox.Status.Conditions, "Ready") || sandbox.Status.ObservedGeneration != sandbox.Generation {
		t.Fatalf("unexpected sandbox status: %+v", sandbox.Status)
	}
}

func TestNetworkSandboxMetadataDenyLifecycle(t *testing.T) {
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(sandbox).WithStatusSubresource(sandbox).Build()
	reconcileMetadataSandbox(t, kube, sandbox)
	policy := assertMetadataDeny(t, kube, sandbox)
	stableSandbox := sandbox.DeepCopy()
	reconcileMetadataSandbox(t, kube, sandbox)
	stablePolicy := assertMetadataDeny(t, kube, sandbox)
	if stablePolicy.GetResourceVersion() != policy.GetResourceVersion() || !reflect.DeepEqual(sandbox, stableSandbox) {
		t.Fatal("steady reconciliation rewrote policy or status")
	}
	policy.Object["spec"] = map[string]interface{}{"endpointSelector": map[string]interface{}{}}
	policy.Object["specs"] = []interface{}{map[string]interface{}{"endpointSelector": map[string]interface{}{}}}
	policy.SetLabels(nil)
	policy.SetAnnotations(nil)
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	reconcileMetadataSandbox(t, kube, sandbox)
	policy = assertMetadataDeny(t, kube, sandbox)
	sandbox.Spec.PodSelector.MatchLabels["owner"] = "changed"
	sandbox.Spec.PolicyLabels["owner"] = "changed"
	sandbox.Generation++
	if err := kube.Update(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	reconcileMetadataSandbox(t, kube, sandbox)
	policy = assertMetadataDeny(t, kube, sandbox)
	if err := kube.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	reconcileMetadataSandbox(t, kube, sandbox)
	assertMetadataDeny(t, kube, sandbox)
}

func TestNetworkSandboxMetadataDenyDeletionUsesOwnerGarbageCollection(t *testing.T) {
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	sandbox.Finalizers = []string{"test.orama.cloud/hold"}
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(sandbox).WithStatusSubresource(sandbox).Build()
	reconcileMetadataSandbox(t, kube, sandbox)
	policy := assertMetadataDeny(t, kube, sandbox)
	if len(policy.GetFinalizers()) != 0 {
		t.Fatal("metadata deny must not block garbage collection")
	}
	if err := kube.Delete(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	// The fake client has no garbage collector. The exact controller owner reference is checked
	// above; finish dependent deletion here and verify the controller does not recreate it.
	if err := kube.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	r := &NetworkSandboxReconciler{Client: kube, Scheme: kube.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}
	for _, state := range []string{"terminating", "deleted"} {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); !apierrors.IsNotFound(err) {
			t.Fatalf("%s sandbox recreated its metadata deny: %v", state, err)
		}
		if state == "terminating" {
			if err := kube.Get(ctx, req.NamespacedName, sandbox); err != nil {
				t.Fatal(err)
			}
			sandbox.Finalizers = nil
			if err := kube.Update(ctx, sandbox); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestNetworkSandboxMetadataDenyCannotBeOverridden(t *testing.T) {
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	sandbox.Spec.CiliumPolicies = []oramav1alpha1.NetworkSandboxCiliumPolicy{{
		Name: networkSandboxMetadataDenyName, Comment: "try to override the deny", Spec: apiextensionsv1.JSON{Raw: []byte(`{"endpointSelector":{}}`)},
	}}
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(sandbox).WithStatusSubresource(sandbox).Build()
	r := &NetworkSandboxReconciler{Client: kube, Scheme: kube.Scheme()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("mandatory deny override was not refused: %v", err)
	}
	assertMetadataDeny(t, kube, sandbox)
}

func TestNetworkSandboxMetadataDenyRefusesForeignOwner(t *testing.T) {
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	foreign := metadataSandboxForTest()
	foreign.Name = "another-sandbox"
	foreign.UID = "another-uid"
	scheme := newLakeScheme(t)
	policy := metadataDenyForTest(sandbox.Namespace)
	if err := controllerutil.SetControllerReference(foreign, policy, scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, policy).Build()
	if _, _, err := reconcileNetworkSandboxPolicies(ctx, kube, scheme, sandbox); err == nil || !strings.Contains(err.Error(), "controlled by") {
		t.Fatalf("foreign policy ownership was not refused: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(policy, foreign) || policy.Object["spec"] != nil {
		t.Fatal("foreign policy was mutated")
	}
}

func TestNetworkSandboxMetadataDenyRequiredBeforeAllowances(t *testing.T) {
	ctx := context.Background()
	sandbox := metadataSandboxForTest()
	sandbox.Spec.Policies = []oramav1alpha1.NetworkSandboxPolicy{{Name: "allow-something", Comment: "test allowance"}}
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(sandbox).WithStatusSubresource(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, kube client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind() == ciliumNetworkPolicyGVK {
				return &apimeta.NoKindMatchError{GroupKind: ciliumNetworkPolicyGVK.GroupKind()}
			}
			return kube.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &NetworkSandboxReconciler{Client: kube, Scheme: kube.Scheme()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}); err == nil {
		t.Fatal("sandbox accepted a missing Cilium API")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(sandbox.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "RenderFailed" ||
		sandbox.Status.CiliumNetworkPolicies != 0 || sandbox.Status.NetworkPolicies != 1 {
		t.Fatalf("missing Cilium API status = %+v", sandbox.Status)
	}
	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies); err != nil || len(policies.Items) != 1 || policies.Items[0].Name != networkSandboxDefaultDenyName {
		t.Fatalf("allowances rendered without metadata deny: %v, error %v", policies.Items, err)
	}
}
