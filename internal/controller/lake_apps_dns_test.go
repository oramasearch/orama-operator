/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeAppsWildcardDNSLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeAppScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{Namespace: "lake", Apps: oramav1alpha1.LakeAppsSpec{Wildcard: "apps-example.test"},
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare"}},
	}
	class := "nginx"
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-prod", Namespace: "lake"},
		Spec:       networkingv1.IngressSpec{IngressClassName: &class},
	}
	if err := controllerutil.SetControllerReference(lake, ingress, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, ingress).WithStatusSubresource(ingress).Build()
	r := &LakeAppsReconciler{Client: c, Scheme: scheme}
	reconcile := func() {
		t.Helper()
		if err := r.reconcileLakeAppsDNS(ctx, lake); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []oramav1alpha1.CloudflareDNSRecord {
		t.Helper()
		var records oramav1alpha1.CloudflareDNSRecordList
		if err := c.List(ctx, &records); err != nil {
			t.Fatal(err)
		}
		return records.Items
	}
	reconcile()
	if len(list()) != 0 {
		t.Fatal("DNS created without a load-balancer target")
	}
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.20"}}
	if err := c.Status().Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	reconcile()
	records := list()
	if len(records) != 1 || records[0].Spec.Name != "*.apps-example.test" || records[0].Spec.Content != "192.0.2.20" || !records[0].Spec.Proxied || !metav1.IsControlledBy(&records[0], lake) {
		t.Fatalf("wildcard DNS = %+v", records)
	}
	version := records[0].ResourceVersion
	reconcile()
	if list()[0].ResourceVersion != version {
		t.Fatal("steady DNS record was rewritten")
	}
	foreign := records[0].DeepCopy()
	foreign.Name, foreign.ResourceVersion, foreign.UID = "foreign", "", ""
	foreign.OwnerReferences = nil
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Apps.Wildcard = "next.example.test"
	reconcile()
	if len(list()) != 2 {
		t.Fatal("wildcard change did not replace only the owned record")
	}
	lake.Spec.DNSSecretRef = nil
	reconcile()
	if records := list(); len(records) != 1 || records[0].Name != "foreign" {
		t.Fatal("removing the DNS credentials did not preserve the foreign DNS record")
	}
}

func TestLakeAppsDNSUsesConfiguredIngressClass(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	lakeClass, appClass := "nginx", "apps-nginx"
	h.lake.Spec.Apps.IngressClass = appClass
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-" + h.lake.Name, Namespace: "lake"},
		Spec:       networkingv1.IngressSpec{IngressClassName: &lakeClass},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}},
		}},
	}
	if err := controllerutil.SetControllerReference(h.lake, ingress, scheme); err != nil {
		t.Fatal(err)
	}
	appIngress := ingress.DeepCopy()
	appIngress.ObjectMeta = lakeAppObjectMeta(app, "app", lakeAppNamespace(app))
	appIngress.Spec.IngressClassName = &appClass
	appIngress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{Hostname: "app-lb.example.test"}}
	for _, object := range []client.Object{ingress, appIngress} {
		if err := c.Create(h.ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	target, err := h.sync.lakeAppsIngressTarget(h.ctx, h.lake)
	if err != nil || target != "app-lb.example.test" {
		t.Fatalf("wrong ingress class target: %q, %v", target, err)
	}
}
