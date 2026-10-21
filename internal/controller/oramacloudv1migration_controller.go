/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// OramaCloudV1Migration (operator#653) copies one Prod 1 team into one OramaCloudV1 stack. The
// operator does not copy anything itself: each step runs the cloud-v1-migration image's run-step
// (oramacore-aws#108) as a Job owned by the resource, and the tool's status --json, written to the
// termination message, becomes status.result. The resource is the declared state ("team T is
// copied into stack S, up to step X"); each Job is how the operator reaches it, kept as the record.

const (
	migrationFinalizer = "orama.cloud/migration"
	migrationComponent = "migration"
	// migrationLabel names the migration on its Jobs, their pods and its run volume.
	migrationLabel = "orama.cloud/migration"
	// migrationContainer is the tool's container; its termination message is the tool's status.
	migrationContainer = "migration"
	// migrationWaitContainer waits, before the tool starts, until the migration role can sign in.
	migrationWaitContainer = "wait-for-database"

	migrationRunsDir      = "/runs"
	migrationEvidenceDir  = "/evidence"
	migrationEvidenceFile = "isolation.json"
	// migrationUID is the image's user; fsGroup makes the run volume writable to it.
	migrationUID = int64(10001)
	// migrationRunVolumeSize is the run volume when spec.runVolume.size is unset.
	migrationRunVolumeSize = "20Gi"
	// migrationDeadline bounds one step: a seed of a large team copies every document.
	migrationDeadline = int64(6 * 60 * 60)
	// migrationJobTTL keeps a finished Job, the record of what ran, for a week.
	migrationJobTTL = int32(7 * 24 * 60 * 60)
	// migrationWait is how often a migration waiting on its target, a Secret or an earlier Job
	// checks again.
	migrationWait = 20 * time.Second

	// migrationSeededCondition is True once an applied seed succeeded under this resource. It is set
	// once and never cleared: catchUp and verify need it.
	migrationSeededCondition = "Seeded"
	// migrationTargetWrittenCondition is True once an applied seed or catch-up started, so the
	// target may hold part of the team: deletion with removeOnDelete removes it.
	migrationTargetWrittenCondition = "TargetWritten"
	// migrationRemovedCondition is True once the removal on deletion succeeded. It is set once and
	// never cleared: the removal never runs again, even once its Job is gone.
	migrationRemovedCondition = "Removed"

	// The tool's exit codes (oramacore-aws#108): 0 completed, 3 blocked by a guard or preflight,
	// 1 an unexpected error, 4 verification found a difference.
	migrationExitBlocked    = 3
	migrationExitDifference = 4
)

// The migration role: one CNPG managed role on every stack's control DB, able to sign in only while
// an OramaCloudV1Migration targets the stack (principle 4: the tool never uses the owner).
const (
	migrationRole = "migration"
	// migrationRoleConnectionLimit caps the tool's sessions.
	migrationRoleConnectionLimit = 4
)

// migrationRoleSecretName is the migration role's password Secret, which CNPG reads to set the
// password, and whose uri key is the tool's ORAMA_MIGRATION_PROD2_DSN.
func migrationRoleSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-migration"
}

// OramaCloudV1MigrationReconciler runs a migration's steps as Jobs and reports their outcome.
type OramaCloudV1MigrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads a finished Job's pods uncached: the manager keeps no pod informer.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1migrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1migrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1migrations/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s;postgresschemas,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

// migrationRun is one Job of a migration: a step (or the removal), whether it writes, and its
// attempt. Each distinct run is one Job, run once.
type migrationRun struct {
	// step is the spec.step value, or "remove" for the removal on deletion.
	step    string
	command string
	apply   bool
	attempt int32
	jobName string
}

const migrationRemoveStep = "remove"

// migrationCommands maps a step to run-step's command.
var migrationCommands = map[string]string{
	oramav1alpha1.MigrationStepPlan:    "plan",
	oramav1alpha1.MigrationStepSeed:    "seed",
	oramav1alpha1.MigrationStepCatchUp: "catch-up",
	oramav1alpha1.MigrationStepVerify:  "verify",
	migrationRemoveStep:                "remove-target",
}

// migrationWrites reports whether a step can write to the target: seed and catchUp, with apply.
func migrationWrites(step string) bool {
	return step == oramav1alpha1.MigrationStepSeed || step == oramav1alpha1.MigrationStepCatchUp
}

// migrationRunFor is the run spec.step, spec.apply and spec.attempt ask for. apply counts only for
// seed and catchUp, so toggling it never re-runs plan or verify.
func migrationRunFor(m *oramav1alpha1.OramaCloudV1Migration) migrationRun {
	apply := m.Spec.Apply && migrationWrites(m.Spec.Step)
	command := migrationCommands[m.Spec.Step]
	suffix := command
	if migrationWrites(m.Spec.Step) && !apply {
		suffix += "-dry-run"
	}
	return migrationRun{step: m.Spec.Step, command: command, apply: apply, attempt: m.Spec.Attempt,
		jobName: migrationJobName(m.Name, fmt.Sprintf("-%s-%d", suffix, m.Spec.Attempt))}
}

// migrationRemovalRun is the removal on deletion, retried by raising spec.attempt.
func migrationRemovalRun(m *oramav1alpha1.OramaCloudV1Migration) migrationRun {
	return migrationRun{step: migrationRemoveStep, command: migrationCommands[migrationRemoveStep], apply: true,
		attempt: m.Spec.Attempt, jobName: migrationJobName(m.Name, fmt.Sprintf("-remove-%d", m.Spec.Attempt))}
}

