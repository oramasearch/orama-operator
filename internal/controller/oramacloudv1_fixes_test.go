/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

func ocv1TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := oramav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newOCV1(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	oc := &oramav1alpha1.OramaCloudV1{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "orama-system", UID: types.UID("uid-v1")},
		Spec: oramav1alpha1.OramaCloudV1Spec{
			Region: oramav1alpha1.RegionSpec{Name: "us-east", Role: "primary"},
		},
	}
	if mut != nil {
		mut(oc)
	}
	return oc
}

// TestBrokerTopologyStreamRetentionMatchesOramaCore is the regression for the live AKS 400: the
// operator MUST declare the oramacore-operations stream with the SAME x-max-length-bytes OramaCore
// uses when IT creates the stream on connect (5 GB). A mismatched (or absent) arg makes RabbitMQ
// reject the redeclare ("inequivalent arg 'x-max-length-bytes'"), which fails the whole topology
// reconcile before the reader/writer users are created — so the reader can never auth to the stream.
func TestBrokerTopologyStreamRetentionMatchesOramaCore(t *testing.T) {
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Broker = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	})
	if err := r.reconcileBrokerTopology(context.Background(), oc); err != nil {
		t.Fatalf("reconcileBrokerTopology: %v", err)
	}
	topo := &oramav1alpha1.RabbitMQTopology{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "v1-broker", Namespace: "orama-system"}, topo); err != nil {
		t.Fatalf("get RabbitMQTopology: %v", err)
	}
	if len(topo.Spec.Streams) != 1 {
		t.Fatalf("streams=%d, want 1", len(topo.Spec.Streams))
	}
	if got := topo.Spec.Streams[0].MaxLengthBytes; got != brokerOperationsStreamMaxBytes {
		t.Errorf("stream MaxLengthBytes=%d, want %d (must match OramaCore, else RabbitMQ 400s the redeclare and the reader can't auth)",
			got, brokerOperationsStreamMaxBytes)
	}
	if got := topo.Spec.Streams[0].LeaderLocator; got != brokerOperationsStreamLeaderLocator {
		t.Errorf("stream LeaderLocator=%q, want %q (OramaCore's second stream arg — a mismatch 400s the redeclare too)",
			got, brokerOperationsStreamLeaderLocator)
	}
}

// TestDashboardDbMigrateGatedOnSchemaOwnership is the regression for the dashboard crash-loop
// (Postgres 42710, "type already exists"): when the operator owns the control-DB schema (in-cluster
// controlDB → reconcileControlDBMigrations applies it via a PostgresSchema Job), the dashboard must
// NOT also run its own drizzle-kit db-migrate — the double-apply collides. It runs its own migrate
// ONLY for an external control DB (where the operator manages no PostgresSchema).
func TestDashboardDbMigrateGatedOnSchemaOwnership(t *testing.T) {
	hasDbMigrateInit := func(o *oramav1alpha1.OramaCloudV1) bool {
		s := ocv1TestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
		if err := r.reconcileDashboard(context.Background(), o); err != nil {
			t.Fatalf("reconcileDashboard: %v", err)
		}
		dep := &appsv1.Deployment{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: "v1-dashboard", Namespace: "orama-system"}, dep); err != nil {
			t.Fatalf("get dashboard Deployment: %v", err)
		}
		for _, ic := range dep.Spec.Template.Spec.InitContainers {
			if ic.Name == "db-migrate" {
				return true
			}
		}
		return false
	}

	// in-cluster controlDB → the operator's PostgresSchema owns the schema → NO dashboard db-migrate.
	inCluster := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true}
		o.Spec.Components.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	})
	if hasDbMigrateInit(inCluster) {
		t.Error("in-cluster controlDB: dashboard must NOT run db-migrate (PostgresSchema owns the schema — double-apply → Postgres 42710)")
	}

	// external controlDB → no operator-managed PostgresSchema → dashboard runs its own migrate.
	external := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true}
		o.Spec.Components.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external", Endpoint: "pg.example.com:5432"}
	})
	if !hasDbMigrateInit(external) {
		t.Error("external controlDB: dashboard SHOULD run its own db-migrate (operator manages no PostgresSchema)")
	}
}
