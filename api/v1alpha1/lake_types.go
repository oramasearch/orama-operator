/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

	// Image for the lake binary. Required: there is no production default. The
	// sample CR points at an HTTP echo until the lake image exists.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas of the lake Deployment.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// IngressEnabled gates external HTTP exposure of the lake Service. When true AND Host is set, the
	// operator provisions an nginx Ingress at Host (the whole host proxies to the lake Service) and,
	// if DNSSecretRef is also set, a proxied Cloudflare DNS record (Host -> the ingress LoadBalancer).
	// When false (the default) no Ingress or DNS record is created, and any previously created are
	// torn down, even if Host is still set. Host names the endpoint; this bool is the on/off switch.
	// Same contract as OramaZettaCluster.
	// +kubebuilder:default=false
	// +optional
	IngressEnabled bool `json:"ingressEnabled,omitempty"`

	// Host is the domain name the lake is exposed at (e.g. hello.lake.live) when IngressEnabled is
	// true. Requires an ingress controller (class nginx).
	// +optional
	Host string `json:"host,omitempty"`

	// DNSSecretRef, if set together with Host, creates a proxied Cloudflare DNS record (Host -> the
	// ingress LoadBalancer) using the Cloudflare credentials (keys `token`, `zoneID`) in the named
	// Secret. The Secret must live in spec.namespace (the CloudflareDNSRecord is created there) and
	// hold the zone ID of Host's zone: a lake.live host needs a lake.live-zone Secret, not the
	// oramadev.net one the Zetta clusters use.
	// +optional
	DNSSecretRef *corev1.LocalObjectReference `json:"dnsSecretRef,omitempty"`
}

// LakeStatus defines the observed state of Lake.
type LakeStatus struct {
	// ReadyReplicas mirrors the Deployment's ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Phase is a coarse summary in the operator's vocabulary: Progressing, Ready, Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
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
