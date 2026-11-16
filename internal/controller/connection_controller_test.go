// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testConnectionID       = "40000000-0000-4000-8000-000000000004"
	testConnectionSourceID = "50000000-0000-4000-8000-000000000005"
	testConnectionTargetID = "60000000-0000-4000-8000-000000000006"
)

func ownedConnectionObjects(t *testing.T) (*oramav1alpha1.Lake, *oramav1alpha1.NetworkSandbox, *oramav1alpha1.Connection) {
	t.Helper()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")}}
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: "workspace", UID: types.UID("sandbox-uid")},
		Spec: oramav1alpha1.NetworkSandboxSpec{
			PodSelector: metav1.LabelSelector{},
			Policies: []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(
				"Compatibility allowance before the initial connections are ready.",
				workspaceInternalPolicy("workspace", nil),
			)},
		},
	}
	connection := &oramav1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lakeConnectionName(testConnectionID),
			Namespace:  "workspace",
			UID:        types.UID("connection-uid"),
			Finalizers: []string{connectionFinalizer},
		},
		Spec: oramav1alpha1.ConnectionSpec{
			LakeRef:      lake.Name,
			ConnectionID: testConnectionID,
			CustomerID:   testServiceCustomer,
			WorkspaceID:  testWorkspaceID,
			SourceKind:   oramav1alpha1.ConnectionSourceService,
			SourceID:     testConnectionSourceID,
			TargetKind:   oramav1alpha1.ConnectionTargetService,
			TargetID:     testConnectionTargetID,
			Ports:        []int32{5432},
			Desired:      oramav1alpha1.ConnectionDesiredPresent,
			Generation:   1,
		},
	}
	for _, object := range []client.Object{sandbox, connection} {
		if err := controllerutil.SetControllerReference(lake, object, scheme); err != nil {
			t.Fatal(err)
		}
	}
	return lake, sandbox, connection
}

func TestConnectionRendersBothSidesAndRemovalClosesPathInOneReconcile(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake, sandbox, connection := ownedConnectionObjects(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake, sandbox, connection).
		Build()
	reconciler := &ConnectionReconciler{Client: kube, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var got oramav1alpha1.Connection
	if err := kube.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseReady || got.Status.ObservedGeneration != 1 {
		t.Fatalf("status = %+v", got.Status)
	}
	egressName, ingressName := connectionPolicyNames(connection)
	for _, name := range []string{egressName, ingressName} {
		var policy networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: name}, &policy); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !metav1.IsControlledBy(&policy, sandbox) {
			t.Fatalf("%s is not owned by the workspace NetworkSandbox", name)
		}
	}
	var egress networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: egressName}, &egress); err != nil {
		t.Fatal(err)
	}
	if egress.Spec.PodSelector.MatchLabels[serviceIDLabel] != testConnectionSourceID ||
		len(egress.Spec.Egress) != 1 ||
		egress.Spec.Egress[0].To[0].PodSelector.MatchLabels[serviceIDLabel] != testConnectionTargetID ||
		egress.Spec.Egress[0].Ports[0].Port.IntVal != 5432 {
		t.Fatalf("egress policy = %#v", egress.Spec)
	}

	got.Spec.Desired = oramav1alpha1.ConnectionDesiredAbsent
	got.Spec.Generation = 2
	if err := kube.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseDeleted || got.Status.ObservedGeneration != 2 {
		t.Fatalf("deleted status = %+v", got.Status)
	}
	for _, name := range []string{egressName, ingressName} {
		var policy networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: name}, &policy); client.IgnoreNotFound(err) != nil {
			t.Fatalf("policy %s remains after one absent reconcile: %v", name, err)
		}
	}
}

func TestConnectionWithEmptyPortsUsesOnlyTargetServicePorts(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake, sandbox, connection := ownedConnectionObjects(t)
	connection.Spec.Ports = nil
	target := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target",
			Namespace: connection.Namespace,
			Labels:    map[string]string{serviceIDLabel: testConnectionTargetID},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "postgres", Port: 5432, TargetPort: intstr.FromString("postgres")},
			{Name: "metrics", Port: 9187, TargetPort: intstr.FromInt32(9187)},
		}},
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake, sandbox, connection, target).
		Build()
	reconciler := &ConnectionReconciler{Client: kube, Scheme: scheme}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}); err != nil {
		t.Fatal(err)
	}
	egressName, _ := connectionPolicyNames(connection)
	var egress networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: egressName}, &egress); err != nil {
		t.Fatal(err)
	}
	ports := egress.Spec.Egress[0].Ports
	if len(ports) != 2 || ports[0].Port.String() != "9187" || ports[1].Port.String() != "postgres" {
		t.Fatalf("resolved ports = %#v", ports)
	}
}

func TestAppConnectionUsesTheExistingAppWorkloadIdentity(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake, sandbox, connection := ownedConnectionObjects(t)
	connection.Spec.SourceKind = oramav1alpha1.ConnectionSourceApp
	connection.Spec.SourceID = testConnectionSourceID
	lake.Spec.Namespace = "lake-system"
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "source-app",
			Namespace: lake.Spec.Namespace,
			Labels:    map[string]string{lakeAppRowIDLabel: testConnectionSourceID},
		},
		Spec: oramav1alpha1.LakeAppSpec{
			LakeRef: lake.Name,
			Sync: &oramav1alpha1.LakeAppSync{
				ID:                 testConnectionSourceID,
				WorkspaceID:        testWorkspaceID,
				WorkspaceNamespace: connection.Namespace,
			},
		},
	}
	if err := controllerutil.SetControllerReference(lake, app, scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake, sandbox, connection, app).
		Build()
	reconciler := &ConnectionReconciler{Client: kube, Scheme: scheme}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}); err != nil {
		t.Fatal(err)
	}
	egressName, ingressName := connectionPolicyNames(connection)
	var egress networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: egressName}, &egress); err != nil {
		t.Fatal(err)
	}
	wantIdentity := lakeAppLabels(app)[lakeAppIdentityLabel]
	if egress.Spec.PodSelector.MatchLabels[lakeAppIdentityLabel] != wantIdentity {
		t.Fatalf("app selector = %v, want existing identity %q", egress.Spec.PodSelector, wantIdentity)
	}
	var ingress networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: ingressName}, &ingress); err != nil {
		t.Fatal(err)
	}
	if ingress.Spec.Ingress[0].From[0].PodSelector.MatchLabels[lakeAppIdentityLabel] != wantIdentity {
		t.Fatalf("target ingress source = %#v", ingress.Spec.Ingress[0].From)
	}
}
