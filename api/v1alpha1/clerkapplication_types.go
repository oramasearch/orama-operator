/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClerkApplicationSpec is the desired state of a Clerk application, one per environment
// (orama-bridge#116). The Platform API token comes from the referenced Secret (key
// `platformToken`) — never in the CR, never logged.
type ClerkApplicationSpec struct {
	// SecretRef names the Secret holding the Clerk Platform API access token (key `platformToken`).
	SecretRef string `json:"secretRef"`

	// ApplicationName is the Clerk application display name (the idempotency key — the reconciler
	// adopts an existing app with this name rather than creating a duplicate). e.g. orama-studio-dev.
	ApplicationName string `json:"applicationName"`

	// Environment this app serves. Informational, and the default for instanceType.
	// +kubebuilder:validation:Enum=dev;testing;staging;prod
	Environment string `json:"environment"`

	// InstanceType selects which Clerk instance the keys come from. Defaults from environment:
	// prod -> production (needs a custom domain), everything else -> development (free
	// clerk.accounts.dev domain).
	// +kubebuilder:validation:Enum=development;production
	// +optional
	InstanceType string `json:"instanceType,omitempty"`

	// Template is an optional Clerk application template (e.g. b2c-saas, b2b-saas, waitlist).
	// +optional
	Template string `json:"template,omitempty"`

	// Domain is the custom domain for a production instance (ignored for development).
	// +optional
	Domain string `json:"domain,omitempty"`

	// KeysSecretName is the Secret the operator writes the provisioned keys + issuer into.
	// Defaults to <cr-name>-clerk-keys. Keys: publishableKey, secretKey, issuer.
	// +optional
	KeysSecretName string `json:"keysSecretName,omitempty"`

	// DeletionPolicy decides what happens to the real Clerk app when this CR is deleted.
	// Retain (default) orphans the Clerk app — the user pool is NEVER destroyed by accident;
	// only the CR + owned keys Secret go away. Delete removes the Clerk app via the Platform API.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// ClerkApplicationStatus is the observed state — the provisioned Clerk app + instance.
type ClerkApplicationStatus struct {
	// ApplicationID is the Clerk-assigned application id (app_...).
	// +optional
	ApplicationID string `json:"applicationID,omitempty"`
	// InstanceID is the Clerk-assigned instance id (ins_...) for the selected environment type.
	// +optional
	InstanceID string `json:"instanceID,omitempty"`
	// EnvironmentType is the Clerk instance environment_type actually used (development|production).
	// +optional
	EnvironmentType string `json:"environmentType,omitempty"`
	// Issuer is the Frontend API URL (-> CLERK_ISSUER on the node), derived from the publishable key.
	// +optional
	Issuer string `json:"issuer,omitempty"`
	// PublishableKey is safe to surface (client-side, non-secret); the secret key lives only in the
	// keys Secret, never in status.
	// +optional
	PublishableKey string `json:"publishableKey,omitempty"`
	// KeysSecretRef is the Secret the keys + issuer were written into.
	// +optional
	KeysSecretRef string `json:"keysSecretRef,omitempty"`
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the spec generation last reconciled (anti-flap).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=clerkapp
//+kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.spec.environment`
//+kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.status.applicationID`
//+kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=`.status.issuer`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// ClerkApplication is a Clerk application the operator provisions via the Clerk Platform API,
// one per environment (dev/testing/staging/prod). The reconciler adopts-by-name (idempotent),
// writes the instance keys + issuer into an owned Secret, and — per deletionPolicy — retains
// (default) or deletes the Clerk app on CR delete. Template: CloudflareKVNamespace.
type ClerkApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClerkApplicationSpec   `json:"spec,omitempty"`
	Status ClerkApplicationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClerkApplicationList contains a list of ClerkApplication.
type ClerkApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClerkApplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClerkApplication{}, &ClerkApplicationList{})
}
