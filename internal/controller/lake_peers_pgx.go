// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
)

const listPeersSQL = `SELECT p.id::text, p.customer_id::text, p.workspace_id::text, w.namespace, p.name, p.remote,
       p.exports::text, p.imports::text, p.desired, p.generation,
       st.peer_id IS NOT NULL, COALESCE(st.phase, ''), COALESCE(st.message, ''), COALESCE(st.observed_generation, 0)
  FROM network_peers p
  JOIN workspaces w ON w.id = p.workspace_id
  LEFT JOIN network_peer_status st ON st.peer_id = p.id
 WHERE NOT (p.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted' AND st.observed_generation = p.generation)
 ORDER BY p.created_at, p.id`

const upsertPeerStatusSQL = `INSERT INTO network_peer_status (peer_id, phase, message, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, now())
ON CONFLICT (peer_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

func (s *pgxServiceStore) ListPeers(ctx context.Context) ([]LakePeer, error) {
	guardRows, err := s.db.Query(ctx, networkGuardSQL)
	if err != nil {
		return nil, err
	}
	var installed bool
	if !guardRows.Next() {
		err := guardRows.Err()
		guardRows.Close()
		if err == nil {
			err = errors.New("network guard query returned no row")
		}
		return nil, err
	}
	err = guardRows.Scan(&installed)
	guardRows.Close()
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, errNetworkGuardMissing
	}
	rows, err := s.db.Query(ctx, listPeersSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []LakePeer
	for rows.Next() {
		var peer LakePeer
		var exports, imports string
		var hasStatus bool
		var status LakePeerStatus
		if err := rows.Scan(
			&peer.ID, &peer.CustomerID, &peer.WorkspaceID, &peer.WorkspaceNamespace, &peer.Name, &peer.Remote,
			&exports, &imports, &peer.Desired, &peer.Generation,
			&hasStatus, &status.Phase, &status.Message, &status.ObservedGeneration,
		); err != nil {
			return nil, err
		}
		peer.Exports, peer.Imports = []byte(exports), []byte(imports)
		if hasStatus {
			status.PeerID = peer.ID
			peer.Status = &status
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func (s *pgxServiceStore) UpsertPeerStatus(ctx context.Context, status LakePeerStatus) error {
	if status.PeerID == "" {
		return errors.New("peer status has no peer id")
	}
	_, err := s.db.Exec(ctx, upsertPeerStatusSQL, status.PeerID, status.Phase, status.Message, status.ObservedGeneration)
	return err
}
