/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeVolumeSnapshotsStore serves policies and applies every status write, as lake's tables do.
type fakeVolumeSnapshotsStore struct {
	policies []LakeVolumeSnapshotPolicy
	writes   int
	// now is Postgres's now() as the list reports it.
	now func() time.Time
}

func (s *fakeVolumeSnapshotsStore) ListVolumeSnapshotPolicies(context.Context) ([]LakeVolumeSnapshotPolicy, error) {
	out := make([]LakeVolumeSnapshotPolicy, len(s.policies))
	copy(out, s.policies)
	if s.now != nil {
		for i := range out {
			out[i].DBNow = s.now()
		}
	}
	return out, nil
}

func (s *fakeVolumeSnapshotsStore) UpsertVolumeSnapshotStatus(_ context.Context, status LakeVolumeSnapshotStatus) error {
	s.writes++
	for i := range s.policies {
		if s.policies[i].Target == status.Target {
			copied := status
			s.policies[i].Status = &copied
		}
	}
	return nil
}

func (s *fakeVolumeSnapshotsStore) Close(context.Context) error { return nil }

func (s *fakeVolumeSnapshotsStore) policy(target string) *LakeVolumeSnapshotPolicy {
	for i := range s.policies {
		if s.policies[i].Target == target {
			return &s.policies[i]
		}
	}
	return nil
}

type snapshotHarness struct {
	t          *testing.T
	kube       client.Client
	lake       *oramav1alpha1.Lake
	store      *fakeVolumeSnapshotsStore
	reconciler *LakeVolumeSnapshotsReconciler
	clock      time.Time
	storeClaim string
	wsClaim    string
}

// newSnapshotHarness starts an API server with the operator's CRDs and the upstream
// snapshot.storage.k8s.io CRDs (external-snapshotter v8.2.0, as installed on the dev cluster),
// lake's store claim, and one workspace's Zetta claim.
func newSnapshotHarness(t *testing.T) *snapshotHarness {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the volume snapshot envtests")
	}
	operatorCRDs, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	snapshotCRDs, err := filepath.Abs("testdata/snapshot-crds")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{operatorCRDs, snapshotCRDs},
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
	ctx := context.Background()
	scheme := newLakeScheme(t)
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	lake := testWorkspaceLake()
	lake.Spec.PullSecretName = ""
	lake.Spec.Snapshots = oramav1alpha1.LakeSnapshotsSpec{ClassName: "lake-retain", Driver: "disk.csi.azure.com"}
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	h := &snapshotHarness{
		t: t, kube: kube, lake: lake,
		clock:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		storeClaim: lakeStoreClaimName(lake),
		wsClaim:    "data-" + workspaceZettaClusterName(lake, testWorkspaceID) + "-zetta-0",
	}
	for _, namespace := range []string{lakeNamespace(lake), testWorkspaceName} {
		if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := kube.Create(ctx, controlDBAppSecretForTest(lake.Name, "postgresql://unused")); err != nil {
		t.Fatal(err)
	}
	h.createClaim(lakeNamespace(lake), h.storeClaim)
	h.createClaim(testWorkspaceName, h.wsClaim)
	h.store = &fakeVolumeSnapshotsStore{policies: []LakeVolumeSnapshotPolicy{
		{Target: snapshotStoreTarget, Schedule: "0 3 * * *", Keep: 2},
		{Target: testWorkspaceID, Schedule: "0 3 * * *", Keep: 3,
			WorkspaceDesired: "running", WorkspaceNamespace: testWorkspaceName},
	}, now: func() time.Time { return h.clock }}
	h.reconciler = &LakeVolumeSnapshotsReconciler{
		Client:    kube,
		Scheme:    scheme,
		APIReader: kube,
		SnapshotStores: func(context.Context, string) (LakeVolumeSnapshotsStore, error) {
			return h.store, nil
		},
		Now: func() time.Time { return h.clock },
	}
	return h
}

func (h *snapshotHarness) createClaim(namespace, name string) {
	h.t.Helper()
	class := "managed-csi"
	if err := h.kube.Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
			},
		},
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *snapshotHarness) poll() time.Duration {
	return h.reconciler.pollVolumeSnapshots(context.Background(), h.lake)
}

