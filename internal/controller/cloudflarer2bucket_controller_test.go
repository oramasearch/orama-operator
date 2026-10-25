/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeR2 is the part of Cloudflare's API the bucket controller calls.
type fakeR2 struct {
	t        *testing.T
	mu       sync.Mutex
	buckets  map[string]bool
	tokens   map[string]map[string]any // id -> create body
	revoked  []string
	minted   int
	config   map[string]json.RawMessage
	puts     map[string]int
	fail     string
	requests []string
	domains  map[string]map[string]any // domain -> stored settings
}

func (f *fakeR2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer account-token" {
		http.Error(w, `{"success":false}`, http.StatusForbidden)
		return
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	path := strings.TrimPrefix(r.URL.Path, "/accounts/acct")
	switch {
	case strings.HasSuffix(path, "/cors") || strings.HasSuffix(path, "/lifecycle"):
		if f.fail == r.Method+" "+path {
			http.Error(w, `{"success":false,"errors":[{"message":"configuration denied"}]}`, http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			body := f.config[path]
			if body == nil {
				if strings.HasSuffix(path, "/cors") {
					http.Error(w, `{"success":false,"errors":[{"code":10059,"message":"The CORS configuration does not exist."}]}`, http.StatusNotFound)
				} else {
					http.Error(w, `{"success":false,"errors":[{"message":"The lifecycle configuration does not exist."}]}`, http.StatusNotFound)
				}
				return
			}
			fmt.Fprintf(w, `{"success":true,"result":%s}`, body)
		case http.MethodPut:
			var body json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Error(err)
			}
			f.config[path] = body
			f.puts[path]++
			fmt.Fprint(w, `{"success":true}`)
		default:
			f.t.Errorf("unexpected configuration method %s", r.Method)
		}
	case strings.Contains(path, "/domains/custom"):
		if f.domains == nil {
			f.domains = map[string]map[string]any{}
		}
		domain := path[strings.Index(path, "/domains/custom")+len("/domains/custom"):]
		domain = strings.TrimPrefix(domain, "/")
		switch r.Method {
		case http.MethodGet:
			d, ok := f.domains[domain]
			if !ok {
				http.Error(w, `{"success":false,"errors":[{"message":"not found"}]}`, http.StatusNotFound)
				return
			}
			out, _ := json.Marshal(map[string]any{"domain": domain, "enabled": d["enabled"], "minTLS": d["minTLS"],
				"status": map[string]string{"ownership": "active", "ssl": "active"}})
			fmt.Fprintf(w, `{"success":true,"result":%s}`, out)
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.domains[body["domain"].(string)] = body
			fmt.Fprint(w, `{"success":true}`)
		case http.MethodPut:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body {
				f.domains[domain][k] = v
			}
			fmt.Fprint(w, `{"success":true}`)
		case http.MethodDelete:
			if _, ok := f.domains[domain]; !ok {
				http.Error(w, `{"success":false}`, http.StatusNotFound)
				return
			}
			delete(f.domains, domain)
			fmt.Fprint(w, `{"success":true}`)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/r2/buckets/"):
		if !f.buckets[strings.TrimPrefix(path, "/r2/buckets/")] {
			http.Error(w, `{"success":false}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"success":true}`)
	case r.Method == http.MethodPost && path == "/r2/buckets":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.buckets[body["name"]] = true
		fmt.Fprint(w, `{"success":true}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/r2/buckets/"):
		delete(f.buckets, strings.TrimPrefix(path, "/r2/buckets/"))
		fmt.Fprint(w, `{"success":true}`)
	case r.Method == http.MethodGet && path == "/tokens/permission_groups":
		fmt.Fprint(w, `{"success":true,"result":[{"id":"pg-read","name":"Workers R2 Storage Bucket Item Read"},{"id":"pg-write","name":"Workers R2 Storage Bucket Item Write"}]}`)
	case r.Method == http.MethodPost && path == "/tokens":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.minted++
		id := fmt.Sprintf("tok-%d", f.minted)
		f.tokens[id] = body
		fmt.Fprintf(w, `{"success":true,"result":{"id":%q,"value":"value-%d"}}`, id, f.minted)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/tokens/"):
		id := strings.TrimPrefix(path, "/tokens/")
		if _, ok := f.tokens[id]; !ok {
			http.Error(w, `{"success":false}`, http.StatusNotFound)
			return
		}
		delete(f.tokens, id)
		f.revoked = append(f.revoked, id)
		fmt.Fprint(w, `{"success":true}`)
	default:
		f.t.Errorf("unexpected Cloudflare call %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func newR2Fixture(t *testing.T, bucket *oramav1alpha1.CloudflareR2Bucket) (*CloudflareR2BucketReconciler, client.Client, *fakeR2) {
	t.Helper()
	scheme := backupTestScheme(t)
	account := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare", Namespace: "backups"}, Data: map[string][]byte{"token": []byte("account-token"), "accountID": []byte("acct")}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.CloudflareR2Bucket{}).WithObjects(account, bucket).Build()
	cf := &fakeR2{t: t, config: map[string]json.RawMessage{}, puts: map[string]int{}, buckets: map[string]bool{}, tokens: map[string]map[string]any{}}
	server := httptest.NewServer(cf)
	t.Cleanup(server.Close)
	return &CloudflareR2BucketReconciler{Client: kube, Scheme: scheme, APIBase: server.URL}, kube, cf
}

func testR2Bucket(policy oramav1alpha1.BucketDeletionPolicy, credentials bool) *oramav1alpha1.CloudflareR2Bucket {
	b := &oramav1alpha1.CloudflareR2Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-backups", Namespace: "backups"},
		Spec:       oramav1alpha1.CloudflareR2BucketSpec{SecretRef: "cloudflare", Name: "lake-backups", DeletionPolicy: policy},
	}
	if credentials {
		b.Spec.Credentials = &oramav1alpha1.CloudflareR2BucketCredentials{SecretName: "lake-backups-s3"}
	}
	return b
}

func reconcileR2(t *testing.T, r *CloudflareR2BucketReconciler, b *oramav1alpha1.CloudflareR2Bucket) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestR2BucketMintsCredentialsScopedToItself(t *testing.T) {
	b := testR2Bucket(oramav1alpha1.BucketDeletionRetain, true)
	r, kube, cf := newR2Fixture(t, b)
	reconcileR2(t, r, b)
	if !b.Status.Ready || !cf.buckets["lake-backups"] || b.Status.CredentialsTokenID != "tok-1" || b.Status.CredentialsSecret != "lake-backups-s3" {
		t.Fatalf("status = %+v, buckets %v", b.Status, cf.buckets)
	}
	// The token's one policy: object write on this bucket alone.
	policies := cf.tokens["tok-1"]["policies"].([]any)
	policy := policies[0].(map[string]any)
	resources := policy["resources"].(map[string]any)
	groups := policy["permission_groups"].([]any)
	if len(policies) != 1 || len(resources) != 1 || resources["com.cloudflare.edge.r2.bucket.acct_default_lake-backups"] != "*" ||
		len(groups) != 1 || groups[0].(map[string]any)["id"] != "pg-write" {
		t.Fatalf("token body = %v", cf.tokens["tok-1"])
	}
	var secret corev1.Secret
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "backups", Name: "lake-backups-s3"}, &secret); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("value-1"))
	if string(secret.Data["accessKeyId"]) != "tok-1" || string(secret.Data["secretAccessKey"]) != hex.EncodeToString(sum[:]) ||
		string(secret.Data["endpoint"]) != "https://acct.r2.cloudflarestorage.com" || string(secret.Data["bucket"]) != "lake-backups" ||
		!metav1.IsControlledBy(&secret, b) {
		t.Fatalf("secret = %v", secret.Data)
	}

	// Steady: no new token.
	reconcileR2(t, r, b)
	if cf.minted != 1 {
		t.Fatalf("minted %d tokens; a steady bucket mints one", cf.minted)
	}
	// The Secret is lost: the value cannot be read back, so a new token replaces the old one.
	if err := kube.Delete(context.Background(), &secret); err != nil {
		t.Fatal(err)
	}
	reconcileR2(t, r, b)
	if cf.minted != 2 || len(cf.revoked) != 1 || cf.revoked[0] != "tok-1" || b.Status.CredentialsTokenID != "tok-2" {
		t.Fatalf("minted %d, revoked %v, status %+v", cf.minted, cf.revoked, b.Status)
	}
	// Credentials removed from the spec: the token is revoked and the Secret deleted.
	b.Spec.Credentials = nil
	if err := kube.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	reconcileR2(t, r, b)
	if len(cf.tokens) != 0 || b.Status.CredentialsTokenID != "" {
		t.Fatalf("tokens %v, status %+v", cf.tokens, b.Status)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "backups", Name: "lake-backups-s3"}, &secret); !apierrors.IsNotFound(err) {
		t.Fatalf("credentials Secret survived: %v", err)
	}
}

func TestR2BucketDeletionPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy     oramav1alpha1.BucketDeletionPolicy
		bucketKept bool
	}{
		{oramav1alpha1.BucketDeletionRetain, true},
		{oramav1alpha1.BucketDeletionDelete, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			b := testR2Bucket(tc.policy, true)
			r, kube, cf := newR2Fixture(t, b)
			reconcileR2(t, r, b) // finalizer, bucket, token
			if err := kube.Delete(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			reconcileR2(t, r, b)
			if cf.buckets["lake-backups"] != tc.bucketKept {
				t.Fatalf("bucket exists = %v, want %v", cf.buckets["lake-backups"], tc.bucketKept)
			}
			if len(cf.tokens) != 0 {
				t.Fatalf("the bucket token must be revoked whatever the policy: %v", cf.tokens)
			}
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(b), b); !apierrors.IsNotFound(err) {
				t.Fatalf("resource not released: %v", err)
			}
		})
	}
}

// Configured buckets converge once and compare provider ordering/defaults by their meaning.
func TestR2BucketCORSAndExpiry(t *testing.T) {
	b := testR2Bucket(oramav1alpha1.BucketDeletionDelete, false)
	days := int32(7)
	b.Spec.ExpireAfterDays = &days
	b.Spec.CORS = []oramav1alpha1.CloudflareR2CORSRule{
		{AllowedOrigins: []string{"https://app.example", "https://extra.example"}, AllowedMethods: []string{"PUT", "GET"}, AllowedHeaders: []string{"X-Upload", "Content-Type"}, MaxAgeSeconds: 3600},
		{AllowedOrigins: []string{"https://other.example"}, AllowedMethods: []string{"PUT"}},
	}
	r, kube, cf := newR2Fixture(t, b)
	const corsPath = "/r2/buckets/lake-backups/cors"
	const lifecyclePath = "/r2/buckets/lake-backups/lifecycle"
	other := `{"id":"multipart-default","enabled":true,"conditions":{"prefix":""},"abortMultipartUploadsTransition":{"condition":{"type":"Age","maxAge":604800}}}`
	cf.config[lifecyclePath] = json.RawMessage(`{"rules":[` + other + `]}`)
	reconcileR2(t, r, b)
	var gotCORS, wantCORS any
	if err := json.Unmarshal(cf.config[corsPath], &gotCORS); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"rules":[{"allowed":{"origins":["https://app.example","https://extra.example"],"methods":["PUT","GET"],"headers":["X-Upload","Content-Type"]},"maxAgeSeconds":3600},{"allowed":{"origins":["https://other.example"],"methods":["PUT"]}}]}`), &wantCORS); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotCORS, wantCORS) {
		t.Fatalf("CORS body = %v", gotCORS)
	}
	var gotLifecycle, wantLifecycle any
	_ = json.Unmarshal(cf.config[lifecyclePath], &gotLifecycle)
	_ = json.Unmarshal([]byte(`{"rules":[`+other+`,{"id":"orama-expire","enabled":true,"conditions":{"prefix":""},"deleteObjectsTransition":{"condition":{"type":"Age","maxAge":604800}}}]}`), &wantLifecycle)
	if !reflect.DeepEqual(gotLifecycle, wantLifecycle) {
		t.Fatalf("lifecycle body = %v", gotLifecycle)
	}
	reconcileR2(t, r, b)
	if cf.puts[corsPath] != 1 || cf.puts[lifecyclePath] != 1 {
		t.Fatalf("steady PUT counts = %v", cf.puts)
	}

	// Cloudflare may reorder rules and sets, assign CORS IDs, and emit empty optional fields.
	cf.config[corsPath] = json.RawMessage(`{"rules":[{"id":"generated-2","allowed":{"origins":["https://other.example"],"methods":["PUT"],"headers":[]},"exposeHeaders":[],"maxAgeSeconds":0},{"id":"generated-1","allowed":{"origins":["https://extra.example","https://app.example"],"methods":["GET","PUT"],"headers":["content-type","x-upload"]},"exposeHeaders":[],"maxAgeSeconds":3600}]}`)
	cf.config[lifecyclePath] = json.RawMessage(`{"rules":[{"id":"orama-expire","enabled":true,"conditions":{"prefix":""},"deleteObjectsTransition":{"condition":{"maxAge":604800,"type":"Age","date":""}},"abortMultipartUploadsTransition":null,"storageClassTransitions":[]},` + other + `]}`)
	reconcileR2(t, r, b)
	if cf.puts[corsPath] != 1 || cf.puts[lifecyclePath] != 1 {
		t.Fatalf("normalized PUT counts = %v", cf.puts)
	}

	// Drift is repaired while preserving unrelated lifecycle rules.
	days = 8
	b.Spec.ExpireAfterDays = &days
	b.Spec.CORS[0].AllowedOrigins = []string{"https://new.example"}
	if err := kube.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	reconcileR2(t, r, b)
	if cf.puts[corsPath] != 2 || cf.puts[lifecyclePath] != 2 || !strings.Contains(string(cf.config[lifecyclePath]), "691200") || !strings.Contains(string(cf.config[lifecyclePath]), "multipart-default") {
		t.Fatalf("drift repair PUT counts = %v", cf.puts)
	}

	// Removing the fields leaves existing provider configuration alone.
	b.Spec.CORS, b.Spec.ExpireAfterDays = nil, nil
	if err := kube.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	before := len(cf.requests)
	reconcileR2(t, r, b)
	for _, request := range cf.requests[before:] {
		if strings.HasSuffix(request, "/cors") || strings.HasSuffix(request, "/lifecycle") {
			t.Fatalf("unset fields called %s", request)
		}
	}
}

func TestR2BucketUnsetConfigurationMakesNoCalls(t *testing.T) {
	b := testR2Bucket(oramav1alpha1.BucketDeletionRetain, true)
	r, _, cf := newR2Fixture(t, b)
	reconcileR2(t, r, b)
	reconcileR2(t, r, b)
	if slices.ContainsFunc(cf.requests, func(request string) bool {
		return strings.HasSuffix(request, "/cors") || strings.HasSuffix(request, "/lifecycle")
	}) {
		t.Fatalf("unset configuration made new calls: %v", cf.requests)
	}
}

func TestR2BucketConfigurationFailureReportsNotReady(t *testing.T) {
	for _, endpoint := range []string{"cors", "lifecycle"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(method+" "+endpoint, func(t *testing.T) {
				b := testR2Bucket(oramav1alpha1.BucketDeletionDelete, false)
				days := int32(7)
				b.Spec.ExpireAfterDays = &days
				b.Spec.CORS = []oramav1alpha1.CloudflareR2CORSRule{{AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"PUT"}}}
				r, kube, cf := newR2Fixture(t, b)
				reconcileR2(t, r, b)
				// Force PUTs to repair drift after an initially Ready bucket.
				cf.config = map[string]json.RawMessage{}
				cf.fail = method + " /r2/buckets/lake-backups/" + endpoint
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
				if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "configuration denied") {
					t.Fatalf("error = %v", err)
				}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil {
					t.Fatal(err)
				}
				if b.Status.Ready || !b.Status.Created || b.Status.Message != err.Error() {
					t.Fatalf("status = %+v", b.Status)
				}
				version := b.ResourceVersion
				_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil {
					t.Fatal(err)
				}
				if b.ResourceVersion != version {
					t.Fatal("unchanged failure rewrote status")
				}
				cf.fail = ""
				reconcileR2(t, r, b)
				if !b.Status.Ready {
					t.Fatal("did not recover")
				}
			})
		}
	}
}

