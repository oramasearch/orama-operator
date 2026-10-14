/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeWorkspacesPoll = 60 * time.Second

	WorkspacePhasePending  = "Pending"
	WorkspacePhaseReady    = "Ready"
	WorkspacePhaseFailed   = "Failed"
	WorkspacePhaseStopping = "Stopping"
	WorkspacePhaseStopped  = "Stopped"
	WorkspacePhaseDeleting = "Deleting"
	WorkspacePhaseDeleted  = "Deleted"

	workspaceLabel            = "orama.cloud/workspace"
	workspaceCustomerLabel    = "orama.cloud/customer"
	workspaceEnvironmentLabel = "orama.cloud/environment"
	workspaceLakeLabel        = "orama.cloud/lake"
	workspaceManagedByLabel   = "app.kubernetes.io/managed-by"

	workspaceProfileName     = "workspace-defaults"
	workspaceDefaultDenyName = "default-deny"
	workspaceInternalName    = "allow-workspace-internal"
	workspaceLakeIngressName = "allow-lake-ingress"
	workspaceDNSName         = "allow-dns-egress"
	workspaceLakeEgressName  = "allow-lake-egress"
	workspaceInternetName    = "allow-internet-https"
	workspaceDefaultPlanName = "default"
	workspaceZettaReplicas   = int32(1)

	workspaceWaitingZettaMessage = "waiting for the workspace Zetta cluster"
	workspaceDeliveringMessage   = "delivering the connection"
)

var workspacePublicIPExceptions = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"100::/64",
	"2001:db8::/32",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
}

type LakeWorkspacesReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	APIReader         client.Reader
	WorkspaceStores   LakeWorkspacesStoreOpener
	Events            <-chan event.GenericEvent
	OperatorNamespace string
	// Now is the clock resize deadlines are judged by; nil means time.Now. Injected for tests.
	Now func() time.Time
}

func (r *LakeWorkspacesReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *LakeWorkspacesReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=egressrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets;resourcequotas;limitranges;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;endpoints;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

func lakeWorkspacesEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Workspaces.Enabled && lakePostgresEnabled(lake) && lake.DeletionTimestamp.IsZero()
}

func (r *LakeWorkspacesReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, request.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lakeWorkspacesEnabled(&lake) {
		return ctrl.Result{}, nil
	}
	parent := &LakeReconciler{Client: r.Client, Scheme: r.Scheme}
	_, _, schema := parent.lakeSchemaStatus(ctx, &lake, lakeNamespace(&lake))
	if schema.phase != "Ready" {
		return ctrl.Result{RequeueAfter: lakeServicesChangingPoll}, nil
	}
	changing := r.pollLakeWorkspaces(ctx, &lake)
	if changing {
		return ctrl.Result{RequeueAfter: lakeServicesChangingPoll}, nil
	}
	return ctrl.Result{RequeueAfter: lakeWorkspacesPoll}, nil
}

func (r *LakeWorkspacesReconciler) pollLakeWorkspaces(ctx context.Context, lake *oramav1alpha1.Lake) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name) + "-app"}
	if err := r.Get(ctx, key, &secret); err != nil || len(secret.Data["uri"]) == 0 {
		log.V(1).Info("workspaces deferred: control DB credential unavailable")
		return false
	}
	passCtx, cancel := context.WithTimeout(ctx, servicePassTimeout)
	defer cancel()
	open := r.WorkspaceStores
	if open == nil {
		open = openPgxWorkspacesStore
	}
	store, err := open(passCtx, string(secret.Data["uri"]))
	if err != nil {
		log.Info("workspaces deferred: cannot connect to control DB")
		return false
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = store.Close(closeCtx)
	}()
	rows, err := store.ListWorkspaces(passCtx)
	if errors.Is(err, errWorkspacesTableMissing) {
		log.Info("workspaces skipped: " + err.Error())
		return false
	}
	if err != nil {
		log.Info("workspaces deferred: complete list unavailable")
		return false
	}
	return r.syncLakeWorkspaces(passCtx, lake, store, rows)
}

func (r *LakeWorkspacesReconciler) syncLakeWorkspaces(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeWorkspacesStore,
	rows []LakeWorkspaceRow,
) bool {
	live := r.liveWorkspaceCount(ctx, lake, rows)
	changing := false
	for desiredDeleted := 1; desiredDeleted >= 0; desiredDeleted-- {
		for i := range rows {
			row := &rows[i]
			isDeleted := row.Desired == "deleted"
			if (desiredDeleted == 1) != isDeleted {
				continue
			}
			status, rowChanging, err := r.reconcileLakeWorkspace(ctx, lake, row, &live)
			if err != nil {
				logf.FromContext(ctx).Error(err, "workspace reconciliation deferred", "lake", lake.Name, "workspace", row.ID)
				changing = true
				continue
			}
			if status.StoragePhase == "" {
				status.StorageGiObserved, status.StoragePhase, status.StorageMessage = unobservedStorage(row)
			}
			changing = changing || rowChanging
			if err := writeWorkspaceZettaServiceStatus(ctx, store, lake, row, status); err != nil {
				logf.FromContext(ctx).Error(err, "workspace Zetta service status write deferred", "lake", lake.Name, "workspace", row.ID)
				changing = true
			}
			if status == row.status() {
				continue
			}
			updated, err := store.UpdateWorkspaceStatus(ctx, row, status)
			if err != nil || !updated {
				changing = true
				continue
			}
			row.Phase = status.Phase
			row.ObservedGeneration = status.ObservedGeneration
			row.Message = status.Message
			row.Namespace = status.Namespace
			row.StorageGiObserved = status.StorageGiObserved
			row.StoragePhase = status.StoragePhase
			row.StorageMessage = status.StorageMessage
		}
	}
	return changing
}

