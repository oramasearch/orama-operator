/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"

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

// embeddingsImage resolves the OramaCore ai-server (embeddings) image. That image has cpu/gpu
// VARIANTS published as :cpu-latest / :gpu-latest, so the tag is picked to match the engine's
// GPU setting (writer.gpu) rather than the global spec.images.tag — a CPU deployment gets the
// CPU embeddings, a GPU one gets CUDA. An explicit spec.images.overrides["embeddings"] wins.
func embeddingsImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides["embeddings"]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	variant := "cpu"
	if oc.Spec.Components.Engine.Writer.GPU {
		variant = "gpu"
	}
	return reg + "/embeddings-server-py:" + variant + "-latest"
}
