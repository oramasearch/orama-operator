/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
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
)

// RestoreRehearsal restores the newest succeeded Backup of a BackupSchedule into a scratch datastore
// on a cron schedule (UTC) and verifies it (operator#741), generalising LakeRestoreRehearsal to every
// kind of the backup system. Each tick is one BackupRestore with spec.scratch, owned by the rehearsal,
// so what it restored is removed afterwards and the next tick finds the target empty. The rehearsal
// says when the newest one failed, and when a whole tick has passed without a verified restore. Its
// timing comes from its newest BackupRestore, like BackupSchedule's.

const (
	restoreRehearsalLabel        = "orama.cloud/restore-rehearsal"
	restoreRehearsalReady        = "Ready"
	restoreRehearsalLast         = "LastRehearsalSucceeded"
	restoreRehearsalOnSchedule   = "OnSchedule"
	restoreRehearsalDefaultCron  = "0 5 * * 0"
	restoreRehearsalDefaultKeep  = 4
	restoreRehearsalWaitForFirst = 10 * time.Minute
)

// RestoreRehearsalReconciler reconciles RestoreRehearsal.
type RestoreRehearsalReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=restorerehearsals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=restorerehearsals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=backuprestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=backupschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=backups,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;update;patch

func (r *RestoreRehearsalReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *RestoreRehearsalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rh oramav1alpha1.RestoreRehearsal
	if err := r.Get(ctx, req.NamespacedName, &rh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()
	desired := *rh.Status.DeepCopy()
	desired.ObservedGeneration = rh.Generation

	var list oramav1alpha1.BackupRestoreList
	if err := r.List(ctx, &list, client.InNamespace(rh.Namespace), client.MatchingLabels{restoreRehearsalLabel: dnsLabelValue(rh.Name)}); err != nil {
		return ctrl.Result{}, err
	}
	restores := list.Items

	cronText := rh.Spec.Schedule
	if cronText == "" {
		cronText = restoreRehearsalDefaultCron
	}
	schedule, cronErr := cron.ParseStandard(cronText)
	ready := metav1.Condition{Type: restoreRehearsalReady, Status: metav1.ConditionTrue, Reason: "Scheduled",
		Message: fmt.Sprintf("restores the newest backup of %s into %s on %q (UTC)", rh.Spec.BackupScheduleName, rh.Spec.Target.Name, cronText)}
	source, refusal, err := r.source(ctx, &rh)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case cronErr != nil:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "InvalidSchedule", fmt.Sprintf("spec.schedule %q: %v", cronText, cronErr)
	case refusal != "":
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "Refused", refusal
	case rh.Spec.Suspend:
		ready.Reason, ready.Message = "Suspended", "spec.suspend is true: no new rehearsals"
	}
	canRun := ready.Status == metav1.ConditionTrue && !rh.Spec.Suspend

	var lastTick time.Time
	running := false
	for i := range restores {
		if t := restoreScheduledAt(&restores[i]); t.After(lastTick) {
			lastTick = t
		}
		if phase := restores[i].Status.Phase; phase != oramav1alpha1.BackupSucceeded && phase != oramav1alpha1.BackupFailed {
			running = true
		}
	}
	var next time.Time
	waiting := ""
	if cronErr == nil {
		base := rh.CreationTimestamp.UTC()
		if !lastTick.IsZero() {
			base = lastTick
		}
		next = schedule.Next(base)
		if canRun && !now.Before(next) && !running {
			tick := next
			for i := 0; i < backupScheduleMaxCatchUpIterator; i++ {
				following := schedule.Next(tick)
				if following.After(now) {
					break
				}
				tick = following
			}
			waiting = ""
			if rh.Spec.Target.Kind == oramav1alpha1.BackupDatastoreZetta {
				var err error
				if waiting, err = r.ensureZettaRestoreTarget(ctx, &rh); err != nil {
					return ctrl.Result{}, err
				}
			}
			if waiting != "" {
				next = now.Add(restoreRehearsalWaitForFirst)
			} else if source == nil {
				// No backup to rehearse yet: look again soon rather than at the next tick.
				waiting = "no succeeded Backup of " + rh.Spec.BackupScheduleName + " yet"
				next = now.Add(restoreRehearsalWaitForFirst)
			} else {
				created, err := r.createRestore(ctx, &rh, source, tick)
				if err != nil {
					return ctrl.Result{}, err
				}
				restores = append(restores, *created)
				lastTick, next = tick, schedule.Next(tick)
			}
		}
	}
	desired.LastScheduleTime, desired.NextScheduleTime = nil, nil
	if !lastTick.IsZero() {
		t := metav1.NewTime(lastTick)
		desired.LastScheduleTime = &t
	}
	if canRun && !next.IsZero() {
		t := metav1.NewTime(next)
		desired.NextScheduleTime = &t
	}
	if err := r.applyRetention(ctx, &rh, restores); err != nil {
		return ctrl.Result{}, err
	}

	var newestFinished, newestVerified *oramav1alpha1.BackupRestore
	for i := range restores {
		rs := &restores[i]
		if rs.Status.CompletionTime == nil || !rs.DeletionTimestamp.IsZero() {
			continue
		}
		if newestFinished == nil || rs.Status.CompletionTime.After(newestFinished.Status.CompletionTime.Time) {
			newestFinished = rs
		}
		if rs.Status.Phase == oramav1alpha1.BackupSucceeded && rs.Status.Verified &&
			(newestVerified == nil || rs.Status.CompletionTime.After(newestVerified.Status.CompletionTime.Time)) {
			newestVerified = rs
		}
	}
	desired.LastSuccessfulRestore, desired.LastSuccessTime = "", nil
	if newestVerified != nil {
		desired.LastSuccessfulRestore = newestVerified.Name
		desired.LastSuccessTime = newestVerified.Status.CompletionTime.DeepCopy()
	}
	last := metav1.Condition{Type: restoreRehearsalLast, Status: metav1.ConditionUnknown, Reason: "NoRehearsalYet", Message: "no rehearsal has finished yet"}
	if waiting != "" && newestFinished == nil {
		last.Message = waiting
	}
	if newestFinished != nil {
		last.Message = fmt.Sprintf("%s: %s", newestFinished.Name, newestFinished.Status.Message)
		if newestFinished.Status.Phase == oramav1alpha1.BackupSucceeded && newestFinished.Status.Verified {
			last.Status, last.Reason = metav1.ConditionTrue, "RestoredAndVerified"
		} else {
			last.Status, last.Reason = metav1.ConditionFalse, "RehearsalFailed"
			if c := apimeta.FindStatusCondition(newestFinished.Status.Conditions, backupRestoreComplete); c != nil && c.Reason != "" {
				last.Reason = c.Reason
			}
		}
	}
	onSchedule := metav1.Condition{Type: restoreRehearsalOnSchedule, Status: metav1.ConditionUnknown, Reason: ready.Reason, Message: ready.Message}
	var lateAt time.Time
	if canRun {
		reference := rh.CreationTimestamp.UTC()
		if newestVerified != nil {
			reference = newestVerified.Status.CompletionTime.UTC()
		}
		due := schedule.Next(reference)
		lateAt = schedule.Next(due)
		if now.Before(lateAt) {
			onSchedule.Status, onSchedule.Reason, onSchedule.Message = metav1.ConditionTrue, "OnSchedule", fmt.Sprintf("a verified restore is due by %s", due.Format(time.RFC3339))
		} else {
			onSchedule.Status, onSchedule.Reason = metav1.ConditionFalse, "RehearsalLate"
			onSchedule.Message = fmt.Sprintf("no restore of %s has been verified since %s; one was due by %s", rh.Spec.BackupScheduleName, reference.Format(time.RFC3339), due.Format(time.RFC3339))
		}
	}
	for _, condition := range []metav1.Condition{ready, last, onSchedule} {
		condition.ObservedGeneration = rh.Generation
		apimeta.SetStatusCondition(&desired.Conditions, condition)
	}
	if !apiequality.Semantic.DeepEqual(rh.Status, desired) {
		rh.Status = desired
		if err := r.Status().Update(ctx, &rh); err != nil {
			return ctrl.Result{}, err
		}
	}
	var wake time.Time
	for _, t := range []time.Time{next, lateAt} {
		if canRun && !t.IsZero() && t.After(now) && (wake.IsZero() || t.Before(wake)) {
			wake = t
		}
	}
	if wake.IsZero() {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: wake.Sub(now) + time.Second}, nil
}