// snapshots lists the snapshots of a target's namespace, by name.
func (h *snapshotHarness) snapshots(namespace string) []unstructured.Unstructured {
	h.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := h.kube.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
		h.t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
	return list.Items
}

func (h *snapshotHarness) names(namespace string) []string {
	var names []string
	for _, item := range h.snapshots(namespace) {
		names = append(names, item.GetName())
	}
	return names
}

// bind stands in for the CSI snapshotter: every snapshot in namespace that is neither ready nor
// failed is bound to a content that is ready to use.
func (h *snapshotHarness) bind(namespace string) {
	h.t.Helper()
	for _, item := range h.snapshots(namespace) {
		ready, _, _ := unstructured.NestedBool(item.Object, "status", "readyToUse")
		failed, _, _ := unstructured.NestedString(item.Object, "status", "error", "message")
		if !ready && failed == "" {
			h.bindOne(namespace, item.GetName(), "disk.csi.azure.com")
		}
	}
}

func (h *snapshotHarness) snapshot(namespace, name string) *unstructured.Unstructured {
	h.t.Helper()
	item := &unstructured.Unstructured{}
	item.SetGroupVersionKind(volumeSnapshotGVK)
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, item); err != nil {
		h.t.Fatal(err)
	}
	return item
}

// bindOne binds one snapshot to a ready content of driver.
func (h *snapshotHarness) bindOne(namespace, name, driver string) string {
	h.t.Helper()
	ctx := context.Background()
	item := h.snapshot(namespace, name)
	contentName := "snapcontent-" + string(item.GetUID())
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	content.SetName(contentName)
	content.Object["spec"] = map[string]interface{}{
		"deletionPolicy":          snapshotRetainPolicy,
		"driver":                  driver,
		"volumeSnapshotClassName": "lake-retain",
		"source":                  map[string]interface{}{"volumeHandle": "/subscriptions/x/disks/" + item.GetName()},
		"volumeSnapshotRef": map[string]interface{}{
			"name": item.GetName(), "namespace": namespace, "uid": string(item.GetUID()),
		},
	}
	if err := h.kube.Create(ctx, content); err != nil {
		h.t.Fatal(err)
	}
	item.Object["status"] = map[string]interface{}{
		"boundVolumeSnapshotContentName": contentName,
		"readyToUse":                     true,
	}
	if err := h.kube.Status().Update(ctx, item); err != nil {
		h.t.Fatal(err)
	}
	return contentName
}

// fail marks one snapshot as failed by the snapshotter: not ready, with an error and no content.
func (h *snapshotHarness) fail(namespace, name string) {
	h.t.Helper()
	item := h.snapshot(namespace, name)
	item.Object["status"] = map[string]interface{}{
		"readyToUse": false,
		"error":      map[string]interface{}{"message": "disk is busy"},
	}
	if err := h.kube.Status().Update(context.Background(), item); err != nil {
		h.t.Fatal(err)
	}
}

func (h *snapshotHarness) contentPolicy(name string) string {
	h.t.Helper()
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := h.kube.Get(context.Background(), client.ObjectKey{Name: name}, content); err != nil {
		h.t.Fatal(err)
	}
	policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy")
	return policy
}

