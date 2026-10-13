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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeCloudflareDNS stands in for the Cloudflare DNS records API: it keeps records per zone and
// logs every request as "METHOD zone path".
type fakeCloudflareDNS struct {
	mu       sync.Mutex
	records  map[string]map[string]cfDNSRecord // zone -> id -> record
	requests []string
	bodies   []map[string]interface{}
	nextID   int
}

// newFakeCloudflareDNS starts the fake and points cfAPIBase at it for the test's duration.
func newFakeCloudflareDNS(t *testing.T) *fakeCloudflareDNS {
	t.Helper()
	f := &fakeCloudflareDNS{records: map[string]map[string]cfDNSRecord{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	previous := cfAPIBase
	cfAPIBase = srv.URL
	t.Cleanup(func() { cfAPIBase = previous; srv.Close() })
	return f
}

func (f *fakeCloudflareDNS) seed(zone string, rec cfDNSRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records[zone] == nil {
		f.records[zone] = map[string]cfDNSRecord{}
	}
	f.records[zone][rec.ID] = rec
}

func (f *fakeCloudflareDNS) get(zone, id string) (cfDNSRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[zone][id]
	return rec, ok
}

// writes returns the logged POST, PUT and DELETE requests.
func (f *fakeCloudflareDNS) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if !strings.HasPrefix(r, http.MethodGet) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCloudflareDNS) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Path: /zones/<zone>/dns_records[/<id>]
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "zones" || parts[2] != "dns_records" {
		http.Error(w, "unexpected path", http.StatusBadRequest)
		return
	}
	zone, id := parts[1], ""
	if len(parts) == 4 {
		id = parts[3]
	}
	f.requests = append(f.requests, req.Method+" "+zone+" "+req.URL.Path)
	if f.records[zone] == nil {
		f.records[zone] = map[string]cfDNSRecord{}
	}
	var body map[string]interface{}
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&body)
	}
	if body != nil {
		f.bodies = append(f.bodies, body)
	}
	toRecord := func(id string) cfDNSRecord {
		comment, _ := body["comment"].(string)
		proxied, _ := body["proxied"].(bool)
		ttl, _ := body["ttl"].(float64)
		return cfDNSRecord{ID: id, Type: body["type"].(string), Name: body["name"].(string),
			Content: body["content"].(string), Proxied: proxied, TTL: int(ttl), Comment: comment}
	}
	reply := func(result interface{}) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "result": result})
	}
	switch {
	case req.Method == http.MethodGet && id == "":
		name := req.URL.Query().Get("name")
		matches := []cfDNSRecord{}
		for _, rec := range f.records[zone] {
			if rec.Name == name {
				matches = append(matches, rec)
			}
		}
		reply(matches)
	case req.Method == http.MethodGet:
		rec, ok := f.records[zone][id]
		if !ok {
			http.Error(w, `{"success":false}`, http.StatusNotFound)
			return
		}
		reply(rec)
	case req.Method == http.MethodPost:
		f.nextID++
		rec := toRecord(fmt.Sprintf("new-%d", f.nextID))
		f.records[zone][rec.ID] = rec
		reply(rec)
	case req.Method == http.MethodPut:
		rec := toRecord(id)
		f.records[zone][id] = rec
		reply(rec)
	case req.Method == http.MethodDelete:
		delete(f.records[zone], id)
		reply(map[string]string{"id": id})
	default:
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}

// dnsRecordFixture builds a reconciler over a fake client holding the CF Secret (zone "zone-secret")
// and the given CloudflareDNSRecord.
func dnsRecordFixture(t *testing.T, rec *oramav1alpha1.CloudflareDNSRecord) (client.Client, *CloudflareDNSRecordReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := newLakeAppScheme(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: rec.Namespace},
		Data: map[string][]byte{"token": []byte("test-token"), "zoneID": []byte("zone-secret")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, rec).
		WithStatusSubresource(&oramav1alpha1.CloudflareDNSRecord{}).Build()
	recorder := record.NewFakeRecorder(10)
	return cl, &CloudflareDNSRecordReconciler{Client: cl, Scheme: scheme, Recorder: recorder}, recorder
}

func newDNSRecord(status oramav1alpha1.CloudflareDNSRecordStatus) *oramav1alpha1.CloudflareDNSRecord {
	return &oramav1alpha1.CloudflareDNSRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "web-dns", Namespace: "apps"},
		Spec:       oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "app.example.com", Content: "lb.example.net", Proxied: true},
		Status:     status,
	}
}

const testDNSMarker = "orama-operator:apps/web-dns"