// migrationJobName is <name><suffix> (<name>-seed-1, <name>-catch-up-dry-run-0, <name>-remove-0). A
// Job's name is a pod label value, so it is at most 63 characters: a longer resource name is cut and
// given a hash of the whole name, so two migrations never share a Job.
func migrationJobName(name, suffix string) string {
	if len(name)+len(suffix) <= 63 {
		return name + suffix
	}
	sum := sha256.Sum256([]byte(name))
	hash := "-" + hex.EncodeToString(sum[:])[:6]
	keep := strings.TrimRight(name[:63-len(suffix)-len(hash)], "-.")
	return keep + hash + suffix
}

// migrationRunVolumeName is the PersistentVolumeClaim holding the tool's run directory.
func migrationRunVolumeName(m *oramav1alpha1.OramaCloudV1Migration) string {
	return m.Name + "-runs"
}

// migrationLabels are the labels of a migration's Jobs, pods and run volume: the stack's labels
// with component migration, so the pod is inside the stack's network sandbox and admitted by its
// migration allowances, plus the migration's name.
func migrationLabels(target *oramav1alpha1.OramaCloudV1, m *oramav1alpha1.OramaCloudV1Migration) map[string]string {
	l := labelsFor(target, migrationComponent)
	l[migrationLabel] = m.Name
	return l
}

// Reconcile runs the step the spec asks for once, after the target and every input are ready and
// the step is safe, and records how the tool ended. Deletion removes the copied team when asked,
// then the run volume.
func (r *OramaCloudV1MigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var m oramav1alpha1.OramaCloudV1Migration
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &m)
	}
	if controllerutil.AddFinalizer(&m, migrationFinalizer) {
		if err := r.Update(ctx, &m); err != nil {
			return ctrl.Result{}, err
		}
	}
	status := m.Status.DeepCopy()
	status.ObservedGeneration = m.Generation
	result, err := r.reconcileStep(ctx, &m, status)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.writeStatus(ctx, &m, status, result)
}

// reconcileStep advances the run the spec asks for and fills status.
func (r *OramaCloudV1MigrationReconciler) reconcileStep(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus) (ctrl.Result, error) {
	run := migrationRunFor(m)
	// A run that finished is never started again, even once its Job has expired.
	if status.JobName == run.jobName && migrationFinished(status.Phase) {
		return ctrl.Result{}, nil
	}
	job, err := r.ownedJob(ctx, m, run.jobName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job != nil {
		return migrationRequeue(status), r.followJob(ctx, m, status, run, job)
	}
	if status.JobName == run.jobName && status.Phase == oramav1alpha1.MigrationPhaseRunning {
		migrationSet(status, oramav1alpha1.MigrationPhaseFailed, fmt.Sprintf("Job %s was deleted before it finished; raise spec.attempt to run %s again", run.jobName, run.command))
		return ctrl.Result{}, nil
	}

	target, wait, err := r.prepareRun(ctx, m, run)
	if err != nil || wait != nil {
		if wait != nil {
			migrationSet(status, wait.phase, wait.message)
		}
		return migrationRequeue(status), err
	}
	return ctrl.Result{}, r.startJob(ctx, m, status, run, target)
}

// migrationHold is why a run cannot start yet (Pending) or must not start (Blocked).
type migrationHold struct {
	phase, message string
}

func migrationPending(format string, args ...any) *migrationHold {
	return &migrationHold{oramav1alpha1.MigrationPhasePending, fmt.Sprintf(format, args...)}
}

func migrationBlocked(format string, args ...any) *migrationHold {
	return &migrationHold{oramav1alpha1.MigrationPhaseBlocked, fmt.Sprintf(format, args...)}
}

// migrationTarget is the stack a run goes to, with the revisions it reports to the tool.
type migrationTarget struct {
	oc                 *oramav1alpha1.OramaCloudV1
	deploymentRevision string
	schemaRevision     string
}

// prepareRun checks, in order, that no other Job of this migration is running, that the target is
// ready, that the step is safe for the target and in order, and that every Secret the Job reads
// exists with its key. It returns the hold that stops the run, or the target.
func (r *OramaCloudV1MigrationReconciler) prepareRun(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, run migrationRun) (*migrationTarget, *migrationHold, error) {
	if active, err := r.activeJob(ctx, m, run.jobName); err != nil || active != "" {
		if active != "" {
			return nil, migrationPending("waiting for Job %s to finish before running %s: two runs never share the run directory", active, run.jobName), nil
		}
		return nil, nil, err
	}
	target, hold, err := r.readyTarget(ctx, m)
	if err != nil || hold != nil {
		return nil, hold, err
	}
	if hold := migrationUnsafeTarget(target.oc, run); hold != nil {
		return nil, hold, nil
	}
	if hold := migrationOutOfOrder(m, run); hold != nil {
		return nil, hold, nil
	}
	if hold, err := r.missingSecret(ctx, m, target.oc, run); err != nil || hold != nil {
		return nil, hold, err
	}
	return target, nil, nil
}

// readyTarget waits for the target to exist, be Ready, have its cluster row, and have every control
// DB schema current: the migration role's grants are the last of them.
func (r *OramaCloudV1MigrationReconciler) readyTarget(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration) (*migrationTarget, *migrationHold, error) {
	oc := &oramav1alpha1.OramaCloudV1{}
	err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.TargetRef}, oc)
	switch {
	case apierrors.IsNotFound(err):
		return nil, migrationPending("waiting for OramaCloudV1 %s/%s", m.Namespace, m.Spec.TargetRef), nil
	case err != nil:
		return nil, nil, err
	}
	if !oc.DeletionTimestamp.IsZero() {
		return nil, migrationPending("OramaCloudV1 %s is being deleted", oc.Name), nil
	}
	if oc.Status.Phase != "Ready" {
		return nil, migrationPending("waiting for OramaCloudV1 %s to be Ready (it is %q)", oc.Name, oc.Status.Phase), nil
	}
	if row := meta.FindStatusCondition(oc.Status.Conditions, clusterRowReadyCondition); row == nil || row.Status != metav1.ConditionTrue {
		reason := "not reported"
		if row != nil {
			reason = row.Reason
		}
		return nil, migrationPending("waiting for OramaCloudV1 %s's %s condition (%s)", oc.Name, clusterRowReadyCondition, reason), nil
	}
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		return nil, nil, err
	}
	revision := ""
	for _, s := range schemas {
		ps := &oramav1alpha1.PostgresSchema{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + s.suffix}, ps); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, migrationPending("waiting for PostgresSchema %s%s", oc.Name, s.suffix), nil
			}
			return nil, nil, err
		}
		if !postgresSchemaCurrent(ps) {
			return nil, migrationPending("waiting for PostgresSchema %s to apply its current version", ps.Name), nil
		}
		// The schema revision is the last migration that creates tables; the reference rows and the
		// grants are applied again on their own changes and say nothing about the schema.
		if !s.everyVersion {
			revision = ps.Status.AppliedVersion
		}
	}
	return &migrationTarget{oc: oc, deploymentRevision: imageRevision(engineSideFor(oc, "writer").image), schemaRevision: revision}, nil, nil
}

