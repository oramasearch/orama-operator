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
	requests []string
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
	cf := &fakeR2{t: t, buckets: map[string]bool{}, tokens: map[string]map[string]any{}}
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
