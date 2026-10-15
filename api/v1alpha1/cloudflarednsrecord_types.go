/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareDNSRecordSpec is the desired state of a Cloudflare DNS record. The zone + API
// token come from the referenced Secret (keys `token` + `zoneID`), so no credential is in spec.
type CloudflareDNSRecordSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `zoneID`,
	// `accountID`) — normally the CR's `spec.cloudflare.apiTokenSecretRef`.
	SecretRef string `json:"secretRef"`
	// Name is the record FQDN (e.g. app.orama.com).
	Name string `json:"name"`
	// Type is A or CNAME; empty = inferred from Content (IP → A, hostname → CNAME).
	// +optional
	Type string `json:"type,omitempty"`
	// Content is the record target (an IP for A, a hostname for CNAME).
	Content string `json:"content"`
	// Proxied routes the record through Cloudflare's proxy (orange-cloud).
	// +kubebuilder:default=true
	Proxied bool `json:"proxied,omitempty"`
}

// CloudflareDNSRecordStatus is the observed state — the provisioned Cloudflare record.
type CloudflareDNSRecordStatus struct {
	// RecordID is the Cloudflare ID created by this resource or tracked before comments.
	// +optional
	RecordID string `json:"recordID,omitempty"`
	// OwnerComment is the exact ownership comment written on creation. Empty with a
	// RecordID identifies a legacy record; reconciliation never adds a comment to it.
	// +optional
	OwnerComment string `json:"ownerComment,omitempty"`
	// Ready is true once the record is reconciled in Cloudflare.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=cfdns
//+kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
//+kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.spec.content`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// CloudflareDNSRecord is a Cloudflare DNS record the operator provisions via the CF API,
// optionally owned by a parent CR. It never adopts foreign records. Its finalizer deletes
// only records still carrying its ownership comment, or pre-comment legacy status IDs.
type CloudflareDNSRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareDNSRecordSpec   `json:"spec,omitempty"`
	Status CloudflareDNSRecordStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareDNSRecordList contains a list of CloudflareDNSRecord.
type CloudflareDNSRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareDNSRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareDNSRecord{}, &CloudflareDNSRecordList{})
}
