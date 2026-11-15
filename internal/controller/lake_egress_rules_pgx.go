// SPDX-License-Identifier: MIT

package controller

import (
	"context"
)

const listNetworkEgressRulesSQL = `SELECT r.id::text, r.customer_id::text, r.workspace_id::text,
       w.namespace, COALESCE(r.source_kind, ''), COALESCE(r.source_id::text, ''),
       COALESCE(r.cidr::text, ''), COALESCE(r.dns_name, ''), r.ports, r.desired, r.generation,
       st.rule_id IS NOT NULL, COALESCE(st.phase, ''), COALESCE(st.message, ''),
       COALESCE(st.observed_generation, 0)
  FROM network_egress_rules r
  JOIN workspaces w ON w.id = r.workspace_id AND w.customer_id = r.customer_id
  LEFT JOIN network_egress_rule_status st ON st.rule_id = r.id
 WHERE NOT (r.desired = 'absent' AND COALESCE(st.phase, '') = 'Deleted')
 ORDER BY r.created_at, r.id`

const upsertNetworkEgressRuleStatusSQL = `INSERT INTO network_egress_rule_status
    (rule_id, phase, message, observed_generation, updated_at)
VALUES ($1::uuid, $2, $3, $4, now())
ON CONFLICT (rule_id) DO UPDATE SET
    phase = EXCLUDED.phase,
    message = EXCLUDED.message,
    observed_generation = EXCLUDED.observed_generation,
    updated_at = now()`

// ListNetworkEgressRules reads the egress desired rows after checking network_guard, with the
// same workspace list the connection intake uses.
func (s *pgxServiceStore) ListNetworkEgressRules(ctx context.Context) (LakeNetworkEgressSnapshot, error) {
	if err := s.requireNetworkGuard(ctx); err != nil {
		return LakeNetworkEgressSnapshot{}, err
	}
	workspaces, err := s.listNetworkWorkspaces(ctx)
	if err != nil {
		return LakeNetworkEgressSnapshot{}, err
	}
	snapshot := LakeNetworkEgressSnapshot{Workspaces: workspaces}
	rows, err := s.db.Query(ctx, listNetworkEgressRulesSQL)
	if err != nil {
		return LakeNetworkEgressSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var row LakeNetworkEgressRule
		var hasStatus bool
		var status LakeNetworkEgressRuleStatus
		if err := rows.Scan(
			&row.ID,
			&row.CustomerID,
			&row.WorkspaceID,
			&row.WorkspaceNamespace,
			&row.SourceKind,
			&row.SourceID,
			&row.CIDR,
			&row.DNSName,
			&row.Ports,
			&row.Desired,
			&row.Generation,
			&hasStatus,
			&status.Phase,
			&status.Message,
			&status.ObservedGeneration,
		); err != nil {
			return LakeNetworkEgressSnapshot{}, err
		}
		if hasStatus {
			status.RuleID = row.ID
			row.Status = &status
		}
		snapshot.Rules = append(snapshot.Rules, row)
	}
	return snapshot, rows.Err()
}

func (s *pgxServiceStore) UpsertNetworkEgressRuleStatus(ctx context.Context, status LakeNetworkEgressRuleStatus) error {
	_, err := s.db.Exec(
		ctx,
		upsertNetworkEgressRuleStatusSQL,
		status.RuleID,
		status.Phase,
		status.Message,
		status.ObservedGeneration,
	)
	return err
}
