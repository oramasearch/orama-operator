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

// The workload resources half of the pgx LakeServiceStore (lake migration
// 0075_workload_resources, lake#643): the same connection as the services poll.

const listWorkloadResourcesSQL = `SELECT object_kind, object_id::text, pods, cpu_millis, memory_bytes, gpus, gpu_class, vram_bytes
  FROM workload_resources
 ORDER BY object_kind, object_id`

// listWorkloadObjectsSQL reads the objects the operator runs pods for. With $2 true it returns
// every live one: workspaces, services, apps not yet Deleted, and service-mode agent launches
// that are present (a job launch or a build gets a row only once a pod or Job of it is seen).
// Every object whose id is in $1 is returned too, whatever its state, so a row or a pod whose
// object lake no longer has is the one id missing from the result. A workspace's own Zetta is its
// workspace's row, never a service's.
const listWorkloadObjectsSQL = `SELECT 'workspace', w.id::text, COALESCE(w.namespace, ''), w.desired, '', ''
  FROM workspaces w
 WHERE ($2::boolean AND NOT (w.desired = 'deleted' AND COALESCE(w.phase, '') = 'Deleted'))
    OR w.id = ANY($1::text[]::uuid[])
UNION ALL
SELECT 'service', s.id::text, COALESCE(w.namespace, ''), s.desired, s.kind, s.slug
  FROM services s
  JOIN workspaces w ON w.id = s.workspace_id
  LEFT JOIN service_status st ON st.service_id = s.id
 WHERE NOT EXISTS (SELECT 1 FROM workspaces z WHERE z.zetta_service_id = s.id)
   AND (($2::boolean AND NOT (s.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted'))
        OR s.id = ANY($1::text[]::uuid[]))
UNION ALL
SELECT 'app', a.id::text, COALESCE(w.namespace, ''), a.desired, a.mode, ''
  FROM apps a
  JOIN workspaces w ON w.id = a.workspace_id
 WHERE ($2::boolean AND NOT (a.desired = 'deleted' AND COALESCE(a.phase, '') = 'Deleted'))
    OR a.id = ANY($1::text[]::uuid[])
UNION ALL
SELECT 'build', b.id::text, '', b.desired, '', ''
  FROM builds b
 WHERE b.id = ANY($1::text[]::uuid[])
UNION ALL
SELECT 'agent_launch', l.id::text, COALESCE(w.namespace, ''), l.desired, l.mode, l.slug
  FROM agent_launches l
  JOIN workspaces w ON w.id = l.workspace_id
 WHERE ($2::boolean AND l.mode = 'service' AND l.desired = 'present')
    OR l.id = ANY($1::text[]::uuid[])
UNION ALL
SELECT 'agent_cron_run', r.id::text, COALESCE(w.namespace, ''), s.desired, 'job', s.slug
  FROM agent_cron_runs r
  JOIN agent_cron_schedules s ON s.id = r.schedule_id
  JOIN workspaces w ON w.id = s.workspace_id
 WHERE r.id = ANY($1::text[]::uuid[])`

// listUnmeteredJobsSQL reads the finished runs with no workload_resources row (lake#658): builds
// and job-mode launches that Succeeded or Failed (a Cancelled build may never have run), and cron
// runs that Succeeded or Failed. The operator backfills them from the pod spec it schedules such
// runs with.
const listUnmeteredJobsSQL = `SELECT 'build', b.id::text, '', b.desired, '', ''
  FROM builds b
 WHERE b.phase IN ('Succeeded', 'Failed')
   AND NOT EXISTS (SELECT 1 FROM workload_resources x
                    WHERE x.object_kind = 'build' AND x.object_id = b.id)
UNION ALL
SELECT 'agent_launch', l.id::text, COALESCE(w.namespace, ''), l.desired, l.mode, l.slug
  FROM agent_launches l
  JOIN workspaces w ON w.id = l.workspace_id
  JOIN agent_launch_status st ON st.launch_id = l.id
 WHERE l.mode = 'job' AND st.phase IN ('Succeeded', 'Failed')
   AND NOT EXISTS (SELECT 1 FROM workload_resources x
                    WHERE x.object_kind = 'agent_launch' AND x.object_id = l.id)
UNION ALL
SELECT 'agent_cron_run', r.id::text, COALESCE(w.namespace, ''), s.desired, 'job', s.slug
  FROM agent_cron_runs r
  JOIN agent_cron_schedules s ON s.id = r.schedule_id
  JOIN workspaces w ON w.id = s.workspace_id
 WHERE r.phase IN ('Succeeded', 'Failed')
   AND NOT EXISTS (SELECT 1 FROM workload_resources x
                    WHERE x.object_kind = 'agent_cron_run' AND x.object_id = r.id)`

