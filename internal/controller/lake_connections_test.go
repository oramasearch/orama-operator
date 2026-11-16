// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeConnectionsGuardedRolloutAndAbsentRow(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:  "lake-system",
			Workspaces: oramav1alpha1.LakeWorkspacesSpec{},
		},
	}
	workspace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: testWorkspaceName,
		Labels: map[string]string{
			workspaceLabel:          testWorkspaceID,
			workspaceCustomerLabel:  testServiceCustomer,
			workspaceLakeLabel:      lake.Name,
			workspaceManagedByLabel: "orama-operator",
		},
	}}
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: workspace.Name, UID: types.UID("sandbox-uid")},
		Spec: oramav1alpha1.NetworkSandboxSpec{Policies: []oramav1alpha1.NetworkSandboxPolicy{
			sandboxAllowance("Compatibility path.", workspaceInternalPolicy(workspace.Name, workspace.Labels)),
		}},
	}
	if err := controllerutil.SetControllerReference(lake, sandbox, scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake, workspace, sandbox).
		Build()
	lakeReconciler := &LakeReconciler{Client: kube, Scheme: scheme}
	connectionReconciler := &ConnectionReconciler{Client: kube, Scheme: scheme}
	store := &fakeServiceStore{network: LakeNetworkSnapshot{
		Workspaces: []LakeNetworkWorkspace{{ID: testWorkspaceID, Namespace: testWorkspaceName}},
		Connections: []LakeNetworkConnection{{
			ID:                 testConnectionID,
			CustomerID:         testServiceCustomer,
			WorkspaceID:        testWorkspaceID,
			WorkspaceNamespace: testWorkspaceName,
			SourceKind:         oramav1alpha1.ConnectionSourceService,
			SourceID:           testConnectionSourceID,
			TargetKind:         oramav1alpha1.ConnectionTargetService,
			TargetID:           testConnectionTargetID,
			Ports:              []int32{5432},
			Desired:            oramav1alpha1.ConnectionDesiredPresent,
			Generation:         1,
		}},
	}}

	if changing := lakeReconciler.reconcileLakeConnections(ctx, lake, store); !changing {
		t.Fatal("initial materialization did not report a changing connection")
	}
	var before corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: workspace.Name}, &before); err != nil {
		t.Fatal(err)
	}
	if before.Annotations[workspaceConnectionsActiveAnnotation] == "true" {
		t.Fatal("blanket access was retired before the initial Connection became Ready")
	}
	request := ctrl.Request{NamespacedName: client.ObjectKey{
		Namespace: workspace.Name,
		Name:      lakeConnectionName(testConnectionID),
	}}
	for i := 0; i < 2; i++ {
		if _, err := connectionReconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if changing := lakeReconciler.reconcileLakeConnections(ctx, lake, store); changing {
		t.Fatal("ready initial connection did not settle")
	}
	var active corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: workspace.Name}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Annotations[workspaceConnectionsActiveAnnotation] != "true" {
		t.Fatal("workspace was not switched to declared connections")
	}
	var activeSandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: workspace.Name, Name: networkSandboxName}, &activeSandbox); err != nil {
		t.Fatal(err)
	}
	for _, policy := range activeSandbox.Spec.Policies {
		if policy.Name == workspaceInternalName {
			t.Fatal("allow-workspace-internal remains after all initial Connections became Ready")
		}
	}
	if len(store.networkUpserts) != 2 || store.networkUpserts[1].Phase != oramav1alpha1.ConnectionPhaseReady {
		t.Fatalf("status transitions = %+v", store.networkUpserts)
	}
	if changing := lakeReconciler.reconcileLakeConnections(ctx, lake, store); changing {
		t.Fatal("steady Connection reported changing work")
	}
	if len(store.networkUpserts) != 2 {
		t.Fatalf("steady Connection rewrote Postgres status: %+v", store.networkUpserts)
	}

	store.mu.Lock()
	store.network.Connections[0].Desired = oramav1alpha1.ConnectionDesiredAbsent
	store.network.Connections[0].Generation = 2
	store.network.Connections[0].Status = nil
	store.mu.Unlock()
	if changing := lakeReconciler.reconcileLakeConnections(ctx, lake, store); !changing {
		t.Fatal("absent desired row did not enter deletion")
	}
	if _, err := connectionReconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	egressName, ingressName := connectionPolicyNames(&oramav1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: request.Name}})
	for _, name := range []string{egressName, ingressName} {
		var policy networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: workspace.Name, Name: name}, &policy); client.IgnoreNotFound(err) != nil {
			t.Fatalf("removed connection still allows %s: %v", name, err)
		}
	}
	_ = lakeReconciler.reconcileLakeConnections(ctx, lake, store)
	store.mu.Lock()
	last := store.networkUpserts[len(store.networkUpserts)-1]
	store.mu.Unlock()
	if last.Phase != oramav1alpha1.ConnectionPhaseDeleted || last.ObservedGeneration != 2 {
		t.Fatalf("final database status = %+v", last)
	}
}

