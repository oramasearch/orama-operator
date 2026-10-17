/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeCloudflareDNS is the DNS records API of one zone: list by type and name, create, update and
// delete by ID.
type fakeCloudflareDNS struct {
	mu      sync.Mutex
	next    int
	records map[string]map[string]any // by ID
}

func (f *fakeCloudflareDNS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, record := range f.records {
		out = append(out, record["name"].(string))
	}
	return out
}

func (f *fakeCloudflareDNS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const prefix = "/zones/zone-1/dns_records"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	reply := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
	switch {
	case r.Method == http.MethodGet && id == "":
		var found []map[string]any
		for recordID, record := range f.records {
			if record["name"] == r.URL.Query().Get("name") && record["type"] == r.URL.Query().Get("type") {
				found = append(found, map[string]any{"id": recordID})
			}
		}
		reply(found)
	case r.Method == http.MethodPost && id == "":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.next++
		recordID := fmt.Sprintf("record-%d", f.next)
		f.records[recordID] = body
		reply(map[string]any{"id": recordID})
	case r.Method == http.MethodPut && f.records[id] != nil:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.records[id] = body
		reply(map[string]any{"id": id})
	case r.Method == http.MethodDelete && f.records[id] != nil:
		delete(f.records, id)
		reply(map[string]any{"id": id})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`))
	}
}

// Renaming a record replaces it in Cloudflare: the old name is deleted, never left live (#529,
// found by moving a nonprod Lake's host in #520).
func TestRenamingADNSRecordDeletesTheOldOne(t *testing.T) {
	cloudflare := &fakeCloudflareDNS{records: map[string]map[string]any{}}
	server := httptest.NewServer(cloudflare)
	defer server.Close()

	ctx := context.Background()
	rec := &oramav1alpha1.CloudflareDNSRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-testing-dns", Namespace: "lake-testing"},
		Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cloudflare-api-token", Name: "lake-testing.oramadev.net",
			Content: "20.120.187.234", Proxied: true},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-api-token", Namespace: "lake-testing"},
		Data: map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-1")}}
	c := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(rec, secret).WithStatusSubresource(rec).Build()
	r := &CloudflareDNSRecordReconciler{Client: c, Scheme: c.Scheme(), APIBase: server.URL}
	key := types.NamespacedName{Namespace: "lake-testing", Name: "lake-testing-dns"}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, key, rec); err != nil {
			t.Fatal(err)
		}
	}

	reconcile()
	if got := cloudflare.names(); len(got) != 1 || got[0] != "lake-testing.oramadev.net" || rec.Status.RecordID != "record-1" {
		t.Fatalf("created: records %v, status %+v", got, rec.Status)
	}

	rec.Spec.Name = "lake-testing2.oramadev.net"
	if err := c.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if got := cloudflare.names(); len(got) != 1 || got[0] != "lake-testing2.oramadev.net" {
		t.Fatalf("after the rename Cloudflare has %v, want only the new name", got)
	}
	if rec.Status.RecordID != "record-2" || !rec.Status.Ready {
		t.Fatalf("status after the rename: %+v", rec.Status)
	}

	// A record someone already removed counts as deleted, so the resource can still be released.
	cloudflare.mu.Lock()
	delete(cloudflare.records, "record-2")
	cloudflare.mu.Unlock()
	if err := c.Delete(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("deleting a resource whose record is already gone: %v", err)
	}
	if err := c.Get(ctx, key, rec); err == nil {
		t.Fatal("the resource was not released")
	}
}