func (h *snapshotHarness) contents() []unstructured.Unstructured {
	h.t.Helper()
	list := &unstructured.UnstructuredList{}
	gvk := volumeSnapshotContentGVK
	gvk.Kind += "List"
	list.SetGroupVersionKind(gvk)
	if err := h.kube.List(context.Background(), list); err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

// operator#260: the class is created with deletionPolicy Retain; each target gets a snapshot of
// each claim at once, named <claim>-<UTC timestamp> and labelled with its target; the status
// reports the newest and turns ready with it; the bound contents are labelled; nothing is taken
// before the schedule comes due, and the oldest beyond keep are pruned; a one-off request newer
// than the last snapshot takes one at once; steady polls write nothing.
func TestVolumeSnapshotsAreTakenOnScheduleAndOnRequestAndPruned(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.poll()

	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	if err := h.kube.Get(ctx, client.ObjectKey{Name: "lake-retain"}, class); err != nil {
		t.Fatalf("snapshot class not created: %v", err)
	}
	if class.Object["deletionPolicy"] != "Retain" || class.Object["driver"] != "disk.csi.azure.com" {
		t.Fatalf("snapshot class = %v", class.Object)
	}

	wantStore := h.storeClaim + "-20261003t120000z"
	wantWorkspace := h.wsClaim + "-20261003t120000z"
	if names := h.names(lakeNamespace(h.lake)); len(names) != 1 || names[0] != wantStore {
		t.Fatalf("store snapshots = %v, want [%s]", names, wantStore)
	}
	if names := h.names(testWorkspaceName); len(names) != 1 || names[0] != wantWorkspace {
		t.Fatalf("workspace snapshots = %v, want [%s]", names, wantWorkspace)
	}
	snapshot := h.snapshots(testWorkspaceName)[0]
	source, _, _ := unstructured.NestedString(snapshot.Object, "spec", "source", "persistentVolumeClaimName")
	className, _, _ := unstructured.NestedString(snapshot.Object, "spec", "volumeSnapshotClassName")
	if snapshot.GetLabels()[snapshotTargetLabel] != testWorkspaceID || source != h.wsClaim || className != "lake-retain" ||
		len(snapshot.GetOwnerReferences()) != 0 {
		t.Fatalf("workspace snapshot = labels %v source %q class %q owners %v",
			snapshot.GetLabels(), source, className, snapshot.GetOwnerReferences())
	}
	status := h.store.policy(testWorkspaceID).Status
	if status == nil || status.SnapshotName != wantWorkspace || status.ReadyToUse ||
		status.SnapshotAt == nil || !status.SnapshotAt.Equal(h.clock) {
		t.Fatalf("workspace status before ready = %+v", status)
	}

	h.bind(lakeNamespace(h.lake))
	h.bind(testWorkspaceName)
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	if status := h.store.policy(snapshotStoreTarget).Status; status == nil || !status.ReadyToUse || status.SnapshotName != wantStore {
		t.Fatalf("store status after ready = %+v", status)
	}
	labelled := 0
	for _, content := range h.contents() {
		if content.GetLabels()[snapshotTargetLabel] != "" {
			labelled++
		}
		if content.GetLabels()[snapshotTargetLabel] == testWorkspaceID &&
			content.GetAnnotations()[snapshotClaimAnnot] != h.wsClaim {
			t.Fatalf("workspace content annotations = %v", content.GetAnnotations())
		}
	}
	if labelled != 2 {
		t.Fatalf("labelled contents = %d, want 2", labelled)
	}

	// Not due before 03:00 the next day; the steady polls write nothing.
	writes := h.store.writes
	h.clock = time.Date(2026, 10, 4, 2, 59, 0, 0, time.UTC)
	for range 3 {
		h.poll()
	}
	if names := h.names(testWorkspaceName); len(names) != 1 {
		t.Fatalf("snapshots before the schedule = %v", names)
	}
	if h.store.writes != writes {
		t.Fatalf("steady polls rewrote the status: %d -> %d", writes, h.store.writes)
	}

	// Four days of the daily schedule: the store keeps 2, the workspace 3, newest kept.
	for day := 4; day <= 7; day++ {
		h.clock = time.Date(2026, 10, day, 3, 0, 5, 0, time.UTC)
		h.poll()
		h.bind(lakeNamespace(h.lake))
		h.bind(testWorkspaceName)
	}
	// Only ready snapshots count toward keep: the last one counts once it is ready.
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	store := h.names(lakeNamespace(h.lake))
	if len(store) != 2 || store[1] != h.storeClaim+"-20261007t030005z" {
		t.Fatalf("store snapshots after pruning = %v", store)
	}
	workspace := h.names(testWorkspaceName)
	if len(workspace) != 3 || workspace[0] != h.wsClaim+"-20261005t030005z" {
		t.Fatalf("workspace snapshots after pruning = %v", workspace)
	}
	// keep bounds what is really kept: a pruned snapshot's content is switched to Delete so the
	// cloud snapshot goes with it; every kept one stays Retain.
	live := map[string]bool{}
	for _, namespace := range []string{lakeNamespace(h.lake), testWorkspaceName} {
		for _, item := range h.snapshots(namespace) {
			bound, _, _ := unstructured.NestedString(item.Object, "status", "boundVolumeSnapshotContentName")
			live[bound] = true
		}
	}
	deleted := 0
	for _, content := range h.contents() {
		policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy")
		switch {
		case live[content.GetName()] && policy != "Retain":
			t.Fatalf("kept snapshot's content %s is %s", content.GetName(), policy)
		case !live[content.GetName()] && policy != "Delete":
			t.Fatalf("pruned snapshot's content %s is %s, want Delete", content.GetName(), policy)
		case !live[content.GetName()]:
			deleted++
		}
	}
	if deleted != 5 {
		t.Fatalf("contents switched to Delete = %d, want 5 (three of the store, two of the workspace)", deleted)
	}

	// A one-off request newer than the last snapshot takes one at once, outside the schedule.
	h.clock = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)
	requested := h.clock.Add(-time.Second)
	h.store.policy(testWorkspaceID).RequestedAt = &requested
	h.poll()
	workspace = h.names(testWorkspaceName)
	if len(workspace) != 4 || workspace[3] != h.wsClaim+"-20261007t093000z" {
		t.Fatalf("workspace snapshots after a request = %v (a not-ready one does not count toward keep)", workspace)
	}
	if status := h.store.policy(testWorkspaceID).Status; status.SnapshotName != h.wsClaim+"-20261007t093000z" || status.ReadyToUse {
		t.Fatalf("status after a request = %+v", status)
	}
	h.bind(testWorkspaceName)
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	if workspace := h.names(testWorkspaceName); len(workspace) != 3 || workspace[2] != h.wsClaim+"-20261007t093000z" {
		t.Fatalf("an honored request was honored again, or keep was not applied: %v", workspace)
	}
	if names := h.names(lakeNamespace(h.lake)); len(names) != 2 {
		t.Fatalf("a workspace request snapshotted the store: %v", names)
	}
}

