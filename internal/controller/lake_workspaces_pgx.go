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
	ID                     string
	CustomerID             string
	Name                   string
	Environment            string
	Desired                string
	Generation             int64
	Phase                  string
	ObservedGeneration     int64
	Message                string
	Namespace              string
	ZettaServiceID         string
	ZettaServiceGeneration int64
	ZettaServiceStatus     *LakeServiceStatus
	// StorageGi is the desired size of each of the workspace Zetta's volumes in GiB (lake
	// migration 0069, operator#259); lake's trigger keeps it from decreasing.
	StorageGi         int32
	StorageGiObserved int32
	StoragePhase      string
	StorageMessage    string
}

// LakeWorkspaceStatus is what the operator writes on the workspace row. The storage fields are
// the volumes' observation: the smallest claim capacity in GiB, Pending | Resizing | Ready |
// Failed, and why.
type LakeWorkspaceStatus struct {
	Phase              string
	ObservedGeneration int64
	Message            string
	Namespace          string
	StorageGiObserved  int32
	StoragePhase       string
	StorageMessage     string
}

func (row *LakeWorkspaceRow) status() LakeWorkspaceStatus {
	return LakeWorkspaceStatus{
		Phase:              row.Phase,
		ObservedGeneration: row.ObservedGeneration,
		Message:            row.Message,
		Namespace:          row.Namespace,
		StorageGiObserved:  row.StorageGiObserved,
		StoragePhase:       row.StoragePhase,
		StorageMessage:     row.StorageMessage,
	}
}

// LakeWorkspacesStore uses generation-guarded updates so an observation from an old list cannot
// acknowledge newer desired state.
type LakeWorkspacesStore interface {
	ListWorkspaces(context.Context) ([]LakeWorkspaceRow, error)
	UpdateWorkspaceStatus(context.Context, *LakeWorkspaceRow, LakeWorkspaceStatus) (bool, error)
	UpsertServiceStatus(context.Context, LakeServiceStatus) error
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

const listWorkspacesSQL = `SELECT w.id::text, w.customer_id::text, w.name, w.environment, w.desired, w.generation,
 COALESCE(w.phase, ''), COALESCE(w.observed_generation, 0), COALESCE(w.message, ''),
 COALESCE(w.namespace, ''), COALESCE(w.zetta_service_id::text, ''), COALESCE(s.generation, 0),
 ss.service_id IS NOT NULL, COALESCE(ss.phase, ''), COALESCE(ss.message, ''),
 COALESCE(ss.endpoint, ''), COALESCE(ss.observed_generation, 0),
 w.storage_gi, w.storage_gi_observed, w.storage_phase, w.storage_message
 FROM workspaces w
 LEFT JOIN services s ON s.id = w.zetta_service_id
 LEFT JOIN service_status ss ON ss.service_id = w.zetta_service_id
 WHERE NOT (w.desired = 'deleted' AND COALESCE(w.phase, '') = 'Deleted')
 ORDER BY w.created_at, w.id`

func (s *pgxWorkspacesStore) ListWorkspaces(ctx context.Context) ([]LakeWorkspaceRow, error) {
	rows, err := s.db.Query(ctx, listWorkspacesSQL)
	if err != nil {
		return nil, workspacesReadError(err)
	}
	defer rows.Close()
	var result []LakeWorkspaceRow
	for rows.Next() {
		var row LakeWorkspaceRow
		var hasServiceStatus bool
		var serviceStatus LakeServiceStatus
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
			&row.ZettaServiceGeneration,
			&hasServiceStatus,
			&serviceStatus.Phase,
			&serviceStatus.Message,
			&serviceStatus.Endpoint,
			&serviceStatus.ObservedGeneration,
			&row.StorageGi,
			&row.StorageGiObserved,
			&row.StoragePhase,
			&row.StorageMessage,
		); err != nil {
			return nil, errors.New("cannot decode workspaces row")
		}
		if hasServiceStatus {
			serviceStatus.ServiceID = row.ZettaServiceID
			row.ZettaServiceStatus = &serviceStatus
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
 SET phase=$5, observed_generation=$6, message=$7, namespace=$8,
 storage_gi_observed=$9, storage_phase=$10, storage_message=$11, updated_at=now()
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
		status.StorageGiObserved,
		status.StoragePhase,
		status.StorageMessage,
	)
	if err != nil {
		return false, errors.New("cannot update workspaces row")
	}
	return result.RowsAffected() == 1, nil
}
