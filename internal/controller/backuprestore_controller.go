/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// BackupRestore restores one Backup into a datastore of the same kind and verifies it against the
// backup's manifest (operator#741, docs/backups.md "Restore"). Like a Backup it runs a Job from the
// operator's own image (`/manager restore ...`), restricted and without Kubernetes credentials, next to
// a copy of the bucket's scoped credentials and the run's network policies, all owned by the restore
// and removed when it finishes; or, for a kind whose own operator restores (restoreDrivers), that
// operator's resources. It refuses to restore into the datastore the backup was taken of unless
// spec.replaceSource says so.

const (
	backupRestoreLabel      = "orama.cloud/backup-restore"
	backupRestoreComplete   = "Complete"
	backupRestoreJobSuffix  = "-restore"
	backupRestoreJobNameMax = 56
)

// runOutcome is where a run a driver advances is.
type runOutcome struct {
	done, succeeded, verified bool
	reason, message, evidence string
	// requeue is when to look again while it is not done; zero means on the next event.
	requeue time.Duration
}

// restoreDriver restores a kind through its own operator's resources (CloudNativePG, Medusa, TiDB
// Operator), instead of a Job of the operator's image.
type restoreDriver interface {
	restore(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup, bucket *corev1.Secret) (runOutcome, error)
	// cleanup removes what a scratch restore created or restored, once it has finished.
	cleanup(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore) error
}

// restoreDrivers are the kinds restored through their own operator.
var restoreDrivers = map[oramav1alpha1.BackupDatastoreKind]restoreDriver{}

// BackupRestoreReconciler reconciles BackupRestore.
type BackupRestoreReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	APIReader         client.Reader
	OperatorNamespace string
	// Image is the operator's own image; empty means read it from the manager pod.
	Image string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=backuprestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=backuprestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=backups,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *BackupRestoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BackupRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rs oramav1alpha1.BackupRestore
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if rs.Status.Phase == oramav1alpha1.BackupSucceeded || rs.Status.Phase == oramav1alpha1.BackupFailed {
		if err := cleanupRunResources(ctx, r.Client, rs.Namespace, restoreJobName(rs.Name)); err != nil {
			return ctrl.Result{}, err
		}
		if driver, ok := restoreDrivers[rs.Spec.Target.Kind]; ok && rs.Spec.Scratch {
			return ctrl.Result{}, driver.cleanup(ctx, r, &rs)
		}
		return ctrl.Result{}, nil
	}
	desired := *rs.Status.DeepCopy()
	result, err := r.run(ctx, &rs, &desired)
	if !apiequality.Semantic.DeepEqual(rs.Status, desired) {
		rs.Status = desired
		if writeErr := r.Status().Update(ctx, &rs); writeErr != nil {
			return ctrl.Result{}, writeErr
		}
	}
	return settle(result, err)
}

