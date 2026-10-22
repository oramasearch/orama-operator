/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

var scheduleCreated = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func testBackupSchedule(kind oramav1alpha1.BackupDatastoreKind) *oramav1alpha1.BackupSchedule {
	return &oramav1alpha1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "events", Namespace: "ws-1", CreationTimestamp: metav1.NewTime(scheduleCreated)},
		Spec: oramav1alpha1.BackupScheduleSpec{
			Datastore: oramav1alpha1.BackupDatastoreRef{Kind: kind, Name: "events", ConnectionSecretRef: oramav1alpha1.LocalSecretReference{Name: "events-connection"}},
			Bucket:    oramav1alpha1.BackupBucketRef{Name: "lake-backups", Namespace: "backups"},
			Schedule:  "0 3 * * *",
			Keep:      2,
		},
	}
}

// scheduledBackup is a Backup the schedule made at tick, finished with phase at tick+5m.
func scheduledBackup(tick time.Time, phase oramav1alpha1.BackupPhase) *oramav1alpha1.Backup {
	b := &oramav1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "events-" + tick.Format(snapshotTimeLayout), Namespace: "ws-1",
			Labels:      map[string]string{backupScheduleLabel: "events"},
			Annotations: map[string]string{backupScheduledAtAnnot: tick.Format(time.RFC3339)},
		},
		Spec:   oramav1alpha1.BackupSpec{Datastore: testBackupSchedule("ClickHouse").Spec.Datastore, Bucket: testBackupSchedule("ClickHouse").Spec.Bucket},
		Status: oramav1alpha1.BackupStatus{Phase: phase, Message: fmt.Sprintf("backup %s", phase)},
	}
	if phase == oramav1alpha1.BackupSucceeded || phase == oramav1alpha1.BackupFailed {
		done := metav1.NewTime(tick.Add(5 * time.Minute))
		b.Status.CompletionTime = &done
	}
	return b
}

type scheduleFixture struct {
	t     *testing.T
	kube  client.Client
	r     *BackupScheduleReconciler
	clock time.Time
}

func newScheduleFixture(t *testing.T, objects ...client.Object) *scheduleFixture {
	scheme := backupTestScheme(t)
	f := &scheduleFixture{t: t}
	f.kube = fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.BackupSchedule{}, &oramav1alpha1.Backup{}).
		WithObjects(objects...).Build()
	f.r = &BackupScheduleReconciler{Client: f.kube, Scheme: scheme, Now: func() time.Time { return f.clock }}
	return f
}

func (f *scheduleFixture) reconcile(at time.Time) (ctrl.Result, *oramav1alpha1.BackupSchedule, []oramav1alpha1.Backup) {
	f.t.Helper()
	f.clock = at
	result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ws-1", Name: "events"}})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	var s oramav1alpha1.BackupSchedule
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: "events"}, &s); err != nil {
		f.t.Fatal(err)
	}
	var list oramav1alpha1.BackupList
	if err := f.kube.List(context.Background(), &list, client.InNamespace("ws-1")); err != nil {
		f.t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return result, &s, list.Items
}

func TestBackupScheduleTakesOneBackupPerTick(t *testing.T) {
	f := newScheduleFixture(t, testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse))
	// Before the first tick: nothing, and a wake-up for 03:00.
	result, s, backups := f.reconcile(time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC))
	if len(backups) != 0 || result.RequeueAfter != 14*time.Hour+time.Second {
		t.Fatalf("backups %d, requeue %s", len(backups), result.RequeueAfter)
	}
	if s.Status.NextScheduleTime == nil || !s.Status.NextScheduleTime.Time.Equal(time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v", s.Status.NextScheduleTime)
	}
	// The tick: one Backup, named for the tick, carrying the schedule's spec.
	_, s, backups = f.reconcile(time.Date(2026, 10, 2, 3, 0, 1, 0, time.UTC))
	if len(backups) != 1 || backups[0].Name != "events-20261002t030000z" || backups[0].Spec.Bucket.Name != "lake-backups" {
		t.Fatalf("backups = %+v", backups)
	}
	if s.Status.LastScheduleTime == nil || !s.Status.LastScheduleTime.Time.Equal(time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("last schedule = %v", s.Status.LastScheduleTime)
	}
	// Reconciling again, even past the next tick, starts nothing while that one still runs.
	_, _, backups = f.reconcile(time.Date(2026, 10, 3, 3, 30, 0, 0, time.UTC))
	if len(backups) != 1 {
		t.Fatalf("a second Backup started while the first runs: %d", len(backups))
	}
}

func TestBackupScheduleTimesFromTheNewestBackupAndTakesNoBacklog(t *testing.T) {
	// The operator was down from the 2nd's backup until the 5th at noon: three ticks were missed.
	f := newScheduleFixture(t, testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse),
		scheduledBackup(time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC), oramav1alpha1.BackupSucceeded))
	_, _, backups := f.reconcile(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if len(backups) != 2 || backups[1].Name != "events-20261005t030000z" {
		t.Fatalf("backups = %v; want the 2nd's and one for the latest tick (the 5th)", backupNames(backups))
	}
}

