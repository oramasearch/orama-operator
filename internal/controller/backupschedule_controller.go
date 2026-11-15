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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// BackupSchedule creates a Backup on each cron tick (UTC) and deletes those past retention
// (operator#453, docs/backups.md). Its timing is taken from the newest Backup it made, so an
// operator restart neither skips nor repeats a tick; missed ticks are one backup, not a backlog.
// No new Backup starts while one is still running. Only succeeded Backups count toward keep, and
// the newest succeeded one is never deleted. Backups are not owned by the schedule: deleting the
// schedule keeps them.

const (
	backupScheduleConditionReady     = "Ready"
	backupScheduleConditionLast      = "LastBackupSucceeded"
	backupScheduleConditionOnSched   = "OnSchedule"
	backupScheduleDefaultCron        = "0 3 * * *"
	backupScheduleDefaultKeep        = int32(7)
	backupScheduleMaxCatchUpIterator = 100000
)

// BackupScheduleReconciler reconciles BackupSchedule.
type BackupScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=backupschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=backupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=backups,verbs=get;list;watch;create;update;patch;delete

func (r *BackupScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *BackupScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s oramav1alpha1.BackupSchedule
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()
	desired := *s.Status.DeepCopy()
	desired.ObservedGeneration = s.Generation

	var list oramav1alpha1.BackupList
	if err := r.List(ctx, &list, client.InNamespace(s.Namespace), client.MatchingLabels{backupScheduleLabel: dnsLabelValue(s.Name)}); err != nil {
		return ctrl.Result{}, err
	}
	backups := list.Items

	cronText := s.Spec.Schedule
	if cronText == "" {
		cronText = backupScheduleDefaultCron
	}
	schedule, cronErr := cron.ParseStandard(cronText)
	ready := metav1.Condition{Type: backupScheduleConditionReady, Status: metav1.ConditionTrue, Reason: "Scheduled", Message: fmt.Sprintf("backs up %s %s on %q (UTC)", s.Spec.Datastore.Kind, s.Spec.Datastore.Name, cronText)}
	switch {
	case cronErr != nil:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "InvalidSchedule", fmt.Sprintf("spec.schedule %q: %v", cronText, cronErr)
	case isRefused(s.Spec.Datastore.Kind):
		refusal, _ := backup.Refusal(string(s.Spec.Datastore.Kind))
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, backupReasonNoBackupByDecision, refusal
	case !backup.Supported(string(s.Spec.Datastore.Kind)):
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "MechanismNotAvailable",
			fmt.Sprintf("no backup mechanism for datastore kind %s yet; supported: %s (operator#453)", s.Spec.Datastore.Kind, strings.Join(backup.SupportedKinds(), ", "))
	case s.Spec.Suspend:
		ready.Reason, ready.Message = "Suspended", "spec.suspend is true: no new backups"
	}
	// A Postgres schedule owns the archive the Cluster's WAL goes to continuously (operator#731).
	if ready.Status == metav1.ConditionTrue && s.Spec.Datastore.Kind == oramav1alpha1.BackupDatastorePostgres {
		reason, message, err := r.ensurePostgresArchive(ctx, &s, cronText)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reason != "" {
			ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, reason, message
		}
	}
	canRun := ready.Status == metav1.ConditionTrue && !s.Spec.Suspend

	// The newest tick is taken from the Backups themselves.
	var lastTick time.Time
	running := false
	for i := range backups {
		if t := backupScheduledAt(&backups[i]); t.After(lastTick) {
			lastTick = t
		}
		if phase := backups[i].Status.Phase; phase == "" || phase == oramav1alpha1.BackupPending || phase == oramav1alpha1.BackupRunning {
			running = true
		}
	}

	var next time.Time
	if cronErr == nil {
		base := s.CreationTimestamp.UTC()
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
			created, err := r.createBackup(ctx, &s, tick)
			if err != nil {
				return ctrl.Result{}, err
			}
			backups = append(backups, *created)
			lastTick, next = tick, schedule.Next(tick)
		}
	}
	if !lastTick.IsZero() {
		t := metav1.NewTime(lastTick)
		desired.LastScheduleTime = &t
	}
	desired.NextScheduleTime = nil
	if canRun && !next.IsZero() {
		t := metav1.NewTime(next)
		desired.NextScheduleTime = &t
	}

	if err := r.applyRetention(ctx, &s, backups); err != nil {
		return ctrl.Result{}, err
	}

	// Outcome of the newest finished backup, and the newest success.
	var newestFinished, newestSucceeded *oramav1alpha1.Backup
	for i := range backups {
		b := &backups[i]
		if b.Status.CompletionTime == nil || !b.DeletionTimestamp.IsZero() {
			continue
		}
		if newestFinished == nil || b.Status.CompletionTime.After(newestFinished.Status.CompletionTime.Time) {
			newestFinished = b
		}
		if b.Status.Phase == oramav1alpha1.BackupSucceeded && (newestSucceeded == nil || b.Status.CompletionTime.After(newestSucceeded.Status.CompletionTime.Time)) {
			newestSucceeded = b
		}
	}
	desired.LastSuccessfulBackup, desired.LastSuccessTime = "", nil
	if newestSucceeded != nil {
		desired.LastSuccessfulBackup = newestSucceeded.Name
		desired.LastSuccessTime = newestSucceeded.Status.CompletionTime.DeepCopy()
	}
	last := metav1.Condition{Type: backupScheduleConditionLast, Status: metav1.ConditionUnknown, Reason: "NoBackupYet", Message: "no backup has finished yet"}
	if newestFinished != nil {
		last.Message = fmt.Sprintf("%s: %s", newestFinished.Name, newestFinished.Status.Message)
		if newestFinished.Status.Phase == oramav1alpha1.BackupSucceeded {
			last.Status, last.Reason = metav1.ConditionTrue, "BackupSucceeded"
		} else {
			last.Status, last.Reason = metav1.ConditionFalse, backupConditionReason(newestFinished, "BackupFailed")
		}
	}

	// Late: a whole tick has passed since the one that should have followed the last success.
	onSchedule := metav1.Condition{Type: backupScheduleConditionOnSched, Status: metav1.ConditionUnknown, Reason: ready.Reason, Message: ready.Message}
	var lateAt time.Time
	if canRun {
		reference := s.CreationTimestamp.UTC()
		if newestSucceeded != nil {
			reference = newestSucceeded.Status.CompletionTime.UTC()
		}
		due := schedule.Next(reference)
		lateAt = schedule.Next(due)
		if now.Before(lateAt) {
			onSchedule.Status, onSchedule.Reason, onSchedule.Message = metav1.ConditionTrue, "OnSchedule", fmt.Sprintf("a backup is due by %s", due.Format(time.RFC3339))
			if newestSucceeded == nil {
				onSchedule.Message = fmt.Sprintf("the first backup is due at %s", due.Format(time.RFC3339))
			}
		} else {
			since := "this schedule was created"
			if newestSucceeded != nil {
				since = "the last success at " + reference.Format(time.RFC3339)
			}
			onSchedule.Status, onSchedule.Reason = metav1.ConditionFalse, "BackupLate"
			onSchedule.Message = fmt.Sprintf("no backup has succeeded since %s; one was due by %s", since, due.Format(time.RFC3339))
		}
	}
	for _, condition := range []metav1.Condition{ready, last, onSchedule} {
		condition.ObservedGeneration = s.Generation
		apimeta.SetStatusCondition(&desired.Conditions, condition)
	}
	if !apiequality.Semantic.DeepEqual(s.Status, desired) {
		s.Status = desired
		if err := r.Status().Update(ctx, &s); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Wake for the next tick, or for the moment the schedule would turn late.
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

func isRefused(kind oramav1alpha1.BackupDatastoreKind) bool {
	_, refused := backup.Refusal(string(kind))
	return refused
}

func backupConditionReason(b *oramav1alpha1.Backup, fallback string) string {
	if c := apimeta.FindStatusCondition(b.Status.Conditions, backupConditionComplete); c != nil && c.Reason != "" {
		return c.Reason
	}
	return fallback
}

func backupScheduledAt(b *oramav1alpha1.Backup) time.Time {
	if t, err := time.Parse(time.RFC3339, b.Annotations[backupScheduledAtAnnot]); err == nil {
		return t.UTC()
	}
	return b.CreationTimestamp.UTC()
}

func (r *BackupScheduleReconciler) createBackup(ctx context.Context, s *oramav1alpha1.BackupSchedule, tick time.Time) (*oramav1alpha1.Backup, error) {
	stamp := tick.UTC().Format(snapshotTimeLayout)
	name := s.Name
	if max := 253 - len(stamp) - 1; len(name) > max {
		name = name[:max]
	}
	b := &oramav1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimRight(name, "-.") + "-" + stamp,
			Namespace: s.Namespace,
			Labels: map[string]string{
				backupScheduleLabel:  dnsLabelValue(s.Name),
				backupDatastoreLabel: dnsLabelValue(s.Spec.Datastore.Name),
			},
			Annotations: map[string]string{backupScheduledAtAnnot: tick.UTC().Format(time.RFC3339)},
		},
		Spec: oramav1alpha1.BackupSpec{
			Datastore: s.Spec.Datastore,
			Bucket:    s.Spec.Bucket,
			Path:      s.Spec.Path,
		},
	}
	if err := r.Create(ctx, b); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// applyRetention deletes succeeded Backups beyond keep (newest kept first, so the newest succeeded
