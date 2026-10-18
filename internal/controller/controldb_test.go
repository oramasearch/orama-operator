/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The vendored 0008 file, its upstream bytes and its dashboard commit change together: a dropped
// column or constraint in a re-vendor fails here until the header and these pins are updated.
const (
	dashboardSchema0008Commit     = "8e8f9a661f8f23461c5e5a2ded94e9d84e0a67b6"
	dashboardSchema0008FileSHA256 = "7df81388abc29d8a898dc941a02dc0e0d5065c2a8a615032cc0bbda66454ced1"
	dashboardSchema0008Marker     = "-- ============ 0008_bouncy_ravenous.sql ============\n"
)

var dashboardSchema0008Tables = []string{
	"limits_errors", "llms", "pinning_rules_map", "subscription_plans",
	"stripe_addon_prices", "subscription_items", "team_migrations",
}

func newControlDBStack() *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(oc *oramav1alpha1.OramaCloudV1) {
		oc.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
	})
}

func newControlDBReconciler(t *testing.T, objs ...client.Object) (*OramaCloudV1Reconciler, client.Client) {
	t.Helper()
	scheme := ocv1TestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&oramav1alpha1.PostgresSchema{}, &batchv1.Job{}).Build()
	return &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}, kube
}

func getSchema(t *testing.T, kube client.Client, oc *oramav1alpha1.OramaCloudV1, suffix string) (*oramav1alpha1.PostgresSchema, bool) {
	t.Helper()
	ps := &oramav1alpha1.PostgresSchema{}
	err := kube.Get(context.Background(), client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + suffix}, ps)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return ps, true
}

