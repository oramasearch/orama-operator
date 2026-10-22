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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// lake#802: Lake's pods are isolated by its NetworkSandbox, so a methodology stage or collect pod's
// egress (methodology-transfer) reaches nothing without the Lake's matching ingress, and Lake's
// egress must match methodology-serve-ingress for predictions. Every Validate failed on the stage
// pod's timed-out calls before these halves existed.
func TestLakeNetworkSandboxAdmitsOnlyItsMethodologyTransferPods(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := methodologyTestLake()
	objects := append([]client.Object{lake}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &LakeReconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
	if err := r.reconcileLakeNetworkSandbox(ctx, lake, lakeNamespace(lake), map[string]string{"test": "lake"}); err != nil {
		t.Fatalf("reconcile Lake NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)

	lakePod := map[string]string{"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": lake.Name, lakeInstanceLabel: lake.Name}
	runsNamespace := map[string]string{"kubernetes.io/metadata.name": methodologyRunsNamespace(lake.Name)}
	run := testMethodologyRun()
	deployment := &LakeMethodologyDeployment{ID: "dep-1", Slug: "md-777777777777"}
	other := lake.DeepCopy()
	other.Name = "another"
	port := []int32{int32(lakeContainerPort(lake))}

	ingress := allowanceByName(t, &sandbox, "allow-methodology-ingress-to-lake")
	if !selectorMatches(t, ingress.Spec.PodSelector, lakePod) || len(ingress.Spec.Egress) != 0 || len(ingress.Spec.Ingress) != 1 {
		t.Fatalf("not ingress to Lake's pods only: %+v", ingress.Spec)
	}
	rule := ingress.Spec.Ingress[0]
	if !reflect.DeepEqual(policyPorts(t, rule.Ports), port) || len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil || rule.From[0].PodSelector == nil {
		t.Fatalf("unexpected methodology ingress: %+v", rule)
	}
	peer := rule.From[0]
	if !selectorMatches(t, *peer.NamespaceSelector, runsNamespace) ||
		selectorMatches(t, *peer.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": methodologyRunsNamespace(other.Name)}) {
		t.Fatal("methodology ingress comes from this Lake's methodology-runs namespace only")
	}
	for name, admitted := range map[string]map[string]string{
		"a run's stage pod":          methodologyLabels(lake, &run, methodologyRoleStage),
		"a run's collect pod":        methodologyLabels(lake, &run, methodologyRoleCollect),
		"a served model's stage pod": methodologyDeploymentLabels(lake, deployment, methodologyRoleStage),
	} {
		if !selectorMatches(t, *peer.PodSelector, admitted) {
			t.Errorf("%s cannot reach its Lake", name)
		}
	}
	granted := run
	granted.Network = true
	for name, refused := range map[string]map[string]string{
		"a sandbox, the code that is not trusted": methodologyLabels(lake, &run, methodologyRoleSandbox),
		"a sandbox granted the internet":          methodologyLabels(lake, &granted, methodologyRoleSandbox),
		"a serve pod":                             methodologyDeploymentLabels(lake, deployment, methodologyRoleServe),
		"another Lake's stage pod":                methodologyLabels(other, &run, methodologyRoleStage),
	} {
		if selectorMatches(t, *peer.PodSelector, refused) {
			t.Errorf("%s reaches Lake", name)
		}
	}
	// The other end: methodology-transfer's egress names the same pods and the same port.
	transfer := methodologyTransferNetworkPolicy(lake, methodologyRunsNamespace(lake.Name))
	if !reflect.DeepEqual(transfer.Spec.PodSelector, *peer.PodSelector) {
		t.Fatalf("the two halves select different pods: %+v vs %+v", transfer.Spec.PodSelector, *peer.PodSelector)
	}
	toLake := transfer.Spec.Egress[1]
	if !reflect.DeepEqual(policyPorts(t, toLake.Ports), port) || !selectorMatches(t, *toLake.To[0].PodSelector, lakePod) {
		t.Fatalf("the transfer egress does not reach the port Lake admits: %+v", toLake)
	}

	serve := allowanceByName(t, &sandbox, "allow-lake-to-methodology-serve")
	if !selectorMatches(t, serve.Spec.PodSelector, lakePod) || len(serve.Spec.Ingress) != 0 || len(serve.Spec.Egress) != 1 {
		t.Fatalf("not Lake egress only: %+v", serve.Spec)
	}
	out := serve.Spec.Egress[0]
	if !reflect.DeepEqual(policyPorts(t, out.Ports), []int32{methodologyServePort}) || len(out.To) != 1 ||
		!selectorMatches(t, *out.To[0].NamespaceSelector, runsNamespace) {
		t.Fatalf("unexpected serve egress: %+v", out)
	}
	if !selectorMatches(t, *out.To[0].PodSelector, methodologyDeploymentLabels(lake, deployment, methodologyRoleServe)) {
		t.Fatal("Lake cannot reach its serve pods")
	}
	for _, refused := range []map[string]string{
		methodologyLabels(lake, &run, methodologyRoleSandbox),
		methodologyLabels(lake, &run, methodologyRoleStage),
		methodologyDeploymentLabels(other, deployment, methodologyRoleServe),
	} {
		if selectorMatches(t, *out.To[0].PodSelector, refused) {
			t.Errorf("Lake reaches %v", refused)
		}
	}
	// And the serve pods' own ingress admits exactly these Lake pods.
	admit := methodologyServeIngressPolicy(lake, methodologyRunsNamespace(lake.Name)).Spec.Ingress[0].From[0]
	if !selectorMatches(t, *admit.PodSelector, lakePod) || !selectorMatches(t, *admit.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": lakeNamespace(lake)}) {
		t.Fatal("methodology-serve-ingress does not admit this Lake's pods")
	}
}

// The LakeE2ETest runner is a Job in the Lake's namespace, labelled orama.cloud/lake-e2e, and calls
// the Lake's in-cluster Service (LAKE_E2E_BASE_URL). The Lake's ingress is sandboxed, so without
// this allowance every REST, MCP and liquid step times out (orama-operator#589). Only the runner's
// pods in the Lake's own namespace are let in, on the Lake's port only.
func TestLakeNetworkSandboxLetsItsE2ERunnerCallTheLake(t *testing.T) {
	lake := serviceTestLake()
	pods := lakePodSelector(lake)
	policies := lakeInternalIngressAllowances(lake, pods, metav1.LabelSelector{}, metav1.LabelSelector{}, metav1.LabelSelector{}, metav1.LabelSelector{})
	sandbox := &oramav1alpha1.NetworkSandbox{Spec: oramav1alpha1.NetworkSandboxSpec{Policies: policies}}
	assertJustifiedNarrowAllowances(t, sandbox)
	policy := allowanceByName(t, sandbox, "allow-e2e-ingress-to-lake")
	if !reflect.DeepEqual(policy.Spec.PodSelector, pods) || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 1 {
		t.Fatalf("e2e path is not ingress to the Lake only: %+v", policy.Spec)
	}
	rule := policy.Spec.Ingress[0]
	if !reflect.DeepEqual(policyPorts(t, rule.Ports), []int32{int32(lakeContainerPort(lake))}) || len(rule.From) != 1 {
		t.Fatalf("unexpected e2e ingress: %+v", rule)
	}
	peer := rule.From[0]
	if peer.NamespaceSelector != nil || peer.IPBlock != nil || peer.PodSelector == nil {
		t.Fatalf("e2e ingress reaches beyond the Lake's own namespace: %+v", peer)
	}
	// The runner's pod as the LakeE2ETest controller labels it.
	test := &oramav1alpha1.LakeE2ETest{ObjectMeta: metav1.ObjectMeta{Name: "testing-gate"}, Spec: oramav1alpha1.LakeE2ETestSpec{TargetRef: lake.Name, Image: "oramasearch/lake-e2e@sha256:0"}}
	job := (&LakeE2ETestReconciler{}).runnerJob(test, lake, lakeNamespace(lake), lakeE2EJobName(test))
	if !selectorMatches(t, *peer.PodSelector, job.Spec.Template.Labels) {
		t.Fatalf("the runner's pod %v cannot call its Lake", job.Spec.Template.Labels)
	}
	if selectorMatches(t, *peer.PodSelector, nil) || selectorMatches(t, *peer.PodSelector, map[string]string{"app.kubernetes.io/name": "anything"}) {
		t.Fatal("e2e ingress lets in pods that are not the runner")
	}
}
