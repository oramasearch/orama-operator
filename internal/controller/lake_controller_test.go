/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello"}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 4900 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
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
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token"},
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
	if len(env) != 3 || env[0].Name != "LAKE_ADMIN_TOKEN" || env[0].ValueFrom.SecretKeyRef.Name != "lake-hello-admin" ||
		env[1].Name != "LAKE_CONTROL_DB_URI" || env[1].ValueFrom.SecretKeyRef.Name != "lake-hello-controldb-app" ||
		env[1].ValueFrom.SecretKeyRef.Key != "uri" || env[1].ValueFrom.SecretKeyRef.Optional == nil || !*env[1].ValueFrom.SecretKeyRef.Optional {
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
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-admin"}, &admin); err != nil || string(admin.Data["token"]) != first {
		t.Error("admin token must be minted once and preserved")
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
		ps.Spec.SchemaConfigMapRef.Name != "lake-hello-controldb-migrations" || ps.Spec.GuardTable != "repositories" ||
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
	if e := d.Spec.Template.Spec.Containers[0].Env; len(e) != 1 || e[0].Name != "LAKE_ADMIN_TOKEN" {
		t.Errorf("env should be only the admin token when postgres is disabled: %+v", e)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-schema"}, &ps); err == nil {
		t.Error("PostgresSchema not torn down when postgres.enabled went false")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-hello-controldb-migrations"}, &cm); err == nil {
		t.Error("migrations ConfigMap not torn down when postgres.enabled went false")
	}
}

func TestLakeSchemaMatchesVendoredSource(t *testing.T) {
	// The schema's source of truth is oramasearch/lake db/schema.sql; the embedded copy must say so.
	if !strings.Contains(lakeSchemaSQL, "SOURCE OF TRUTH: oramasearch/lake, db/schema.sql") {
		t.Error("assets/lake-schema.sql must be the vendored copy of lake's db/schema.sql")
	}
	for _, tbl := range []string{"CREATE TABLE IF NOT EXISTS users", "CREATE TABLE IF NOT EXISTS organizations",
		"CREATE TABLE IF NOT EXISTS customers", "CREATE TABLE IF NOT EXISTS repositories", "customer_type_check", "customer_name_unique", "customer_name_format"} {
		if !strings.Contains(lakeSchemaSQL, tbl) {
			t.Errorf("schema lacks %q", tbl)
		}
	}
}
