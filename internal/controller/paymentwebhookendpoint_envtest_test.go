/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// paymentReconciler is a reconciler the payment envtests run.
type paymentReconciler interface{ SetupWithManager(ctrl.Manager) error }

// startPaymentEnvtest starts an API server with the chart's CRDs and a manager running a
// PaymentProvider reconciler and the given ones against stripe, and creates namespace "billing"
// with the operator's key Secret and a stripe PaymentProvider "stripe". It skips the test without
// KUBEBUILDER_ASSETS. The context ends with the test.
func startPaymentEnvtest(t *testing.T, stripe *fakeStripe, reconcilers func(ctrl.Manager, *runtime.Scheme) []paymentReconciler) (context.Context, client.Client) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the payment envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../chart/crds"}, ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := newLakeScheme(t)
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, setup := range append(reconcilers(manager, scheme),
		&PaymentProviderReconciler{Client: manager.GetClient(), Scheme: scheme, StripeAPIBase: stripe.url}) {
		if err := setup.SetupWithManager(manager); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop manager: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "billing"}},
		paymentKeySecret(testStripeKey),
		&oramav1alpha1.PaymentProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "stripe", Namespace: "billing"},
			Spec: oramav1alpha1.PaymentProviderSpec{Type: "stripe", Stripe: &oramav1alpha1.StripePaymentProviderSpec{
				AccountID: testStripeAccount, APIKeySecretRef: oramav1alpha1.PaymentSecretKeyRef{Name: "stripe-operator", Key: "apiKey"},
			}},
		},
	} {
		if err := kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, kube
}

// The three payment controllers against an API server and the fake Stripe: the CRDs refuse a
// plain-HTTP URL and an empty event list, the abstract endpoint owns its concrete one, the
// concrete one owns the signing Secret, and the endpoint ID and Ready come back to the abstract.
func TestPaymentWebhookEndpointEnvtestOwnsItsStripeEndpoint(t *testing.T) {
	stripe := newFakeStripe(t, testStripeKey, testStripeAccount)
	ctx, kube := startPaymentEnvtest(t, stripe, func(manager ctrl.Manager, scheme *runtime.Scheme) []paymentReconciler {
		return []paymentReconciler{
			&PaymentWebhookEndpointReconciler{Client: manager.GetClient(), Scheme: scheme},
			&StripeWebhookEndpointReconciler{Client: manager.GetClient(), Scheme: scheme, StripeAPIBase: stripe.url,
				Recorder: manager.GetEventRecorderFor("stripewebhookendpoint-controller")},
		}
	})
	for name, change := range map[string]func(*oramav1alpha1.PaymentWebhookEndpointSpec){
		"plain HTTP":  func(spec *oramav1alpha1.PaymentWebhookEndpointSpec) { spec.URL = "http://eu.lakelive.example/stripe" },
		"no events":   func(spec *oramav1alpha1.PaymentWebhookEndpointSpec) { spec.Events = nil },
		"unknown app": func(spec *oramav1alpha1.PaymentWebhookEndpointSpec) { spec.App = "dashboard" },
	} {
		refused := testPaymentWebhookEndpoint()
		refused.UID, refused.Generation, refused.Name = "", 0, "refused"
		change(&refused.Spec)
		if err := kube.Create(ctx, refused); err == nil {
			t.Errorf("an endpoint with %s was accepted", name)
		}
	}
	endpoint := testPaymentWebhookEndpoint()
	endpoint.UID, endpoint.Generation = "", 0
	if err := kube.Create(ctx, endpoint); err != nil {
		t.Fatal(err)
	}

	key := types.NamespacedName{Namespace: "billing", Name: "lake"}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := kube.Get(ctx, key, endpoint); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(endpoint.Status.Conditions, paymentConditionReady) && endpoint.Status.EndpointID != "", nil
	}); err != nil {
		t.Fatalf("the abstract endpoint never became Ready: %+v (%v)", endpoint.Status, err)
	}
	var concrete oramav1alpha1.StripeWebhookEndpoint
	if err := kube.Get(ctx, key, &concrete); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&concrete, endpoint) || concrete.Status.EndpointID != endpoint.Status.EndpointID {
		t.Errorf("concrete = %+v", concrete)
	}
	var secret corev1.Secret
	if err := kube.Get(ctx, types.NamespacedName{Namespace: "billing", Name: testWebhookSecretName}, &secret); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&secret, &concrete) || len(secret.Data[paymentWebhookSigningSecretKey]) == 0 {
		t.Errorf("signing Secret = %+v", secret.ObjectMeta)
	}
	if endpoints := stripe.webhookEndpoints(); len(endpoints) != 1 || endpoints[0].ID != endpoint.Status.EndpointID {
		t.Errorf("Stripe endpoints = %+v", endpoints)
	}
}
