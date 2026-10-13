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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// kubernetesClusterFinalizer holds a KubernetesCluster until its concrete resource is gone,
	// so its status can say Deleting for as long as the real cluster is being deleted.
	kubernetesClusterFinalizer = "orama.cloud/kubernetes-cluster-cleanup"
	// kubernetesClusterProviderFinalizer keeps a provider while a cluster points at it.
	kubernetesClusterProviderFinalizer = "orama.cloud/kubernetes-cluster-provider-in-use"
)

// KubernetesClusterReconciler reconciles the abstract kind (operator#351, #354). It resolves the
// cluster's provider, provisions the concrete kind that provider's type names, and summarizes
// the concrete resource's status. It never calls a vendor's API.
type KubernetesClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusterproviders,verbs=get;list;watch

func (r *KubernetesClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster oramav1alpha1.KubernetesCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := cluster.Status.DeepCopy()

	if !cluster.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cluster, observed)
	}
	if !controllerutil.ContainsFinalizer(&cluster, kubernetesClusterFinalizer) {
		controllerutil.AddFinalizer(&cluster, kubernetesClusterFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	if problem := validateKubernetesCluster(&cluster); problem != "" {
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "InvalidSpec", problem)
		cluster.Status.ObservedGeneration = cluster.Generation
		return ctrl.Result{}, r.writeStatus(ctx, &cluster, observed)
	}

	var provider oramav1alpha1.KubernetesClusterProvider
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Spec.ProviderRef}, &provider); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// A provider that appears later triggers a reconcile through the watch below.
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhasePending, "ProviderNotFound",
			fmt.Sprintf("KubernetesClusterProvider %q does not exist", cluster.Spec.ProviderRef))
		cluster.Status.ObservedGeneration = cluster.Generation
		return ctrl.Result{}, r.writeStatus(ctx, &cluster, observed)
	}
	cluster.Status.ProviderType = provider.Spec.Type

	switch provider.Spec.Type {
	case oramav1alpha1.KubernetesClusterProviderAKS:
		if err := r.reconcileAKS(ctx, &cluster, &provider); err != nil {
			setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
				cluster.Generation, clusterPhaseDegraded, "ProvisionFailed", err.Error())
			if statusErr := r.writeStatus(ctx, &cluster, observed); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
	default:
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDegraded, "UnsupportedProvider",
			fmt.Sprintf("this operator has no concrete kind for provider type %q", provider.Spec.Type))
	}
	cluster.Status.ObservedGeneration = cluster.Generation
	return ctrl.Result{}, r.writeStatus(ctx, &cluster, observed)
}

// validateKubernetesCluster checks what the schema cannot.
func validateKubernetesCluster(cluster *oramav1alpha1.KubernetesCluster) string {
	system := false
	for _, pool := range cluster.Spec.NodePools {
		if pool.Role == oramav1alpha1.ClusterNodePoolSystem {
			system = true
		}
		switch {
		case pool.MaxCount != nil:
			min := int32(0)
			if pool.MinCount != nil {
				min = *pool.MinCount
			}
			if min > *pool.MaxCount {
				return fmt.Sprintf("node pool %s: minCount %d is greater than maxCount %d", pool.Name, min, *pool.MaxCount)
			}
			if pool.Role == oramav1alpha1.ClusterNodePoolSystem && min < 1 {
				return fmt.Sprintf("node pool %s: a system pool needs minCount of at least 1", pool.Name)
			}
		case pool.MinCount != nil:
			return fmt.Sprintf("node pool %s: minCount is set without maxCount", pool.Name)
		case pool.Count == nil:
			return fmt.Sprintf("node pool %s: set count, or minCount and maxCount", pool.Name)
		case pool.Role == oramav1alpha1.ClusterNodePoolSystem && *pool.Count < 1:
			return fmt.Sprintf("node pool %s: a system pool needs at least 1 node", pool.Name)
		}
	}
	if !system {
		return "at least one node pool must have role system"
	}
	return ""
}

