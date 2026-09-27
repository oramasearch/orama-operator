/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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

// The agent cron schedules half (lake migration 0031_agent_cron_schedules).

// listAgentSchedulesSQL reads every schedule that still needs the operator, as for launches, with
// the phase of its latest runs by id (a CronJob keeps at most six Jobs).
const listAgentSchedulesSQL = `SELECT s.id::text, s.customer_id::text, s.agent_id, s.cron, s.time_zone, s.input::text,
       s.slug, s.desired, s.generation, ss.schedule_id IS NOT NULL,
       COALESCE(ss.phase, ''), COALESCE(ss.message, ''), ss.last_schedule_at,
       COALESCE(ss.credential_sha256, ''), COALESCE(ss.observed_generation, 0),
       COALESCE((SELECT json_object_agg(r.id::text, r.phase) FROM (SELECT id, phase FROM agent_cron_runs
                  WHERE schedule_id = s.id ORDER BY started_at DESC LIMIT 20) r), '{}')::text
  FROM agent_cron_schedules s
  LEFT JOIN agent_cron_schedule_status ss ON ss.schedule_id = s.id
 WHERE s.desired = 'present'
    OR ss.schedule_id IS NULL
    OR ss.observed_generation <> s.generation
    OR ss.credential_sha256 <> ''
    OR ss.phase <> 'Stopped'
 ORDER BY s.created_at, s.id`

const upsertAgentScheduleStatusSQL = `INSERT INTO agent_cron_schedule_status (schedule_id, phase, message, last_schedule_at,
    credential_sha256, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, now())
ON CONFLICT (schedule_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    last_schedule_at = EXCLUDED.last_schedule_at,
    credential_sha256 = EXCLUDED.credential_sha256,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

const upsertAgentScheduleRunSQL = `INSERT INTO agent_cron_runs (id, schedule_id, job_name, phase, message, exit_code, result,
    started_at, finished_at, updated_at)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::jsonb, $8, $9, now())
ON CONFLICT (id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    exit_code = EXCLUDED.exit_code,
    result = EXCLUDED.result,
    finished_at = EXCLUDED.finished_at,
    updated_at = now()`

func agentSchedulesTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errAgentSchedulesTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListAgentSchedules(ctx context.Context) ([]LakeAgentSchedule, error) {
	rows, err := s.db.Query(ctx, listAgentSchedulesSQL)
	if err != nil {
		return nil, agentSchedulesTableError(err)
	}
	defer rows.Close()
	var schedules []LakeAgentSchedule
	for rows.Next() {
		var schedule LakeAgentSchedule
		var hasStatus bool
		var status LakeAgentScheduleStatus
		var runs string
		if err := rows.Scan(&schedule.ID, &schedule.CustomerID, &schedule.AgentID, &schedule.Cron, &schedule.TimeZone,
			&schedule.Input, &schedule.Slug, &schedule.Desired, &schedule.Generation, &hasStatus,
			&status.Phase, &status.Message, &status.LastScheduleTime, &status.CredentialSHA256,
			&status.ObservedGeneration, &runs); err != nil {
			return nil, err
		}
		if hasStatus {
			status.ScheduleID = schedule.ID
			if status.LastScheduleTime != nil {
				at := status.LastScheduleTime.UTC().Truncate(time.Second)
				status.LastScheduleTime = &at
			}
			schedule.Status = &status
		}
		if err := json.Unmarshal([]byte(runs), &schedule.RunPhases); err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, agentSchedulesTableError(err)
	}
	return schedules, nil
}

func (s *pgxServiceStore) UpsertAgentScheduleStatus(ctx context.Context, status LakeAgentScheduleStatus) error {
	_, err := s.db.Exec(ctx, upsertAgentScheduleStatusSQL,
		status.ScheduleID, status.Phase, status.Message, status.LastScheduleTime,
		status.CredentialSHA256, status.ObservedGeneration)
	return err
}

func (s *pgxServiceStore) UpsertAgentScheduleRun(ctx context.Context, run LakeAgentScheduleRun) error {
	result := run.Result
	if result == "" {
		result = "null"
	}
	_, err := s.db.Exec(ctx, upsertAgentScheduleRunSQL,
		run.ID, run.ScheduleID, run.JobName, run.Phase, run.Message, run.ExitCode, result,
		run.StartedAt, run.FinishedAt)
	return err
}
