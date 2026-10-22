package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Use an isolated local test server: roles are server-wide, and this proof refuses existing roles.
func TestLakeControlDBPrivilegesAgainstPostgres(t *testing.T) {
	uri := os.Getenv("LAKE_ROLES_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("set LAKE_ROLES_TEST_POSTGRES_URI to a disposable PostgreSQL server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminConfig, err := pgx.ParseConfig(uri)
	if err != nil {
		t.Fatal("parse test database connection")
	}
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("connect test database: %v", errors.Unwrap(err))
	}
	defer func() { _ = admin.Close(context.Background()) }()
	exec := func(conn *pgx.Conn, sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("database proof: %v", err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	database, owner := "lake_roles_"+suffix, "lake_owner_"+suffix
	password, err := randHex(24)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{owner, lakeRuntimeRole, lakeOperatorRole} {
		exec(admin, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD '"+password+"'")
		t.Cleanup(func() {
			cleanup, err := pgx.ConnectConfig(context.Background(), adminConfig)
			if err != nil {
				t.Error("cleanup connection failed")
				return
			}
			defer func() { _ = cleanup.Close(context.Background()) }()
			if _, err := cleanup.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		})
	}
	exec(admin, "CREATE DATABASE "+database+" OWNER "+owner)
	t.Cleanup(func() {
		cleanup, err := pgx.ConnectConfig(context.Background(), adminConfig)
		if err != nil {
			t.Error("cleanup connection failed")
			return
		}
		defer func() { _ = cleanup.Close(context.Background()) }()
		if _, err := cleanup.Exec(context.Background(), "DROP DATABASE "+database+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	connect := func(role string) *pgx.Conn {
		t.Helper()
		config := adminConfig.Copy()
		config.Database, config.User, config.Password = database, role, password
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatalf("connect proof role: %v", errors.Unwrap(err))
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "roles-proof"}}
	env := envValue(t, lakeEnv(lake, lakeSealingMaterial{}), "LAKE_CONTROL_DB_URI")
	roleBySecret := map[string]string{lakeControlDB(lake.Name) + "-app": owner, lakeRoleSecret(lake.Name, lakeRuntimeRole): lakeRuntimeRole}
	selectedRole, ok := roleBySecret[env.ValueFrom.SecretKeyRef.Name]
	if !ok {
		t.Fatal("unknown Lake database credential")
	}
	migration, runtime, operator := connect(owner), connect(selectedRole), connect(lakeOperatorRole)
	exec(migration, lakeSchemaSQL)
	apply := func(sql string) {
		exec(migration, "BEGIN")
		exec(migration, sql)
		exec(migration, "COMMIT")
	}
	apply(lakeControlDBGrantsSQL)
	// Policy reapplies must revoke stale column grants as well as table grants.
	exec(migration, "GRANT UPDATE (phase) ON public.apps TO lake_runtime")
	apply(lakeControlDBGrantsSQL)
	denied := func(conn *pgx.Conn, sql string) {
		t.Helper()
		_, err := conn.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected permission denial for %s, got %v", sql, err)
		}
	}
	for _, conn := range []*pgx.Conn{runtime, operator} {
		denied(conn, "ALTER TABLE public.workspaces DISABLE TRIGGER workspace_storage_growth")
		denied(conn, "ALTER TABLE public.workspaces ADD COLUMN compromised text")
		denied(conn, "CREATE TABLE public.compromised (id int)")
		denied(conn, "CREATE TEMP TABLE compromised (id int)")
		denied(conn, "SET ROLE "+owner)
		denied(conn, "SET session_replication_role = replica")
		denied(conn, "TRUNCATE public.workspaces CASCADE")
		denied(conn, "DELETE FROM public.schema_migrations")
	}
	for _, table := range []string{"service_status", "store_status", "agent_launch_status", "agent_cron_schedule_status", "agent_cron_runs", "volume_snapshot_status", "network_connection_status", "network_egress_rule_status", "network_tunnel_grant_status", "network_peer_status"} {
		denied(runtime, "DELETE FROM public."+table)
		denied(runtime, "UPDATE public."+table+" SET updated_at = updated_at")
	}
	for table, column := range map[string]string{"apps": "phase", "builds": "token_hashes_json", "workspaces": "namespace"} {
		denied(runtime, "UPDATE public."+table+" SET "+column+" = "+column)
		denied(runtime, "INSERT INTO public."+table+" ("+column+") VALUES (DEFAULT)")
	}
	for _, table := range []string{"users", "sessions", "identity_documents", "credentials", "custodial_identities", "keystore", "agent_sessions"} {
		denied(operator, "SELECT * FROM public."+table)
	}
	denied(operator, "UPDATE public.apps SET desired = 'deleted'")
	denied(operator, "UPDATE public.workspaces SET storage_gi = 1")
	denied(operator, "UPDATE public.cloud_orders SET offer = 'dedicated_cluster'")
	exec(migration, "CREATE TABLE public.future_identity (secret text)")
	denied(runtime, "SELECT * FROM public.future_identity")
	denied(operator, "SELECT * FROM public.future_identity")

	// The application can still bootstrap a workspace: its routing and snapshot triggers run.
	exec(runtime, `INSERT INTO users (id, taap_id, email) VALUES ('11111111-1111-4111-8111-111111111111', 'roles-proof', 'proof@example.invalid');
INSERT INTO customers (id, name, customer_type, user_id) VALUES ('22222222-2222-4222-8222-222222222222', 'roles-proof', 'individual', '11111111-1111-4111-8111-111111111111');
INSERT INTO workspaces (id, customer_id, name, environment) VALUES ('33333333-3333-4333-8333-333333333333', '22222222-2222-4222-8222-222222222222', 'proof', 'development');`)
	exec(operator, "SELECT * FROM public.lake_operator_tunnel_users")
	exec(operator, "UPDATE public.workspaces SET phase='Ready', namespace='workspace-proof', observed_generation=1")
	_, err = runtime.Exec(ctx, "UPDATE public.workspaces SET storage_gi=1")
	if err == nil || !strings.Contains(err.Error(), "never decreases") {
		t.Fatalf("runtime bypassed workspace growth trigger: %v", err)
	}
	// EXPLAIN asks PostgreSQL to validate every production query with the operator's grants,
	// including columns read by ON CONFLICT and sequence-backed cloud-order events.
	queries := []string{
		listAppsSQL, updateAppStatusSQL, updateAppTokenHashesSQL, listBuildsSQL, updateBuildStatusSQL,
		listWorkspacesSQL, updateWorkspaceStatusSQL, listServicesSQL, upsertServiceStatusSQL,
		listStoresSQL, upsertStoreStatusSQL, listStoreMountsSQL, listStoreUsageSQL, updateStoreUsedBytesSQL,
		listAgentLaunchesSQL, upsertAgentLaunchStatusSQL, listAgentSchedulesSQL, upsertAgentScheduleStatusSQL, upsertAgentScheduleRunSQL,
		listMethodologyRunsSQL, upsertMethodologyRunStatusSQL, listMethodologyRunObjectsSQL,
		listMethodologyDeploymentsSQL, upsertMethodologyDeploymentStatusSQL, listMethodologyDeploymentObjectsSQL,
		listNetworkWorkspacesSQL, listNetworkConnectionsSQL, upsertNetworkConnectionStatusSQL,
		listNetworkEgressRulesSQL, upsertNetworkEgressRuleStatusSQL, listTunnelGrantsSQL, upsertTunnelGrantStatusSQL,
		listPeersSQL, upsertPeerStatusSQL, listVolumeSnapshotPoliciesSQL, upsertVolumeSnapshotStatusSQL,
		listWorkloadResourcesSQL, listWorkloadObjectsSQL, listUnmeteredJobsSQL, upsertWorkloadResourcesSQL, deleteWorkloadResourcesSQL,
		addAppTransferSQL, pruneAppTransferSQL, listCloudOrdersSQL, claimCloudOrderSQL, finishCloudOrderSQL,
	}
	for i, sql := range queries {
		name := fmt.Sprintf("proof_%d", i)
		description, err := operator.Prepare(ctx, name, sql)
		if err != nil {
			t.Fatalf("prepare query %d: %v", i, err)
		}
		args := make([]string, len(description.ParamOIDs))
		for i := range args {
			args[i] = "NULL"
		}
		exec(operator, "DEALLOCATE "+name)
		exec(operator, "PREPARE "+name+" AS "+sql)
		params := ""
		if len(args) > 0 {
			params = "(" + strings.Join(args, ",") + ")"
		}
		if _, err := operator.Exec(ctx, "EXPLAIN EXECUTE "+name+params); err != nil {
			t.Fatalf("operator query %d: %v\n%s", i, err, sql)
		}
	}
	t.Logf("both runtime roles denied owner powers; %d operator queries checked", len(queries))
}
