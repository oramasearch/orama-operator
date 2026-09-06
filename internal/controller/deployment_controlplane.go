/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
	controlplanecontract "github.com/oramasearch/orama-cloud-v1-operator/docs/control-plane"
)

const (
	deploymentControlPlaneDefaultPort int32 = 8080
	deploymentRecordTable                   = "deployment_records"
	deploymentMCPProtocol                   = "2025-03-26"
	deploymentRequestLimit            int64 = 1 << 20
)

var changeDigestPattern = regexp.MustCompile(`^[a-z0-9_+-]+:[0-9a-f]{64}$`)

// DeploymentControlPlane serves liquid's deployment REST and MCP contract.
type DeploymentControlPlane struct {
	Client      client.Client
	APIReader   client.Reader
	BindAddress string
	ZettaURL    string
	Now         func() time.Time
	Log         logr.Logger
}

type deploymentTarget struct {
	Lake       oramav1alpha1.Lake
	Zetta      oramav1alpha1.OramaZettaCluster
	BaseURL    string
	ZettaToken string
}

type deploymentRecord struct {
	ID          string   `json:"id"`
	Repository  string   `json:"repository"`
	Environment string   `json:"environment"`
	Change      string   `json:"change"`
	Image       string   `json:"image"`
	Result      string   `json:"result,omitempty"`
	RollbackOf  string   `json:"rollback_of,omitempty"`
	Secrets     []string `json:"-"`
	CreatedAt   string   `json:"-"`
}

type deployRequest struct {
	Repository  string   `json:"repository"`
	Environment string   `json:"environment"`
	Change      string   `json:"change"`
	Image       string   `json:"image"`
	Result      string   `json:"result"`
	Secrets     []string `json:"secrets"`
}

type rollbackRequest struct {
	Repository  string `json:"repository"`
	Environment string `json:"environment"`
	To          string `json:"to"`
}

