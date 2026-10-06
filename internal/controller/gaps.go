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

// Gap-fill components surfaced by the techops-docs gap analysis (operator issues #5–#7).
// Airbyte (#4) is a platform dependency installed via its Helm chart (helmfile), and the
// QStash-equivalent queue (#8) is config wiring (workers → the in-cluster RabbitMQ) — neither
// is a reconciled component here.

// analyticsProcessorConfigYAML renders the config the `server` binary reads (config.rs). It
// connects to ClickHouse + Postgres at startup, so these are the real in-cluster endpoints.
// Secret values are __PLACEHOLDER__s the render init container substitutes — never in the CM.
//
// oramacores lists every OramaCore whose reader side logs search events (#55). Searches land on
// the reader Service, so it comes first; the writer pod runs a reader side too and is drained so
// nothing it logged is stranded. Both authenticate with the master key, which the engine config
// sets as reader_side.analytics.api_key (oramacoreConfigYAML).
func analyticsProcessorConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	n := oc.Name
	return fmt.Sprintf(`clickhouse:
  url: http://%s-analytics:8123
  user: default
  password: __CLICKHOUSE_PASSWORD__
  database: default
  search_table: oramacore_analytics_search_raw
  interaction_table: oramacore_analytics_interaction_raw
  aggregated_search_table: oramacore_analytics_search_aggregated
create_tables: true
oramacores:
  - name: oramacore-reader
    api_url: http://%s-reader/v1/analytics
    api_key: __MASTER_API_KEY__
    enabled: true
  - name: oramacore-writer
    api_url: http://%s-writer:8080/v1/analytics
    api_key: __MASTER_API_KEY__
    enabled: true
postgres:
  url: postgres://%s:__PG_PASSWORD__@%s-controldb-rw:5432/%s
  schema: analytics
  runs_table: runs
  delta: 60m
`, n, n, n, controlDBName, n, controlDBName)
}

// reconcileAnalyticsProcessor deploys analytics-processor (#5): the worker that pulls
// /v1/analytics into ClickHouse. Its `server` binary reads a YAML config FILE (CONFIG_PATH) and
// connects to ClickHouse + Postgres at startup — so the operator renders that file via an init
// container (ClickHouse password + master key from the component Secret; the CNPG-generated
// Postgres password from the <cr>-controldb-app Secret) and mounts it. (The image is also
// patched at build to bind 0.0.0.0:8080 instead of its hardcoded 127.0.0.1:8888.)
func (r *OramaCloudV1Reconciler) reconcileAnalyticsProcessor(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.AnalyticsProcessor.Enabled {
		return r.teardownByComponent(ctx, oc, "analytics-processor")
	}
	img := componentImage(oc, "analytics-processor", "analytics-processor")
	cmName := oc.Name + "-analytics-processor-config"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"config.yaml": analyticsProcessorConfigYAML(oc)}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}
	render := corev1.Container{
		Name:  "render-config",
		Image: img,
		Command: []string{"sh", "-c",
			`sed -e "s#__CLICKHOUSE_PASSWORD__#${CLICKHOUSE_PASSWORD}#g" -e "s#__MASTER_API_KEY__#${MASTER_API_KEY}#g" -e "s#__PG_PASSWORD__#${PG_PASSWORD}#g" /tmpl/config.yaml > /etc/analytics-processor/config.yaml`},
		EnvFrom: envFromFor(oc, "analytics-processor"),
		Env: []corev1.EnvVar{{Name: "PG_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: oc.Name + "-controldb-app"}, Key: "password"}}}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config-tmpl", MountPath: "/tmpl"},
			{Name: "rendered-config", MountPath: "/etc/analytics-processor"},
		},
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:      "analytics-processor",
		image:          img,
		port:           8080,
		env:            []corev1.EnvVar{{Name: "CONFIG_PATH", Value: "/etc/analytics-processor/config.yaml"}},
		resources:      resourcesOr(oc.Spec.Components.AnalyticsProcessor.Resources, resourceReqs("128Mi", "50m", "512Mi")),
		initContainers: []corev1.Container{render},
		volumes: []corev1.Volume{
			{Name: "config-tmpl", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
			{Name: "rendered-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		volumeMounts: []corev1.VolumeMount{{Name: "rendered-config", MountPath: "/etc/analytics-processor"}},
	})
}

// reconcileOramasearchApp deploys the legacy api.askorama.ai app (#7).
// Deprecated: UNWIRED from the reconcile step-list (operator#35) — the legacy app just redirects
// to the new app, so we don't run it in-cluster (this also moots the MySQL-vs-CNPG mismatch). Kept
// for reference; not called.
func (r *OramaCloudV1Reconciler) reconcileOramasearchApp(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	a := oc.Spec.Components.OramasearchApp
	if !a.Enabled {
		return r.teardownByComponent(ctx, oc, "oramasearch-app")
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "oramasearch-app",
		image:       componentImage(oc, "oramasearch-app", "oramasearch-app"),
		port:        3000,
		ingressHost: a.Host,
		resources:   resourcesOr(a.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}

// searchWorkerNames are index-manager's hosted-search SERVING workers (#6) — distinct from the
// index-BUILDER the ingestion component already deploys. They're Cloudflare Workers, so (like the
// orama-workers bundle) each still needs its CF bindings mapped to in-cluster equivalents; this
// reconciles a Deployment + Service per worker as the scaffold.
var searchWorkerNames = []string{
	"index-shared-worker",
	"large-index-worker",
	"openai-decrypter",
	"telemetry-worker",
}

func (r *OramaCloudV1Reconciler) reconcileSearchWorkers(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.SearchWorkers.Enabled {
		return r.teardownByComponent(ctx, oc, searchWorkerNames...)
	}
	for _, w := range searchWorkerNames {
		if err := r.reconcileApp(ctx, oc, appDef{
			component: w,
			image:     componentImage(oc, w, w),
			port:      8080,
			resources: resourcesOr(oc.Spec.Components.SearchWorkers.Resources, resourceReqs("64Mi", "25m", "256Mi")),
		}); err != nil {
			return err
		}
	}
	return nil
}
