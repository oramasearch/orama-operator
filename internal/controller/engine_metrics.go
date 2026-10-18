/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// OramaCore serves Prometheus metrics on :8080/metrics (http.with_prometheus). The public engine
// hosts never serve it (#314): Envoy answers 404 on the readers (envoyConfig), and every public
// engine Ingress (<cr>-writer, <cr>-reader) has a companion <cr>-writer-metrics or
// <cr>-reader-metrics that ingress-nginx answers with 403 for every client. ingress-nginx prefers
// the companion's Exact /metrics location to the main Ingress's Prefix /. zetta-tunnel's gateway
// cannot refuse a source, so it serves no host that a companion covers (sourceDeniedHosts), an
// interim guard until #575. Inside the cluster the endpoint stays on the pod's :8080.

const (
	annotationNginxDenylistSourceRange = "nginx.ingress.kubernetes.io/denylist-source-range"
	// denyAllSources is every IPv4 and IPv6 client. A source range, not a server or configuration
	// snippet: ingress-nginx refuses snippet annotations by default.
	denyAllSources = "0.0.0.0/0,::/0"
)

// metricsBlockIngressSpec is the companion of the main Ingress spec main (ingressSpec): the same
// class, TLS and hosts, each rule's paths replaced by one Exact /metrics path to that rule's main
// backend. main is deep-copied and left as it was. The TLS entries name the main Ingress's Secrets;
// the companion carries no cert-manager annotation, so it requests no certificate of its own.
func metricsBlockIngressSpec(main networkingv1.IngressSpec) networkingv1.IngressSpec {
	spec := main.DeepCopy()
	exact := networkingv1.PathTypeExact
	for i := range spec.Rules {
		http := spec.Rules[i].HTTP
		if http == nil || len(http.Paths) == 0 {
			continue
		}
		http.Paths = []networkingv1.HTTPIngressPath{{Path: "/metrics", PathType: &exact, Backend: http.Paths[0].Backend}}
	}
	return networkingv1.IngressSpec{IngressClassName: spec.IngressClassName, TLS: spec.TLS, Rules: spec.Rules}
}

// upsertMetricsBlockIngress writes <name>-metrics, the deny-all companion of the main Ingress name
// whose spec is main, with component's labels and this CR as its controller. Its annotations are
// replaced whole, so it only ever carries the deny-all source range.
func (r *OramaCloudV1Reconciler) upsertMetricsBlockIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component string, main networkingv1.IngressSpec) error {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name + "-metrics", Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = labelsFor(oc, component)
		ing.Annotations = map[string]string{annotationNginxDenylistSourceRange: denyAllSources}
		ing.Spec = metricsBlockIngressSpec(main)
		return controllerutil.SetControllerReference(oc, ing, r.Scheme)
	})
	return err
}
