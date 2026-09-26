/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestServicesPgxStoreAgainstPostgres runs the pgx store's SQL against the vendored lake schema on
// a real Postgres. Everything happens inside one transaction, in a scratch schema, that is rolled
// back: the database is left exactly as it was.
//
//	LAKE_SERVICES_TEST_POSTGRES=1 [LAKE_SERVICES_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run PgxStore
func TestServicesPgxStoreAgainstPostgres(t *testing.T) {
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

	scratch := fmt.Sprintf("orama_operator_services_test_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE SCHEMA " + scratch,
		"SET LOCAL search_path TO " + scratch + ", pg_catalog",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := tx.Exec(ctx, lakeSchemaSQL); err != nil {
		t.Fatalf("apply the vendored lake schema: %v", err)
	}
	var customerID string
	if err := tx.QueryRow(ctx, `WITH u AS (INSERT INTO users (display_name) VALUES ('t') RETURNING id)
		INSERT INTO customers (customer_type, user_id, name) SELECT 'individual', id, 'services-test' FROM u RETURNING id::text`).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	insert := `INSERT INTO services (id, customer_id, kind, name, slug, is_default, spec, desired, generation)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)`
	for _, row := range [][]any{
		{testDefaultID, customerID, "zetta", "workspace", testDefaultSlug, true, `{"instances":1,"storage_gi":10}`, "present", 1},
		{testPostgresID, customerID, "postgres", "db", testPostgresSlug, false, `{}`, "present", 3},
		{testZettaServiceID, customerID, "zetta", "gone", testZettaSlug, false, `{}`, "absent", 2},
	} {
		if _, err := tx.Exec(ctx, insert, row...); err != nil {
			t.Fatalf("seed service: %v", err)
		}
	}

	store := &pgxServiceStore{db: tx}
	services, err := store.ListServices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(services) != 3 || services[0].Status != nil {
		t.Fatalf("services = %+v", services)
	}
	byID := map[string]LakeService{}
	for _, svc := range services {
		byID[svc.ID] = svc
	}
	if got := byID[testDefaultID]; got.Kind != "zetta" || !got.IsDefault || got.CustomerID != customerID || got.Slug != testDefaultSlug {
		t.Fatalf("default service = %+v", got)
	}
	if spec, err := parseServiceSpec(byID[testDefaultID].Spec); err != nil || spec.Instances != 1 || spec.StorageGi != 10 {
		t.Fatalf("spec %q parsed to %+v, %v", byID[testDefaultID].Spec, spec, err)
	}
	if byID[testPostgresID].Generation != 3 {
		t.Fatalf("generation = %d", byID[testPostgresID].Generation)
	}

	status := LakeServiceStatus{ServiceID: testPostgresID, Phase: ServicePhaseProvisioning, Message: "waiting", Endpoint: "h:5432", ObservedGeneration: 3}
	if err := store.UpsertServiceStatus(ctx, status); err != nil {
		t.Fatalf("insert status: %v", err)
	}
	status.Phase, status.Message = ServicePhaseReady, "service ready"
	if err := store.UpsertServiceStatus(ctx, status); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := store.UpsertServiceStatus(ctx, LakeServiceStatus{ServiceID: testZettaServiceID, Phase: ServicePhaseDeleted, Message: "service deleted", ObservedGeneration: 2}); err != nil {
		t.Fatalf("deleted status: %v", err)
	}
	services, err = store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("history (absent and Deleted) was listed: %+v", services)
	}
	for _, svc := range services {
		if svc.ID == testPostgresID && (svc.Status == nil || *svc.Status != status) {
			t.Fatalf("status = %+v, want %+v", svc.Status, status)
		}
	}

	// Migration 0025_service_catalog: every catalog kind is a valid row and reads back as that
	// kind; anything else is still refused by the kind check.
	for i, kind := range []string{serviceKindKafka, serviceKindClickHouse, serviceKindTiDB, serviceKindCassandra, serviceKindValkey} {
		id := fmt.Sprintf("c0000000-0000-4000-8000-00000000000%d", i)
		if _, err := tx.Exec(ctx, insert, id, customerID, kind, kind, fmt.Sprintf("svc-c0000000000%d", i), false, `{}`, "present", 1); err != nil {
			t.Fatalf("seed %s service: %v", kind, err)
		}
	}
	services, err = store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, svc := range services {
		kinds[svc.Kind] = true
		if !knownServiceKind(svc.Kind) {
			t.Fatalf("lake accepted kind %q, which the operator does not run", svc.Kind)
		}
	}
	if len(kinds) != len(serviceKindNames) {
		t.Fatalf("kinds read back = %v, want %v", kinds, serviceKindNames)
	}
	refused, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refused.Exec(ctx, insert, "d0000000-0000-4000-8000-000000000000", customerID, "sqlite", "lite", "svc-d00000000000", false, `{}`, "present", 1); err == nil {
		t.Fatal("the kind check accepted sqlite")
	}
	_ = refused.Rollback(ctx)
}