// setSchemaStatus writes a PostgresSchema status as its own controller would.
func setSchemaStatus(t *testing.T, kube client.Client, ps *oramav1alpha1.PostgresSchema, phase, applied string, observed int64) {
	t.Helper()
	ps.Status = oramav1alpha1.PostgresSchemaStatus{Phase: phase, AppliedVersion: applied, ObservedGeneration: observed, Message: phase + " message"}
	if err := kube.Status().Update(context.Background(), ps); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardSchema0008Vendored(t *testing.T) {
	_, body, ok := strings.Cut(dashboardSchema0008SQL, dashboardSchema0008Marker)
	if !ok {
		t.Fatal("dashboard-schema-0008.sql has no 0008 marker line")
	}
	sum := sha256.Sum256([]byte(body))
	if got := hex.EncodeToString(sum[:]); got != dashboardSchema0008FileSHA256 {
		t.Fatalf("vendored 0008 body sha256 = %s, want %s (re-vendor from the dashboard and update the header and pins together)", got, dashboardSchema0008FileSHA256)
	}
	header, _, _ := strings.Cut(dashboardSchema0008SQL, dashboardSchema0008Marker)
	for _, want := range []string{"oramasearch/dashboard", "branch cloud-migration", dashboardSchema0008Commit, dashboardSchema0008FileSHA256} {
		if !strings.Contains(header, want) {
			t.Errorf("0008 header does not name %q", want)
		}
	}
	var tables []string
	for _, m := range regexp.MustCompile(`CREATE TABLE IF NOT EXISTS "([a-z_]+)"`).FindAllStringSubmatch(body, -1) {
		tables = append(tables, m[1])
	}
	if !slices.Equal(tables, dashboardSchema0008Tables) {
		t.Fatalf("0008 creates %v, want %v", tables, dashboardSchema0008Tables)
	}
	if got := schemaVersion(dashboardSchemaSQL); got != "sha256:461cc9d132ba" {
		t.Fatalf("0000..0007 schema version = %s; it must stay byte-identical so existing databases do not re-run it", got)
	}
}

func TestControlDBMigrationsApplyInOrder(t *testing.T) {
	ctx := context.Background()
	oc := newControlDBStack()
	r, kube := newControlDBReconciler(t, oc)
	reconcile := func() {
		t.Helper()
		if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()

	var cm corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb-migrations"}, &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) != 1 || cm.Data["schema.sql"] != dashboardSchemaSQL {
		t.Fatalf("ConfigMap keys = %v, want only schema.sql, byte-identical, before the base schema applies", slices.Sorted(maps.Keys(cm.Data)))
	}

	base, ok := getSchema(t, kube, oc, "-controldb-schema")
	if !ok {
		t.Fatal("base PostgresSchema not created")
	}
	if base.Spec.Version != "sha256:461cc9d132ba" || base.Spec.GuardTable != "crawler_configs" || base.Spec.SchemaConfigMapRef.Key != "schema.sql" {
		t.Fatalf("base schema spec changed from dev: %+v", base.Spec)
	}
	if _, ok := getSchema(t, kube, oc, "-controldb-schema-0008"); ok {
		t.Fatal("0008 PostgresSchema created before the base schema applied")
	}

	// Not current: pending, failed, an Applied status for an older version, and an Applied status
	// for another generation. None may release 0008.
	for _, status := range []struct {
		phase, applied string
		observed       int64
	}{
		{"Pending", "", base.Generation},
		{"Failed", "", base.Generation},
		{"Applied", "sha256:000000000000", base.Generation},
		{"Applied", base.Spec.Version, base.Generation + 1},
	} {
		setSchemaStatus(t, kube, base, status.phase, status.applied, status.observed)
		reconcile()
		if _, ok := getSchema(t, kube, oc, "-controldb-schema-0008"); ok {
			t.Fatalf("0008 PostgresSchema created while base is %+v", status)
		}
		base, _ = getSchema(t, kube, oc, "-controldb-schema")
	}

	setSchemaStatus(t, kube, base, "Applied", base.Spec.Version, base.Generation)
	reconcile()
	ps, ok := getSchema(t, kube, oc, "-controldb-schema-0008")
	if !ok {
		t.Fatal("0008 PostgresSchema not created once the base schema applied")
	}
	want := oramav1alpha1.PostgresSchemaSpec{
		ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: oc.Name + "-controldb-app", Key: "uri"},
		SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: oc.Name + "-controldb-migrations", Key: "schema-0008.sql"},
		Version:             schemaVersion(mustSQL(controlDBSchema0008SQL())),
		GuardTable:          "team_migrations",
		Image:               "postgres:16-alpine",
	}
	if ps.Spec != want {
		t.Fatalf("0008 spec = %+v, want %+v", ps.Spec, want)
	}
	if !metav1.IsControlledBy(ps, oc) || ps.Labels["app.kubernetes.io/component"] != "controldb" || ps.Labels["app.kubernetes.io/instance"] != oc.Name {
		t.Fatalf("0008 schema owner/labels: %v %v", ps.OwnerReferences, ps.Labels)
	}

	if _, ok := getSchema(t, kube, oc, "-controldb-reference-data"); ok {
		t.Fatal("reference-data PostgresSchema created before 0008 applied")
	}
	setSchemaStatus(t, kube, ps, "Applied", ps.Spec.Version, ps.Generation)
	reconcile()
	ref, ok := getSchema(t, kube, oc, "-controldb-reference-data")
	if !ok {
		t.Fatal("reference-data PostgresSchema not created once 0008 applied")
	}
	if !ref.Spec.ApplyEveryVersion || ref.Spec.SchemaConfigMapRef.Key != "reference-data.sql" ||
		ref.Spec.Version != schemaVersion(mustSQL(controlDBReferenceSQL(dashboardReferenceRowsSQL, oc.Spec.Components.ControlDB.StripePrices))) ||
		!metav1.IsControlledBy(ref, oc) {
		t.Fatalf("reference-data spec = %+v", ref.Spec)
	}
	if base.Spec.ApplyEveryVersion || ps.Spec.ApplyEveryVersion {
		t.Fatal("guarded schemas must not apply every version")
	}

	if err := kube.Get(ctx, client.ObjectKeyFromObject(&cm), &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) != 3 || cm.Data["schema.sql"] != dashboardSchemaSQL ||
		cm.Data["schema-0008.sql"] != mustSQL(controlDBSchema0008SQL()) ||
		cm.Data["reference-data.sql"] != mustSQL(controlDBReferenceSQL(dashboardReferenceRowsSQL, oc.Spec.Components.ControlDB.StripePrices)) {
		t.Fatalf("ConfigMap keys = %v, want schema.sql, schema-0008.sql and reference-data.sql", slices.Sorted(maps.Keys(cm.Data)))
	}

	versions := map[string]string{}
	for _, suffix := range []string{"-controldb-schema", "-controldb-schema-0008", "-controldb-reference-data"} {
		got, _ := getSchema(t, kube, oc, suffix)
		versions[suffix] = got.ResourceVersion
	}
	reconcile()
	reconcile()
	for suffix, rv := range versions {
		if got, _ := getSchema(t, kube, oc, suffix); got.ResourceVersion != rv {
			t.Fatalf("repeated reconciles rewrote converged %s", got.Name)
		}
	}

	// A stack's Stripe prices change only the reference-data schema.
	oc.Spec.Components.ControlDB.StripePrices.Plans = []oramav1alpha1.StripePlanPrice{{ID: "0d735744-e58e-4438-b618-97523e407cdc", Monthly: "price_test"}}
	reconcile()
	for suffix, rv := range versions {
		got, _ := getSchema(t, kube, oc, suffix)
		if changed := got.ResourceVersion != rv; changed != (suffix == "-controldb-reference-data") {
			t.Fatalf("Stripe price change: %s changed = %t", got.Name, changed)
		}
	}
}