// operator#260: deleting a workspace leaves its snapshots. Once lake marks the
// workspace deleted the operator takes and prunes nothing for it and leaves its status, and every
// content it was bound to stays (deletionPolicy Retain), labelled with the workspace for a restore.
func TestVolumeSnapshotsOutliveTheirWorkspace(t *testing.T) {
	h := newSnapshotHarness(t)
	h.poll()
	h.bind(testWorkspaceName)
	h.poll()
	before := h.names(testWorkspaceName)
	status := *h.store.policy(testWorkspaceID).Status

	policy := h.store.policy(testWorkspaceID)
	policy.WorkspaceDesired = "deleted"
	policy.Keep = 1
	requested := h.clock.Add(time.Hour)
	policy.RequestedAt = &requested
	for day := 4; day <= 6; day++ {
		h.clock = time.Date(2026, 10, day, 3, 0, 5, 0, time.UTC)
		h.poll()
	}
	if after := h.names(testWorkspaceName); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a deleted workspace's snapshots changed: %v -> %v", before, after)
	}
	if got := h.store.policy(testWorkspaceID).Status; !got.equal(status) {
		t.Fatalf("a deleted workspace's status was rewritten: %+v -> %+v", status, *got)
	}
	found := false
	for _, content := range h.contents() {
		policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy")
		if content.GetLabels()[snapshotTargetLabel] == testWorkspaceID && policy == "Retain" {
			found = true
		}
	}
	if !found {
		t.Fatal("the deleted workspace's retained content cannot be found by its target label")
	}
	// The store kept its own schedule; none of its snapshots is ready, so none counts toward keep.
	if names := h.names(lakeNamespace(h.lake)); len(names) != 4 {
		t.Fatalf("the store kept its own schedule: %v", names)
	}
}

// operator#260: an existing class that would delete its contents is refused; every target reports
// why and nothing is taken. A policy whose workspace has no namespace yet says so.
func TestVolumeSnapshotsRefuseAClassThatDeletesContents(t *testing.T) {
	h := newSnapshotHarness(t)
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	class.SetName("lake-retain")
	class.Object["driver"] = "disk.csi.azure.com"
	class.Object["deletionPolicy"] = "Delete"
	if err := h.kube.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	h.store.policies = append(h.store.policies, LakeVolumeSnapshotPolicy{
		Target: "423e4567-e89b-42d3-a456-426614174000", Schedule: "0 3 * * *", Keep: 7, WorkspaceDesired: "running",
	})
	h.poll()
	for _, namespace := range []string{lakeNamespace(h.lake), testWorkspaceName} {
		if names := h.names(namespace); len(names) != 0 {
			t.Fatalf("snapshots taken through a Delete class in %s: %v", namespace, names)
		}
	}
	for _, target := range []string{snapshotStoreTarget, testWorkspaceID} {
		status := h.store.policy(target).Status
		if status == nil || !strings.Contains(status.Message, "deletionPolicy Delete") || status.SnapshotName != "" {
			t.Fatalf("%s status = %+v", target, status)
		}
	}
	if status := h.store.policy("423e4567-e89b-42d3-a456-426614174000").Status; status == nil ||
		status.Message != "the workspace has no namespace yet" {
		t.Fatalf("namespaceless workspace status = %+v", status)
	}
}