// reconcileAKS provisions the AksCluster for this cluster and summarizes its status.
func (r *KubernetesClusterReconciler) reconcileAKS(ctx context.Context, cluster *oramav1alpha1.KubernetesCluster, provider *oramav1alpha1.KubernetesClusterProvider) error {
	if provider.Spec.AKS == nil {
		return fmt.Errorf("KubernetesClusterProvider %q has type aks and no spec.aks", provider.Name)
	}
	// Azure's tag rules are the provider's, so they are checked where the provider is known.
	if problem := azureTagProblem(cluster.Spec.Tags); problem != "" {
		return fmt.Errorf("spec.tags: %s", problem)
	}
	desired := aksClusterSpecFor(cluster, provider)

	concrete := &oramav1alpha1.AksCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, concrete, func() error {
		if !concrete.CreationTimestamp.IsZero() && !metav1.IsControlledBy(concrete, cluster) {
			return fmt.Errorf("AksCluster %q exists and is not owned by this KubernetesCluster", concrete.Name)
		}
		concrete.Spec = desired
		return controllerutil.SetControllerReference(cluster, concrete, r.Scheme)
	})
	if err != nil {
		return err
	}

	status := &cluster.Status
	status.ConcreteRef = "AksCluster/" + concrete.Name
	status.Phase = concrete.Status.Phase
	if status.Phase == "" {
		status.Phase = clusterPhasePending
	}
	status.Message = concrete.Status.Message
	status.KubernetesVersion = concrete.Status.KubernetesVersion
	status.Endpoint = ""
	if concrete.Status.FQDN != "" {
		status.Endpoint = "https://" + concrete.Status.FQDN + ":443"
	}
	status.KubeconfigSecretRef = nil
	if concrete.Status.KubeconfigWritten {
		ref := desired.KubeconfigSecretRef
		status.KubeconfigSecretRef = &ref
	}
	pools := make([]oramav1alpha1.ClusterNodePoolStatus, 0, len(concrete.Status.NodePools))
	for _, pool := range concrete.Status.NodePools {
		pools = append(pools, oramav1alpha1.ClusterNodePoolStatus{Name: pool.Name, State: pool.ProvisioningState, Count: pool.Count})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	status.NodePools = pools

	// Carry the concrete resource's conditions across, restamped with this generation.
	for _, kind := range []string{clusterConditionReady, clusterConditionInSync} {
		from := apimeta.FindStatusCondition(concrete.Status.Conditions, kind)
		if from == nil {
			if kind == clusterConditionReady {
				apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type: kind, Status: metav1.ConditionFalse, Reason: "Pending",
					Message: "the concrete resource has not reported yet", ObservedGeneration: cluster.Generation,
				})
			}
			continue
		}
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: kind, Status: from.Status, Reason: from.Reason, Message: from.Message, ObservedGeneration: cluster.Generation,
		})
	}
	return nil
}

