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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	rehearsalTestSourceImage = "ghcr.io/cloudnative-pg/postgresql:18.1@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	rehearsalTestTargetImage = "ghcr.io/cloudnative-pg/postgresql:19.0@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestLakeRestoreRehearsalTestsBackupAndRestore(t *testing.T) {
	fixture := newRehearsalFixture(t, "restore", 1)
	scratch := advanceScratchRehearsalToReadyCluster(t, fixture)
	assertRecoveryCluster(t, scratch, "backup-20260905", rehearsalTestSourceImage)

	setCNPGClusterReady(t, fixture.client, scratch, "postgres-1", rehearsalTestSourceImage)
	reconcileRehearsal(t, fixture)
	job := getRehearsalJob(t, fixture, "verify", fixture.scratchNamespace())
	container := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(container.Command[2], "diff -u") || !jobEnvHas(container.Env, "QUERY", "agent_guard") {
		t.Fatalf("restore verification Job = %+v", container)
	}
	completeJob(t, fixture.client, job, true)
	reconcileRehearsal(t, fixture)

	rehearsal := getRehearsal(t, fixture)
	if rehearsal.Status.Phase != "Succeeded" || rehearsal.Status.CompletedAt == nil ||
		!strings.Contains(rehearsal.Status.Message, "backup-20260905") {
		t.Fatalf("rehearsal status = %+v", rehearsal.Status)
	}
	lake := getRehearsalLake(t, fixture)
	postgres := lake.Status.DataServices[0]
	if postgres.Backup != "tested" || postgres.Restore != "tested" ||
		postgres.Upgrade != "untested" || postgres.Failover != "none" ||
		postgres.Managed || postgres.LastVerified == nil {
		t.Fatalf("postgres data service = %+v", postgres)
	}
	assertScratchObjectsDeleted(t, fixture)
}

func TestLakeRestoreRehearsalFailureLeavesEvidenceUntested(t *testing.T) {
	fixture := newRehearsalFixture(t, "restore", 1)
	lake := getRehearsalLake(t, fixture)
	lake.Status.DataServices = []oramav1alpha1.LakeDataServiceStatus{{
		Name:     "postgres",
		Kind:     "postgres",
		Backup:   "tested",
		Restore:  "tested",
		Upgrade:  "untested",
		Failover: "none",
	}}
	if err := fixture.client.Status().Update(context.Background(), lake); err != nil {
		t.Fatal(err)
	}
	scratch := advanceScratchRehearsalToReadyCluster(t, fixture)
	setCNPGClusterReady(t, fixture.client, scratch, "postgres-1", rehearsalTestSourceImage)
	reconcileRehearsal(t, fixture)
	job := getRehearsalJob(t, fixture, "verify", fixture.scratchNamespace())
	completeJob(t, fixture.client, job, false)
	reconcileRehearsal(t, fixture)

	rehearsal := getRehearsal(t, fixture)
	if rehearsal.Status.Phase != "Failed" || rehearsal.Status.Message != "verification Job failed" {
		t.Fatalf("rehearsal status = %+v", rehearsal.Status)
	}
	postgres := getRehearsalLake(t, fixture).Status.DataServices[0]
	if postgres.Backup != "untested" || postgres.Restore != "untested" ||
		!strings.Contains(postgres.Notes, "last restore rehearsal failed") || postgres.LastVerified == nil {
		t.Fatalf("postgres data service = %+v", postgres)
	}
	assertScratchObjectsDeleted(t, fixture)
}

