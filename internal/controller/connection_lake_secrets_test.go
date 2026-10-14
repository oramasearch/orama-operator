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
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeLakeSecretsAPI answers list_secret_mounts, create_secret_mount and put_secret like Lake.
type fakeLakeSecretsAPI struct {
	mu     sync.Mutex
	mounts []map[string]string
	calls  []string
	puts   []map[string]any
	bearer string
}

func (f *fakeLakeSecretsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	capability := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.calls = append(f.calls, capability)
	f.bearer = r.Header.Get("Authorization")
	var input map[string]any
	_ = json.NewDecoder(r.Body).Decode(&input)
	answer := func(value any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "value": value}) }
	switch capability {
	case "list_secret_mounts":
		answer(map[string]any{"items": f.mounts})
	case "create_secret_mount":
		mount := map[string]string{"id": testCredentialMountID, "name": input["name"].(string)}
		f.mounts = append(f.mounts, mount)
		answer(mount)
	case "put_secret":
		f.puts = append(f.puts, input)
		answer(map[string]any{"mount_id": input["mount_id"], "name": input["name"], "version": len(f.puts)})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": "not_found", "message": capability}})
	}
}

func TestLakeSecretWriterCreatesTheMountOnceAndPutsVersions(t *testing.T) {
	api := &fakeLakeSecretsAPI{mounts: []map[string]string{{"id": "80000000-0000-4000-8000-000000000008", "name": "customer-mount"}}}
	server := httptest.NewServer(api)
	defer server.Close()
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake-system"}}
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeAdminSecret("prod"), Namespace: "lake-system"},
		Data:       map[string][]byte{"token": []byte("operator-bearer\n")},
	}
	writer := &lakeHTTPSecretWriter{
		Reader:  fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(admin).Build(),
		BaseURL: server.URL,
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		reference, err := writer.PutWorkspaceSecret(ctx, lake, testServiceCustomer, testWorkspaceID, testCredentialSecretName,
			map[string]any{"password": "p"}, map[string]string{"generation": "1"})
		if err != nil {
			t.Fatal(err)
		}
		if reference != lakeSecretReference(testCredentialMountID, testCredentialSecretName) {
			t.Fatalf("reference = %q", reference)
		}
	}
	want := []string{"list_secret_mounts", "create_secret_mount", "put_secret", "list_secret_mounts", "put_secret"}
	if strings.Join(api.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", api.calls, want)
	}
	if api.bearer != "Bearer operator-bearer" {
		t.Fatalf("authorization = %q", api.bearer)
	}
	if len(api.puts) != 2 || api.puts[0]["mount_id"] != testCredentialMountID || api.puts[0]["name"] != testCredentialSecretName {
		t.Fatalf("puts = %+v", api.puts)
	}

	// A refusal from Lake is an error, never a reference.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": "unauthorized", "message": "no"}})
	}))
	defer failing.Close()
	writer.BaseURL = failing.URL
	if reference, err := writer.PutWorkspaceSecret(ctx, lake, testServiceCustomer, testWorkspaceID, "x", map[string]any{}, nil); err == nil ||
		reference != "" || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("refused put = %q, %v", reference, err)
	}
}
