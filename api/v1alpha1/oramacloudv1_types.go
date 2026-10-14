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

	// Profile applies a preset group of replication + sizing defaults, overridable
	// per component:
	//   light  = no replication, single-cluster / dev
	//   medium = single-tenant dedicated (prod) settings
	//   heavy  = multi-tenant prod
	// +kubebuilder:validation:Enum=light;medium;heavy
	// +kubebuilder:default=light
	Profile string `json:"profile,omitempty"`

	// IDP selects the identity provider for the dashboard / customer auth.
	// +kubebuilder:default={provider: none}
	IDP IDPSpec `json:"idp,omitempty"`

	// Images sets the default registry/tag and per-component image overrides.
	// Defaults reuse the published oramasearch/* images.
	Images ImagesSpec `json:"images,omitempty"`

	// LLM configures the available answer/RAG language-model providers (multiple).
	// +kubebuilder:default={default: groq}
	LLM LLMSpec `json:"llm,omitempty"`

	// Cloudflare keeps Cloudflare as the edge / geo load-balancer; the operator
	// reconciles CF resources (DNS records, worker routes) via the CF API based on
	// this config — provisioning/deprovisioning as components change.
	// +optional
	Cloudflare CloudflareSpec `json:"cloudflare,omitempty"`

	// Components toggles + configures each part of the stack. Any component can be
	// enabled/disabled independently.
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

	// Resources overrides the in-cluster IdP (Keycloak) pod resources. When unset the
	// operator applies a sensible per-component default (workload hardening, #18).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
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
	// PullSecretName is a docker-registry Secret (in the CR's namespace) used to pull
	// images from a private registry — e.g. the private Docker Hub repos the bridge's
	// build/images tooling pushes to. Attached to every pod the operator creates.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// LLMSpec configures the answer/RAG language-model providers. v1 offers MULTIPLE
// providers at once (Groq/OpenAI/Anthropic in prod) and end users/customers pick per
// query — so this is a list, not a single choice. Keys come from the engine/workers/
// dashboard scoped secrets (or a per-provider apiKeySecretRef).
type LLMSpec struct {
	// Providers available for selection. If empty, defaults to groq + openai + anthropic.
	// +optional
	Providers []LLMProvider `json:"providers,omitempty"`
	// Default provider used when a request doesn't name one.
	// +kubebuilder:default=groq
	Default string `json:"default,omitempty"`
}

// LLMProvider is one selectable LLM backend.
type LLMProvider struct {
	// Name of the provider.
	// +kubebuilder:validation:Enum=groq;openai;anthropic;vllm
	Name string `json:"name"`
	// Models this provider offers (the catalog the end user can pick from).
	// +optional
	Models []string `json:"models,omitempty"`
	// APIKeySecretRef overrides the default per-component key for this provider.
	// +optional
	APIKeySecretRef *corev1.LocalObjectReference `json:"apiKeySecretRef,omitempty"`
	// GPU requests a GPU for an in-cluster vLLM provider (name=vllm).
	// +optional
	GPU bool `json:"gpu,omitempty"`
}

// CloudflareSpec configures the operator's Cloudflare API reconciler. DNS + load
// balancing ALWAYS live in Cloudflare (the permanent edge / geo-LB); the operator
// provisions/deprovisions those CF resources as this cluster's components change.
type CloudflareSpec struct {
	// Enabled defaults true — the operator manages Cloudflare by default. Set false
	// only to opt this cluster out of operator-managed CF (rare).
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
	// APITokenSecretRef references the Secret holding the Cloudflare **control-plane** API
	// credentials as three keys: `token` (the account API token), `zoneID`, and `accountID`.
	// This token provisions DNS, Load Balancing, Workers KV, and R2 buckets.
	// +optional
	APITokenSecretRef *corev1.LocalObjectReference `json:"apiTokenSecretRef,omitempty"`
	// R2SecretRef references a SEPARATE Secret holding the Cloudflare **R2 (S3-compatible)
	// data-plane** credentials as three keys: `s3ApiEndpoint`, `accessKeyId`, and
	// `secretAccessKey`. Kept apart from apiTokenSecretRef because these are object-storage
	// access keys (read/write R2 objects) — a different credential type from the account API
	// token. Components that read/write R2 (e.g. legacy indexes) consume it.
	// +optional
	R2SecretRef *corev1.LocalObjectReference `json:"r2SecretRef,omitempty"`
	// ManageDNS: create/update DNS records pointing at this cluster's ingress.
	// +kubebuilder:default=true
	ManageDNS bool `json:"manageDNS,omitempty"`
	// ManageLoadBalancers: reconcile Cloudflare Load Balancing — geo-steered pools
	// across the regional clusters + health checks. This is the geo load-balancer.
	// +kubebuilder:default=true
	ManageLoadBalancers bool `json:"manageLoadBalancers,omitempty"`
	// ManageWorkerRoutes: reconcile CF Worker routes for any workers kept on CF
	// rather than the in-cluster bundle.
	// +optional
	ManageWorkerRoutes bool `json:"manageWorkerRoutes,omitempty"`
}

