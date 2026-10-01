/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// This contract fixture belongs to the test only. Lake #293 owns the real migration; the
// operator's byte-identical vendored schema is not modified to fabricate an upstream migration.
const lakeAppsTestTable = `CREATE TEMP TABLE apps (
 id uuid PRIMARY KEY, customer_id uuid NOT NULL, workspace_id uuid NOT NULL, name text NOT NULL, mode text NOT NULL,
 source_repository text, source_revision text, source_path text, source_entrypoint text,
 image text, framework text, port integer, replicas integer, host text, auth text,
 bindings_json jsonb, env_json jsonb, secret_env_refs jsonb, desired text, generation bigint,
 phase text, url text, ready boolean, message text, observed_generation bigint,
 updated_at timestamptz DEFAULT now(), token_hashes_json jsonb
)`

func TestLakeAppsPgxStoreAgainstPostgres(t *testing.T) {
	uri := os.Getenv("LAKE_APPS_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAKE_APPS_TEST_POSTGRES_URI is required for native apps SQL verification")
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
	workspaceFixture, err := os.ReadFile("testdata/lake-workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(workspaceFixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, lakeAppsTestTable); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces
 (id, customer_id, name, environment, desired, generation, namespace)
 VALUES ($1, $2, 'Production', 'prod', 'running', 1, $3)`, testWorkspaceID, testServiceCustomer, testWorkspaceName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO apps (id, customer_id, workspace_id, name, mode, desired, generation,
 source_repository, source_revision, framework, bindings_json)
 VALUES ($1, $2, $3, 'sales', 'runtime', 'running', 1, 'sales', 'revision-a', 'fastapi', '{"zetta":{"scopes":["read"]}}')`,
		testAppRowID, testServiceCustomer, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	store := &pgxAppsStore{pgxServiceStore: &pgxServiceStore{db: tx}}
	rows, err := store.ListApps(ctx)
	if err != nil || len(rows) != 1 || rows[0].SourceEntrypoint != "" || rows[0].TokenHashes != nil || rows[0].Port != 8080 || rows[0].Replicas != 1 || rows[0].SecretEnvRefs != "{}" {
		t.Fatalf("nullable row decoding failed: %v", err)
	}
	row := &rows[0]
	hashes := &oramav1alpha1.LakeAppTokenHashes{
		Source: credentialDigest("source"),
		Events: credentialDigest("events"),
		Pull:   credentialDigest("pull"),
	}
	if updated, err := store.UpdateAppTokenHashes(ctx, row, hashes); err != nil || !updated {
		t.Fatalf("register hashes: %v", err)
	}
	status := LakeAppRowStatus{Phase: "Running", URL: "https://sales.apps.example.test", Ready: true, Message: "ready", ObservedGeneration: 1}
	if updated, err := store.UpdateAppStatus(ctx, row, status); err != nil || !updated {
		t.Fatalf("write status: %v", err)
	}
	rows, err = store.ListApps(ctx)
	if err != nil || len(rows) != 1 || rows[0].Status != status || !reflect.DeepEqual(rows[0].TokenHashes, hashes) {
		t.Fatal("status or token hashes did not round trip")
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET generation=2, desired='stopped' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if updated, err := store.UpdateAppStatus(ctx, row, status); err != nil || updated {
		t.Fatal("stale status overwrote a newer desired generation")
	}
	if updated, err := store.UpdateAppTokenHashes(ctx, row, nil); err != nil || updated {
		t.Fatal("stale token hashes overwrote a newer desired generation")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM apps WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if updated, err := store.UpdateAppStatus(ctx, row, status); err != nil || updated {
		t.Fatal("removed app was recreated by a status write")
	}
	rows, err = store.ListApps(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatal("deleted row remained in the list")
	}
	if _, err := tx.Exec(ctx, "DROP TABLE pg_temp.apps"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListApps(ctx); !errors.Is(err, errAppsTableMissing) {
		t.Fatalf("missing schema not recognized: %v", err)
	}
}
