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

// CredentialRotation is one generation per generated store credential (#594).
type CredentialRotation struct {
	// Broker rotates <cr>-broker-admin's password.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Broker int64 `json:"broker,omitempty"`
	// Cache rotates <cr>-cache-auth's password.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Cache int64 `json:"cache,omitempty"`
}

// OramaCloudV1Spec describes one regional deployment of the entire Orama Cloud v1
// stack. One CR per cluster; region role selects the write path vs read tier.
// +kubebuilder:validation:XValidation:rule="!has(self.components) || !has(self.components.importConsumer) || !has(self.components.importConsumer.enabled) || !self.components.importConsumer.enabled || (has(self.cloudflare) && has(self.cloudflare.enabled) && self.cloudflare.enabled)",message="importConsumer requires cloudflare.enabled"
// +kubebuilder:validation:XValidation:rule="!has(self.components) || !has(self.components.importConsumer) || !has(self.components.importConsumer.enabled) || !self.components.importConsumer.enabled || (has(self.components.dashboard) && has(self.components.dashboard.enabled) && self.components.dashboard.enabled)",message="importConsumer requires the dashboard enabled"
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

	// CredentialRotation rotates the generated broker and Redis passwords (#594). Raising a
	// generation above the one the credential Secret records makes the operator write a new
	// password there; the broker re-imports its admin user on boot and every consumer rolls on the
	// changed Secret, as for any credential change (#411). Lowering it never rotates back.
	// +optional
	CredentialRotation *CredentialRotation `json:"credentialRotation,omitempty"`

	// IDP selects the identity provider for the dashboard / customer auth.
	// +kubebuilder:default={provider: none}
	IDP IDPSpec `json:"idp,omitempty"`

	// JWTIssuer is the issuer the dashboard signs write tokens with (JWT_ISSUER) and the only one
	// the writer accepts. It has no CRD default: engine.writer.jwt requires it set, so a production
	// issuer is chosen on purpose. Without writer JWT verification an empty value falls back to
	// "orama-cloud" for the dashboard.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	JWTIssuer string `json:"jwtIssuer,omitempty"`

	// Images sets the default registry/tag and per-component image overrides.
	// Defaults reuse the published oramasearch/* images.
	Images ImagesSpec `json:"images,omitempty"`

	// LLM configures the available answer/RAG language-model providers (multiple).
	// +kubebuilder:default={default: groq}
	LLM LLMSpec `json:"llm,omitempty"`

	// MasterAPIKeySecretRef names the Secret, in this resource's namespace, that holds the engine
	// master API key. Every component that reads MASTER_API_KEY takes it from here: the writer's
	// config, analytics-processor, index-builder, workers, the dashboard and the e2e runner. The
	// operator never creates or changes it. While the engine is enabled and this reference, its
	// Secret or its key is missing, empty or a dev-* value, the writer and readers are held at zero
	// replicas and Ready names the cause. The value must equal the target control database's
	// oramacore_clusters.master_api_key.
	// +optional
	MasterAPIKeySecretRef *MasterAPIKeySecretRef `json:"masterAPIKeySecretRef,omitempty"`

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

// MasterAPIKeySecretRef is a key of a Secret in the OramaCloudV1's namespace.
type MasterAPIKeySecretRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key is the Secret key that holds the master API key. Default: MASTER_API_KEY.
	// +optional
	Key string `json:"key,omitempty"`
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
	// RequireDigests rejects rendered workload images without a SHA-256 digest. Disabled by default
	// during the production rollout; ImagesPinned reports mutable selections without blocking.
	// +optional
	RequireDigests bool `json:"requireDigests,omitempty"`

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
// providers at once (Prod 1 runs OpenAI, Google, Groq and Anthropic) and end users/customers pick
// per query, so this is a list, not a single choice. Keys come from <cr>-engine-secret under
// <PROVIDER>_API_KEY, or from a provider's apiKeySecretRef.
type LLMSpec struct {
	// Providers available for selection. Each renders one ai_server.remote_llms entry whose default
	// model is the provider's first model. If empty, defaults to Prod 1's set: openai (gpt-5.2),
	// google (gemini-2.0-flash-001), groq (openai/gpt-oss-120b), anthropic (claude-sonnet-4-5).
	// +optional
	Providers []LLMProvider `json:"providers,omitempty"`
	// Default provider used when a request doesn't name one. It must be one of the providers; the
	// engine sends such requests to its built-in URL with its first model and key. Empty means groq.
	// +kubebuilder:default=groq
	Default string `json:"default,omitempty"`
	// AutomaticEmbeddingsSelector is the provider and model the engine asks to choose which document
	// properties to embed for collections with automatic embeddings. Its provider must be one of the
	// providers. Unset renders no selector, including on a CR without spec.llm.
	// +optional
	AutomaticEmbeddingsSelector *LLMModelRef `json:"automaticEmbeddingsSelector,omitempty"`
}

