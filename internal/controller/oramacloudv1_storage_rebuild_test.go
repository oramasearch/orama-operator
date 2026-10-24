/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// rebuildStack is a stack on the cluster default class with a rebuild requested: two data
// StatefulSets, their claims (a retained reader ordinal among them), the control DB with its claim,
// a PostgresSchema and an owned Job, plus a claim that belongs to none of them.
func rebuildStack(t *testing.T, mut func(*oramav1alpha1.OramaCloudV1)) (*OramaCloudV1Reconciler, *oramav1alpha1.OramaCloudV1, *rebuildWrites) {
	t.Helper()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.StorageRebuild = &oramav1alpha1.StorageRebuildSpec{ID: "premium", InstanceUID: "uid-v1", DataLoss: "DiscardAll"}
	})
	if mut != nil {
		mut(oc)
	}
	controlled := func(obj client.Object) client.Object {
		obj.SetNamespace(oc.Namespace)
		obj.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))})
		return obj
	}
	claim := func(name, uid string, labels map[string]string) client.Object {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, UID: types.UID(uid), Labels: labels}}
	}
	controlDBClaim := claim("v1-controldb-1", "controldb-1", map[string]string{cnpgClusterLabel: "v1-controldb"})
	controlDBClaim.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "v1-controldb", UID: "cluster", Controller: boolPtr(true)}})
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("v1-controldb")
	objects := []client.Object{
		oc,
		controlled(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v1-index-builder"}}),
		controlled(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "v1-writer"}}),
		controlled(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "v1-reader"}}),
		controlled(cluster),
		controlled(&oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: "v1-controldb-schema"}}),
		controlled(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "v1-analytics-retention"}}),
		claim("data-v1-writer-0", "writer-0", nil),
		claim("data-v1-reader-0", "reader-0", nil),
		claim("data-v1-reader-3", "reader-3", nil),
		claim("data-v1-harvest-mongo-0", "mongo-0", nil),
		controlDBClaim,
		claim("team-a-runs", "runs", nil),
		claim("data-v1-writer-other-0", "lookalike", nil),
	}
	s := ocv1TestScheme(t)
	writes := &rebuildWrites{}
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(oc).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes.deleted = append(writes.deleted, obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if writes.failStatus {
				return fmt.Errorf("injected status write failure")
			}
			writes.status++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	return r, rebuildLive(t, r, oc), writes
}

type rebuildWrites struct {
	deleted    []string
	status     int
	failStatus bool
}

func rebuildLive(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1) *oramav1alpha1.OramaCloudV1 {
	t.Helper()
	return storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
}

