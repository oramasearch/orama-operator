/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// pgAdminImage is the official, maintained pgAdmin4 web image (pinned for determinism). No
// custom build needed — it's the canonical upstream image, reused like clickhouse/postgres/redis.
const pgAdminImage = "dpage/pgadmin4:8.14@sha256:8a68677a97b8c8d1427dc915672a26d2c4a04376916a68256f53d669d6171be7"

// reconcilePgAdmin runs pgAdmin4 (web) as a Deployment + ClusterIP Service (NO ingress) so an
// operator can read/write the control-DB via kubectl port-forward. The control-DB server is
// pre-registered via servers.json; the DB password is entered on first connect (it lives in the
// <cr>-controldb-app Secret). Opt-in via spec.components.pgAdmin.enabled.
func (r *OramaCloudV1Reconciler) reconcilePgAdmin(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.PgAdmin.Enabled {
		return r.teardownByComponent(ctx, oc, "pgadmin")
	}

	// Pre-register the control-DB so it's one click after login.
	cmName := oc.Name + "-pgadmin-servers"
	serversJSON := fmt.Sprintf(`{
  "Servers": {
    "1": {
      "Name": "Orama control-DB",
      "Group": "Servers",
      "Host": "%s-controldb-rw",
      "Port": 5432,
      "MaintenanceDB": "%s",
      "Username": "%s",
      "SSLMode": "prefer"
    }
  }
}`, oc.Name, controlDBName, controlDBName)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"servers.json": serversJSON}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}

	return r.reconcileApp(ctx, oc, appDef{
		component: "pgadmin",
		image:     storeImage(oc, "pgadmin", pgAdminImage),
		port:      80,
		env: []corev1.EnvVar{
			{Name: "PGADMIN_DEFAULT_EMAIL", Value: "steve@orama.com"}, // pgAdmin rejects non-TLD domains (.local)
			// PGADMIN_DEFAULT_PASSWORD is injected from the <cr>-pgadmin-secret via envFrom.
			{Name: "PGADMIN_LISTEN_PORT", Value: "80"},
			{Name: "PGADMIN_SERVER_JSON_FILE", Value: "/config/servers.json"},
		},
		// pgAdmin runs as uid 5050 and writes its config DB to /var/lib/pgadmin — fsGroup makes the
		// mounted emptyDir writable by that non-root user. (Pod-level; composes with the per-container
		// hardening baseline reconcileApp applies.)
		securityContext: &corev1.PodSecurityContext{FSGroup: int64Ptr(5050)},
		resources:       resourcesOr(oc.Spec.Components.PgAdmin.Resources, resourceReqs("128Mi", "50m", "512Mi")),
		volumes: []corev1.Volume{
			{Name: "pgadmin-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "servers", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
		},
		volumeMounts: []corev1.VolumeMount{
			{Name: "pgadmin-data", MountPath: "/var/lib/pgadmin"},
			{Name: "servers", MountPath: "/config", ReadOnly: true},
		},
	})
}