// ComponentsSpec toggles + configures each part of v1.
type ComponentsSpec struct {
	Engine EngineSpec `json:"engine,omitempty"` // OramaCore writer + readers + Envoy
	// IngressClassName is the ingress controller that serves this resource's Ingresses: "nginx"
	// (ingress-nginx, the default until a cluster's cutover) or "zetta-tunnel" (zetta-tunnel's
	// ingress gateway, docs/infrastructure/ingress.md). Changing it moves the hosts' DNS, because DNS
	// follows the Ingress's load-balancer address.
	// +kubebuilder:validation:Enum=nginx;zetta-tunnel
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	Dashboard ToggleWithHost `json:"dashboard,omitempty"` // app.orama.com (Next.js)
	Docs      ToggleWithHost `json:"docs,omitempty"`      // docs.orama.com (Next.js + Fumadocs)
	Ingestion IngestionSpec  `json:"ingestion,omitempty"` // index-builder (Deployment) + HARVEST
	Analytics DataStoreSpec  `json:"analytics,omitempty"` // ClickHouse
	ControlDB DataStoreSpec  `json:"controlDB,omitempty"` // Postgres control plane
	PgAdmin   Toggle         `json:"pgAdmin,omitempty"`   // pgAdmin4 web — admin UI for the control-DB (ClusterIP, no ingress)
	Cache     DataStoreSpec  `json:"cache,omitempty"`     // Redis (+ QStash shim)
	Broker    DataStoreSpec  `json:"broker,omitempty"`    // RabbitMQ
	Search    DataStoreSpec  `json:"search,omitempty"`    // Typesense hosted-search clusters
	Disco     Toggle         `json:"disco,omitempty"`     // DISCO semantic cache
	Workers   WorkersSpec    `json:"workers,omitempty"`   // the ported CF workers (one bundled image)

	// Additional v1 components (gap fill — techops-docs issues #5–#7):
	AnalyticsProcessor Toggle `json:"analyticsProcessor,omitempty"` // pulls /v1/analytics → ClickHouse
	// Deprecated: oramasearch-app (api.askorama.ai, the legacy "old dashboard") just redirects to
	// the new app, so it is NOT run in-cluster — the reconciler is unwired (operator#35). Field
	// kept for API compatibility; setting it has no effect.
	OramasearchApp ToggleWithHost `json:"oramasearchApp,omitempty"`
	SearchWorkers  Toggle         `json:"searchWorkers,omitempty"` // index-manager hosted-search serving workers
	// Airbyte (#4) is a heavy multi-service platform → installed via its Helm chart (helmfile
	// platformDeps), not a CR component; the dashboard/index-builder reference its endpoint.
}

// EngineSpec configures the OramaCore engine.
type EngineSpec struct {
	// +kubebuilder:default=true
	Enabled bool       `json:"enabled"`
	Writer  WriterSpec `json:"writer,omitempty"`
	Reader  ReaderSpec `json:"reader,omitempty"`
	// Envoy enables the per-reader :80->:8080 reverse proxy (matches prod).
	// +kubebuilder:default=true
	Envoy bool `json:"envoy,omitempty"`
}

type WriterSpec struct {
	// GPU requests nvidia.com/gpu for the writer's BUILT-IN embeddings generation
	// (ONNX Runtime + CUDA; prod: L40S). Set false to use CPU embeddings or a remote
	// embeddings server. (This is the main reason a GPU node is needed at all; the
	// other is an in-cluster vLLM — see spec.llm.gpu.)
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
	MinReplicas          int32 `json:"minReplicas,omitempty"`
	MaxReplicas          int32 `json:"maxReplicas,omitempty"`
	TargetCPUUtilization int32 `json:"targetCPUUtilization,omitempty"`
}

// IngestionSpec configures the ingest path (index-builder as a plain Deployment + HARVEST).
type IngestionSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Harvest enables the crawler (Deployment + Mongo + Selenium grid).
	// +kubebuilder:default=true
	Harvest bool `json:"harvest,omitempty"`
	// Resources overrides the index-builder pod resources. When unset the operator applies
	// a sensible per-component default (workload hardening, #18). HARVEST's master/Mongo/
	// Selenium keep their own defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// WorkersSpec configures the ported Cloudflare Workers, bundled into ONE image and