func rebuildExists(t *testing.T, r *OramaCloudV1Reconciler, obj client.Object, name string) bool {
	t.Helper()
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func rebuildCondition(t *testing.T, oc *oramav1alpha1.OramaCloudV1, reason string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(oc.Status.Conditions, storageRebuildCondition)
	if c == nil || c.Reason != reason {
		t.Fatalf("StorageRebuild condition = %+v, want reason %s", c, reason)
	}
	return c
}

func TestStorageRebuildDeletesTheStacksDataOnceAndHandsBack(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	ctx := context.Background()

	proceed, err := r.reconcileStorageRebuild(ctx, oc)
	if err != nil || proceed {
		t.Fatalf("first pass = %v, %v; want the reconcile held while the old storage goes", proceed, err)
	}
	live := rebuildLive(t, r, oc)
	got := live.Status.StorageRebuild
	if got == nil || got.ID != "premium" || got.Phase != rebuildPhaseDeleting {
		t.Fatalf("status.storageRebuild = %+v", got)
	}
	var names []string
	for _, c := range got.Claims {
		names = append(names, c.Name+"="+c.UID)
	}
	want := "data-v1-harvest-mongo-0=mongo-0,data-v1-reader-0=reader-0,data-v1-reader-3=reader-3,data-v1-writer-0=writer-0,v1-controldb-1=controldb-1"
	if strings.Join(names, ",") != want {
		t.Fatalf("recorded claims %v, want %s", names, want)
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, "Ready"); c == nil || c.Reason != "StorageRebuilding" {
		t.Fatalf("Ready = %+v, want StorageRebuilding", c)
	}
	for _, gone := range []struct {
		obj  client.Object
		name string
	}{
		{&appsv1.Deployment{}, "v1-index-builder"}, {&appsv1.StatefulSet{}, "v1-writer"}, {&appsv1.StatefulSet{}, "v1-reader"},
		{&oramav1alpha1.PostgresSchema{}, "v1-controldb-schema"}, {&batchv1.Job{}, "v1-analytics-retention"},
		{&corev1.PersistentVolumeClaim{}, "data-v1-reader-3"}, {&corev1.PersistentVolumeClaim{}, "v1-controldb-1"},
	} {
		if rebuildExists(t, r, gone.obj, gone.name) {
			t.Fatalf("%s %s still exists", gone.obj.GetObjectKind().GroupVersionKind().Kind, gone.name)
		}
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if rebuildExists(t, r, cluster, "v1-controldb") {
		t.Fatal("the control DB's CNPG Cluster still exists")
	}
	for _, kept := range []string{"team-a-runs", "data-v1-writer-other-0"} {
		if !rebuildExists(t, r, &corev1.PersistentVolumeClaim{}, kept) {
			t.Fatalf("claim %s, which is not the stack's data, was deleted", kept)
		}
	}

	// The next pass finds everything gone, completes, and lets the normal reconcile build again.
	proceed, err = r.reconcileStorageRebuild(ctx, live)
	if err != nil || !proceed {
		t.Fatalf("second pass = %v, %v; want completed", proceed, err)
	}
	live = rebuildLive(t, r, oc)
	if live.Status.StorageRebuild.Phase != rebuildPhaseCompleted {
		t.Fatalf("phase = %s, want Completed", live.Status.StorageRebuild.Phase)
	}
	rebuildCondition(t, live, rebuildPhaseCompleted)

	// The same ID never runs again: a steady stack writes and deletes nothing.
	deletes, statuses := len(writes.deleted), writes.status
	for i := 0; i < 3; i++ {
		if proceed, err := r.reconcileStorageRebuild(ctx, live); err != nil || !proceed {
			t.Fatalf("completed rebuild pass = %v, %v", proceed, err)
		}
	}
	if len(writes.deleted) != deletes || writes.status != statuses {
		t.Fatalf("a completed rebuild wrote again: %d deletes, %d status writes", len(writes.deleted)-deletes, writes.status-statuses)
	}
}

func TestStorageRebuildIsRefusedAndDeletesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		mut           func(*oramav1alpha1.OramaCloudV1)
		extra         client.Object
	}{
		{name: "another instance's uid", refusal: "is not this OramaCloudV1's uid",
			mut: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.StorageRebuild.InstanceUID = "uid-of-a-deleted-instance" }},
		{name: "a migration in the namespace", refusal: "may have copied customer data",
			extra: &oramav1alpha1.OramaCloudV1Migration{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "orama-system"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, oc, writes := rebuildStack(t, tc.mut)
			if tc.extra != nil {
				if err := r.Create(context.Background(), tc.extra); err != nil {
					t.Fatal(err)
				}
			}
			proceed, err := r.reconcileStorageRebuild(context.Background(), oc)
			if err != nil || !proceed {
				t.Fatalf("refused rebuild = %v, %v; want the normal reconcile to go on", proceed, err)
			}
			live := rebuildLive(t, r, oc)
			if c := rebuildCondition(t, live, "Refused"); !strings.Contains(c.Message, tc.refusal) {
				t.Fatalf("refusal %q does not say %q", c.Message, tc.refusal)
			}
			if len(writes.deleted) != 0 || live.Status.StorageRebuild != nil {
				t.Fatalf("a refused rebuild deleted %v or recorded %+v", writes.deleted, live.Status.StorageRebuild)
			}
		})
	}
}

