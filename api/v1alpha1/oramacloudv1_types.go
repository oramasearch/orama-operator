/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	// JWTIssuer is shared by dashboard token signing and writer verification. It has no CRD
	// default: engine.writer.jwt requires it set, so a production issuer is always chosen on
	// purpose. A CR without writer JWT verification falls back to "orama-cloud" for the dashboard.
	// +kubebuilder:validation:MaxLength=128
	JWTIssuer string `json:"jwtIssuer,omitempty"`

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

	// Placement pins every pod the operator creates for this CR (apps, data stores, engine,
	// Jobs, and the CNPG control-DB instances) to a node pool. The writer's own
	// nodeSelector/tolerations, when set, replace these for the writer.
	// +optional
	Placement PlacementSpec `json:"placement,omitempty"`

	// DeletionProtection keeps the stack's data through CR deletion and component disable: the
	// CNPG control-DB Cluster (and its ScheduledBackup) carry no owner reference and are never
	// deleted by the operator, and every StatefulSet sets
	// persistentVolumeClaimRetentionPolicy Retain/Retain. Default false.
	// +optional
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// MasterAPIKeySecretRef names the Secret key holding OramaCore's MASTER_API_KEY, which feeds
	// every component that reads it (engine, analytics-processor, index-builder, dashboard,
	// workers). Unset, the operator uses <cr>-master-api-key (key MASTER_API_KEY), creating it once
	// if absent. The value must equal the control DB's oramacore_clusters.master_api_key.
	// +optional
	MasterAPIKeySecretRef *MasterAPIKeySecretRef `json:"masterAPIKeySecretRef,omitempty"`

	// RequireRealCredentials turns a False CredentialsReady condition (a Secret key still holding
	// a seeded dummy value, or missing) into phase Degraded. Default false: the condition is
	// reported but the phase follows component readiness only.
	// +optional
	RequireRealCredentials bool `json:"requireRealCredentials,omitempty"`
}

// MasterAPIKeySecretRef names the Secret key that holds the master API key.
type MasterAPIKeySecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key in the Secret. Defaults to MASTER_API_KEY.
	// +optional
	Key string `json:"key,omitempty"`
}

