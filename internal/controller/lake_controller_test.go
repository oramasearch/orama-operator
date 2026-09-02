/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

func newLakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

func reconcileLake(t *testing.T, c client.Client, s *runtime.Scheme, name string) {
	t.Helper()
	r := &LakeReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestLakeReconcileCreatesWorkloads(t *testing.T) {
	s := newLakeScheme(t)
	one := int32(1)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-dev", Image: "hashicorp/http-echo:1.0", Replicas: &one},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()

	reconcileLake(t, c, s, "hello")

	if err := c.Get(context.Background(), types.NamespacedName{Name: "lake-dev"}, &corev1.Namespace{}); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &d); err != nil {
		t.Fatalf("deployment not created: %v", err)
	}
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "hashicorp/http-echo:1.0" {
		t.Errorf("image = %q", got)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v", d.Spec.Replicas)
	}
	if len(d.OwnerReferences) != 1 || d.OwnerReferences[0].Kind != "Lake" {
		t.Errorf("deployment owner refs = %+v", d.OwnerReferences)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 5678 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}
}

func TestLakeReconcileIsIdempotent(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec:       oramav1alpha1.LakeSpec{Image: "hashicorp/http-echo:1.0"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	reconcileLake(t, c, s, "hello")
	reconcileLake(t, c, s, "hello") // second pass must not error or duplicate

	var ds appsv1.DeploymentList
	if err := c.List(context.Background(), &ds, client.InNamespace("lake-dev")); err != nil || len(ds.Items) != 1 {
		t.Fatalf("deployments after two reconciles = %d (err %v)", len(ds.Items), err)
	}
}

func TestLakeReconcileRequiresImage(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "noimage"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "noimage"}}); err == nil {
		t.Fatal("expected an error when spec.image is empty")
	}
}

func TestLakeReconcileIngressAndDNS(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "hashicorp/http-echo:1.0", IngressEnabled: true, Host: "hello.lake.live",
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake, &networkingv1.Ingress{}).Build()

	// 1. Ingress appears; the DNS record waits for an address.
	reconcileLake(t, c, s, "hello")
	var ing networkingv1.Ingress
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &ing); err != nil {
		t.Fatalf("ingress not created: %v", err)
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Errorf("ingress class = %v", ing.Spec.IngressClassName)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "hello.lake.live" {
		t.Errorf("ingress rules = %+v", ing.Spec.Rules)
	}
	if b := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service; b == nil || b.Name != "lake-hello" || b.Port.Number != lakeServicePort {
		t.Errorf("ingress backend = %+v", b)
	}
	if len(ing.OwnerReferences) != 1 || ing.OwnerReferences[0].Name != "hello" {
		t.Errorf("ingress owner refs = %+v", ing.OwnerReferences)
	}
	var rec oramav1alpha1.CloudflareDNSRecord
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err == nil {
		t.Fatal("DNS record created before the ingress had an address")
	}

	// 2. The ingress controller assigns an address: the DNS record follows.
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "172.212.70.79"}}
	if err := c.Status().Update(ctx, &ing); err != nil {
		t.Fatalf("set ingress address: %v", err)
	}
	reconcileLake(t, c, s, "hello")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err != nil {
		t.Fatalf("DNS record not created: %v", err)
	}
	if rec.Spec.Name != "hello.lake.live" || rec.Spec.Content != "172.212.70.79" || !rec.Spec.Proxied || rec.Spec.SecretRef != "cloudflare-api-token" {
		t.Errorf("DNS record spec = %+v", rec.Spec)
	}
	if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].Name != "hello" {
		t.Errorf("DNS record owner refs = %+v", rec.OwnerReferences)
	}

	// 3. Gate off: both are torn down, the workloads stay.
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.IngressEnabled = false
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "hello")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &ing); err == nil {
		t.Error("ingress not torn down when ingressEnabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err == nil {
		t.Error("DNS record not torn down when ingressEnabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &appsv1.Deployment{}); err != nil {
		t.Errorf("deployment should survive the gate: %v", err)
	}
}