func writeWorkspaceZettaServiceStatus(
	ctx context.Context,
	store LakeWorkspacesStore,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	workspaceStatus LakeWorkspaceStatus,
) error {
	if row.ZettaServiceID == "" || row.ZettaServiceGeneration == 0 {
		return nil
	}
	status := LakeServiceStatus{
		ServiceID:          row.ZettaServiceID,
		Message:            workspaceStatus.Message,
		ObservedGeneration: row.ZettaServiceGeneration,
	}
	endpoint := func() string {
		return workspaceZettaEndpoint(lakeWorkspaceZettaName(lake, row), workspaceStatus.Namespace)
	}
	switch workspaceStatus.Phase {
	case WorkspacePhaseReady:
		status.Phase = ServicePhaseReady
		status.Message = "service ready"
		status.Endpoint = endpoint()
	case WorkspacePhasePending:
		status.Phase = ServicePhaseProvisioning
		if workspaceStatus.Message == workspaceWaitingZettaMessage || workspaceStatus.Message == workspaceDeliveringMessage {
			status.Endpoint = endpoint()
		}
	case WorkspacePhaseFailed:
		status.Phase = ServicePhaseFailed
	case WorkspacePhaseStopping, WorkspacePhaseDeleting:
		status.Phase = ServicePhaseDeleting
		status.Endpoint = endpoint()
	case WorkspacePhaseStopped:
		status.Phase = ServicePhaseDeleted
		status.Message = "workspace Zetta stopped"
	case WorkspacePhaseDeleted:
		status.Phase = ServicePhaseDeleted
		status.Message = "workspace Zetta deleted"
	default:
		return nil
	}
	if row.ZettaServiceStatus != nil && *row.ZettaServiceStatus == status {
		return nil
	}
	if err := store.UpsertServiceStatus(ctx, status); err != nil {
		return err
	}
	row.ZettaServiceStatus = &status
	return nil
}

func (r *LakeWorkspacesReconciler) liveWorkspaceCount(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	rows []LakeWorkspaceRow,
) int {
	live := 0
	for i := range rows {
		row := &rows[i]
		if row.Desired == "deleted" {
			continue
		}
		name, err := lakeWorkspaceNamespace(row)
		if err != nil {
			continue
		}
		var namespace corev1.Namespace
		if r.apiReader().Get(ctx, client.ObjectKey{Name: name}, &namespace) == nil && workspaceNamespaceOwnedBy(&namespace, lake, row) {
			live++
		}
	}
	return live
}

func (r *LakeWorkspacesReconciler) reconcileLakeWorkspace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	live *int,
) (LakeWorkspaceStatus, bool, error) {
	status := LakeWorkspaceStatus{ObservedGeneration: row.Generation}
	name, err := lakeWorkspaceNamespace(row)
	if err != nil {
		status.Phase = WorkspacePhaseFailed
		status.Message = err.Error()
		return status, false, nil
	}
	status.Namespace = name
	clusterName := lakeWorkspaceZettaName(lake, row)
	var plan oramav1alpha1.LakeWorkspacePlanSpec
	internet := false
	switch row.Desired {
	case "running":
		plan, internet, err = workspacePlan(lake, row.ID)
		if err != nil {
			status.Phase = WorkspacePhaseFailed
			status.Message = err.Error()
			return status, false, nil
		}
	case "stopped", "deleted":
	default:
		status.Phase = WorkspacePhaseFailed
		status.Message = "desired must be running, stopped or deleted"
		return status, false, nil
	}
	if changing, err := r.deletePreviousWorkspaceNamespace(ctx, lake, row, name); err != nil || changing {
		status.Phase = WorkspacePhasePending
		status.Message = "waiting for the previous workspace namespace to be deleted"
		return status, true, err
	}
	if changing, err := r.deleteLegacyWorkspace(ctx, lake, row, clusterName); err != nil || changing {
		status.Phase = WorkspacePhasePending
		status.Message = "removing the legacy workspace from the Lake namespace"
		return status, true, err
	}
	switch row.Desired {
	case "running":
		return r.reconcileRunningWorkspace(ctx, lake, row, name, clusterName, live, plan, internet)
	case "stopped":
		return r.reconcileStoppedWorkspace(ctx, lake, row, name, clusterName)
	case "deleted":
		return r.reconcileDeletedWorkspace(ctx, lake, row, name, live)
	}
	return status, false, nil
}

func lakeWorkspaceNamespace(row *LakeWorkspaceRow) (string, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil || id.String() != row.ID {
		return "", errors.New("workspace id must be a lowercase canonical UUID")
	}
	if problems := validation.IsDNS1123Label(row.Environment); len(problems) != 0 {
		return "", errors.New("workspace environment must be a DNS label")
	}
	if row.CustomerID == "" || len(validation.IsValidLabelValue(row.CustomerID)) != 0 {
		return "", errors.New("workspace customer id must be a Kubernetes label value")
	}
	if row.ZettaServiceID != "" {
		serviceID, err := uuid.Parse(row.ZettaServiceID)
		if err != nil || serviceID.String() != row.ZettaServiceID {
			return "", errors.New("workspace Zetta service id must be a lowercase canonical UUID")
		}
	}
	hex := strings.ReplaceAll(row.ID, "-", "")
	name := row.Environment + "-ws-" + hex[:12]
	if problems := validation.IsDNS1123Label(name); len(problems) != 0 {
		return "", errors.New("workspace namespace is not a valid DNS label")
	}
	return name, nil
}