// zettaRestoreTargetEnv marks a Zetta as a scratch restore target: the engine empties a restored
// node with restore_snapshot:discard only when it was started with it (zetta#1521).
const zettaRestoreTargetEnv = "ZETTA_RESTORE_TARGET"

// ensureZettaRestoreTarget sets ZETTA_RESTORE_TARGET=1 on the scratch OramaZettaCluster, so the
// rehearsal can empty it afterwards. It returns what it waits for: the cluster to exist, or its
// nodes to restart with the variable.
func (r *RestoreRehearsalReconciler) ensureZettaRestoreTarget(ctx context.Context, rh *oramav1alpha1.RestoreRehearsal) (string, error) {
	var cluster oramav1alpha1.OramaZettaCluster
	if err := r.Get(ctx, client.ObjectKey{Namespace: rh.Namespace, Name: rh.Spec.Target.Name}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("the scratch OramaZettaCluster %s does not exist", rh.Spec.Target.Name), nil
		}
		return "", err
	}
	for _, variable := range cluster.Spec.Env {
		if variable.Name == zettaRestoreTargetEnv && variable.Value == "1" {
			if cluster.Status.Phase != "Ready" {
				return fmt.Sprintf("the scratch OramaZettaCluster %s is %s", cluster.Name, cluster.Status.Phase), nil
			}
			return "", nil
		}
	}
	env := cluster.Spec.Env[:0:0]
	for _, variable := range cluster.Spec.Env {
		if variable.Name != zettaRestoreTargetEnv {
			env = append(env, variable)
		}
	}
	cluster.Spec.Env = append(env, corev1.EnvVar{Name: zettaRestoreTargetEnv, Value: "1"})
	if err := r.Update(ctx, &cluster); err != nil {
		return "", err
	}
	return fmt.Sprintf("setting %s=1 on the scratch OramaZettaCluster %s", zettaRestoreTargetEnv, cluster.Name), nil
}