func (r *BackupRestoreReconciler) run(ctx context.Context, rs *oramav1alpha1.BackupRestore, desired *oramav1alpha1.BackupRestoreStatus) (ctrl.Result, error) {
	var b oramav1alpha1.Backup
	if err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.BackupName}, &b); err != nil {
		if apierrors.IsNotFound(err) {
			r.wait(rs, desired, "BackupNotFound", fmt.Sprintf("Backup %s does not exist in %s", rs.Spec.BackupName, rs.Namespace))
			return ctrl.Result{RequeueAfter: backupWaitPoll}, nil
		}
		return ctrl.Result{}, err
	}
	switch b.Status.Phase {
	case oramav1alpha1.BackupSucceeded:
	case oramav1alpha1.BackupFailed:
		r.finish(rs, desired, runOutcome{reason: "BackupNotSucceeded", message: fmt.Sprintf("Backup %s failed: %s", b.Name, b.Status.Message)})
		return ctrl.Result{}, nil
	default:
		r.wait(rs, desired, "BackupNotFinished", fmt.Sprintf("Backup %s has not finished", b.Name))
		return ctrl.Result{RequeueAfter: backupWaitPoll}, nil
	}
	if refusal := restoreRefusal(rs, &b); refusal != "" {
		r.finish(rs, desired, runOutcome{reason: "Refused", message: refusal})
		return ctrl.Result{}, nil
	}
	kind := rs.Spec.Target.Kind
	driver, viaDriver := restoreDrivers[kind]
	if !viaDriver && !backup.RunsInJob(string(kind)) {
		r.finish(rs, desired, runOutcome{reason: "MechanismNotAvailable", message: fmt.Sprintf("no restore mechanism for datastore kind %s yet; supported: %s", kind, strings.Join(backup.SupportedKinds(), ", "))})
		return ctrl.Result{}, nil
	}
	bucket, reason, message, err := resolveBackupBucket(ctx, r.Client, b.Namespace, b.Spec.Bucket)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		r.wait(rs, desired, reason, message)
		return ctrl.Result{RequeueAfter: backupWaitPoll}, nil
	}
	desired.Source = b.Status.Location
	if desired.StartTime == nil {
		now := metav1.NewTime(r.now())
		desired.StartTime = &now
	}
	var outcome runOutcome
	if viaDriver {
		outcome, err = driver.restore(ctx, r, rs, &b, bucket)
	} else {
		outcome, err = r.runJob(ctx, rs, &b, bucket, desired)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if outcome.done {
		r.finish(rs, desired, outcome)
		return ctrl.Result{}, nil
	}
	desired.Phase, desired.Message = oramav1alpha1.BackupRunning, outcome.message
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupRestoreComplete, Status: metav1.ConditionFalse, Reason: "Running", Message: outcome.message, ObservedGeneration: rs.Generation})
	return ctrl.Result{RequeueAfter: outcome.requeue}, nil
}

// restoreRefusal is why a restore must not run: a kind that differs from the backup's, or the
// backup's own datastore without spec.replaceSource.
func restoreRefusal(rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup) string {
	if rs.Spec.Target.Kind != b.Spec.Datastore.Kind {
		return fmt.Sprintf("Backup %s is of a %s datastore; the target is %s", b.Name, b.Spec.Datastore.Kind, rs.Spec.Target.Kind)
	}
	if rs.Spec.Target.Name == b.Spec.Datastore.Name && !rs.Spec.ReplaceSource {
		return fmt.Sprintf("the target %s is the datastore Backup %s was taken of; set spec.replaceSource to restore into it", rs.Spec.Target.Name, b.Name)
	}
	if rs.Spec.Target.Name == b.Spec.Datastore.Name && rs.Spec.Scratch {
		return "a scratch restore (spec.scratch) is emptied afterwards, so it never targets the backup's own datastore"
	}
	return ""
}

