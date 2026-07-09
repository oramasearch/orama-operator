/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

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
	if !db.Enabled || db.Mode == "external" {
		return nil
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
