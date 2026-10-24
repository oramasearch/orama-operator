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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const premium = "managed-csi-premium"

// approved is storage declaring size and class with data deletion approved.
func approved(size, class string) *oramav1alpha1.VolumeSpec {
	spec := storageSpec(size, class)
	spec.ApproveDataDeletion = true
	return spec
}

type replaceWrites struct {
	deleted []string
	status  int
}

// replaceStack is a stack whose volumes were made on the cluster's default class: the writer's
// StatefulSet and claim, a reader with one claim already on premium and one on default, the
// analytics store with its retention Job, the control DB with its claim, a PostgresSchema and the c1
// row's Job, and a claim that belongs to none of them.
func replaceStack(t *testing.T, mut func(*oramav1alpha1.OramaCloudV1)) (*OramaCloudV1Reconciler, *oramav1alpha1.OramaCloudV1, *replaceWrites) {
	t.Helper()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Enabled = true
		o.Spec.Components.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
	})
	if mut != nil {
		mut(oc)
	}
	controlled := func(obj client.Object) client.Object {
		obj.SetNamespace(oc.Namespace)
		obj.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))})
		return obj
	}
	statefulSet := func(name, class string) client.Object {
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataPVC(storageSpec("10Gi", class), defaultVolumeSize)}
		return controlled(sts)
	}
	claim := func(name, uid, class string) *corev1.PersistentVolumeClaim {
		c := dataPVC(storageSpec("10Gi", class), defaultVolumeSize)
		c.Name, c.Namespace, c.UID = name, oc.Namespace, types.UID(uid)
		return &c
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("v1-controldb")
	if err := unstructured.SetNestedMap(cluster.Object, map[string]interface{}{"size": "10Gi", "storageClass": "default"}, "spec", "storage"); err != nil {
		t.Fatal(err)
	}
	controlDBClaim := claim("v1-controldb-1", "controldb-1", "default")
	controlDBClaim.Labels = map[string]string{cnpgClusterLabel: "v1-controldb"}
	controlDBClaim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "v1-controldb", UID: "cluster", Controller: boolPtr(true)}}
	objects := []client.Object{
		oc,
		statefulSet("v1-writer", "default"), statefulSet("v1-reader", premium), statefulSet("v1-analytics", "default"),
		controlled(cluster),
		controlled(&oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: "v1-controldb-schema"}}),
		controlled(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: controlDBClusterRowJobName(oc)}}),
		controlled(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: analyticsRetentionName(oc)}}),
		controlled(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v1-analytics-processor"}}),
		claim("data-v1-writer-0", "writer-0", "default"),
		claim("data-v1-reader-0", "reader-0", premium), claim("data-v1-reader-1", "reader-1", "default"),
		claim("data-v1-analytics-0", "analytics-0", "default"),
		controlDBClaim,
		claim("team-a-runs", "runs", "default"),
	}
	s := ocv1TestScheme(t)
	writes := &replaceWrites{}
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(oc).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes.deleted = append(writes.deleted, obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes.status++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	return r, replaceLive(t, r, oc), writes
}

func replaceLive(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1) *oramav1alpha1.OramaCloudV1 {
	t.Helper()
	return storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
}

func replaceExists(t *testing.T, r *OramaCloudV1Reconciler, obj client.Object, name string) bool {
	t.Helper()
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// replaceUntilDone runs passes until the ordinary reconcile may go on, and returns how many held it.
func replaceUntilDone(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1) int {
	t.Helper()
	for held := 0; held < 10; held++ {
		proceed, err := r.reconcileVolumeReplacement(context.Background(), replaceLive(t, r, oc))
		if err != nil {
			t.Fatal(err)
		}
		if proceed {
			return held
		}
	}
	t.Fatal("the replacement never finished")
	return 0
}

func TestVolumesAreNotReplacedWithoutApproval(t *testing.T) {
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = storageSpec("512Gi", premium)
		o.Spec.Components.ControlDB.Storage = storageSpec("10Gi", premium)
	})
	ctx := context.Background()
	if proceed, err := r.reconcileVolumeReplacement(ctx, oc); err != nil || !proceed {
		t.Fatalf("pass = %v, %v; want the ordinary reconcile to go on", proceed, err)
	}
	if len(writes.deleted) != 0 {
		t.Fatalf("deleted %v without approveDataDeletion", writes.deleted)
	}
	r.reconcileStorage(ctx, oc)
	c := storageCondition(t, r, oc, "StorageClassChangeNotSupported")
	if !strings.Contains(c.Message, "needs storage.approveDataDeletion") {
		t.Fatalf("StorageApplied does not say how to replace the volume: %s", c.Message)
	}
}

