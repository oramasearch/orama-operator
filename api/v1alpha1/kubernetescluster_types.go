/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The kinds in this file are the abstract half of the operator's infrastructure work
// (operator#351, #354). A KubernetesCluster says what cluster is wanted in provider-neutral
// terms and names a KubernetesClusterProvider; the provider's type decides which concrete kind
// is provisioned (AksCluster for type aks). Nothing here names a vendor's API.

// KubernetesClusterProviderType is the vendor a provider resource stands for.
// +kubebuilder:validation:Enum=aks
type KubernetesClusterProviderType string

const (
	// KubernetesClusterProviderAKS provisions an AksCluster.
	KubernetesClusterProviderAKS KubernetesClusterProviderType = "aks"
)

// ClusterDeletionPolicy decides what happens to the real cluster when its resource is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type ClusterDeletionPolicy string

const (
	// ClusterDeletionDelete destroys the real cluster. Clusters are ephemeral (operator#351).
	ClusterDeletionDelete ClusterDeletionPolicy = "Delete"
	// ClusterDeletionRetain leaves the real cluster running and only releases the resource.
	ClusterDeletionRetain ClusterDeletionPolicy = "Retain"
)

// ClusterNodePoolRole is what a node pool is for.
// +kubebuilder:validation:Enum=system;user
type ClusterNodePoolRole string

const (
	// ClusterNodePoolSystem runs the cluster's own components. A cluster needs at least one.
	ClusterNodePoolSystem ClusterNodePoolRole = "system"
	// ClusterNodePoolUser runs workloads.
	ClusterNodePoolUser ClusterNodePoolRole = "user"
)

// ClusterSecretReference names a Secret in a namespace. The kinds here are cluster-scoped, so a
// Secret reference always carries its namespace.
type ClusterSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// ClusterNodePool is one pool of nodes, in provider-neutral terms.
type ClusterNodePool struct {
	// Name of the pool. AKS allows 1 to 12 lowercase letters and digits, starting with a letter.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]{0,11}$`
	Name string `json:"name"`
	// Role is system or user.
	Role ClusterNodePoolRole `json:"role"`
	// MachineSize is the provider's name for the machine type, such as Standard_D8as_v7. The
	// value is the vendor's word; the field is neutral.
	// +kubebuilder:validation:MinLength=1
	MachineSize string `json:"machineSize"`
	// Count is the number of nodes when the pool does not autoscale. Ignored when maxCount is set.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Count *int32 `json:"count,omitempty"`
	// MinCount and MaxCount turn on the provider's node autoscaling between the two. This is the
	// pool's bound; autoscaling inside the bound belongs to the cluster itself.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinCount *int32 `json:"minCount,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxCount *int32 `json:"maxCount,omitempty"`
	// Zones are the availability zones the pool's nodes may use. Empty means the provider chooses.
	// +optional
	Zones []string `json:"zones,omitempty"`
	// Labels are added to every node of the pool.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Taints are added to every node of the pool.
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
}

