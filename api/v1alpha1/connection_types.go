// SPDX-License-Identifier: MIT

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

	ConnectionAccessRead  = "read"
	ConnectionAccessWrite = "write"
)

// ConnectionCredentialScope asks for a credential issued for this one connection, limited to what
// its source needs (operator#469, design decision D4). The operator creates a native role for the
// connection in the target datastore and delivers its credential as a workspace secret.
type ConnectionCredentialScope struct {
	// Access is read (query only) or write (read plus insert, update and delete; never DDL).
	// +kubebuilder:validation:Enum=read;write
	Access string `json:"access"`

	// Schema limits the role to one schema. Empty means every schema of the service's database.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_$]*$`
	Schema string `json:"schema,omitempty"`
}

// ConnectionSpec is one Lake-declared path between two workloads in the same workspace.
// +kubebuilder:validation:XValidation:rule="!has(self.credentialScope) || (self.sourceKind == 'app' && self.targetKind == 'service')",message="credentialScope is valid only from an app to a service"
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

	// CredentialScope, when set, issues a credential for this connection only: a role in the target
	// service's datastore with these grants, delivered as a workspace secret. It is valid only from
	// an app to a service. A new Generation rotates the credential.
	// +optional
	CredentialScope *ConnectionCredentialScope `json:"credentialScope,omitempty"`

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

	// CredentialSecret is the workspace secret holding the issued credential, as
	// lake-secret://<mount id>/<name>. It never holds the credential itself.
	// +optional
	CredentialSecret string `json:"credentialSecret,omitempty"`

	// CredentialGeneration is the Lake generation the current credential was issued for. A spec
	// Generation above it is a rotation request.
	// +optional
	CredentialGeneration int64 `json:"credentialGeneration,omitempty"`

	// CredentialRole is the datastore role issued for this connection.
	// +optional
	CredentialRole string `json:"credentialRole,omitempty"`

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
