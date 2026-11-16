// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const storeTestNamespace = "lake-shared"

// storeTestSnapshot is a VolumeSnapshot as the lake-volume-snapshots controller takes it of the
// store claim, bound to content <name>-content.
func storeTestSnapshot(name string, at time.Time, ready bool, labels map[string]interface{}) *unstructured.Unstructured {
	if labels == nil {
		labels = map[string]interface{}{
			snapshotTargetLabel:     snapshotStoreTarget,
			workspaceLakeLabel:      "shared",
			workspaceManagedByLabel: "orama-operator",
		}
	}
	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":        name,
			"namespace":   storeTestNamespace,
			"labels":      labels,
			"annotations": map[string]interface{}{snapshotAtAnnotation: at.Format(time.RFC3339Nano)},
		},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": "lake-retain",
			"source":                  map[string]interface{}{"persistentVolumeClaimName": "lake-shared-store"},
		},
		"status": map[string]interface{}{
			"readyToUse":                     ready,
			"restoreSize":                    "20Gi",
			"boundVolumeSnapshotContentName": name + "-content",
		},
	}}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	return snapshot
}

func storeTestContent(name string, handle string) *unstructured.Unstructured {
	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"deletionPolicy": "Retain",
			"driver":         "disk.csi.azure.com",
			"source":         map[string]interface{}{"volumeHandle": "live-disk"},
		},
		"status": map[string]interface{}{"snapshotHandle": handle, "readyToUse": true},
	}}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	return content
}

type storeRehearsalFixture struct {
	client client.Client
	scheme *runtime.Scheme
	name   string
}

func newStoreRehearsalFixture(t *testing.T, from string, extra ...client.Object) *storeRehearsalFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: storeTestNamespace, Image: "oramasearch/lake:test"},
	}
	class := "managed-csi"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-shared-store", Namespace: storeTestNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
		},
	}
	rehearsal := &oramav1alpha1.LakeRestoreRehearsal{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-store-restore", UID: types.UID("store-uid")},
		Spec:       oramav1alpha1.LakeRestoreRehearsalSpec{LakeRef: "shared", Service: "store", Operation: "restore", From: from},
	}
	day := time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)
	objects := []client.Object{
		lake, claim, rehearsal,
		storeTestSnapshot("lake-shared-store-20261023t030000z", day, true, nil),
		storeTestContent("lake-shared-store-20261023t030000z-content", "snapshot-23"),
		storeTestSnapshot("lake-shared-store-20261024t030000z", day.Add(24*time.Hour), true, nil),
		storeTestContent("lake-shared-store-20261024t030000z-content", "snapshot-24"),
		// A newer snapshot still being taken is never selected.
		storeTestSnapshot("lake-shared-store-20261024t040000z", day.Add(25*time.Hour), false, nil),
	}
	objects = append(objects, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(lake, rehearsal, &batchv1.Job{}).Build()
	return &storeRehearsalFixture{client: c, scheme: scheme, name: rehearsal.Name}
}

func (f *storeRehearsalFixture) reconcile(t *testing.T) {
	t.Helper()
	reconcileRehearsal(t, &rehearsalFixture{t: t, client: f.client, scheme: f.scheme, name: f.name})
}

func (f *storeRehearsalFixture) rehearsal(t *testing.T) *oramav1alpha1.LakeRestoreRehearsal {
	t.Helper()
	var rehearsal oramav1alpha1.LakeRestoreRehearsal
	if err := f.client.Get(context.Background(), client.ObjectKey{Name: f.name}, &rehearsal); err != nil {
		t.Fatal(err)
	}
	return &rehearsal
}

