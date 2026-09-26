/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Per-customer Zetta workspaces (liquid-bridge decision 45). Zetta has no tenancy, so every lake
// customer that uses a workspace gets its own OramaZettaCluster. lake has no Kubernetes access: it
// appends record_type='desired' rows to orama_control.workspaces on its own data plane, and the
// operator materializes each valid workspace_id into a Lake-owned cluster and appends
// record_type='status' rows on phase transitions. A record_type='withdrawn' row newer than the
// newest desired row deletes the workspace: its cluster, its data volumes and its credential.
const (
	workspaceRecordTypeDesired   = "desired"
	workspaceRecordTypeWithdrawn = "withdrawn"
	workspaceRecordTypeStatus    = "status"

	WorkspacePhaseProvisioning = "Provisioning"
	WorkspacePhaseReady        = "Ready"
	WorkspacePhaseFailed       = "Failed"
	WorkspacePhaseDeleted      = "Deleted"

	// Labels on a workspace OramaZettaCluster. workspaceIDLabel also distinguishes workspace
	// clusters from the Lake's own data plane among the Lake's controlled OramaZettaClusters.
	workspaceIDLabel       = "orama.cloud/workspace-id"
	workspaceLakeLabel     = "orama.cloud/lake"
	workspaceCustomerLabel = "orama.cloud/customer-id"
	// workspaceCustomerAnnotation carries the raw customer_id; the label carries it verbatim when
	// it is a valid label value and a hash otherwise.
	workspaceCustomerAnnotation = "orama.cloud/customer-id"

	lakeWorkspaceMountPath        = "/var/run/lake/workspaces"
	lakeWorkspaceVolume           = "workspaces"
	lakeWorkspacePlaceholder      = "{workspace}"
	workspaceDefaultStorageSize   = "5Gi"
	workspaceDefaultMemoryReq     = "64Mi"
	workspaceDefaultMemoryLimit   = "1Gi"
	workspaceLongestServiceSuffix = "-zetta-headless"
	// workspaceDataClaimTemplate is the volumeClaimTemplate name of the OramaZettaCluster
	// StatefulSet; its PVCs are data-<cluster>-zetta-<ordinal>.
	workspaceDataClaimTemplate = "data"
)

var workspaceIDPattern = regexp.MustCompile(`^ws-[0-9a-f]{12}$`)

type zettaWorkspaceEvent struct {
	WorkspaceID string
	RecordType  string
	Lake        string
	CustomerID  string
	Phase       string
	URL         string
	Message     string
	UpdatedAt   string
}

type zettaWorkspaceState struct {
	Desired   *zettaWorkspaceEvent
	Withdrawn *zettaWorkspaceEvent
	Status    *zettaWorkspaceEvent
}

// withdrawn reports whether the newest withdrawn row is newer than the newest desired row, under
// the ordering the reducer uses (updated_at, then a deterministic tiebreak that favours the
// withdrawal when both rows carry the same updated_at).
func (s *zettaWorkspaceState) withdrawn() bool {
	return s.Withdrawn != nil && (s.Desired == nil || workspaceEventIsNewer(s.Desired, s.Withdrawn))
}

func lakeWorkspacesEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Workspaces.Enabled && lakeZettaEnabled(lake)
}

// lakeWorkspacesSecret aggregates every workspace cluster's admin token, keyed by workspace_id.
func lakeWorkspacesSecret(name string) string { return "lake-" + name + "-workspaces" }

// workspaceClusterName is the OramaZettaCluster for one workspace: <lake-name>-<workspace_id>.
func workspaceClusterName(lake, workspaceID string) string { return lake + "-" + workspaceID }

// workspaceClientService is the in-cluster client Service the OramaZettaCluster reconciler creates
// for a workspace cluster (zettaNames: <cluster>-zetta).
func workspaceClientService(lake, workspaceID string) string {
	return workspaceClusterName(lake, workspaceID) + "-zetta"
}

func workspaceClientURL(lake, workspaceID, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", workspaceClientService(lake, workspaceID), namespace, zettaAPIPort)
}

// lakeWorkspaceURLTemplate is LAKE_WORKSPACE_URL_TEMPLATE: lake replaces the literal {workspace}
// with a workspace_id to get that workspace's client URL.
func lakeWorkspaceURLTemplate(lake *oramav1alpha1.Lake) string {
	return workspaceClientURL(lake.Name, lakeWorkspacePlaceholder, lakeNamespace(lake))
}

