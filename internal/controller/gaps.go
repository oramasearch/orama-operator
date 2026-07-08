/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// Gap-fill components surfaced by the techops-docs gap analysis (operator issues #5–#7).
// Airbyte (#4) is a platform dependency installed via its Helm chart (helmfile), and the
// QStash-equivalent queue (#8) is config wiring (workers → the in-cluster RabbitMQ) — neither
// is a reconciled component here.

// reconcileAnalyticsProcessor deploys analytics-processor (#5): the worker that pulls
// /v1/analytics into ClickHouse (deployed US/EU/Asia in prod). Plain Deployment + Service; it
// reads the in-cluster ClickHouse + engine endpoints from the shared config + its scoped secret.
func (r *OramaCloudV1Reconciler) reconcileAnalyticsProcessor(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.AnalyticsProcessor.Enabled {
		return nil
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: "analytics-processor",
		image:     componentImage(oc, "analytics-processor", "analytics-processor"),
		port:      8080,
	})
}

// reconcileOramasearchApp deploys the legacy api.askorama.ai app (#7): Deployment + Service +
// Ingress. Still-serving legacy frontend (the "old dashboard", predecessor to app.orama.com).
func (r *OramaCloudV1Reconciler) reconcileOramasearchApp(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	a := oc.Spec.Components.OramasearchApp
	if !a.Enabled {
		return nil
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "oramasearch-app",
		image:       componentImage(oc, "oramasearch-app", "oramasearch-app"),
		port:        3000,
		ingressHost: a.Host,
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
		return nil
	}
	for _, w := range searchWorkerNames {
		if err := r.reconcileApp(ctx, oc, appDef{
			component: w,
			image:     componentImage(oc, w, w),
			port:      8080,
		}); err != nil {
			return err
		}
	}
	return nil
}
