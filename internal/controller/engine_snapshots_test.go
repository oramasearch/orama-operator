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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for the engine's scheduled volume snapshots (operator#316).

var engineSnapshotClaimNames = []string{"data-v1-writer-0", "data-v1-reader-0", "data-v1-reader-1"}

// snapshotCR is an OramaCloudV1 named name with engine snapshots enabled, adjusted by mut.
func snapshotCR(name string, mut func(*oramav1alpha1.EngineSnapshotsSpec)) *oramav1alpha1.OramaCloudV1 {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Name = name
		o.Spec.Components.Engine.Enabled = true
		o.Spec.Components.Engine.Snapshots = &oramav1alpha1.EngineSnapshotsSpec{Enabled: true}
	})
	if mut != nil {
		mut(oc.Spec.Components.Engine.Snapshots)
	}
	return oc
}

// engineClaimObjects is the reader StatefulSet (2 replicas) and the writer and reader claims of CR name.
func engineClaimObjects(name string) []client.Object {
	objs := []client.Object{&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-reader", Namespace: "orama-system"},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(2)},
	}}
	for _, claim := range []string{"data-" + name + "-writer-0", "data-" + name + "-reader-0", "data-" + name + "-reader-1"} {
		objs = append(objs, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: "orama-system"}})
	}
	return objs
}

// existingSnapshot is a snapshot of claim the operator took at at for CR oc, bound to a content of the
// default driver and ready unless failure is set.
func existingSnapshot(oc *oramav1alpha1.OramaCloudV1, claim, component string, at time.Time, ready bool, failure string) (*unstructured.Unstructured, *unstructured.Unstructured) {
	name := snapshotName(claim, at)
	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": engineSnapshotsDefaultClass,
			"source":                  map[string]interface{}{"persistentVolumeClaimName": claim},
		},
	}}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetNamespace("orama-system")
	snapshot.SetName(name)
	snapshot.SetLabels(labelsFor(oc, component))
	snapshot.SetAnnotations(map[string]string{snapshotAtAnnotation: at.Format(time.RFC3339Nano)})
	status := map[string]interface{}{"readyToUse": ready, "boundVolumeSnapshotContentName": "content-" + name}
	if failure != "" {
		status["error"] = map[string]interface{}{"message": failure}
	}
	snapshot.Object["status"] = status
	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"driver":            snapshotDefaultCSI,
			"deletionPolicy":    snapshotRetainPolicy,
			"volumeSnapshotRef": map[string]interface{}{"name": name, "namespace": "orama-system"},
		},
	}}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	content.SetName("content-" + name)
	return snapshot, content
}

// dailySnapshots is one ready snapshot per claim of CR oc at each of the given times.
func dailySnapshots(oc *oramav1alpha1.OramaCloudV1, claims []string, times ...time.Time) []client.Object {
	var objs []client.Object
	for _, claim := range claims {
		component := "reader"
		if strings.Contains(claim, "-writer-") {
			component = "writer"
		}
		for _, at := range times {
			snapshot, content := existingSnapshot(oc, claim, component, at, true, "")
			objs = append(objs, snapshot, content)
		}
	}
	return objs
}

func snapshotClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(ocv1TestScheme(t)).WithObjects(objs...).Build()
}

func snapshotReconciler(cl client.Client, now time.Time) *OramaCloudV1Reconciler {
	return &OramaCloudV1Reconciler{Client: cl, Scheme: cl.Scheme(), Now: func() time.Time { return now }}
}

// listSnapshots answers the VolumeSnapshots in the namespace by name.
func listSnapshots(t *testing.T, cl client.Client) map[string]unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := cl.List(context.Background(), list, client.InNamespace("orama-system")); err != nil {
		t.Fatal(err)
	}
	out := map[string]unstructured.Unstructured{}
	for _, item := range list.Items {
		out[item.GetName()] = item
	}
	return out
}