func workspaceCustomerLabelValue(customerID string) string {
	if len(validation.IsValidLabelValue(customerID)) == 0 {
		return customerID
	}
	return fmt.Sprintf("sha256-%x", sha256.Sum256([]byte(customerID)))[:23]
}

// workspaceResources is spec.workspaces.resources, or the small default: 64Mi memory request, 1Gi
// memory limit, no CPU request (a workspace idles most of the time).
func workspaceResources(lake *oramav1alpha1.Lake) *corev1.ResourceRequirements {
	if lake.Spec.Workspaces.Resources != nil {
		return lake.Spec.Workspaces.Resources.DeepCopy()
	}
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(workspaceDefaultMemoryReq)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(workspaceDefaultMemoryLimit)},
	}
}

func workspaceStorage(lake *oramav1alpha1.Lake) oramav1alpha1.OramaZettaStorage {
	storage := lake.Spec.Workspaces.Storage
	if storage.Size == "" {
		storage.Size = workspaceDefaultStorageSize
	}
	return storage
}

// reconcileZettaWorkspaces pulls desired workspace rows from this cluster's orama_control.workspaces
// for every local Lake with workspaces enabled. Best-effort, like reconcileZettaDeployments: the
// periodic poll retries, and a failure never destabilizes the Zetta workload being reconciled.
func (r *OramaZettaClusterReconciler) reconcileZettaWorkspaces(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientService string) {
	log := logf.FromContext(ctx)
	lakes := r.localDeploymentLakes(ctx, z.Namespace)
	enabled := false
	for i := range lakes {
		enabled = enabled || lakeWorkspacesEnabled(&lakes[i])
	}
	if !enabled {
		return
	}
	token, err := r.zettaAdminToken(ctx, z)
	if err != nil {
		log.V(1).Info("workspace sync deferred — admin token not ready", "err", err.Error())
		return
	}
	base := r.zettaBaseURL(z, clientService)
	for i := range lakes {
		lake := &lakes[i]
		if !lakeWorkspacesEnabled(lake) {
			continue
		}
		rows, err := zettaSQLQuery(ctx, base, token, "orama_control", workspaceSelectSQL(lake.Name))
		if err != nil {
			log.V(1).Info("workspace sync deferred — Zetta query failed", "lake", lake.Name, "err", err.Error())
			continue
		}
		r.materializeZettaWorkspaceRows(ctx, z, lake, base, token, rows)
	}
}

func workspaceSelectSQL(lake string) string {
	return "SELECT workspace_id, record_type, lake, customer_id, phase, url, message, updated_at " +
		"FROM workspaces WHERE lake = " + deploymentSQLQuote(lake)
}

func reduceWorkspaceRows(rows []map[string]string) map[string]*zettaWorkspaceState {
	states := make(map[string]*zettaWorkspaceState)
	for _, row := range rows {
		event := &zettaWorkspaceEvent{
			WorkspaceID: row["workspace_id"],
			RecordType:  row["record_type"],
			Lake:        row["lake"],
			CustomerID:  row["customer_id"],
			Phase:       row["phase"],
			URL:         row["url"],
			Message:     row["message"],
			UpdatedAt:   row["updated_at"],
		}
		if event.WorkspaceID == "" {
			continue
		}
		if event.RecordType == "" {
			event.RecordType = workspaceRecordTypeDesired
		}
		state := states[event.WorkspaceID]
		if state == nil {
			state = &zettaWorkspaceState{}
			states[event.WorkspaceID] = state
		}
		switch event.RecordType {
		case workspaceRecordTypeStatus:
			if workspaceEventIsNewer(state.Status, event) {
				state.Status = event
			}
		case workspaceRecordTypeDesired:
			if workspaceEventIsNewer(state.Desired, event) {
				state.Desired = event
			}
		case workspaceRecordTypeWithdrawn:
			if workspaceEventIsNewer(state.Withdrawn, event) {
				state.Withdrawn = event
			}
		}
	}
	return states
}

func workspaceEventIsNewer(current, candidate *zettaWorkspaceEvent) bool {
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
	return fmt.Sprintf("%+v", *candidate) > fmt.Sprintf("%+v", *current)
}

