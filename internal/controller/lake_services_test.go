/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testServiceNamespace = "lake"
	testServiceCustomer  = "7a1c9e2b-0000-4000-8000-0123456789ab"
	testZettaServiceID   = "11111111-1111-4111-8111-111111111111"
	testZettaSlug        = "svc-111111111111"
	testPostgresID       = "22222222-2222-4222-8222-222222222222"
	testPostgresSlug     = "svc-222222222222"
	testDefaultID        = "33333333-3333-4333-8333-333333333333"
	testDefaultSlug      = "ws-0123456789ab"
)

// fakeServiceStore is the in-memory LakeServiceStore: it serves the rows it holds and records
// every status upsert, applying it so the next read sees it.
type fakeServiceStore struct {
	mu       sync.Mutex
	services []LakeService
	upserts  []LakeServiceStatus
	opened   int
}

func (f *fakeServiceStore) ListServices(context.Context) ([]LakeService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]LakeService, 0, len(f.services))
	for _, svc := range f.services {
		if svc.Desired == serviceDesiredAbsent && svc.Status != nil && svc.Status.Phase == ServicePhaseDeleted {
			continue
		}
		if svc.Status != nil {
			status := *svc.Status
			svc.Status = &status
		}
		out = append(out, svc)
	}
	return out, nil
}

func (f *fakeServiceStore) UpsertServiceStatus(_ context.Context, status LakeServiceStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, status)
	for i := range f.services {
		if f.services[i].ID == status.ServiceID {
			copied := status
			f.services[i].Status = &copied
		}
	}
	return nil
}

func (f *fakeServiceStore) Close(context.Context) error { return nil }

func (f *fakeServiceStore) opener() LakeServiceStoreOpener {
	return func(context.Context, string) (LakeServiceStore, error) {
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		return f, nil
	}
}

func (f *fakeServiceStore) status(t *testing.T, id string) LakeServiceStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, svc := range f.services {
		if svc.ID == id {
			if svc.Status == nil {
				t.Fatalf("service %s has no status", id)
			}
			return *svc.Status
		}
	}
	t.Fatalf("service %s not in the store", id)
	return LakeServiceStatus{}
}

func (f *fakeServiceStore) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

func (f *fakeServiceStore) setDesired(id, desired string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.services {
		if f.services[i].ID == id {
			f.services[i].Desired = desired
			f.services[i].Generation++
		}
	}
}

func testService(id, slug, kind, spec string, isDefault bool) LakeService {
	return LakeService{ID: id, CustomerID: testServiceCustomer, Kind: kind, Name: kind, Slug: slug,
		IsDefault: isDefault, Spec: spec, Desired: serviceDesiredPresent, Generation: 1}
}

func serviceTestLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      testServiceNamespace,
			Image:          "oramasearch/lake:test",
			PullSecretName: "oramasearch-dockerhub",
			Services:       oramav1alpha1.LakeServicesSpec{Enabled: true, ZettaImage: "oramasearch/zetta:service"},
		},
	}
}

func controlDBAppSecretForTest(lake string, uri string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB(lake) + "-app", Namespace: testServiceNamespace},
		Data:       map[string][]byte{"uri": []byte(uri)},
	}
}

type serviceHarness struct {
	t          *testing.T
	client     client.Client
	scheme     *runtime.Scheme
	store      *fakeServiceStore
	reconciler *LakeReconciler
	lake       *oramav1alpha1.Lake
	deletes    *[]string
}

func newServiceHarness(t *testing.T, lake *oramav1alpha1.Lake, store *fakeServiceStore, objects ...client.Object) *serviceHarness {
	t.Helper()
	s := newLakeScheme(t)
	deletes := &[]string{}
	objects = append(objects, lake, controlDBAppSecretForTest(lake.Name, "postgresql://lake:pw@lake-prod-controldb-rw.lake:5432/lake"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.OramaZettaCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*deletes = append(*deletes, fmt.Sprintf("%T/%s", obj, obj.GetName()))
			return c.Delete(ctx, obj, opts...)
		}}).Build()
	return &serviceHarness{t: t, client: c, scheme: s, store: store, lake: lake, deletes: deletes,
		reconciler: &LakeReconciler{Client: c, Scheme: s, ServiceStores: store.opener()}}
}

