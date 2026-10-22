/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lakeBillingEnvName is lake's billing switch (lake src/main.cpp): billing is on only when it is
// on, true or 1. Off, lake registers no billing capabilities and builds no payment provider.
const lakeBillingEnvName = "LAKE_BILLING"

// lakeBillingEnv is the LAKE_BILLING spec.billing gives, and whether spec.billing decides it.
// With spec.billing the variable is the operator's: LAKE_BILLING=on when enabled, none when not,
// and a spec.env entry is dropped. Without it (the transition, docs/lake/payments.md) spec.env
// keeps whatever it says, so a member billing through spec.env bills until its set file moves.
func lakeBillingEnv(lake *oramav1alpha1.Lake) (env []corev1.EnvVar, owned bool) {
	if lake.Spec.Billing == nil {
		return nil, false
	}
	if lake.Spec.Billing.Enabled {
		return []corev1.EnvVar{{Name: lakeBillingEnvName, Value: "on"}}, true
	}
	return nil, true
}

// lakeSpecEnvBillingIgnored is whether spec.env names LAKE_BILLING while spec.billing, which
// replaces it, is set. The reconciler logs it so the leftover entry is seen and removed.
func lakeSpecEnvBillingIgnored(lake *oramav1alpha1.Lake) bool {
	if lake.Spec.Billing == nil {
		return false
	}
	for _, variable := range lake.Spec.Env {
		if variable.Name == lakeBillingEnvName {
			return true
		}
	}
	return false
}
