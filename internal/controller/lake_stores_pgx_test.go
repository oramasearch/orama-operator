/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestStoresPgxStoreAgainstPostgres runs the stores half of the pgx store against the vendored lake
// schema on a real Postgres, inside one transaction in a scratch schema that is rolled back.
//
//	LAKE_SERVICES_TEST_POSTGRES=1 [LAKE_SERVICES_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run PgxStore
func TestStoresPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_SERVICES_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_SERVICES_TEST_POSTGRES=1 to run the pgx store against a local Postgres")
	}
	uri := os.Getenv("LAKE_SERVICES_TEST_POSTGRES_URI")
	if uri == "" {
		uri = "postgres://127.0.0.1:5432/postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	scratch := fmt.Sprintf("orama_operator_stores_test_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE SCHEMA " + scratch,
		"SET LOCAL search_path TO " + scratch + ", pg_catalog",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	// Before 0028_stores: the missing table is recognised, in a savepoint so the transaction lives.
	before, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&pgxServiceStore{db: before}).ListStores(ctx); !errors.Is(err, errStoresTableMissing) {
		t.Fatalf("list without a stores table: %v, want errStoresTableMissing", err)
	}
	_ = before.Rollback(ctx)
	beforeMounts, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&pgxServiceStore{db: beforeMounts}).ListStoreMounts(ctx); !errors.Is(err, errStoreMountsTableMissing) {
		t.Fatalf("list without a store_mounts table: %v, want errStoreMountsTableMissing", err)
	}
	_ = beforeMounts.Rollback(ctx)

	if _, err := tx.Exec(ctx, lakeSchemaSQL); err != nil {
		t.Fatalf("apply the vendored lake schema: %v", err)
	}
	workspaceFixture, err := os.ReadFile("testdata/lake-workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(workspaceFixture)); err != nil {
		t.Fatalf("apply workspace fixture: %v", err)
	}
	var customerID string
	if err := tx.QueryRow(ctx, `WITH u AS (INSERT INTO users (display_name) VALUES ('t') RETURNING id)
		INSERT INTO customers (customer_type, user_id, name) SELECT 'individual', id, 'stores-test' FROM u RETURNING id::text`).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces
		(id, customer_id, name, environment, desired, generation, namespace)
		VALUES ($1, $2, 'Production', 'prod', 'running', 1, $3)`, testWorkspaceID, customerID, testWorkspaceName); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	insert := `INSERT INTO stores (id, customer_id, workspace_id, type, provider, name, slug, spec, desired, generation, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, now() + $11 * interval '1 second')`
	for _, row := range [][]any{
		{testVolumeStoreID, customerID, testWorkspaceID, "volume", "cluster", "data", testVolumeStoreSlug, `{"size_gi":20,"public":false}`, "present", 1, 0},
		{testBucketStoreID, customerID, testWorkspaceID, "object", "r2", "gone", testBucketStoreSlug, `{}`, "absent", 2, 1},
	} {
		if _, err := tx.Exec(ctx, insert, row...); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}

	store := &pgxServiceStore{db: tx}
	stores, err := store.ListStores(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stores) != 2 || stores[0].ID != testVolumeStoreID || stores[0].Status != nil {
		t.Fatalf("stores = %+v", stores)
	}
	if got := stores[0]; got.Type != "volume" || got.Provider != "cluster" || got.CustomerID != customerID || got.Slug != testVolumeStoreSlug || got.Desired != "present" {
		t.Fatalf("volume store = %+v", got)
	}
	if spec, err := parseStoreSpec(stores[0].Spec); err != nil || spec.SizeGi != 20 {
		t.Fatalf("spec %q parsed to %+v, %v", stores[0].Spec, spec, err)
	}

	status := LakeStoreStatus{StoreID: testVolumeStoreID, Phase: ServicePhaseProvisioning, Message: "delivering the connection", Endpoint: "lake/prod-st-444444444444", ObservedGeneration: 1}
	if err := store.UpsertStoreStatus(ctx, status); err != nil {
		t.Fatalf("insert status: %v", err)
	}
	status.Phase, status.Message = ServicePhaseReady, storeReadyUnboundMessage
	if err := store.UpsertStoreStatus(ctx, status); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := store.UpsertStoreStatus(ctx, LakeStoreStatus{StoreID: testBucketStoreID, Phase: ServicePhaseDeleted, Message: "store deleted", ObservedGeneration: 2}); err != nil {
		t.Fatalf("deleted status: %v", err)
	}
	stores, err = store.ListStores(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 1 || stores[0].Status == nil || *stores[0].Status != status {
		t.Fatalf("stores after status writes = %+v, want the volume with %+v", stores, status)
	}

	// Store mounts and used space (0080_store_mounts, lake#666).
	appID := "0d0d0d0d-1111-4222-8333-444444444444"
	if _, err := tx.Exec(ctx, `INSERT INTO store_mounts (store_id, object_kind, object_id, mount_path, read_only)
		VALUES ($1, 'app', $2, '/data/x', true)`, testVolumeStoreID, appID); err != nil {
		t.Fatalf("seed mount: %v", err)
	}
	mounts, err := store.ListStoreMounts(ctx)
	if err != nil {
		t.Fatalf("list mounts: %v", err)
	}
	want := LakeStoreMount{StoreID: testVolumeStoreID, ObjectKind: "app", ObjectID: appID, MountPath: "/data/x", ReadOnly: true,
		Slug: testVolumeStoreSlug, Type: "volume", Provider: "cluster", Name: "data", Desired: "present"}
	if len(mounts) != 1 || mounts[0] != want {
		t.Fatalf("mounts = %+v, want %+v", mounts, want)
	}
	usage, err := store.ListStoreUsage(ctx)
	if err != nil || len(usage) != 0 {
		t.Fatalf("usage before a measurement = %v, %v", usage, err)
	}
	if err := store.UpdateStoreUsedBytes(ctx, testVolumeStoreID, 5<<20); err != nil {
		t.Fatalf("write used bytes: %v", err)
	}
	usage, err = store.ListStoreUsage(ctx)
	if err != nil || len(usage) != 1 || usage[testVolumeStoreID].UsedBytes != 5<<20 || usage[testVolumeStoreID].At.IsZero() {
		t.Fatalf("usage = %v, %v", usage, err)
	}
	// The status write leaves the used space alone.
	if err := store.UpsertStoreStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	if usage, err = store.ListStoreUsage(ctx); err != nil || usage[testVolumeStoreID].UsedBytes != 5<<20 {
		t.Fatalf("usage after a status write = %v, %v", usage, err)
	}
}