// A new bucket has no configuration: Cloudflare returns 404 until the first PUT.
func TestR2BucketInitialConfigurationAfterNotFound(t *testing.T) {
	b := testR2Bucket(oramav1alpha1.BucketDeletionDelete, false)
	days := int32(7)
	b.Spec.ExpireAfterDays = &days
	b.Spec.CORS = []oramav1alpha1.CloudflareR2CORSRule{{AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"PUT"}, AllowedHeaders: []string{"*"}, MaxAgeSeconds: 3600}}
	r, _, cf := newR2Fixture(t, b)
	reconcileR2(t, r, b)
	if !b.Status.Ready {
		t.Fatalf("fresh bucket is not Ready: %+v", b.Status)
	}
	var configurationRequests []string
	for _, request := range cf.requests {
		if strings.HasSuffix(request, "/cors") || strings.HasSuffix(request, "/lifecycle") {
			configurationRequests = append(configurationRequests, request)
		}
	}
	want := []string{"GET /accounts/acct/r2/buckets/lake-backups/cors", "PUT /accounts/acct/r2/buckets/lake-backups/cors", "GET /accounts/acct/r2/buckets/lake-backups/lifecycle", "PUT /accounts/acct/r2/buckets/lake-backups/lifecycle"}
	if !slices.Equal(configurationRequests, want) {
		t.Fatalf("initial configuration requests = %v", configurationRequests)
	}
	reconcileR2(t, r, b)
	if cf.puts["/r2/buckets/lake-backups/cors"] != 1 || cf.puts["/r2/buckets/lake-backups/lifecycle"] != 1 {
		t.Fatalf("steady configuration PUT counts = %v", cf.puts)
	}
}