func TestLakeUpgradeRehearsalUpdatesScratchCloneFirst(t *testing.T) {
	fixture := newRehearsalFixture(t, "upgrade", 2)
	scratch := advanceScratchRehearsalToReadyCluster(t, fixture)
	setCNPGClusterReady(t, fixture.client, scratch, "postgres-1", rehearsalTestSourceImage)
	reconcileRehearsal(t, fixture)

	if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(scratch), scratch); err != nil {
		t.Fatal(err)
	}
	image, _, _ := unstructured.NestedString(scratch.Object, "spec", "imageName")
	strategy, _, _ := unstructured.NestedString(scratch.Object, "spec", "primaryUpdateStrategy")
	method, _, _ := unstructured.NestedString(scratch.Object, "spec", "primaryUpdateMethod")
	if image != fixture.targetImage || strategy != "unsupervised" || method != "switchover" {
		t.Fatalf("scratch upgrade spec = %+v", scratch.Object["spec"])
	}
	setCNPGClusterReady(t, fixture.client, scratch, "postgres-2", fixture.targetImage)
	reconcileRehearsal(t, fixture)
	job := getRehearsalJob(t, fixture, "verify", fixture.scratchNamespace())
	completeJob(t, fixture.client, job, true)
	reconcileRehearsal(t, fixture)

	postgres := getRehearsalLake(t, fixture).Status.DataServices[0]
	if postgres.Upgrade != "tested" || postgres.Managed {
		t.Fatalf("postgres data service = %+v", postgres)
	}
	assertScratchObjectsDeleted(t, fixture)
}

func TestLakeFailoverRehearsalSwitchesPrimaryAndTestsReadOnlySecret(t *testing.T) {
	fixture := newRehearsalFixture(t, "failover", 2)
	lake := getRehearsalLake(t, fixture)
	lake.Status.DataServices = []oramav1alpha1.LakeDataServiceStatus{{
		Name:     "postgres",
		Kind:     "postgres",
		Backup:   "tested",
		Restore:  "tested",
		Upgrade:  "tested",
		Failover: "untested",
	}}
	if err := fixture.client.Status().Update(context.Background(), lake); err != nil {
		t.Fatal(err)
	}

	reconcileRehearsal(t, fixture)
	reconcileRehearsal(t, fixture)
	rehearsal := getRehearsal(t, fixture)
	if rehearsal.Status.PreviousPrimary != "lake-test-controldb-1" ||
		rehearsal.Status.TargetPrimary != "lake-test-controldb-2" {
		t.Fatalf("selected primary status = %+v", rehearsal.Status)
	}
	reconcileRehearsal(t, fixture)

	cluster := getSourceCluster(t, fixture)
	target, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	if target != "lake-test-controldb-2" || phase != "Switchover in progress" {
		t.Fatalf("switchover status = %+v", cluster.Object["status"])
	}
	setCNPGClusterReady(t, fixture.client, cluster, "lake-test-controldb-2", rehearsalTestSourceImage)
	reconcileRehearsal(t, fixture)
	job := getRehearsalJob(t, fixture, lakeRehearsalJobName(fixture.name), "lake-test")
	container := job.Spec.Template.Spec.Containers[0]
	if !jobEnvSecretIs(container.Env, "PGPASSWORD", "lake-test-controldb-rehearsal") {
		t.Fatalf("failover verification env = %+v", container.Env)
	}
	completeJob(t, fixture.client, job, true)
	reconcileRehearsal(t, fixture)

	lake = getRehearsalLake(t, fixture)
	postgres := lake.Status.DataServices[0]
	if postgres.Failover != "tested" || !postgres.Managed ||
		lake.Status.DataServicesSummary != "1 managed, 1 unmanaged" {
		t.Fatalf("postgres data service = %+v, summary = %q", postgres, lake.Status.DataServicesSummary)
	}
	if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(job), job); err == nil {
		t.Fatal("failover verification Job was not deleted")
	}
}

func TestLakeFailoverRehearsalRejectsSingleInstance(t *testing.T) {
	fixture := newRehearsalFixture(t, "failover", 1)
	reconcileRehearsal(t, fixture)
	rehearsal := getRehearsal(t, fixture)
	if rehearsal.Status.Phase != "Failed" || !strings.Contains(rehearsal.Status.Message, "at least 2") {
		t.Fatalf("rehearsal status = %+v", rehearsal.Status)
	}
	postgres := getRehearsalLake(t, fixture).Status.DataServices[0]
	if postgres.Failover != "none" || postgres.Managed ||
		!strings.Contains(postgres.Notes, "single-instance") {
		t.Fatalf("postgres data service = %+v", postgres)
	}
}

