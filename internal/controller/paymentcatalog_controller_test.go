// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func testPaymentCatalog() *oramav1alpha1.PaymentCatalog {
	return &oramav1alpha1.PaymentCatalog{
		ObjectMeta: metav1.ObjectMeta{
			Name: testCatalogKey.Name, Namespace: testCatalogKey.Namespace, UID: "abstract-uid", Generation: 1,
			Labels: map[string]string{"app": "cloud-v1"},
		},
		Spec: testCatalogSpec(),
	}
}

// The abstract kind resolves to the concrete one through its provider and copies its Ready
// condition and prices; without a provider it says so and makes nothing.
func TestPaymentCatalogResolvesToStripe(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	provider := &oramav1alpha1.PaymentProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing"},
		Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: testStripeAccount, APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
		}},
	}
	catalog := testPaymentCatalog()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(catalog).
		WithStatusSubresource(catalog, &oramav1alpha1.StripeCatalog{}).Build()
	r := &PaymentCatalogReconciler{Client: c, Scheme: s}
	readyOf := func() metav1.Condition {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: testCatalogKey}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, testCatalogKey, catalog); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(catalog.Status.Conditions, paymentConditionReady)
		if condition == nil {
			t.Fatalf("no Ready condition: %+v", catalog.Status)
		}
		return *condition
	}

	if ready := readyOf(); ready.Reason != "ProviderNotReady" {
		t.Errorf("Ready = %+v", ready)
	}
	var concretes oramav1alpha1.StripeCatalogList
	if err := c.List(ctx, &concretes); err != nil || len(concretes.Items) != 0 {
		t.Fatalf("concretes without a provider: %+v, %v", concretes.Items, err)
	}

	if err := c.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if ready := readyOf(); ready.Status != metav1.ConditionFalse || ready.Reason != "Progressing" {
		t.Errorf("Ready = %+v", ready)
	}
	var concrete oramav1alpha1.StripeCatalog
	if err := c.Get(ctx, testCatalogKey, &concrete); err != nil {
		t.Fatalf("concrete catalog: %v", err)
	}
	if concrete.Spec.Version != 3 || len(concrete.Spec.Products) != 2 || concrete.Spec.APIKeySecretRef.Name != "stripe-operator" ||
		concrete.Labels["app"] != "cloud-v1" || !metav1.IsControlledBy(&concrete, catalog) {
		t.Errorf("concrete = %+v", concrete)
	}

	concrete.Status.Prices = map[string]string{testCatalogMonthly: "price_1"}
	concrete.Status.Writes = 1
	meta.SetStatusCondition(&concrete.Status.Conditions, metav1.Condition{
		Type: paymentConditionReady, Status: metav1.ConditionTrue, Reason: "Configured", Message: "configured", ObservedGeneration: concrete.Generation,
	})
	if err := c.Status().Update(ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	if ready := readyOf(); ready.Status != metav1.ConditionTrue || ready.Reason != "Configured" || catalog.Status.Prices[testCatalogMonthly] != "price_1" {
		t.Errorf("Ready = %+v, status %+v", ready, catalog.Status)
	}
	version := catalog.ResourceVersion
	readyOf()
	if catalog.ResourceVersion != version {
		t.Error("an unchanged status was written again")
	}

	meta.SetStatusCondition(&concrete.Status.Conditions, metav1.Condition{
		Type: paymentConditionReady, Status: metav1.ConditionFalse, Reason: "LiveModeRefused",
		Message: "Cloud v1's live catalog belongs to Prod 1", ObservedGeneration: concrete.Generation,
	})
	if err := c.Status().Update(ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	if ready := readyOf(); ready.Status != metav1.ConditionFalse || ready.Reason != "LiveModeRefused" ||
		ready.Message != concrete.Status.Conditions[0].Message || ready.ObservedGeneration != catalog.Generation {
		t.Errorf("Ready = %+v", ready)
	}
}
