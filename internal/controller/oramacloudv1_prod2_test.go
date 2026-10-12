/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, oc)...).WithStatusSubresource(oc, &oramav1alpha1.CloudflareDNSRecord{}).Build()
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
	sb := newCNPGScheduledBackupFor("v1-controldb-backup", "orama-system")
	return sb, cl.Get(context.Background(), client.ObjectKeyFromObject(sb), sb)
}

func pvcTemplate(t *testing.T, sts *appsv1.StatefulSet) corev1.PersistentVolumeClaim {
	t.Helper()
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Fatalf("%s volumeClaimTemplates = %+v, want one named data", sts.Name, sts.Spec.VolumeClaimTemplates)
	}
	return sts.Spec.VolumeClaimTemplates[0]
}

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// --- 1. persistent readers ---

func TestReadersAreAStatefulSetWithPersistentData(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))

	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement || *sts.Spec.Replicas != 3 || sts.Spec.ServiceName != "v1-reader" {
		t.Errorf("reader StatefulSet spec = policy %q replicas %d service %q", sts.Spec.PodManagementPolicy, *sts.Spec.Replicas, sts.Spec.ServiceName)
	}
	pvc := pvcTemplate(t, sts)
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" || pvc.Spec.StorageClassName != nil {
		t.Errorf("reader PVC = %s class %v, want 10Gi on the default class", got.String(), pvc.Spec.StorageClassName)
	}
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			t.Errorf("reader pod still declares a data volume %+v; data must come from the PVC", v)
		}
	}
	if !absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Error("a fresh CR must not create a reader Deployment")
	}
	if !absent(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("no reader.hpa: no HPA expected")
	}
}

// A CR reconciled before #141 has an emptyDir reader Deployment. It keeps serving until every
// StatefulSet reader is Ready, and the status counts both until then.
func TestReaderDeploymentRetiredOnlyOnceStatefulSetIsReady(t *testing.T) {
	ctx := context.Background()
	oc := fullStackOCV1(nil)
	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: "orama-system", Labels: labelsFor(oc, "reader"),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))}},
		Spec:   appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
	}
	cl, r := reconcileOCV1(t, oc, old)
	if absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Fatal("old reader Deployment deleted before the StatefulSet readers were ready")
	}
	if c := findComponent(t, cl, "reader"); c.Ready || c.Detail != "3/6" {
		t.Errorf("reader component during the switch = %+v, want not ready 3/6 (old 3/3 + new 0/3)", c)
	}

	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	sts.Status.ReadyReplicas = 3
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if !absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Error("old reader Deployment not retired once the StatefulSet readers were ready")
	}
	if c := findComponent(t, cl, "reader"); !c.Ready || c.Detail != "3/3" {
		t.Errorf("reader component after the switch = %+v, want ready 3/3", c)
	}
}

func findComponent(t *testing.T, cl client.Client, name string) oramav1alpha1.ComponentStatus {
	t.Helper()
	oc := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})
	for _, c := range oc.Status.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %q not in status %+v", name, oc.Status.Components)
	return oramav1alpha1.ComponentStatus{}
}

func TestReaderHPATargetsStatefulSetAndOwnsReplicas(t *testing.T) {
	ctx := context.Background()
	cl, r := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.HPA = &oramav1alpha1.HPASpec{MinReplicas: 2, MaxReplicas: 6}
	}))
	hpa := getObj(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{})
	ref := hpa.Spec.ScaleTargetRef
	if ref.Kind != "StatefulSet" || ref.Name != "v1-reader" || ref.APIVersion != "apps/v1" ||
		*hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 6 ||
		*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != 70 {
		t.Errorf("HPA spec = %+v", hpa.Spec)
	}

	// The autoscaler scales the StatefulSet; a reconcile must not reset it.
	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	sts.Spec.Replicas = int32Ptr(5)
	if err := cl.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if got := *getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.Replicas; got != 5 {
		t.Errorf("reconcile reset HPA-managed replicas to %d, want 5", got)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.HPA = nil })
	if !absent(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("HPA not removed when reader.hpa was cleared")
	}
	if got := *getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.Replicas; got != 3 {
		t.Errorf("without an HPA the operator owns replicas again: got %d, want 3", got)
	}
}

