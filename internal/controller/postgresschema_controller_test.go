// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func pgSchemaCR() *oramav1alpha1.PostgresSchema {
	return &oramav1alpha1.PostgresSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "cr-controldb-schema", Namespace: "ns", Generation: 1},
		Spec: oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: "cr-controldb-app", Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: "cr-controldb-migrations", Key: "schema.sql"},
			Version:             "sha256:abc123",
			GuardTable:          "crawler_configs",
			Image:               "postgres:16-alpine",
		},
	}
}

// TestPostgresSchemaWaitsForDatabase asserts the reconciler is a safe no-op (Pending /
// WaitingForDatabase) until the connection Secret exists, and creates no Job.
func TestPostgresSchemaWaitsForDatabase(t *testing.T) {
	scheme := rmqTestScheme(t)
	ps := pgSchemaCR()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ps).
		WithStatusSubresource(&oramav1alpha1.PostgresSchema{}).Build()
	r := &PostgresSchemaReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ps)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got oramav1alpha1.PostgresSchema
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ps), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != "Pending" {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if cond := condReason(got.Status.Conditions, "Applied"); cond != "WaitingForDatabase" {
		t.Errorf("Applied reason = %q, want WaitingForDatabase", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("expected no Job before the DB is ready, got %d", len(jobs.Items))
	}
}

// TestPostgresSchemaAppliesAndTracksVersion asserts that once the connection Secret exists the
// reconciler creates the migration Job (Phase=Applying), and once that Job succeeds it records the
// applied version (read back from the Job annotation) with Phase=Applied.
func TestPostgresSchemaAppliesAndTracksVersion(t *testing.T) {
	scheme := rmqTestScheme(t)
	ps := pgSchemaCR()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cr-controldb-app", Namespace: "ns"},
		Data:       map[string][]byte{"uri": []byte("postgresql://u:p@host:5432/db")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ps, secret).
		WithStatusSubresource(&oramav1alpha1.PostgresSchema{}, &batchv1.Job{}).Build()
	r := &PostgresSchemaReconciler{Client: c, Scheme: scheme}
	key := client.ObjectKeyFromObject(ps)

	// First pass: creates the Job, which has not succeeded yet.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "cr-controldb-schema-migrate"}, job); err != nil {
		t.Fatalf("expected migration Job created: %v", err)
	}
	if job.Annotations[pgSchemaVersionAnnotation] != "sha256:abc123" {
		t.Errorf("job version annotation = %q", job.Annotations[pgSchemaVersionAnnotation])
	}
	var got oramav1alpha1.PostgresSchema
	_ = c.Get(context.Background(), key, &got)
	if got.Status.Phase != "Applying" {
		t.Errorf("phase after create = %q, want Applying", got.Status.Phase)
	}

	// Mark the Job succeeded, reconcile again → Applied + version recorded.
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("update job status: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	_ = c.Get(context.Background(), key, &got)
	if got.Status.Phase != "Applied" {
		t.Errorf("phase after success = %q, want Applied", got.Status.Phase)
	}
	if got.Status.AppliedVersion != "sha256:abc123" {
		t.Errorf("appliedVersion = %q, want sha256:abc123", got.Status.AppliedVersion)
	}
	if cond := condStatus(got.Status.Conditions, "Applied"); cond != metav1.ConditionTrue {
		t.Errorf("Applied condition = %q, want True", cond)
	}
}

// ---- helpers ----

func condReason(conds []metav1.Condition, typ string) string {
	for _, c := range conds {
		if c.Type == typ {
			return c.Reason
		}
	}
	return ""
}

func condStatus(conds []metav1.Condition, typ string) metav1.ConditionStatus {
	for _, c := range conds {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

func TestPostgresSchemaReplacesObsoleteRunningAndFailedJobs(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "running"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			scheme := rmqTestScheme(t)
			ps := pgSchemaCR()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: ps.Spec.ConnectionSecretRef.Name, Namespace: ps.Namespace},
				Data:       map[string][]byte{"uri": []byte("postgresql://local/proof")},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ps, secret).
				WithStatusSubresource(ps, &batchv1.Job{}).Build()
			r := &PostgresSchemaReconciler{Client: c, Scheme: scheme}
			ctx := context.Background()
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ps)}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			var job batchv1.Job
			jobKey := client.ObjectKey{Namespace: ps.Namespace, Name: ps.Name + "-migrate"}
			if err := c.Get(ctx, jobKey, &job); err != nil {
				t.Fatal(err)
			}
			if failed {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
				if err := c.Status().Update(ctx, &job); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Get(ctx, request.NamespacedName, ps); err != nil {
				t.Fatal(err)
			}
			ps.Spec.Version = "sha256:new"
			if err := c.Update(ctx, ps); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, request.NamespacedName, ps); err != nil {
				t.Fatal(err)
			}
			if ps.Status.Phase != "Applying" || condReason(ps.Status.Conditions, "Applied") != "ReplacingMigrationJob" {
				t.Fatalf("replacement status = %+v", ps.Status)
			}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, jobKey, &job); err != nil {
				t.Fatal(err)
			}
			if job.Annotations[pgSchemaVersionAnnotation] != ps.Spec.Version || job.Status.Succeeded != 0 || jobFailed(&job) {
				t.Fatalf("replacement Job did not start fresh: %+v", job)
			}
		})
	}
}
