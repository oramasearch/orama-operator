/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// reconcileIngestion provisions the ingest path: index-builder (Cloud Run -> a plain
// Deployment) and, when enabled, HARVEST (master + Mongo record store + Selenium grid).
func (r *OramaCloudV1Reconciler) reconcileIngestion(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	ing := oc.Spec.Components.Ingestion
	if !ing.Enabled {
		return nil
	}

	// index-builder orchestrator (was GCP Cloud Run).
	if err := r.reconcileApp(ctx, oc, appDef{
		component: "index-builder",
		image:     componentImage(oc, "index-builder", "index-builder"),
		port:      8080,
	}); err != nil {
		return err
	}

	if !ing.Harvest {
		return nil
	}

	// HARVEST crawler master (Node/Express job API).
	if err := r.reconcileApp(ctx, oc, appDef{
		component: "harvest",
		image:     componentImage(oc, "harvest", "harvest"),
		port:      8080,
	}); err != nil {
		return err
	}

	// HARVEST's record store (Mongo). Single-node StatefulSet + PVC + Service.
	if err := r.provisionStatefulSet(ctx, oc, dataStoreDef{
		component: "harvest-mongo",
		image:     "mongo:7",
		ports:     []corev1.ContainerPort{{Name: "mongo", ContainerPort: 27017}},
		dataPath:  "/data/db",
	}); err != nil {
		return err
	}

	// Selenium grid for JS-rendered crawls.
	return r.reconcileApp(ctx, oc, appDef{
		component: "selenium",
		image:     "selenium/standalone-chromium:latest",
		port:      4444,
	})
}
