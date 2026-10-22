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
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// aksClusterFinalizer holds the resource until its deletionPolicy has been honored: for
	// Delete, until Azure reports the managed cluster gone.
	aksClusterFinalizer = "orama.cloud/aks-cluster-cleanup"

	clusterPhasePending      = "Pending"
	clusterPhaseProvisioning = "Provisioning"
	clusterPhaseReady        = "Ready"
	clusterPhaseDegraded     = "Degraded"
	clusterPhaseDeleting     = "Deleting"

	clusterConditionReady  = "Ready"
	clusterConditionInSync = "InSync"

	// kubeconfigSecretKey is the key the identity-only kubeconfig is written under.
	kubeconfigSecretKey = "kubeconfig"

	aksPollInterval  = 30 * time.Second
	aksSteadyRecheck = 5 * time.Minute
)

// AksClusterReconciler creates, observes and deletes one AKS managed cluster (operator#354). It
// follows the operator's "external resource as an owned CRD" pattern: a finalizer registered
// before any external state exists, create only when absent, status written only on change.
//
// Pool/version drift is observed only. Security access settings are updated on an existing
// cluster only when its own spec.access.enforce explicitly authorizes the migration.
type AksClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Azure is the Azure Resource Manager client; nil uses the public cloud endpoints.
	Azure    *azureClient
	Recorder record.EventRecorder
	// AccessCheck overrides the read-only identity access probe in tests.
	AccessCheck func(context.Context, []byte) error
}

//+kubebuilder:rbac:groups=orama.cloud,resources=aksclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=aksclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=aksclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *AksClusterReconciler) azure() *azureClient {
	if r.Azure == nil {
		r.Azure = newAzureClient()
	}
	return r.Azure
}