func TestControlDBSchema0008SeedsBeforeTeamForeignKey(t *testing.T) {
	sql := mustSQL(controlDBSchema0008SQL())
	head, rest, ok := strings.Cut(sql, seedBeginMarker)
	if !ok {
		t.Fatal("0008 schema has no operator seed block")
	}
	seed, tail, ok := strings.Cut(rest, seedEndMarker)
	if !ok {
		t.Fatal("0008 seed block is not closed")
	}
	if head+tail != dashboardSchema0008SQL {
		t.Fatal("the operator's 0008 schema changes a vendored statement")
	}
	if !strings.HasPrefix(tail, schema0008SeedPoint) || strings.Contains(head, "ALTER TABLE \"teams\"") ||
		!strings.Contains(head, `CREATE TABLE IF NOT EXISTS "subscription_plans"`) || !strings.Contains(tail, "teams_subscription_plan_id_subscription_plans_id_fk") {
		t.Fatal("the seed must follow 0008's tables and precede its teams columns and foreign key")
	}
	if seed != mustSQL(controlDBReferenceSQL(dashboardReferenceRowsSQL, oramav1alpha1.ControlDBStripePrices{})) {
		t.Fatal("0008's seed must be the reference rows without Stripe prices, the same on every stack")
	}
}

// mustSQL returns rendered control-DB SQL; the embedded assets always carry their anchors, and
// TestControlDBSchemasFailWithoutAnchors covers the error path.
func mustSQL(sql string, err error) string {
	if err != nil {
		panic(err)
	}
	return sql
}

// TestControlDBSchemasFailWithoutAnchors: a re-vendored file that loses its anchor fails the
// reconcile instead of dropping the reference rows or splicing them in the wrong place.
func TestControlDBSchemasFailWithoutAnchors(t *testing.T) {
	oc := &oramav1alpha1.OramaCloudV1{}
	for name, swap := range map[string]*string{
		"0008 seed point":       &dashboardSchema0008SQL,
		"reference rows marker": &dashboardReferenceDataSQL,
	} {
		t.Run(name, func(t *testing.T) {
			saved := *swap
			t.Cleanup(func() { *swap = saved })
			*swap = "-- no anchor\n"
			if _, err := controlDBSchemas(oc); err == nil {
				t.Fatal("controlDBSchemas succeeded without the anchor")
			}
		})
	}
}

func TestControlDBReferenceSQLQuotesStripePrices(t *testing.T) {
	sql := mustSQL(controlDBReferenceSQL("", oramav1alpha1.ControlDBStripePrices{
		Plans:  []oramav1alpha1.StripePlanPrice{{ID: "0d735744-e58e-4438-b618-97523e407cdc", Yearly: "price_y'early"}},
		Addons: []oramav1alpha1.StripeAddonPrice{{ID: "11111111-1111-4111-8111-111111111111", PriceID: "price_addon"}},
	}))
	for _, want := range []string{
		"INSERT INTO stack_stripe_plan_prices VALUES ('0d735744-e58e-4438-b618-97523e407cdc', NULL, 'price_y''early');",
		"INSERT INTO stack_stripe_addon_prices VALUES ('11111111-1111-4111-8111-111111111111', 'price_addon');",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(sql, "@reference-rows") {
		t.Error("rows marker left in the rendered SQL")
	}
}

func TestControlDBSchemasOnlyOnWritableControlDB(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"disabled": func(oc *oramav1alpha1.OramaCloudV1) { oc.Spec.Components.ControlDB.Enabled = false },
		"streaming replica": func(oc *oramav1alpha1.OramaCloudV1) {
			oc.Spec.Region.Role = "read"
			oc.Spec.Components.ControlDB.Replicate = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			oc := newControlDBStack()
			mutate(oc)
			r, kube := newControlDBReconciler(t, oc)
			if err := r.reconcileControlDB(ctx, oc); err != nil {
				t.Fatal(err)
			}
			var list oramav1alpha1.PostgresSchemaList
			if err := kube.List(ctx, &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Fatalf("%s control DB scheduled %d schemas", name, len(list.Items))
			}
			for _, c := range r.componentReadiness(ctx, oc) {
				if c.Name == "controldb-schema" {
					t.Fatalf("%s control DB reports a schema component", name)
				}
			}
		})
	}
}

