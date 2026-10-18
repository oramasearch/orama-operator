/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type dnsRequest struct {
	Method, Path string
	Body         cfDNSRecord
}

// dnsFixture records every HTTP call and retains external state across reconciles.
type dnsFixture struct {
	kube          client.WithWatch
	t             *testing.T
	records       []cfDNSRecord
	requests      []dnsRequest
	pageSize      int
	failMethod    string
	failCode      int
	emptyCreateID bool
	recorder      *record.FakeRecorder
	reconciler    *CloudflareDNSRecordReconciler
	rec           *oramav1alpha1.CloudflareDNSRecord
}

func newDNSFixture(t *testing.T) *dnsFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := oramav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rec := &oramav1alpha1.CloudflareDNSRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "web-dns", Namespace: "apps"},
		Spec:       oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "app.oramadev.net", Content: "lb.example.net", Proxied: true},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "apps"}, Data: map[string][]byte{"token": []byte("test-token"), "zoneID": []byte("test-zone")}}
	f := &dnsFixture{t: t, rec: rec, recorder: record.NewFakeRecorder(10)}
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.CloudflareDNSRecord{}).WithObjects(secret).Build()
	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	f.reconciler = &CloudflareDNSRecordReconciler{Scheme: scheme, Recorder: f.recorder, Client: f.kube, APIBase: server.URL}
	return f
}

func (f *dnsFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var body cfDNSRecord
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode request: %v", err)
		}
	}
	f.requests = append(f.requests, dnsRequest{Method: r.Method, Path: r.URL.Path, Body: body})
	if r.Header.Get("Authorization") != "Bearer test-token" {
		f.t.Error("missing API authorization")
	}
	if r.Method == f.failMethod {
		http.Error(w, "injected API failure", f.failCode)
		return
	}
	const endpoint = "/zones/test-zone/dns_records"
	if r.URL.Path == endpoint && r.Method == http.MethodGet {
		if r.URL.Query().Get("name") != f.rec.Spec.Name || r.URL.Query().Has("type") {
			f.t.Errorf("list query = %s", r.URL.RawQuery)
		}
		page := 1
		if _, err := fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page); err != nil {
			f.t.Errorf("invalid page: %v", err)
		}
		size := f.pageSize
		if size == 0 {
			size = len(f.records) + 1
		}
		start, end := (page-1)*size, page*size
		if start > len(f.records) {
			start = len(f.records)
		}
		if end > len(f.records) {
			end = len(f.records)
		}
		pages := max(1, (len(f.records)+size-1)/size)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": f.records[start:end], "result_info": map[string]int{"page": page, "total_pages": pages}})
		return
	}
	if r.URL.Path == endpoint && r.Method == http.MethodPost {
		var current oramav1alpha1.CloudflareDNSRecord
		if err := f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.rec), &current); err != nil || len(current.Finalizers) != 1 || current.Finalizers[0] != cfDNSFinalizer {
			f.t.Errorf("POST before durable finalizer: %v %+v", err, current.Finalizers)
		}
		if !f.emptyCreateID {
			body.ID = "new-1"
			f.records = append(f.records, body)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": body})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, endpoint+"/")
	for i, remote := range f.records {
		if remote.ID != id {
			continue
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": remote})
		case http.MethodPut:
			body.ID = id
			f.records[i] = body
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": body})
		case http.MethodDelete:
			f.records = append(f.records[:i], f.records[i+1:]...)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]string{"id": id}})
		default:
			f.t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (f *dnsFixture) matching(comment string) cfDNSRecord {
	return cfDNSRecord{ID: "existing-1", Type: "CNAME", Name: f.rec.Spec.Name, Content: f.rec.Spec.Content, Proxied: true, TTL: 1, Comment: comment}
}

func (f *dnsFixture) create() {
	f.t.Helper()
	deleting := !f.rec.DeletionTimestamp.IsZero()
	f.rec.DeletionTimestamp = nil
	if err := f.reconciler.Create(context.Background(), f.rec); err != nil {
		f.t.Fatal(err)
	}
	if deleting {
		if err := f.reconciler.Delete(context.Background(), f.rec); err != nil {
			f.t.Fatal(err)
		}
		if err := f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.rec), f.rec); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *dnsFixture) reconcile() (ctrl.Result, error) {
	f.t.Helper()
	result, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.rec)})
	var current oramav1alpha1.CloudflareDNSRecord
	if getErr := f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.rec), &current); getErr == nil {
		f.rec = &current
	} else if !apierrors.IsNotFound(getErr) {
		f.t.Fatal(getErr)
	}
	return result, err
}

