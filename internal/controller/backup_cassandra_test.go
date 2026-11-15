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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestMedusaSpecPointsAtTheBucketWithItsScopedKeys(t *testing.T) {
	bucket := &corev1.Secret{Data: map[string][]byte{"endpoint": []byte("https://acct.r2.cloudflarestorage.com"), "bucket": []byte("lake-backups")}}
	spec := medusaSpec(bucket, "ws-1/cassandra/prod-db", "prod-db-backup-medusa")
	storage := spec["storageProperties"].(map[string]interface{})
	if storage["storageProvider"] != "s3_compatible" || storage["host"] != "acct.r2.cloudflarestorage.com" || storage["bucketName"] != "lake-backups" ||
		storage["prefix"] != "ws-1/cassandra/prod-db" || storage["secure"] != true || storage["region"] != "auto" {
		t.Fatalf("storage = %+v", storage)
	}
	if spec["securityContext"].(map[string]interface{})["allowPrivilegeEscalation"] != false {
		t.Fatal("the Medusa container must meet the restricted Pod Security level")
	}
}

func TestMedusaJobOutcome(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{"finished": []interface{}{"a", "b"}}}}
	if done, failed, _ := medusaJobOutcome(job); done || failed {
		t.Fatal("a job without finishTime is not done")
	}
	job.Object["status"].(map[string]interface{})["finishTime"] = "2026-11-14T03:10:00Z"
	if done, failed, detail := medusaJobOutcome(job); !done || failed || !strings.Contains(detail, "2 nodes") {
		t.Fatalf("done %v failed %v %s", done, failed, detail)
	}
	job.Object["status"].(map[string]interface{})["failed"] = []interface{}{"c"}
	if _, failed, _ := medusaJobOutcome(job); !failed {
		t.Fatal("a failed node fails the job")
	}
}

// A Cassandra Backup waits for Medusa, then asks K8ssandra for a MedusaBackupJob of its datacenter.
func TestCassandraBackupRequestsAMedusaBackupJob(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastoreCassandra)
	cluster := newK8ssandraCluster("ws-1", "events")
	cluster.Object["spec"] = map[string]interface{}{"cassandra": map[string]interface{}{"datacenters": []interface{}{
		map[string]interface{}{"metadata": map[string]interface{}{"name": "events-dc"}},
	}}}
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b, cluster)...)
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupRunning || !strings.Contains(b.Status.Message, "no Medusa") {
		t.Fatalf("status = %+v", b.Status)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedMap(cluster.Object, map[string]interface{}{"storageProperties": map[string]interface{}{}}, "spec", "medusa")
	if err := kube.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	job := &unstructured.Unstructured{}
	job.SetGroupVersionKind(medusaBackupJobGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: b.Name}, job); err != nil {
		t.Fatalf("no MedusaBackupJob: %v", err)
	}
	if dc, _, _ := unstructured.NestedString(job.Object, "spec", "cassandraDatacenter"); dc != "events-dc" {
		t.Fatalf("datacenter = %q", dc)
	}
}
