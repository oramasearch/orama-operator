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
	"sync"
	"testing"
)

// fakeCassandraManagement answers the management API's role list and role drop.
type fakeCassandraManagement struct {
	mu      sync.Mutex
	roles   []string
	dropped []string
	calls   int
}

func (f *fakeCassandraManagement) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.URL.Path != "/api/v0/ops/auth/role" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		listed := make([]map[string]string, 0, len(f.roles))
		for _, role := range f.roles {
			listed = append(listed, map[string]string{"name": role, "super": "true", "login": "true"})
		}
		_ = json.NewEncoder(w).Encode(listed)
	case http.MethodDelete:
		name := r.URL.Query().Get("username")
		kept := f.roles[:0]
		for _, role := range f.roles {
			if role != name {
				kept = append(kept, role)
			}
		}
		f.roles = kept
		f.dropped = append(f.dropped, name)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func withFakeCassandraManagement(t *testing.T, fake *fakeCassandraManagement) {
	t.Helper()
	server := httptest.NewServer(fake)
	saved := cassandraManagement
	cassandraManagement.client = server.Client()
	cassandraManagement.baseURL = func(catalogService) string { return server.URL }
	t.Cleanup(func() {
		server.Close()
		cassandraManagement = saved
	})
}

func cassandraTestService(namespace string) catalogService {
	return catalogService{svc: &LakeService{Slug: "cass1"}, namespace: namespace, name: "lake-cass1"}
}

// The default cassandra superuser is dropped once the generated one is listed, and only then.
func TestCassandraDefaultSuperuserIsDroppedOnceTheGeneratedOneExists(t *testing.T) {
	ctx := context.Background()
	fake := &fakeCassandraManagement{roles: []string{"cassandra"}}
	withFakeCassandraManagement(t, fake)
	in := cassandraTestService("ws-drop")

	if err := dropCassandraDefaultRole(ctx, in, "cass1-superuser"); err == nil {
		t.Fatal("the default role was handled before the generated superuser existed")
	}
	if len(fake.dropped) != 0 {
		t.Fatal("the default role was dropped while it was the only superuser")
	}

	fake.roles = append(fake.roles, "cass1-superuser")
	if err := dropCassandraDefaultRole(ctx, in, "cass1-superuser"); err != nil {
		t.Fatal(err)
	}
	if len(fake.dropped) != 1 || fake.dropped[0] != "cassandra" {
		t.Fatalf("dropped %v, want [cassandra]", fake.dropped)
	}
	if len(fake.roles) != 1 || fake.roles[0] != "cass1-superuser" {
		t.Fatalf("roles left %v, want only the generated superuser", fake.roles)
	}

	calls := fake.calls
	if err := dropCassandraDefaultRole(ctx, in, "cass1-superuser"); err != nil {
		t.Fatal(err)
	}
	if fake.calls != calls {
		t.Fatal("a datacenter whose default role is gone was asked again")
	}
}

// A generated superuser that is itself named cassandra is never dropped.
func TestCassandraGeneratedSuperuserNamedCassandraIsKept(t *testing.T) {
	fake := &fakeCassandraManagement{roles: []string{"cassandra"}}
	withFakeCassandraManagement(t, fake)
	if err := dropCassandraDefaultRole(context.Background(), cassandraTestService("ws-keep"), "cassandra"); err == nil || len(fake.dropped) != 0 {
		t.Fatalf("err %v, dropped %v: the only superuser must be kept", err, fake.dropped)
	}
}
