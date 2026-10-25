/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LakeRestoreRehearsalSpec defines a lifecycle rehearsal for one Lake data service.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="rehearsal spec is immutable"
// +kubebuilder:validation:XValidation:rule="self.operation != 'upgrade' || (has(self.targetImage) && size(self.targetImage) > 0)",message="targetImage is required for upgrade"
// +kubebuilder:validation:XValidation:rule="self.service != 'store' || self.operation == 'restore'",message="the store service supports only the restore operation"
type LakeRestoreRehearsalSpec struct {
	// LakeRef names the cluster-scoped Lake whose data service is rehearsed.
	// +kubebuilder:validation:MinLength=1
	LakeRef string `json:"lakeRef"`

	// Service is what is rehearsed: postgres, the CNPG control database restored from its CNPG
	// object-store Backups, or store, Lake's store volume restored from the operator's CSI
	// VolumeSnapshots of it (docs/lake/volume-snapshots.md). The store supports only restore.
	// +kubebuilder:validation:Enum=postgres;store
	Service string `json:"service"`

	// Operation selects the lifecycle procedure.
	// +kubebuilder:validation:Enum=restore;upgrade;failover
	Operation string `json:"operation"`

	// From selects what is restored. Empty or latest selects the newest completed CNPG Backup
	// (postgres) or the newest ready VolumeSnapshot the operator took of the store (store). Any
	// other value is the name of a Backup owned by the Lake control DB, or of an operator-taken
	// store VolumeSnapshot, in the Lake's namespace.
	// +kubebuilder:default=latest
	// +optional
	From string `json:"from,omitempty"`

	// TargetImage is the CloudNativePG PostgreSQL operand image tested by an upgrade rehearsal.
	// It is required when operation is upgrade and ignored by other operations.
	// +optional
	TargetImage string `json:"targetImage,omitempty"`
}

// LakeRestoreRehearsalStatus records a durable lifecycle rehearsal outcome.
type LakeRestoreRehearsalStatus struct {
	// Phase is Pending, Running, Succeeded, or Failed.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message describes the current stage or terminal outcome.
	// +optional
	Message string `json:"message,omitempty"`
	// ScratchNamespace contains the isolated recovery cluster for restore and upgrade operations.
	// +optional
	ScratchNamespace string `json:"scratchNamespace,omitempty"`
	// SelectedBackup is the CNPG Backup object selected for a restore or upgrade.
	// +optional
	SelectedBackup string `json:"selectedBackup,omitempty"`
	// SelectedSnapshot is the store VolumeSnapshot selected for a store restore, in the Lake's
	// namespace.
	// +optional
	SelectedSnapshot string `json:"selectedSnapshot,omitempty"`
	// Evidence is what the verifier reported on success or failure: for a store restore, the
	// restored volume's file count, size and top-level entries.
	// +optional
	Evidence string `json:"evidence,omitempty"`
	// PreviousPrimary is the primary observed before a failover rehearsal.
	// +optional
	PreviousPrimary string `json:"previousPrimary,omitempty"`
	// TargetPrimary is the healthy replica selected for a failover rehearsal.
	// +optional
	TargetPrimary string `json:"targetPrimary,omitempty"`
	// StartedAt is set once when the controller accepts the rehearsal.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is set once with the terminal outcome and Lake status recording.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// ObservedGeneration is the spec generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lrr
// +kubebuilder:printcolumn:name="Lake",type=string,JSONPath=`.spec.lakeRef`
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.service`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.spec.operation`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completedAt`

// LakeRestoreRehearsal is the Schema for Lake data-service lifecycle rehearsals.
type LakeRestoreRehearsal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec LakeRestoreRehearsalSpec `json:"spec"`
	// +optional
	Status LakeRestoreRehearsalStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LakeRestoreRehearsalList contains a list of LakeRestoreRehearsal objects.
type LakeRestoreRehearsalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LakeRestoreRehearsal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeRestoreRehearsal{}, &LakeRestoreRehearsalList{})
}
