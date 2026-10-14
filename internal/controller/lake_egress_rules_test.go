/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func testEgressRow(id string) LakeNetworkEgressRule {
	return LakeNetworkEgressRule{
		ID:                 id,
		CustomerID:         testServiceCustomer,
		WorkspaceID:        testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName,
		SourceKind:         oramav1alpha1.EgressRuleSourceApp,
		SourceID:           testEgressSourceID,
		DNSName:            "API.Example.com.",
		Ports:              []int32{8443, 443, 443},
		Desired:            oramav1alpha1.EgressRuleDesiredPresent,
		Generation:         2,
	}
}

func TestLakeEgressRowsBecomeEgressRulesAndStatusReturns(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")}}
	orphan := &oramav1alpha1.EgressRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakeEgressRuleName("f0000000-0000-4000-8000-00000000000f"),
			Namespace: testWorkspaceName,
			Labels:    map[string]string{egressRuleIDLabel: "f0000000-0000-4000-8000-00000000000f"},
		},
	}
	internet := &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{Name: workspaceInternetRuleName, Namespace: testWorkspaceName}}
	for _, object := range []client.Object{orphan, internet} {
		if err := controllerutil.SetControllerReference(lake, object, scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.EgressRule{}).
		WithObjects(lake, orphan, internet).Build()
	reconciler := &LakeReconciler{Client: kube, Scheme: scheme}

	cidr := testEgressRow(testEgressCIDRRuleID)
	cidr.DNSName, cidr.CIDR, cidr.SourceKind, cidr.SourceID = "", "203.0.113.0/24", "", ""
	invalid := map[string]func(*LakeNetworkEgressRule){
		"10000000-0000-4000-8000-0000000000e1": func(r *LakeNetworkEgressRule) { r.CIDR = "10.0.0.0/8" },
		"10000000-0000-4000-8000-0000000000e2": func(r *LakeNetworkEgressRule) { r.DNSName = "93.184.216.34" },
		"10000000-0000-4000-8000-0000000000e3": func(r *LakeNetworkEgressRule) { r.DNSName = "*.example.com" },
		"10000000-0000-4000-8000-0000000000e4": func(r *LakeNetworkEgressRule) { r.SourceID = "" },
		"10000000-0000-4000-8000-0000000000e5": func(r *LakeNetworkEgressRule) { r.Ports = nil },
	}
	store := &fakeServiceStore{egress: LakeNetworkEgressSnapshot{
		Workspaces: []LakeNetworkWorkspace{{ID: testWorkspaceID, Namespace: testWorkspaceName}},
		Rules:      []LakeNetworkEgressRule{testEgressRow(testEgressNameRuleID), cidr},
	}}
	for id, mutate := range invalid {
		row := testEgressRow(id)
		mutate(&row)
		store.egress.Rules = append(store.egress.Rules, row)
	}
	if changing := reconciler.reconcileLakeEgressRules(ctx, lake, store); !changing {
		t.Fatal("new rules did not report changing work")
	}

	var name oramav1alpha1.EgressRule
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lakeEgressRuleName(testEgressNameRuleID)}, &name); err != nil {
		t.Fatal(err)
	}
	if name.Spec.DNSName != "api.example.com" || len(name.Spec.Ports) != 2 || name.Spec.Ports[0] != 443 ||
		name.Spec.RuleID != testEgressNameRuleID || name.Spec.SourceKind != "app" || !metav1.IsControlledBy(&name, lake) {
		t.Fatalf("name rule = %+v", name.Spec)
	}
	var cidrRule oramav1alpha1.EgressRule
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lakeEgressRuleName(testEgressCIDRRuleID)}, &cidrRule); err != nil {
		t.Fatal(err)
	}
	if cidrRule.Spec.CIDR != "203.0.113.0/24" || cidrRule.Spec.SourceKind != "" {
		t.Fatalf("cidr rule = %+v", cidrRule.Spec)
	}
	failed := map[string]bool{}
	for _, upsert := range store.egressUpserts {
		if upsert.Phase == oramav1alpha1.EgressRulePhaseFailed {
			failed[upsert.RuleID] = true
		}
	}
	for id := range invalid {
		var refused oramav1alpha1.EgressRule
		err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lakeEgressRuleName(id)}, &refused)
		if !failed[id] || client.IgnoreNotFound(err) != nil || err == nil {
			t.Errorf("invalid row %s: failed=%v, materialized err=%v", id, failed[id], err)
		}
	}
	// The orphan of a hard-removed row goes; the operator's own internet rule is not Lake's to remove.
	if err := kube.Get(ctx, client.ObjectKeyFromObject(orphan), &oramav1alpha1.EgressRule{}); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("orphan remains: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(internet), &oramav1alpha1.EgressRule{}); err != nil {
		t.Fatalf("internet rule was removed: %v", err)
	}

	// The controller's observation returns to Postgres; absent rows are deleted once Deleted.
	name.Status = oramav1alpha1.EgressRuleStatus{Phase: oramav1alpha1.EgressRulePhaseReady, Message: "ready", ObservedGeneration: 2}
	if err := kube.Status().Update(ctx, &name); err != nil {
		t.Fatal(err)
	}
	reconciler.reconcileLakeEgressRules(ctx, lake, store)
	var returned bool
	for _, upsert := range store.egressUpserts {
		if upsert.RuleID == testEgressNameRuleID && upsert.Phase == oramav1alpha1.EgressRulePhaseReady && upsert.ObservedGeneration == 2 {
			returned = true
		}
	}
	if !returned {
		t.Fatalf("status writes = %+v", store.egressUpserts)
	}
	writes := len(store.egressUpserts)
	reconciler.reconcileLakeEgressRules(ctx, lake, store)
	if len(store.egressUpserts) != writes {
		t.Fatalf("a steady pass rewrote status: %+v", store.egressUpserts[writes:])
	}

	store.mu.Lock()
	store.egress.Rules[0].Desired = oramav1alpha1.EgressRuleDesiredAbsent
	store.egress.Rules[0].Generation = 3
	store.mu.Unlock()
	reconciler.reconcileLakeEgressRules(ctx, lake, store)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&name), &name); err != nil || name.Spec.Desired != oramav1alpha1.EgressRuleDesiredAbsent {
		t.Fatalf("absent row = %+v, %v", name.Spec, err)
	}
	name.Status = oramav1alpha1.EgressRuleStatus{Phase: oramav1alpha1.EgressRulePhaseDeleted, ObservedGeneration: 3}
	if err := kube.Status().Update(ctx, &name); err != nil {
		t.Fatal(err)
	}
	reconciler.reconcileLakeEgressRules(ctx, lake, store)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&name), &name); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("Deleted rule's CR remains: %v", err)
	}
}

func TestLakeEgressRulesReadNothingBeforeNetworkGuard(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	store := &fakeServiceStore{networkMissing: true}
	if changing := (&LakeReconciler{Client: kube, Scheme: scheme}).reconcileLakeEgressRules(ctx, lake, store); changing {
		t.Fatal("a missing guard reported work")
	}
	var rules oramav1alpha1.EgressRuleList
	if err := kube.List(ctx, &rules); err != nil || len(rules.Items) != 0 || len(store.egressUpserts) != 0 {
		t.Fatalf("rules = %d, writes = %d, %v", len(rules.Items), len(store.egressUpserts), err)
	}
}
