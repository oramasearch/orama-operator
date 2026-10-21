/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The operator refuses, on its own, what a tenant could put in an apps row that lake's intake is
// meant to stop (operator#713): it never relies on lake's check alone.

func rowTrustLake(wildcard string) *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake", Host: "lake.example.test", PreviewWildcard: "*.preview.example.test",
			Apps: oramav1alpha1.LakeAppsSpec{Wildcard: wildcard},
		},
	}
}

func rowTrustServices(row *LakeAppRow) []LakeService {
	return []LakeService{{ID: testPostgresID, CustomerID: row.CustomerID, WorkspaceID: row.WorkspaceID, Name: "db", Desired: "present"}}
}

func TestLakeAppRowRefusesPlatformHosts(t *testing.T) {
	for _, tc := range []struct{ wildcard, host string }{
		// The managing cluster's Lake/prod: apps one label beneath oramadev.net, next to
		// app.oramadev.net (Cloud v1's dashboard) and lake.oramadev.net (the Lake itself).
		{"example.test", "lake.example.test"},
		{"example.test", "app.example.test"},
		{"example.test", "preview.example.test"},
		{"apps.example.test", "login.apps.example.test"},
		{"apps.example.test", "grafana.apps.example.test"},
		{"apps.example.test", "lake-eu.apps.example.test"},
		{"apps.example.test", "orama-status.apps.example.test"},
	} {
		lake := rowTrustLake(tc.wildcard)
		row := testLakeAppRow()
		row.Host = tc.host
		if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err == nil {
			t.Errorf("host %s under wildcard %s was accepted", tc.host, tc.wildcard)
		}
	}
	lake := rowTrustLake("apps.example.test")
	row := testLakeAppRow()
	row.Host = "sales-dashboard.apps.example.test"
	if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err != nil {
		t.Fatalf("an ordinary app host was refused: %v", err)
	}
	// The Lake's public host from spec.env counts too.
	lake.Spec.Env = []corev1.EnvVar{{Name: methodologyLakePublicHost, Value: "eu.apps.example.test"}}
	row.Host = "eu.apps.example.test"
	if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err == nil {
		t.Fatal("the Lake's public host was accepted as an app host")
	}
}

func TestLakeAppRowRefusesReplicasAboveLakesCeiling(t *testing.T) {
	lake := rowTrustLake("apps.example.test")
	row := testLakeAppRow()
	row.Replicas = 100
	if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err != nil {
		t.Fatalf("100 replicas refused: %v", err)
	}
	row.Replicas = 101 // lake's app_replicas_check is 1..100
	if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err == nil {
		t.Fatal("replicas above lake's ceiling accepted")
	}
}

func TestLakeAppRowZettaBindingIsReadOnly(t *testing.T) {
	lake := rowTrustLake("apps.example.test")
	for _, bindings := range []string{
		`{"zetta":{"scopes":["read","write"]}}`,
		`{"zetta":{"scopes":["admin"]}}`,
		`{"zetta":{"scopes":["*"]}}`,
		`{"zetta":{"scopes":[]}}`,
	} {
		row := testLakeAppRow()
		row.BindingsJSON = bindings
		if _, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row)); err == nil {
			t.Errorf("bindings %s accepted", bindings)
		}
	}
	row := testLakeAppRow()
	row.BindingsJSON = `{"zetta":{"scopes":["read","read"]}}`
	spec, err := lakeAppRowSpec(lake, &row, rowTrustServices(&row))
	if err != nil || spec.Bindings == nil || spec.Bindings.Zetta == nil || strings.Join(spec.Bindings.Zetta.Scopes, ",") != "read" {
		t.Fatalf("read binding = %+v, %v", spec.Bindings, err)
	}
}

func TestSyncedLakeAppNamespaceMustBeItsWorkspaceAndCustomer(t *testing.T) {
	const otherCustomer = "00000000-0000-4000-8000-0000000000ff"
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "lake"},
		Spec: oramav1alpha1.LakeAppSpec{Sync: &oramav1alpha1.LakeAppSync{
			ID: testAppRowID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID, WorkspaceNamespace: testWorkspaceName,
		}},
	}
	for _, tc := range []struct {
		name      string
		namespace *corev1.Namespace
		target    string
		ok        bool
	}{
		{"own workspace", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Labels: map[string]string{
			workspaceLabel: testWorkspaceID, workspaceCustomerLabel: testServiceCustomer}}}, testWorkspaceName, true},
		{"workspace label only", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Labels: map[string]string{
			workspaceLabel: testWorkspaceID}}}, testWorkspaceName, false},
		{"another customer's namespace", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Labels: map[string]string{
			workspaceLabel: testWorkspaceID, workspaceCustomerLabel: otherCustomer}}}, testWorkspaceName, false},
		{"the Lake namespace", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake", Labels: map[string]string{
			workspaceLabel: testWorkspaceID, workspaceCustomerLabel: testServiceCustomer}}}, "lake", false},
	} {
		r := &LakeAppReconciler{Client: fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(tc.namespace).Build()}
		err := r.reconcileLakeAppNamespace(context.Background(), app, tc.target)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