// --- 2. configurable storage ---

func TestStorageSizeAndClassPerComponent(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		c := &o.Spec.Components
		c.Engine.Writer.Storage = oramav1alpha1.VolumeSpec{Size: quantity("200Gi"), StorageClassName: "premium"}
		c.Engine.Reader.Storage = oramav1alpha1.VolumeSpec{Size: quantity("150Gi"), StorageClassName: "premium"}
		c.Analytics.Storage = oramav1alpha1.VolumeSpec{Size: quantity("40Gi")}
		c.Broker.Storage = oramav1alpha1.VolumeSpec{StorageClassName: "standard"}
		c.ControlDB.Storage = oramav1alpha1.VolumeSpec{Size: quantity("30Gi"), StorageClassName: "premium"}
	}))
	for name, want := range map[string]struct{ size, class string }{
		"v1-writer":    {"200Gi", "premium"},
		"v1-reader":    {"150Gi", "premium"},
		"v1-analytics": {"40Gi", ""},
		"v1-broker":    {"10Gi", "standard"},
		"v1-cache":     {"10Gi", ""}, // unset stays at today's default
	} {
		pvc := pvcTemplate(t, getObj(t, cl, name, &appsv1.StatefulSet{}))
		size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		class := ""
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		if size.String() != want.size || class != want.class {
			t.Errorf("%s PVC = %s/%q, want %s/%q", name, size.String(), class, want.size, want.class)
		}
	}
	storage, _, _ := unstructured.NestedStringMap(controlDBCluster(t, cl).Object, "spec", "storage")
	if storage["size"] != "30Gi" || storage["storageClass"] != "premium" {
		t.Errorf("CNPG storage = %v, want 30Gi on premium", storage)
	}
}

func TestStorageDefaultsUnchangedForOldCR(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	for _, name := range []string{"v1-writer", "v1-analytics", "v1-cache", "v1-broker", "v1-search", "v1-harvest-mongo"} {
		pvc := pvcTemplate(t, getObj(t, cl, name, &appsv1.StatefulSet{}))
		if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" || pvc.Spec.StorageClassName != nil {
			t.Errorf("%s PVC = %s class %v, want the old 10Gi on the default class", name, got.String(), pvc.Spec.StorageClassName)
		}
	}
	storage, _, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "storage")
	if len(storage) != 1 || storage["size"] != "10Gi" {
		t.Errorf("CNPG storage = %v, want exactly {size: 10Gi}", storage)
	}
}

// volumeClaimTemplates are immutable: a size change on an existing StatefulSet must not fail the
// reconcile (which would stall every later component); the StatefulSet keeps its template.
func TestStorageChangeKeepsExistingVolumeClaimTemplates(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage.Size = quantity("500Gi")
	})
	pvc := pvcTemplate(t, getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" {
		t.Errorf("existing writer volumeClaimTemplate changed to %s; it must keep the created size", got.String())
	}
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
	o.Spec.Components.ControlDB.Backup = oramav1alpha1.PostgresBackupSpec{
		Enabled:     &enabled,
		Destination: oramav1alpha1.PostgresBackupDestination{Bucket: "prod2-backups"},
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
	backup.SetGroupVersionKind(cnpgBackupGVK)
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
	for _, k := range []string{"backup", "affinity"} {
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

// --- 4. deletion protection ---

func TestDeletionProtectionKeepsControlDBAndVolumes(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		withBackup(o)
		o.Spec.DeletionProtection = true
	}), backupSecret())

	if refs := controlDBCluster(t, cl).GetOwnerReferences(); len(refs) != 0 {
		t.Errorf("protected Cluster owner refs = %+v, want none (CR deletion must not GC it)", refs)
	}
	if sb, err := scheduledBackup(cl); err != nil || len(sb.GetOwnerReferences()) != 0 {
		t.Errorf("protected ScheduledBackup err=%v owners=%+v, want present and unowned", err, sb.GetOwnerReferences())
	}
	for _, name := range []string{"v1-writer", "v1-reader", "v1-analytics", "v1-harvest-mongo"} {
		p := getObj(t, cl, name, &appsv1.StatefulSet{}).Spec.PersistentVolumeClaimRetentionPolicy
		if p == nil || p.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType || p.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
			t.Errorf("%s PVC retention = %+v, want Retain/Retain", name, p)
		}
	}

	// Disabling the control DB keeps the Cluster and its ScheduledBackup.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = false })
	controlDBCluster(t, cl)
	if _, err := scheduledBackup(cl); err != nil {
		t.Errorf("protected ScheduledBackup deleted on disable: %v", err)
	}
}

