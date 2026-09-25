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
	"regexp"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testWorkspaceID = "ws-0123456789ab"

// workspaceSQLDouble serves the Zetta /v1/sql capability for orama_control.workspaces: SELECTs
// return the current rows, and status INSERTs are parsed and appended so the next poll sees them.
type workspaceSQLDouble struct {
	server       *httptest.Server
	mu           sync.Mutex
	rows         []map[string]string
	statusWrites []map[string]string
	statements   []string
}

var workspaceStatusInsertPattern = regexp.MustCompile(`^INSERT INTO workspaces \(workspace_id, record_type, lake, customer_id, phase, url, message, updated_at\) VALUES \('((?:[^']|'')*)', 'status', '((?:[^']|'')*)', '((?:[^']|'')*)', '((?:[^']|'')*)', '((?:[^']|'')*)', '((?:[^']|'')*)', '((?:[^']|'')*)'\)$`)

func newWorkspaceSQLDouble(t *testing.T, rows ...map[string]string) *workspaceSQLDouble {
	t.Helper()
	double := &workspaceSQLDouble{rows: rows}
	double.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		statement := request.URL.Query().Get("statement")
		double.mu.Lock()
		defer double.mu.Unlock()
		double.statements = append(double.statements, statement)
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(statement, "SELECT ") {
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"ok": true, "value": map[string]interface{}{"rows": double.rows}})
			return
		}
		matches := workspaceStatusInsertPattern.FindStringSubmatch(statement)
		if len(matches) != 8 {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"ok": false, "error": map[string]string{"message": "unexpected statement"}})
			return
		}
		row := map[string]string{
			"workspace_id": sqlTestUnquote(matches[1]),
			"record_type":  workspaceRecordTypeStatus,
			"lake":         sqlTestUnquote(matches[2]),
			"customer_id":  sqlTestUnquote(matches[3]),
			"phase":        sqlTestUnquote(matches[4]),
			"url":          sqlTestUnquote(matches[5]),
			"message":      sqlTestUnquote(matches[6]),
			"updated_at":   sqlTestUnquote(matches[7]),
		}
		double.rows = append(double.rows, row)
		double.statusWrites = append(double.statusWrites, row)
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{"ok": true, "value": map[string]interface{}{}})
	}))
	t.Cleanup(double.server.Close)
	return double
}

func (double *workspaceSQLDouble) writes() []map[string]string {
	double.mu.Lock()
	defer double.mu.Unlock()
	return append([]map[string]string(nil), double.statusWrites...)
}

func (double *workspaceSQLDouble) addRow(row map[string]string) {
	double.mu.Lock()
	defer double.mu.Unlock()
	double.rows = append(double.rows, row)
}

func desiredWorkspaceRow(lake, workspaceID, customerID, updatedAt string) map[string]string {
	return map[string]string{
		"workspace_id": workspaceID,
		"record_type":  workspaceRecordTypeDesired,
		"lake":         lake,
		"customer_id":  customerID,
		"updated_at":   updatedAt,
	}
}

func workspaceTestLake(enabled bool) *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake",
			Image:          "oramasearch/lake:test",
			PullSecretName: "oramasearch-dockerhub",
			Workspaces: oramav1alpha1.LakeWorkspacesSpec{
				Enabled: enabled,
				Image:   "oramasearch/zetta:workspace",
			},
		},
	}
}

func workspaceTestDataPlane() *oramav1alpha1.OramaZettaCluster {
	return &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-zetta", Namespace: "lake"}}
}

// syncWorkspaceRows runs one poll the way reconcileZettaWorkspaces does, against the double.
func syncWorkspaceRows(t *testing.T, reconciler *OramaZettaClusterReconciler, zetta *oramav1alpha1.OramaZettaCluster, lakeName string, double *workspaceSQLDouble) {
	t.Helper()
	ctx := context.Background()
	var lake oramav1alpha1.Lake
	if err := reconciler.Get(ctx, client.ObjectKey{Name: lakeName}, &lake); err != nil {
		t.Fatalf("get lake: %v", err)
	}
	rows, err := zettaSQLQuery(ctx, double.server.URL, "test-token", "orama_control", workspaceSelectSQL(lakeName))
	if err != nil {
		t.Fatalf("query workspace rows: %v", err)
	}
	reconciler.materializeZettaWorkspaceRows(ctx, zetta, &lake, double.server.URL, "test-token", rows)
}