func (f *dnsFixture) methods(want ...string) {
	f.t.Helper()
	got := make([]string, 0, len(f.requests))
	for _, req := range f.requests {
		got = append(got, req.Method)
	}
	if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
		f.t.Fatalf("methods = %v, want %v", got, want)
	}
}

func (f *dnsFixture) status(want oramav1alpha1.CloudflareDNSRecordStatus) {
	f.t.Helper()
	if f.rec.Status != want {
		f.t.Fatalf("status = %+v, want %+v", f.rec.Status, want)
	}
}

func (f *dnsFixture) wantReconcile(retry bool) {
	f.t.Helper()
	result, err := f.reconcile()
	want := ctrl.Result{}
	if retry {
		want.RequeueAfter = 5 * time.Minute
	}
	if err != nil || result != want {
		f.t.Fatalf("reconcile = %+v, %v; want %+v, nil", result, err, want)
	}
}

func TestCloudflareDNSConflicts(t *testing.T) {
	// A TXT resource next to a foreign TXT record is refused too: before, it POSTed a duplicate that
	// Cloudflare rejected, on every pass.
	for _, kind := range []string{"CNAME", "A", "AAAA", "TXT"} {
		for _, identical := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/identical=%t", kind, identical), func(t *testing.T) {
				f := newDNSFixture(t)
				if kind == "TXT" {
					f.rec.Spec.Type = "TXT"
				}
				foreign := f.matching("")
				foreign.Type = kind
				foreign.ID = "prod1"
				if !identical {
					foreign.Content = "foreign.example.net"
				}
				f.records = []cfDNSRecord{foreign}
				f.create()
				f.wantReconcile(true)
				f.methods("GET")
				f.status(oramav1alpha1.CloudflareDNSRecordStatus{Message: (&cfDNSConflictError{Record: foreign}).Error()})
				if f.records[0] != foreign {
					t.Fatal("foreign record changed")
				}
			})
		}
	}
	for _, scenario := range []string{"owned-first", "foreign-first", "later-page", "ambiguous", "lost-comment", "changed-comment"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDNSFixture(t)
			marker := cfDNSOwnerMarker(f.rec)
			owned, foreign := f.matching(marker), f.matching("")
			foreign.ID = "foreign"
			f.records = []cfDNSRecord{owned, foreign}
			switch scenario {
			case "foreign-first":
				f.records = []cfDNSRecord{foreign, owned}
			case "later-page":
				f.pageSize = 1
			case "ambiguous":
				foreign.Comment = marker
				f.records[1] = foreign
			case "lost-comment", "changed-comment":
				f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: owned.ID, OwnerComment: marker, Ready: true, Message: "reconciled"}
				owned.Comment = ""
				if scenario == "changed-comment" {
					owned.Comment = "someone-else"
				}
				f.records = []cfDNSRecord{owned}
			}
			before := append([]cfDNSRecord(nil), f.records...)
			f.create()
			f.wantReconcile(true)
			if f.rec.Status.Ready || f.rec.Status.Message == "" {
				t.Fatalf("status = %+v", f.rec.Status)
			}
			if strings.Contains(scenario, "comment") && (f.rec.Status.OwnerComment != marker || f.rec.Status.RecordID != owned.ID) {
				t.Fatal("lost ownership status")
			}
			if scenario == "later-page" {
				f.methods("GET", "GET")
			} else {
				f.methods("GET")
			}
			if !reflect.DeepEqual(before, f.records) {
				t.Fatal("remote records changed")
			}
		})
	}
}