// Turning protection on for a CR whose control DB disable lands in the same edit must still release
// the Cluster from the CR, so a later CR deletion can't garbage-collect it.
func TestDeletionProtectionReleasesClusterOwnedBefore(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	if len(controlDBCluster(t, cl).GetOwnerReferences()) != 1 {
		t.Fatal("an unprotected Cluster should be owned by the CR")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.DeletionProtection = true
		o.Spec.Components.ControlDB.Enabled = false
	})
	if refs := controlDBCluster(t, cl).GetOwnerReferences(); len(refs) != 0 {
		t.Errorf("Cluster still owned after protection was turned on: %+v", refs)
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = true })
	if refs := controlDBCluster(t, cl).GetOwnerReferences(); len(refs) != 0 {
		t.Errorf("re-enabled protected Cluster re-owned: %+v", refs)
	}
}

func TestWithoutDeletionProtectionBehavesAsBefore(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	if refs := controlDBCluster(t, cl).GetOwnerReferences(); len(refs) != 1 || refs[0].UID != "uid-v1" {
		t.Errorf("Cluster owner refs = %+v, want the CR", refs)
	}
	if p := getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.PersistentVolumeClaimRetentionPolicy; p != nil {
		t.Errorf("old CR's writer gained a PVC retention policy %+v", p)
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = false })
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if !absent(t, cl, "v1-controldb", cluster) {
		t.Error("unprotected control DB disable must still delete the Cluster")
	}
}

// --- 5. digest pinning ---

func TestDigestPinnedImagesArePulledIfNotPresent(t *testing.T) {
	const digest = "oramasearch/dashboard@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{
			"dashboard": digest,
			"oramacore": "oramasearch/oramacore:v1.2.3@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
	}))
	dash := getObj(t, cl, "v1-dashboard", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0]
	if dash.Image != digest || dash.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("dashboard image %q policy %q, want the digest ref pulled IfNotPresent", dash.Image, dash.ImagePullPolicy)
	}
	docs := getObj(t, cl, "v1-docs", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0]
	if docs.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("tag-only docs image policy %q, want Always (mutable tag, as before)", docs.ImagePullPolicy)
	}
	writer := getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Spec.Containers[0]
	if writer.Image != "oramasearch/oramacore:v1.2.3@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("writer image %q, want the digest override verbatim", writer.Image)
	}
}

func TestAppImagePullPolicy(t *testing.T) {
	for image, want := range map[string]corev1.PullPolicy{
		"oramasearch/dashboard:v1-sandbox":         corev1.PullAlways,
		"oramasearch/dashboard":                    corev1.PullAlways,
		"registry:5000/dashboard:v1":               corev1.PullAlways,
		"oramasearch/dashboard@sha256:abc":         corev1.PullIfNotPresent,
		"oramasearch/dashboard:v1@sha256:abc":      corev1.PullIfNotPresent,
		"registry:5000/team/dashboard@sha256:abcd": corev1.PullIfNotPresent,
	} {
		if got := appImagePullPolicy(image); got != want {
			t.Errorf("appImagePullPolicy(%q) = %q, want %q", image, got, want)
		}
	}
}