func lakeWorkspaceZettaName(lake *oramav1alpha1.Lake, row *LakeWorkspaceRow) string {
	return workspaceZettaClusterName(lake, row.ID)
}

// workspaceZettaClusterName is <lake>-ws-<first 12 hex of the workspace id>, or "" for an id too
// short to name one.
func workspaceZettaClusterName(lake *oramav1alpha1.Lake, workspaceID string) string {
	hex := strings.ReplaceAll(workspaceID, "-", "")
	if len(hex) < 12 {
		return ""
	}
	return serviceResourceName(lake.Name, "ws-"+hex[:12])
}

// workspaceZettaEndpoint is the in-cluster client URL of a workspace Zetta.
func workspaceZettaEndpoint(clusterName, namespace string) string {
	return fmt.Sprintf("http://%s-zetta.%s.svc.cluster.local:%d", clusterName, namespace, zettaAPIPort)
}

// workspaceZettaCredential reads the connection of a workspace's Zetta from its namespace: the
// <slug>.json value lake reads from the services Secret. credential is nil until the cluster is
// Ready and its admin Secret holds a token; pending is true while a live, Lake-controlled cluster
// is still on its way there. A missing, foreign or terminating cluster is neither. The services
// poll writes the credential (it is the one writer of that Secret); the workspace reconciler
// reports the workspace Ready only once the Secret holds the same bytes.
func workspaceZettaCredential(
	ctx context.Context,
	reader client.Reader,
	lake *oramav1alpha1.Lake,
	workspaceID string,
	namespace string,
) (credential []byte, pending bool, err error) {
	clusterName := workspaceZettaClusterName(lake, workspaceID)
	if clusterName == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, false, nil
	}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, cluster); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(cluster, lake) || cluster.Labels[workspaceLabel] != workspaceID || !cluster.DeletionTimestamp.IsZero() {
		return nil, false, nil
	}
	if cluster.Status.Phase != "Ready" || cluster.Status.ReadyNodes < workspaceZettaReplicas {
		return nil, true, nil
	}
	var admin corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName + "-zetta-admin"}, &admin); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	token := string(admin.Data["token"])
	if token == "" {
		return nil, true, nil
	}
	credential, err = zettaCredential(workspaceZettaEndpoint(clusterName, namespace), token)
	return credential, false, err
}

// workspaceConnectionDelivered reports whether lake-<lake>-services holds the workspace Zetta's
// current connection under its service id. A workspace with no Zetta service row has nothing to
// deliver.
func (r *LakeWorkspacesReconciler) workspaceConnectionDelivered(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
) (bool, error) {
	if row.ZettaServiceID == "" {
		return true, nil
	}
	credential, _, err := workspaceZettaCredential(ctx, r.Client, lake, row.ID, namespace)
	if err != nil || credential == nil {
		return false, err
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeServicesSecret(lake.Name)}
	if err := r.apiReader().Get(ctx, key, &secret); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return bytes.Equal(secret.Data[row.ZettaServiceID], credential), nil
}

func workspaceLabels(lake *oramav1alpha1.Lake, row *LakeWorkspaceRow) map[string]string {
	return map[string]string{
		workspaceLabel:            row.ID,
		workspaceCustomerLabel:    row.CustomerID,
		workspaceEnvironmentLabel: row.Environment,
		workspaceLakeLabel:        lake.Name,
		workspaceManagedByLabel:   "orama-operator",
	}
}

func workspaceNamespaceOwnedBy(
	namespace *corev1.Namespace,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
) bool {
	labels := namespace.Labels
	return labels[workspaceLabel] == row.ID &&
		labels[workspaceCustomerLabel] == row.CustomerID &&
		labels[workspaceEnvironmentLabel] == row.Environment &&
		labels[workspaceLakeLabel] == lake.Name
}

func (r *LakeWorkspacesReconciler) reconcileRunningWorkspace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	name string,
	clusterName string,
	live *int,
	plan oramav1alpha1.LakeWorkspacePlanSpec,
	internet bool,
) (LakeWorkspaceStatus, bool, error) {
	status := LakeWorkspaceStatus{
		Phase:              WorkspacePhasePending,
		ObservedGeneration: row.Generation,
		Message:            "preparing the workspace namespace",
		Namespace:          name,
	}
	created, err := r.reconcileWorkspaceNamespace(ctx, lake, row, name, live)
	if err != nil {
		status.Phase = WorkspacePhaseFailed
		status.Message = err.Error()
		return status, false, nil
	}
	if created {
		*live++
	}
	if err := r.reconcileWorkspaceProfile(ctx, lake, row, name, plan, internet); err != nil {
		return status, true, err
	}
	if err := r.reconcileWorkspacePullSecret(ctx, lake, row, name); err != nil {
		return status, true, err
	}
	cluster, err := r.reconcileWorkspaceZetta(ctx, lake, row, name, clusterName)
	if err != nil {
		return status, true, err
	}
	// The volumes grow independently of the workspace's phase: a Zetta keeps serving while its
	// claims expand online (operator#259).
	storageChanging := false
	if storage, changing, err := r.reconcileWorkspaceStorage(ctx, lake, row, name, cluster); err != nil {
		logf.FromContext(ctx).Error(err, "workspace storage deferred", "lake", lake.Name, "workspace", row.ID)
		storageChanging = true
	} else {
		status.StorageGiObserved, status.StoragePhase, status.StorageMessage = storage.ObservedGi, storage.Phase, storage.Message
		storageChanging = changing
	}
	if cluster.Status.Phase == "Ready" && cluster.Status.ReadyNodes >= workspaceZettaReplicas {
		// Ready means lake can reach the Zetta: its connection is in the services Secret, which
		// the services poll writes (operator#264).
		delivered, err := r.workspaceConnectionDelivered(ctx, lake, row, name)
		if err != nil {
			return status, true, err
		}
		if !delivered {
			status.Message = workspaceDeliveringMessage
			return status, true, nil
		}
		status.Phase = WorkspacePhaseReady
		status.Message = "workspace ready"
		return status, storageChanging, nil
	}
	status.Message = workspaceWaitingZettaMessage
	return status, true, nil
}

