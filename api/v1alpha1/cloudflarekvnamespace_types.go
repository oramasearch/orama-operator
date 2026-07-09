/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareKVNamespaceSpec is the desired state of a Workers KV namespace. The API token +
// account come from the referenced Secret (keys `token` + `accountID`).
type CloudflareKVNamespaceSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `accountID`).
	SecretRef string `json:"secretRef"`
	// Title is the KV namespace title (unique per account).
	Title string `json:"title"`
}

// CloudflareKVNamespaceStatus is the observed state — the provisioned namespace.
type CloudflareKVNamespaceStatus struct {
	// NamespaceID is the Cloudflare-assigned KV namespace ID (consumed by index-builder).
	// +optional
	NamespaceID string `json:"namespaceID,omitempty"`
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=cfkv
//+kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
//+kubebuilder:printcolumn:name="NamespaceID",type=string,JSONPath=`.status.namespaceID`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// CloudflareKVNamespace is a Workers KV namespace the operator provisions via the CF API,
// owned by an OramaCloudV1 CR. A finalizer deletes the namespace from Cloudflare on delete.
type CloudflareKVNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareKVNamespaceSpec   `json:"spec,omitempty"`
	Status CloudflareKVNamespaceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareKVNamespaceList contains a list of CloudflareKVNamespace.
type CloudflareKVNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareKVNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareKVNamespace{}, &CloudflareKVNamespaceList{})
}
