/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The steps of a migration, in order, as spec.step names them.
const (
	MigrationStepPlan    = "plan"
	MigrationStepSeed    = "seed"
	MigrationStepCatchUp = "catchUp"
	MigrationStepVerify  = "verify"
)

// The phases of a migration's status.
const (
	MigrationPhasePending   = "Pending"
	MigrationPhaseRunning   = "Running"
	MigrationPhaseSucceeded = "Succeeded"
	MigrationPhaseBlocked   = "Blocked"
	MigrationPhaseFailed    = "Failed"
	MigrationPhaseRemoving  = "Removing"
)

// OramaCloudV1MigrationSpec declares one Prod 1 team copied into one OramaCloudV1 stack.
// +kubebuilder:validation:XValidation:rule="self.targetRef == oldSelf.targetRef && self.teamID == oldSelf.teamID && self.projectIDs == oldSelf.projectIDs && self.runID == oldSelf.runID && self.source == oldSelf.source",message="the target, team, projects, run and source are immutable; declare a new migration instead"
type OramaCloudV1MigrationSpec struct {
	// TargetRef names the OramaCloudV1 in this namespace that receives the team.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	TargetRef string `json:"targetRef"`

	// TeamID is the Prod 1 team (a UUID) being migrated.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	TeamID string `json:"teamID"`

	// ProjectIDs is the reviewed, complete project set of the team. The tool refuses a list that
	// differs from the team's actual projects.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +listType=set
	ProjectIDs []string `json:"projectIDs"`

	// RunID names the tool's durable run; the same run is resumed by every step.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	RunID string `json:"runID"`

	// Source is Prod 1: where it is and the Secrets that reach it read-only.
	Source OramaCloudV1MigrationSource `json:"source"`

	// Step is how far the migration should go: plan, seed, catchUp or verify. Each change runs that
	// step once.
	// +kubebuilder:validation:Enum=plan;seed;catchUp;verify
	Step string `json:"step"`

	// Apply lets seed and catchUp write to the target. Without it they only check (the tool's dry
	// run). plan and verify never write, and ignore it.
	// +optional
	Apply bool `json:"apply,omitempty"`

	// Attempt runs the current step again when raised (a catch-up before the cutover, a retry after
	// a Blocked or Failed step was fixed). Raised while the resource is being deleted, it retries a
	// removal that did not finish.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// EvidenceSecretRef names the Secret key holding the isolation receipt the tool requires for
	// seed and catchUp (the migration tool's isolation and preflight evidence). It is mounted read-only for those two steps only. The operator does not create it.
	// +optional
	EvidenceSecretRef *corev1.SecretKeySelector `json:"evidenceSecretRef,omitempty"`

	// Image is the cloud-v1-migration image, pinned by digest (name@sha256:<64 hex>).
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// PullSecretName names the Secret the Job pulls Image with.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// RunVolume holds the tool's run directory (manifest and payloads), kept across steps so the
	// tool resumes from it. It contains customer data. Default 20Gi, the cluster's default class.
	// +optional
	RunVolume VolumeSpec `json:"runVolume,omitempty"`

	// RemoveOnDelete removes the copied team from the target when this resource is deleted, with the
	// tool's remove-target. The tool refuses once the team has been switched to the target, and the
	// resource then stays until a person decides.
	// +optional
	RemoveOnDelete bool `json:"removeOnDelete,omitempty"`
}

// OramaCloudV1MigrationSource is Prod 1, reached read-only.
type OramaCloudV1MigrationSource struct {
	// DatabaseSecretRef names the Secret key holding the SELECT-only PlanetScale URL
	// (planetscale-dump-ro).
	DatabaseSecretRef corev1.SecretKeySelector `json:"databaseSecretRef"`

	// DatabaseHost is the host name in that URL. It is used only to admit the migration pod's
	// egress; the URL itself stays in the Secret.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	DatabaseHost string `json:"databaseHost"`

	// WriterURL and ReaderURL are Prod 1's HTTPS engine origins (https://<host>, no port or path).
	// +kubebuilder:validation:MaxLength=261
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	WriterURL string `json:"writerURL"`
	// +kubebuilder:validation:MaxLength=261
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	ReaderURL string `json:"readerURL"`

	// MasterKeySecretRef names the Secret key holding Prod 1's engine master key.
	MasterKeySecretRef corev1.SecretKeySelector `json:"masterKeySecretRef"`

	// DeploymentRevision and SchemaRevision are the reviewed Prod 1 revisions (v1.2.39 and
	// 890a7e97 on 2026-10-19).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DeploymentRevision string `json:"deploymentRevision"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	SchemaRevision string `json:"schemaRevision"`

	// Settings reaches the Prod 1 writer's per-index info.json through one read-only SSM document.
	Settings OramaCloudV1MigrationSettings `json:"settings"`
}

// OramaCloudV1MigrationSettings is where the read-only SSM document that reads the Prod 1 writer's
// index settings runs, and the IAM key allowed to run it.
type OramaCloudV1MigrationSettings struct {
	// Region is the AWS region of the writer instance (us-east-2).
	// +kubebuilder:validation:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	// +kubebuilder:validation:MaxLength=32
	Region string `json:"region"`

	// InstanceID is the writer's EC2 instance.
	// +kubebuilder:validation:Pattern=`^i-[0-9a-f]{8,17}$`
	InstanceID string `json:"instanceID"`

	// Document is the SSM document's name.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{3,128}$`
	Document string `json:"document"`

	// DataDir is the writer's writer_side.collections.data_dir, an absolute path.
	// +kubebuilder:validation:Pattern=`^/[^\s]*$`
	// +kubebuilder:validation:MaxLength=1024
	DataDir string `json:"dataDir"`

	// CredentialsSecretRef names a Secret with access_key_id and secret_access_key: the IAM user that
	// may only run that document on that instance.
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// OramaCloudV1MigrationStatus is what the last Job ran and how it ended. Written only on a real
// transition (principle 7).
type OramaCloudV1MigrationStatus struct {
	// Phase: Pending (waiting for the target or a Secret), Running, Succeeded, Blocked (the operator
	// refused the step, or the tool's guard did: exit 3), Failed, Removing.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Step, Attempt and Apply are what the last Job ran.
	// +optional
	Step string `json:"step,omitempty"`
	// +optional
	Attempt int32 `json:"attempt,omitempty"`
	// +optional
	Apply bool `json:"apply,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`

	// Result is the tool's status --json from the Job's termination message, verbatim.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Result *apiextensionsv1.JSON `json:"result,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Seeded (an applied seed succeeded under this resource), TargetWritten (an applied
	// seed or catch-up started, so deletion with removeOnDelete removes the team) and Removed (the
	// removal on deletion succeeded, so it never runs again). Each is set once and never cleared.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=ocv1m
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
//+kubebuilder:printcolumn:name="Step",type=string,JSONPath=`.spec.step`
//+kubebuilder:printcolumn:name="Apply",type=boolean,JSONPath=`.spec.apply`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OramaCloudV1Migration copies one Prod 1 team into one OramaCloudV1 stack in its namespace. The
// operator runs the cloud-v1-migration tool's run-step as one Job per step, attempt and apply,
// and reports the tool's outcome in status.
type OramaCloudV1Migration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaCloudV1MigrationSpec   `json:"spec,omitempty"`
	Status OramaCloudV1MigrationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OramaCloudV1MigrationList contains a list of OramaCloudV1Migration.
type OramaCloudV1MigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaCloudV1Migration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OramaCloudV1Migration{}, &OramaCloudV1MigrationList{})
}
