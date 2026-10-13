/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

	// kubeconfigSecretKey is the key the admin kubeconfig is written under.
	kubeconfigSecretKey = "kubeconfig"

	aksPollInterval  = 30 * time.Second
	aksSteadyRecheck = 5 * time.Minute
)

// AksClusterReconciler creates, observes and deletes one AKS managed cluster (operator#354). It
// follows the operator's "external resource as an owned CRD" pattern: a finalizer registered
// before any external state exists, create only when absent, status written only on change.
//
// This version does not change a cluster after it exists. When the spec asks for something the
// cluster does not have, the InSync condition says what, and nothing is sent to Azure.
type AksClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Azure is the Azure Resource Manager client; nil uses the public cloud endpoints.
	Azure *azureClient
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

	created, err := az.ensureResourceGroup(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, spec.Location, spec.Tags)
	if err != nil {
		return r.fail(ctx, &cluster, observed, "ResourceGroupFailed", err)
	}
	if created {
		// Recorded before anything else, so a later delete knows the group is the operator's.
		cluster.Status.CreatedResourceGroup = true
		if err := r.writeStatus(ctx, &cluster, observed); err != nil {
			return settle(ctrl.Result{}, err)
		}
		observed = cluster.Status.DeepCopy()
	}

	found, err := az.getManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name)
	if err != nil {
		return r.fail(ctx, &cluster, observed, "ReadFailed", err)
	}
	if found == nil {
		if err := az.createManagedCluster(ctx, creds, spec.SubscriptionID, spec.ResourceGroup, name, azureManagedClusterBody(&cluster)); err != nil {
			return r.fail(ctx, &cluster, observed, "CreateFailed", err)
		}
		cluster.Status.ProvisioningState = "Creating"
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseProvisioning, "Creating", "Azure accepted the request to create the cluster")
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &cluster, observed))
	}

	r.observe(&cluster, found)

	switch state := found.Properties.ProvisioningState; state {
	case "Succeeded":
		if err := r.writeKubeconfig(ctx, &cluster, creds, name); err != nil {
			return r.fail(ctx, &cluster, observed, "KubeconfigFailed", err)
		}
		cluster.Status.KubeconfigWritten = true
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseReady, "Provisioned", "")
		setInSync(&cluster.Status.Conditions, cluster.Generation, aksDrift(&cluster, found))
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &cluster, observed))
	case "Failed", "Canceled":
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "Azure"+state, "Azure reports the cluster's provisioning state as "+state)
		cluster.Status.ObservedGeneration = cluster.Generation
		return settle(ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &cluster, observed))
	default:
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseProvisioning, "Azure"+state, "Azure reports the cluster's provisioning state as "+state)
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
	pools := make([]oramav1alpha1.AksNodePoolStatus, 0, len(props.AgentPoolProfiles))
	for _, pool := range props.AgentPoolProfiles {
		pools = append(pools, oramav1alpha1.AksNodePoolStatus{
			Name: pool.Name, ProvisioningState: pool.ProvisioningState, Count: pool.Count,
		})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	status.NodePools = pools
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

// writeKubeconfig fetches the admin credential and writes it to the kubeconfig Secret, which the
// AksCluster owns. The credential is never logged and never put in status.
func (r *AksClusterReconciler) writeKubeconfig(ctx context.Context, cluster *oramav1alpha1.AksCluster, creds azureCredentials, name string) error {
	ref := cluster.Spec.KubeconfigSecretRef
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, existing)
	if err == nil && len(existing.Data[kubeconfigSecretKey]) > 0 && cluster.Status.KubeconfigWritten {
		// Already delivered. The admin credential does not change unless it is rotated, and
		// re-fetching it on every reconcile would be an Azure call and a Secret write for nothing.
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	kubeconfig, err := r.azure().adminKubeconfig(ctx, creds, cluster.Spec.SubscriptionID, cluster.Spec.ResourceGroup, name)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Type = corev1.SecretTypeOpaque
		sec.Data[kubeconfigSecretKey] = kubeconfig
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels["orama.cloud/kubernetes-cluster"] = cluster.Name
		return controllerutil.SetControllerReference(cluster, sec, r.Scheme)
	})
	return err
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.AksCluster{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