func TestCloudflareDNSCreateRecoverAndNoStatusFlapping(t *testing.T) {
	for _, scenario := range []string{"create", "txt", "recover", "status-failure", "unproxied"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDNSFixture(t)
			if scenario == "unproxied" {
				f.rec.Spec.Proxied = false
			}
			marker := cfDNSOwnerMarker(f.rec)
			if scenario == "txt" {
				txt := f.matching("")
				txt.Type = "TXT"
				f.records = []cfDNSRecord{txt}
			}
			if scenario == "recover" {
				f.records = []cfDNSRecord{f.matching(marker)}
			}
			f.create()
			statusWrites := 0
			f.reconciler.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					data, err := patch.Data(obj)
					if err != nil {
						return err
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(data, &fields); err != nil {
						return err
					}
					if len(fields) != 1 || fields["metadata"] == nil {
						t.Fatalf("finalizer patch must contain metadata only: %s", data)
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					statusWrites++
					if scenario == "status-failure" && statusWrites == 1 {
						return errors.New("injected status failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})
			if scenario == "status-failure" {
				if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "injected status failure") {
					t.Fatalf("status update error = %v", err)
				}
				f.methods("GET", "POST")
				f.status(oramav1alpha1.CloudflareDNSRecordStatus{})
				f.requests = nil
			}
			f.wantReconcile(false)
			id := "new-1"
			if scenario == "recover" {
				id = "existing-1"
			}
			f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: id, OwnerComment: marker, Ready: true, Message: "reconciled"})
			if scenario == "recover" || scenario == "status-failure" {
				f.methods("GET")
			} else {
				f.methods("GET", "POST")
			}
			if scenario != "recover" {
				created := f.records[len(f.records)-1]
				expected := f.matching(marker)
				expected.Proxied = f.rec.Spec.Proxied
				expected.ID = "new-1"
				if created != expected {
					t.Fatalf("POST = %+v, want %+v", created, expected)
				}
			}
			f.requests = nil
			writes := statusWrites
			f.wantReconcile(false)
			f.methods("GET")
			if statusWrites != writes {
				t.Fatal("unchanged status was rewritten")
			}
		})
	}
}

func TestCloudflareDNSOwnedSettings(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, change := range []string{"none", "content", "type", "proxying", "ttl"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, change), func(t *testing.T) {
				f := newDNSFixture(t)
				marker := cfDNSOwnerMarker(f.rec)
				remote := f.matching(marker)
				f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: remote.ID, OwnerComment: marker}
				if legacy {
					remote.Comment = ""
					f.rec.Status.OwnerComment = ""
				}
				switch change {
				case "content":
					remote.Content = "old.example.net"
				case "type":
					remote.Type = "A"
				case "proxying":
					f.rec.Spec.Proxied = false
				case "ttl":
					remote.TTL = 300
				}
				f.records = []cfDNSRecord{remote}
				f.create()
				f.wantReconcile(false)
				if change == "none" {
					f.methods("GET")
				} else {
					f.methods("GET", "PUT")
					if f.requests[1].Path != "/zones/test-zone/dns_records/existing-1" {
						t.Fatalf("PUT path = %s", f.requests[1].Path)
					}
				}
				want := f.matching(remote.Comment)
				want.Proxied = f.rec.Spec.Proxied
				if f.records[0] != want {
					t.Fatalf("record = %+v, want %+v", f.records[0], want)
				}
				f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: remote.ID, OwnerComment: remote.Comment, Ready: true, Message: "reconciled"})
			})
		}
	}
}

func TestCloudflareDNSMarkerLengthAndType(t *testing.T) {
	for _, size := range []int{100, 101} {
		t.Run(fmt.Sprintf("marker-%d", size), func(t *testing.T) {
			f := newDNSFixture(t)
			f.rec.Name = strings.Repeat("x", size-len("orama-operator:apps/"))
			f.create()
			f.wantReconcile(size > 100)
			if size == 100 {
				f.methods("GET", "POST")
				if len(f.rec.Status.OwnerComment) != 100 {
					t.Fatal("wrong marker length")
				}
			} else {
				f.methods("GET")
				if f.rec.Status.Ready || !strings.Contains(f.rec.Status.Message, "101 characters; maximum is 100") {
					t.Fatalf("status = %+v", f.rec.Status)
				}
			}
		})
	}
	for _, tc := range []struct{ content, explicit, want string }{
		{"192.0.2.1", "", "A"}, {"lb.example.net", "", "CNAME"}, {"2001:db8::1", "", "A"}, {"2001:db8::1", "AAAA", "AAAA"},
	} {
		t.Run(tc.content+tc.explicit, func(t *testing.T) {
			f := newDNSFixture(t)
			f.rec.Spec.Content = tc.content
			f.rec.Spec.Type = tc.explicit
			// A query metacharacter must survive encoding, rather than becoming another filter.
			f.rec.Spec.Name = "test+name&tag.oramadev.net"
			f.create()
			f.wantReconcile(false)
			f.methods("GET", "POST")
			if f.requests[1].Body.Type != tc.want {
				t.Fatalf("type = %s, want %s", f.requests[1].Body.Type, tc.want)
			}
		})
	}
	t.Run("long-legacy-read", func(t *testing.T) {
		f := newDNSFixture(t)
		f.rec.Name = strings.Repeat("x", 100)
		remote := f.matching("")
		f.records = []cfDNSRecord{remote}
		f.rec.Status.RecordID = remote.ID
		f.create()
		f.wantReconcile(false)
		f.methods("GET")
		f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: remote.ID, Ready: true, Message: "reconciled"})
	})
}