// The store rehearsal restores the newest ready store snapshot into a scratch namespace through a
// new Retain content bound to the same cloud snapshot, reads every file of the restored volume
// read-only, records the verifier's report, and deletes everything it made. The live claim, the
// live snapshots and their contents are never changed, and the Lake's Postgres evidence is left
// alone.
func TestLakeStoreRehearsalRestoresTheNewestReadySnapshotIntoScratch(t *testing.T) {
	f := newStoreRehearsalFixture(t, "latest")
	ctx := context.Background()
	f.reconcile(t) // accepted
	f.reconcile(t) // selects
	if got := f.rehearsal(t).Status.SelectedSnapshot; got != "lake-shared-store-20261024t030000z" {
		t.Fatalf("selected %q, want the newest ready snapshot", got)
	}
	f.reconcile(t) // namespace, content, snapshot
	scratch := lakeRehearsalNamespace(f.name)

	var namespace corev1.Namespace
	if err := f.client.Get(ctx, client.ObjectKey{Name: scratch}, &namespace); err != nil {
		t.Fatalf("scratch namespace: %v", err)
	}
	if namespace.Labels[lakeRehearsalLakeLabel] != "shared" {
		t.Fatalf("scratch namespace labels = %v", namespace.Labels)
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := f.client.Get(ctx, client.ObjectKey{Name: storeRehearsalContentName(f.name)}, content); err != nil {
		t.Fatalf("rehearsal content: %v", err)
	}
	policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy")
	handle, _, _ := unstructured.NestedString(content.Object, "spec", "source", "snapshotHandle")
	refNamespace, _, _ := unstructured.NestedString(content.Object, "spec", "volumeSnapshotRef", "namespace")
	if policy != "Retain" || handle != "snapshot-24" || refNamespace != scratch {
		t.Fatalf("rehearsal content spec = %+v", content.Object["spec"])
	}
	snapshot := newStoreRehearsalSnapshot(f.rehearsal(t))
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); err != nil {
		t.Fatalf("rehearsal snapshot: %v", err)
	}
	var claim corev1.PersistentVolumeClaim
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: storeRehearsalClaim}, &claim); err == nil {
		t.Fatal("the claim was created before its snapshot was ready")
	}

	if err := unstructured.SetNestedField(snapshot.Object, true, "status", "readyToUse"); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Update(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t) // claim and verifier
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: storeRehearsalClaim}, &claim); err != nil {
		t.Fatalf("restored claim: %v", err)
	}
	size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if claim.Spec.DataSource == nil || claim.Spec.DataSource.Kind != "VolumeSnapshot" || claim.Spec.DataSource.Name != storeRehearsalSnapshot ||
		claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "managed-csi" || size.String() != "20Gi" {
		t.Fatalf("restored claim spec = %+v", claim.Spec)
	}
	var job batchv1.Job
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: "verify"}, &job); err != nil {
		t.Fatalf("verifier: %v", err)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		len(pod.Volumes) != 2 || pod.Volumes[0].PersistentVolumeClaim == nil || !pod.Volumes[0].PersistentVolumeClaim.ReadOnly ||
		!pod.Containers[0].VolumeMounts[0].ReadOnly || pod.Containers[0].Image != lakeRehearsalVerifyImage ||
		!strings.Contains(pod.Containers[0].Command[2], "-exec cat") {
		t.Fatalf("verifier pod = %+v", pod)
	}

	report := "files=412 kib=81234 top=registry transparency tuf"
	verifier := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-abcde", Namespace: scratch, Labels: map[string]string{"batch.kubernetes.io/job-name": "verify"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "verify",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: report}},
		}}},
	}
	if err := f.client.Create(ctx, verifier); err != nil {
		t.Fatal(err)
	}
	completeJob(t, f.client, &job, true)
	f.reconcile(t)

	rehearsal := f.rehearsal(t)
	if rehearsal.Status.Phase != "Succeeded" || rehearsal.Status.Evidence != report || rehearsal.Status.CompletedAt == nil ||
		!strings.Contains(rehearsal.Status.Message, "lake-shared-store-20261024t030000z") {
		t.Fatalf("rehearsal status = %+v", rehearsal.Status)
	}
	var lake oramav1alpha1.Lake
	if err := f.client.Get(ctx, client.ObjectKey{Name: "shared"}, &lake); err != nil {
		t.Fatal(err)
	}
	if len(lake.Status.DataServices) != 0 {
		t.Fatalf("a store rehearsal changed the Lake's Postgres evidence: %+v", lake.Status.DataServices)
	}

	// Everything the rehearsal made is gone; the live snapshots and contents are untouched.
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: storeRehearsalClaim}, &corev1.PersistentVolumeClaim{}); err == nil {
		t.Error("the restored claim still exists")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: "verify"}, &batchv1.Job{}); err == nil {
		t.Error("the verifier Job still exists")
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot.DeepCopy()); err == nil {
		t.Error("the rehearsal snapshot still exists")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: storeRehearsalContentName(f.name)}, content.DeepCopy()); err == nil {
		t.Error("the rehearsal content still exists")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: scratch}, &corev1.Namespace{}); err == nil {
		t.Error("the scratch namespace still exists")
	}
	for _, name := range []string{"lake-shared-store-20261023t030000z-content", "lake-shared-store-20261024t030000z-content"} {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(volumeSnapshotContentGVK)
		if err := f.client.Get(ctx, client.ObjectKey{Name: name}, live); err != nil {
			t.Fatalf("live content %s was deleted: %v", name, err)
		}
		if policy, _, _ := unstructured.NestedString(live.Object, "spec", "deletionPolicy"); policy != "Retain" {
			t.Fatalf("live content %s was changed: %+v", name, live.Object["spec"])
		}
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(volumeSnapshotGVK)
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: storeTestNamespace, Name: "lake-shared-store-20261024t030000z"}, live); err != nil {
		t.Fatalf("live snapshot was deleted: %v", err)
	}
	var liveClaim corev1.PersistentVolumeClaim
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: storeTestNamespace, Name: "lake-shared-store"}, &liveClaim); err != nil || liveClaim.Spec.DataSource != nil {
		t.Fatalf("live store claim changed: %v %+v", err, liveClaim.Spec)
	}
}

