/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OramaCloudV1E2ETestSpec describes an e2e test run against a deployed OramaCloudV1 stack.
type OramaCloudV1E2ETestSpec struct {
	// TargetRef is the name of the OramaCloudV1 to test (same namespace) — the controller
	// points the test at that CR's engine Services (<target>-writer / <target>-reader).
	TargetRef string `json:"targetRef"`

	// Image is the RUNNER base image for the embedded functional e2e suite
	// (assets/e2e-functional.sh). It must provide `sh`; the Job installs curl + jq into it
	// before running the suite. Default is a pinned alpine (has apk). Override only with
	// another curl/jq-capable base.
	// +kubebuilder:default="alpine:3.20"
	Image string `json:"image,omitempty"`

	// Args passed to the test container.
	// +optional
	Args []string `json:"args,omitempty"`

	// PullSecretName lets the Job pull a private test image.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`
}

// OramaCloudV1E2ETestStatus is the observed state of a test run.
type OramaCloudV1E2ETestStatus struct {
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
//+kubebuilder:resource:shortName=ocv1e2e
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`

// OramaCloudV1E2ETest runs the oramacore-test-e2e suite as a Kubernetes Job against a deployed
// OramaCloudV1 stack and reports pass/fail in status. A separate CRD so a test is a first-class,
// repeatable resource — the operator never runs suites during normal reconcile.
type OramaCloudV1E2ETest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaCloudV1E2ETestSpec   `json:"spec,omitempty"`
	Status OramaCloudV1E2ETestStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OramaCloudV1E2ETestList contains a list of OramaCloudV1E2ETest.
type OramaCloudV1E2ETestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaCloudV1E2ETest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OramaCloudV1E2ETest{}, &OramaCloudV1E2ETestList{})
}
