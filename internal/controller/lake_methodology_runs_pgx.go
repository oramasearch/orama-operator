// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The methodology runs half of the pgx LakeServiceStore (lake migration 0078_methodology_runs,
// lake#655): the same connection as the services poll.

// listMethodologyRunsSQL reads every run that still needs the operator: present ones, and absent
// ones not yet reported Stopped, Succeeded or Failed at their generation or whose credential
// digest is still set. Each comes with its image build's state.
const listMethodologyRunsSQL = `SELECT m.id::text, m.customer_id::text, m.workspace_id::text, COALESCE(w.namespace, ''),
       m.methodology_id, m.mode,
       COALESCE(m.build_id::text, ''), m.image, m.runtime_image, m.cpu_millis, m.memory_bytes, m.gpus,
       m.gpu_class, m.timeout_seconds, m.network, m.slug, m.desired, m.generation,
       b.id IS NOT NULL, COALESCE(b.desired, ''), COALESCE(b.phase, ''), COALESCE(b.digest, ''),
       COALESCE(b.target, ''), COALESCE(b.message, ''),
       ms.run_id IS NOT NULL, COALESCE(ms.phase, ''), COALESCE(ms.message, ''), ms.exit_code,
       COALESCE(ms.credential_sha256, ''), COALESCE(ms.observed_generation, 0), ms.started_at, ms.finished_at
  FROM methodology_runs m
  LEFT JOIN workspaces w ON w.id = m.workspace_id AND w.customer_id = m.customer_id
  LEFT JOIN builds b ON b.id = m.build_id AND b.customer_id = m.customer_id
  LEFT JOIN methodology_run_status ms ON ms.run_id = m.id
 WHERE m.desired = 'present'
    OR ms.run_id IS NULL
    OR ms.observed_generation <> m.generation
    OR ms.credential_sha256 <> ''
    OR ms.phase NOT IN ('Stopped', 'Succeeded', 'Failed')
 ORDER BY m.created_at, m.id`

const upsertMethodologyRunStatusSQL = `INSERT INTO methodology_run_status (run_id, phase, message, exit_code,
    credential_sha256, observed_generation, started_at, finished_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (run_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    exit_code = EXCLUDED.exit_code,
    credential_sha256 = EXCLUDED.credential_sha256,
    observed_generation = EXCLUDED.observed_generation,
    started_at = EXCLUDED.started_at,
    finished_at = EXCLUDED.finished_at,
    updated_at = now()`

// listMethodologyRunObjectsSQL: the methodology runs among the ids the workload pass asks about,
// in any state (a run is never in the live read: it gets a row once its sandbox is seen).
const listMethodologyRunObjectsSQL = `SELECT 'methodology_run', m.id::text, '', m.desired, m.mode, m.slug
  FROM methodology_runs m
 WHERE m.id = ANY($1::text[]::uuid[])`

func methodologyRunsTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errMethodologyRunsTableMissing
	}
	return err
}

func utcSecond(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	value := at.UTC().Truncate(time.Second)
	return &value
}

func (s *pgxServiceStore) ListMethodologyRuns(ctx context.Context) ([]LakeMethodologyRun, error) {
	rows, err := s.db.Query(ctx, listMethodologyRunsSQL)
	if err != nil {
		return nil, methodologyRunsTableError(err)
	}
	defer rows.Close()
	var runs []LakeMethodologyRun
	for rows.Next() {
		var run LakeMethodologyRun
		var build LakeMethodologyRunBuild
		var status LakeMethodologyRunStatus
		var hasBuild, hasStatus bool
		if err := rows.Scan(&run.ID, &run.CustomerID, &run.WorkspaceID, &run.WorkspaceNamespace, &run.MethodologyID, &run.Mode,
			&run.BuildID, &run.Image, &run.RuntimeImage, &run.CPUMillis, &run.MemoryBytes, &run.GPUs,
			&run.GPUClass, &run.TimeoutSeconds, &run.Network, &run.Slug, &run.Desired, &run.Generation,
			&hasBuild, &build.Desired, &build.Phase, &build.Digest, &build.Target, &build.Message,
			&hasStatus, &status.Phase, &status.Message, &status.ExitCode,
			&status.CredentialSHA256, &status.ObservedGeneration, &status.StartedAt, &status.FinishedAt); err != nil {
			return nil, err
		}
		if hasBuild {
			run.Build = &build
		}
		if hasStatus {
			status.RunID = run.ID
			status.StartedAt, status.FinishedAt = utcSecond(status.StartedAt), utcSecond(status.FinishedAt)
			run.Status = &status
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, methodologyRunsTableError(err)
	}
	return runs, nil
}

func (s *pgxServiceStore) UpsertMethodologyRunStatus(ctx context.Context, status LakeMethodologyRunStatus) error {
	_, err := s.db.Exec(ctx, upsertMethodologyRunStatusSQL,
		status.RunID, status.Phase, status.Message, status.ExitCode,
		status.CredentialSHA256, status.ObservedGeneration, status.StartedAt, status.FinishedAt)
	return err
}

// listMethodologyRunObjects returns nothing, without an error, before 0078_methodology_runs: the
// workload pass then simply finds no methodology runs.
func (s *pgxServiceStore) listMethodologyRunObjects(ctx context.Context, ids []string) ([]WorkloadObject, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, listMethodologyRunObjectsSQL, ids)
	if err != nil {
		if errors.Is(methodologyRunsTableError(err), errMethodologyRunsTableMissing) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []WorkloadObject
	for rows.Next() {
		var object WorkloadObject
		if err := rows.Scan(&object.Kind, &object.ID, &object.Namespace, &object.Desired,
			&object.Subkind, &object.Slug); err != nil {
			return nil, err
		}
		out = append(out, object)
	}
	if err := rows.Err(); err != nil {
		if errors.Is(methodologyRunsTableError(err), errMethodologyRunsTableMissing) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}
