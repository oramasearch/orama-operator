/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type lakeReadinessFixture struct {
	t          *testing.T
	ctx        context.Context
	client     client.Client
	reconciler *LakeReconciler
	lake       *oramav1alpha1.Lake
}

func newLakeReadinessFixture(t *testing.T) *lakeReadinessFixture {
	t.Helper()
	scheme := newLakeScheme(t)
	disabled := false
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "readiness", UID: "lake-readiness", Generation: 1},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:20260906",
			Zetta: oramav1alpha1.LakeZettaSpec{Enabled: &disabled},
		},
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(lake, cluster, &oramav1alpha1.PostgresSchema{}, &oramav1alpha1.OramaZettaCluster{}, &appsv1.Deployment{}, &corev1.PersistentVolumeClaim{}).
		Build()
	f := &lakeReadinessFixture{
		t: t, ctx: context.Background(), client: c, lake: lake,
		reconciler: &LakeReconciler{Client: c, Scheme: scheme},
	}
	f.reconcile()
	f.schema("Applied")
	f.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	f.get("lake-readiness-store", pvc)
	pvc.Status.Phase = corev1.ClaimBound
	f.updateStatus(pvc)
	f.deployment(1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
	})
	return f
}

func (f *lakeReadinessFixture) get(name string, object client.Object) {
	f.t.Helper()
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: "lake-dev", Name: name}, object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *lakeReadinessFixture) updateStatus(object client.Object) {
	f.t.Helper()
	if err := f.client.Status().Update(f.ctx, object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *lakeReadinessFixture) reconcile() ctrl.Result {
	f.t.Helper()
	result, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: f.lake.Name}})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.client.Get(f.ctx, client.ObjectKey{Name: f.lake.Name}, f.lake); err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *lakeReadinessFixture) deployment(generation int64, status appsv1.DeploymentStatus) {
	f.t.Helper()
	deployment := &appsv1.Deployment{}
	f.get("lake-readiness", deployment)
	deployment.Generation = generation
	if err := f.client.Update(f.ctx, deployment); err != nil {
		f.t.Fatal(err)
	}
	deployment.Status = status
	f.updateStatus(deployment)
}

func (f *lakeReadinessFixture) database(ready, instances int64) {
	f.t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	f.get("lake-readiness-controldb", cluster)
	cluster.Object["status"] = map[string]interface{}{"readyInstances": ready, "instances": instances}
	f.updateStatus(cluster)
}

func (f *lakeReadinessFixture) schema(phase string) {
	f.t.Helper()
	schema := &oramav1alpha1.PostgresSchema{}
	f.get("lake-readiness-controldb-schema", schema)
	schema.Status.Phase = phase
	schema.Status.ObservedGeneration = schema.Generation
	if phase == "Applied" {
		schema.Status.AppliedVersion = schema.Spec.Version
		apimeta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
			Type:               "Applied",
			Status:             metav1.ConditionTrue,
			Reason:             "Applied",
			Message:            "test migration completed",
			ObservedGeneration: schema.Generation,
		})
	}
	f.updateStatus(schema)
}

func (f *lakeReadinessFixture) expectState(phase, reason string) {
	f.t.Helper()
	if f.lake.Status.Phase != phase {
		f.t.Errorf("Lake phase=%s postgresReady=%t schemaPhase=%s; want phase=%s",
			f.lake.Status.Phase, f.lake.Status.PostgresReady, f.lake.Status.SchemaPhase, phase)
	}
	if got := lakeConditionReason(f.lake.Status.Conditions, "Ready"); got != reason {
		f.t.Errorf("Ready condition reason=%q; want %q", got, reason)
	}
	condition := apimeta.FindStatusCondition(f.lake.Status.Conditions, "Ready")
	if condition == nil {
		f.t.Fatal("missing Ready condition")
	}
	wantStatus := metav1.ConditionFalse
	if phase == "Ready" {
		wantStatus = metav1.ConditionTrue
	}
	if condition.Status != wantStatus || condition.ObservedGeneration != f.lake.Generation ||
		condition.Message == "" || condition.LastTransitionTime.IsZero() {
		f.t.Errorf("invalid Ready condition: %+v", condition)
	}
}

type lakeStatusWriteCounter struct {
	client.Client
	writes int
}

func (c *lakeStatusWriteCounter) Status() client.SubResourceWriter {
	return &lakeCountingStatusWriter{SubResourceWriter: c.Client.Status(), counter: c}
}

type lakeCountingStatusWriter struct {
	client.SubResourceWriter
	counter *lakeStatusWriteCounter
}

