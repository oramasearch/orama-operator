/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LakeSealedSecretSpec defines one-way secret delivery into a Kubernetes Secret.
// +kubebuilder:validation:XValidation:rule="size(self.lakeRef.name) > 0",message="lakeRef.name is required"
// +kubebuilder:validation:XValidation:rule="self.source.type != 'literal' || (has(self.data) && size(self.data) > 0)",message="literal sources require sealed data"
// +kubebuilder:validation:XValidation:rule="self.source.type == 'literal' || !has(self.data) || size(self.data) == 0",message="only literal sources may include data"
// +kubebuilder:validation:XValidation:rule="self.source.type != 'lake' || has(self.source.lake)",message="lake sources require source.lake"
// +kubebuilder:validation:XValidation:rule="self.source.type != 'dynamic' || has(self.source.dynamic)",message="dynamic sources require source.dynamic"
// +kubebuilder:validation:XValidation:rule="self.source.type != 'delivery' || has(self.source.delivery)",message="delivery sources require source.delivery"
type LakeSealedSecretSpec struct {
	// LakeRef names the cluster-scoped Lake that owns the sealing key and lake endpoint.
	LakeRef corev1.LocalObjectReference `json:"lakeRef"`

	// Template describes the Kubernetes Secret to produce in this resource's namespace.
	Template LakeSealedSecretTemplate `json:"template"`

	// Data maps Secret data keys to base64-encoded canonical CBOR envelopes. It is required for a
	// literal source and omitted for lake and dynamic sources.
	// +optional
	Data map[string]string `json:"data,omitempty"`

	// Source selects ciphertext, a lake reference, a dynamic lease, or an environment delivery.
	Source LakeSealedSecretSource `json:"source"`
}

// LakeSealedSecretTemplate describes the produced Secret.
type LakeSealedSecretTemplate struct {
	// Name is the produced Secret name and the name bound into envelope AAD.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Type is the produced Kubernetes Secret type. Empty means Opaque.
	// +optional
	Type corev1.SecretType `json:"type,omitempty"`
}

// LakeSealedSecretSource selects exactly one value source.
// +kubebuilder:validation:XValidation:rule="self.type != 'literal' || (!has(self.lake) && !has(self.dynamic))",message="literal source cannot include lake or dynamic configuration"
// +kubebuilder:validation:XValidation:rule="self.type != 'lake' || (has(self.lake) && !has(self.dynamic))",message="lake source requires only lake configuration"
// +kubebuilder:validation:XValidation:rule="self.type != 'dynamic' || (has(self.dynamic) && !has(self.lake))",message="dynamic source requires only dynamic configuration"
// +kubebuilder:validation:XValidation:rule="self.type == 'delivery' ? (has(self.delivery) && !has(self.lake) && !has(self.dynamic)) : !has(self.delivery)",message="only delivery sources may include delivery configuration"
type LakeSealedSecretSource struct {
	// Type selects literal, lake, dynamic, or operator-managed environment delivery.
	// +kubebuilder:validation:Enum=literal;lake;dynamic;delivery
	Type string `json:"type"`

	// Lake references a static secret stored by lake.
	// +optional
	Lake *LakeStaticSecretSource `json:"lake,omitempty"`

	// Dynamic asks lake to mint or renew a leased secret.
	// +optional
	Dynamic *LakeDynamicSecretSource `json:"dynamic,omitempty"`

	// Delivery binds a namespace-local owner to an environment and its validated source.
	// Only the environment controller creates these resources.
	// +optional
	Delivery *LakeEnvironmentSecretDeliverySource `json:"delivery,omitempty"`
}

// LakeEnvironmentSecretDeliverySource names resources in the referenced Lake namespace.
// No ciphertext is copied across its namespace-bound AAD boundary.
type LakeEnvironmentSecretDeliverySource struct {
	// Environment names the LakeEnvironment that authorizes the delivery.
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// Name names the original LakeSealedSecret in that environment's secretRefs.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LakeStaticSecretSource references one versioned lake secret.
type LakeStaticSecretSource struct {
	// Mount identifies the lake secret mount.
	// +kubebuilder:validation:MinLength=1
	Mount string `json:"mount"`

	// Name identifies the secret within the mount.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version pins an immutable version. Omitted selects the current version.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Version *int64 `json:"version,omitempty"`
}

// LakeDynamicSecretSource requests a leased credential from lake.
type LakeDynamicSecretSource struct {
	// Engine names the configured lake dynamic-secret engine.
	// +kubebuilder:validation:MinLength=1
	Engine string `json:"engine"`

	// Role names the least-privilege engine role.
	// +kubebuilder:validation:MinLength=1
	Role string `json:"role"`

	// TTL is the requested lease duration.
	// +kubebuilder:validation:Format=duration
	TTL metav1.Duration `json:"ttl"`
}

// LakeSealedSecretStatus reports delivery state without exposing plaintext.
type LakeSealedSecretStatus struct {
	// Phase is Pending, Ready, or Error.
	// +optional
	Phase string `json:"phase,omitempty"`

	// UnsealedAt is the last successful Secret update.
	// +optional
	UnsealedAt *metav1.Time `json:"unsealedAt,omitempty"`

	// KID is the key that opened the delivered record set.
	// +optional
	KID string `json:"kid,omitempty"`

	// Lease is present for dynamic sources.
	// +optional
	Lease *LakeSealedSecretLeaseStatus `json:"lease,omitempty"`

	// MintedAt is the last successful delivery from lake for a lake source. An unpinned lake source
	// is delivered again every refresh interval, so a new secret version reaches the Secret.
	// +optional
	MintedAt *metav1.Time `json:"mintedAt,omitempty"`

	// Version is the lake secret version the Secret holds, for a lake source.
	// +optional
	Version int64 `json:"version,omitempty"`

	// ObservedGeneration is the resource generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LakeSealedSecretLeaseStatus tracks renewal without storing the credential.
type LakeSealedSecretLeaseStatus struct {
	// ID is lake's opaque lease identifier.
	ID string `json:"id"`
	// ExpiresAt is the lease expiry reported by lake.
	ExpiresAt metav1.Time `json:"expiresAt"`
	// RenewAt is when the operator will renew before expiry.
	RenewAt metav1.Time `json:"renewAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=lss
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.spec.template.name`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.type`
// +kubebuilder:printcolumn:name="KID",type=string,JSONPath=`.status.kid`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// LakeSealedSecret is ciphertext or a lake reference delivered into a Secret.
type LakeSealedSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LakeSealedSecretSpec `json:"spec"`
	// +optional
	Status LakeSealedSecretStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeSealedSecretList contains LakeSealedSecret resources.
type LakeSealedSecretList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LakeSealedSecret `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeSealedSecret{}, &LakeSealedSecretList{})
}
