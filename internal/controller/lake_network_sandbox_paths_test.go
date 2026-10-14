/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeNetworkSandboxServiceConnectionsUseOnlyDeclaredPorts(t *testing.T) {
	namespaces := &metav1.LabelSelector{MatchLabels: map[string]string{workspaceLakeLabel: "ours"}}
	pods := metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "lake"}}
	allowances := lakeWorkspaceServiceAllowances(pods, namespaces)
	want := map[string][]int32{
		"postgres": {5432}, "kafka": {9092}, "clickhouse": {8123, 9000},
		"tidb": {4000}, "cassandra": {9042}, "valkey": {6379},
	}
	if len(allowances) != len(want) {
		t.Fatalf("service allowances = %d, want %d", len(allowances), len(want))
	}
	sandbox := &oramav1alpha1.NetworkSandbox{Spec: oramav1alpha1.NetworkSandboxSpec{Policies: allowances}}
	assertJustifiedNarrowAllowances(t, sandbox)
	for kind, ports := range want {
		policy := allowanceByName(t, sandbox, "allow-lake-to-workspace-"+kind)
		if !reflect.DeepEqual(policy.Spec.PodSelector, pods) || len(policy.Spec.Egress) != 1 {
			t.Fatalf("%s selects unexpected clients or rules: %+v", kind, policy.Spec)
		}
		rule := policy.Spec.Egress[0]
		if !reflect.DeepEqual(policyPorts(t, rule.Ports), ports) || len(rule.To) != 1 {
			t.Fatalf("%s targets unexpected ports/peers: %+v", kind, rule)
		}
		peer := rule.To[0]
		if peer.NamespaceSelector == nil || !reflect.DeepEqual(peer.NamespaceSelector, namespaces) || peer.PodSelector == nil || selectorMatches(t, *peer.PodSelector, nil) {
			t.Fatalf("%s can reach unowned namespaces or unlabelled workloads: %+v", kind, peer)
		}
	}
}

func TestLakeNetworkSandboxBuildIngressIsOneWayAndOwnerScoped(t *testing.T) {
	lake := serviceTestLake()
	pods := lakePodSelector(lake)
	policies := lakeInternalIngressAllowances(lake, pods, metav1.LabelSelector{}, metav1.LabelSelector{}, metav1.LabelSelector{}, metav1.LabelSelector{})
	sandbox := &oramav1alpha1.NetworkSandbox{Spec: oramav1alpha1.NetworkSandboxSpec{Policies: policies}}
	policy := allowanceByName(t, sandbox, "allow-build-ingress-to-lake")
	if !reflect.DeepEqual(policy.Spec.PodSelector, pods) || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 1 {
		t.Fatalf("build path is not ingress to Lake only: %+v", policy.Spec)
	}
	rule := policy.Spec.Ingress[0]
	if !reflect.DeepEqual(policyPorts(t, rule.Ports), []int32{int32(lakeContainerPort(lake))}) || len(rule.From) != 1 {
		t.Fatalf("unexpected build ingress: %+v", rule)
	}
	peer := rule.From[0]
	if !selectorMatches(t, *peer.NamespaceSelector, buildNamespaceLabels(lake, "space")) || !selectorMatches(t, *peer.PodSelector, buildLabels(lake, &LakeBuild{ID: "build"}, buildTarget{})) {
		t.Fatal("builder cannot call its Lake")
	}
	if selectorMatches(t, *peer.NamespaceSelector, map[string]string{buildLakeLabel: "another"}) || selectorMatches(t, *peer.PodSelector, map[string]string{buildLakeLabel: lake.Name}) {
		t.Fatal("build ingress selects another Lake or a non-builder pod")
	}
}

func TestLakeNetworkSandboxAppBackendsFollowExactOwnershipAndPort(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := serviceTestLake()
	ours := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "ours", Namespace: lakeNamespace(lake)},
		Spec:       oramav1alpha1.LakeAppSpec{LakeRef: lake.Name, Port: 9123},
	}
	foreign := ours.DeepCopy()
	foreign.Name = "foreign"
	foreign.Spec.LakeRef = "another-lake"
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ours, foreign).Build()
	r := &LakeReconciler{Client: kube, Scheme: scheme}
	policies, err := r.lakeAppProxyAllowances(ctx, lake, lakePodSelector(lake))
	if err != nil || len(policies) != 1 || len(policies[0].Spec.Egress) != 1 {
		t.Fatalf("app policies = %v, error %v", policies, err)
	}
	rule := policies[0].Spec.Egress[0]
	want := namespacePodPeer(lakeAppNamespace(ours), lakeAppLabels(ours))
	if !reflect.DeepEqual(rule.To, []networkingv1.NetworkPolicyPeer{want}) || !reflect.DeepEqual(policyPorts(t, rule.Ports), []int32{9123}) {
		t.Fatalf("app rule = %+v", rule)
	}
	requests := lakeRequestsForAppSandbox(ctx, ours)
	if len(requests) != 1 || requests[0].Name != lake.Name {
		t.Fatalf("app change did not requeue its Lake: %v", requests)
	}
	if err := kube.Delete(ctx, ours); err != nil {
		t.Fatal(err)
	}
	if policies, err = r.lakeAppProxyAllowances(ctx, lake, lakePodSelector(lake)); err != nil || len(policies) != 0 {
		t.Fatalf("removed backend remains: %v, error %v", policies, err)
	}
}
