/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testVolumeStoreID   = "44444444-4444-4444-8444-444444444444"
	testVolumeStoreSlug = "st-444444444444"
	testVolumeClaim     = "prod-st-444444444444"
	testBucketStoreID   = "55555555-5555-4555-8555-555555555555"
	testBucketStoreSlug = "st-555555555555"
)

func (f *fakeServiceStore) ListStores(context.Context) ([]LakeStore, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storesMissing {
		return nil, errStoresTableMissing
	}
	if f.storesErr != nil {
		return nil, f.storesErr
	}
	out := make([]LakeStore, 0, len(f.stores))
	for _, st := range f.stores {
		if st.Desired == serviceDesiredAbsent && st.Status != nil && st.Status.Phase == ServicePhaseDeleted {
			continue
		}
		if st.Status != nil {
			status := *st.Status
			st.Status = &status
		}
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeServiceStore) UpsertStoreStatus(_ context.Context, status LakeStoreStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeUpserts = append(f.storeUpserts, status)
	for i := range f.stores {
		if f.stores[i].ID == status.StoreID {
			copied := status
			f.stores[i].Status = &copied
		}
	}
	return nil
}

func (f *fakeServiceStore) storeStatus(t *testing.T, id string) LakeStoreStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.stores {
		if st.ID == id {
			if st.Status == nil {
				t.Fatalf("store %s has no status", id)
			}
			return *st.Status
		}
	}
	t.Fatalf("store %s not in the store", id)
	return LakeStoreStatus{}
}

func (f *fakeServiceStore) storeUpsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.storeUpserts)
}

func (f *fakeServiceStore) setStoreDesired(id, desired string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.stores {
		if f.stores[i].ID == id {
			f.stores[i].Desired = desired
			f.stores[i].Generation++
		}
	}
}

func testStore(id, slug, storeType, provider, spec string) LakeStore {
	return LakeStore{ID: id, CustomerID: testServiceCustomer, Type: storeType, Provider: provider, Name: "data", Slug: slug,
		Spec: spec, Desired: serviceDesiredPresent, Generation: 1}
}

func expectStoreStatus(t *testing.T, got LakeStoreStatus, phase, message, endpoint string) {
	t.Helper()
	if got.Phase != phase || got.Message != message || got.Endpoint != endpoint {
		t.Fatalf("store status = %+v, want phase %q message %q endpoint %q", got, phase, message, endpoint)
	}
}

func (h *serviceHarness) storeClaim(name string) *corev1.PersistentVolumeClaim {
	h.t.Helper()
	claim := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: name}, claim); err != nil {
		h.t.Fatalf("get store claim %s: %v", name, err)
	}
	return claim
}