func TestApprovedVolumesAreReplacedAndTheReconcileWaitsForThem(t *testing.T) {
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = approved("512Gi", premium)
		o.Spec.Components.Engine.Reader.Storage = approved("512Gi", premium)
		o.Spec.Components.Analytics.Storage = approved("200Gi", premium)
		o.Spec.Components.ControlDB.Storage = approved("10Gi", premium)
	})
	ctx := context.Background()
	proceed, err := r.reconcileVolumeReplacement(ctx, oc)
	if err != nil || proceed {
		t.Fatalf("first pass = %v, %v; want the reconcile held while volumes are replaced", proceed, err)
	}
	live := replaceLive(t, r, oc)
	for _, condition := range []string{"Ready", "StorageApplied"} {
		if c := meta.FindStatusCondition(live.Status.Conditions, condition); c == nil || c.Reason != "ReplacingStorage" {
			t.Fatalf("%s = %+v, want ReplacingStorage", condition, c)
		}
	}
	for _, gone := range []struct {
		obj  client.Object
		name string
	}{
		{&appsv1.StatefulSet{}, "v1-writer"}, {&appsv1.StatefulSet{}, "v1-analytics"},
		{&corev1.PersistentVolumeClaim{}, "data-v1-writer-0"}, {&corev1.PersistentVolumeClaim{}, "data-v1-reader-1"},
		{&corev1.PersistentVolumeClaim{}, "data-v1-analytics-0"}, {&corev1.PersistentVolumeClaim{}, "v1-controldb-1"},
		{&oramav1alpha1.PostgresSchema{}, "v1-controldb-schema"},
		{&batchv1.Job{}, controlDBClusterRowJobName(oc)}, {&batchv1.Job{}, analyticsRetentionName(oc)},
		{&appsv1.Deployment{}, "v1-analytics-processor"},
	} {
		if replaceExists(t, r, gone.obj, gone.name) {
			t.Fatalf("%s survived its replacement", gone.name)
		}
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if replaceExists(t, r, cluster, "v1-controldb") {
		t.Fatal("the control DB's Cluster on the default class survived")
	}
	// The reader's claim already on premium, and a claim of no volume, are kept.
	for _, kept := range []string{"data-v1-reader-0", "team-a-runs"} {
		if !replaceExists(t, r, &corev1.PersistentVolumeClaim{}, kept) {
			t.Fatalf("%s was deleted, but it needs no replacement", kept)
		}
	}
	// With everything gone, the ordinary reconcile goes on and builds the volumes again.
	replaceUntilDone(t, r, oc)
	if slices.Contains(writes.deleted, "data-v1-reader-0") || slices.Contains(writes.deleted, "team-a-runs") {
		t.Fatalf("deleted a claim that needs no replacement: %v", writes.deleted)
	}
}

