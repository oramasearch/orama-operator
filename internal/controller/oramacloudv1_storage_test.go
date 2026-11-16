// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func storageSpec(size, class string) *oramav1alpha1.VolumeSpec {
	spec := &oramav1alpha1.VolumeSpec{}
	if size != "" {
		q := resource.MustParse(size)
		spec.Size = &q
	}
	if class != "" {
		spec.StorageClassName = &class
	}
	return spec
}

type storageWrites struct {
	patches    map[string]int
	status     int
	fail       string
	failList   bool
	failCreate string
}

func storageFake(t *testing.T, oc *oramav1alpha1.OramaCloudV1) (*OramaCloudV1Reconciler, *storageWrites) {
	t.Helper()
	s := ocv1TestScheme(t)
	writes := &storageWrites{patches: map[string]int{}}
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(oc).WithObjects(oc).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == writes.failCreate {
				return fmt.Errorf("injected workload creation denial")
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes.patches[obj.GetName()]++
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			var fields map[string]interface{}
			if err := json.Unmarshal(data, &fields); err != nil {
				return err
			}
			// Every storage patch must have optimistic concurrency and only its one size field.
			metadata, ok := fields["metadata"].(map[string]interface{})
			if !ok || metadata["resourceVersion"] == nil || len(fields) != 2 {
				t.Errorf("unsafe storage patch: %s", data)
			}
			wantSpec := map[string]interface{}{}
			switch v := obj.(type) {
			case *corev1.PersistentVolumeClaim:
				size := v.Spec.Resources.Requests[corev1.ResourceStorage]
				wantSpec["resources"] = map[string]interface{}{"requests": map[string]interface{}{"storage": size.String()}}
			case *unstructured.Unstructured:
				size, _, _ := unstructured.NestedString(v.Object, "spec", "storage", "size")
				wantSpec["storage"] = map[string]interface{}{"size": size}
			}
			if !reflect.DeepEqual(fields["spec"], wantSpec) {
				t.Errorf("patch changed more than size: %s", data)
			}
			if obj.GetName() == writes.fail {
				return fmt.Errorf("injected expansion denial")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PersistentVolumeClaimList); ok && writes.failList {
				return fmt.Errorf("injected claim read denial")
			}
			return c.List(ctx, list, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if sub == "status" {
				writes.status++
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	return &OramaCloudV1Reconciler{Client: cl, Scheme: s}, writes
}

func storageGet[T client.Object](t *testing.T, r *OramaCloudV1Reconciler, obj T, ns, name string) T {
	t.Helper()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// storageCondition asserts StorageApplied at the current generation. want is "Applied", or the
// problems the message must name, joined by "+" (for example "ShrinkNotSupported+ClaimsPending").
func storageCondition(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1, want string) *metav1.Condition {
	t.Helper()
	live := storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
	c := meta.FindStatusCondition(live.Status.Conditions, "StorageApplied")
	reason, status := "NotApplied", metav1.ConditionFalse
	if want == "Applied" {
		reason, status = "Applied", metav1.ConditionTrue
	}
	if c == nil || c.Reason != reason || c.Status != status || c.ObservedGeneration != oc.Generation {
		t.Fatalf("storage condition = %+v, want %s at generation %d", c, want, oc.Generation)
	}
	if want != "Applied" {
		for _, problem := range strings.Split(want, "+") {
			if !strings.Contains(c.Message, problem+": ") {
				t.Fatalf("storage condition message %q does not name %s", c.Message, problem)
			}
		}
	}
	return c
}

func storageClaim(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1, name, size, class string) *corev1.PersistentVolumeClaim {
	t.Helper()
	claim := dataPVC(storageSpec(size, class), defaultVolumeSize)
	claim.Name, claim.Namespace = name, oc.Namespace
	claim.Labels = map[string]string{"keep": "unchanged"}
	claim.Annotations = map[string]string{"retain": "me"}
	if err := r.Create(context.Background(), &claim); err != nil {
		t.Fatal(err)
	}
	return &claim
}

func assertClaimSize(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1, name, size string) {
	t.Helper()
	claim := storageGet(t, r, &corev1.PersistentVolumeClaim{}, oc.Namespace, name)
	got := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse(size)) != 0 {
		t.Fatalf("%s requests %s, want %s", name, got.String(), size)
	}
	if claim.Annotations["retain"] != "me" || claim.Labels["keep"] != "unchanged" {
		t.Fatalf("claim metadata changed: %+v", claim.ObjectMeta)
	}
}

func TestOCV1StorageFreshVolumes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spec        *oramav1alpha1.VolumeSpec
		size, class string
	}{
		{"omitted", nil, "10Gi", ""}, {"empty", &oramav1alpha1.VolumeSpec{}, "10Gi", ""},
		{"missing size", storageSpec("", "managed-csi"), "10Gi", "managed-csi"},
		{"explicit", storageSpec("50Gi", "managed-csi"), "50Gi", "managed-csi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newOCV1(nil)
			oc.Spec.Components.Engine.Writer.Storage = tc.spec
			oc.Spec.Components.Engine.Reader.Storage = tc.spec
			oc.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true, Storage: tc.spec}}
			r, _ := storageFake(t, oc)
			ctx := context.Background()
			if err := r.reconcileWriter(ctx, oc, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
				t.Fatal(err)
			}
			reader := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, oc.Name+"-reader")
			if !reflect.DeepEqual(reader.Spec.VolumeClaimTemplates, []corev1.PersistentVolumeClaim{dataPVC(tc.spec, readerDataSize)}) {
				t.Fatalf("reader template: %+v", reader.Spec.VolumeClaimTemplates)
			}
			for _, comp := range []string{"analytics", "cache", "broker", "search"} {
				if err := r.reconcileDataStore(ctx, oc, oramav1alpha1.DataStoreSpec{Enabled: true, Storage: tc.spec}, dataStoreDef{component: comp, image: "store:test", dataPath: "/data"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.provisionStatefulSet(ctx, oc, dataStoreDef{component: "harvest-mongo", image: "mongo:test", dataPath: "/data"}); err != nil {
				t.Fatal(err)
			}
			for _, comp := range []string{"writer", "analytics", "cache", "broker", "search", "harvest-mongo"} {
				sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, oc.Name+"-"+comp)
				want := tc.spec
				if comp == "harvest-mongo" {
					want = nil
				}
				if !reflect.DeepEqual(sts.Spec.VolumeClaimTemplates, []corev1.PersistentVolumeClaim{dataPVC(want, defaultVolumeSize)}) {
					t.Fatalf("%s template: %+v", comp, sts.Spec.VolumeClaimTemplates)
				}
			}
			if err := r.reconcileControlDB(ctx, oc); err != nil {
				t.Fatal(err)
			}
			cluster := &unstructured.Unstructured{}
			cluster.SetGroupVersionKind(cnpgClusterGVK)
			storageGet(t, r, cluster, oc.Namespace, oc.Name+"-controldb")
			got, _, _ := unstructured.NestedMap(cluster.Object, "spec", "storage")
			if !reflect.DeepEqual(got, controlDBStorage(tc.spec)) {
				t.Fatalf("CNPG storage: %v", got)
			}
		})
	}
}