func reconcileDNS(t *testing.T, cl client.Client, r *CloudflareDNSRecordReconciler) (ctrl.Result, *oramav1alpha1.CloudflareDNSRecord) {
	t.Helper()
	key := types.NamespacedName{Namespace: "apps", Name: "web-dns"}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	rec := &oramav1alpha1.CloudflareDNSRecord{}
	if err := cl.Get(context.Background(), key, rec); err != nil {
		t.Fatal(err)
	}
	return res, rec
}

func TestCloudflareDNSRecordNeverAdoptsUnmarkedRecord(t *testing.T) {
	for _, existingType := range []string{"CNAME", "A"} {
		t.Run(existingType, func(t *testing.T) {
			cf := newFakeCloudflareDNS(t)
			cf.seed("zone-secret", cfDNSRecord{ID: "prod1", Type: existingType, Name: "app.example.com", Content: "cname.vercel-dns.com", TTL: 1})
			cl, r, _ := dnsRecordFixture(t, newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{}))
			res, rec := reconcileDNS(t, cl, r)
			if writes := cf.writes(); len(writes) != 0 {
				t.Fatalf("foreign record was written: %v", writes)
			}
			if rec.Status.Ready || rec.Status.RecordID != "" || !strings.Contains(rec.Status.Message, "prod1") || !strings.Contains(rec.Status.Message, existingType) {
				t.Fatalf("status = %+v", rec.Status)
			}
			if res.RequeueAfter == 0 {
				t.Error("conflict is not retried")
			}
			if got, _ := cf.get("zone-secret", "prod1"); got.Content != "cname.vercel-dns.com" || got.Comment != "" {
				t.Errorf("foreign record changed: %+v", got)
			}
		})
	}
}

func TestCloudflareDNSRecordCreatesMarkedRecord(t *testing.T) {
	cf := newFakeCloudflareDNS(t)
	cl, r, _ := dnsRecordFixture(t, newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{}))
	_, rec := reconcileDNS(t, cl, r)
	if writes := cf.writes(); len(writes) != 1 || !strings.HasPrefix(writes[0], http.MethodPost) {
		t.Fatalf("writes = %v", writes)
	}
	if cf.bodies[0]["comment"] != testDNSMarker {
		t.Errorf("POST body = %v", cf.bodies[0])
	}
	if !rec.Status.Ready || rec.Status.RecordID != "new-1" || rec.Status.OwnerComment != testDNSMarker || rec.Status.ZoneID != "zone-secret" {
		t.Fatalf("status = %+v", rec.Status)
	}
	// A steady record sends no further writes.
	reconcileDNS(t, cl, r)
	if writes := cf.writes(); len(writes) != 1 {
		t.Errorf("steady record rewritten: %v", writes)
	}
}

// A record created before ownership comments, found through status.recordID after the upgrade
// (status.zoneID and status.ownerComment empty), is recognised without any write: no comment is
// added, and a TXT record at the same name is not a conflict.
func TestCloudflareDNSRecordLegacyRecordUnchangedSendsOnlyGET(t *testing.T) {
	cf := newFakeCloudflareDNS(t)
	cf.seed("zone-secret", cfDNSRecord{ID: "legacy", Type: "CNAME", Name: "app.example.com", Content: "lb.example.net", Proxied: true, TTL: 1})
	cf.seed("zone-secret", cfDNSRecord{ID: "txt", Type: "TXT", Name: "app.example.com", Content: "verification", TTL: 1})
	cl, r, _ := dnsRecordFixture(t, newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "legacy", Ready: true, Message: "reconciled"}))
	for range 2 {
		_, rec := reconcileDNS(t, cl, r)
		if !rec.Status.Ready || rec.Status.RecordID != "legacy" || rec.Status.ZoneID != "zone-secret" || rec.Status.OwnerComment != "" {
			t.Fatalf("status = %+v", rec.Status)
		}
	}
	if writes := cf.writes(); len(writes) != 0 {
		t.Fatalf("unchanged legacy record was written: %v", writes)
	}
	if got, _ := cf.get("zone-secret", "legacy"); got.Comment != "" {
		t.Errorf("legacy record gained a comment: %+v", got)
	}
}

