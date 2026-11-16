/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Edge kinds (principle 10: an abstract kind, a provider resource, a concrete kind).
//
//   - EdgeProvider: the vendor, account and credentials for edge resources.
//   - EdgeZonePolicy: the provider-neutral request "secure the edge of this DNS zone".
//   - CloudflareZonePolicy: created and owned by an EdgeZonePolicy of a cloudflare provider; the only
//     kind that changes a Cloudflare zone's settings.
//
// operator#578 introduced them so a Lake's zone is secured by the operator, never by hand.

// EdgeProviderSecretKeyRef names one key of a Secret in the provider's namespace.
type EdgeProviderSecretKeyRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// CloudflareEdgeProviderSpec is a Cloudflare account and an API token.
type CloudflareEdgeProviderSpec struct {
	// AccountID is the Cloudflare account. Zone policies do not need it; account-level edge
	// resources do.
	// +optional
	AccountID string `json:"accountId,omitempty"`

	// APITokenSecretRef names the API token in this namespace. What it must be allowed depends on
	// the kinds that use the provider. An EdgeZonePolicy needs, on its zone: Zone > Zone Settings >
	// Edit, Zone > DNS > Read (DNS > Edit includes it), Zone > Zone > Read (the zone's name), and
	// Zone > SSL and Certificates > Edit to order edge certificates for hosts more than one level
	// below the apex and for zone-level authenticated origin pulls. An EdgeHostPolicy needs, on each
	// host's zone: Zone > Zone > Read, Zone > Config Rules > Edit (API permission group "Config
	// Settings Write", for originTLS) and Zone > Single Redirect > Edit ("Dynamic URL Redirects
	// Write", for redirectToHTTPS). The operator never writes or logs it.
	APITokenSecretRef EdgeProviderSecretKeyRef `json:"apiTokenSecretRef"`
}

// EdgeProviderSpec carries the vendor, the account and the credentials behind edge resources.
// +kubebuilder:validation:XValidation:rule="self.type != 'cloudflare' || has(self.cloudflare)",message="a cloudflare provider needs spec.cloudflare"
type EdgeProviderSpec struct {
	// Type is the edge vendor. Only cloudflare exists.
	// +kubebuilder:validation:Enum=cloudflare
	Type string `json:"type"`

	// Cloudflare holds the Cloudflare account and token.
	// +optional
	Cloudflare *CloudflareEdgeProviderSpec `json:"cloudflare,omitempty"`
}

// EdgeProviderStatus reports whether the provider's credentials are present.
type EdgeProviderStatus struct {
	// Ready is true when the token Secret and key exist. Whether the token is allowed what a kind
	// needs is reported by that kind.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message explains a provider that is not ready.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=edgeprov
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeProvider is the vendor, account and credentials the edge resources in its namespace use.
type EdgeProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EdgeProviderSpec   `json:"spec"`
	Status EdgeProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EdgeProviderList contains a list of EdgeProvider resources.
type EdgeProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeProvider `json:"items"`
}

