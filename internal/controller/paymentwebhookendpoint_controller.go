/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// PaymentWebhookEndpointReconciler resolves a PaymentWebhookEndpoint against its provider: for a
// stripe provider it keeps an owned StripeWebhookEndpoint of the same name and copies its Ready
// condition and endpoint ID back. Nothing here calls a vendor.
type PaymentWebhookEndpointReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=paymentwebhookendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentwebhookendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=stripewebhookendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders,verbs=get;list;watch

func (r *PaymentWebhookEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var endpoint oramav1alpha1.PaymentWebhookEndpoint
	if err := r.Get(ctx, req.NamespacedName, &endpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, err := r.resolve(ctx, &endpoint)
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(endpoint.Status, status) {
		return ctrl.Result{}, nil
	}
	endpoint.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, &endpoint)
}

func (r *PaymentWebhookEndpointReconciler) resolve(ctx context.Context, endpoint *oramav1alpha1.PaymentWebhookEndpoint) (oramav1alpha1.PaymentWebhookEndpointStatus, error) {
	withReady := func(status oramav1alpha1.PaymentWebhookEndpointStatus, condition metav1.Condition) oramav1alpha1.PaymentWebhookEndpointStatus {
		status.Conditions = append([]metav1.Condition{}, endpoint.Status.Conditions...)
		condition.Type, condition.ObservedGeneration = paymentConditionReady, endpoint.Generation
		meta.SetStatusCondition(&status.Conditions, condition)
		return status
	}
	notReady := func(message string) oramav1alpha1.PaymentWebhookEndpointStatus {
		return withReady(*endpoint.Status.DeepCopy(), paymentCondition(false, "ProviderNotReady", message))
	}
	var provider oramav1alpha1.PaymentProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: endpoint.Namespace, Name: endpoint.Spec.ProviderRef.Name}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return notReady(fmt.Sprintf("PaymentProvider %q does not exist", endpoint.Spec.ProviderRef.Name)), nil
		}
		return endpoint.Status, err
	}
	if provider.Spec.Type != paymentProviderStripe || provider.Spec.Stripe == nil {
		return notReady(fmt.Sprintf("PaymentProvider %q has no supported type", provider.Name)), nil
	}
	concrete := &oramav1alpha1.StripeWebhookEndpoint{ObjectMeta: metav1.ObjectMeta{Name: endpoint.Name, Namespace: endpoint.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, concrete, func() error {
		concrete.Labels = endpoint.Labels
		concrete.Spec.PaymentWebhookEndpointSpec = *endpoint.Spec.DeepCopy()
		concrete.Spec.APIKeySecretRef = provider.Spec.Stripe.APIKeySecretRef
		return controllerutil.SetControllerReference(endpoint, concrete, r.Scheme)
	}); err != nil {
		return endpoint.Status, err
	}
	status := oramav1alpha1.PaymentWebhookEndpointStatus{EndpointID: concrete.Status.EndpointID}
	ready := meta.FindStatusCondition(concrete.Status.Conditions, paymentConditionReady)
	if ready == nil || ready.ObservedGeneration != concrete.Generation {
		return withReady(status, paymentCondition(false, "Progressing",
			fmt.Sprintf("waiting for StripeWebhookEndpoint %q to be configured", concrete.Name))), nil
	}
	return withReady(status, *ready), nil
}

func (r *PaymentWebhookEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PaymentWebhookEndpoint{}).
		Owns(&oramav1alpha1.StripeWebhookEndpoint{}).
		Watches(&oramav1alpha1.PaymentProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("payment-webhook-endpoint").
		Complete(r)
}

func (r *PaymentWebhookEndpointReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var endpoints oramav1alpha1.PaymentWebhookEndpointList
	if err := r.List(ctx, &endpoints, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range endpoints.Items {
		if endpoints.Items[i].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&endpoints.Items[i])})
		}
	}
	return requests
}
