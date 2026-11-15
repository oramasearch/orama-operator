/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testLakeHost = "lake-dev.oramadev.net"

// insertTestRow appends a row with exactly the given text columns, as a writer would.
func insertTestRow(t *testing.T, zetta *fakeZetta, table string, row map[string]string) {
	t.Helper()
	columns := make([]string, 0, len(row))
	for column := range row {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	values := make([]string, len(columns))
	for i, column := range columns {
		values[i] = deploymentSQLQuote(row[column])
	}
	zetta.mustExec(t, "INSERT INTO "+table+" ("+strings.Join(columns, ", ")+") VALUES ("+strings.Join(values, ", ")+")")
}

// desiredTestRow is a desired deployment row in lake's shape, with the operator's columns.
func desiredTestRow(overrides map[string]string) map[string]string {
	row := map[string]string{
		"operator":         "test-operator",
		"lake":             testLakeHost,
		"repository":       "repo-1",
		"environment":      "production",
		"kind":             oramav1alpha1.LakeEnvironmentKindNamed,
		"change":           "blake3:" + strings.Repeat("1", 64),
		"image":            testEnvironmentImage,
		"deployment_id":    "deploy-1",
		"requested_by":     "lake-admin-token/prod",
		"requested_at":     "2026-09-05T10:00:00.000000Z",
		"phase":            deploymentPhaseRequested,
		"namespace":        "",
		"url":              "",
		"message":          "",
		"updated_at":       "2026-09-05T10:00:00.000000Z",
		"ordering_token":   "00000000-0000-4000-8000-000000000001",
		"record_type":      deploymentRecordTypeDesired,
		"replicas":         "1",
		"port":             "8080",
		"env_json":         "[]",
		"secret_refs_json": "[]",
		"host":             "",
		"withdrawn":        "false",
		"change_closed":    "false",
	}
	for column, value := range overrides {
		row[column] = value
	}
	return row
}

// statusRows are the status rows the operator appended, oldest first, as text.
func statusRows(zetta *fakeZetta) []map[string]string {
	out := make([]map[string]string, 0)
	for _, row := range zetta.rows("orama_control", "deployments") {
		if row["record_type"] != deploymentRecordTypeStatus {
			continue
		}
		text := make(map[string]string, len(row))
		for column, value := range row {
			text[column] = coerceSQLString(value)
		}
		out = append(out, text)
	}
	return out
}

func syncDeploymentRows(t *testing.T, reconciler *OramaZettaClusterReconciler, lake *oramav1alpha1.Lake, zetta *fakeZetta) {
	t.Helper()
	host := lakeRowHost(lake)
	rows, err := readZettaPlatformRows(context.Background(), zetta.url(), "test-token", host)
	if err != nil {
		t.Fatalf("read platform rows: %v", err)
	}
	if reconciler.DomainClaims == nil {
		// The tests of the other rules start from verified claims of every domain lake requested.
		claims := verifiedClaimsFor(rows.Domains)
		reconciler.DomainClaims = func(context.Context, *oramav1alpha1.Lake) ([]LakeDomainClaim, error) {
			return claims, nil
		}
		defer func() { reconciler.DomainClaims = nil }()
	}
	reconciler.materializeZettaPlatformRows(context.Background(), lake, host, zetta.url(), "test-token", rows)
}

func testDeploymentLake(spec oramav1alpha1.LakeSpec) *oramav1alpha1.Lake {
	spec.Namespace = "lake"
	spec.Image = "oramasearch/lake:test"
	spec.Host = testLakeHost
	return &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"}, Spec: spec}
}

func TestZettaDeploymentMaterializationLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{IngressEnabled: true, PreviewWildcard: "preview.example.com"})
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake).
		WithStatusSubresource(&oramav1alpha1.LakeEnvironment{}, &appsv1.Deployment{}, &networkingv1.Ingress{}).
		Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	desired := desiredTestRow(map[string]string{
		"environment": "checkout-preview",
		"kind":        oramav1alpha1.LakeEnvironmentKindPreview,
		"change":      "7ac4c59b92e46235a6f2",
		"env_json":    `[{"name":"APP_ENV","value":"preview"}]`,
	})
	zetta := newFakeZetta(t, "test-token")
	insertTestRow(t, zetta, "deployments", desired)

	syncDeploymentRows(t, reconciler, lake, zetta)
	event, err := parseDeploymentRow(desired)
	if err != nil {
		t.Fatal(err)
	}
	environmentKey := types.NamespacedName{Namespace: "lake", Name: deploymentEnvironmentName(lake.Name, event)}
	var environment oramav1alpha1.LakeEnvironment
	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatalf("LakeEnvironment: %v", err)
	}
	if environment.Spec.Image != testEnvironmentImage || environment.Spec.Env[0].Value != "preview" ||
		environment.Spec.LakeRef != "prod" ||
		environment.Annotations[deploymentIDAnnotation] != "deploy-1" || len(environment.OwnerReferences) != 1 ||
		environment.OwnerReferences[0].Name != "prod" {
		t.Errorf("LakeEnvironment = %+v", environment)
	}
	resourceVersion := environment.ResourceVersion
	syncDeploymentRows(t, reconciler, lake, zetta)
	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatal(err)
	}
	if environment.ResourceVersion != resourceVersion || len(statusRows(zetta)) != 0 {
		t.Errorf("identical desired row changed state: rv %s -> %s, status writes %d", resourceVersion, environment.ResourceVersion, len(statusRows(zetta)))
	}

	environmentReconciler := &LakeEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	request := client.ObjectKey{Namespace: environment.Namespace, Name: environment.Name}
	if _, err := environmentReconciler.Reconcile(ctx, ctrlRequest(request)); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}
	syncDeploymentRows(t, reconciler, lake, zetta)
	written := statusRows(zetta)
	if len(written) != 1 || written[0]["phase"] != oramav1alpha1.LakeEnvironmentPhaseProgressing ||
		written[0]["namespace"] != lakeEnvironmentNamespace(&environment) || written[0]["lake"] != testLakeHost ||
		written[0]["repository"] != "repo-1" || written[0]["change"] != "7ac4c59b92e46235a6f2" {
		t.Fatalf("progressing callbacks = %+v", written)
	}
	syncDeploymentRows(t, reconciler, lake, zetta)
	if len(statusRows(zetta)) != 1 {
		t.Fatalf("steady Progressing callback count = %d", len(statusRows(zetta)))
	}

	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatal(err)
	}
	workloadNamespace := lakeEnvironmentNamespace(&environment)
	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := environmentReconciler.Reconcile(ctx, ctrlRequest(request)); err != nil {
		t.Fatalf("ready environment reconcile: %v", err)
	}
	syncDeploymentRows(t, reconciler, lake, zetta)
	written = statusRows(zetta)
	if len(written) != 2 || written[1]["phase"] != oramav1alpha1.LakeEnvironmentPhaseReady ||
		written[1]["url"] != "https://7ac4c59b92e4.preview.example.com" {
		t.Fatalf("ready callbacks = %+v", written)
	}
	syncDeploymentRows(t, reconciler, lake, zetta)
	if len(statusRows(zetta)) != 2 {
		t.Fatalf("steady Ready callback count = %d", len(statusRows(zetta)))
	}

	withdrawn := desiredTestRow(nil)
	for key, value := range desired {
		withdrawn[key] = value
	}
	withdrawn["withdrawn"] = "true"
	withdrawn["updated_at"] = "2026-09-05T10:01:00.000000Z"
	insertTestRow(t, zetta, "deployments", withdrawn)
	syncDeploymentRows(t, reconciler, lake, zetta)
	written = statusRows(zetta)
	if len(written) != 3 || written[2]["phase"] != oramav1alpha1.LakeEnvironmentPhaseTornDown {
		t.Fatalf("teardown callbacks = %+v", written)
	}
	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatalf("LakeEnvironment should remain until its finalizer runs: %v", err)
	}
	if environment.DeletionTimestamp.IsZero() {
		t.Fatal("withdrawn LakeEnvironment was not marked for deletion")
	}
	if _, err := environmentReconciler.Reconcile(ctx, ctrlRequest(request)); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	if _, err := environmentReconciler.Reconcile(ctx, ctrlRequest(request)); err != nil {
		t.Fatalf("second finalize: %v", err)
	}
	if err := kubeClient.Get(ctx, environmentKey, &environment); !apierrors.IsNotFound(err) {
		t.Errorf("LakeEnvironment still exists after finalization: %v", err)
	}
	if zetta.authFailures != 0 {
		t.Errorf("Zetta requests missing bearer: %d", zetta.authFailures)
	}
}

