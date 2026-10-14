/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	ConnectionSourceApp     = "app"
	ConnectionSourceService = "service"
	ConnectionSourceAgent   = "agent"
	ConnectionSourceTunnel  = "tunnel"

	ConnectionTargetService = "service"
	ConnectionTargetApp     = "app"

	ConnectionDesiredPresent = "present"
	ConnectionDesiredAbsent  = "absent"

	ConnectionPhasePending = "Pending"
	ConnectionPhaseReady   = "Ready"
	ConnectionPhaseFailed  = "Failed"
	ConnectionPhaseDeleted = "Deleted"
)

// ConnectionSpec is one Lake-declared path between two workloads in the same workspace.
type ConnectionSpec struct {
	// LakeRef names the cluster-scoped Lake that materialized this connection.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lakeRef is immutable"
	LakeRef string `json:"lakeRef"`

	// ConnectionID is the immutable network_connections row id.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="connectionId is immutable"
	ConnectionID string `json:"connectionId"`

	// CustomerID is Lake's immutable customer id.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="customerId is immutable"
	CustomerID string `json:"customerId"`

	// WorkspaceID is Lake's immutable workspace id. The CR must live in that workspace's namespace.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspaceId is immutable"
	WorkspaceID string `json:"workspaceId"`

	// SourceKind identifies the workload family selected by SourceID.
	// +kubebuilder:validation:Enum=app;service;agent;tunnel
	SourceKind string `json:"sourceKind"`

	// SourceID is the Lake id of the source app, service, agent or tunnel grant.
	// +kubebuilder:validation:Format=uuid
	SourceID string `json:"sourceId"`

	// TargetKind identifies the workload family selected by TargetID.
	// +kubebuilder:validation:Enum=service;app
	TargetKind string `json:"targetKind"`

	// TargetID is the Lake id of the destination service or app.
	// +kubebuilder:validation:Format=uuid
	TargetID string `json:"targetId"`

	// Ports are destination TCP ports. Empty means every port exposed by the target's Kubernetes
	// Services; it never means every pod port.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports,omitempty"`

	// Desired removes the rendered path before the CR is deleted when set to absent.
	// +kubebuilder:validation:Enum=present;absent
	Desired string `json:"desired"`

	// Generation is Lake's desired-row generation, acknowledged in status and Postgres.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// ConnectionStatus is the stable observation returned to network_connection_status.
type ConnectionStatus struct {
	// Phase is Pending, Ready, Failed or Deleted.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase without exposing credentials.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the Lake desired-row generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=conn
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceKind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetKind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Connection permits one Lake workload to reach one target on explicit ports.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec"`
	Status ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionList contains a list of Connection resources.
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connection{}, &ConnectionList{})
}
