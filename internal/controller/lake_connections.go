/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	connectionIDLabel                    = "orama.cloud/connection-id"
	workspaceConnectionsActiveAnnotation = "orama.cloud/connections-active"
)

// LakeNetworkWorkspace is a workspace that must leave rollout compatibility mode once its
// initial connections are all enforced.
type LakeNetworkWorkspace struct {
	ID        string
	Namespace string
}

// LakeNetworkConnection is one network_connections desired row and its current operator status.
type LakeNetworkConnection struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	SourceKind         string
	SourceID           string
	TargetKind         string
	TargetID           string
	Ports              []int32
	// CredentialScope is the raw credential_scope jsonb, empty when NULL (operator#469):
	// {"access": "read"|"write", "schema": "<name>"?}.
	CredentialScope string
	Desired         string
	Generation      int64
	Status          *LakeNetworkConnectionStatus
}

// LakeNetworkConnectionStatus is written only to network_connection_status.
type LakeNetworkConnectionStatus struct {
	ConnectionID       string
	Phase              string
	Message            string
	CredentialSecret   string
	ObservedGeneration int64
}

// LakeNetworkSnapshot is returned only after network_guard exists. Workspaces includes rows with
// no connections so their blanket same-namespace allowance can still be retired.
type LakeNetworkSnapshot struct {
	Workspaces  []LakeNetworkWorkspace
	Connections []LakeNetworkConnection
}

func lakeConnectionName(id string) string {
	return "connection-" + id
}

func (r *LakeReconciler) reconcileLakeConnections(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeServiceStore,
) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	snapshot, err := store.ListNetworkConnections(ctx)
	if errors.Is(err, errNetworkGuardMissing) {
		log.Info("connections skipped: " + err.Error())
		return false
	}
	if err != nil {
		log.Info("connections deferred: cannot read network connections", "err", err.Error())
		return false
	}
	return r.syncLakeConnections(ctx, lake, store, snapshot)
}

func (r *LakeReconciler) syncLakeConnections(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeServiceStore,
	snapshot LakeNetworkSnapshot,
) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	workspaceByID := make(map[string]LakeNetworkWorkspace, len(snapshot.Workspaces))
	ready := make(map[string]bool, len(snapshot.Workspaces))
	keep := make(map[string]map[string]bool, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		workspaceByID[workspace.ID] = workspace
		ready[workspace.ID] = true
		keep[workspace.Namespace] = map[string]bool{}
	}
	changing := false
	for i := range snapshot.Connections {
		row := &snapshot.Connections[i]
		workspace, found := workspaceByID[row.WorkspaceID]
		if !found || workspace.Namespace == "" || workspace.Namespace != row.WorkspaceNamespace {
			status := failedNetworkConnectionStatus(row, "workspace and its reconciled namespace are required")
			if writeNetworkConnectionStatus(ctx, store, row, status) != nil {
				changing = true
			}
			ready[row.WorkspaceID] = false
			continue
		}
		name := lakeConnectionName(row.ID)
		keep[workspace.Namespace][name] = true
		status, err := r.syncLakeConnection(ctx, lake, row)
		if err != nil {
			log.Error(err, "connection reconciliation deferred", "connection", row.ID)
			ready[row.WorkspaceID] = false
			changing = true
			continue
		}
		if writeNetworkConnectionStatus(ctx, store, row, status) != nil {
			log.Info("connection status write deferred", "connection", row.ID)
			changing = true
		}
		settled := status.ObservedGeneration == row.Generation
		if row.Desired == oramav1alpha1.ConnectionDesiredPresent {
			settled = settled && status.Phase == oramav1alpha1.ConnectionPhaseReady
		} else {
			settled = settled && status.Phase == oramav1alpha1.ConnectionPhaseDeleted
		}
		if !settled {
			ready[row.WorkspaceID] = false
			changing = true
		}
	}

	for _, workspace := range snapshot.Workspaces {
		var connections oramav1alpha1.ConnectionList
		if err := r.List(ctx, &connections, client.InNamespace(workspace.Namespace), client.MatchingLabels{
			serviceLakeLabel: lake.Name,
		}); err != nil {
			log.Error(err, "connection orphan list deferred", "workspace", workspace.ID)
			ready[workspace.ID] = false
			changing = true
			continue
		}
		for i := range connections.Items {
			connection := &connections.Items[i]
			if keep[workspace.Namespace][connection.Name] || !metav1.IsControlledBy(connection, lake) {
				continue
			}
			ready[workspace.ID] = false
			changing = true
			if connection.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, connection); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "orphan connection deletion deferred", "connection", connection.Name)
				}
			}
		}
	}

	for _, workspace := range snapshot.Workspaces {
		if !ready[workspace.ID] {
			continue
		}
		if err := r.activateWorkspaceConnections(ctx, lake, workspace); err != nil {
			log.Error(err, "workspace connection activation deferred", "workspace", workspace.ID)
			changing = true
		}
	}
	return changing
}

