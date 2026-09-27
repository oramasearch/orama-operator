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

// The agent launches half of the pgx LakeServiceStore (lake migration 0030_agent_launches): the
// same connection as the services poll.

// listAgentLaunchesSQL reads every launch that still needs the operator: present ones, and absent
// ones whose resources may not be gone yet (their status is not yet Stopped, Succeeded or Failed at
// the current generation, or their credential digest is still set).
const listAgentLaunchesSQL = `SELECT l.id::text, l.customer_id::text, l.agent_id, l.mode, l.input::text, l.slug,
       l.desired, l.generation, ls.launch_id IS NOT NULL,
       COALESCE(ls.phase, ''), COALESCE(ls.message, ''), COALESCE(ls.endpoint, ''), ls.exit_code,
       COALESCE(ls.result, 'null'::jsonb)::text, COALESCE(ls.credential_sha256, ''),
       COALESCE(ls.observed_generation, 0)
  FROM agent_launches l
  LEFT JOIN agent_launch_status ls ON ls.launch_id = l.id
 WHERE l.desired = 'present'
    OR ls.launch_id IS NULL
    OR ls.observed_generation <> l.generation
    OR ls.credential_sha256 <> ''
    OR ls.phase NOT IN ('Stopped', 'Succeeded', 'Failed')
 ORDER BY l.created_at, l.id`

const upsertAgentLaunchStatusSQL = `INSERT INTO agent_launch_status (launch_id, phase, message, endpoint, exit_code, result,
    credential_sha256, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7, $8, now())
ON CONFLICT (launch_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    endpoint = EXCLUDED.endpoint,
    exit_code = EXCLUDED.exit_code,
    result = EXCLUDED.result,
    credential_sha256 = EXCLUDED.credential_sha256,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

func agentLaunchesTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errAgentLaunchesTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListAgentLaunches(ctx context.Context) ([]LakeAgentLaunch, error) {
	rows, err := s.db.Query(ctx, listAgentLaunchesSQL)
	if err != nil {
		return nil, agentLaunchesTableError(err)
	}
	defer rows.Close()
	var launches []LakeAgentLaunch
	for rows.Next() {
		var launch LakeAgentLaunch
		var hasStatus bool
		var status LakeAgentLaunchStatus
		if err := rows.Scan(&launch.ID, &launch.CustomerID, &launch.AgentID, &launch.Mode, &launch.Input, &launch.Slug,
			&launch.Desired, &launch.Generation, &hasStatus,
			&status.Phase, &status.Message, &status.Endpoint, &status.ExitCode,
			&status.Result, &status.CredentialSHA256, &status.ObservedGeneration); err != nil {
			return nil, err
		}
		if hasStatus {
			status.LaunchID = launch.ID
			launch.Status = &status
		}
		launches = append(launches, launch)
	}
	if err := rows.Err(); err != nil {
		return nil, agentLaunchesTableError(err)
	}
	return launches, nil
}

func (s *pgxServiceStore) UpsertAgentLaunchStatus(ctx context.Context, status LakeAgentLaunchStatus) error {
	result := status.Result
	if result == "" {
		result = "null"
	}
	_, err := s.db.Exec(ctx, upsertAgentLaunchStatusSQL,
		status.LaunchID, status.Phase, status.Message, status.Endpoint, status.ExitCode, result,
		status.CredentialSHA256, status.ObservedGeneration)
	return err
}