// one always stays), and failed Backups beyond keep. A running Backup is never deleted.
func (r *BackupScheduleReconciler) applyRetention(ctx context.Context, s *oramav1alpha1.BackupSchedule, backups []oramav1alpha1.Backup) error {
	keep := int(s.Spec.Keep)
	if keep < 1 {
		keep = int(backupScheduleDefaultKeep)
	}
	byPhase := map[oramav1alpha1.BackupPhase][]*oramav1alpha1.Backup{}
	for i := range backups {
		b := &backups[i]
		if !b.DeletionTimestamp.IsZero() || b.Status.CompletionTime == nil {
			continue
		}
		byPhase[b.Status.Phase] = append(byPhase[b.Status.Phase], b)
	}
	for _, phase := range []oramav1alpha1.BackupPhase{oramav1alpha1.BackupSucceeded, oramav1alpha1.BackupFailed} {
		list := byPhase[phase]
		sort.Slice(list, func(i, j int) bool { return list[i].Status.CompletionTime.After(list[j].Status.CompletionTime.Time) })
		for _, b := range list[min(keep, len(list)):] {
			if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *BackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toSchedule := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
		name := object.GetLabels()[backupScheduleLabel]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: object.GetNamespace(), Name: name}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.BackupSchedule{}).
		Watches(&oramav1alpha1.Backup{}, toSchedule).
		Named("backupschedule").
		Complete(r)
}
