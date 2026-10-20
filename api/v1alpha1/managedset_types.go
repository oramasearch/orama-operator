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

	// Clusters are the members: one instance on one cluster each. Members are independent: none
	// waits for another. Every member's cluster must carry the same `purpose` tag: a set never
	// combines nonprod and production. At most 64: OramaCloudV1Set's CEL rule that each migration
	// names one member loops over them, and the API server refuses a rule over an unbounded list.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
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
	// Phase is Pending (waiting for the cluster or a prerequisite), Progressing (applied, not
	// yet Ready), Ready or Degraded.
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
	// MemberStatuses reports each member, in the order the set lists them.
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
//+kubebuilder:printcolumn:name="Migrations",type=string,JSONPath=`.status.migrationSummary`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// OramaCloudV1Set installs OramaCloudV1 instances onto clusters this one manages and keeps their
// versions and settings as described. Each instance's region is its cluster's. It also copies Prod 1
// teams into its members (spec.migrations).
type OramaCloudV1Set struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaCloudV1SetSpec   `json:"spec,omitempty"`
	Status OramaCloudV1SetStatus `json:"status,omitempty"`
}

// OramaCloudV1SetSpec is a ManagedSetSpec plus the team migrations into its members.
// +kubebuilder:validation:XValidation:rule="!has(self.migrations) || self.migrations.all(m, self.clusters.exists_one(c, c.name == m.member))",message="every migrations[].member must name exactly one clusters[].name"
type OramaCloudV1SetSpec struct {
	ManagedSetSpec `json:",inline"`
	// Migrations copy Prod 1 teams into this set's members. Each becomes an OramaCloudV1Migration
	// named Name in its member's namespace on its member's cluster. Removing one deletes it there.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=256
	Migrations []SetMigration `json:"migrations,omitempty"`
}

// SetMigration is one Prod 1 team copied into one member (operator#654).
type SetMigration struct {
	// Name is the OramaCloudV1Migration's name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Member names the set member (SetMember.Name) the team is copied into. It cannot change once
	// applied: a migration into another member is a new entry under a new name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	Member string `json:"member"`
	// Secrets are copied into the member's namespace before the migration is applied (the Prod 1
	// credentials and the evidence receipt), as SetMember.Secrets are, and deleted once the migration
	// is gone. One with the name of a set or member Secret there is refused: a migration never changes
	// what the stack mounts.
	// +optional
	Secrets []SetSecret `json:"secrets,omitempty"`
	// Spec is the OramaCloudV1Migration's spec. targetRef is filled in from the member; leave it out.
	// It is validated by the member's cluster when applied, and what that cluster refuses is reported
	// on the migration.
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec apiextensionsv1.JSON `json:"spec"`
}

// SetMigrationStatus is one migration as its member's cluster reports it.
type SetMigrationStatus struct {
	Name   string `json:"name"`
	Member string `json:"member"`
	// Cluster and Namespace are where it was applied. A migration removed from the set is deleted
	// there, and its member, if it leaves too, waits for it.
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Step is the step its last Job ran.
	// +optional
	Step string `json:"step,omitempty"`
	// Phase is the migration's own phase (Pending, Running, Succeeded, Blocked, Failed, Removing), or
	// Pending while the set waits to apply it, or Blocked when the set refuses to (Message says why).
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Secrets are the Secrets the set copied for it, by their name on the member's cluster, deleted
	// once it is gone.
	// +optional
	Secrets []string `json:"secrets,omitempty"`
}

// OramaCloudV1SetStatus is a ManagedSetStatus plus the migrations into its members. The set's phase
// does not depend on them.
type OramaCloudV1SetStatus struct {
	ManagedSetStatus `json:",inline"`
	// Migrations reports each migration: the declared ones in the order the set lists them, then the
	// ones still being removed.
	// +optional
	Migrations []SetMigrationStatus `json:"migrations,omitempty"`
	// MigrationSummary counts the declared migrations whose last step succeeded ("1/2 Succeeded").
	// Empty when the set declares none.
	// +optional
	MigrationSummary string `json:"migrationSummary,omitempty"`
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

func (s *OramaCloudV1Set) SetSpec() *ManagedSetSpec     { return &s.Spec.ManagedSetSpec }
func (s *OramaCloudV1Set) SetStatus() *ManagedSetStatus { return &s.Status.ManagedSetStatus }

func init() {
	SchemeBuilder.Register(&LakeSet{}, &LakeSetList{}, &OramaCloudV1Set{}, &OramaCloudV1SetList{})
}