func TestOCV1StoragePreservesStatefulTemplates(t *testing.T) {
	ctx := context.Background()
	for _, component := range []string{"writer", "reader", "cache"} {
		t.Run(component, func(t *testing.T) {
			oc := newOCV1(nil)
			r, writes := storageFake(t, oc)
			provision := func() {
				t.Helper()
				var err error
				switch component {
				case "writer":
					err = r.reconcileWriter(ctx, oc, "", "")
				case "reader":
					err = r.reconcileReaders(ctx, oc, "", "", "")
				default:
					err = r.reconcileCache(ctx, oc)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			oc.Spec.Components.Engine.Enabled = true
			oc.Spec.Components.Cache.Enabled = true
			provision()
			sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, oc.Name+"-"+component)
			sts.UID = types.UID("preserved-sts")
			sts.Spec.VolumeClaimTemplates[0].Annotations = map[string]string{"admission": "preserve"}
			class := "default-admitted"
			sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName = &class
			mode := corev1.PersistentVolumeFilesystem
			sts.Spec.VolumeClaimTemplates[0].Spec.VolumeMode = &mode
			if err := r.Update(ctx, sts); err != nil {
				t.Fatal(err)
			}
			original := sts.DeepCopy()
			for _, spec := range []*oramav1alpha1.VolumeSpec{nil, storageSpec("10240Mi", ""), storageSpec("20Gi", ""), storageSpec("5Gi", ""), storageSpec("20Gi", "new-class")} {
				oc.Spec.Components.Engine.Writer.Storage = spec
				oc.Spec.Components.Engine.Reader.Storage = spec
				oc.Spec.Components.Cache.Storage = spec
				oc.Spec.Images.Overrides = map[string]string{"oramacore": "engine:changed", "cache": "cache:changed"}
				provision()
				current := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, sts.Name)
				if current.UID != original.UID || !reflect.DeepEqual(current.Spec.VolumeClaimTemplates, original.Spec.VolumeClaimTemplates) {
					t.Fatal("immutable template or UID changed")
				}
				if !strings.HasSuffix(current.Spec.Template.Spec.Containers[0].Image, ":changed") {
					t.Fatal("mutable pod image did not reconcile")
				}
			}
			if len(writes.patches) != 0 {
				t.Fatalf("provisioning patched storage: %v", writes.patches)
			}
		})
	}
}

