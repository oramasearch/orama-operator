// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// This proof uses a real API server. Claims and capacities are explicit fixtures: envtest has
// no StatefulSet controller, CSI resizer, kubelet, or CNPG operator and cannot prove disk growth.
func TestOCV1StorageAgainstAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for storage admission and immutability envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases/orama.cloud_oramacloudv1s.yaml"}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := ocv1TestScheme(t)
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "storage-envtest"}}
	if err := kube.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	t.Run("approveDataDeletion and HARVEST Mongo storage admission", func(t *testing.T) {
		for i, tc := range []struct {
			path  []string
			value map[string]interface{}
			valid bool
		}{
			{[]string{"engine", "writer", "storage"}, map[string]interface{}{"size": "512Gi", "storageClassName": "managed-csi-premium", "approveDataDeletion": true}, true},
			{[]string{"controlDB", "storage"}, map[string]interface{}{"storageClassName": "managed-csi-premium", "approveDataDeletion": false}, true},
			{[]string{"engine", "writer", "storage"}, map[string]interface{}{"approveDataDeletion": "yes"}, false},
			{[]string{"ingestion", "harvestMongoStorage"}, map[string]interface{}{"size": "20Gi", "storageClassName": "managed-csi-premium", "approveDataDeletion": true}, true},
			{[]string{"ingestion", "harvestMongoStorage"}, map[string]interface{}{"size": "0"}, false},
		} {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "orama.cloud/v1alpha1", "kind": "OramaCloudV1", "metadata": map[string]interface{}{"name": fmt.Sprintf("approve-%d", i), "namespace": ns.Name}, "spec": map[string]interface{}{"region": map[string]interface{}{"name": "test", "role": "primary"}}}}
			if err := unstructured.SetNestedMap(obj.Object, tc.value, append([]string{"spec", "components"}, tc.path...)...); err != nil {
				t.Fatal(err)
			}
			err := kube.Create(ctx, obj)
			if tc.valid && err != nil {
				t.Fatalf("%v %v rejected: %v", tc.path, tc.value, err)
			}
			if !tc.valid && !apierrors.IsInvalid(err) {
				t.Fatalf("%v %v: err = %v, want Invalid", tc.path, tc.value, err)
			}
			if tc.valid {
				// The field survives the round trip: the API server does not prune it.
				live := &unstructured.Unstructured{}
				live.SetGroupVersionKind(obj.GroupVersionKind())
				if err := kube.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
					t.Fatal(err)
				}
				got, _, _ := unstructured.NestedFieldNoCopy(live.Object, append([]string{"spec", "components"}, tc.path...)...)
				if fmt.Sprint(got) != fmt.Sprint(tc.value) {
					t.Fatalf("%v stored as %v, want %v", tc.path, got, tc.value)
				}
			}
		}
	})

	t.Run("quantity and class admission on every component", func(t *testing.T) {
		for i, path := range [][]string{{"engine", "writer"}, {"analytics"}, {"broker"}, {"cache"}, {"search"}, {"controlDB"}} {
			for j, tc := range []struct {
				storage map[string]interface{}
				valid   bool
			}{
				{map[string]interface{}{}, true},
				{map[string]interface{}{"size": "10240Mi"}, true},
				{map[string]interface{}{"size": "0.5Gi", "storageClassName": "managed-csi"}, true},
				{map[string]interface{}{"size": int64(1024)}, true},
				{map[string]interface{}{"size": "garbage"}, false},
				{map[string]interface{}{"size": "0"}, false},
				{map[string]interface{}{"size": int64(0)}, false},
				{map[string]interface{}{"size": int64(-1)}, false},
				{map[string]interface{}{"size": "-1Gi"}, false},
				{map[string]interface{}{"storageClassName": ""}, false},
			} {
				obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "orama.cloud/v1alpha1", "kind": "OramaCloudV1", "metadata": map[string]interface{}{"name": fmt.Sprintf("validate-%d-%d", i, j), "namespace": ns.Name}, "spec": map[string]interface{}{"region": map[string]interface{}{"name": "test", "role": "primary"}}}}
				fieldPath := append([]string{"spec", "components"}, path...)
				fieldPath = append(fieldPath, "storage")
				if err := unstructured.SetNestedMap(obj.Object, tc.storage, fieldPath...); err != nil {
					t.Fatal(err)
				}
				err := kube.Create(ctx, obj)
				if tc.valid && err != nil {
					t.Fatalf("%s valid %v rejected: %v", strings.Join(path, "."), tc.storage, err)
				}
				if !tc.valid && !apierrors.IsInvalid(err) {
					t.Fatalf("%s invalid %v: %v", strings.Join(path, "."), tc.storage, err)
				}
			}
		}
	})

	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
	for _, tc := range []struct{ component, created, grown string }{
		{"writer", "10Gi", "20Gi"}, {"reader", readerDataSize, "600Gi"}, {"cache", "10Gi", "20Gi"},
	} {
		component := tc.component
		t.Run(component+" immutable templates and late claims", func(t *testing.T) {
			oc := newOCV1(nil)
			oc.Name = "env-" + component
			oc.Namespace = ns.Name
			oc.UID = ""
			oc.Spec.IDP.Provider = "none"
			oc.Spec.Components.Engine.Enabled = component != "cache"
			oc.Spec.Components.Engine.Reader.Replicas = 1
			oc.Spec.Components.Cache.Enabled = component == "cache"
			if component == "reader" {
				// A read region runs readers and no writer, so only the reader's claims are managed.
				oc.Spec.Region.Role = "read"
			}
			if err := kube.Create(ctx, oc); err != nil {
				t.Fatal(err)
			}
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
			provision()
			key := client.ObjectKey{Namespace: ns.Name, Name: oc.Name + "-" + component}
			original := &appsv1.StatefulSet{}
			if err := kube.Get(ctx, key, original); err != nil {
				t.Fatal(err)
			}
			// Confirm the real API enforces immutability, so fake-client permissiveness cannot hide it.
			forbidden := original.DeepCopy()
			forbidden.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(tc.grown)
			if err := kube.Update(ctx, forbidden); !apierrors.IsInvalid(err) {
				t.Fatalf("template mutation should be forbidden: %v", err)
			}
			class := "managed-csi"
			expand := true
			sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}, Provisioner: "disk.csi.azure.com", AllowVolumeExpansion: &expand}
			if err := kube.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
			claimName := "data-" + key.Name + "-0"
			setStorage := func(desired *oramav1alpha1.VolumeSpec) {
				switch component {
				case "writer":
					oc.Spec.Components.Engine.Writer.Storage = desired
				case "reader":
					oc.Spec.Components.Engine.Reader.Storage = desired
				default:
					oc.Spec.Components.Cache.Storage = desired
				}
			}
			for _, desired := range []*oramav1alpha1.VolumeSpec{storageSpec(tc.grown, ""), storageSpec("5Gi", ""), storageSpec(tc.grown, "other-class")} {
				setStorage(desired)
				oc.Spec.Images.Overrides = map[string]string{"oramacore": "example.com/engine:changed", "cache": "example.com/cache:changed"}
				if err := kube.Update(ctx, oc); err != nil {
					t.Fatal(err)
				}
				provision()
				got := &appsv1.StatefulSet{}
				if err := kube.Get(ctx, key, got); err != nil {
					t.Fatal(err)
				}
				if got.UID != original.UID || !reflect.DeepEqual(got.Spec.VolumeClaimTemplates, original.Spec.VolumeClaimTemplates) {
					t.Fatal("immutable template or UID changed")
				}
				if !strings.HasSuffix(got.Spec.Template.Spec.Containers[0].Image, ":changed") {
					t.Fatal("mutable fields were blocked")
				}
			}
			// An empty PVC list is pending; the operator must not manufacture the missing claim.
			r.reconcileStorage(ctx, oc)
			storageCondition(t, r, oc, "ClaimsPending")
			claims := []string{claimName}
			if component == "writer" {
				// The primary's readers are managed too: give them their claim at the reader default.
				if err := r.reconcileReaders(ctx, oc, "", "", ""); err != nil {
					t.Fatal(err)
				}
				claims = append(claims, "data-"+oc.Name+"-reader-0")
			}
			for _, name := range claims {
				size := tc.created
				if name != claimName {
					size = readerDataSize
				}
				claim := dataPVC(storageSpec(size, class), defaultVolumeSize)
				claim.Name = name
				claim.Namespace = ns.Name
				if err := kube.Create(ctx, &claim); err != nil {
					t.Fatal(err)
				}
				claim.Status.Phase = corev1.ClaimBound
				claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}
				if err := kube.Status().Update(ctx, &claim); err != nil {
					t.Fatal(err)
				}
			}
			claim := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: ns.Name}}
			setStorage(storageSpec(tc.grown, ""))
			if err := kube.Update(ctx, oc); err != nil {
				t.Fatal(err)
			}
			r.reconcileStorage(ctx, oc)
			storageCondition(t, r, oc, "Applied")
			if err := kube.Get(ctx, client.ObjectKeyFromObject(&claim), &claim); err != nil {
				t.Fatal(err)
			}
			request := claim.Spec.Resources.Requests[corev1.ResourceStorage]
			if request.Cmp(resource.MustParse(tc.grown)) != 0 {
				t.Fatalf("request = %s", request.String())
			}
			rv := claim.ResourceVersion
			r.reconcileStorage(ctx, oc)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(&claim), &claim); err != nil {
				t.Fatal(err)
			}
			if claim.ResourceVersion != rv {
				t.Fatal("same request patched twice")
			}
			// Accepted request differs from physical completion: capacity stays at the explicit fixture.
			capacity := claim.Status.Capacity[corev1.ResourceStorage]
			if capacity.Cmp(resource.MustParse(tc.created)) != 0 {
				t.Fatal("operator modified capacity")
			}
		})
	}
}