func TestWorkspaceSchemaIncludesEventTable(t *testing.T) {
	want := "CREATE TABLE workspaces (workspace_id TEXT, record_type TEXT, lake TEXT, customer_id TEXT, phase TEXT, url TEXT, message TEXT, updated_at TEXT)"
	for _, statement := range operatorZettaSchema {
		if statement.db == "orama_control" && strings.HasPrefix(statement.sql, "CREATE TABLE workspaces ") {
			if statement.sql != want {
				t.Errorf("workspaces schema = %s, want %s", statement.sql, want)
			}
			return
		}
	}
	t.Fatal("operatorZettaSchema does not create orama_control.workspaces")
}

func TestWorkspaceSQLStatements(t *testing.T) {
	if got := workspaceSelectSQL("o'prod"); got != "SELECT workspace_id, record_type, lake, customer_id, phase, url, message, updated_at FROM workspaces WHERE lake = 'o''prod'" {
		t.Errorf("select = %s", got)
	}
	got := insertWorkspaceStatusSQL(testWorkspaceID, "prod", "cus_1", "Failed", "", "it's bad", "2026-09-25T00:00:00Z")
	want := "INSERT INTO workspaces (workspace_id, record_type, lake, customer_id, phase, url, message, updated_at) VALUES ('ws-0123456789ab', 'status', 'prod', 'cus_1', 'Failed', '', 'it''s bad', '2026-09-25T00:00:00Z')"
	if got != want {
		t.Errorf("insert = %s", got)
	}
}

func TestZettaWorkspaceMaterializationLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := workspaceTestLake(true)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(&oramav1alpha1.OramaZettaCluster{}).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	zetta := workspaceTestDataPlane()
	double := newWorkspaceSQLDouble(t, desiredWorkspaceRow("prod", testWorkspaceID, "cus_1", "2026-09-25T10:00:00Z"))

	syncWorkspaceRows(t, reconciler, zetta, "prod", double)

	key := types.NamespacedName{Namespace: "lake", Name: "prod-" + testWorkspaceID}
	var cluster oramav1alpha1.OramaZettaCluster
	if err := kubeClient.Get(ctx, key, &cluster); err != nil {
		t.Fatalf("workspace OramaZettaCluster: %v", err)
	}
	memoryRequest := cluster.Spec.Resources.Requests[corev1.ResourceMemory]
	memoryLimit := cluster.Spec.Resources.Limits[corev1.ResourceMemory]
	if cluster.Spec.Replicas != 1 || cluster.Spec.Image != "oramasearch/zetta:workspace" ||
		cluster.Spec.PullSecretName != "oramasearch-dockerhub" || cluster.Spec.Storage.Size != "5Gi" ||
		cluster.Spec.IngressEnabled || cluster.Spec.ProvisionOperatorSchema == nil || *cluster.Spec.ProvisionOperatorSchema ||
		memoryRequest.Cmp(resource.MustParse("64Mi")) != 0 || memoryLimit.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("workspace spec = %+v", cluster.Spec)
	}
	if _, found := cluster.Spec.Resources.Requests[corev1.ResourceCPU]; found {
		t.Errorf("workspace requests CPU: %+v", cluster.Spec.Resources)
	}
	if !metav1.IsControlledBy(&cluster, lake) {
		t.Errorf("workspace owner refs = %+v", cluster.OwnerReferences)
	}
	if cluster.Labels[workspaceLakeLabel] != "prod" || cluster.Labels[workspaceIDLabel] != testWorkspaceID ||
		cluster.Labels[workspaceCustomerLabel] != "cus_1" || cluster.Annotations[workspaceCustomerAnnotation] != "cus_1" {
		t.Errorf("workspace metadata labels=%v annotations=%v", cluster.Labels, cluster.Annotations)
	}

	wantURL := "http://prod-ws-0123456789ab-zetta.lake.svc.cluster.local:4800"
	writes := double.writes()
	if len(writes) != 1 || writes[0]["phase"] != WorkspacePhaseProvisioning || writes[0]["url"] != wantURL ||
		writes[0]["customer_id"] != "cus_1" || writes[0]["lake"] != "prod" {
		t.Fatalf("first status writes = %+v", writes)
	}

	// Re-polling, including a repeated desired row for the same workspace, changes nothing.
	resourceVersion := cluster.ResourceVersion
	double.addRow(desiredWorkspaceRow("prod", testWorkspaceID, "cus_1", "2026-09-25T10:05:00Z"))
	syncWorkspaceRows(t, reconciler, zetta, "prod", double)
	syncWorkspaceRows(t, reconciler, zetta, "prod", double)
	if err := kubeClient.Get(ctx, key, &cluster); err != nil {
		t.Fatal(err)
	}
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := kubeClient.List(ctx, &clusters); err != nil {
		t.Fatal(err)
	}
	if cluster.ResourceVersion != resourceVersion || len(clusters.Items) != 1 || len(double.writes()) != 1 {
		t.Errorf("re-poll changed state: rv %s -> %s, clusters %d, writes %d",
			resourceVersion, cluster.ResourceVersion, len(clusters.Items), len(double.writes()))
	}

	// The node is ready but lake cannot connect yet: still Provisioning, with a new message.
	cluster.Status.Phase = "Ready"
	cluster.Status.ReadyNodes = 1
	if err := kubeClient.Status().Update(ctx, &cluster); err != nil {
		t.Fatal(err)
	}
	syncWorkspaceRows(t, reconciler, zetta, "prod", double)
	writes = double.writes()
	if len(writes) != 2 || writes[1]["phase"] != WorkspacePhaseProvisioning || writes[1]["message"] != "waiting for the workspace credential" {
		t.Fatalf("credential-wait writes = %+v", writes)
	}

	// The workspace admin token is minted and aggregated into the Lake's Secret: Ready.
	if err := kubeClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name + "-zetta-admin", Namespace: "lake"},
		Data:       map[string][]byte{"token": []byte("workspace-token")},
	}); err != nil {
		t.Fatal(err)
	}
	lakeReconciler := &LakeReconciler{Client: kubeClient, Scheme: scheme}
	if err := lakeReconciler.reconcileLakeWorkspaceSecret(ctx, lake, "lake", map[string]string{"app.kubernetes.io/name": "lake"}); err != nil {
		t.Fatal(err)
	}
	syncWorkspaceRows(t, reconciler, zetta, "prod", double)
	syncWorkspaceRows(t, reconciler, zetta, "prod", double)
	writes = double.writes()
	if len(writes) != 3 || writes[2]["phase"] != WorkspacePhaseReady || writes[2]["url"] != wantURL {
		t.Fatalf("ready writes = %+v", writes)
	}
}

func TestZettaWorkspaceLocalityRule(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		mutate   func(*oramav1alpha1.Lake)
		zetta    *oramav1alpha1.OramaZettaCluster
		rowLake  string
		pollLake string
	}{
		{name: "workspaces disabled", mutate: func(l *oramav1alpha1.Lake) { l.Spec.Workspaces.Enabled = false }, zetta: workspaceTestDataPlane(), rowLake: "prod", pollLake: "prod"},
		{name: "zetta disabled", mutate: func(l *oramav1alpha1.Lake) { off := false; l.Spec.Zetta.Enabled = &off }, zetta: workspaceTestDataPlane(), rowLake: "prod", pollLake: "prod"},
		{name: "other namespace", mutate: func(*oramav1alpha1.Lake) {}, zetta: &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "elsewhere"}}, rowLake: "prod", pollLake: "prod"},
		{name: "row names another lake", mutate: func(*oramav1alpha1.Lake) {}, zetta: workspaceTestDataPlane(), rowLake: "staging", pollLake: "prod"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := newLakeScheme(t)
			lake := workspaceTestLake(true)
			test.mutate(lake)
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
			reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
			double := newWorkspaceSQLDouble(t, desiredWorkspaceRow(test.rowLake, testWorkspaceID, "cus_1", "2026-09-25T10:00:00Z"))
			syncWorkspaceRows(t, reconciler, test.zetta, test.pollLake, double)
			var clusters oramav1alpha1.OramaZettaClusterList
			if err := kubeClient.List(ctx, &clusters); err != nil {
				t.Fatal(err)
			}
			if len(clusters.Items) != 0 || len(double.writes()) != 0 {
				t.Errorf("non-local row materialized: clusters %d, writes %+v", len(clusters.Items), double.writes())
			}
		})
	}
}