func (r *LakeWorkspacesReconciler) reconcileStoppedWorkspace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	name string,
	clusterName string,
) (LakeWorkspaceStatus, bool, error) {
	status := LakeWorkspaceStatus{
		Phase:              WorkspacePhaseStopping,
		ObservedGeneration: row.Generation,
		Message:            "stopping the workspace Zetta cluster",
		Namespace:          name,
	}
	var namespace corev1.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		if client.IgnoreNotFound(err) == nil {
			status.Phase = WorkspacePhaseStopped
			status.Message = "workspace stopped"
			return status, false, nil
		}
		return status, true, err
	}
	if !workspaceNamespaceOwnedBy(&namespace, lake, row) {
		status.Phase = WorkspacePhaseFailed
		status.Message = fmt.Sprintf("namespace %q is not owned by workspace %s", name, row.ID)
		return status, false, nil
	}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	key := client.ObjectKey{Namespace: name, Name: clusterName}
	if err := r.Get(ctx, key, cluster); err != nil {
		if client.IgnoreNotFound(err) == nil {
			status.Phase = WorkspacePhaseStopped
			status.Message = "workspace stopped"
			return status, false, nil
		}
		return status, true, err
	}
	if cluster.Labels[workspaceLabel] != row.ID || !metav1.IsControlledBy(cluster, lake) {
		status.Phase = WorkspacePhaseFailed
		status.Message = fmt.Sprintf("OramaZettaCluster %s/%s is not owned by workspace %s", name, clusterName, row.ID)
		return status, false, nil
	}
	if cluster.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, cluster); err != nil {
			return status, true, err
		}
	}
	return status, true, nil
}

func (r *LakeWorkspacesReconciler) reconcileDeletedWorkspace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	name string,
	live *int,
) (LakeWorkspaceStatus, bool, error) {
	status := LakeWorkspaceStatus{
		Phase:              WorkspacePhaseDeleting,
		ObservedGeneration: row.Generation,
		Message:            "deleting the workspace namespace",
		Namespace:          name,
	}
	var namespace corev1.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		if client.IgnoreNotFound(err) == nil {
			status.Phase = WorkspacePhaseDeleted
			status.Message = "workspace deleted"
			return status, false, nil
		}
		return status, true, err
	}
	if !workspaceNamespaceOwnedBy(&namespace, lake, row) {
		status.Phase = WorkspacePhaseFailed
		status.Message = fmt.Sprintf("refusing to delete namespace %q because its workspace labels do not match", name)
		return status, false, nil
	}
	if namespace.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &namespace); err != nil {
			return status, true, err
		}
		if *live > 0 {
			*live--
		}
	}
	return status, true, nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceNamespace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	name string,
	live *int,
) (bool, error) {
	namespace := &corev1.Namespace{}
	err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, namespace)
	if err == nil {
		if !workspaceNamespaceOwnedBy(namespace, lake, row) {
			return false, fmt.Errorf("namespace %q is not owned by workspace %s", name, row.ID)
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	} else {
		if limit := int(lake.Spec.Workspaces.MaxWorkspaces); limit > 0 && *live >= limit {
			return false, fmt.Errorf("workspace quota reached (%d)", limit)
		}
		namespace.Name = name
	}
	created := namespace.ResourceVersion == ""
	before := namespace.DeepCopy()
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	for key, value := range workspaceLabels(lake, row) {
		namespace.Labels[key] = value
	}
	namespace.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
	namespace.Labels["pod-security.kubernetes.io/enforce-version"] = "latest"
	namespace.Labels["pod-security.kubernetes.io/audit"] = "restricted"
	namespace.Labels["pod-security.kubernetes.io/audit-version"] = "latest"
	namespace.Labels["pod-security.kubernetes.io/warn"] = "restricted"
	namespace.Labels["pod-security.kubernetes.io/warn-version"] = "latest"
	if err := controllerutil.SetControllerReference(lake, namespace, r.Scheme); err != nil {
		return false, err
	}
	if created {
		return true, r.Create(ctx, namespace)
	}
	if apiequality.Semantic.DeepEqual(before.Labels, namespace.Labels) &&
		apiequality.Semantic.DeepEqual(before.OwnerReferences, namespace.OwnerReferences) {
		return false, nil
	}
	return false, r.Update(ctx, namespace)
}

