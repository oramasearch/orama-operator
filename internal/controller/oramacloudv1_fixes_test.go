/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
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

// TestDashboardNeverMigratesControlDB guards the shared production PlanetScale (orama-bridge#142):
// the dashboard runs no db-migrate in either mode. In-cluster, the operator's PostgresSchema owns the
// schema (a second migrate collides, Postgres 42710); external, the database's owner runs migrations.
// External mode must also read DATABASE_URL from credentialsSecretRef, not the in-cluster CNPG Secret.
func TestDashboardNeverMigratesControlDB(t *testing.T) {
	dashboard := func(o *oramav1alpha1.OramaCloudV1) *appsv1.Deployment {
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
		return dep
	}
	databaseURLSecret := func(dep *appsv1.Deployment) string {
		for _, c := range dep.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == "DATABASE_URL" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					return e.ValueFrom.SecretKeyRef.Name
				}
			}
		}
		return ""
	}

	inCluster := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true}
		o.Spec.Components.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	})
	external := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true}
		o.Spec.Components.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external",
			CredentialsSecretRef: &corev1.LocalObjectReference{Name: "planetscale"}}
	})
	for name, oc := range map[string]*oramav1alpha1.OramaCloudV1{"in-cluster": inCluster, "external": external} {
		if n := len(dashboard(oc).Spec.Template.Spec.InitContainers); n != 0 {
			t.Errorf("%s controlDB: dashboard has %d init containers, want none (no db-migrate)", name, n)
		}
	}
	if got := databaseURLSecret(dashboard(inCluster)); got != "v1-controldb-app" {
		t.Errorf("in-cluster DATABASE_URL secret = %q, want v1-controldb-app", got)
	}
	if got := databaseURLSecret(dashboard(external)); got != "planetscale" {
		t.Errorf("external DATABASE_URL secret = %q, want planetscale (credentialsSecretRef)", got)
	}
}

// TestExternalControlDBRequiresCredentials: an external control DB without credentialsSecretRef
// fails the reconcile (Degraded) instead of starting clients against a Secret that doesn't exist.
func TestExternalControlDBRequiresCredentials(t *testing.T) {
	s := ocv1TestScheme(t)
	r := &OramaCloudV1Reconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external", Endpoint: "pg.example.com:5432"}
	})
	if err := r.reconcileControlDB(context.Background(), oc); err == nil {
		t.Error("external controlDB without credentialsSecretRef: want an error")
	}
	oc.Spec.Components.ControlDB.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "planetscale"}
	if err := r.reconcileControlDB(context.Background(), oc); err != nil {
		t.Errorf("external controlDB with credentialsSecretRef: %v", err)
	}
}

// TestEnvoySidecarBypassesEntrypointChown is the regression for the reader's second blocker: the
// envoyproxy/envoy image's /docker-entrypoint.sh chowns /dev/stdout, which fails under the non-root
// hardened securityContext (#18) and exits the container. The operator must run envoy directly (its
// own Command) so the entrypoint's chown never runs.
func TestEnvoySidecarBypassesEntrypointChown(t *testing.T) {
	c := envoySidecar()
	if len(c.Command) == 0 || c.Command[0] != "envoy" {
		t.Errorf("envoy sidecar Command=%v, want it to exec `envoy` directly (bypass /docker-entrypoint.sh, whose chown fails as non-root)", c.Command)
	}
}
