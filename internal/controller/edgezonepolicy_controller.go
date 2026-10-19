/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// EdgeProviderReconciler reports whether a provider's token Secret is present. Whether the token is
// allowed what a kind needs is reported by that kind (an EdgeZonePolicy names the missing
// permission).
type EdgeProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=edgeproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=edgeproviders/status,verbs=get;update;patch

func (r *EdgeProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var provider oramav1alpha1.EdgeProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := oramav1alpha1.EdgeProviderStatus{ObservedGeneration: provider.Generation, Ready: true}
	if message := edgeProviderProblem(ctx, r.Client, &provider); message != "" {
		status.Ready, status.Message = false, message
	}
	if equality.Semantic.DeepEqual(provider.Status, status) {
		return ctrl.Result{}, nil
	}
	provider.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, &provider)
}

// edgeProviderProblem is empty when the provider's token Secret and key exist.
func edgeProviderProblem(ctx context.Context, c client.Client, provider *oramav1alpha1.EdgeProvider) string {
	if provider.Spec.Type != "cloudflare" || provider.Spec.Cloudflare == nil {
		return fmt.Sprintf("provider type %q is not supported", provider.Spec.Type)
	}
	ref := provider.Spec.Cloudflare.APITokenSecretRef
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: provider.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("token Secret %q does not exist", ref.Name)
		}
		return fmt.Sprintf("read token Secret %q: %v", ref.Name, err)
	}
	if len(secret.Data[ref.Key]) == 0 {
		return fmt.Sprintf("token Secret %q has no key %q", ref.Name, ref.Key)
	}
	return ""
}

func (r *EdgeProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.EdgeProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Named("edge-provider").
		Complete(r)
}

func (r *EdgeProviderReconciler) requestsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	var providers oramav1alpha1.EdgeProviderList
	if err := r.List(ctx, &providers, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range providers.Items {
		if c := providers.Items[i].Spec.Cloudflare; c != nil && c.APITokenSecretRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&providers.Items[i])})
		}
	}
	return requests
}

// EdgeZonePolicyReconciler resolves an EdgeZonePolicy against its provider: for a cloudflare
// provider it keeps an owned CloudflareZonePolicy of the same name and copies its status back.
// Nothing here calls a vendor.
type EdgeZonePolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=edgezonepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=edgezonepolicies/status,verbs=get;update;patch

func (r *EdgeZonePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy oramav1alpha1.EdgeZonePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, err := r.resolve(ctx, &policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	status.ObservedGeneration = policy.Generation
	if equality.Semantic.DeepEqual(policy.Status, status) {
		return ctrl.Result{}, nil
	}
	policy.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, &policy)
}

func (r *EdgeZonePolicyReconciler) resolve(ctx context.Context, policy *oramav1alpha1.EdgeZonePolicy) (oramav1alpha1.EdgeZonePolicyStatus, error) {
	pending := func(message string) oramav1alpha1.EdgeZonePolicyStatus {
		status := policy.Status
		status.Phase, status.Message = edgePhaseFailed, message
		status.Conditions = mergeEdgeConditions(policy.Status.Conditions, []metav1.Condition{{
			Type: edgeConditionCredentials, Status: metav1.ConditionFalse, Reason: "ProviderNotReady", Message: message,
		}}, policy.Generation)
		return status
	}
	var provider oramav1alpha1.EdgeProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: policy.Spec.ProviderRef.Name}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return pending(fmt.Sprintf("EdgeProvider %q does not exist", policy.Spec.ProviderRef.Name)), nil
		}
		return policy.Status, err
	}
	if provider.Spec.Type != "cloudflare" || provider.Spec.Cloudflare == nil {
		return pending(fmt.Sprintf("EdgeProvider %q has no supported type", provider.Name)), nil
	}
	concrete := &oramav1alpha1.CloudflareZonePolicy{ObjectMeta: metav1.ObjectMeta{Name: policy.Name, Namespace: policy.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, concrete, func() error {
		concrete.Labels = policy.Labels
		concrete.Spec.ZoneID = policy.Spec.ZoneID
		concrete.Spec.APITokenSecretRef = provider.Spec.Cloudflare.APITokenSecretRef
		return controllerutil.SetControllerReference(policy, concrete, r.Scheme)
	}); err != nil {
		return policy.Status, err
	}
	if concrete.Status.Phase == "" {
		status := policy.Status
		status.Phase, status.Message = edgePhaseProgressing, "waiting for the zone to be read"
		return status, nil
	}
	status := *concrete.Status.DeepCopy()
	return status, nil
}

func (r *EdgeZonePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.EdgeZonePolicy{}).
		Owns(&oramav1alpha1.CloudflareZonePolicy{}).
		Watches(&oramav1alpha1.EdgeProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("edge-zone-policy").
		Complete(r)
}

func (r *EdgeZonePolicyReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var policies oramav1alpha1.EdgeZonePolicyList
	if err := r.List(ctx, &policies, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range policies.Items {
		if policies.Items[i].Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&policies.Items[i])})
		}
	}
	return requests
}
