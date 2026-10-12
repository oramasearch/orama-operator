/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const discoPort = 8080

// reconcileDisco provisions the DISCO semantic cache as a Deployment: the Go DISCO service, with no
// embeddings sidecar. DISCO reaches its embeddings over its own EmbeddingService gRPC contract at
// EMBEDDINGS_GRPC_HOST/EMBEDDINGS_GRPC_PORT; the operator runs no service that implements it, so
// those stay at DISCO's defaults (#305). In-memory (no PVC — a restart cold-starts the cache),
// single-node, exposed on discoPort. Routed through the shared reconcileApp builder so it inherits
// the workload-hardening baseline (probes, securityContext, resources, PDB, grace) like every
// other app (#18).
func (r *OramaCloudV1Reconciler) reconcileDisco(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.Disco.Enabled {
		return r.teardownByComponent(ctx, oc, "disco")
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: "disco",
		image:     componentImage(oc, "disco", "disco"),
		port:      discoPort,
		env: []corev1.EnvVar{
			{Name: "DISCO_PORT", Value: "8080"},
		},
		// The Go semantic cache holds entries in memory; give it a roomier limit than the light apps.
		// Override via spec.components.disco.resources.
		resources: resourcesOr(oc.Spec.Components.Disco.Resources, resourceReqs("128Mi", "100m", "1Gi")),
	})
}
