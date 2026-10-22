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

// The served methodology models half of the pgx LakeServiceStore (lake migration
// 0083_methodology_deployments, lake#690): the same connection as the services poll.

// listMethodologyDeploymentsSQL reads every deployment that still needs the operator: present
// ones, and absent ones not yet reported Stopped or Failed at their generation or whose credential
// digest is still set. Each comes with its image build's state.
const listMethodologyDeploymentsSQL = `SELECT d.id::text, d.customer_id::text, d.workspace_id::text, COALESCE(w.namespace, ''),
       d.methodology_id, d.model_id,
       COALESCE(d.build_id::text, ''), d.image, d.runtime_image, d.cpu_millis, d.memory_bytes, d.gpus,
       d.gpu_class, d.network, d.slug, d.desired, d.generation,
       b.id IS NOT NULL, COALESCE(b.desired, ''), COALESCE(b.phase, ''), COALESCE(b.digest, ''),
       COALESCE(b.target, ''), COALESCE(b.message, ''),
       ds.deployment_id IS NOT NULL, COALESCE(ds.phase, ''), COALESCE(ds.message, ''),
       COALESCE(ds.credential_sha256, ''), COALESCE(ds.endpoint, ''), COALESCE(ds.observed_generation, 0),
       ds.ready_at, ds.stopped_at
  FROM methodology_deployments d
  LEFT JOIN workspaces w ON w.id = d.workspace_id
  LEFT JOIN builds b ON b.id = d.build_id
  LEFT JOIN methodology_deployment_status ds ON ds.deployment_id = d.id
 WHERE d.desired = 'present'
    OR ds.deployment_id IS NULL
    OR ds.observed_generation <> d.generation
    OR ds.credential_sha256 <> ''
    OR ds.phase NOT IN ('Stopped', 'Failed')
 ORDER BY d.created_at, d.id`

const upsertMethodologyDeploymentStatusSQL = `INSERT INTO methodology_deployment_status (deployment_id, phase, message,
    credential_sha256, endpoint, observed_generation, ready_at, stopped_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (deployment_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    credential_sha256 = EXCLUDED.credential_sha256,
    endpoint = EXCLUDED.endpoint,
    observed_generation = EXCLUDED.observed_generation,
    ready_at = EXCLUDED.ready_at,
    stopped_at = EXCLUDED.stopped_at,
    updated_at = now()`

// listMethodologyDeploymentObjectsSQL: with $2 true every present deployment (a live workload),
// and every deployment among the ids the workload pass asks about, in any state.
const listMethodologyDeploymentObjectsSQL = `SELECT 'methodology_deployment', d.id::text, '', d.desired, '', d.slug
  FROM methodology_deployments d
 WHERE ($2::boolean AND d.desired = 'present')
    OR d.id = ANY($1::text[]::uuid[])`

func methodologyDeploymentsTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return errMethodologyDeploymentsTableMissing
	}
	return err
}

func (s *pgxServiceStore) ListMethodologyDeployments(ctx context.Context) ([]LakeMethodologyDeployment, error) {
	rows, err := s.db.Query(ctx, listMethodologyDeploymentsSQL)
	if err != nil {
		return nil, methodologyDeploymentsTableError(err)
	}
	defer rows.Close()
	var deployments []LakeMethodologyDeployment
	for rows.Next() {
		var deployment LakeMethodologyDeployment
		var build LakeMethodologyRunBuild
		var status LakeMethodologyDeploymentStatus
		var hasBuild, hasStatus bool
		if err := rows.Scan(&deployment.ID, &deployment.CustomerID, &deployment.WorkspaceID, &deployment.WorkspaceNamespace, &deployment.MethodologyID,
			&deployment.ModelID, &deployment.BuildID, &deployment.Image, &deployment.RuntimeImage, &deployment.CPUMillis,
			&deployment.MemoryBytes, &deployment.GPUs, &deployment.GPUClass, &deployment.Network, &deployment.Slug,
			&deployment.Desired, &deployment.Generation,
			&hasBuild, &build.Desired, &build.Phase, &build.Digest, &build.Target, &build.Message,
			&hasStatus, &status.Phase, &status.Message, &status.CredentialSHA256, &status.Endpoint,
			&status.ObservedGeneration, &status.ReadyAt, &status.StoppedAt); err != nil {
			return nil, err
		}
		if hasBuild {
			deployment.Build = &build
		}
		if hasStatus {
			status.DeploymentID = deployment.ID
			status.ReadyAt, status.StoppedAt = utcSecond(status.ReadyAt), utcSecond(status.StoppedAt)
			deployment.Status = &status
		}
		deployments = append(deployments, deployment)
	}
	if err := rows.Err(); err != nil {
		return nil, methodologyDeploymentsTableError(err)
	}
	return deployments, nil
}

func (s *pgxServiceStore) UpsertMethodologyDeploymentStatus(ctx context.Context, status LakeMethodologyDeploymentStatus) error {
	_, err := s.db.Exec(ctx, upsertMethodologyDeploymentStatusSQL,
		status.DeploymentID, status.Phase, status.Message, status.CredentialSHA256, status.Endpoint,
		status.ObservedGeneration, status.ReadyAt, status.StoppedAt)
	return err
}

// listMethodologyDeploymentObjects returns nothing, without an error, before
// 0083_methodology_deployments: the workload pass then simply finds no served models.
func (s *pgxServiceStore) listMethodologyDeploymentObjects(ctx context.Context, ids []string, live bool) ([]WorkloadObject, error) {
	if ids == nil {
		ids = []string{}
	}
	rows, err := s.db.Query(ctx, listMethodologyDeploymentObjectsSQL, ids, live)
	if err != nil {
		if errors.Is(methodologyDeploymentsTableError(err), errMethodologyDeploymentsTableMissing) {
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
		if errors.Is(methodologyDeploymentsTableError(err), errMethodologyDeploymentsTableMissing) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}
