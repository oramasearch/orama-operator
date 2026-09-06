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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LakeSpec defines the desired state of Lake.
type LakeSpec struct {
	// Namespace the lake workloads are deployed into. Created if missing.
	// +kubebuilder:default=lake-dev
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Image for the lake binary. Required: there is no production default.
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

	// Resources configures the lake container's CPU and memory requests and limits. When omitted,
	// the operator requests 50m CPU and 128Mi memory and limits memory to 512Mi.
	// +kubebuilder:default={requests:{cpu:"50m",memory:"128Mi"},limits:{memory:"512Mi"}}
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// TopologySpreadConstraints overrides the default pod placement constraints. When omitted and
	// Replicas is greater than one, the operator spreads replicas across kubernetes.io/hostname
	// with ScheduleAnyway so a single-node cluster can still schedule the Deployment.
	// +listType=map
	// +listMapKey=topologyKey
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Store configures the persistent root shared by all lake replicas. When ExistingClaim is
	// empty, the operator creates lake-<name>-store once from Size, StorageClassName, and
	// AccessModes. Later spec changes never replace or delete that claim. ReadWriteOnce requires
	// Replicas=1; multiple replicas need storage that supports ReadWriteMany.
	// +optional
	Store LakeStoreSpec `json:"store,omitempty"`

	// Env appends lake-specific environment variables after the operator-managed variables.
	// Operator-managed names take precedence and conflicting entries are ignored.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Sealing configures the X25519 cluster delivery key. The operator mints generation 1 when
	// omitted. Increasing Generation rotates the public key while retaining private keys still
	// needed by LakeSealedSecret resources.
	// +optional
	Sealing LakeSealingSpec `json:"sealing,omitempty"`

	// Postgres is lake's control plane: a CloudNativePG Cluster the operator provisions in
	// spec.namespace, the same path OramaCloudV1 uses for its control DB (requires the
	// CloudNativePG operator, see the helmfile). CNPG generates the `lake-<name>-controldb-app`
	// Secret (owner credentials + connection URI); the lake pods receive it as
	// LAKE_CONTROL_DB_URI. The Zetta field below configures lake's owned data plane.
	// +optional
	Postgres LakePostgresSpec `json:"postgres,omitempty"`

	// Zetta is lake's data plane: an owned OramaZettaCluster in spec.namespace. Its client
	// Service and seeded admin token are injected into lake as LAKE_DATA_PLANE_URL and
	// LAKE_DATA_PLANE_TOKEN. The child is deleted before the Lake finalizer is released.
	// +optional
	Zetta LakeZettaSpec `json:"zetta,omitempty"`

	// IngressEnabled gates external HTTP exposure of the lake Service. When true AND Host is set, the
	// operator provisions an nginx Ingress at Host (the whole host proxies to the lake Service) and,
	// if DNSSecretRef is also set, a proxied Cloudflare DNS record (Host -> the ingress LoadBalancer).
	// When false (the default) no Ingress or DNS record is created, and any previously created are
	// torn down, even if Host is still set. Host names the endpoint; this bool is the on/off switch.
	// Same contract as OramaZettaCluster.
	// +kubebuilder:default=false
	// +optional
	IngressEnabled bool `json:"ingressEnabled,omitempty"`

	// TLS terminates HTTPS at the nginx Ingress. It has no effect unless IngressEnabled and Host
	// are also set. The referenced kubernetes.io/tls Secret must exist in spec.namespace.
	// +optional
	TLS LakeTLSSpec `json:"tls,omitempty"`

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

	// PreviewWildcard is the DNS suffix used for per-change preview environments. For example,
	// preview.example.com yields <change-short>.preview.example.com. When Lake ingress and DNS are
	// enabled, the operator maintains *.preview.example.com against the ingress load balancer so
	// individual preview environments do not create Cloudflare records.
	// +optional
	PreviewWildcard string `json:"previewWildcard,omitempty"`

	// DeploySync configures how frequently this operator materializes desired deployment rows from
	// its Zetta orama_control database into LakeEnvironment resources.
	// +optional
	DeploySync LakeDeploySyncSpec `json:"deploySync,omitempty"`
}

