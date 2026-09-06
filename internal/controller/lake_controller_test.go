/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

func newLakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

func reconcileLake(t *testing.T, c client.Client, s *runtime.Scheme, name string) {
	t.Helper()
	r := &LakeReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestLakeReconcileCreatesWorkloads(t *testing.T) {
	s := newLakeScheme(t)
	one := int32(1)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{Namespace: "lake-dev", Image: "oramasearch/lake:v1-sandbox", Replicas: &one,
			PullSecretName: "oramasearch-dockerhub", Args: []string{"--node", "hello"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()

	reconcileLake(t, c, s, "hello")

	if err := c.Get(context.Background(), types.NamespacedName{Name: "lake-dev"}, &corev1.Namespace{}); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &d); err != nil {
		t.Fatalf("deployment not created: %v", err)
	}
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "oramasearch/lake:v1-sandbox" {
		t.Errorf("image = %q", got)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v", d.Spec.Replicas)
	}
	if ps := d.Spec.Template.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "oramasearch-dockerhub" {
		t.Errorf("imagePullSecrets = %+v", ps)
	}
	if args := d.Spec.Template.Spec.Containers[0].Args; len(args) != 2 || args[0] != "--node" {
		t.Errorf("args = %v", args)
	}
	if cp := d.Spec.Template.Spec.Containers[0].Ports; len(cp) != 1 || cp[0].ContainerPort != 4900 {
		t.Errorf("container ports = %+v", cp)
	}
	if len(d.OwnerReferences) != 1 || d.OwnerReferences[0].Kind != "Lake" {
		t.Errorf("deployment owner refs = %+v", d.OwnerReferences)
	}
	sealingKey := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-sealing-key"}, sealingKey); err != nil {
		t.Fatalf("sealing key Secret not created: %v", err)
	}
	activeKID := string(sealingKey.Data[sealingActiveKIDKey])
	if len(sealingKey.Data[sealingPrivatePrefix+activeKID]) != 32 ||
		len(sealingKey.Data[sealingPublicPrefix+activeKID]) != 32 || len(sealingKey.OwnerReferences) != 1 {
		t.Errorf("sealing key Secret = data keys %v owners %v", sealingKey.Data, sealingKey.OwnerReferences)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	if lake.Status.Sealing == nil || lake.Status.Sealing.KID != activeKID || lake.Status.Sealing.PublicKey == "" {
		t.Errorf("sealing status = %+v", lake.Status.Sealing)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 4900 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-store"}, pvc); err != nil {
		t.Fatalf("default store PVC not created: %v", err)
	}
	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if requested.Cmp(resource.MustParse("100Gi")) != 0 || len(pvc.Spec.AccessModes) != 1 ||
		pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("default store PVC = %+v", pvc.Spec)
	}
}

func TestLakeReconcileCreatesStorePVC(t *testing.T) {
	s := newLakeScheme(t)
	size := resource.MustParse("250Gi")
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "store"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:20260905a",
			Store: oramav1alpha1.LakeStoreSpec{
				Size:             size,
				StorageClassName: "azurefile-csi-premium",
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
					corev1.ReadOnlyMany,
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	reconcileLake(t, c, s, "store")

	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: "lake-dev", Name: "lake-store-store"}
	if err := c.Get(context.Background(), key, pvc); err != nil {
		t.Fatalf("store PVC not created: %v", err)
	}
	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if requested.Cmp(size) != 0 {
		t.Errorf("storage request = %s, want %s", requested.String(), size.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "azurefile-csi-premium" {
		t.Errorf("storageClassName = %v", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 2 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany ||
		pvc.Spec.AccessModes[1] != corev1.ReadOnlyMany {
		t.Errorf("accessModes = %v", pvc.Spec.AccessModes)
	}
	if refs := pvc.OwnerReferences; len(refs) != 1 || refs[0].Name != "store" || refs[0].Kind != "Lake" {
		t.Errorf("owner refs = %+v", refs)
	}

	current := &oramav1alpha1.Lake{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "store"}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Store.Size = resource.MustParse("500Gi")
	current.Spec.Store.StorageClassName = "different-class"
	current.Spec.Store.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "store")
	if err := c.Get(context.Background(), key, pvc); err != nil {
		t.Fatal(err)
	}
	requested = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if requested.Cmp(size) != 0 || *pvc.Spec.StorageClassName != "azurefile-csi-premium" ||
		len(pvc.Spec.AccessModes) != 2 {
		t.Errorf("created PVC changed after spec update: %+v", pvc.Spec)
	}
	external := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "external-store", Namespace: "lake-dev"},
	}
	if err := c.Create(context.Background(), external); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "store"}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Store.ExistingClaim = "external-store"
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "store")
	if err := c.Get(context.Background(), key, pvc); err != nil {
		t.Errorf("managed store PVC was deleted after existingClaim changed: %v", err)
	}
}