// A failed verifier fails the rehearsal with the verifier's own words, and still cleans up.
func TestLakeStoreRehearsalFailureCarriesTheVerifierReport(t *testing.T) {
	f := newStoreRehearsalFixture(t, "lake-shared-store-20261023t030000z")
	ctx := context.Background()
	f.reconcile(t)
	f.reconcile(t)
	if got := f.rehearsal(t).Status.SelectedSnapshot; got != "lake-shared-store-20261023t030000z" {
		t.Fatalf("selected %q, want the named snapshot", got)
	}
	f.reconcile(t)
	snapshot := newStoreRehearsalSnapshot(f.rehearsal(t))
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(snapshot.Object, true, "status", "readyToUse")
	if err := f.client.Update(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	scratch := lakeRehearsalNamespace(f.name)
	var job batchv1.Job
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: scratch, Name: "verify"}, &job); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-x", Namespace: scratch, Labels: map[string]string{"batch.kubernetes.io/job-name": "verify"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "verify",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "the restored store has no files"}}}}},
	}); err != nil {
		t.Fatal(err)
	}
	completeJob(t, f.client, &job, false)
	f.reconcile(t)
	rehearsal := f.rehearsal(t)
	if rehearsal.Status.Phase != "Failed" || !strings.Contains(rehearsal.Status.Message, "the restored store has no files") {
		t.Fatalf("rehearsal status = %+v", rehearsal.Status)
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: scratch}, &corev1.Namespace{}); err == nil {
		t.Error("the scratch namespace still exists")
	}
}

// Nothing is restored from a snapshot the operator did not take of this Lake's store, or when
// there is no ready one; the rehearsal fails at once with the reason and makes nothing.
func TestLakeStoreRehearsalRefusesWhatIsNotThisStoresSnapshot(t *testing.T) {
	foreign := storeTestSnapshot("hand-made", time.Date(2026, 10, 24, 5, 0, 0, 0, time.UTC), true,
		map[string]interface{}{snapshotTargetLabel: snapshotStoreTarget, workspaceLakeLabel: "another"})
	for name, from := range map[string]string{"foreign": "hand-made", "missing": "nope"} {
		f := newStoreRehearsalFixture(t, from, foreign)
		f.reconcile(t)
		f.reconcile(t)
		rehearsal := f.rehearsal(t)
		if rehearsal.Status.Phase != "Failed" || rehearsal.Status.SelectedSnapshot != "" {
			t.Fatalf("%s: rehearsal status = %+v", name, rehearsal.Status)
		}
		content := &unstructured.Unstructured{}
		content.SetGroupVersionKind(volumeSnapshotContentGVK)
		if err := f.client.Get(context.Background(), client.ObjectKey{Name: storeRehearsalContentName(f.name)}, content); err == nil {
			t.Fatalf("%s: a content was made", name)
		}
	}

	// No ready snapshot at all.
	f := newStoreRehearsalFixture(t, "latest")
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := f.client.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		_ = unstructured.SetNestedField(list.Items[i].Object, false, "status", "readyToUse")
		if err := f.client.Update(context.Background(), &list.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	f.reconcile(t)
	f.reconcile(t)
	if rehearsal := f.rehearsal(t); rehearsal.Status.Phase != "Failed" || !strings.Contains(rehearsal.Status.Message, "no ready VolumeSnapshot") {
		t.Fatalf("no ready snapshot: %+v", rehearsal.Status)
	}
}

// The content is pre-provisioned Retain and refused for deletion otherwise: deleting a Delete
// content bound to the live snapshot's cloud handle would delete the live backup.
func TestLakeStoreRehearsalNeverDeletesAContentThatIsNotItsRetainContent(t *testing.T) {
	f := newStoreRehearsalFixture(t, "latest")
	rehearsal := f.rehearsal(t)
	content := storeTestContent(storeRehearsalContentName(f.name), "snapshot-24")
	_ = unstructured.SetNestedField(content.Object, "Delete", "spec", "deletionPolicy")
	if err := f.client.Create(context.Background(), content); err != nil {
		t.Fatal(err)
	}
	reconciler := &LakeRestoreRehearsalReconciler{Client: f.client, APIReader: f.client, Scheme: f.scheme}
	if err := reconciler.deleteStoreRehearsalContent(context.Background(), rehearsal); err == nil {
		t.Fatal("a content not controlled by the rehearsal, with deletionPolicy Delete, was accepted for deletion")
	}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(content), content); err != nil {
		t.Fatalf("the content was deleted: %v", err)
	}
}
