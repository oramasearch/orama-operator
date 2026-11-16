// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestTunnelGrantsPgxStoreAgainstPostgres creates Lake migrations 0084 and 0085's tunnel contract
// itself, in a scratch schema inside a transaction that is rolled back. The vendored Lake schema
// stays unchanged until Lake merges those migrations.
//
//	LAKE_TUNNELS_TEST_POSTGRES=1 [LAKE_TUNNELS_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run TunnelGrantsPgxStore
func TestTunnelGrantsPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_TUNNELS_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_TUNNELS_TEST_POSTGRES=1 to run the tunnel grant store against local Postgres")
	}
	uri := os.Getenv("LAKE_TUNNELS_TEST_POSTGRES_URI")
	if uri == "" {
		uri = "postgres://127.0.0.1:5432/postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	exec := func(statements ...string) {
		t.Helper()
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	scratch := fmt.Sprintf("orama_operator_tunnels_test_%d", time.Now().UnixNano())
	exec(
		"CREATE SCHEMA "+scratch,
		"SET LOCAL search_path TO "+scratch+", pg_catalog",
		`CREATE TABLE workspaces (id uuid PRIMARY KEY, customer_id uuid NOT NULL, namespace text NOT NULL DEFAULT '')`,
		`CREATE TABLE users (id uuid PRIMARY KEY, email text)`,
		`CREATE TABLE network_tunnel_grants (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            customer_id uuid NOT NULL,
            workspace_id uuid NOT NULL REFERENCES workspaces(id),
            user_id uuid NOT NULL,
            target_kind text NOT NULL CHECK (target_kind IN ('service', 'app')),
            target_id uuid NOT NULL,
            port integer NOT NULL,
            expires_at timestamptz NOT NULL,
            revoked_at timestamptz,
            generation bigint NOT NULL DEFAULT 1,
            created_by uuid,
            created_at timestamptz NOT NULL DEFAULT now(),
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE TABLE network_tunnel_grant_status (
            grant_id uuid PRIMARY KEY REFERENCES network_tunnel_grants(id) ON DELETE CASCADE,
            phase text NOT NULL DEFAULT '',
            message text NOT NULL DEFAULT '',
            endpoint text NOT NULL DEFAULT '',
            observed_generation bigint NOT NULL DEFAULT 0,
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
	)
	store := &pgxServiceStore{db: tx}
	if _, err := store.ListTunnelGrants(ctx); !errors.Is(err, errNetworkGuardMissing) {
		t.Fatalf("before network_guard: %v", err)
	}
	exec(`CREATE TABLE network_guard (applied_at timestamptz NOT NULL DEFAULT now())`)
	if _, err := store.ListTunnelGrants(ctx); !errors.Is(err, errTunnelCAMissing) {
		t.Fatalf("before 0085: %v", err)
	}
	exec(
		`ALTER TABLE network_tunnel_grants ADD COLUMN IF NOT EXISTS client_ca_pem text NOT NULL DEFAULT ''`,
		`ALTER TABLE network_tunnel_grant_status ADD COLUMN IF NOT EXISTS relay_ca_pem text NOT NULL DEFAULT ''`,
		fmt.Sprintf(`INSERT INTO workspaces (id, customer_id, namespace) VALUES ('%s', '%s', 'workspace')`, testWorkspaceID, testServiceCustomer),
		fmt.Sprintf(`INSERT INTO users (id, email) VALUES ('%s', 'dev@example.com')`, testTunnelUserID),
		fmt.Sprintf(`INSERT INTO network_tunnel_grants
            (id, customer_id, workspace_id, user_id, target_kind, target_id, port, expires_at, client_ca_pem, created_at)
            VALUES ('%s', '%s', '%s', '%s', 'service', '%s', 5432, '2026-10-14T14:00:00Z', 'CA', now())`,
			testTunnelGrantID, testServiceCustomer, testWorkspaceID, testTunnelUserID, testConnectionTargetID),
		fmt.Sprintf(`INSERT INTO network_tunnel_grants
            (id, customer_id, workspace_id, user_id, target_kind, target_id, port, expires_at, revoked_at, generation, created_at)
            VALUES ('%s', '%s', '%s', '%s', 'app', '%s', 8080, '2026-10-14T14:00:00Z', '2026-10-14T12:30:00Z', 2, now() + interval '1 second')`,
			testTunnelOther, testServiceCustomer, testWorkspaceID, testConnectionSourceID, testConnectionTargetID),
	)
	grants, err := store.ListTunnelGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v", grants)
	}
	first, second := grants[0], grants[1]
	if first.ID != testTunnelGrantID || first.UserEmail != "dev@example.com" || first.WorkspaceNamespace != "workspace" ||
		first.Port != 5432 || first.ClientCAPEM != "CA" || first.RevokedAt != nil || first.Status != nil ||
		!first.ExpiresAt.Equal(time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("first grant = %+v", first)
	}
	if second.UserEmail != "" || second.RevokedAt == nil || second.Generation != 2 {
		t.Fatalf("second grant = %+v", second)
	}
	ready := LakeTunnelGrantStatus{GrantID: testTunnelGrantID, Phase: "Ready", Message: "ok", Endpoint: "20.1.2.3:443", RelayCAPEM: "RELAY", ObservedGeneration: 1}
	if err := store.UpsertTunnelGrantStatus(ctx, ready); err != nil {
		t.Fatal(err)
	}
	revoked := LakeTunnelGrantStatus{GrantID: testTunnelOther, Phase: "Revoked", Message: "revoked", ObservedGeneration: 2}
	if err := store.UpsertTunnelGrantStatus(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	grants, err = store.ListTunnelGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Status == nil || *grants[0].Status != ready {
		t.Fatalf("after status writes: %+v", grants)
	}
}
