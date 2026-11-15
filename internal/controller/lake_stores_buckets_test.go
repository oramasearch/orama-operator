// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testStoreWorkspaceNamespace = "prod-ws-123e4567e89b"
	testBucketSecret            = "prod-st-555555555555"
	testBucketMount             = "0b0b0b0b-1111-4222-8333-444444444444"
)

func testBucketSpec(endpoint string) string {
	return fmt.Sprintf(`{"public":false,"bucket":"reports","endpoint":%q,"region":"auto","prefix":"daily/","credential":{"mount_id":%q,"name":"r2-reports"}}`,
		endpoint, testBucketMount)
}

func testBucketStore(provider, spec string) LakeStore {
	st := testStore(testBucketStoreID, testBucketStoreSlug, storeTypeObject, provider, spec)
	st.WorkspaceNamespace = testStoreWorkspaceNamespace
	return st
}

func (h *serviceHarness) storeSealedSecret(name string) *oramav1alpha1.LakeSealedSecret {
	h.t.Helper()
	sealed := &oramav1alpha1.LakeSealedSecret{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: name}, sealed); err != nil {
		h.t.Fatalf("get store LakeSealedSecret %s: %v", name, err)
	}
	return sealed
}

// deliverStoreSecret plays the LakeSealedSecret controller: the Secret it produces, controlled by
// the delivery.
func (h *serviceHarness) deliverStoreSecret(name string) {
	h.t.Helper()
	sealed := h.storeSealedSecret(name)
	produced := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testServiceNamespace},
		Data: map[string][]byte{"access_key_id": []byte("AKIA"), "secret_access_key": []byte("secret")}}
	if err := controllerutil.SetControllerReference(sealed, produced, h.scheme); err != nil {
		h.t.Fatal(err)
	}
	h.create(produced)
}

func TestStoresBucketPairsProvisioningToReady(t *testing.T) {
	for _, tc := range []struct{ provider, endpoint string }{
		{"r2", "https://0123456789abcdef.r2.cloudflarestorage.com"},
		{"s3", "https://s3.eu-west-1.amazonaws.com"},
		{"b2", "https://s3.us-west-004.backblazeb2.com"},
		{"gcs", "https://storage.googleapis.com"},
		{"azure", "https://reportsacct.blob.core.windows.net"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			store := &fakeServiceStore{stores: []LakeStore{testBucketStore(tc.provider, testBucketSpec(tc.endpoint))}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()

			sealed := h.storeSealedSecret(testBucketSecret)
			if !metav1.IsControlledBy(sealed, h.lake) {
				t.Fatal("store LakeSealedSecret is not controlled by the Lake")
			}
			for label, want := range map[string]string{storeIDLabel: testBucketStoreID, serviceLakeLabel: "prod", workspaceLabel: testWorkspaceID} {
				if sealed.Labels[label] != want {
					t.Errorf("label %s = %q, want %q", label, sealed.Labels[label], want)
				}
			}
			if sealed.Spec.LakeRef.Name != "prod" || sealed.Spec.Template.Name != testBucketSecret || sealed.Spec.Template.Type != corev1.SecretTypeOpaque {
				t.Fatalf("delivery = %+v", sealed.Spec)
			}
			if sealed.Spec.Source.Type != "lake" || sealed.Spec.Source.Lake == nil ||
				sealed.Spec.Source.Lake.Mount != testBucketMount || sealed.Spec.Source.Lake.Name != "r2-reports" || sealed.Spec.Source.Lake.Version != nil {
				t.Fatalf("source = %+v", sealed.Spec.Source)
			}
			endpoint := tc.endpoint + "/reports"
			expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseProvisioning, storeBucketDeliveringMessage, endpoint)
			if _, found := h.credentials()[testBucketStoreSlug+".json"]; found {
				t.Fatal("connection written before the credentials were delivered")
			}
			if !h.changing {
				t.Fatal("a provisioning store must keep the Lake on the fast poll")
			}

			h.deliverStoreSecret(testBucketSecret)
			h.poll()
			expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseReady, storeBucketReadyMessage, endpoint)
			want := fmt.Sprintf(`{"type":"object","provider":%q,"bucket":"reports","endpoint":%q,"region":"auto","prefix":"daily/","secret":"prod-st-555555555555","namespace":"lake"}`,
				tc.provider, tc.endpoint)
			if got := string(h.credentials()[testBucketStoreSlug+".json"]); got != want {
				t.Fatalf("connection = %s\nwant %s", got, want)
			}
			if h.changing {
				t.Fatal("a Ready store must let the Lake settle")
			}

			// Steady: no status writes and no updates.
			upserts, updates := store.storeUpsertCount(), *h.updates
			h.poll()
			h.poll()
			if store.storeUpsertCount() != upserts || *h.updates != updates {
				t.Fatalf("steady passes wrote: %d status upserts, %d updates", store.storeUpsertCount()-upserts, *h.updates-updates)
			}
			// Nothing but the delivery was created: no claim, nothing in the workspace namespace.
			var claims corev1.PersistentVolumeClaimList
			if err := h.client.List(context.Background(), &claims); err != nil {
				t.Fatal(err)
			}
			if len(claims.Items) != 0 {
				t.Fatalf("claims created for a bucket: %d", len(claims.Items))
			}
		})
	}
}