func (r *OramaZettaClusterReconciler) materializeZettaWorkspaceRows(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, lake *oramav1alpha1.Lake, base, token string, rows []map[string]string) {
	log := logf.FromContext(ctx)
	states := reduceWorkspaceRows(rows)
	ids := make([]string, 0, len(states))
	for id, state := range states {
		if state.Desired != nil || state.Withdrawn != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// Withdrawals first, so a workspace withdrawn in the same poll frees its quota slot.
	for _, id := range ids {
		if states[id].withdrawn() {
			if err := r.withdrawZettaWorkspace(ctx, z, lake, base, token, states[id]); err != nil {
				log.Error(err, "workspace withdrawal deferred", "workspaceId", id)
			}
		}
	}
	live, err := r.liveWorkspaceCount(ctx, lake, states)
	if err != nil {
		log.Error(err, "workspace sync deferred — cannot count live workspaces", "lake", lake.Name)
		return
	}
	for _, id := range ids {
		if states[id].withdrawn() {
			continue
		}
		if err := r.reconcileZettaWorkspace(ctx, z, lake, base, token, states[id], &live); err != nil {
			log.Error(err, "workspace sync deferred", "workspaceId", id)
		}
	}
}

// ownsWorkspaceCluster is the ownership rule behind every destructive and quota decision: the
// cluster is controlled by this Lake and labelled as this Lake's workspace with this workspace_id.
func ownsWorkspaceCluster(lake *oramav1alpha1.Lake, cluster *oramav1alpha1.OramaZettaCluster, workspaceID string) bool {
	return metav1.IsControlledBy(cluster, lake) &&
		cluster.Labels[workspaceLakeLabel] == lake.Name &&
		cluster.Labels[workspaceIDLabel] == workspaceID
}

// liveWorkspaceCount counts the workspace clusters that count against
// spec.workspaces.maxWorkspaces: owned by this Lake, not being deleted, and not withdrawn in the
// current stream (the cache may not show a deletion issued moments ago).
func (r *OramaZettaClusterReconciler) liveWorkspaceCount(ctx context.Context, lake *oramav1alpha1.Lake, states map[string]*zettaWorkspaceState) (int, error) {
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(lakeNamespace(lake)), client.MatchingLabels{workspaceLakeLabel: lake.Name}); err != nil {
		return 0, err
	}
	live := 0
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		id := cluster.Labels[workspaceIDLabel]
		if !workspaceIDPattern.MatchString(id) || !ownsWorkspaceCluster(lake, cluster, id) || cluster.DeletionTimestamp != nil {
			continue
		}
		if state := states[id]; state != nil && state.withdrawn() {
			continue
		}
		live++
	}
	return live, nil
}

// workspaceDataClaims lists the data PVCs the workspace cluster's StatefulSet created from its
// volumeClaimTemplate: named data-<cluster>-zetta-<ordinal> and carrying the StatefulSet's
// selector labels, which the StatefulSet controller copies onto every claim. The StatefulSet never
// deletes these claims itself.
func (r *OramaZettaClusterReconciler) workspaceDataClaims(ctx context.Context, namespace, clusterName string) ([]corev1.PersistentVolumeClaim, error) {
	owner := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}
	statefulSet, _, _ := zettaNames(owner)
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(workspaceDataClaimTemplate+"-"+statefulSet+"-") + "[0-9]+$")
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(namespace), client.MatchingLabels(zettaLabels(owner))); err != nil {
		return nil, err
	}
	matched := make([]corev1.PersistentVolumeClaim, 0, len(claims.Items))
	for _, claim := range claims.Items {
		if pattern.MatchString(claim.Name) {
			matched = append(matched, claim)
		}
	}
	return matched, nil
}