func (r *LakeReconciler) syncLakeConnection(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeNetworkConnection,
) (LakeNetworkConnectionStatus, error) {
	if message := validateNetworkConnection(row); message != "" {
		return failedNetworkConnectionStatus(row, message), nil
	}
	connection := &oramav1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakeConnectionName(row.ID),
			Namespace: row.WorkspaceNamespace,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(connection), connection); err == nil {
		if !metav1.IsControlledBy(connection, lake) {
			return failedNetworkConnectionStatus(row, "Connection name is occupied by another owner"), nil
		}
		if connection.Spec.ConnectionID != row.ID ||
			connection.Spec.LakeRef != lake.Name ||
			connection.Spec.CustomerID != row.CustomerID ||
			connection.Spec.WorkspaceID != row.WorkspaceID {
			return failedNetworkConnectionStatus(row, "Connection immutable ownership fields do not match the Lake row"), nil
		}
	} else if !apierrors.IsNotFound(err) {
		return LakeNetworkConnectionStatus{}, err
	}
	scope, _ := parseConnectionCredentialScope(row)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, connection, func() error {
		if connection.ResourceVersion != "" {
			if !metav1.IsControlledBy(connection, lake) ||
				connection.Spec.ConnectionID != "" && connection.Spec.ConnectionID != row.ID {
				return fmt.Errorf("Connection %s/%s is owned by another desired row", connection.Namespace, connection.Name)
			}
		}
		connection.Labels = withLabels(connection.Labels, map[string]string{
			connectionIDLabel: row.ID,
			serviceLakeLabel:  lake.Name,
			workspaceLabel:    row.WorkspaceID,
		})
		connection.Spec = oramav1alpha1.ConnectionSpec{
			LakeRef:         lake.Name,
			ConnectionID:    row.ID,
			CustomerID:      row.CustomerID,
			WorkspaceID:     row.WorkspaceID,
			SourceKind:      row.SourceKind,
			SourceID:        row.SourceID,
			TargetKind:      row.TargetKind,
			TargetID:        row.TargetID,
			Ports:           append([]int32(nil), row.Ports...),
			CredentialScope: scope,
			Desired:         row.Desired,
			Generation:      row.Generation,
		}
		return controllerutil.SetControllerReference(lake, connection, r.Scheme)
	})
	if err != nil {
		return LakeNetworkConnectionStatus{}, err
	}
	status := LakeNetworkConnectionStatus{
		ConnectionID:       row.ID,
		Phase:              oramav1alpha1.ConnectionPhasePending,
		Message:            "waiting for the Connection controller",
		ObservedGeneration: row.Generation,
	}
	if connection.Status.ObservedGeneration == row.Generation && connection.Status.Phase != "" {
		status.Phase = connection.Status.Phase
		status.Message = connection.Status.Message
		// Only the reference to the workspace secret, never the credential.
		status.CredentialSecret = connection.Status.CredentialSecret
	}
	if row.Desired == oramav1alpha1.ConnectionDesiredAbsent &&
		status.Phase == oramav1alpha1.ConnectionPhaseDeleted &&
		connection.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, connection); err != nil && !apierrors.IsNotFound(err) {
			return LakeNetworkConnectionStatus{}, err
		}
	}
	return status, nil
}

func validateNetworkConnection(row *LakeNetworkConnection) string {
	for name, value := range map[string]string{
		"connection id": row.ID,
		"customer id":   row.CustomerID,
		"workspace id":  row.WorkspaceID,
		"source id":     row.SourceID,
		"target id":     row.TargetID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return name + " must be a canonical UUID"
		}
	}
	if len(validation.IsDNS1123Label(row.WorkspaceNamespace)) != 0 {
		return "workspace namespace is invalid"
	}
	switch row.SourceKind {
	case oramav1alpha1.ConnectionSourceApp,
		oramav1alpha1.ConnectionSourceService,
		oramav1alpha1.ConnectionSourceAgent,
		oramav1alpha1.ConnectionSourceTunnel:
	default:
		return "source kind must be app, service, agent or tunnel"
	}
	switch row.TargetKind {
	case oramav1alpha1.ConnectionTargetService, oramav1alpha1.ConnectionTargetApp:
	default:
		return "target kind must be service or app"
	}
	if row.Desired != oramav1alpha1.ConnectionDesiredPresent && row.Desired != oramav1alpha1.ConnectionDesiredAbsent {
		return "desired must be present or absent"
	}
	if row.Generation < 1 {
		return "generation must be positive"
	}
	for _, port := range row.Ports {
		if port < 1 || port > 65535 {
			return "ports must be between 1 and 65535"
		}
	}
	if _, message := parseConnectionCredentialScope(row); message != "" {
		return message
	}
	return ""
}