type deploymentAudit struct {
	Repository   string `json:"repository,omitempty"`
	Environment  string `json:"environment,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
}

type capabilityError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Start runs the control-plane server with the manager lifecycle.
func (s *DeploymentControlPlane) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.BindAddress,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// NeedLeaderElection keeps the read/write API available on every manager replica. Zetta appends and
// deterministic deployment IDs make create requests safe across replicas.
func (s *DeploymentControlPlane) NeedLeaderElection() bool {
	return false
}

// Handler returns the complete deployment control-plane HTTP surface.
func (s *DeploymentControlPlane) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/openapi.json", s.serveOpenAPI)
	mux.HandleFunc("/v1/create_deployment", s.serveCreateDeployment)
	mux.HandleFunc("/v1/list_deployments", s.serveListDeployments)
	mux.HandleFunc("/v1/rollback_deployment", s.serveRollbackDeployment)
	mux.HandleFunc("/v1/deployment_status", s.serveDeploymentStatus)
	mux.HandleFunc("/mcp", s.serveMCP)
	return mux
}

func (s *DeploymentControlPlane) reader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

func (s *DeploymentControlPlane) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *DeploymentControlPlane) logger() logr.Logger {
	if s.Log.GetSink() == nil {
		return logr.Discard()
	}
	return s.Log
}

func (s *DeploymentControlPlane) serveOpenAPI(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeCapabilityError(writer, &capabilityError{Status: http.StatusMethodNotAllowed, Code: "invalid_input", Message: "method not allowed"})
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(controlplanecontract.OpenAPI)
}

func (s *DeploymentControlPlane) serveCreateDeployment(writer http.ResponseWriter, request *http.Request) {
	s.serveRESTOperation(writer, request, http.MethodPost, "create_deployment", request.Body)
}

func (s *DeploymentControlPlane) serveRollbackDeployment(writer http.ResponseWriter, request *http.Request) {
	s.serveRESTOperation(writer, request, http.MethodPost, "rollback_deployment", request.Body)
}

func (s *DeploymentControlPlane) serveListDeployments(writer http.ResponseWriter, request *http.Request) {
	input, _ := json.Marshal(map[string]string{"repository": request.URL.Query().Get("repository")})
	s.serveRESTOperation(writer, request, http.MethodGet, "list_deployments", strings.NewReader(string(input)))
}

func (s *DeploymentControlPlane) serveDeploymentStatus(writer http.ResponseWriter, request *http.Request) {
	input, _ := json.Marshal(map[string]string{
		"repository":  request.URL.Query().Get("repository"),
		"environment": request.URL.Query().Get("environment"),
	})
	s.serveRESTOperation(writer, request, http.MethodGet, "deployment_status", strings.NewReader(string(input)))
}

func (s *DeploymentControlPlane) serveRESTOperation(
	writer http.ResponseWriter,
	request *http.Request,
	method string,
	operation string,
	body io.Reader,
) {
	if request.Method != method {
		writer.Header().Set("Allow", method)
		writeCapabilityError(writer, &capabilityError{Status: http.StatusMethodNotAllowed, Code: "invalid_input", Message: "method not allowed"})
		return
	}
	target, authError := s.authenticate(request.Context(), request.Header.Get("Authorization"))
	if authError != nil {
		s.logger().Info("deployment control-plane audit", "operation", operation, "outcome", "denied", "reason", authError.Message)
		writeCapabilityError(writer, authError)
		return
	}
	input, readError := io.ReadAll(io.LimitReader(body, deploymentRequestLimit+1))
	if readError != nil {
		s.writeEarlyOperationFailure(writer, request.Context(), target, operation, &capabilityError{Status: http.StatusBadRequest, Code: "invalid_input", Message: "request body could not be read"})
		return
	}
	if int64(len(input)) > deploymentRequestLimit {
		s.writeEarlyOperationFailure(writer, request.Context(), target, operation, &capabilityError{Status: http.StatusRequestEntityTooLarge, Code: "invalid_input", Message: "request body is too large"})
		return
	}
	value, operationAudit, operationError := s.runOperation(request.Context(), target, operation, input)
	if operationError != nil {
		operationAudit.Outcome = "failure"
		operationAudit.Reason = operationError.Message
	} else {
		operationAudit.Outcome = "success"
	}
	if err := s.writeAudit(request.Context(), target, operation, operationAudit); err != nil {
		s.logger().Error(err, "deployment control-plane audit failed", "operation", operation, "lake", target.Lake.Name)
		writeCapabilityError(writer, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "audit entry could not be recorded"})
		return
	}
	if operationError != nil {
		writeCapabilityError(writer, operationError)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "value": value})
}

func (s *DeploymentControlPlane) writeEarlyOperationFailure(
	writer http.ResponseWriter,
	ctx context.Context,
	target *deploymentTarget,
	operation string,
	operationError *capabilityError,
) {
	audit := deploymentAudit{Outcome: "failure", Reason: operationError.Message}
	if err := s.writeAudit(ctx, target, operation, audit); err != nil {
		writeCapabilityError(writer, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "audit entry could not be recorded"})
		return
	}
	writeCapabilityError(writer, operationError)
}

func (s *DeploymentControlPlane) authenticate(ctx context.Context, authorization string) (*deploymentTarget, *capabilityError) {
	token, err := bearerToken(authorization)
	if err != nil {
		return nil, &capabilityError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: err.Error(), Path: "/authorization"}
	}
	var lakes oramav1alpha1.LakeList
	if err := s.reader().List(ctx, &lakes); err != nil {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "Lake credentials could not be read"}
	}
	sort.Slice(lakes.Items, func(i, j int) bool {
		return lakes.Items[i].Name < lakes.Items[j].Name
	})
	var authenticated *oramav1alpha1.Lake
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		var secret corev1.Secret
		key := client.ObjectKey{Namespace: lakeWorkloadNamespace(lake), Name: lakeAdminSecret(lake.Name)}
		if err := s.reader().Get(ctx, key, &secret); err != nil {
			continue
		}
		candidate := secret.Data["token"]
		if subtle.ConstantTimeCompare(candidate, []byte(token)) == 1 {
			authenticated = lake.DeepCopy()
			break
		}
	}
	if authenticated == nil {
		return nil, &capabilityError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "environment token was rejected", Path: "/authorization"}
	}
	namespace := lakeWorkloadNamespace(authenticated)
	var zettas oramav1alpha1.OramaZettaClusterList
	if err := s.reader().List(ctx, &zettas, client.InNamespace(namespace)); err != nil {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "deployment control plane could not be located"}
	}
	available := make([]oramav1alpha1.OramaZettaCluster, 0, len(zettas.Items))
	for i := range zettas.Items {
		zetta := &zettas.Items[i]
		if zetta.Spec.ProvisionOperatorSchema == nil || *zetta.Spec.ProvisionOperatorSchema {
			available = append(available, *zetta.DeepCopy())
		}
	}
	sort.Slice(available, func(i, j int) bool {
		return available[i].Name < available[j].Name
	})
	if len(available) != 1 {
		message := fmt.Sprintf("Lake %q requires exactly one operator-schema Zetta cluster; found %d", authenticated.Name, len(available))
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: message}
	}
	zetta := available[0]
	var zettaSecret corev1.Secret
	zettaKey := client.ObjectKey{Namespace: zetta.Namespace, Name: zetta.Name + "-zetta-admin"}
	if err := s.reader().Get(ctx, zettaKey, &zettaSecret); err != nil || len(zettaSecret.Data["token"]) == 0 {
		return nil, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "Zetta control-plane credential is not ready"}
	}
	return &deploymentTarget{
		Lake:       *authenticated,
		Zetta:      zetta,
		BaseURL:    s.zettaBaseURL(&zetta),
		ZettaToken: string(zettaSecret.Data["token"]),
	}, nil
}

func bearerToken(authorization string) (string, error) {
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", fmt.Errorf("a bearer environment token is required")
	}
	if strings.ContainsAny(parts[1], " \t\r\n") {
		return "", fmt.Errorf("the bearer environment token is malformed")
	}
	return parts[1], nil
}

func lakeWorkloadNamespace(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Namespace == "" {
		return "lake-dev"
	}
	return lake.Spec.Namespace
}

func (s *DeploymentControlPlane) zettaBaseURL(zetta *oramav1alpha1.OramaZettaCluster) string {
	if s.ZettaURL != "" {
		return strings.TrimRight(s.ZettaURL, "/")
	}
	if zetta.Spec.Host != "" {
		return "https://" + zetta.Spec.Host
	}
	_, _, service := zettaNames(zetta)
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, zetta.Namespace, zettaAPIPort)
}

func (s *DeploymentControlPlane) runOperation(
	ctx context.Context,
	target *deploymentTarget,
	operation string,
	input []byte,
) (any, deploymentAudit, *capabilityError) {
	switch operation {
	case "create_deployment":
		return s.createDeployment(ctx, target, input)
	case "list_deployments":
		return s.listDeployments(ctx, target, input)
	case "rollback_deployment":
		return s.rollbackDeployment(ctx, target, input)
	case "deployment_status":
		return s.deploymentStatus(ctx, target, input)
	default:
		return nil, deploymentAudit{}, &capabilityError{Status: http.StatusBadRequest, Code: "invalid_input", Message: "unknown deployment operation"}
	}
}

func decodeOperationInput(input []byte, output any) *capabilityError {
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return &capabilityError{Status: http.StatusBadRequest, Code: "invalid_input", Message: "invalid JSON input: " + err.Error()}
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return &capabilityError{Status: http.StatusBadRequest, Code: "invalid_input", Message: "request body must contain one JSON object"}
	}
	return nil
}

func (s *DeploymentControlPlane) createDeployment(
	ctx context.Context,
	target *deploymentTarget,
	input []byte,
) (any, deploymentAudit, *capabilityError) {
	var request deployRequest
	if err := decodeOperationInput(input, &request); err != nil {
		return nil, deploymentAudit{}, err
	}
	audit := deploymentAudit{Repository: request.Repository, Environment: request.Environment}
	if err := validateDeployRequest(&request); err != nil {
		return nil, audit, err
	}
	records, err := s.readDeploymentRecords(ctx, target)
	if err != nil {
		return nil, audit, unavailableError(err)
	}
	id := deploymentActionID(target.Lake.Name, &request)
	for i := range records {
		if records[i].ID == id {
			audit.DeploymentID = id
			return records[i], audit, nil
		}
	}
	if err := s.validateSecretReferences(ctx, target, request.Secrets); err != nil {
		return nil, audit, err
	}
	record := deploymentRecord{
		ID:          id,
		Repository:  request.Repository,
		Environment: request.Environment,
		Change:      request.Change,
		Image:       request.Image,
		Result:      request.Result,
		Secrets:     append([]string(nil), request.Secrets...),
		CreatedAt:   formatDeploymentTime(s.now()),
	}
	audit.DeploymentID = record.ID
	if err := s.appendDeployment(ctx, target, &record); err != nil {
		return nil, audit, unavailableError(err)
	}
	return record, audit, nil
}

func validateDeployRequest(request *deployRequest) *capabilityError {
	switch {
	case request.Repository == "":
		return invalidInput("repository is required", "/repository")
	case request.Environment == "":
		return invalidInput("environment is required", "/environment")
	case !changeDigestPattern.MatchString(request.Change):
		return invalidInput("change must be an algorithm-prefixed lowercase digest", "/change")
	case !digestReferencePattern.MatchString(request.Image):
		return invalidInput("image must be an immutable sha256 digest reference", "/image")
	case request.Result != "" && !changeDigestPattern.MatchString(request.Result):
		return invalidInput("result must be an algorithm-prefixed lowercase digest", "/result")
	default:
		return nil
	}
}

func deploymentActionID(lake string, request *deployRequest) string {
	value := strings.Join([]string{lake, request.Repository, request.Environment, request.Change, request.Image}, "\n")
	digest := sha256.Sum256([]byte(value))
	return "deployment-" + hex.EncodeToString(digest[:16])
}

func (s *DeploymentControlPlane) listDeployments(
	ctx context.Context,
	target *deploymentTarget,
	input []byte,
) (any, deploymentAudit, *capabilityError) {
	var request struct {
		Repository string `json:"repository"`
	}
	if err := decodeOperationInput(input, &request); err != nil {
		return nil, deploymentAudit{}, err
	}
	audit := deploymentAudit{Repository: request.Repository}
	if request.Repository == "" {
		return nil, audit, invalidInput("repository is required", "/repository")
	}
	records, err := s.readDeploymentRecords(ctx, target)
	if err != nil {
		return nil, audit, unavailableError(err)
	}
	items := make([]deploymentRecord, 0)
	for i := range records {
		if records[i].Repository == request.Repository {
			items = append(items, records[i])
		}
	}
	return map[string]any{"items": items}, audit, nil
}

func (s *DeploymentControlPlane) rollbackDeployment(
	ctx context.Context,
	target *deploymentTarget,
	input []byte,
) (any, deploymentAudit, *capabilityError) {
	var request rollbackRequest
	if err := decodeOperationInput(input, &request); err != nil {
		return nil, deploymentAudit{}, err
	}
	audit := deploymentAudit{Repository: request.Repository, Environment: request.Environment}
	if request.Repository == "" {
		return nil, audit, invalidInput("repository is required", "/repository")
	}
	if request.Environment == "" {
		return nil, audit, invalidInput("environment is required", "/environment")
	}
	if request.To == "" {
		return nil, audit, invalidInput("rollback target is required", "/to")
	}
	records, err := s.readDeploymentRecords(ctx, target)
	if err != nil {
		return nil, audit, unavailableError(err)
	}
	var current *deploymentRecord
	var prior *deploymentRecord
	for i := range records {
		record := &records[i]
		if record.Repository != request.Repository || record.Environment != request.Environment {
			continue
		}
		if current == nil {
			current = record
		}
		if prior == nil && (record.ID == request.To || record.Change == request.To) {
			prior = record
		}
	}
	if prior == nil {
		return nil, audit, &capabilityError{Status: http.StatusNotFound, Code: "not_found", Message: "rollback target was not deployed", Path: "/to"}
	}
	if current != nil && current.RollbackOf == prior.ID {
		audit.DeploymentID = current.ID
		return *current, audit, nil
	}
	if err := s.validateSecretReferences(ctx, target, prior.Secrets); err != nil {
		return nil, audit, err
	}
	currentID := "none"
	if current != nil {
		currentID = current.ID
	}
	idInput := strings.Join([]string{target.Lake.Name, request.Repository, request.Environment, currentID, prior.ID}, "\n")
	idDigest := sha256.Sum256([]byte(idInput))
	record := *prior
	record.ID = "deployment-" + hex.EncodeToString(idDigest[:16])
	record.RollbackOf = prior.ID
	record.CreatedAt = formatDeploymentTime(s.now())
	audit.DeploymentID = record.ID
	if err := s.appendDeployment(ctx, target, &record); err != nil {
		return nil, audit, unavailableError(err)
	}
	return record, audit, nil
}

func (s *DeploymentControlPlane) deploymentStatus(
	ctx context.Context,
	target *deploymentTarget,
	input []byte,
) (any, deploymentAudit, *capabilityError) {
	var request struct {
		Repository  string `json:"repository"`
		Environment string `json:"environment"`
	}
	if err := decodeOperationInput(input, &request); err != nil {
		return nil, deploymentAudit{}, err
	}
	audit := deploymentAudit{Repository: request.Repository, Environment: request.Environment}
	if request.Repository == "" {
		return nil, audit, invalidInput("repository is required", "/repository")
	}
	if request.Environment == "" {
		return nil, audit, invalidInput("environment is required", "/environment")
	}
	records, err := s.readDeploymentRecords(ctx, target)
	if err != nil {
		return nil, audit, unavailableError(err)
	}
	var current *deploymentRecord
	for i := range records {
		if records[i].Repository == request.Repository && records[i].Environment == request.Environment {
			current = &records[i]
			break
		}
	}
	if current == nil {
		return nil, audit, &capabilityError{Status: http.StatusNotFound, Code: "not_found", Message: "environment was not deployed"}
	}
	audit.DeploymentID = current.ID
	var environments oramav1alpha1.LakeEnvironmentList
	if err := s.reader().List(ctx, &environments, client.InNamespace(lakeWorkloadNamespace(&target.Lake))); err != nil {
		return nil, audit, &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "environment status could not be read"}
	}
	for i := range environments.Items {
		environment := &environments.Items[i]
		if environment.Spec.LakeRef != target.Lake.Name ||
			environment.Spec.Repository != request.Repository ||
			environment.Spec.Environment != request.Environment ||
			environment.Annotations[deploymentIDAnnotation] != current.ID {
			continue
		}
		if environment.Status.DeploymentID != current.ID {
			return map[string]any{
				"phase":   oramav1alpha1.LakeEnvironmentPhaseProgressing,
				"url":     environment.Status.URL,
				"ready":   false,
				"message": "waiting for the latest deployment reconciliation",
			}, audit, nil
		}
		condition := apimeta.FindStatusCondition(environment.Status.Conditions, "Ready")
		message := ""
		if condition != nil {
			message = condition.Message
		}
		phase := environment.Status.Phase
		if phase == "" {
			phase = oramav1alpha1.LakeEnvironmentPhaseProgressing
		}
		return map[string]any{
			"phase":   phase,
			"url":     environment.Status.URL,
			"ready":   phase == oramav1alpha1.LakeEnvironmentPhaseReady,
			"message": message,
		}, audit, nil
	}
	return map[string]any{
		"phase":   oramav1alpha1.LakeEnvironmentPhaseProgressing,
		"url":     "",
		"ready":   false,
		"message": "waiting for the operator to materialize the deployment",
	}, audit, nil
}

func (s *DeploymentControlPlane) validateSecretReferences(
	ctx context.Context,
	target *deploymentTarget,
	references []string,
) *capabilityError {
	for index, reference := range references {
		name, err := sealedSecretReferenceName(reference)
		path := fmt.Sprintf("/secrets/%d", index)
		if err != nil {
			return invalidInput(err.Error(), path)
		}
		var sealed oramav1alpha1.LakeSealedSecret
		key := client.ObjectKey{Namespace: lakeWorkloadNamespace(&target.Lake), Name: name}
		if err := s.reader().Get(ctx, key, &sealed); err != nil {
			if apierrors.IsNotFound(err) {
				return &capabilityError{Status: http.StatusNotFound, Code: "not_found", Message: fmt.Sprintf("sealed-secret reference %q was not found", reference), Path: path}
			}
			return &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "sealed-secret reference could not be read", Path: path}
		}
		if sealed.Spec.LakeRef.Name != target.Lake.Name {
			return &capabilityError{Status: http.StatusForbidden, Code: "unauthorized", Message: fmt.Sprintf("sealed-secret reference %q belongs to another Lake", reference), Path: path}
		}
		ready := apimeta.FindStatusCondition(sealed.Status.Conditions, "Ready")
		if sealed.Status.Phase != "Ready" || sealed.Status.ObservedGeneration != sealed.Generation || ready == nil || ready.Status != metav1.ConditionTrue {
			return &capabilityError{Status: http.StatusConflict, Code: "conflict", Message: fmt.Sprintf("sealed-secret reference %q is not ready", reference), Path: path}
		}
		var produced corev1.Secret
		producedKey := client.ObjectKey{Namespace: sealed.Namespace, Name: sealed.Spec.Template.Name}
		if err := s.reader().Get(ctx, producedKey, &produced); err != nil || !metav1.IsControlledBy(&produced, &sealed) {
			return &capabilityError{Status: http.StatusConflict, Code: "conflict", Message: fmt.Sprintf("sealed-secret reference %q has no delivered Secret", reference), Path: path}
		}
	}
	return nil
}

func (s *DeploymentControlPlane) appendDeployment(ctx context.Context, target *deploymentTarget, record *deploymentRecord) error {
	if err := zettaSQL(ctx, target.BaseURL, target.ZettaToken, "orama_control", insertDesiredDeploymentSQL(target.Lake.Name, record)); err != nil {
		return err
	}
	return zettaSQL(ctx, target.BaseURL, target.ZettaToken, "orama_control", insertDeploymentRecordSQL(target.Lake.Name, record))
}

func insertDesiredDeploymentSQL(lake string, record *deploymentRecord) string {
	secretJSON, _ := json.Marshal(record.Secrets)
	kind := oramav1alpha1.LakeEnvironmentKindNamed
	if record.Environment == record.Change {
		kind = oramav1alpha1.LakeEnvironmentKindPreview
	}
	return fmt.Sprintf(
		"INSERT INTO deployments (deployment_id, record_type, lake, repository, environment, kind, change_id, image, replicas, port, env_json, secret_refs_json, host, withdrawn, change_closed, updated_at) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
		deploymentSQLQuote(record.ID),
		deploymentSQLQuote(deploymentRecordTypeDesired),
		deploymentSQLQuote(lake),
		deploymentSQLQuote(record.Repository),
		deploymentSQLQuote(record.Environment),
		deploymentSQLQuote(kind),
		deploymentSQLQuote(record.Change),
		deploymentSQLQuote(record.Image),
		deploymentSQLQuote("1"),
		deploymentSQLQuote(fmt.Sprintf("%d", deploymentControlPlaneDefaultPort)),
		deploymentSQLQuote("[]"),
		deploymentSQLQuote(string(secretJSON)),
		deploymentSQLQuote(""),
		deploymentSQLQuote("false"),
		deploymentSQLQuote("false"),
		deploymentSQLQuote(record.CreatedAt),
	)
}

func insertDeploymentRecordSQL(lake string, record *deploymentRecord) string {
	secretJSON, _ := json.Marshal(record.Secrets)
	return fmt.Sprintf(
		"INSERT INTO %s (deployment_id, lake, repository, environment, change_id, image, result_id, rollback_of, secret_refs_json, created_at) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
		deploymentRecordTable,
		deploymentSQLQuote(record.ID),
		deploymentSQLQuote(lake),
		deploymentSQLQuote(record.Repository),
		deploymentSQLQuote(record.Environment),
		deploymentSQLQuote(record.Change),
		deploymentSQLQuote(record.Image),
		deploymentSQLQuote(record.Result),
		deploymentSQLQuote(record.RollbackOf),
		deploymentSQLQuote(string(secretJSON)),
		deploymentSQLQuote(record.CreatedAt),
	)
}

func (s *DeploymentControlPlane) readDeploymentRecords(ctx context.Context, target *deploymentTarget) ([]deploymentRecord, error) {
	statement := "SELECT deployment_id, lake, repository, environment, change_id, image, result_id, rollback_of, secret_refs_json, created_at FROM " +
		deploymentRecordTable + " WHERE lake = " + deploymentSQLQuote(target.Lake.Name)
	rows, err := zettaSQLQuery(ctx, target.BaseURL, target.ZettaToken, "orama_control", statement)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]deploymentRecord)
	for _, row := range rows {
		record := deploymentRecord{
			ID:          row["deployment_id"],
			Repository:  row["repository"],
			Environment: row["environment"],
			Change:      row["change_id"],
			Image:       row["image"],
			Result:      row["result_id"],
			RollbackOf:  row["rollback_of"],
			CreatedAt:   row["created_at"],
		}
		if raw := row["secret_refs_json"]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &record.Secrets); err != nil {
				continue
			}
		}
		if record.ID == "" {
			continue
		}
		current, found := byID[record.ID]
		if !found || record.CreatedAt > current.CreatedAt {
			byID[record.ID] = record
		}
	}
	records := make([]deploymentRecord, 0, len(byID))
	for _, record := range byID {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt == records[j].CreatedAt {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt > records[j].CreatedAt
	})
	return records, nil
}

func (s *DeploymentControlPlane) writeAudit(
	ctx context.Context,
	target *deploymentTarget,
	operation string,
	audit deploymentAudit,
) error {
	detail, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	statement := fmt.Sprintf(
		"INSERT INTO reconcile_events (ts, cluster, action, detail) VALUES (%s, %s, %s, %s)",
		deploymentSQLQuote(formatDeploymentTime(s.now())),
		deploymentSQLQuote(target.Lake.Name),
		deploymentSQLQuote("deployment."+operation),
		deploymentSQLQuote(string(detail)),
	)
	return zettaSQL(ctx, target.BaseURL, target.ZettaToken, "orama_control", statement)
}

func formatDeploymentTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func invalidInput(message string, path string) *capabilityError {
	return &capabilityError{Status: http.StatusBadRequest, Code: "invalid_input", Message: message, Path: path}
}

func unavailableError(err error) *capabilityError {
	return &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "Zetta control plane is unavailable: " + err.Error()}
}

func writeCapabilityError(writer http.ResponseWriter, capabilityErr *capabilityError) {
	writeJSON(writer, capabilityErr.Status, map[string]any{
		"ok":    false,
		"error": capabilityErr,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (s *DeploymentControlPlane) serveMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeMCPError(writer, nil, -32600, "method not allowed")
		return
	}
	target, authError := s.authenticate(request.Context(), request.Header.Get("Authorization"))
	if authError != nil {
		s.logger().Info("deployment control-plane audit", "operation", "mcp", "outcome", "denied", "reason", authError.Message)
		writeCapabilityError(writer, authError)
		return
	}
	var message mcpRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, deploymentRequestLimit+1))
	if err := decoder.Decode(&message); err != nil || message.JSONRPC != "2.0" {
		writeMCPError(writer, message.ID, -32600, "invalid JSON-RPC request")
		return
	}
	switch message.Method {
	case "initialize":
		writeMCPResult(writer, message.ID, map[string]any{
			"protocolVersion": deploymentMCPProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "orama-operator", "version": "0.1.0"},
		})
	case "notifications/initialized":
		writer.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeMCPResult(writer, message.ID, map[string]any{"tools": deploymentMCPTools()})
	case "tools/call":
		var call mcpToolCall
		if err := json.Unmarshal(message.Params, &call); err != nil {
			writeMCPError(writer, message.ID, -32602, "invalid tool call")
			return
		}
		if !deploymentToolExists(call.Name) {
			writeMCPError(writer, message.ID, -32602, "unknown deployment tool")
			return
		}
		value, audit, operationError := s.runOperation(request.Context(), target, call.Name, call.Arguments)
		if operationError != nil {
			audit.Outcome = "failure"
			audit.Reason = operationError.Message
		} else {
			audit.Outcome = "success"
		}
		if err := s.writeAudit(request.Context(), target, call.Name, audit); err != nil {
			operationError = &capabilityError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "audit entry could not be recorded"}
		}
		if operationError != nil {
			envelope := map[string]any{"ok": false, "error": operationError}
			encoded, _ := json.Marshal(envelope)
			writeMCPResult(writer, message.ID, map[string]any{
				"content": []map[string]string{{"type": "text", "text": string(encoded)}},
				"isError": true,
			})
			return
		}
		envelope := map[string]any{"ok": true, "value": value}
		encoded, _ := json.Marshal(envelope)
		writeMCPResult(writer, message.ID, map[string]any{
			"content":           []map[string]string{{"type": "text", "text": string(encoded)}},
			"structuredContent": value,
			"isError":           false,
		})
	default:
		writeMCPError(writer, message.ID, -32601, "method not found")
	}
}

func deploymentToolExists(name string) bool {
	for _, tool := range deploymentMCPTools() {
		if tool["name"] == name {
			return true
		}
	}
	return false
}

func deploymentMCPTools() []map[string]any {
	stringSchema := func() map[string]any {
		return map[string]any{"type": "string"}
	}
	return []map[string]any{
		{
			"name":        "create_deployment",
			"description": "Create or reuse a digest-pinned Lake deployment.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repository", "environment", "change", "image"},
				"properties": map[string]any{
					"repository":  stringSchema(),
					"environment": stringSchema(),
					"change":      map[string]any{"type": "string", "pattern": "^[a-z0-9_+-]+:[0-9a-f]{64}$"},
					"image":       map[string]any{"type": "string", "pattern": ".+@sha256:[0-9a-fA-F]{64}$"},
					"result":      map[string]any{"type": "string", "pattern": "^[a-z0-9_+-]+:[0-9a-f]{64}$"},
					"secrets":     map[string]any{"type": "array", "items": stringSchema()},
				},
			},
		},
		{
			"name":        "list_deployments",
			"description": "List deployments for a repository in reverse creation order.",
			"inputSchema": map[string]any{
				"type":       "object",
				"required":   []string{"repository"},
				"properties": map[string]any{"repository": stringSchema()},
			},
		},
		{
			"name":        "rollback_deployment",
			"description": "Re-point an environment to a deployment or change used previously.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repository", "environment", "to"},
				"properties": map[string]any{
					"repository":  stringSchema(),
					"environment": stringSchema(),
					"to":          stringSchema(),
				},
			},
		},
		{
			"name":        "deployment_status",
			"description": "Read reconciled phase, readiness, message, and URL for an environment.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repository", "environment"},
				"properties": map[string]any{
					"repository":  stringSchema(),
					"environment": stringSchema(),
				},
			},
		},
	}
}

func writeMCPResult(writer http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(writer, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeMCPError(writer http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}