// The counts of Service and Secret objects are sized for a workspace that runs many datastores:
// one Postgres is three Services, one Cassandra five, and each brings several Secrets. At 20
// Services a workspace of two Postgres, a Kafka, a Polars and one stuck Cassandra was full, and
// no other kind could be created (operator#435). The memory ceiling is 32Gi for the same reason:
// it is a sum of limits, and a dozen small services reach 16Gi of limits while reserving 3Gi.
//
// The quota does not cap the sum of CPU limits. A CPU limit is only the ceiling a container may
// burst to, so their sum says nothing about what a workspace uses or reserves: with a default limit
// of one CPU, a workspace of nine pods using a third of a CPU between them was reported full at
// "8 of 8", and a Kafka and a Cassandra could not start (operator#428, operator#435). What a
// workspace may reserve is capped by requests.cpu, and its memory by both memory entries.
func defaultWorkspacePlan() oramav1alpha1.LakeWorkspacePlanSpec {
	return oramav1alpha1.LakeWorkspacePlanSpec{
		Quota: corev1.ResourceList{
			corev1.ResourceLimitsMemory:           resource.MustParse("32Gi"),
			corev1.ResourcePods:                   resource.MustParse("50"),
			corev1.ResourceRequestsCPU:            resource.MustParse("4"),
			corev1.ResourceRequestsMemory:         resource.MustParse("8Gi"),
			corev1.ResourceRequestsStorage:        resource.MustParse("100Gi"),
			corev1.ResourcePersistentVolumeClaims: resource.MustParse("20"),
			corev1.ResourceSecrets:                resource.MustParse("200"),
			corev1.ResourceServices:               resource.MustParse("100"),
		},
		DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Default: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

func workspacePlan(
	lake *oramav1alpha1.Lake,
	workspaceID string,
) (oramav1alpha1.LakeWorkspacePlanSpec, bool, error) {
	name := lake.Spec.Workspaces.DefaultPlan
	if name == "" {
		name = workspaceDefaultPlanName
	}
	override, overridden := lake.Spec.Workspaces.Overrides[workspaceID]
	if overridden && override.Plan != "" {
		name = override.Plan
	}
	plan, found := lake.Spec.Workspaces.Plans[name]
	if !found && name == workspaceDefaultPlanName {
		plan = defaultWorkspacePlan()
		found = true
	}
	if !found {
		return plan, override.InternetEgress, fmt.Errorf("workspace plan %q does not exist", name)
	}
	if len(plan.Quota) == 0 || len(plan.DefaultRequest) == 0 || len(plan.Default) == 0 {
		return plan, override.InternetEgress, fmt.Errorf("workspace plan %q must define quota, defaultRequest and default", name)
	}
	return plan, override.InternetEgress, nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceProfile(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
	plan oramav1alpha1.LakeWorkspacePlanSpec,
	internet bool,
) error {
	labels := workspaceLabels(lake, row)
	// internetEgress is an EgressRule (operator#470). It is written before the sandbox is rebuilt,
	// so the rebuild keeps the rule's allowance and the path never closes during the change.
	if err := r.reconcileWorkspaceInternetRule(ctx, lake, row, namespace, internet); err != nil {
		return err
	}
	if err := r.reconcileWorkspaceNetworkPolicies(ctx, lake, namespace, labels, internet); err != nil {
		return err
	}
	if err := r.reconcileWorkspaceServiceAccount(ctx, namespace, labels); err != nil {
		return err
	}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels = labels
		quota.Spec.Hard = workspaceQuota(lake, row, plan)
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile workspace ResourceQuota: %w", err)
	}
	limitRange := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, limitRange, func() error {
		limitRange.Labels = labels
		limitRange.Spec.Limits = []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Default:        plan.Default.DeepCopy(),
			DefaultRequest: plan.DefaultRequest.DeepCopy(),
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile workspace LimitRange: %w", err)
	}
	return nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceServiceAccount(
	ctx context.Context,
	namespace string,
	labels map[string]string,
) error {
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, account, func() error {
		account.Labels = labels
		no := false
		account.AutomountServiceAccountToken = &no
		return nil
	})
	if err != nil {
		return fmt.Errorf("disable the workspace default ServiceAccount token: %w", err)
	}
	return nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceNetworkPolicies(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	labels map[string]string,
	internet bool,
) error {
	connectionsActive, err := r.workspaceConnectionsActive(ctx, namespace)
	if err != nil {
		return err
	}
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Lake and the operator need workspace Zetta and agent HTTP ports to reconcile the workspace and its workloads.",
			workspaceLakeIngressPolicy(lake, r.OperatorNamespace, namespace, labels),
		),
		sandboxAllowance(
			"Every workspace workload needs cluster DNS over UDP and TCP for Service discovery.",
			workspaceDNSPolicy(namespace, labels),
		),
		sandboxAllowance(
			"Workspace workloads call their owning Lake API for control-plane operations and status.",
			workspaceLakeEgressPolicy(lake, namespace, labels),
		),
	}
	if !connectionsActive {
		policies = append(policies, sandboxAllowance(
			"Existing workspace workloads retain same-namespace traffic until every initial Lake Connection is enforced.",
			workspaceInternalPolicy(namespace, labels),
		))
	}
	for _, allowance := range workspaceOperatorIngresses {
		policies = append(
			policies,
			sandboxAllowance(allowance.comment, workspaceOperatorIngressPolicy(allowance, namespace, labels)),
		)
	}
	if r.OperatorNamespace != "" {
		policies = append(policies, sandboxAllowance(
			"The Orama operator signs in to workspace Postgres services to keep the roles of credential-scoped Connections (operator#469).",
			workspaceOperatorPostgresPolicy(r.OperatorNamespace, namespace, labels),
		))
	}
	policies = append(policies, workspaceLakeServiceIngressAllowances(lake)...)
	connectionPolicies, connectionErr := r.existingWorkspaceConnectionAllowances(ctx, namespace)
	policies = append(policies, connectionPolicies...)
	target, apiErr := r.readWorkspaceAPIServerTarget(ctx)
	if apiErr == nil {
		for _, apiClient := range workspaceAPIServerClients {
			policies = append(
				policies,
				sandboxAllowance(
					apiClient.comment,
					workspaceAPIServerEgressPolicy(apiClient, target, namespace, labels),
				),
			)
		}
	} else {
		var names []string
		for _, apiClient := range workspaceAPIServerClients {
			names = append(names, apiClient.name)
		}
		preserved, preserveErr := existingSandboxAllowances(ctx, r.Client, namespace, names...)
		policies = append(policies, preserved...)
		apiErr = errors.Join(apiErr, preserveErr)
	}
	// The internetEgress allowance is rendered by its EgressRule (internet-https); while the
	// override is on, the rebuild keeps what that rule rendered. Turning the override off drops it
	// here at once, and the rule is deleted.
	var internetErr error
	if internet {
		var preserved []oramav1alpha1.NetworkSandboxPolicy
		preserved, internetErr = existingSandboxAllowances(ctx, r.Client, namespace, workspaceInternetName)
		policies = append(policies, preserved...)
	}
	egressPolicies, egressErr := r.existingWorkspaceEgressAllowances(ctx, namespace)
	policies = append(policies, egressPolicies...)
	internetErr = errors.Join(internetErr, egressErr)
	var mesh *oramav1alpha1.NetworkSandboxMesh
	if lake.Spec.Workspaces.Overrides[labels[workspaceLabel]].Mesh {
		mesh = &oramav1alpha1.NetworkSandboxMesh{Enrolled: true}
	}
	_, reconcileErr := reconcileOwnedMeshNetworkSandbox(
		ctx,
		r.Client,
		r.Scheme,
		lake,
		namespace,
		labels,
		metav1.LabelSelector{},
		policies,
		mesh,
	)
	return errors.Join(connectionErr, apiErr, internetErr, reconcileErr)
}

func (r *LakeWorkspacesReconciler) workspaceConnectionsActive(
	ctx context.Context,
	namespace string,
) (bool, error) {
	var workspace corev1.Namespace
	// Uncached: the operator never lists or watches Namespaces (operator#106).
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: namespace}, &workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read workspace connection rollout state: %w", err)
	}
	return workspace.Annotations[workspaceConnectionsActiveAnnotation] == "true", nil
}

