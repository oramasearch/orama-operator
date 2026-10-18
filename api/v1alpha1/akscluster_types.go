/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AksCluster is the concrete kind behind a KubernetesCluster whose provider has type aks
// (operator#354). Its spec is complete on its own: everything Azure needs to create one managed
// cluster. It is the only kind whose controller calls Azure.

// AksNodePool is one AKS agent pool.
type AksNodePool struct {
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]{0,11}$`
	Name string `json:"name"`
	// Mode is System or User.
	// +kubebuilder:validation:Enum=System;User
	Mode string `json:"mode"`
	// +kubebuilder:validation:MinLength=1
	VMSize string `json:"vmSize"`
	// Count is the node count, and the starting count when autoscaling.
	// +kubebuilder:validation:Minimum=0
	Count int32 `json:"count"`
	// EnableAutoScaling turns on the AKS cluster autoscaler for the pool, between MinCount and
	// MaxCount.
	// +optional
	EnableAutoScaling bool `json:"enableAutoScaling,omitempty"`
	// +optional
	MinCount *int32 `json:"minCount,omitempty"`
	// +optional
	MaxCount *int32 `json:"maxCount,omitempty"`
	// +kubebuilder:validation:Minimum=30
	// +optional
	OSDiskSizeGB int32 `json:"osDiskSizeGB,omitempty"`
	// +optional
	AvailabilityZones []string `json:"availabilityZones,omitempty"`
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	// +optional
	NodeTaints []corev1.Taint `json:"nodeTaints,omitempty"`
	// Sysctls uses Linux kernel names and maps to linuxOSConfig.sysctls in the AKS API.
	// Unsupported names or values are refused before any Azure request. Existing pools are
	// observed only; changes to these settings are reported by InSync.
	// +optional
	Sysctls NodeSysctls `json:"sysctls,omitempty"`
}

// AksClusterSpec is one AKS managed cluster.
type AksClusterSpec struct {
	// SubscriptionID, ResourceGroup and Location say where the cluster lives. Immutable.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F-]{36}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="subscriptionID is immutable"
	SubscriptionID string `json:"subscriptionID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="resourceGroup is immutable"
	ResourceGroup string `json:"resourceGroup"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="location is immutable"
	Location string `json:"location"`
	// ClusterName is the managed cluster's name in Azure. Defaults to the resource's name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
	// CredentialsSecretRef names the Secret the operator authenticates to Azure with: tenantID
	// and clientID, plus clientSecret for a service principal.
	CredentialsSecretRef ClusterSecretReference `json:"credentialsSecretRef"`
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+(\.[0-9]+)?$`
	KubernetesVersion string `json:"kubernetesVersion"`
	// +kubebuilder:validation:Enum=Free;Standard;Premium
	// +kubebuilder:default=Standard
	// +optional
	SKUTier string `json:"skuTier,omitempty"`
	// +optional
	Network AksNetworkSettings `json:"network,omitempty"`
	// +kubebuilder:validation:Enum=none;patch;stable;rapid;node-image
	// +optional
	UpgradeChannel string `json:"upgradeChannel,omitempty"`
	// +kubebuilder:validation:Enum=None;Unmanaged;SecurityPatch;NodeImage
	// +optional
	NodeOSUpgradeChannel string `json:"nodeOSUpgradeChannel,omitempty"`
	// WorkloadIdentity turns on the OIDC issuer and workload identity.
	// +optional
	WorkloadIdentity bool `json:"workloadIdentity,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	NodePools []AksNodePool `json:"nodePools"`
	// DeletionPolicy is required: Delete destroys the managed cluster and its resource group,
	// Retain leaves both.
	DeletionPolicy ClusterDeletionPolicy `json:"deletionPolicy"`
	// KubeconfigSecretRef is where the admin kubeconfig is written (key `kubeconfig`).
	KubeconfigSecretRef ClusterSecretReference `json:"kubeconfigSecretRef"`
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
}

// AksNodePoolStatus is Azure's view of one agent pool.
type AksNodePoolStatus struct {
	Name string `json:"name"`
	// +optional
	ProvisioningState string `json:"provisioningState,omitempty"`
	// +optional
	Count int32 `json:"count,omitempty"`
	// Error is the provisioning error Azure reports for the pool, as "code: message".
	// +optional
	Error string `json:"error,omitempty"`
}

// AksOperationStatus is the cluster's most recent long-running operation, as Azure reports it.
type AksOperationStatus struct {
	// ID is the operation's identifier, to quote to Azure support.
	// +optional
	ID string `json:"id,omitempty"`
	// Status is Azure's own text: a progress line while the operation runs (for example
	// "CreatingAgentPools: 0/1 nodes completed"), then Succeeded, Failed or Canceled.
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	StartTime string `json:"startTime,omitempty"`
	// +optional
	EndTime string `json:"endTime,omitempty"`
	// Error is the error Azure attached to the operation, as "code: message", with the details
	// Azure nests under it. Empty when Azure reports none.
	// +optional
	Error string `json:"error,omitempty"`
}

// AksClusterStatus is what Azure reports.
type AksClusterStatus struct {
	// Phase is Pending, Provisioning, Ready, Degraded or Deleting.
	// +optional
	Phase string `json:"phase,omitempty"`
	// ProvisioningState is Azure's own word: Creating, Succeeded, Updating, Failed, Deleting.
	// +optional
	ProvisioningState string `json:"provisioningState,omitempty"`
	// PowerState is Running or Stopped.
	// +optional
	PowerState string `json:"powerState,omitempty"`
	// ResourceID is the managed cluster's Azure resource ID.
	// +optional
	ResourceID string `json:"resourceID,omitempty"`
	// FQDN is the API server's host name.
	// +optional
	FQDN string `json:"fqdn,omitempty"`
	// KubernetesVersion is the version Azure reports as current.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// NodeResourceGroup is the group Azure creates for the cluster's nodes.
	// +optional
	NodeResourceGroup string `json:"nodeResourceGroup,omitempty"`
	// OIDCIssuerURL is set when workload identity is on.
	// +optional
	OIDCIssuerURL string `json:"oidcIssuerURL,omitempty"`
	// CreatedResourceGroup is true when the operator created the resource group, and so may
	// delete it.
	// +optional
	CreatedResourceGroup bool `json:"createdResourceGroup,omitempty"`
	// KubeconfigWritten is true once the admin kubeconfig Secret holds a credential.
	// +optional
	KubeconfigWritten bool `json:"kubeconfigWritten,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	NodePools []AksNodePoolStatus `json:"nodePools,omitempty"`
	// Operation is the latest operation Azure ran on the cluster: its progress while it runs and
	// its error when it fails.
	// +optional
	Operation *AksOperationStatus `json:"operation,omitempty"`
	// ProvisioningError is the provisioning error Azure reports on the cluster itself.
	// +optional
	ProvisioningError string `json:"provisioningError,omitempty"`
	// Message says why the cluster is not Ready, in Azure's words where Azure gave any.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready, and InSync.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=aks
//+kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.location`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.kubernetesVersion`
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.provisioningState`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.status.operation.status`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// AksCluster is one AKS managed cluster. It is the concrete kind a KubernetesCluster provisions
// when its provider has type aks, and the only kind whose controller calls Azure.
type AksCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AksClusterSpec   `json:"spec,omitempty"`
	Status AksClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AksClusterList is a list of AksCluster.
type AksClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AksCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AksCluster{}, &AksClusterList{})
}
