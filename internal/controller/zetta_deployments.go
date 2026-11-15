/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	defaultDeploymentSyncInterval = 15 * time.Second
	idleDeploymentSyncInterval    = 60 * time.Second
	deploymentRecordTypeDesired   = "desired"
	deploymentRecordTypeStatus    = "status"
	deploymentRecordLabel         = "orama.cloud/deployment-id"
	// deploymentPhaseRequested is the phase of a desired row: asked for, not yet materialized.
	deploymentPhaseRequested = "Pending"
	// domainStatusDeleted answers a domain tombstone once the operator has removed the domain.
	domainStatusDeleted = "deleted"
	// zettaRowTimeLayout is lake's row time: UTC with exactly six fractional digits, so that the
	// string order lake sorts rows by is the time order (lake docs/api/platform.md).
	zettaRowTimeLayout = "2006-01-02T15:04:05.000000Z"
)

// The operator's orama_control columns are lake's contract (operator#560, Steve 2026-10-17: lake's
// format is the contract and the operator conforms). Each list is a copy of lake's
// include/lake/control/zetta_control.hpp; lake selects exactly these columns WHERE lake =
// LAKE_PUBLIC_HOST, and Zetta refuses a SELECT naming a column no row of the table carries
// (zetta#1218), so every row the operator writes carries every one of them.
var (
	lakeDeploymentColumns = []string{
		"operator", "lake", "repository", "environment", "kind", "change", "image", "deployment_id",
		"requested_by", "requested_at", "phase", "namespace", "url", "message", "updated_at",
		"ordering_token",
	}
	lakeEnvironmentEnvColumns = []string{
		"lake", "repository", "environment", "key", "value", "deleted", "set_by", "updated_at",
		"ordering_token",
	}
	lakeDomainColumns = []string{
		"lake", "repository", "environment", "host", "tls", "status", "message", "deleted",
		"requested_by", "updated_at", "ordering_token",
	}
	// operatorDeploymentColumns are the operator's own columns on the same rows: what a
	// LakeEnvironment needs that lake does not read. lake ignores them.
	operatorDeploymentColumns = []string{
		"record_type", "replicas", "port", "env_json", "secret_refs_json", "host", "withdrawn",
		"change_closed",
	}
)