func TestStorageRebuildRecordsBeforeItDeletes(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	writes.failStatus = true
	if proceed, err := r.reconcileStorageRebuild(context.Background(), oc); err == nil || proceed {
		t.Fatalf("pass with an unrecordable rebuild = %v, %v; want an error", proceed, err)
	}
	if len(writes.deleted) != 0 {
		t.Fatalf("deleted %v without a record of what it deletes", writes.deleted)
	}
}

func TestStorageRebuildDeletesClaimsMadeAfterItStarted(t *testing.T) {
	r, oc, _ := rebuildStack(t, nil)
	ctx := context.Background()
	if _, err := r.reconcileStorageRebuild(ctx, oc); err != nil {
		t.Fatal(err)
	}
	// While the old StatefulSets wind down, one makes a claim for a new ordinal and another makes its
	// deleted claim again: both are old storage on the old template.
	for _, c := range []struct{ name, uid string }{{"data-v1-reader-5", "reader-5"}, {"data-v1-writer-0", "writer-0-again"}} {
		if err := r.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: oc.Namespace, UID: types.UID(c.uid)}}); err != nil {
			t.Fatal(err)
		}
	}
	if proceed, err := r.reconcileStorageRebuild(ctx, rebuildLive(t, r, oc)); err != nil || proceed {
		t.Fatalf("pass that found new claims = %v, %v; want held", proceed, err)
	}
	record := map[string]string{}
	for _, c := range rebuildLive(t, r, oc).Status.StorageRebuild.Claims {
		record[c.Name] = c.UID
	}
	if record["data-v1-reader-5"] != "reader-5" || record["data-v1-writer-0"] != "writer-0-again" {
		t.Fatalf("record %v does not hold the claims found after the start", record)
	}
	for _, name := range []string{"data-v1-reader-5", "data-v1-writer-0"} {
		if rebuildExists(t, r, &corev1.PersistentVolumeClaim{}, name) {
			t.Fatalf("%s, made after the rebuild started, survived it", name)
		}
	}
	if proceed, err := r.reconcileStorageRebuild(ctx, rebuildLive(t, r, oc)); err != nil || !proceed {
		t.Fatalf("final pass = %v, %v; want completed", proceed, err)
	}
}

func TestStorageRebuildNeverRunsACompletedIDAgain(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	ctx := context.Background()
	run := func(id string) {
		t.Helper()
		live := rebuildLive(t, r, oc)
		live.Spec.StorageRebuild.ID = id
		if err := r.Update(ctx, live); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := r.reconcileStorageRebuild(ctx, rebuildLive(t, r, oc)); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("first")
	run("second")
	if got := rebuildLive(t, r, oc).Status.StorageRebuild.CompletedIDs; strings.Join(got, ",") != "first,second" {
		t.Fatalf("completedIDs = %v", got)
	}
	// An older set file with the first rebuild in it comes back: nothing is deleted.
	if err := r.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: oc.Namespace, UID: "customer-data"}}); err != nil {
		t.Fatal(err)
	}
	before := len(writes.deleted)
	run("first")
	if len(writes.deleted) != before || !rebuildExists(t, r, &corev1.PersistentVolumeClaim{}, "data-v1-writer-0") {
		t.Fatalf("a completed id ran again and deleted %v", writes.deleted[before:])
	}
}

