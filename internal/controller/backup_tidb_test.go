/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A TiDB Backup is a TiDB Operator Backup of the TidbCluster to the Backup's own prefix, with keys
// in the shape TiDB Operator reads and the ServiceAccount its pods report through.
func TestTiDBBackupRequestsATiDBOperatorBackup(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastoreTiDB)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(tidbClusterGVK)
	cluster.SetNamespace("ws-1")
	cluster.SetName("events")
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b, cluster)...)
	if err := rbacv1.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	backupCR := &unstructured.Unstructured{}
	backupCR.SetGroupVersionKind(tidbBackupGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: b.Name}, backupCR); err != nil {
		t.Fatalf("no TiDB Operator Backup: %v (status %+v)", err, b.Status)
	}
	prefix, _, _ := unstructured.NestedString(backupCR.Object, "spec", "s3", "prefix")
	secret, _, _ := unstructured.NestedString(backupCR.Object, "spec", "s3", "secretName")
	tc, _, _ := unstructured.NestedString(backupCR.Object, "spec", "br", "cluster")
	if prefix != "ws-1/tidb/events/"+b.Name || tc != "events" || secret != backupJobName(b.Name)+"-tidb-s3" {
		t.Fatalf("spec = %+v", backupCR.Object["spec"])
	}
	var role rbacv1.Role
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: tidbBackupManager}, &role); err != nil {
		t.Fatalf("no %s Role: %v", tidbBackupManager, err)
	}

	_ = unstructured.SetNestedField(backupCR.Object, "Complete", "status", "phase")
	_ = unstructured.SetNestedField(backupCR.Object, int64(4096), "status", "backupSize")
	if err := kube.Update(context.Background(), backupCR); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupSucceeded || b.Status.SizeBytes != 4096 {
		t.Fatalf("status = %+v", b.Status)
	}
}