func TestOCV1StorageGrowthMatchingClaimsAndPending(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Components.Engine.Enabled = true
	oc.Spec.Components.Engine.Writer.Storage = storageSpec("20Gi", "")
	oc.Spec.Components.Engine.Reader.Replicas = 1
	r, writes := storageFake(t, oc)
	if err := r.reconcileWriter(ctx, oc, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
		t.Fatal(err)
	}
	storageClaim(t, r, oc, "data-v1-reader-0", readerDataSize, "managed-csi")
	for _, name := range []string{"data-v1-writer-0", "data-v1-writer-4", "data-v1-writer-00", "data-v1-writer-1-extra", "data-v1-writer-other-0", "data-v1-controldb-0"} {
		storageClaim(t, r, oc, name, "10Gi", "managed-csi")
	}
	otherNS := oc.DeepCopy()
	otherNS.Namespace = "elsewhere"
	storageClaim(t, r, otherNS, "data-v1-writer-0", "10Gi", "managed-csi")
	if !r.reconcileStorage(ctx, oc) {
		t.Fatal("managed storage must poll")
	}
	storageCondition(t, r, oc, "Applied")
	for _, name := range []string{"data-v1-writer-0", "data-v1-writer-4"} {
		assertClaimSize(t, r, oc, name, "20Gi")
	}
	if len(writes.patches) != 2 {
		t.Fatalf("patched unrelated claims: %v", writes.patches)
	}
	assertClaimSize(t, r, otherNS, "data-v1-writer-0", "10Gi")
	before := writes.status
	r.reconcileStorage(ctx, oc)
	if writes.status != before || writes.patches["data-v1-writer-0"] != 1 || writes.patches["data-v1-writer-4"] != 1 {
		t.Fatal("repeated reconcile wrote unchanged storage/status")
	}
	// No StatefulSet controller in fake client: create a later required claim at the old template size.
	sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-writer")
	sts.Spec.Replicas = int32Ptr(2)
	if err := r.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	r.reconcileStorage(ctx, oc)
	c := storageCondition(t, r, oc, "ClaimsPending")
	if !strings.Contains(c.Message, "data-v1-writer-1") {
		t.Fatal(c.Message)
	}
	storageClaim(t, r, oc, "data-v1-writer-1", "10Gi", "managed-csi")
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "Applied")
	assertClaimSize(t, r, oc, "data-v1-writer-1", "20Gi")
}

