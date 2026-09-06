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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestGatherOperatorStateIncludesLakesInDatastoreNamespace(t *testing.T) {
	scheme := newLakeScheme(t)
	two := int32(2)
	controlLake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "control",
			Image:     "oramasearch/lake:test",
			Replicas:  &two,
		},
		Status: oramav1alpha1.LakeStatus{
			Phase:             "Ready",
			ReadyReplicas:     2,
			PostgresReady:     true,
			PostgresInstances: "1/1",
			SchemaPhase:       "Applied",
		},
	}
	defaultLake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       oramav1alpha1.LakeSpec{Image: "oramasearch/lake:test"},
	}
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(controlLake, defaultLake).
		Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubernetesClient}

	rows := reconciler.gatherOperatorState(context.Background(), "control", "operator-a")
	if len(rows) != 1 {
		t.Fatalf("control rows = %+v", rows)
	}
	row := rows[0]
	if row.operator != "operator-a" || row.name != "prod" || row.kind != "Lake" ||
		row.owner != "prod" || row.status != "Ready" {
		t.Fatalf("Lake row = %+v", row)
	}
	var detail struct {
		DesiredReplicas int32                    `json:"desiredReplicas"`
		Status          oramav1alpha1.LakeStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(row.detail), &detail); err != nil {
		t.Fatalf("decode Lake detail: %v", err)
	}
	if detail.DesiredReplicas != 2 || detail.Status.ReadyReplicas != 2 ||
		!detail.Status.PostgresReady || detail.Status.SchemaPhase != "Applied" {
		t.Fatalf("Lake detail = %+v", detail)
	}

	defaultRows := reconciler.gatherOperatorState(context.Background(), "lake-dev", "operator-a")
	if len(defaultRows) != 1 || defaultRows[0].name != "default" || defaultRows[0].status != "Unknown" {
		t.Fatalf("default namespace rows = %+v", defaultRows)
	}
}

func TestLakeStateSyncRetriesMissingEventAndAntiFlaps(t *testing.T) {
	componentWrites := make([]string, 0)
	eventWrites := make([]string, 0)
	failNextEvent := true
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer admin-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Query().Get("database") != "orama_control" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		statement := request.URL.Query().Get("statement")
		switch {
		case strings.HasPrefix(statement, "INSERT INTO components "):
			componentWrites = append(componentWrites, statement)
		case strings.HasPrefix(statement, "INSERT INTO reconcile_events "):
			eventWrites = append(eventWrites, statement)
			if failNextEvent {
				failNextEvent = false
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		default:
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	reconciler := &OramaZettaClusterReconciler{}
	row := componentRow{
		operator: "operator-a",
		name:     "prod",
		kind:     "Lake",
		owner:    "prod",
		status:   "Ready",
		detail:   `{"desiredReplicas":2,"status":{"readyReplicas":2,"phase":"Ready"}}`,
	}
	ctx := context.Background()
	now := "2026-09-06T12:00:00Z"
	if synced := reconciler.syncOperatorState(ctx, server.URL, "admin-token", []componentRow{row}, now); synced != 0 {
		t.Fatalf("first sync completed %d rows, want 0 after event failure", synced)
	}
	if len(componentWrites) != 1 || len(eventWrites) != 1 {
		t.Fatalf("first sync writes: components=%d events=%d", len(componentWrites), len(eventWrites))
	}
	if synced := reconciler.syncOperatorState(ctx, server.URL, "admin-token", []componentRow{row}, now); synced != 1 {
		t.Fatalf("retry completed %d rows, want 1", synced)
	}
	if len(componentWrites) != 1 || len(eventWrites) != 2 {
		t.Fatalf("retry writes: components=%d events=%d", len(componentWrites), len(eventWrites))
	}
	if synced := reconciler.syncOperatorState(ctx, server.URL, "admin-token", []componentRow{row}, now); synced != 0 {
		t.Fatalf("steady sync completed %d rows, want 0", synced)
	}
	if len(componentWrites) != 1 || len(eventWrites) != 2 {
		t.Fatalf("steady sync wrote: components=%d events=%d", len(componentWrites), len(eventWrites))
	}

	eventValues, err := parseSQLInsertValues(eventWrites[1])
	if err != nil || len(eventValues) != 4 {
		t.Fatalf("parse reconcile event: values=%v err=%v", eventValues, err)
	}
	if eventValues[0] != now || eventValues[1] != "operator-a" || eventValues[2] != "component.state_changed" {
		t.Fatalf("reconcile event values = %v", eventValues)
	}
	var eventDetail struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Owner  string `json:"owner"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(eventValues[3]), &eventDetail); err != nil {
		t.Fatalf("decode reconcile event detail: %v", err)
	}
	if eventDetail.Name != "prod" || eventDetail.Kind != "Lake" || eventDetail.Owner != "prod" ||
		eventDetail.Status != "Ready" || eventDetail.Detail != row.detail {
		t.Fatalf("reconcile event detail = %+v", eventDetail)
	}

	row.status = "Degraded"
	if synced := reconciler.syncOperatorState(ctx, server.URL, "admin-token", []componentRow{row}, now); synced != 1 {
		t.Fatalf("transition completed %d rows, want 1", synced)
	}
	if len(componentWrites) != 2 || len(eventWrites) != 3 {
		t.Fatalf("transition writes: components=%d events=%d", len(componentWrites), len(eventWrites))
	}
}
