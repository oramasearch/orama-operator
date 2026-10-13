/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for the Prod 2 readiness changes (orama-bridge#141): persistent readers, configurable
// storage, control-DB backups, deletion protection, digest pinning and node placement. Each
// change also asserts that a CR without the new fields renders as it did before.

// fullStackOCV1 is a CR with every component on, as an existing (pre-#141) CR would declare it.
func fullStackOCV1(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Profile = "medium"
		o.Spec.DemoSeed = true
		o.Spec.IDP.Provider = "keycloak"
		c := &o.Spec.Components
		c.Engine.Enabled, c.Engine.Envoy = true, true
		c.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app.example.com"}
		c.Docs.Enabled = true
		c.Ingestion.Enabled, c.Ingestion.Harvest = true, true
		c.ControlDB.Enabled = true
		for _, ds := range []*oramav1alpha1.DataStoreSpec{&c.Analytics, &c.Cache, &c.Broker, &c.Search} {
			ds.Enabled = true
		}
		c.PgAdmin.Enabled, c.Disco.Enabled, c.Workers.Enabled = true, true, true
		c.AnalyticsProcessor.Enabled, c.SearchWorkers.Enabled = true, true
		if mut != nil {
			mut(o)
		}
	})
}

// reconcileOCV1 runs the full OramaCloudV1 reconcile against a fake client holding oc (+ objs).
func reconcileOCV1(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) (client.Client, *OramaCloudV1Reconciler) {
	t.Helper()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, oc)...).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return cl, r
}

func reconcileAgain(t *testing.T, cl client.Client, r *OramaCloudV1Reconciler, mut func(*oramav1alpha1.OramaCloudV1)) {
	t.Helper()
	ctx := context.Background()
	oc := &oramav1alpha1.OramaCloudV1{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "v1", Namespace: "orama-system"}, oc); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(oc)
		if err := cl.Update(ctx, oc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getObj[T client.Object](t *testing.T, cl client.Client, name string, obj T) T {
	t.Helper()
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, name, err)
	}
	return obj
}

func absent(t *testing.T, cl client.Client, name string, obj client.Object) bool {
	t.Helper()
	err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

func controlDBCluster(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	return getObj(t, cl, "v1-controldb", cluster)
}

func scheduledBackup(cl client.Client) (*unstructured.Unstructured, error) {
	sb := newControlDBScheduledBackup("v1-controldb-backup", "orama-system")
	return sb, cl.Get(context.Background(), client.ObjectKeyFromObject(sb), sb)
}

// --- 3. control-DB backups ---

func backupSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "orama-system"}, Data: map[string][]byte{
		"s3ApiEndpoint":   []byte("https://account.r2.cloudflarestorage.com"),
		"accessKeyId":     []byte("access-key"),
		"secretAccessKey": []byte("secret-key"),
	}}
}

func withBackup(o *oramav1alpha1.OramaCloudV1) {
	enabled := true
	o.Spec.Components.ControlDB.Backup = oramav1alpha1.ControlDBBackupSpec{
		Enabled:     &enabled,
		Destination: oramav1alpha1.ControlDBBackupDestination{Bucket: "prod2-backups"},
		SecretRef:   corev1.LocalObjectReference{Name: "r2"},
	}
}

func TestControlDBBackupsConfigureBarmanAndScheduledBackup(t *testing.T) {
	ctx := context.Background()
	cl, r := reconcileOCV1(t, fullStackOCV1(withBackup), backupSecret())

	cluster := controlDBCluster(t, cl)
	dest, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "destinationPath")
	endpoint, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "endpointURL")
	retention, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "retentionPolicy")
	if dest != "s3://prod2-backups/orama-system/v1/controldb" || endpoint != "https://account.r2.cloudflarestorage.com" || retention != "30d" {
		t.Errorf("cluster backup = %+v", cluster.Object["spec"].(map[string]interface{})["backup"])
	}
	sb, err := scheduledBackup(cl)
	if err != nil {
		t.Fatalf("ScheduledBackup not created: %v", err)
	}
	schedule, _, _ := unstructured.NestedString(sb.Object, "spec", "schedule")
	target, _, _ := unstructured.NestedString(sb.Object, "spec", "cluster", "name")
	if schedule != "0 0 3 * * *" || target != "v1-controldb" || len(sb.GetOwnerReferences()) != 1 {
		t.Errorf("ScheduledBackup = %+v owners %+v", sb.Object["spec"], sb.GetOwnerReferences())
	}
	if c := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "BackupReady"); c == nil || c.Reason != "BackupPending" {
		t.Errorf("BackupReady = %+v, want False/BackupPending before the first Backup", c)
	}

	stopped := time.Date(2026, 10, 6, 3, 1, 0, 0, time.UTC)
	backup := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "v1-controldb-backup-1", "namespace": "orama-system",
			"labels": map[string]interface{}{"cnpg.io/cluster": "v1-controldb"}},
		"status": map[string]interface{}{"phase": "completed", "stoppedAt": stopped.Format(time.RFC3339)},
	}}
	backup.SetGroupVersionKind(controlDBBackupGVK)
	if err := cl.Create(ctx, backup); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if c := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "BackupReady"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("BackupReady = %+v, want True after a completed Backup", c)
	}

	// Disabling backups removes the object store, the ScheduledBackup and the condition.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Backup.Enabled = nil })
	if _, found, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "backup"); found {
		t.Error("spec.backup left on the Cluster after backups were disabled")
	}
	if _, err := scheduledBackup(cl); !apierrors.IsNotFound(err) {
		t.Errorf("ScheduledBackup left after backups were disabled: %v", err)
	}
	if c := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "BackupReady"); c != nil {
		t.Errorf("BackupReady condition left after backups were disabled: %+v", c)
	}
}

func TestControlDBBackupInvalidConfigIsReportedNotApplied(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(withBackup)) // no Secret
	if _, found, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "backup"); found {
		t.Error("object store configured although the credentials Secret is missing")
	}
	if _, err := scheduledBackup(cl); !apierrors.IsNotFound(err) {
		t.Errorf("ScheduledBackup created for an invalid config: %v", err)
	}
	c := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "BackupReady")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "BackupSecretNotFound" {
		t.Errorf("BackupReady = %+v, want False/BackupSecretNotFound", c)
	}
}

func TestControlDBWithoutBackupRendersAsBefore(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	spec, _, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec")
	for _, k := range []string{"backup"} {
		if _, ok := spec[k]; ok {
			t.Errorf("old CR's Cluster gained spec.%s", k)
		}
	}
	if _, err := scheduledBackup(cl); !apierrors.IsNotFound(err) {
		t.Errorf("old CR got a ScheduledBackup: %v", err)
	}
	if c := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "BackupReady"); c != nil {
		t.Errorf("old CR got a BackupReady condition: %+v", c)
	}
}
