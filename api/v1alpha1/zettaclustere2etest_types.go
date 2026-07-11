/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZettaClusterE2ETestSpec describes an e2e test run against a deployed OramaZettaCluster.
type ZettaClusterE2ETestSpec struct {
	// TargetRef is the name of the OramaZettaCluster to test (same namespace) — the controller
	// points the test at that cluster's per-pod headless DNS (node-0 as the writer, ordinals
	// 1..replicas-1 as the read peers).
	TargetRef string `json:"targetRef"`

	// Image is the RUNNER image that carries the e2e script (tests/e2e/zetta-e2e.sh, baked into
	// the image; its ENTRYPOINT runs the script). Unlike the OramaCloudV1 suite (embedded in the
	// operator), the Zetta suite lives in the zetta repo and ships in this image so it evolves with
	// the engine. Default is the pinned sandbox runner.
	// +kubebuilder:default="oramasearch/zetta-e2e:v1-sandbox"
	Image string `json:"image,omitempty"`

	// Levels selects which test levels the script runs (ZETTA_E2E_LEVELS). Default "2,3".
	// +kubebuilder:default="2,3"
	Levels string `json:"levels,omitempty"`

	// PullSecretName lets the Job pull a private test image.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// ZettaClusterE2ETestStatus is the observed state of a test run.
type ZettaClusterE2ETestStatus struct {
	// Phase: Pending | Running | Succeeded | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=ozce2e
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
//+kubebuilder:printcolumn:name="Levels",type=string,JSONPath=`.spec.levels`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`

// ZettaClusterE2ETest runs the live-cluster zetta e2e suite as a Kubernetes Job against a deployed
// OramaZettaCluster and reports pass/fail in status. A separate CRD so a test is a first-class,
// repeatable resource — the operator never runs suites during normal reconcile. The suite itself
// lives in the zetta repo (tests/e2e/zetta-e2e.sh) and is baked into the runner image, so it stays
// in lockstep with the engine; this CRD only triggers it and wires the cluster addressing via env.
type ZettaClusterE2ETest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZettaClusterE2ETestSpec   `json:"spec,omitempty"`
	Status ZettaClusterE2ETestStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ZettaClusterE2ETestList contains a list of ZettaClusterE2ETest.
type ZettaClusterE2ETestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZettaClusterE2ETest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZettaClusterE2ETest{}, &ZettaClusterE2ETestList{})
}
