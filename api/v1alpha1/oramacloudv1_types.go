/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OramaCloudV1Spec describes one regional deployment of the entire Orama Cloud v1
// stack. One CR per cluster; region role selects the write path vs read tier.
type OramaCloudV1Spec struct {
	// Region selects this cluster's role in the multi-region topology.
	Region RegionSpec `json:"region"`

	// IDP selects the identity provider for the dashboard / customer auth.
	// +kubebuilder:default={provider: none}
	IDP IDPSpec `json:"idp,omitempty"`

	// Secrets configures the External Secrets Operator backend. No secret
	// values are ever set inline on this CR.
	Secrets SecretsSpec `json:"secrets,omitempty"`

	// Images sets the default registry/tag and per-component image overrides.
	// Defaults reuse the published oramasearch/* images.
	Images ImagesSpec `json:"images,omitempty"`

	// LLM configures the answer/RAG language-model backend.
	// +kubebuilder:default={provider: groq}
	LLM LLMSpec `json:"llm,omitempty"`

	// Components toggles + configures each part of the stack.
	Components ComponentsSpec `json:"components,omitempty"`

	// Peers lists the other regions (used by read tiers to reach the primary's
	// writer + Postgres; and by the primary to know its replicas). No mesh.
	// +optional
	Peers []PeerSpec `json:"peers,omitempty"`

	// DemoSeed, when true, has the operator seed a demo project + index so a
	// fresh install answers queries out of the box.
	// +optional
	DemoSeed bool `json:"demoSeed,omitempty"`
}

// RegionSpec selects the cluster role.
type RegionSpec struct {
	// Name is a human label for this region (e.g. "us-east", "eu", "ap").
	Name string `json:"name"`

	// Role is "primary" (runs the write path + stateful stores) or "read"
	// (regional read tier that points at the primary via .spec.peers).
	// +kubebuilder:validation:Enum=primary;read
	// +kubebuilder:default=primary
	Role string `json:"role"`
}

// IDPSpec configures customer/dashboard auth.
type IDPSpec struct {
	// +kubebuilder:validation:Enum=none;keycloak;firebase;clerk
	// +kubebuilder:default=none
	Provider string `json:"provider"`

	// ConfigSecretRef points at an ExternalSecret-materialized secret holding the
	// provider config (e.g. Firebase service account, Clerk keys). Ignored for "none".
	// +optional
	ConfigSecretRef *corev1.LocalObjectReference `json:"configSecretRef,omitempty"`
}

// SecretsSpec configures External Secrets Operator.
type SecretsSpec struct {
	// Backend is the ESO provider: aws | gcp | vault | kubernetes.
	// +kubebuilder:validation:Enum=aws;gcp;vault;kubernetes
	// +kubebuilder:default=kubernetes
	Backend string `json:"backend,omitempty"`

	// StoreRef names the (Cluster)SecretStore ESO should pull from.
	// +optional
	StoreRef string `json:"storeRef,omitempty"`
}

// ImagesSpec sets default and per-component images.
type ImagesSpec struct {
	// +kubebuilder:default="oramasearch"
	Registry string `json:"registry,omitempty"`
	// +kubebuilder:default="latest"
	Tag string `json:"tag,omitempty"`
	// Overrides maps a component key ("oramacore", "dashboard", "disco", ...) to a
	// full image ref, bypassing registry/tag for that component.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`
}

// LLMSpec configures the answer/RAG model backend.
type LLMSpec struct {
	// +kubebuilder:validation:Enum=vllm;groq;openai;anthropic
	// +kubebuilder:default=groq
	Provider string `json:"provider"`
	// Model name (provider-specific). For vllm this is the served HF model.
	// +optional
	Model string `json:"model,omitempty"`
	// APIKeySecretRef holds the provider API key (remote providers only).
	// +optional
	APIKeySecretRef *corev1.LocalObjectReference `json:"apiKeySecretRef,omitempty"`
	// GPU requests a GPU for the in-cluster vLLM (provider=vllm only).
	// +optional
	GPU bool `json:"gpu,omitempty"`
}

// ComponentsSpec toggles + configures each part of v1.
type ComponentsSpec struct {
	Engine     EngineSpec     `json:"engine,omitempty"`     // OramaCore writer + readers + Envoy
	Dashboard  ToggleWithHost `json:"dashboard,omitempty"`  // app.orama.com (Next.js)
	Ingestion  IngestionSpec  `json:"ingestion,omitempty"`  // index-builder (Knative) + HARVEST
	Analytics  DataStoreSpec  `json:"analytics,omitempty"`  // ClickHouse
	ControlDB  DataStoreSpec  `json:"controlDB,omitempty"`  // Postgres control plane
	Cache      DataStoreSpec  `json:"cache,omitempty"`      // Redis (+ QStash shim)
	Broker     DataStoreSpec  `json:"broker,omitempty"`     // RabbitMQ
	Search     DataStoreSpec  `json:"search,omitempty"`     // Typesense hosted-search clusters
	Disco      Toggle         `json:"disco,omitempty"`      // DISCO semantic cache
	Workers    WorkersSpec    `json:"workers,omitempty"`    // Cloudflare Workers -> Knative
}