func (r *AksClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster oramav1alpha1.AksCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := cluster.Status.DeepCopy()

	creds, credsErr := r.credentials(ctx, cluster.Spec.CredentialsSecretRef)

	if !cluster.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cluster, observed, creds, credsErr)
	}

	// The finalizer is registered before any Azure state is created.
	if !controllerutil.ContainsFinalizer(&cluster, aksClusterFinalizer) {
		controllerutil.AddFinalizer(&cluster, aksClusterFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			return settle(ctrl.Result{}, err)
		}
	}

	if credsErr != nil {
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhasePending, "CredentialsUnavailable", credsErr.Error())
		return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &cluster, observed))
	}

	spec := cluster.Spec
	name := aksClusterAzureName(&cluster)
	az := r.azure()

	// Refused here, before any Azure state exists, with a reason that names the tag.
	if problem := azureTagProblem(spec.Tags); problem != "" {
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "InvalidTags", problem)
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{}, r.writeStatus(ctx, &cluster, observed))
	}

	body, err := azureManagedClusterSettingsBody(&cluster)
	if err != nil {
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "InvalidSysctls", err.Error())
		// Do not leave an old InSync=True condition behind after an invalid edit.
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type: clusterConditionInSync, Status: metav1.ConditionFalse, Reason: "InvalidSysctls",
			Message: err.Error(), ObservedGeneration: cluster.Generation,
		})
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{}, r.writeStatus(ctx, &cluster, observed))
	}

	found, err := az.getManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name)
	if err != nil {
		return r.fail(ctx, &cluster, observed, "ReadFailed", err)
	}
	if found == nil {
		if err := validateAksAccess(spec.Access); err != nil {
			return r.fail(ctx, &cluster, observed, "InvalidAccess", err)
		}
		if creds.ClientSecret != "" {
			return r.fail(ctx, &cluster, observed, "WorkloadIdentityRequired", fmt.Errorf("AKS administration requires workload identity"))
		}
		created, err := az.ensureResourceGroup(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, spec.Location, spec.Tags)
		if err != nil {
			return r.fail(ctx, &cluster, observed, "ResourceGroupFailed", err)
		}
		if created {
			cluster.Status.CreatedResourceGroup = true
			if err := r.writeStatus(ctx, &cluster, observed); err != nil {
				return settle(ctrl.Result{}, err)
			}
			observed = cluster.Status.DeepCopy()
		}
		body.Properties.AADProfile.TenantID = creds.TenantID
		if err := az.createManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name, body); err != nil {
			return r.fail(ctx, &cluster, observed, "CreateFailed", err)
		}
		cluster.Status.AccessSecured = true
		cluster.Status.ProvisioningState = "Creating"
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseProvisioning, "Creating", "Azure accepted the request to create the cluster")
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &cluster, observed))
	}

	r.observe(&cluster, found)

	// What Azure says about the operation it is running or last ran: its progress, and its error.
	operation, err := az.latestOperation(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name)
	if err != nil {
		return r.fail(ctx, &cluster, observed, "OperationReadFailed", err)
	}
	observeOperation(&cluster.Status, operation)

	switch state := found.Properties.ProvisioningState; state {
	case "Succeeded":
		drift := aksAccessDrift(&cluster, found, creds.TenantID)
		if len(drift) == 0 && !spec.Access.Enforce && !cluster.Status.AccessSecured {
			drift = append(drift, "workload-identity cutover requires spec.access.enforce")
		}
		if len(drift) > 0 {
			message := strings.Join(drift, "; ")
			if !spec.Access.Enforce && !cluster.Status.AccessSecured {
				// Preserve pre-migration management until this cluster explicitly permits the cutover.
				var delivered corev1.Secret
				ref := spec.KubeconfigSecretRef
				err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &delivered)
				if err != nil && !apierrors.IsNotFound(err) {
					return r.fail(ctx, &cluster, observed, "KubeconfigFailed", err)
				}
				cluster.Status.KubeconfigWritten = err == nil && len(delivered.Data[kubeconfigSecretKey]) > 0 && metav1.IsControlledBy(&delivered, &cluster)
				if r.Recorder != nil && (observed.Phase != clusterPhaseDegraded || observed.Message != message) {
					r.Recorder.Event(&cluster, corev1.EventTypeWarning, "InsecureAccess", message)
				}
				setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions, cluster.Generation, clusterPhaseDegraded, "InsecureAccess", message)
				setInSync(&cluster.Status.Conditions, cluster.Generation, drift)
				cluster.Status.ObservedGeneration = cluster.Generation
				return settle(ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &cluster, observed))
			}
			cluster.Status.KubeconfigWritten = false
			if err := validateAksAccess(spec.Access); err != nil {
				return r.fail(ctx, &cluster, observed, "InvalidAccess", err)
			}
			if creds.ClientSecret != "" {
				return r.fail(ctx, &cluster, observed, "WorkloadIdentityRequired", fmt.Errorf("AKS administration requires workload identity"))
			}
			scope := azureManagedClusterPath(spec.SubscriptionID, spec.ResourceGroup, name)
			if err := az.ensureAksRole(ctx, creds, scope, spec.Access.PrincipalID); err != nil {
				return r.fail(ctx, &cluster, observed, "AccessRoleFailed", err)
			}
			if err := az.enforceAksAccess(ctx, creds, scope, found, spec.Access); err != nil {
				return r.fail(ctx, &cluster, observed, "AccessUpdateFailed", err)
			}
			setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions, cluster.Generation, clusterPhaseProvisioning, "SecuringAccess", "waiting for Azure to confirm secure API access")
			cluster.Status.ObservedGeneration = cluster.Generation
			return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &cluster, observed))
		}
		if creds.ClientSecret != "" {
			return r.fail(ctx, &cluster, observed, "WorkloadIdentityRequired", fmt.Errorf("AKS administration requires workload identity"))
		}
		if err := az.ensureAksRole(ctx, creds, azureManagedClusterPath(spec.SubscriptionID, spec.ResourceGroup, name), spec.Access.PrincipalID); err != nil {
			return r.fail(ctx, &cluster, observed, "AccessRoleFailed", err)
		}
		if err := r.writeKubeconfig(ctx, &cluster, creds, name); err != nil {
			return r.fail(ctx, &cluster, observed, "KubeconfigFailed", err)
		}
		cluster.Status.AccessSecured = true
		cluster.Status.KubeconfigWritten = true
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseReady, "Provisioned", "")
		setInSync(&cluster.Status.Conditions, cluster.Generation, aksDrift(&cluster, found))
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &cluster, observed))
	case "Failed", "Canceled":
		message := aksAzureMessage(&cluster.Status)
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "Azure"+state, message)
		cluster.Status.ObservedGeneration = cluster.Generation
		// Logged once per distinct failure, when it is first seen, so it is in the log to be
		// corrected without repeating on every recheck.
		if observed.Message != message {
			logf.FromContext(ctx).Error(errors.New(message), "Azure failed the cluster's operation",
				"cluster", name, "resourceGroup", spec.ResourceGroup, "provisioningState", state)
		}
		return settle(ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &cluster, observed))
	default:
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseProvisioning, "Azure"+state, aksAzureMessage(&cluster.Status))
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &cluster, observed))
	}
}