func octAt(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

func TestEngineSnapshotsDueTakesOnePerWriterAndReaderClaim(t *testing.T) {
	oc := snapshotCR("v1", nil)
	objs := append(engineClaimObjects("v1"), dailySnapshots(oc, engineSnapshotClaimNames, octAt(11, 3, 0))...)
	cl := snapshotClient(t, objs...)
	now := time.Date(2026, 10, 12, 3, 0, 5, 0, time.UTC)
	result, err := snapshotReconciler(cl, now).reconcileEngineSnapshots(context.Background(), oc)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := listSnapshots(t, cl)
	if len(snapshots) != 6 {
		t.Fatalf("snapshots = %d, want 3 old + 3 new", len(snapshots))
	}
	for _, claim := range engineSnapshotClaimNames {
		name := claim + "-20261012t030005z"
		snapshot, ok := snapshots[name]
		if !ok {
			t.Fatalf("no snapshot %s", name)
		}
		class, _, _ := unstructured.NestedString(snapshot.Object, "spec", "volumeSnapshotClassName")
		source, _, _ := unstructured.NestedString(snapshot.Object, "spec", "source", "persistentVolumeClaimName")
		if class != engineSnapshotsDefaultClass || source != claim {
			t.Errorf("%s: class %q, source %q", name, class, source)
		}
		labels := snapshot.GetLabels()
		wantComponent := "reader"
		if strings.Contains(claim, "writer") {
			wantComponent = "writer"
		}
		if labels["app.kubernetes.io/instance"] != "v1" || labels["app.kubernetes.io/managed-by"] != "orama-cloud-v1-operator" ||
			labels["app.kubernetes.io/component"] != wantComponent {
			t.Errorf("%s labels = %v", name, labels)
		}
		if len(snapshot.GetOwnerReferences()) != 0 {
			t.Errorf("%s has an owner reference; deleting the CR would delete it", name)
		}
	}
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: engineSnapshotsDefaultClass}, class); err != nil {
		t.Fatalf("default class not created: %v", err)
	}
	if class.Object["deletionPolicy"] != snapshotRetainPolicy || class.Object["driver"] != snapshotDefaultCSI {
		t.Errorf("class = %v", class.Object)
	}
	if len(result.statuses) != 3 || result.statuses[0].SnapshotName != "data-v1-writer-0-20261012t030005z" || result.statuses[0].ReadyToUse {
		t.Errorf("statuses = %+v", result.statuses)
	}
	if result.wait != engineSnapshotsPendingPoll {
		t.Errorf("wait = %s while snapshots are being cut", result.wait)
	}
}

func TestEngineSnapshotsNotDueTakeNothingAcrossARestart(t *testing.T) {
	oc := snapshotCR("v1", nil)
	objs := append(engineClaimObjects("v1"), dailySnapshots(oc, engineSnapshotClaimNames, octAt(12, 3, 0))...)
	cl := snapshotClient(t, objs...)

	result, err := snapshotReconciler(cl, octAt(12, 10, 0)).reconcileEngineSnapshots(context.Background(), oc)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listSnapshots(t, cl)); n != 3 {
		t.Fatalf("snapshots = %d before due, want 3", n)
	}
	if result.wait != 17*time.Hour {
		t.Errorf("wait = %s, want 17h to the next run", result.wait)
	}
	if result.condition == nil || result.condition.Status != metav1.ConditionTrue {
		t.Errorf("condition = %+v", result.condition)
	}

	// A new reconciler holds no memory: the timing comes from the snapshots alone.
	restarted := snapshotReconciler(cl, octAt(13, 2, 59))
	if _, err := restarted.reconcileEngineSnapshots(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if n := len(listSnapshots(t, cl)); n != 3 {
		t.Fatalf("snapshots = %d after a restart before due, want 3", n)
	}
	restarted.Now = func() time.Time { return octAt(13, 3, 0) }
	if _, err := restarted.reconcileEngineSnapshots(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if n := len(listSnapshots(t, cl)); n != 6 {
		t.Fatalf("snapshots = %d once due, want 6", n)
	}
}

func TestEngineSnapshotsPruneBeyondKeepAndLeaveOtherCRs(t *testing.T) {
	oc := snapshotCR("v1", func(s *oramav1alpha1.EngineSnapshotsSpec) { s.Keep = 7 })
	other := snapshotCR("v2", nil)
	var days []time.Time
	for d := 5; d <= 12; d++ {
		days = append(days, octAt(d, 3, 0))
	}
	objs := []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: "orama-system"}}}
	objs = append(objs, dailySnapshots(oc, []string{"data-v1-writer-0"}, days...)...)
	objs = append(objs, dailySnapshots(other, []string{"data-v2-writer-0"}, days...)...)
	cl := snapshotClient(t, objs...)

	result, err := snapshotReconciler(cl, octAt(12, 10, 0)).reconcileEngineSnapshots(context.Background(), oc)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := listSnapshots(t, cl)
	if _, ok := snapshots["data-v1-writer-0-20261005t030000z"]; ok {
		t.Error("the oldest of 8 snapshots was not pruned with keep 7")
	}
	for _, day := range days[1:] {
		if _, ok := snapshots[snapshotName("data-v1-writer-0", day)]; !ok {
			t.Errorf("%s was pruned", snapshotName("data-v1-writer-0", day))
		}
	}
	for _, day := range days {
		if _, ok := snapshots[snapshotName("data-v2-writer-0", day)]; !ok {
			t.Errorf("another CR's snapshot %s was touched", snapshotName("data-v2-writer-0", day))
		}
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "content-data-v1-writer-0-20261005t030000z"}, content); err != nil {
		t.Fatal(err)
	}
	if policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy"); policy != "Delete" {
		t.Errorf("pruned snapshot's content deletionPolicy = %q; its cloud snapshot would be retained forever", policy)
	}
	newest := &unstructured.Unstructured{}
	newest.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "content-data-v1-writer-0-20261012t030000z"}, newest); err != nil {
		t.Fatal(err)
	}
	if newest.GetLabels()["app.kubernetes.io/instance"] != "v1" || newest.GetAnnotations()[snapshotClaimAnnot] != "data-v1-writer-0" {
		t.Errorf("newest content not labelled for restore: %v %v", newest.GetLabels(), newest.GetAnnotations())
	}
	if len(result.statuses) != 1 || result.statuses[0].SnapshotName != "data-v1-writer-0-20261012t030000z" || !result.statuses[0].ReadyToUse {
		t.Errorf("statuses = %+v", result.statuses)
	}
}

