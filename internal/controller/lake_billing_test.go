// SPDX-License-Identifier: MIT

package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lakeBillingValues is every LAKE_BILLING the Lake's container gets, in order.
func lakeBillingValues(lake *oramav1alpha1.Lake) []string {
	var values []string
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		if variable.Name == lakeBillingEnvName {
			values = append(values, variable.Value)
		}
	}
	return values
}

func billingTestLake(billing *oramav1alpha1.LakeBillingSpec, env ...corev1.EnvVar) *oramav1alpha1.Lake {
	lake := paymentsTestLake("billing")
	lake.Spec.Payments, lake.Spec.Billing = nil, billing
	lake.Spec.Env = env
	return lake
}

// spec.billing.enabled gives the Lake LAKE_BILLING=on, once, and replaces spec.env's entry.
func TestLakeBillingEnabledGivesLakeBillingOn(t *testing.T) {
	enabled := &oramav1alpha1.LakeBillingSpec{Enabled: true}
	if got := lakeBillingValues(billingTestLake(enabled)); !reflect.DeepEqual(got, []string{"on"}) {
		t.Errorf("billing enabled: LAKE_BILLING %v, want [on]", got)
	}
	lake := billingTestLake(enabled, corev1.EnvVar{Name: lakeBillingEnvName, Value: "off"})
	if got := lakeBillingValues(lake); !reflect.DeepEqual(got, []string{"on"}) {
		t.Errorf("billing enabled, spec.env LAKE_BILLING=off: LAKE_BILLING %v, want [on]", got)
	}
	if !lakeSpecEnvBillingIgnored(lake) {
		t.Error("spec.env's LAKE_BILLING beside spec.billing is not reported as ignored")
	}
}

// spec.billing with enabled false is off: no LAKE_BILLING, even when spec.env says on.
func TestLakeBillingDisabledGivesNoLakeBilling(t *testing.T) {
	disabled := &oramav1alpha1.LakeBillingSpec{Enabled: false}
	if got := lakeBillingValues(billingTestLake(disabled)); got != nil {
		t.Errorf("billing disabled: LAKE_BILLING %v, want none", got)
	}
	lake := billingTestLake(disabled, corev1.EnvVar{Name: lakeBillingEnvName, Value: "on"})
	if got := lakeBillingValues(lake); got != nil {
		t.Errorf("billing disabled, spec.env LAKE_BILLING=on: LAKE_BILLING %v, want none", got)
	}
	if !lakeSpecEnvBillingIgnored(lake) {
		t.Error("spec.env's LAKE_BILLING beside spec.billing is not reported as ignored")
	}
}

// Without spec.billing (an on-prem install, or a member not yet moved off spec.env) the operator
// gives no LAKE_BILLING, and spec.env's entry passes through unchanged.
func TestLakeBillingUnsetLeavesSpecEnv(t *testing.T) {
	if got := lakeBillingValues(billingTestLake(nil)); got != nil {
		t.Errorf("billing unset: LAKE_BILLING %v, want none", got)
	}
	lake := billingTestLake(nil, corev1.EnvVar{Name: lakeBillingEnvName, Value: "on"})
	if got := lakeBillingValues(lake); !reflect.DeepEqual(got, []string{"on"}) {
		t.Errorf("billing unset, spec.env LAKE_BILLING=on: LAKE_BILLING %v, want [on]", got)
	}
	if lakeSpecEnvBillingIgnored(lake) {
		t.Error("spec.env's LAKE_BILLING without spec.billing is reported as ignored")
	}
}
