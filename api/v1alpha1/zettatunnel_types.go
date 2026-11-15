// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TunnelGrantSpec is the part of a Tunnel every concrete provider carries: who may reach which
// one target, until when.
type TunnelGrantSpec struct {
	// LakeRef names the cluster-scoped Lake whose control plane granted the tunnel.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lakeRef is immutable"
	LakeRef string `json:"lakeRef"`

	// GrantID is the immutable network_tunnel_grants row id.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="grantId is immutable"
	GrantID string `json:"grantId"`

	// CustomerID is Lake's immutable customer id.
	// +kubebuilder:validation:Format=uuid
	CustomerID string `json:"customerId"`

	// WorkspaceID is Lake's immutable workspace id.
	// +kubebuilder:validation:Format=uuid
	WorkspaceID string `json:"workspaceId"`

	// TargetKind identifies the workload family selected by TargetID.
	// +kubebuilder:validation:Enum=service;app
	TargetKind string `json:"targetKind"`

	// TargetID is the Lake id of the one service or app the tunnel reaches.
	// +kubebuilder:validation:Format=uuid
	TargetID string `json:"targetId"`

	// Port is the target's Kubernetes Service port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// ExpiresAt ends the grant.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// ZettaTunnelSpec is one grant carried by Orama's own zetta-tunnel: an endpoint in the workspace
// that reaches only the target, and an entry in the cluster's relay.
type ZettaTunnelSpec struct {
	TunnelGrantSpec `json:",inline"`

	// ClientCAPEM is the public CA (PEM) that signs the grant's developer certificates. The relay
	// trusts the union of these over the live grants.
	// +kubebuilder:validation:MinLength=1
	ClientCAPEM string `json:"clientCAPEM"`
}

// ZettaTunnelStatus reports the endpoint and whether the relay serves the grant.
type ZettaTunnelStatus struct {
	// Phase is Pending, Ready, Expired or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Endpoint is the relay's public host:port.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// RelayCAPEM is the CA of the relay's server certificate.
	// +optional
	RelayCAPEM string `json:"relayCAPEM,omitempty"`

	// Target is the in-cluster host:port the endpoint is fixed to.
	// +optional
	Target string `json:"target,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ztun
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZettaTunnel is a Tunnel carried by zetta-tunnel. A Tunnel creates and owns it.
type ZettaTunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZettaTunnelSpec   `json:"spec"`
	Status ZettaTunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZettaTunnelList contains a list of ZettaTunnel resources.
type ZettaTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZettaTunnel `json:"items"`
}

// CloudflareTunnelSpec is one grant carried by Cloudflare Tunnel in the customer's own account.
type CloudflareTunnelSpec struct {
	TunnelGrantSpec `json:",inline"`

	// ProviderRef names the cloudflare TunnelProvider in this namespace.
	// +kubebuilder:validation:MinLength=1
	ProviderRef string `json:"providerRef"`

	// Subject is the e-mail address Cloudflare Access admits, and nobody else.
	// +kubebuilder:validation:MinLength=3
	Subject string `json:"subject"`
}

// CloudflareTunnelStatus records the Cloudflare objects the controller made, so it can delete
// exactly those.
type CloudflareTunnelStatus struct {
	// Phase is Pending, Ready, Expired or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Endpoint is the Access-protected hostname.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// TunnelID is the Cloudflare tunnel id.
	// +optional
	TunnelID string `json:"tunnelId,omitempty"`

	// DNSRecordID is the CNAME that points the hostname at the tunnel.
	// +optional
	DNSRecordID string `json:"dnsRecordId,omitempty"`

	// DNSRecordOwnerComment is the ownership marker the operator wrote on that record
	// (orama-operator:<namespace>/<name>, operator#585). The record is deleted only while it still
	// carries it; empty for a record tracked before markers, which keeps its earlier cleanup.
	// +optional
	DNSRecordOwnerComment string `json:"dnsRecordOwnerComment,omitempty"`

	// AccessApplicationID is the Access application admitting only Subject.
	// +optional
	AccessApplicationID string `json:"accessApplicationId,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cftun
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareTunnel is a Tunnel carried by Cloudflare Tunnel in a customer's own Cloudflare account.
// A Tunnel whose provider is of type cloudflare creates and owns it.
type CloudflareTunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareTunnelSpec   `json:"spec"`
	Status CloudflareTunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareTunnelList contains a list of CloudflareTunnel resources.
type CloudflareTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareTunnel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZettaTunnel{}, &ZettaTunnelList{}, &CloudflareTunnel{}, &CloudflareTunnelList{})
}