func TestLakeReconcileUsesExistingStorePVC(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "existing"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:20260905a",
			Store: oramav1alpha1.LakeStoreSpec{ExistingClaim: "lake-data"},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-data", Namespace: "lake-dev"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, pvc).WithStatusSubresource(lake).Build()
	reconcileLake(t, c, s, "existing")

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-existing-store"}, &corev1.PersistentVolumeClaim{}); err == nil {
		t.Fatal("managed store PVC created when existingClaim was set")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-data"}, pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Errorf("existing claim was adopted: %+v", pvc.OwnerReferences)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-existing"}, deployment); err != nil {
		t.Fatal(err)
	}
	volumes := deployment.Spec.Template.Spec.Volumes
	mounts := deployment.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(volumes) != 1 || volumes[0].PersistentVolumeClaim == nil ||
		volumes[0].PersistentVolumeClaim.ClaimName != "lake-data" {
		t.Errorf("store volume = %+v", volumes)
	}
	if len(mounts) != 1 || mounts[0].Name != lakeStoreVolume || mounts[0].MountPath != lakeStorePath {
		t.Errorf("store mount = %+v", mounts)
	}
}

func TestLakeReconcileAppendsCustomEnvWithOperatorPrecedence(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "env"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:20260905a",
			Env: []corev1.EnvVar{
				{Name: "LAKE_LOG_MODE", Value: "air-gapped"},
				{Name: "LAKE_STORE_ROOT", Value: "/tmp/wrong"},
				{Name: "LAKE_SEALING_KID", Value: "wrong"},
				{Name: "LAKE_PUBLIC_HOST", Value: "lake.example.com"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	reconcileLake(t, c, s, "env")

	deployment := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-env"}, deployment); err != nil {
		t.Fatal(err)
	}
	env := deployment.Spec.Template.Spec.Containers[0].Env
	if len(env) != 9 {
		t.Fatalf("env = %+v", env)
	}
	if env[0].Name != "LAKE_ADMIN_TOKEN" || env[1].Name != "LAKE_STORE_ROOT" ||
		env[1].Value != lakeStorePath || env[2].Name != "LAKE_SEALING_PUBLIC_KEY" ||
		env[2].Value == "" || env[3].Name != "LAKE_SEALING_KID" || env[3].Value == "" ||
		env[7].Name != "LAKE_LOG_MODE" || env[7].Value != "air-gapped" ||
		env[8].Name != "LAKE_PUBLIC_HOST" {
		t.Errorf("env order or values = %+v", env)
	}
	for i, variable := range env {
		if variable.Name == "LAKE_STORE_ROOT" && i != 1 {
			t.Errorf("custom LAKE_STORE_ROOT overrode operator value: %+v", env)
		}
	}
}

func TestLakeStoreStatusTracksClaimBinding(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "status"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:20260905a",
			Store: oramav1alpha1.LakeStoreSpec{ExistingClaim: "lake-data"},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-data", Namespace: "lake-dev"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, pvc).
		WithStatusSubresource(lake, &corev1.PersistentVolumeClaim{}).
		Build()
	reconcileLake(t, c, s, "status")
	if err := c.Get(context.Background(), types.NamespacedName{Name: "status"}, lake); err != nil {
		t.Fatal(err)
	}
	if lake.Status.Store == nil || lake.Status.Store.ClaimName != "lake-data" ||
		lake.Status.Store.Phase != corev1.ClaimPending ||
		lakeConditionStatus(lake.Status.Conditions, "StoreReady") != metav1.ConditionFalse ||
		lakeConditionReason(lake.Status.Conditions, "StoreReady") != "StoreUnbound" {
		t.Errorf("pending store status = %+v", lake.Status)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-data"}, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("200Gi")}
	if err := c.Status().Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "status")
	if err := c.Get(context.Background(), types.NamespacedName{Name: "status"}, lake); err != nil {
		t.Fatal(err)
	}
	if lake.Status.Store == nil || lake.Status.Store.Phase != corev1.ClaimBound ||
		lake.Status.Store.Capacity == nil || lake.Status.Store.Capacity.Cmp(resource.MustParse("200Gi")) != 0 ||
		lakeConditionStatus(lake.Status.Conditions, "StoreReady") != metav1.ConditionTrue {
		t.Errorf("bound store status = %+v", lake.Status)
	}
}

func TestLakeReconcileIsIdempotent(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec:       oramav1alpha1.LakeSpec{Image: "hashicorp/http-echo:1.0"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	reconcileLake(t, c, s, "hello")
	reconcileLake(t, c, s, "hello") // second pass must not error or duplicate

	var ds appsv1.DeploymentList
	if err := c.List(context.Background(), &ds, client.InNamespace("lake-dev")); err != nil || len(ds.Items) != 1 {
		t.Fatalf("deployments after two reconciles = %d (err %v)", len(ds.Items), err)
	}
}

func TestLakeReconcileRequiresImage(t *testing.T) {
	s := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "noimage"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "noimage"}}); err == nil {
		t.Fatal("expected an error when spec.image is empty")
	}
}

