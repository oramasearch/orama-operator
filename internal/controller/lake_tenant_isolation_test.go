/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The leftover of orama-operator#778: a tenant's CloudNativePG Cluster in the Lake namespace, with
// the Service and Secret CloudNativePG made from it (no service labels on those two).
const testLeftoverName = "prod-svc-5d97f8ed8de9"

func leftoverTenantPostgres(lake *oramav1alpha1.Lake) []client.Object {
	cluster := newCNPGCluster(testServiceNamespace, testLeftoverName)
	cluster.SetOwnerReferences(lakeOwnerRef(lake))
	cluster.SetLabels(map[string]string{serviceLakeLabel: lake.Name, serviceIDLabel: testPostgresID, serviceKindLabel: serviceKindPostgres})
	return []client.Object{
		cluster,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testLeftoverName + "-rw", Namespace: testServiceNamespace,
			Labels: map[string]string{cnpgClusterLabel: testLeftoverName}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testLeftoverName + "-app", Namespace: testServiceNamespace,
			Labels: map[string]string{cnpgClusterLabel: testLeftoverName}}},
	}
}

// A service row that names the Lake namespace as its workspace namespace is refused: nothing is
// created there, and an absent one deletes nothing there either.
func TestServicesNeverRunInTheLakeNamespace(t *testing.T) {
	lake := serviceTestLake()
	postgres := testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false)
	postgres.WorkspaceNamespace = testServiceNamespace
	zetta := testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false)
	zetta.WorkspaceNamespace = testServiceNamespace
	sqlite := testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false)
	sqlite.WorkspaceNamespace = testServiceNamespace
	store := &fakeServiceStore{services: []LakeService{postgres, zetta, sqlite}}
	h := newServiceHarness(t, lake, store)
	h.poll()

	want := lakeNamespaceServiceMessage(lake)
	for _, id := range []string{testPostgresID, testZettaServiceID, testSQLiteID} {
		expectStatus(t, store.status(t, id), ServicePhaseFailed, want, "")
	}
	ctx := context.Background()
	for _, object := range []client.Object{
		newCNPGCluster(testServiceNamespace, serviceResourceName(lake.Name, testPostgresSlug)),
		&oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName(lake.Name, testZettaSlug), Namespace: testServiceNamespace}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testSQLiteName, Namespace: testServiceNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSQLiteName + "-credentials", Namespace: testServiceNamespace}},
	} {
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s was created in the Lake namespace: %v", object, object.GetName(), err)
		}
	}

	// An absent service in the Lake namespace deletes nothing, not even an object it owns there.
	h2Lake := serviceTestLake()
	absent := testService(testPostgresID, "svc-5d97f8ed8de9", serviceKindPostgres, `{}`, false)
	absent.WorkspaceNamespace = testServiceNamespace
	absent.Desired = serviceDesiredAbsent
	h2Store := &fakeServiceStore{services: []LakeService{absent}}
	h2 := newServiceHarness(t, h2Lake, h2Store, leftoverTenantPostgres(h2Lake)...)
	h2.poll()
	expectStatus(t, h2Store.status(t, testPostgresID), ServicePhaseFailed, want, "")
	if len(*h2.deletes) != 0 {
		t.Fatalf("a service in the Lake namespace deleted %v", *h2.deletes)
	}
}

