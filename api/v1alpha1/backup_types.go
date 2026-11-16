/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// One backup system for every datastore the operator runs (operator#453). A BackupSchedule says
// which datastore, which bucket, when and how many to keep; on each tick it creates a Backup. A
// Backup is one backup, run as a Job from the operator's own image (`/manager backup ...`) in the
// datastore's namespace. Each datastore kind has its own mechanism behind one Go interface
// (internal/backup); a kind with no mechanism yet is refused in the status, never half-run.
// See docs/backups.md.

// BackupDatastoreKind names the kind of datastore a backup is taken of. Kafka has no backup, by
// decision (operator#733): its schedules and Backups are refused with reason NoBackupByDecision and
// the decision in words; see docs/backups.md "Kafka".
// +kubebuilder:validation:Enum=Postgres;ClickHouse;Zetta;Kafka;Cassandra;TiDB;Valkey;SQLite;DuckDB;Polars;Volume
type BackupDatastoreKind string

const (
	BackupDatastorePostgres   BackupDatastoreKind = "Postgres"
	BackupDatastoreClickHouse BackupDatastoreKind = "ClickHouse"
	BackupDatastoreZetta      BackupDatastoreKind = "Zetta"
	BackupDatastoreKafka      BackupDatastoreKind = "Kafka"
	BackupDatastoreCassandra  BackupDatastoreKind = "Cassandra"
	BackupDatastoreTiDB       BackupDatastoreKind = "TiDB"
	BackupDatastoreValkey     BackupDatastoreKind = "Valkey"
	BackupDatastoreSQLite     BackupDatastoreKind = "SQLite"
	BackupDatastoreDuckDB     BackupDatastoreKind = "DuckDB"
	BackupDatastorePolars     BackupDatastoreKind = "Polars"
	// BackupDatastoreVolume is a volume claim, copied file by file from a CSI snapshot of it
	// (operator#738): a Lake's store.
	BackupDatastoreVolume BackupDatastoreKind = "Volume"
)

// BackupDatastoreRef identifies the datastore, which is in the same namespace as the backup.
// +kubebuilder:validation:XValidation:rule="!(self.kind in ['ClickHouse', 'Zetta', 'Valkey']) || has(self.connectionSecretRef)",message="connectionSecretRef is required for a ClickHouse, Zetta or Valkey datastore"
type BackupDatastoreRef struct {
	// Kind is the datastore's kind; it selects the backup mechanism.
	Kind BackupDatastoreKind `json:"kind"`
	// Name is the datastore's name in the namespace (its Service or resource name). It is part
	// of the default bucket path and labels each Backup.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// ConnectionSecretRef names a Secret in the namespace with the datastore's connection: keys
	// `host` and `username` and `password`, and optionally `port` and `database`. The backup Job
	// reads it as environment variables; the operator never copies it. Required for the kinds whose
	// backup Job talks to the server (ClickHouse, Zetta, Valkey); the kinds whose own operator
	// takes the backup need none.
	// +optional
	ConnectionSecretRef *LocalSecretReference `json:"connectionSecretRef,omitempty"`
	// PodSelector selects the datastore's pods. While a backup or restore runs, the operator lets
	// those pods accept the run's pod, and, for a kind whose server writes to the bucket itself
	// (ClickHouse, TiDB, Cassandra), reach public HTTPS on 443 (operator#740). Empty means the
	// kind's default: the labels its operator puts on the pods of a datastore of this name.
	// +optional
	PodSelector map[string]string `json:"podSelector,omitempty"`
	// Volume is how a volume-backed datastore (Volume, SQLite, DuckDB, Polars; name is then the
	// volume claim) is read: the backup takes a CSI VolumeSnapshot of the claim, mounts a claim made
	// from it read-only in the backup Job, and copies its files to the bucket (operator#738,
	// operator#737).
	// +optional
	Volume *BackupVolumeSource `json:"volume,omitempty"`
}

// BackupVolumeSource is how a volume-backed datastore's claim is snapshotted and read.
type BackupVolumeSource struct {
	// VolumeSnapshotClassName is the class of the snapshot the copy is made from. Empty means
	// lake-retain, the Retain class the platform layer installs.
	// +optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
	// RunAsUser and RunAsGroup are the user and group the copy runs as, so it can read files only
	// their owner may: the workload's own. Default 65532.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
}

