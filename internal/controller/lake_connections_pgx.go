/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
)

var errNetworkGuardMissing = errors.New("the network_guard table is not installed yet")

const networkGuardSQL = `SELECT to_regclass('network_guard') IS NOT NULL`

const listNetworkWorkspacesSQL = `SELECT id::text, namespace
  FROM workspaces
 WHERE COALESCE(namespace, '') <> ''
 ORDER BY created_at, id`

const listNetworkConnectionsSQL = `SELECT c.id::text, c.customer_id::text, c.workspace_id::text,
       w.namespace, c.source_kind, c.source_id::text, c.target_kind, c.target_id::text,
       c.ports, c.credential_scope IS NOT NULL, c.desired, c.generation,
       st.connection_id IS NOT NULL, COALESCE(st.phase, ''), COALESCE(st.message, ''),
       COALESCE(st.credential_secret, ''), COALESCE(st.observed_generation, 0)
  FROM network_connections c
  JOIN workspaces w ON w.id = c.workspace_id
  LEFT JOIN network_connection_status st ON st.connection_id = c.id
 WHERE NOT (c.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted')
 ORDER BY c.created_at, c.id`

const upsertNetworkConnectionStatusSQL = `INSERT INTO network_connection_status
    (connection_id, phase, message, credential_secret, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, '', $4, now())
ON CONFLICT (connection_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    credential_secret = '',
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

func (s *pgxServiceStore) ListNetworkConnections(ctx context.Context) (LakeNetworkSnapshot, error) {
	var installed bool
	guardRows, err := s.db.Query(ctx, networkGuardSQL)
	if err != nil {
		return LakeNetworkSnapshot{}, err
	}
	if !guardRows.Next() {
		err := guardRows.Err()
		guardRows.Close()
		if err != nil {
			return LakeNetworkSnapshot{}, err
		}
		return LakeNetworkSnapshot{}, errors.New("network guard query returned no row")
	}
	if err := guardRows.Scan(&installed); err != nil {
		guardRows.Close()
		return LakeNetworkSnapshot{}, err
	}
	guardRows.Close()
	if !installed {
		return LakeNetworkSnapshot{}, errNetworkGuardMissing
	}

	workspaceRows, err := s.db.Query(ctx, listNetworkWorkspacesSQL)
	if err != nil {
		return LakeNetworkSnapshot{}, err
	}
	var snapshot LakeNetworkSnapshot
	for workspaceRows.Next() {
		var workspace LakeNetworkWorkspace
		if err := workspaceRows.Scan(&workspace.ID, &workspace.Namespace); err != nil {
			workspaceRows.Close()
			return LakeNetworkSnapshot{}, err
		}
		snapshot.Workspaces = append(snapshot.Workspaces, workspace)
	}
	if err := workspaceRows.Err(); err != nil {
		workspaceRows.Close()
		return LakeNetworkSnapshot{}, err
	}
	workspaceRows.Close()

	rows, err := s.db.Query(ctx, listNetworkConnectionsSQL)
	if err != nil {
		return LakeNetworkSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var row LakeNetworkConnection
		var hasStatus bool
		var status LakeNetworkConnectionStatus
		if err := rows.Scan(
			&row.ID,
			&row.CustomerID,
			&row.WorkspaceID,
			&row.WorkspaceNamespace,
			&row.SourceKind,
			&row.SourceID,
			&row.TargetKind,
			&row.TargetID,
			&row.Ports,
			&row.HasCredentialScope,
			&row.Desired,
			&row.Generation,
			&hasStatus,
			&status.Phase,
			&status.Message,
			&status.CredentialSecret,
			&status.ObservedGeneration,
		); err != nil {
			return LakeNetworkSnapshot{}, err
		}
		if hasStatus {
			status.ConnectionID = row.ID
			row.Status = &status
		}
		snapshot.Connections = append(snapshot.Connections, row)
	}
	return snapshot, rows.Err()
}

func (s *pgxServiceStore) UpsertNetworkConnectionStatus(
	ctx context.Context,
	status LakeNetworkConnectionStatus,
) error {
	_, err := s.db.Exec(
		ctx,
		upsertNetworkConnectionStatusSQL,
		status.ConnectionID,
		status.Phase,
		status.Message,
		status.ObservedGeneration,
	)
	return err
}