func TestLakeConnectionsReadNothingBeforeNetworkGuard(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	reconciler := &LakeReconciler{Client: kube, Scheme: scheme}
	store := &fakeServiceStore{networkMissing: true}
	if reconciler.reconcileLakeConnections(context.Background(), lake, store) {
		t.Fatal("missing network_guard reported changing work")
	}
	var connections oramav1alpha1.ConnectionList
	if err := kube.List(context.Background(), &connections); err != nil {
		t.Fatal(err)
	}
	if len(connections.Items) != 0 || len(store.networkUpserts) != 0 {
		t.Fatalf("pre-guard work = %d Connections, %d status writes", len(connections.Items), len(store.networkUpserts))
	}
}

func TestLakeConnectionsCarryTheCredentialScopeAndReturnOnlyTheSecretReference(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-system"},
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake).
		Build()
	lakeReconciler := &LakeReconciler{Client: kube, Scheme: scheme}
	row := func(id, scope string) LakeNetworkConnection {
		return LakeNetworkConnection{
			ID:                 id,
			CustomerID:         testServiceCustomer,
			WorkspaceID:        testWorkspaceID,
			WorkspaceNamespace: testWorkspaceName,
			SourceKind:         oramav1alpha1.ConnectionSourceApp,
			SourceID:           testConnectionSourceID,
			TargetKind:         oramav1alpha1.ConnectionTargetService,
			TargetID:           testConnectionTargetID,
			CredentialScope:    scope,
			Desired:            oramav1alpha1.ConnectionDesiredPresent,
			Generation:         3,
		}
	}
	const (
		badAccessID = "40000000-0000-4000-8000-0000000000a1"
		unknownID   = "40000000-0000-4000-8000-0000000000a2"
	)
	store := &fakeServiceStore{network: LakeNetworkSnapshot{
		Workspaces: []LakeNetworkWorkspace{{ID: testWorkspaceID, Namespace: testWorkspaceName}},
		Connections: []LakeNetworkConnection{
			row(testConnectionID, `{"access": "read", "schema": "analytics"}`),
			row(badAccessID, `{"access": "admin"}`),
			row(unknownID, `{"access": "read", "superuser": true}`),
		},
	}}
	lakeReconciler.reconcileLakeConnections(ctx, lake, store)

	var connection oramav1alpha1.Connection
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: lakeConnectionName(testConnectionID)}
	if err := kube.Get(ctx, key, &connection); err != nil {
		t.Fatal(err)
	}
	if connection.Spec.CredentialScope == nil ||
		*connection.Spec.CredentialScope != (oramav1alpha1.ConnectionCredentialScope{Access: "read", Schema: "analytics"}) {
		t.Fatalf("scope = %+v", connection.Spec.CredentialScope)
	}
	for _, id := range []string{badAccessID, unknownID} {
		var refused oramav1alpha1.Connection
		err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lakeConnectionName(id)}, &refused)
		if client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("an invalid scope %s materialized a Connection: %v", id, err)
		}
	}
	failed := map[string]bool{}
	for _, upsert := range store.networkUpserts {
		if upsert.Phase == oramav1alpha1.ConnectionPhaseFailed && strings.Contains(upsert.Message, "credential_scope") {
			failed[upsert.ConnectionID] = true
		}
	}
	if !failed[badAccessID] || !failed[unknownID] {
		t.Fatalf("invalid scopes were not refused: %+v", store.networkUpserts)
	}

	// The Connection controller's observation travels to Postgres as a reference only.
	reference := lakeSecretReference(testCredentialMountID, testCredentialSecretName)
	connection.Status = oramav1alpha1.ConnectionStatus{
		Phase:                oramav1alpha1.ConnectionPhaseReady,
		Message:              "connection policies and scoped credential ready",
		ObservedGeneration:   3,
		CredentialSecret:     reference,
		CredentialGeneration: 3,
		CredentialRole:       connectionRoleName(testConnectionID),
	}
	if err := kube.Status().Update(ctx, &connection); err != nil {
		t.Fatal(err)
	}
	lakeReconciler.reconcileLakeConnections(ctx, lake, store)
	store.mu.Lock()
	defer store.mu.Unlock()
	var last LakeNetworkConnectionStatus
	for _, upsert := range store.networkUpserts {
		if upsert.ConnectionID == testConnectionID {
			last = upsert
		}
	}
	if last.Phase != oramav1alpha1.ConnectionPhaseReady || last.CredentialSecret != reference || last.ObservedGeneration != 3 {
		t.Fatalf("database status = %+v", last)
	}
}