func TestOCV1StorageUnsupportedChangesAndRecovery(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, size, class, reason string }{
		{"shrink", "5Gi", "", "ShrinkNotSupported"},
		{"class blocks growth", "30Gi", "premium", "StorageClassChangeNotSupported"},
		{"combined", "5Gi", "premium", "ShrinkNotSupported+StorageClassChangeNotSupported"},
		{"removed storage", "", "", "ShrinkNotSupported"},
		{"equivalent rounded capacity", "20480Mi", "", "Applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newOCV1(nil)
			oc.Generation = 3
			oc.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Storage: storageSpec(tc.size, tc.class)}
			if tc.name == "removed storage" {
				oc.Spec.Components.Cache.Storage = nil
			}
			meta.SetStatusCondition(&oc.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllComponentsReady", ObservedGeneration: 3})
			oc.Status.Phase = "Ready"
			r, writes := storageFake(t, oc)
			if err := r.reconcileCache(ctx, oc); err != nil {
				t.Fatal(err)
			}
			claim := storageClaim(t, r, oc, "data-v1-cache-0", "20Gi", "managed-csi")
			claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Gi")}
			if err := r.Status().Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
			r.reconcileStorage(ctx, oc)
			c := storageCondition(t, r, oc, tc.reason)
			if tc.reason != "Applied" && !strings.Contains(c.Message, claim.Name) {
				t.Fatal(c.Message)
			}
			assertClaimSize(t, r, oc, claim.Name, "20Gi")
			if len(writes.patches) != 0 {
				t.Fatalf("unsupported/equal volume patched: %v", writes.patches)
			}
			live := storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
			if live.Status.Phase != "Ready" || !meta.IsStatusConditionTrue(live.Status.Conditions, "Ready") {
				t.Fatal("storage changed Ready")
			}
			before := writes.status
			r.reconcileStorage(ctx, oc)
			if writes.status != before {
				t.Fatal("unchanged rejection rewrote status")
			}
			oc.Spec.Components.Cache.Storage = storageSpec("20Gi", "managed-csi")
			oc.Generation++
			if err := r.Update(ctx, oc); err != nil {
				t.Fatal(err)
			}
			r.reconcileStorage(ctx, oc)
			storageCondition(t, r, oc, "Applied")
		})
	}
}

func TestOCV1StorageFailureIsolationRetryAndReady(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Storage: storageSpec("20Gi", "")}
	oc.Spec.Components.Search = oramav1alpha1.DataStoreSpec{Enabled: true, Storage: storageSpec("30Gi", "")}
	r, writes := storageFake(t, oc)
	// The cache brings its REST shim (#562), a Deployment that must be ready too.
	for _, provision := range []func(context.Context, *oramav1alpha1.OramaCloudV1) error{r.reconcileCache, r.reconcileRedisHTTP, r.reconcileSearch} {
		if err := provision(ctx, oc); err != nil {
			t.Fatal(err)
		}
	}
	for _, component := range []string{"cache", "search"} {
		storageClaim(t, r, oc, "data-v1-"+component+"-0", "10Gi", "managed-csi")
		sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-"+component)
		sts.Status.ReadyReplicas = 1
		if err := r.Status().Update(ctx, sts); err != nil {
			t.Fatal(err)
		}
	}
	shim := storageGet(t, r, &appsv1.Deployment{}, oc.Namespace, "v1-redis-http")
	shim.Status.ReadyReplicas = 1
	if err := r.Status().Update(ctx, shim); err != nil {
		t.Fatal(err)
	}
	writes.fail = "data-v1-cache-0"
	reconcile := func() ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != 20*time.Second {
			t.Fatalf("retry = %v", result)
		}
		return result
	}
	reconcile()
	c := storageCondition(t, r, oc, "StorageUpdateFailed")
	if !strings.Contains(c.Message, "injected expansion denial") {
		t.Fatal(c.Message)
	}
	assertClaimSize(t, r, oc, "data-v1-search-0", "30Gi")
	live := storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
	if !meta.IsStatusConditionTrue(live.Status.Conditions, "Ready") {
		t.Fatalf("ready lost: %+v", live.Status)
	}
	before := writes.status
	reconcile()
	if before != writes.status {
		t.Fatal("repeated failure rewrote status")
	}
	writes.fail = ""
	reconcile()
	storageCondition(t, r, oc, "Applied")
	assertClaimSize(t, r, oc, "data-v1-cache-0", "20Gi")
	before = writes.status
	reconcile()
	if before != writes.status {
		t.Fatal("repeated success rewrote status")
	}
}