// imageRevision is an image reference's tag (oramasearch/oramacore-cloud-v1:v1.2.39-ready-9d8ed373
// → v1.2.39-ready-9d8ed373), or its digest when it has no tag.
func imageRevision(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		return name[i+1:]
	}
	if digest != "" {
		return digest
	}
	return "latest"
}

// migrationUnsafeTarget refuses a writing step against a stack that would act on the copied team
// before its cutover: its dashboard schedules or its analytics import must be suspended.
func migrationUnsafeTarget(oc *oramav1alpha1.OramaCloudV1, run migrationRun) *migrationHold {
	if !run.apply || !migrationWrites(run.step) {
		return nil
	}
	if !suspended(oc.Spec.Components.Dashboard.Crons.Suspend) {
		return migrationBlocked("OramaCloudV1 %s has spec.components.dashboard.crons.suspend false: a copied team must not run schedules before its cutover", oc.Name)
	}
	if imp := oc.Spec.Components.AnalyticsProcessor.Import; imp.Enabled && !imp.Suspend {
		return migrationBlocked("OramaCloudV1 %s has spec.components.analyticsProcessor.import.suspend false: a copied team must not feed analytics before its cutover", oc.Name)
	}
	return nil
}

// migrationOutOfOrder refuses catchUp and verify before an applied seed succeeded here, and seed
// and catchUp without the isolation receipt the tool requires.
func migrationOutOfOrder(m *oramav1alpha1.OramaCloudV1Migration, run migrationRun) *migrationHold {
	switch run.step {
	case oramav1alpha1.MigrationStepCatchUp, oramav1alpha1.MigrationStepVerify:
		if !meta.IsStatusConditionTrue(m.Status.Conditions, migrationSeededCondition) {
			return migrationBlocked("%s needs an applied seed that succeeded under this migration: run step seed with apply first", run.step)
		}
	}
	if migrationWrites(run.step) && m.Spec.EvidenceSecretRef == nil {
		return migrationBlocked("%s needs spec.evidenceSecretRef, the reviewed isolation receipt", run.step)
	}
	return nil
}

// migrationSecretKeys are the Secrets and keys a run's pod reads. The pull secret is checked by
// name only.
func migrationSecretKeys(m *oramav1alpha1.OramaCloudV1Migration, oc *oramav1alpha1.OramaCloudV1, run migrationRun) []corev1.SecretKeySelector {
	src := m.Spec.Source
	sel := func(name, key string) corev1.SecretKeySelector {
		return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
	}
	masterName, masterKey := masterAPIKeyRef(oc)
	keys := []corev1.SecretKeySelector{
		sel(src.DatabaseSecretRef.Name, src.DatabaseSecretRef.Key),
		sel(src.MasterKeySecretRef.Name, src.MasterKeySecretRef.Key),
		sel(src.Settings.CredentialsSecretRef.Name, "access_key_id"),
		sel(src.Settings.CredentialsSecretRef.Name, "secret_access_key"),
		sel(migrationRoleSecretName(oc), "uri"),
		sel(migrationRoleSecretName(oc), "password"),
		sel(masterName, masterKey),
	}
	if migrationUsesEvidence(m, run) {
		keys = append(keys, sel(m.Spec.EvidenceSecretRef.Name, m.Spec.EvidenceSecretRef.Key))
	}
	if m.Spec.PullSecretName != "" {
		keys = append(keys, sel(m.Spec.PullSecretName, ""))
	}
	return keys
}

// migrationUsesEvidence reports whether a run mounts the isolation receipt: seed and catchUp.
func migrationUsesEvidence(m *oramav1alpha1.OramaCloudV1Migration, run migrationRun) bool {
	return migrationWrites(run.step) && m.Spec.EvidenceSecretRef != nil
}

// missingSecret names the first Secret or key a run's pod reads that does not exist. Values are
// never read into status, events or logs: only the keys' presence is checked.
func (r *OramaCloudV1MigrationReconciler) missingSecret(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, oc *oramav1alpha1.OramaCloudV1, run migrationRun) (*migrationHold, error) {
	for _, ref := range migrationSecretKeys(m, oc, run) {
		sec := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: ref.Name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				return migrationPending("waiting for Secret %s/%s", m.Namespace, ref.Name), nil
			}
			return nil, err
		}
		if _, ok := sec.Data[ref.Key]; ref.Key != "" && !ok {
			return migrationPending("waiting for Secret %s/%s to have key %s", m.Namespace, ref.Name, ref.Key), nil
		}
	}
	return nil, nil
}