func TestCloudflareDNSDeletion(t *testing.T) {
	for _, scenario := range []string{"owned", "legacy", "long-legacy", "lost-comment", "changed-comment", "marked-absent", "legacy-absent", "delete-404", "no-id", "nil-recorder", "retain", "retain-legacy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDNSFixture(t)
			marker := cfDNSOwnerMarker(f.rec)
			remote := f.matching(marker)
			f.rec.Finalizers = []string{cfDNSFinalizer}
			now := metav1.Now()
			f.rec.DeletionTimestamp = &now
			f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: remote.ID, OwnerComment: marker}
			switch scenario {
			case "legacy", "long-legacy", "legacy-absent":
				f.rec.Status.OwnerComment = ""
				remote.Comment = ""
				if scenario == "long-legacy" {
					f.rec.Name = strings.Repeat("x", 100)
				}
			case "lost-comment", "nil-recorder":
				remote.Comment = ""
			case "changed-comment":
				remote.Comment = "someone-else"
			case "no-id":
				f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{}
			case "retain", "retain-legacy":
				// Retain makes no Cloudflare call at all, so it needs no credentials either.
				f.rec.Spec.DeletionPolicy = "Retain"
				f.rec.Spec.SecretRef = "absent"
				if scenario == "retain-legacy" {
					f.rec.Status.OwnerComment = ""
					remote.Comment = ""
				}
			case "delete-404":
				f.failMethod = "DELETE"
				f.failCode = 404
			}
			if scenario == "nil-recorder" {
				f.reconciler.Recorder = nil
			}
			if scenario != "marked-absent" && scenario != "legacy-absent" {
				f.records = []cfDNSRecord{remote}
			}
			f.create()
			f.wantReconcile(false)
			var current oramav1alpha1.CloudflareDNSRecord
			if err := f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.rec), &current); !apierrors.IsNotFound(err) {
				t.Fatalf("finalizer not released: %v %+v", err, current.Finalizers)
			}
			switch scenario {
			case "owned", "delete-404":
				f.methods("GET", "DELETE")
			case "legacy", "long-legacy", "legacy-absent":
				f.methods("DELETE")
			case "no-id", "retain", "retain-legacy":
				f.methods()
			default:
				f.methods("GET")
			}
			if scenario == "lost-comment" || scenario == "changed-comment" {
				select {
				case event := <-f.recorder.Events:
					if !strings.Contains(event, "Warning RecordNotOwned") || !strings.Contains(event, remote.ID) || !strings.Contains(event, marker) || !strings.Contains(event, remote.Name) {
						t.Fatalf("event = %s", event)
					}
				default:
					t.Fatal("missing RecordNotOwned event")
				}
				if len(f.records) != 1 || f.records[0] != remote {
					t.Fatal("unowned record changed")
				}
			}
			if scenario == "owned" || scenario == "legacy" || scenario == "long-legacy" {
				if len(f.records) != 0 {
					t.Fatal("owned record survived cleanup")
				}
			}
			if strings.HasPrefix(scenario, "retain") && (len(f.records) != 1 || f.records[0] != remote) {
				t.Fatal("a Retain record was changed")
			}
		})
	}
}

