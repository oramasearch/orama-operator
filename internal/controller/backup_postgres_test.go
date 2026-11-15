/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestPostgresRetentionIsKeepTicksInDays(t *testing.T) {
	for cron, want := range map[string]string{"0 3 * * *": "7d", "0 */6 * * *": "2d", "0 5 * * 0": "49d"} {
		if got := postgresRetention(cron, 7); got != want {
			t.Fatalf("%s: %s, want %s", cron, got, want)
		}
	}
}

// The archive plugin is named, replaced and removed without touching another plugin entry.
func TestSetBarmanArchivePluginKeepsOtherPlugins(t *testing.T) {
	cluster := newCNPGCluster("ws-1", "db")
	_ = unstructured.SetNestedSlice(cluster.Object, []interface{}{map[string]interface{}{"name": "other.example"}}, "spec", "plugins")
	if err := setBarmanArchivePlugin(cluster, "db-backup-archive"); err != nil {
		t.Fatal(err)
	}
	if clusterArchive(cluster) != "db-backup-archive" {
		t.Fatalf("archive = %q", clusterArchive(cluster))
	}
	_ = setBarmanArchivePlugin(cluster, "")
	plugins, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if len(plugins) != 1 || clusterArchive(cluster) != "" {
		t.Fatalf("plugins = %v", plugins)
	}
}

// A Postgres Backup waits until its Cluster archives through the plugin, then asks CloudNativePG for
// a plugin backup, and records what a restore needs when it completes.
func TestPostgresBackupIsACloudNativePGPluginBackup(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastorePostgres)
	cluster := newCNPGCluster("ws-1", "events")
	cluster.Object["spec"] = map[string]interface{}{}
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b, cluster)...)
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupRunning {
		t.Fatalf("phase = %s", b.Status.Phase)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	_ = setBarmanArchivePlugin(cluster, "events-backup-archive")
	if err := kube.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	cnpg := &unstructured.Unstructured{}
	cnpg.SetGroupVersionKind(cnpgBackupGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "ws-1", Name: b.Name}, cnpg); err != nil {
		t.Fatalf("no CNPG Backup: %v", err)
	}
	if method, _, _ := unstructured.NestedString(cnpg.Object, "spec", "method"); method != "plugin" {
		t.Fatalf("method = %q", method)
	}
	_ = unstructured.SetNestedField(cnpg.Object, "completed", "status", "phase")
	_ = unstructured.SetNestedField(cnpg.Object, "20261114T030000", "status", "backupId")
	if err := kube.Update(context.Background(), cnpg); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupSucceeded || b.Annotations[postgresBackupIDAnnot] != "20261114T030000" || b.Annotations[postgresArchiveAnnot] != "events-backup-archive" {
		t.Fatalf("status %+v annotations %v", b.Status, b.Annotations)
	}
}
