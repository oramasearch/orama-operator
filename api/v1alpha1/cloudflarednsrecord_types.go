/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareDNSRecordSpec is the desired state of a Cloudflare DNS record. The API token and the
// default zone come from the referenced Secret (keys `token` + `zoneID`), so no credential is in spec.
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
	// ZoneID is the Cloudflare zone holding the record. It overrides the Secret's `zoneID`, so
	// one token can manage records in several zones. Empty = the Secret's `zoneID`.
	// +optional
	ZoneID string `json:"zoneID,omitempty"`
	// Proxied routes the record through Cloudflare's proxy (orange-cloud).
	// +kubebuilder:default=true
	Proxied bool `json:"proxied,omitempty"`
}

// CloudflareDNSRecordStatus is the observed state — the provisioned Cloudflare record.
type CloudflareDNSRecordStatus struct {
	// RecordID is the Cloudflare-assigned DNS record ID (set once created).
	// +optional
	RecordID string `json:"recordID,omitempty"`
	// ZoneID is the Cloudflare zone the record lives in. A record is never moved between zones:
	// when the spec resolves to another zone the record is left in place and Ready is false.
	// Empty on records created before this field existed, which live in the Secret's `zoneID`.
	// +optional
	ZoneID string `json:"zoneID,omitempty"`
	// OwnerComment is the ownership comment (`orama-operator:<namespace>/<name>`) the record was
	// created with. Such a record is deleted only while it still carries the comment. Empty for a
	// record created before ownership comments, which is deleted with its object by ID, as before.
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
// owned by an OramaCloudV1 CR. A finalizer deletes the record from Cloudflare on delete, so
// tearing down the CR cascades to real external cleanup.
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