func TestOCV1StorageCNPGPreservationAndFailure(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
	r, writes := storageFake(t, oc)
	if err := r.reconcileControlDB(ctx, oc); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	storageGet(t, r, cluster, oc.Namespace, "v1-controldb")
	original := map[string]interface{}{"size": "10Gi", "storageClass": "managed-csi", "resizeInUseVolumes": true, "pvcTemplate": map[string]interface{}{"metadata": map[string]interface{}{"annotations": map[string]interface{}{"keep": "yes"}}}}
	if err := unstructured.SetNestedMap(cluster.Object, original, "spec", "storage"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	claim := storageClaim(t, r, oc, "v1-controldb-1", "10Gi", "managed-csi")
	for _, tc := range []struct{ size, class, reason string }{
		{"20Gi", "", "StorageUpdateFailed"}, {"20Gi", "", "Applied"},
		{"5Gi", "premium", "ShrinkNotSupported+StorageClassChangeNotSupported"}, {"30Gi", "premium", "StorageClassChangeNotSupported"},
		{"20480Mi", "", "Applied"},
	} {
		oc.Spec.Components.ControlDB.Storage = storageSpec(tc.size, tc.class)
		oc.Spec.Profile = "heavy"
		schema := storageGet(t, r, &oramav1alpha1.PostgresSchema{}, oc.Namespace, "v1-controldb-schema")
		schema.Spec.Version = "stale-schema"
		if err := r.Update(ctx, schema); err != nil {
			t.Fatal(err)
		}
		writes.fail = ""
		if tc.reason == "StorageUpdateFailed" {
			writes.fail = "v1-controldb"
		}
		if err := r.reconcileControlDB(ctx, oc); err != nil {
			t.Fatal(err)
		}
		r.reconcileStorage(ctx, oc)
		storageCondition(t, r, oc, tc.reason)
		storageGet(t, r, cluster, oc.Namespace, cluster.GetName())
		got, _, _ := unstructured.NestedMap(cluster.Object, "spec", "storage")
		want := "20Gi"
		if tc.reason == "StorageUpdateFailed" {
			want = "10Gi"
		}
		expected := map[string]interface{}{}
		for k, v := range original {
			expected[k] = v
		}
		expected["size"] = want
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("storage=%v want %v", got, expected)
		}
		instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
		if instances != 3 {
			t.Fatal("unrelated spec did not reconcile")
		}
		schema = storageGet(t, r, &oramav1alpha1.PostgresSchema{}, oc.Namespace, "v1-controldb-schema")
		if schema.Spec.Version != schemaVersion(dashboardSchemaSQL) {
			t.Fatal("storage rejection blocked schema reconciliation")
		}
		assertClaimSize(t, r, oc, claim.Name, "10Gi")
	}
	if writes.patches["v1-controldb"] != 2 {
		t.Fatalf("CNPG patch attempts = %v", writes.patches)
	}
	// Adding an explicit class to an existing classless Cluster is also unsupported.
	storageGet(t, r, cluster, oc.Namespace, cluster.GetName())
	unstructured.RemoveNestedField(cluster.Object, "spec", "storage", "storageClass")
	if err := r.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	oc.Spec.Components.ControlDB.Storage = storageSpec("30Gi", "managed-csi")
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "StorageClassChangeNotSupported")
}

