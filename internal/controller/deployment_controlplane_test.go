/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
	controlplanecontract "github.com/oramasearch/orama-cloud-v1-operator/docs/control-plane"
)

const liquidOpenAPISHA256 = "96dee72f3085489ef586a8c3108fa4922d14e61a9f869c9e851580236d2e7b2e"

type contractSQLDouble struct {
	server        *httptest.Server
	mutex         sync.Mutex
	records       []map[string]string
	desiredWrites [][]string
	auditWrites   [][]string
	authFailures  int
}

func newContractSQLDouble(t *testing.T) *contractSQLDouble {
	t.Helper()
	double := &contractSQLDouble{}
	double.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer zetta-token" {
			double.mutex.Lock()
			double.authFailures++
			double.mutex.Unlock()
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		statement := request.URL.Query().Get("statement")
		double.mutex.Lock()
		defer double.mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(statement, "SELECT ") && strings.Contains(statement, "FROM deployment_records"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok":    true,
				"value": map[string]any{"rows": double.records},
			})
		case strings.HasPrefix(statement, "INSERT INTO deployment_records "):
			values, err := parseSQLInsertValues(statement)
			if err != nil || len(values) != 10 {
				writer.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false})
				return
			}
			double.records = append(double.records, map[string]string{
				"deployment_id":    values[0],
				"lake":             values[1],
				"repository":       values[2],
				"environment":      values[3],
				"change_id":        values[4],
				"image":            values[5],
				"result_id":        values[6],
				"rollback_of":      values[7],
				"secret_refs_json": values[8],
				"created_at":       values[9],
			})
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "value": map[string]any{}})
		case strings.HasPrefix(statement, "INSERT INTO deployments "):
			values, err := parseSQLInsertValues(statement)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			double.desiredWrites = append(double.desiredWrites, values)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "value": map[string]any{}})
		case strings.HasPrefix(statement, "INSERT INTO reconcile_events "):
			values, err := parseSQLInsertValues(statement)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			double.auditWrites = append(double.auditWrites, values)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "value": map[string]any{}})
		default:
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "error": map[string]string{"message": "unexpected SQL"}})
		}
	}))
	return double
}

func (double *contractSQLDouble) close() {
	double.server.Close()
}

func (double *contractSQLDouble) counts() (int, int, int) {
	double.mutex.Lock()
	defer double.mutex.Unlock()
	return len(double.records), len(double.desiredWrites), len(double.auditWrites)
}

func parseSQLInsertValues(statement string) ([]string, error) {
	marker := " VALUES ("
	start := strings.Index(statement, marker)
	if start < 0 || !strings.HasSuffix(statement, ")") {
		return nil, os.ErrInvalid
	}
	input := statement[start+len(marker) : len(statement)-1]
	values := make([]string, 0)
	for len(input) > 0 {
		if input[0] != '\'' {
			return nil, os.ErrInvalid
		}
		input = input[1:]
		var value strings.Builder
		closed := false
		for len(input) > 0 {
			if input[0] != '\'' {
				value.WriteByte(input[0])
				input = input[1:]
				continue
			}
			if len(input) > 1 && input[1] == '\'' {
				value.WriteByte('\'')
				input = input[2:]
				continue
			}
			input = input[1:]
			closed = true
			break
		}
		if !closed {
			return nil, os.ErrInvalid
		}
		values = append(values, value.String())
		if input == "" {
			break
		}
		if !strings.HasPrefix(input, ", ") {
			return nil, os.ErrInvalid
		}
		input = input[2:]
	}
	return values, nil
}