// finalize honors the deletion policy. Retain releases the resource at once. Delete asks Azure to
// delete the managed cluster, waits for it to be gone, deletes the resource group if the operator
// created it, and only then releases the resource.
func (r *AksClusterReconciler) finalize(ctx context.Context, cluster *oramav1alpha1.AksCluster, observed *oramav1alpha1.AksClusterStatus, creds azureCredentials, credsErr error) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, aksClusterFinalizer) {
		return ctrl.Result{}, nil
	}
	if cluster.Spec.DeletionPolicy == oramav1alpha1.ClusterDeletionDelete {
		if credsErr != nil {
			// The cluster cannot be deleted without credentials, and silently releasing the
			// resource would orphan a running cluster. Hold, and say why.
			setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
				cluster.Generation, clusterPhaseDeleting, "CredentialsUnavailable",
				"cannot delete the cluster in Azure: "+credsErr.Error())
			return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, cluster, observed))
		}
		spec := cluster.Spec
		name := aksClusterAzureName(cluster)
		az := r.azure()

		found, err := az.getManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name)
		if err != nil {
			return r.fail(ctx, cluster, observed, "ReadFailed", err)
		}
		if found != nil {
			if found.Properties.ProvisioningState != "Deleting" {
				if err := az.deleteManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name); err != nil {
					return r.fail(ctx, cluster, observed, "DeleteFailed", err)
				}
			}
			cluster.Status.ProvisioningState = "Deleting"
			setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
				cluster.Generation, clusterPhaseDeleting, "Deleting", "waiting for Azure to delete the cluster")
			return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, cluster, observed))
		}
		if cluster.Status.CreatedResourceGroup {
			exists, err := az.resourceGroupExists(ctx, creds, spec.SubscriptionID, spec.ResourceGroup)
			if err != nil {
				return r.fail(ctx, cluster, observed, "ReadFailed", err)
			}
			if exists {
				if err := az.deleteResourceGroup(ctx, creds, spec.SubscriptionID, spec.ResourceGroup); err != nil {
					return r.fail(ctx, cluster, observed, "DeleteFailed", err)
				}
				setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
					cluster.Generation, clusterPhaseDeleting, "Deleting", "waiting for Azure to delete the resource group")
				return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, cluster, observed))
			}
		}
	}
	controllerutil.RemoveFinalizer(cluster, aksClusterFinalizer)
	return settle(ctrl.Result{}, r.Update(ctx, cluster))
}

