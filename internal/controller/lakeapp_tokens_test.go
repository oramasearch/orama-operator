/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeAppsTokensRefuseForeignSecret(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeAppTokensSecretName, Namespace: lakeAppNamespace(app)}, Data: map[string][]byte{"source": []byte("foreign")}}
	if err := c.Create(h.ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := h.apps.reconcileLakeAppTokens(h.ctx, app, secret.Namespace); err == nil {
		t.Fatal("foreign token Secret was adopted")
	}
	if err := c.Get(h.ctx, client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data["source"]) != "foreign" {
		t.Fatal("foreign token Secret was changed")
	}
}

func TestLakeAppsFinalizerWaitsForIndividualDNS(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	record := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: lakeAppObjectMeta(app, "app-custom-dns", app.Namespace)}
	record.Finalizers = []string{"test-cloudflare-cleanup"}
	if err := c.Create(h.ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(h.ctx, app); err != nil {
		t.Fatal(err)
	}
	result, err := h.apps.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("finalizer did not wait for DNS: %+v, %v", result, err)
	}
	if err := c.Get(h.ctx, h.key, &oramav1alpha1.LakeApp{}); err != nil {
		t.Fatal("LakeApp removed before DNS cleanup")
	}
}

func TestLakeAppsTokensRemoveUnboundZetta(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	secret, err := h.apps.reconcileLakeAppTokens(h.ctx, app, lakeAppNamespace(app))
	if err != nil {
		t.Fatal(err)
	}
	source, events := string(secret.Data["source"]), string(secret.Data["events"])
	app.Spec.Bindings.Zetta = nil
	secret, err = h.apps.reconcileLakeAppTokens(h.ctx, app, lakeAppNamespace(app))
	if err != nil || len(secret.Data["zetta"]) != 0 || lakeAppTokenHashes(secret).Zetta != nil {
		t.Fatal("unbound workspace token was retained")
	}
	if string(secret.Data["source"]) != source || string(secret.Data["events"]) != events {
		t.Fatal("unbinding rotated unrelated credentials")
	}
}

func TestPublishSyncedLakeAppEvent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/v1/publish_app_event" || request.Header.Get("Authorization") != "Bearer scoped" {
			t.Error("incorrect app event endpoint or bearer")
		}
		var body struct {
			AppID string       `json:"app_id"`
			Event lakeAppEvent `json:"event"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.AppID != testAppRowID || !body.Event.Ready {
			t.Error("incorrect app event payload")
		}
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	event := lakeAppEvent{Phase: oramav1alpha1.LakeAppPhaseReady, Ready: true}
	if err := publishSyncedLakeAppEvent(context.Background(), server.URL, "scoped", testAppRowID, event); err != nil || calls != 1 {
		t.Fatalf("event not delivered: %v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, server.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if err := publishSyncedLakeAppEvent(context.Background(), redirect.URL, "scoped", testAppRowID, event); err == nil || calls != 1 {
		t.Fatal("publisher followed a redirect with a scoped credential")
	}
}