func (r *LakeWorkspacesReconciler) existingWorkspaceConnectionAllowances(
	ctx context.Context,
	namespace string,
) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	var connections oramav1alpha1.ConnectionList
	if err := r.List(ctx, &connections, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list workspace Connections: %w", err)
	}
	var names []string
	for i := range connections.Items {
		connection := &connections.Items[i]
		if connection.Spec.Desired != oramav1alpha1.ConnectionDesiredPresent || !connection.DeletionTimestamp.IsZero() {
			continue
		}
		names = append(names, connectionAllowanceNames(connection)...)
	}
	tunnelNames, err := workspaceTunnelAllowanceNames(ctx, r.Client, namespace)
	if err != nil {
		return nil, err
	}
	names = append(names, tunnelNames...)
	sort.Strings(names)
	return existingSandboxAllowances(ctx, r.Client, namespace, names...)
}

// existingWorkspaceEgressAllowances keeps what the workspace's Lake-declared EgressRules rendered.
func (r *LakeWorkspacesReconciler) existingWorkspaceEgressAllowances(
	ctx context.Context,
	namespace string,
) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	var rules oramav1alpha1.EgressRuleList
	if err := r.List(ctx, &rules, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list workspace EgressRules: %w", err)
	}
	var names []string
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.RuleID == "" || rule.Spec.Desired != oramav1alpha1.EgressRuleDesiredPresent || !rule.DeletionTimestamp.IsZero() {
			continue
		}
		names = append(names, egressRuleAllowanceNames(rule)...)
	}
	sort.Strings(names)
	return existingSandboxAllowances(ctx, r.Client, namespace, names...)
}

// reconcileWorkspaceInternetRule keeps the EgressRule that carries the workspace's internetEgress:
// port 443 to every public address, exactly what the override has always allowed. It is the
// operator's own rule (Lake writes no row for it) and is deleted when the override is off.
func (r *LakeWorkspacesReconciler) reconcileWorkspaceInternetRule(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
	internet bool,
) error {
	rule := &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{Name: workspaceInternetRuleName, Namespace: namespace}}
	if !internet {
		if err := r.Get(ctx, client.ObjectKeyFromObject(rule), rule); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(rule, lake) || !rule.DeletionTimestamp.IsZero() {
			return nil
		}
		if err := r.Delete(ctx, rule); err != nil && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove the workspace internet EgressRule: %w", err)
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rule, func() error {
		if rule.ResourceVersion != "" && !metav1.IsControlledBy(rule, lake) {
			return fmt.Errorf("EgressRule %s/%s is not controlled by Lake %q", namespace, rule.Name, lake.Name)
		}
		rule.Labels = withLabels(rule.Labels, map[string]string{
			serviceLakeLabel: lake.Name,
			workspaceLabel:   row.ID,
		})
		customer := ""
		if parsed, err := uuid.Parse(row.CustomerID); err == nil && parsed.String() == row.CustomerID {
			customer = row.CustomerID
		}
		rule.Spec = oramav1alpha1.EgressRuleSpec{
			LakeRef:        lake.Name,
			CustomerID:     customer,
			WorkspaceID:    row.ID,
			PublicInternet: true,
			Ports:          []int32{443},
			Desired:        oramav1alpha1.EgressRuleDesiredPresent,
			Generation:     1,
		}
		return controllerutil.SetControllerReference(lake, rule, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile the workspace internet EgressRule: %w", err)
	}
	return nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceInternet(
	ctx context.Context,
	namespace string,
	labels map[string]string,
	internet bool,
) error {
	if internet {
		policy, err := r.workspaceInternetPolicy(ctx, namespace, labels)
		if err != nil {
			if deleteErr := r.deleteWorkspaceInternetPolicy(ctx, namespace); deleteErr != nil {
				return errors.Join(err, deleteErr)
			}
			return err
		}
		return r.reconcileWorkspaceNetworkPolicy(ctx, labels, policy)
	}
	return r.deleteWorkspaceInternetPolicy(ctx, namespace)
}

func (r *LakeWorkspacesReconciler) deleteWorkspaceInternetPolicy(ctx context.Context, namespace string) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceInternetName, Namespace: namespace},
	}
	if err := r.Delete(ctx, policy); err != nil && client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("remove workspace internet NetworkPolicy: %w", err)
	}
	return nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceNetworkPolicy(
	ctx context.Context,
	labels map[string]string,
	desired *networkingv1.NetworkPolicy,
) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: desired.ObjectMeta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = labels
		policy.Spec = desired.Spec
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile workspace NetworkPolicy %s: %w", desired.Name, err)
	}
	return nil
}