func TestBackupScheduleKeepsTheNewestSucceededAndNeverARunningOne(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 0, 0, 0, time.UTC) }
	s := testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse)
	s.Spec.Keep = 2
	f := newScheduleFixture(t, s,
		scheduledBackup(day(2), oramav1alpha1.BackupSucceeded),
		scheduledBackup(day(3), oramav1alpha1.BackupSucceeded),
		scheduledBackup(day(4), oramav1alpha1.BackupFailed),
		scheduledBackup(day(5), oramav1alpha1.BackupSucceeded),
		scheduledBackup(day(6), oramav1alpha1.BackupSucceeded),
		scheduledBackup(day(7), oramav1alpha1.BackupRunning),
	)
	_, status, backups := f.reconcile(day(7).Add(time.Hour))
	want := []string{"events-20261004t030000z", "events-20261005t030000z", "events-20261006t030000z", "events-20261007t030000z"}
	if fmt.Sprint(backupNames(backups)) != fmt.Sprint(want) {
		t.Fatalf("kept %v, want %v", backupNames(backups), want)
	}
	if status.Status.LastSuccessfulBackup != "events-20261006t030000z" {
		t.Fatalf("last successful = %q", status.Status.LastSuccessfulBackup)
	}
}

func TestBackupScheduleSaysWhenTheLastBackupFailedAndWhenItIsLate(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 0, 0, 0, time.UTC) }
	f := newScheduleFixture(t, testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse),
		scheduledBackup(day(2), oramav1alpha1.BackupSucceeded),
		scheduledBackup(day(3), oramav1alpha1.BackupFailed),
	)
	// On the 3rd after the failure: the last backup failed, but a success is not yet overdue.
	_, s, _ := f.reconcile(day(3).Add(time.Hour))
	last := apimeta.FindStatusCondition(s.Status.Conditions, "LastBackupSucceeded")
	onSchedule := apimeta.FindStatusCondition(s.Status.Conditions, "OnSchedule")
	if last.Status != metav1.ConditionFalse || last.Message != "events-20261003t030000z: backup Failed" {
		t.Fatalf("LastBackupSucceeded = %+v", last)
	}
	if onSchedule.Status != metav1.ConditionTrue {
		t.Fatalf("OnSchedule = %+v; one failure within a tick is not late yet", onSchedule)
	}
	// On the 4th past 03:00 with the new one failed too, a whole tick has passed: late.
	if err := f.kube.Create(context.Background(), scheduledBackup(day(4), oramav1alpha1.BackupFailed)); err != nil {
		t.Fatal(err)
	}
	_, s, _ = f.reconcile(day(4).Add(time.Hour))
	onSchedule = apimeta.FindStatusCondition(s.Status.Conditions, "OnSchedule")
	if onSchedule.Status != metav1.ConditionFalse || onSchedule.Reason != "BackupLate" {
		t.Fatalf("OnSchedule = %+v; want BackupLate", onSchedule)
	}
	if s.Status.LastSuccessTime == nil || !s.Status.LastSuccessTime.Time.Equal(day(2).Add(5*time.Minute)) {
		t.Fatalf("last success = %v", s.Status.LastSuccessTime)
	}
}

func TestBackupScheduleRefusesAKindWithNoMechanismAndABadSchedule(t *testing.T) {
	f := newScheduleFixture(t, testBackupSchedule(oramav1alpha1.BackupDatastoreKafka))
	_, s, backups := f.reconcile(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	ready := apimeta.FindStatusCondition(s.Status.Conditions, "Ready")
	if len(backups) != 0 || ready.Status != metav1.ConditionFalse || ready.Reason != "MechanismNotAvailable" {
		t.Fatalf("backups %d, Ready %+v", len(backups), ready)
	}

	bad := testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse)
	bad.Spec.Schedule = "every day"
	f = newScheduleFixture(t, bad)
	_, s, backups = f.reconcile(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	ready = apimeta.FindStatusCondition(s.Status.Conditions, "Ready")
	if len(backups) != 0 || ready.Reason != "InvalidSchedule" {
		t.Fatalf("backups %d, Ready %+v", len(backups), ready)
	}
}

func TestBackupScheduleSuspendedTakesNoBackupAndAWriteOnlyOnChange(t *testing.T) {
	s := testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse)
	s.Spec.Suspend = true
	f := newScheduleFixture(t, s)
	_, status, backups := f.reconcile(time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC))
	if len(backups) != 0 || status.Status.NextScheduleTime != nil {
		t.Fatalf("suspended schedule: backups %d, next %v", len(backups), status.Status.NextScheduleTime)
	}
	version := status.ResourceVersion
	_, status, _ = f.reconcile(time.Date(2026, 10, 9, 5, 0, 0, 0, time.UTC))
	if status.ResourceVersion != version {
		t.Fatal("a steady schedule must not write its status again")
	}
}

func backupNames(backups []oramav1alpha1.Backup) []string {
	out := make([]string, len(backups))
	for i, b := range backups {
		out[i] = b.Name
	}
	return out
}
