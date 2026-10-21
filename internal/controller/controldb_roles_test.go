/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// appliedDashboardGrants creates the stack's dashboard grants PostgresSchema as its controller leaves
// it once its Job applied the current version.
func appliedDashboardGrants(t *testing.T, kube client.Client, oc *oramav1alpha1.OramaCloudV1) {
	t.Helper()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + dashboardGrantsSchemaSuffix, Namespace: oc.Namespace}}
	for _, s := range schemas {
		if s.suffix == dashboardGrantsSchemaSuffix {
			ps.Spec.Version = schemaVersion(s.sql)
		}
	}
	if err := kube.Create(context.Background(), ps); err != nil {
		t.Fatal(err)
	}
	ps.Status = oramav1alpha1.PostgresSchemaStatus{Phase: "Applied", AppliedVersion: ps.Spec.Version, ObservedGeneration: ps.Generation}
	if err := kube.Status().Update(context.Background(), ps); err != nil {
		if err := kube.Update(context.Background(), ps); err != nil {
			t.Fatal(err)
		}
	}
}

func dashboardStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
		o.Spec.Components.Dashboard.Enabled = true
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
		if mut != nil {
			mut(o)
		}
	})
}

// envSecret is the Secret and key a container's env entry name reads, or "" when it reads none.
func envSecret(env []corev1.EnvVar, name string) (string, string) {
	for _, e := range env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key
		}
	}
	return "", ""
}

// The control DB declares the dashboard's managed role next to the migration role: it signs in, is
// never a superuser and can create nothing, at most dashboardRoleConnectionLimit sessions, its
// password from <cr>-controldb-dashboard, which is generated once and never changed.
func TestDashboardManagedRoleAndSecret(t *testing.T) {
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
		"name": "dashboard", "comment": "the dashboard's control DB client (operator#99)", "ensure": "present",
		"login": true, "superuser": false, "createdb": false, "createrole": false, "replication": false, "bypassrls": false,
		"inherit": true, "connectionLimit": int64(40), "passwordSecret": map[string]interface{}{"name": "v1-controldb-dashboard"},
	}
	if len(roles) != 2 || !reflect.DeepEqual(roles[1], want) {
		t.Fatalf("managed roles = %v, want the migration role then %v", roles, want)
	}
	sec := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-controldb-dashboard"}, sec); err != nil {
		t.Fatal(err)
	}
	pass := string(sec.Data["password"])
	if sec.Type != corev1.SecretTypeBasicAuth || string(sec.Data["username"]) != "dashboard" || len(pass) != 48 ||
		string(sec.Data["uri"]) != "postgresql://dashboard:"+pass+"@v1-controldb-rw.orama-system:5432/oramacore" ||
		sec.Labels["cnpg.io/reload"] != "true" || !metav1.IsControlledBy(sec, oc) {
		t.Fatalf("dashboard role Secret = %v %v %v", sec.Type, sec.Labels, sec.OwnerReferences)
	}
	if err := r.reconcileControlDB(ctx, oc); err != nil {
		t.Fatal(err)
	}
	again := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sec), again); err != nil || string(again.Data["password"]) != pass {
		t.Fatalf("the password changed: %v", err)
	}
}

// The dashboard's grants are the last control DB schema: rows of public, read-only on the rows the
// operator owns, the temporary table its delete trigger needs, never DDL, TRUNCATE or another schema,
// applied again whenever a schema that creates tables changes and never for a Stripe price.
func TestDashboardGrantsSQL(t *testing.T) {
	oc := newControlDBStack()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	last := schemas[len(schemas)-1]
	if last.suffix != dashboardGrantsSchemaSuffix || !last.everyVersion || last.key != "dashboard-grants.sql" || last.guard != "crawler_configs" {
		t.Fatalf("last schema = %+v", last)
	}
	for _, want := range []string{
		"GRANT CONNECT, TEMPORARY ON DATABASE %I TO dashboard', current_database()",
		"GRANT USAGE ON SCHEMA public TO dashboard;",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO dashboard;",
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON subscription_plans, stripe_addon_prices, llms, oramacore_clusters FROM dashboard;",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO dashboard;",
		"controldb-schema " + schemaVersion(dashboardSchemaSQL),
		"controldb-schema-0008 " + schemaVersion(mustSQL(controlDBSchema0008SQL())),
	} {
		if !strings.Contains(last.sql, want) {
			t.Errorf("dashboard grants lack %q:\n%s", want, last.sql)
		}
	}
	grantLines := []string{}
	for _, l := range strings.Split(last.sql, "\n") {
		if strings.HasPrefix(l, "GRANT") || strings.HasPrefix(l, "ALTER DEFAULT") {
			grantLines = append(grantLines, l)
		}
	}
	for _, never := range []string{"TRUNCATE", "CREATE", "SUPERUSER", "ALL PRIVILEGES", "pg_write_all_data", "analytics", "drizzle", "migration"} {
		for _, l := range grantLines {
			if strings.Contains(l, never) {
				t.Errorf("a grant contains %q: %s", never, l)
			}
		}
	}
	oc.Spec.Components.ControlDB.StripePrices.Plans = []oramav1alpha1.StripePlanPrice{{ID: "0d735744-e58e-4438-b618-97523e407cdc", Monthly: "price_test"}}
	again, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	if again[len(again)-1].sql != last.sql {
		t.Fatal("a Stripe price change changed the dashboard grants")
	}
}

