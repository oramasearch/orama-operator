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
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, oramav1alpha1.AddToScheme} {
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