// LLMProvider is one selectable LLM backend.
type LLMProvider struct {
	// Name of the provider.
	// +kubebuilder:validation:Enum=groq;openai;anthropic;google;vllm
	Name string `json:"name"`
	// Models this provider offers. The first is the provider's default model; the list does not
	// restrict the model a request names.
	// +optional
	Models []string `json:"models,omitempty"`
	// APIKeySecretRef takes this provider's key from <PROVIDER>_API_KEY (for example GOOGLE_API_KEY)
	// in a Secret in the CR's namespace instead of <cr>-engine-secret. The operator reads it and
	// never creates or changes it.
	// +optional
	APIKeySecretRef *LLMKeySecretRef `json:"apiKeySecretRef,omitempty"`
	// GPU requests a GPU for an in-cluster vLLM provider (name=vllm).
	// +optional
	GPU bool `json:"gpu,omitempty"`
}

// LLMKeySecretRef names the Secret, in the CR's namespace, that holds a provider's API key under
// <PROVIDER>_API_KEY.
type LLMKeySecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LLMModelRef is one model of one remote provider.
type LLMModelRef struct {
	// Provider of the model.
	// +kubebuilder:validation:Enum=groq;openai;anthropic;google
	Provider string `json:"provider"`
	// Model name, sent to the provider as is.
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
	// This token provisions DNS, Load Balancing, Workers KV, and R2 buckets. index-builder reads
	// its CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID from `token` and `accountID` here, so it
	// writes to the KV namespaces this token created; a missing key fails the ingestion step.
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
	// IngressClassName is ignored. Every Ingress of the stack is served by the stack's own
	// ingress-nginx, whose IngressClass the operator derives from the stack (operator#573); it is
	// never the platform's ingress-nginx or zetta-tunnel's gateway. Kept so stored objects stay valid.
	// +kubebuilder:validation:Enum=nginx;zetta-tunnel
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	Dashboard DashboardSpec  `json:"dashboard,omitempty"` // app.orama.com (Next.js)
	Docs      ToggleWithHost `json:"docs,omitempty"`      // docs.orama.com (Next.js + Fumadocs)
	Ingestion IngestionSpec  `json:"ingestion,omitempty"` // index-builder (Deployment) + HARVEST
	Analytics DataStoreSpec  `json:"analytics,omitempty"` // ClickHouse
	// ControlDB is the Postgres control plane, always in-cluster.
	// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode == 'in-cluster'",message="controlDB.mode external is not supported: the control DB is always in-cluster"
	ControlDB ControlDBSpec `json:"controlDB,omitempty"`
	PgAdmin   Toggle        `json:"pgAdmin,omitempty"` // pgAdmin4 web — admin UI for the control-DB (ClusterIP, no ingress)
	Cache     DataStoreSpec `json:"cache,omitempty"`   // Redis (+ QStash shim)
	Broker    DataStoreSpec `json:"broker,omitempty"`  // RabbitMQ
	Search    DataStoreSpec `json:"search,omitempty"`  // Typesense hosted-search clusters
	Disco     Toggle        `json:"disco,omitempty"`   // DISCO semantic cache
	Workers   WorkersSpec   `json:"workers,omitempty"` // the ported CF workers (one bundled image)

	// Additional v1 components (gap fill — techops-docs issues #5–#7):
	AnalyticsProcessor AnalyticsProcessorSpec `json:"analyticsProcessor,omitempty"` // pulls /v1/analytics → ClickHouse
	// Deprecated: oramasearch-app (api.askorama.ai, the legacy "old dashboard") just redirects to
	// the new app, so it is NOT run in-cluster — the reconciler is unwired (operator#35). Field
	// kept for API compatibility; setting it has no effect.
	OramasearchApp ToggleWithHost `json:"oramasearchApp,omitempty"`
	SearchWorkers  Toggle         `json:"searchWorkers,omitempty"` // index-manager hosted-search serving workers
	// ImportConsumer lists the stack's R2 uploads bucket and imports files and crawl exports
	// through this stack's dashboard. It requires the dashboard and Cloudflare enabled.
	// +optional
	ImportConsumer ImportConsumerSpec `json:"importConsumer,omitempty"`
}

