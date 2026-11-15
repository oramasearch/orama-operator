// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// checkMethodologyDeploymentsPgx runs the served methodology models SQL (lake#690) against the
// vendored schema, inside TestServicesPgxStoreAgainstPostgres's rolled-back transaction, after
// its build testBuildID has succeeded.
func checkMethodologyDeploymentsPgx(t *testing.T, ctx context.Context, tx pgx.Tx, store *pgxServiceStore, customerID string) {
	t.Helper()
	const deploymentID = "f0000000-0000-4000-8000-0000000000d1"
	if _, err := tx.Exec(ctx, `INSERT INTO methodology_deployments (id, customer_id, workspace_id, methodology_id, model_id,
		build_id, runtime_image, cpu_millis, memory_bytes, gpus, gpu_class, network, slug)
		VALUES ($1, $2, $3, 'W-0123abcd', 'model-1', $4, 'oramasearch/methodology-runtime:1', 1000, 2147483648, 0, '',
		        false, 'md-f000000000d1')`, deploymentID, customerID, testWorkspaceID, testBuildID); err != nil {
		t.Fatalf("seed methodology deployment: %v", err)
	}
	deployments, err := store.ListMethodologyDeployments(ctx)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments = %+v, %v", deployments, err)
	}
	deployment := deployments[0]
	if deployment.ID != deploymentID || deployment.ModelID != "model-1" || deployment.CPUMillis != 1000 ||
		deployment.MemoryBytes != 2<<30 || deployment.Slug != "md-f000000000d1" || deployment.WorkspaceNamespace != testWorkspaceName || deployment.Status != nil ||
		deployment.Build == nil || deployment.Build.Phase != BuildPhaseSucceeded || deployment.Build.Target != testBuildTarget {
		t.Fatalf("deployment = %+v (build %+v)", deployment, deployment.Build)
	}
	// One present deployment per model of a workspace.
	if _, err := tx.Exec(ctx, "SAVEPOINT second"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO methodology_deployments (customer_id, workspace_id, methodology_id, model_id,
		runtime_image, cpu_millis, memory_bytes, slug) VALUES ($1, $2, 'W-0123abcd', 'model-1', 'r', 1, 1, 'md-f000000000d2')`,
		customerID, testWorkspaceID); err == nil {
		t.Fatal("a second present deployment of one model is refused")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT second"); err != nil {
		t.Fatal(err)
	}

	ready := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	status := LakeMethodologyDeploymentStatus{DeploymentID: deploymentID, Phase: DeploymentPhaseReady, Message: "serving",
		CredentialSHA256: "abc", Endpoint: methodologyServeEndpoint("md-f000000000d1", "lake-prod-methodology-runs"),
		ObservedGeneration: 1, ReadyAt: &ready}
	if err := store.UpsertMethodologyDeploymentStatus(ctx, status); err != nil {
		t.Fatalf("upsert status: %v", err)
	}
	deployments, err = store.ListMethodologyDeployments(ctx)
	if err != nil || len(deployments) != 1 || deployments[0].Status == nil || !deployments[0].Status.equal(status) {
		t.Fatalf("status read back = %+v, %v", deployments, err)
	}
	// The schema refuses an endpoint that is not a serve Service.
	if _, err := tx.Exec(ctx, "SAVEPOINT endpoint"); err != nil {
		t.Fatal(err)
	}
	bad := status
	bad.Endpoint = "http://169.254.169.254"
	if err := store.UpsertMethodologyDeploymentStatus(ctx, bad); err == nil {
		t.Fatal("an endpoint outside the cluster's serve Services is refused")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT endpoint"); err != nil {
		t.Fatal(err)
	}

	// Its resources: the kind is accepted, and the live read lists the present deployment.
	row := WorkloadResources{Kind: WorkloadKindMethodologyDeployment, ID: deploymentID, Pods: 1, CPUMillis: 1000, MemoryBytes: 2 << 30}
	if err := store.UpsertWorkloadResources(ctx, row); err != nil {
		t.Fatalf("methodology_deployment workload row: %v", err)
	}
	objects, err := store.ListWorkloadObjects(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, object := range objects {
		found = found || (object.Kind == WorkloadKindMethodologyDeployment && object.ID == deploymentID && object.live())
	}
	if !found {
		t.Fatalf("live objects = %+v", objects)
	}
	if err := store.DeleteWorkloadResources(ctx, WorkloadKindMethodologyDeployment, deploymentID); err != nil {
		t.Fatal(err)
	}

	// A stopped, absent deployment with no credential left is not read again.
	if _, err := tx.Exec(ctx, `UPDATE methodology_deployments SET desired = 'absent', generation = 2 WHERE id = $1`, deploymentID); err != nil {
		t.Fatal(err)
	}
	status.Phase, status.CredentialSHA256, status.Endpoint, status.ObservedGeneration = DeploymentPhaseStopped, "", "", 2
	if err := store.UpsertMethodologyDeploymentStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	if deployments, err := store.ListMethodologyDeployments(ctx); err != nil || len(deployments) != 0 {
		t.Fatalf("a settled deployment is still read: %+v, %v", deployments, err)
	}
}
