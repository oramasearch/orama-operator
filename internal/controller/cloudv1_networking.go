/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Public engine endpoints (operator#385). engine.writer.host and engine.reader.host publish the
// writer and the readers, each through its own Ingress (<cr>-writer, <cr>-reader) with origin TLS
// from the stack's Issuer, a sandbox rule admitting the ingress gateway to the pods behind it, and,
// with Cloudflare DNS managed, its own CloudflareDNSRecord (cloudflare.go). Every function that
// decides whether an endpoint is published reads writerPublicHost or readerPublicHost, so the
// Ingress, the sandbox rule and the record appear and go away together.

// writerPublicHost is the host the writer is published on, or "" while it is not: the engine is
// disabled, the region is not primary (a read region runs no writer), or no host is set.
func writerPublicHost(oc *oramav1alpha1.OramaCloudV1) string {
	engine := oc.Spec.Components.Engine
	if !engine.Enabled || oc.Spec.Region.Role != "primary" {
		return ""
	}
	return engine.Writer.Host
}

// readerPublicHost is the host the readers are published on, or "" while the engine is disabled or
// no host is set.
func readerPublicHost(oc *oramav1alpha1.OramaCloudV1) string {
	engine := oc.Spec.Components.Engine
	if !engine.Enabled {
		return ""
	}
	return engine.Reader.Host
}

// annotationNginxProxyBodySize is ingress-nginx's request body limit for an Ingress. Without it nginx
// answers 413 above 1m, stricter than the engine behind it (operator#1056).
const annotationNginxProxyBodySize = "nginx.ingress.kubernetes.io/proxy-body-size"

// The engine Ingresses admit exactly the largest body OramaCore accepts on that side, so the engine,
// not nginx, decides what is too large. OramaCore's DefaultBodyLimit (oramacore-private
// 1e0dc346, src/web_server/api/collection/mod.rs; in builds b6ee20fb and 86bc17ea): write routes
// WRITE_MAX_BODY_SIZE 400 MiB, read routes and the server default READ_MAX_BODY_SIZE 16 MiB (MCP
// lower, 1 MiB). nginx's "m" is MiB. Change these with the engine's limits.
const (
	writerProxyBodySize = "400m"
	readerProxyBodySize = "16m"
)

// readerPodPort is the reader pod port behind the reader Service's port 80: Envoy's when it is
// enabled, OramaCore's otherwise. A NetworkPolicy matches the destination pod port, not the
// Service port.
func readerPodPort(oc *oramav1alpha1.OramaCloudV1) int32 {
	if oc.Spec.Components.Engine.Envoy {
		return envoyPort
	}
	return oramacorePort
}

// reconcileEngineIngresses keeps the writer and reader Ingresses for their public hosts (plus each
// endpoint's extraHosts, which get no DNS record), each with its deny-all /metrics companion
// (<name>-metrics, engine_metrics.go) built from the same spec, and deletes both once the endpoint
// is no longer published. reconcileEngine calls it on both its enabled and disabled paths. An
// object at any of these names that this CR does not control is never adopted, changed or deleted;
// publishing over it fails with a conflict, before either Ingress is written.
func (r *OramaCloudV1Reconciler) reconcileEngineIngresses(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	engine := oc.Spec.Components.Engine
	for _, endpoint := range []struct {
		component, host string
		extraHosts      []string
		port            int32
		bodySize        string
	}{
		{"writer", writerPublicHost(oc), engine.Writer.ExtraHosts, oramacorePort, writerProxyBodySize},
		{"reader", readerPublicHost(oc), engine.Reader.ExtraHosts, envoyPort, readerProxyBodySize},
	} {
		name := oc.Name + "-" + endpoint.component
		// The main Ingress comes first: it is deleted before its companion, so a host never serves
		// / without its /metrics refused.
		ingresses := []*networkingv1.Ingress{
			{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: name + "-metrics", Namespace: oc.Namespace}},
		}
		if endpoint.host == "" {
			for _, ing := range ingresses {
				if err := r.deletePublicEndpointOwned(ctx, oc, ing); err != nil {
					return fmt.Errorf("%s ingress: %w", endpoint.component, err)
				}
			}
			continue
		}
		for _, ing := range ingresses {
			if _, err := r.refuseForeignPublicEndpoint(ctx, oc, ing); err != nil {
				return err
			}
		}
		// Checked before the companion is written, so a stack without an Issuer gets neither Ingress.
		if _, err := stackIngressIssuer(oc); err != nil {
			return fmt.Errorf("%s ingress: %w", endpoint.component, err)
		}
		main := ingressSpec(oc, name, endpoint.host, endpoint.extraHosts, endpoint.port)
		// The companion is written first, so a host the main Ingress newly serves already refuses /metrics.
		if err := r.upsertMetricsBlockIngress(ctx, oc, name, endpoint.component, main); err != nil {
			return fmt.Errorf("%s metrics ingress: %w", endpoint.component, err)
		}
		if err := r.upsertIngress(ctx, oc, name, endpoint.component, main, endpoint.bodySize); err != nil {
			return fmt.Errorf("%s ingress: %w", endpoint.component, err)
		}
	}
	return nil
}

// refuseForeignPublicEndpoint reads obj (named, in oc's namespace) and reports whether it exists. It
// returns a conflict error when obj exists but this CR does not control it, and a read error other
// than NotFound as is.
func (r *OramaCloudV1Reconciler) refuseForeignPublicEndpoint(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) (bool, error) {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if metav1.IsControlledBy(obj, oc) {
		return true, nil
	}
	kind := "object"
	if gvk, err := apiutil.GVKForObject(obj, r.Scheme); err == nil {
		kind = gvk.Kind
	}
	return true, fmt.Errorf("%s %s/%s exists and is not controlled by OramaCloudV1 %s; it is left untouched, so this endpoint cannot be published until it is removed or renamed",
		kind, obj.GetNamespace(), obj.GetName(), oc.Name)
}

// deletePublicEndpointOwned deletes obj (named, in oc's namespace) when this CR controls it, for an
// endpoint whose host was cleared or whose component was disabled. An absent object, one this CR
// does not control and one already being deleted are left alone. The fetched UID and
// resourceVersion are delete preconditions, so a replaced or changed object is never deleted by
// mistake. Deleting the CR itself needs none of this: owned objects go with their owner.
func (r *OramaCloudV1Reconciler) deletePublicEndpointOwned(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, oc) || !obj.GetDeletionTimestamp().IsZero() {
		return nil
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}
