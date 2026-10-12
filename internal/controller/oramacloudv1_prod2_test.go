/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for the Prod 2 readiness changes (orama-bridge#141): persistent readers, configurable
// storage. Each change also asserts that a CR without the new fields renders as it did before.

// fullStackOCV1 is a CR with every component on, as an existing (pre-#141) CR would declare it.
func fullStackOCV1(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Profile = "medium"
		o.Spec.DemoSeed = true
		o.Spec.IDP.Provider = "keycloak"
		c := &o.Spec.Components
		c.Engine.Enabled, c.Engine.Envoy = true, true
		c.Dashboard = oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app.example.com"}
		c.Docs.Enabled = true
		c.Ingestion.Enabled, c.Ingestion.Harvest = true, true
		c.ControlDB.Enabled = true
		for _, ds := range []*oramav1alpha1.DataStoreSpec{&c.Analytics, &c.Cache, &c.Broker, &c.Search} {
			ds.Enabled = true
		}
		c.PgAdmin.Enabled, c.Disco.Enabled, c.Workers.Enabled = true, true, true
		c.AnalyticsProcessor.Enabled, c.SearchWorkers.Enabled = true, true
		if mut != nil {
			mut(o)
		}
	})
}

// reconcileOCV1 runs the full OramaCloudV1 reconcile against a fake client holding oc (+ objs).
func reconcileOCV1(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) (client.Client, *OramaCloudV1Reconciler) {
	t.Helper()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, oc)...).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return cl, r
}

func reconcileAgain(t *testing.T, cl client.Client, r *OramaCloudV1Reconciler, mut func(*oramav1alpha1.OramaCloudV1)) {
	t.Helper()
	ctx := context.Background()
	oc := &oramav1alpha1.OramaCloudV1{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "v1", Namespace: "orama-system"}, oc); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(oc)
		if err := cl.Update(ctx, oc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getObj[T client.Object](t *testing.T, cl client.Client, name string, obj T) T {
	t.Helper()
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, name, err)
	}
	return obj
}

func absent(t *testing.T, cl client.Client, name string, obj client.Object) bool {
	t.Helper()
	err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

func controlDBCluster(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	return getObj(t, cl, "v1-controldb", cluster)
}

func pvcTemplate(t *testing.T, sts *appsv1.StatefulSet) corev1.PersistentVolumeClaim {
	t.Helper()
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Fatalf("%s volumeClaimTemplates = %+v, want one named data", sts.Name, sts.Spec.VolumeClaimTemplates)
	}
	return sts.Spec.VolumeClaimTemplates[0]
}

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// --- 1. persistent readers ---

func TestReadersAreAStatefulSetWithPersistentData(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))

	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement || *sts.Spec.Replicas != 3 || sts.Spec.ServiceName != "v1-reader" {
		t.Errorf("reader StatefulSet spec = policy %q replicas %d service %q", sts.Spec.PodManagementPolicy, *sts.Spec.Replicas, sts.Spec.ServiceName)
	}
	pvc := pvcTemplate(t, sts)
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" || pvc.Spec.StorageClassName != nil {
		t.Errorf("reader PVC = %s class %v, want 10Gi on the default class", got.String(), pvc.Spec.StorageClassName)
	}
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			t.Errorf("reader pod still declares a data volume %+v; data must come from the PVC", v)
		}
	}
	if !absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Error("a fresh CR must not create a reader Deployment")
	}
	if !absent(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("no reader.hpa: no HPA expected")
	}
}

// A CR reconciled before #141 has an emptyDir reader Deployment. It keeps serving until every
// StatefulSet reader is Ready, and the status counts both until then.
func TestReaderDeploymentRetiredOnlyOnceStatefulSetIsReady(t *testing.T) {
	ctx := context.Background()
	oc := fullStackOCV1(nil)
	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: "orama-system", Labels: labelsFor(oc, "reader")},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 3},
	}
	cl, r := reconcileOCV1(t, oc, old)
	if absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Fatal("old reader Deployment deleted before the StatefulSet readers were ready")
	}
	if c := findComponent(t, cl, "reader"); c.Ready || c.Detail != "3/6" {
		t.Errorf("reader component during the switch = %+v, want not ready 3/6 (old 3/3 + new 0/3)", c)
	}

	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	sts.Status.ReadyReplicas = 3
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if !absent(t, cl, "v1-reader", &appsv1.Deployment{}) {
		t.Error("old reader Deployment not retired once the StatefulSet readers were ready")
	}
	if c := findComponent(t, cl, "reader"); !c.Ready || c.Detail != "3/3" {
		t.Errorf("reader component after the switch = %+v, want ready 3/3", c)
	}
}

