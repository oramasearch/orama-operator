// SPDX-License-Identifier: MIT

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LakeE2ETestMember names the Lake a LakeSet put on this cluster: the member's cluster-scoped
// Lake is named after the member and carries the label orama.cloud/managed-by-set=LakeSet.<set>.
type LakeE2ETestMember struct {
	// LakeSet is the set's name, e.g. lake-nonprod.
	// +kubebuilder:validation:MinLength=1
	LakeSet string `json:"lakeSet"`

	// Name is the member's name, e.g. testing. It is also the Lake's name on this cluster.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LakeE2ETestSpec describes an e2e test run against a deployed Lake.
// +kubebuilder:validation:XValidation:rule="has(self.targetRef) != has(self.member)",message="set exactly one of targetRef and member"
type LakeE2ETestSpec struct {
	// Member is the LakeSet member to test, the usual target: the testing member is the testing ->
	// staging gate of a substantial lake release. The controller refuses a Lake of that name that
	// the set does not manage. Exactly one of member and targetRef is set.
	// +optional
	Member *LakeE2ETestMember `json:"member,omitempty"`

	// TargetRef is the name of a cluster-scoped Lake to test that no set manages. Exactly one of
	// member and targetRef is set.
	// +optional
	TargetRef string `json:"targetRef,omitempty"`

	// Image is the runner, oramasearch/lake-e2e at an immutable tag. Its entrypoint runs every
	// Lake feature as a fresh test person (REST, MCP and a headless browser) and prints one
	// result line per step and a final `Lake e2e: pass=N fail=N skip=N`.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// PullSecretName authenticates the runner image pull. When empty, the controller reuses the
	// target Lake's spec.pullSecretName. The Secret must exist in the Lake namespace.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// RunID names a run. Changing it (or anything else in spec) runs the suite again in a new Job,
	// without deleting this resource; the previous run's Job is kept until a newer run finishes.
	// +optional
	RunID string `json:"runID,omitempty"`

	// Env adds environment variables to the runner, e.g. LAKE_E2E_CLERK_SECRET_KEY from a Secret
	// for a Lake that signs people in with Clerk, or LAKE_E2E_GROUPS to run part of the suite.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// TimeoutSeconds bounds one run (the Job's activeDeadlineSeconds). Default 3 hours.
	// +kubebuilder:validation:Minimum=60
	// +optional
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// LakeE2ETestStep is one step that did not pass, with the runner's reason.
type LakeE2ETestStep struct {
	// Step is the step's id, e.g. repositories.liquid-push.
	Step string `json:"step"`

	// Reason is why it failed or was skipped.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// LakeE2ETestStatus is the observed state of the latest run.
type LakeE2ETestStatus struct {
	// Phase is Pending, Running, Succeeded, or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// JobName is the namespaced Job running the latest run.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// Target is the Lake the latest run tests.
	// +optional
	Target string `json:"target,omitempty"`

	// ObservedGeneration is the spec generation the latest run was started for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RunID is the LAKE_E2E_RUN_ID of the latest run.
	// +optional
	RunID string `json:"runID,omitempty"`

	// LakeRevision is the revision of the Lake the run tested, as its server_info reported it.
	// +optional
	LakeRevision string `json:"lakeRevision,omitempty"`

	// Pass, Fail and Skip count the latest run's steps, read from the runner's result.
	// +optional
	Pass int32 `json:"pass,omitempty"`
	// +optional
	Fail int32 `json:"fail,omitempty"`
	// +optional
	Skip int32 `json:"skip,omitempty"`

	// Failed lists the steps that failed, with their reasons (at most 50).
	// +optional
	Failed []LakeE2ETestStep `json:"failed,omitempty"`

	// Skipped lists the steps that were skipped, with their reasons (at most 50).
	// +optional
	Skipped []LakeE2ETestStep `json:"skipped,omitempty"`

	// StartedAt and CompletedAt bound the latest run.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lakee2e
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.target`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pass",type=integer,JSONPath=`.status.pass`
// +kubebuilder:printcolumn:name="Fail",type=integer,JSONPath=`.status.fail`
// +kubebuilder:printcolumn:name="Skip",type=integer,JSONPath=`.status.skip`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`

// LakeE2ETest runs lake's end-to-end suite (the oramasearch/lake-e2e runner) as a Kubernetes Job
// against a deployed Lake, usually a LakeSet member, and reports each run's pass, fail and skip
// counts with the reasons. Changing the spec (spec.runID) runs it again.
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