func TestLakeReconcileIngressAndDNS(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "hashicorp/http-echo:1.0", IngressEnabled: true, Host: "hello.lake.live",
			DNSSecretRef:    &corev1.LocalObjectReference{Name: "cloudflare-api-token"},
			PreviewWildcard: "preview.lake.live",
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake, &networkingv1.Ingress{}).Build()

	// 1. Ingress appears; the DNS record waits for an address.
	reconcileLake(t, c, s, "hello")
	var ing networkingv1.Ingress
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &ing); err != nil {
		t.Fatalf("ingress not created: %v", err)
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Errorf("ingress class = %v", ing.Spec.IngressClassName)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "hello.lake.live" {
		t.Errorf("ingress rules = %+v", ing.Spec.Rules)
	}
	if b := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service; b == nil || b.Name != "lake-hello" || b.Port.Number != lakeServicePort {
		t.Errorf("ingress backend = %+v", b)
	}
	if len(ing.OwnerReferences) != 1 || ing.OwnerReferences[0].Name != "hello" {
		t.Errorf("ingress owner refs = %+v", ing.OwnerReferences)
	}
	var rec oramav1alpha1.CloudflareDNSRecord
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err == nil {
		t.Fatal("DNS record created before the ingress had an address")
	}

	// 2. The ingress controller assigns an address: the DNS record follows.
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "172.212.70.79"}}
	if err := c.Status().Update(ctx, &ing); err != nil {
		t.Fatalf("set ingress address: %v", err)
	}
	reconcileLake(t, c, s, "hello")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err != nil {
		t.Fatalf("DNS record not created: %v", err)
	}
	if rec.Spec.Name != "hello.lake.live" || rec.Spec.Content != "172.212.70.79" || !rec.Spec.Proxied || rec.Spec.SecretRef != "cloudflare-api-token" {
		t.Errorf("DNS record spec = %+v", rec.Spec)
	}
	if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].Name != "hello" {
		t.Errorf("DNS record owner refs = %+v", rec.OwnerReferences)
	}
	var previewRec oramav1alpha1.CloudflareDNSRecord
	previewName := lakePreviewDNSRecordName("lake-hello", "preview.lake.live")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: previewName}, &previewRec); err != nil {
		t.Fatalf("preview wildcard DNS record not created: %v", err)
	}
	if previewRec.Spec.Name != "*.preview.lake.live" || previewRec.Spec.Content != "172.212.70.79" {
		t.Errorf("preview wildcard DNS spec = %+v", previewRec.Spec)
	}

	// 3. Gate off: both are torn down, the workloads stay.
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.IngressEnabled = false
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "hello")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &ing); err == nil {
		t.Error("ingress not torn down when ingressEnabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-dns"}, &rec); err == nil {
		t.Error("DNS record not torn down when ingressEnabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: previewName}, &previewRec); err == nil {
		t.Error("preview wildcard DNS record not torn down when ingressEnabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &appsv1.Deployment{}); err != nil {
		t.Errorf("deployment should survive the gate: %v", err)
	}
}

func TestLakeReconcileControlDB(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	two := int32(2)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:v1-sandbox",
			Postgres: oramav1alpha1.LakePostgresSpec{Instances: &two, StorageSize: "20Gi", StorageClassName: "fast"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the control DB is not ready")
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb"}, cluster); err != nil {
		t.Fatalf("CNPG cluster not created: %v", err)
	}
	inst, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	sc, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	db, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "database")
	if inst != 2 || size != "20Gi" || sc != "fast" || db != "lake" {
		t.Errorf("cluster spec = %+v", cluster.Object["spec"])
	}
	if refs := cluster.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "hello" {
		t.Errorf("cluster owner refs = %+v", refs)
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &d); err != nil {
		t.Fatal(err)
	}
	env := d.Spec.Template.Spec.Containers[0].Env
	if len(env) != 7 || env[0].Name != "LAKE_ADMIN_TOKEN" || env[0].ValueFrom.SecretKeyRef.Name != "lake-hello-admin" ||
		env[1].Name != "LAKE_STORE_ROOT" || env[1].Value != lakeStorePath ||
		env[2].Name != "LAKE_SEALING_PUBLIC_KEY" || env[2].Value == "" ||
		env[3].Name != "LAKE_SEALING_KID" || env[3].Value == "" ||
		env[4].Name != "LAKE_INSTANCE_KEK" || env[4].ValueFrom.SecretKeyRef.Name != "lake-hello-instance-kek" ||
		env[4].ValueFrom.SecretKeyRef.Key != "key" || env[5].Name != "LAKE_CONTROL_DB_URI" ||
		env[5].ValueFrom.SecretKeyRef.Name != "lake-hello-controldb-app" || env[5].ValueFrom.SecretKeyRef.Key != "uri" ||
		env[5].ValueFrom.SecretKeyRef.Optional == nil || !*env[5].ValueFrom.SecretKeyRef.Optional {
		t.Errorf("env = %+v", env)
	}
	var admin corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-admin"}, &admin); err != nil {
		t.Fatalf("admin token Secret not created: %v", err)
	}
	if len(admin.Data["token"]) != 64 || len(admin.OwnerReferences) != 1 {
		t.Errorf("admin token = %d chars, owners %d", len(admin.Data["token"]), len(admin.OwnerReferences))
	}
	first := string(admin.Data["token"])
	var instanceKek corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-instance-kek"}, &instanceKek); err != nil {
		t.Fatalf("instance KEK Secret not created: %v", err)
	}
	if len(instanceKek.Data["key"]) != 64 || len(instanceKek.OwnerReferences) != 1 {
		t.Errorf("instance KEK = %d chars, owners %d", len(instanceKek.Data["key"]), len(instanceKek.OwnerReferences))
	}
	firstKek := string(instanceKek.Data["key"])
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-admin"}, &admin); err != nil || string(admin.Data["token"]) != first {
		t.Error("admin token must be minted once and preserved")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-instance-kek"}, &instanceKek); err != nil || string(instanceKek.Data["key"]) != firstKek {
		t.Error("instance KEK must be minted once and preserved")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	if lake.Status.PostgresReady || lake.Status.PostgresInstances != "0/0" || lake.Status.SchemaPhase != "Pending" {
		t.Errorf("status = %+v", lake.Status)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-migrations"}, &cm); err != nil {
		t.Fatalf("migrations ConfigMap not created: %v", err)
	}
	if cm.Data["schema.sql"] != lakeSchemaSQL || len(lakeSchemaSQL) == 0 {
		t.Error("migrations ConfigMap does not carry the embedded schema")
	}
	var ps oramav1alpha1.PostgresSchema
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-schema"}, &ps); err != nil {
		t.Fatalf("PostgresSchema not created: %v", err)
	}
	if ps.Spec.ConnectionSecretRef.Name != "lake-hello-controldb-app" || ps.Spec.ConnectionSecretRef.Key != "uri" ||
		ps.Spec.SchemaConfigMapRef.Name != "lake-hello-controldb-migrations" || ps.Spec.GuardTable != "identity_exit_guard" ||
		ps.Spec.Version != schemaVersion(lakeSchemaSQL) {
		t.Errorf("PostgresSchema spec = %+v", ps.Spec)
	}
	if refs := ps.OwnerReferences; len(refs) != 1 || refs[0].Name != "hello" {
		t.Errorf("PostgresSchema owner refs = %+v", refs)
	}

	// Disable: the cluster is torn down and the env goes away.
	off := false
	lake.Spec.Postgres.Enabled = &off
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb"}, cluster); err == nil {
		t.Error("CNPG cluster not torn down when postgres.enabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &d); err != nil {
		t.Fatal(err)
	}
	if e := d.Spec.Template.Spec.Containers[0].Env; len(e) != 4 || e[0].Name != "LAKE_ADMIN_TOKEN" ||
		e[1].Name != "LAKE_STORE_ROOT" || e[2].Name != "LAKE_SEALING_PUBLIC_KEY" ||
		e[3].Name != "LAKE_SEALING_KID" {
		t.Errorf("operator env when postgres is disabled = %+v", e)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-instance-kek"}, &instanceKek); err == nil {
		t.Error("instance KEK must be removed with the disabled database")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-schema"}, &ps); err == nil {
		t.Error("PostgresSchema not torn down when postgres.enabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-migrations"}, &cm); err == nil {
		t.Error("migrations ConfigMap not torn down when postgres.enabled went false")
	}
}