var digestReferencePattern = regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`)

type zettaDeploymentEvent struct {
	DeploymentID  string
	RecordType    string
	Operator      string
	Lake          string
	Repository    string
	Environment   string
	Kind          string
	Change        string
	Image         string
	RequestedBy   string
	RequestedAt   string
	Replicas      int32
	Port          int32
	Env           []oramav1alpha1.LakeEnvironmentVariable
	SecretRefs    []string
	Host          string
	Withdrawn     bool
	ChangeClosed  bool
	Phase         string
	Namespace     string
	URL           string
	Message       string
	UpdatedAt     string
	OrderingToken string
	ParseError    string
	// raw is the row as read, the last tie-break of lake's row order.
	raw map[string]string
}

type zettaDeploymentState struct {
	Desired *zettaDeploymentEvent
	Status  *zettaDeploymentEvent
	// Latest is the newest row of either kind, in lake's order: the row lake shows. A status row
	// is written after it, so lake shows the status.
	Latest *zettaDeploymentEvent
}

// zettaPlatformRows is one Lake's three orama_control tables, as read.
type zettaPlatformRows struct {
	Deployments    []map[string]string
	EnvironmentEnv []map[string]string
	Domains        []map[string]string
}

// reconcileZettaDeployments pulls lake's platform rows from the operator's own Zetta control
// plane and materializes them into LakeEnvironment CRs. Errors are best-effort: the Zetta HTTP
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
		host := lakeRowHost(&lake)
		if host == "" {
			log.V(1).Info("deployment sync skipped — the Lake has no literal public host to name its rows", "lake", lake.Name)
			continue
		}
		rows, err := readZettaPlatformRows(ctx, base, token, host)
		if err != nil {
			log.V(1).Info("deployment sync deferred — Zetta query failed", "lake", lake.Name, "err", err.Error())
			continue
		}
		r.materializeZettaPlatformRows(ctx, &lake, host, base, token, rows)
	}
}

func readZettaPlatformRows(ctx context.Context, base, token, host string) (zettaPlatformRows, error) {
	var rows zettaPlatformRows
	var err error
	if rows.Deployments, err = zettaSQLQuery(ctx, base, token, "orama_control", deploymentSelectSQL(host)); err != nil {
		return rows, err
	}
	if rows.EnvironmentEnv, err = zettaSQLQuery(ctx, base, token, "orama_control", platformSelectSQL("environment_env", lakeEnvironmentEnvColumns, host)); err != nil {
		return rows, err
	}
	if rows.Domains, err = zettaSQLQuery(ctx, base, token, "orama_control", platformSelectSQL("domains", lakeDomainColumns, host)); err != nil {
		return rows, err
	}
	return rows, nil
}

// lakeRowHost is the value lake writes in every orama_control row's `lake` column: its
// LAKE_PUBLIC_HOST (lakePublicHost: spec.env's literal value, else spec.host, as lakeEnv gives it).
// A LAKE_PUBLIC_HOST taken from a Secret or ConfigMap cannot be known here, so that Lake has none.
func lakeRowHost(lake *oramav1alpha1.Lake) string {
	for _, variable := range lake.Spec.Env {
		if variable.Name == "LAKE_PUBLIC_HOST" && variable.Value == "" && variable.ValueFrom != nil {
			return ""
		}
	}
	return lakePublicHost(lake)
}

func (r *OramaZettaClusterReconciler) materializeZettaPlatformRows(ctx context.Context, lake *oramav1alpha1.Lake, host, base, token string, rows zettaPlatformRows) {
	log := logf.FromContext(ctx)
	env := latestEnvironmentEnv(rows.EnvironmentEnv)
	guard, err := lakeDomainGuardFor(ctx, r.Client, lake)
	if err != nil {
		// Without the guard no domain can be checked against the platform: apply nothing this poll.
		log.Error(err, "deployment sync deferred — the domain guard could not be read", "lake", lake.Name)
		return
	}
	// Only a verified claim's domain is routed (lake#949). Without the domain_claims table (a lake
	// schema before 0106) no claim is verified and every domain is refused. Any other failed read
	// changes no domain this poll: each environment keeps the domains it has and no status is
	// written, so a transient read never unroutes a verified host nor routes an unverified one.
	var domains lakeDomainPlan
	claims, err := r.domainClaims()(ctx, lake)
	switch {
	case errors.Is(err, errDomainClaimsTableMissing):
		log.Info("every domain refused: "+err.Error(), "lake", lake.Name)
		domains = planLakeDomains(lake, host, rows.Domains, guard, nil)
	case err != nil:
		log.Info("domain changes deferred — the domain claims could not be read", "lake", lake.Name, "err", err.Error())
		kept, keptErr := r.currentLakeDomains(ctx, lake)
		if keptErr != nil {
			log.Error(keptErr, "deployment sync deferred — the current domains could not be read", "lake", lake.Name)
			return
		}
		domains = lakeDomainPlan{active: kept}
	default:
		domains = planLakeDomains(lake, host, rows.Domains, guard, newLakeVerifiedDomains(claims))
	}
	operator := r.operatorID(ctx)
	for _, state := range latestDeploymentEnvironmentStates(reduceDeploymentRows(rows.Deployments)) {
		deploymentID := state.Desired.DeploymentID
		identity := platformEnvironmentKey(state.Desired.Repository, state.Desired.Environment)
		if err := r.reconcileZettaDeployment(ctx, lake, base, token, operator, state, env[identity], domains.active[identity]); err != nil {
			log.Error(err, "deployment sync deferred", "deploymentId", deploymentID)
		}
	}
	if err := r.writeDomainStatuses(ctx, lake, base, token, domains); err != nil {
		log.Error(err, "domain status sync deferred", "lake", lake.Name)
	}
}

func latestDeploymentEnvironmentStates(states map[string]*zettaDeploymentState) []*zettaDeploymentState {
	latest := make(map[string]*zettaDeploymentState)
	for deploymentID, state := range states {
		if state.Desired == nil {
			continue
		}
		key := strings.Join([]string{state.Desired.Lake, state.Desired.Repository, state.Desired.Environment}, "\n")
		if state.Desired.Lake == "" || state.Desired.Repository == "" || state.Desired.Environment == "" {
			key = "invalid\n" + deploymentID
		}
		current := latest[key]
		if current == nil || deploymentEventIsNewer(current.Desired, state.Desired) {
			latest[key] = state
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]*zettaDeploymentState, 0, len(keys))
	for _, key := range keys {
		result = append(result, latest[key])
	}
	return result
}

// deploymentSelectSQL reads one Lake's deployment rows: lake's columns, then the operator's own.
func deploymentSelectSQL(host string) string {
	return platformSelectSQL("deployments", append(append([]string(nil), lakeDeploymentColumns...), operatorDeploymentColumns...), host)
}

func platformSelectSQL(table string, columns []string, host string) string {
	return "SELECT " + strings.Join(columns, ", ") + " FROM " + table + " WHERE lake = " + deploymentSQLQuote(host)
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
		if deploymentEventIsNewer(state.Latest, event) {
			state.Latest = event
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

// lakeRowOrder is lake's platform_row_order (src/control/platform_records.cpp): the updated_at
// string with a second-resolution time widened to six zero digits, then ordering_token, then the
// row itself as the final tie-break.
func lakeRowOrder(updatedAt, orderingToken string, raw map[string]string) [3]string {
	if strings.HasSuffix(updatedAt, "Z") && !strings.Contains(updatedAt, ".") {
		updatedAt = strings.TrimSuffix(updatedAt, "Z") + ".000000Z"
	}
	canonical, _ := json.Marshal(raw)
	return [3]string{updatedAt, orderingToken, string(canonical)}
}

func lakeRowIsNewer(candidate, current [3]string) bool {
	for i := range candidate {
		if candidate[i] != current[i] {
			return candidate[i] > current[i]
		}
	}
	return false
}

func deploymentEventIsNewer(current, candidate *zettaDeploymentEvent) bool {
	if current == nil {
		return true
	}
	return lakeRowIsNewer(
		lakeRowOrder(candidate.UpdatedAt, candidate.OrderingToken, candidate.raw),
		lakeRowOrder(current.UpdatedAt, current.OrderingToken, current.raw),
	)
}

func parseDeploymentRow(row map[string]string) (*zettaDeploymentEvent, error) {
	event := &zettaDeploymentEvent{
		DeploymentID:  row["deployment_id"],
		RecordType:    row["record_type"],
		Operator:      row["operator"],
		Lake:          row["lake"],
		Repository:    row["repository"],
		Environment:   row["environment"],
		Kind:          row["kind"],
		Change:        row["change"],
		Image:         row["image"],
		RequestedBy:   row["requested_by"],
		RequestedAt:   row["requested_at"],
		Host:          row["host"],
		Withdrawn:     parseDeploymentBool(row["withdrawn"]),
		ChangeClosed:  parseDeploymentBool(row["change_closed"]),
		Phase:         row["phase"],
		Namespace:     row["namespace"],
		URL:           row["url"],
		Message:       row["message"],
		UpdatedAt:     row["updated_at"],
		OrderingToken: row["ordering_token"],
		raw:           row,
	}
	if event.RecordType == "" {
		event.RecordType = deploymentRecordTypeDesired
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
		return fmt.Errorf("change is required")
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

func (r *OramaZettaClusterReconciler) reconcileZettaDeployment(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	base, token, operator string,
	state *zettaDeploymentState,
	env []oramav1alpha1.LakeEnvironmentVariable,
	domains []oramav1alpha1.LakeEnvironmentDomain,
) error {
	desired := state.Desired
	deploymentID := desired.DeploymentID
	lakeNamespace := lakeWorkloadNamespace(lake)
	transition := func(phase, namespace, url, message string) error {
		return writeDeploymentTransition(ctx, base, token, operator, state, phase, namespace, url, message, time.Now())
	}

	if err := validateDeploymentEvent(desired); err != nil {
		return transition(oramav1alpha1.LakeEnvironmentPhaseDegraded, "", "", err.Error())
	}
	name := deploymentEnvironmentName(lake.Name, desired)
	key := client.ObjectKey{Namespace: lakeNamespace, Name: name}
	if err := r.deleteSupersededLakeEnvironments(ctx, lake, desired, lakeNamespace, name); err != nil {
		return err
	}
	if deploymentShouldTearDown(desired) {
		var environment oramav1alpha1.LakeEnvironment
		exists := true
		if err := r.Get(ctx, key, &environment); apierrors.IsNotFound(err) {
			exists = false
		} else if err != nil {
			return err
		} else if !metav1.IsControlledBy(&environment, lake) {
			return fmt.Errorf("refusing to delete LakeEnvironment %s/%s not owned by Lake %s", key.Namespace, key.Name, lake.Name)
		}
		if err := transition(oramav1alpha1.LakeEnvironmentPhaseTornDown, "", "", deploymentTeardownMessage(desired)); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if err := r.Delete(ctx, &environment); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	var existing oramav1alpha1.LakeEnvironment
	if err := r.Get(ctx, key, &existing); err == nil {
		if !metav1.IsControlledBy(&existing, lake) {
			return fmt.Errorf("LakeEnvironment %s/%s is not owned by Lake %s", key.Namespace, key.Name, lake.Name)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
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
			LakeRef:     lake.Name,
			Repository:  desired.Repository,
			Environment: desired.Environment,
			Kind:        desired.Kind,
			Change:      desired.Change,
			Image:       desired.Image,
			Replicas:    desired.Replicas,
			Port:        desired.Port,
			Env:         mergeEnvironmentVariables(desired.Env, env),
			SecretRefs:  desired.SecretRefs,
			Host:        desired.Host,
			Domains:     domains,
		}
		return controllerutil.SetControllerReference(lake, environment, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.Get(ctx, key, environment); err != nil {
		return err
	}
	if environment.Status.Phase == "" || environment.Status.DeploymentID != deploymentID {
		return nil
	}
	return transition(environment.Status.Phase, lakeEnvironmentNamespace(environment), environment.Status.URL, lakeEnvironmentStatusMessage(environment))
}

// mergeEnvironmentVariables is the deployment's own variables, then lake's environment values
// (orama_control.environment_env), which win for the same name.
func mergeEnvironmentVariables(desired, values []oramav1alpha1.LakeEnvironmentVariable) []oramav1alpha1.LakeEnvironmentVariable {
	if len(values) == 0 {
		return desired
	}
	override := make(map[string]string, len(values))
	for _, variable := range values {
		override[variable.Name] = variable.Value
	}
	merged := make([]oramav1alpha1.LakeEnvironmentVariable, 0, len(desired)+len(values))
	for _, variable := range desired {
		if _, found := override[variable.Name]; !found {
			merged = append(merged, variable)
		}
	}
	return append(merged, values...)
}

func (r *OramaZettaClusterReconciler) deleteSupersededLakeEnvironments(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	desired *zettaDeploymentEvent,
	namespace string,
	keepName string,
) error {
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range environments.Items {
		environment := &environments.Items[i]
		if environment.Name == keepName ||
			!metav1.IsControlledBy(environment, lake) ||
			environment.Spec.LakeRef != lake.Name ||
			environment.Spec.Repository != desired.Repository ||
			environment.Spec.Environment != desired.Environment {
			continue
		}
		if err := r.Delete(ctx, environment); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
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

// deploymentEnvironmentName names the LakeEnvironment of a row by the Lake's resource name, not
// the public host in the row, so a Lake's environments keep their names whatever its host.
func deploymentEnvironmentName(lakeName string, event *zettaDeploymentEvent) string {
	return lakeEnvironmentResourceName(lakeName, event.Repository, event.Environment)
}

func lakeEnvironmentResourceName(lake, repository, environment string) string {
	identity := strings.Join([]string{lake, repository, environment}, "\n")
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))[:10]
	prefix := dnsLabel(environment)
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

// writeDeploymentTransition appends one status row when the observed state differs from the last
// status row. The row repeats the desired row's identity and request (lake shows the newest row of
// a deployment, and refuses a row without repository, environment or updated_at), and it is
// timed after the newest row of the deployment, so lake shows it whatever the clocks say.
func writeDeploymentTransition(
	ctx context.Context,
	base, token, operator string,
	state *zettaDeploymentState,
	phase, namespace, url, message string,
	now time.Time,
) error {
	desired := state.Desired
	if desired.DeploymentID == "" || desired.Repository == "" || desired.Environment == "" || desired.Lake == "" {
		// lake drops a whole sync on a row without its identity; such a desired row gets no answer.
		return nil
	}
	if previous := state.Status; previous != nil && previous.Phase == phase && previous.URL == url &&
		previous.Message == message && previous.Namespace == namespace {
		return nil
	}
	after := desired.UpdatedAt
	if state.Latest != nil {
		after = state.Latest.UpdatedAt
	}
	row := *desired
	row.RecordType = deploymentRecordTypeStatus
	row.Operator = operator
	row.Phase = phase
	row.Namespace = namespace
	row.URL = url
	row.Message = message
	row.UpdatedAt = zettaRowTimeAfter(now, after)
	row.OrderingToken = uuid.NewString()
	return zettaSQL(ctx, base, token, "orama_control", insertDeploymentRowSQL(&row))
}

// insertDeploymentRowSQL appends one deployments row with every column, lake's and the
// operator's, so no SELECT of either is refused.
func insertDeploymentRowSQL(event *zettaDeploymentEvent) string {
	envJSON := "[]"
	if len(event.Env) > 0 {
		encoded, _ := json.Marshal(event.Env)
		envJSON = string(encoded)
	}
	secretJSON := "[]"
	if len(event.SecretRefs) > 0 {
		encoded, _ := json.Marshal(event.SecretRefs)
		secretJSON = string(encoded)
	}
	values := map[string]string{
		"operator":         event.Operator,
		"lake":             event.Lake,
		"repository":       event.Repository,
		"environment":      event.Environment,
		"kind":             event.Kind,
		"change":           event.Change,
		"image":            event.Image,
		"deployment_id":    event.DeploymentID,
		"requested_by":     event.RequestedBy,
		"requested_at":     event.RequestedAt,
		"phase":            event.Phase,
		"namespace":        event.Namespace,
		"url":              event.URL,
		"message":          event.Message,
		"updated_at":       event.UpdatedAt,
		"ordering_token":   event.OrderingToken,
		"record_type":      event.RecordType,
		"replicas":         strconv.Itoa(int(event.Replicas)),
		"port":             strconv.Itoa(int(event.Port)),
		"env_json":         envJSON,
		"secret_refs_json": secretJSON,
		"host":             event.Host,
		"withdrawn":        strconv.FormatBool(event.Withdrawn),
		"change_closed":    strconv.FormatBool(event.ChangeClosed),
	}
	columns := append(append([]string(nil), lakeDeploymentColumns...), operatorDeploymentColumns...)
	literals := make([]string, len(columns))
	for i, column := range columns {
		literals[i] = deploymentSQLQuote(values[column])
	}
	return "INSERT INTO deployments (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(literals, ", ") + ")"
}

// zettaRowTime formats a row time as lake does.
func zettaRowTime(value time.Time) string {
	return value.UTC().Format(zettaRowTimeLayout)
}

// zettaRowTimeAfter is now, or one microsecond after `after` when now is not later: a row that
// answers another must sort after it in lake's order even if this clock is behind lake's.
func zettaRowTimeAfter(now time.Time, after string) string {
	now = now.UTC().Truncate(time.Microsecond)
	if previous, err := time.Parse(time.RFC3339Nano, after); err == nil {
		floor := previous.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
		if now.Before(floor) {
			now = floor
		}
	}
	return zettaRowTime(now)
}

func deploymentSQLQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func sqlBool(value bool) string {
	if value {
		return "TRUE"
	}
	return "FALSE"
}

func platformEnvironmentKey(repository, environment string) string {
	return repository + "\n" + environment
}

// latestPlatformRows reduces an append-only lake table to its newest row per key, in lake's
// order. Rows without the key's fields are skipped (lake refuses them too).
func latestPlatformRows(rows []map[string]string, keyColumns ...string) map[string]map[string]string {
	latest := make(map[string]map[string]string)
	for _, row := range rows {
		parts := make([]string, 0, len(keyColumns))
		complete := row["updated_at"] != ""
		for _, column := range keyColumns {
			if row[column] == "" {
				complete = false
			}
			parts = append(parts, row[column])
		}
		if !complete {
			continue
		}
		key := strings.Join(parts, "\n")
		current, found := latest[key]
		if !found || lakeRowIsNewer(
			lakeRowOrder(row["updated_at"], row["ordering_token"], row),
			lakeRowOrder(current["updated_at"], current["ordering_token"], current),
		) {
			latest[key] = row
		}
	}
	return latest
}

// latestEnvironmentEnv is lake's environment values per environment: the newest row per
// (repository, environment, key), tombstones dropped, sorted by name.
func latestEnvironmentEnv(rows []map[string]string) map[string][]oramav1alpha1.LakeEnvironmentVariable {
	byEnvironment := make(map[string][]oramav1alpha1.LakeEnvironmentVariable)
	for _, row := range latestPlatformRows(rows, "repository", "environment", "key") {
		if parseDeploymentBool(row["deleted"]) || strings.Contains(row["key"], "=") {
			continue
		}
		identity := platformEnvironmentKey(row["repository"], row["environment"])
		byEnvironment[identity] = append(byEnvironment[identity], oramav1alpha1.LakeEnvironmentVariable{Name: row["key"], Value: row["value"]})
	}
	for identity := range byEnvironment {
		variables := byEnvironment[identity]
		sort.Slice(variables, func(i, j int) bool { return variables[i].Name < variables[j].Name })
	}
	return byEnvironment
}

// lakeDomainPlan is what the operator does with one Lake's domains rows.
type lakeDomainPlan struct {
	// latest is the newest row per (repository, environment, host), in key order.
	latest []map[string]string
	// active is the valid, not deleted domains per environment: what goes in spec.domains.
	active map[string][]oramav1alpha1.LakeEnvironmentDomain
	// refused names the domains that are not applied, by row key, with the reason.
	refused map[string]string
}

func domainRowKey(row map[string]string) string {
	return strings.Join([]string{row["repository"], row["environment"], row["host"]}, "\n")
}

// lakeDomainClaimSince is when each live claim was made: per (repository, environment, host), lake's
// row order of the claim's first row since its last tombstone. A later row of the same claim (a tls
// change, the operator's status answer) does not make it younger; a claim withdrawn and made again
// is a new claim. A key whose newest row is a tombstone has no live claim and no entry.
func lakeDomainClaimSince(rows []map[string]string) map[string][3]string {
	type claim struct {
		since   [3]string
		deleted bool
	}
	byKey := make(map[string]*claim)
	ordered := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		if row["repository"] != "" && row["environment"] != "" && row["host"] != "" && row["updated_at"] != "" {
			ordered = append(ordered, row)
		}
	}
	order := func(row map[string]string) [3]string {
		return lakeRowOrder(row["updated_at"], row["ordering_token"], row)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return lakeRowIsNewer(order(ordered[j]), order(ordered[i])) })
	for _, row := range ordered {
		key := domainRowKey(row)
		current := byKey[key]
		deleted := parseDeploymentBool(row["deleted"])
		if current == nil {
			current = &claim{deleted: true}
			byKey[key] = current
		}
		if current.deleted && !deleted {
			current.since = order(row)
		}
		current.deleted = deleted
	}
	since := make(map[string][3]string, len(byKey))
	for key, current := range byKey {
		if !current.deleted {
			since[key] = current.since
		}
	}
	return since
}

// planLakeDomains reduces lake's domain requests and decides which ones an environment serves.
// A host must be a lowercase DNS name and not a platform host (lakeDomainGuard), and it belongs to
// the first claim: the earliest live claim by its creation time (lakeDomainClaimSince), then by the
// claim's id (repository, environment, host). Every later claim of the host by another environment
// is refused, so a later claim never takes a host already served, whatever its repository id
// (#776). A row whose host has no verified claim of its repository and environment in lake's
// domain_claims (verified, lake#949) is refused before it can claim anything.
func planLakeDomains(lake *oramav1alpha1.Lake, host string, rows []map[string]string, guard lakeDomainGuard, verified lakeVerifiedDomains) lakeDomainPlan {
	latest := latestPlatformRows(rows, "repository", "environment", "host")
	since := lakeDomainClaimSince(rows)
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	// The first claim first: by creation time, then by the claim's id.
	sort.Slice(keys, func(i, j int) bool {
		left, right := since[keys[i]], since[keys[j]]
		if left[0] != right[0] {
			return left[0] < right[0]
		}
		if left[1] != right[1] {
			return left[1] < right[1]
		}
		return keys[i] < keys[j]
	})
	plan := lakeDomainPlan{
		active:  make(map[string][]oramav1alpha1.LakeEnvironmentDomain),
		refused: make(map[string]string),
	}
	claimed := make(map[string]string)
	for _, key := range keys {
		row := latest[key]
		plan.latest = append(plan.latest, row)
		if parseDeploymentBool(row["deleted"]) {
			continue
		}
		domain := strings.ToLower(row["host"])
		identity := platformEnvironmentKey(row["repository"], row["environment"])
		switch {
		case domain != row["host"] || len(validation.IsDNS1123Subdomain(domain)) != 0 || !strings.Contains(domain, "."):
			plan.refused[key] = fmt.Sprintf("%q is not a lowercase DNS name", row["host"])
			continue
		case domain == host || domain == lake.Spec.Host:
			plan.refused[key] = fmt.Sprintf("%q is the Lake's own host", domain)
			continue
		}
		if reason := guard.refuses(lake, identity, domain); reason != "" {
			plan.refused[key] = reason
			continue
		}
		if !verified.allows(row) {
			plan.refused[key] = unverifiedDomainReason(domain)
			continue
		}
		if claimed[domain] != "" && claimed[domain] != identity {
			plan.refused[key] = fmt.Sprintf("%q is already claimed by another environment", domain)
			continue
		}
		claimed[domain] = identity
		tls := row["tls"]
		if tls != "provided" {
			tls = "acme"
		}
		plan.active[identity] = append(plan.active[identity], oramav1alpha1.LakeEnvironmentDomain{Host: domain, TLS: tls})
	}
	// spec.domains and the status answers in a stable order, not in claim order.
	sort.Slice(plan.latest, func(i, j int) bool { return domainRowKey(plan.latest[i]) < domainRowKey(plan.latest[j]) })
	for identity := range plan.active {
		domains := plan.active[identity]
		sort.Slice(domains, func(i, j int) bool { return domains[i].Host < domains[j].Host })
	}
	return plan
}

// currentLakeDomains is the domains each environment of lake serves now, by identity: what a poll
// that cannot read the domain claims keeps.
func (r *OramaZettaClusterReconciler) currentLakeDomains(ctx context.Context, lake *oramav1alpha1.Lake) (map[string][]oramav1alpha1.LakeEnvironmentDomain, error) {
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(lakeWorkloadNamespace(lake))); err != nil {
		return nil, err
	}
	kept := make(map[string][]oramav1alpha1.LakeEnvironmentDomain)
	for i := range environments.Items {
		environment := &environments.Items[i]
		if !metav1.IsControlledBy(environment, lake) {
			continue
		}
		identity := platformEnvironmentKey(environment.Spec.Repository, environment.Spec.Environment)
		kept[identity] = append([]oramav1alpha1.LakeEnvironmentDomain(nil), environment.Spec.Domains...)
	}
	return kept, nil
}

// writeDomainStatuses answers each domain request with a status row (lake docs/api/platform.md:
// "requests by Lake; operator materializes status"): deleted once a tombstone is applied, failed
// when refused, pending until the environment serves it, then what the environment reports. A row
// is appended only when the status or message differs from the newest row.
//
// lake and the operator both append rows for one domain, and the newest by updated_at wins. So an
// answer is timed one microsecond after the row it answers, never at this clock's now: an answer
// timed by a clock ahead of lake's would sort after lake's next request (a tombstone, a changed
// tls) and hide it.
func (r *OramaZettaClusterReconciler) writeDomainStatuses(ctx context.Context, lake *oramav1alpha1.Lake, base, token string, plan lakeDomainPlan) error {
	if len(plan.latest) == 0 {
		return nil
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := r.List(ctx, &environments, client.InNamespace(lakeWorkloadNamespace(lake))); err != nil {
		return err
	}
	byIdentity := make(map[string]*oramav1alpha1.LakeEnvironment)
	for i := range environments.Items {
		environment := &environments.Items[i]
		if !metav1.IsControlledBy(environment, lake) || !environment.DeletionTimestamp.IsZero() {
			continue
		}
		byIdentity[platformEnvironmentKey(environment.Spec.Repository, environment.Spec.Environment)] = environment
	}
	for _, row := range plan.latest {
		status, message := domainObservedStatus(row, plan.refused[domainRowKey(row)], byIdentity[platformEnvironmentKey(row["repository"], row["environment"])])
		if row["status"] == status && row["message"] == message {
			continue
		}
		if err := zettaSQL(ctx, base, token, "orama_control", insertDomainStatusSQL(row, status, message, zettaRowTimeAfter(time.Time{}, row["updated_at"]), uuid.NewString())); err != nil {
			return err
		}
	}
	return nil
}

func domainObservedStatus(row map[string]string, refused string, environment *oramav1alpha1.LakeEnvironment) (string, string) {
	switch {
	case parseDeploymentBool(row["deleted"]):
		return domainStatusDeleted, ""
	case refused != "":
		return oramav1alpha1.LakeEnvironmentDomainFailed, refused
	case environment == nil:
		return oramav1alpha1.LakeEnvironmentDomainPending, fmt.Sprintf("waiting for a deployment of environment %q", row["environment"])
	}
	for _, domain := range environment.Status.Domains {
		if domain.Host == row["host"] {
			return domain.Status, domain.Message
		}
	}
	return oramav1alpha1.LakeEnvironmentDomainPending, "waiting for the environment to apply the domain"
}

// insertDomainStatusSQL appends the operator's answer to a domain row, in lake's columns.
func insertDomainStatusSQL(row map[string]string, status, message, updatedAt, orderingToken string) string {
	tls := row["tls"]
	if tls == "" {
		tls = "acme"
	}
	values := []string{
		deploymentSQLQuote(row["lake"]),
		deploymentSQLQuote(row["repository"]),
		deploymentSQLQuote(row["environment"]),
		deploymentSQLQuote(row["host"]),
		deploymentSQLQuote(tls),
		deploymentSQLQuote(status),
		deploymentSQLQuote(message),
		sqlBool(parseDeploymentBool(row["deleted"])),
		deploymentSQLQuote(row["requested_by"]),
		deploymentSQLQuote(updatedAt),
		deploymentSQLQuote(orderingToken),
	}
	return "INSERT INTO domains (" + strings.Join(lakeDomainColumns, ", ") + ") VALUES (" + strings.Join(values, ", ") + ")"
}

func (r *OramaZettaClusterReconciler) localDeploymentLakes(ctx context.Context, namespace string) []oramav1alpha1.Lake {
	var lakes oramav1alpha1.LakeList
	if err := r.List(ctx, &lakes); err != nil {
		return nil
	}
	local := make([]oramav1alpha1.Lake, 0, len(lakes.Items))
	for i := range lakes.Items {
		if lakeWorkloadNamespace(&lakes.Items[i]) == namespace {
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
		if lakeWorkloadNamespace(lake) != namespace {
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