// EdgeZonePolicySpec asks for one DNS zone's edge to be secured. It has no settings: a policy
// always means all of them (no off switch).
//
// What the operator does, in this order, each step only when it is safe:
//  1. Origin encryption Full: the edge connects to every origin over TLS. Set only when every
//     proxied host's origin completes a TLS handshake, and only from a weaker mode.
//  2. Origin encryption Full (strict): the edge also verifies the origin's certificate. Set only
//     when every proxied host's origin presents a certificate valid for that host.
//  3. Authenticated origin pulls: the edge presents a client certificate to every origin, which
//     an origin may then require (status.originClientCAPEM is the CA to verify it with). Set once
//     the mode is Full or Full (strict). Then zone-level authenticated origin pulls: the zone's
//     CA (generated once by the managing cluster's operator and copied by the sets to every
//     member cluster that serves the zone) signs a client certificate that the operator uploads,
//     turning zone-level pulls on once every proxied origin that asks for a client certificate
//     accepts that CA; it renews the certificate before it expires.
//  4. Edge certificates: a proxied host more than one level below the zone apex (an app host
//     such as <app>.lake-testing.example.net) is not covered by the provider's free certificate.
//     For each such host without a valid edge certificate, and not covered by an existing one, the
//     operator orders one (Cloudflare: an Advanced Certificate, which needs Advanced Certificate
//     Manager on the zone). Certificates are never deleted.
//  5. Always use HTTPS: the edge redirects plain HTTP. Set only when every proxied host has a
//     valid edge certificate.
//
// Nothing is ever turned back: a host that later fails a check is reported, not downgraded. Several
// policies may name one zone (one per Lake); they ask for the same settings. Deleting a policy
// changes nothing in the zone.
type EdgeZonePolicySpec struct {
	// ProviderRef names an EdgeProvider in this namespace.
	ProviderRef corev1.LocalObjectReference `json:"providerRef"`

	// ZoneID is the provider's identifier of the DNS zone (Cloudflare: the zone ID).
	// +kubebuilder:validation:MinLength=1
	ZoneID string `json:"zoneID"`

	// OriginPullSecretRef names a Secret in this namespace holding the zone's origin-pull CA
	// certificate and client certificate (keys ca.crt, tls.crt, tls.key), distributed by the
	// managing cluster: a LakeSet copies it into every member that serves the zone, so every cluster
	// serving one zone presents and trusts the same CA (operator#633). When set, the operator uses
	// that Secret only: it never creates a CA or a client certificate of its own for the zone, waits
	// while the Secret is missing, and fails while its CA cannot be read. Unset, the operator keeps
	// its own Secret orama-edge-origin-pull-<zone ID> in its namespace, created once. A Lake that a
	// set manages sets it; nothing else needs to.
	// +optional
	OriginPullSecretRef *corev1.LocalObjectReference `json:"originPullSecretRef,omitempty"`
}

// EdgeZoneHostStatus is one proxied host that does not pass a check.
type EdgeZoneHostStatus struct {
	// Name is the DNS record's name; a wildcard is checked with a name under it.
	Name string `json:"name"`

	// Origin is empty when the host's origin presents a valid certificate, else why not.
	// +optional
	Origin string `json:"origin,omitempty"`

	// Edge is empty when the edge presents a valid certificate for the host, else why not.
	// +optional
	Edge string `json:"edge,omitempty"`
}