// runJob advances a restore that runs as a Job of the operator's image.
func (r *BackupRestoreReconciler) runJob(ctx context.Context, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup, bucket *corev1.Secret, desired *oramav1alpha1.BackupRestoreStatus) (runOutcome, error) {
	jobName := restoreJobName(rs.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: jobName}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return runOutcome{}, err
	}
	if apierrors.IsNotFound(err) {
		if desired.JobName != "" {
			return runOutcome{done: true, reason: "JobLost", message: fmt.Sprintf("Job %s was deleted before it reported", jobName)}, nil
		}
		image, err := operatorOwnImage(ctx, r.Image, r.APIReader, r.OperatorNamespace)
		if err != nil {
			return runOutcome{}, err
		}
		if image == "" {
			return runOutcome{message: "the operator's own image could not be read from its pod", requeue: backupWaitPoll}, nil
		}
		labels := map[string]string{
			"app.kubernetes.io/managed-by": backupOperatorManagedTag,
			"app.kubernetes.io/component":  "restore",
			backupRestoreLabel:             dnsLabelValue(rs.Name),
		}
		runPod := map[string]string{backupRestoreLabel: dnsLabelValue(rs.Name)}
		credentials, err := writeRunResources(ctx, r.Client, r.Scheme, rs, jobName, bucket, rs.Spec.Target, runPod, labels)
		if err != nil {
			return runOutcome{}, err
		}
		pullSecrets, err := copyOperatorPullSecrets(ctx, r.Client, r.APIReader, r.OperatorNamespace, rs.Namespace)
		if err != nil {
			return runOutcome{}, err
		}
		verify := rs.Spec.Verify == nil || *rs.Spec.Verify
		args := []string{
			"--datastore", string(rs.Spec.Target.Kind),
			"--restore", rs.Name,
			"--prefix", backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path) + "/" + b.Name,
			fmt.Sprintf("--verify=%t", verify),
			fmt.Sprintf("--cleanup=%t", rs.Spec.Scratch),
			"--timeout", fmt.Sprintf("%ds", backupJobDeadline-60),
		}
		created := newBackupSystemJob(rs.Namespace, jobName, image, "restore", args, rs.Spec.Target, credentials, pullSecrets, labels)
		if err := controllerutil.SetControllerReference(rs, created, r.Scheme); err != nil {
			return runOutcome{}, err
		}
		if err := r.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
			return runOutcome{}, err
		}
		desired.JobName = jobName
		return runOutcome{message: fmt.Sprintf("Job %s is running", jobName)}, nil
	}
	finished, succeeded, jobMessage := jobOutcome(job)
	if !finished {
		return runOutcome{message: fmt.Sprintf("Job %s is running", jobName)}, nil
	}
	report, ok, err := readRunReport(ctx, r.Client, job)
	if err != nil {
		return runOutcome{}, err
	}
	switch {
	case ok && report.Succeeded && succeeded:
		return runOutcome{done: true, succeeded: true, verified: report.Verified, reason: "Restored", message: report.Message, evidence: report.Evidence}, nil
	case ok:
		return runOutcome{done: true, reason: "RestoreFailed", message: report.Message, evidence: report.Evidence}, nil
	case succeeded:
		return runOutcome{done: true, reason: "NoReport", message: fmt.Sprintf("Job %s completed without a report", jobName)}, nil
	}
	return runOutcome{done: true, reason: "JobFailed", message: fmt.Sprintf("Job %s failed: %s", jobName, jobMessage)}, nil
}

func (r *BackupRestoreReconciler) wait(rs *oramav1alpha1.BackupRestore, desired *oramav1alpha1.BackupRestoreStatus, reason, message string) {
	desired.Phase, desired.Message = oramav1alpha1.BackupPending, message
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupRestoreComplete, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: rs.Generation})
}

func (r *BackupRestoreReconciler) finish(rs *oramav1alpha1.BackupRestore, desired *oramav1alpha1.BackupRestoreStatus, outcome runOutcome) {
	now := metav1.NewTime(r.now())
	desired.CompletionTime, desired.Message, desired.Evidence, desired.Verified = &now, outcome.message, outcome.evidence, outcome.verified
	verify := rs.Spec.Verify == nil || *rs.Spec.Verify
	status := metav1.ConditionFalse
	desired.Phase = oramav1alpha1.BackupFailed
	if outcome.succeeded && (outcome.verified || !verify) {
		status, desired.Phase = metav1.ConditionTrue, oramav1alpha1.BackupSucceeded
	} else if outcome.succeeded {
		outcome.reason = "NotVerified"
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupRestoreComplete, Status: status, Reason: outcome.reason, Message: outcome.message, ObservedGeneration: rs.Generation})
}

// restoreJobName is a DNS label derived from the restore's name.
func restoreJobName(name string) string {
	label := dnsLabel(name)
	if len(label)+len(backupRestoreJobSuffix) <= backupRestoreJobNameMax {
		return label + backupRestoreJobSuffix
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return strings.Trim(label[:backupRestoreJobNameMax-len(backupRestoreJobSuffix)-len(hash)-1], "-") + "-" + hash + backupRestoreJobSuffix
}

func (r *BackupRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.BackupRestore{}).
		Owns(&batchv1.Job{}).
		Named("backuprestore").
		Complete(r)
}