func TestLakeReconcileControlDBBackups(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	enabled := true
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:v1-sandbox",
			Postgres: oramav1alpha1.LakePostgresSpec{Backup: oramav1alpha1.LakePostgresBackupSpec{
				Enabled: &enabled,
				Destination: oramav1alpha1.LakePostgresBackupDestination{
					Bucket: "lake-backups",
				},
				SecretRef: corev1.LocalObjectReference{Name: "r2"},
			}},
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "lake-dev"}, Data: map[string][]byte{
		"s3ApiEndpoint":   []byte("https://account.r2.cloudflarestorage.com"),
		"accessKeyId":     []byte("access-key"),
		"secretAccessKey": []byte("secret-key"),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret).WithStatusSubresource(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: s}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != lakeStatusRequeue {
		t.Errorf("requeueAfter = %s, want %s", res.RequeueAfter, lakeStatusRequeue)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	assertInitialLakeDataServices(t, lake.Status)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: lakeControlDB("hello")}, cluster); err != nil {
		t.Fatalf("CNPG cluster not created: %v", err)
	}
	destination, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "destinationPath")
	endpoint, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "endpointURL")
	retention, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "retentionPolicy")
	compression, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "wal", "compression")
	accessKeySecret, _, _ := unstructured.NestedString(cluster.Object, "spec", "backup", "barmanObjectStore", "s3Credentials", "accessKeyId", "name")
	if destination != "s3://lake-backups/lake-hello/controldb" || endpoint != "https://account.r2.cloudflarestorage.com" ||
		retention != "30d" || compression != "gzip" || accessKeySecret != "r2" {
		t.Errorf("cluster backup spec = %+v", cluster.Object["spec"])
	}

	scheduledBackup := newCNPGScheduledBackup(lake, "lake-dev")
	if err := c.Get(ctx, client.ObjectKeyFromObject(scheduledBackup), scheduledBackup); err != nil {
		t.Fatalf("ScheduledBackup not created: %v", err)
	}
	schedule, _, _ := unstructured.NestedString(scheduledBackup.Object, "spec", "schedule")
	owner, _, _ := unstructured.NestedString(scheduledBackup.Object, "spec", "backupOwnerReference")
	clusterName, _, _ := unstructured.NestedString(scheduledBackup.Object, "spec", "cluster", "name")
	immediate, _, _ := unstructured.NestedBool(scheduledBackup.Object, "spec", "immediate")
	if schedule != "0 0 3 * * *" || owner != "self" || clusterName != lakeControlDB("hello") || !immediate {
		t.Errorf("ScheduledBackup spec = %+v", scheduledBackup.Object["spec"])
	}
	if refs := scheduledBackup.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "hello" {
		t.Errorf("ScheduledBackup owner refs = %+v", refs)
	}

	backupTime := time.Date(2026, 9, 3, 3, 1, 0, 0, time.UTC)
	backup := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              "lake-hello-controldb-backup-1",
			"namespace":         "lake-dev",
			"creationTimestamp": backupTime.Add(-time.Minute).Format(time.RFC3339),
			"labels":            map[string]interface{}{"cnpg.io/cluster": lakeControlDB("hello")},
		},
		"status": map[string]interface{}{"phase": "completed", "stoppedAt": backupTime.Format(time.RFC3339)},
	}}
	backup.SetGroupVersionKind(cnpgBackupGVK)
	if err := c.Create(ctx, backup); err != nil {
		t.Fatalf("create completed Backup: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatalf("reconcile completed Backup: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	if !lake.Status.BackupReady || lake.Status.LastBackupTime == nil || !lake.Status.LastBackupTime.Time.Equal(backupTime) || lakeConditionStatus(lake.Status.Conditions, "BackupReady") != metav1.ConditionTrue {
		t.Errorf("backup status = %+v", lake.Status)
	}

	runningBackup := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":              "lake-hello-controldb-backup-2",
			"namespace":         "lake-dev",
			"creationTimestamp": backupTime.Add(time.Minute).Format(time.RFC3339),
			"labels":            map[string]interface{}{"cnpg.io/cluster": lakeControlDB("hello")},
		},
		"status": map[string]interface{}{"phase": "running"},
	}}
	runningBackup.SetGroupVersionKind(cnpgBackupGVK)
	if err := c.Create(ctx, runningBackup); err != nil {
		t.Fatalf("create running Backup: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatalf("reconcile running Backup: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "hello"}, lake); err != nil {
		t.Fatal(err)
	}
	if lake.Status.BackupReady || lake.Status.LastBackupTime != nil || lakeConditionReason(lake.Status.Conditions, "BackupReady") != "BackupNotCompleted" {
		t.Errorf("newest Backup was not reflected in status: %+v", lake.Status)
	}

	disabled := false
	lake.Spec.Postgres.Backup.Enabled = &disabled
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatalf("reconcile disabled Backup: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(scheduledBackup), scheduledBackup); err == nil {
		t.Error("ScheduledBackup not deleted when backups were disabled")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: lakeControlDB("hello")}, cluster); err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedMap(cluster.Object, "spec", "backup"); err != nil || found {
		t.Errorf("cluster backup block = %+v, found=%t, err=%v", cluster.Object["spec"], found, err)
	}
}

