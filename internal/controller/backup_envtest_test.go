/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The backup CRDs' admission, against a real API server: a bucket must choose its deletion
// policy, a schedule gets its defaults, and a Backup's spec cannot change after it is created.
func TestBackupCRDAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for backup admission envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths: []string{
			"../../config/crd/bases/orama.cloud_cloudflarer2buckets.yaml",
			"../../config/crd/bases/orama.cloud_backupschedules.yaml",
			"../../config/crd/bases/orama.cloud_backups.yaml",
		},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	kube, err := client.New(config, client.Options{Scheme: backupTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}}); err != nil {
		t.Fatal(err)
	}

	noPolicy := &oramav1alpha1.CloudflareR2Bucket{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ws-1"}, Spec: oramav1alpha1.CloudflareR2BucketSpec{SecretRef: "cf", Name: "b"}}
	if err := kube.Create(ctx, noPolicy); err == nil || !strings.Contains(err.Error(), "deletionPolicy") {
		t.Fatalf("a bucket without deletionPolicy was admitted: %v", err)
	}
	noPolicy.Spec.DeletionPolicy = oramav1alpha1.BucketDeletionRetain
	if err := kube.Create(ctx, noPolicy); err != nil {
		t.Fatal(err)
	}

	schedule := testBackupSchedule(oramav1alpha1.BackupDatastoreClickHouse)
	schedule.CreationTimestamp = metav1.Time{}
	schedule.Spec.Schedule, schedule.Spec.Keep = "", 0
	if err := kube.Create(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.Spec.Schedule != "0 3 * * *" || schedule.Spec.Keep != 7 || schedule.Spec.Bucket.Kind != "CloudflareR2Bucket" {
		t.Fatalf("defaults = %q keep %d kind %q", schedule.Spec.Schedule, schedule.Spec.Keep, schedule.Spec.Bucket.Kind)
	}
	unknown := testBackupSchedule("MongoDB")
	unknown.Name, unknown.CreationTimestamp = "mongo", metav1.Time{}
	if err := kube.Create(ctx, unknown); err == nil {
		t.Fatal("a datastore kind outside the enum was admitted")
	}

	b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	if err := kube.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Spec.Path = "elsewhere"
	if err := kube.Update(ctx, b); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a Backup's spec changed: %v", err)
	}
}
