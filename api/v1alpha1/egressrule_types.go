/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	EgressRuleSourceApp     = "app"
	EgressRuleSourceService = "service"
	EgressRuleSourceAgent   = "agent"

	EgressRuleDesiredPresent = "present"
	EgressRuleDesiredAbsent  = "absent"

	EgressRulePhasePending = "Pending"
	EgressRulePhaseReady   = "Ready"
	EgressRulePhaseFailed  = "Failed"
	EgressRulePhaseDeleted = "Deleted"
)

// EgressRuleSpec lets workloads of one workspace reach one outside destination (operator#470).
// Exactly one of cidr, dnsName and publicInternet is set.
// +kubebuilder:validation:XValidation:rule="(has(self.cidr) ? 1 : 0) + (has(self.dnsName) ? 1 : 0) + ((has(self.publicInternet) && self.publicInternet) ? 1 : 0) == 1",message="exactly one of cidr, dnsName and publicInternet is required"
// +kubebuilder:validation:XValidation:rule="has(self.sourceKind) == has(self.sourceId)",message="sourceKind and sourceId are set together"
// +kubebuilder:validation:XValidation:rule="!(has(self.publicInternet) && self.publicInternet) || !has(self.ruleId)",message="publicInternet is the operator's own rule for a workspace's internetEgress"
type EgressRuleSpec struct {
	// LakeRef names the cluster-scoped Lake that materialized this rule.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lakeRef is immutable"
	LakeRef string `json:"lakeRef"`

	// RuleID is the network_egress_rules row id. Empty for the operator's own rule that carries a
	// workspace's internetEgress.
	// +optional
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ruleId is immutable"
	RuleID string `json:"ruleId,omitempty"`

	// CustomerID is Lake's customer id.
	// +optional
	// +kubebuilder:validation:Format=uuid
	CustomerID string `json:"customerId,omitempty"`

	// WorkspaceID is Lake's workspace id. The rule lives in that workspace's namespace.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspaceId is immutable"
	WorkspaceID string `json:"workspaceId"`

	// SourceKind and SourceID select the workload the rule applies to. Unset: every workload in
	// the workspace.
	// +optional
	// +kubebuilder:validation:Enum=app;service;agent
	SourceKind string `json:"sourceKind,omitempty"`

	// +optional
	// +kubebuilder:validation:Format=uuid
	SourceID string `json:"sourceId,omitempty"`

	// CIDR is an outside address range, enforced by generated policy. Cluster addresses inside it
	// (nodes, pods, the API server, link-local and loopback) are never reachable through a rule.
	// +optional
	CIDR string `json:"cidr,omitempty"`

	// DNSName is an exact host name, enforced by zetta-tunnel's egress gateway: the workload may
	// reach only the gateway, which resolves and authorizes by name.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DNSName string `json:"dnsName,omitempty"`

	// PublicInternet is every public address (private, shared, link-local, multicast, reserved
	// and cluster ranges excluded): what a workspace's internetEgress has always allowed.
	// +optional
	PublicInternet bool `json:"publicInternet,omitempty"`

	// Ports are destination TCP ports.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports"`

	// Desired removes the rule's allowances before the CR is deleted when set to absent.
	// +kubebuilder:validation:Enum=present;absent
	Desired string `json:"desired"`

	// Generation is Lake's desired-row generation, acknowledged in status and Postgres.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// EgressRuleStatus is the observation returned to network_egress_rule_status.
type EgressRuleStatus struct {
	// Phase is Pending, Ready, Failed or Deleted.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the Lake generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions contains the Ready condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=egr
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.spec.cidr`
// +kubebuilder:printcolumn:name="DNS name",type=string,JSONPath=`.spec.dnsName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EgressRule lets workloads of a workspace reach one outside destination by address or by name.
type EgressRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressRuleSpec   `json:"spec"`
	Status EgressRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressRuleList contains a list of EgressRule resources.
type EgressRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressRule{}, &EgressRuleList{})
}
