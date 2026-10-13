/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetDeletionPolicy says what happens to an instance on a managed cluster when its member is
// removed from the set, or the set is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type SetDeletionPolicy string

const (
	// SetDeletionRetain leaves the instance running and stops managing it.
	SetDeletionRetain SetDeletionPolicy = "Retain"
	// SetDeletionDelete deletes the instance from its cluster.
	SetDeletionDelete SetDeletionPolicy = "Delete"
)

// SetSecret names a Secret on this cluster that is copied into a member's namespace on its
// cluster before the instance is applied.
type SetSecret struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// As is the Secret's name on the member's cluster. Default: Name. A member's secret replaces
	// the set's secret that has the same name there.
	// +optional
	As string `json:"as,omitempty"`
}

// SetMember is one instance on one cluster.
type SetMember struct {
	// ClusterRef names the KubernetesCluster the instance runs on.
	// +kubebuilder:validation:MinLength=1
	ClusterRef string `json:"clusterRef"`
	// Name is the instance's name on that cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Namespace is the instance's own namespace on that cluster. Default: the kind's prefix and
	// Name (lake-<name>, v1-<name>). No two members on one cluster share a namespace.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
	// Spec is this member's own settings, merged over the template: a field here wins over the
	// template's, an object is merged field by field, and a list replaces the template's list.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Spec *apiextensionsv1.JSON `json:"spec,omitempty"`
	// Secrets are this member's own, added to the set's. One with the same name on the member's
	// cluster as a set secret replaces it for this member.
	// +optional
	Secrets []SetSecret `json:"secrets,omitempty"`
}

// ManagedSetSpec is the shape LakeSet and OramaCloudV1Set share.
type ManagedSetSpec struct {
	// Template is the instance's spec (a Lake's, an OramaCloudV1's), the default for every
	// member. It is validated by each member's cluster when applied, and what that cluster
	// refuses is reported on the member.
	// +kubebuilder:pruning:PreserveUnknownFields
	Template apiextensionsv1.JSON `json:"template"`

	// Secrets are copied from this cluster into every member's namespace before its instance is
	// applied, and kept in step afterwards.
	// +optional
	Secrets []SetSecret `json:"secrets,omitempty"`

	// Clusters are the members: one instance on one cluster each, applied in this order. A member
	// waits until the one before it is Ready. Every member's cluster must carry the same
	// `purpose` tag: a set never combines nonprod and production.
	// +kubebuilder:validation:MinItems=1
	Clusters []SetMember `json:"clusters"`

	// DeletionPolicy says what happens to an instance when its member is removed or the set is
	// deleted. Retain (the default) leaves it running.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy SetDeletionPolicy `json:"deletionPolicy,omitempty"`
}

// SetMemberStatus is one instance as its cluster reports it.
type SetMemberStatus struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Phase is Pending (waiting for the cluster, a prerequisite or the member before it),
	// Progressing (applied, not yet Ready), Ready or Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Version is what the instance runs: a Lake's image, an OramaCloudV1's image tag.
	// +optional
	Version string `json:"version,omitempty"`
	// Message says why the member is not Ready, in the words of whatever refused or reported it.
	// +optional
	Message string `json:"message,omitempty"`
}

// ManagedSetStatus is the status LakeSet and OramaCloudV1Set share.
type ManagedSetStatus struct {
	// Phase is Ready (every member Ready), Progressing, or Degraded (the set was refused or a
	// member failed).
	// +optional
	Phase string `json:"phase,omitempty"`
	// Purpose is the `purpose` tag every member's cluster carries.
	// +optional
	Purpose string `json:"purpose,omitempty"`
	// +optional
	Members int32 `json:"members,omitempty"`
	// +optional
	ReadyMembers int32 `json:"readyMembers,omitempty"`
	// MemberStatuses reports each member, in the set's order.
	// +optional
	MemberStatuses []SetMemberStatus `json:"memberStatuses,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=lset
//+kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.status.purpose`
//+kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.members`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyMembers`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// LakeSet installs Lake instances onto clusters this one manages and keeps their versions and
// settings as described: one template, and one member per instance per cluster.
type LakeSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedSetSpec   `json:"spec,omitempty"`
	Status ManagedSetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LakeSetList is a list of LakeSet.
type LakeSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LakeSet `json:"items"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=ocv1set
//+kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.status.purpose`
//+kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.members`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyMembers`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// OramaCloudV1Set installs OramaCloudV1 instances onto clusters this one manages and keeps their
// versions and settings as described. Each instance's region is its cluster's.
type OramaCloudV1Set struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedSetSpec   `json:"spec,omitempty"`
	Status ManagedSetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OramaCloudV1SetList is a list of OramaCloudV1Set.
type OramaCloudV1SetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaCloudV1Set `json:"items"`
}

// SetSpec and SetStatus let one controller reconcile both kinds.
func (s *LakeSet) SetSpec() *ManagedSetSpec     { return &s.Spec }
func (s *LakeSet) SetStatus() *ManagedSetStatus { return &s.Status }

func (s *OramaCloudV1Set) SetSpec() *ManagedSetSpec     { return &s.Spec }
func (s *OramaCloudV1Set) SetStatus() *ManagedSetStatus { return &s.Status }

func init() {
	SchemeBuilder.Register(&LakeSet{}, &LakeSetList{}, &OramaCloudV1Set{}, &OramaCloudV1SetList{})
}