func TestOCV1StorageMultipleIssuesAndSkippedComponents(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Region.Role = "read"
	oc.Spec.Components.Engine.Enabled = true
	oc.Spec.Components.Engine.Writer.Storage = storageSpec("1Gi", "premium")
	oc.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external", Storage: storageSpec("1Gi", "premium")}
	oc.Spec.Components.Broker = oramav1alpha1.DataStoreSpec{Enabled: true, Placement: "central", Storage: storageSpec("1Gi", "premium")}
	oc.Spec.Components.Analytics.Storage = storageSpec("1Gi", "premium")
	oc.Spec.Components.Engine.Reader.Replicas = 1
	r, writes := storageFake(t, oc)
	// A read region still runs readers, so their claims are the only managed storage.
	if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
		t.Fatal(err)
	}
	storageClaim(t, r, oc, "data-v1-reader-0", readerDataSize, "managed-csi")
	if !r.reconcileStorage(ctx, oc) {
		t.Fatal("reader storage must be managed")
	}
	if c := storageCondition(t, r, oc, "Applied"); strings.Contains(c.Message, "writer") {
		t.Fatalf("a read region managed writer storage: %s", c.Message)
	}
	oc.Spec.Components.Cache.Mode = "in-cluster"
	oc.Spec.Components.Cache.Storage = storageSpec("5Gi", "premium")
	oc.Spec.Components.Search.Enabled = true
	if err := r.Update(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileCache(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSearch(ctx, oc); err != nil {
		t.Fatal(err)
	}
	storageClaim(t, r, oc, "data-v1-cache-0", "20Gi", "managed-csi")
	r.reconcileStorage(ctx, oc)
	c := storageCondition(t, r, oc, "ShrinkNotSupported+StorageClassChangeNotSupported+ClaimsPending")
	before := c.Message
	r.reconcileStorage(ctx, oc)
	if storageCondition(t, r, oc, "ShrinkNotSupported+StorageClassChangeNotSupported+ClaimsPending").Message != before {
		t.Fatal("message order changed")
	}
	if len(writes.patches) != 0 {
		t.Fatal("patched incompatible class")
	}
	writes.failList = true
	r.reconcileStorage(ctx, oc)
	c = storageCondition(t, r, oc, "StorageUpdateFailed")
	if !strings.Contains(c.Message, "injected claim read denial") {
		t.Fatal(c.Message)
	}
}

// A workload error must retain the controller's ordinary failure path; storage cannot turn it
// into success simply because no claim has appeared yet.
func TestOCV1StorageDoesNotMaskWorkloadErrors(t *testing.T) {
	oc := newOCV1(nil)
	oc.Spec.Components.Cache.Enabled = true
	r, writes := storageFake(t, oc)
	writes.failCreate = "v1-cache"
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)})
	if err == nil || !strings.Contains(err.Error(), "injected workload creation denial") {
		t.Fatalf("workload error lost: %v", err)
	}
	live := storageGet(t, r, &oramav1alpha1.OramaCloudV1{}, oc.Namespace, oc.Name)
	if meta.IsStatusConditionTrue(live.Status.Conditions, "StorageApplied") || meta.IsStatusConditionTrue(live.Status.Conditions, "Ready") {
		t.Fatalf("workload error reported success: %+v", live.Status)
	}
}

func TestOCV1StorageRefusesForeignOwnership(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Storage: storageSpec("20Gi", "")}
	r, writes := storageFake(t, oc)
	if err := r.reconcileCache(ctx, oc); err != nil {
		t.Fatal(err)
	}
	claim := storageClaim(t, r, oc, "data-v1-cache-0", "10Gi", "managed-csi")
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "unrelated-db", UID: "unrelated-uid"}}
	if err := r.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	r.reconcileStorage(ctx, oc)
	if len(writes.patches) != 0 {
		t.Fatal("CNPG-owned claim was patched")
	}
	assertClaimSize(t, r, oc, claim.Name, "10Gi")
	// A name match alone cannot authorize storage work on a foreign StatefulSet.
	sts := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-cache")
	sts.OwnerReferences[0].UID = "foreign-cloud"
	if err := r.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "StorageUpdateFailed")
	if len(writes.patches) != 0 {
		t.Fatal("foreign StatefulSet's claim was patched")
	}
}

