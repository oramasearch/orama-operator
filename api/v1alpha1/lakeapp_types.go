/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LakeAppModeRuntime = "runtime"
	LakeAppModeImage   = "image"

	LakeAppAuthPublic = "public"
	LakeAppAuthLake   = "lake"

	LakeAppPhaseProgressing = "Progressing"
	LakeAppPhaseReady       = "Ready"
	LakeAppPhaseDegraded    = "Degraded"
	LakeAppPhaseTornDown    = "TornDown"
)

// LakeAppSource identifies one immutable application source revision for runtime mode.
type LakeAppSource struct {
	// Repository is the Lake repository identifier.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Revision is the pinned repository revision loaded by app-runtime.
	// +kubebuilder:validation:MinLength=1
	Revision string `json:"revision"`

	// Path is the application directory within the repository. Empty means the repository root.
	// +optional
	Path string `json:"path,omitempty"`

	// Entrypoint overrides app.toml. Empty lets app-runtime read the app's own configuration.
	// +optional
	Entrypoint string `json:"entrypoint,omitempty"`

	// Framework selects the app-runtime adapter.
	// +kubebuilder:validation:Enum=fastapi;streamlit;static
	Framework string `json:"framework"`

	// TokenSecretRef selects the Lake-minted repository read token from a Secret in the Lake
	// namespace. The controller copies only this key and exposes it as APP_TOKEN.
	// Apps synchronized from Postgres use their generated app-tokens Secret instead.
	// +optional
	TokenSecretRef corev1.SecretKeySelector `json:"tokenSecretRef,omitempty"`
}

// LakeAppZettaBinding requests scoped read-only workspace access through Lake.
type LakeAppZettaBinding struct {
	// CustomerID identifies the workspace owner.
	// +kubebuilder:validation:MinLength=1
	CustomerID string `json:"customerId"`

	// Scopes narrows the read-only workspace credential.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Scopes []string `json:"scopes"`
}

