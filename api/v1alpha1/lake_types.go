/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LakeSpec defines the desired state of Lake.
type LakeSpec struct {
	// Namespace the lake workloads are deployed into. Created if missing.
	// +kubebuilder:default=lake-dev
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Image for the lake binary. Until the lake binary exists this defaults to a
	// hello-world HTTP echo so the reconcile loop and network path can be proven.
	// +kubebuilder:default="hashicorp/http-echo:1.0"
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas of the lake Deployment.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// LakeStatus defines the observed state of Lake.
type LakeStatus struct {
	// ReadyReplicas mirrors the Deployment's ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Phase is a coarse summary: Pending, Ready.
	// +optional
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Lake is the Schema for the lakes API
type Lake struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Lake
	// +required
	Spec LakeSpec `json:"spec"`

	// status defines the observed state of Lake
	// +optional
	Status LakeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeList contains a list of Lake
type LakeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Lake `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lake{}, &LakeList{})
}