func assertInitialLakeDataServices(t *testing.T, status oramav1alpha1.LakeStatus) {
	t.Helper()
	if status.DataServicesSummary != "0 managed, 2 unmanaged" {
		t.Errorf("data services summary = %q", status.DataServicesSummary)
	}
	if len(status.DataServices) != 2 {
		t.Fatalf("data services = %+v", status.DataServices)
	}
	postgres := status.DataServices[0]
	if postgres.Name != "postgres" || postgres.Kind != "postgres" || postgres.Managed ||
		postgres.Backup != "untested" || postgres.Restore != "untested" ||
		postgres.Upgrade != "untested" || postgres.Failover != "none" || postgres.Notes == "" {
		t.Errorf("postgres data service = %+v", postgres)
	}
	zetta := status.DataServices[1]
	if zetta.Name != "zetta" || zetta.Kind != "zetta" || zetta.Managed ||
		zetta.Backup != "none" || zetta.Restore != "none" || zetta.Upgrade != "none" ||
		zetta.Failover != "none" || zetta.Notes == "" {
		t.Errorf("zetta data service = %+v", zetta)
	}
}

func TestLakeDataServiceStatusRequiresFailoverForReplicatedPostgres(t *testing.T) {
	two := int32(2)
	enabled := true
	lake := &oramav1alpha1.Lake{
		Spec: oramav1alpha1.LakeSpec{
			Postgres: oramav1alpha1.LakePostgresSpec{
				Instances: &two,
				Backup: oramav1alpha1.LakePostgresBackupSpec{
					Enabled: &enabled,
				},
			},
		},
		Status: oramav1alpha1.LakeStatus{DataServices: []oramav1alpha1.LakeDataServiceStatus{{
			Name:     "postgres",
			Backup:   "tested",
			Restore:  "tested",
			Upgrade:  "tested",
			Failover: "tested",
			Notes:    postgresSingleInstanceNote,
		}}},
	}
	services, summary := lakeDataServiceStatus(lake)
	if !services[0].Managed || services[0].Failover != "tested" || services[0].Notes != "" ||
		summary != "1 managed, 1 unmanaged" {
		t.Fatalf("data services = %+v, summary = %q", services, summary)
	}
	lake.Status.DataServices[0].Failover = "untested"
	services, summary = lakeDataServiceStatus(lake)
	if services[0].Managed || services[0].Failover != "untested" || summary != "0 managed, 2 unmanaged" {
		t.Fatalf("data services = %+v, summary = %q", services, summary)
	}
}

