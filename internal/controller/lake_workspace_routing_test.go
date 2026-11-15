/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// operator#781: a row's objects go only into the namespace the workspaces reconciler labelled for
// that workspace, that customer and that Lake, never into the Lake namespace or another tenant's.
func TestWorkspaceNamespaceRefusal(t *testing.T) {
	const workspace, customer, other = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake-prod"}}
	namespace := func(name string, labels map[string]string) client.Object {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	owned := map[string]string{workspaceLabel: workspace, workspaceCustomerLabel: customer, workspaceLakeLabel: "prod"}
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(
		namespace("prod-ws-own", owned),
		namespace("prod-ws-other-customer", map[string]string{workspaceLabel: workspace, workspaceCustomerLabel: other, workspaceLakeLabel: "prod"}),
		namespace("prod-ws-other-workspace", map[string]string{workspaceLabel: other, workspaceCustomerLabel: customer, workspaceLakeLabel: "prod"}),
		namespace("other-lake-ws", map[string]string{workspaceLabel: workspace, workspaceCustomerLabel: customer, workspaceLakeLabel: "staging"}),
		namespace("kube-system", nil),
		namespace("lake-prod", owned),
	).Build()
	for name, refused := range map[string]bool{
		"prod-ws-own":             false,
		"prod-ws-other-customer":  true,
		"prod-ws-other-workspace": true,
		"other-lake-ws":           true,
		"kube-system":             true,
		"lake-prod":               true,
		// No intake creates a namespace, so a write into a missing one fails at the API server.
		"prod-ws-missing": false,
	} {
		refusal, err := workspaceNamespaceRefusal(context.Background(), kube, lake, name, workspace, customer)
		if err != nil {
			t.Fatal(err)
		}
		if (refusal != "") != refused {
			t.Errorf("%s: refusal = %q, want refused=%v", name, refusal, refused)
		}
	}
}

// operator#781: every listing that routes a row to a workspace joins the workspace on the row's own
// customer, and a methodology row's build on it too.
func TestRowListingsJoinTheWorkspaceOfTheRowsCustomer(t *testing.T) {
	for name, statement := range map[string]string{
		"services": listServicesSQL, "stores": listStoresSQL, "agent launches": listAgentLaunchesSQL,
		"agent schedules": listAgentSchedulesSQL, "connections": listNetworkConnectionsSQL,
		"egress rules": listNetworkEgressRulesSQL, "tunnel grants": listTunnelGrantsSQL, "peers": listPeersSQL,
		"methodology runs": listMethodologyRunsSQL, "methodology deployments": listMethodologyDeploymentsSQL,
		"apps": listAppsSQL,
	} {
		if !regexp.MustCompile(`JOIN workspaces w ON w\.id = (\w)\.workspace_id AND w\.customer_id = (\w)\.customer_id`).MatchString(statement) {
			t.Errorf("%s: the workspace join does not compare customers", name)
		}
	}
	for name, statement := range map[string]string{"methodology runs": listMethodologyRunsSQL, "methodology deployments": listMethodologyDeploymentsSQL} {
		if !regexp.MustCompile(`LEFT JOIN builds b ON b\.id = (\w)\.build_id AND b\.customer_id = (\w)\.customer_id`).MatchString(statement) {
			t.Errorf("%s: the build join does not compare customers", name)
		}
	}
}
