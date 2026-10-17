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

// TestConnectionsPgxStoreAgainstPostgres creates Lake migration 0084's connection contract
// itself. The vendored schema intentionally remains unchanged until Lake merges that migration.
//
//	LAKE_CONNECTIONS_TEST_POSTGRES=1 [LAKE_CONNECTIONS_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run ConnectionsPgxStore
func TestConnectionsPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_CONNECTIONS_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_CONNECTIONS_TEST_POSTGRES=1 to run the Connection store against local Postgres")
	}
	uri := os.Getenv("LAKE_CONNECTIONS_TEST_POSTGRES_URI")
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
	scratch := fmt.Sprintf("orama_operator_connections_test_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE SCHEMA " + scratch,
		"SET LOCAL search_path TO " + scratch + ", pg_catalog",
		`CREATE TABLE workspaces (
            id uuid PRIMARY KEY,
            namespace text NOT NULL DEFAULT '',
            created_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE TABLE network_connections (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            customer_id uuid NOT NULL,
            workspace_id uuid NOT NULL REFERENCES workspaces(id),
            source_kind text NOT NULL CHECK (source_kind IN ('app', 'service', 'agent', 'tunnel')),
            source_id uuid NOT NULL,
            target_kind text NOT NULL CHECK (target_kind IN ('service', 'app')),
            target_id uuid NOT NULL,
            ports integer[] NOT NULL DEFAULT '{}',
            credential_scope jsonb,
            desired text NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
            generation bigint NOT NULL DEFAULT 1,
            created_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE TABLE network_connection_status (
            connection_id uuid PRIMARY KEY REFERENCES network_connections(id) ON DELETE CASCADE,
            phase text NOT NULL DEFAULT '',
            message text NOT NULL DEFAULT '',
            credential_secret text NOT NULL DEFAULT '',
            observed_generation bigint NOT NULL DEFAULT 0,
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("apply contract statement: %v", err)
		}
	}
	store := &pgxServiceStore{db: tx}
	if _, err := store.ListNetworkConnections(ctx); !errors.Is(err, errNetworkGuardMissing) {
		t.Fatalf("pre-guard read error = %v", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE network_guard (applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id, namespace) VALUES ($1, $2)`, testWorkspaceID, testWorkspaceName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO network_connections
        (id, customer_id, workspace_id, source_kind, source_id, target_kind, target_id, ports, credential_scope, generation)
        VALUES ($1, $2, $3, 'app', $4, 'service', $5, ARRAY[5432, 9187], '{"access": "read", "schema": "analytics"}', 3)`,
		testConnectionID, testServiceCustomer, testWorkspaceID, testConnectionSourceID, testConnectionTargetID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ListNetworkConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workspaces) != 1 || len(snapshot.Connections) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	row := snapshot.Connections[0]
	if row.WorkspaceNamespace != testWorkspaceName || row.SourceKind != "app" || row.TargetKind != "service" ||
		len(row.Ports) != 2 || row.Generation != 3 || row.Status != nil {
		t.Fatalf("connection row = %+v", row)
	}
	// operator#469: the scope is read as written and parses to the CR's scope.
	if scope, message := parseConnectionCredentialScope(&row); message != "" || scope == nil ||
		scope.Access != "read" || scope.Schema != "analytics" {
		t.Fatalf("credential scope %q parsed to %+v, %q", row.CredentialScope, scope, message)
	}
	status := LakeNetworkConnectionStatus{
		ConnectionID:       testConnectionID,
		Phase:              "Ready",
		Message:            "connection policies ready",
		CredentialSecret:   "lake-secret://70000000-0000-4000-8000-000000000007/connection-" + testConnectionID,
		ObservedGeneration: 3,
	}
	if err := store.UpsertNetworkConnectionStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ListNetworkConnections(ctx)
	if err != nil || snapshot.Connections[0].Status == nil || *snapshot.Connections[0].Status != status {
		t.Fatalf("stored status = %+v, %v", snapshot, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE network_connections SET desired = 'absent', generation = 4 WHERE id = $1`, testConnectionID); err != nil {
		t.Fatal(err)
	}
	status.Phase = "Deleted"
	status.Message = "connection policies deleted"
	status.CredentialSecret = ""
	status.ObservedGeneration = 4
	if err := store.UpsertNetworkConnectionStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ListNetworkConnections(ctx)
	if err != nil || len(snapshot.Connections) != 0 {
		t.Fatalf("Deleted history was listed: %+v, %v", snapshot.Connections, err)
	}
}