func TestStoresBucketCredentialChangeUpdatesTheDelivery(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testBucketStore("s3", testBucketSpec("https://s3.amazonaws.com"))}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	store.mu.Lock()
	store.stores[0].Spec = `{"bucket":"reports","endpoint":"https://s3.amazonaws.com","credential":{"mount_id":"m2","name":"other"}}`
	store.mu.Unlock()
	h.poll()
	sealed := h.storeSealedSecret(testBucketSecret)
	if sealed.Spec.Source.Lake.Mount != "m2" || sealed.Spec.Source.Lake.Name != "other" {
		t.Fatalf("source after the credential changed = %+v", sealed.Spec.Source.Lake)
	}
}

func TestStoresBucketDeliveryErrorIsShown(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testBucketStore("r2", testBucketSpec("https://acct.r2.cloudflarestorage.com"))}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	sealed := h.storeSealedSecret(testBucketSecret)
	sealed.Status.Phase = "Error"
	sealed.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "LakeSecretFailed",
		Message: "secret r2-reports not found", LastTransitionTime: metav1.Now()}}
	if err := h.client.Update(context.Background(), sealed); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseProvisioning,
		storeBucketDeliveringMessage+": secret r2-reports not found", "https://acct.r2.cloudflarestorage.com/reports")
}

func TestStoresBucketForeignResourcesRefused(t *testing.T) {
	t.Run("delivery", func(t *testing.T) {
		store := &fakeServiceStore{stores: []LakeStore{testBucketStore("r2", testBucketSpec("https://acct.r2.cloudflarestorage.com"))}}
		foreign := &oramav1alpha1.LakeSealedSecret{ObjectMeta: metav1.ObjectMeta{Name: testBucketSecret, Namespace: testServiceNamespace,
			Labels: map[string]string{storeIDLabel: testBucketStoreID}},
			Spec: oramav1alpha1.LakeSealedSecretSpec{LakeRef: corev1.LocalObjectReference{Name: "prod"},
				Template: oramav1alpha1.LakeSealedSecretTemplate{Name: testBucketSecret},
				Source:   oramav1alpha1.LakeSealedSecretSource{Type: "lake", Lake: &oramav1alpha1.LakeStaticSecretSource{Mount: "x", Name: "y"}}}}
		h := newServiceHarness(t, serviceTestLake(), store, foreign)
		h.poll()
		expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseFailed,
			"LakeSealedSecret lake/prod-st-555555555555 exists and is not this Lake's store", "")
		if got := h.storeSealedSecret(testBucketSecret); got.Spec.Source.Lake.Mount != "x" {
			t.Fatal("a foreign LakeSealedSecret was rewritten")
		}
		if h.changing {
			t.Fatal("a Failed store must let the Lake settle")
		}

		store.setStoreDesired(testBucketStoreID, serviceDesiredAbsent)
		h.poll()
		h.storeSealedSecret(testBucketSecret)
		if len(*h.deletes) != 0 {
			t.Fatalf("deleted %v", *h.deletes)
		}
		expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseDeleted,
			storeBucketDeletedMessage+"; LakeSealedSecret lake/prod-st-555555555555 exists and is not this Lake's store and was left in place", "")
	})
	t.Run("produced Secret", func(t *testing.T) {
		store := &fakeServiceStore{stores: []LakeStore{testBucketStore("r2", testBucketSpec("https://acct.r2.cloudflarestorage.com"))}}
		// A Secret of the delivery's name that the delivery did not produce never makes it Ready.
		foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testBucketSecret, Namespace: testServiceNamespace},
			Data: map[string][]byte{"access_key_id": []byte("planted")}}
		h := newServiceHarness(t, serviceTestLake(), store, foreign)
		h.poll()
		h.poll()
		expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseProvisioning, storeBucketDeliveringMessage,
			"https://acct.r2.cloudflarestorage.com/reports")
		if _, found := h.credentials()[testBucketStoreSlug+".json"]; found {
			t.Fatal("connection written for a foreign Secret")
		}
	})
}

