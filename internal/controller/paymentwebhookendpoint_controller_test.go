// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func testPaymentWebhookEndpoint() *oramav1alpha1.PaymentWebhookEndpoint {
	return &oramav1alpha1.PaymentWebhookEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", Namespace: "billing", UID: "abstract-uid", Generation: 1, Labels: map[string]string{"app": "lake"}},
		Spec: oramav1alpha1.PaymentWebhookEndpointSpec{
			ProviderRef: corev1.LocalObjectReference{Name: "stripe"}, URL: testWebhookURL, Events: []string{"invoice.paid"},
			APIVersion: testWebhookAPIVersion, App: "lake", SigningSecretName: testWebhookSecretName,
		},
	}
}

// The abstract kind resolves to the concrete one through its provider and copies its Ready
// condition and endpoint ID; without a provider it says so and makes nothing.
func TestPaymentWebhookEndpointResolvesToStripe(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	provider := &oramav1alpha1.PaymentProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing"},
		Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
			AccountID: testStripeAccount, APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
		}},
	}
	endpoint := testPaymentWebhookEndpoint()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(endpoint).
		WithStatusSubresource(endpoint, &oramav1alpha1.StripeWebhookEndpoint{}).Build()
	r := &PaymentWebhookEndpointReconciler{Client: c, Scheme: s}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "billing", Name: "lake"}}
	readyOf := func() metav1.Condition {
		t.Helper()
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, request.NamespacedName, endpoint); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(endpoint.Status.Conditions, paymentConditionReady)
		if condition == nil {
			t.Fatalf("no Ready condition: %+v", endpoint.Status)
		}
		return *condition
	}

	if ready := readyOf(); ready.Reason != "ProviderNotReady" {
		t.Errorf("Ready = %+v", ready)
	}
	var concretes oramav1alpha1.StripeWebhookEndpointList
	if err := c.List(ctx, &concretes); err != nil || len(concretes.Items) != 0 {
		t.Fatalf("concretes without a provider: %+v, %v", concretes.Items, err)
	}

	if err := c.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if ready := readyOf(); ready.Status != metav1.ConditionFalse || ready.Reason != "Progressing" {
		t.Errorf("Ready = %+v", ready)
	}
	var concrete oramav1alpha1.StripeWebhookEndpoint
	if err := c.Get(ctx, request.NamespacedName, &concrete); err != nil {
		t.Fatalf("concrete endpoint: %v", err)
	}
	if concrete.Spec.URL != testWebhookURL || concrete.Spec.APIKeySecretRef.Name != "stripe-operator" ||
		concrete.Labels["app"] != "lake" || !metav1.IsControlledBy(&concrete, endpoint) {
		t.Errorf("concrete = %+v", concrete)
	}

	concrete.Status.EndpointID = "we_1"
	concrete.Status.Writes = 1
	meta.SetStatusCondition(&concrete.Status.Conditions, metav1.Condition{
		Type: paymentConditionReady, Status: metav1.ConditionTrue, Reason: "Configured", Message: "configured", ObservedGeneration: concrete.Generation,
	})
	if err := c.Status().Update(ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	if ready := readyOf(); ready.Status != metav1.ConditionTrue || ready.Reason != "Configured" || endpoint.Status.EndpointID != "we_1" {
		t.Errorf("Ready = %+v, status %+v", ready, endpoint.Status)
	}
	version := endpoint.ResourceVersion
	readyOf()
	if endpoint.ResourceVersion != version {
		t.Error("an unchanged status was written again")
	}
}