func TestReplacementIsDeclarativeNotOneTime(t *testing.T) {
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = approved("512Gi", premium)
	})
	ctx := context.Background()
	replaceUntilDone(t, r, oc)
	// The ordinary reconcile makes the writer again on premium: with approval still declared, a steady
	// stack is left alone and causes no writes.
	if err := r.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: oc.Namespace, UID: "writer-premium"},
		Spec: dataPVC(storageSpec("512Gi", premium), defaultVolumeSize).Spec}); err != nil {
		t.Fatal(err)
	}
	deletes, statuses := len(writes.deleted), writes.status
	for i := 0; i < 3; i++ {
		if held := replaceUntilDone(t, r, oc); held != 0 {
			t.Fatal("a writer already on its declared storage was replaced again")
		}
	}
	if len(writes.deleted) != deletes || writes.status != statuses {
		t.Fatalf("a steady stack wrote: %d deletes, %d status writes", len(writes.deleted)-deletes, writes.status-statuses)
	}
	// A later change to the declared storage is reconciled the same way.
	live := replaceLive(t, r, oc)
	live.Spec.Components.Engine.Writer.Storage = approved("512Gi", "managed-csi-premium-v2")
	if err := r.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	replaceUntilDone(t, r, oc)
	if replaceExists(t, r, &corev1.PersistentVolumeClaim{}, "data-v1-writer-0") {
		t.Fatal("the writer was not replaced after its declared class changed")
	}
}

func TestApprovedShrinkReplacesTheVolume(t *testing.T) {
	r, oc, _ := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = approved("5Gi", "")
	})
	replaceUntilDone(t, r, oc)
	if replaceExists(t, r, &corev1.PersistentVolumeClaim{}, "data-v1-writer-0") {
		t.Fatal("a 10Gi writer survived a declared 5Gi with approveDataDeletion")
	}
}

func TestOnlyDeclaredSizeAndClassReplaceAVolume(t *testing.T) {
	// Approval alone, with no size or class declared, never replaces a volume grown past the default
	// or one on a non-default class.
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = &oramav1alpha1.VolumeSpec{ApproveDataDeletion: true}
	})
	ctx := context.Background()
	claim := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, "data-v1-writer-0")
	claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("512Gi")
	if err := r.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if held := replaceUntilDone(t, r, oc); held != 0 || len(writes.deleted) != 0 {
		t.Fatalf("an undeclared size or class replaced a volume: held %d, deleted %v", held, writes.deleted)
	}
}

func TestAStatefulSetTemplateOnTheOldClassIsReplacedWithItsClaimsKept(t *testing.T) {
	r, oc, _ := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = approved("10Gi", premium)
	})
	ctx := context.Background()
	claim := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, "data-v1-writer-0")
	claim.Spec.StorageClassName = ptrTo(premium)
	if err := r.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	replaceUntilDone(t, r, oc)
	if replaceExists(t, r, &appsv1.StatefulSet{}, "v1-writer") {
		t.Fatal("the StatefulSet whose template names the old class survived")
	}
	if !replaceExists(t, r, &corev1.PersistentVolumeClaim{}, "data-v1-writer-0") {
		t.Fatal("a claim already on the declared class was deleted")
	}
}

func TestVolumesAnotherControllerOwnsAreNeverReplaced(t *testing.T) {
	for _, tc := range []struct {
		name, claim string
		labels      map[string]string
		mut         func(*oramav1alpha1.OramaCloudV1)
	}{
		{name: "a data claim", claim: "data-v1-writer-0",
			mut: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.Writer.Storage = approved("512Gi", premium)
			}},
		{name: "a claim with the control DB's label", claim: "v1-controldb-1", labels: map[string]string{cnpgClusterLabel: "v1-controldb"},
			mut: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Storage = approved("10Gi", premium) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, oc, writes := replaceStack(t, tc.mut)
			ctx := context.Background()
			claim := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, tc.claim)
			claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "someone-else", UID: "x", Controller: boolPtr(true)}}
			if err := r.Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
			if tc.labels != nil {
				// The Cluster is gone, so its claims are examined on their own.
				cluster := &unstructured.Unstructured{}
				cluster.SetGroupVersionKind(cnpgClusterGVK)
				cluster.SetName("v1-controldb")
				cluster.SetNamespace(oc.Namespace)
				if err := r.Client.Delete(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				writes.deleted = nil
			}
			_, err := r.reconcileVolumeReplacement(ctx, oc)
			if err == nil || !strings.Contains(err.Error(), "someone-else") {
				t.Fatalf("err = %v, want the foreign controller named", err)
			}
			if slices.Contains(writes.deleted, tc.claim) {
				t.Fatalf("deleted %s, which another controller owns", tc.claim)
			}
		})
	}
}