// withdrawZettaWorkspace deletes a withdrawn workspace — its data PVCs, its OramaZettaCluster and
// its key in lake-<lake>-workspaces — and appends one Deleted status row once none of them remains.
// It touches only a cluster this Lake controls and labels as this workspace. Data PVCs are deleted
// before the cluster, so an interrupted pass never leaves live volumes behind a cluster that is
// already gone. Every poll repeats the same steps, so the path is idempotent.
func (r *OramaZettaClusterReconciler) withdrawZettaWorkspace(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, lake *oramav1alpha1.Lake, base, token string, state *zettaWorkspaceState) error {
	withdrawn := state.Withdrawn
	if withdrawn.Lake != lake.Name || lakeNamespace(lake) != z.Namespace || !lakeWorkspacesEnabled(lake) {
		return nil
	}
	if err := validateWorkspaceEvent(lake, withdrawn); err != nil {
		return r.writeWorkspaceTransition(ctx, base, token, withdrawn, state.Status, WorkspacePhaseFailed, "", err.Error())
	}
	namespace := lakeNamespace(lake)
	name := workspaceClusterName(lake.Name, withdrawn.WorkspaceID)
	key := client.ObjectKey{Namespace: namespace, Name: name}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	clusterFound := true
	if err := r.Get(ctx, key, cluster); apierrors.IsNotFound(err) {
		clusterFound = false
	} else if err != nil {
		return err
	}
	if clusterFound {
		if !ownsWorkspaceCluster(lake, cluster, withdrawn.WorkspaceID) {
			return r.writeWorkspaceTransition(ctx, base, token, withdrawn, state.Status, WorkspacePhaseFailed, "",
				fmt.Sprintf("OramaZettaCluster %s exists and is not this Lake's workspace", key))
		}
		if bound := cluster.Annotations[workspaceCustomerAnnotation]; bound != "" && bound != withdrawn.CustomerID {
			return r.writeWorkspaceTransition(ctx, base, token, withdrawn, state.Status, WorkspacePhaseFailed, "",
				"workspace_id is already bound to another customer")
		}
	}

	claims, err := r.workspaceDataClaims(ctx, namespace, name)
	if err != nil {
		return err
	}
	// Without the cluster, a live claim is deleted only when the status stream proves this
	// reconciler materialized the workspace: a non-empty URL is written only for a cluster it owns.
	claimsProven := clusterFound || (state.Status != nil && state.Status.URL != "")
	for i := range claims {
		claim := &claims[i]
		if claim.DeletionTimestamp != nil {
			continue
		}
		if !claimsProven {
			return r.writeWorkspaceTransition(ctx, base, token, withdrawn, state.Status, WorkspacePhaseFailed, "",
				fmt.Sprintf("PersistentVolumeClaim %s/%s exists and is not this Lake's workspace", namespace, claim.Name))
		}
		if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete workspace data volume %s/%s: %w", namespace, claim.Name, err)
		}
	}
	if clusterFound && cluster.DeletionTimestamp == nil {
		if err := r.Delete(ctx, cluster, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete workspace cluster %s: %w", key, err)
		}
	}
	if err := r.removeWorkspaceCredential(ctx, lake, withdrawn.WorkspaceID); err != nil {
		return err
	}

	// Deleted only once the cluster and every data volume are gone; until then the next poll
	// repeats the steps above and checks again.
	if err := r.Get(ctx, key, &oramav1alpha1.OramaZettaCluster{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	remaining, err := r.workspaceDataClaims(ctx, namespace, name)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return nil
	}
	return r.writeWorkspaceTransition(ctx, base, token, withdrawn, state.Status, WorkspacePhaseDeleted, "", "workspace deleted")
}

// removeWorkspaceCredential drops a workspace's key from lake-<lake>-workspaces. The Lake
// reconciler does not re-add it: it skips workspace clusters that are gone or being deleted.
func (r *OramaZettaClusterReconciler) removeWorkspaceCredential(ctx context.Context, lake *oramav1alpha1.Lake, workspaceID string) error {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeWorkspacesSecret(lake.Name)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(&secret, lake) {
		return nil
	}
	if _, found := secret.Data[workspaceID]; !found {
		return nil
	}
	delete(secret.Data, workspaceID)
	return r.Update(ctx, &secret)
}

// validateWorkspaceEvent rejects a desired row the operator will not materialize. The name-length
// check keeps <lake>-<workspace_id>-zetta-headless inside the 63-character Service name limit.
func validateWorkspaceEvent(lake *oramav1alpha1.Lake, event *zettaWorkspaceEvent) error {
	switch {
	case !workspaceIDPattern.MatchString(event.WorkspaceID):
		return fmt.Errorf("workspace_id must match %s", workspaceIDPattern.String())
	case event.CustomerID == "":
		return fmt.Errorf("customer_id is required")
	case len(workspaceClusterName(lake.Name, event.WorkspaceID)+workspaceLongestServiceSuffix) > validation.DNS1035LabelMaxLength:
		return fmt.Errorf("lake name %q is too long for workspace cluster names", lake.Name)
	default:
		return nil
	}
}