// A legacy record whose content differs from the spec is rewritten as before, keeping its comment.
func TestCloudflareDNSRecordLegacyRecordRewrittenWithoutComment(t *testing.T) {
	cf := newFakeCloudflareDNS(t)
	cf.seed("zone-secret", cfDNSRecord{ID: "legacy", Type: "CNAME", Name: "app.example.com", Content: "old-lb.example.net", Proxied: true, TTL: 1})
	cl, r, _ := dnsRecordFixture(t, newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "legacy", Ready: true, Message: "reconciled"}))
	_, rec := reconcileDNS(t, cl, r)
	if writes := cf.writes(); len(writes) != 1 || !strings.HasPrefix(writes[0], http.MethodPut) || !strings.HasSuffix(writes[0], "/legacy") {
		t.Fatalf("writes = %v", writes)
	}
	if got, _ := cf.get("zone-secret", "legacy"); got.Comment != "" || got.Content != "lb.example.net" {
		t.Errorf("legacy record = %+v", got)
	}
	if !rec.Status.Ready || rec.Status.RecordID != "legacy" || rec.Status.OwnerComment != "" {
		t.Errorf("status = %+v", rec.Status)
	}
}

// An existing record is never moved between zones: the reconcile sends nothing to Cloudflare and
// reports ready=false, whether the old zone is recorded in status or is the Secret's (legacy).
func TestCloudflareDNSRecordRefusesZoneChange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusZone string
		oldZone    string
	}{
		{"legacy status", "", "zone-secret"},
		{"recorded zone", "zone-a", "zone-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cf := newFakeCloudflareDNS(t)
			cf.seed(tc.oldZone, cfDNSRecord{ID: "rec", Type: "CNAME", Name: "app.example.com", Content: "lb.example.net", Proxied: true, TTL: 1})
			obj := newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "rec", ZoneID: tc.statusZone, Ready: true, Message: "reconciled"})
			obj.Spec.ZoneID = "zone-new"
			cl, r, _ := dnsRecordFixture(t, obj)
			res, rec := reconcileDNS(t, cl, r)
			if len(cf.requests) != 0 {
				t.Fatalf("zone change sent requests: %v", cf.requests)
			}
			if rec.Status.Ready || rec.Status.RecordID != "rec" || !strings.Contains(rec.Status.Message, "zone change refused") {
				t.Fatalf("status = %+v", rec.Status)
			}
			if res.RequeueAfter == 0 {
				t.Error("refused zone change is not retried")
			}
			if _, ok := cf.get(tc.oldZone, "rec"); !ok {
				t.Error("record removed from its zone")
			}
		})
	}
}

// Naming the zone a legacy record already lives in (the Secret's) changes nothing in Cloudflare.
func TestCloudflareDNSRecordSameZoneSendsOnlyGET(t *testing.T) {
	cf := newFakeCloudflareDNS(t)
	cf.seed("zone-secret", cfDNSRecord{ID: "legacy", Type: "CNAME", Name: "app.example.com", Content: "lb.example.net", Proxied: true, TTL: 1})
	obj := newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "legacy", Ready: true, Message: "reconciled"})
	obj.Spec.ZoneID = "zone-secret"
	cl, r, _ := dnsRecordFixture(t, obj)
	_, rec := reconcileDNS(t, cl, r)
	if writes := cf.writes(); len(writes) != 0 {
		t.Fatalf("writes = %v", writes)
	}
	if !rec.Status.Ready || rec.Status.RecordID != "legacy" || rec.Status.ZoneID != "zone-secret" {
		t.Errorf("status = %+v", rec.Status)
	}
}

func TestCloudflareDNSRecordDeletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		comment      string
		ownerComment string
		wantDeleted  bool
	}{
		{"owned", testDNSMarker, testDNSMarker, true},
		{"marker removed", "", testDNSMarker, false},
		{"marker changed", "orama-operator:other/web-dns", testDNSMarker, false},
		{"legacy record", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cf := newFakeCloudflareDNS(t)
			cf.seed("zone-secret", cfDNSRecord{ID: "rec", Type: "CNAME", Name: "app.example.com", Content: "lb.example.net", Proxied: true, TTL: 1, Comment: tc.comment})
			obj := newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "rec", OwnerComment: tc.ownerComment, Ready: true})
			obj.Finalizers = []string{cfDNSFinalizer}
			cl, r, recorder := dnsRecordFixture(t, obj)
			if err := cl.Delete(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Namespace: "apps", Name: "web-dns"}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(context.Background(), key, &oramav1alpha1.CloudflareDNSRecord{}); err == nil {
				t.Error("finalizer not released")
			}
			writes := cf.writes()
			_, stillThere := cf.get("zone-secret", "rec")
			if tc.wantDeleted {
				if len(writes) != 1 || !strings.HasPrefix(writes[0], http.MethodDelete) || stillThere {
					t.Errorf("owned record not deleted: %v", writes)
				}
				return
			}
			if len(writes) != 0 || !stillThere {
				t.Errorf("record owned by someone else was deleted: %v", writes)
			}
			if len(recorder.Events) != 1 {
				t.Error("no event for the record left in place")
			}
		})
	}
}