// LakeTLSSpec configures HTTPS termination for a Lake Ingress.
type LakeTLSSpec struct {
	// SecretName names a kubernetes.io/tls Secret in spec.namespace. Empty leaves the Ingress on
	// HTTP only for backward compatibility.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// LakeDeploySyncSpec configures deployment-row polling for a Lake.
type LakeDeploySyncSpec struct {
	// Interval is the deployment polling interval. Empty defaults to 15 seconds.
	// +kubebuilder:default="15s"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
}

// LakeStoreSpec describes the persistent libliquid store root for a Lake.
type LakeStoreSpec struct {
	// Size requested for an operator-created claim. It is used only when the claim is first
	// created; later changes do not replace or resize the data volume.
	// +kubebuilder:default="100Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName for an operator-created claim. Empty uses the cluster default.
	// It is used only when the claim is first created.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// AccessModes for an operator-created claim. The default is ReadWriteMany because every
	// Deployment replica mounts the same store root. ReadWriteOnce requires spec.replicas=1.
	// It is used only when the claim is first created.
	// +kubebuilder:default={ReadWriteMany}
	// +kubebuilder:validation:MinItems=1
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// ExistingClaim names a PersistentVolumeClaim in spec.namespace to mount instead of creating
	// lake-<name>-store. The operator does not adopt, modify, or delete this claim.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`
}

// LakeSealingSpec selects the desired cluster sealing-key generation.
type LakeSealingSpec struct {
	// Generation rotates the sealing key when increased. It cannot be decreased.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Generation int64 `json:"generation,omitempty"`
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

	// Backup configures scheduled Barman object-store backups of the control-plane Cluster.
	// +optional
	Backup LakePostgresBackupSpec `json:"backup,omitempty"`
}

// LakeZettaSpec describes the owned Zetta data plane for a Lake.
type LakeZettaSpec struct {
	// Enabled provisions the OramaZettaCluster. Default true. Setting it false tears the cluster
	// down, including its per-node data volumes.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Image is the Zetta image. Empty uses the OramaZettaCluster default.
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas is the number of equal-peer Zetta nodes.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Storage configures each Zetta node's persistent data volume.
	// +optional
	Storage OramaZettaStorage `json:"storage,omitempty"`

	// Reachability declares whether this data-plane slice is dial-out-only or wan-accessible.
	// Empty defaults to dial-out-only.
	// +kubebuilder:validation:Enum=dial-out-only;wan-accessible
	// +optional
	Reachability string `json:"reachability,omitempty"`
}

// LakePostgresBackupSpec describes scheduled object-store backups for a Lake control-plane
// Postgres cluster. The Secret is in spec.namespace and has the same R2/S3 shape as
// OramaCloudV1's r2SecretRef: s3ApiEndpoint, accessKeyId, and secretAccessKey.
type LakePostgresBackupSpec struct {
	// Enabled turns on the CNPG Barman object store and ScheduledBackup. Default false.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule is a six-field CNPG cron expression.
	// +kubebuilder:default="0 0 3 * * *"
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Retention is the Barman retentionPolicy.
	// +kubebuilder:default="30d"
	// +optional
	Retention string `json:"retention,omitempty"`

	// Destination identifies the object-store location. Bucket is required when Enabled is true.
	// +optional
	Destination LakePostgresBackupDestination `json:"destination,omitempty"`

	// SecretRef names the R2/S3 credentials Secret in spec.namespace. It is required when
	// Enabled is true and must contain s3ApiEndpoint, accessKeyId, and secretAccessKey.
	// +optional
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// LakePostgresBackupDestination identifies the Barman object-store location.
type LakePostgresBackupDestination struct {
	// Bucket is the object-store bucket name. Required when backups are enabled.
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Path is the bucket prefix. Empty defaults to lake-<name>/controldb.
	// +optional
	Path string `json:"path,omitempty"`

	// EndpointURL overrides the Secret's s3ApiEndpoint.
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`
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
	// ZettaReady is true once every desired node in the owned data-plane cluster is ready.
	// +optional
	ZettaReady bool `json:"zettaReady,omitempty"`
	// SchemaPhase mirrors the owned PostgresSchema: Pending | Applying | Applied | Failed.
	// +optional
	SchemaPhase string `json:"schemaPhase,omitempty"`
	// SchemaVersion is the applied schema's content hash (sha256:<12 hex>), from the PostgresSchema.
	// +optional
	SchemaVersion string `json:"schemaVersion,omitempty"`
	// BackupReady is true when the ScheduledBackup exists and the newest CNPG Backup completed.
	// +optional
	BackupReady bool `json:"backupReady,omitempty"`
	// LastBackupTime is the completion time reported by the newest CNPG Backup.
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`
	// DataServices reports the operational evidence for every data service associated with the
	// Lake. Managed is true only when every applicable procedure has a successful rehearsal.
	// +listType=map
	// +listMapKey=name
	// +optional
	DataServices []LakeDataServiceStatus `json:"dataServices,omitempty"`
	// DataServicesSummary is the managed/unmanaged count displayed by kubectl get lakes.
	// +optional
	DataServicesSummary string `json:"dataServicesSummary,omitempty"`
	// Store reports the selected claim and its observed binding state.
	// +optional
	Store *LakeStoreStatus `json:"store,omitempty"`
	// Sealing publishes the cluster-bound recipient used by authoring tools and lake.
	// +optional
	Sealing *LakeSealingStatus `json:"sealing,omitempty"`
	// Conditions includes StoreReady, BackupReady, and ZettaReady, including invalid
	// configuration details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Phase is a coarse summary in the operator's vocabulary: Progressing, Ready, Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
}

// LakeDataServiceStatus reports whether a Lake data service has proven lifecycle procedures.
type LakeDataServiceStatus struct {
	// Name is the stable service identifier within the Lake.
	Name string `json:"name"`
	// Kind identifies the service implementation.
	// +kubebuilder:validation:Enum=postgres;zetta
	Kind string `json:"kind"`
	// Managed is true only when every procedure required by the current configuration is tested.
	Managed bool `json:"managed"`
	// Backup is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Backup string `json:"backup"`
	// Restore is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Restore string `json:"restore"`
	// Upgrade is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Upgrade string `json:"upgrade"`
	// Failover is tested, untested, or none.
	// +kubebuilder:validation:Enum=tested;untested;none
	Failover string `json:"failover"`
	// LastVerified is the completion time of the newest rehearsal for this service.
	// +optional
	LastVerified *metav1.Time `json:"lastVerified,omitempty"`
	// Notes explains an unmanaged label, an unavailable procedure, or the last failed rehearsal.
	// +optional
	Notes string `json:"notes,omitempty"`
}

// LakeSealingStatus publishes the active recipient and retained-key progress.
type LakeSealingStatus struct {
	// Cluster is the AAD cluster value. It is derived from the Lake UID.
	Cluster string `json:"cluster"`
	// Generation is the active key generation.
	Generation int64 `json:"generation"`
	// KID identifies the active public key.
	KID string `json:"kid"`
	// PublicKey is the raw 32-byte X25519 public key encoded as unpadded base64url.
	PublicKey string `json:"publicKey"`
	// PreviousKeys reports retained private keys and records that still require each one.
	// +optional
	PreviousKeys []LakeSealingPreviousKeyStatus `json:"previousKeys,omitempty"`
	// UnresolvedRecords counts malformed records whose KID cannot be established. Old keys are
	// retained while this is nonzero.
	// +optional
	UnresolvedRecords int32 `json:"unresolvedRecords,omitempty"`
}

// LakeSealingPreviousKeyStatus reports why an old private key remains available.
type LakeSealingPreviousKeyStatus struct {
	// KID identifies the retained key.
	KID string `json:"kid"`
	// Remaining is the number of sealed records that still name this key.
	Remaining int32 `json:"remaining"`
}

// LakeStoreStatus is the observed state of the claim mounted at LAKE_STORE_ROOT.
type LakeStoreStatus struct {
	// ClaimName is the selected existing or operator-created PersistentVolumeClaim.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Phase is the PersistentVolumeClaim phase, such as Pending, Bound, or Lost.
	// +optional
	Phase corev1.PersistentVolumeClaimPhase `json:"phase,omitempty"`

	// Capacity is the bound storage capacity reported by the claim.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="DB",type=string,JSONPath=`.status.postgresInstances`
// +kubebuilder:printcolumn:name="Zetta",type=boolean,JSONPath=`.status.zettaReady`
// +kubebuilder:printcolumn:name="Schema",type=string,JSONPath=`.status.schemaPhase`
// +kubebuilder:printcolumn:name="Store",type=string,JSONPath=`.status.store.claimName`
// +kubebuilder:printcolumn:name="Backup",type=date,JSONPath=`.status.lastBackupTime`
// +kubebuilder:printcolumn:name="Data",type=string,JSONPath=`.status.dataServicesSummary`
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
