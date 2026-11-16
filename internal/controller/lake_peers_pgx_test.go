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

// TestPeersPgxStoreAgainstPostgres creates Lake migration 0084's peer contract itself, in a scratch
// schema inside a transaction that is rolled back.
//
//	LAKE_PEERS_TEST_POSTGRES=1 [LAKE_PEERS_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run PeersPgxStore
func TestPeersPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_PEERS_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_PEERS_TEST_POSTGRES=1 to run the peer store against local Postgres")
	}
	uri := os.Getenv("LAKE_PEERS_TEST_POSTGRES_URI")
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
	scratch := fmt.Sprintf("orama_operator_peers_test_%d", time.Now().UnixNano())
	exec(
		"CREATE SCHEMA "+scratch,
		"SET LOCAL search_path TO "+scratch+", pg_catalog",
		`CREATE TABLE workspaces (id uuid PRIMARY KEY, namespace text NOT NULL DEFAULT '')`,
		`CREATE TABLE network_peers (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            customer_id uuid NOT NULL,
            workspace_id uuid NOT NULL REFERENCES workspaces(id),
            name text NOT NULL,
            remote text NOT NULL,
            exports jsonb NOT NULL DEFAULT '[]',
            imports jsonb NOT NULL DEFAULT '[]',
            desired text NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
            generation bigint NOT NULL DEFAULT 1,
            created_by uuid,
            created_at timestamptz NOT NULL DEFAULT now(),
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE TABLE network_peer_status (
            peer_id uuid PRIMARY KEY REFERENCES network_peers(id) ON DELETE CASCADE,
            phase text NOT NULL DEFAULT '',
            message text NOT NULL DEFAULT '',
            observed_generation bigint NOT NULL DEFAULT 0,
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
	)
	store := &pgxServiceStore{db: tx}
	if _, err := store.ListPeers(ctx); !errors.Is(err, errNetworkGuardMissing) {
		t.Fatalf("before network_guard: %v", err)
	}
	exec(
		`CREATE TABLE network_guard (applied_at timestamptz NOT NULL DEFAULT now())`,
		fmt.Sprintf(`INSERT INTO workspaces (id, namespace) VALUES ('%s', 'workspace')`, testWorkspaceID),
		fmt.Sprintf(`INSERT INTO network_peers (id, customer_id, workspace_id, name, remote, exports, imports)
            VALUES ('%s', '%s', '%s', 'to-us', '40.0.0.3:7446', '[{"target_kind":"service","target_id":"%s","port":5432}]', '[{"name":"cache.us","port":6379}]')`,
			testPeerID, testServiceCustomer, testWorkspaceID, testConnectionTargetID),
	)
	peers, err := store.ListPeers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Remote != "40.0.0.3:7446" || peers[0].Desired != "present" || peers[0].Status != nil {
		t.Fatalf("peers = %+v", peers)
	}
	var exports []lakePeerExportRow
	if err := decodeStrict(peers[0].Exports, &exports); err != nil || exports[0].Port != 5432 || exports[0].TargetID != testConnectionTargetID {
		t.Fatalf("exports %s: %v", peers[0].Exports, err)
	}
	if err := store.UpsertPeerStatus(ctx, LakePeerStatus{PeerID: testPeerID, Phase: "Ready", Message: "ok", ObservedGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	exec(fmt.Sprintf(`UPDATE network_peers SET desired = 'absent', generation = 2 WHERE id = '%s'`, testPeerID))
	if peers, err = store.ListPeers(ctx); err != nil || len(peers) != 1 || peers[0].Status.Phase != "Ready" {
		t.Fatalf("absent before Deleted: %+v (%v)", peers, err)
	}
	if err := store.UpsertPeerStatus(ctx, LakePeerStatus{PeerID: testPeerID, Phase: "Deleted", ObservedGeneration: 2}); err != nil {
		t.Fatal(err)
	}
	if peers, err = store.ListPeers(ctx); err != nil || len(peers) != 0 {
		t.Fatalf("a recorded deletion is still read: %+v (%v)", peers, err)
	}
}