// parseConnectionCredentialScope decodes credential_scope. Unknown fields are refused, so a scope
// the operator does not understand is never silently widened or narrowed.
func parseConnectionCredentialScope(row *LakeNetworkConnection) (*oramav1alpha1.ConnectionCredentialScope, string) {
	raw := strings.TrimSpace(row.CredentialScope)
	if raw == "" || raw == "null" {
		return nil, ""
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var scope oramav1alpha1.ConnectionCredentialScope
	if err := decoder.Decode(&scope); err != nil {
		return nil, "credential_scope must be {\"access\": \"read\"|\"write\", \"schema\"?: \"<name>\"}"
	}
	if !validConnectionScope(&scope) {
		return nil, "credential_scope access must be read or write, and schema a plain identifier"
	}
	if row.SourceKind != oramav1alpha1.ConnectionSourceApp || row.TargetKind != oramav1alpha1.ConnectionTargetService {
		return nil, "credential_scope is valid only from an app to a service"
	}
	return &scope, ""
}

func failedNetworkConnectionStatus(
	row *LakeNetworkConnection,
	message string,
) LakeNetworkConnectionStatus {
	return LakeNetworkConnectionStatus{
		ConnectionID:       row.ID,
		Phase:              oramav1alpha1.ConnectionPhaseFailed,
		Message:            message,
		ObservedGeneration: row.Generation,
	}
}

func writeNetworkConnectionStatus(
	ctx context.Context,
	store LakeServiceStore,
	row *LakeNetworkConnection,
	status LakeNetworkConnectionStatus,
) error {
	if row.Status != nil && *row.Status == status {
		return nil
	}
	if err := store.UpsertNetworkConnectionStatus(ctx, status); err != nil {
		return err
	}
	row.Status = &status
	return nil
}

func (r *LakeReconciler) activateWorkspaceConnections(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	workspace LakeNetworkWorkspace,
) error {
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: workspace.Namespace}, &namespace); err != nil {
		return err
	}
	if namespace.Labels[workspaceLabel] != workspace.ID || namespace.Labels[workspaceLakeLabel] != lake.Name {
		return fmt.Errorf("namespace %q is not owned by workspace %s", workspace.Namespace, workspace.ID)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	key := client.ObjectKey{Namespace: workspace.Namespace, Name: networkSandboxName}
	if err := r.Get(ctx, key, &sandbox); err != nil {
		return err
	}
	if !metav1.IsControlledBy(&sandbox, lake) {
		return fmt.Errorf("NetworkSandbox %s/%s is not controlled by Lake %q", sandbox.Namespace, sandbox.Name, lake.Name)
	}
	if namespace.Annotations[workspaceConnectionsActiveAnnotation] != "true" {
		before := namespace.DeepCopy()
		if namespace.Annotations == nil {
			namespace.Annotations = map[string]string{}
		}
		namespace.Annotations[workspaceConnectionsActiveAnnotation] = "true"
		if err := r.Patch(ctx, &namespace, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("mark workspace connections active: %w", err)
		}
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(
		ctx,
		&apps,
		client.InNamespace(lakeNamespace(lake)),
		client.MatchingLabels{workspaceLabel: workspace.ID},
	); err != nil {
		return fmt.Errorf("list workspace apps for connection activation: %w", err)
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.Sync == nil || app.Spec.Sync.WorkspaceNamespace != workspace.Namespace || !metav1.IsControlledBy(app, lake) {
			continue
		}
		if app.Annotations[workspaceConnectionsActiveAnnotation] == "true" {
			continue
		}
		before := app.DeepCopy()
		if app.Annotations == nil {
			app.Annotations = map[string]string{}
		}
		app.Annotations[workspaceConnectionsActiveAnnotation] = "true"
		if err := r.Patch(ctx, app, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("activate Connection-generated app policy for %s: %w", app.Name, err)
		}
	}
	policies := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(sandbox.Spec.Policies))
	for _, policy := range sandbox.Spec.Policies {
		if policy.Name != workspaceInternalName {
			policies = append(policies, policy)
		}
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	if !apiequality.Semantic.DeepEqual(sandbox.Spec.Policies, policies) {
		sandbox.Spec.Policies = policies
		if err := r.Update(ctx, &sandbox); err != nil {
			return fmt.Errorf("retire workspace internal allowance: %w", err)
		}
	}
	if _, _, err := reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, &sandbox); err != nil {
		return fmt.Errorf("render workspace connections: %w", err)
	}
	return nil
}

func connectionAllowanceNames(connection *oramav1alpha1.Connection) []string {
	egress, ingress := connectionPolicyNames(connection)
	return []string{egress, ingress}
}

func isConnectionAllowanceName(name string) bool {
	return strings.HasPrefix(name, "connection-") &&
		(strings.HasSuffix(name, "-egress") || strings.HasSuffix(name, "-ingress"))
}