func schemaComponent(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1) oramav1alpha1.ComponentStatus {
	t.Helper()
	for _, c := range r.componentReadiness(context.Background(), oc) {
		if c.Name == "controldb-schema" {
			return c
		}
	}
	t.Fatal("no controldb-schema component")
	return oramav1alpha1.ComponentStatus{}
}

func TestControlDBSchemaReadiness(t *testing.T) {
	ctx := context.Background()
	oc := newControlDBStack()
	r, kube := newControlDBReconciler(t, oc)
	if c := schemaComponent(t, r, oc); c.Ready || !strings.Contains(c.Detail, "0/3 applied; v1-controldb-schema not created yet") {
		t.Fatalf("missing base schema: %+v", c)
	}
	if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
		t.Fatal(err)
	}
	base, _ := getSchema(t, kube, oc, "-controldb-schema")
	setSchemaStatus(t, kube, base, "Applied", base.Spec.Version, base.Generation)
	if c := schemaComponent(t, r, oc); c.Ready || !strings.Contains(c.Detail, "1/3 applied; v1-controldb-schema-0008 not created yet") {
		t.Fatalf("missing 0008 schema: %+v", c)
	}
	if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
		t.Fatal(err)
	}
	ps, _ := getSchema(t, kube, oc, "-controldb-schema-0008")
	setSchemaStatus(t, kube, ps, "Failed", "", ps.Generation)
	if c := schemaComponent(t, r, oc); c.Ready || !strings.Contains(c.Detail, "v1-controldb-schema-0008 Failed") || !strings.Contains(c.Detail, "Failed message") {
		t.Fatalf("failed 0008 schema: %+v", c)
	}
	setSchemaStatus(t, kube, ps, "Applied", "sha256:000000000000", ps.Generation)
	if c := schemaComponent(t, r, oc); c.Ready {
		t.Fatalf("stale 0008 schema reported ready: %+v", c)
	}
	setSchemaStatus(t, kube, ps, "Applied", ps.Spec.Version, ps.Generation)
	if c := schemaComponent(t, r, oc); c.Ready || !strings.Contains(c.Detail, "2/3 applied; v1-controldb-reference-data not created yet") {
		t.Fatalf("missing reference data: %+v", c)
	}
	if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
		t.Fatal(err)
	}
	ref, _ := getSchema(t, kube, oc, "-controldb-reference-data")
	setSchemaStatus(t, kube, ref, "Applied", ref.Spec.Version, ref.Generation)
	if c := schemaComponent(t, r, oc); !c.Ready || c.Detail != "3/3 applied" {
		t.Fatalf("applied schemas: %+v", c)
	}
}

func failedMigrationJob(ps *oramav1alpha1.PostgresSchema, version string, owner metav1.Object) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: ps.Name + "-migrate", Namespace: ps.Namespace, UID: types.UID("job-uid"),
		Annotations: map[string]string{pgSchemaVersionAnnotation: version},
	}}
	if owner != nil {
		job.OwnerReferences = []metav1.OwnerReference{{APIVersion: "orama.cloud/v1alpha1", Kind: "PostgresSchema",
			Name: owner.GetName(), UID: owner.GetUID(), Controller: boolPtr(true)}}
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	return job
}

