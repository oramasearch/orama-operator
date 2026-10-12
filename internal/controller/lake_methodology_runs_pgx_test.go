/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// checkMethodologyRunsPgx runs the methodology runs SQL against the vendored schema, inside
// TestServicesPgxStoreAgainstPostgres's rolled-back transaction, after its build testBuildID has
// succeeded.
func checkMethodologyRunsPgx(t *testing.T, ctx context.Context, tx pgx.Tx, store *pgxServiceStore, customerID string) {
	t.Helper()
	const runID = "f0000000-0000-4000-8000-000000000001"
	if _, err := tx.Exec(ctx, `INSERT INTO methodology_runs (id, customer_id, workspace_id, methodology_id, mode, build_id,
		runtime_image, cpu_millis, memory_bytes, timeout_seconds, network, slug)
		VALUES ($1, $2, $3, 'W-0123abcd', 'train', $4, 'oramasearch/methodology-runtime:1', 1500, 2147483648, 600, true,
		        'mr-f00000000001')`, runID, customerID, testWorkspaceID, testBuildID); err != nil {
		t.Fatalf("seed methodology run: %v", err)
	}
	runs, err := store.ListMethodologyRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	run := runs[0]
	if run.ID != runID || run.Mode != methodologyModeTrain || run.CPUMillis != 1500 || run.MemoryBytes != 2<<30 ||
		run.TimeoutSeconds != 600 || !run.Network || run.Slug != "mr-f00000000001" || run.Status != nil ||
		run.Build == nil || run.Build.Phase != BuildPhaseSucceeded || run.Build.Target != testBuildTarget || run.Build.Digest == "" {
		t.Fatalf("run = %+v (build %+v)", run, run.Build)
	}

	// Queued (0082_methodology_run_queue, orama-operator#293) is a phase the schema accepts.
	queued := LakeMethodologyRunStatus{RunID: runID, Phase: MethodologyPhaseQueued, ObservedGeneration: 1,
		Message: "Queued: 2 of this workspace's methodology runs are running (the limit is 2); this run is next."}
	if err := store.UpsertMethodologyRunStatus(ctx, queued); err != nil {
		t.Fatalf("upsert a queued status: %v", err)
	}
	if runs, err := store.ListMethodologyRuns(ctx); err != nil || len(runs) != 1 || runs[0].Status == nil || !runs[0].Status.equal(queued) {
		t.Fatalf("queued status read back = %+v, %v", runs, err)
	}

	started := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	code := int32(0)
	status := LakeMethodologyRunStatus{RunID: runID, Phase: MethodologyPhaseCollecting, Message: "reporting the result",
		ExitCode: &code, CredentialSHA256: "abc", ObservedGeneration: 1, StartedAt: &started, FinishedAt: &finished}
	if err := store.UpsertMethodologyRunStatus(ctx, status); err != nil {
		t.Fatalf("upsert status: %v", err)
	}
	runs, err = store.ListMethodologyRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].Status == nil || !runs[0].Status.equal(status) {
		t.Fatalf("status read back = %+v, %v", runs, err)
	}

	// A finished, absent run with no credential left is not read again.
	if _, err := tx.Exec(ctx, `UPDATE methodology_runs SET desired = 'absent', generation = 2 WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	status.Phase, status.CredentialSHA256, status.ObservedGeneration = MethodologyPhaseSucceeded, "", 2
	if err := store.UpsertMethodologyRunStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	if runs, err := store.ListMethodologyRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("a settled run is still read: %+v, %v", runs, err)
	}

	// Its resources: the kind is accepted, and the workload pass finds the run by id.
	row := WorkloadResources{Kind: WorkloadKindMethodologyRun, ID: runID, CPUMillis: 1500, MemoryBytes: 2 << 30}
	if err := store.UpsertWorkloadResources(ctx, row); err != nil {
		t.Fatalf("methodology_run workload row: %v", err)
	}
	objects, err := store.ListWorkloadObjects(ctx, []string{runID}, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, object := range objects {
		found = found || (object.Kind == WorkloadKindMethodologyRun && object.ID == runID && object.Slug == "mr-f00000000001")
	}
	if !found {
		t.Fatalf("objects by id = %+v", objects)
	}
	if err := store.DeleteWorkloadResources(ctx, WorkloadKindMethodologyRun, runID); err != nil {
		t.Fatal(err)
	}
}