// aksClusterSpecFor maps the neutral cluster and the provider's settings onto an AksCluster. It is
// a pure function, so the same pair always produces the same concrete resource.
func aksClusterSpecFor(cluster *oramav1alpha1.KubernetesCluster, provider *oramav1alpha1.KubernetesClusterProvider) oramav1alpha1.AksClusterSpec {
	settings := provider.Spec.AKS
	suffix := settings.ResourceGroupSuffix
	if suffix == "" && settings.ResourceGroupPrefix == "" {
		suffix = "_group"
	}
	spec := oramav1alpha1.AksClusterSpec{
		SubscriptionID:       settings.SubscriptionID,
		ResourceGroup:        settings.ResourceGroupPrefix + cluster.Name + suffix,
		Location:             cluster.Spec.Region,
		ClusterName:          cluster.Name,
		CredentialsSecretRef: provider.Spec.CredentialsSecretRef,
		KubernetesVersion:    cluster.Spec.KubernetesVersion,
		SKUTier:              settings.SKUTier,
		Network:              settings.Network,
		UpgradeChannel:       settings.UpgradeChannel,
		NodeOSUpgradeChannel: settings.NodeOSUpgradeChannel,
		WorkloadIdentity:     settings.WorkloadIdentity == nil || *settings.WorkloadIdentity,
		DeletionPolicy:       cluster.Spec.DeletionPolicy,
		Tags:                 cluster.Spec.Tags,
	}
	if cluster.Spec.KubeconfigSecretRef != nil {
		spec.KubeconfigSecretRef = *cluster.Spec.KubeconfigSecretRef
	} else {
		spec.KubeconfigSecretRef = oramav1alpha1.ClusterSecretReference{
			Name:      cluster.Name + "-kubeconfig",
			Namespace: provider.Spec.CredentialsSecretRef.Namespace,
		}
	}
	for _, pool := range cluster.Spec.NodePools {
		mode := "User"
		if pool.Role == oramav1alpha1.ClusterNodePoolSystem {
			mode = "System"
		}
		concrete := oramav1alpha1.AksNodePool{
			Name:              pool.Name,
			Mode:              mode,
			VMSize:            pool.MachineSize,
			OSDiskSizeGB:      settings.OSDiskSizeGB,
			AvailabilityZones: pool.Zones,
			NodeLabels:        pool.Labels,
			NodeTaints:        pool.Taints,
		}
		if pool.MaxCount != nil {
			min := int32(0)
			if pool.MinCount != nil {
				min = *pool.MinCount
			}
			max := *pool.MaxCount
			concrete.EnableAutoScaling = true
			concrete.MinCount = &min
			concrete.MaxCount = &max
			// The starting count: what was asked for if it is inside the range, else the minimum.
			concrete.Count = min
			if pool.Count != nil && *pool.Count >= min && *pool.Count <= max {
				concrete.Count = *pool.Count
			}
		} else if pool.Count != nil {
			concrete.Count = *pool.Count
		}
		spec.NodePools = append(spec.NodePools, concrete)
	}
	return spec
}

// finalize deletes the concrete resource and waits for it to be gone. The concrete resource's own
// finalizer is what honors the deletion policy against the vendor.
func (r *KubernetesClusterReconciler) finalize(ctx context.Context, cluster *oramav1alpha1.KubernetesCluster, observed *oramav1alpha1.KubernetesClusterStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, kubernetesClusterFinalizer) {
		return ctrl.Result{}, nil
	}
	concrete := &oramav1alpha1.AksCluster{}
	err := r.Get(ctx, types.NamespacedName{Name: cluster.Name}, concrete)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil && metav1.IsControlledBy(concrete, cluster) {
		if concrete.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, concrete); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		message := concrete.Status.Message
		if message == "" {
			message = "waiting for the concrete resource to be deleted"
		}
		setClusterStatus(&cluster.Status.Phase, &cluster.Status.Message, &cluster.Status.Conditions,
			cluster.Generation, clusterPhaseDeleting, "Deleting", message)
		// The concrete resource's removal triggers the next reconcile through the Owns watch.
		return ctrl.Result{}, r.writeStatus(ctx, cluster, observed)
	}
	controllerutil.RemoveFinalizer(cluster, kubernetesClusterFinalizer)
	return ctrl.Result{}, r.Update(ctx, cluster)
}

func (r *KubernetesClusterReconciler) writeStatus(ctx context.Context, cluster *oramav1alpha1.KubernetesCluster, observed *oramav1alpha1.KubernetesClusterStatus) error {
	if reflect.DeepEqual(&cluster.Status, observed) {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}

func (r *KubernetesClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.KubernetesCluster{}).
		Owns(&oramav1alpha1.AksCluster{}).
		Watches(&oramav1alpha1.KubernetesClusterProvider{}, handler.EnqueueRequestsFromMapFunc(r.clustersOfProvider)).
		Complete(r)
}

// clustersOfProvider requeues every cluster that points at a provider when the provider changes.
func (r *KubernetesClusterReconciler) clustersOfProvider(ctx context.Context, provider client.Object) []reconcile.Request {
	var clusters oramav1alpha1.KubernetesClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, cluster := range clusters.Items {
		if cluster.Spec.ProviderRef == provider.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}})
		}
	}
	return requests
}

