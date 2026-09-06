/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const (
	defaultDeploymentSyncInterval = 15 * time.Second
	idleDeploymentSyncInterval    = 60 * time.Second
	deploymentRecordTypeDesired   = "desired"
	deploymentRecordTypeStatus    = "status"
	deploymentRecordLabel         = "orama.cloud/deployment-id"
)

var digestReferencePattern = regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`)

type zettaDeploymentEvent struct {
	DeploymentID string
	RecordType   string
	Lake         string
	Repository   string
	Environment  string
	Kind         string
	Change       string
	Image        string
	Replicas     int32
	Port         int32
	Env          []oramav1alpha1.LakeEnvironmentVariable
	SecretRefs   []string
	Host         string
	Withdrawn    bool
	ChangeClosed bool
	Phase        string
	URL          string
	Message      string
	UpdatedAt    string
	ParseError   string
}

type zettaDeploymentState struct {
	Desired *zettaDeploymentEvent
	Status  *zettaDeploymentEvent
}

// reconcileZettaDeployments pulls desired deployment events from the operator's own Zetta control
// plane and materializes local rows into LakeEnvironment CRs. Errors are best-effort: the Zetta HTTP
// surface is known to flap, and the periodic poll retries without destabilizing the Zetta workload.
func (r *OramaZettaClusterReconciler) reconcileZettaDeployments(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientService string) {
	log := logf.FromContext(ctx)
	token, err := r.zettaAdminToken(ctx, z)
	if err != nil {
		log.V(1).Info("deployment sync deferred — admin token not ready", "err", err.Error())
		return
	}
	base := r.zettaBaseURL(z, clientService)
	for _, lake := range r.localDeploymentLakes(ctx, z.Namespace) {
		rows, err := zettaSQLQuery(ctx, base, token, "orama_control", deploymentSelectSQL(lake.Name))
		if err != nil {
			log.V(1).Info("deployment sync deferred — Zetta query failed", "lake", lake.Name, "err", err.Error())
			continue
		}
		r.materializeZettaDeploymentRows(ctx, z, base, token, rows)
	}
}

func (r *OramaZettaClusterReconciler) materializeZettaDeploymentRows(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, base, token string, rows []map[string]string) {
	log := logf.FromContext(ctx)
	states := reduceDeploymentRows(rows)
	for deploymentID, state := range states {
		if state.Desired == nil {
			continue
		}
		if err := r.reconcileZettaDeployment(ctx, z, base, token, deploymentID, state); err != nil {
			log.Error(err, "deployment sync deferred", "deploymentId", deploymentID)
		}
	}
}

func deploymentSelectSQL(lake string) string {
	return "SELECT deployment_id, record_type, lake, repository, environment, kind, change_id, image, " +
		"replicas, port, env_json, secret_refs_json, host, withdrawn, change_closed, phase, url, " +
		"message, updated_at FROM deployments WHERE lake = " + deploymentSQLQuote(lake)
}

func reduceDeploymentRows(rows []map[string]string) map[string]*zettaDeploymentState {
	states := make(map[string]*zettaDeploymentState)
	for _, row := range rows {
		event, err := parseDeploymentRow(row)
		if err != nil || event.DeploymentID == "" {
			continue
		}
		state := states[event.DeploymentID]
		if state == nil {
			state = &zettaDeploymentState{}
			states[event.DeploymentID] = state
		}
		if event.RecordType == deploymentRecordTypeStatus {
			if deploymentEventIsNewer(state.Status, event) {
				state.Status = event
			}
			continue
		}
		if deploymentEventIsNewer(state.Desired, event) {
			state.Desired = event
		}
	}
	return states
}

func deploymentEventIsNewer(current, candidate *zettaDeploymentEvent) bool {
	if current == nil {
		return true
	}
	currentTime, currentErr := time.Parse(time.RFC3339Nano, current.UpdatedAt)
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate.UpdatedAt)
	if currentErr == nil && candidateErr == nil {
		if !candidateTime.Equal(currentTime) {
			return candidateTime.After(currentTime)
		}
	} else if candidate.UpdatedAt != current.UpdatedAt {
		return candidate.UpdatedAt > current.UpdatedAt
	}
	currentJSON, _ := json.Marshal(current)
	candidateJSON, _ := json.Marshal(candidate)
	return string(candidateJSON) > string(currentJSON)
}

func parseDeploymentRow(row map[string]string) (*zettaDeploymentEvent, error) {
	event := &zettaDeploymentEvent{
		DeploymentID: row["deployment_id"],
		RecordType:   row["record_type"],
		Lake:         row["lake"],
		Repository:   row["repository"],
		Environment:  row["environment"],
		Kind:         row["kind"],
		Change:       row["change_id"],
		Image:        row["image"],
		Host:         row["host"],
		Withdrawn:    parseDeploymentBool(row["withdrawn"]),
		ChangeClosed: parseDeploymentBool(row["change_closed"]),
		Phase:        row["phase"],
		URL:          row["url"],
		Message:      row["message"],
		UpdatedAt:    row["updated_at"],
	}
	if event.RecordType == "" {
		event.RecordType = deploymentRecordTypeDesired
	}
	if event.RecordType == deploymentRecordTypeStatus {
		return event, nil
	}
	replicas, err := parseDeploymentInt(row["replicas"], 1)
	if err != nil {
		event.ParseError = fmt.Sprintf("replicas: %v", err)
	}
	port, err := parseDeploymentInt(row["port"], 0)
	if err != nil {
		event.ParseError = fmt.Sprintf("port: %v", err)
	}
	event.Replicas = replicas
	event.Port = port
	if raw := row["env_json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &event.Env); err != nil {
			event.ParseError = fmt.Sprintf("env_json: %v", err)
		}
	}
	if len(event.Env) == 0 {
		event.Env = nil
	}
	if raw := row["secret_refs_json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &event.SecretRefs); err != nil {
			event.ParseError = fmt.Sprintf("secret_refs_json: %v", err)
		}
	}
	if len(event.SecretRefs) == 0 {
		event.SecretRefs = nil
	}
	return event, nil
}

func parseDeploymentInt(value string, defaultValue int32) (int32, error) {
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(parsed), nil
}

func parseDeploymentBool(value string) bool {
	parsed, _ := strconv.ParseBool(value)
	return parsed
}

func validateDeploymentEvent(event *zettaDeploymentEvent) error {
	switch {
	case event.ParseError != "":
		return fmt.Errorf("%s", event.ParseError)
	case event.DeploymentID == "":
		return fmt.Errorf("deployment_id is required")
	case event.Lake == "":
		return fmt.Errorf("lake is required")
	case event.Repository == "":
		return fmt.Errorf("repository is required")
	case event.Environment == "":
		return fmt.Errorf("environment is required")
	case event.Kind != oramav1alpha1.LakeEnvironmentKindNamed && event.Kind != oramav1alpha1.LakeEnvironmentKindPreview:
		return fmt.Errorf("kind must be named or preview")
	case event.Change == "":
		return fmt.Errorf("change_id is required")
	case !digestReferencePattern.MatchString(event.Image):
		return fmt.Errorf("image must be an immutable sha256 digest reference")
	case event.Replicas < 1:
		return fmt.Errorf("replicas must be at least 1")
	case event.Port < 1 || event.Port > 65535:
		return fmt.Errorf("port must be between 1 and 65535")
	default:
		return nil
	}
}

func (r *OramaZettaClusterReconciler) reconcileZettaDeployment(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, base, token, deploymentID string, state *zettaDeploymentState) error {
	desired := state.Desired
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: desired.Lake}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	lakeNamespace := lake.Spec.Namespace
	if lakeNamespace == "" {
		lakeNamespace = "lake-dev"
	}
	if lakeNamespace != z.Namespace {
		return nil
	}

	if err := validateDeploymentEvent(desired); err != nil {
		return r.writeDeploymentTransition(ctx, base, token, deploymentID, desired.Lake, state.Status, oramav1alpha1.LakeEnvironmentPhaseDegraded, "", err.Error())
	}
	name := deploymentEnvironmentName(desired)
	key := client.ObjectKey{Namespace: lakeNamespace, Name: name}
	if deploymentShouldTearDown(desired) {
		message := deploymentTeardownMessage(desired)
		if err := r.writeDeploymentTransition(ctx, base, token, deploymentID, desired.Lake, state.Status, oramav1alpha1.LakeEnvironmentPhaseTornDown, "", message); err != nil {
			return err
		}
		environment := &oramav1alpha1.LakeEnvironment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lakeNamespace}}
		if err := r.Delete(ctx, environment); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	environment := &oramav1alpha1.LakeEnvironment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lakeNamespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, environment, func() error {
		if environment.Annotations == nil {
			environment.Annotations = map[string]string{}
		}
		environment.Annotations[deploymentIDAnnotation] = deploymentID
		if environment.Labels == nil {
			environment.Labels = map[string]string{}
		}
		environment.Labels[deploymentRecordLabel] = deploymentIDLabelValue(deploymentID)
		environment.Spec = oramav1alpha1.LakeEnvironmentSpec{
			LakeRef:     desired.Lake,
			Repository:  desired.Repository,
			Environment: desired.Environment,
			Kind:        desired.Kind,
			Change:      desired.Change,
			Image:       desired.Image,
			Replicas:    desired.Replicas,
			Port:        desired.Port,
			Env:         desired.Env,
			SecretRefs:  desired.SecretRefs,
			Host:        desired.Host,
		}
		return controllerutil.SetControllerReference(&lake, environment, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.Get(ctx, key, environment); err != nil {
		return err
	}
	if environment.Status.Phase == "" {
		return nil
	}
	return r.writeDeploymentTransition(ctx, base, token, deploymentID, desired.Lake, state.Status, environment.Status.Phase, environment.Status.URL, lakeEnvironmentStatusMessage(environment))
}

func deploymentShouldTearDown(event *zettaDeploymentEvent) bool {
	return event.Withdrawn || event.Kind == oramav1alpha1.LakeEnvironmentKindPreview && event.ChangeClosed
}

func deploymentTeardownMessage(event *zettaDeploymentEvent) string {
	if event.Withdrawn {
		return "deployment withdrawn"
	}
	return "preview change closed"
}

func deploymentEnvironmentName(event *zettaDeploymentEvent) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(event.DeploymentID)))[:10]
	prefix := dnsLabel(event.Environment)
	maxPrefixLength := 63 - len(hash) - 1
	if len(prefix) > maxPrefixLength {
		prefix = strings.Trim(prefix[:maxPrefixLength], "-")
	}
	if prefix == "" {
		prefix = "deployment"
	}
	return prefix + "-" + hash
}

func deploymentIDLabelValue(deploymentID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(deploymentID)))[:16]
}

func lakeEnvironmentStatusMessage(environment *oramav1alpha1.LakeEnvironment) string {
	condition := apimetaFindStatusCondition(environment.Status.Conditions, "Ready")
	if condition == nil {
		return ""
	}
	return condition.Message
}

func apimetaFindStatusCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func (r *OramaZettaClusterReconciler) writeDeploymentTransition(ctx context.Context, base, token, deploymentID, lake string, previous *zettaDeploymentEvent, phase, url, message string) error {
	if previous != nil && previous.Phase == phase && previous.URL == url && previous.Message == message {
		return nil
	}
	statement := insertDeploymentStatusSQL(deploymentID, lake, phase, url, message, time.Now().UTC().Format(time.RFC3339Nano))
	return zettaSQL(ctx, base, token, "orama_control", statement)
}

func insertDeploymentStatusSQL(deploymentID, lake, phase, url, message, updatedAt string) string {
	return fmt.Sprintf(
		"INSERT INTO deployments (deployment_id, record_type, lake, phase, url, message, updated_at) VALUES (%s, %s, %s, %s, %s, %s, %s)",
		deploymentSQLQuote(deploymentID),
		deploymentSQLQuote(deploymentRecordTypeStatus),
		deploymentSQLQuote(lake),
		deploymentSQLQuote(phase),
		deploymentSQLQuote(url),
		deploymentSQLQuote(message),
		deploymentSQLQuote(updatedAt),
	)
}

func deploymentSQLQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (r *OramaZettaClusterReconciler) localDeploymentLakes(ctx context.Context, namespace string) []oramav1alpha1.Lake {
	var lakes oramav1alpha1.LakeList
	if err := r.List(ctx, &lakes); err != nil {
		return nil
	}
	local := make([]oramav1alpha1.Lake, 0, len(lakes.Items))
	for i := range lakes.Items {
		lakeNamespace := lakes.Items[i].Spec.Namespace
		if lakeNamespace == "" {
			lakeNamespace = "lake-dev"
		}
		if lakeNamespace == namespace {
			local = append(local, lakes.Items[i])
		}
	}
	return local
}

func (r *OramaZettaClusterReconciler) deploymentSyncInterval(ctx context.Context, namespace string) time.Duration {
	var lakes oramav1alpha1.LakeList
	if err := r.List(ctx, &lakes); err != nil {
		return defaultDeploymentSyncInterval
	}
	interval := idleDeploymentSyncInterval
	found := false
	for i := range lakes.Items {
		lake := &lakes.Items[i]
		lakeNamespace := lake.Spec.Namespace
		if lakeNamespace == "" {
			lakeNamespace = "lake-dev"
		}
		if lakeNamespace != namespace {
			continue
		}
		candidate := lake.Spec.DeploySync.Interval.Duration
		if candidate <= 0 {
			candidate = defaultDeploymentSyncInterval
		}
		if candidate < time.Second {
			candidate = time.Second
		}
		if !found || candidate < interval {
			interval = candidate
		}
		found = true
	}
	return interval
}
