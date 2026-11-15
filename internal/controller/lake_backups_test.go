/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func backupsTestLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake",
			Backups:   &oramav1alpha1.LakeBackupsSpec{CloudflareSecretRef: corev1.LocalObjectReference{Name: "cloudflare-r2"}},
		},
	}
}

func backupsTestService(kind string) *LakeService {
	return &LakeService{ID: "svc-1", CustomerID: "cus-1", WorkspaceID: "ws-1", WorkspaceNamespace: "ws-1", Kind: kind, Slug: "events"}
}

// A ready ClickHouse service gets a schedule in its workspace namespace, its connection in the
// shape the Job reads, the Lake's Retain bucket, and the pods the run opens.
func TestLakeServiceGetsABackupScheduleAndConnection(t *testing.T) {
	ctx := context.Background()
	scheme := backupTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &LakeReconciler{Client: c, Scheme: scheme}
	lake := backupsTestLake()
	svc := backupsTestService(serviceKindClickHouse)
	credential := []byte(`{"kind":"clickhouse","host":"chi-prod-events.ws-1.svc.cluster.local","http_port":8123,"native_port":9000,"user":"lake","password":"pw","database":"default"}`)

	note, err := r.reconcileServiceBackup(ctx, lake, svc, credential)
	if err != nil || note != "" {
		t.Fatalf("reconcileServiceBackup = %q, %v; want no note for a fresh schedule", note, err)
	}
	var schedule oramav1alpha1.BackupSchedule
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: "prod-events-backup"}, &schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.Spec.Datastore.Kind != oramav1alpha1.BackupDatastoreClickHouse || schedule.Spec.Datastore.Name != "prod-events" ||
		schedule.Spec.Bucket.Name != "lake-prod-backups" || schedule.Spec.Bucket.Namespace != "lake" ||
		schedule.Spec.Schedule != backupScheduleDefaultCron || schedule.Spec.Keep != backupScheduleDefaultKeep {
		t.Fatalf("schedule spec = %+v", schedule.Spec)
	}
	if len(schedule.Spec.Datastore.PodSelector) == 0 {
		t.Fatal("the ClickHouse schedule names no pods, so its server could not reach the bucket")
	}
	if !metav1.IsControlledBy(&schedule, lake) || schedule.Labels[serviceIDLabel] != "svc-1" {
		t.Fatalf("schedule metadata = %+v", schedule.ObjectMeta)
	}
	var connection corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: "prod-events-backup-connection"}, &connection); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"host": "chi-prod-events.ws-1.svc.cluster.local", "port": "8123", "username": "lake", "password": "pw", "database": "default"}
	for key, value := range want {
		if string(connection.Data[key]) != value {
			t.Fatalf("connection %s = %q, want %q", key, connection.Data[key], value)
		}
	}

	// The sweep writes the bucket with the namespace allowed, Retain.
	if err := r.sweepLakeBackups(ctx, lake, map[string]bool{"svc-1": true}); err != nil {
		t.Fatal(err)
	}
	var bucket oramav1alpha1.CloudflareR2Bucket
	if err := c.Get(ctx, client.ObjectKey{Namespace: "lake", Name: "lake-prod-backups"}, &bucket); err != nil {
		t.Fatal(err)
	}
	if bucket.Spec.DeletionPolicy != oramav1alpha1.BucketDeletionRetain || bucket.Spec.SecretRef != "cloudflare-r2" ||
		bucket.Spec.Credentials == nil || strings.Join(bucket.Spec.Credentials.AllowedNamespaces, ",") != "ws-1" {
		t.Fatalf("bucket spec = %+v", bucket.Spec)
	}
}