func TestStorageRebuildWaitingWritesStatusOnlyOnChange(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	ctx := context.Background()
	// Two objects stay terminating, so every pass waits on the same set.
	for _, held := range []struct {
		obj  client.Object
		name string
	}{{&batchv1.Job{}, "v1-analytics-retention"}, {&oramav1alpha1.PostgresSchema{}, "v1-controldb-schema"}} {
		live := storageGet(t, r, held.obj, oc.Namespace, held.name)
		live.SetFinalizers([]string{"test/hold"})
		if err := r.Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.reconcileStorageRebuild(ctx, rebuildLive(t, r, oc)); err != nil {
			t.Fatal(err)
		}
	}
	steady := writes.status
	for i := 0; i < 3; i++ {
		if _, err := r.reconcileStorageRebuild(ctx, rebuildLive(t, r, oc)); err != nil {
			t.Fatal(err)
		}
	}
	if writes.status != steady {
		t.Fatalf("an unchanged wait wrote status %d more times", writes.status-steady)
	}
	c := rebuildCondition(t, rebuildLive(t, r, oc), rebuildPhaseDeleting)
	if !strings.Contains(c.Message, "Job v1-analytics-retention, PostgresSchema v1-controldb-schema") {
		t.Fatalf("waiting message %q does not list the remaining objects in order", c.Message)
	}
}

func TestStorageRebuildRefusesAClaimAnotherControllerOwns(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	ctx := context.Background()
	foreign := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, "data-v1-writer-0")
	foreign.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "someone-else", UID: "x", Controller: boolPtr(true)}}
	if err := r.Update(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileStorageRebuild(ctx, oc); err == nil || !strings.Contains(err.Error(), "someone-else") {
		t.Fatalf("err = %v, want the foreign controller named", err)
	}
	if len(writes.deleted) != 0 {
		t.Fatalf("deleted %v", writes.deleted)
	}
}

func TestStorageRebuildRefusesAControlDBLabelOnAnotherObjectsClaim(t *testing.T) {
	r, oc, writes := rebuildStack(t, nil)
	ctx := context.Background()
	if err := r.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "lookalike-db", Namespace: oc.Namespace, UID: "x", Labels: map[string]string{cnpgClusterLabel: "v1-controldb"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileStorageRebuild(ctx, oc); err == nil || !strings.Contains(err.Error(), "lookalike-db") {
		t.Fatalf("err = %v, want the claim named", err)
	}
	if len(writes.deleted) != 0 {
		t.Fatalf("deleted %v", writes.deleted)
	}
}

func TestIndexBuilderRollsWhenTheControlDBPasswordChanges(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Ingestion.Enabled = true })
	if !slices.Contains(secretsReadBy(oc), controlDBAppSecret(oc)) {
		t.Fatalf("a change to %s does not reach the stack that index-builder reads it for", controlDBAppSecret(oc))
	}
}

func TestHarvestMongoStorageIsManagedLikeEveryDataStore(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion = oramav1alpha1.IngestionSpec{Enabled: true, Harvest: true, HarvestMongoStorage: storageSpec("20Gi", "managed-csi-premium")}
	})
	r, _ := storageFake(t, oc)
	ctx := context.Background()
	if err := r.provisionStatefulSet(ctx, oc, dataStoreDef{component: "harvest-mongo", image: "mongo:test", dataPath: "/data/db", storage: oc.Spec.Components.Ingestion.HarvestMongoStorage}); err != nil {
		t.Fatal(err)
	}
	sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-harvest-mongo")
	template := sts.Spec.VolumeClaimTemplates[0]
	if size := template.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "20Gi" || template.Spec.StorageClassName == nil || *template.Spec.StorageClassName != "managed-csi-premium" {
		t.Fatalf("harvest-mongo template = %s on %v", size.String(), template.Spec.StorageClassName)
	}
	// An existing claim on another class is reported like any other store's.
	storageClaim(t, r, oc, "data-v1-harvest-mongo-0", "10Gi", "default")
	r.reconcileStorage(ctx, oc)
	if c := storageCondition(t, r, oc, "StorageClassChangeNotSupported"); !strings.Contains(c.Message, "harvest-mongo: ") {
		t.Fatalf("StorageApplied does not report harvest-mongo: %s", c.Message)
	}
}