// LocalSecretReference names a Secret in the same namespace.
type LocalSecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// BackupBucketRef identifies the bucket backups are written to: a bucket the operator provisions.
type BackupBucketRef struct {
	// Kind is the bucket resource's kind. Only CloudflareR2Bucket today.
	// +kubebuilder:validation:Enum=CloudflareR2Bucket
	// +kubebuilder:default=CloudflareR2Bucket
	// +optional
	Kind string `json:"kind,omitempty"`
	// Name is the bucket resource's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace is the bucket resource's namespace; empty means the backup's own. A bucket in
	// another namespace must list this namespace in its spec.credentials.allowedNamespaces.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// BackupScheduleSpec is the desired state of a BackupSchedule.
type BackupScheduleSpec struct {
	// Datastore is what is backed up.
	Datastore BackupDatastoreRef `json:"datastore"`
	// Bucket is where backups are written.
	Bucket BackupBucketRef `json:"bucket"`
	// Path is the prefix in the bucket; each Backup writes under <path>/<backup name>/. Empty means
	// <namespace>/<datastore kind>/<datastore name>, in lower case.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`
	// +optional
	Path string `json:"path,omitempty"`
	// Schedule is a five-field cron expression, in UTC. Default daily at 03:00.
	// +kubebuilder:default="0 3 * * *"
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// Keep is how many succeeded backups are kept; older ones are deleted with their objects. The
	// newest succeeded backup is never deleted. Default 7.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=7
	// +optional
	Keep int32 `json:"keep,omitempty"`
	// Suspend stops new backups without deleting any.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupScheduleStatus is the observed state of a BackupSchedule.
type BackupScheduleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastScheduleTime is the tick of the newest Backup this schedule created.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// NextScheduleTime is the next tick.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// LastSuccessfulBackup names the newest succeeded Backup.
	// +optional
	LastSuccessfulBackup string `json:"lastSuccessfulBackup,omitempty"`
	// LastSuccessTime is when the newest succeeded Backup completed.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// Conditions: Ready (the schedule can run: valid schedule, a mechanism for the datastore kind),
	// LastBackupSucceeded (the newest finished Backup's outcome, in the words of what refused), and
	// OnSchedule (False when a whole tick has passed without a succeeded backup).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bsched
// +kubebuilder:printcolumn:name="Datastore",type=string,JSONPath=`.spec.datastore.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.datastore.kind`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Last success",type=date,JSONPath=`.status.lastSuccessTime`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// BackupSchedule takes a Backup of one datastore on a cron schedule and deletes those past
// retention. Deleting it keeps every Backup it made.
type BackupSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupScheduleSpec   `json:"spec,omitempty"`
	Status BackupScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupScheduleList contains a list of BackupSchedule.
type BackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupSchedule `json:"items"`
}

// BackupSpec is the desired state of a Backup. It is immutable: a Backup is one run.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a Backup's spec is immutable"
type BackupSpec struct {
	// Datastore is what is backed up.
	Datastore BackupDatastoreRef `json:"datastore"`
	// Bucket is where the backup is written.
	Bucket BackupBucketRef `json:"bucket"`
	// Path is the prefix in the bucket; the backup writes under <path>/<backup name>/. Empty means
	// <namespace>/<datastore kind>/<datastore name>, in lower case.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`
	// +optional
	Path string `json:"path,omitempty"`
}

// BackupPhase is where a Backup is in its run.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type BackupPhase string

const (
	BackupPending   BackupPhase = "Pending"
	BackupRunning   BackupPhase = "Running"
	BackupSucceeded BackupPhase = "Succeeded"
	BackupFailed    BackupPhase = "Failed"
)

// BackupStatus is the observed state of a Backup.
type BackupStatus struct {
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`
	// Location is where the backup is written: s3://<bucket>/<path>/<backup name>/.
	// +optional
	Location string `json:"location,omitempty"`
	// JobName is the Job that runs the backup.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// StartTime is when the Job was created.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the backup succeeded or failed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// SizeBytes is the size the mechanism reported for the backup.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Message is the outcome in the words of whatever refused: the object store, the datastore,
	// the pod.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions: Complete (True when the backup succeeded; False with the reason it is waiting or
	// failed).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkp
// +kubebuilder:printcolumn:name="Datastore",type=string,JSONPath=`.spec.datastore.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.datastore.kind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTime`

// Backup is one backup of one datastore. Creating one by hand takes a backup now. Deleting one
// deletes its objects from the bucket.
type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSpec   `json:"spec,omitempty"`
	Status BackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backup.
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Backup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupSchedule{}, &BackupScheduleList{}, &Backup{}, &BackupList{})
}
