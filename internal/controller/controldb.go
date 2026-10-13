/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// dashboardSchemaSQL is the control-plane Postgres schema — the dashboard's Drizzle migrations
// (0000..0008, journal order) vendored from oramasearch/dashboard .drizzle. The operator applies it
// because the dashboard image ships no .drizzle and nothing else runs the migrations (operator#34).
// Re-sync when the dashboard schema changes; the content hash changes, so the PostgresSchema
// controller replaces the Job, which still skips a database that already has the guard table.
//
//go:embed assets/dashboard-schema.sql
var dashboardSchemaSQL string

// cnpgClusterGVK is the CloudNativePG Cluster CRD. We provision it as an unstructured
// object so this operator stays decoupled from CNPG's Go types / version.
var cnpgClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const controlDBName = "oramacore" // database + owner

// controlDBSchemaGuardTable is created by migration 0000. The migration Job skips the apply when it
// exists, so the schema applies once, to an empty database; a database migrated from an older
// vendored file keeps that schema (the file's 0000..0007 statements are not idempotent).
const controlDBSchemaGuardTable = "crawler_configs"

// controlDBAppSecret is the Secret CloudNativePG generates for the control-DB owner — it holds
// the REAL generated password + a ready-made connection `uri` (keys: password, uri, user, host,
// port, dbname, …). Postgres clients must source creds from HERE, not the operator's dummy
// secret, or their queries fail with the wrong password (#32).
func controlDBAppSecret(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-controldb-app" }

// controlDBExternal reports whether the control DB is an external database (e.g. the shared
// production PlanetScale) that the operator connects to but does not provision or migrate.
func controlDBExternal(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Components.ControlDB.Mode == "external"
}

// controlDBSecret is the Secret every control-DB client reads its connection from: the CNPG
// owner Secret in-cluster, or spec.components.controlDB.credentialsSecretRef when external. Both
// carry the same keys (`uri`, `password`), so consumers don't care which mode is active.
// reconcileControlDB rejects an external DB without a credentialsSecretRef before any consumer runs.
func controlDBSecret(oc *oramav1alpha1.OramaCloudV1) string {
	if ref := oc.Spec.Components.ControlDB.CredentialsSecretRef; controlDBExternal(oc) && ref != nil {
		return ref.Name
	}
	return controlDBAppSecret(oc)
}

// pgPasswordEnv sources the control-DB password into env var `name` (#32).
func pgPasswordEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBSecret(oc)}, Key: "password"}}}
}

// pgURIEnv sources the control-DB connection URI into env var `name` (#32) — avoids hand-building
// a URL around a placeholder password.
func pgURIEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBSecret(oc)}, Key: "uri"}}}
}

// controlDBClusterName is the CNPG Cluster name; CNPG derives `<name>-app` (controlDBAppSecret)
// and `<name>-rw` (the primary Service) from it.
func controlDBClusterName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-controldb" }

func controlDBScheduledBackupName(oc *oramav1alpha1.OramaCloudV1) string {
	return controlDBClusterName(oc) + "-backup"
}

// controlDBInCluster reports whether the operator runs the control DB as a CNPG Cluster here.
func controlDBInCluster(oc *oramav1alpha1.OramaCloudV1) bool {
	db := oc.Spec.Components.ControlDB
	return db.Enabled && db.Mode != "external"
}

// controlDBIsReplica reports whether this cluster's control DB is a CNPG replica of the primary
// region's. A replica neither applies the schema nor takes backups (the primary does both).
func controlDBIsReplica(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Region.Role == "read" && oc.Spec.Components.ControlDB.Replicate
}

// resolveControlDBBackup validates spec.components.controlDB.backup (see resolveControlDBBackupConfig).
// The default bucket prefix is <namespace>/<name>/controldb. Backups are never configured on a
// replica control DB.
func (r *OramaCloudV1Reconciler) resolveControlDBBackup(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (controlDBBackupConfig, error) {
	if !controlDBInCluster(oc) || controlDBIsReplica(oc) {
		return controlDBBackupConfig{}, nil
	}
	return resolveControlDBBackupConfig(ctx, r.Client, oc.Namespace, oc.Spec.Components.ControlDB.Backup,
		oc.Namespace+"/"+oc.Name+"/controldb")
}

// reconcileControlDB provisions the control-plane Postgres (PlanetScale in prod) as a
// CloudNativePG Cluster. On the primary it bootstraps a fresh cluster; on a read tier
// with replicate=true it stands up a replica cluster streaming from the primary peer.
// spec.components.controlDB.backup adds a Barman object store + an owned ScheduledBackup.
// Requires the CloudNativePG operator to be installed (see helmfile).
func (r *OramaCloudV1Reconciler) reconcileControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	db := oc.Spec.Components.ControlDB
	if controlDBExternal(oc) {
		// External DB: the operator provisions nothing and applies no schema; whoever owns the
		// database runs its migrations. Clients need the credentials Secret, so fail loudly here
		// (Degraded) rather than let them start against the in-cluster Secret that doesn't exist.
		if db.CredentialsSecretRef == nil || db.CredentialsSecretRef.Name == "" {
			return fmt.Errorf("controlDB mode=external requires credentialsSecretRef (a Secret with keys uri and password)")
		}
		return nil
	}
	if !db.Enabled {
		return r.teardownControlDB(ctx, oc) // disable ≠ leave running (#23)
	}
	backup, err := r.resolveControlDBBackup(ctx, oc)
	if err != nil {
		return err
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(controlDBClusterName(oc))
	cluster.SetNamespace(oc.Namespace)

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		cluster.SetLabels(labelsFor(oc, "controldb"))
		spec := map[string]interface{}{
			"instances": int64(profileInstances(oc)),
			"storage":   map[string]interface{}{"size": "10Gi"},
		}
		if controlDBIsReplica(oc) {
			// Replica cluster: stream from the primary region's Postgres endpoint.
			// Credentials wiring (the streaming_replica secret) is a follow-up.
			host, port := splitHostPort(primaryPostgresEndpoint(oc), "5432")
			spec["replica"] = map[string]interface{}{"enabled": true, "source": "primary"}
			spec["externalClusters"] = []interface{}{
				map[string]interface{}{
					"name": "primary",
					"connectionParameters": map[string]interface{}{
						"host":   host,
						"port":   port,
						"dbname": controlDBName,
						"user":   "streaming_replica",
					},
				},
			}
		} else {
			spec["bootstrap"] = map[string]interface{}{
				"initdb": map[string]interface{}{"database": controlDBName, "owner": controlDBName},
			}
		}
		if backup.Enabled && backup.Valid {
			spec["backup"] = controlDBBarmanBackup(backup)
		}
		cluster.Object["spec"] = spec
		return controllerutil.SetControllerReference(oc, cluster, r.Scheme)
	})
	if err != nil {
		return err
	}
	if err := r.reconcileControlDBScheduledBackup(ctx, oc, backup); err != nil {
		return err
	}
	// Apply the dashboard schema on the PRIMARY (read-tier replicas stream it) — operator#34.
	if controlDBIsReplica(oc) {
		return nil
	}
	return r.reconcileControlDBMigrations(ctx, oc)
}

