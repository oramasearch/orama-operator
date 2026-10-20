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
