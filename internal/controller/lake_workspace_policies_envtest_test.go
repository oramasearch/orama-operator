// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestWorkspaceDatastorePoliciesAgainstAPIServer runs the workspace reconcile against a real API
// server: every generated policy passes validation, and the API egress rule points at the API
// server's own default/kubernetes Service and endpoints (operator#268).
func TestWorkspaceDatastorePoliciesAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	kube, scheme, lake := startWorkspaceEnvtest(t)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler := &LakeWorkspacesReconciler{
		Client:            kube,
		Scheme:            scheme,
		APIReader:         kube,
		OperatorNamespace: "orama-system",
	}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)

	var service corev1.Service
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &service); err != nil {
		t.Fatal(err)
	}
	var endpoints corev1.Endpoints
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &endpoints); err != nil {
		t.Fatal(err)
	}
	want, err := workspaceAPIServerTargetFrom(&service, &endpoints)
	if err != nil {
		t.Fatalf("envtest API server target: %v", err)
	}

	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies, client.InNamespace(testWorkspaceName)); err != nil {
		t.Fatal(err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, policy := range policies.Items {
		byName[policy.Name] = policy
	}
	for _, allowance := range workspaceOperatorIngresses {
		if _, ok := byName[allowance.name]; !ok {
			t.Errorf("operator ingress %s was not created", allowance.name)
		}
	}
	for _, apiClient := range workspaceAPIServerClients {
		policy, ok := byName[apiClient.name]
		if !ok {
			t.Errorf("API egress %s was not created", apiClient.name)
			continue
		}
		if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != len(want.cidrs) ||
			len(policy.Spec.Egress[0].Ports) != len(want.ports) {
			t.Errorf("%s egress = %#v, want %+v", apiClient.name, policy.Spec.Egress, want)
			continue
		}
		for i, peer := range policy.Spec.Egress[0].To {
			if peer.IPBlock == nil || peer.IPBlock.CIDR != want.cidrs[i] {
				t.Errorf("%s peer %d = %#v, want %s", apiClient.name, i, peer, want.cidrs[i])
			}
		}
		for i, port := range policy.Spec.Egress[0].Ports {
			if port.Port == nil || port.Port.IntVal != want.ports[i] {
				t.Errorf("%s port %d = %#v, want %d", apiClient.name, i, port, want.ports[i])
			}
		}
	}

	// A steady second pass rewrites nothing.
	versions := map[string]string{}
	for _, policy := range policies.Items {
		versions[policy.Name] = policy.ResourceVersion
	}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	if err := kube.List(ctx, &policies, client.InNamespace(testWorkspaceName)); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies.Items {
		if versions[policy.Name] != policy.ResourceVersion {
			t.Errorf("steady reconcile rewrote %s", policy.Name)
		}
	}
}
