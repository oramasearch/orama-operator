// SPDX-License-Identifier: MIT

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
	if _, err := tx.Exec(ctx, `INSERT INTO services (id, generation) VALUES ($1, 9)`, testWorkspaceZettaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces
 (id, customer_id, name, environment, desired, generation, zetta_service_id)
	 VALUES ($1, $2, 'Production', 'prod', 'running', 3, $3)`, testWorkspaceID, testWorkspaceCustomer, testWorkspaceZettaID); err != nil {
		t.Fatal(err)
	}
	store := &pgxWorkspacesStore{pgxServiceStore: &pgxServiceStore{db: tx}}
	rows, err := store.ListWorkspaces(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != testWorkspaceID || rows[0].Phase != "" || rows[0].StorageGi != 5 ||
		rows[0].ZettaServiceID != testWorkspaceZettaID || rows[0].ZettaServiceGeneration != 9 || rows[0].ZettaServiceStatus != nil {
		t.Fatalf("workspace rows = %+v, %v", rows, err)
	}
	row := &rows[0]
	status := LakeWorkspaceStatus{
		Phase:              WorkspacePhaseReady,
		ObservedGeneration: row.Generation,
		Message:            "workspace ready",
		Namespace:          testWorkspaceName,
		StorageGiObserved:  5,
		StoragePhase:       WorkspaceStorageReady,
		StorageMessage:     "1 volume at 5 GiB",
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
	serviceStatus := LakeServiceStatus{
		ServiceID:          testWorkspaceZettaID,
		Phase:              ServicePhaseReady,
		Message:            "service ready",
		Endpoint:           "http://prod-ws-123e4567e89b-zetta.prod-ws-123e4567e89b.svc.cluster.local:4800",
		ObservedGeneration: 9,
	}
	if err := store.UpsertServiceStatus(ctx, serviceStatus); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListWorkspaces(ctx)
	if err != nil || len(rows) != 1 || rows[0].ZettaServiceStatus == nil || *rows[0].ZettaServiceStatus != serviceStatus {
		t.Fatalf("service status did not round trip: %+v, %v", rows, err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE workspaces"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListWorkspaces(ctx); !errors.Is(err, errWorkspacesTableMissing) {
		t.Fatalf("missing schema not recognized: %v", err)
	}
}