// startJob ensures the run volume and creates the run's Job, recording it as Running.
func (r *OramaCloudV1MigrationReconciler) startJob(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus, run migrationRun, target *migrationTarget) error {
	if err := r.ensureRunVolume(ctx, m, target.oc); err != nil {
		return err
	}
	job := migrationJob(m, target, run)
	if err := controllerutil.SetControllerReference(m, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil {
		return err
	}
	markTargetWritten(m, status, run)
	started := job.CreationTimestamp
	if started.IsZero() {
		started = metav1.Now()
	}
	status.Step, status.Attempt, status.Apply, status.JobName = run.step, run.attempt, run.apply, run.jobName
	status.Result, status.CompletedAt, status.StartedAt = nil, nil, &started
	migrationSet(status, migrationRunningPhase(run), fmt.Sprintf("Job %s runs run-step %s", run.jobName, migrationArgs(run)))
	return nil
}

// migrationRunningPhase is Removing for the removal, Running for a step.
func migrationRunningPhase(run migrationRun) string {
	if run.step == migrationRemoveStep {
		return oramav1alpha1.MigrationPhaseRemoving
	}
	return oramav1alpha1.MigrationPhaseRunning
}

// ensureRunVolume creates the run volume once, ReadWriteOnce, owned by the migration, and grows it
// when spec.runVolume.size grows. It is kept across steps: the tool resumes from it.
func (r *OramaCloudV1MigrationReconciler) ensureRunVolume(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, oc *oramav1alpha1.OramaCloudV1) error {
	want := volumeSize(&m.Spec.RunVolume, migrationRunVolumeSize)
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: migrationRunVolumeName(m)}, pvc)
	if apierrors.IsNotFound(err) {
		claim := dataPVC(&m.Spec.RunVolume, migrationRunVolumeSize)
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: migrationRunVolumeName(m), Namespace: m.Namespace, Labels: migrationLabels(oc, m)},
			Spec:       claim.Spec,
		}
		if err := controllerutil.SetControllerReference(m, pvc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, pvc)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(pvc, m) {
		return fmt.Errorf("PersistentVolumeClaim %s/%s is not controlled by OramaCloudV1Migration %s", pvc.Namespace, pvc.Name, m.Name)
	}
	if current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; current.Cmp(want) < 0 {
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		return r.Patch(ctx, pvc, patch)
	}
	return nil
}

// migrationArgs is run-step's arguments: the command, and --apply for a writing run.
func migrationArgs(run migrationRun) string {
	if run.apply {
		return run.command + " --apply"
	}
	return run.command
}

