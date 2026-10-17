/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	TunnelTargetService = "service"
	TunnelTargetApp     = "app"

	TunnelPhasePending = "Pending"
	TunnelPhaseReady   = "Ready"
	TunnelPhaseExpired = "Expired"
	TunnelPhaseRevoked = "Revoked"
	TunnelPhaseFailed  = "Failed"

	// TunnelProviderZetta is Orama's own provider (zetta-tunnel's relay and endpoint roles).
	TunnelProviderZetta = "zetta"
	// TunnelProviderCloudflare is Cloudflare Tunnel in a customer's own Cloudflare account.
	TunnelProviderCloudflare = "cloudflare"
)

// TunnelSpec lets one person reach one target from outside the cluster until ExpiresAt.
// It names no vendor: the provider resource it points at decides what carries it.
type TunnelSpec struct {
	// LakeRef names the cluster-scoped Lake whose control plane granted this tunnel.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lakeRef is immutable"
	LakeRef string `json:"lakeRef"`

	// GrantID is the immutable network_tunnel_grants row id. It is the identity the developer's
	// client certificate carries (urn:orama:tunnel-grant:<id>).
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="grantId is immutable"
	GrantID string `json:"grantId"`

	// CustomerID is Lake's immutable customer id.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="customerId is immutable"
	CustomerID string `json:"customerId"`

	// WorkspaceID is Lake's immutable workspace id. The resource lives in that workspace's namespace.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspaceId is immutable"
	WorkspaceID string `json:"workspaceId"`

	// UserID is the Lake user the grant is for. Only that person can obtain its credential.
	// +kubebuilder:validation:Format=uuid
	UserID string `json:"userId"`

	// TargetKind identifies the workload family selected by TargetID.
	// +kubebuilder:validation:Enum=service;app
	TargetKind string `json:"targetKind"`

	// TargetID is the Lake id of the one service or app the tunnel reaches.
	// +kubebuilder:validation:Format=uuid
	TargetID string `json:"targetId"`

	// Port is the target's Kubernetes Service port the tunnel reaches, and nothing else.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// ExpiresAt ends the grant. The provider is torn down at that time.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// RevokedAt, when set, ends the grant at once.
	// +optional
	RevokedAt *metav1.Time `json:"revokedAt,omitempty"`

	// ClientCAPEM is the public CA certificate (PEM) that signs this grant's client certificates
	// (Lake's per-customer CA, lake migration 0085_network_tunnel_ca). Used by the ZettaTunnel
	// provider only.
	// +optional
	ClientCAPEM string `json:"clientCAPEM,omitempty"`

	// Subject is the e-mail address of the person, used only by providers that authenticate
	// people themselves (Cloudflare Access). Empty for ZettaTunnel.
	// +optional
	Subject string `json:"subject,omitempty"`

	// ProviderRef names a TunnelProvider in this namespace. Empty selects Orama's own provider,
	// ZettaTunnel.
	// +optional
	ProviderRef string `json:"providerRef,omitempty"`

	// Generation is Lake's desired-row generation, acknowledged in status and Postgres.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// TunnelStatus is the stable observation returned to network_tunnel_grant_status.
type TunnelStatus struct {
	// Phase is Pending, Ready, Expired, Revoked or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase without exposing credentials.
	// +optional
	Message string `json:"message,omitempty"`

	// Provider is the concrete kind carrying the tunnel: ZettaTunnel or CloudflareTunnel.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Endpoint is what the developer's client connects to: the relay's public host:port for
	// ZettaTunnel, the Access-protected hostname for CloudflareTunnel.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// RelayCAPEM is the CA certificate (PEM) of the relay's server certificate, so the client can
	// verify the relay. Empty for providers without a relay.
	// +optional
	RelayCAPEM string `json:"relayCAPEM,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=tun
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.status.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tunnel lets one person reach one target from outside the cluster, for a time.
type Tunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelSpec   `json:"spec"`
	Status TunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelList contains a list of Tunnel resources.
type TunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tunnel `json:"items"`
}

// TunnelProviderSecretKeyRef names one key of a Secret in the provider's namespace.
type TunnelProviderSecretKeyRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// CloudflareTunnelProviderSpec is a customer's own Cloudflare account. Cloudflare's terms forbid
// reselling Cloudflare Tunnel from Orama's account, so this provider exists only with the
// customer's account and the customer's API token.
type CloudflareTunnelProviderSpec struct {
	// AccountID is the customer's Cloudflare account id.
	// +kubebuilder:validation:MinLength=1
	AccountID string `json:"accountId"`

	// ZoneID is the customer's Cloudflare zone that holds the tunnel hostnames.
	// +kubebuilder:validation:MinLength=1
	ZoneID string `json:"zoneId"`

	// Domain is the DNS name under which each tunnel gets its hostname, tunnel-<grant id>.<domain>.
	// It must be in ZoneID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	Domain string `json:"domain"`

	// APITokenSecretRef is the customer's API token, with Cloudflare Tunnel edit, Access apps and
	// policies edit and DNS edit permissions on that account and zone. It is a projection of a
	// workspace secret; the operator never writes or logs it.
	APITokenSecretRef TunnelProviderSecretKeyRef `json:"apiTokenSecretRef"`
}

// TunnelProviderSpec carries the vendor, the account and the credentials behind a Tunnel.
// +kubebuilder:validation:XValidation:rule="self.type != 'cloudflare' || has(self.cloudflare)",message="a cloudflare provider needs spec.cloudflare"
// +kubebuilder:validation:XValidation:rule="self.type != 'zetta' || !has(self.cloudflare)",message="a zetta provider takes no cloudflare settings"
type TunnelProviderSpec struct {
	// Type is zetta (Orama's own zetta-tunnel) or cloudflare (the customer's own account).
	// +kubebuilder:validation:Enum=zetta;cloudflare
	Type string `json:"type"`

	// CustomerID is the Lake customer this provider belongs to. A Tunnel of another customer cannot
	// use it.
	// +kubebuilder:validation:Format=uuid
	CustomerID string `json:"customerId"`

	// Cloudflare holds the customer's own Cloudflare account settings.
	// +optional
	Cloudflare *CloudflareTunnelProviderSpec `json:"cloudflare,omitempty"`
}

// TunnelProviderStatus reports whether the provider can be used.
type TunnelProviderStatus struct {
	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tunprov
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TunnelProvider is the vendor, account and credentials a Tunnel in its namespace may use. A
// TunnelProvider named "default" is the provider for the namespace's Lake-granted tunnels.
type TunnelProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelProviderSpec   `json:"spec"`
	Status TunnelProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelProviderList contains a list of TunnelProvider resources.
type TunnelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tunnel{}, &TunnelList{}, &TunnelProvider{}, &TunnelProviderList{})
}