// reconcileControlDBScheduledBackup owns the control DB's ScheduledBackup while backups are
// enabled and valid, and removes it otherwise. CNPG creates the individual Backup resources
// (owned by the ScheduledBackup) on its schedule, starting immediately.
func (r *OramaCloudV1Reconciler) reconcileControlDBScheduledBackup(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, backup controlDBBackupConfig) error {
	scheduledBackup := newControlDBScheduledBackup(controlDBScheduledBackupName(oc), oc.Namespace)
	if !backup.Enabled || !backup.Valid {
		return deleteIfExists(ctx, r.Client, scheduledBackup)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, scheduledBackup, func() error {
		scheduledBackup.SetLabels(labelsFor(oc, "controldb"))
		scheduledBackup.Object["spec"] = controlDBScheduledBackupSpec(backup, controlDBClusterName(oc))
		return controllerutil.SetControllerReference(oc, scheduledBackup, r.Scheme)
	})
	return err
}

// reconcileControlDBMigrations applies the vendored dashboard schema (operator#34) as an owned,
// reconciled PostgresSchema CRD (operator#40) — the datastore-state-as-declarative-CRDs pattern.
// The operator renders the schema into a ConfigMap and declares a PostgresSchema referencing it; the
// PostgresSchema controller owns the idempotent migration Job (wait → guard → apply-in-one-txn) and
// records the applied version in its `.status`. The dashboard image ships no .drizzle and nothing
// else runs the migrations, so the operator owns this. To re-apply after a schema re-sync, delete
// the `<cr>-controldb-schema-migrate` Job.
func (r *OramaCloudV1Reconciler) reconcileControlDBMigrations(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cmName := oc.Name + "-controldb-migrations"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"schema.sql": dashboardSchemaSQL}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}

	// Clean up the pre-CRD migration Job (superseded by the PostgresSchema-owned Job under a distinct
	// name) so an upgraded cluster doesn't leave dangling cruft. Safe — the schema is already applied
	// and the new Job's guard is idempotent.
	if err := deleteIfExists(ctx, r.Client, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-migrate", Namespace: oc.Namespace}}); err != nil {
		return err
	}

	// The guard is created by migration 0000; with --single-transaction it exists iff the whole
	// schema applied, so it's a correct "already migrated" guard. The version is a content hash of the
	// schema SQL so it changes exactly when the vendored schema does (auditable in `.status`).
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-schema", Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
		ps.Labels = labelsFor(oc, "controldb")
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: controlDBAppSecret(oc), Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: "schema.sql"},
			Version:             schemaVersion(dashboardSchemaSQL),
			GuardTable:          controlDBSchemaGuardTable,
			Image:               "postgres:16-alpine",
		}
		return controllerutil.SetControllerReference(oc, ps, r.Scheme)
	})
	return err
}

// schemaVersion is a short content hash identifying a schema SQL, recorded in PostgresSchema status
// so the applied version changes exactly when the vendored schema does.
func schemaVersion(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// profileInstances maps the deployment profile to a Postgres instance count.
func profileInstances(oc *oramav1alpha1.OramaCloudV1) int {
	switch oc.Spec.Profile {
	case "heavy":
		return 3
	case "medium":
		return 2
	default: // light
		return 1
	}
}

func primaryPostgresEndpoint(oc *oramav1alpha1.OramaCloudV1) string {
	for _, p := range oc.Spec.Peers {
		if p.PostgresEndpoint != "" {
			return p.PostgresEndpoint
		}
	}
	return ""
}

func splitHostPort(endpoint, defaultPort string) (string, string) {
	if endpoint == "" {
		return "", defaultPort
	}
	if i := strings.LastIndex(endpoint, ":"); i >= 0 {
		return endpoint[:i], endpoint[i+1:]
	}
	return endpoint, defaultPort
}