// migrationJob renders a run's Job. Every credential reaches the pod through a secretKeyRef or a
// read-only Secret volume; the spec holds no value. backoffLimit is 0: the tool resumes from its
// manifest, and a failed copy needs a person to read it, not an automatic retry.
func migrationJob(m *oramav1alpha1.OramaCloudV1Migration, target *migrationTarget, run migrationRun) *batchv1.Job {
	oc := target.oc
	lbls := migrationLabels(oc, m)
	args := []string{run.command}
	if run.apply {
		args = append(args, "--apply")
	}
	volumes := []corev1.Volume{
		{Name: "runs", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: migrationRunVolumeName(m)}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "runs", MountPath: migrationRunsDir},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if migrationUsesEvidence(m, run) {
		volumes = append(volumes, corev1.Volume{Name: "evidence", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  m.Spec.EvidenceSecretRef.Name,
			Items:       []corev1.KeyToPath{{Key: m.Spec.EvidenceSecretRef.Key, Path: migrationEvidenceFile}},
			DefaultMode: int32Ptr(0o400),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "evidence", MountPath: migrationEvidenceDir, ReadOnly: true})
	}
	security := hardenedSecurityContext()
	security.ReadOnlyRootFilesystem = boolPtr(true)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: run.jobName, Namespace: m.Namespace, Labels: lbls},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(migrationDeadline),
			TTLSecondsAfterFinished: int32Ptr(migrationJobTTL),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					ImagePullSecrets:             migrationPullSecrets(m, oc),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						RunAsUser:      int64Ptr(migrationUID),
						RunAsGroup:     int64Ptr(migrationUID),
						FSGroup:        int64Ptr(migrationUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// CNPG turns the migration role's login on only after the stack reconciles this
					// migration, so the tool starts once the role can sign in, not before.
					InitContainers: []corev1.Container{{
						Name:            migrationWaitContainer,
						Image:           postgresClientImage(oc),
						Command:         []string{"sh", "-c", migrationWaitScript},
						Env:             migrationRoleEnv(oc),
						VolumeMounts:    []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
						Resources:       *resourceReqs("64Mi", "25m", "256Mi"),
						SecurityContext: security.DeepCopy(),
					}},
					Containers: []corev1.Container{{
						Name:                     migrationContainer,
						Image:                    m.Spec.Image,
						Command:                  []string{"run-step"},
						Args:                     args,
						Env:                      migrationEnv(m, target, run),
						VolumeMounts:             mounts,
						Resources:                *resourceReqs("512Mi", "250m", "2Gi"),
						SecurityContext:          security,
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// migrationWaitScript waits up to ten minutes for the migration role to sign in. psql takes the
// connection from PG* variables, so no credential is in its argv.
const migrationWaitScript = `set -eu
for i in $(seq 1 200); do
  if psql -X -q -tA -c 'SELECT 1' >/dev/null 2>&1; then exit 0; fi
  sleep 3
done
echo "wait-for-database: the migration role could not sign in to $PGHOST for ten minutes" >&2
exit 1`

// migrationRoleEnv is the migration role's connection as libpq's variables, for the wait.
func migrationRoleEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: oc.Name + "-controldb-rw"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGDATABASE", Value: controlDBName},
		{Name: "PGUSER", Value: migrationRole},
		secretKeyEnv("PGPASSWORD", migrationRoleSecretName(oc), "password"),
		{Name: "PGCONNECT_TIMEOUT", Value: "5"},
	}
}

// migrationPullSecrets are spec.pullSecretName for the tool's image and the stack's pull secret for
// its postgres-client image, each once.
func migrationPullSecrets(m *oramav1alpha1.OramaCloudV1Migration, oc *oramav1alpha1.OramaCloudV1) []corev1.LocalObjectReference {
	var names []string
	for _, n := range []string{m.Spec.PullSecretName, oc.Spec.Images.PullSecretName} {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var refs []corev1.LocalObjectReference
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return refs
}

// migrationEnv is the tool's environment, a contract with run-step (oramacore-aws#108). Only the
// scope, paths, URLs, revisions and SSM location are literal; every credential is a secretKeyRef.
// ORAMA_MIGRATION_PROD2_CLUSTER_ID is left unset: the tool reads the target's c1 row itself.
func migrationEnv(m *oramav1alpha1.OramaCloudV1Migration, target *migrationTarget, run migrationRun) []corev1.EnvVar {
	src := m.Spec.Source
	oc := target.oc
	projects := slices.Clone(m.Spec.ProjectIDs)
	sort.Strings(projects)
	masterName, masterKey := masterAPIKeyRef(oc)
	env := []corev1.EnvVar{
		{Name: "TEAM_ID", Value: m.Spec.TeamID},
		{Name: "RUN_ID", Value: m.Spec.RunID},
		{Name: "PROJECT_IDS", Value: strings.Join(projects, ",")},
		{Name: "RUNS_DIR", Value: migrationRunsDir},
	}
	if migrationUsesEvidence(m, run) {
		env = append(env, corev1.EnvVar{Name: "EVIDENCE_PATH", Value: migrationEvidenceDir + "/" + migrationEvidenceFile})
	}
	return append(env,
		secretKeyEnv("ORAMA_MIGRATION_PROD1_DSN", src.DatabaseSecretRef.Name, src.DatabaseSecretRef.Key),
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_WRITER_URL", Value: src.WriterURL},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_READER_URL", Value: src.ReaderURL},
		secretKeyEnv("ORAMA_MIGRATION_PROD1_MASTER_KEY", src.MasterKeySecretRef.Name, src.MasterKeySecretRef.Key),
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_DEPLOYMENT_REVISION", Value: src.DeploymentRevision},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_SCHEMA_REVISION", Value: src.SchemaRevision},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_SETTINGS_SSM_REGION", Value: src.Settings.Region},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_SETTINGS_SSM_INSTANCE_ID", Value: src.Settings.InstanceID},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_SETTINGS_SSM_DOCUMENT", Value: src.Settings.Document},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD1_SETTINGS_DATA_DIR", Value: src.Settings.DataDir},
		secretKeyEnv("ORAMA_MIGRATION_PROD1_SETTINGS_AWS_ACCESS_KEY_ID", src.Settings.CredentialsSecretRef.Name, "access_key_id"),
		secretKeyEnv("ORAMA_MIGRATION_PROD1_SETTINGS_AWS_SECRET_ACCESS_KEY", src.Settings.CredentialsSecretRef.Name, "secret_access_key"),
		secretKeyEnv("ORAMA_MIGRATION_PROD2_DSN", migrationRoleSecretName(oc), "uri"),
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD2_WRITER_URL", Value: fmt.Sprintf("http://%s-writer:%d", oc.Name, oramacorePort)},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD2_READER_URL", Value: fmt.Sprintf("http://%s-reader", oc.Name)},
		secretKeyEnv("ORAMA_MIGRATION_PROD2_MASTER_KEY", masterName, masterKey),
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD2_DEPLOYMENT_REVISION", Value: target.deploymentRevision},
		corev1.EnvVar{Name: "ORAMA_MIGRATION_PROD2_SCHEMA_REVISION", Value: target.schemaRevision},
	)
}

// ownedJob returns the named Job of this migration, nil when there is none. A Job of that name
// controlled by anything else is an error: the run never adopts it.
func (r *OramaCloudV1MigrationReconciler) ownedJob(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, name string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: name}, job); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(job, m) {
		return nil, fmt.Errorf("Job %s/%s is not controlled by OramaCloudV1Migration %s", m.Namespace, name, m.Name)
	}
	return job, nil
}

// activeJob names another of this migration's Jobs that has not finished, or "".
func (r *OramaCloudV1MigrationReconciler) activeJob(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, except string) (string, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels{migrationLabel: m.Name}); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == except || !metav1.IsControlledBy(job, m) {
			continue
		}
		if finished, _ := lakeE2EJobFinished(job); !finished {
			return job.Name, nil
		}
	}
	return "", nil
}