// LakeAppBindings describes the data credentials delivered to an app.
type LakeAppBindings struct {
	// Zetta requests a scoped, read-only workspace token.
	// +optional
	Zetta *LakeAppZettaBinding `json:"zetta,omitempty"`

	// Services contains the Lake service identifiers whose connections are delivered.
	// +listType=set
	// +optional
	Services []string `json:"services,omitempty"`

	// ServiceNames maps service identifiers to filenames under bindings/services. The apps
	// sync resolves these names from same-workspace services; standalone apps default to the ID.
	// +optional
	ServiceNames map[string]string `json:"serviceNames,omitempty"`

	// SecretRef names a standalone app's Lake-namespace source credential Secret. Key
	// "zetta" contains the workspace token and each service identifier names that service's
	// connection value. The controller copies only requested keys into the app namespace.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// LakeAppTokenHashes acknowledges tokens registered in lake's apps row. These are SHA-256
// digests only; plaintext credentials stay in the workload namespace's app token Secret.
type LakeAppTokenHashes struct {
	Source string `json:"source"`
	// +nullable
	Zetta  *string `json:"zetta"`
	Events string  `json:"events"`
	Pull   string  `json:"pull"`
}

// LakeAppSync identifies the desired row managed by the Lake apps sync.
type LakeAppSync struct {
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="app ID is immutable"
	ID string `json:"id"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="customer ID is immutable"
	CustomerID string `json:"customerId"`
	// WorkspaceID is the immutable workspace that owns this app.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspace ID is immutable"
	WorkspaceID string `json:"workspaceId"`
	// WorkspaceNamespace is the operator-reconciled namespace containing the app's data services.
	// +kubebuilder:validation:MinLength=1
	WorkspaceNamespace string `json:"workspaceNamespace"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// TokenHashes gates rollout until the sync has registered these hashes in Postgres.
	// +optional
	TokenHashes *LakeAppTokenHashes `json:"tokenHashes,omitempty"`
}

// LakeAppSecretKeyReference selects one key from a Secret in the Lake namespace.
type LakeAppSecretKeyReference struct {
	// Name is the source Secret name.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key is the source Secret data key.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// LakeAppSecretEnvironmentVariable exposes one Secret key as an application environment variable.
type LakeAppSecretEnvironmentVariable struct {
	// Name is the application environment variable name.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z][-._a-zA-Z0-9]*$`
	Name string `json:"name"`

	// SecretKeyRef selects the source Secret and key in the Lake namespace.
	SecretKeyRef LakeAppSecretKeyReference `json:"secretKeyRef"`
}

// LakeAppSpec defines one isolated application belonging to a Lake.
// +kubebuilder:validation:XValidation:rule="self.mode != 'runtime' || has(self.source)",message="source is required in runtime mode"
// +kubebuilder:validation:XValidation:rule="self.mode != 'image' || has(self.image)",message="image is required in image mode"
// +kubebuilder:validation:XValidation:rule="has(self.sync) || !has(self.bindings) || (!has(self.bindings.zetta) && (!has(self.bindings.services) || size(self.bindings.services) == 0)) || has(self.bindings.secretRef)",message="bindings.secretRef is required for standalone data bindings"
type LakeAppSpec struct {
	// LakeRef names the cluster-scoped Lake whose namespace contains this resource.
	// +kubebuilder:validation:MinLength=1
	LakeRef string `json:"lakeRef"`

	// Sync is set by the apps-table controller. It selects generated local tokens and
	// service connections, and is valid only on a Lake-controlled app in the Lake namespace.
	// +optional
	Sync *LakeAppSync `json:"sync,omitempty"`

	// Mode selects app-runtime source loading or an immutable OCI image.
	// +kubebuilder:default=runtime
	// +kubebuilder:validation:Enum=runtime;image
	// +optional
	Mode string `json:"mode,omitempty"`

	// Source configures runtime mode.
	// +optional
	Source *LakeAppSource `json:"source,omitempty"`

	// Image is the digest-pinned OCI image used in image mode.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-fA-F0-9]{64}$`
	// +optional
	Image string `json:"image,omitempty"`

	// PullSecretName overrides the Lake pull Secret for image mode. The source Secret is read from
	// the Lake namespace and copied into the isolated app namespace.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// Port is the container port exposed by the app Service.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Replicas is the desired app replica count. Zero stops the app while preserving its resources.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources configures CPU and memory requests and limits for the app container.
	// +kubebuilder:default={requests:{cpu:"100m",memory:"128Mi"},limits:{cpu:"1",memory:"1Gi"}}
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env contains literal application environment variables. Operator-managed names take
	// precedence over entries with the same name.
	// +optional
	Env []LakeEnvironmentVariable `json:"env,omitempty"`

	// SecretEnv contains environment variables sourced from individual Secret keys. Source Secrets
	// live in the Lake namespace; the controller copies only selected keys into the app namespace.
	// +listType=map
	// +listMapKey=name
	// +optional
	SecretEnv []LakeAppSecretEnvironmentVariable `json:"secretEnv,omitempty"`

	// SecretRefs is the one-release legacy whole-Secret input. Named Secrets are copied into the app
	// namespace and exposed through envFrom.
	// +listType=set
	// +optional
	SecretRefs []string `json:"secretRefs,omitempty"`

	// Host is the public hostname. Empty defaults to <app>.<Lake spec.apps.wildcard>.
	// +kubebuilder:validation:Format=hostname
	// +optional
	Host string `json:"host,omitempty"`

	// Auth selects public access or Lake session authentication.
	// +kubebuilder:default=lake
	// +kubebuilder:validation:Enum=public;lake
	// +optional
	Auth string `json:"auth,omitempty"`

	// Bindings configures workspace and managed-service data access.
	// +optional
	Bindings *LakeAppBindings `json:"bindings,omitempty"`

	// EventsSecretRef names a Lake-namespace Secret with "url" and "token" keys. The controller
	// and app use it to publish to app-events.<app>.
	// +optional
	EventsSecretRef *corev1.LocalObjectReference `json:"eventsSecretRef,omitempty"`
}

// LakeAppStatus is the observed state of an application.
type LakeAppStatus struct {
	// Phase is Progressing, Ready, Degraded, or TornDown.
	// +optional
	Phase string `json:"phase,omitempty"`

	// URL is the app's public URL when a host is configured.
	// +optional
	URL string `json:"url,omitempty"`

	// Ready reports whether all desired replicas are available at the observed revision.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ObservedRevision is the runtime source revision or image digest currently observed.
	// +optional
	ObservedRevision string `json:"observedRevision,omitempty"`

	// Message summarizes rollout progress or the highest-priority pod failure.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions contains the standard Ready condition and transition details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lakeapp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.observedRevision`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`

// LakeApp runs one application in a dedicated, restricted namespace.
type LakeApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the desired application.
	// +required
	Spec LakeAppSpec `json:"spec"`

	// Status defines the observed application state.
	// +optional
	Status LakeAppStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeAppList contains a list of LakeApp resources.
type LakeAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LakeApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeApp{}, &LakeAppList{})
}