func TestLakeReconcileRejectsInvalidBackupConfigInStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		backup oramav1alpha1.LakePostgresBackupSpec
		reason string
	}{
		{name: "missing bucket", backup: oramav1alpha1.LakePostgresBackupSpec{}, reason: "BackupBucketRequired"},
		{name: "missing Secret reference", backup: oramav1alpha1.LakePostgresBackupSpec{
			Destination: oramav1alpha1.LakePostgresBackupDestination{Bucket: "lake-backups"},
		}, reason: "BackupSecretRequired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLakeScheme(t)
			enabled := true
			tc.backup.Enabled = &enabled
			lake := &oramav1alpha1.Lake{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-backup"},
				Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:v1-sandbox",
					Postgres: oramav1alpha1.LakePostgresSpec{Backup: tc.backup}},
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake).Build()
			reconcileLake(t, c, s, "invalid-backup")
			if err := c.Get(context.Background(), types.NamespacedName{Name: "invalid-backup"}, lake); err != nil {
				t.Fatal(err)
			}
			if lake.Status.Phase != "Degraded" || lake.Status.BackupReady || lakeConditionStatus(lake.Status.Conditions, "BackupReady") != metav1.ConditionFalse ||
				lakeConditionReason(lake.Status.Conditions, "BackupReady") != tc.reason {
				t.Errorf("invalid backup status = %+v", lake.Status)
			}
		})
	}
}