// followJob records a run's Job: Running (or Removing) until it finishes, then its outcome. An
// applied seed that succeeded sets the Seeded condition, a removal that succeeded Removed.
func (r *OramaCloudV1MigrationReconciler) followJob(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus, run migrationRun, job *batchv1.Job) error {
	if status.JobName != run.jobName {
		status.Result, status.StartedAt, status.CompletedAt = nil, nil, nil
	}
	status.Step, status.Attempt, status.Apply, status.JobName = run.step, run.attempt, run.apply, run.jobName
	if status.StartedAt == nil {
		started := job.CreationTimestamp
		status.StartedAt = &started
	}
	// Also here: the status write that followed the Job's creation may have been lost.
	markTargetWritten(m, status, run)
	finished, _ := lakeE2EJobFinished(job)
	if !finished {
		migrationSet(status, migrationRunningPhase(run), fmt.Sprintf("Job %s runs run-step %s", run.jobName, migrationArgs(run)))
		return nil
	}
	outcome, err := r.jobOutcome(ctx, job, run)
	if err != nil {
		return err
	}
	if status.CompletedAt == nil {
		completed := migrationCompletedAt(job)
		status.CompletedAt = &completed
	}
	status.Result = outcome.result
	migrationSet(status, outcome.phase, outcome.message)
	if outcome.phase == oramav1alpha1.MigrationPhaseSucceeded && run.step == oramav1alpha1.MigrationStepSeed && run.apply {
		markOnce(status, metav1.Condition{Type: migrationSeededCondition, Status: metav1.ConditionTrue,
			Reason: "SeedApplied", Message: fmt.Sprintf("Job %s applied the seed", run.jobName), ObservedGeneration: m.Generation})
	}
	if outcome.phase == oramav1alpha1.MigrationPhaseSucceeded && run.step == migrationRemoveStep {
		markOnce(status, metav1.Condition{Type: migrationRemovedCondition, Status: metav1.ConditionTrue,
			Reason: "TeamRemoved", Message: fmt.Sprintf("Job %s removed the team from OramaCloudV1 %s", run.jobName, m.Spec.TargetRef), ObservedGeneration: m.Generation})
	}
	return nil
}

// markTargetWritten records that a writing run started: the target may hold part of the team.
func markTargetWritten(m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus, run migrationRun) {
	if run.apply && migrationWrites(run.step) {
		markOnce(status, metav1.Condition{Type: migrationTargetWrittenCondition, Status: metav1.ConditionTrue,
			Reason: "ApplyStarted", Message: fmt.Sprintf("Job %s writes to OramaCloudV1 %s", run.jobName, m.Spec.TargetRef), ObservedGeneration: m.Generation})
	}
}

// markOnce sets a condition that, once True, is never changed again: it records the first time.
func markOnce(status *oramav1alpha1.OramaCloudV1MigrationStatus, c metav1.Condition) {
	if !meta.IsStatusConditionTrue(status.Conditions, c.Type) {
		meta.SetStatusCondition(&status.Conditions, c)
	}
}

// migrationCompletedAt is when a finished Job ended: its completion time, or its Failed condition.
func migrationCompletedAt(job *batchv1.Job) metav1.Time {
	if job.Status.CompletionTime != nil {
		return *job.Status.CompletionTime
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime
		}
	}
	return job.CreationTimestamp
}

// migrationOutcome is how a finished run ended.
type migrationOutcome struct {
	phase, message string
	result         *apiextensionsv1.JSON
}

// jobOutcome reads a finished Job's pod: the tool's exit code decides the phase (0 Succeeded,
// 3 Blocked, anything else Failed), and its termination message, the status --json, is copied to
// status.result when it is JSON. A Job past its deadline is Failed, whatever its pod says.
func (r *OramaCloudV1MigrationReconciler) jobOutcome(ctx context.Context, job *batchv1.Job, run migrationRun) (migrationOutcome, error) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == batchv1.JobReasonDeadlineExceeded {
			return migrationOutcome{phase: oramav1alpha1.MigrationPhaseFailed,
				message: fmt.Sprintf("Job %s ran past its %dh deadline and was stopped; raise spec.attempt to resume %s", job.Name, migrationDeadline/3600, run.command)}, nil
		}
	}
	pod, err := r.newestPod(ctx, job)
	if err != nil {
		return migrationOutcome{}, err
	}
	if pod != nil {
		for _, c := range pod.Status.InitContainerStatuses {
			if c.Name == migrationWaitContainer && c.State.Terminated != nil && c.State.Terminated.ExitCode != 0 {
				return migrationOutcome{phase: oramav1alpha1.MigrationPhaseFailed,
					message: fmt.Sprintf("Job %s: the migration role could not sign in to the control DB, so %s never started; see its %s log", job.Name, run.command, migrationWaitContainer)}, nil
			}
		}
		for _, c := range pod.Status.ContainerStatuses {
			if c.Name == migrationContainer && c.State.Terminated != nil {
				return migrationExitOutcome(job.Name, run, c.State.Terminated), nil
			}
		}
	}
	if job.Status.Succeeded > 0 {
		return migrationOutcome{phase: oramav1alpha1.MigrationPhaseSucceeded, message: fmt.Sprintf("Job %s completed %s; its pod is gone, so no result was read", job.Name, migrationArgs(run))}, nil
	}
	return migrationOutcome{phase: oramav1alpha1.MigrationPhaseFailed, message: fmt.Sprintf("Job %s failed and left no exit code; see its pod log", job.Name)}, nil
}