// Every object the operator writes for a service carries the Lake's controller reference and the
// service labels, so deleting the Lake or the service removes it (the Lake is cluster-scoped, so
// the reference holds across namespaces).
func TestEveryServiceObjectCarriesItsOwner(t *testing.T) {
	lake := serviceTestLake()
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
		testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	h.poll()

	check := func(object metav1.Object, id string) {
		t.Helper()
		if object.GetNamespace() != testServiceWorkspaceNamespace {
			t.Errorf("%s is in %q, not its workspace namespace", object.GetName(), object.GetNamespace())
		}
		if !metav1.IsControlledBy(object, h.lake) {
			t.Errorf("%s is not controlled by the Lake: %v", object.GetName(), object.GetOwnerReferences())
		}
		labels := object.GetLabels()
		for label, value := range map[string]string{
			serviceLakeLabel: lake.Name, serviceIDLabel: id, workspaceLabel: testWorkspaceID,
		} {
			if labels[label] != value {
				t.Errorf("%s label %s = %q, want %q", object.GetName(), label, labels[label], value)
			}
		}
	}
	check(h.zettaCluster(testZettaSlug), testZettaServiceID)
	check(h.postgresCluster(testPostgresSlug), testPostgresID)

	ctx := context.Background()
	selector := client.MatchingLabels{serviceIDLabel: testSQLiteID}
	var statefulSets appsv1.StatefulSetList
	var services corev1.ServiceList
	var secrets corev1.SecretList
	for _, list := range []client.ObjectList{&statefulSets, &services, &secrets} {
		if err := h.client.List(ctx, list, client.InNamespace(testServiceWorkspaceNamespace), selector); err != nil {
			t.Fatal(err)
		}
	}
	if len(statefulSets.Items) == 0 || len(services.Items) == 0 || len(secrets.Items) == 0 {
		t.Fatalf("sqlite objects: %d StatefulSets, %d Services, %d Secrets", len(statefulSets.Items), len(services.Items), len(secrets.Items))
	}
	for i := range statefulSets.Items {
		check(&statefulSets.Items[i], testSQLiteID)
	}
	for i := range services.Items {
		check(&services.Items[i], testSQLiteID)
	}
	for i := range secrets.Items {
		check(&secrets.Items[i], testSQLiteID)
	}
}

// A tenant service object left in the Lake namespace is reported, as the condition and one Warning
// event, and never deleted. The Lake's own objects there are not reported.
func TestTenantObjectsInTheLakeNamespaceAreReportedNeverDeleted(t *testing.T) {
	lake := serviceTestLake()
	own := []client.Object{
		&oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: lakeZettaName(lake.Name), Namespace: testServiceNamespace,
			OwnerReferences: lakeOwnerRef(lake)}},
		newCNPGCluster(testServiceNamespace, lakeControlDB(lake.Name)),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeServicesSecret(lake.Name), Namespace: testServiceNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": "lake"}}},
		// A bucket store's delivered credential lives in the Lake namespace by design.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: storeResourceName(lake.Name, testBucketStoreSlug), Namespace: testServiceNamespace,
			Labels: map[string]string{workspaceLabel: testWorkspaceID, storeIDLabel: testBucketStoreID}}},
		// The same service in its workspace namespace is where it belongs.
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testLeftoverName + "-rw", Namespace: testServiceWorkspaceNamespace}},
	}
	h := newServiceHarness(t, lake, &fakeServiceStore{}, append(own, leftoverTenantPostgres(lake)...)...)
	recorder := record.NewFakeRecorder(10)
	h.reconciler.Recorder = recorder
	ctx := context.Background()

	condition := h.reconciler.reconcileLakeTenantIsolation(ctx, h.lake, testServiceNamespace)
	if condition.Type != lakeTenantIsolationConditionType || condition.Status != metav1.ConditionFalse || condition.Reason != lakeTenantObjectsFoundReason {
		t.Fatalf("condition = %+v", condition)
	}
	for _, want := range []string{
		"3 tenant service object(s) are in the Lake namespace lake",
		"Cluster " + testLeftoverName, "Secret " + testLeftoverName + "-app", "Service " + testLeftoverName + "-rw",
		"does not delete them",
	} {
		if !strings.Contains(condition.Message, want) {
			t.Errorf("condition message %q lacks %q", condition.Message, want)
		}
	}
	for _, notWant := range []string{lakeZettaName(lake.Name), lakeControlDB(lake.Name), lakeServicesSecret(lake.Name), storeResourceName(lake.Name, testBucketStoreSlug)} {
		if strings.Contains(condition.Message, notWant) {
			t.Errorf("the Lake's own %s was reported: %q", notWant, condition.Message)
		}
	}
	select {
	case event := <-recorder.Events:
		if !strings.HasPrefix(event, corev1.EventTypeWarning+" "+lakeTenantObjectEventReason+" ") {
			t.Fatalf("event = %q", event)
		}
	default:
		t.Fatal("no Warning event for the leftover")
	}

	// The same finding again records no second event.
	apimeta.SetStatusCondition(&h.lake.Status.Conditions, condition)
	h.reconciler.reconcileLakeTenantIsolation(ctx, h.lake, testServiceNamespace)
	select {
	case event := <-recorder.Events:
		t.Fatalf("a repeated finding recorded another event: %q", event)
	default:
	}

	// Nothing was deleted: the leftovers are still there.
	if len(*h.deletes) != 0 {
		t.Fatalf("the check deleted %v", *h.deletes)
	}
	for _, object := range leftoverTenantPostgres(lake) {
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("leftover %s is gone: %v", object.GetName(), err)
		}
	}

	// Once a person has removed them, the Lake namespace is reported clean.
	for _, object := range leftoverTenantPostgres(lake) {
		if err := h.client.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	condition = h.reconciler.reconcileLakeTenantIsolation(ctx, h.lake, testServiceNamespace)
	if condition.Status != metav1.ConditionTrue || condition.Reason != lakeTenantIsolatedReason {
		t.Fatalf("condition after the cleanup = %+v", condition)
	}
}