func lakeConditionStatus(conditions []metav1.Condition, typ string) metav1.ConditionStatus {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}

func lakeConditionReason(conditions []metav1.Condition, typ string) string {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition.Reason
		}
	}
	return ""
}

func TestLakeSchemaMatchesVendoredSource(t *testing.T) {
	// The schema's source of truth is oramasearch/lake db/schema.sql; the embedded copy must say so.
	if !strings.Contains(lakeSchemaSQL, "SOURCE OF TRUTH: oramasearch/lake, db/schema.sql") {
		t.Error("assets/lake-schema.sql must be the vendored copy of lake's db/schema.sql")
	}
	for _, tbl := range []string{"CREATE TABLE IF NOT EXISTS users", "CREATE TABLE IF NOT EXISTS organizations",
		"CREATE TABLE IF NOT EXISTS customers", "CREATE TABLE IF NOT EXISTS repositories", "CREATE TABLE IF NOT EXISTS objects",
		"CREATE TABLE IF NOT EXISTS teams", "CREATE TABLE IF NOT EXISTS memberships", "CREATE TABLE IF NOT EXISTS grants",
		"CREATE TABLE IF NOT EXISTS audit_events", "CREATE TABLE IF NOT EXISTS invitations", "customer_type_check",
		"customer_name_unique", "customer_name_format", "0004_organizations_policy", "CREATE TABLE IF NOT EXISTS secrets_mounts",
		"CREATE TABLE IF NOT EXISTS secrets_versions", "CREATE TABLE IF NOT EXISTS secrets_encryption_keys",
		"CREATE TABLE IF NOT EXISTS secrets_data_keys", "CREATE TABLE IF NOT EXISTS secrets_guard",
		"0009_secrets", "CREATE TABLE IF NOT EXISTS sso_providers", "CREATE TABLE IF NOT EXISTS sso_federation_guard",
		"CREATE TABLE IF NOT EXISTS offline_recovery_grants", "CREATE TABLE IF NOT EXISTS offline_recovery_attempts",
		"CREATE TABLE IF NOT EXISTS offline_recovery_contacts", "CREATE TABLE IF NOT EXISTS offline_recovery_guard",
		"0011_offline_recovery", "CREATE TABLE IF NOT EXISTS forge_guard", "CREATE TABLE IF NOT EXISTS pki_guard",
		"CREATE TABLE IF NOT EXISTS repository_refs", "0006_repository_lifecycle",
		"CREATE TABLE IF NOT EXISTS metering_guard", "CREATE TABLE IF NOT EXISTS drive_guard",
		"CREATE TABLE IF NOT EXISTS auth_app_guard", "CREATE TABLE IF NOT EXISTS agent_sessions",
		"CREATE TABLE IF NOT EXISTS agent_guard", "0017_agent_sessions"} {
		if !strings.Contains(lakeSchemaSQL, tbl) {
			t.Errorf("schema lacks %q", tbl)
		}
	}
	guardPattern := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS ([a-z_]+_guard)`)
	for _, match := range guardPattern.FindAllStringSubmatch(lakeSchemaSQL, -1) {
		if !strings.Contains(postgresVerificationQuery, match[1]) {
			t.Errorf("Postgres rehearsal query lacks guard table %q", match[1])
		}
	}
}

func TestLakePodRunsWithStoreFSGroup(t *testing.T) {
	if lakeRunAsGroup != 10001 {
		t.Fatalf("lakeRunAsGroup = %d, want 10001 (the lake image's gid)", lakeRunAsGroup)
	}
	sc := &corev1.PodSecurityContext{FSGroup: ptrInt64(lakeRunAsGroup), FSGroupChangePolicy: ptrFSGroupChangePolicy(corev1.FSGroupChangeOnRootMismatch)}
	if sc.FSGroup == nil || *sc.FSGroup != 10001 || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("unexpected security context %+v", sc)
	}
}