func TestDeploymentOpenAPIContractMatchesLiquidVendoredCopy(t *testing.T) {
	operatorDocument, err := os.ReadFile("../../docs/control-plane/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(operatorDocument, controlplanecontract.OpenAPI) {
		t.Fatal("served OpenAPI bytes differ from docs/control-plane/openapi.json")
	}
	digest := sha256.Sum256(operatorDocument)
	actual := hex.EncodeToString(digest[:])
	if actual != liquidOpenAPISHA256 {
		t.Fatalf("OpenAPI contract differs from liquid's vendored copy: operator sha256:%s, liquid sha256:%s", actual, liquidOpenAPISHA256)
	}
	liquidSource := os.Getenv("LIQUID_SOURCE_DIR")
	if liquidSource == "" {
		candidate, pathErr := filepath.Abs("../../../../../liquid")
		if pathErr == nil {
			if _, statErr := os.Stat(filepath.Join(candidate, "contracts/control-plane/openapi.json")); statErr == nil {
				liquidSource = candidate
			}
		}
	}
	if liquidSource == "" {
		return
	}
	liquidDocument, err := os.ReadFile(filepath.Join(liquidSource, "contracts/control-plane/openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(operatorDocument, liquidDocument) {
		liquidDigest := sha256.Sum256(liquidDocument)
		t.Fatalf("OpenAPI contract drift: operator sha256:%s, liquid sha256:%s", actual, hex.EncodeToString(liquidDigest[:]))
	}
}

func deploymentControlPlaneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		oramav1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func deploymentControlPlaneObjects(includeSecret bool) []client.Object {
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake", Image: "oramasearch/lake:test"},
	}
	zetta := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "control", Namespace: "lake", UID: "zetta-uid"},
	}
	objects := []client.Object{
		lake,
		zetta,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeAdminSecret("prod"), Namespace: "lake"}, Data: map[string][]byte{"token": []byte("environment-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "control-zetta-admin", Namespace: "lake"}, Data: map[string][]byte{"token": []byte("zetta-token")}},
	}
	if !includeSecret {
		return objects
	}
	controller := true
	sealed := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "lake", UID: "sealed-uid", Generation: 1},
		Spec: oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  corev1.LocalObjectReference{Name: "prod"},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "application-database"},
			Source:   oramav1alpha1.LakeSealedSecretSource{Type: "literal"},
		},
		Status: oramav1alpha1.LakeSealedSecretStatus{
			Phase:              "Ready",
			ObservedGeneration: 1,
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Delivered",
				ObservedGeneration: 1,
			}},
		},
	}
	produced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "application-database",
			Namespace: "lake",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: oramav1alpha1.GroupVersion.String(),
				Kind:       "LakeSealedSecret",
				Name:       sealed.Name,
				UID:        sealed.UID,
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{"DATABASE_URL": []byte("redacted")},
	}
	return append(objects, sealed, produced)
}

func controlPlaneRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	body string,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeDeploymentResponse(t *testing.T, response *httptest.ResponseRecorder) deploymentRecord {
	t.Helper()
	var envelope struct {
		OK    bool             `json:"ok"`
		Value deploymentRecord `json:"value"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response %d %s: %v", response.Code, response.Body.String(), err)
	}
	if response.Code != http.StatusOK || !envelope.OK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	return envelope.Value
}

func TestDeploymentControlPlaneRESTAndMCP(t *testing.T) {
	scheme := deploymentControlPlaneScheme(t)
	objects := deploymentControlPlaneObjects(true)
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.LakeEnvironment{}, &oramav1alpha1.LakeSealedSecret{}).
		Build()
	double := newContractSQLDouble(t)
	defer double.close()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	server := &DeploymentControlPlane{Client: kubeClient, ZettaURL: double.server.URL, Now: func() time.Time {
		current := now
		now = now.Add(time.Second)
		return current
	}}
	handler := server.Handler()
	openAPIResponse := controlPlaneRequest(t, handler, http.MethodGet, "/v1/openapi.json", "", "")
	if openAPIResponse.Code != http.StatusOK || !bytes.Equal(openAPIResponse.Body.Bytes(), controlplanecontract.OpenAPI) {
		t.Errorf("served OpenAPI differs: status %d", openAPIResponse.Code)
	}
	first, second, changeOne, image := exerciseDeploymentCreates(t, handler, double)
	exerciseDeploymentStatusAndRollback(t, handler, kubeClient, first, second)
	exerciseDeploymentMCPAndRefusals(t, handler, double, first, changeOne, image)
}

func exerciseDeploymentCreates(
	t *testing.T,
	handler http.Handler,
	double *contractSQLDouble,
) (deploymentRecord, deploymentRecord, string, string) {
	t.Helper()
	changeOne := "blake3:" + strings.Repeat("1", 64)
	changeTwo := "blake3:" + strings.Repeat("2", 64)
	image := "registry.example.test/app@sha256:" + strings.Repeat("a", 64)
	createOne := fmtJSON(map[string]any{
		"repository":  "repo-1",
		"environment": "production",
		"change":      changeOne,
		"image":       image,
		"secrets":     []string{"secret://database"},
	})
	first := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/create_deployment", createOne, "environment-token"))
	if first.Repository != "repo-1" || first.Change != changeOne {
		t.Errorf("first deployment = %+v", first)
	}
	secondAttempt := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/create_deployment", createOne, "environment-token"))
	if secondAttempt.ID != first.ID {
		t.Errorf("idempotent create IDs = %q, %q", first.ID, secondAttempt.ID)
	}
	if records, desired, audits := double.counts(); records != 1 || desired != 1 || audits != 2 {
		t.Errorf("after idempotent create records=%d desired=%d audits=%d", records, desired, audits)
	}

	createTwo := fmtJSON(map[string]any{
		"repository":  "repo-1",
		"environment": "production",
		"change":      changeTwo,
		"image":       image,
	})
	second := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/create_deployment", createTwo, "environment-token"))
	listResponse := controlPlaneRequest(t, handler, http.MethodGet, "/v1/list_deployments?repository=repo-1", "", "environment-token")
	var listEnvelope struct {
		OK    bool `json:"ok"`
		Value struct {
			Items []deploymentRecord `json:"items"`
		} `json:"value"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listEnvelope); err != nil || !listEnvelope.OK {
		t.Fatalf("list response = %d %s, err %v", listResponse.Code, listResponse.Body.String(), err)
	}
	if len(listEnvelope.Value.Items) != 2 || listEnvelope.Value.Items[0].ID != second.ID {
		t.Errorf("list items = %+v", listEnvelope.Value.Items)
	}
	return first, second, changeOne, image
}