func TestR2BucketCustomDomain(t *testing.T) {
	b := testR2Bucket(oramav1alpha1.BucketDeletionRetain, false)
	b.Spec.CustomDomain = &oramav1alpha1.CloudflareR2CustomDomain{Domain: "status.lake.live", ZoneID: "zone-1"}
	r, kube, cf := newR2Fixture(t, b)
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil {
		t.Fatal(err)
	}
	got := cf.domains["status.lake.live"]
	if got == nil || got["zoneId"] != "zone-1" || got["enabled"] != true || got["minTLS"] != "1.2" {
		t.Fatalf("attached %v", got)
	}
	if b.Status.CustomDomain != "status.lake.live" || b.Status.CustomDomainStatus != "ownership=pending ssl=pending" || result.RequeueAfter == 0 {
		t.Fatalf("status %+v requeue %v: a pending domain is looked at again", b.Status, result.RequeueAfter)
	}
	// Next pass reads Cloudflare's verdict and stops requeueing; a disabled domain is re-enabled.
	cf.domains["status.lake.live"]["enabled"] = false
	result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	if err != nil {
		t.Fatal(err)
	}
	_ = kube.Get(context.Background(), client.ObjectKeyFromObject(b), b)
	if cf.domains["status.lake.live"]["enabled"] != true || b.Status.CustomDomainStatus != r2CustomDomainActive || result.RequeueAfter != 0 {
		t.Fatalf("domain %v status %+v requeue %v", cf.domains["status.lake.live"], b.Status, result.RequeueAfter)
	}
	// Dropping it from the spec detaches it.
	b.Spec.CustomDomain = nil
	if err := kube.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	reconcileR2(t, r, b)
	if len(cf.domains) != 0 || b.Status.CustomDomain != "" {
		t.Fatalf("domains %v status %+v", cf.domains, b.Status)
	}
}
