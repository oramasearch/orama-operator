/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// checkAppTransferPgx runs the app transfer statements against the vendored schema
// (0079_app_transfer, lake#661), inside TestServicesPgxStoreAgainstPostgres's transaction.
func checkAppTransferPgx(t *testing.T, ctx context.Context, tx pgx.Tx, store *pgxServiceStore) {
	t.Helper()
	const app, other, workspace = "cccccccc-0000-4000-8000-000000000001", "cccccccc-0000-4000-8000-000000000002", "cccccccc-0000-4000-8000-000000000003"
	hour := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	old := hour.Add(-10 * 24 * time.Hour)
	if err := store.AddAppTransfer(ctx, []AppTransfer{{AppID: other, WorkspaceID: workspace, Hour: old, InBytes: 1, OutBytes: 1}}, old.Add(-time.Hour)); err != nil {
		t.Fatalf("an old hour: %v", err)
	}
	for _, rows := range [][]AppTransfer{
		{{AppID: app, WorkspaceID: workspace, Hour: hour, InBytes: 100, OutBytes: 1000}, {AppID: other, WorkspaceID: workspace, Hour: hour, InBytes: 1, OutBytes: 2}},
		{{AppID: app, WorkspaceID: workspace, Hour: hour.Add(10 * time.Minute).Truncate(time.Hour), InBytes: 5, OutBytes: 50}},
	} {
		if err := store.AddAppTransfer(ctx, rows, hour.Add(-8*24*time.Hour)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	var in, out int64
	var stored string
	if err := tx.QueryRow(ctx, `SELECT in_bytes, out_bytes, workspace_id::text FROM app_transfer WHERE app_id = $1::uuid AND hour = $2`, app, hour).Scan(&in, &out, &stored); err != nil {
		t.Fatal(err)
	}
	if in != 105 || out != 1050 || stored != workspace {
		t.Fatalf("app hour = %d %d %s, want each pass added", in, out, stored)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_transfer WHERE hour < $1`, hour.Add(-8*24*time.Hour)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired rows = %d, %v", count, err)
	}

	// Before 0079 the table is missing, and the pass is skipped.
	missing, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Exec(ctx, `DROP TABLE app_transfer`); err != nil {
		t.Fatal(err)
	}
	if err := (&pgxServiceStore{db: missing}).AddAppTransfer(ctx, nil, hour); !errors.Is(err, errAppTransferTableMissing) {
		t.Errorf("missing table err = %v", err)
	}
	_ = missing.Rollback(ctx)
}