// reconcileZettaWorkspace materializes one desired workspace. live is the Lake's live workspace
// count: it is compared with spec.workspaces.maxWorkspaces before a new cluster is created and
// incremented after one is. An existing workspace is never refused by the quota.
func (r *OramaZettaClusterReconciler) reconcileZettaWorkspace(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, lake *oramav1alpha1.Lake, base, token string, state *zettaWorkspaceState, live *int) error {
	desired := state.Desired
	// Locality: the row must name this Lake, which is local to the polled cluster's namespace
	// (localDeploymentLakes) and has workspaces enabled (reconcileZettaWorkspaces).
	if desired.Lake != lake.Name || lakeNamespace(lake) != z.Namespace || !lakeWorkspacesEnabled(lake) {
		return nil
	}
	if err := validateWorkspaceEvent(lake, desired); err != nil {
		return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseFailed, "", err.Error())
	}
	namespace := lakeNamespace(lake)
	name := workspaceClusterName(lake.Name, desired.WorkspaceID)
	key := client.ObjectKey{Namespace: namespace, Name: name}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := r.Get(ctx, key, cluster); err == nil {
		if !ownsWorkspaceCluster(lake, cluster, desired.WorkspaceID) {
			return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseFailed, "",
				fmt.Sprintf("OramaZettaCluster %s exists and is not this Lake's workspace", key))
		}
		if bound := cluster.Annotations[workspaceCustomerAnnotation]; bound != "" && bound != desired.CustomerID {
			return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseFailed, "",
				"workspace_id is already bound to another customer")
		}
		// A re-desire after a withdrawal waits for the old cluster to finish deleting, so the
		// workspace is recreated from scratch rather than written into a terminating object.
		if cluster.DeletionTimestamp != nil {
			return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseProvisioning,
				workspaceClientURL(lake.Name, desired.WorkspaceID, namespace), "waiting for the previous workspace cluster to be deleted")
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	} else {
		// A new cluster must not adopt an old data volume: its StatefulSet would bind a claim of
		// the same name. Wait for a terminating one; refuse a live one this path cannot prove is
		// its own.
		claims, err := r.workspaceDataClaims(ctx, namespace, name)
		if err != nil {
			return err
		}
		for _, claim := range claims {
			if claim.DeletionTimestamp != nil {
				return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseProvisioning,
					workspaceClientURL(lake.Name, desired.WorkspaceID, namespace), "waiting for the previous workspace data volume to be deleted")
			}
			return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseFailed, "",
				fmt.Sprintf("PersistentVolumeClaim %s/%s exists and is not this Lake's workspace", namespace, claim.Name))
		}
		if limit := int(lake.Spec.Workspaces.MaxWorkspaces); limit > 0 && *live >= limit {
			return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, WorkspacePhaseFailed, "",
				fmt.Sprintf("workspace quota reached (%d)", limit))
		}
		cluster = &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	}

	operatorSchema := false
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		if cluster.Labels == nil {
			cluster.Labels = map[string]string{}
		}
		cluster.Labels["app.kubernetes.io/name"] = "lake-workspace"
		cluster.Labels["app.kubernetes.io/instance"] = lake.Name
		cluster.Labels["app.kubernetes.io/managed-by"] = "orama-operator"
		cluster.Labels[workspaceLakeLabel] = lake.Name
		cluster.Labels[workspaceIDLabel] = desired.WorkspaceID
		cluster.Labels[workspaceCustomerLabel] = workspaceCustomerLabelValue(desired.CustomerID)
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[workspaceCustomerAnnotation] = desired.CustomerID
		// Only the fields the workspace contract owns are written, so server-side defaults on the
		// rest of the spec do not turn every poll into an update.
		cluster.Spec.Replicas = 1
		cluster.Spec.Image = lake.Spec.Workspaces.Image
		cluster.Spec.PullSecretName = lake.Spec.PullSecretName
		cluster.Spec.Storage = workspaceStorage(lake)
		cluster.Spec.Resources = workspaceResources(lake)
		cluster.Spec.IngressEnabled = false
		cluster.Spec.ProvisionOperatorSchema = &operatorSchema
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	})
	if err != nil {
		return err
	}
	if result == controllerutil.OperationResultCreated {
		*live++
	}

	phase, message := r.workspacePhase(ctx, lake, cluster, desired.WorkspaceID)
	return r.writeWorkspaceTransition(ctx, base, token, desired, state.Status, phase,
		workspaceClientURL(lake.Name, desired.WorkspaceID, namespace), message)
}

