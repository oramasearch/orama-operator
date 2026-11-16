// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"regexp"
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

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
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

// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
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

	// Replace an obsolete Job before applying the desired combined schema. Foreground deletion
	// waits for its pods, so two versions cannot apply concurrently.
	job, err := r.ensureMigrationJob(ctx, &ps)
	if err != nil {
		return ctrl.Result{}, err
	}

	if job == nil || !job.DeletionTimestamp.IsZero() || job.Annotations[pgSchemaVersionAnnotation] != ps.Spec.Version {
		r.setStatus(ctx, &ps, "Applying", ps.Status.AppliedVersion, "ReplacingMigrationJob",
			"waiting for the obsolete migration Job and its pods to be deleted", metav1.ConditionFalse)
		return ctrl.Result{RequeueAfter: pgSchemaWaitRequeue}, nil
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
		Owns(&corev1.ServiceAccount{}).
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

// ensureMigrationJob returns the current version's Job, or nil while an obsolete Job is deleting.
// The immutable spec is set only on creation; foreground deletion prevents overlapping applies.
func (r *PostgresSchemaReconciler) ensureMigrationJob(ctx context.Context, ps *oramav1alpha1.PostgresSchema) (*batchv1.Job, error) {
	name := ps.Name + "-migrate"
	if err := ensureCloudV1ServiceAccount(ctx, r.Client, r.Scheme, ps, name, pgMigrationPodLabels(ps)); err != nil {
		return nil, err
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ps.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		if !metav1.IsControlledBy(job, ps) {
			return nil, fmt.Errorf("migration Job %s is not controlled by PostgresSchema %s", name, ps.Name)
		}
		if !job.DeletionTimestamp.IsZero() {
			return nil, nil
		}
		desiredImage := pgMigrationJobSpec(ps).Template.Spec.Containers[0].Image
		obsoleteImage := job.Status.Succeeded == 0 && (len(job.Spec.Template.Spec.Containers) == 0 || job.Spec.Template.Spec.Containers[0].Image != desiredImage)
		if job.Annotations[pgSchemaVersionAnnotation] != ps.Spec.Version || !pgMigrationScopeCurrent(ps, job) || obsoleteImage {
			policy := metav1.DeletePropagationForeground
			options := &client.DeleteOptions{
				PropagationPolicy: &policy,
				Preconditions:     &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion},
			}
			if err := r.Delete(ctx, job, options); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			return nil, nil
		}
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = pgMigrationPodLabels(ps)
	job.Annotations = map[string]string{pgSchemaVersionAnnotation: ps.Spec.Version}
	job.Spec = pgMigrationJobSpec(ps)
	if err := controllerutil.SetControllerReference(ps, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func pgMigrationPodLabels(ps *oramav1alpha1.PostgresSchema) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"orama.cloud/postgresschema":   ps.Name,
	}
	// Copy only sandbox identity, not application Service labels. In particular a Lake Service
	// must never send application requests to a schema Job.
	for _, key := range []string{"app.kubernetes.io/instance", lakeInstanceLabel} {
		if value := ps.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	return labels
}

func pgMigrationScopeCurrent(ps *oramav1alpha1.PostgresSchema, job *batchv1.Job) bool {
	for key, value := range pgMigrationPodLabels(ps) {
		if job.Spec.Template.Labels[key] != value {
			return false
		}
	}
	return true
}

// schemaMigrationPattern matches a schema file's migration records,
// `INSERT INTO schema_migrations (version) VALUES ('NNNN_name')`, with or without whitespace before
// `(version)`: lake writes 0098_authentication_attempts as `schema_migrations(version)` (#948). The
// Job script extracts the same lines with pgSchemaMigrationSed; TestSchemaMigrationSedMatchesGo keeps
// the two in step.
var schemaMigrationPattern = regexp.MustCompile(`(?m)^[ \t]*INSERT INTO schema_migrations[ \t]*\(version\) VALUES \('([0-9]{4}_[A-Za-z0-9_]+)'\)`)

// pgSchemaMigrationSed prints each migration version a schema file records, one per line.
const pgSchemaMigrationSed = `sed -n "s/^[[:space:]]*INSERT INTO schema_migrations[[:space:]]*(version) VALUES ('\([0-9][0-9][0-9][0-9]_[A-Za-z0-9_]*\)').*/\1/p"`

// schemaMigrationVersions returns the migration versions a schema file records, in file order.
func schemaMigrationVersions(sql string) []string {
	var versions []string
	for _, match := range schemaMigrationPattern.FindAllStringSubmatch(sql, -1) {
		versions = append(versions, match[1])
	}
	return versions
}

// pgMigrationJobSpec builds the idempotent migration Job: wait for the DB, skip only if the guard
// table exists AND every migration the schema file records is present in schema_migrations, else
// apply the whole schema in one transaction (all-or-nothing). The guard alone is not enough: a
// migration numbered before the guard's can merge after it (#271), and the guard then hides it.
// A database with the guard but no schema_migrations table, and a schema file that records no
// migrations, keep the guard-only behavior. With spec.applyEveryVersion there is no guard: the Job
// of each version applies the file.
func pgMigrationJobSpec(ps *oramav1alpha1.PostgresSchema) batchv1.JobSpec {
	if ps.Spec.Migrate != nil {
		return pgImageMigrationJobSpec(ps)
	}
	guard := ps.Spec.GuardTable
	if guard == "" {
		guard = "crawler_configs"
	}
	image := ps.Spec.Image
	if image == "" || image == "postgres:16-alpine" {
		image = pinnedPostgres16Image
	}
	schemaFile := "/migrations/" + ps.Spec.SchemaConfigMapRef.Key
	guardCheck := `if psql "$DATABASE_URL" -tAc "SELECT to_regclass('public.` + guard + `')" | grep -q ` + guard + `; then
  expected=$(` + pgSchemaMigrationSed + ` ` + schemaFile + `)
  if [ -z "$expected" ]; then
    echo "schema already applied (` + guard + ` present) — nothing to do"; exit 0
  fi
  if ! psql "$DATABASE_URL" -tAc "SELECT to_regclass('public.schema_migrations')" | grep -q schema_migrations; then
    echo "schema already applied (` + guard + ` present, no schema_migrations table) — nothing to do"; exit 0
  fi
  values=$(printf "('%s')," $expected)
  missing=$(psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -tAc "SELECT string_agg(v, ' ' ORDER BY v) FROM (VALUES ${values%,}) AS expected(v) WHERE NOT EXISTS (SELECT 1 FROM schema_migrations m WHERE m.version = expected.v)")
  if [ -z "$missing" ]; then
    echo "schema already applied (` + guard + ` present, every schema_migrations row recorded) — nothing to do"; exit 0
  fi
  echo "` + guard + ` present but schema_migrations lacks: $missing"
fi
`
	if ps.Spec.ApplyEveryVersion {
		guardCheck = ""
	}
	script := `set -eu
echo "postgres-schema: waiting for the database..."
until psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; do sleep 3; done
` + guardCheck + `echo "applying schema (version ` + ps.Spec.Version + `)..."
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f ` + schemaFile + `
echo "postgres-schema: done"`

	lbls := pgMigrationPodLabels(ps)
	return batchv1.JobSpec{
		BackoffLimit: int32Ptr(20), // retry while the database comes up
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				ServiceAccountName:           ps.Name + "-migrate",
				AutomountServiceAccountToken: boolPtr(false),
				EnableServiceLinks:           boolPtr(false),
				ImagePullSecrets:             ps.Spec.ImagePullSecrets,
				Containers: []corev1.Container{{
					Name:            "migrate",
					Image:           image, // ships psql
					Command:         []string{"sh", "-c", script},
					Resources:       *resourceReqs("64Mi", "25m", "256Mi"), // scheduler requests (#18, preserved through the CRD swap)
					SecurityContext: hardenedSecurityContext(),             // workload hardening baseline (#18)
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

// pgImageMigrationJobSpec runs spec.migrate: the image's own migration command, with the
// connection URI in spec.migrate.databaseEnv and no schema file (orama-operator#936). Lake's
// `lake migrate` waits for the database itself, takes an advisory lock, and applies its embedded
// schema in one transaction with the same skip rule as the psql script above. Exit codes listed in
// failOnExitCodes fail the Job at once; any other failure is retried up to the backoff limit.
func pgImageMigrationJobSpec(ps *oramav1alpha1.PostgresSchema) batchv1.JobSpec {
	migrate := ps.Spec.Migrate
	securityContext := hardenedSecurityContext()
	securityContext.RunAsNonRoot = boolPtr(true)
	podSecurityContext := &corev1.PodSecurityContext{RunAsNonRoot: boolPtr(true), RunAsUser: migrate.RunAsUser, RunAsGroup: migrate.RunAsGroup}
	spec := batchv1.JobSpec{
		BackoffLimit: int32Ptr(20), // retry while the database is unreachable
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: pgMigrationPodLabels(ps)},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				ServiceAccountName:           ps.Name + "-migrate",
				AutomountServiceAccountToken: boolPtr(false),
				EnableServiceLinks:           boolPtr(false),
				ImagePullSecrets:             ps.Spec.ImagePullSecrets,
				SecurityContext:              podSecurityContext,
				Containers: []corev1.Container{{
					Name:            "migrate",
					Image:           ps.Spec.Image, // the image carries the schema and the command
					Args:            append([]string(nil), migrate.Args...),
					Resources:       *resourceReqs("64Mi", "25m", "512Mi"),
					SecurityContext: securityContext,
					// The JSON report on stdout stays in the pod log; a failure's one-line summary
					// (stderr) becomes the termination message.
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					Env: []corev1.EnvVar{{Name: migrate.DatabaseEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: ps.Spec.ConnectionSecretRef.Name}, Key: ps.Spec.ConnectionSecretRef.Key}}}},
				}},
			},
		},
	}
	if len(migrate.FailOnExitCodes) > 0 {
		spec.PodFailurePolicy = &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
			Action: batchv1.PodFailurePolicyActionFailJob,
			OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
				ContainerName: strPtr("migrate"),
				Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
				Values:        append([]int32(nil), migrate.FailOnExitCodes...),
			},
		}}}
	}
	return spec
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
