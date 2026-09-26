/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// The stores half of the pgx LakeServiceStore (lake migration 0028_stores): the same connection
// as the services poll.

// listStoresSQL reads every store with its status, oldest first, leaving out history (absent and
// already Deleted).
const listStoresSQL = `SELECT s.id::text, s.customer_id::text, s.type, s.provider, s.name, s.slug, s.spec::text,
       s.desired, s.generation, st.store_id IS NOT NULL,
       COALESCE(st.phase, ''), COALESCE(st.message, ''), COALESCE(st.endpoint, ''),
       COALESCE(st.observed_generation, 0)
  FROM stores s
  LEFT JOIN store_status st ON st.store_id = s.id
 WHERE NOT (s.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted')
 ORDER BY s.created_at, s.id`

const upsertStoreStatusSQL = `INSERT INTO store_status (store_id, phase, message, endpoint, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, now())
ON CONFLICT (store_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    endpoint = EXCLUDED.endpoint,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

// pgUndefinedTable is SQLSTATE 42P01, undefined_table.
const pgUndefinedTable = "42P01"

// storesTableError turns "relation stores does not exist" into errStoresTableMissing.
func storesTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errStoresTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListStores(ctx context.Context) ([]LakeStore, error) {
	rows, err := s.db.Query(ctx, listStoresSQL)
	if err != nil {
		return nil, storesTableError(err)
	}
	defer rows.Close()
	var stores []LakeStore
	for rows.Next() {
		var st LakeStore
		var hasStatus bool
		var status LakeStoreStatus
		if err := rows.Scan(&st.ID, &st.CustomerID, &st.Type, &st.Provider, &st.Name, &st.Slug, &st.Spec,
			&st.Desired, &st.Generation, &hasStatus,
			&status.Phase, &status.Message, &status.Endpoint, &status.ObservedGeneration); err != nil {
			return nil, err
		}
		if hasStatus {
			status.StoreID = st.ID
			st.Status = &status
		}
		stores = append(stores, st)
	}
	if err := rows.Err(); err != nil {
		return nil, storesTableError(err)
	}
	return stores, nil
}

func (s *pgxServiceStore) UpsertStoreStatus(ctx context.Context, status LakeStoreStatus) error {
	_, err := s.db.Exec(ctx, upsertStoreStatusSQL,
		status.StoreID, status.Phase, status.Message, status.Endpoint, status.ObservedGeneration)
	return err
}