// A reader's claims start at readerDataSize (10Gi, as every volume) and grow like any other.
func TestOCV1StorageGrowsReaderClaimsFromTheReaderDefault(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(nil)
	oc.Spec.Region.Role = "read"
	oc.Spec.Components.Engine.Enabled = true
	oc.Spec.Components.Engine.Reader.Replicas = 2
	r, writes := storageFake(t, oc)
	if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data-v1-reader-0", "data-v1-reader-1"} {
		storageClaim(t, r, oc, name, readerDataSize, "managed-csi")
	}
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "Applied")
	if len(writes.patches) != 0 {
		t.Fatalf("unset reader storage patched claims: %v", writes.patches)
	}
	template := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-reader").Spec.VolumeClaimTemplates
	oc.Spec.Components.Engine.Reader.Storage = storageSpec("600Gi", "")
	if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
		t.Fatal(err)
	}
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "Applied")
	for _, name := range []string{"data-v1-reader-0", "data-v1-reader-1"} {
		assertClaimSize(t, r, oc, name, "600Gi")
	}
	if got := storageGet(t, r, &appsv1.StatefulSet{}, oc.Namespace, "v1-reader").Spec.VolumeClaimTemplates; !reflect.DeepEqual(got, template) {
		t.Fatalf("reader claim template changed: %+v", got)
	}
	// Unsetting the field after growth asks for readerDataSize again, a refused shrink.
	oc.Spec.Components.Engine.Reader.Storage = nil
	r.reconcileStorage(ctx, oc)
	storageCondition(t, r, oc, "ShrinkNotSupported")
	for _, name := range []string{"data-v1-reader-0", "data-v1-reader-1"} {
		assertClaimSize(t, r, oc, name, "600Gi")
	}
}

// ClickHouse's own memory cap is three quarters of its pod's memory limit: the default 4Gi keeps the
// 3 GiB cap it always had, and a stack that sets a larger limit gets a larger cap (#1139).
func TestAnalyticsClickHouseCapFollowsItsMemoryLimit(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		resources *corev1.ResourceRequirements
		limit     string
		want      string
	}{
		{"default", nil, "4Gi", "<!-- ABSOLUTE cap (bytes) = 3 GiB, derived from the 4Gi pod limit. NOT the host-relative ratio. -->\n    <max_server_memory_usage>3221225472</max_server_memory_usage>"},
		{"8Gi", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
		}, "8Gi", "= 6 GiB, derived from the 8Gi pod limit. NOT the host-relative ratio. -->\n    <max_server_memory_usage>6442450944</max_server_memory_usage>"},
		{"no memory limit", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		}, "", "<max_server_memory_usage>3221225472</max_server_memory_usage>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Analytics.Enabled = true
				o.Spec.Components.Analytics.Resources = tc.resources
			})
			r, _ := storageFake(t, oc)
			if err := r.reconcileAnalytics(ctx, oc); err != nil {
				t.Fatal(err)
			}
			var cm corev1.ConfigMap
			if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-analytics-config"}, &cm); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(cm.Data["orama-memory.xml"], tc.want) {
				t.Errorf("orama-memory.xml lacks %q:\n%s", tc.want, cm.Data["orama-memory.xml"])
			}
			var sts appsv1.StatefulSet
			if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-analytics"}, &sts); err != nil {
				t.Fatal(err)
			}
			got := sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
			if tc.limit != "" && got.String() != tc.limit {
				t.Errorf("pod memory limit %s, want %s", got.String(), tc.limit)
			}
		})
	}
}