type rehearsalFixture struct {
	t             *testing.T
	client        client.Client
	scheme        *runtime.Scheme
	name          string
	targetImage   string
	sourceCluster *unstructured.Unstructured
}

func newRehearsalFixture(t *testing.T, operation string, instances int32) *rehearsalFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	enabled := true
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake-test",
			Image:     "oramasearch/lake:test",
			Postgres: oramav1alpha1.LakePostgresSpec{
				Instances: &instances,
				Backup: oramav1alpha1.LakePostgresBackupSpec{
					Enabled: &enabled,
					Destination: oramav1alpha1.LakePostgresBackupDestination{
						Bucket: "lake-backups",
						Path:   "lake-test/controldb",
					},
					SecretRef: corev1.LocalObjectReference{Name: "r2"},
				},
			},
		},
	}
	targetImage := rehearsalTestTargetImage
	rehearsal := &oramav1alpha1.LakeRestoreRehearsal{
		ObjectMeta: metav1.ObjectMeta{Name: operation + "-test", UID: types.UID(operation + "-uid")},
		Spec: oramav1alpha1.LakeRestoreRehearsalSpec{
			LakeRef:     "test",
			Service:     "postgres",
			Operation:   operation,
			From:        "latest",
			TargetImage: targetImage,
		},
	}
	sourceCluster := testCNPGCluster("lake-test", "lake-test-controldb", int64(instances))
	setCNPGStatus(sourceCluster, "lake-test-controldb-1", rehearsalTestSourceImage, instances)
	backup := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              "backup-20260905",
			"namespace":         "lake-test",
			"creationTimestamp": time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"labels": map[string]interface{}{
				"cnpg.io/cluster": "lake-test-controldb",
			},
		},
		"status": map[string]interface{}{
			"phase":    "completed",
			"backupId": "20260905T120000",
		},
	}}
	backup.SetGroupVersionKind(cnpgBackupGVK)
	r2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "lake-test"},
		Data: map[string][]byte{
			"s3ApiEndpoint":   []byte("https://account.r2.cloudflarestorage.com"),
			"accessKeyId":     []byte("access"),
			"secretAccessKey": []byte("secret"),
		},
	}
	app := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-test-controldb-rehearsal", Namespace: "lake-test"},
		Data: map[string][]byte{
			"username": []byte(lakeRehearsalRoleName),
			"password": []byte("password"),
		},
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake, rehearsal, sourceCluster, backup, r2, app).
		WithStatusSubresource(lake, rehearsal, sourceCluster, &batchv1.Job{})
	return &rehearsalFixture{
		t:             t,
		client:        builder.Build(),
		scheme:        scheme,
		name:          rehearsal.Name,
		targetImage:   targetImage,
		sourceCluster: sourceCluster,
	}
}

func reconcileRehearsal(t *testing.T, fixture *rehearsalFixture) {
	t.Helper()
	reconciler := &LakeRestoreRehearsalReconciler{
		Client:    fixture.client,
		APIReader: fixture.client,
		Scheme:    fixture.scheme,
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: fixture.name},
	}); err != nil {
		t.Fatalf("reconcile rehearsal: %v", err)
	}
}

func advanceScratchRehearsalToReadyCluster(t *testing.T, fixture *rehearsalFixture) *unstructured.Unstructured {
	t.Helper()
	reconcileRehearsal(t, fixture)
	reconcileRehearsal(t, fixture)
	reconcileRehearsal(t, fixture)
	scratch := &unstructured.Unstructured{}
	scratch.SetGroupVersionKind(cnpgClusterGVK)
	key := client.ObjectKey{Namespace: fixture.scratchNamespace(), Name: "postgres"}
	if err := fixture.client.Get(context.Background(), key, scratch); err != nil {
		t.Fatalf("scratch Cluster was not created: %v", err)
	}
	return scratch
}

func (fixture *rehearsalFixture) scratchNamespace() string {
	return lakeRehearsalNamespace(fixture.name)
}