// KubernetesClusterSpec is the cluster that is wanted.
type KubernetesClusterSpec struct {
	// ProviderRef names the KubernetesClusterProvider that provisions this cluster. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="providerRef is immutable"
	ProviderRef string `json:"providerRef"`
	// Region is the provider's name for where the cluster runs, such as westus2. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
	Region string `json:"region"`
	// KubernetesVersion is the version wanted, such as 1.35 or 1.35.8.
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+(\.[0-9]+)?$`
	KubernetesVersion string `json:"kubernetesVersion"`
	// NodePools are the cluster's pools. At least one must have role system.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	NodePools []ClusterNodePool `json:"nodePools"`
	// DeletionPolicy is required and has no default: whoever writes the resource chooses whether
	// deleting it destroys the cluster (Delete) or leaves it running (Retain).
	DeletionPolicy ClusterDeletionPolicy `json:"deletionPolicy"`
	// KubeconfigSecretRef is where the cluster's admin kubeconfig is written (key `kubeconfig`).
	// Defaults to a Secret named <name>-kubeconfig in the provider's credentials namespace.
	// +optional
	KubeconfigSecretRef *ClusterSecretReference `json:"kubeconfigSecretRef,omitempty"`
	// Tags are attached to the cluster where the provider supports them.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
}

// ClusterNodePoolStatus is what is true of one pool.
type ClusterNodePoolStatus struct {
	Name string `json:"name"`
	// State is the provider's provisioning state for the pool.
	// +optional
	State string `json:"state,omitempty"`
	// Count is the number of nodes the provider reports.
	// +optional
	Count int32 `json:"count,omitempty"`
}

// KubernetesClusterStatus is what is true of the cluster, summarized from the concrete resource
// so that a person only needs to read this kind.
type KubernetesClusterStatus struct {
	// Phase is Pending, Provisioning, Ready, Degraded or Deleting.
	// +optional
	Phase string `json:"phase,omitempty"`
	// ProviderType is the type of the resolved provider.
	// +optional
	ProviderType KubernetesClusterProviderType `json:"providerType,omitempty"`
	// ConcreteRef names the concrete resource provisioned for this cluster, as Kind/name.
	// +optional
	ConcreteRef string `json:"concreteRef,omitempty"`
	// Endpoint is the address of the cluster's API server.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// KubernetesVersion is the version the cluster is running.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// KubeconfigSecretRef is where the admin kubeconfig was written.
	// +optional
	KubeconfigSecretRef *ClusterSecretReference `json:"kubeconfigSecretRef,omitempty"`
	// NodePools reports each pool.
	// +optional
	// +listType=map
	// +listMapKey=name
	NodePools []ClusterNodePoolStatus `json:"nodePools,omitempty"`
	// Message says, in a sentence, why the cluster is not Ready.
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready, and InSync (false when the spec asks for something the cluster does not
	// have and this version of the operator does not apply).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=kc
//+kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef`
//+kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.kubernetesVersion`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1

// KubernetesCluster is a Kubernetes cluster the operator provisions and keeps as described. It is
// the abstract kind: it names a KubernetesClusterProvider, and that provider's type decides the
// concrete kind created for it. Reconciled by the management cluster's operator.
type KubernetesCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubernetesClusterSpec   `json:"spec,omitempty"`
	Status KubernetesClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KubernetesClusterList is a list of KubernetesCluster.
type KubernetesClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubernetesCluster `json:"items"`
}

// AksProviderSettings is what an AKS provider needs: the account, and the settings that have no
// provider-neutral equivalent. Every cluster that points at the provider gets them.
type AksProviderSettings struct {
	// SubscriptionID is the Azure subscription clusters are created in.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F-]{36}$`
	SubscriptionID string `json:"subscriptionID"`
	// ResourceGroupPrefix names each cluster's resource group as <prefix><cluster name>.
	// Defaults to empty, with the suffix below.
	// +optional
	ResourceGroupPrefix string `json:"resourceGroupPrefix,omitempty"`
	// ResourceGroupSuffix defaults to "_group", matching orama-production-cluster-01_group.
	// +kubebuilder:default="_group"
	// +optional
	ResourceGroupSuffix string `json:"resourceGroupSuffix,omitempty"`
	// SKUTier is Free, Standard or Premium.
	// +kubebuilder:validation:Enum=Free;Standard;Premium
	// +kubebuilder:default=Standard
	// +optional
	SKUTier string `json:"skuTier,omitempty"`
	// Network settings for every cluster of this provider.
	// +optional
	Network AksNetworkSettings `json:"network,omitempty"`
	// UpgradeChannel is the cluster auto-upgrade channel: none, patch, stable, rapid, node-image.
	// +kubebuilder:validation:Enum=none;patch;stable;rapid;node-image
	// +kubebuilder:default=patch
	// +optional
	UpgradeChannel string `json:"upgradeChannel,omitempty"`
	// NodeOSUpgradeChannel is None, Unmanaged, SecurityPatch or NodeImage.
	// +kubebuilder:validation:Enum=None;Unmanaged;SecurityPatch;NodeImage
	// +kubebuilder:default=NodeImage
	// +optional
	NodeOSUpgradeChannel string `json:"nodeOSUpgradeChannel,omitempty"`
	// OSDiskSizeGB is each node's OS disk size.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:default=256
	// +optional
	OSDiskSizeGB int32 `json:"osDiskSizeGB,omitempty"`
	// WorkloadIdentity turns on the OIDC issuer and workload identity in each cluster.
	// +kubebuilder:default=true
	// +optional
	WorkloadIdentity *bool `json:"workloadIdentity,omitempty"`
}