func TestWaitingForAReplacementWritesStatusOnlyOnChange(t *testing.T) {
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage = approved("512Gi", premium)
		o.Spec.Components.Analytics.Storage = approved("200Gi", premium)
	})
	ctx := context.Background()
	for _, name := range []string{"data-v1-writer-0", "data-v1-analytics-0"} {
		claim := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, name)
		claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
		if err := r.Update(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.reconcileVolumeReplacement(ctx, replaceLive(t, r, oc)); err != nil {
			t.Fatal(err)
		}
	}
	steady := writes.status
	for i := 0; i < 3; i++ {
		if _, err := r.reconcileVolumeReplacement(ctx, replaceLive(t, r, oc)); err != nil {
			t.Fatal(err)
		}
	}
	if writes.status != steady {
		t.Fatalf("an unchanged wait wrote status %d more times", writes.status-steady)
	}
	c := meta.FindStatusCondition(replaceLive(t, r, oc).Status.Conditions, "Ready")
	if c == nil || !strings.Contains(c.Message, "claim data-v1-analytics-0, claim data-v1-writer-0") {
		t.Fatalf("waiting message %q does not list what remains in order", fmt.Sprint(c))
	}
}

func TestAControlDBAlreadyOnTheDeclaredClassIsKept(t *testing.T) {
	// The Cluster was created without a class; its claim is on "default", which is now declared.
	r, oc, writes := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB.Storage = approved("10Gi", "default")
	})
	ctx := context.Background()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster = storageGet(t, r, cluster, oc.Namespace, "v1-controldb")
	unstructured.RemoveNestedField(cluster.Object, "spec", "storage", "storageClass")
	if err := r.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if held := replaceUntilDone(t, r, oc); held != 0 || len(writes.deleted) != 0 {
		t.Fatalf("a control DB already on its declared storage was replaced: held %d, deleted %v", held, writes.deleted)
	}
}

func TestAnInterruptedControlDBReplacementStillCleansUp(t *testing.T) {
	// The Cluster's deletion started, but the pass that started it stopped before the schemas and the
	// c1 row's Job went: the next pass removes them.
	r, oc, _ := replaceStack(t, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB.Storage = approved("10Gi", premium)
	})
	ctx := context.Background()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster = storageGet(t, r, cluster, oc.Namespace, "v1-controldb")
	cluster.SetFinalizers([]string{"cnpg.io/hold"})
	if err := r.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileVolumeReplacement(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if replaceExists(t, r, &oramav1alpha1.PostgresSchema{}, "v1-controldb-schema") || replaceExists(t, r, &batchv1.Job{}, controlDBClusterRowJobName(oc)) {
		t.Fatal("the old database's schema record or c1 row Job survived an interrupted replacement")
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
		o.Spec.Components.Ingestion = oramav1alpha1.IngestionSpec{Enabled: true, Harvest: true, HarvestMongoStorage: storageSpec("20Gi", premium)}
	})
	r, _ := storageFake(t, oc)
	ctx := context.Background()
	if err := r.provisionStatefulSet(ctx, oc, dataStoreDef{component: "harvest-mongo", image: "mongo:test", dataPath: "/data/db", storage: oc.Spec.Components.Ingestion.HarvestMongoStorage}); err != nil {
		t.Fatal(err)
	}
	sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-harvest-mongo")
	template := sts.Spec.VolumeClaimTemplates[0]
	if size := template.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "20Gi" || template.Spec.StorageClassName == nil || *template.Spec.StorageClassName != premium {
		t.Fatalf("harvest-mongo template = %s on %v", size.String(), template.Spec.StorageClassName)
	}
	storageClaim(t, r, oc, "data-v1-harvest-mongo-0", "10Gi", "default")
	r.reconcileStorage(ctx, oc)
	if c := storageCondition(t, r, oc, "StorageClassChangeNotSupported"); !strings.Contains(c.Message, "harvest-mongo: ") {
		t.Fatalf("StorageApplied does not report harvest-mongo: %s", c.Message)
	}
}
