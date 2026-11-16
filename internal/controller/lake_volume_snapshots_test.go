// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeVolumeSnapshotsListenerUsesDedicatedChannel(t *testing.T) {
	lake := testWorkspaceLake()
	listener := NewLakeVolumeSnapshotsListener(fake.NewClientBuilder().WithScheme(newLakeScheme(t)).Build())
	if listener.channel != "lake_volume_snapshots" || listener.enabled == nil || !listener.enabled(lake) {
		t.Fatal("volume snapshot listener is not scoped to snapshot-enabled Lakes")
	}
	lake.Spec.Postgres.Enabled = boolPtr(false)
	if listener.enabled(lake) {
		t.Fatal("volume snapshot listener stays enabled without the control Postgres")
	}
}

func TestSnapshotNameIsTheClaimAndTheUTCInstant(t *testing.T) {
	at := time.Date(2026, 10, 3, 19, 0, 12, 500, time.FixedZone("PDT", -7*3600))
	if got := snapshotName("data-prod-ws-123e4567e89b-zetta-0", at); got != "data-prod-ws-123e4567e89b-zetta-0-20261004t020012z" {
		t.Fatalf("snapshot name = %q", got)
	}
	long := snapshotName(stringOf('a', 260), at)
	if len(long) > 253 {
		t.Fatalf("snapshot name is %d characters", len(long))
	}
	if lakeSnapshotClassName(&oramav1alpha1.Lake{}) != "lake-retain" || lakeSnapshotDriver(&oramav1alpha1.Lake{}) != "disk.csi.azure.com" {
		t.Fatal("snapshot defaults changed")
	}
}

func stringOf(c byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = c
	}
	return string(out)
}

func TestLakeVolumeSnapshotsPgxStoreAgainstPostgres(t *testing.T) {
	uri := os.Getenv("LAKE_WORKSPACES_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAKE_WORKSPACES_TEST_POSTGRES_URI is required for native volume snapshot SQL verification")
	}
	fixture, err := os.ReadFile("testdata/lake-workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatal("cannot connect to test Postgres")
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id, customer_id, name, environment, desired, generation, namespace)
	 VALUES ($1, $2, 'Production', 'prod', 'running', 3, $3)`, testWorkspaceID, testWorkspaceCustomer, testWorkspaceName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO volume_snapshot_policies (target, keep, requested_at)
	 VALUES ('store', 2, NULL), ($1, 3, '2026-10-03T12:00:00Z')`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	store := &pgxVolumeSnapshotsStore{pgxServiceStore: &pgxServiceStore{db: tx}}
	policies, err := store.ListVolumeSnapshotPolicies(ctx)
	if err != nil || len(policies) != 2 || policies[0].Target != "store" || policies[0].RequestedAt != nil ||
		policies[0].WorkspaceDesired != "" || policies[1].Target != testWorkspaceID || policies[1].Keep != 3 ||
		policies[1].WorkspaceDesired != "running" || policies[1].WorkspaceNamespace != testWorkspaceName ||
		policies[1].RequestedAt == nil || policies[1].Schedule != "0 3 * * *" || policies[1].Status != nil {
		t.Fatalf("policies = %+v, %v", policies, err)
	}
	at := time.Date(2026, 10, 3, 12, 0, 1, 123456000, time.UTC)
	status := LakeVolumeSnapshotStatus{Target: testWorkspaceID, SnapshotName: "data-x-20261003t120001z", ReadyToUse: true, SnapshotAt: &at}
	if err := store.UpsertVolumeSnapshotStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	status.Message = "again"
	if err := store.UpsertVolumeSnapshotStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	policies, err = store.ListVolumeSnapshotPolicies(ctx)
	if err != nil || policies[1].Status == nil || !policies[1].Status.equal(status) {
		t.Fatalf("status did not round trip: %+v, %v", policies[1].Status, err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE volume_snapshot_status"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE volume_snapshot_policies"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListVolumeSnapshotPolicies(ctx); err != errVolumeSnapshotsTableMissing {
		t.Fatalf("missing schema not recognized: %v", err)
	}
}
