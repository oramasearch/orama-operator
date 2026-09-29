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

var errWorkspacesTableMissing = errors.New("the workspaces table is not installed yet")

// LakeWorkspaceRow is lake's desired workspace row and the observation last written by the
// operator. The SQL contract is owned by lake#265/lake#311.
type LakeWorkspaceRow struct {
	ID                 string
	CustomerID         string
	Name               string
	Environment        string
	Desired            string
	Generation         int64
	Phase              string
	ObservedGeneration int64
	Message            string
	Namespace          string
	ZettaServiceID     string
}

type LakeWorkspaceStatus struct {
	Phase              string
	ObservedGeneration int64
	Message            string
	Namespace          string
}

func (row *LakeWorkspaceRow) status() LakeWorkspaceStatus {
	return LakeWorkspaceStatus{
		Phase:              row.Phase,
		ObservedGeneration: row.ObservedGeneration,
		Message:            row.Message,
		Namespace:          row.Namespace,
	}
}

// LakeWorkspacesStore uses generation-guarded updates so an observation from an old list cannot
// acknowledge newer desired state.
type LakeWorkspacesStore interface {
	ListWorkspaces(context.Context) ([]LakeWorkspaceRow, error)
	UpdateWorkspaceStatus(context.Context, *LakeWorkspaceRow, LakeWorkspaceStatus) (bool, error)
	Close(context.Context) error
}

type LakeWorkspacesStoreOpener func(context.Context, string) (LakeWorkspacesStore, error)

type pgxWorkspacesStore struct {
	*pgxServiceStore
}

func openPgxWorkspacesStore(ctx context.Context, uri string) (LakeWorkspacesStore, error) {
	store, err := openPgxServiceStore(ctx, uri)
	if err != nil {
		return nil, errors.New("cannot connect to Lake control Postgres")
	}
	return &pgxWorkspacesStore{pgxServiceStore: store.(*pgxServiceStore)}, nil
}

const listWorkspacesSQL = `SELECT id::text, customer_id::text, name, environment, desired, generation,
 COALESCE(phase, ''), COALESCE(observed_generation, 0), COALESCE(message, ''),
 COALESCE(namespace, ''), COALESCE(zetta_service_id::text, '')
 FROM workspaces
 WHERE NOT (desired = 'deleted' AND COALESCE(phase, '') = 'Deleted')
 ORDER BY created_at, id`

func (s *pgxWorkspacesStore) ListWorkspaces(ctx context.Context) ([]LakeWorkspaceRow, error) {
	rows, err := s.db.Query(ctx, listWorkspacesSQL)
	if err != nil {
		return nil, workspacesReadError(err)
	}
	defer rows.Close()
	var result []LakeWorkspaceRow
	for rows.Next() {
		var row LakeWorkspaceRow
		if err := rows.Scan(
			&row.ID,
			&row.CustomerID,
			&row.Name,
			&row.Environment,
			&row.Desired,
			&row.Generation,
			&row.Phase,
			&row.ObservedGeneration,
			&row.Message,
			&row.Namespace,
			&row.ZettaServiceID,
		); err != nil {
			return nil, errors.New("cannot decode workspaces row")
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, workspacesReadError(err)
	}
	return result, nil
}

func workspacesReadError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errWorkspacesTableMissing
	}
	return errors.New("workspaces list did not complete")
}

const updateWorkspaceStatusSQL = `UPDATE workspaces
 SET phase=$5, observed_generation=$6, message=$7, namespace=$8, updated_at=now()
 WHERE id=$1::uuid AND customer_id=$2::uuid AND generation=$3 AND desired=$4`

func (s *pgxWorkspacesStore) UpdateWorkspaceStatus(
	ctx context.Context,
	row *LakeWorkspaceRow,
	status LakeWorkspaceStatus,
) (bool, error) {
	result, err := s.db.Exec(
		ctx,
		updateWorkspaceStatusSQL,
		row.ID,
		row.CustomerID,
		row.Generation,
		row.Desired,
		status.Phase,
		status.ObservedGeneration,
		status.Message,
		status.Namespace,
	)
	if err != nil {
		return false, errors.New("cannot update workspaces row")
	}
	return result.RowsAffected() == 1, nil
}
