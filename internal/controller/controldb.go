/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
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
			"storage":   map[string]interface{}{"size": "10Gi"},
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

// reconcileControlDBMigrations applies the vendored dashboard schema (operator#34) via an idempotent
// psql Job: it waits for the control-DB, skips if already migrated, then applies the whole schema in
// one transaction (all-or-nothing). The dashboard image ships no .drizzle and nothing else runs the
// migrations, so the operator owns this. To re-apply after a schema re-sync, delete the Job.
func (r *OramaCloudV1Reconciler) reconcileControlDBMigrations(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cmName := oc.Name + "-controldb-migrations"
	jobName := oc.Name + "-controldb-migrate"

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"schema.sql": dashboardSchemaSQL}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}

	// crawler_configs is created by migration 0000; with --single-transaction it exists iff the whole
	// schema applied, so it's a correct "already migrated" guard.
	script := `set -eu
echo "control-DB migrate: waiting for the database..."
until psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; do sleep 3; done
if psql "$DATABASE_URL" -tAc "SELECT to_regclass('public.crawler_configs')" | grep -q crawler_configs; then
  echo "control-DB already migrated (crawler_configs present) — nothing to do"; exit 0
fi
echo "applying dashboard schema (vendored Drizzle migrations)..."
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f /migrations/schema.sql
echo "control-DB migrate: done"`

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		if job.CreationTimestamp.IsZero() {
			lbls := labelsFor(oc, "controldb-migrate")
			job.Labels = lbls
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(20), // retry while the control-DB comes up
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: lbls},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:            "migrate",
							Image:           "postgres:16-alpine", // ships psql
							Command:         []string{"sh", "-c", script},
							Env:             []corev1.EnvVar{pgURIEnv(oc, "DATABASE_URL")},
							Resources:       *resourceReqs("64Mi", "25m", "256Mi"), // scheduler requests (#18)
							SecurityContext: hardenedSecurityContext(),             // workload hardening baseline (#18)
							VolumeMounts:    []corev1.VolumeMount{{Name: "migrations", MountPath: "/migrations", ReadOnly: true}},
						}},
						Volumes: []corev1.Volume{{
							Name: "migrations",
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}},
						}},
					},
				},
			}
		}
		return controllerutil.SetControllerReference(oc, job, r.Scheme)
	})
	return err
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
