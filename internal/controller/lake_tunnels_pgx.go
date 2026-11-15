// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"time"
)

// The guard and the 0085 column are checked together, before any network row is read.
const tunnelGuardSQL = `SELECT to_regclass('network_guard') IS NOT NULL,
       EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema()
                  AND table_name = 'network_tunnel_grants'
                  AND column_name = 'client_ca_pem'),
       EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema()
                  AND table_name = 'network_tunnel_grant_status'
                  AND column_name = 'relay_ca_pem')`

// listTunnelGrantsSQL reads every grant whose end is not yet recorded. The user's e-mail address is
// joined for a Cloudflare provider's Access policy; nothing else about the user is read.
const listTunnelGrantsSQL = `SELECT g.id::text, g.customer_id::text, g.workspace_id::text, w.namespace,
       g.user_id::text, COALESCE(u.email, ''), g.target_kind, g.target_id::text, g.port,
       g.expires_at, g.revoked_at, g.client_ca_pem, g.generation,
       st.grant_id IS NOT NULL, COALESCE(st.phase, ''), COALESCE(st.message, ''),
       COALESCE(st.endpoint, ''), COALESCE(st.relay_ca_pem, ''), COALESCE(st.observed_generation, 0)
  FROM network_tunnel_grants g
  JOIN workspaces w ON w.id = g.workspace_id
  LEFT JOIN users u ON u.id = g.user_id
  LEFT JOIN network_tunnel_grant_status st ON st.grant_id = g.id
 WHERE NOT (COALESCE(st.phase, '') IN ('Expired', 'Revoked') AND st.observed_generation = g.generation)
 ORDER BY g.created_at, g.id`

const upsertTunnelGrantStatusSQL = `INSERT INTO network_tunnel_grant_status
    (grant_id, phase, message, endpoint, relay_ca_pem, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, now())
ON CONFLICT (grant_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    endpoint = EXCLUDED.endpoint,
    relay_ca_pem = EXCLUDED.relay_ca_pem,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

func (s *pgxServiceStore) ListTunnelGrants(ctx context.Context) ([]LakeTunnelGrant, error) {
	var guard, clientCA, relayCA bool
	guardRows, err := s.db.Query(ctx, tunnelGuardSQL)
	if err != nil {
		return nil, err
	}
	if !guardRows.Next() {
		err := guardRows.Err()
		guardRows.Close()
		if err == nil {
			err = errors.New("tunnel guard query returned no row")
		}
		return nil, err
	}
	err = guardRows.Scan(&guard, &clientCA, &relayCA)
	guardRows.Close()
	if err != nil {
		return nil, err
	}
	if !guard {
		return nil, errNetworkGuardMissing
	}
	if !clientCA || !relayCA {
		return nil, errTunnelCAMissing
	}
	rows, err := s.db.Query(ctx, listTunnelGrantsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []LakeTunnelGrant
	for rows.Next() {
		var grant LakeTunnelGrant
		var hasStatus bool
		var status LakeTunnelGrantStatus
		var revokedAt *time.Time
		if err := rows.Scan(
			&grant.ID, &grant.CustomerID, &grant.WorkspaceID, &grant.WorkspaceNamespace,
			&grant.UserID, &grant.UserEmail, &grant.TargetKind, &grant.TargetID, &grant.Port,
			&grant.ExpiresAt, &revokedAt, &grant.ClientCAPEM, &grant.Generation,
			&hasStatus, &status.Phase, &status.Message, &status.Endpoint, &status.RelayCAPEM, &status.ObservedGeneration,
		); err != nil {
			return nil, err
		}
		grant.RevokedAt = revokedAt
		if hasStatus {
			status.GrantID = grant.ID
			grant.Status = &status
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grants, nil
}

func (s *pgxServiceStore) UpsertTunnelGrantStatus(ctx context.Context, status LakeTunnelGrantStatus) error {
	if status.GrantID == "" {
		return errors.New("tunnel grant status has no grant id")
	}
	_, err := s.db.Exec(ctx, upsertTunnelGrantStatusSQL,
		status.GrantID, status.Phase, status.Message, status.Endpoint, status.RelayCAPEM, status.ObservedGeneration)
	return err
}