// ImportConsumerSpec configures the single-replica <cr>-import-consumer Deployment. The bucket
// worker lists <cr>-uploads and runs the per-object import against this stack's dashboard.
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || (has(self.image) && size(self.image) > 0)",message="importConsumer.image is required when the import consumer is enabled"
type ImportConsumerSpec struct {
	// Enabled runs the consumer against this stack's own uploads bucket. Off by default.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Image is the consumer's full image reference with src/bucket-worker.ts. No default.
	// +optional
	Image string `json:"image,omitempty"`
	// Concurrency is how many objects one replica imports at once, each in its own process, as the
	// Fargate path ran one task per object.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`
	// Resources overrides the consumer's pod resources. Each import process's heap is sized from the
	// memory limit and Concurrency.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
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

// VolumeSpec is the desired state of a component's data volume. The operator grows an existing
// volume in place. A different class or a smaller size cannot be applied in place: the operator
// replaces the volume, erasing its data, only when approveDataDeletion is true.
type VolumeSpec struct {
	// Size is a positive Kubernetes quantity, compared numerically. Omission resolves to the
	// component's default (10Gi) even without API defaulting. An existing volume grows in place;
	// a size smaller than an existing volume's replaces it only with approveDataDeletion.
	// +optional
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="size must be a positive Kubernetes quantity"
	Size *resource.Quantity `json:"size,omitempty"`
	// StorageClassName is the volume's class. Omission uses the cluster default for new volumes
	// and retains the existing class. An existing volume on another class is replaced only with
	// approveDataDeletion.
	// +optional
	// +kubebuilder:validation:MinLength=1
	StorageClassName *string `json:"storageClassName,omitempty"`
	// ApproveDataDeletion allows the operator to replace this volume when it cannot reach the
	// declared class or size in place (another class, or a smaller size): it deletes the volume,
	// erasing its data, and the component starts again on a new one. False, the default, leaves
	// the volume as it is and reports the difference in status.conditions[StorageApplied].
	// Copying the data to the new volume is not supported yet.
	// +optional
	ApproveDataDeletion bool `json:"approveDataDeletion,omitempty"`
}

type WriterSpec struct {
	// Storage configures the writer's data volume (default 10Gi and cluster default class).
	// +optional
	Storage *VolumeSpec `json:"storage,omitempty"`
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
	// JWT makes the writer verify the dashboard's write tokens through OramaCore's JWKS provider.
	// Unset renders no writer_side.jwt block.
	// +optional
	JWT *WriterJWTSpec `json:"jwt,omitempty"`
	// Host publishes the writer on this public DNS name (for example writer.orama.ing), without a
	// scheme, path or port: an owned Ingress with origin TLS to the writer Service on 8080 and, with
	// Cloudflare DNS managed, a proxied DNS record. Only a primary region publishes its writer.
	// Empty keeps the writer internal.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +optional
	Host string `json:"host,omitempty"`
	// ExtraHosts are further names the component's Ingress answers, each routed to the same backend
	// as host and covered by the origin certificate. The operator creates no DNS record for them: a
	// name such as app.orama.com is pointed at the cluster by a reviewed Cloudflare change. Requires
	// host; no name may repeat across the hosts and extra hosts of every component.
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +optional
	ExtraHosts []string `json:"extraHosts,omitempty"`
}

// WriterJWTSpec is the writer's dashboard JWKS provider. The writer accepts a token whose issuer is
// spec.jwtIssuer and whose audience is one of Audiences, both by exact match.
type WriterJWTSpec struct {
	// JWKSURL is the dashboard's JWKS endpoint. Unset uses the in-cluster dashboard Service,
	// http://<cr>-dashboard:3000/api/.well-known/jwks.json, which requires the dashboard enabled.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	JWKSURL string `json:"jwksURL,omitempty"`
	// Audiences are the accepted audiences: each exact writer URL the dashboard signs tokens for
	// (the writer_url of a control-DB oramacore_clusters row). Required, at least one. With
	// engine.writer.host set, https://<host> (no trailing slash) must be one of them.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	Audiences []string `json:"audiences,omitempty"`
}

type ReaderSpec struct {
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`
	// Storage configures each reader's data volume (default 10Gi and cluster default class).
	// The StatefulSet's claim template keeps the values it was created with; the operator grows
	// every reader's existing claim instead.
	// +optional
	Storage *VolumeSpec `json:"storage,omitempty"`
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Host publishes the readers on this public DNS name (for example reader.orama.ing), without a
	// scheme, path or port: an owned Ingress with origin TLS to the reader Service on 80 (through
	// Envoy when it is enabled) and, with Cloudflare DNS managed, a proxied DNS record. Empty keeps
	// the readers internal.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +optional
	Host string `json:"host,omitempty"`
	// ExtraHosts are further names the component's Ingress answers, each routed to the same backend
	// as host and covered by the origin certificate. The operator creates no DNS record for them: a
	// name such as app.orama.com is pointed at the cluster by a reviewed Cloudflare change. Requires
	// host; no name may repeat across the hosts and extra hosts of every component.
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +optional
	ExtraHosts []string `json:"extraHosts,omitempty"`
}

// IngestionSpec configures the ingest path (index-builder as a plain Deployment + HARVEST).
type IngestionSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// Harvest enables the crawler (Deployment + Mongo + Selenium grid).
	// +kubebuilder:default=true
	Harvest bool `json:"harvest,omitempty"`
	// AblyKeySecretRef supplies index-builder's ABLY_API_KEY for deployment progress.
	// The Secret must exist in this CR's namespace with a nonempty value; it is never written
	// by the operator. Unset preserves the historical sandbox component-Secret defaults.
	// HARVEST worker spawning is not implemented (#12); its separate seed is unchanged.
	// +optional
	AblyKeySecretRef *SecretKeyRef `json:"ablyKeySecretRef,omitempty"`
	// Resources overrides the index-builder pod resources. When unset the operator applies
	// a sensible per-component default (workload hardening, #18). HARVEST's master/Mongo/
	// Selenium keep their own defaults.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// HPESResources overrides the resources of HPES (embeddings-server-py), the embeddings server
	// index-builder calls, which runs whenever ingestion runs (#574). When unset the operator requests
	// 7Gi and 2 CPUs with a 10Gi memory limit, sized for the six models it loads, on CPU.
	// +optional
	HPESResources *corev1.ResourceRequirements `json:"hpesResources,omitempty"`
	// HarvestMongoStorage configures the data volume of HARVEST's Mongo (default 10Gi and the
	// cluster default class), like every other data store's storage.
	// +optional
	HarvestMongoStorage *VolumeSpec `json:"harvestMongoStorage,omitempty"`
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
	// SentryDSNSecretRef takes the workers' SENTRY_DSN from a key of a Secret in the CR's namespace
	// instead of <cr>-workers-secret. The operator reads it and never creates or changes it; a
	// missing Secret or key fails the workers step and Ready names it. The workers' OPENAI_API_KEY,
	// GROQ_API_KEY and ANTHROPIC_API_KEY come from the same providers' spec.llm.providers[].apiKeySecretRef
	// as the engine's.
	// +optional
	SentryDSNSecretRef *SecretKeyRef `json:"sentryDSNSecretRef,omitempty"`
}

// SecretKeyRef is one key of a Secret in the OramaCloudV1's namespace: a third-party key the
// operator reads and never creates or changes.
type SecretKeyRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key of the Secret that holds the value.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ControlDBSpec is the control-plane Postgres: a DataStoreSpec plus this stack's values for the
// dashboard reference rows the operator seeds (operator#378).
type ControlDBSpec struct {
	DataStoreSpec `json:",inline"`
	// StripePrices are this stack's Stripe price IDs for the seeded reference rows: test-mode IDs on
	// a nonprod stack, live-mode IDs on production. They are never part of the operator's committed
	// rows. Empty leaves every plan's Stripe columns NULL and seeds no add-on price.
	// +optional
	StripePrices ControlDBStripePrices `json:"stripePrices,omitempty"`
}

// ControlDBStripePrices maps seeded reference rows, by their ID, to this stack's Stripe price IDs.
// An ID that is not one of the operator's reference rows fails the seed, visibly.
type ControlDBStripePrices struct {
	// Plans sets subscription_plans.stripe_price_id_monthly and stripe_price_id_yearly. A plan not
	// listed has both NULL.
	// +optional
	// +listType=map
	// +listMapKey=id
	Plans []StripePlanPrice `json:"plans,omitempty"`
	// Addons sets stripe_addon_prices.stripe_price_id. That column is NOT NULL, so an add-on price row
	// is seeded only when it is listed here.
	// +optional
	// +listType=map
	// +listMapKey=id
	Addons []StripeAddonPrice `json:"addons,omitempty"`
}

// StripePlanPrice is one subscription plan's Stripe prices.
type StripePlanPrice struct {
	// ID is the subscription_plans row ID.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	ID string `json:"id"`
	// Monthly is the Stripe price ID for monthly billing.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,255}$`
	// +optional
	Monthly string `json:"monthly,omitempty"`
	// Yearly is the Stripe price ID for yearly billing.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,255}$`
	// +optional
	Yearly string `json:"yearly,omitempty"`
}

// StripeAddonPrice is one add-on price row's Stripe price.
type StripeAddonPrice struct {
	// ID is the stripe_addon_prices row ID.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	ID string `json:"id"`
	// PriceID is the Stripe price ID.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,255}$`
	PriceID string `json:"priceID"`
}

// DataStoreSpec: a stateful dependency that can be run in-cluster (via an upstream
// operator/chart), pointed at an external endpoint, or centrally located.
type DataStoreSpec struct {
	// Storage configures local data volumes (default 10Gi and cluster default class).
	// The operator grows PVC requests, or the CNPG Cluster size for controlDB.
	// +optional
	Storage *VolumeSpec `json:"storage,omitempty"`
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

// AnalyticsProcessorSpec is the analytics processor's toggle plus its scheduled import.
type AnalyticsProcessorSpec struct {
	Toggle `json:",inline"`
	// Import schedules the processor's POST /import, which pulls every reader once.
	// +optional
	Import AnalyticsImportSpec `json:"import,omitempty"`
}

// AnalyticsImportSpec controls the <cr>-analytics-import CronJob. Its schedule is fixed (every five
// minutes, UTC).
type AnalyticsImportSpec struct {
	// Enabled creates the CronJob. It requires the engine, analytics and controlDB components.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Suspend keeps the CronJob, the processor and its sources in place but starts no new Job. It is
	// copied to the CronJob's spec.suspend.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ToggleWithHost is a toggle plus an ingress host.
type ToggleWithHost struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// +optional
	Host string `json:"host,omitempty"`
	// ExtraHosts are further names the component's Ingress answers, each routed to the same backend
	// as host and covered by the origin certificate. The operator creates no DNS record for them: a
	// name such as app.orama.com is pointed at the cluster by a reviewed Cloudflare change. Requires
	// host; no name may repeat across the hosts and extra hosts of every component.
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +optional
	ExtraHosts []string `json:"extraHosts,omitempty"`
	// Resources overrides the component's pod resources. When unset the operator applies a
	// sensible per-component default (workload hardening, #18).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// DashboardSpec is the dashboard's toggle and host plus the environment it takes from Secrets and
// ConfigMaps in the instance's namespace (operator#311). Precedence, lowest to highest: the
// operator's envFrom (<cr>-config, <cr>-dashboard-secret, <cr>-jwt), envFrom, the operator's
// explicit env, env. DATABASE_URL, POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DB, POSTGRES_PASSWORD,
// JWT_PRIVATE_KEY, JWT_PUBLIC_KEY, JWT_ISSUER and MASTER_API_KEY belong to the operator: supplying one
// through either field fails the reconcile.
// +kubebuilder:validation:XValidation:rule="!has(self.crons) || !has(self.crons.enabled) || !self.crons.enabled || (has(self.enabled) && self.enabled)",message="dashboard.crons requires the dashboard enabled"
type DashboardSpec struct {
	ToggleWithHost `json:",inline"`
	// EnvFrom adds Secrets and ConfigMaps (in the instance's namespace) after the operator's own
	// envFrom sources, so a key here overrides the shared ConfigMap and the seeded dashboard Secret,
	// and a later entry overrides an earlier one. It never overrides the operator's explicit env.
	// Each entry sets exactly one of secretRef and configMapRef. A missing object fails the
	// reconcile unless the reference is optional. A change to a referenced object rolls the
	// dashboard (pod-template annotation orama.cloud/dashboard-env-hash).
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Env adds variables after the operator's explicit env (e.g. STRIPE_SECRET_KEY from a
	// secretKeyRef). An entry with the same name as an operator variable replaces it; for a name
	// listed twice the last entry wins. A non-optional secretKeyRef or configMapKeyRef whose object
	// or key is missing fails the reconcile; a change to the selected key rolls the dashboard.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Crons runs the dashboard's scheduled calls (Prod 1's Vercel crons) as CronJobs, and optionally
	// the e2e canary (operator#17). Off unless enabled; requires the dashboard enabled.
	// +optional
	Crons DashboardCronsSpec `json:"crons,omitempty"`
	// StripeSecretKeySecretRef takes the dashboard's STRIPE_SECRET_KEY from a key of a Secret in the
	// CR's namespace (operator#802): a Stripe restricted key holding only the permissions the dashboard
	// uses (docs/components.md lists them), never the account's full secret key. The operator reads it
	// and never creates or changes it; a missing Secret, key or value fails the dashboard step. While it
	// is set, STRIPE_SECRET_KEY in env or in an envFrom source fails the reconcile, so no second key
	// sits beside it. A production instance (one its set labels orama.cloud/purpose=production)
	// requires it, with a live-mode restricted key (rk_live_).
	// +optional
	StripeSecretKeySecretRef *SecretKeyRef `json:"stripeSecretKeySecretRef,omitempty"`
}

// DashboardCronsSpec turns on the dashboard's three scheduled calls, with Prod 1's fixed schedules:
// <cr>-dashboard-cron-crawler (every minute), -index-gc (hourly) and -consumption-sync (every six
// hours, UTC). Each GETs the in-cluster dashboard Service with Authorization: Bearer $CRON_SECRET,
// where CRON_SECRET is read from the Secret key the dashboard itself takes it from (envFrom or env).
// Running them on Prod 1 and Prod 2 at once would crawl and bill twice, so they are created
// suspended; the cutover unsuspends them through the set file.
// +kubebuilder:validation:XValidation:rule="!has(self.e2eCanary) || (has(self.enabled) && self.enabled)",message="dashboard.crons.e2eCanary requires dashboard.crons enabled"
type DashboardCronsSpec struct {
	// Enabled creates the three CronJobs. Disabling deletes them.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Suspend is copied to the spec.suspend of the three CronJobs and the e2e canary's. It stops new
	// runs and does not cancel a run already in progress.
	// +kubebuilder:default=true
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
	// E2ECanary runs Prod 1's e2e canary (every five minutes) and its collection prune (daily at
	// 04:30 UTC) as one CronJob, <cr>-e2e-canary, against this stack. It follows crons.suspend.
	// Removing it deletes the CronJob and its state volume.
	// +optional
	E2ECanary *E2ECanarySpec `json:"e2eCanary,omitempty"`
}

// E2ECanarySpec is the canary's image and target. The canary signs in through the in-cluster
// dashboard's /api/user/jwt with the canary project's private API key, writes a probe document and
// checks it on the readers the dashboard returns and on the in-cluster reader.
// +kubebuilder:validation:XValidation:rule="has(self.secretRef.name) && size(self.secretRef.name) > 0",message="dashboard.crons.e2eCanary.secretRef needs a name"
type E2ECanarySpec struct {
	// Image runs monitor-test (oramacore-test-e2e) and provides /bin/sh, curl and jq for the prune.
	// It must be pinned to a digest (name@sha256:<64 hex>).
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`
	// SecretRef names a Secret in the instance's namespace with the non-empty keys collectionID,
	// indexID and apiKey: the canary collection, its index, and the project's private API key.
	// Delivered through the set's secrets list.
	SecretRef corev1.LocalObjectReference `json:"secretRef"`
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
	// machine-readable workload roll-up). StorageApplied independently reports accepted
	// storage configuration, not completed disk/filesystem expansion.
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