func TestCloudflareDNSRecordSpecZoneOverridesSecret(t *testing.T) {
	cf := newFakeCloudflareDNS(t)
	obj := newDNSRecord(oramav1alpha1.CloudflareDNSRecordStatus{})
	obj.Spec.ZoneID = "zone-spec"
	cl, r, _ := dnsRecordFixture(t, obj)
	reconcileDNS(t, cl, r)
	if err := cl.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "apps", Name: "web-dns"}}); err != nil {
		t.Fatal(err)
	}
	if len(cf.requests) < 4 { // list, POST, GET, DELETE
		t.Fatalf("requests = %v", cf.requests)
	}
	for _, req := range cf.requests {
		if strings.Split(req, " ")[1] != "zone-spec" {
			t.Errorf("request outside spec.zoneID: %s", req)
		}
	}
}

func TestCloudflareZoneIDForHost(t *testing.T) {
	zones := map[string]string{"orama.ing": "A", "orama.com": "B", "eu.cluster.orama.ing": "C"}
	for host, want := range map[string]string{
		"app.orama.ing":               "A",
		"app.orama.com":               "B",
		"orama.com":                   "B",
		"writer.eu.cluster.orama.ing": "C",
		"notorama.com":                "",
		"app.example.net":             "",
	} {
		if got := cfZoneIDForHost(zones, host); got != want {
			t.Errorf("%s: zone %q, want %q", host, got, want)
		}
	}
}

func TestCloudV1DNSZonePerHost(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Host = "app.orama.ing"
		o.Spec.Components.Engine.Reader.Host = "app.orama.com"
		o.Spec.Components.Engine.Writer.Host = "writer.example.net"
		o.Spec.Cloudflare = oramav1alpha1.CloudflareSpec{Enabled: true, ManageDNS: true, APITokenSecretRef: &corev1.LocalObjectReference{Name: "cf"},
			ZoneIDs: map[string]string{"orama.ing": "A", "orama.com": "B"}}
	})
	cl, r := reconcileOCV1(t, oc)
	ctx := context.Background()
	for _, component := range []string{"dashboard", "writer", "reader"} {
		ing := getObj(t, cl, "v1-"+component, &networkingv1.Ingress{})
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}}
		if err := cl.Status().Update(ctx, ing); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileCloudflare(ctx, oc); err != nil {
		t.Fatal(err)
	}
	for component, want := range map[string]string{"dashboard": "A", "reader": "B", "writer": ""} {
		if got := getObj(t, cl, "v1-"+component+"-dns", &oramav1alpha1.CloudflareDNSRecord{}).Spec.ZoneID; got != want {
			t.Errorf("%s zoneID = %q, want %q", component, got, want)
		}
	}
}

// Adding cloudflare.zoneIDs for a host whose record already exists only updates the child's
// spec.zoneID: the child is not finalized, so its record is never deleted and recreated (the child
// controller then refuses a real zone move, or does nothing when the zone is unchanged).
func TestCloudV1DNSZoneChangeKeepsExistingChild(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Host = "app.orama.com"
		o.Spec.Cloudflare = oramav1alpha1.CloudflareSpec{Enabled: true, ManageDNS: true, APITokenSecretRef: &corev1.LocalObjectReference{Name: "cf"}}
	})
	cl, r := reconcileOCV1(t, oc)
	ctx := context.Background()
	ing := getObj(t, cl, "v1-dashboard", &networkingv1.Ingress{})
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := cl.Status().Update(ctx, ing); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileCloudflare(ctx, oc); err != nil {
		t.Fatal(err)
	}
	child := getObj(t, cl, "v1-dashboard-dns", &oramav1alpha1.CloudflareDNSRecord{})
	child.Finalizers = []string{cfDNSFinalizer}
	if err := cl.Update(ctx, child); err != nil {
		t.Fatal(err)
	}
	child.Status.RecordID, child.Status.Ready = "legacy", true
	if err := cl.Status().Update(ctx, child); err != nil {
		t.Fatal(err)
	}

	oc.Spec.Cloudflare.ZoneIDs = map[string]string{"orama.com": "zone-com"}
	if err := r.reconcileCloudflare(ctx, oc); err != nil {
		t.Fatal(err)
	}
	child = getObj(t, cl, "v1-dashboard-dns", &oramav1alpha1.CloudflareDNSRecord{})
	if !child.DeletionTimestamp.IsZero() || child.Status.RecordID != "legacy" || child.Spec.ZoneID != "zone-com" {
		t.Fatalf("child = deleting %v, status %+v, spec.zoneID %q", !child.DeletionTimestamp.IsZero(), child.Status, child.Spec.ZoneID)
	}
}