// run as a single Deployment (for now).
type WorkersSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Image overrides the single image bundling all ported workers.
	// +optional
	Image string `json:"image,omitempty"`
	// Replicas of the bundled workers Deployment.
	// +kubebuilder:default=2
	Replicas int32 `json:"replicas,omitempty"`
	// Resources overrides the workers pod resources. When unset the operator applies a
	// sensible per-component default (workload hardening, #18).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// DataStoreSpec: a stateful dependency that can be run in-cluster (via an upstream
// operator/chart), pointed at an external endpoint, or centrally located.
type DataStoreSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Mode: "in-cluster" (operator provisions it) or "external" (use Endpoint).
	// +kubebuilder:validation:Enum=in-cluster;external
	// +kubebuilder:default=in-cluster
	Mode string `json:"mode,omitempty"`
	// Placement: "local" (run in every cluster) or "central" (only the primary runs
	// it; other clusters reference it via Endpoint). e.g. broker = central so one
	// cluster hosts RabbitMQ. Set by spec.profile unless overridden.
	// +kubebuilder:validation:Enum=local;central
	// +kubebuilder:default=local
	Placement string `json:"placement,omitempty"`
	// Replicate: for a read tier, stream/replicate this store from the primary
	// (e.g. Postgres read replica). Ignored on the primary. Set by spec.profile.
	// +optional
	Replicate bool `json:"replicate,omitempty"`
	// Endpoint (host:port or URL) when Mode=external or Placement=central.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialsSecretRef (ExternalSecret-materialized) when Mode=external.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
	// Resources overrides the store's pod resources (requests + memory limit). When unset the
	// operator applies a sensible per-component default — a GENEROUS memory limit sized so the
	// store isn't OOM-killed in normal operation (workload hardening, #18). Ignored when
	// Mode=external. Binary-side right-sizing (Redis maxmemory, etc.) is tracked in #42.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// Toggle is a simple on/off component.
type Toggle struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Resources overrides the component's pod resources. When unset the operator applies a
	// sensible per-component default (workload hardening, #18).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ToggleWithHost is a toggle plus an ingress host.
type ToggleWithHost struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// +optional
	Host string `json:"host,omitempty"`
	// Resources overrides the component's pod resources. When unset the operator applies a
	// sensible per-component default (workload hardening, #18).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PeerSpec identifies another region for cross-cluster wiring (no mesh).
type PeerSpec struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
	// WriterEndpoint is the primary's OramaCore writer (read tiers write via this).
	// +optional
	WriterEndpoint string `json:"writerEndpoint,omitempty"`
	// PostgresEndpoint is the primary's control-plane DB (for replica streaming).
	// +optional
	PostgresEndpoint string `json:"postgresEndpoint,omitempty"`
}

// ComponentStatus is the readiness of ONE deployable component (middle granularity — one entry
// per workload the operator owns, e.g. writer/reader/broker/dashboard — not per-pod, not per-field).
// Kept terse on purpose: the status subresource is written only on a real state transition, so it
// stays an accurate signal without flapping or churning the k8s control plane.
type ComponentStatus struct {
	// Name of the component (its app.kubernetes.io/component label).
	Name string `json:"name"`
	// Ready is true when the component's workload is fully available (readyReplicas >= desired).
	Ready bool `json:"ready"`
	// Detail is a short human summary (e.g. "3/3"). Coarse by design — avoids per-pod churn.
	// +optional
	Detail string `json:"detail,omitempty"`
}

// OramaCloudV1Status is the observed state.
type OramaCloudV1Status struct {
	// Phase: Progressing (coming up) | Ready (all components ready) | Degraded (was ready, a
	// component regressed) | Pending.
	Phase string `json:"phase,omitempty"`
	// Conditions follows the standard k8s condition convention (the Ready condition is the
	// machine-readable roll-up).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ComponentsReady is the ready/total count (e.g. "22/22").
	// +optional
	ComponentsReady string `json:"componentsReady,omitempty"`
	// Components is the per-component readiness summary — accurate component-level visibility
	// without per-pod detail. Ordered by name for stability (so re-ordering never triggers a write).
	// +optional
	// +listType=map
	// +listMapKey=name
	Components         []ComponentStatus `json:"components,omitempty"`
	ObservedGeneration int64             `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ocv1,categories=orama
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region.name`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.region.role`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
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
