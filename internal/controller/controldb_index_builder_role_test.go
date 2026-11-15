/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func indexBuilderStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
		o.Spec.Components.Ingestion.Enabled = true
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
		if mut != nil {
			mut(o)
		}
	})
}

// appliedIndexBuilderGrants creates the stack's index-builder grants PostgresSchema as its controller
// leaves it once its Job applied the current version.
func appliedIndexBuilderGrants(t *testing.T, kube client.Client, oc *oramav1alpha1.OramaCloudV1) {
	t.Helper()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + indexBuilderGrantsSchemaSuffix, Namespace: oc.Namespace}}
	for _, s := range schemas {
		if s.suffix == indexBuilderGrantsSchemaSuffix {
			ps.Spec.Version = schemaVersion(s.sql)
		}
	}
	if err := kube.Create(context.Background(), ps); err != nil {
		t.Fatal(err)
	}
	setSchemaStatus(t, kube, ps, "Applied", ps.Spec.Version, ps.Generation)
}

// The control DB declares index-builder's managed role after the dashboard's: it signs in, is never
// a superuser and can create nothing, its password from <cr>-controldb-index-builder, which is
// generated once and never changed (#822).
func TestIndexBuilderManagedRoleAndSecret(t *testing.T) {
	ctx := context.Background()
	oc := newControlDBStack()
	r, kube := newControlDBReconciler(t, oc)
	if err := r.reconcileControlDB(ctx, oc); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb"}, cluster); err != nil {
		t.Fatal(err)
	}
	roles, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
	want := map[string]interface{}{
		"name": "index_builder", "comment": "index-builder's control DB client, with no privileges (operator#822)", "ensure": "present",
		"login": true, "superuser": false, "createdb": false, "createrole": false, "replication": false, "bypassrls": false,
		"inherit": true, "connectionLimit": int64(10), "passwordSecret": map[string]interface{}{"name": "v1-controldb-index-builder"},
	}
	if len(roles) != 3 || !reflect.DeepEqual(roles[2], want) {
		t.Fatalf("managed roles = %v, want the migration and dashboard roles then %v", roles, want)
	}
	sec := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-controldb-index-builder"}, sec); err != nil {
		t.Fatal(err)
	}
	pass := string(sec.Data["password"])
	if sec.Type != corev1.SecretTypeBasicAuth || string(sec.Data["username"]) != "index_builder" || len(pass) != 48 ||
		string(sec.Data["uri"]) != "postgresql://index_builder:"+pass+"@v1-controldb-rw.orama-system:5432/oramacore" ||
		sec.Labels["cnpg.io/reload"] != "true" || len(sec.OwnerReferences) != 1 {
		t.Fatalf("index-builder role Secret = %v %v %v", sec.Type, sec.Labels, sec.OwnerReferences)
	}
	if err := r.reconcileControlDB(ctx, oc); err != nil {
		t.Fatal(err)
	}
	again := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-controldb-index-builder"}, again); err != nil || string(again.Data["password"]) != pass {
		t.Fatalf("the password changed: %v", err)
	}
}

// index-builder's grants are the last control DB schema and grant nothing: every privilege on the
// tables and sequences of public, and CREATE on the schema, is revoked, now and for later tables.
func TestIndexBuilderGrantsSQLGrantNothing(t *testing.T) {
	oc := newControlDBStack()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	last := schemas[len(schemas)-1]
	if last.suffix != indexBuilderGrantsSchemaSuffix || !last.everyVersion || last.key != "index-builder-grants.sql" || last.guard != "crawler_configs" {
		t.Fatalf("last schema = %+v", last)
	}
	if !slices.Contains(controlDBSchemaSuffixes, indexBuilderGrantsSchemaSuffix) {
		t.Fatal("the network sandbox does not admit the index-builder grants Job")
	}
	for _, want := range []string{
		"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM index_builder;",
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM index_builder;",
		"REVOKE CREATE ON SCHEMA public FROM index_builder;",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM index_builder;",
		"controldb-schema " + schemaVersion(dashboardSchemaSQL),
	} {
		if !strings.Contains(last.sql, want) {
			t.Errorf("index-builder grants lack %q:\n%s", want, last.sql)
		}
	}
	for _, line := range strings.Split(last.sql, "\n") {
		if strings.HasPrefix(line, "GRANT") || strings.Contains(line, " GRANT ") {
			t.Errorf("index-builder grants give a privilege: %q", line)
		}
	}
}

// index-builder is written only once its role's grants applied, and then reads DATABASE_URL from its
// own role's Secret, never the owner's; one already running keeps its template until then.
func TestIndexBuilderWaitsForItsRoleAndNeverHoldsTheOwnerCredential(t *testing.T) {
	ctx := context.Background()
	oc := indexBuilderStack(nil)
	key := client.ObjectKey{Namespace: oc.Namespace, Name: "v1-index-builder"}
	running := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	running.Spec.Template.Spec.Containers = []corev1.Container{{Name: "index-builder", Image: "index-builder:old",
		Env: []corev1.EnvVar{pgURIEnv(oc, "DATABASE_URL")}}}
	r, kube := newControlDBReconciler(t, oc, running, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey}))

	if err := r.reconcileIngestion(ctx, oc); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := kube.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dep.Spec.Template, running.Spec.Template) {
		t.Fatal("index-builder was changed before its role's grants applied")
	}

	appliedIndexBuilderGrants(t, kube, oc)
	if err := r.reconcileIngestion(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	c := containerIn(t, dep.Spec.Template.Spec, "index-builder")
	if s, k := envSecret(c.Env, "DATABASE_URL"); s != "v1-controldb-index-builder" || k != "uri" {
		t.Errorf("DATABASE_URL from %s/%s, want v1-controldb-index-builder/uri", s, k)
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == controlDBAppSecret(oc) {
			t.Errorf("index-builder %s still reads the owner Secret", e.Name)
		}
	}
	if !slices.Contains(secretsReadBy(oc), "v1-controldb-index-builder") {
		t.Error("a changed index-builder role Secret does not requeue the stack")
	}
}

// A read-tier replica's index-builder keeps the owner's credentials and does not wait for grants it
// never applies.
func TestReadReplicaIndexBuilderKeepsOwnerCredentials(t *testing.T) {
	oc := indexBuilderStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role = "read"
		o.Spec.Components.ControlDB.Replicate = true
	})
	if s, _ := envSecret([]corev1.EnvVar{indexBuilderDBEnv(oc)}, "DATABASE_URL"); s != controlDBAppSecret(oc) {
		t.Errorf("replica index-builder DATABASE_URL from %s", s)
	}
	r, _ := newControlDBReconciler(t, oc)
	if ready, err := r.indexBuilderRoleReady(context.Background(), oc); err != nil || !ready {
		t.Fatalf("replica waits for grants: %v %v", ready, err)
	}
}