func TestStoresVolumeProvisioningToReady(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{"size_gi":20,"region":"","public":false}`)}}
	lake := serviceTestLake()
	lake.Spec.Workspaces.Storage.StorageClassName = "managed-csi"
	// A services Secret the Lake does not control: the connection cannot be delivered, so the
	// store is not Ready even though its claim exists.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeServicesSecret("prod"), Namespace: testServiceNamespace}}
	h := newServiceHarness(t, lake, store, foreign)
	h.poll()

	claim := h.storeClaim(testVolumeClaim)
	if !metav1.IsControlledBy(claim, h.lake) {
		t.Fatal("store claim is not controlled by the Lake")
	}
	for label, want := range map[string]string{storeIDLabel: testVolumeStoreID, serviceLakeLabel: "prod", "app.kubernetes.io/name": "lake"} {
		if claim.Labels[label] != want {
			t.Errorf("label %s = %q, want %q", label, claim.Labels[label], want)
		}
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes = %v", claim.Spec.AccessModes)
	}
	if size := claim.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("size = %s", size.String())
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "managed-csi" {
		t.Fatalf("storage class = %v", claim.Spec.StorageClassName)
	}
	endpoint := "lake/" + testVolumeClaim
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseProvisioning, "delivering the connection", endpoint)
	if !h.changing {
		t.Fatal("a provisioning store must keep the Lake on the fast poll")
	}

	if err := h.client.Delete(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseReady, storeReadyUnboundMessage, endpoint)
	want := `{"type":"volume","provider":"cluster","claim":"prod-st-444444444444","namespace":"lake","size_gi":20,"storage_class":"managed-csi"}`
	if got := string(h.credentials()[testVolumeStoreSlug+".json"]); got != want {
		t.Fatalf("connection = %s\nwant %s", got, want)
	}
	if h.changing {
		t.Fatal("a Ready store must let the Lake settle")
	}
	if got := store.storeStatus(t, testVolumeStoreID).ObservedGeneration; got != 1 {
		t.Fatalf("observed_generation = %d", got)
	}

	// Bound once a service mounts it.
	claim = h.storeClaim(testVolumeClaim)
	claim.Status.Phase = corev1.ClaimBound
	if err := h.client.Status().Update(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseReady, storeReadyBoundMessage, endpoint)

	// Steady: no status writes and no claim or Secret updates.
	upserts, updates := store.storeUpsertCount(), *h.updates
	h.poll()
	h.poll()
	if store.storeUpsertCount() != upserts || *h.updates != updates {
		t.Fatalf("steady passes wrote: %d status upserts, %d updates", store.storeUpsertCount()-upserts, *h.updates-updates)
	}
}

func TestStoresVolumeDefaultsAndNeverShrinks(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	claim := h.storeClaim(testVolumeClaim)
	if size := claim.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("default size = %s", size.String())
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("storage class = %q, want the cluster default (unset)", *claim.Spec.StorageClassName)
	}
	want := `{"type":"volume","provider":"cluster","claim":"prod-st-444444444444","namespace":"lake","size_gi":10,"storage_class":""}`
	if got := string(h.credentials()[testVolumeStoreSlug+".json"]); got != want {
		t.Fatalf("connection = %s\nwant %s", got, want)
	}

	// A smaller spec later never shrinks the claim.
	store.mu.Lock()
	store.stores[0].Spec = `{"size_gi":5}`
	store.mu.Unlock()
	h.poll()
	if size := h.storeClaim(testVolumeClaim).Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("claim resized to %s", size.String())
	}
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseReady, storeReadyUnboundMessage, "lake/"+testVolumeClaim)
}

func TestStoresForeignClaimRefused(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)}}
	lake := serviceTestLake()
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testVolumeClaim, Namespace: testServiceNamespace,
		Labels: map[string]string{storeIDLabel: testVolumeStoreID}}}
	h := newServiceHarness(t, lake, store, foreign)
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseFailed,
		"PersistentVolumeClaim lake/prod-st-444444444444 exists and is not this Lake's store", "")
	if _, found := h.credentials()[testVolumeStoreSlug+".json"]; found {
		t.Fatal("connection written for a foreign claim")
	}
	if h.changing {
		t.Fatal("a Failed store must let the Lake settle")
	}

	// Withdrawn: the foreign claim is left in place.
	store.setStoreDesired(testVolumeStoreID, serviceDesiredAbsent)
	h.poll()
	h.storeClaim(testVolumeClaim)
	if len(*h.deletes) != 0 {
		t.Fatalf("deleted %v", *h.deletes)
	}
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseDeleted,
		"store deleted; PersistentVolumeClaim lake/prod-st-444444444444 exists and is not this Lake's store and was left in place", "")
}

func TestStoresUnavailableProviderFails(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testStore(testBucketStoreID, testBucketStoreSlug, "object", "r2", `{}`)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseFailed, "r2 object stores are not available on this lake yet", "")
	var claims corev1.PersistentVolumeClaimList
	if err := h.client.List(context.Background(), &claims); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatalf("claims created for an unavailable store: %d", len(claims.Items))
	}
	if h.changing {
		t.Fatal("a Failed store must let the Lake settle")
	}
}

func TestStoresDeletion(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseReady, storeReadyUnboundMessage, "lake/"+testVolumeClaim)

	// kubernetes.io/pvc-protection holds a mounted claim: it stays, terminating, after the delete.
	claim := h.storeClaim(testVolumeClaim)
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if err := h.client.Update(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	store.setStoreDesired(testVolumeStoreID, serviceDesiredAbsent)
	h.poll()
	if len(*h.deletes) != 1 || (*h.deletes)[0] != "*v1.PersistentVolumeClaim/"+testVolumeClaim {
		t.Fatalf("deletes = %v", *h.deletes)
	}
	if _, found := h.credentials()[testVolumeStoreSlug+".json"]; found {
		t.Fatal("connection kept for an absent store")
	}
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseDeleting, "deleting the volume", "lake/"+testVolumeClaim)
	if got := store.storeStatus(t, testVolumeStoreID).ObservedGeneration; got != 2 {
		t.Fatalf("observed_generation = %d", got)
	}
	if !h.changing {
		t.Fatal("a deleting store must keep the Lake on the fast poll")
	}

	claim = h.storeClaim(testVolumeClaim)
	claim.Finalizers = nil
	if err := h.client.Update(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(claim), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("claim still there: %v", err)
	}
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseDeleted, "store deleted", "")

	// History: skipped, and the Lake settles.
	upserts := store.storeUpsertCount()
	h.poll()
	if store.storeUpsertCount() != upserts || h.changing {
		t.Fatalf("history was reconciled: %d upserts, changing %v", store.storeUpsertCount()-upserts, h.changing)
	}
}

func TestStoresShareTheServicesSecret(t *testing.T) {
	store := &fakeServiceStore{
		services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false)},
		stores:   []LakeStore{testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)},
	}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.markZettaReady(testZettaSlug, 1)
	h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-svc-111111111111-zetta-admin", Namespace: testServiceNamespace},
		Data: map[string][]byte{"token": []byte("zetta-admin-token")}})
	h.poll()
	credentials := h.credentials()
	if len(credentials) != 2 || credentials[testZettaSlug+".json"] == nil || credentials[testVolumeStoreSlug+".json"] == nil {
		t.Fatalf("services Secret keys = %v", credentials)
	}
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseReady, "service ready", serviceZettaEndpoint("prod", testZettaSlug, testServiceNamespace))
	expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseReady, storeReadyUnboundMessage, "lake/"+testVolumeClaim)

	// A failed stores read keeps the store's connection; a withdrawn service still loses its own.
	store.mu.Lock()
	store.storesErr = errors.New("connection reset")
	store.mu.Unlock()
	store.setDesired(testZettaServiceID, serviceDesiredAbsent)
	h.poll()
	credentials = h.credentials()
	if _, found := credentials[testVolumeStoreSlug+".json"]; !found || len(credentials) != 1 {
		t.Fatalf("services Secret keys after a failed stores read = %v", credentials)
	}
}

func TestStoresMissingTableLeavesServicesWorking(t *testing.T) {
	store := &fakeServiceStore{
		services:      []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false)},
		storesMissing: true,
	}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.zettaCluster(testZettaSlug)
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseProvisioning, "waiting for the Zetta nodes",
		serviceZettaEndpoint("prod", testZettaSlug, testServiceNamespace))
	h.markZettaReady(testZettaSlug, 1)
	h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-svc-111111111111-zetta-admin", Namespace: testServiceNamespace},
		Data: map[string][]byte{"token": []byte("zetta-admin-token")}})
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseReady, "service ready",
		serviceZettaEndpoint("prod", testZettaSlug, testServiceNamespace))
	if store.storeUpsertCount() != 0 {
		t.Fatalf("store statuses written without a stores table: %d", store.storeUpsertCount())
	}
	if h.changing {
		t.Fatal("a missing stores table must not keep the Lake on the fast poll")
	}
}

func TestStoresInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		mutate        func(*LakeStore)
	}{
		{"size", "spec.size_gi must be between 1 and 1024", func(st *LakeStore) { st.Spec = `{"size_gi":0}` }},
		{"slug", "slug must match ^st-[0-9a-f]{12}$", func(st *LakeStore) { st.Slug = "svc-444444444444" }},
		{"desired", "desired must be present or absent", func(st *LakeStore) { st.Desired = "gone" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)
			tc.mutate(&st)
			store := &fakeServiceStore{stores: []LakeStore{st}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			expectStoreStatus(t, store.storeStatus(t, testVolumeStoreID), ServicePhaseFailed, tc.message, "")
		})
	}
}
