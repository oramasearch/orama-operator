// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
)

// checkWorkloadResourcesPgx runs the workload resources SQL against the vendored schema, inside
// TestServicesPgxStoreAgainstPostgres's rolled-back transaction and on its seeded rows: a
// workspace whose Zetta is testDefaultID, a present postgres, an absent and Deleted zetta, the
// catalog kinds, a job agent launch and a queued build.
func checkWorkloadResourcesPgx(t *testing.T, ctx context.Context, tx pgx.Tx, store *pgxServiceStore, customerID string) {
	t.Helper()
	const (
		appID        = "e0000000-0000-4000-8000-000000000001"
		serviceAgent = "e0000000-0000-4000-8000-000000000002"
		jobAgent     = "a1000000-0000-4000-8000-000000000001"
		unknownID    = "e0000000-0000-4000-8000-0000000000ff"
	)
	if _, err := tx.Exec(ctx, `INSERT INTO apps (id, customer_id, workspace_id, name, mode, image, port, host, desired)
		VALUES ($1, $2, $3, 'web', 'image', 'oramasearch/web:1', 8080, 'web.example.test', 'stopped')`,
		appID, customerID, testWorkspaceID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_launches
		(id, customer_id, workspace_id, agent_id, mode, scopes, slug)
		VALUES ($1, $2, $3, 'agent', 'service', ARRAY['read'], 'al-e00000000002')`, serviceAgent, customerID, testWorkspaceID); err != nil {
		t.Fatalf("seed service launch: %v", err)
	}

	rows, err := store.ListWorkloadResources(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty workload_resources = %+v, %v", rows, err)
	}
	live, err := store.ListWorkloadObjects(ctx, nil, true)
	if err != nil {
		t.Fatalf("live objects: %v", err)
	}
	byKey := map[workloadKey]WorkloadObject{}
	for _, object := range live {
		byKey[workloadKey{object.Kind, object.ID}] = object
	}
	if got := byKey[workloadKey{WorkloadKindWorkspace, testWorkspaceID}]; got.Namespace != testWorkspaceName || got.Desired != "running" {
		t.Errorf("workspace = %+v", got)
	}
	if _, found := byKey[workloadKey{WorkloadKindService, testDefaultID}]; found {
		t.Error("the workspace's own Zetta was listed as a service")
	}
	if _, found := byKey[workloadKey{WorkloadKindService, testZettaServiceID}]; found {
		t.Error("an absent, Deleted service was listed")
	}
	if got := byKey[workloadKey{WorkloadKindService, testPostgresID}]; got.Subkind != serviceKindPostgres || got.Slug != testPostgresSlug || got.Namespace != testWorkspaceName {
		t.Errorf("postgres = %+v", got)
	}
	if got := byKey[workloadKey{WorkloadKindApp, appID}]; got.Desired != "stopped" || got.Subkind != "image" {
		t.Errorf("app = %+v", got)
	}
	if got := byKey[workloadKey{WorkloadKindAgentLaunch, serviceAgent}]; got.Subkind != agentLaunchModeService || got.Slug != "al-e00000000002" {
		t.Errorf("service launch = %+v", got)
	}
	for _, key := range []workloadKey{{WorkloadKindAgentLaunch, jobAgent}, {WorkloadKindBuild, testBuildID}} {
		if _, found := byKey[key]; found {
			t.Errorf("%s %s was listed without being asked for", key.kind, key.id)
		}
	}

	// By id: history and builds come back; an unknown id does not.
	byID, err := store.ListWorkloadObjects(ctx, []string{jobAgent, testBuildID, testZettaServiceID, testDefaultID, unknownID}, false)
	if err != nil {
		t.Fatalf("objects by id: %v", err)
	}
	var got []string
	for _, object := range byID {
		got = append(got, object.Kind+"/"+object.ID)
	}
	sort.Strings(got)
	want := []string{"agent_launch/" + jobAgent, "build/" + testBuildID, "service/" + testZettaServiceID}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("objects by id = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("objects by id = %v, want %v", got, want)
		}
	}

	row := WorkloadResources{Kind: WorkloadKindBuild, ID: testBuildID, Pods: 1, CPUMillis: 500, MemoryBytes: 1 << 30,
		GPUs: 1, GPUClass: "l4", VRAMBytes: 24 * gib}
	if err := store.UpsertWorkloadResources(ctx, row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	row.Pods = 0
	if err := store.UpsertWorkloadResources(ctx, row); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := store.UpsertWorkloadResources(ctx, WorkloadResources{Kind: WorkloadKindWorkspace, ID: testWorkspaceID}); err != nil {
		t.Fatalf("zero row: %v", err)
	}
	rows, err = store.ListWorkloadResources(ctx)
	if err != nil || len(rows) != 2 || rows[0] != row || rows[1] != (WorkloadResources{Kind: WorkloadKindWorkspace, ID: testWorkspaceID}) {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err := store.DeleteWorkloadResources(ctx, WorkloadKindBuild, testBuildID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows, err = store.ListWorkloadResources(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("rows after delete = %+v, %v", rows, err)
	}

	// Finished runs without a row are the backfill (lake#658): a build and a job launch that
	// finished, and a cron run; once a row is written, a run is no longer listed.
	const (
		scheduleID = "e0000000-0000-4000-8000-000000000003"
		cronRunID  = "e0000000-0000-4000-8000-000000000004"
	)
	if _, err := tx.Exec(ctx, `UPDATE builds SET phase = 'Succeeded' WHERE id = $1`, testBuildID); err != nil {
		t.Fatalf("finish build: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_launch_status (launch_id, phase) VALUES ($1, 'Failed')
		ON CONFLICT (launch_id) DO UPDATE SET phase = 'Failed'`, jobAgent); err != nil {
		t.Fatalf("finish job launch: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_cron_schedules (id, customer_id, workspace_id, agent_id, cron, scopes, slug)
		VALUES ($1, $2, $3, 'agent', '0 3 * * *', ARRAY['read'], 'as-e00000000003')`, scheduleID, customerID, testWorkspaceID); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_cron_runs (id, schedule_id, job_name, phase) VALUES ($1, $2, 'nightly-1', 'Succeeded')`,
		cronRunID, scheduleID); err != nil {
		t.Fatalf("seed cron run: %v", err)
	}
	unmetered, err := store.ListUnmeteredJobs(ctx)
	if err != nil {
		t.Fatalf("unmetered jobs: %v", err)
	}
	var kinds []string
	for _, object := range unmetered {
		kinds = append(kinds, object.Kind+"/"+object.ID)
		if object.Kind == WorkloadKindAgentCronRun && (object.Namespace != testWorkspaceName || object.Slug != "as-e00000000003") {
			t.Errorf("cron run = %+v", object)
		}
	}
	sort.Strings(kinds)
	wantKinds := []string{"agent_cron_run/" + cronRunID, "agent_launch/" + jobAgent, "build/" + testBuildID}
	if len(kinds) != len(wantKinds) || kinds[0] != wantKinds[0] || kinds[1] != wantKinds[1] || kinds[2] != wantKinds[2] {
		t.Fatalf("unmetered = %v, want %v", kinds, wantKinds)
	}
	if err := store.UpsertWorkloadResources(ctx, WorkloadResources{Kind: WorkloadKindAgentCronRun, ID: cronRunID, CPUMillis: 100, MemoryBytes: 256 << 20}); err != nil {
		t.Fatalf("cron run row (0077_metered_jobs): %v", err)
	}
	if unmetered, err = store.ListUnmeteredJobs(ctx); err != nil || len(unmetered) != 2 {
		t.Fatalf("unmetered after a row = %+v, %v", unmetered, err)
	}
	if byID, err := store.ListWorkloadObjects(ctx, []string{cronRunID}, false); err != nil || len(byID) != 1 || byID[0].Kind != WorkloadKindAgentCronRun {
		t.Fatalf("cron run by id = %+v, %v", byID, err)
	}

	// The table's checks hold the contract: a class the column refuses is refused.
	bad, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&pgxServiceStore{db: bad}).UpsertWorkloadResources(ctx, WorkloadResources{Kind: WorkloadKindApp, ID: appID, GPUClass: "NVIDIA-L4"}); err == nil {
		t.Error("the gpu_class check accepted NVIDIA-L4")
	}
	_ = bad.Rollback(ctx)

	// Before 0075 the table is missing, and the pass is skipped.
	missing, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Exec(ctx, `DROP TABLE workload_resources`); err != nil {
		t.Fatal(err)
	}
	if _, err := (&pgxServiceStore{db: missing}).ListWorkloadResources(ctx); !errors.Is(err, errWorkloadResourcesTableMissing) {
		t.Errorf("missing table err = %v", err)
	}
	_ = missing.Rollback(ctx)
}