func (h *serviceHarness) poll() {
	h.t.Helper()
	var lake oramav1alpha1.Lake
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: h.lake.Name}, &lake); err != nil {
		h.t.Fatalf("get lake: %v", err)
	}
	h.reconciler.reconcileLakeServices(context.Background(), &lake, testServiceNamespace, map[string]string{"app.kubernetes.io/name": "lake"})
}

func (h *serviceHarness) zettaCluster(slug string) *oramav1alpha1.OramaZettaCluster {
	h.t.Helper()
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: serviceResourceName("prod", slug)}, cluster); err != nil {
		h.t.Fatalf("get zetta service cluster %s: %v", slug, err)
	}
	return cluster
}

func (h *serviceHarness) postgresCluster(slug string) *unstructured.Unstructured {
	h.t.Helper()
	cluster := newCNPGCluster(testServiceNamespace, serviceResourceName("prod", slug))
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		h.t.Fatalf("get postgres service cluster %s: %v", slug, err)
	}
	return cluster
}

func (h *serviceHarness) credentials() map[string][]byte {
	h.t.Helper()
	var secret corev1.Secret
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: lakeServicesSecret("prod")}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		h.t.Fatalf("get services secret: %v", err)
	}
	return secret.Data
}

func (h *serviceHarness) markZettaReady(slug string, nodes int32) {
	h.t.Helper()
	cluster := h.zettaCluster(slug)
	cluster.Status.Phase = "Ready"
	cluster.Status.ReadyNodes = nodes
	if err := h.client.Status().Update(context.Background(), cluster); err != nil {
		h.t.Fatalf("mark ready: %v", err)
	}
}

func (h *serviceHarness) create(obj client.Object) {
	h.t.Helper()
	if err := h.client.Create(context.Background(), obj); err != nil {
		h.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func expectStatus(t *testing.T, got LakeServiceStatus, phase, message, endpoint string) {
	t.Helper()
	if got.Phase != phase || got.Message != message || got.Endpoint != endpoint {
		t.Fatalf("status = %+v, want phase %q message %q endpoint %q", got, phase, message, endpoint)
	}
}

func lakeOwnerRef(lake *oramav1alpha1.Lake) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: lake.Name, UID: lake.UID, Controller: &controller}}
}

func zettaDataClaim(cluster string, ordinal int, deleting bool) *corev1.PersistentVolumeClaim {
	owner := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster}}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: fmt.Sprintf("data-%s-zetta-%d", cluster, ordinal), Namespace: testServiceNamespace, Labels: zettaLabels(owner),
	}}
	if deleting {
		now := metav1.Now()
		claim.DeletionTimestamp = &now
		claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	}
	return claim
}