// migrationExitOutcome maps the tool's exit code and termination message to an outcome.
func migrationExitOutcome(jobName string, run migrationRun, t *corev1.ContainerStateTerminated) migrationOutcome {
	out := migrationOutcome{result: migrationResult(t.Message)}
	what := "run-step " + migrationArgs(run)
	switch t.ExitCode {
	case 0:
		out.phase, out.message = oramav1alpha1.MigrationPhaseSucceeded, fmt.Sprintf("Job %s: %s completed", jobName, what)
	case migrationExitBlocked:
		out.phase, out.message = oramav1alpha1.MigrationPhaseBlocked, fmt.Sprintf("Job %s: %s was refused by the tool's guard or preflight (exit 3); its log says why. Fix the cause, then raise spec.attempt", jobName, what)
	case migrationExitDifference:
		out.phase, out.message = oramav1alpha1.MigrationPhaseFailed, fmt.Sprintf("Job %s: %s found a difference between Prod 1 and the target (exit 4)", jobName, what)
	default:
		out.phase, out.message = oramav1alpha1.MigrationPhaseFailed, fmt.Sprintf("Job %s: %s failed with exit code %d; see its pod log", jobName, what, t.ExitCode)
	}
	if out.result == nil {
		out.message += "; it left no status --json"
	}
	return out
}

// migrationResult is the tool's status --json as the API server stores it: the same object, its
// keys sorted and its whitespace dropped, numbers as written. Stored that way, the result read back
// equals the one rendered, so an unchanged run writes no status. nil when the message is not a JSON
// object (the tool crashed before writing it).
func migrationResult(message string) *apiextensionsv1.JSON {
	dec := json.NewDecoder(strings.NewReader(message))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil || object == nil || dec.More() {
		return nil
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return nil
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// newestPod is the Job's most recent pod, read uncached, or nil when it has none. A failed read is
// an error, so a finished run is never recorded without its exit code because of it.
func (r *OramaCloudV1MigrationReconciler) newestPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		return nil, fmt.Errorf("list the pods of Job %s: %w", job.Name, err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	return &pods.Items[0], nil
}

// reconcileDelete removes the copied team when spec.removeOnDelete asks and the target may hold
// part of it, then deletes the finished Jobs and their pods, then the run volume, which holds
// customer payloads, then lets the resource go. A removal the tool refuses (exit 3: the team was
// switched to the target) or that fails keeps the finalizer: a switched customer is never deleted
// by deleting a resource, and a person decides.
func (r *OramaCloudV1MigrationReconciler) reconcileDelete(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(m, migrationFinalizer) {
		return ctrl.Result{}, nil
	}
	status := m.Status.DeepCopy()
	status.ObservedGeneration = m.Generation
	if m.Spec.RemoveOnDelete && meta.IsStatusConditionTrue(m.Status.Conditions, migrationTargetWrittenCondition) {
		done, result, err := r.reconcileRemoval(ctx, m, status)
		if err != nil || !done {
			if err != nil {
				return ctrl.Result{}, err
			}
			return r.writeStatus(ctx, m, status, result)
		}
		// Removed is stored before the removal's Job, the other record of it, is deleted.
		if _, err := r.writeStatus(ctx, m, status, ctrl.Result{}); err != nil {
			return ctrl.Result{}, err
		}
	} else if active, err := r.activeJob(ctx, m, ""); err != nil || active != "" {
		if err != nil {
			return ctrl.Result{}, err
		}
		migrationSet(status, oramav1alpha1.MigrationPhaseRemoving, fmt.Sprintf("waiting for Job %s to finish before deleting the run volume", active))
		return r.writeStatus(ctx, m, status, ctrl.Result{RequeueAfter: migrationWait})
	}

	if left, err := r.deleteJobs(ctx, m); err != nil || left != "" {
		if err != nil {
			return ctrl.Result{}, err
		}
		migrationSet(status, oramav1alpha1.MigrationPhaseRemoving, fmt.Sprintf("deleting the finished Jobs before the run volume %s: pod %s is still there", migrationRunVolumeName(m), left))
		return r.writeStatus(ctx, m, status, ctrl.Result{RequeueAfter: 5 * time.Second})
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: migrationRunVolumeName(m)}, pvc)
	switch {
	case err == nil && metav1.IsControlledBy(pvc, m):
		if pvc.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pvc); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		migrationSet(status, oramav1alpha1.MigrationPhaseRemoving, fmt.Sprintf("deleting the run volume %s", pvc.Name))
		return r.writeStatus(ctx, m, status, ctrl.Result{RequeueAfter: 5 * time.Second})
	case err != nil && !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(m, migrationFinalizer)
	return ctrl.Result{}, r.Update(ctx, m)
}

// deleteJobs deletes this migration's Jobs, which have all finished, and waits for their pods:
// pvc-protection keeps the run volume while any pod that mounts it exists, finished or not, and a
// finished Job keeps its pod for its TTL. It names a pod still there, or "" once none is.
func (r *OramaCloudV1MigrationReconciler) deleteJobs(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration) (string, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels{migrationLabel: m.Name}); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, m) || !job.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return "", err
		}
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(m.Namespace), client.MatchingLabels{migrationLabel: m.Name}); err != nil {
		return "", fmt.Errorf("list the pods of OramaCloudV1Migration %s: %w", m.Name, err)
	}
	if len(pods.Items) > 0 {
		return pods.Items[0].Name, nil
	}
	return "", nil
}

