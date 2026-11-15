// SPDX-License-Identifier: MIT

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
const listStoresSQL = `SELECT s.id::text, s.customer_id::text, s.workspace_id::text, COALESCE(w.namespace, ''),
       s.type, s.provider, s.name, s.slug, s.spec::text,
       s.desired, s.generation, st.store_id IS NOT NULL,
       COALESCE(st.phase, ''), COALESCE(st.message, ''), COALESCE(st.endpoint, ''),
       COALESCE(st.observed_generation, 0)
  FROM stores s
  JOIN workspaces w ON w.id = s.workspace_id
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
		if err := rows.Scan(&st.ID, &st.CustomerID, &st.WorkspaceID, &st.WorkspaceNamespace,
			&st.Type, &st.Provider, &st.Name, &st.Slug, &st.Spec,
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

// The store mounts half (lake migration 0080_store_mounts, lake#666).

// listStoreMountsSQL reads every mount joined to its store; the poll keeps the present ones.
const listStoreMountsSQL = `SELECT m.store_id::text, m.object_kind, m.object_id::text, m.mount_path, m.read_only,
       s.slug, s.type, s.provider, s.name, s.desired
  FROM store_mounts m
  JOIN stores s ON s.id = m.store_id
 ORDER BY m.object_kind, m.object_id, s.name, m.store_id`

// listStoreUsageSQL reads the used space the operator last wrote.
const listStoreUsageSQL = `SELECT store_id::text, used_bytes, used_bytes_at
  FROM store_status
 WHERE used_bytes IS NOT NULL`

// updateStoreUsedBytesSQL writes a store's used space beside its status.
const updateStoreUsedBytesSQL = `UPDATE store_status
   SET used_bytes = $2, used_bytes_at = now()
 WHERE store_id = $1::uuid`

// pgUndefinedColumn is SQLSTATE 42703, undefined_column: store_status before 0080 has no
// used_bytes.
const pgUndefinedColumn = "42703"

// storeMountsTableError turns a missing store_mounts table or used_bytes column into
// errStoreMountsTableMissing.
func storeMountsTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == pgUndefinedTable || pgErr.Code == pgUndefinedColumn) {
		return errStoreMountsTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListStoreMounts(ctx context.Context) ([]LakeStoreMount, error) {
	rows, err := s.db.Query(ctx, listStoreMountsSQL)
	if err != nil {
		return nil, storeMountsTableError(err)
	}
	defer rows.Close()
	var mounts []LakeStoreMount
	for rows.Next() {
		var m LakeStoreMount
		if err := rows.Scan(&m.StoreID, &m.ObjectKind, &m.ObjectID, &m.MountPath, &m.ReadOnly,
			&m.Slug, &m.Type, &m.Provider, &m.Name, &m.Desired); err != nil {
			return nil, err
		}
		mounts = append(mounts, m)
	}
	if err := rows.Err(); err != nil {
		return nil, storeMountsTableError(err)
	}
	return mounts, nil
}

func (s *pgxServiceStore) ListStoreUsage(ctx context.Context) (map[string]LakeStoreUsage, error) {
	rows, err := s.db.Query(ctx, listStoreUsageSQL)
	if err != nil {
		return nil, storeMountsTableError(err)
	}
	defer rows.Close()
	usage := map[string]LakeStoreUsage{}
	for rows.Next() {
		var id string
		var u LakeStoreUsage
		if err := rows.Scan(&id, &u.UsedBytes, &u.At); err != nil {
			return nil, err
		}
		usage[id] = u
	}
	if err := rows.Err(); err != nil {
		return nil, storeMountsTableError(err)
	}
	return usage, nil
}

func (s *pgxServiceStore) UpdateStoreUsedBytes(ctx context.Context, storeID string, usedBytes int64) error {
	_, err := s.db.Exec(ctx, updateStoreUsedBytesSQL, storeID, usedBytes)
	return storeMountsTableError(err)
}
