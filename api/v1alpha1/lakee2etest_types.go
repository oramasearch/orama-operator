/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LakeE2ETestSpec describes an e2e test run against a deployed Lake.
type LakeE2ETestSpec struct {
	// TargetRef is the name of the cluster-scoped Lake to test. The controller runs the test Job
	// in that Lake's spec.namespace and points it at the Lake's in-cluster Service.
	// +kubebuilder:validation:MinLength=1
	TargetRef string `json:"targetRef"`

	// Image is the runner image whose entrypoint signs up a fresh user, creates a repository,
	// pushes it through liquid sync, pulls it into an empty checkout, and verifies the file bytes.
	// The image is required because the suite evolves with lake and liquid, outside this operator.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// PullSecretName authenticates the runner image pull. When empty, the controller reuses the
	// target Lake's spec.pullSecretName. The Secret must exist in the Lake namespace.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// LakeE2ETestStatus is the observed state of a test run.
type LakeE2ETestStatus struct {
	// Phase is Pending, Running, Succeeded, or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// JobName is the namespaced Job running the suite.
	// +optional
	JobName string `json:"jobName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lakee2e
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`

// LakeE2ETest runs lake's live service suite as a Kubernetes Job against a deployed Lake.
// The runner image owns the signup and liquid sync workflow; this resource supplies the target
// address, schedules the Job only after the Lake is ready, and reports its terminal result.
type LakeE2ETest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired test run.
	// +required
	Spec LakeE2ETestSpec `json:"spec"`

	// status defines the observed test run.
	// +optional
	Status LakeE2ETestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeE2ETestList contains a list of LakeE2ETest resources.
type LakeE2ETestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LakeE2ETest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeE2ETest{}, &LakeE2ETestList{})
}
