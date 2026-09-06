/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

type deploymentSQLDouble struct {
	server       *httptest.Server
	mu           sync.Mutex
	rows         []map[string]string
	statusWrites []map[string]string
	authFailures int
}

var statusInsertPattern = regexp.MustCompile(`VALUES \('([^']*)', 'status', '([^']*)', '([^']*)', '([^']*)', '([^']*)', '([^']*)'\)$`)

func newDeploymentSQLDouble(t *testing.T, rows []map[string]string) *deploymentSQLDouble {
	t.Helper()
	double := &deploymentSQLDouble{rows: rows}
	double.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			double.authFailures++
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		statement := request.URL.Query().Get("statement")
		double.mu.Lock()
		defer double.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(statement, "SELECT ") {
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"ok": true,
				"value": map[string]interface{}{
					"rows": double.rows,
				},
			})
			return
		}
		matches := statusInsertPattern.FindStringSubmatch(statement)
		if len(matches) != 7 {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"ok": false,
				"error": map[string]string{
					"message": "unexpected statement",
				},
			})
			return
		}
		row := map[string]string{
			"deployment_id": sqlTestUnquote(matches[1]),
			"record_type":   deploymentRecordTypeStatus,
			"lake":          sqlTestUnquote(matches[2]),
			"phase":         sqlTestUnquote(matches[3]),
			"url":           sqlTestUnquote(matches[4]),
			"message":       sqlTestUnquote(matches[5]),
			"updated_at":    sqlTestUnquote(matches[6]),
		}
		double.rows = append(double.rows, row)
		double.statusWrites = append(double.statusWrites, row)
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{
			"ok":    true,
			"value": map[string]interface{}{},
		})
	}))
	return double
}

func sqlTestUnquote(value string) string {
	return strings.ReplaceAll(value, "''", "'")
}

func (double *deploymentSQLDouble) close() {
	double.server.Close()
}

func (double *deploymentSQLDouble) addDesired(row map[string]string) {
	double.mu.Lock()
	defer double.mu.Unlock()
	double.rows = append(double.rows, row)
}

func (double *deploymentSQLDouble) writeCount() int {
	double.mu.Lock()
	defer double.mu.Unlock()
	return len(double.statusWrites)
}

func syncDeploymentRows(t *testing.T, reconciler *OramaZettaClusterReconciler, zetta *oramav1alpha1.OramaZettaCluster, double *deploymentSQLDouble) {
	t.Helper()
	rows, err := zettaSQLQuery(context.Background(), double.server.URL, "test-token", "orama_control", deploymentSelectSQL("prod"))
	if err != nil {
		t.Fatalf("query deployment rows: %v", err)
	}
	reconciler.materializeZettaDeploymentRows(context.Background(), zetta, double.server.URL, "test-token", rows)
}

func TestZettaDeploymentMaterializationLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:       "lake",
			Image:           "oramasearch/lake:test",
			IngressEnabled:  true,
			PreviewWildcard: "preview.example.com",
		},
	}
	zetta := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "control", Namespace: "lake"},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake).
		WithStatusSubresource(&oramav1alpha1.LakeEnvironment{}, &appsv1.Deployment{}, &networkingv1.Ingress{}).
		Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	desired := map[string]string{
		"deployment_id":    "deploy-1",
		"record_type":      deploymentRecordTypeDesired,
		"lake":             "prod",
		"repository":       "repo-1",
		"environment":      "checkout-preview",
		"kind":             oramav1alpha1.LakeEnvironmentKindPreview,
		"change_id":        "7ac4c59b92e46235a6f2",
		"image":            testEnvironmentImage,
		"replicas":         "1",
		"port":             "8080",
		"env_json":         `[{"name":"APP_ENV","value":"preview"}]`,
		"secret_refs_json": `[]`,
		"withdrawn":        "false",
		"change_closed":    "false",
		"updated_at":       "2026-09-05T10:00:00Z",
	}
	double := newDeploymentSQLDouble(t, []map[string]string{desired})
	defer double.close()

	syncDeploymentRows(t, reconciler, zetta, double)
	event, err := parseDeploymentRow(desired)
	if err != nil {
		t.Fatal(err)
	}
	environmentKey := types.NamespacedName{Namespace: "lake", Name: deploymentEnvironmentName(event)}
	var environment oramav1alpha1.LakeEnvironment
	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatalf("LakeEnvironment: %v", err)
	}
	if environment.Spec.Image != testEnvironmentImage || environment.Spec.Env[0].Value != "preview" ||
		environment.Annotations[deploymentIDAnnotation] != "deploy-1" || len(environment.OwnerReferences) != 1 ||
		environment.OwnerReferences[0].Name != "prod" {
		t.Errorf("LakeEnvironment = %+v", environment)
	}
	resourceVersion := environment.ResourceVersion
	syncDeploymentRows(t, reconciler, zetta, double)
	if err := kubeClient.Get(ctx, environmentKey, &environment); err != nil {
		t.Fatal(err)
	}
	if environment.ResourceVersion != resourceVersion || double.writeCount() != 0 {
		t.Errorf("identical desired row changed state: rv %s -> %s, status writes %d", resourceVersion, environment.ResourceVersion, double.writeCount())
	}

	environmentReconciler := &LakeEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	request := client.ObjectKey{Namespace: environment.Namespace, Name: environment.Name}
	if _, err := environmentReconciler.Reconcile(ctx, ctrlRequest(request)); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}
	syncDeploymentRows(t, reconciler, zetta, double)
	if double.writeCount() != 1 || double.statusWrites[0]["phase"] != oramav1alpha1.LakeEnvironmentPhaseProgressing {
		t.Fatalf("progressing callbacks = %+v", double.statusWrites)
	}
	syncDeploymentRows(t, reconciler, zetta, double)
	if double.writeCount() != 1 {
		t.Fatalf("steady Progressing callback count = %d", double.writeCount())
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
	syncDeploymentRows(t, reconciler, zetta, double)
	if double.writeCount() != 2 || double.statusWrites[1]["phase"] != oramav1alpha1.LakeEnvironmentPhaseReady ||
		double.statusWrites[1]["url"] != "https://7ac4c59b92e4.preview.example.com" {
		t.Fatalf("ready callbacks = %+v", double.statusWrites)
	}
	syncDeploymentRows(t, reconciler, zetta, double)
	if double.writeCount() != 2 {
		t.Fatalf("steady Ready callback count = %d", double.writeCount())
	}

	withdrawn := make(map[string]string, len(desired))
	for key, value := range desired {
		withdrawn[key] = value
	}
	withdrawn["withdrawn"] = "true"
	withdrawn["updated_at"] = "2026-09-05T10:01:00Z"
	double.addDesired(withdrawn)
	syncDeploymentRows(t, reconciler, zetta, double)
	if double.writeCount() != 3 || double.statusWrites[2]["phase"] != oramav1alpha1.LakeEnvironmentPhaseTornDown {
		t.Fatalf("teardown callbacks = %+v", double.statusWrites)
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
	if double.authFailures != 0 {
		t.Errorf("Zetta requests missing bearer: %d", double.authFailures)
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
	found := false
	for _, statement := range operatorZettaSchema {
		if statement.db == "orama_control" && strings.HasPrefix(statement.sql, "CREATE TABLE deployments ") {
			found = true
			for _, column := range []string{"deployment_id", "record_type", "change_id", "phase", "updated_at"} {
				if !strings.Contains(statement.sql, column+" TEXT") {
					t.Errorf("deployments schema missing %s: %s", column, statement.sql)
				}
			}
		}
	}
	if !found {
		t.Fatal("operatorZettaSchema does not create deployments")
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
	requestURL := "http://example.test/v1/sql?statement=" + url.QueryEscape(deploymentSelectSQL("prod"))
	parsed, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("statement") != deploymentSelectSQL("prod") {
		t.Fatal("deployment SELECT did not survive URL encoding")
	}
}