// PlacementSpec is node placement applied to every operator-created pod.
type PlacementSpec struct {
	// NodeSelector is merged into each pod's nodeSelector.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are added to each pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// VolumeSpec sizes one persistent data volume (a StatefulSet volumeClaimTemplate or the CNPG
// instance storage).
type VolumeSpec struct {
	// Size of the volume. Default 10Gi.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// StorageClassName for the PVC; empty uses the cluster default StorageClass.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
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
	// provider config. For firebase, the dashboard reads FIREBASE_PROJECT_ID,
	// FIREBASE_CLIENT_EMAIL and FIREBASE_PRIVATE_KEY from this Secret. Ignored for "none".
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
	// full image ref, bypassing registry/tag for that component. A digest-pinned ref
	// (`repo@sha256:<digest>` or `repo:tag@sha256:<digest>`) is pulled IfNotPresent;
	// a tag-only ref of an operator-built app is pulled Always.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`
	// PullSecretName is a docker-registry Secret (in the CR's namespace) used to pull
	// images from a private registry — e.g. the private Docker Hub repos the bridge's
	// build/images tooling pushes to. Attached to every pod the operator creates.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// LLMSpec configures the engine's language-model providers. v1 offers MULTIPLE providers at once
// (Groq/OpenAI/Google/Anthropic in prod) and end users/customers pick per query, so this is a list,
// not a single choice. The operator renders it into the OramaCore config's ai_server section: the
// default provider becomes ai_server.llm, every provider a remote_llms entry. Keys come from the
// engine Secret (<cr>-engine-secret, key <PROVIDER>_API_KEY) or a per-provider apiKeySecretRef.
type LLMSpec struct {
	// Providers available for selection. If empty, defaults to groq (llama-3.3-70b-versatile),
	// openai (gpt-5.2) and anthropic (claude-sonnet-4-5).
	// +optional
	Providers []LLMProvider `json:"providers,omitempty"`
	// Default provider used when a request doesn't name one. It must be one of providers.
	// +kubebuilder:default=groq
	Default string `json:"default,omitempty"`
	// AutomaticEmbeddingsSelector is the provider and model the writer asks to choose the properties
	// of a collection with automatic embeddings (ai_server.embeddings.automatic_embeddings_selector).
	// Its provider must be one of providers. Unset renders no selector.
	// +optional
	AutomaticEmbeddingsSelector *LLMModelRef `json:"automaticEmbeddingsSelector,omitempty"`
}

// LLMProvider is one selectable LLM backend.
type LLMProvider struct {
	// Name of the provider.
	// +kubebuilder:validation:Enum=groq;openai;google;anthropic;vllm
	Name string `json:"name"`
	// Models this provider offers (the catalog the end user can pick from). The first is the
	// provider's default model; at least one is required.
	// +optional
	Models []string `json:"models,omitempty"`
	// URL overrides the provider's OpenAI-compatible API base URL. Empty uses OramaCore's built-in
	// one (for example https://api.openai.com/v1).
	// +optional
	URL string `json:"url,omitempty"`
	// APIKeySecretRef takes this provider's key from a Secret in the CR's namespace instead of the
	// engine Secret.
	// +optional
	APIKeySecretRef *LLMKeySecretRef `json:"apiKeySecretRef,omitempty"`
	// GPU requests a GPU for an in-cluster vLLM provider (name=vllm). In-cluster vLLM is not
	// rendered yet, so a vllm provider is refused at reconcile.
	// +optional
	GPU bool `json:"gpu,omitempty"`
}

// LLMKeySecretRef names the Secret key that holds one provider's API key.
type LLMKeySecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key in the Secret. Defaults to <PROVIDER>_API_KEY, for example OPENAI_API_KEY.
	// +optional
	Key string `json:"key,omitempty"`
}

// LLMModelRef names one model of one provider.
type LLMModelRef struct {
	// Provider is the name of one of spec.llm.providers.
	// +kubebuilder:validation:Enum=groq;openai;google;anthropic
	Provider string `json:"provider"`
	// Model is the provider's model name, for example gpt-5.2.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
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
	// ZoneIDs maps a Cloudflare zone name (e.g. orama.com) to its zone ID. Each managed DNS host
	// uses the zone with the longest matching suffix; a host matching no entry uses the
	// apiTokenSecretRef Secret's `zoneID`. Lets one CR manage names in several zones.
	// +optional
	ZoneIDs map[string]string `json:"zoneIDs,omitempty"`
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
	Engine    EngineSpec     `json:"engine,omitempty"`    // OramaCore writer + readers + Envoy
	Dashboard DashboardSpec  `json:"dashboard,omitempty"` // app.orama.com (Next.js)
	Docs      ToggleWithHost `json:"docs,omitempty"`      // docs.orama.com (Next.js + Fumadocs)
	Ingestion IngestionSpec  `json:"ingestion,omitempty"` // index-builder (Deployment) + HARVEST
	Analytics DataStoreSpec  `json:"analytics,omitempty"` // ClickHouse
	ControlDB ControlDBSpec  `json:"controlDB,omitempty"` // Postgres control plane (CNPG)
	PgAdmin   Toggle         `json:"pgAdmin,omitempty"`   // pgAdmin4 web — admin UI for the control-DB (ClusterIP, no ingress)
	Cache     DataStoreSpec  `json:"cache,omitempty"`     // Redis (+ QStash shim)
	Broker    DataStoreSpec  `json:"broker,omitempty"`    // RabbitMQ
	Search    DataStoreSpec  `json:"search,omitempty"`    // Typesense hosted-search clusters
	Disco     Toggle         `json:"disco,omitempty"`     // DISCO semantic cache
	Workers   WorkersSpec    `json:"workers,omitempty"`   // the ported CF workers (one bundled image)

	// Additional v1 components (gap fill — techops-docs issues #5–#7):
	AnalyticsProcessor AnalyticsProcessorSpec `json:"analyticsProcessor,omitempty"` // pulls /v1/analytics → ClickHouse
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
	// MinMaxMapCount is the lowest node vm.max_map_count the writer and reader pods start on: a
	// preflight init container fails below it. A reader holds every mapping it opens until it
	// restarts, so the Ubuntu default (1048576) runs out within weeks. vm.max_map_count is not a
	// namespaced sysctl; it is set on the node pool (see docs/install.md). Unset means 4194304,
	// Prod 1's value; 0 skips the check.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinMaxMapCount *int64 `json:"minMaxMapCount,omitempty"`
	// MinOpenFiles is the lowest open-files limit (ulimit -n) the writer and reader pods start
	// with: the preflight init container fails below it. Unset means 65535, Prod 1's
	// LimitNOFILE; 0 skips the check.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinOpenFiles *int64 `json:"minOpenFiles,omitempty"`
	// Hooks tunes the runtime that runs customer JavaScript hooks on the writer and readers. The
	// operator always renders an outbound deny list (Prod 1's private, loopback, link-local and
	// reserved ranges plus the cluster's own names); this only adds to it and sets the timeouts.
	// +optional
	Hooks *EngineHooksSpec `json:"hooks,omitempty"`
}

// EngineHooksSpec adds to the hooks runtime's outbound deny list and sets its timeouts. There is no
// allow list: OramaCore refuses a config that sets both lists.
type EngineHooksSpec struct {
	// ExtraDeniedDomains are appended to the default deny list, once each. An entry is a host
	// pattern matched against the URL host a hook fetches (not the resolved address): a literal
	// host, or a glob in which "*" stays within one dot-separated label and "**" spans labels
	// ("10.244.**" denies every 10.244.x.y). CIDR notation does not match anything, so it is refused.
	// +kubebuilder:validation:items:Pattern=`^[^/\s]+$`
	// +optional
	ExtraDeniedDomains []string `json:"extraDeniedDomains,omitempty"`
	// EvaluationTimeout bounds loading a hook's code. Default 200ms, as on Prod 1.
	// +optional
	EvaluationTimeout *metav1.Duration `json:"evaluationTimeout,omitempty"`
	// ExecutionTimeout bounds one run of a hook. Default 1s, as on Prod 1.
	// +optional
	ExecutionTimeout *metav1.Duration `json:"executionTimeout,omitempty"`
}

type WriterSpec struct {
	// Host publishes the writer through an owned Ingress and optional Cloudflare DNS.
	// Empty keeps the writer internal.
	// +optional
	Host string `json:"host,omitempty"`
	// JWT enables the dashboard JWKS provider. Unset preserves the existing config without JWT.
	// +optional
	JWT *WriterJWTSpec `json:"jwt,omitempty"`
	// GPU requests nvidia.com/gpu: 1 on the writer pod, where OramaCore computes embeddings
	// in-process (ONNX Runtime + CUDA, falling back to CPU; prod: L40S). Set false to run the
	// writer's embeddings on CPU on a node without a GPU. (This is the main reason a GPU node is
	// needed at all; the other is an in-cluster vLLM — see spec.llm.gpu.)
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
	// Storage sizes the writer's data PVC (default 10Gi, cluster default StorageClass).
	// volumeClaimTemplates are immutable, so changing it on a running writer is refused by the
	// API server; see docs/components.md for the resize procedure.
	// +optional
	Storage VolumeSpec `json:"storage,omitempty"`
	// StartupTimeout is how long the writer may take to answer GET /health before it is
	// restarted (the startup probe's failureThreshold × periodSeconds). Default 20m, Prod 1's
	// port-open timeout.
	// +optional
	StartupTimeout *metav1.Duration `json:"startupTimeout,omitempty"`
}

// WriterJWTSpec matches OramaCore's dashboard JWKS provider. The writer accepts a token whose
// issuer is spec.jwtIssuer and whose audience is any of Audience and Audiences.
type WriterJWTSpec struct {
	// JWKSURL is the dashboard's JWKS endpoint. Unset uses the in-cluster dashboard Service,
	// http://<cr>-dashboard:3000/api/.well-known/jwks.json, which requires the dashboard enabled.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	JWKSURL string `json:"jwksURL,omitempty"`
	// Audience is a single accepted audience. Deprecated: use Audiences; both are merged.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Audience string `json:"audience,omitempty"`
	// Audiences are the accepted audiences: each exact clusterWriterUrl the dashboard's token
	// endpoint signs for (the writer_url of a control-DB oramacore_clusters row). When the writer
	// has a host, https://<host> must be one of them.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	Audiences []string `json:"audiences,omitempty"`
}

// AnalyticsProcessorSpec configures the processor and its opt-in import trigger.
type AnalyticsProcessorSpec struct {
	Toggle `json:",inline"`
	// Import schedules POST /import. Unset leaves the processor unscheduled, as before.
	// +optional
	Import AnalyticsImportSpec `json:"import,omitempty"`
}

type AnalyticsImportSpec struct {
	// Enabled opts in to an owned CronJob. Requires engine, analytics and controlDB enabled.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Schedule is a five-field CronJob schedule in UTC.
	// +kubebuilder:default="*/5 * * * *"
	Schedule string `json:"schedule,omitempty"`
}

type ReaderSpec struct {
	// Host publishes reads through an owned Ingress and optional Cloudflare DNS.
	// +optional
	Host string `json:"host,omitempty"`
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`
	// HPA autoscales readers on load when set: the operator owns a HorizontalPodAutoscaler
	// targeting the reader StatefulSet and stops setting its replica count.
	// +optional
	HPA *HPASpec `json:"hpa,omitempty"`
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Storage sizes each reader's data PVC (default 10Gi, cluster default StorageClass). The
	// reader keeps its index and stream offset there, so a replaced reader resumes instead of
	// replaying the replication stream from offset 0.
	// +optional
	Storage VolumeSpec `json:"storage,omitempty"`
	// StartupTimeout is how long a reader may take to load its index and answer GET /health
	// before it is restarted (the startup probe's failureThreshold × periodSeconds). A restart
	// begins the load again, so a large reader needs the full time. Default 40m, Prod 1's unit
	// start timeout.
	// +optional
	StartupTimeout *metav1.Duration `json:"startupTimeout,omitempty"`
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
	// EmbeddingsURL is the embeddings HTTP API index-builder POSTs to (its ORAMA_EMBEDDINGS_URL;
	// Prod 1 uses the hosted embeddings API). Unset renders ORAMA_EMBEDDINGS_URL empty:
	// index-builder requires the variable at boot, and nothing in the stack serves this API.
	// +optional
	EmbeddingsURL string `json:"embeddingsURL,omitempty"`
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
	// CredentialsSecretRef (ExternalSecret-materialized) when Mode=external. For controlDB it is
	// required and must hold keys `uri` (connection URI) and `password`; the operator connects its
	// clients with it but never provisions or migrates an external control DB. External analytics
	// uses `CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD` for the dashboard and processor.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
	// Resources overrides the store's pod resources (requests + memory limit). When unset the
	// operator applies a sensible per-component default — a GENEROUS memory limit sized so the
	// store isn't OOM-killed in normal operation (workload hardening, #18). Ignored when
	// Mode=external. Binary-side right-sizing (Redis maxmemory, etc.) is tracked in #42.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Storage sizes the store's data PVC (default 10Gi, cluster default StorageClass). Ignored
	// when Mode=external.
	// +optional
	Storage VolumeSpec `json:"storage,omitempty"`
}

// ControlDBSpec is the control-plane Postgres: a DataStoreSpec (run as a CNPG Cluster when
// in-cluster) plus opt-in object-store backups.
type ControlDBSpec struct {
	DataStoreSpec `json:",inline"`
	// Backup configures scheduled CNPG Barman object-store backups of the control DB. The
	// credentials Secret lives in the CR's namespace. The default prefix is
	// <namespace>/<name>/controldb.
	// +optional
	Backup LakePostgresBackupSpec `json:"backup,omitempty"`
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

// DashboardSpec is the dashboard toggle and host, plus the environment a user supplies on top of
// the operator's wiring (the Stripe, Airbyte, S3, Slack, Upstash and other keys Prod 1 sets in Vercel).
type DashboardSpec struct {
	ToggleWithHost `json:",inline"`
	// EnvFrom lists Secrets and ConfigMaps in the CR's namespace, appended after the operator's own
	// envFrom sources (the shared ConfigMap, <cr>-dashboard-secret, <cr>-jwt), so a key here
	// overrides the shared ConfigMap and the seeded dashboard Secret. It does not override explicit
	// env (use env for that). A source that is missing (and not optional) or that carries an
	// operator-owned key (DATABASE_URL, POSTGRES_PASSWORD, JWT_PRIVATE_KEY, JWT_PUBLIC_KEY,
	// JWT_ISSUER, MASTER_API_KEY) fails the reconcile. A change to a source's data rolls the dashboard.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Env is appended after the operator's explicit env; an entry replaces the operator's variable of
	// the same name (e.g. CLICKHOUSE_HOST for external analytics). Operator-owned names are refused
	// as for envFrom. A change to a referenced Secret or ConfigMap key rolls the dashboard.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Crons runs the dashboard's scheduled calls (Prod 1's Vercel crons) as CronJobs. Off by default.
	// +optional
	Crons DashboardCronsSpec `json:"crons,omitempty"`
}

// DashboardCronsSpec turns the dashboard's three scheduled calls into CronJobs (operator#17). Each one
// is a GET to the in-cluster dashboard Service with `Authorization: Bearer $CRON_SECRET`; a non-2xx
// response fails the Job. Running them on two stacks at once crawls and bills twice, so they are off
// by default and, once enabled, suspended until suspend is set to false at the cutover.
type DashboardCronsSpec struct {
	// Enabled creates the CronJobs (requires the dashboard). Turning it off deletes the CR-owned ones.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Suspend stops new runs without deleting the CronJobs. Unset means true.
	// +kubebuilder:default=true
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
	// SecretRef is the Secret key holding CRON_SECRET. Unset means wherever the dashboard itself reads
	// CRON_SECRET from (a dashboard.envFrom Secret or a dashboard.env secretKeyRef); the reconcile fails
	// when that is not a Secret key. Set this only when it must differ, since the dashboard compares
	// the bearer with its own CRON_SECRET.
	// +optional
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
	// Schedules overrides individual cron schedules (five fields, UTC). An empty entry keeps its default.
	// +optional
	Schedules DashboardCronSchedules `json:"schedules,omitempty"`
}

// DashboardCronSchedules holds per-call schedule overrides; the defaults are Prod 1's vercel.json.
type DashboardCronSchedules struct {
	// Crawler schedules GET /api/private/v1/run-crawler-cron. Default "*/1 * * * *".
	// +optional
	Crawler string `json:"crawler,omitempty"`
	// IndexGC schedules GET /api/private/v1/run-index-gc-cron. Default "0 * * * *".
	// +optional
	IndexGC string `json:"indexGC,omitempty"`
	// ConsumptionSync schedules GET /api/billing/queue-consumption-sync. Default "0 */6 * * *".
	// +optional
	ConsumptionSync string `json:"consumptionSync,omitempty"`
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