// source is the newest succeeded Backup of the rehearsal's schedule, or why the rehearsal is refused.
func (r *RestoreRehearsalReconciler) source(ctx context.Context, rh *oramav1alpha1.RestoreRehearsal) (*oramav1alpha1.Backup, string, error) {
	var schedule oramav1alpha1.BackupSchedule
	if err := r.Get(ctx, client.ObjectKey{Namespace: rh.Namespace, Name: rh.Spec.BackupScheduleName}, &schedule); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("BackupSchedule %s does not exist in %s", rh.Spec.BackupScheduleName, rh.Namespace), nil
		}
		return nil, "", err
	}
	if schedule.Spec.Datastore.Kind != rh.Spec.Target.Kind {
		return nil, fmt.Sprintf("BackupSchedule %s backs up a %s datastore; the target is %s", schedule.Name, schedule.Spec.Datastore.Kind, rh.Spec.Target.Kind), nil
	}
	if schedule.Spec.Datastore.Name == rh.Spec.Target.Name {
		return nil, fmt.Sprintf("the target %s is the datastore the schedule backs up; a rehearsal restores into a scratch datastore", rh.Spec.Target.Name), nil
	}
	var backups oramav1alpha1.BackupList
	if err := r.List(ctx, &backups, client.InNamespace(rh.Namespace), client.MatchingLabels{backupScheduleLabel: dnsLabelValue(schedule.Name)}); err != nil {
		return nil, "", err
	}
	var newest *oramav1alpha1.Backup
	for i := range backups.Items {
		b := &backups.Items[i]
		if b.Status.Phase != oramav1alpha1.BackupSucceeded || b.Status.CompletionTime == nil || !b.DeletionTimestamp.IsZero() {
			continue
		}
		if newest == nil || b.Status.CompletionTime.After(newest.Status.CompletionTime.Time) {
			newest = b
		}
	}
	return newest, "", nil
}

func restoreScheduledAt(rs *oramav1alpha1.BackupRestore) time.Time {
	if t, err := time.Parse(time.RFC3339, rs.Annotations[backupScheduledAtAnnot]); err == nil {
		return t.UTC()
	}
	return rs.CreationTimestamp.UTC()
}

func (r *RestoreRehearsalReconciler) createRestore(ctx context.Context, rh *oramav1alpha1.RestoreRehearsal, source *oramav1alpha1.Backup, tick time.Time) (*oramav1alpha1.BackupRestore, error) {
	stamp := tick.UTC().Format(snapshotTimeLayout)
	name := rh.Name
	if max := 253 - len(stamp) - 1; len(name) > max {
		name = name[:max]
	}
	verify := true
	rs := &oramav1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:        strings.TrimRight(name, "-.") + "-" + stamp,
			Namespace:   rh.Namespace,
			Labels:      map[string]string{restoreRehearsalLabel: dnsLabelValue(rh.Name)},
			Annotations: map[string]string{backupScheduledAtAnnot: tick.UTC().Format(time.RFC3339)},
		},
		Spec: oramav1alpha1.BackupRestoreSpec{BackupName: source.Name, Target: rh.Spec.Target, Verify: &verify, Scratch: true},
	}
	if err := controllerutil.SetControllerReference(rh, rs, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, rs); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return nil, err
		}
	}
	return rs, nil
}

// applyRetention keeps the newest finished BackupRestores, up to keep; a running one is never deleted.
func (r *RestoreRehearsalReconciler) applyRetention(ctx context.Context, rh *oramav1alpha1.RestoreRehearsal, restores []oramav1alpha1.BackupRestore) error {
	keep := int(rh.Spec.Keep)
	if keep < 1 {
		keep = restoreRehearsalDefaultKeep
	}
	var finished []*oramav1alpha1.BackupRestore
	for i := range restores {
		if restores[i].Status.CompletionTime != nil && restores[i].DeletionTimestamp.IsZero() {
			finished = append(finished, &restores[i])
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].Status.CompletionTime.After(finished[j].Status.CompletionTime.Time)
	})
	for _, rs := range finished[min(keep, len(finished)):] {
		if err := r.Delete(ctx, rs); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *RestoreRehearsalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.RestoreRehearsal{}).
		Owns(&oramav1alpha1.BackupRestore{}).
		Named("restorerehearsal").
		Complete(r)
}