func TestZettaWorkspaceRejectsInvalidRows(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		lakeName    string
		workspaceID string
		customerID  string
		wantMessage string
	}{
		{name: "uppercase hex", lakeName: "prod", workspaceID: "ws-0123456789AB", customerID: "cus_1", wantMessage: "workspace_id must match"},
		{name: "short id", lakeName: "prod", workspaceID: "ws-0123", customerID: "cus_1", wantMessage: "workspace_id must match"},
		{name: "no prefix", lakeName: "prod", workspaceID: "0123456789abcdef", customerID: "cus_1", wantMessage: "workspace_id must match"},
		{name: "quote injection", lakeName: "prod", workspaceID: "ws-'; DROP", customerID: "cus_1", wantMessage: "workspace_id must match"},
		{name: "missing customer", lakeName: "prod", workspaceID: testWorkspaceID, customerID: "", wantMessage: "customer_id is required"},
		{name: "lake name too long", lakeName: strings.Repeat("l", 33), workspaceID: testWorkspaceID, customerID: "cus_1", wantMessage: "too long"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := newLakeScheme(t)
			lake := workspaceTestLake(true)
			lake.Name = test.lakeName
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
			reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
			double := newWorkspaceSQLDouble(t, desiredWorkspaceRow(test.lakeName, test.workspaceID, test.customerID, "2026-09-25T10:00:00Z"))
			syncWorkspaceRows(t, reconciler, workspaceTestDataPlane(), test.lakeName, double)
			syncWorkspaceRows(t, reconciler, workspaceTestDataPlane(), test.lakeName, double)
			var clusters oramav1alpha1.OramaZettaClusterList
			if err := kubeClient.List(ctx, &clusters); err != nil {
				t.Fatal(err)
			}
			writes := double.writes()
			if len(clusters.Items) != 0 {
				t.Errorf("invalid row created %d clusters", len(clusters.Items))
			}
			if len(writes) != 1 || writes[0]["phase"] != WorkspacePhaseFailed || writes[0]["url"] != "" ||
				!strings.Contains(writes[0]["message"], test.wantMessage) {
				t.Errorf("status writes = %+v", writes)
			}
		})
	}
}

func TestZettaWorkspaceRefusesAnotherCustomer(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := workspaceTestLake(true)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	double := newWorkspaceSQLDouble(t, desiredWorkspaceRow("prod", testWorkspaceID, "cus_1", "2026-09-25T10:00:00Z"))
	syncWorkspaceRows(t, reconciler, workspaceTestDataPlane(), "prod", double)
	double.addRow(desiredWorkspaceRow("prod", testWorkspaceID, "cus_2", "2026-09-25T11:00:00Z"))
	syncWorkspaceRows(t, reconciler, workspaceTestDataPlane(), "prod", double)

	var cluster oramav1alpha1.OramaZettaCluster
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "prod-" + testWorkspaceID}, &cluster); err != nil {
		t.Fatal(err)
	}
	writes := double.writes()
	if cluster.Annotations[workspaceCustomerAnnotation] != "cus_1" || len(writes) != 2 ||
		writes[1]["phase"] != WorkspacePhaseFailed || !strings.Contains(writes[1]["message"], "another customer") {
		t.Errorf("rebind: annotations %v writes %+v", cluster.Annotations, writes)
	}
}