// AksNetworkSettings mirrors the AKS network profile fields the operator sets.
type AksNetworkSettings struct {
	// +kubebuilder:default=azure
	// +optional
	Plugin string `json:"plugin,omitempty"`
	// +kubebuilder:default=overlay
	// +optional
	PluginMode string `json:"pluginMode,omitempty"`
	// +kubebuilder:default=cilium
	// +optional
	Dataplane string `json:"dataplane,omitempty"`
	// +kubebuilder:default=cilium
	// +optional
	Policy string `json:"policy,omitempty"`
	// +kubebuilder:default="10.244.0.0/16"
	// +optional
	PodCIDR string `json:"podCIDR,omitempty"`
	// +kubebuilder:default="10.0.0.0/16"
	// +optional
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
	// +kubebuilder:default="10.0.0.10"
	// +optional
	DNSServiceIP string `json:"dnsServiceIP,omitempty"`
}

// KubernetesClusterProviderSpec says which vendor provisions clusters, in which account, and how
// the operator authenticates to it.
// +kubebuilder:validation:XValidation:rule="self.type != 'aks' || has(self.aks)",message="spec.aks is required when type is aks"
type KubernetesClusterProviderSpec struct {
	// Type is the vendor. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type KubernetesClusterProviderType `json:"type"`
	// CredentialsSecretRef names the Secret the operator authenticates with. For type aks its
	// keys are tenantID and clientID, plus clientSecret for a service principal. Without
	// clientSecret the operator uses workload identity: it exchanges its projected
	// service-account token for an Azure token.
	CredentialsSecretRef ClusterSecretReference `json:"credentialsSecretRef"`
	// AKS holds the settings for type aks.
	// +optional
	AKS *AksProviderSettings `json:"aks,omitempty"`
}

// KubernetesClusterProviderStatus reports whether the provider can be used.
type KubernetesClusterProviderStatus struct {
	// Ready is true once the credentials were used successfully.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// AuthMethod is workload-identity or service-principal, as read from the credentials Secret.
	// +optional
	AuthMethod string `json:"authMethod,omitempty"`
	// Clusters is the number of KubernetesCluster resources that point at this provider.
	// +optional
	Clusters int32 `json:"clusters,omitempty"`
	// Message says why the provider is not Ready.
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=kcp
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
//+kubebuilder:printcolumn:name="Auth",type=string,JSONPath=`.status.authMethod`
//+kubebuilder:printcolumn:name="Clusters",type=integer,JSONPath=`.status.clusters`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// KubernetesClusterProvider is where a vendor, an account and its credentials are written once.
// A KubernetesCluster points at one, and the provider's type decides the concrete kind that is
// provisioned. Swapping a provider means pointing at a different one.
type KubernetesClusterProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubernetesClusterProviderSpec   `json:"spec,omitempty"`
	Status KubernetesClusterProviderStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KubernetesClusterProviderList is a list of KubernetesClusterProvider.
type KubernetesClusterProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubernetesClusterProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&KubernetesCluster{}, &KubernetesClusterList{},
		&KubernetesClusterProvider{}, &KubernetesClusterProviderList{},
	)
}