func exerciseDeploymentStatusAndRollback(
	t *testing.T,
	handler http.Handler,
	kubeClient client.Client,
	first deploymentRecord,
	second deploymentRecord,
) {
	t.Helper()
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "lake",
			Annotations: map[string]string{
				deploymentIDAnnotation: second.ID,
			},
		},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     "prod",
			Repository:  "repo-1",
			Environment: "production",
			Kind:        oramav1alpha1.LakeEnvironmentKindNamed,
			Change:      second.Change,
			Image:       second.Image,
			Port:        deploymentControlPlaneDefaultPort,
		},
		Status: oramav1alpha1.LakeEnvironmentStatus{
			Phase:        oramav1alpha1.LakeEnvironmentPhaseReady,
			URL:          "https://production.example.test",
			DeploymentID: second.ID,
			Conditions: []metav1.Condition{{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "DeploymentReady",
				Message: "all 1 replicas ready",
			}},
		},
	}
	if err := kubeClient.Create(context.Background(), environment); err != nil {
		t.Fatal(err)
	}
	statusResponse := controlPlaneRequest(t, handler, http.MethodGet, "/v1/deployment_status?repository=repo-1&environment=production", "", "environment-token")
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"ready":true`) || !strings.Contains(statusResponse.Body.String(), `"phase":"Ready"`) {
		t.Errorf("status response = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	rollback := fmtJSON(map[string]string{"repository": "repo-1", "environment": "production", "to": first.ID})
	rolledBack := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/rollback_deployment", rollback, "environment-token"))
	if rolledBack.RollbackOf != first.ID || rolledBack.Change != first.Change || rolledBack.ID == first.ID {
		t.Errorf("rollback = %+v", rolledBack)
	}
}

func exerciseDeploymentMCPAndRefusals(
	t *testing.T,
	handler http.Handler,
	double *contractSQLDouble,
	first deploymentRecord,
	changeOne string,
	image string,
) {
	t.Helper()
	mcpList := controlPlaneRequest(t, handler, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, "environment-token")
	for _, name := range []string{"create_deployment", "list_deployments", "rollback_deployment", "deployment_status"} {
		if !strings.Contains(mcpList.Body.String(), `"name":"`+name+`"`) {
			t.Errorf("MCP tools/list missing %s: %s", name, mcpList.Body.String())
		}
	}
	mcpCall := controlPlaneRequest(t, handler, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_deployments","arguments":{"repository":"repo-1"}}}`, "environment-token")
	if !strings.Contains(mcpCall.Body.String(), `"structuredContent"`) || !strings.Contains(mcpCall.Body.String(), first.ID) {
		t.Errorf("MCP tools/call = %s", mcpCall.Body.String())
	}

	unknownSecret := fmtJSON(map[string]any{
		"repository":  "repo-2",
		"environment": "production",
		"change":      changeOne,
		"image":       image,
		"secrets":     []string{"secret://unknown"},
	})
	refused := controlPlaneRequest(t, handler, http.MethodPost, "/v1/create_deployment", unknownSecret, "environment-token")
	if refused.Code != http.StatusNotFound || !strings.Contains(refused.Body.String(), "sealed-secret reference") || !strings.Contains(refused.Body.String(), `"path":"/secrets/0"`) {
		t.Errorf("unknown secret response = %d %s", refused.Code, refused.Body.String())
	}
	_, desiredBeforeDenied, auditsBeforeDenied := double.counts()
	denied := controlPlaneRequest(t, handler, http.MethodGet, "/v1/list_deployments?repository=repo-1", "", "wrong-token")
	if denied.Code != http.StatusUnauthorized {
		t.Errorf("denied response = %d %s", denied.Code, denied.Body.String())
	}
	_, desiredAfterDenied, auditsAfterDenied := double.counts()
	if desiredAfterDenied != desiredBeforeDenied || auditsAfterDenied != auditsBeforeDenied {
		t.Error("denied request changed Zetta state")
	}
	if double.authFailures != 0 {
		t.Errorf("Zetta auth failures = %d", double.authFailures)
	}
}

func fmtJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestDeploymentControlPlaneEnvtestFourOperations(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	testEnvironment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := deploymentControlPlaneScheme(t)
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake"}}); err != nil {
		t.Fatal(err)
	}
	for _, object := range deploymentControlPlaneObjects(false) {
		object.SetUID(types.UID(""))
		if err := kubeClient.Create(ctx, object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	double := newContractSQLDouble(t)
	defer double.close()
	server := &DeploymentControlPlane{Client: kubeClient, APIReader: kubeClient, ZettaURL: double.server.URL}
	handler := server.Handler()
	change := "blake3:" + strings.Repeat("3", 64)
	image := "registry.example.test/app@sha256:" + strings.Repeat("b", 64)
	create := fmtJSON(map[string]string{"repository": "repo-envtest", "environment": "production", "change": change, "image": image})
	created := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/create_deployment", create, "environment-token"))
	listed := controlPlaneRequest(t, handler, http.MethodGet, "/v1/list_deployments?repository=repo-envtest", "", "environment-token")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), created.ID) {
		t.Fatalf("envtest list = %d %s", listed.Code, listed.Body.String())
	}
	status := controlPlaneRequest(t, handler, http.MethodGet, "/v1/deployment_status?repository=repo-envtest&environment=production", "", "environment-token")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"phase":"Progressing"`) {
		t.Fatalf("envtest status = %d %s", status.Code, status.Body.String())
	}
	rollback := fmtJSON(map[string]string{"repository": "repo-envtest", "environment": "production", "to": created.ID})
	rolledBack := decodeDeploymentResponse(t, controlPlaneRequest(t, handler, http.MethodPost, "/v1/rollback_deployment", rollback, "environment-token"))
	if rolledBack.RollbackOf != created.ID {
		t.Errorf("envtest rollback = %+v", rolledBack)
	}
}
