/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestServicesPgxStoreAgainstPostgres runs the pgx store's SQL against the vendored lake schema on
// a real Postgres. Everything happens inside one transaction, in a scratch schema, that is rolled
// back: the database is left exactly as it was.
//
//	LAKE_SERVICES_TEST_POSTGRES=1 [LAKE_SERVICES_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run PgxStore
func TestServicesPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_SERVICES_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_SERVICES_TEST_POSTGRES=1 to run the pgx store against a local Postgres")
	}
	uri := os.Getenv("LAKE_SERVICES_TEST_POSTGRES_URI")
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

	scratch := fmt.Sprintf("orama_operator_services_test_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE SCHEMA " + scratch,
		"SET LOCAL search_path TO " + scratch + ", pg_catalog",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := tx.Exec(ctx, lakeSchemaSQL); err != nil {
		t.Fatalf("apply the vendored lake schema: %v", err)
	}
	workspaceFixture, err := os.ReadFile("testdata/lake-workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(workspaceFixture)); err != nil {
		t.Fatalf("apply workspace fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE services ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id)`); err != nil {
		t.Fatalf("ensure services workspace contract: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE services ALTER COLUMN workspace_id SET NOT NULL`); err != nil {
		t.Fatalf("require services workspace identity: %v", err)
	}
	for _, table := range []string{"agent_launches", "agent_cron_schedules"} {
		if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id)`); err != nil {
			t.Fatalf("ensure %s workspace contract: %v", table, err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` ALTER COLUMN workspace_id SET NOT NULL`); err != nil {
			t.Fatalf("require %s workspace identity: %v", table, err)
		}
	}
	var customerID string
	if err := tx.QueryRow(ctx, `WITH u AS (INSERT INTO users (display_name) VALUES ('t') RETURNING id)
		INSERT INTO customers (customer_type, user_id, name) SELECT 'individual', id, 'services-test' FROM u RETURNING id::text`).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces
		(id, customer_id, name, environment, desired, generation, namespace)
		VALUES ($1, $2, 'Production', 'prod', 'running', 1, $3)`, testWorkspaceID, customerID, testWorkspaceName); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_launches
		(id, customer_id, workspace_id, agent_id, mode, scopes, slug)
		VALUES ('a1000000-0000-4000-8000-000000000001', $1, $2, 'agent', 'job', ARRAY['read'], 'al-a10000000000')`, customerID, testWorkspaceID); err != nil {
		t.Fatalf("seed agent launch: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_cron_schedules
		(id, customer_id, workspace_id, agent_id, cron, scopes, slug)
		VALUES ('a2000000-0000-4000-8000-000000000002', $1, $2, 'agent', '0 9 * * *', ARRAY['read'], 'as-a20000000000')`, customerID, testWorkspaceID); err != nil {
		t.Fatalf("seed agent schedule: %v", err)
	}
	insert := `INSERT INTO services (id, customer_id, workspace_id, kind, name, slug, is_default, spec, desired, generation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`
	for _, row := range [][]any{
		{testDefaultID, customerID, testWorkspaceID, "zetta", "workspace", testDefaultSlug, true, `{"instances":1,"storage_gi":10}`, "present", 1},
		{testPostgresID, customerID, testWorkspaceID, "postgres", "db", testPostgresSlug, false, `{}`, "present", 3},
		{testZettaServiceID, customerID, testWorkspaceID, "zetta", "gone", testZettaSlug, false, `{}`, "absent", 2},
	} {
		if _, err := tx.Exec(ctx, insert, row...); err != nil {
			t.Fatalf("seed service: %v", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET zetta_service_id=$1 WHERE id=$2`, testDefaultID, testWorkspaceID); err != nil {
		t.Fatalf("link workspace Zetta service: %v", err)
	}

	store := &pgxServiceStore{db: tx}
	launches, err := store.ListAgentLaunches(ctx)
	if err != nil || len(launches) != 1 || launches[0].WorkspaceID != testWorkspaceID || launches[0].WorkspaceNamespace != testWorkspaceName {
		t.Fatalf("workspace agent launches = %+v, %v", launches, err)
	}
	schedules, err := store.ListAgentSchedules(ctx)
	if err != nil || len(schedules) != 1 || schedules[0].WorkspaceID != testWorkspaceID || schedules[0].WorkspaceNamespace != testWorkspaceName {
		t.Fatalf("workspace agent schedules = %+v, %v", schedules, err)
	}
	services, err := store.ListServices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(services) != 3 || services[0].Status != nil {
		t.Fatalf("services = %+v", services)
	}
	byID := map[string]LakeService{}
	for _, svc := range services {
		byID[svc.ID] = svc
	}
	if got := byID[testDefaultID]; got.Kind != "zetta" || !got.IsDefault || !got.WorkspaceZetta || got.CustomerID != customerID || got.Slug != testDefaultSlug {
		t.Fatalf("default service = %+v", got)
	}
	if byID[testPostgresID].WorkspaceZetta || byID[testZettaServiceID].WorkspaceZetta {
		t.Fatalf("non-workspace services were marked as workspace Zetta: %+v", byID)
	}
	if spec, err := parseServiceSpec(byID[testDefaultID].Spec); err != nil || spec.Instances != 1 || spec.StorageGi != 10 {
		t.Fatalf("spec %q parsed to %+v, %v", byID[testDefaultID].Spec, spec, err)
	}
	if byID[testPostgresID].Generation != 3 {
		t.Fatalf("generation = %d", byID[testPostgresID].Generation)
	}

	status := LakeServiceStatus{ServiceID: testPostgresID, Phase: ServicePhaseProvisioning, Message: "waiting", Endpoint: "h:5432", ObservedGeneration: 3}
	if err := store.UpsertServiceStatus(ctx, status); err != nil {
		t.Fatalf("insert status: %v", err)
	}
	status.Phase, status.Message = ServicePhaseReady, "service ready"
	if err := store.UpsertServiceStatus(ctx, status); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := store.UpsertServiceStatus(ctx, LakeServiceStatus{ServiceID: testZettaServiceID, Phase: ServicePhaseDeleted, Message: "service deleted", ObservedGeneration: 2}); err != nil {
		t.Fatalf("deleted status: %v", err)
	}
	services, err = store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("history (absent and Deleted) was listed: %+v", services)
	}
	for _, svc := range services {
		if svc.ID == testPostgresID && (svc.Status == nil || *svc.Status != status) {
			t.Fatalf("status = %+v, want %+v", svc.Status, status)
		}
	}

	// Migrations 0025_service_catalog, 0026_sqlite_services and 0027_analytics_services: every catalog kind is a valid row
	// and reads back as that kind; anything else is still refused by the kind check.
	for i, kind := range []string{serviceKindKafka, serviceKindClickHouse, serviceKindTiDB, serviceKindCassandra, serviceKindValkey, serviceKindSQLite, serviceKindDuckDB, serviceKindPolars} {
		id := fmt.Sprintf("c0000000-0000-4000-8000-00000000000%d", i)
		if _, err := tx.Exec(ctx, insert, id, customerID, testWorkspaceID, kind, kind, fmt.Sprintf("svc-c0000000000%d", i), false, `{}`, "present", 1); err != nil {
			t.Fatalf("seed %s service: %v", kind, err)
		}
	}
	services, err = store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, svc := range services {
		kinds[svc.Kind] = true
		if !knownServiceKind(svc.Kind) {
			t.Fatalf("lake accepted kind %q, which the operator does not run", svc.Kind)
		}
	}
	if len(kinds) != len(serviceKindNames) {
		t.Fatalf("kinds read back = %v, want %v", kinds, serviceKindNames)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO builds
		(id, customer_id, workspace_id, repository, revision, context, dockerfile, target, build_args_json, desired)
		VALUES ($1, $2, $3, 'repo', 'abc123', '.', 'Dockerfile', $4, '{"PYTHON_VERSION":"3.13"}', 'queued')`,
		testBuildID, customerID, testWorkspaceID, testBuildTarget); err != nil {
		t.Fatalf("seed build: %v", err)
	}
	builds, err := store.ListBuilds(ctx)
	if err != nil || len(builds) != 1 || builds[0].ID != testBuildID || builds[0].BuildArgs != `{"PYTHON_VERSION": "3.13"}` {
		t.Fatalf("builds = %+v, %v", builds, err)
	}
	buildStatus := LakeBuildStatus{
		BuildID: testBuildID, Phase: BuildPhaseSucceeded, Digest: "sha256:" + strings.Repeat("c", 64),
		Size: 42, Message: "image pushed", TokenHashes: `{}`,
	}
	if err := store.UpdateBuildStatus(ctx, buildStatus); err != nil {
		t.Fatalf("update build: %v", err)
	}
	var phase, digest string
	var size int64
	if err := tx.QueryRow(ctx, `SELECT phase, digest, size FROM builds WHERE id = $1`, testBuildID).Scan(&phase, &digest, &size); err != nil {
		t.Fatal(err)
	}
	if phase != BuildPhaseSucceeded || digest != buildStatus.Digest || size != buildStatus.Size {
		t.Fatalf("stored build status = %s %s %d", phase, digest, size)
	}
	checkWorkloadResourcesPgx(t, ctx, tx, store, customerID)
	checkMethodologyRunsPgx(t, ctx, tx, store, customerID)
	checkMethodologyDeploymentsPgx(t, ctx, tx, store, customerID)
	refused, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refused.Exec(ctx, insert, "d0000000-0000-4000-8000-000000000000", customerID, "mysql", "my", "svc-d00000000000", false, `{}`, "present", 1); err == nil {
		t.Fatal("the kind check accepted mysql")
	}
	_ = refused.Rollback(ctx)
}
