/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Payment kinds (principle 10: a provider resource, an abstract kind, a concrete kind).
//
//   - PaymentProvider: the payment vendor, account and the operator's key.
//   - PaymentWebhookEndpoint: the provider-neutral request "deliver these payment events to this URL".
//   - StripeWebhookEndpoint: created and owned by a PaymentWebhookEndpoint of a stripe provider; the
//     only kind that calls Stripe's webhook API.
//
// Cloud v1 and Lake bill through one shared Stripe account (orama-bridge#199); these kinds let the
// operator manage what the apps need there from committed resources.

// PaymentSecretKeyRef names one key of a Secret in the referring resource's namespace.
type PaymentSecretKeyRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// StripePaymentProviderSpec is one Stripe account or sandbox, in one mode, and the operator's key.
type StripePaymentProviderSpec struct {
	// AccountID is the account or sandbox (acct_...). The provider refuses a key of another account.
	// +kubebuilder:validation:Pattern=`^acct_[A-Za-z0-9]+$`
	AccountID string `json:"accountId"`

	// Livemode says whether this is live mode. The key's prefix (sk_live_ or rk_live_, against
	// sk_test_ or rk_test_) must agree.
	Livemode bool `json:"livemode"`

	// APIKeySecretRef names the operator's restricted key in this namespace: products, prices and
	// webhook endpoints write, account read. The operator never writes or logs it.
	APIKeySecretRef PaymentSecretKeyRef `json:"apiKeySecretRef"`
}

// PaymentProviderSpec carries the vendor, the account and the credential behind payment resources.
// +kubebuilder:validation:XValidation:rule="self.type != 'stripe' || has(self.stripe)",message="a stripe provider needs spec.stripe"
type PaymentProviderSpec struct {
	// Type is the payment vendor. Only stripe exists.
	// +kubebuilder:validation:Enum=stripe
	Type string `json:"type"`

	// Stripe holds the Stripe account and key.
	// +optional
	Stripe *StripePaymentProviderSpec `json:"stripe,omitempty"`
}

// PaymentProviderStatus reports whether the provider's key works for its account and mode.
type PaymentProviderStatus struct {
	// Conditions: Credentials.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=payprov
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.stripe.accountId`
// +kubebuilder:printcolumn:name="Live",type=boolean,JSONPath=`.spec.stripe.livemode`
// +kubebuilder:printcolumn:name="Credentials",type=string,JSONPath=`.status.conditions[?(@.type=="Credentials")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Credentials")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PaymentProvider is the vendor, account and key the payment resources in its namespace use.
type PaymentProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PaymentProviderSpec   `json:"spec"`
	Status PaymentProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PaymentProviderList contains a list of PaymentProvider resources.
type PaymentProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PaymentProvider `json:"items"`
}

// PaymentWebhookEndpointSpec asks for one app's payment events to be delivered to one URL. Each
// host has its own endpoint, because each endpoint has its own signing secret.
type PaymentWebhookEndpointSpec struct {
	// ProviderRef names a PaymentProvider in this namespace.
	ProviderRef corev1.LocalObjectReference `json:"providerRef"`

	// URL receives the events. https only.
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	URL string `json:"url"`

	// Events are the provider's event types (invoice.paid, ...): only those the app handles. They
	// are sorted and deduplicated when written to the provider.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	Events []string `json:"events"`

	// APIVersion pins the payload version the app parses (Lake: 2024-06-20). A provider may not
	// change it on an existing endpoint, so a new value replaces the endpoint and its signing secret.
	// +kubebuilder:validation:Pattern=`^[0-9]{4}-[0-9]{2}-[0-9]{2}(\.[a-z]+)?$`
	APIVersion string `json:"apiVersion"`

	// App is the metadata[orama_app] tag of the endpoint: cloud_v1 or lake.
	// +kubebuilder:validation:Enum=cloud_v1;lake
	App string `json:"app"`

	// SigningSecretName is the Secret, in this namespace, that receives the endpoint's signing
	// secret under key signingSecret. The operator owns it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SigningSecretName string `json:"signingSecretName"`
}

// PaymentWebhookEndpointStatus is the endpoint as the operator last configured it.
// StripeWebhookEndpoint reports it and PaymentWebhookEndpoint copies it.
type PaymentWebhookEndpointStatus struct {
	// EndpointID is the provider's ID of the endpoint (Stripe: we_...).
	// +optional
	EndpointID string `json:"endpointId,omitempty"`

	// Conditions: Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=paywh
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpointId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PaymentWebhookEndpoint delivers an app's payment events to a URL through its provider (see
// PaymentWebhookEndpointSpec).
type PaymentWebhookEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PaymentWebhookEndpointSpec   `json:"spec"`
	Status PaymentWebhookEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PaymentWebhookEndpointList contains a list of PaymentWebhookEndpoint resources.
type PaymentWebhookEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PaymentWebhookEndpoint `json:"items"`
}

// StripeWebhookEndpointSpec is a PaymentWebhookEndpoint resolved against a stripe PaymentProvider.
// A person never writes one.
type StripeWebhookEndpointSpec struct {
	PaymentWebhookEndpointSpec `json:",inline"`

	// APIKeySecretRef is the provider's operator key: webhook endpoints write.
	APIKeySecretRef PaymentSecretKeyRef `json:"apiKeySecretRef"`
}

// StripeWebhookEndpointStatus is the shared endpoint status and what the controller needs to name
// its next request to Stripe.
type StripeWebhookEndpointStatus struct {
	PaymentWebhookEndpointStatus `json:",inline"`

	// Writes counts the writes Stripe has answered for this object, a 5xx included. It is the
	// attempt suffix of every idempotency key, so a request Stripe answered is never sent again
	// under its key; a request that got no answer is.
	// +optional
	Writes int32 `json:"writes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=stripewh
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpointId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StripeWebhookEndpoint keeps one Stripe webhook endpoint and its signing secret for a
// PaymentWebhookEndpoint.
type StripeWebhookEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StripeWebhookEndpointSpec   `json:"spec"`
	Status StripeWebhookEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StripeWebhookEndpointList contains a list of StripeWebhookEndpoint resources.
type StripeWebhookEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StripeWebhookEndpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&PaymentProvider{}, &PaymentProviderList{},
		&PaymentWebhookEndpoint{}, &PaymentWebhookEndpointList{},
		&StripeWebhookEndpoint{}, &StripeWebhookEndpointList{},
	)
}
