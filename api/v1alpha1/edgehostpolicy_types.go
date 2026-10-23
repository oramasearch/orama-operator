/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Per-host edge settings (operator#527), principle 10: EdgeHostPolicy is the provider-neutral
// request, CloudflareHostPolicy the concrete kind it creates for a cloudflare EdgeProvider.

// EdgeHostPolicySpec asks for per-host edge settings on a list of hosts. It changes only the hosts
// it names: zone-wide settings are never touched (the same zones carry hosts the operator does not
// run).
//
// Order with certificates: Full (strict) on a host whose origin certificate is not yet valid takes
// that host down. This kind does not check certificates. Whatever creates an EdgeHostPolicy adds a
// host only once that host's origin certificate is ready.
type EdgeHostPolicySpec struct {
	// ProviderRef names an EdgeProvider in this namespace.
	ProviderRef corev1.LocalObjectReference `json:"providerRef"`

	// Hosts are exact host names (no wildcards), each in a zone the provider's token can read.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	Hosts []string `json:"hosts"`

	// OriginTLS "strict" requires a valid, trusted certificate at the origin for these hosts
	// (Cloudflare: origin encryption Full (strict)). Empty leaves the zone's mode in force.
	// +kubebuilder:validation:Enum=strict
	// +optional
	OriginTLS string `json:"originTLS,omitempty"`

	// RedirectToHTTPS answers plain-HTTP requests for these hosts with a 301 to HTTPS.
	// +optional
	RedirectToHTTPS bool `json:"redirectToHTTPS,omitempty"`
}

// EdgeHostPolicyStatus reports whether the provider applied the settings.
type EdgeHostPolicyStatus struct {
	// Ready is true when every requested setting is in force for every host.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message explains a policy that is not ready.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=edgehp
// +kubebuilder:printcolumn:name="OriginTLS",type=string,JSONPath=`.spec.originTLS`
// +kubebuilder:printcolumn:name="Redirect",type=boolean,JSONPath=`.spec.redirectToHTTPS`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeHostPolicy applies per-host origin TLS and an HTTPS redirect through its provider (see
// EdgeHostPolicySpec).
type EdgeHostPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EdgeHostPolicySpec   `json:"spec"`
	Status EdgeHostPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EdgeHostPolicyList contains a list of EdgeHostPolicy resources.
type EdgeHostPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeHostPolicy `json:"items"`
}

// CloudflareHostPolicySpec is an EdgeHostPolicy resolved against a cloudflare EdgeProvider. A
// person never writes one.
type CloudflareHostPolicySpec struct {
	// AccountID narrows the zone lookup to one Cloudflare account; empty searches every zone the
	// token can read.
	// +optional
	AccountID string `json:"accountId,omitempty"`

	// APITokenSecretRef is the provider's token. It needs, on each host's zone: Zone > Zone > Read
	// (to find the zone), Zone > Config Rules > Edit (originTLS) and Zone > Single Redirect > Edit
	// (redirectToHTTPS).
	APITokenSecretRef EdgeProviderSecretKeyRef `json:"apiTokenSecretRef"`

	// Hosts are the parent's hosts.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	Hosts []string `json:"hosts"`

	// OriginTLS is the parent's originTLS.
	// +kubebuilder:validation:Enum=strict
	// +optional
	OriginTLS string `json:"originTLS,omitempty"`

	// RedirectToHTTPS is the parent's redirectToHTTPS.
	// +optional
	RedirectToHTTPS bool `json:"redirectToHTTPS,omitempty"`

	// RuleRefPrefix identifies this policy's rules (orama-operator-<namespace>-<name>, set by the
	// parent). The controller changes and deletes only the rules whose ref is <prefix>-tls or
	// <prefix>-https.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9][-a-z0-9]*$`
	RuleRefPrefix string `json:"ruleRefPrefix"`
}

// CloudflareHostPolicyZoneStatus is one zone's rules.
type CloudflareHostPolicyZoneStatus struct {
	// ZoneID is the Cloudflare zone.
	ZoneID string `json:"zoneID"`

	// ZoneName is the zone's name.
	// +optional
	ZoneName string `json:"zoneName,omitempty"`

	// ConfigRuleID is our rule in the http_config_settings phase, when originTLS is set.
	// +optional
	ConfigRuleID string `json:"configRuleID,omitempty"`

	// RedirectRuleID is our rule in the http_request_dynamic_redirect phase, when
	// redirectToHTTPS is set.
	// +optional
	RedirectRuleID string `json:"redirectRuleID,omitempty"`

	// Ready is true when the zone's rules are in place and no other rule overrides them.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message explains a zone that is not ready.
	// +optional
	Message string `json:"message,omitempty"`
}

// CloudflareHostPolicyStatus is the zones' rules as last applied.
type CloudflareHostPolicyStatus struct {
	// Ready is true when every zone is ready.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message explains a policy that is not ready.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Zones are the zones holding our rules, sorted by zone ID. A zone stays listed until its
	// rules are deleted.
	// +optional
	Zones []CloudflareHostPolicyZoneStatus `json:"zones,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfhp
// +kubebuilder:printcolumn:name="OriginTLS",type=string,JSONPath=`.spec.originTLS`
// +kubebuilder:printcolumn:name="Redirect",type=boolean,JSONPath=`.spec.redirectToHTTPS`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareHostPolicy applies an EdgeHostPolicy as Cloudflare rules identified by ref; it is the
// only kind that calls the Cloudflare Rulesets API.
type CloudflareHostPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareHostPolicySpec   `json:"spec"`
	Status CloudflareHostPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareHostPolicyList contains a list of CloudflareHostPolicy resources.
type CloudflareHostPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareHostPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&EdgeHostPolicy{}, &EdgeHostPolicyList{},
		&CloudflareHostPolicy{}, &CloudflareHostPolicyList{},
	)
}
