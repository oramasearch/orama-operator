/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A volume backup snapshots the claim, waits for the snapshot, makes a claim from it and records
// the size a restore needs; the workload's own claim is never mounted.
func TestVolumeBackupCopiesFromASnapshotClone(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastoreVolume)
	b.Spec.Datastore = oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastoreVolume, Name: "lake-store"}
	class := "managed-csi"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-store", Namespace: "ws-1"},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
	}
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b, claim)...)
	reconcileBackup(t, r, b)
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	jobName := backupJobName(b.Name)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: jobName + "-snap"}, snapshot); err != nil {
		t.Fatalf("no snapshot: %v", err)
	}
	if b.Status.Phase != oramav1alpha1.BackupPending {
		t.Fatalf("phase = %s, want Pending while the snapshot is taken", b.Status.Phase)
	}
	_ = unstructured.SetNestedField(snapshot.Object, true, "status", "readyToUse")
	if err := kube.Update(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	var clone corev1.PersistentVolumeClaim
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: jobName + "-source"}, &clone); err != nil {
		t.Fatalf("no clone: %v", err)
	}
	if clone.Spec.DataSource == nil || clone.Spec.DataSource.Name != jobName+"-snap" || *clone.Spec.StorageClassName != class {
		t.Fatalf("clone = %+v", clone.Spec)
	}
	if b.Annotations[volumeSizeAnnot] != "20Gi" || b.Status.Phase != oramav1alpha1.BackupRunning {
		t.Fatalf("annotations %v, phase %s", b.Annotations, b.Status.Phase)
	}
}