func TestCloudflareDNSMissingCredentials(t *testing.T) {
	for _, scenario := range []string{"missing-secret", "missing-token", "missing-zone", "empty-ref", "other-namespace", "deleting", "deleting-no-id"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDNSFixture(t)
			sec := &corev1.Secret{}
			if err := f.reconciler.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "cf"}, sec); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing-token":
				delete(sec.Data, "token")
				if err := f.reconciler.Update(context.Background(), sec); err != nil {
					t.Fatal(err)
				}
			case "missing-zone":
				delete(sec.Data, "zoneID")
				if err := f.reconciler.Update(context.Background(), sec); err != nil {
					t.Fatal(err)
				}
			case "empty-ref":
				f.rec.Spec.SecretRef = ""
			case "other-namespace":
				if err := f.reconciler.Delete(context.Background(), sec); err != nil {
					t.Fatal(err)
				}
				sec.Namespace = "elsewhere"
				sec.ResourceVersion = ""
				if err := f.reconciler.Create(context.Background(), sec); err != nil {
					t.Fatal(err)
				}
			default:
				f.rec.Spec.SecretRef = "absent"
			}
			marker := cfDNSOwnerMarker(f.rec)
			f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "tracked", OwnerComment: marker, Ready: true, Message: "reconciled"}
			if strings.HasPrefix(scenario, "deleting") {
				f.rec.Finalizers = []string{cfDNSFinalizer}
				now := metav1.Now()
				f.rec.DeletionTimestamp = &now
				if scenario == "deleting-no-id" {
					f.rec.Status.RecordID = ""
				}
			}
			f.create()
			f.wantReconcile(scenario != "deleting-no-id")
			f.methods()
			if scenario == "deleting-no-id" {
				return
			}
			f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "tracked", OwnerComment: marker, Message: "waiting for Cloudflare credentials (token and zoneID) in spec.secretRef"})
			if len(f.rec.Finalizers) != 1 {
				t.Fatal("missing finalizer while waiting for credentials")
			}
		})
	}
}

func TestCloudflareDNSAPIFailures(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PUT"} {
		for _, code := range []int{403, 429, 500} {
			t.Run(fmt.Sprintf("%s-%d", method, code), func(t *testing.T) {
				f := newDNSFixture(t)
				marker := cfDNSOwnerMarker(f.rec)
				f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "tracked", OwnerComment: marker, Ready: true, Message: "reconciled"}
				if method == "PUT" {
					remote := f.matching(marker)
					remote.Content = "old.example.net"
					f.records = []cfDNSRecord{remote}
				}
				before := append([]cfDNSRecord(nil), f.records...)
				f.failMethod = method
				f.failCode = code
				f.create()
				_, err := f.reconcile()
				var apiErr *cfAPIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
					t.Fatalf("error = %v", err)
				}
				f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "tracked", OwnerComment: marker, Message: err.Error()})
				if method == "GET" {
					f.methods("GET")
				} else {
					f.methods("GET", method)
				}
				if !reflect.DeepEqual(before, f.records) {
					t.Fatal("API failure changed records")
				}
			})
		}
	}
	for _, method := range []string{"GET", "DELETE"} {
		for _, code := range []int{403, 429, 500} {
			t.Run(fmt.Sprintf("deletion-%s-%d", method, code), func(t *testing.T) {
				f := newDNSFixture(t)
				remote := f.matching(cfDNSOwnerMarker(f.rec))
				f.records = []cfDNSRecord{remote}
				f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: remote.ID, OwnerComment: remote.Comment}
				f.rec.Finalizers = []string{cfDNSFinalizer}
				now := metav1.Now()
				f.rec.DeletionTimestamp = &now
				f.failMethod = method
				f.failCode = code
				f.create()
				_, err := f.reconcile()
				var apiErr *cfAPIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
					t.Fatalf("error = %v", err)
				}
				if len(f.rec.Finalizers) != 1 || len(f.records) != 1 || f.records[0] != remote {
					t.Fatal("failed cleanup released ownership")
				}
				if method == "GET" {
					f.methods("GET")
				} else {
					f.methods("GET", "DELETE")
				}
			})
		}
	}
	t.Run("empty-create-id", func(t *testing.T) {
		f := newDNSFixture(t)
		f.emptyCreateID = true
		f.create()
		_, err := f.reconcile()
		if err == nil || !strings.Contains(err.Error(), "empty record ID") {
			t.Fatalf("error = %v", err)
		}
		f.status(oramav1alpha1.CloudflareDNSRecordStatus{Message: err.Error()})
		f.methods("GET", "POST")
	})
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("transport/deleting=%t", deleting), func(t *testing.T) {
			f := newDNSFixture(t)
			f.rec.Status = oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "tracked", OwnerComment: cfDNSOwnerMarker(f.rec)}
			if deleting {
				f.rec.Finalizers = []string{cfDNSFinalizer}
				now := metav1.Now()
				f.rec.DeletionTimestamp = &now
			}
			// A closed local server is a deterministic connection refusal; never call the real API.
			closed := httptest.NewServer(http.NotFoundHandler())
			closed.Close()
			f.reconciler.APIBase = closed.URL
			f.create()
			_, err := f.reconcile()
			if err == nil {
				t.Fatal("missing transport error")
			}
			if len(f.rec.Finalizers) != 1 || f.rec.Status.RecordID != "tracked" || f.rec.Status.OwnerComment == "" || f.rec.Status.Ready {
				t.Fatalf("lost status/finalizer: %+v", f.rec)
			}
			f.methods()
		})
	}
}

