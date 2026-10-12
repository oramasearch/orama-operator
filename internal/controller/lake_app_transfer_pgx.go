/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The app transfer half of the pgx LakeServiceStore (lake migration 0079_app_transfer, lake#661):
// the same connection as the services poll.

// addAppTransferSQL adds every app's bytes to its hour's row in one statement, so a pass is
// recorded whole or not at all. $1..$4 are parallel arrays, one entry per app (never the same app
// twice); $5 is the hour.
const addAppTransferSQL = `INSERT INTO app_transfer (app_id, workspace_id, hour, in_bytes, out_bytes)
SELECT t.app_id::uuid, t.workspace_id::uuid, $5::timestamptz, t.in_bytes, t.out_bytes
  FROM unnest($1::text[], $2::text[], $3::bigint[], $4::bigint[])
       AS t(app_id, workspace_id, in_bytes, out_bytes)
ON CONFLICT (app_id, hour) DO UPDATE SET
    in_bytes = app_transfer.in_bytes + EXCLUDED.in_bytes,
    out_bytes = app_transfer.out_bytes + EXCLUDED.out_bytes,
    observed_at = now()`

// pruneAppTransferSQL deletes the hours lake no longer charges.
const pruneAppTransferSQL = `DELETE FROM app_transfer WHERE hour < $1::timestamptz`

func appTransferTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errAppTransferTableMissing
	}
	return err
}

func (s *pgxServiceStore) AddAppTransfer(ctx context.Context, rows []AppTransfer, before time.Time) error {
	byHour := map[time.Time][]AppTransfer{}
	for _, row := range rows {
		byHour[row.Hour.UTC()] = append(byHour[row.Hour.UTC()], row)
	}
	for hour, hourRows := range byHour {
		apps := make([]string, len(hourRows))
		workspaces := make([]string, len(hourRows))
		in := make([]int64, len(hourRows))
		out := make([]int64, len(hourRows))
		for i, row := range hourRows {
			apps[i], workspaces[i], in[i], out[i] = row.AppID, row.WorkspaceID, row.InBytes, row.OutBytes
		}
		if _, err := s.db.Exec(ctx, addAppTransferSQL, apps, workspaces, in, out, hour); err != nil {
			return appTransferTableError(err)
		}
	}
	if _, err := s.db.Exec(ctx, pruneAppTransferSQL, before.UTC()); err != nil {
		return appTransferTableError(err)
	}
	return nil
}
