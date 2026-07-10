/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const appName = "orama-cloud-v1"

// labelsFor returns the standard label set for a child object of a given component.
func labelsFor(oc *oramav1alpha1.OramaCloudV1, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   oc.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"orama.cloud/region":           oc.Spec.Region.Name,
	}
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

// imagePullSecrets returns the pull secret (if any) for pods, so private-registry images
// (e.g. the private Docker Hub repos built by the bridge's build/images tooling) can be
// pulled. nil when spec.images.pullSecretName is unset (public images need nothing).
func imagePullSecrets(oc *oramav1alpha1.OramaCloudV1) []corev1.LocalObjectReference {
	if oc.Spec.Images.PullSecretName == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: oc.Spec.Images.PullSecretName}}
}

// componentImage resolves the image for a component: an explicit override wins,
// otherwise registry/tag defaults (published oramasearch/* images).
func componentImage(oc *oramav1alpha1.OramaCloudV1, key, defaultRepo string) string {
	if o := oc.Spec.Images.Overrides[key]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	tag := oc.Spec.Images.Tag
	if tag == "" {
		tag = "latest"
	}
	return reg + "/" + defaultRepo + ":" + tag
}

// embeddingsImage resolves the OramaCore engine ai-server (embeddings, gRPC :50051) sidecar. This
// is the PUBLISHED oramacore-ai-server image (the engine's own ai-server) — the CUDA build for a
// GPU writer, else the base build (which auto-falls back to CPUExecutionProvider on a CPU node).
// It is NOT embeddings-server-py: that's the separate HPES / hosted-search (index-manager) path.
// A reused published image, so it uses :latest, not the global spec.images.tag. Override wins.
func embeddingsImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides["embeddings"]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	if oc.Spec.Components.Engine.Writer.GPU {
		return reg + "/oramacore-ai-server-cuda:latest"
	}
	return reg + "/oramacore-ai-server:latest"
}

// oramacoreImage resolves the OramaCore engine image. OramaCore is a REUSED published image, so it
// defaults to :latest (its published tag) rather than the global spec.images.tag — that tag is for
// the operator's OWN built images (which don't include oramacore). An explicit
// spec.images.overrides["oramacore"] wins; pin a version there in production.
func oramacoreImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides["oramacore"]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	return reg + "/oramacore:latest"
}

func strPtr(s string) *string { return &s }

// deleteIfExists deletes an object, treating an already-absent object as success — for tearing down
// an optional child (e.g. an Ingress/DNS record) when its enabling spec field is cleared.
func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ingressClassName is the IngressClass the operator binds its Ingresses to — the
// helmfile-installed ingress-nginx (which fronts the cluster LoadBalancer). TODO: make
// configurable via a spec field if a cluster uses a different controller.
func ingressClassName(_ *oramav1alpha1.OramaCloudV1) string { return "nginx" }