func workspacePolicyMeta(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}
}

func workspaceDefaultDenyPolicy(namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceDefaultDenyName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

func workspaceInternalPolicy(namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceInternalName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{peer}}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{peer}}},
		},
	}
}

func workspaceLakeIngressPolicy(
	lake *oramav1alpha1.Lake,
	operatorNamespace string,
	namespace string,
	labels map[string]string,
) *networkingv1.NetworkPolicy {
	lakePeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: namespaceNameSelector(lakeNamespace(lake)),
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name":     "lake",
			"app.kubernetes.io/instance": lake.Name,
		}},
	}
	peers := []networkingv1.NetworkPolicyPeer{lakePeer}
	if operatorNamespace != "" {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: namespaceNameSelector(operatorNamespace),
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": "orama-operator",
			}},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceLakeIngressName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: peers,
				Ports: []networkingv1.NetworkPolicyPort{
					workspaceTCPPort(zettaAPIPort),
					workspaceTCPPort(agentPort),
				},
			}},
		},
	}
}

// workspaceOperatorPostgresName lets the Orama operator reach workspace Postgres instances.
const workspaceOperatorPostgresName = "allow-operator-to-postgres"

func workspaceOperatorPostgresPolicy(operatorNamespace, namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceOperatorPostgresName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels:      map[string]string{"cnpg.io/podRole": "instance"},
				MatchExpressions: []metav1.LabelSelectorRequirement{labelExists("cnpg.io/cluster")},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(operatorNamespace),
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app.kubernetes.io/name": "orama-operator",
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{workspaceTCPPort(servicePostgresPort)},
			}},
		},
	}
}

func workspaceDNSPolicy(namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceDNSName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector("kube-system"),
					PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "k8s-app",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"kube-dns", "coredns"},
					}}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: protocolPtr(corev1.ProtocolUDP), Port: intstrPtr(intstr.FromInt(53))},
					{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(intstr.FromInt(53))},
				},
			}},
		},
	}
}

func workspaceLakeEgressPolicy(
	lake *oramav1alpha1.Lake,
	namespace string,
	labels map[string]string,
) *networkingv1.NetworkPolicy {
	port := lakeDefaultPort
	if lake.Spec.Port != nil {
		port = int(*lake.Spec.Port)
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceLakeEgressName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(lakeNamespace(lake)),
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app.kubernetes.io/name":     "lake",
						"app.kubernetes.io/instance": lake.Name,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{workspaceTCPPort(int32(port))},
			}},
		},
	}
}

func namespaceNameSelector(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": name}}
}

func workspaceTCPPort(port int32) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{
		Protocol: protocolPtr(corev1.ProtocolTCP),
		Port:     intstrPtr(intstr.FromInt32(port)),
	}
}

func (r *LakeWorkspacesReconciler) workspaceInternetPolicy(
	ctx context.Context,
	namespace string,
	labels map[string]string,
) (*networkingv1.NetworkPolicy, error) {
	peers, err := publicInternetPeers(ctx, r.apiReader())
	if err != nil {
		return nil, err
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(workspaceInternetName, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    peers,
				Ports: []networkingv1.NetworkPolicyPort{workspaceTCPPort(443)},
			}},
		},
	}, nil
}

func appendAddressException(exceptions []string, value string) []string {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return exceptions
	}
	return append(exceptions, netip.PrefixFrom(address, address.BitLen()).String())
}

func (r *LakeWorkspacesReconciler) reconcileWorkspacePullSecret(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
) error {
	name := lake.Spec.PullSecretName
	if name == "" {
		return nil
	}
	return reconcileLakeSecretCopy(ctx, r.Client, r.Scheme, lake, row.ID, namespace, name, workspaceLabels(lake, row))
}