// EngineSpec configures the OramaCore engine.
type EngineSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	Writer  WriterSpec `json:"writer,omitempty"`
	Reader  ReaderSpec `json:"reader,omitempty"`
	// Envoy enables the per-reader :80->:8080 reverse proxy (matches prod).
	// +kubebuilder:default=true
	Envoy bool `json:"envoy,omitempty"`
}

type WriterSpec struct {
	// GPU requests nvidia.com/gpu for on-box embeddings/inference (prod: L40S).
	// Set false to use CPU embeddings or a remote embeddings server.
	// +kubebuilder:default=true
	GPU bool `json:"gpu,omitempty"`
	// Resources overrides the writer pod resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// NodeSelector/Tolerations for GPU node placement.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

type ReaderSpec struct {
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`
	// HPA autoscales readers on load when set.
	// +optional
	HPA *HPASpec `json:"hpa,omitempty"`
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

type HPASpec struct {
	MinReplicas           int32 `json:"minReplicas,omitempty"`
	MaxReplicas           int32 `json:"maxReplicas,omitempty"`
	TargetCPUUtilization  int32 `json:"targetCPUUtilization,omitempty"`
}

// IngestionSpec configures the ingest path (index-builder as a Knative Service + HARVEST).
type IngestionSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Harvest enables the crawler (Deployment + Mongo + Selenium grid).
	// +kubebuilder:default=true
	Harvest bool `json:"harvest,omitempty"`
	// IndexBuilderKnative runs index-builder as a scale-to-zero Knative Service.
	// +kubebuilder:default=true
	IndexBuilderKnative bool `json:"indexBuilderKnative,omitempty"`
}

// WorkersSpec configures the ported Cloudflare Workers.
type WorkersSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Runtime: "workerd" (faithful, needs binding shims) or "node" (ported services).
	// +kubebuilder:validation:Enum=workerd;node
	// +kubebuilder:default=node
	Runtime string `json:"runtime,omitempty"`
	// Knative runs each worker as a scale-to-zero Knative Service.
	// +kubebuilder:default=true
	Knative bool `json:"knative,omitempty"`
}

// DataStoreSpec: a stateful dependency that can be run in-cluster (via an upstream
// operator/chart) or pointed at an external endpoint.
type DataStoreSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Mode: "in-cluster" (operator provisions it) or "external" (use Endpoint).
	// +kubebuilder:validation:Enum=in-cluster;external
	// +kubebuilder:default=in-cluster
	Mode string `json:"mode,omitempty"`
	// Endpoint (host:port or URL) when Mode=external.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialsSecretRef (ExternalSecret-materialized) when Mode=external.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// Toggle is a simple on/off component.
type Toggle struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
}

// ToggleWithHost is a toggle plus an ingress host.
type ToggleWithHost struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// +optional
	Host string `json:"host,omitempty"`
}

// PeerSpec identifies another region for cross-cluster wiring (no mesh).
type PeerSpec struct {
	Name             string `json:"name"`
	Region           string `json:"region,omitempty"`
	// WriterEndpoint is the primary's OramaCore writer (read tiers write via this).
	// +optional
	WriterEndpoint   string `json:"writerEndpoint,omitempty"`
	// PostgresEndpoint is the primary's control-plane DB (for replica streaming).
	// +optional
	PostgresEndpoint string `json:"postgresEndpoint,omitempty"`
}

// OramaCloudV1Status is the observed state.
type OramaCloudV1Status struct {
	// Phase: Pending | Provisioning | Ready | Degraded.
	Phase string `json:"phase,omitempty"`
	// Conditions follows the standard k8s condition convention.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ComponentsReady counts reconciled/ready child components.
	// +optional
	ComponentsReady string `json:"componentsReady,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ocv1,categories=orama
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region.name`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.region.role`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.componentsReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OramaCloudV1 is one regional deployment of the entire Orama Cloud v1 stack.
type OramaCloudV1 struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaCloudV1Spec   `json:"spec,omitempty"`
	Status OramaCloudV1Status `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OramaCloudV1List contains a list of OramaCloudV1.
type OramaCloudV1List struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaCloudV1 `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OramaCloudV1{}, &OramaCloudV1List{})
}
