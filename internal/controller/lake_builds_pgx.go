/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

const listBuildsSQL = `SELECT id::text, customer_id::text, repository, revision, context, dockerfile, target,
       COALESCE(build_args_json, '{}'::jsonb)::text, desired, COALESCE(phase, ''),
       COALESCE(digest, ''), COALESCE(size, 0), COALESCE(message, ''),
       COALESCE(token_hashes_json, '{}'::jsonb)::text
  FROM builds
 WHERE (desired = 'queued' AND COALESCE(phase, '') NOT IN ('Succeeded', 'Failed'))
    OR (desired = 'cancelled' AND COALESCE(phase, '') <> 'Cancelled')
 ORDER BY created_at, id`

const updateBuildStatusSQL = `UPDATE builds
   SET phase = $2, digest = $3, size = $4, message = $5, token_hashes_json = $6::jsonb, updated_at = now()
 WHERE id = $1::uuid`

func buildsTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errBuildsTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListBuilds(ctx context.Context) ([]LakeBuild, error) {
	rows, err := s.db.Query(ctx, listBuildsSQL)
	if err != nil {
		return nil, buildsTableError(err)
	}
	defer rows.Close()
	var builds []LakeBuild
	for rows.Next() {
		var build LakeBuild
		if err := rows.Scan(
			&build.ID,
			&build.CustomerID,
			&build.Repository,
			&build.Revision,
			&build.Context,
			&build.Dockerfile,
			&build.Target,
			&build.BuildArgs,
			&build.Desired,
			&build.Phase,
			&build.Digest,
			&build.Size,
			&build.Message,
			&build.TokenHashes,
		); err != nil {
			return nil, err
		}
		builds = append(builds, build)
	}
	if err := rows.Err(); err != nil {
		return nil, buildsTableError(err)
	}
	return builds, nil
}

func (s *pgxServiceStore) UpdateBuildStatus(ctx context.Context, status LakeBuildStatus) error {
	result, err := s.db.Exec(ctx, updateBuildStatusSQL,
		status.BuildID,
		status.Phase,
		status.Digest,
		status.Size,
		status.Message,
		status.TokenHashes,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("build %s no longer exists", status.BuildID)
	}
	return nil
}
