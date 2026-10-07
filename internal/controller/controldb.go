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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// pgPasswordEnv sources the CNPG-generated owner password into env var `name` (#32).
func pgPasswordEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBAppSecret(oc)}, Key: "password"}}}
}

// pgURIEnv sources the CNPG-generated connection URI (postgresql://owner:pw@<cr>-controldb-rw:5432/db)
// into env var `name` (#32) — avoids hand-building a URL around a placeholder password.
func pgURIEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBAppSecret(oc)}, Key: "uri"}}}
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

// resolveControlDBBackup validates spec.components.controlDB.backup (see resolveCNPGBackupConfig).
// The default bucket prefix is <namespace>/<name>/controldb. Backups are never configured on a
// replica control DB.
func (r *OramaCloudV1Reconciler) resolveControlDBBackup(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (cnpgBackupConfig, error) {
	if !controlDBInCluster(oc) || controlDBIsReplica(oc) {
		return cnpgBackupConfig{}, nil
	}
	return resolveCNPGBackupConfig(ctx, r.Client, oc.Namespace, oc.Spec.Components.ControlDB.Backup,
		"spec.components.controlDB.backup", oc.Namespace+"/"+oc.Name+"/controldb")
}

// reconcileControlDB provisions the control-plane Postgres (PlanetScale in prod) as a
// CloudNativePG Cluster. On the primary it bootstraps a fresh cluster; on a read tier
// with replicate=true it stands up a replica cluster streaming from the primary peer.
// spec.components.controlDB.storage sizes each instance's PVC, spec.placement pins the instances
// through the Cluster's affinity, and controlDB.backup adds a Barman object store + an owned
// ScheduledBackup. Under spec.deletionProtection the Cluster and ScheduledBackup carry no owner
// reference, so deleting the CR leaves them (and the data) in place.
// Requires the CloudNativePG operator to be installed (see helmfile).
func (r *OramaCloudV1Reconciler) reconcileControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	db := oc.Spec.Components.ControlDB
	if db.Mode == "external" {
		return nil // external DB — the operator manages nothing to tear down
	}
	if !db.Enabled {
		return r.teardownControlDB(ctx, oc) // disable ≠ leave running (#23)
	}
	backup, err := r.resolveControlDBBackup(ctx, oc)
	if err != nil {
		return err
	}
	affinity, err := cnpgAffinity(oc)
	if err != nil {
		return err
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(controlDBClusterName(oc))
	cluster.SetNamespace(oc.Namespace)

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		cluster.SetLabels(labelsFor(oc, "controldb"))
		storage := map[string]interface{}{"size": defaultDataVolumeSize}
		if db.Storage.Size != nil {
			storage["size"] = db.Storage.Size.String()
		}
		if db.Storage.StorageClassName != "" {
			storage["storageClass"] = db.Storage.StorageClassName
		}
		spec := map[string]interface{}{
			"instances": int64(profileInstances(oc)),
			"storage":   storage,
		}
		if affinity != nil {
			spec["affinity"] = affinity
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
			spec["backup"] = cnpgBarmanBackup(backup)
		}
		cluster.Object["spec"] = spec
		return r.ownUnlessProtected(oc, cluster)
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
func (r *OramaCloudV1Reconciler) reconcileControlDBScheduledBackup(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, backup cnpgBackupConfig) error {
	scheduledBackup := newCNPGScheduledBackupFor(controlDBScheduledBackupName(oc), oc.Namespace)
	if !backup.Enabled || !backup.Valid {
		return deleteIfExists(ctx, r.Client, scheduledBackup)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, scheduledBackup, func() error {
		scheduledBackup.SetLabels(labelsFor(oc, "controldb"))
		scheduledBackup.Object["spec"] = cnpgScheduledBackupSpec(backup, controlDBClusterName(oc))
		return r.ownUnlessProtected(oc, scheduledBackup)
	})
	return err
}

// cnpgAffinity renders spec.placement as the CNPG Cluster's spec.affinity (nodeSelector +
// tolerations, which CNPG copies onto every instance pod). nil when no placement is set, so a CR
// without placement renders the Cluster exactly as before.
func cnpgAffinity(oc *oramav1alpha1.OramaCloudV1) (map[string]interface{}, error) {
	nodeSelector, tolerations := podPlacement(oc)
	if len(nodeSelector) == 0 && len(tolerations) == 0 {
		return nil, nil
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(&struct {
		NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
		Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	}{nodeSelector, tolerations})
}

// ownUnlessProtected controller-references a control-DB object (Cluster, ScheduledBackup) to the
// CR. Under spec.deletionProtection it instead drops any reference to the CR, so garbage
// collection never deletes the database when the CR is deleted.
func (r *OramaCloudV1Reconciler) ownUnlessProtected(oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if !oc.Spec.DeletionProtection {
		return controllerutil.SetControllerReference(oc, obj, r.Scheme)
	}
	refs := obj.GetOwnerReferences()
	kept := refs[:0]
	for _, ref := range refs {
		if ref.UID != oc.UID {
			kept = append(kept, ref)
		}
	}
	obj.SetOwnerReferences(kept)
	return nil
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
		nodeSelector, tolerations := podPlacement(oc)
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: controlDBAppSecret(oc), Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: "schema.sql"},
			Version:             schemaVersion(dashboardSchemaSQL),
			GuardTable:          controlDBSchemaGuardTable,
			Image:               "postgres:16-alpine",
			NodeSelector:        nodeSelector,
			Tolerations:         tolerations,
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