// Exercise defaulting and status pruning against a real local API server: the fake
// client alone cannot detect a metadata update restoring the proxied=true default.
func TestCloudflareDNSAPIServerPreservesProxyAndOwnership(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for DNS API-server proof")
	}
	path, err := filepath.Abs("../../config/crd/bases/orama.cloud_cloudflarednsrecords.yaml")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{path}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop DNS envtest: %v", err)
		}
	})
	f := newDNSFixture(t)
	kube, err := client.New(config, client.Options{Scheme: f.reconciler.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.rec.Namespace}}); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.rec.Namespace, Name: f.rec.Spec.SecretRef}, &secret); err != nil {
		t.Fatal(err)
	}
	secret.ResourceVersion = ""
	if err := kube.Create(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	f.reconciler.Client = kube
	// A shared record is never deleted by one of the resources sharing it: the API server refuses
	// sharedOwner without deletionPolicy Retain.
	shared := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: f.rec.Namespace},
		Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "clerk.oramadev.net", Content: "x.example.net", SharedOwner: "clerk/ins_1"}}
	if err := kube.Create(ctx, shared); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "sharedOwner requires deletionPolicy Retain") {
		t.Fatalf("sharedOwner without Retain: %v", err)
	}
	shared.Spec.DeletionPolicy = "Retain"
	if err := kube.Create(ctx, shared); err != nil {
		t.Fatalf("sharedOwner with Retain: %v", err)
	}
	f.create()
	// Raw JSON represents the explicit false present in a user-supplied manifest.
	if err := kube.Patch(ctx, f.rec, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"proxied":false}}`))); err != nil {
		t.Fatal(err)
	}
	f.wantReconcile(false)
	marker := cfDNSOwnerMarker(f.rec)
	f.status(oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "new-1", OwnerComment: marker, Ready: true, Message: "reconciled"})
	f.methods("GET", "POST")
	if f.rec.Spec.Proxied || f.records[0].Proxied {
		t.Fatal("explicit proxied=false was replaced by the CRD default")
	}
	f.requests = nil
	f.wantReconcile(false)
	f.methods("GET")
	if err := kube.Delete(ctx, f.rec); err != nil {
		t.Fatal(err)
	}
	f.requests = nil
	f.wantReconcile(false)
	f.methods("GET", "DELETE")
	var current oramav1alpha1.CloudflareDNSRecord
	if err := kube.Get(ctx, client.ObjectKeyFromObject(f.rec), &current); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not released: %v", err)
	}
	if len(f.records) != 0 {
		t.Fatal("owned record survived deletion")
	}
}

// fakeCloudflareDNS is the DNS records API of one zone: list by name (and type, when given),
// create, read, update and delete by ID. Unlike dnsFixture it serves many names, so a rename can
// be followed from one record to the next.
type fakeCloudflareDNS struct {
	mu      sync.Mutex
	next    int
	records map[string]cfDNSRecord // by ID
}

func (f *fakeCloudflareDNS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, record := range f.records {
		out = append(out, record.Name)
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
	var body cfDNSRecord
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.Method == http.MethodGet && id == "":
		found := []cfDNSRecord{}
		query := r.URL.Query()
		for _, record := range f.records {
			if record.Name == query.Get("name") && (!query.Has("type") || record.Type == query.Get("type")) {
				found = append(found, record)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": found, "result_info": map[string]int{"page": 1, "total_pages": 1}})
	case r.Method == http.MethodPost && id == "":
		f.next++
		body.ID = fmt.Sprintf("record-%d", f.next)
		f.records[body.ID] = body
		reply(body)
	case r.Method == http.MethodGet && f.records[id].ID != "":
		reply(f.records[id])
	case r.Method == http.MethodPut && f.records[id].ID != "":
		body.ID = id
		f.records[id] = body
		reply(body)
	case r.Method == http.MethodDelete && f.records[id].ID != "":
		delete(f.records, id)
		reply(map[string]any{"id": id})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`))
	}
}