func TestWorkspaceCustomerLabelValue(t *testing.T) {
	if got := workspaceCustomerLabelValue("cus_1"); got != "cus_1" {
		t.Errorf("valid label value rewritten to %q", got)
	}
	got := workspaceCustomerLabelValue("person@example.com")
	if !strings.HasPrefix(got, "sha256-") || len(got) != 23 || got != workspaceCustomerLabelValue("person@example.com") {
		t.Errorf("hashed label value = %q", got)
	}
}

func TestLakeWorkspaceSecretAggregatesTokens(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := workspaceTestLake(true)
	owner := metav1.OwnerReference{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: "prod", UID: "lake-uid", Controller: ptrBool(true)}
	workspace := func(id string, owned bool) *oramav1alpha1.OramaZettaCluster {
		cluster := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
			Name: "prod-" + id, Namespace: "lake",
			Labels: map[string]string{workspaceLakeLabel: "prod", workspaceIDLabel: id},
		}}
		if owned {
			cluster.OwnerReferences = []metav1.OwnerReference{owner}
		}
		return cluster
	}
	adminSecret := func(id, token string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-" + id + "-zetta-admin", Namespace: "lake"},
			Data:       map[string][]byte{"token": []byte(token)},
		}
	}
	const second = "ws-bbbbbbbbbbbb"
	const foreign = "ws-cccccccccccc"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		lake,
		workspace(testWorkspaceID, true), adminSecret(testWorkspaceID, "token-a"),
		workspace(second, true),
		workspace(foreign, false), adminSecret(foreign, "token-foreign"),
	).Build()
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: scheme}
	if err := reconciler.reconcileLakeWorkspaceSecret(ctx, lake, "lake", map[string]string{"app.kubernetes.io/name": "lake"}); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: "lake", Name: "lake-prod-workspaces"}
	if err := kubeClient.Get(ctx, key, &secret); err != nil {
		t.Fatalf("workspace Secret: %v", err)
	}
	if len(secret.Data) != 1 || string(secret.Data[testWorkspaceID]) != "token-a" || !metav1.IsControlledBy(&secret, lake) {
		t.Errorf("first aggregation data=%v owners=%v", secret.Data, secret.OwnerReferences)
	}

	if err := kubeClient.Create(ctx, adminSecret(second, "token-b")); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reconcileLakeWorkspaceSecret(ctx, lake, "lake", nil); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 2 || string(secret.Data[second]) != "token-b" || string(secret.Data[testWorkspaceID]) != "token-a" {
		t.Errorf("second aggregation data=%v", secret.Data)
	}
	resourceVersion := secret.ResourceVersion
	if err := reconciler.reconcileLakeWorkspaceSecret(ctx, lake, "lake", nil); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	if secret.ResourceVersion != resourceVersion {
		t.Errorf("steady aggregation rewrote the Secret: %s -> %s", resourceVersion, secret.ResourceVersion)
	}
}

