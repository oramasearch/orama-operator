/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// pgSchemaVersionAnnotation records, on the migration Job, the schema version it was created to
// apply — so status.appliedVersion is read back from what actually ran, not the (possibly bumped)
// desired spec.version.
const pgSchemaVersionAnnotation = "orama.cloud/schema-version"

// pgSchemaRequeue is the steady-state resync; pgSchemaWaitRequeue is the retry cadence while the
// database (its connection Secret) or the migration Job is not yet done — ordered readiness gating.
const (
	pgSchemaRequeue     = 5 * time.Minute
	pgSchemaWaitRequeue = 15 * time.Second
)

// PostgresSchemaReconciler applies + tracks a versioned Postgres schema by owning an idempotent
// migration Job and recording the applied version in status. It supersedes the bare migration Job
// (#34) with a reconciled, auditable CRD (#40). No finalizer — deleting the CR must never drop the
// schema/data; it only stops managing the migration (the Job is garbage-collected).
type PostgresSchemaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=postgresschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=postgresschemas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *PostgresSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ps oramav1alpha1.PostgresSchema
	if err := r.Get(ctx, req.NamespacedName, &ps); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ordered gating: no-op until the connection Secret is present.
	present, err := r.connectionSecretPresent(ctx, ps.Namespace, ps.Spec.ConnectionSecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		r.setStatus(ctx, &ps, "Pending", ps.Status.AppliedVersion, "WaitingForDatabase",
			"waiting for the connection Secret ("+ps.Spec.ConnectionSecretRef.Name+")", metav1.ConditionFalse)
		return ctrl.Result{RequeueAfter: pgSchemaWaitRequeue}, nil
	}

	// Ensure the migration Job (create-if-absent, annotated with the version it applies). A version
	// bump re-applies only after the prior Job is deleted — matching the combined-schema apply-once
	// model (incremental/versioned re-apply is a tracked follow-up).
	job, err := r.ensureMigrationJob(ctx, &ps)
	if err != nil {
		return ctrl.Result{}, err
	}

	appliedVersion := job.Annotations[pgSchemaVersionAnnotation]
	switch {
	case job.Status.Succeeded > 0:
		r.setStatus(ctx, &ps, "Applied", appliedVersion, "Applied",
			fmt.Sprintf("schema version %s applied", appliedVersion), metav1.ConditionTrue)
		return ctrl.Result{RequeueAfter: pgSchemaRequeue}, nil
	case jobFailed(job):
		r.setStatus(ctx, &ps, "Failed", ps.Status.AppliedVersion, "MigrationFailed",
			"migration Job "+job.Name+" failed — inspect its pod logs", metav1.ConditionFalse)
		// Terminal until the Job is deleted/re-run; resync slowly.
		return ctrl.Result{RequeueAfter: pgSchemaRequeue}, nil
	default:
		r.setStatus(ctx, &ps, "Applying", ps.Status.AppliedVersion, "Applying",
			"migration Job "+job.Name+" is running", metav1.ConditionFalse)
		return ctrl.Result{RequeueAfter: pgSchemaWaitRequeue}, nil
	}
}

func (r *PostgresSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PostgresSchema{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

// connectionSecretPresent reports whether the connection Secret + key exist (nil-safe wait).
func (r *PostgresSchemaReconciler) connectionSecretPresent(ctx context.Context, ns string, ref oramav1alpha1.PostgresSecretKeyRef) (bool, error) {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return len(sec.Data[ref.Key]) > 0, nil
}

// ensureMigrationJob creates the migration Job if absent (its spec is immutable, so it is only set
// on creation — matching the existing control-DB migrate Job), and returns the live Job.
func (r *PostgresSchemaReconciler) ensureMigrationJob(ctx context.Context, ps *oramav1alpha1.PostgresSchema) (*batchv1.Job, error) {
	name := ps.Name + "-migrate"
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ps.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		if job.CreationTimestamp.IsZero() {
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/postgresschema": ps.Name}
			job.Annotations = map[string]string{pgSchemaVersionAnnotation: ps.Spec.Version}
			job.Spec = pgMigrationJobSpec(ps)
		}
		return controllerutil.SetControllerReference(ps, job, r.Scheme)
	}); err != nil {
		return nil, err
	}
	return job, nil
}

// pgMigrationJobSpec builds the idempotent migration Job: wait for the DB, skip if the guard table
// already exists, else apply the whole schema in one transaction (all-or-nothing).
func pgMigrationJobSpec(ps *oramav1alpha1.PostgresSchema) batchv1.JobSpec {
	guard := ps.Spec.GuardTable
	if guard == "" {
		guard = "crawler_configs"
	}
	image := ps.Spec.Image
	if image == "" {
		image = "postgres:16-alpine"
	}
	schemaFile := "/migrations/" + ps.Spec.SchemaConfigMapRef.Key
	script := `set -eu
echo "postgres-schema: waiting for the database..."
until psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; do sleep 3; done
if psql "$DATABASE_URL" -tAc "SELECT to_regclass('public.` + guard + `')" | grep -q ` + guard + `; then
  echo "schema already applied (` + guard + ` present) — nothing to do"; exit 0
fi
echo "applying schema (version ` + ps.Spec.Version + `)..."
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f ` + schemaFile + `
echo "postgres-schema: done"`

	lbls := map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/postgresschema": ps.Name}
	return batchv1.JobSpec{
		BackoffLimit: int32Ptr(20), // retry while the database comes up
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "migrate",
					Image:   image, // ships psql
					Command: []string{"sh", "-c", script},
					Env: []corev1.EnvVar{{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: ps.Spec.ConnectionSecretRef.Name}, Key: ps.Spec.ConnectionSecretRef.Key}}}},
					VolumeMounts: []corev1.VolumeMount{{Name: "migrations", MountPath: "/migrations", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{
					Name: "migrations",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: ps.Spec.SchemaConfigMapRef.Name}}},
				}},
			},
		},
	}
}

// jobFailed reports whether the Job has a terminal Failed condition (backoff exhausted).
func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// setStatus applies the computed status under the anti-flap guard (write ONLY on a real transition).
func (r *PostgresSchemaReconciler) setStatus(ctx context.Context, ps *oramav1alpha1.PostgresSchema, phase, appliedVersion, reason, msg string, applied metav1.ConditionStatus) {
	desired := oramav1alpha1.PostgresSchemaStatus{
		Phase:              phase,
		AppliedVersion:     appliedVersion,
		ObservedGeneration: ps.Generation,
		Message:            msg,
	}
	cond := metav1.Condition{Type: "Applied", Status: applied, Reason: reason, Message: msg, ObservedGeneration: ps.Generation}
	changed := ps.Status.Phase != desired.Phase ||
		ps.Status.AppliedVersion != desired.AppliedVersion ||
		ps.Status.ObservedGeneration != desired.ObservedGeneration ||
		ps.Status.Message != desired.Message
	desired.Conditions = ps.Status.Conditions
	if apimeta.SetStatusCondition(&desired.Conditions, cond) {
		changed = true
	}
	if !changed {
		return
	}
	ps.Status = desired
	if err := r.Status().Update(ctx, ps); err != nil {
		logf.FromContext(ctx).Error(err, "status update failed", "postgresschema", ps.Name)
	}
}