// observe copies what Azure reports into status.
func (r *AksClusterReconciler) observe(cluster *oramav1alpha1.AksCluster, found *azureManagedCluster) {
	status := &cluster.Status
	props := found.Properties
	status.ProvisioningState = props.ProvisioningState
	status.ResourceID = found.ID
	status.FQDN = props.FQDN
	status.NodeResourceGroup = props.NodeResourceGroup
	status.KubernetesVersion = props.CurrentKubernetesVersion
	if status.KubernetesVersion == "" {
		status.KubernetesVersion = props.KubernetesVersion
	}
	status.PowerState = ""
	if props.PowerState != nil {
		status.PowerState = props.PowerState.Code
	}
	status.OIDCIssuerURL = ""
	if props.OIDCIssuerProfile != nil {
		status.OIDCIssuerURL = props.OIDCIssuerProfile.IssuerURL
	}
	status.ProvisioningError = ""
	if props.Status != nil {
		status.ProvisioningError = props.Status.ProvisioningError.text()
	}
	pools := make([]oramav1alpha1.AksNodePoolStatus, 0, len(props.AgentPoolProfiles))
	for _, pool := range props.AgentPoolProfiles {
		entry := oramav1alpha1.AksNodePoolStatus{
			Name: pool.Name, ProvisioningState: pool.ProvisioningState, Count: pool.Count,
		}
		if pool.Status != nil {
			entry.Error = pool.Status.ProvisioningError.text()
		}
		pools = append(pools, entry)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	status.NodePools = pools
}

// observeOperation copies Azure's latest operation into status. Azure keeping none clears it.
func observeOperation(status *oramav1alpha1.AksClusterStatus, operation *azureOperation) {
	if operation == nil {
		status.Operation = nil
		return
	}
	status.Operation = &oramav1alpha1.AksOperationStatus{
		ID: operation.Name, Status: operation.Status, StartTime: operation.StartTime, EndTime: operation.EndTime,
		Error: operation.Error.text(),
	}
}

// aksAzureMessage says where the cluster stands in Azure's words: the provisioning state, the
// operation's progress line, and every error Azure reports, on the operation, on the cluster and
// on each pool. A reader should not need the Azure portal to learn what to correct.
func aksAzureMessage(status *oramav1alpha1.AksClusterStatus) string {
	message := "Azure reports the cluster's provisioning state as " + status.ProvisioningState
	if op := status.Operation; op != nil {
		if op.Status != "" && op.Status != status.ProvisioningState {
			message += " (" + op.Status + ")"
		}
		if op.Error != "" {
			message += ". Operation " + op.ID + " failed: " + op.Error
		}
	}
	if status.ProvisioningError != "" && (status.Operation == nil || status.ProvisioningError != status.Operation.Error) {
		message += ". Cluster: " + status.ProvisioningError
	}
	for _, pool := range status.NodePools {
		if pool.Error != "" {
			message += ". Node pool " + pool.Name + ": " + pool.Error
		}
	}
	return message
}

// aksDrift lists what the spec asks for that the cluster does not have. This version reports it
// and does not act on it.
func aksDrift(cluster *oramav1alpha1.AksCluster, found *azureManagedCluster) []string {
	var drift []string
	running := found.Properties.CurrentKubernetesVersion
	if running == "" {
		running = found.Properties.KubernetesVersion
	}
	want := cluster.Spec.KubernetesVersion
	if running != "" && running != want && !strings.HasPrefix(running, want+".") {
		drift = append(drift, fmt.Sprintf("kubernetesVersion: want %s, cluster runs %s", want, running))
	}
	existing := map[string]azureAgentPool{}
	for _, pool := range found.Properties.AgentPoolProfiles {
		existing[pool.Name] = pool
	}
	for _, pool := range cluster.Spec.NodePools {
		have, ok := existing[pool.Name]
		if !ok {
			drift = append(drift, fmt.Sprintf("node pool %s: not in the cluster", pool.Name))
			continue
		}
		delete(existing, pool.Name)
		drift = append(drift, aksSysctlDrift(pool, have.LinuxOSConfig)...)
		if !strings.EqualFold(have.VMSize, pool.VMSize) {
			drift = append(drift, fmt.Sprintf("node pool %s: want vmSize %s, cluster has %s", pool.Name, pool.VMSize, have.VMSize))
		}
		if have.EnableAutoScaling != pool.EnableAutoScaling {
			drift = append(drift, fmt.Sprintf("node pool %s: want autoscaling %t, cluster has %t", pool.Name, pool.EnableAutoScaling, have.EnableAutoScaling))
		} else if pool.EnableAutoScaling {
			if int32Value(have.MinCount) != int32Value(pool.MinCount) || int32Value(have.MaxCount) != int32Value(pool.MaxCount) {
				drift = append(drift, fmt.Sprintf("node pool %s: want %d to %d nodes, cluster allows %d to %d",
					pool.Name, int32Value(pool.MinCount), int32Value(pool.MaxCount), int32Value(have.MinCount), int32Value(have.MaxCount)))
			}
		} else if have.Count != pool.Count {
			drift = append(drift, fmt.Sprintf("node pool %s: want %d nodes, cluster has %d", pool.Name, pool.Count, have.Count))
		}
	}
	extra := make([]string, 0, len(existing))
	for name := range existing {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		drift = append(drift, fmt.Sprintf("node pool %s: in the cluster, not in the spec", name))
	}
	return drift
}

func int32Value(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// writeKubeconfig delivers only public connection metadata and an in-process identity provider.
func (r *AksClusterReconciler) writeKubeconfig(ctx context.Context, cluster *oramav1alpha1.AksCluster, creds azureCredentials, name string) error {
	ref := cluster.Spec.KubeconfigSecretRef
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	endpoint := "https://" + cluster.Status.FQDN + ":443"
	if err == nil {
		if !metav1.IsControlledBy(existing, cluster) {
			return fmt.Errorf("kubeconfig Secret is not owned by this AksCluster")
		}
		if validateAksIdentityKubeconfig(existing.Data[kubeconfigSecretKey], creds, endpoint) == nil {
			return r.removeSupersededKubeconfigs(ctx, cluster)
		}
	}
	cluster.Status.KubeconfigWritten = false
	raw, err := r.azure().userKubeconfig(ctx, creds, cluster.Spec.SubscriptionID, cluster.Spec.ResourceGroup, name)
	if err != nil {
		return err
	}
	kubeconfig, err := aksIdentityKubeconfig(raw, creds, endpoint)
	if err != nil {
		return err
	}
	check := r.AccessCheck
	if check == nil {
		check = r.verifyIdentityAccess
	}
	if err := check(ctx, kubeconfig); err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.ResourceVersion != "" && !metav1.IsControlledBy(sec, cluster) {
			return fmt.Errorf("kubeconfig Secret is not owned by this AksCluster")
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		if !bytes.Equal(sec.Data[kubeconfigSecretKey], kubeconfig) {
			sec.Data[kubeconfigSecretKey] = kubeconfig
		}
		sec.Type = corev1.SecretTypeOpaque
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels["orama.cloud/kubernetes-cluster"] = cluster.Name
		return controllerutil.SetControllerReference(cluster, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	return r.removeSupersededKubeconfigs(ctx, cluster)
}

// Retiring a configured destination must not strand its old admin certificate in another
// namespace. Only labeled Secrets owned by this exact cluster lose their superseded key.
func (r *AksClusterReconciler) removeSupersededKubeconfigs(ctx context.Context, cluster *oramav1alpha1.AksCluster) error {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.MatchingLabels{"orama.cloud/kubernetes-cluster": cluster.Name}); err != nil {
		return err
	}
	ref := cluster.Spec.KubeconfigSecretRef
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if (sec.Name == ref.Name && sec.Namespace == ref.Namespace) || !metav1.IsControlledBy(sec, cluster) || len(sec.Data[kubeconfigSecretKey]) == 0 {
			continue
		}
		delete(sec.Data, kubeconfigSecretKey)
		if err := r.Update(ctx, sec); err != nil {
			return err
		}
	}
	return nil
}

// credentials reads the Azure credentials Secret.
func (r *AksClusterReconciler) credentials(ctx context.Context, ref oramav1alpha1.ClusterSecretReference) (azureCredentials, error) {
	return readAzureCredentials(ctx, r.Client, ref)
}

func readAzureCredentials(ctx context.Context, c client.Client, ref oramav1alpha1.ClusterSecretReference) (azureCredentials, error) {
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return azureCredentials{}, fmt.Errorf("credentials Secret %s/%s does not exist", ref.Namespace, ref.Name)
		}
		return azureCredentials{}, err
	}
	return azureCredentialsFromSecret(sec)
}