func TestLakeReconcileWorkspacesWiring(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := workspaceTestLake(false)
	lake.Spec.Namespace = "lake-dev"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &oramav1alpha1.OramaZettaCluster{}).Build()
	applyLakeSchemaForWorkloadTest(t, kubeClient, scheme, lake)
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod"}}
	deploymentKey := types.NamespacedName{Namespace: "lake-dev", Name: "lake-prod"}
	dataPlaneKey := types.NamespacedName{Namespace: "lake-dev", Name: "lake-prod-zetta"}
	secretKey := types.NamespacedName{Namespace: "lake-dev", Name: "lake-prod-workspaces"}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile disabled: %v", err)
	}
	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if findEnv(container.Env, "LAKE_WORKSPACE_DIR") != nil || findEnv(container.Env, "LAKE_WORKSPACE_URL_TEMPLATE") != nil ||
		len(deployment.Spec.Template.Spec.Volumes) != 1 || len(container.VolumeMounts) != 1 {
		t.Errorf("disabled workspaces rendered env/volume: env=%+v volumes=%+v", container.Env, deployment.Spec.Template.Spec.Volumes)
	}
	var dataPlane oramav1alpha1.OramaZettaCluster
	if err := kubeClient.Get(ctx, dataPlaneKey, &dataPlane); err != nil {
		t.Fatal(err)
	}
	if dataPlane.Spec.ProvisionOperatorSchema == nil || *dataPlane.Spec.ProvisionOperatorSchema {
		t.Errorf("disabled workspaces provision the operator schema: %+v", dataPlane.Spec.ProvisionOperatorSchema)
	}
	if err := kubeClient.Get(ctx, secretKey, &corev1.Secret{}); err == nil {
		t.Error("workspace Secret created while disabled")
	}

	// Enable, and give the Lake an existing workspace cluster the data-plane loop must keep.
	if err := kubeClient.Get(ctx, request.NamespacedName, lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Workspaces.Enabled = true
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_WORKSPACE_DIR", Value: "/override"}}
	if err := kubeClient.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	workspace := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "prod-" + testWorkspaceID, Namespace: "lake-dev",
		Labels: map[string]string{workspaceLakeLabel: "prod", workspaceIDLabel: testWorkspaceID},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: "prod", UID: lake.UID, Controller: ptrBool(true),
		}},
	}}
	if err := kubeClient.Create(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile enabled: %v", err)
	}
	if result.RequeueAfter != lakeStatusRequeue {
		t.Errorf("requeueAfter = %s", result.RequeueAfter)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: workspace.Name}, workspace); err != nil {
		t.Errorf("workspace cluster removed by the data-plane reconcile: %v", err)
	}
	if err := kubeClient.Get(ctx, dataPlaneKey, &dataPlane); err != nil {
		t.Fatal(err)
	}
	if dataPlane.Spec.ProvisionOperatorSchema == nil || !*dataPlane.Spec.ProvisionOperatorSchema {
		t.Errorf("enabled workspaces do not provision the operator schema: %+v", dataPlane.Spec.ProvisionOperatorSchema)
	}
	var secret corev1.Secret
	if err := kubeClient.Get(ctx, secretKey, &secret); err != nil || !metav1.IsControlledBy(&secret, lake) {
		t.Errorf("workspace Secret = %+v (err %v)", secret.ObjectMeta, err)
	}
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	container = deployment.Spec.Template.Spec.Containers[0]
	dir := findEnv(container.Env, "LAKE_WORKSPACE_DIR")
	template := findEnv(container.Env, "LAKE_WORKSPACE_URL_TEMPLATE")
	if dir == nil || dir.Value != "/var/run/lake/workspaces" || countEnv(container.Env, "LAKE_WORKSPACE_DIR") != 1 {
		t.Errorf("LAKE_WORKSPACE_DIR = %+v", container.Env)
	}
	if template == nil || template.Value != "http://prod-{workspace}-zetta.lake-dev.svc.cluster.local:4800" {
		t.Errorf("LAKE_WORKSPACE_URL_TEMPLATE = %+v", template)
	}
	var volume *corev1.Volume
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == lakeWorkspaceVolume {
			volume = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "lake-prod-workspaces" ||
		volume.Secret.Optional == nil || !*volume.Secret.Optional {
		t.Errorf("workspace volume = %+v", volume)
	}
	mounted := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == lakeWorkspaceVolume && mount.MountPath == "/var/run/lake/workspaces" && mount.ReadOnly && mount.SubPath == "" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("workspace mount = %+v", container.VolumeMounts)
	}

	// Disabling removes the env and volume but never the workspace cluster.
	if err := kubeClient.Get(ctx, request.NamespacedName, lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Workspaces.Enabled = false
	if err := kubeClient.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if findEnv(deployment.Spec.Template.Spec.Containers[0].Env, "LAKE_WORKSPACE_URL_TEMPLATE") != nil ||
		len(deployment.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("disable left workspace wiring: %+v", deployment.Spec.Template.Spec)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: workspace.Name}, workspace); err != nil {
		t.Errorf("disabling workspaces deleted a workspace cluster: %v", err)
	}
}

func countEnv(env []corev1.EnvVar, name string) int {
	count := 0
	for _, variable := range env {
		if variable.Name == name {
			count++
		}
	}
	return count
}

func ptrBool(value bool) *bool { return &value }
