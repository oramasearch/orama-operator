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
// +kubebuilder:validation:XValidation:rule="!has(self.sharedOwner) || size(self.sharedOwner) == 0 || (has(self.deletionPolicy) && self.deletionPolicy == 'Retain')",message="sharedOwner requires deletionPolicy Retain"
type CloudflareDNSRecordSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `zoneID`,
	// `accountID`) — normally the CR's `spec.cloudflare.apiTokenSecretRef`.
	SecretRef string `json:"secretRef"`
	// Name is the record FQDN (e.g. app.orama.com).
	Name string `json:"name"`
	// Type is A, CNAME or TXT; empty = inferred from Content (IP → A, hostname → CNAME).
	// +optional
	Type string `json:"type,omitempty"`
	// Content is the record target (an IP for A, a hostname for CNAME, the text for TXT).
	Content string `json:"content"`
	// Proxied routes the record through Cloudflare's proxy (orange-cloud). Always written, so a
	// controller's false is kept rather than replaced by the default (a Clerk record must not be
	// proxied); a resource that leaves it out gets true.
	// +kubebuilder:default=true
	// +optional
	Proxied bool `json:"proxied"`
	// DeletionPolicy decides what deleting this resource does to the Cloudflare record. Delete (the
	// default) removes it. Retain leaves it: for a record another resource may also keep, such as a
	// Clerk production instance's records, which every Lake sharing that instance writes.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
	// SharedOwner names the one owner of a record several resources keep, such as `clerk/<instance
	// ID>` for a Clerk production instance's records. The record is created with the comment
	// `orama-operator:<sharedOwner>` instead of `orama-operator:<namespace>/<name>`, so every
	// resource with the same sharedOwner recognises it as the operator's own; a record without that
	// comment is still refused. It requires deletionPolicy Retain: no single resource deletes it.
	// +optional
	SharedOwner string `json:"sharedOwner,omitempty"`
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
