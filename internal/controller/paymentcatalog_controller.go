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

// PaymentCatalogReconciler resolves a PaymentCatalog against its provider: for a stripe provider it
// keeps an owned StripeCatalog of the same name and copies its Ready condition and prices back.
// Nothing here calls a vendor.
type PaymentCatalogReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=paymentcatalogs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentcatalogs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=stripecatalogs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=paymentproviders,verbs=get;list;watch

func (r *PaymentCatalogReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var catalog oramav1alpha1.PaymentCatalog
	if err := r.Get(ctx, req.NamespacedName, &catalog); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, err := r.resolve(ctx, &catalog)
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(catalog.Status, status) {
		return ctrl.Result{}, nil
	}
	catalog.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, &catalog)
}

func (r *PaymentCatalogReconciler) resolve(ctx context.Context, catalog *oramav1alpha1.PaymentCatalog) (oramav1alpha1.PaymentCatalogStatus, error) {
	withReady := func(status oramav1alpha1.PaymentCatalogStatus, condition metav1.Condition) oramav1alpha1.PaymentCatalogStatus {
		status.Conditions = append([]metav1.Condition{}, catalog.Status.Conditions...)
		condition.Type, condition.ObservedGeneration = paymentConditionReady, catalog.Generation
		meta.SetStatusCondition(&status.Conditions, condition)
		return status
	}
	notReady := func(message string) oramav1alpha1.PaymentCatalogStatus {
		return withReady(*catalog.Status.DeepCopy(), paymentCondition(false, "ProviderNotReady", message))
	}
	var provider oramav1alpha1.PaymentProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: catalog.Namespace, Name: catalog.Spec.ProviderRef.Name}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return notReady(fmt.Sprintf("PaymentProvider %q does not exist", catalog.Spec.ProviderRef.Name)), nil
		}
		return catalog.Status, err
	}
	if provider.Spec.Type != paymentProviderStripe || provider.Spec.Stripe == nil {
		return notReady(fmt.Sprintf("PaymentProvider %q has no supported type", provider.Name)), nil
	}
	concrete := &oramav1alpha1.StripeCatalog{ObjectMeta: metav1.ObjectMeta{Name: catalog.Name, Namespace: catalog.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, concrete, func() error {
		concrete.Labels = catalog.Labels
		concrete.Spec.PaymentCatalogSpec = *catalog.Spec.DeepCopy()
		concrete.Spec.APIKeySecretRef = provider.Spec.Stripe.APIKeySecretRef
		return controllerutil.SetControllerReference(catalog, concrete, r.Scheme)
	}); err != nil {
		return catalog.Status, err
	}
	status := oramav1alpha1.PaymentCatalogStatus{Prices: concrete.Status.Prices}
	ready := meta.FindStatusCondition(concrete.Status.Conditions, paymentConditionReady)
	if ready == nil || ready.ObservedGeneration != concrete.Generation {
		return withReady(status, paymentCondition(false, "Progressing",
			fmt.Sprintf("waiting for StripeCatalog %q to be configured", concrete.Name))), nil
	}
	return withReady(status, *ready), nil
}

func (r *PaymentCatalogReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PaymentCatalog{}).
		Owns(&oramav1alpha1.StripeCatalog{}).
		Watches(&oramav1alpha1.PaymentProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("payment-catalog").
		Complete(r)
}

func (r *PaymentCatalogReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var catalogs oramav1alpha1.PaymentCatalogList
	if err := r.List(ctx, &catalogs, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range catalogs.Items {
		if catalogs.Items[i].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&catalogs.Items[i])})
		}
	}
	return requests
}