// workspacePhase is Ready only when the cluster reports every node ready AND its admin token is in
// the Lake's workspace Secret, so a Ready row means lake can connect.
func (r *OramaZettaClusterReconciler) workspacePhase(ctx context.Context, lake *oramav1alpha1.Lake, cluster *oramav1alpha1.OramaZettaCluster, workspaceID string) (string, string) {
	if cluster.Status.Phase != "Ready" || cluster.Status.ReadyNodes < 1 {
		return WorkspacePhaseProvisioning, "waiting for the workspace Zetta node"
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeWorkspacesSecret(lake.Name)}, &secret); err != nil ||
		len(secret.Data[workspaceID]) == 0 {
		return WorkspacePhaseProvisioning, "waiting for the workspace credential"
	}
	return WorkspacePhaseReady, "workspace ready"
}

// writeWorkspaceTransition appends a status row only when the phase, url or message differs from
// the newest status row already in the table, so a steady workspace produces no Zetta writes.
func (r *OramaZettaClusterReconciler) writeWorkspaceTransition(ctx context.Context, base, token string, desired, previous *zettaWorkspaceEvent, phase, url, message string) error {
	if previous != nil && previous.Phase == phase && previous.URL == url && previous.Message == message {
		return nil
	}
	statement := insertWorkspaceStatusSQL(desired.WorkspaceID, desired.Lake, desired.CustomerID, phase, url, message, time.Now().UTC().Format(time.RFC3339Nano))
	return zettaSQL(ctx, base, token, "orama_control", statement)
}

func insertWorkspaceStatusSQL(workspaceID, lake, customerID, phase, url, message, updatedAt string) string {
	return fmt.Sprintf(
		"INSERT INTO workspaces (workspace_id, record_type, lake, customer_id, phase, url, message, updated_at) VALUES (%s, %s, %s, %s, %s, %s, %s, %s)",
		deploymentSQLQuote(workspaceID),
		deploymentSQLQuote(workspaceRecordTypeStatus),
		deploymentSQLQuote(lake),
		deploymentSQLQuote(customerID),
		deploymentSQLQuote(phase),
		deploymentSQLQuote(url),
		deploymentSQLQuote(message),
		deploymentSQLQuote(updatedAt),
	)
}

// reconcileLakeWorkspaceSecret maintains lake-<name>-workspaces: one key per workspace_id whose
// value is that workspace cluster's admin token, copied from <cluster>-zetta-admin once minted. A
// key is kept while its token is not readable (so a transient read failure never removes a file
// lake depends on) and dropped when its cluster no longer exists or is being deleted (a withdrawn
// workspace).
func (r *LakeReconciler) reconcileLakeWorkspaceSecret(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) error {
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace), client.MatchingLabels{workspaceLakeLabel: lake.Name}); err != nil {
		return err
	}
	tokens := map[string][]byte{}
	present := map[string]bool{}
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		id := cluster.Labels[workspaceIDLabel]
		if !workspaceIDPattern.MatchString(id) || !metav1.IsControlledBy(cluster, lake) || cluster.DeletionTimestamp != nil {
			continue
		}
		present[id] = true
		var admin corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster.Name + "-zetta-admin"}, &admin); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if token := admin.Data["token"]; len(token) > 0 {
			tokens[id] = token
		}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeWorkspacesSecret(lake.Name), Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		secret.Type = corev1.SecretTypeOpaque
		data := map[string][]byte{}
		for id, token := range secret.Data {
			if present[id] {
				data[id] = token
			}
		}
		for id, token := range tokens {
			data[id] = token
		}
		secret.Data = data
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	return err
}

func isLakeWorkspaceCluster(cluster *oramav1alpha1.OramaZettaCluster) bool {
	return cluster.Labels[workspaceIDLabel] != ""
}