// EdgeZonePolicyStatus is the zone as the operator last observed it. CloudflareZonePolicy reports
// it and EdgeZonePolicy copies it.
type EdgeZonePolicyStatus struct {
	// Phase is Ready (every setting applied), Progressing (a setting waits on a check, listed in
	// conditions and hosts), or Failed (the zone cannot be read or changed; see message).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message summarises the phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Zone is the zone's name (its apex domain, such as example.com) as the provider reports it. A
	// Lake's Issuer solves DNS-01 for this zone and every name under it, and HTTP-01 for any other
	// name; until the zone is known it solves DNS-01 for every name (operator#632).
	// +optional
	Zone string `json:"zone,omitempty"`

	// OriginEncryption is the zone's observed mode: off, flexible, full or strict. An origin may
	// redirect HTTP to HTTPS only when it is full or strict; under flexible that is a loop.
	// +optional
	OriginEncryption string `json:"originEncryption,omitempty"`

	// AuthenticatedOriginPulls is whether the edge presents a client certificate that
	// originClientCAPEM verifies, so origins may require it. It is false while the zone may present
	// a certificate the bundle cannot verify (one of another CA).
	// +optional
	AuthenticatedOriginPulls bool `json:"authenticatedOriginPulls,omitempty"`

	// ZoneOriginPulls is whether the edge presents the zone's own client certificate, signed by the
	// zone's origin-pull CA (zone-level authenticated origin pulls), rather than Cloudflare's certificate
	// shared by every account.
	// +optional
	ZoneOriginPulls bool `json:"zoneOriginPulls,omitempty"`

	// OriginClientCAPEM is what origins verify the edge's client certificate against: Cloudflare's
	// global origin-pull CA and the zone's origin-pull CA until the zone presents its own
	// certificate, then the zone's CA alone.
	// +optional
	OriginClientCAPEM string `json:"originClientCAPEM,omitempty"`

	// AlwaysUseHTTPS is whether the edge redirects plain HTTP to HTTPS.
	// +optional
	AlwaysUseHTTPS bool `json:"alwaysUseHTTPS,omitempty"`

	// ProxiedHosts is how many proxied hosts the zone has.
	// +optional
	ProxiedHosts int32 `json:"proxiedHosts,omitempty"`

	// Hosts are the proxied hosts that fail a check, sorted by name, at most 50.
	// +optional
	Hosts []EdgeZoneHostStatus `json:"hosts,omitempty"`

	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Credentials, OriginCertificates, EdgeCertificates, OriginTLSStrict,
	// AuthenticatedOriginPulls, ZoneOriginPulls, EdgeCertificateOrders, AlwaysUseHTTPS, ZoneName.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=edgezp
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=`.status.originEncryption`
// +kubebuilder:printcolumn:name="AOP",type=boolean,JSONPath=`.status.authenticatedOriginPulls`
// +kubebuilder:printcolumn:name="ZoneAOP",type=boolean,JSONPath=`.status.zoneOriginPulls`
// +kubebuilder:printcolumn:name="HTTPS",type=boolean,JSONPath=`.status.alwaysUseHTTPS`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeZonePolicy secures one DNS zone's edge through its provider (see EdgeZonePolicySpec).
type EdgeZonePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EdgeZonePolicySpec   `json:"spec"`
	Status EdgeZonePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EdgeZonePolicyList contains a list of EdgeZonePolicy resources.
type EdgeZonePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeZonePolicy `json:"items"`
}

// CloudflareZonePolicySpec is an EdgeZonePolicy resolved against a cloudflare EdgeProvider. A
// person never writes one.
type CloudflareZonePolicySpec struct {
	// ZoneID is the Cloudflare zone.
	// +kubebuilder:validation:MinLength=1
	ZoneID string `json:"zoneID"`

	// APITokenSecretRef is the provider's token: Zone > Zone Settings > Edit, Zone > DNS > Read,
	// Zone > Zone > Read and Zone > SSL and Certificates > Edit on the zone.
	APITokenSecretRef EdgeProviderSecretKeyRef `json:"apiTokenSecretRef"`

	// OriginPullSecretRef is the EdgeZonePolicy's: the zone's distributed origin-pull Secret in this
	// namespace. Unset, the operator's own per-cluster Secret.
	// +optional
	OriginPullSecretRef *corev1.LocalObjectReference `json:"originPullSecretRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfzp
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=`.status.originEncryption`
// +kubebuilder:printcolumn:name="AOP",type=boolean,JSONPath=`.status.authenticatedOriginPulls`
// +kubebuilder:printcolumn:name="ZoneAOP",type=boolean,JSONPath=`.status.zoneOriginPulls`
// +kubebuilder:printcolumn:name="HTTPS",type=boolean,JSONPath=`.status.alwaysUseHTTPS`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareZonePolicy applies an EdgeZonePolicy to a Cloudflare zone.
type CloudflareZonePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareZonePolicySpec `json:"spec"`
	Status EdgeZonePolicyStatus     `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareZonePolicyList contains a list of CloudflareZonePolicy resources.
type CloudflareZonePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareZonePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&EdgeProvider{}, &EdgeProviderList{},
		&EdgeZonePolicy{}, &EdgeZonePolicyList{},
		&CloudflareZonePolicy{}, &CloudflareZonePolicyList{},
	)
}
