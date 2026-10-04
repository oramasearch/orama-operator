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

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// workspaceStorageHarness is a workspace reconciler on envtest with one running workspace row.
type workspaceStorageHarness struct {
	t          *testing.T
	kube       client.Client
	lake       *oramav1alpha1.Lake
	store      *fakeWorkspacesStore
	reconciler *LakeWorkspacesReconciler
	cluster    client.ObjectKey
	claim      client.ObjectKey
}

func newWorkspaceStorageHarness(t *testing.T, storageGi int32) *workspaceStorageHarness {
	t.Helper()
	kube, scheme, lake := startWorkspaceEnvtest(t)
	expandable := true
	fixed := false
	for _, class := range []*storagev1.StorageClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "managed-csi"}, Provisioner: "disk.csi.azure.com", AllowVolumeExpansion: &expandable},
		{ObjectMeta: metav1.ObjectMeta{Name: "fixed"}, Provisioner: "disk.csi.azure.com", AllowVolumeExpansion: &fixed},
	} {
		if err := kube.Create(context.Background(), class); err != nil {
			t.Fatal(err)
		}
	}
	row := testWorkspaceRow("running")
	row.ZettaServiceID = ""
	row.ZettaServiceGeneration = 0
	row.StorageGi = storageGi
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{row}}
	reconciler := &LakeWorkspacesReconciler{
		Client:    kube,
		Scheme:    scheme,
		APIReader: kube,
		WorkspaceStores: func(context.Context, string) (LakeWorkspacesStore, error) {
			return store, nil
		},
		OperatorNamespace: "orama-system",
	}
	clusterName := workspaceZettaClusterName(lake, testWorkspaceID)
	return &workspaceStorageHarness{
		t: t, kube: kube, lake: lake, store: store, reconciler: reconciler,
		cluster: client.ObjectKey{Namespace: testWorkspaceName, Name: clusterName},
		claim:   client.ObjectKey{Namespace: testWorkspaceName, Name: "data-" + clusterName + "-zetta-0"},
	}
}

func (h *workspaceStorageHarness) poll() bool {
	return h.reconciler.pollLakeWorkspaces(context.Background(), h.lake)
}

// createClaim stands in for the StatefulSet controller, which envtest does not run: the claim the
// cluster's template makes, bound at its request.
func (h *workspaceStorageHarness) createClaim(className, size string) {
	h.t.Helper()
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: h.claim.Name, Namespace: h.claim.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &className,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if err := h.kube.Create(ctx, claim); err != nil {
		h.t.Fatal(err)
	}
	h.setCapacity(size)
}

// setCapacity stands in for the CSI resizer and kubelet: the volume reached size.
func (h *workspaceStorageHarness) setCapacity(size string) {
	h.t.Helper()
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{}
	if err := h.kube.Get(ctx, h.claim, claim); err != nil {
		h.t.Fatal(err)
	}
	claim.Status.Phase = corev1.ClaimBound
	claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}
	if err := h.kube.Status().Update(ctx, claim); err != nil {
		h.t.Fatal(err)
	}
}

func (h *workspaceStorageHarness) request() string {
	h.t.Helper()
	claim := &corev1.PersistentVolumeClaim{}
	if err := h.kube.Get(context.Background(), h.claim, claim); err != nil {
		h.t.Fatal(err)
	}
	quantity := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	return quantity.String()
}

func (h *workspaceStorageHarness) clusterSize() string {
	h.t.Helper()
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := h.kube.Get(context.Background(), h.cluster, cluster); err != nil {
		h.t.Fatal(err)
	}
	return cluster.Spec.Storage.Size
}

// grow is what lake's set_workspace_storage does to the row: a new size and a new generation.
func (h *workspaceStorageHarness) grow(gi int32) {
	h.store.rows[0].StorageGi = gi
	h.store.rows[0].Generation++
}

func (h *workspaceStorageHarness) row() LakeWorkspaceRow {
	return h.store.rows[0]
}

