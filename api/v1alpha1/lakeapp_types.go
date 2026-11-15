// SPDX-License-Identifier: MIT

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
	// RestartGeneration is the generation Lake's restart_app last wrote (lake#1228), 0 when the app
	// was never restarted. It is written on the pod template, so each restart replaces the pods
	// even when nothing else about the app changed.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RestartGeneration int64 `json:"restartGeneration,omitempty"`
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

	// SecretKeyRef selects the source Secret and key in the Lake namespace. With LakeSecret set it
	// names the Secret the controller's LakeSealedSecret produces there.
	SecretKeyRef LakeAppSecretKeyReference `json:"secretKeyRef"`

	// LakeSecret, when set, is the source of truth: one field of one secret in a lake secret mount
	// (lake#625). The controller owns a LakeSealedSecret with a lake source in the Lake namespace
	// that produces SecretKeyRef.Name; lake answers it sealed to the cluster's key, and a new
	// version reaches the app at the next delivery.
	// +optional
	LakeSecret *LakeAppLakeSecretReference `json:"lakeSecret,omitempty"`
}

// LakeAppLakeSecretReference names one field of one secret in a lake secret mount.
type LakeAppLakeSecretReference struct {
	// Mount is the lake secret mount id.
	// +kubebuilder:validation:MinLength=1
	Mount string `json:"mount"`

	// Name is the secret's name in the mount.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Field is the secret's field, delivered as the produced Secret's key of the same name.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Field string `json:"field"`
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

	// Stores are the workspace's stores the app mounts (lake#666), written by the apps sync from
	// lake's store_mounts table; only a synchronized app may carry them. A volume is its claim
	// mounted at mountPath; a bucket is its connection as STORE_<NAME>_* environment variables, with
	// the credential fields copied from secretName.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Stores []LakeAppStoreMount `json:"stores,omitempty"`
}

// LakeAppStoreMount is one store an app mounts.
type LakeAppStoreMount struct {
	// ID is the store's id in lake.
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// Name is the store's name; a bucket's environment variables are STORE_<NAME>_* with the name
	// upper-cased and "-" as "_".
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,30}$`
	Name string `json:"name"`

	// Slug is the store's slug, st- and 12 hex digits.
	// +kubebuilder:validation:Pattern=`^st-[0-9a-f]{12}$`
	Slug string `json:"slug"`

	// Type is volume or object.
	// +kubebuilder:validation:Enum=volume;object
	Type string `json:"type"`

	// Provider is the store's provider (cluster for a volume; r2, s3, b2, gcs or azure for a bucket).
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// ClaimName is a volume's PersistentVolumeClaim in the app's workspace namespace.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// MountPath is where a volume is mounted in the app's container.
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9._/-]{1,255}$`
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// ReadOnly mounts a volume read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`

	// Bucket, Endpoint, Region and Prefix are a bucket's connection.
	// +optional
	Bucket string `json:"bucket,omitempty"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// SecretName is the Secret, in this LakeApp's namespace, that the store's LakeSealedSecret
	// delivered with the bucket's credential fields.
	// +optional
	SecretName string `json:"secretName,omitempty"`
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