func TestServicesZettaCreateAndReadyGating(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{"instances":2,"storage_gi":20}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()

	cluster := h.zettaCluster(testZettaSlug)
	if !metav1.IsControlledBy(cluster, h.lake) {
		t.Fatal("service cluster is not controlled by the Lake")
	}
	for label, want := range map[string]string{serviceLakeLabel: "prod", serviceIDLabel: testZettaServiceID, serviceKindLabel: serviceKindZetta} {
		if cluster.Labels[label] != want {
			t.Errorf("label %s = %q, want %q", label, cluster.Labels[label], want)
		}
	}
	if cluster.Annotations[serviceCustomerAnnotation] != testServiceCustomer {
		t.Errorf("customer annotation = %q", cluster.Annotations[serviceCustomerAnnotation])
	}
	if cluster.Spec.Replicas != 2 || cluster.Spec.Storage.Size != "20Gi" || cluster.Spec.Image != "oramasearch/zetta:service" ||
		cluster.Spec.IngressEnabled || cluster.Spec.ProvisionOperatorSchema == nil || *cluster.Spec.ProvisionOperatorSchema ||
		cluster.Spec.PullSecretName != "oramasearch-dockerhub" {
		t.Fatalf("unexpected zetta service spec: %+v", cluster.Spec)
	}
	endpoint := "http://prod-svc-111111111111-zetta.lake.svc.cluster.local:4800"
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseProvisioning, "waiting for the Zetta nodes", endpoint)

	// One of two nodes ready is not ready.
	h.markZettaReady(testZettaSlug, 1)
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseProvisioning, "waiting for the Zetta nodes", endpoint)

	// Every node ready but no admin token yet: not Ready, and no credential key.
	h.markZettaReady(testZettaSlug, 2)
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseProvisioning, "waiting for the service credential", endpoint)
	if _, found := h.credentials()[testZettaSlug+".json"]; found {
		t.Fatal("credential written before the admin token exists")
	}

	h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-svc-111111111111-zetta-admin", Namespace: testServiceNamespace},
		Data: map[string][]byte{"token": []byte("zetta-admin-token")}})
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseReady, "service ready", endpoint)
	var credential map[string]interface{}
	if err := json.Unmarshal(h.credentials()[testZettaSlug+".json"], &credential); err != nil {
		t.Fatalf("credential: %v", err)
	}
	if credential["kind"] != "zetta" || credential["endpoint"] != endpoint || credential["token"] != "zetta-admin-token" || len(credential) != 3 {
		t.Fatalf("unexpected zetta credential: %v", credential)
	}
	if got := store.status(t, testZettaServiceID).ObservedGeneration; got != 1 {
		t.Fatalf("observed_generation = %d", got)
	}
}