// --- 6. node placement ---

var prod2Placement = oramav1alpha1.PlacementSpec{
	NodeSelector: map[string]string{"agentpool": "prod2"},
	Tolerations:  []corev1.Toleration{{Key: "agentpool", Operator: corev1.TolerationOpEqual, Value: "prod2", Effect: corev1.TaintEffectNoSchedule}},
}

// podTemplates returns every pod spec the operator rendered, keyed by kind/name.
func podTemplates(t *testing.T, cl client.Client) map[string]corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	out := map[string]corev1.PodSpec{}
	var deps appsv1.DeploymentList
	var sts appsv1.StatefulSetList
	var jobs batchv1.JobList
	for _, l := range []client.ObjectList{&deps, &sts, &jobs} {
		if err := cl.List(ctx, l, client.InNamespace("orama-system")); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range deps.Items {
		out["Deployment/"+d.Name] = d.Spec.Template.Spec
	}
	for _, s := range sts.Items {
		out["StatefulSet/"+s.Name] = s.Spec.Template.Spec
	}
	for _, j := range jobs.Items {
		out["Job/"+j.Name] = j.Spec.Template.Spec
	}
	return out
}

func TestPlacementAppliesToEveryPod(t *testing.T) {
	gpuPool := map[string]string{"agentpool": "gpu"}
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Placement = prod2Placement
		o.Spec.Components.Engine.Writer.GPU = false
	}))
	pods := podTemplates(t, cl)
	if len(pods) < 20 {
		t.Fatalf("only %d pod templates rendered; the full stack should render 20+", len(pods))
	}
	for name, p := range pods {
		if p.NodeSelector["agentpool"] != "prod2" || len(p.Tolerations) != 1 || p.Tolerations[0].Value != "prod2" {
			t.Errorf("%s placement = %v / %v, want the prod2 pool", name, p.NodeSelector, p.Tolerations)
		}
	}
	if _, ok := pods["Job/v1-demo-seed"]; !ok {
		t.Error("demo-seed Job not rendered")
	}

	ps := getObj(t, cl, "v1-controldb-schema", &oramav1alpha1.PostgresSchema{})
	job := pgMigrationJobSpec(ps).Template.Spec
	if job.NodeSelector["agentpool"] != "prod2" || len(job.Tolerations) != 1 {
		t.Errorf("PostgresSchema migration Job placement = %v / %v", job.NodeSelector, job.Tolerations)
	}
	affinity, _, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "affinity")
	sel, _, _ := unstructured.NestedStringMap(affinity, "nodeSelector")
	tols, _, _ := unstructured.NestedSlice(affinity, "tolerations")
	if sel["agentpool"] != "prod2" || len(tols) != 1 || tols[0].(map[string]interface{})["value"] != "prod2" {
		t.Errorf("CNPG affinity = %v", affinity)
	}

	// Writer settings take precedence over the CR-level placement (writer + GPU embeddings).
	cl, _ = reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Placement = prod2Placement
		o.Spec.Components.Engine.Writer.GPU = true
		o.Spec.Components.Engine.Writer.NodeSelector = gpuPool
	}))
	pods = podTemplates(t, cl)
	for _, name := range []string{"StatefulSet/v1-writer", "Deployment/v1-embeddings"} {
		p := pods[name]
		if p.NodeSelector["agentpool"] != "gpu" || len(p.Tolerations) != 1 || p.Tolerations[0].Value != "prod2" {
			t.Errorf("%s placement = %v / %v, want the writer nodeSelector + the CR tolerations", name, p.NodeSelector, p.Tolerations)
		}
	}
	if p := pods["StatefulSet/v1-reader"]; p.NodeSelector["agentpool"] != "prod2" {
		t.Errorf("reader placement = %v, want the CR-level prod2 pool", p.NodeSelector)
	}
}

