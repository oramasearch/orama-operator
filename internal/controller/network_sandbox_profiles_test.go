/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type failingSandboxDiscoveryReader struct {
	client.Reader
}

func (r failingSandboxDiscoveryReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if key.Namespace == "default" && key.Name == "kubernetes" {
		return apierrors.NewServiceUnavailable("test API discovery failure")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func sandboxDiscoveryObjects() []client.Object {
	return []client.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				ClusterIP:  "10.96.0.1",
				ClusterIPs: []string{"10.96.0.1"},
				Ports:      []corev1.ServicePort{{Port: 443}},
			},
		},
		&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Subsets: []corev1.EndpointSubset{{
				Addresses: []corev1.EndpointAddress{{IP: "192.0.2.10"}},
				Ports:     []corev1.EndpointPort{{Port: 6443}},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Spec:       corev1.NodeSpec{PodCIDR: "10.244.0.0/24", PodCIDRs: []string{"10.244.0.0/24"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
				Type:    corev1.NodeInternalIP,
				Address: "192.0.2.20",
			}}},
		},
	}
}

func allowanceByName(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox, name string) oramav1alpha1.NetworkSandboxPolicy {
	t.Helper()
	for _, allowance := range sandbox.Spec.Policies {
		if allowance.Name == name {
			return allowance
		}
	}
	t.Fatalf("allowance %q not found", name)
	return oramav1alpha1.NetworkSandboxPolicy{}
}

func assertJustifiedNarrowAllowances(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, allowance := range sandbox.Spec.Policies {
		if strings.TrimSpace(allowance.Comment) == "" {
			t.Errorf("allowance %q has no component justification", allowance.Name)
		}
		if _, ok := seen[allowance.Name]; ok {
			t.Errorf("duplicate allowance %q", allowance.Name)
		}
		seen[allowance.Name] = struct{}{}
		for _, ingress := range allowance.Spec.Ingress {
			if len(ingress.From) == 0 {
				t.Errorf("allowance %q has unrestricted ingress", allowance.Name)
			}
		}
		for _, egress := range allowance.Spec.Egress {
			if len(egress.To) == 0 {
				t.Errorf("allowance %q has unrestricted egress", allowance.Name)
			}
		}
	}
}

func TestLakeNetworkSandboxListsOnlyJustifiedNarrowPaths(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", UID: types.UID("lake-uid")},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake-system",
			Image:          "oramasearch/lake:test",
			IngressEnabled: true,
			Host:           "lake.example.test",
		},
	}
	objects := append([]client.Object{lake}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &LakeReconciler{
		Client:            kube,
		Scheme:            scheme,
		APIReader:         kube,
		OperatorNamespace: "orama-system",
	}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, map[string]string{"test": "lake"}); err != nil {
		t.Fatalf("reconcile Lake NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)
	for _, name := range []string{
		"allow-lake-to-controldb",
		"allow-lake-to-zetta",
		"allow-lake-to-workspaces",
		"allow-lake-apiserver-egress",
		"allow-cnpg-apiserver-egress",
		"allow-lake-public-https",
		"allow-ingress-nginx",
	} {
		allowanceByName(t, &sandbox, name)
	}
	var deny networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxDefaultDenyName}, &deny); err != nil {
		t.Fatalf("default deny was not rendered: %v", err)
	}
}

func TestOramaCloudNetworkSandboxKeepsEngineHTTPForIssue466(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	stack.Spec.Components.ControlDB.Mode = "external"
	objects := append([]client.Object{stack}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &OramaCloudV1Reconciler{
		Client:            kube,
		Scheme:            scheme,
		APIReader:         kube,
		OperatorNamespace: "orama-system",
	}
	if err := reconciler.reconcileNetworkSandbox(ctx, stack); err != nil {
		t.Fatalf("reconcile Cloud v1 NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)
	engine := allowanceByName(t, &sandbox, "allow-engine-public-internet")
	if len(engine.Spec.Egress) != 1 || !reflect.DeepEqual(policyPorts(t, engine.Spec.Egress[0].Ports), []int32{80, 443}) {
		t.Fatalf("engine public ports = %+v, want [80 443] until issue #466", engine.Spec.Egress)
	}
	if len(engine.Spec.Egress[0].To) != 2 || engine.Spec.Egress[0].To[0].IPBlock == nil || len(engine.Spec.Egress[0].To[0].IPBlock.Except) == 0 {
		t.Fatalf("engine public rule does not exclude private and cluster ranges: %+v", engine.Spec.Egress[0].To)
	}
	for _, name := range []string{
		"allow-engine-egress",
		"allow-dashboard-egress",
		"allow-cnpg-apiserver-egress",
		"allow-dashboard-ingress-nginx",
		"allow-docs-ingress-nginx",
		"allow-control-clients-external-postgres",
	} {
		allowanceByName(t, &sandbox, name)
	}
}

func TestNetworkSandboxPreservesDiscoveredPathsDuringReadFailure(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", UID: types.UID("lake-uid")},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-system", Image: "oramasearch/lake:test"},
	}
	objects := append([]client.Object{lake}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &LakeReconciler{Client: kube, Scheme: scheme, APIReader: kube}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, nil); err != nil {
		t.Fatal(err)
	}
	reconciler.APIReader = failingSandboxDiscoveryReader{Reader: kube}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, nil); err == nil {
		t.Fatal("reconcile succeeded during Kubernetes API discovery failure")
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"allow-lake-apiserver-egress",
		"allow-cnpg-apiserver-egress",
		"allow-lake-public-https",
		"allow-agents-public-https",
		"allow-controldb-backup-public-https",
	} {
		allowanceByName(t, &sandbox, name)
	}
}
