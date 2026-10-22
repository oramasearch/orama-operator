/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeAppsListenerWakesAppsOnlyLakeAndStops(t *testing.T) {
	scheme := newLakeAppScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{
		Namespace: "lake", Apps: oramav1alpha1.LakeAppsSpec{},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeRoleSecret("prod", lakeOperatorRole), Namespace: "lake"}, Data: map[string][]byte{"uri": []byte("postgresql://db/lake")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, secret).Build()
	listener := NewLakeAppsListener(c)
	listener.Resync = 10 * time.Millisecond
	if listener.channel != "lake_apps" || !listener.NeedLeaderElection() {
		t.Fatal("apps listener has the wrong channel or leadership policy")
	}
	stopped := make(chan struct{})
	listener.Listen = func(ctx context.Context, uri string, notify func()) error {
		if uri != "postgresql://db/lake" {
			t.Error("listener used the wrong DSN")
		}
		notify()
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = listener.Start(ctx)
		close(done)
	}()
	select {
	case event := <-listener.Events:
		if event.Object.GetName() != lake.Name {
			t.Fatal("notification woke the wrong Lake")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification did not wake the apps-only Lake")
	}
	lake.Spec.Postgres.Enabled = boolPtr(false)
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("disabled Lake kept its listener")
	}
	cancel()
	<-done
}

func TestLakeAppsSchemaGateAndFallback(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.PostgresSchema{}, &oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	request := ctrl.Request{NamespacedName: client.ObjectKey{Name: h.lake.Name}}
	result, err := h.sync.Reconcile(h.ctx, request)
	if err != nil || result.RequeueAfter != lakeServicesChangingPoll {
		t.Fatalf("schema gate: %+v, %v", result, err)
	}
	var apps oramav1alpha1.LakeAppList
	if err := c.List(h.ctx, &apps); err != nil || len(apps.Items) != 0 {
		t.Fatal("apps started before the Lake schema was applied")
	}
	applyLakeSchemaForWorkloadTest(t, c, scheme, h.lake)
	h.store.rows = nil
	result, err = h.sync.Reconcile(h.ctx, request)
	if err != nil || result.RequeueAfter != 60*time.Second {
		t.Fatalf("fallback interval: %+v, %v", result, err)
	}
	h.lake.Spec.Postgres.Enabled = boolPtr(false)
	if err := c.Update(h.ctx, h.lake); err != nil {
		t.Fatal(err)
	}
	result, err = h.sync.Reconcile(h.ctx, request)
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("disabled apps continue polling: %+v, %v", result, err)
	}
}