// The migration grants' file is byte-for-byte what it was before the dashboard role, so the roll does
// not re-run it for nothing.
func TestMigrationGrantsUnchangedByDashboardRole(t *testing.T) {
	schemas, err := controlDBSchemas(newControlDBStack())
	if err != nil {
		t.Fatal(err)
	}
	got := schemas[3].sql
	if !strings.HasPrefix(got, "-- The migration role's grants (operator#653), applied after:\n--   controldb-schema sha256:") ||
		strings.Contains(got, "dashboard") {
		t.Fatalf("migration grants =\n%s", got)
	}
}

// The dashboard is written only once its role's grants applied, and then signs in as that role; a
// dashboard that already runs keeps the owner's credentials until then.
func TestDashboardWaitsForItsRoleGrants(t *testing.T) {
	ctx := context.Background()
	oc := dashboardStack(nil)
	key := client.ObjectKey{Namespace: oc.Namespace, Name: "v1-dashboard"}
	running := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	running.Spec.Template.Spec.Containers = []corev1.Container{{Name: "dashboard", Image: "dashboard:old",
		Env: []corev1.EnvVar{pgURIEnv(oc, "DATABASE_URL")}}}
	r, kube := newControlDBReconciler(t, oc, running, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey}))

	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := kube.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dep.Spec.Template, running.Spec.Template) {
		t.Fatal("the dashboard was changed before its role's grants applied")
	}

	// A grants schema of another version (an older operator's) does not count.
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: "v1" + dashboardGrantsSchemaSuffix, Namespace: oc.Namespace},
		Spec: oramav1alpha1.PostgresSchemaSpec{Version: "sha256:000000000000"}}
	if err := kube.Create(ctx, ps); err != nil {
		t.Fatal(err)
	}
	setSchemaStatus(t, kube, ps, "Applied", ps.Spec.Version, ps.Generation)
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, key, dep); err != nil || !reflect.DeepEqual(dep.Spec.Template, running.Spec.Template) {
		t.Fatal("the dashboard was changed on another version's grants")
	}
	if err := kube.Delete(ctx, ps); err != nil {
		t.Fatal(err)
	}

	appliedDashboardGrants(t, kube, oc)
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	env := dep.Spec.Template.Spec.Containers[0].Env
	for name, wantKey := range map[string]string{"DATABASE_URL": "uri", "POSTGRES_PASSWORD": "password"} {
		if s, k := envSecret(env, name); s != "v1-controldb-dashboard" || k != wantKey {
			t.Errorf("%s from %s/%s, want v1-controldb-dashboard/%s", name, s, k, wantKey)
		}
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "v1-controldb-app" {
				t.Errorf("dashboard %s still reads the owner Secret", e.Name)
			}
		}
	}
	if !strings.Contains(strings.Join(secretsReadBy(oc), " "), "v1-controldb-dashboard") {
		t.Error("a changed dashboard role Secret does not requeue the stack")
	}
}

// A read-tier replica's dashboard keeps the owner's credentials and does not wait for grants it never
// applies.
func TestReadReplicaDashboardKeepsOwnerCredentials(t *testing.T) {
	ctx := context.Background()
	oc := dashboardStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role = "read"
		o.Spec.Components.ControlDB.Replicate = true
	})
	r, kube := newControlDBReconciler(t, oc, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey}))
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-dashboard"}, dep); err != nil {
		t.Fatal(err)
	}
	if s, _ := envSecret(dep.Spec.Template.Spec.Containers[0].Env, "DATABASE_URL"); s != "v1-controldb-app" {
		t.Errorf("replica dashboard DATABASE_URL from %s", s)
	}
}