func reconcileLakeSecretCopy(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	lake *oramav1alpha1.Lake,
	workspaceID string,
	namespace string,
	name string,
	labels map[string]string,
) error {
	if name == "" || namespace == lakeNamespace(lake) {
		return nil
	}
	var sourceSecret corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: name}, &sourceSecret); err != nil {
		return fmt.Errorf("read Lake Secret %s: %w", name, err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, kube, secret, func() error {
		if secret.ResourceVersion != "" && secret.Labels[workspaceLabel] != workspaceID {
			return fmt.Errorf("Secret %s/%s is not owned by workspace %s", namespace, name, workspaceID)
		}
		secret.Labels = labels
		secret.Type = sourceSecret.Type
		secret.Immutable = sourceSecret.Immutable
		secret.Data = make(map[string][]byte, len(sourceSecret.Data))
		for key, value := range sourceSecret.Data {
			secret.Data[key] = append([]byte(nil), value...)
		}
		return controllerutil.SetControllerReference(lake, secret, scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile workspace Secret %s: %w", name, err)
	}
	return nil
}

func (r *LakeWorkspacesReconciler) reconcileWorkspaceZetta(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
	clusterName string,
) (*oramav1alpha1.OramaZettaCluster, error) {
	cluster := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}
	operatorSchema := false
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		if cluster.ResourceVersion != "" && cluster.Labels[workspaceLabel] != row.ID {
			return fmt.Errorf("OramaZettaCluster %s/%s is not owned by workspace %s", namespace, clusterName, row.ID)
		}
		creating := cluster.ResourceVersion == ""
		cluster.Labels = workspaceLabels(lake, row)
		if row.ZettaServiceID != "" {
			cluster.Labels[serviceIDLabel] = row.ZettaServiceID
			cluster.Labels[serviceKindLabel] = serviceKindZetta
		}
		cluster.Spec.Replicas = workspaceZettaReplicas
		cluster.Spec.Image = serviceZettaImage(lake)
		cluster.Spec.PullSecretName = lake.Spec.PullSecretName
		// Created at lake's storage_gi; later only ever raised, to keep the recorded size in step
		// with the claims the storage reconcile grows (operator#259).
		cluster.Spec.Storage = workspaceZettaStorage(lake, row, cluster.Spec.Storage, creating)
		cluster.Spec.Resources = serviceZettaResources(lake)
		cluster.Spec.Env = append([]corev1.EnvVar(nil), lake.Spec.Services.ZettaEnv...)
		cluster.Spec.IngressEnabled = false
		cluster.Spec.Host = ""
		cluster.Spec.DNSSecretRef = nil
		cluster.Spec.HostNetwork = false
		cluster.Spec.ExposePeerLoadBalancer = false
		cluster.Spec.ProvisionOperatorSchema = &operatorSchema
		return controllerutil.SetControllerReference(lake, cluster, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile workspace Zetta cluster: %w", err)
	}
	return cluster, nil
}

func (r *LakeWorkspacesReconciler) deletePreviousWorkspaceNamespace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	wanted string,
) (bool, error) {
	if row.Namespace == "" || row.Namespace == wanted {
		return false, nil
	}
	var namespace corev1.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: row.Namespace}, &namespace); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if namespace.Labels[workspaceLabel] != row.ID || namespace.Labels[workspaceLakeLabel] != lake.Name {
		return false, fmt.Errorf("refusing to replace namespace %q because its workspace labels do not match", row.Namespace)
	}
	if namespace.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &namespace); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (r *LakeWorkspacesReconciler) deleteLegacyWorkspace(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	legacyName string,
) (bool, error) {
	legacyNamespace := lakeNamespace(lake)
	cluster := &oramav1alpha1.OramaZettaCluster{}
	key := client.ObjectKey{Namespace: legacyNamespace, Name: legacyName}
	if err := r.Get(ctx, key, cluster); err == nil {
		owned := metav1.IsControlledBy(cluster, lake) || cluster.Labels[serviceLakeLabel] == lake.Name
		if !owned {
			return false, fmt.Errorf("legacy OramaZettaCluster %s/%s is not owned by Lake %s", legacyNamespace, legacyName, lake.Name)
		}
		if cluster.DeletionTimestamp.IsZero() {
			logf.FromContext(ctx).Info(
				"deleting legacy workspace Zetta cluster before isolated recreation",
				"lake",
				lake.Name,
				"workspace",
				row.ID,
				"cluster",
				key.String(),
			)
			if err := r.Delete(ctx, cluster); err != nil {
				return true, err
			}
		}
		return true, nil
	} else if client.IgnoreNotFound(err) != nil {
		return false, err
	}
	changing := false
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(legacyNamespace)); err != nil {
		return false, err
	}
	prefix := "data-" + legacyName + "-zetta-"
	for i := range claims.Items {
		claim := &claims.Items[i]
		if !strings.HasPrefix(claim.Name, prefix) {
			continue
		}
		changing = true
		if claim.DeletionTimestamp.IsZero() {
			logf.FromContext(ctx).Info(
				"deleting legacy workspace data volume",
				"lake",
				lake.Name,
				"workspace",
				row.ID,
				"claim",
				client.ObjectKeyFromObject(claim).String(),
			)
			if err := r.Delete(ctx, claim); err != nil {
				return true, err
			}
		}
	}
	for _, name := range []string{legacyName + "-zetta-admin", legacyName + "-zetta-cluster"} {
		var secret corev1.Secret
		key := client.ObjectKey{Namespace: legacyNamespace, Name: name}
		if err := r.Get(ctx, key, &secret); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return changing, err
		}
		changing = true
		if secret.Labels["app.kubernetes.io/instance"] != legacyName {
			return changing, fmt.Errorf("refusing to delete legacy Secret %s because its Zetta labels do not match", key.String())
		}
		if secret.DeletionTimestamp.IsZero() {
			logf.FromContext(ctx).Info(
				"deleting legacy workspace credential",
				"lake",
				lake.Name,
				"workspace",
				row.ID,
				"secret",
				key.String(),
			)
			if err := r.Delete(ctx, &secret); err != nil {
				return true, err
			}
		}
	}
	return changing, nil
}

func (r *LakeWorkspacesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&oramav1alpha1.NetworkSandbox{}).
		Owns(&oramav1alpha1.OramaZettaCluster{})
	if r.Events != nil {
		builder = builder.WatchesRawSource(source.Channel(r.Events, &handler.EnqueueRequestForObject{}))
	}
	return builder.Named("lake-workspaces").Complete(r)
}
