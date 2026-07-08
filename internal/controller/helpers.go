/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
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
