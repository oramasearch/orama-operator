/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Restore and the scheduled restore rehearsal of the backup system (operator#741, docs/backups.md
// "Restore"). A BackupRestore restores one Backup into a datastore of the same kind and verifies the
// restored content against the backup's manifest. A RestoreRehearsal does that on a schedule, with the
// newest succeeded Backup of a BackupSchedule, into a scratch datastore, and says when the rehearsal is
// late or failed: a backup that has never been restored is not counted as working (operator#453).
// Both run like Backup: a Job from the operator's own image (`/manager restore ...`), or, for a kind
// whose own operator restores, that operator's resources.

// BackupRestoreSpec is the desired state of a BackupRestore. It is immutable: a restore is one run.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a BackupRestore's spec is immutable"
type BackupRestoreSpec struct {
	// BackupName is the Backup restored, in this namespace. It must have succeeded.
	// +kubebuilder:validation:MinLength=1
	BackupName string `json:"backupName"`
	// Target is the datastore restored into. Its kind must be the Backup's. It must hold none of the
	// backup's content: a restore never overwrites, and a kind whose restore creates the datastore
	// (a CloudNativePG Cluster, a volume claim) creates target.name, which must not exist yet.
	Target BackupDatastoreRef `json:"target"`
	// ReplaceSource allows the target to be the very datastore the Backup was taken of (recovering it
	// in place after a loss). Without it such a restore is refused. Default false.
	// +optional
	ReplaceSource bool `json:"replaceSource,omitempty"`
	// Verify compares the restored content with the backup's manifest. Default true.
	// +kubebuilder:default=true
	// +optional
	Verify *bool `json:"verify,omitempty"`
	// Scratch marks the target as a scratch datastore for a rehearsal: once the restore has finished,
	// what it restored is removed (the databases a ClickHouse restore created, the keys of a Valkey
	// restore, a Cluster or claim the restore created), so the next rehearsal finds it empty.
	// +optional
	Scratch bool `json:"scratch,omitempty"`
}

// BackupRestoreStatus is the observed state of a BackupRestore.
type BackupRestoreStatus struct {
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`
	// Source is where the restored backup is: s3://<bucket>/<path>/<backup>/.
	// +optional
	Source string `json:"source,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// Verified is true when the restored content matched the backup's manifest.
	// +optional
	Verified bool `json:"verified,omitempty"`
	// Evidence is what was compared: how many tables, keys or files, and their totals.
	// +optional
	Evidence string `json:"evidence,omitempty"`
	// Message is the outcome, in the words of whatever refused.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions: Complete (True once restored and, when asked, verified; False with the reason it is
	// waiting or failed).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=brestore
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupName`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Verified",type=boolean,JSONPath=`.status.verified`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTime`

// BackupRestore restores one Backup into a datastore and verifies it.
type BackupRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupRestoreSpec   `json:"spec,omitempty"`
	Status BackupRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupRestoreList contains a list of BackupRestore.
type BackupRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupRestore `json:"items"`
}

// RestoreRehearsalSpec is the desired state of a RestoreRehearsal.
type RestoreRehearsalSpec struct {
	// BackupScheduleName is the schedule, in this namespace, whose newest succeeded Backup each
	// rehearsal restores.
	// +kubebuilder:validation:MinLength=1
	BackupScheduleName string `json:"backupScheduleName"`
	// Target is the scratch datastore restored into. It must not be the schedule's datastore (the
	// controller refuses that too), and is emptied after each rehearsal.
	Target BackupDatastoreRef `json:"target"`
	// Schedule is a five-field cron expression, in UTC. Default weekly, Sunday at 05:00.
	// +kubebuilder:default="0 5 * * 0"
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// Keep is how many finished BackupRestores of this rehearsal are kept. Default 4.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=4
	// +optional
	Keep int32 `json:"keep,omitempty"`
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// RestoreRehearsalStatus is the observed state of a RestoreRehearsal.
type RestoreRehearsalStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// LastSuccessfulRestore names the newest BackupRestore that restored and verified.
	// +optional
	LastSuccessfulRestore string `json:"lastSuccessfulRestore,omitempty"`
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// Conditions: Ready (it can run), LastRehearsalSucceeded, and OnSchedule (False once a whole tick
	// has passed without a verified restore).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rrehearsal
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.backupScheduleName`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Last success",type=date,JSONPath=`.status.lastSuccessTime`
// +kubebuilder:printcolumn:name="On schedule",type=string,JSONPath=`.status.conditions[?(@.type=="OnSchedule")].status`

// RestoreRehearsal restores the newest backup of a schedule into a scratch datastore on a schedule.
type RestoreRehearsal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreRehearsalSpec   `json:"spec,omitempty"`
	Status RestoreRehearsalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestoreRehearsalList contains a list of RestoreRehearsal.
type RestoreRehearsalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RestoreRehearsal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupRestore{}, &BackupRestoreList{}, &RestoreRehearsal{}, &RestoreRehearsalList{})
}