func (w *lakeCountingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.counter.writes++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *lakeCountingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.counter.writes++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (f *lakeReadinessFixture) expectQuiet(requeue time.Duration) {
	f.t.Helper()
	// Old transition times make accidental timestamp refreshes visible without sleeping.
	for i := range f.lake.Status.Conditions {
		f.lake.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Unix(100, 0))
	}
	f.updateStatus(f.lake)
	before := f.lake.Status.DeepCopy()
	counter := &lakeStatusWriteCounter{Client: f.client}
	for i := 0; i < 3; i++ {
		// Recreate the reconciler to prove that stability does not depend on process memory.
		f.reconciler = &LakeReconciler{Client: counter, Scheme: f.reconciler.Scheme}
		if result := f.reconcile(); result.RequeueAfter != requeue {
			f.t.Errorf("RequeueAfter=%s; want %s", result.RequeueAfter, requeue)
		}
	}
	if counter.writes != 0 || !reflect.DeepEqual(before, &f.lake.Status) {
		f.t.Errorf("unchanged %s state: status writes=%d, status changed=%t",
			before.Phase, counter.writes, !reflect.DeepEqual(before, &f.lake.Status))
	}
}

func TestLakeReadinessFailedSchemaAndUnavailableDatabase(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(0, 1)
	f.schema("Failed")
	f.reconcile()
	t.Logf("Lake phase=%s postgresReady=%t schemaPhase=%s",
		f.lake.Status.Phase, f.lake.Status.PostgresReady, f.lake.Status.SchemaPhase)
	f.expectState("Degraded", "SchemaFailed")
}

func TestLakeReadinessOldRolloutReplicas(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(1, 1)
	f.schema("Applied")
	f.deployment(2, appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1})
	f.reconcile()
	deployment := &appsv1.Deployment{}
	f.get("lake-readiness", deployment)
	t.Logf("Lake phase=%s postgresReady=%t schemaPhase=%s deploymentObservedGeneration=%d updatedReplicas=%d",
		f.lake.Status.Phase, f.lake.Status.PostgresReady, f.lake.Status.SchemaPhase,
		deployment.Status.ObservedGeneration, deployment.Status.UpdatedReplicas)
	f.expectState("Progressing", "DeploymentProgressing")
}

func TestLakeReadinessRecoveryAndDatabaseLoss(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(0, 1)
	f.schema("Failed")
	f.reconcile()
	f.expectState("Degraded", "SchemaFailed")
	f.expectQuiet(lakeStatusRequeue)

	f.database(1, 1)
	f.schema("Applying")
	f.reconcile()
	f.expectState("Progressing", "SchemaPending")
	f.expectQuiet(lakeStatusRequeue)

	f.schema("Applied")
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	f.expectQuiet(lakeStatusRequeue)

	// Only CNPG status changes after success; the returned poll must observe the loss.
	f.database(0, 1)
	f.reconcile()
	f.expectState("Progressing", "PostgresNotReady")
	f.expectQuiet(lakeStatusRequeue)
	if f.lake.Status.PostgresReady {
		t.Error("database readiness remained true after CNPG readiness loss")
	}

	f.database(1, 1)
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	f.expectQuiet(lakeStatusRequeue)
	t.Log("schema/database recovery and subsequent database loss verified; steady-state status writes=0")
}

func TestLakeReadinessDeploymentCounts(t *testing.T) {
	cases := []struct {
		name   string
		status appsv1.DeploymentStatus
	}{
		{"unobserved", appsv1.DeploymentStatus{ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}},
		{"old replicas", appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 2, AvailableReplicas: 2}},
		{"partially updated", appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2}},
		{"unavailable", appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 1}},
		{"unready", appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 1, UpdatedReplicas: 2, AvailableReplicas: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLakeReadinessFixture(t)
			f.database(1, 1)
			f.schema("Applied")
			f.lake.Spec.Replicas = int32Ptr(2)
			if err := f.client.Update(f.ctx, f.lake); err != nil {
				t.Fatal(err)
			}
			f.deployment(2, tc.status)
			f.reconcile()
			f.expectState("Progressing", "DeploymentProgressing")
			f.expectQuiet(lakeStatusRequeue)

			f.deployment(2, appsv1.DeploymentStatus{
				ObservedGeneration: 2, ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2,
			})
			f.reconcile()
			f.expectState("Ready", "LakeReady")
			f.expectQuiet(lakeStatusRequeue)
		})
	}
}

func TestLakeReadinessDeploymentFailures(t *testing.T) {
	cases := []struct {
		condition appsv1.DeploymentCondition
		reason    string
	}{
		{appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: "replica creation failed"}, "ReplicaFailure"},
		{appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "rollout stalled"}, "ProgressDeadlineExceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			f := newLakeReadinessFixture(t)
			f.database(1, 1)
			f.schema("Applied")
			status := appsv1.DeploymentStatus{
				ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
				Conditions: []appsv1.DeploymentCondition{tc.condition},
			}
			f.deployment(1, status)
			f.reconcile()
			f.expectState("Degraded", tc.reason)
			f.expectQuiet(lakeStatusRequeue)

			// A new rollout first waits for the controller to replace the old failure status.
			f.deployment(2, status)
			f.reconcile()
			f.expectState("Progressing", "DeploymentProgressing")
			status.ObservedGeneration = 2
			status.Conditions = nil
			f.deployment(2, status)
			f.reconcile()
			f.expectState("Ready", "LakeReady")
		})
	}
}

