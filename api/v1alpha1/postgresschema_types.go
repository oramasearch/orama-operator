/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostgresSchemaSpec is the desired schema state of a Postgres database — a versioned SQL schema
// the operator applies once and tracks. It supersedes the bare idempotent migration Job
// ([#34](https://github.com/oramasearch/orama-operator/issues/34)) with a reconciled,
// auditable CRD ([#40](https://github.com/oramasearch/orama-operator/issues/40)) that
// records the applied version in `.status` and feeds the readiness roll-up (#37). The controller
// owns the migration Job; the applied version is read back from the Job so it stays honest even if
// the desired `version` changes while an obsolete Job is being replaced.
type PostgresSchemaSpec struct {
	// ConnectionSecretRef names the Secret + key holding the Postgres connection URI
	// (postgresql://…). For the control DB this is the CNPG-generated `<cr>-controldb-app` Secret,
	// key `uri`. Nil-safe: until the Secret exists the reconciler waits.
	ConnectionSecretRef PostgresSecretKeyRef `json:"connectionSecretRef"`

	// SchemaConfigMapRef names the ConfigMap + key holding the schema SQL to apply (all-or-nothing,
	// in a single transaction).
	SchemaConfigMapRef PostgresConfigMapKeyRef `json:"schemaConfigMapRef"`

	// Version identifies the schema being applied (e.g. a content hash of the SQL). Recorded in
	// `.status.appliedVersion` once a migration Job applies it. Changing it automatically replaces
	// the prior Job after its pods stop, then re-runs the guarded combined schema.
	Version string `json:"version"`

	// GuardTable is a `public` table that must exist for the schema to count as already applied.
	// When the schema file records migrations (`INSERT INTO schema_migrations (version) VALUES
	// ('NNNN_…')`), every one of them must also be in the database's `schema_migrations`; a guard
	// alone cannot see an earlier-numbered migration that merged later (#271). Without that table,
	// or for a file that records none, the guard alone decides.
	// +kubebuilder:default=crawler_configs
	// +optional
	GuardTable string `json:"guardTable,omitempty"`

	// ApplyEveryVersion applies the file once for every new Version, with no guard check, for a
	// file that is safe to apply again, such as an upsert of reference rows (operator#378).
	// GuardTable is then ignored. Off by default: the guarded behavior above.
	// +optional
	ApplyEveryVersion bool `json:"applyEveryVersion,omitempty"`
	// Image is the psql-carrying image used to run the migration Job.
	// +kubebuilder:default="postgres:16-alpine"
	// +optional
	Image string `json:"image,omitempty"`
}

// PostgresSecretKeyRef references a single key within a Secret in the CR's namespace.
type PostgresSecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key within the Secret holding the value.
	Key string `json:"key"`
}

// PostgresConfigMapKeyRef references a single key within a ConfigMap in the CR's namespace.
type PostgresConfigMapKeyRef struct {
	// Name of the ConfigMap.
	Name string `json:"name"`
	// Key within the ConfigMap holding the value.
	Key string `json:"key"`
}

// PostgresSchemaStatus is the observed schema state. Written to the status subresource ONLY on a
// real state transition (deep-compare first) — anti-flap, principle #7.
type PostgresSchemaStatus struct {
	// Phase is the high-level schema state: Pending | Applying | Applied | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// AppliedVersion is the schema version the migration Job actually applied (read back from the
	// Job, so a bumped-but-not-re-run desired version does not misreport). Empty until first applied.
	// +optional
	AppliedVersion string `json:"appliedVersion,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message is a human-readable summary of the current state (or the last error).
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions carry the standard `Applied` condition. Reason distinguishes
	// WaitingForDatabase, ReplacingMigrationJob, Applying, Applied, and MigrationFailed.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=pgschema
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
//+kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.appliedVersion`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// PostgresSchema is a versioned Postgres schema the operator applies + tracks, owned by an
// OramaCloudV1 CR. The controller owns an idempotent migration Job (wait-for-DB → guard →
// apply-in-one-transaction) and records the applied version in `.status`. It carries **no
// finalizer** — deleting the CR must never drop the schema/data; it only stops managing the
// migration (the Job is garbage-collected). Until the connection Secret exists the reconciler is a
// safe no-op reporting `Applied=False (WaitingForDatabase)`.
type PostgresSchema struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresSchemaSpec   `json:"spec,omitempty"`
	Status PostgresSchemaStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PostgresSchemaList contains a list of PostgresSchema.
type PostgresSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresSchema `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresSchema{}, &PostgresSchemaList{})
}