// Review of #267, item 1: only ready snapshots count toward keep, and the newest ready one is never
// deleted, even at keep=1; of the failed ones the newest is kept for diagnosis and older ones are
// deleted; a not-ready one still in progress is kept until a newer one is ready.
func TestVolumeSnapshotsPruneOnlyReadyOnesTowardKeep(t *testing.T) {
	h := newSnapshotHarness(t)
	policy := h.store.policy(testWorkspaceID)
	policy.Keep = 1
	h.store.policies = h.store.policies[1:] // the workspace alone
	day := func(d int) string {
		return h.wsClaim + time.Date(2026, 10, d, 3, 0, 5, 0, time.UTC).Format("-20060102t150405z")
	}

	h.clock = time.Date(2026, 10, 3, 3, 0, 5, 0, time.UTC)
	h.poll()
	first := h.bindOne(testWorkspaceName, day(3), "disk.csi.azure.com")
	for d := 4; d <= 6; d++ {
		h.clock = time.Date(2026, 10, d, 3, 0, 5, 0, time.UTC)
		h.poll()
		if d < 6 {
			h.fail(testWorkspaceName, day(d))
		}
	}
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	// The ready day 3 (keep=1), the newest failed day 5, the in-progress day 6; day 4 pruned.
	if got := strings.Join(h.names(testWorkspaceName), ","); got != strings.Join([]string{day(3), day(5), day(6)}, ",") {
		t.Fatalf("snapshots = %s", got)
	}
	if status := h.store.policy(testWorkspaceID).Status; status.SnapshotName != day(6) || status.ReadyToUse {
		t.Fatalf("status = %+v", status)
	}

	newest := h.bindOne(testWorkspaceName, day(6), "disk.csi.azure.com")
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	if got := strings.Join(h.names(testWorkspaceName), ","); got != strings.Join([]string{day(5), day(6)}, ",") {
		t.Fatalf("snapshots after day 6 is ready = %s", got)
	}
	if h.contentPolicy(first) != "Delete" || h.contentPolicy(newest) != "Retain" {
		t.Fatalf("content policies: pruned %s, newest %s", h.contentPolicy(first), h.contentPolicy(newest))
	}
	if status := h.store.policy(testWorkspaceID).Status; status.SnapshotName != day(6) || !status.ReadyToUse {
		t.Fatalf("status after day 6 is ready = %+v", status)
	}
}

