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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// dashboardSchemaSQL is the control-plane Postgres schema — the dashboard's Drizzle migrations
// (0000..0007, journal order) vendored from oramasearch/dashboard .drizzle. The operator applies it
// because the dashboard image ships no .drizzle and nothing else runs the migrations (operator#34).
// Re-sync when the dashboard schema changes; then delete the migrate Job to re-apply.
//
//go:embed assets/dashboard-schema.sql
var dashboardSchemaSQL string

// cnpgClusterGVK is the CloudNativePG Cluster CRD. We provision it as an unstructured
// object so this operator stays decoupled from CNPG's Go types / version.
var cnpgClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const controlDBName = "oramacore" // database + owner

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

// reconcileControlDB provisions the control-plane Postgres (PlanetScale in prod) as a
// CloudNativePG Cluster. On the primary it bootstraps a fresh cluster; on a read tier
// with replicate=true it stands up a replica cluster streaming from the primary peer.
// Requires the CloudNativePG operator to be installed (see helmfile).
func (r *OramaCloudV1Reconciler) reconcileControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	db := oc.Spec.Components.ControlDB
	if db.Mode == "external" {
		return nil // external DB — the operator manages nothing to tear down
	}
	if !db.Enabled {
		return r.teardownControlDB(ctx, oc) // disable ≠ leave running (#23)
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(oc.Name + "-controldb")
	cluster.SetNamespace(oc.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		cluster.SetLabels(labelsFor(oc, "controldb"))
		spec := map[string]interface{}{
			"instances": int64(profileInstances(oc)),
			"storage":   controlDBStorage(db.Storage),
		}
		// The storage pass owns changes to existing storage; ordinary spec rendering
		// must retain the class, grown size, and any other CNPG storage keys.
		if cluster.GetResourceVersion() != "" {
			storage, found, err := unstructured.NestedMap(cluster.Object, "spec", "storage")
			if err != nil {
				return err
			}
			delete(spec, "storage")
			if found {
				spec["storage"] = storage
			}
		}
		if oc.Spec.Region.Role == "read" && db.Replicate {
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
		cluster.Object["spec"] = spec
		// Cluster metadata alone is not inherited by CNPG instance and bootstrap Job pods.
		if err := unstructured.SetNestedStringMap(cluster.Object, labelsFor(oc, "controldb"), "spec", "inheritedMetadata", "labels"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(oc, cluster, r.Scheme)
	})
	if err != nil {
		return err
	}
	// Apply the dashboard schema on the PRIMARY (read-tier replicas stream it) — operator#34.
	if oc.Spec.Region.Role == "read" && db.Replicate {
		return nil
	}
	return r.reconcileControlDBMigrations(ctx, oc)
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

	// crawler_configs is created by migration 0000; with --single-transaction it exists iff the whole
	// schema applied, so it's a correct "already migrated" guard. The version is a content hash of the
	// schema SQL so it changes exactly when the vendored schema does (auditable in `.status`).
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-schema", Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
		ps.Labels = labelsFor(oc, "controldb")
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: controlDBAppSecret(oc), Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: "schema.sql"},
			Version:             schemaVersion(dashboardSchemaSQL),
			GuardTable:          "crawler_configs",
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