func TestControlDBRetriesExhaustedMigrationJob(t *testing.T) {
	ctx := context.Background()
	base := func(oc *oramav1alpha1.OramaCloudV1) *oramav1alpha1.PostgresSchema {
		return &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-schema", Namespace: oc.Namespace, UID: types.UID("ps-uid")}}
	}
	cases := map[string]struct {
		job     func(ps *oramav1alpha1.PostgresSchema) *batchv1.Job
		replace bool
	}{
		"failed current Job is replaced": {
			job: func(ps *oramav1alpha1.PostgresSchema) *batchv1.Job {
				return failedMigrationJob(ps, ps.Spec.Version, ps)
			},
			replace: true,
		},
		"active Job is kept": {
			job: func(ps *oramav1alpha1.PostgresSchema) *batchv1.Job {
				job := failedMigrationJob(ps, ps.Spec.Version, ps)
				job.Status.Conditions = nil
				job.Status.Active = 1
				return job
			},
		},
		"obsolete Job is left to the PostgresSchema controller": {
			job: func(ps *oramav1alpha1.PostgresSchema) *batchv1.Job {
				return failedMigrationJob(ps, "sha256:000000000000", ps)
			},
		},
		"unowned Job is kept": {
			job: func(ps *oramav1alpha1.PostgresSchema) *batchv1.Job {
				return failedMigrationJob(ps, ps.Spec.Version, nil)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			oc := newControlDBStack()
			ps := base(oc)
			ps.Spec.Version = schemaVersion(dashboardSchemaSQL)
			r, kube := newControlDBReconciler(t, oc, ps, tc.job(ps))
			if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
				t.Fatal(err)
			}
			var job batchv1.Job
			err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: ps.Name + "-migrate"}, &job)
			replaced := apierrors.IsNotFound(err) || (err == nil && !job.DeletionTimestamp.IsZero())
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			if replaced != tc.replace {
				t.Fatalf("Job replaced = %t, want %t", replaced, tc.replace)
			}
			got, _ := getSchema(t, kube, oc, "-controldb-schema")
			if retried := got.Annotations[schemaJobRetriedAnnotation]; (retried != "") != tc.replace {
				t.Fatalf("retried annotation = %q", retried)
			}
			if !tc.replace {
				return
			}
			setSchemaStatus(t, kube, got, "Failed", "", got.Generation)
			if c := schemaComponent(t, r, oc); c.Ready || !strings.Contains(c.Detail, "-migrate failed; replaced") {
				t.Fatalf("retry is not visible in readiness: %+v", c)
			}
			// Once the replacement applies, the record is cleared.
			got, _ = getSchema(t, kube, oc, "-controldb-schema")
			setSchemaStatus(t, kube, got, "Applied", got.Spec.Version, got.Generation)
			if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
				t.Fatal(err)
			}
			got, _ = getSchema(t, kube, oc, "-controldb-schema")
			if _, ok := got.Annotations[schemaJobRetriedAnnotation]; ok {
				t.Fatal("retried annotation kept after the schema applied")
			}
		})
	}
}

func TestDisabledControlDBRemovesItsSchemas(t *testing.T) {
	ctx := context.Background()
	oc := newControlDBStack()
	other := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { o.UID = types.UID("uid-other") })
	r, kube := newControlDBReconciler(t, oc)
	suffixes := []string{"-controldb-schema", "-controldb-schema-0008", "-controldb-reference-data"}
	for _, suffix := range suffixes {
		if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
			t.Fatal(err)
		}
		ps, ok := getSchema(t, kube, oc, suffix)
		if !ok {
			t.Fatalf("%s not created", suffix)
		}
		setSchemaStatus(t, kube, ps, "Applied", ps.Spec.Version, ps.Generation)
	}
	// Same labels, controlled by another object: never removed by this stack., controlled by another object: never removed by this stack.
	foreign := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-schema", Namespace: oc.Namespace, Labels: labelsFor(oc, "controldb"),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "orama.cloud/v1alpha1", Kind: "OramaCloudV1",
			Name: other.Name, UID: other.UID, Controller: boolPtr(true)}},
	}}
	if err := kube.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	oc.Spec.Components.ControlDB.Enabled = false
	if err := r.reconcileControlDB(ctx, oc); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range suffixes {
		if _, ok := getSchema(t, kube, oc, suffix); ok {
			t.Fatalf("disabled control DB kept %s", oc.Name+suffix)
		}
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(foreign), &oramav1alpha1.PostgresSchema{}); err != nil {
		t.Fatalf("teardown removed a PostgresSchema this stack does not control: %v", err)
	}
	var cm corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb-migrations"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("disabled control DB kept its migrations ConfigMap: %v", err)
	}
}

func TestApplyEveryVersionHasNoGuard(t *testing.T) {
	ps := &oramav1alpha1.PostgresSchema{Spec: oramav1alpha1.PostgresSchemaSpec{
		SchemaConfigMapRef: oramav1alpha1.PostgresConfigMapKeyRef{Name: "cm", Key: "reference-data.sql"},
		GuardTable:         "subscription_plans", Version: "sha256:000000000000",
	}}
	guarded := pgMigrationJobSpec(ps).Template.Spec.Containers[0].Command[2]
	ps.Spec.ApplyEveryVersion = true
	every := pgMigrationJobSpec(ps).Template.Spec.Containers[0].Command[2]
	if !strings.Contains(guarded, "to_regclass('public.subscription_plans')") {
		t.Fatal("the default Job lost its guard")
	}
	if strings.Contains(every, "to_regclass") || !strings.Contains(every, "--single-transaction -f /migrations/reference-data.sql") {
		t.Fatalf("applyEveryVersion script:\n%s", every)
	}
}