func TestStoresBucketDeletionLeavesTheBucket(t *testing.T) {
	store := &fakeServiceStore{stores: []LakeStore{testBucketStore("s3", testBucketSpec("https://s3.amazonaws.com"))}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.deliverStoreSecret(testBucketSecret)
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseReady, storeBucketReadyMessage, "https://s3.amazonaws.com/reports")

	// A finalizer holds the delivery: Deleting until it goes.
	sealed := h.storeSealedSecret(testBucketSecret)
	sealed.Finalizers = []string{"test.orama.cloud/hold"}
	if err := h.client.Update(context.Background(), sealed); err != nil {
		t.Fatal(err)
	}
	store.setStoreDesired(testBucketStoreID, serviceDesiredAbsent)
	h.poll()
	if len(*h.deletedKinds) != 1 || (*h.deletedKinds)[0] != "LakeSealedSecret/"+testBucketSecret {
		t.Fatalf("deleted = %v, want only the store's LakeSealedSecret", *h.deletedKinds)
	}
	if _, found := h.credentials()[testBucketStoreSlug+".json"]; found {
		t.Fatal("connection kept for an absent store")
	}
	expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseDeleting, "removing the credentials", "")
	if !h.changing {
		t.Fatal("a deleting store must keep the Lake on the fast poll")
	}

	sealed = h.storeSealedSecret(testBucketSecret)
	sealed.Finalizers = nil
	if err := h.client.Update(context.Background(), sealed); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(sealed), &oramav1alpha1.LakeSealedSecret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("delivery still there: %v", err)
	}
	h.poll()
	expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseDeleted, storeBucketDeletedMessage, "")
	// The bucket is the customer's: the only thing the operator ever deleted was the delivery.
	if len(*h.deletedKinds) != 1 {
		t.Fatalf("deleted = %v", *h.deletedKinds)
	}

	upserts := store.storeUpsertCount()
	h.poll()
	if store.storeUpsertCount() != upserts || h.changing {
		t.Fatalf("history was reconciled: %d upserts, changing %v", store.storeUpsertCount()-upserts, h.changing)
	}
}

func TestStoresBucketInvalidSpecs(t *testing.T) {
	for _, tc := range []struct{ name, spec, message string }{
		{"bucket", `{"bucket":"","endpoint":"https://s3.amazonaws.com","credential":{"mount_id":"m","name":"n"}}`, "spec.bucket is required"},
		{"http", `{"bucket":"b","endpoint":"http://s3.amazonaws.com","credential":{"mount_id":"m","name":"n"}}`, "spec.endpoint must be an https:// URL"},
		{"no endpoint", `{"bucket":"b","credential":{"mount_id":"m","name":"n"}}`, "spec.endpoint must be an https:// URL"},
		{"credential", `{"bucket":"b","endpoint":"https://s3.amazonaws.com","credential":{"mount_id":"m","name":""}}`, "spec.credential.mount_id and spec.credential.name are required"},
		{"no credential", `{"bucket":"b","endpoint":"https://s3.amazonaws.com"}`, "spec.credential.mount_id and spec.credential.name are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeServiceStore{stores: []LakeStore{testBucketStore("s3", tc.spec)}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseFailed, tc.message, "")
			var deliveries oramav1alpha1.LakeSealedSecretList
			if err := h.client.List(context.Background(), &deliveries); err != nil {
				t.Fatal(err)
			}
			if len(deliveries.Items) != 0 {
				t.Fatalf("delivery created for an invalid spec: %d", len(deliveries.Items))
			}
		})
	}
}

func TestStoresLakeRunPairs(t *testing.T) {
	for _, tc := range []struct{ storeType, provider, spec string }{
		{storeTypeObject, storeProviderZetta, `{"public":false}`},
		{storeTypeFilesystem, storeProviderZetta, `{"public":false}`},
		{storeTypeObject, storeProviderLake, `{"public":false,"repository_id":"0b0b0b0b-1111-4222-8333-555555555555"}`},
		{storeTypeFilesystem, storeProviderLake, `{"public":false,"repository_id":"0b0b0b0b-1111-4222-8333-555555555555"}`},
	} {
		t.Run(tc.storeType+"/"+tc.provider, func(t *testing.T) {
			st := testStore(testBucketStoreID, testBucketStoreSlug, tc.storeType, tc.provider, tc.spec)
			st.WorkspaceNamespace = testStoreWorkspaceNamespace
			store := &fakeServiceStore{stores: []LakeStore{st}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseReady, storeLakeRunReadyMessage, "")
			want := fmt.Sprintf(`{"type":%q,"provider":%q}`, tc.storeType, tc.provider)
			if got := string(h.credentials()[testBucketStoreSlug+".json"]); got != want {
				t.Fatalf("connection = %s\nwant %s", got, want)
			}
			if h.changing {
				t.Fatal("a Ready store must let the Lake settle")
			}
			var deliveries oramav1alpha1.LakeSealedSecretList
			var claims corev1.PersistentVolumeClaimList
			if err := h.client.List(context.Background(), &deliveries); err != nil {
				t.Fatal(err)
			}
			if err := h.client.List(context.Background(), &claims); err != nil {
				t.Fatal(err)
			}
			if len(deliveries.Items) != 0 || len(claims.Items) != 0 {
				t.Fatalf("a Lake-run store created %d deliveries and %d claims", len(deliveries.Items), len(claims.Items))
			}

			store.setStoreDesired(testBucketStoreID, serviceDesiredAbsent)
			h.poll()
			expectStoreStatus(t, store.storeStatus(t, testBucketStoreID), ServicePhaseDeleted, "store deleted", "")
			if _, found := h.credentials()[testBucketStoreSlug+".json"]; found {
				t.Fatal("connection kept for an absent store")
			}
			if len(*h.deletes) != 0 {
				t.Fatalf("deleted %v", *h.deletes)
			}
		})
	}
}
