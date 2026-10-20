/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Payment kinds (principle 10: a provider resource, an abstract kind, a concrete kind).
//
//   - PaymentProvider: the payment vendor, account and the operator's key.
//   - PaymentWebhookEndpoint and StripeWebhookEndpoint follow (operator#637).
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

func init() {
	SchemeBuilder.Register(&PaymentProvider{}, &PaymentProviderList{})
}
