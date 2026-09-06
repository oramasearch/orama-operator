/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	LakeEnvironmentKindNamed   = "named"
	LakeEnvironmentKindPreview = "preview"

	LakeEnvironmentPhaseProgressing = "Progressing"
	LakeEnvironmentPhaseReady       = "Ready"
	LakeEnvironmentPhaseDegraded    = "Degraded"
	LakeEnvironmentPhaseTornDown    = "TornDown"
)

// LakeEnvironmentVariable is a literal environment variable supplied by a deployment record.
// Secrets use SecretRefs and are delivered by LakeSealedSecret once operator #129 lands.
type LakeEnvironmentVariable struct {
	// Name is the environment variable name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Value is the literal environment variable value.
	Value string `json:"value"`
}

// LakeEnvironmentSpec defines one named or per-change environment belonging to a Lake.
type LakeEnvironmentSpec struct {
	// LakeRef names the cluster-scoped Lake whose namespace contains this resource.
	// +kubebuilder:validation:MinLength=1
	LakeRef string `json:"lakeRef"`

	// Repository is the stable Lake repository identifier.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Environment is the deployment environment name.
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// Kind selects a durable named environment or a per-change preview.
	// +kubebuilder:validation:Enum=named;preview
	Kind string `json:"kind"`

	// Change is the immutable liquid change identifier being deployed.
	// +kubebuilder:validation:MinLength=1
	Change string `json:"change"`

	// Image is an immutable OCI digest reference.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Replicas is the desired workload replica count.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Port is the container port exposed by the environment Service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Env contains literal environment variables for the workload.
	// +optional
	Env []LakeEnvironmentVariable `json:"env,omitempty"`

	// SecretRefs names LakeSealedSecret resources in this resource's namespace. Delivery is
	// intentionally deferred to operator #129; a non-empty list keeps status Degraded until then.
	// +optional
	SecretRefs []string `json:"secretRefs,omitempty"`

	// Host is the exact public hostname for a named environment. Preview environments derive their
	// hostname from Change and the Lake's previewWildcard instead.
	// +optional
	Host string `json:"host,omitempty"`
}

// LakeEnvironmentStatus is the observed state of a deployed environment.
type LakeEnvironmentStatus struct {
	// Phase is Progressing, Ready, Degraded, or TornDown.
	// +optional
	Phase string `json:"phase,omitempty"`

	// URL is the public environment URL when ingress is configured.
	// +optional
	URL string `json:"url,omitempty"`

	// ReadyReplicas mirrors the environment Deployment.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// DeploymentID is the stable deployment-row identifier materialized into this resource.
	// +optional
	DeploymentID string `json:"deploymentId,omitempty"`

	// Conditions contains the standard Ready condition and failure details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lakeenv
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Change",type=string,JSONPath=`.spec.change`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`

// LakeEnvironment materializes one desired deployment row into an isolated namespace and workload.
type LakeEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the desired environment.
	// +required
	Spec LakeEnvironmentSpec `json:"spec"`

	// Status defines the observed environment state.
	// +optional
	Status LakeEnvironmentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeEnvironmentList contains a list of LakeEnvironment resources.
type LakeEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LakeEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeEnvironment{}, &LakeEnvironmentList{})
}
