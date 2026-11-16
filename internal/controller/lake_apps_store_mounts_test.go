// SPDX-License-Identifier: MIT

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testAppVolumeStoreID = "44444444-4444-4444-8444-444444444444"
	testAppBucketStoreID = "55555555-5555-4555-8555-555555555555"
)

// assertSyncedAppStoreMounts runs lake#666 through the apps envtest: a volume and a bucket the app
// mounts reach its LakeApp spec and its pods, the bucket's credential is copied into the workspace
// namespace and rolls the pods on a new version, and removing the mounts takes them out again.
func assertSyncedAppStoreMounts(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	volume := LakeStore{ID: testAppVolumeStoreID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName, Type: storeTypeVolume, Provider: storeProviderCluster, Name: "scratch",
		Slug: "st-444444444444", Spec: `{"size_gi":5}`, Desired: serviceDesiredPresent, Generation: 1}
	bucket := LakeStore{ID: testAppBucketStoreID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName, Type: storeTypeObject, Provider: "r2", Name: "daily-reports",
		Slug: "st-555555555555", Desired: serviceDesiredPresent, Generation: 1,
		Spec: `{"bucket":"reports","endpoint":"https://acct.r2.cloudflarestorage.com","region":"auto","prefix":"daily/","credential":{"mount_id":"m","name":"r2"}}`}
	// A Lake-run store and another workspace's volume are never mounted.
	lakeRun := LakeStore{ID: "66666666-6666-4666-8666-666666666666", CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName, Type: storeTypeObject, Provider: storeProviderZetta, Name: "sealed",
		Slug: "st-666666666666", Spec: `{}`, Desired: serviceDesiredPresent, Generation: 1}
	h.store.stores = []LakeStore{volume, bucket, lakeRun}
	h.store.storeMounts = []LakeStoreMount{
		{StoreID: volume.ID, ObjectKind: StoreMountKindApp, ObjectID: testAppRowID, MountPath: "/data/scratch", ReadOnly: true},
		{StoreID: bucket.ID, ObjectKind: StoreMountKindApp, ObjectID: testAppRowID},
		{StoreID: lakeRun.ID, ObjectKind: StoreMountKindApp, ObjectID: testAppRowID},
	}
	h.store.rows[0].Generation++
	h.converge()
	app := h.app()
	if len(app.Spec.Stores) != 2 || app.Spec.Stores[0].Name != "daily-reports" || app.Spec.Stores[1].Name != "scratch" {
		t.Fatalf("app stores = %+v", app.Spec.Stores)
	}
	if got := app.Spec.Stores[1]; got.ClaimName != "apps-test-st-444444444444" || got.MountPath != "/data/scratch" || !got.ReadOnly {
		t.Fatalf("volume store = %+v", got)
	}
	if got := app.Spec.Stores[0]; got.Bucket != "reports" || got.SecretName != "apps-test-st-555555555555" || got.Prefix != "daily/" {
		t.Fatalf("bucket store = %+v", got)
	}
	if ready := apimeta.FindStatusCondition(app.Status.Conditions, "Ready"); ready == nil || ready.Reason != "StoreCredentialPending" {
		t.Fatalf("app before the bucket's credential: %+v", app.Status.Conditions)
	}

	// The store's delivery and the Secret it produced, as the bucket store reconcile and the
	// LakeSealedSecret controller leave them.
	sealed := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "apps-test-st-555555555555", Namespace: "lake", Labels: map[string]string{storeIDLabel: bucket.ID}},
		Spec: oramav1alpha1.LakeSealedSecretSpec{LakeRef: corev1.LocalObjectReference{Name: h.lake.Name},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "apps-test-st-555555555555"},
			Source:   oramav1alpha1.LakeSealedSecretSource{Type: "lake", Lake: &oramav1alpha1.LakeStaticSecretSource{Mount: "m", Name: "r2"}}},
	}
	if err := controllerutil.SetControllerReference(h.lake, sealed, h.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Create(h.ctx, sealed); err != nil {
		t.Fatal(err)
	}
	delivered := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "apps-test-st-555555555555", Namespace: "lake"},
		Data: map[string][]byte{"access_key_id": []byte("AKIA1"), "secret_access_key": []byte("s1"), "unrelated": []byte("x")}}
	if err := controllerutil.SetControllerReference(sealed, delivered, h.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Create(h.ctx, delivered); err != nil {
		t.Fatal(err)
	}
	h.converge()
	app = h.app()
	namespace := lakeAppNamespace(app)
	deploymentKey := client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadObjectName(app)}
	var deployment appsv1.Deployment
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	copyName := lakeAppStoreSecretName(app, "st-555555555555")
	var claimVolume *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "store-st-444444444444" {
			claimVolume = &pod.Volumes[i]
		}
	}
	if claimVolume == nil || claimVolume.PersistentVolumeClaim == nil || claimVolume.PersistentVolumeClaim.ClaimName != "apps-test-st-444444444444" ||
		!claimVolume.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("volumes = %+v", pod.Volumes)
	}
	mounted := false
	for _, mount := range pod.Containers[0].VolumeMounts {
		mounted = mounted || (mount.Name == "store-st-444444444444" && mount.MountPath == "/data/scratch" && mount.ReadOnly)
	}
	if !mounted {
		t.Fatalf("volume mounts = %+v", pod.Containers[0].VolumeMounts)
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != lakeAppRunAsUser {
		t.Fatalf("a mounted volume needs the app's group: %+v", pod.SecurityContext)
	}
	env := pod.Containers[0].Env
	assertLakeAppEnv(t, env, "STORE_DAILY_REPORTS_BUCKET", "reports")
	assertLakeAppEnv(t, env, "STORE_DAILY_REPORTS_ENDPOINT", "https://acct.r2.cloudflarestorage.com")
	assertLakeAppEnv(t, env, "STORE_DAILY_REPORTS_REGION", "auto")
	assertLakeAppEnv(t, env, "STORE_DAILY_REPORTS_PREFIX", "daily/")
	assertLakeAppSecretEnv(t, env, "STORE_DAILY_REPORTS_ACCESS_KEY_ID", copyName, "access_key_id")
	assertLakeAppSecretEnv(t, env, "STORE_DAILY_REPORTS_SAS_TOKEN", copyName, "sas_token")
	for _, variable := range env {
		if variable.Name == "STORE_SEALED_BUCKET" {
			t.Fatal("a Lake-run store was mounted")
		}
	}
	var copied corev1.Secret
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: namespace, Name: copyName}, &copied); err != nil {
		t.Fatal(err)
	}
	if len(copied.Data) != 2 || string(copied.Data["access_key_id"]) != "AKIA1" {
		t.Fatalf("copied bucket credential = %#v", copied.Data)
	}
	before := deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation]

	// A new credential version rolls the pods.
	delivered.Data["secret_access_key"] = []byte("s2")
	if err := h.client.Update(h.ctx, delivered); err != nil {
		t.Fatal(err)
	}
	h.converge()
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation] == before {
		t.Fatal("a new bucket credential did not roll the app")
	}

	// The mounts removed: the stores leave the pods, and the copy goes.
	h.store.storeMounts = nil
	h.store.rows[0].Generation++
	h.converge()
	if stores := h.app().Spec.Stores; len(stores) != 0 {
		t.Fatalf("stores kept after the mounts were removed: %+v", stores)
	}
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "store-st-444444444444" {
			t.Fatal("the volume stayed mounted")
		}
	}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: namespace, Name: copyName}, &copied); !apierrors.IsNotFound(err) {
		t.Fatalf("credential copy kept: %v", err)
	}
	h.store.stores = nil
}

// TestLakeAppStoresRequireSync: a hand-written LakeApp cannot mount a store.
func TestLakeAppStoresRequireSync(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	app := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{LakeRef: "prod",
		Stores: []oramav1alpha1.LakeAppStoreMount{{ID: "x", Name: "data", Slug: "st-444444444444", Type: "volume", Provider: "cluster"}}}}
	if reason, _ := validateLakeApp(lake, app); reason != "StoresRequireSync" {
		t.Fatalf("reason = %q", reason)
	}
}