// reconcileRemoval runs remove-target --apply once per attempt, until one succeeds. done is true
// once the team is removed (Removed, whatever spec.attempt says since), or when the target no
// longer exists (its control DB went with it).
func (r *OramaCloudV1MigrationReconciler) reconcileRemoval(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus) (bool, ctrl.Result, error) {
	if meta.IsStatusConditionTrue(status.Conditions, migrationRemovedCondition) {
		return true, ctrl.Result{}, nil
	}
	run := migrationRemovalRun(m)
	if status.JobName == run.jobName && migrationFinished(status.Phase) {
		return status.Phase == oramav1alpha1.MigrationPhaseSucceeded, ctrl.Result{}, nil
	}
	job, err := r.ownedJob(ctx, m, run.jobName)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if job != nil {
		if err := r.followJob(ctx, m, status, run, job); err != nil {
			return false, ctrl.Result{}, err
		}
		return status.Phase == oramav1alpha1.MigrationPhaseSucceeded, migrationRequeue(status), nil
	}
	if active, err := r.activeJob(ctx, m, run.jobName); err != nil || active != "" {
		if active != "" {
			migrationSet(status, oramav1alpha1.MigrationPhaseRemoving, fmt.Sprintf("waiting for Job %s to finish before removing the team from the target", active))
		}
		return false, ctrl.Result{RequeueAfter: migrationWait}, err
	}
	// A stack that is gone or going takes its control DB, and the copied team, with it.
	oc := &oramav1alpha1.OramaCloudV1{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.TargetRef}, oc); apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, nil
	} else if err != nil {
		return false, ctrl.Result{}, err
	}
	if !oc.DeletionTimestamp.IsZero() {
		return true, ctrl.Result{}, nil
	}
	target, hold, err := r.readyTarget(ctx, m)
	if err == nil && hold == nil {
		hold, err = r.missingSecret(ctx, m, target.oc, run)
	}
	if err != nil || hold != nil {
		if hold != nil {
			migrationSet(status, oramav1alpha1.MigrationPhaseRemoving, hold.message+", to remove the team from the target")
		}
		return false, ctrl.Result{RequeueAfter: migrationWait}, err
	}
	return false, ctrl.Result{RequeueAfter: migrationWait}, r.startJob(ctx, m, status, run, target)
}

// migrationFinished reports whether a phase ends a run.
func migrationFinished(phase string) bool {
	switch phase {
	case oramav1alpha1.MigrationPhaseSucceeded, oramav1alpha1.MigrationPhaseBlocked, oramav1alpha1.MigrationPhaseFailed:
		return true
	}
	return false
}

// migrationRequeue checks a waiting migration again later. A running Job's events, a target's
// changes and a spec change reconcile it; a Blocked or finished run waits for one of those.
func migrationRequeue(status *oramav1alpha1.OramaCloudV1MigrationStatus) ctrl.Result {
	if status.Phase == oramav1alpha1.MigrationPhasePending || status.Phase == oramav1alpha1.MigrationPhaseRemoving {
		return ctrl.Result{RequeueAfter: migrationWait}
	}
	return ctrl.Result{}
}

func migrationSet(status *oramav1alpha1.OramaCloudV1MigrationStatus, phase, message string) {
	status.Phase, status.Message = phase, message
}

// writeStatus writes the status subresource only when it changed (principle 7).
func (r *OramaCloudV1MigrationReconciler) writeStatus(ctx context.Context, m *oramav1alpha1.OramaCloudV1Migration, status *oramav1alpha1.OramaCloudV1MigrationStatus, result ctrl.Result) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&m.Status, status) {
		return result, nil
	}
	m.Status = *status
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// requestsForTarget maps an OramaCloudV1 to the migrations that target it, so a stack becoming
// Ready, or suspending its schedules, starts a waiting run at once.
func (r *OramaCloudV1MigrationReconciler) requestsForTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	var list oramav1alpha1.OramaCloudV1MigrationList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "list OramaCloudV1Migrations for target", "target", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.TargetRef == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return requests
}

// SetupWithManager registers the migration, its Jobs and run volume, and its target.
func (r *OramaCloudV1MigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1Migration{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&oramav1alpha1.OramaCloudV1{}, handler.EnqueueRequestsFromMapFunc(r.requestsForTarget)).
		Complete(r)
}

// targetingMigrations lists the OramaCloudV1Migrations in the stack's namespace that target it,
// including ones being deleted: their removal still needs the role and the network paths.
func targetingMigrations(ctx context.Context, c client.Reader, oc *oramav1alpha1.OramaCloudV1) ([]oramav1alpha1.OramaCloudV1Migration, error) {
	var list oramav1alpha1.OramaCloudV1MigrationList
	if err := c.List(ctx, &list, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list OramaCloudV1Migrations: %w", err)
	}
	var out []oramav1alpha1.OramaCloudV1Migration
	for _, m := range list.Items {
		if m.Spec.TargetRef == oc.Name {
			out = append(out, m)
		}
	}
	return out, nil
}

// requestsForMigration maps a migration to its target stack, which turns the migration role's login
// and the Prod 1 egress on and off with the migrations that target it.
func (r *OramaCloudV1Reconciler) requestsForMigration(_ context.Context, obj client.Object) []reconcile.Request {
	m, ok := obj.(*oramav1alpha1.OramaCloudV1Migration)
	if !ok || m.Spec.TargetRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.TargetRef}}}
}

// migrationProd1Hosts is the sorted union of the Prod 1 destinations of the migrations, each as
// host:port: the database on 5432, the engine origins and the region's SSM endpoint on 443.
func migrationProd1Hosts(migrations []oramav1alpha1.OramaCloudV1Migration) []string {
	var hosts []string
	add := func(host string, port int) {
		if host == "" {
			return
		}
		if h := host + ":" + strconv.Itoa(port); !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	for _, m := range migrations {
		src := m.Spec.Source
		add(src.DatabaseHost, 5432)
		add(strings.TrimPrefix(src.WriterURL, "https://"), 443)
		add(strings.TrimPrefix(src.ReaderURL, "https://"), 443)
		if src.Settings.Region != "" {
			add("ssm."+src.Settings.Region+".amazonaws.com", 443)
		}
	}
	sort.Strings(hosts)
	return hosts
}