func TestLakeReadinessCurrentSchema(t *testing.T) {
	cases := []struct {
		name       string
		phase      string
		version    string
		observed   int64
		wantPhase  string
		wantReason string
	}{
		{"pending", "Pending", "", 2, "Progressing", "SchemaPending"},
		{"applying", "Applying", "", 2, "Progressing", "SchemaPending"},
		{"failed", "Failed", "", 2, "Degraded", "SchemaFailed"},
		{"old generation", "Applied", lakeSchemaVersion(), 1, "Progressing", "SchemaNotCurrent"},
		{"old version", "Applied", "sha256:old", 2, "Progressing", "SchemaNotCurrent"},
		{"missing version", "Applied", "", 2, "Progressing", "SchemaNotCurrent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLakeReadinessFixture(t)
			f.database(1, 1)
			schema := &oramav1alpha1.PostgresSchema{}
			f.get("lake-readiness-controldb-schema", schema)
			schema.Generation = 2
			if err := f.client.Update(f.ctx, schema); err != nil {
				t.Fatal(err)
			}
			schema.Status = oramav1alpha1.PostgresSchemaStatus{
				Phase: tc.phase, AppliedVersion: tc.version, ObservedGeneration: tc.observed,
			}
			f.updateStatus(schema)
			f.reconcile()
			f.expectState(tc.wantPhase, tc.wantReason)
			f.expectQuiet(lakeStatusRequeue)
			f.schema("Applied")
			f.reconcile()
			f.expectState("Ready", "LakeReady")
		})
	}
}

func TestLakeReadinessConfiguredDatabaseInstances(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.schema("Applied")
	f.lake.Spec.Postgres.Instances = int32Ptr(3)
	if err := f.client.Update(f.ctx, f.lake); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []int64{1, 3} {
		f.database(1, observed)
		f.reconcile()
		f.expectState("Progressing", "PostgresNotReady")
		if f.lake.Status.PostgresReady {
			t.Errorf("one ready instance satisfied a three-instance database (observed total=%d)", observed)
		}
	}
	f.database(3, 3)
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	f.expectQuiet(lakeStatusRequeue)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	f.get("lake-readiness-controldb", cluster)
	if err := f.client.Delete(f.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.expectState("Progressing", "PostgresNotReady")
}

func TestLakeReadinessDisabledPostgresAndZeroReplicas(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.schema("Failed")
	disabled := false
	f.lake.Spec.Postgres.Enabled = &disabled
	if err := f.client.Update(f.ctx, f.lake); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	f.expectQuiet(0)
	if f.lake.Status.PostgresReady || f.lake.Status.PostgresInstances != "" || f.lake.Status.SchemaPhase != "" {
		t.Error("disabled Postgres retained database or schema status")
	}

	f.lake.Spec.Replicas = int32Ptr(0)
	if err := f.client.Update(f.ctx, f.lake); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.expectState("Progressing", "DeploymentProgressing")
}

func TestLakeReadinessStoreAndZettaGates(t *testing.T) {
	f := newLakeReadinessFixture(t)
	f.database(1, 1)
	f.schema("Applied")
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	pvc := &corev1.PersistentVolumeClaim{}
	f.get("lake-readiness-store", pvc)
	pvc.Status.Phase = corev1.ClaimPending
	f.updateStatus(pvc)
	f.reconcile()
	f.expectState("Progressing", "StoreUnbound")
	pvc.Status.Phase = corev1.ClaimBound
	f.updateStatus(pvc)

	f.lake.Spec.Zetta.Enabled = nil
	if err := f.client.Update(f.ctx, f.lake); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.expectState("Progressing", "ZettaProgressing")
	zetta := &oramav1alpha1.OramaZettaCluster{}
	f.get("lake-readiness-zetta", zetta)
	zetta.Status.Phase = "Degraded"
	f.updateStatus(zetta)
	f.reconcile()
	f.expectState("Degraded", "ZettaDegraded")
	f.expectQuiet(lakeStatusRequeue)
	zetta.Status.Phase = "Ready"
	zetta.Status.ReadyNodes = 1
	f.updateStatus(zetta)
	f.reconcile()
	f.expectState("Ready", "LakeReady")
	f.expectQuiet(lakeStatusRequeue)
}