// A deleted service loses its schedule and connection; a Backup it made stays.
func TestLakeBackupSweepKeepsBackups(t *testing.T) {
	ctx := context.Background()
	scheme := backupTestScheme(t)
	existing := &oramav1alpha1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "prod-events-backup-20261114t030000z", Namespace: "ws-1",
		Labels: map[string]string{backupScheduleLabel: "prod-events-backup"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := &LakeReconciler{Client: c, Scheme: scheme}
	lake := backupsTestLake()
	svc := backupsTestService(serviceKindValkey)
	if _, err := r.reconcileServiceBackup(ctx, lake, svc, []byte(`{"kind":"valkey","host":"valkey-prod-events.ws-1.svc.cluster.local","port":6379,"user":"app","password":"pw"}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.sweepLakeBackups(ctx, lake, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	var schedules oramav1alpha1.BackupScheduleList
	if err := c.List(ctx, &schedules); err != nil || len(schedules.Items) != 0 {
		t.Fatalf("schedules after the sweep = %d, %v", len(schedules.Items), err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: "prod-events-backup-connection"}, &corev1.Secret{}); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("the connection Secret outlived its service: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &oramav1alpha1.Backup{}); err != nil {
		t.Fatalf("the Backup was deleted with its service: %v", err)
	}
}

// A late or failed backup is said on the service and on the Lake.
func TestBackupScheduleNoteAndLakeCondition(t *testing.T) {
	late := oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: "a-backup", Namespace: "ws-1"}}
	apimeta.SetStatusCondition(&late.Status.Conditions, metav1.Condition{Type: backupScheduleConditionReady, Status: metav1.ConditionTrue, Reason: "Scheduled"})
	apimeta.SetStatusCondition(&late.Status.Conditions, metav1.Condition{Type: backupScheduleConditionOnSched, Status: metav1.ConditionFalse, Reason: "BackupLate", Message: "no backup has succeeded since x"})
	if note := backupScheduleNote(&late); !strings.HasPrefix(note, "backup late") {
		t.Fatalf("late note = %q", note)
	}
	failed := oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: "b-backup", Namespace: "ws-1"}}
	apimeta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: backupScheduleConditionLast, Status: metav1.ConditionFalse, Reason: "BackupFailed", Message: "refused"})
	steady := oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: "c-backup", Namespace: "ws-1"}}
	apimeta.SetStatusCondition(&steady.Status.Conditions, metav1.Condition{Type: backupScheduleConditionReady, Status: metav1.ConditionTrue, Reason: "Scheduled"})
	if note := backupScheduleNote(&steady); note != "" {
		t.Fatalf("a steady schedule adds %q to the service", note)
	}
	condition, err := backupSchedulesCondition(metav1.Condition{Type: lakeBackupsConditionType}, func() ([]oramav1alpha1.BackupSchedule, error) {
		return []oramav1alpha1.BackupSchedule{late, failed, steady}, nil
	})
	if err != nil || condition.Status != metav1.ConditionFalse || condition.Reason != "BackupFailed" ||
		!strings.Contains(condition.Message, "2 of 3") {
		t.Fatalf("condition = %+v, %v", condition, err)
	}
	condition, _ = backupSchedulesCondition(metav1.Condition{Type: lakeBackupsConditionType}, func() ([]oramav1alpha1.BackupSchedule, error) {
		return []oramav1alpha1.BackupSchedule{steady}, nil
	})
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("steady condition = %+v", condition)
	}
}

func TestBackupConnectionDataReadsEveryKindsDocument(t *testing.T) {
	for _, test := range []struct {
		document map[string]interface{}
		want     map[string]string
	}{
		{map[string]interface{}{"kind": "zetta", "endpoint": "http://prod-z-zetta.ws-1.svc.cluster.local:8080", "token": "t"},
			map[string]string{"host": "prod-z-zetta.ws-1.svc.cluster.local", "port": "8080", "password": "t"}},
		{map[string]interface{}{"kind": "valkey", "host": "h", "port": float64(6379), "user": "app", "password": "p"},
			map[string]string{"host": "h", "port": "6379", "username": "app", "password": "p"}},
	} {
		got := backupConnectionData(test.document)
		if len(got) != len(test.want) {
			t.Fatalf("%v -> %v", test.document, got)
		}
		for key, value := range test.want {
			if string(got[key]) != value {
				t.Fatalf("%v: %s = %q, want %q", test.document["kind"], key, got[key], value)
			}
		}
	}
}

// Cloud v1: the control DB and in-cluster analytics are backed up; nothing without Cloudflare.
func TestV1BackupTargets(t *testing.T) {
	oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "v1-eu"}}
	oc.Spec.Components.ControlDB.Enabled = true
	oc.Spec.Components.Analytics.Enabled = true
	targets := v1BackupTargets(oc)
	if len(targets) != 2 || targets[0].Kind != oramav1alpha1.BackupDatastorePostgres || targets[0].Name != "v1-controldb" ||
		targets[1].Kind != oramav1alpha1.BackupDatastoreClickHouse || targets[1].ConnectionSecretRef == nil {
		t.Fatalf("targets = %+v", targets)
	}
	if v1BackupsEnabled(oc) {
		t.Fatal("backups enabled without a Cloudflare token")
	}
}