func TestNoPlacementRendersAsBefore(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.GPU = true
		o.Spec.Components.Engine.Writer.NodeSelector = map[string]string{"gpu": "true"}
	}))
	for name, p := range podTemplates(t, cl) {
		wantSelector := name == "StatefulSet/v1-writer" || name == "Deployment/v1-embeddings"
		if (p.NodeSelector != nil) != wantSelector || p.Tolerations != nil {
			t.Errorf("%s placement = %v / %v; without spec.placement only the writer (and GPU embeddings) carry a selector", name, p.NodeSelector, p.Tolerations)
		}
	}
	ps := getObj(t, cl, "v1-controldb-schema", &oramav1alpha1.PostgresSchema{})
	if ps.Spec.NodeSelector != nil || ps.Spec.Tolerations != nil {
		t.Errorf("old CR's PostgresSchema gained placement %+v", ps.Spec)
	}
}

// --- 7. the Prod 2 sample ---

// The sample must decode strictly into the API (no misspelt fields silently dropped) and carry the
// decided Prod 2 shape.
func TestProd2SampleMatchesDecisions(t *testing.T) {
	raw, err := os.ReadFile("../../config/samples/prod2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	js, err := utilyaml.ToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	var oc oramav1alpha1.OramaCloudV1
	if err := dec.Decode(&oc); err != nil {
		t.Fatalf("prod2.yaml does not decode strictly: %v", err)
	}
	c := oc.Spec.Components
	b := c.ControlDB.Backup
	switch {
	case oc.Namespace != "orama-prod2" || oc.Spec.Profile != "medium" || !oc.Spec.DeletionProtection:
		t.Errorf("namespace/profile/protection = %s/%s/%v", oc.Namespace, oc.Spec.Profile, oc.Spec.DeletionProtection)
	case oc.Spec.Placement.NodeSelector["agentpool"] != "prod2" || len(oc.Spec.Placement.Tolerations) != 1 || oc.Spec.Placement.Tolerations[0].Value != "prod2":
		t.Errorf("placement = %+v", oc.Spec.Placement)
	case c.Dashboard.Host != "app.orama.ing" || c.Docs.Host != "docs.orama.ing":
		t.Errorf("hosts = %s, %s", c.Dashboard.Host, c.Docs.Host)
	case b.Enabled == nil || !*b.Enabled || b.Destination.Bucket == "" || b.SecretRef.Name == "":
		t.Errorf("control-DB backup = %+v", b)
	case oc.Spec.IDP.Provider != "firebase" || oc.Spec.IDP.ConfigSecretRef == nil || oc.Spec.IDP.ConfigSecretRef.Name != "prod2-firebase":
		t.Errorf("Firebase provider/reference = %+v", oc.Spec.IDP)
	case c.Engine.Reader.Host != "collections.orama.ing" || c.Engine.Writer.Host != "writer.prod2.cluster.orama.ing":
		t.Errorf("engine public endpoints are incorrect")
	case c.Engine.Writer.JWT == nil || c.Engine.Writer.JWT.JWKSURL != "https://app.orama.ing/api/.well-known/jwks.json" || c.Engine.Writer.JWT.Audience != "https://writer.prod2.cluster.orama.ing" || oc.Spec.JWTIssuer != "orama-cloud":
		t.Errorf("Prod 2 JWT config is incorrect")
	case c.Analytics.Mode != "external" || c.Analytics.CredentialsSecretRef == nil || c.Analytics.CredentialsSecretRef.Name != "prod2-clickhouse":
		t.Errorf("ClickHouse must stay external with referenced credentials")
	case !c.AnalyticsProcessor.Import.Enabled || c.AnalyticsProcessor.Import.Schedule != "*/5 * * * *":
		t.Errorf("scheduled analytics import is missing")
	case c.ControlDB.Mode != "in-cluster" || !c.Engine.Enabled:
		t.Errorf("controlDB mode %q engine %v", c.ControlDB.Mode, c.Engine.Enabled)
	}
}