func TestServicesPostgresCreateAndCredential(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{"instances":3,"storage_gi":15}`, false)}}
	lake := serviceTestLake()
	lake.Spec.Postgres.StorageClassName = "managed-premium"
	h := newServiceHarness(t, lake, store)
	h.poll()

	cluster := h.postgresCluster(testPostgresSlug)
	if !metav1.IsControlledBy(cluster, h.lake) || cluster.GetLabels()[serviceKindLabel] != serviceKindPostgres || cluster.GetLabels()[serviceIDLabel] != testPostgresID {
		t.Fatalf("unexpected postgres metadata: %v %v", cluster.GetOwnerReferences(), cluster.GetLabels())
	}
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	class, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	database, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "database")
	owner, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "owner")
	if instances != 3 || size != "15Gi" || class != "managed-premium" || database != "app" || owner != "app" {
		t.Fatalf("unexpected CNPG spec: %v", cluster.Object["spec"])
	}
	endpoint := "prod-svc-222222222222-rw.lake.svc.cluster.local:5432"
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseProvisioning, "waiting for the Postgres instances", endpoint)

	// Ready instances must equal spec instances.
	if err := unstructured.SetNestedField(cluster.Object, int64(2), "status", "readyInstances"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseProvisioning, "waiting for the Postgres instances", endpoint)

	cluster = h.postgresCluster(testPostgresSlug)
	if err := unstructured.SetNestedField(cluster.Object, int64(3), "status", "readyInstances"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseProvisioning, "waiting for the service credential", endpoint)

	uri := "postgresql://app:secret@prod-svc-222222222222-rw.lake:5432/app"
	h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-svc-222222222222-app", Namespace: testServiceNamespace},
		Data: map[string][]byte{"username": []byte("app"), "password": []byte("secret"), "dbname": []byte("app"), "uri": []byte(uri),
			"host": []byte("prod-svc-222222222222-rw"), "port": []byte("5432")}})
	h.poll()
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseReady, "service ready", endpoint)
	raw := h.credentials()[testPostgresSlug+".json"]
	want := `{"kind":"postgres","host":"prod-svc-222222222222-rw.lake.svc.cluster.local","port":5432,"database":"app","user":"app","password":"secret","uri":"` + uri + `"}`
	if string(raw) != want {
		t.Fatalf("postgres credential =\n%s\nwant\n%s", raw, want)
	}
}

func TestServicesAdoptWorkspaceClusterKeepingItsData(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services.MaxServices = 1
	name := serviceResourceName("prod", testDefaultSlug)
	workspace := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testServiceNamespace, OwnerReferences: lakeOwnerRef(lake),
			Labels:      map[string]string{workspaceIDLabel: testDefaultSlug, serviceLakeLabel: "prod", "app.kubernetes.io/name": "lake-workspace"},
			Annotations: map[string]string{serviceCustomerAnnotation: testServiceCustomer}},
		Spec: oramav1alpha1.OramaZettaClusterSpec{Replicas: 1, Storage: oramav1alpha1.OramaZettaStorage{Size: "5Gi"}},
	}
	claim := zettaDataClaim(name, 0, false)
	store := &fakeServiceStore{services: []LakeService{testService(testDefaultID, testDefaultSlug, serviceKindZetta, `{"instances":1,"storage_gi":10}`, true)}}
	h := newServiceHarness(t, lake, store, workspace, claim)
	h.poll()

	adopted := h.zettaCluster(testDefaultSlug)
	if adopted.Labels[serviceIDLabel] != testDefaultID || adopted.Labels[workspaceIDLabel] != testDefaultSlug {
		t.Fatalf("workspace not adopted: %v", adopted.Labels)
	}
	if adopted.Spec.Storage.Size != "5Gi" {
		t.Fatalf("adoption resized the data volume template to %q", adopted.Spec.Storage.Size)
	}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(claim), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("the workspace data volume is gone: %v", err)
	}
	if len(*h.deletes) != 0 {
		t.Fatalf("adoption deleted %v", *h.deletes)
	}
	// Adoption is not a new service: the quota of 1, already used by this workspace, does not refuse it.
	expectStatus(t, store.status(t, testDefaultID), ServicePhaseProvisioning, "waiting for the Zetta nodes",
		"http://prod-ws-0123456789ab-zetta.lake.svc.cluster.local:4800")
}

func TestServicesRefuseForeignResources(t *testing.T) {
	lake := serviceTestLake()
	// A cluster of the right name the Lake does not control.
	foreign := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName("prod", testZettaSlug), Namespace: testServiceNamespace,
		Labels: map[string]string{serviceLakeLabel: "prod", serviceIDLabel: testZettaServiceID}}}
	// A workspace cluster of a non-default service's slug: only the default service adopts.
	workspace := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName("prod", testDefaultSlug), Namespace: testServiceNamespace,
		OwnerReferences: lakeOwnerRef(lake), Labels: map[string]string{workspaceIDLabel: testDefaultSlug, serviceLakeLabel: "prod"}}}
	// A Lake-controlled CNPG cluster labelled as another service.
	postgres := newCNPGCluster(testServiceNamespace, serviceResourceName("prod", testPostgresSlug))
	postgres.SetOwnerReferences(lakeOwnerRef(lake))
	postgres.SetLabels(map[string]string{serviceLakeLabel: "prod", serviceIDLabel: "44444444-4444-4444-8444-444444444444"})
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testDefaultID, testDefaultSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store, foreign, workspace, postgres)
	h.poll()

	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseFailed, "OramaZettaCluster lake/prod-svc-111111111111 exists and is not this Lake's service", "")
	expectStatus(t, store.status(t, testDefaultID), ServicePhaseFailed, "OramaZettaCluster lake/prod-ws-0123456789ab exists and is not this Lake's service", "")
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "CloudNativePG Cluster lake/prod-svc-222222222222 exists and is not this Lake's service", "")
	if h.zettaCluster(testDefaultSlug).Labels[serviceIDLabel] != "" {
		t.Fatal("a non-default service adopted a workspace cluster")
	}

	// Deleting them is refused too, and nothing is deleted.
	store.setDesired(testZettaServiceID, serviceDesiredAbsent)
	store.setDesired(testPostgresID, serviceDesiredAbsent)
	h.poll()
	if len(*h.deletes) != 0 {
		t.Fatalf("deleted a foreign resource: %v", *h.deletes)
	}
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseFailed, "OramaZettaCluster lake/prod-svc-111111111111 exists and is not this Lake's service", "")
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "CloudNativePG Cluster lake/prod-svc-222222222222 exists and is not this Lake's service", "")

	// A live data volume nothing proves is this service's blocks creation.
	h2Store := &fakeServiceStore{services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false)}}
	h2 := newServiceHarness(t, serviceTestLake(), h2Store, zettaDataClaim(serviceResourceName("prod", testZettaSlug), 0, false))
	h2.poll()
	expectStatus(t, h2Store.status(t, testZettaServiceID), ServicePhaseFailed,
		"PersistentVolumeClaim lake/data-prod-svc-111111111111-zetta-0 exists and is not this Lake's service", "")
}

func TestServicesZettaDeletionOrderAndDeleted(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{"instances":2}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	name := serviceResourceName("prod", testZettaSlug)
	h.create(zettaDataClaim(name, 0, false))
	h.create(zettaDataClaim(name, 1, false))
	h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-zetta-admin", Namespace: testServiceNamespace}, Data: map[string][]byte{"token": []byte("t")}})
	h.markZettaReady(testZettaSlug, 2)
	h.poll()
	if store.status(t, testZettaServiceID).Phase != ServicePhaseReady || h.credentials()[testZettaSlug+".json"] == nil {
		t.Fatalf("service not Ready before deletion: %+v", store.status(t, testZettaServiceID))
	}

	// Hold the cluster with a finalizer so the first pass sees it still there.
	cluster := h.zettaCluster(testZettaSlug)
	cluster.Finalizers = []string{"test/hold"}
	if err := h.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	store.setDesired(testZettaServiceID, serviceDesiredAbsent)
	h.poll()
	want := []string{
		"*v1.PersistentVolumeClaim/data-prod-svc-111111111111-zetta-0",
		"*v1.PersistentVolumeClaim/data-prod-svc-111111111111-zetta-1",
		"*v1alpha1.OramaZettaCluster/prod-svc-111111111111",
	}
	if fmt.Sprint(*h.deletes) != fmt.Sprint(want) {
		t.Fatalf("deletion order = %v, want %v", *h.deletes, want)
	}
	if _, found := h.credentials()[testZettaSlug+".json"]; found {
		t.Fatal("credential key kept for an absent service")
	}
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseDeleting, "deleting the Zetta cluster",
		"http://prod-svc-111111111111-zetta.lake.svc.cluster.local:4800")
	if got := store.status(t, testZettaServiceID).ObservedGeneration; got != 2 {
		t.Fatalf("observed_generation = %d, want 2", got)
	}

	cluster = h.zettaCluster(testZettaSlug)
	cluster.Finalizers = nil
	if err := h.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseDeleted, "service deleted", "")

	// History: a Deleted absent service is skipped, so a further poll writes nothing.
	before := store.upsertCount()
	h.poll()
	if store.upsertCount() != before {
		t.Fatal("a Deleted service was written again")
	}
}

func TestServicesPostgresDeletionWaitsForVolumes(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	name := serviceResourceName("prod", testPostgresSlug)
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name + "-1", Namespace: testServiceNamespace,
		Labels: map[string]string{cnpgClusterLabel: name}}}
	h.create(claim)

	store.setDesired(testPostgresID, serviceDesiredAbsent)
	h.poll()
	if fmt.Sprint(*h.deletes) != "[*unstructured.Unstructured/"+name+"]" {
		t.Fatalf("deletes = %v", *h.deletes)
	}
	// The garbage collector has not removed the volume yet.
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseDeleting, "deleting the Postgres data volumes",
		"prod-svc-222222222222-rw.lake.svc.cluster.local:5432")

	if err := h.client.Delete(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	h.poll()
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseDeleted, "service deleted", "")
}

func TestServicesQuotaRefusesOnlyNewServices(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services.MaxServices = 1
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	if store.status(t, testZettaServiceID).Phase != ServicePhaseProvisioning {
		t.Fatalf("first service: %+v", store.status(t, testZettaServiceID))
	}
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "service quota reached (1)", "")
	postgres := newCNPGCluster(testServiceNamespace, serviceResourceName("prod", testPostgresSlug))
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(postgres), postgres); !apierrors.IsNotFound(err) {
		t.Fatalf("a service over quota was created: %v", err)
	}

	// Raising the cap starts the waiting service.
	var current oramav1alpha1.Lake
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: "prod"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Services.MaxServices = 2
	if err := h.client.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if store.status(t, testPostgresID).Phase != ServicePhaseProvisioning {
		t.Fatalf("service did not start once there was room: %+v", store.status(t, testPostgresID))
	}

	// Lowering the cap below the live count refuses neither existing service.
	current = oramav1alpha1.Lake{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: "prod"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Services.MaxServices = 1
	if err := h.client.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	h.poll()
	for _, id := range []string{testZettaServiceID, testPostgresID} {
		if phase := store.status(t, id).Phase; phase != ServicePhaseProvisioning {
			t.Fatalf("existing service %s refused after lowering the cap: %s", id, phase)
		}
	}

	// An absent service frees its slot in the same pass.
	store.setDesired(testZettaServiceID, serviceDesiredAbsent)
	store.services = append(store.services, testService("55555555-5555-4555-8555-555555555555", "svc-555555555555", serviceKindZetta, `{}`, false))
	current = oramav1alpha1.Lake{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: "prod"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Services.MaxServices = 2
	if err := h.client.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if phase := store.status(t, "55555555-5555-4555-8555-555555555555").Phase; phase != ServicePhaseProvisioning {
		t.Fatalf("new service after a deletion: %s", phase)
	}
}

func TestServicesQuotaFallsBackToMaxWorkspaces(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services = oramav1alpha1.LakeServicesSpec{}
	lake.Spec.Workspaces = oramav1alpha1.LakeWorkspacesSpec{Enabled: true, Image: "oramasearch/zetta:workspace", MaxWorkspaces: 1}
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	if h.zettaCluster(testZettaSlug).Spec.Image != "oramasearch/zetta:workspace" {
		t.Fatal("spec.workspaces.image is not the fallback Zetta image")
	}
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "service quota reached (1)", "")
}

func TestServicesStatusUpsertOnlyOnChange(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	if store.upsertCount() != 2 {
		t.Fatalf("first pass wrote %d statuses, want 2", store.upsertCount())
	}
	h.poll()
	h.poll()
	if store.upsertCount() != 2 {
		t.Fatalf("steady passes wrote statuses: %d", store.upsertCount())
	}
	h.markZettaReady(testZettaSlug, 1)
	h.poll()
	if store.upsertCount() != 3 {
		t.Fatalf("a transition wrote %d statuses, want 3", store.upsertCount())
	}
}

func TestServicesDeferUntilControlDBSecretIsReady(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false)}}
	s := newLakeScheme(t)
	lake := serviceTestLake()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: s, ServiceStores: store.opener()}
	r.reconcileLakeServices(context.Background(), lake, testServiceNamespace, nil)
	if err := c.Create(context.Background(), controlDBAppSecretForTest("prod", "")); err != nil {
		t.Fatal(err)
	}
	r.reconcileLakeServices(context.Background(), lake, testServiceNamespace, nil)
	if store.opened != 0 || store.upsertCount() != 0 {
		t.Fatalf("store used before the app Secret had a uri: opened %d, upserts %d", store.opened, store.upsertCount())
	}
}

func TestServicesInvalidSpecFails(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{"instances":4}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseFailed, "spec.instances must be between 1 and 3", "")
}

func TestLakeServicesDeploymentMountAndEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*oramav1alpha1.Lake)
		want   bool
	}{
		{"services", func(*oramav1alpha1.Lake) {}, true},
		{"deprecated workspaces", func(l *oramav1alpha1.Lake) {
			l.Spec.Services = oramav1alpha1.LakeServicesSpec{}
			l.Spec.Workspaces.Enabled = true
		}, true},
		{"disabled", func(l *oramav1alpha1.Lake) { l.Spec.Services = oramav1alpha1.LakeServicesSpec{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLakeScheme(t)
			lake := serviceTestLake()
			tc.mutate(lake)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}).Build()
			applyLakeSchemaForWorkloadTest(t, c, s, lake)
			store := &fakeServiceStore{}
			r := &LakeReconciler{Client: c, Scheme: s, ServiceStores: store.opener()}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod"}})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var deploy appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: "lake-prod"}, &deploy); err != nil {
				t.Fatal(err)
			}
			pod := deploy.Spec.Template.Spec
			env := map[string]string{}
			for _, e := range pod.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if _, found := env["LAKE_WORKSPACE_DIR"]; found {
				t.Error("LAKE_WORKSPACE_DIR is still injected")
			}
			if _, found := env["LAKE_WORKSPACE_URL_TEMPLATE"]; found {
				t.Error("LAKE_WORKSPACE_URL_TEMPLATE is still injected")
			}
			mounted := false
			for _, mount := range pod.Containers[0].VolumeMounts {
				if mount.Name == "services" && mount.MountPath == "/var/run/lake/services" && mount.ReadOnly {
					mounted = true
				}
			}
			volume := false
			for _, v := range pod.Volumes {
				if v.Name == "services" && v.Secret != nil && v.Secret.SecretName == "lake-prod-services" && v.Secret.Optional != nil && *v.Secret.Optional {
					volume = true
				}
			}
			if tc.want {
				if env["LAKE_SERVICE_DIR"] != "/var/run/lake/services" || !mounted || !volume {
					t.Fatalf("services not wired into lake: env %v mounted %v volume %v", env, mounted, volume)
				}
				if result.RequeueAfter != defaultDeploymentSyncInterval {
					t.Fatalf("requeue = %v, want the services poll interval", result.RequeueAfter)
				}
				if store.opened != 0 {
					t.Fatal("store opened before the control DB app Secret exists")
				}
			} else if _, found := env["LAKE_SERVICE_DIR"]; found || mounted || volume {
				t.Fatal("disabled services still wired into lake")
			}
			// The data plane no longer provisions the operator schema for workspace intake.
			var dataPlane oramav1alpha1.OramaZettaCluster
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: lakeZettaName("prod")}, &dataPlane); err != nil {
				t.Fatal(err)
			}
			if dataPlane.Spec.ProvisionOperatorSchema == nil || *dataPlane.Spec.ProvisionOperatorSchema {
				t.Fatalf("data plane provisions the operator schema: %v", dataPlane.Spec.ProvisionOperatorSchema)
			}
		})
	}
}

func TestLakeZettaReconcileKeepsServiceClusters(t *testing.T) {
	lake := serviceTestLake()
	service := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName("prod", testZettaSlug), Namespace: testServiceNamespace,
		OwnerReferences: lakeOwnerRef(lake), Labels: map[string]string{serviceLakeLabel: "prod", serviceIDLabel: testZettaServiceID}}}
	s := newLakeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, service).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	if _, err := r.reconcileLakeZetta(context.Background(), lake, testServiceNamespace, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(service), &oramav1alpha1.OramaZettaCluster{}); err != nil {
		t.Fatalf("spec.zetta reconcile removed a service cluster: %v", err)
	}
}

func TestParseServiceSpecDefaults(t *testing.T) {
	spec, err := parseServiceSpec(`{}`)
	if err != nil || spec.Instances != 1 || spec.StorageGi != 10 {
		t.Fatalf("defaults = %+v, %v", spec, err)
	}
	for _, raw := range []string{`{"instances":0}`, `{"storage_gi":101}`, `{"instances":"two"}`, `[]`} {
		if _, err := parseServiceSpec(raw); err == nil {
			t.Errorf("spec %s accepted", raw)
		}
	}
}