const upsertWorkloadResourcesSQL = `INSERT INTO workload_resources (object_kind, object_id, pods, cpu_millis, memory_bytes,
    gpus, gpu_class, vram_bytes, observed_at)
VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (object_kind, object_id) DO UPDATE SET
    pods = EXCLUDED.pods,
    cpu_millis = EXCLUDED.cpu_millis,
    memory_bytes = EXCLUDED.memory_bytes,
    gpus = EXCLUDED.gpus,
    gpu_class = EXCLUDED.gpu_class,
    vram_bytes = EXCLUDED.vram_bytes,
    observed_at = now()`

const deleteWorkloadResourcesSQL = `DELETE FROM workload_resources WHERE object_kind = $1 AND object_id = $2::uuid`

func workloadResourcesTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errWorkloadResourcesTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListWorkloadResources(ctx context.Context) ([]WorkloadResources, error) {
	rows, err := s.db.Query(ctx, listWorkloadResourcesSQL)
	if err != nil {
		return nil, workloadResourcesTableError(err)
	}
	defer rows.Close()
	var out []WorkloadResources
	for rows.Next() {
		var row WorkloadResources
		if err := rows.Scan(&row.Kind, &row.ID, &row.Pods, &row.CPUMillis, &row.MemoryBytes,
			&row.GPUs, &row.GPUClass, &row.VRAMBytes); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, workloadResourcesTableError(err)
	}
	return out, nil
}

func (s *pgxServiceStore) ListWorkloadObjects(ctx context.Context, ids []string, live bool) ([]WorkloadObject, error) {
	if ids == nil {
		ids = []string{}
	}
	rows, err := s.db.Query(ctx, listWorkloadObjectsSQL, ids, live)
	if err != nil {
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
		return nil, err
	}
	// Methodology runs (lake#655) are a separate read, so a schema before 0078 leaves the rest.
	runs, err := s.listMethodologyRunObjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Served methodology models (lake#690) likewise, so a schema before 0083 leaves the rest.
	deployments, err := s.listMethodologyDeploymentObjects(ctx, ids, live)
	if err != nil {
		return nil, err
	}
	return append(append(out, runs...), deployments...), nil
}

func (s *pgxServiceStore) ListUnmeteredJobs(ctx context.Context) ([]WorkloadObject, error) {
	rows, err := s.db.Query(ctx, listUnmeteredJobsSQL)
	if err != nil {
		return nil, workloadResourcesTableError(err)
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
	return out, rows.Err()
}

func (s *pgxServiceStore) UpsertWorkloadResources(ctx context.Context, row WorkloadResources) error {
	_, err := s.db.Exec(ctx, upsertWorkloadResourcesSQL, row.Kind, row.ID, row.Pods, row.CPUMillis,
		row.MemoryBytes, row.GPUs, row.GPUClass, row.VRAMBytes)
	return err
}

func (s *pgxServiceStore) DeleteWorkloadResources(ctx context.Context, kind, id string) error {
	_, err := s.db.Exec(ctx, deleteWorkloadResourcesSQL, kind, id)
	return err
}