// Rows keyed by another Lake's host, or by the Lake's resource name (the operator's key before
// operator#560), are not this Lake's.
func TestZettaDeploymentReadsOnlyTheLakesPublicHost(t *testing.T) {
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	zetta := newFakeZetta(t, "test-token")
	insertTestRow(t, zetta, "deployments", desiredTestRow(map[string]string{"lake": "prod", "deployment_id": "by-name"}))
	insertTestRow(t, zetta, "deployments", desiredTestRow(map[string]string{"lake": "other.example.com", "deployment_id": "other"}))
	syncDeploymentRows(t, reconciler, lake, zetta)
	var environments oramav1alpha1.LakeEnvironmentList
	if err := kubeClient.List(context.Background(), &environments); err != nil {
		t.Fatal(err)
	}
	if len(environments.Items) != 0 {
		t.Fatalf("materialized another Lake's rows: %+v", environments.Items)
	}

	fromSecret := lake.DeepCopy()
	fromSecret.Spec.Env = []corev1.EnvVar{{Name: "LAKE_PUBLIC_HOST", ValueFrom: &corev1.EnvVarSource{}}}
	if got := lakeRowHost(fromSecret); got != "" {
		t.Errorf("a LAKE_PUBLIC_HOST from a source gave row host %q", got)
	}
	literal := lake.DeepCopy()
	literal.Spec.Env = []corev1.EnvVar{{Name: "LAKE_PUBLIC_HOST", Value: "lake.oramadev.net"}}
	if got := lakeRowHost(literal); got != "lake.oramadev.net" {
		t.Errorf("row host = %q, want spec.env's literal", got)
	}
}

