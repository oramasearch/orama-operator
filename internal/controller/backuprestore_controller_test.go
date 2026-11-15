/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func succeededTestBackup() *oramav1alpha1.Backup {
	b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	done := metav1.NewTime(time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC))
	b.Status = oramav1alpha1.BackupStatus{Phase: oramav1alpha1.BackupSucceeded, CompletionTime: &done, Location: "s3://lake-backups/ws-1/clickhouse/events/" + b.Name + "/"}
	return b
}

func newRestoreReconciler(t *testing.T, objects ...client.Object) (*BackupRestoreReconciler, client.Client) {
	t.Helper()
	scheme := backupTestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Backup{}, &oramav1alpha1.BackupRestore{}, &oramav1alpha1.CloudflareR2Bucket{}, &batchv1.Job{}, &corev1.Pod{}).
		WithObjects(objects...).Build()
	return &BackupRestoreReconciler{Client: kube, Scheme: scheme, Image: testOperatorImage}, kube
}

func reconcileRestore(t *testing.T, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatal(err)
	}
}

// A restore into the datastore the backup was taken of is refused unless replaceSource says so.
func TestBackupRestoreRefusesTheSourceDatastore(t *testing.T) {
	b := succeededTestBackup()
	rs := &oramav1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ws-1"},
		Spec:       oramav1alpha1.BackupRestoreSpec{BackupName: b.Name, Target: b.Spec.Datastore},
	}
	r, kube := newRestoreReconciler(t, append(readyBackupBucket(), b, rs)...)
	reconcileRestore(t, r, rs)
	condition := apimeta.FindStatusCondition(rs.Status.Conditions, backupRestoreComplete)
	if rs.Status.Phase != oramav1alpha1.BackupFailed || condition == nil || condition.Reason != "Refused" || !strings.Contains(condition.Message, "replaceSource") {
		t.Fatalf("status = %+v", rs.Status)
	}
	var jobs batchv1.JobList
	if err := kube.List(context.Background(), &jobs, client.InNamespace("ws-1")); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("jobs = %d, %v", len(jobs.Items), err)
	}
}

// A restore into a scratch target runs `/manager restore` with the backup's prefix, verify and
// cleanup, opens the target's pods to the run, and reports Verified from the Job.
func TestBackupRestoreRunsAJobAndRecordsTheVerification(t *testing.T) {
	b := succeededTestBackup()
	target := oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastoreClickHouse, Name: "events-scratch",
		ConnectionSecretRef: &oramav1alpha1.LocalSecretReference{Name: "scratch-connection"}}
	rs := &oramav1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "ws-1"},
		Spec:       oramav1alpha1.BackupRestoreSpec{BackupName: b.Name, Target: target, Scratch: true},
	}
	r, kube := newRestoreReconciler(t, append(readyBackupBucket(), b, rs)...)
	reconcileRestore(t, r, rs)
	if rs.Status.Phase != oramav1alpha1.BackupRunning || rs.Status.JobName != "r2-restore" {
		t.Fatalf("status = %+v", rs.Status)
	}
	var job batchv1.Job
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: "r2-restore"}, &job); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if job.Spec.Template.Spec.Containers[0].Command[1] != "restore" ||
		!strings.Contains(args, "--prefix ws-1/clickhouse/events/"+b.Name) || !strings.Contains(args, "--verify=true") || !strings.Contains(args, "--cleanup=true") {
		t.Fatalf("command %v args %s", job.Spec.Template.Spec.Containers[0].Command, args)
	}
	var policy networkingv1.NetworkPolicy
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: "r2-restore-ds"}, &policy); err != nil {
		t.Fatalf("the target's pods were not opened to the run: %v", err)
	}
	if policy.Spec.PodSelector.MatchLabels["clickhouse.altinity.com/chi"] != "events-scratch" {
		t.Fatalf("policy selects %v", policy.Spec.PodSelector.MatchLabels)
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := kube.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "r2-restore-x", Namespace: "ws-1", Labels: map[string]string{"job-name": "r2-restore"}}}
	if err := kube.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: backupContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: `{"succeeded":true,"verified":true,"evidence":"1 items compared","message":"restored"}`}}}}
	if err := kube.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileRestore(t, r, rs)
	if rs.Status.Phase != oramav1alpha1.BackupSucceeded || !rs.Status.Verified || rs.Status.Evidence != "1 items compared" {
		t.Fatalf("status = %+v", rs.Status)
	}
	reconcileRestore(t, r, rs)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: "r2-restore-bucket"}, &corev1.Secret{}); err == nil {
		t.Fatal("the bucket credentials copy outlived the restore")
	}
}

// A rehearsal restores the schedule's newest succeeded Backup into its scratch target, and refuses
// the schedule's own datastore.
func TestRestoreRehearsalRestoresTheNewestBackup(t *testing.T) {
	scheme := backupTestScheme(t)
	created := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	schedule := &oramav1alpha1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "events", Namespace: "ws-1"},
		Spec:       oramav1alpha1.BackupScheduleSpec{Datastore: testBackup(oramav1alpha1.BackupDatastoreClickHouse).Spec.Datastore},
	}
	older, newer := succeededTestBackup(), succeededTestBackup()
	older.Name, older.Labels = "events-old", map[string]string{backupScheduleLabel: "events"}
	newer.Name, newer.Labels = "events-new", map[string]string{backupScheduleLabel: "events"}
	later := metav1.NewTime(older.Status.CompletionTime.Add(24 * time.Hour))
	newer.Status.CompletionTime = &later
	rehearsal := &oramav1alpha1.RestoreRehearsal{
		ObjectMeta: metav1.ObjectMeta{Name: "events-rehearsal", Namespace: "ws-1", CreationTimestamp: created},
		Spec: oramav1alpha1.RestoreRehearsalSpec{BackupScheduleName: "events",
			Target: oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastoreClickHouse, Name: "events-scratch"}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.RestoreRehearsal{}, &oramav1alpha1.BackupRestore{}).
		WithObjects(schedule, older, newer, rehearsal).Build()
	r := &RestoreRehearsalReconciler{Client: kube, Scheme: scheme, Now: func() time.Time { return time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC) }}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rehearsal)}); err != nil {
		t.Fatal(err)
	}
	var restores oramav1alpha1.BackupRestoreList
	if err := kube.List(context.Background(), &restores, client.InNamespace("ws-1")); err != nil || len(restores.Items) != 1 {
		t.Fatalf("restores = %d, %v", len(restores.Items), err)
	}
	rs := restores.Items[0]
	if rs.Spec.BackupName != "events-new" || !rs.Spec.Scratch || rs.Spec.Target.Name != "events-scratch" {
		t.Fatalf("restore spec = %+v", rs.Spec)
	}

	rehearsal.Spec.Target.Name = "events"
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(rehearsal), rehearsal); err != nil {
		t.Fatal(err)
	}
	rehearsal.Spec.Target.Name = "events"
	if err := kube.Update(context.Background(), rehearsal); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rehearsal)}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(rehearsal), rehearsal); err != nil {
		t.Fatal(err)
	}
	if c := apimeta.FindStatusCondition(rehearsal.Status.Conditions, restoreRehearsalReady); c == nil || c.Reason != "Refused" {
		t.Fatalf("Ready = %+v; a rehearsal into the schedule's own datastore must be refused", c)
	}
}