func findComponent(t *testing.T, cl client.Client, name string) oramav1alpha1.ComponentStatus {
	t.Helper()
	oc := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})
	for _, c := range oc.Status.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %q not in status %+v", name, oc.Status.Components)
	return oramav1alpha1.ComponentStatus{}
}

func TestReaderHPATargetsStatefulSetAndOwnsReplicas(t *testing.T) {
	ctx := context.Background()
	cl, r := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.HPA = &oramav1alpha1.HPASpec{MinReplicas: 2, MaxReplicas: 6}
	}))
	hpa := getObj(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{})
	ref := hpa.Spec.ScaleTargetRef
	if ref.Kind != "StatefulSet" || ref.Name != "v1-reader" || ref.APIVersion != "apps/v1" ||
		*hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 6 ||
		*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != 70 {
		t.Errorf("HPA spec = %+v", hpa.Spec)
	}

	// The autoscaler scales the StatefulSet; a reconcile must not reset it.
	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	sts.Spec.Replicas = int32Ptr(5)
	if err := cl.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if got := *getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.Replicas; got != 5 {
		t.Errorf("reconcile reset HPA-managed replicas to %d, want 5", got)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.HPA = nil })
	if !absent(t, cl, "v1-reader", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("HPA not removed when reader.hpa was cleared")
	}
	if got := *getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.Replicas; got != 3 {
		t.Errorf("without an HPA the operator owns replicas again: got %d, want 3", got)
	}
}

// --- 2. configurable storage ---

func TestStorageSizeAndClassPerComponent(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		c := &o.Spec.Components
		c.Engine.Writer.Storage = oramav1alpha1.VolumeSpec{Size: quantity("200Gi"), StorageClassName: "premium"}
		c.Engine.Reader.Storage = oramav1alpha1.VolumeSpec{Size: quantity("150Gi"), StorageClassName: "premium"}
		c.Analytics.Storage = oramav1alpha1.VolumeSpec{Size: quantity("40Gi")}
		c.Broker.Storage = oramav1alpha1.VolumeSpec{StorageClassName: "standard"}
		c.ControlDB.Storage = oramav1alpha1.VolumeSpec{Size: quantity("30Gi"), StorageClassName: "premium"}
	}))
	for name, want := range map[string]struct{ size, class string }{
		"v1-writer":    {"200Gi", "premium"},
		"v1-reader":    {"150Gi", "premium"},
		"v1-analytics": {"40Gi", ""},
		"v1-broker":    {"10Gi", "standard"},
		"v1-cache":     {"10Gi", ""}, // unset stays at today's default
	} {
		pvc := pvcTemplate(t, getObj(t, cl, name, &appsv1.StatefulSet{}))
		size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		class := ""
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		if size.String() != want.size || class != want.class {
			t.Errorf("%s PVC = %s/%q, want %s/%q", name, size.String(), class, want.size, want.class)
		}
	}
	storage, _, _ := unstructured.NestedStringMap(controlDBCluster(t, cl).Object, "spec", "storage")
	if storage["size"] != "30Gi" || storage["storageClass"] != "premium" {
		t.Errorf("CNPG storage = %v, want 30Gi on premium", storage)
	}
}

func TestStorageDefaultsUnchangedForOldCR(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	for _, name := range []string{"v1-writer", "v1-analytics", "v1-cache", "v1-broker", "v1-search", "v1-harvest-mongo"} {
		pvc := pvcTemplate(t, getObj(t, cl, name, &appsv1.StatefulSet{}))
		if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" || pvc.Spec.StorageClassName != nil {
			t.Errorf("%s PVC = %s class %v, want the old 10Gi on the default class", name, got.String(), pvc.Spec.StorageClassName)
		}
	}
	storage, _, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "storage")
	if len(storage) != 1 || storage["size"] != "10Gi" {
		t.Errorf("CNPG storage = %v, want exactly {size: 10Gi}", storage)
	}
}

// volumeClaimTemplates are immutable: a size change on an existing StatefulSet must not fail the
// reconcile (which would stall every later component); the StatefulSet keeps its template.
func TestStorageChangeKeepsExistingVolumeClaimTemplates(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Storage.Size = quantity("500Gi")
	})
	pvc := pvcTemplate(t, getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" {
		t.Errorf("existing writer volumeClaimTemplate changed to %s; it must keep the created size", got.String())
	}
}
