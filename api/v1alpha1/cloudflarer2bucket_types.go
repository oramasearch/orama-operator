/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareR2BucketSpec is the desired state of an R2 bucket. The API token + account come
// from the referenced Secret (keys `token` + `accountID`).
type CloudflareR2BucketSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `accountID`).
	SecretRef string `json:"secretRef"`
	// Name is the R2 bucket name.
	Name string `json:"name"`
}

// CloudflareR2BucketStatus is the observed state.
type CloudflareR2BucketStatus struct {
	// +optional
	Created bool `json:"created,omitempty"`
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=cfr2
//+kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.name`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// CloudflareR2Bucket is an R2 (S3-compatible) bucket the operator provisions via the CF API,
// owned by an OramaCloudV1 CR. A finalizer deletes the bucket from Cloudflare on delete.
type CloudflareR2Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareR2BucketSpec   `json:"spec,omitempty"`
	Status CloudflareR2BucketStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareR2BucketList contains a list of CloudflareR2Bucket.
type CloudflareR2BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareR2Bucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareR2Bucket{}, &CloudflareR2BucketList{})
}
