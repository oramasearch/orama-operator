/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestLakeWorkspacesPgxStoreAgainstPostgres(t *testing.T) {
	uri := os.Getenv("LAKE_WORKSPACES_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAKE_WORKSPACES_TEST_POSTGRES_URI is required for native workspace SQL verification")
	}
	fixture, err := os.ReadFile("testdata/lake-workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatal("cannot connect to test Postgres")
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces
 (id, customer_id, name, environment, desired, generation, zetta_service_id)
 VALUES ($1, $2, 'Production', 'prod', 'running', 3, NULL)`, testWorkspaceID, testWorkspaceCustomer); err != nil {
		t.Fatal(err)
	}
	store := &pgxWorkspacesStore{pgxServiceStore: &pgxServiceStore{db: tx}}
	rows, err := store.ListWorkspaces(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != testWorkspaceID || rows[0].Phase != "" || rows[0].ZettaServiceID != "" {
		t.Fatalf("workspace rows = %+v, %v", rows, err)
	}
	row := &rows[0]
	status := LakeWorkspaceStatus{
		Phase:              WorkspacePhaseReady,
		ObservedGeneration: row.Generation,
		Message:            "workspace ready",
		Namespace:          testWorkspaceName,
	}
	updated, err := store.UpdateWorkspaceStatus(ctx, row, status)
	if err != nil || !updated {
		t.Fatalf("update status: updated %v, %v", updated, err)
	}
	rows, err = store.ListWorkspaces(ctx)
	if err != nil || len(rows) != 1 || rows[0].status() != status {
		t.Fatalf("status did not round trip: %+v, %v", rows, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET generation=4, desired='stopped' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if updated, err := store.UpdateWorkspaceStatus(ctx, row, status); err != nil || updated {
		t.Fatal("stale workspace status overwrote newer desired state")
	}
	if _, err := tx.Exec(ctx, "DROP TABLE workspaces"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListWorkspaces(ctx); !errors.Is(err, errWorkspacesTableMissing) {
		t.Fatalf("missing schema not recognized: %v", err)
	}
}