func TestEngineSnapshotsReadyFalseWhenStaleOrFailed(t *testing.T) {
	oc := snapshotCR("v1", nil)
	readers := []string{"data-v1-reader-0", "data-v1-reader-1"}

	t.Run("stale", func(t *testing.T) {
		// The writer's snapshots since the 9th never became ready.
		objs := append(engineClaimObjects("v1"), dailySnapshots(oc, readers, octAt(12, 3, 0))...)
		old, oldContent := existingSnapshot(oc, "data-v1-writer-0", "writer", octAt(9, 3, 0), true, "")
		stuck, stuckContent := existingSnapshot(oc, "data-v1-writer-0", "writer", octAt(12, 3, 0), false, "")
		cl := snapshotClient(t, append(objs, old, oldContent, stuck, stuckContent)...)
		result, err := snapshotReconciler(cl, octAt(12, 10, 0)).reconcileEngineSnapshots(context.Background(), oc)
		if err != nil {
			t.Fatal(err)
		}
		c := result.condition
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SnapshotStale" || !strings.Contains(c.Message, "data-v1-writer-0") {
			t.Fatalf("condition = %+v", c)
		}
	})

	t.Run("failed", func(t *testing.T) {
		objs := append(engineClaimObjects("v1"), dailySnapshots(oc, readers, octAt(12, 3, 0))...)
		good, goodContent := existingSnapshot(oc, "data-v1-writer-0", "writer", octAt(11, 3, 0), true, "")
		bad, badContent := existingSnapshot(oc, "data-v1-writer-0", "writer", octAt(12, 3, 0), false, "quota exceeded")
		cl := snapshotClient(t, append(objs, good, goodContent, bad, badContent)...)
		result, err := snapshotReconciler(cl, octAt(12, 10, 0)).reconcileEngineSnapshots(context.Background(), oc)
		if err != nil {
			t.Fatal(err)
		}
		c := result.condition
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SnapshotFailed" || !strings.Contains(c.Message, "quota exceeded") {
			t.Fatalf("condition = %+v", c)
		}
		if result.statuses[0].Claim != "data-v1-writer-0" || result.statuses[0].Error != "quota exceeded" {
			t.Errorf("statuses = %+v", result.statuses)
		}
	})
}

func TestEngineSnapshotsInvalidScheduleFailsReconcile(t *testing.T) {
	for _, schedule := range []string{"61 * * * *", "@daily", "0 3 * *", "CRON_TZ=Europe/Rome 0 3 * * *"} {
		oc := snapshotCR("v1", func(s *oramav1alpha1.EngineSnapshotsSpec) { s.Schedule = schedule })
		cl := snapshotClient(t, engineClaimObjects("v1")...)
		_, err := snapshotReconciler(cl, octAt(12, 3, 0)).reconcileEngineSnapshots(context.Background(), oc)
		if err == nil || !strings.Contains(err.Error(), "spec.components.engine.snapshots.schedule") || !strings.Contains(err.Error(), "5-field cron") {
			t.Errorf("schedule %q: err = %v", schedule, err)
		}
		if n := len(listSnapshots(t, cl)); n != 0 {
			t.Errorf("schedule %q took %d snapshots", schedule, n)
		}
	}
}

func TestEngineSnapshotsOffTakesNothing(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = true })
	cl := snapshotClient(t, engineClaimObjects("v1")...)
	result, err := snapshotReconciler(cl, octAt(12, 3, 0)).reconcileEngineSnapshots(context.Background(), oc)
	if err != nil || result.condition != nil || len(listSnapshots(t, cl)) != 0 {
		t.Fatalf("snapshots off: result %+v, err %v", result, err)
	}
}

func TestEngineSnapshotsRefuseAClassThatDeletesContents(t *testing.T) {
	oc := snapshotCR("v1", func(s *oramav1alpha1.EngineSnapshotsSpec) { s.VolumeSnapshotClassName = "csi-azuredisk-vsc" })
	class := &unstructured.Unstructured{Object: map[string]interface{}{"driver": snapshotDefaultCSI, "deletionPolicy": "Delete"}}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	class.SetName("csi-azuredisk-vsc")
	cl := snapshotClient(t, append(engineClaimObjects("v1"), class)...)
	result, err := snapshotReconciler(cl, octAt(12, 3, 0)).reconcileEngineSnapshots(context.Background(), oc)
	if err != nil {
		t.Fatal(err)
	}
	if result.condition == nil || result.condition.Reason != "ClassNotUsable" || len(listSnapshots(t, cl)) != 0 {
		t.Fatalf("condition = %+v", result.condition)
	}
}