func TestZettaDeploymentUpdateRepointsStableEnvironment(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	first := desiredTestRow(nil)
	zetta := newFakeZetta(t, "test-token")
	insertTestRow(t, zetta, "deployments", first)
	syncDeploymentRows(t, reconciler, lake, zetta)
	event, err := parseDeploymentRow(first)
	if err != nil {
		t.Fatal(err)
	}
	name := deploymentEnvironmentName(lake.Name, event)
	key := types.NamespacedName{Namespace: "lake", Name: name}
	var environment oramav1alpha1.LakeEnvironment
	if err := kubeClient.Get(ctx, key, &environment); err != nil {
		t.Fatal(err)
	}
	firstNamespace := lakeEnvironmentNamespace(&environment)

	second := desiredTestRow(map[string]string{
		"deployment_id": "deploy-2",
		"change":        "blake3:" + strings.Repeat("2", 64),
		"image":         "registry.example.com/app@sha256:" + strings.Repeat("b", 64),
		"updated_at":    "2026-09-06T10:01:00.000000Z",
	})
	insertTestRow(t, zetta, "deployments", second)
	syncDeploymentRows(t, reconciler, lake, zetta)
	if err := kubeClient.Get(ctx, key, &environment); err != nil {
		t.Fatal(err)
	}
	if environment.Annotations[deploymentIDAnnotation] != "deploy-2" ||
		environment.Spec.Change != second["change"] ||
		environment.Spec.Image != second["image"] {
		t.Errorf("updated environment = %+v", environment)
	}
	if lakeEnvironmentNamespace(&environment) != firstNamespace {
		t.Errorf("workload namespace changed from %q to %q", firstNamespace, lakeEnvironmentNamespace(&environment))
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := kubeClient.List(ctx, &environments, client.InNamespace("lake")); err != nil {
		t.Fatal(err)
	}
	if len(environments.Items) != 1 {
		t.Errorf("stable environment count = %d", len(environments.Items))
	}
}

func TestZettaDeploymentRefusesForeignStableEnvironment(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	desired, err := parseDeploymentRow(desiredTestRow(map[string]string{"deployment_id": "deploy-foreign"}))
	if err != nil {
		t.Fatal(err)
	}
	foreign := &oramav1alpha1.LakeEnvironment{ObjectMeta: metav1.ObjectMeta{
		Name:      deploymentEnvironmentName(lake.Name, desired),
		Namespace: "lake",
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, foreign).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	zetta := newFakeZetta(t, "test-token")
	err = reconciler.reconcileZettaDeployment(ctx, lake, zetta.url(), "test-token", "test-operator", &zettaDeploymentState{Desired: desired, Latest: desired}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not owned by Lake") {
		t.Fatalf("foreign collision error = %v", err)
	}
	if len(statusRows(zetta)) != 0 {
		t.Errorf("foreign collision wrote %d status rows", len(statusRows(zetta)))
	}
}

func ctrlRequest(key client.ObjectKey) ctrl.Request {
	return ctrl.Request{NamespacedName: key}
}

func TestDeploymentSyncIntervalUsesLakeSetting(t *testing.T) {
	scheme := newLakeEnvironmentScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake",
			Image:     "oramasearch/lake:test",
			DeploySync: oramav1alpha1.LakeDeploySyncSpec{
				Interval: metav1.Duration{Duration: 7 * time.Second},
			},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	if got := reconciler.deploymentSyncInterval(context.Background(), "lake"); got != 7*time.Second {
		t.Errorf("interval = %s", got)
	}
	if got := reconciler.deploymentSyncInterval(context.Background(), "other"); got != idleDeploymentSyncInterval {
		t.Errorf("idle interval = %s", got)
	}
}

func TestDeploymentTeardownRules(t *testing.T) {
	tests := []struct {
		name       string
		event      zettaDeploymentEvent
		want       bool
		wantReason string
	}{
		{
			name:       "withdrawn named environment",
			event:      zettaDeploymentEvent{Kind: oramav1alpha1.LakeEnvironmentKindNamed, Withdrawn: true},
			want:       true,
			wantReason: "deployment withdrawn",
		},
		{
			name:       "closed preview",
			event:      zettaDeploymentEvent{Kind: oramav1alpha1.LakeEnvironmentKindPreview, ChangeClosed: true},
			want:       true,
			wantReason: "preview change closed",
		},
		{
			name:  "closed named environment",
			event: zettaDeploymentEvent{Kind: oramav1alpha1.LakeEnvironmentKindNamed, ChangeClosed: true},
			want:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deploymentShouldTearDown(&test.event); got != test.want {
				t.Fatalf("deploymentShouldTearDown = %t, want %t", got, test.want)
			}
			if test.want && deploymentTeardownMessage(&test.event) != test.wantReason {
				t.Errorf("teardown message = %q", deploymentTeardownMessage(&test.event))
			}
		})
	}
}

func TestDeploymentSchemaIncludesEventTable(t *testing.T) {
	foundEvents := false
	foundRecords := false
	for _, statement := range operatorZettaSchema {
		if statement.db == "orama_control" && strings.HasPrefix(statement.sql, "CREATE TABLE deployments ") {
			foundEvents = true
			for _, column := range []string{"deployment_id", "record_type", "change_id", "phase", "updated_at"} {
				if !strings.Contains(statement.sql, column+" TEXT") {
					t.Errorf("deployments schema missing %s: %s", column, statement.sql)
				}
			}
		}
		if statement.db == "orama_control" && strings.HasPrefix(statement.sql, "CREATE TABLE deployment_records ") {
			foundRecords = true
			for _, column := range []string{"deployment_id", "result_id", "rollback_of", "secret_refs_json", "created_at"} {
				if !strings.Contains(statement.sql, column+" TEXT") {
					t.Errorf("deployment records schema missing %s: %s", column, statement.sql)
				}
			}
		}
	}
	if !foundEvents {
		t.Fatal("operatorZettaSchema does not create deployments")
	}
	if !foundRecords {
		t.Fatal("operatorZettaSchema does not create deployment_records")
	}
}

func TestZettaControlPlaneReplacesStaleSchemaJob(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	zetta := newZettaTestCluster()
	stale := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "z0-zetta-schema",
		Namespace: zetta.Namespace,
		Annotations: map[string]string{
			zettaSchemaVersionAnnotation: "sha256:stale",
		},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zetta, stale).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	if err := reconciler.reconcileZettaControlPlane(ctx, zetta, "z0-zetta"); err != nil {
		t.Fatalf("replace stale schema Job: %v", err)
	}
	var job batchv1.Job
	key := types.NamespacedName{Namespace: zetta.Namespace, Name: "z0-zetta-schema"}
	if err := kubeClient.Get(ctx, key, &job); !apierrors.IsNotFound(err) {
		t.Fatalf("stale schema Job was not deleted: %v", err)
	}
	if err := reconciler.reconcileZettaControlPlane(ctx, zetta, "z0-zetta"); err != nil {
		t.Fatalf("create current schema Job: %v", err)
	}
	if err := kubeClient.Get(ctx, key, &job); err != nil {
		t.Fatalf("current schema Job: %v", err)
	}
	if job.Annotations[zettaSchemaVersionAnnotation] == "" ||
		job.Annotations[zettaSchemaVersionAnnotation] == "sha256:stale" {
		t.Errorf("schema version annotation = %q", job.Annotations[zettaSchemaVersionAnnotation])
	}
}

func TestDeploymentSelectUsesQueryParameters(t *testing.T) {
	requestURL := "http://example.test/v1/sql?statement=" + url.QueryEscape(deploymentSelectSQL("it's.example.com"))
	parsed, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("statement") != deploymentSelectSQL("it's.example.com") {
		t.Fatal("deployment SELECT did not survive URL encoding")
	}
}
