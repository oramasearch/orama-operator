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

	// PullSecretName, if set, is attached to the lake pods as an imagePullSecret. Needed for a
	// private spec.image (oramasearch/lake is private). The Secret must exist in spec.namespace.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// Port the container listens on; the Service exposes it on 80. Default 4900, lake's own
	// default (--listen 0.0.0.0:4900). The hello-world echo stand-in overrides it to 5678.
	// +kubebuilder:default=4900
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// Args passed to the image's entrypoint. Empty runs the image as built.
	// +optional
	Args []string `json:"args,omitempty"`

	// Postgres is lake's control plane: a CloudNativePG Cluster the operator provisions in
	// spec.namespace, the same path OramaCloudV1 uses for its control DB (requires the
	// CloudNativePG operator, see the helmfile). CNPG generates the `lake-<name>-controldb-app`
	// Secret (owner credentials + connection URI); the lake pods receive it as
	// LAKE_CONTROL_DB_URI. Zetta is the data plane and is provisioned separately.
	// +optional
	Postgres LakePostgresSpec `json:"postgres,omitempty"`

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

// LakePostgresSpec describes the control-plane Postgres for a Lake.
type LakePostgresSpec struct {
	// Enabled provisions the CNPG Cluster. Default true. Setting it false tears the cluster
	// down (disable is not "leave running"), which deletes its data.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Instances is the CNPG instance count (1 = single primary; 2+ adds streaming replicas).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances *int32 `json:"instances,omitempty"`

	// StorageSize is the per-instance PVC size. Default 50Gi: a Lake is a production control plane,
	// and CNPG grows a PVC in place when the StorageClass allows expansion, so start with headroom.
	// +kubebuilder:default="50Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClassName for the PVCs; empty = the cluster default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// LakeStatus defines the observed state of Lake.
type LakeStatus struct {
	// ReadyReplicas mirrors the Deployment's ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// PostgresReady is true once the control-plane CNPG Cluster has at least one ready instance.
	// +optional
	PostgresReady bool `json:"postgresReady,omitempty"`
	// PostgresInstances is ready/desired for the CNPG Cluster, e.g. "1/1"; empty when disabled.
	// +optional
	PostgresInstances string `json:"postgresInstances,omitempty"`
	// SchemaPhase mirrors the owned PostgresSchema: Pending | Applying | Applied | Failed.
	// +optional
	SchemaPhase string `json:"schemaPhase,omitempty"`
	// SchemaVersion is the applied schema's content hash (sha256:<12 hex>), from the PostgresSchema.
	// +optional
	SchemaVersion string `json:"schemaVersion,omitempty"`
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
// +kubebuilder:printcolumn:name="DB",type=string,JSONPath=`.status.postgresInstances`
// +kubebuilder:printcolumn:name="Schema",type=string,JSONPath=`.status.schemaPhase`
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
