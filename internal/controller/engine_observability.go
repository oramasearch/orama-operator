/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Engine error reporting and metrics (#314): OramaCore's Sentry client, its log levels, and its
// Prometheus endpoint, which is scraped on the pod and never served by the public Ingresses.
const (
	// sentryDSNEnv carries the DSN into the render-config init container; keyPlaceholder(sentryDSNEnv)
	// stands in for it in the ConfigMap.
	sentryDSNEnv = "SENTRY_DSN"

	// sidePlaceholder in the combined config (one template for the writer and the readers) becomes
	// the pod's side, "writer" or "reader", so each side reports to its own Sentry environment.
	sidePlaceholder = "__ENGINE_SIDE__"

	// oramacoreMetricsPath is OramaCore's Prometheus endpoint (http.with_prometheus).
	oramacoreMetricsPath = "/metrics"

	// metricsDenyAnnotation makes ingress-nginx answer 403 to every client on the locations of the
	// Ingress that carries it.
	metricsDenyAnnotation = "nginx.ingress.kubernetes.io/denylist-source-range"
)

// engineLogYAML is the log section. levels match Prod 1's engine configs. With
// spec.components.engine.sentry set it also carries sentry_dns, a placeholder that renderConfigInit
// fills from the DSN Secret, and sentry_environment (sentryEnvironment). OramaCore starts Sentry only
// when sentry_dns is present, so without the field the config has neither key.
func engineLogYAML(oc *oramav1alpha1.OramaCloudV1, side string) string {
	log := "log:\n  levels:\n    oramacore: info\n    orama_js_pool: info\n"
	if oc.Spec.Components.Engine.Sentry == nil {
		return log
	}
	return log + fmt.Sprintf("  sentry_dns: '%s'\n  sentry_environment: %q\n",
		keyPlaceholder(sentryDSNEnv), sentryEnvironment(oc, side))
}

// sentryEnvironment is the Sentry environment the side reports as: <environment>-<side>, where
// environment defaults to spec.region.name (the CR name when that is empty).
func sentryEnvironment(oc *oramav1alpha1.OramaCloudV1, side string) string {
	base := oc.Spec.Components.Engine.Sentry.Environment
	if base == "" {
		base = oc.Spec.Region.Name
	}
	if base == "" {
		base = oc.Name
	}
	return base + "-" + side
}

// sentryDSNSecretEnv is the render-config env that carries the DSN from dsnSecretRef, or nothing
// when Sentry is off. As an explicit env entry it wins over a SENTRY_DSN in the engine Secret.
func sentryDSNSecretEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	s := oc.Spec.Components.Engine.Sentry
	if s == nil {
		return nil
	}
	return []corev1.EnvVar{secretKeyEnv(sentryDSNEnv, s.DSNSecretRef.Name, s.DSNSecretRef.Key)}
}

// sentryRenderExprs are the render-config sed expressions Sentry adds after the placeholder
// substitutions: the side for the combined config's environment, then removing a sentry_dns left
// empty by an empty Secret value (OramaCore panics on a DSN it can't parse, so an empty one means
// Sentry off). sed applies the expressions to each line in order, so the deletion sees the
// substituted line.
func sentryRenderExprs(oc *oramav1alpha1.OramaCloudV1, s engineSide) string {
	if oc.Spec.Components.Engine.Sentry == nil {
		return ""
	}
	var exprs string
	if s.configKey == combinedConfigKey {
		exprs = ` -e "s#` + sidePlaceholder + `#` + s.name + `#g"`
	}
	return exprs + ` -e "/^  sentry_dns: ''$/d"`
}

// upsertMetricsBlockIngress owns the Ingress <name>-metrics, which keeps OramaCore's /metrics off the
// public writer and reader hosts: an Exact /metrics rule on every host of <name>, denied to every
// source address by ingress-nginx, which prefers it to <name>'s Prefix "/" rule. This covers the
// writer, whose Ingress reaches OramaCore directly, and readers without Envoy; with Envoy the reader
// also answers 404 itself (envoyConfig). It uses an annotation rather than a server snippet because
// ingress-nginx refuses snippet annotations by default.
func (r *OramaCloudV1Reconciler) upsertMetricsBlockIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component, host string, ingress oramav1alpha1.IngressHostsSpec, port int32) error {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name + "-metrics", Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = labelsFor(oc, component)
		ing.Annotations = map[string]string{metricsDenyAnnotation: "0.0.0.0/0,::/0"}
		ing.Spec = metricsBlockIngressSpec(oc, name, host, ingress, port)
		return controllerutil.SetControllerReference(oc, ing, r.Scheme)
	})
	return err
}

// metricsBlockIngressSpec is ingressSpec with every rule narrowed to the Exact path /metrics.
func metricsBlockIngressSpec(oc *oramav1alpha1.OramaCloudV1, name, host string, ingress oramav1alpha1.IngressHostsSpec, port int32) networkingv1.IngressSpec {
	spec := ingressSpec(oc, name, host, ingress, port)
	exact := networkingv1.PathTypeExact
	for _, rule := range spec.Rules {
		for i := range rule.HTTP.Paths {
			rule.HTTP.Paths[i].Path, rule.HTTP.Paths[i].PathType = oramacoreMetricsPath, &exact
		}
	}
	return spec
}