func getRehearsal(t *testing.T, fixture *rehearsalFixture) *oramav1alpha1.LakeRestoreRehearsal {
	t.Helper()
	var rehearsal oramav1alpha1.LakeRestoreRehearsal
	if err := fixture.client.Get(context.Background(), client.ObjectKey{Name: fixture.name}, &rehearsal); err != nil {
		t.Fatal(err)
	}
	return &rehearsal
}

func getRehearsalLake(t *testing.T, fixture *rehearsalFixture) *oramav1alpha1.Lake {
	t.Helper()
	var lake oramav1alpha1.Lake
	if err := fixture.client.Get(context.Background(), client.ObjectKey{Name: "test"}, &lake); err != nil {
		t.Fatal(err)
	}
	return &lake
}

func getSourceCluster(t *testing.T, fixture *rehearsalFixture) *unstructured.Unstructured {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := fixture.client.Get(context.Background(), client.ObjectKey{
		Namespace: "lake-test",
		Name:      "lake-test-controldb",
	}, cluster); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func testCNPGCluster(namespace string, name string, instances int64) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"instances": instances,
		},
	}}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	return cluster
}

func setCNPGStatus(cluster *unstructured.Unstructured, primary string, image string, instances int32) {
	healthy := make([]interface{}, 0, instances)
	for index := int32(1); index <= instances; index++ {
		healthy = append(healthy, cluster.GetName()+"-"+string(rune('0'+index)))
	}
	cluster.Object["status"] = map[string]interface{}{
		"readyInstances": int64(instances),
		"instances":      int64(instances),
		"phase":          cnpgHealthyPhase,
		"currentPrimary": primary,
		"image":          image,
		"instancesStatus": map[string]interface{}{
			"healthy": healthy,
		},
	}
}

func setCNPGClusterReady(
	t *testing.T,
	c client.Client,
	cluster *unstructured.Unstructured,
	primary string,
	image string,
) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	setCNPGStatus(cluster, primary, image, int32(instances))
	if err := c.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("update Cluster status: %v", err)
	}
}

func assertRecoveryCluster(t *testing.T, cluster *unstructured.Unstructured, backup string, image string) {
	t.Helper()
	backupID, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID")
	imageName, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	if backupID != "20260905T120000" || imageName != image {
		t.Fatalf("scratch recovery spec = %+v, selected Backup = %s", cluster.Object["spec"], backup)
	}
	external, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	barman := external[0].(map[string]interface{})["barmanObjectStore"].(map[string]interface{})
	if barman["serverName"] != "lake-test-controldb" {
		t.Fatalf("barman object store = %+v", barman)
	}
}

func getRehearsalJob(t *testing.T, fixture *rehearsalFixture, name string, namespace string) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := fixture.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &job); err != nil {
		t.Fatalf("get verification Job: %v", err)
	}
	return &job
}

func completeJob(t *testing.T, c client.Client, job *batchv1.Job, succeeded bool) {
	t.Helper()
	if succeeded {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
	}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("complete Job: %v", err)
	}
}

func jobEnvHas(env []corev1.EnvVar, name string, valuePart string) bool {
	for _, variable := range env {
		if variable.Name == name && strings.Contains(variable.Value, valuePart) {
			return true
		}
	}
	return false
}

func jobEnvSecretIs(env []corev1.EnvVar, name string, secretName string) bool {
	for _, variable := range env {
		if variable.Name == name && variable.ValueFrom != nil &&
			variable.ValueFrom.SecretKeyRef != nil && variable.ValueFrom.SecretKeyRef.Name == secretName {
			return true
		}
	}
	return false
}

func assertScratchObjectsDeleted(t *testing.T, fixture *rehearsalFixture) {
	t.Helper()
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fixture.scratchNamespace()}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "object-store", Namespace: fixture.scratchNamespace()}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source-db", Namespace: fixture.scratchNamespace()}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "verify", Namespace: fixture.scratchNamespace()}},
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("postgres")
	cluster.SetNamespace(fixture.scratchNamespace())
	objects = append(objects, cluster)
	for _, object := range objects {
		if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(object), object); err == nil {
			t.Errorf("scratch object %T %s still exists", object, client.ObjectKeyFromObject(object))
		}
	}
}
