/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkSandboxPolicy is one explicitly justified Kubernetes NetworkPolicy rendered inside the
// sandbox namespace. The controller adds the namespace-wide default-deny policy separately.
type NetworkSandboxPolicy struct {
	// Name is the NetworkPolicy name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Comment records which component needs this allowance and why. It is copied to the rendered
	// policy's orama.cloud/network-policy-comment annotation.
	// +kubebuilder:validation:MinLength=1
	Comment string `json:"comment"`

	// Spec is the complete NetworkPolicy spec for this allowance.
	Spec networkingv1.NetworkPolicySpec `json:"spec"`
}

// NetworkSandboxCiliumPolicy is one explicitly justified CiliumNetworkPolicy used only when a
// Kubernetes NetworkPolicy cannot express the allowance. Spec is the Cilium v2 policy spec.
type NetworkSandboxCiliumPolicy struct {
	// Name is the CiliumNetworkPolicy name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Comment records which component needs this Cilium-only allowance and why.
	// +kubebuilder:validation:MinLength=1
	Comment string `json:"comment"`

	// Spec is passed to the CiliumNetworkPolicy after it has been decoded as a JSON object.
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec apiextensionsv1.JSON `json:"spec"`
}

// NetworkSandboxSpec defines the desired policies for one namespace. Default-deny ingress and
// egress are mandatory and implicit; every entry here is an exception to that baseline.
type NetworkSandboxSpec struct {
	// PolicyLabels are applied to every rendered policy. Parent reconcilers use their existing
	// ownership labels so adopting a live namespace changes no policy selectors or labels.
	// +optional
	PolicyLabels map[string]string `json:"policyLabels,omitempty"`

	// Policies are allowances Kubernetes NetworkPolicy can express.
	// +optional
	// +listType=map
	// +listMapKey=name
	Policies []NetworkSandboxPolicy `json:"policies,omitempty"`

	// CiliumPolicies are allowances that require Cilium-specific policy fields. They stay empty
	// when NetworkPolicy is sufficient.
	// +optional
	// +listType=map
	// +listMapKey=name
	CiliumPolicies []NetworkSandboxCiliumPolicy `json:"ciliumPolicies,omitempty"`
}

// NetworkSandboxStatus is the stable observed summary of rendered policies.
type NetworkSandboxStatus struct {
	// ObservedGeneration is the spec generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// NetworkPolicies is the number of rendered Kubernetes NetworkPolicies, including default-deny.
	// +optional
	NetworkPolicies int32 `json:"networkPolicies,omitempty"`

	// CiliumNetworkPolicies is the number of rendered CiliumNetworkPolicies.
	// +optional
	CiliumNetworkPolicies int32 `json:"ciliumNetworkPolicies,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=netsandbox
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Policies",type=integer,JSONPath=`.status.networkPolicies`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NetworkSandbox makes its namespace default-deny and declares every allowed network path.
type NetworkSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkSandboxSpec   `json:"spec,omitempty"`
	Status NetworkSandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkSandboxList contains a list of NetworkSandbox resources.
type NetworkSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkSandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkSandbox{}, &NetworkSandboxList{})
}
