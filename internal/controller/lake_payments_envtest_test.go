/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Every chart CRD installs, the Lake's with its CEL rule, and the API server refuses spec.payments
// without spec.host, the host the webhook endpoint is at.
func TestLakePaymentsNeedAHostEnvtest(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	ctx, kube := startPaymentEnvtest(t, stripe, func(ctrl.Manager, *runtime.Scheme) []paymentReconciler { return nil })

	hostless := paymentsTestLake("hostless")
	hostless.UID, hostless.Spec.Host = "", ""
	err := kube.Create(ctx, hostless)
	if err == nil || !strings.Contains(err.Error(), "spec.payments needs spec.host") {
		t.Errorf("a Lake with spec.payments and no host: %v", err)
	}
	hosted := paymentsTestLake("hosted")
	hosted.UID = ""
	if err := kube.Create(ctx, hosted); err != nil {
		t.Errorf("a Lake with spec.payments and a host was refused: %v", err)
	}
	plain := paymentsTestLake("plain")
	plain.UID, plain.Spec.Host, plain.Spec.Payments = "", "", nil
	if err := kube.Create(ctx, plain); err != nil {
		t.Errorf("a Lake without spec.payments or a host was refused: %v", err)
	}
}

// The API server refuses spec.payments unless spec.billing.enabled: a Lake with billing off ignores
// its Stripe keys and answers the webhook 404. Billing without payments (the manual provider) and a
// Lake with neither (on-prem) are accepted. Every chart CRD still installs under the CEL cost budget.
func TestLakePaymentsNeedBillingEnvtest(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	ctx, kube := startPaymentEnvtest(t, stripe, func(ctrl.Manager, *runtime.Scheme) []paymentReconciler { return nil })

	unbilled := paymentsTestLake("unbilled")
	unbilled.UID, unbilled.Spec.Billing = "", nil
	if err := kube.Create(ctx, unbilled); err == nil || !strings.Contains(err.Error(), "spec.payments needs spec.billing.enabled") {
		t.Errorf("a Lake with spec.payments and no spec.billing: %v", err)
	}
	off := paymentsTestLake("billing-off")
	off.UID, off.Spec.Billing = "", &oramav1alpha1.LakeBillingSpec{Enabled: false}
	if err := kube.Create(ctx, off); err == nil || !strings.Contains(err.Error(), "spec.payments needs spec.billing.enabled") {
		t.Errorf("a Lake with spec.payments and billing disabled: %v", err)
	}
	billed := paymentsTestLake("billed")
	billed.UID = ""
	if err := kube.Create(ctx, billed); err != nil {
		t.Errorf("a Lake with spec.payments and billing enabled was refused: %v", err)
	}
	manual := paymentsTestLake("manual")
	manual.UID, manual.Spec.Payments = "", nil
	if err := kube.Create(ctx, manual); err != nil {
		t.Errorf("a Lake with billing enabled and no spec.payments was refused: %v", err)
	}
	onPrem := paymentsTestLake("on-prem")
	onPrem.UID, onPrem.Spec.Payments, onPrem.Spec.Billing = "", nil, nil
	if err := kube.Create(ctx, onPrem); err != nil {
		t.Errorf("a Lake with neither spec.payments nor spec.billing was refused: %v", err)
	}
	var stored oramav1alpha1.Lake
	if err := kube.Get(ctx, client.ObjectKeyFromObject(billed), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Billing == nil || !stored.Spec.Billing.Enabled {
		t.Errorf("spec.billing did not round-trip: %+v", stored.Spec.Billing)
	}
}
