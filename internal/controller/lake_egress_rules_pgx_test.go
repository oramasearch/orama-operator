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

// TestEgressRulesPgxStoreAgainstPostgres creates Lake migration 0084's egress contract itself, in a
// scratch schema inside a transaction that is rolled back. The vendored schema waits for Lake.
//
//	LAKE_CONNECTIONS_TEST_POSTGRES=1 [LAKE_CONNECTIONS_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run EgressRulesPgxStore
func TestEgressRulesPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_CONNECTIONS_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_CONNECTIONS_TEST_POSTGRES=1 to run the EgressRule store against local Postgres")
	}
	uri := os.Getenv("LAKE_CONNECTIONS_TEST_POSTGRES_URI")
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
	scratch := fmt.Sprintf("orama_operator_egress_test_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE SCHEMA " + scratch,
		"SET LOCAL search_path TO " + scratch + ", pg_catalog",
		`CREATE TABLE workspaces (
            id uuid PRIMARY KEY,
            customer_id uuid NOT NULL,
            namespace text NOT NULL DEFAULT '',
            created_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE TABLE network_egress_rules (
            id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
            customer_id  uuid        NOT NULL,
            workspace_id uuid        NOT NULL REFERENCES workspaces(id),
            source_kind  text        CHECK (source_kind IN ('app', 'service', 'agent')),
            source_id    uuid,
            cidr         cidr,
            dns_name     text,
            ports        integer[]   NOT NULL,
            desired      text        NOT NULL DEFAULT 'present' CHECK (desired IN ('present', 'absent')),
            generation   bigint      NOT NULL DEFAULT 1,
            created_by   uuid,
            created_at   timestamptz NOT NULL DEFAULT now(),
            updated_at   timestamptz NOT NULL DEFAULT now(),
            CHECK ((cidr IS NULL) <> (dns_name IS NULL)),
            CHECK ((source_kind IS NULL) = (source_id IS NULL))
        )`,
		`CREATE TABLE network_egress_rule_status (
            rule_id             uuid        PRIMARY KEY REFERENCES network_egress_rules(id) ON DELETE CASCADE,
            phase               text        NOT NULL DEFAULT '',
            message             text        NOT NULL DEFAULT '',
            observed_generation bigint      NOT NULL DEFAULT 0,
            updated_at          timestamptz NOT NULL DEFAULT now()
        )`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("apply contract statement: %v", err)
		}
	}
	store := &pgxServiceStore{db: tx}
	if _, err := store.ListNetworkEgressRules(ctx); !errors.Is(err, errNetworkGuardMissing) {
		t.Fatalf("pre-guard read error = %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE network_guard (applied_at timestamptz NOT NULL DEFAULT now())`,
		fmt.Sprintf(`INSERT INTO workspaces (id, customer_id, namespace) VALUES ('%s', '%s', '%s')`, testWorkspaceID, testServiceCustomer, testWorkspaceName),
		fmt.Sprintf(`INSERT INTO network_egress_rules (id, customer_id, workspace_id, source_kind, source_id, dns_name, ports, generation)
            VALUES ('%s', '%s', '%s', 'app', '%s', 'api.example.com', ARRAY[443], 2)`,
			testEgressNameRuleID, testServiceCustomer, testWorkspaceID, testEgressSourceID),
		fmt.Sprintf(`INSERT INTO network_egress_rules (id, customer_id, workspace_id, cidr, ports, created_at)
            VALUES ('%s', '%s', '%s', '203.0.113.0/24', ARRAY[5432, 5433], now() + interval '1 second')`,
			testEgressCIDRRuleID, testServiceCustomer, testWorkspaceID),
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.ListNetworkEgressRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workspaces) != 1 || len(snapshot.Rules) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	name, cidr := snapshot.Rules[0], snapshot.Rules[1]
	if name.DNSName != "api.example.com" || name.CIDR != "" || name.SourceKind != "app" || name.SourceID != testEgressSourceID ||
		name.Generation != 2 || name.WorkspaceNamespace != testWorkspaceName || name.Status != nil {
		t.Fatalf("name row = %+v", name)
	}
	if cidr.CIDR != "203.0.113.0/24" || cidr.DNSName != "" || cidr.SourceKind != "" || cidr.SourceID != "" || len(cidr.Ports) != 2 {
		t.Fatalf("cidr row = %+v", cidr)
	}
	if message := validateNetworkEgressRule(&cidr); message != "" {
		t.Fatalf("a valid Postgres cidr row was refused: %s", message)
	}
	status := LakeNetworkEgressRuleStatus{RuleID: testEgressNameRuleID, Phase: "Ready", Message: "ready", ObservedGeneration: 2}
	if err := store.UpsertNetworkEgressRuleStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ListNetworkEgressRules(ctx)
	if err != nil || snapshot.Rules[0].Status == nil || *snapshot.Rules[0].Status != status {
		t.Fatalf("stored status = %+v, %v", snapshot.Rules, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE network_egress_rules SET desired = 'absent', generation = 3 WHERE id = $1`, testEgressNameRuleID); err != nil {
		t.Fatal(err)
	}
	status = LakeNetworkEgressRuleStatus{RuleID: testEgressNameRuleID, Phase: "Deleted", ObservedGeneration: 3}
	if err := store.UpsertNetworkEgressRuleStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ListNetworkEgressRules(ctx)
	if err != nil || len(snapshot.Rules) != 1 || snapshot.Rules[0].ID != testEgressCIDRRuleID {
		t.Fatalf("Deleted history was listed: %+v, %v", snapshot.Rules, err)
	}
}