// fail records why a reconcile stopped and returns the error so the request is retried.
func (r *AksClusterReconciler) fail(ctx context.Context, cluster *oramav1alpha1.AksCluster, observed *oramav1alpha1.AksClusterStatus, reason string, err error) (ctrl.Result, error) {
	phase := clusterPhaseDegraded
	if !cluster.DeletionTimestamp.IsZero() {
		phase = clusterPhaseDeleting
	}
	setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
		cluster.Generation, phase, reason, err.Error())
	if statusErr := r.writeStatus(ctx, cluster, observed); statusErr != nil {
		return settle(ctrl.Result{}, statusErr)
	}
	return ctrl.Result{}, err
}

// writeStatus writes the status subresource only when it differs from what was read, so a steady
// cluster produces no writes.
func (r *AksClusterReconciler) writeStatus(ctx context.Context, cluster *oramav1alpha1.AksCluster, observed *oramav1alpha1.AksClusterStatus) error {
	if reflect.DeepEqual(&cluster.Status, observed) {
		return nil
	}
	// A patch of what changed, with no version precondition: this controller is the only writer
	// of this status, so a read that is one write behind must not make the write fail.
	base := cluster.DeepCopy()
	base.Status = *observed.DeepCopy()
	return r.Status().Patch(ctx, cluster, client.MergeFrom(base))
}

// setClusterStatus sets the phase, the message and the Ready condition together. The condition's
// transition time moves only when its status changes.
func setClusterStatus(phase, message *string, conditions *[]metav1.Condition, generation int64, newPhase, reason, text string) {
	*phase = newPhase
	*message = text
	status := metav1.ConditionFalse
	if newPhase == clusterPhaseReady {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: clusterConditionReady, Status: status, Reason: reason, Message: text, ObservedGeneration: generation,
	})
}

// setInSync records whether the spec and the real cluster agree.
func setInSync(conditions *[]metav1.Condition, generation int64, drift []string) {
	condition := metav1.Condition{
		Type: clusterConditionInSync, Status: metav1.ConditionTrue, Reason: "InSync", ObservedGeneration: generation,
	}
	if len(drift) > 0 {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "SpecDiffers"
		condition.Message = "not applied by this version of the operator: " + strings.Join(drift, "; ")
	}
	apimeta.SetStatusCondition(conditions, condition)
}

func (r *AksClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("aks-access")
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.AksCluster{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