// Renaming a record replaces it in Cloudflare: the old name is deleted, never left live (#529,
// found by moving a nonprod Lake's host in #520). The old record is deleted only while it is
// still this resource's: a rename never deletes a record the operator did not create.
func TestRenamingADNSRecordDeletesTheOldOne(t *testing.T) {
	cloudflare := &fakeCloudflareDNS{records: map[string]cfDNSRecord{}}
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
	recorder := record.NewFakeRecorder(10)
	r := &CloudflareDNSRecordReconciler{Client: c, Scheme: c.Scheme(), APIBase: server.URL, Recorder: recorder}
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
	rename := func(name string) {
		t.Helper()
		rec.Spec.Name = name
		if err := c.Update(ctx, rec); err != nil {
			t.Fatal(err)
		}
		reconcile()
	}

	reconcile()
	if got := cloudflare.names(); len(got) != 1 || got[0] != "lake-testing.oramadev.net" || rec.Status.RecordID != "record-1" {
		t.Fatalf("created: records %v, status %+v", got, rec.Status)
	}
	if rec.Status.OwnerComment != "orama-operator:lake-testing/lake-testing-dns" {
		t.Fatalf("owner comment = %q", rec.Status.OwnerComment)
	}

	rename("lake-testing2.oramadev.net")
	if got := cloudflare.names(); len(got) != 1 || got[0] != "lake-testing2.oramadev.net" {
		t.Fatalf("after the rename Cloudflare has %v, want only the new name", got)
	}
	if rec.Status.RecordID != "record-2" || !rec.Status.Ready {
		t.Fatalf("status after the rename: %+v", rec.Status)
	}

	// Someone takes the record over in Cloudflare (its comment no longer names this resource):
	// a rename creates the new record and leaves the taken-over one alone.
	cloudflare.mu.Lock()
	taken := cloudflare.records["record-2"]
	taken.Comment = "managed by hand"
	cloudflare.records["record-2"] = taken
	cloudflare.mu.Unlock()
	rename("lake-testing3.oramadev.net")
	if got := cloudflare.names(); len(got) != 2 {
		t.Fatalf("a rename deleted a record this resource no longer owns: Cloudflare has %v", got)
	}
	if rec.Status.RecordID != "record-3" || !rec.Status.Ready {
		t.Fatalf("status after the second rename: %+v", rec.Status)
	}
	if event := <-recorder.Events; !strings.Contains(event, "Warning RecordNotOwned") || !strings.Contains(event, "record-2") {
		t.Fatalf("event = %q, want RecordNotOwned for record-2", event)
	}

	// A record someone already removed counts as deleted, so the resource can still be released.
	cloudflare.mu.Lock()
	delete(cloudflare.records, "record-3")
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

// A rename of a resource tracked before ownership comments (a legacy record: status.recordID and no
// status.ownerComment) creates the new record with this resource's comment and deletes the old one
// by ID, as before. A Retain resource never deletes the old record: it only stops tracking it.
func TestRenamingALegacyOrRetainedDNSRecord(t *testing.T) {
	for _, scenario := range []string{"legacy", "retain"} {
		t.Run(scenario, func(t *testing.T) {
			old := cfDNSRecord{ID: "legacy-1", Type: "A", Name: "lake-old.oramadev.net", Content: "20.120.187.234", Proxied: true, TTL: 1}
			cloudflare := &fakeCloudflareDNS{records: map[string]cfDNSRecord{old.ID: old}}
			server := httptest.NewServer(cloudflare)
			defer server.Close()
			ctx := context.Background()
			rec := &oramav1alpha1.CloudflareDNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "lake-dns", Namespace: "lake-dev", Finalizers: []string{cfDNSFinalizer}},
				Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "lake-new.oramadev.net",
					Content: "20.120.187.234", Proxied: true},
				Status: oramav1alpha1.CloudflareDNSRecordStatus{RecordID: old.ID, Ready: true, Message: "reconciled"},
			}
			if scenario == "retain" {
				rec.Spec.DeletionPolicy = "Retain"
				old.Comment = "orama-operator:lake-dev/lake-dns"
				cloudflare.records[old.ID] = old
				rec.Status.OwnerComment = old.Comment
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "lake-dev"},
				Data: map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-1")}}
			c := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(rec, secret).WithStatusSubresource(rec).Build()
			r := &CloudflareDNSRecordReconciler{Client: c, Scheme: c.Scheme(), APIBase: server.URL}
			key := client.ObjectKeyFromObject(rec)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, rec); err != nil {
				t.Fatal(err)
			}
			want := oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "record-1", OwnerComment: "orama-operator:lake-dev/lake-dns", Ready: true, Message: "reconciled"}
			if rec.Status != want {
				t.Fatalf("status = %+v, want %+v", rec.Status, want)
			}
			cloudflare.mu.Lock()
			defer cloudflare.mu.Unlock()
			created := cloudflare.records["record-1"]
			if created.Name != "lake-new.oramadev.net" || created.Comment != want.OwnerComment {
				t.Fatalf("new record = %+v", created)
			}
			_, kept := cloudflare.records[old.ID]
			if kept != (scenario == "retain") {
				t.Fatalf("old record kept = %t, records %v", kept, cloudflare.records)
			}
		})
	}
}

