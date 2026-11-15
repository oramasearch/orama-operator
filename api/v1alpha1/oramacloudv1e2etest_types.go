/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OramaCloudV1E2ETestSpec describes an e2e test run against a deployed OramaCloudV1 stack.
type OramaCloudV1E2ETestSpec struct {
	// TargetRef is the name of the OramaCloudV1 to test (same namespace) — the controller
	// points the test at that CR's engine Services (<target>-writer / <target>-reader).
	TargetRef string `json:"targetRef"`

	// Image is the RUNNER image for the embedded functional e2e suite
	// (assets/e2e-functional.sh). The default (empty, or the pinned alpine default) is the
	// operator's runner: the prebuilt oramasearch/e2e-runner with curl + jq once it is pinned
	// (operator#1158), until then the alpine into which the Job installs them. Another image
	// must provide `sh`, curl and jq; nothing is installed into it once the prebuilt runner is pinned.
	// +kubebuilder:default="alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	Image string `json:"image,omitempty"`

	// Args passed to the test container.
	// +optional
	Args []string `json:"args,omitempty"`

	// PullSecretName lets the Job pull a private test image.
	// Empty, the Job uses the target stack's spec.pullSecretName.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// DashboardFixtureSecretRef names a Secret in the test's namespace describing a dashboard project
	// of the target that the suite may write to, so it can check the dashboard's write tokens end to
	// end: keys collectionID, indexID and apiKey (the project's private API key, as in the e2e
	// canary's Secret) are required. The optional key readerSessionCookie is the `_orama_session`
	// cookie of a member holding only read access to that project, for the wrong-role checks.
	// Omitted, the suite skips its write-token, plan, expiry and role checks and says why. The Job
	// reads the Secret through its environment; nothing from it lands in the ConfigMap.
	// +optional
	DashboardFixtureSecretRef *corev1.LocalObjectReference `json:"dashboardFixtureSecretRef,omitempty"`
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
