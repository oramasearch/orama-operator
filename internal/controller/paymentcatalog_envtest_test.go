/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The catalog controllers against an API server and the fake Stripe: the CRDs refuse what Stripe
// or the ownership tags cannot take, the abstract catalog owns its concrete one, and the prices and
// Ready come back to the abstract.
func TestPaymentCatalogEnvtestOwnsItsStripeCatalog(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	ctx, kube := startPaymentEnvtest(t, stripe, func(manager ctrl.Manager, scheme *runtime.Scheme) []paymentReconciler {
		return []paymentReconciler{
			&PaymentCatalogReconciler{Client: manager.GetClient(), Scheme: scheme},
			&StripeCatalogReconciler{Client: manager.GetClient(), Scheme: scheme, StripeAPIBase: stripe.url},
		}
	})
	for name, change := range map[string]func(*oramav1alpha1.PaymentCatalogSpec){
		"an unknown app":          func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.App = "dashboard" },
		"version 0":               func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Version = 0 },
		"an uppercase product ID": func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Products[0].ID = "Cloud_V1_Pro" },
		"a repeated product ID":   func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Products[1].ID = spec.Products[0].ID },
		"an uppercase currency":   func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Products[0].Prices[0].Currency = "USD" },
		"a weekly interval":       func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Products[0].Prices[0].Interval = "week" },
		"a space in a lookup key": func(spec *oramav1alpha1.PaymentCatalogSpec) { spec.Products[0].Prices[0].LookupKey = "pro monthly" },
	} {
		refused := testPaymentCatalog()
		refused.UID, refused.Generation, refused.Name = "", 0, "refused"
		change(&refused.Spec)
		if err := kube.Create(ctx, refused); err == nil {
			t.Errorf("a catalog with %s was accepted", name)
		}
	}
	catalog := testPaymentCatalog()
	catalog.UID, catalog.Generation = "", 0
	if err := kube.Create(ctx, catalog); err != nil {
		t.Fatal(err)
	}

	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := kube.Get(ctx, testCatalogKey, catalog); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(catalog.Status.Conditions, paymentConditionReady) && len(catalog.Status.Prices) == 3, nil
	}); err != nil {
		t.Fatalf("the abstract catalog never became Ready: %+v (%v)", catalog.Status, err)
	}
	var concrete oramav1alpha1.StripeCatalog
	if err := kube.Get(ctx, testCatalogKey, &concrete); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&concrete, catalog) || concrete.Status.Prices[testCatalogMonthly] != catalog.Status.Prices[testCatalogMonthly] {
		t.Errorf("concrete = %+v", concrete)
	}
	if product := stripe.product(t, "cloud_v1_plan_pro"); product.Metadata["orama_owner"] != "billing/cloud-v1" {
		t.Errorf("product = %+v", product)
	}
}