// Resources with the same spec.sharedOwner keep one record: the first creates it with the comment
// orama-operator:<sharedOwner>, every other recognises that comment and writes nothing. A record
// made by hand at the name is still refused, and deleting a resource leaves the record (Retain).
func TestSharedOwnerDNSRecord(t *testing.T) {
	for _, scenario := range []string{"shared", "hand-made"} {
		t.Run(scenario, func(t *testing.T) {
			cloudflare := &fakeCloudflareDNS{records: map[string]cfDNSRecord{}}
			if scenario == "hand-made" {
				cloudflare.records["hand-1"] = cfDNSRecord{ID: "hand-1", Type: "CNAME", Name: "clerk.lake.live", Content: "frontend-api.clerk.services", TTL: 1}
			}
			server := httptest.NewServer(cloudflare)
			defer server.Close()
			ctx := context.Background()
			resource := func(ns string) *oramav1alpha1.CloudflareDNSRecord {
				return &oramav1alpha1.CloudflareDNSRecord{
					ObjectMeta: metav1.ObjectMeta{Name: "lake-" + ns + "-clerk-1", Namespace: ns},
					Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "clerk.lake.live", Type: "CNAME",
						Content: "frontend-api.clerk.services", DeletionPolicy: "Retain", SharedOwner: "clerk/ins_1"},
				}
			}
			first, second := resource("eu"), resource("us")
			objects := []client.Object{first, second}
			for _, ns := range []string{"eu", "us"} {
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: ns},
					Data: map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-1")}})
			}
			c := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(objects...).WithStatusSubresource(first).Build()
			r := &CloudflareDNSRecordReconciler{Client: c, Scheme: c.Scheme(), APIBase: server.URL}
			for _, rec := range []*oramav1alpha1.CloudflareDNSRecord{first, second, first} {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rec)}); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(rec), rec); err != nil {
					t.Fatal(err)
				}
			}
			cloudflare.mu.Lock()
			records := len(cloudflare.records)
			cloudflare.mu.Unlock()
			if records != 1 {
				t.Fatalf("Cloudflare records = %d, want 1", records)
			}
			for _, rec := range []*oramav1alpha1.CloudflareDNSRecord{first, second} {
				if scenario == "hand-made" {
					if rec.Status.Ready || rec.Status.RecordID != "" || !strings.Contains(rec.Status.Message, "not owned") {
						t.Fatalf("%s took over a hand-made record: %+v", rec.Namespace, rec.Status)
					}
					continue
				}
				want := oramav1alpha1.CloudflareDNSRecordStatus{RecordID: "record-1", OwnerComment: "orama-operator:clerk/ins_1", Ready: true, Message: "reconciled"}
				if rec.Status != want {
					t.Fatalf("%s status = %+v, want %+v", rec.Namespace, rec.Status, want)
				}
			}
			if scenario == "hand-made" {
				return
			}
			if err := c.Delete(ctx, first); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(first)}); err != nil {
				t.Fatal(err)
			}
			if got := cloudflare.names(); len(got) != 1 {
				t.Fatalf("deleting one sharing resource changed Cloudflare: %v", got)
			}
		})
	}
}