// Services running in their workspace namespaces leave the Lake namespace clean.
func TestServicesInTheirWorkspaceNamespaceLeaveTheLakeNamespaceClean(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
		testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false),
	}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.poll()
	condition := h.reconciler.reconcileLakeTenantIsolation(context.Background(), h.lake, testServiceNamespace)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", condition)
	}
}

func TestTenantIsolationConditionNamesAtMostTenObjects(t *testing.T) {
	lake := serviceTestLake()
	var found []string
	for i := range 12 {
		found = append(found, fmt.Sprintf("Secret prod-svc-%012x-app", i))
	}
	condition := lakeTenantIsolationCondition(lake, testServiceNamespace, found, nil)
	if !strings.Contains(condition.Message, "12 tenant service object(s)") || !strings.Contains(condition.Message, "and 2 more") ||
		strings.Contains(condition.Message, found[10]) {
		t.Fatalf("message = %q", condition.Message)
	}
	failed := lakeTenantIsolationCondition(lake, testServiceNamespace, nil, fmt.Errorf("boom"))
	if failed.Status != metav1.ConditionUnknown || failed.Reason != lakeTenantIsolationCheckFailed {
		t.Fatalf("failed check = %+v", failed)
	}
}

func TestTenantObjectNamePattern(t *testing.T) {
	names := tenantObjectNamePattern(serviceTestLake())
	for _, name := range []string{
		"prod-svc-5d97f8ed8de9", "prod-svc-5d97f8ed8de9-rw", "prod-svc-5d97f8ed8de9-app", "prod-svc-5d97f8ed8de9-1",
		"prod-ws-0123456789ab", "data-prod-svc-111111111111-zetta-0", "data-prod-ws-0123456789ab-zetta-2",
	} {
		if !names.MatchString(name) {
			t.Errorf("%s is not recognised as a tenant service object", name)
		}
	}
	for _, name := range []string{
		"lake-prod", "lake-prod-zetta", "lake-prod-controldb", "lake-prod-controldb-app", "lake-prod-services",
		"prod-st-555555555555", "prod-al-666666666666", "prod-agent-project", "other-svc-5d97f8ed8de9", "prod-svc-short",
	} {
		if names.MatchString(name) {
			t.Errorf("%s is reported as a tenant service object", name)
		}
	}
}

// A workspace whose namespace would be the Lake namespace is refused before anything is made.
func TestWorkspaceNeverRunsInTheLakeNamespace(t *testing.T) {
	lake := testWorkspaceLake()
	lake.Spec.Namespace = testWorkspaceName
	s := newLakeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).Build()
	r := &LakeWorkspacesReconciler{Client: c, Scheme: s, APIReader: c}
	row := testWorkspaceRow("running")
	live := 0
	status, changing, err := r.reconcileLakeWorkspace(context.Background(), lake, &row, &live)
	if err != nil || changing {
		t.Fatalf("reconcile: changing %v, err %v", changing, err)
	}
	if status.Phase != WorkspacePhaseFailed || status.Message != lakeNamespaceServiceMessage(lake) {
		t.Fatalf("status = %+v", status)
	}
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := c.List(context.Background(), &clusters); err != nil || len(clusters.Items) != 0 {
		t.Fatalf("workspace Zetta made in the Lake namespace: %d, %v", len(clusters.Items), err)
	}
}