// KubernetesClusterProviderReconciler checks that a provider's credentials work and keeps the
// provider while a cluster points at it.
type KubernetesClusterProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Azure is the Azure Resource Manager client; nil uses the public cloud endpoints.
	Azure *azureClient
}

//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusterproviders,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusterproviders/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusterproviders/finalizers,verbs=update

func (r *KubernetesClusterProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var provider oramav1alpha1.KubernetesClusterProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := provider.Status.DeepCopy()

	var clusters oramav1alpha1.KubernetesClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return ctrl.Result{}, err
	}
	inUse := int32(0)
	for _, cluster := range clusters.Items {
		if cluster.Spec.ProviderRef == provider.Name {
			inUse++
		}
	}
	provider.Status.Clusters = inUse

	if !provider.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&provider, kubernetesClusterProviderFinalizer) {
			return ctrl.Result{}, nil
		}
		if inUse > 0 {
			provider.Status.Ready = false
			provider.Status.Message = fmt.Sprintf("cannot be deleted while %d KubernetesCluster resource(s) point at it", inUse)
			return ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &provider, observed)
		}
		controllerutil.RemoveFinalizer(&provider, kubernetesClusterProviderFinalizer)
		return ctrl.Result{}, r.Update(ctx, &provider)
	}
	if !controllerutil.ContainsFinalizer(&provider, kubernetesClusterProviderFinalizer) {
		controllerutil.AddFinalizer(&provider, kubernetesClusterProviderFinalizer)
		if err := r.Update(ctx, &provider); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set again: registering the finalizer above replaced the object with the server's copy.
	provider.Status.Clusters = inUse
	provider.Status.ObservedGeneration = provider.Generation
	switch provider.Spec.Type {
	case oramav1alpha1.KubernetesClusterProviderAKS:
		creds, err := readAzureCredentials(ctx, r.Client, provider.Spec.CredentialsSecretRef)
		if err != nil {
			provider.Status.Ready, provider.Status.AuthMethod, provider.Status.Message = false, "", err.Error()
			return ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &provider, observed)
		}
		provider.Status.AuthMethod = creds.authMethod()
		if provider.Spec.AKS == nil {
			provider.Status.Ready, provider.Status.Message = false, "spec.aks is required when type is aks"
			return ctrl.Result{}, r.writeStatus(ctx, &provider, observed)
		}
		if r.Azure == nil {
			r.Azure = newAzureClient()
		}
		if err := r.Azure.subscriptionReachable(ctx, creds, provider.Spec.AKS.SubscriptionID); err != nil {
			provider.Status.Ready, provider.Status.Message = false, err.Error()
			return ctrl.Result{RequeueAfter: aksPollInterval}, r.writeStatus(ctx, &provider, observed)
		}
		provider.Status.Ready, provider.Status.Message = true, ""
	default:
		provider.Status.Ready = false
		provider.Status.Message = fmt.Sprintf("this operator does not know provider type %q", provider.Spec.Type)
	}
	return ctrl.Result{RequeueAfter: aksSteadyRecheck}, r.writeStatus(ctx, &provider, observed)
}

func (r *KubernetesClusterProviderReconciler) writeStatus(ctx context.Context, provider *oramav1alpha1.KubernetesClusterProvider, observed *oramav1alpha1.KubernetesClusterProviderStatus) error {
	if reflect.DeepEqual(&provider.Status, observed) {
		return nil
	}
	return r.Status().Update(ctx, provider)
}

func (r *KubernetesClusterProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.KubernetesClusterProvider{}).
		Watches(&oramav1alpha1.KubernetesCluster{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, cluster client.Object) []reconcile.Request {
				kc, ok := cluster.(*oramav1alpha1.KubernetesCluster)
				if !ok || kc.Spec.ProviderRef == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: kc.Spec.ProviderRef}}}
			})).
		Complete(r)
}