// Review of #267, items 4 and the AlreadyExists nit: a hand-made snapshot, another target's, and an
// unlabelled object squatting on the next snapshot's name are never adopted, counted or pruned;
// this controller's own snapshot bound to a content of another driver is not pruned, and the status
// says so.
func TestVolumeSnapshotsTouchOnlyTheirOwnSnapshots(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.store.policy(testWorkspaceID).Keep = 1
	h.store.policies = h.store.policies[1:]
	create := func(name string, labels map[string]string) string {
		item := &unstructured.Unstructured{}
		item.SetGroupVersionKind(volumeSnapshotGVK)
		item.SetNamespace(testWorkspaceName)
		item.SetName(name)
		item.SetLabels(labels)
		item.Object["spec"] = map[string]interface{}{
			"volumeSnapshotClassName": "lake-retain",
			"source":                  map[string]interface{}{"persistentVolumeClaimName": h.wsClaim},
		}
		if err := h.kube.Create(ctx, item); err != nil {
			t.Fatal(err)
		}
		return h.bindOne(testWorkspaceName, name, "disk.csi.azure.com")
	}
	manual := create("manual-backup", nil)
	other := create("other-target", map[string]string{
		snapshotTargetLabel: "523e4567-e89b-42d3-a456-426614174000", workspaceLakeLabel: h.lake.Name,
		workspaceManagedByLabel: "orama-operator",
	})

	h.clock = time.Date(2026, 10, 3, 3, 0, 5, 0, time.UTC)
	h.poll()
	ours := h.wsClaim + "-20261003t030005z"
	foreign := h.bindOne(testWorkspaceName, ours, "other.csi.example.com")
	// Squat on the next snapshot's name with an unlabelled object.
	squat := h.wsClaim + "-20261004t030005z"
	create(squat, nil)

	h.clock = time.Date(2026, 10, 4, 3, 0, 5, 0, time.UTC)
	h.poll()
	status := h.store.policy(testWorkspaceID).Status
	if !strings.Contains(status.Message, "not adopted") || status.SnapshotName != ours {
		t.Fatalf("status with a squatter = %+v", status)
	}
	if got := h.snapshot(testWorkspaceName, squat).GetLabels(); len(got) != 0 {
		t.Fatalf("the squatter was adopted: %v", got)
	}

	h.clock = time.Date(2026, 10, 5, 3, 0, 5, 0, time.UTC)
	h.poll()
	h.bindOne(testWorkspaceName, h.wsClaim+"-20261005t030005z", "disk.csi.azure.com")
	h.clock = h.clock.Add(time.Minute)
	h.poll()
	names := strings.Join(h.names(testWorkspaceName), ",")
	for _, name := range []string{"manual-backup", "other-target", squat, ours} {
		if !strings.Contains(names, name) {
			t.Fatalf("%s was pruned: %s", name, names)
		}
	}
	for _, content := range []string{manual, other, foreign} {
		if h.contentPolicy(content) != "Retain" {
			t.Fatalf("content %s was switched to %s", content, h.contentPolicy(content))
		}
	}
	if status := h.store.policy(testWorkspaceID).Status; !strings.Contains(status.Message, "other.csi.example.com") ||
		!strings.Contains(status.Message, "not pruned") {
		t.Fatalf("status does not report the declined prune: %+v", status)
	}
}

// Review of #267, item 2: a schedule that never fires (February 31st) takes no scheduled snapshot
// and says so; a requested_at request still takes one, once.
func TestVolumeSnapshotsScheduleThatNeverFires(t *testing.T) {
	h := newSnapshotHarness(t)
	h.store.policies = h.store.policies[1:]
	h.store.policy(testWorkspaceID).Schedule = "0 0 31 2 *"
	h.poll()
	if names := h.names(testWorkspaceName); len(names) != 0 {
		t.Fatalf("a schedule that never fires took %v", names)
	}
	if status := h.store.policy(testWorkspaceID).Status; status == nil || !strings.Contains(status.Message, "never fires") {
		t.Fatalf("status = %+v", status)
	}
	requested := h.clock.Add(-time.Second)
	h.store.policy(testWorkspaceID).RequestedAt = &requested
	h.poll()
	h.clock = h.clock.Add(time.Hour)
	h.poll()
	if names := h.names(testWorkspaceName); len(names) != 1 {
		t.Fatalf("snapshots after one request = %v", names)
	}
	if status := h.store.policy(testWorkspaceID).Status; !strings.Contains(status.Message, "never fires") {
		t.Fatalf("status after the request = %+v", status)
	}
}

// Review of #267, clock skew: requested_at, schedules and recorded instants are judged by Postgres's
// now(), not the operator's clock, so an operator an hour ahead still honors a request made after
// the last snapshot.
func TestVolumeSnapshotsJudgeRequestsByThePostgresClock(t *testing.T) {
	h := newSnapshotHarness(t)
	h.store.policies = h.store.policies[1:]
	h.reconciler.Now = func() time.Time { return h.clock.Add(time.Hour) }
	h.poll()
	h.bind(testWorkspaceName)
	h.clock = h.clock.Add(10 * time.Minute)
	requested := h.clock.Add(-time.Second)
	h.store.policy(testWorkspaceID).RequestedAt = &requested
	h.poll()
	if names := h.names(testWorkspaceName); len(names) != 2 || names[1] != h.wsClaim+"-20261003t121000z" {
		t.Fatalf("snapshots after a request with a skewed operator clock = %v", names)
	}
}