// operator#259: a workspace grows from 5 to 20 GiB. The cluster is created at lake's storage_gi,
// the claim (not the immutable template) is patched, the status says Resizing until the volume's
// capacity reaches the request and then Ready with the observed size, and the cluster's recorded
// size follows. A later smaller value is ignored and reported, and steady polls write nothing.
func TestWorkspaceStorageGrowsTheClaimInPlaceAndReportsIt(t *testing.T) {
	h := newWorkspaceStorageHarness(t, 5)
	h.poll()
	if size := h.clusterSize(); size != "5Gi" {
		t.Fatalf("workspace Zetta created at %q, want lake's storage_gi 5Gi", size)
	}
	if row := h.row(); row.StoragePhase != WorkspaceStoragePending || row.StorageGiObserved != 0 {
		t.Fatalf("storage before the claim exists = %+v", row)
	}

	h.createClaim("managed-csi", "5Gi")
	h.poll()
	if row := h.row(); row.StoragePhase != WorkspaceStorageReady || row.StorageGiObserved != 5 {
		t.Fatalf("storage at 5 GiB = %q %d %q", row.StoragePhase, row.StorageGiObserved, row.StorageMessage)
	}

	h.grow(20)
	if changing := h.poll(); !changing {
		t.Fatal("a resizing volume did not ask for a fast poll")
	}
	if request := h.request(); request != "20Gi" {
		t.Fatalf("claim request = %q, want 20Gi", request)
	}
	if size := h.clusterSize(); size != "20Gi" {
		t.Fatalf("workspace Zetta's recorded size = %q, want 20Gi", size)
	}
	row := h.row()
	if row.StoragePhase != WorkspaceStorageResizing || row.StorageGiObserved != 5 ||
		row.ObservedGeneration != row.Generation || !strings.Contains(row.StorageMessage, "20 GiB") {
		t.Fatalf("storage while resizing = %+v", row)
	}

	h.setCapacity("20Gi")
	h.poll()
	row = h.row()
	if row.StoragePhase != WorkspaceStorageReady || row.StorageGiObserved != 20 {
		t.Fatalf("storage after the resize = %q %d %q", row.StoragePhase, row.StorageGiObserved, row.StorageMessage)
	}
	writes := h.store.writes
	for range 3 {
		h.poll()
	}
	if h.store.writes != writes {
		t.Fatalf("steady polls rewrote the workspace row: %d -> %d", writes, h.store.writes)
	}

	// Lake's trigger refuses a decrease; were one to reach the operator, it is ignored.
	h.grow(10)
	h.poll()
	if request := h.request(); request != "20Gi" {
		t.Fatalf("a smaller storage_gi shrank the claim to %q", request)
	}
	if size := h.clusterSize(); size != "20Gi" {
		t.Fatalf("a smaller storage_gi lowered the recorded size to %q", size)
	}
	row = h.row()
	if row.StoragePhase != WorkspaceStorageReady || row.StorageGiObserved != 20 ||
		!strings.Contains(row.StorageMessage, "never shrink") {
		t.Fatalf("storage after a refused decrease = %+v", row)
	}
}

// operator#259: a claim whose storage class does not allow expansion is reported Failed with the
// reason and left as it is.
func TestWorkspaceStorageOnAFixedClassFails(t *testing.T) {
	h := newWorkspaceStorageHarness(t, 5)
	h.poll()
	h.createClaim("fixed", "5Gi")
	h.grow(20)
	h.poll()
	if request := h.request(); request != "5Gi" {
		t.Fatalf("claim on a fixed class was patched to %q", request)
	}
	row := h.row()
	if row.StoragePhase != WorkspaceStorageFailed || row.StorageGiObserved != 5 ||
		!strings.Contains(row.StorageMessage, `storage class "fixed"`) ||
		!strings.Contains(row.StorageMessage, "does not allow volume expansion") {
		t.Fatalf("storage on a fixed class = %+v", row)
	}
	if row.Phase == WorkspacePhaseFailed {
		t.Fatal("a volume that cannot grow failed the whole workspace")
	}
}

// Review of #267, item 5: the workspace ResourceQuota's requests.storage is the plan's headroom plus
// the workspace Zetta's own volumes, so a grow past the plan's 100Gi is not capped.
func TestWorkspaceStorageGrowsPastThePlanQuota(t *testing.T) {
	h := newWorkspaceStorageHarness(t, 5)
	h.poll()
	h.createClaim("managed-csi", "5Gi")
	h.grow(150)
	h.poll()
	if request := h.request(); request != "150Gi" {
		t.Fatalf("claim request = %q, want 150Gi", request)
	}
	quota := &corev1.ResourceQuota{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceProfileName}, quota); err != nil {
		t.Fatal(err)
	}
	storage := quota.Spec.Hard[corev1.ResourceRequestsStorage]
	if want := resource.MustParse("250Gi"); storage.Cmp(want) != 0 {
		t.Fatalf("quota requests.storage = %s, want 250Gi (100Gi plan headroom + 150Gi volume)", storage.String())
	}
}

// Review of #267, item 6: a stopped workspace whose desired size is above the observed one reports
// Pending, not the stale Ready.
func TestWorkspaceStorageOfAStoppedWorkspaceIsPending(t *testing.T) {
	h := newWorkspaceStorageHarness(t, 5)
	h.poll()
	h.createClaim("managed-csi", "5Gi")
	h.poll()
	if row := h.row(); row.StoragePhase != WorkspaceStorageReady {
		t.Fatalf("storage before stopping = %+v", row)
	}
	h.store.rows[0].Desired = "stopped"
	h.grow(30)
	h.poll()
	row := h.row()
	if row.StoragePhase != WorkspaceStoragePending || row.StorageGiObserved != 5 ||
		!strings.Contains(row.StorageMessage, "stopped") || !strings.Contains(row.StorageMessage, "30 GiB") {
		t.Fatalf("storage of a stopped workspace grown to 30 GiB = %+v", row)
	}
}

// Review of #267, resize nit: a file system resize pending past the deadline says a pod restart may
// be needed, and stays Resizing.
func TestWorkspaceStorageResizePendingTooLongSaysSo(t *testing.T) {
	h := newWorkspaceStorageHarness(t, 5)
	h.poll()
	h.createClaim("managed-csi", "5Gi")
	h.grow(20)
	h.poll()
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{}
	if err := h.kube.Get(ctx, h.claim, claim); err != nil {
		t.Fatal(err)
	}
	claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	if err := h.kube.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	h.poll()
	row := h.row()
	if row.StoragePhase != WorkspaceStorageResizing || !strings.Contains(row.StorageMessage, "restart of the Zetta pod") {
		t.Fatalf("storage with a stuck file system resize = %+v", row)
	}
}
