// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

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

// cfRuleRefPrefixLimit is the longest rule ref prefix the operator writes.
const cfRuleRefPrefixLimit = 64

// edgeHostRuleRefPrefix is orama-operator-<namespace>-<name>, with dots made dashes; one longer
// than the limit keeps its start and ends with 12 hex characters of the full name's SHA-256, so it
// stays unique.
func edgeHostRuleRefPrefix(namespace, name string) string {
	prefix := strings.ReplaceAll("orama-operator-"+namespace+"-"+name, ".", "-")
	if len(prefix) <= cfRuleRefPrefixLimit {
		return prefix
	}
	sum := sha256.Sum256([]byte(prefix))
	return strings.TrimRight(prefix[:cfRuleRefPrefixLimit-13], "-") + "-" + hex.EncodeToString(sum[:])[:12]
}

// EdgeHostPolicyReconciler resolves an EdgeHostPolicy against its provider: for a cloudflare
// provider it keeps an owned CloudflareHostPolicy of the same name and copies its readiness back.
// Nothing here calls a vendor. Deleting the policy deletes the concrete one, whose finalizer
// removes the rules.
type EdgeHostPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=edgehostpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=edgehostpolicies/status,verbs=get;update;patch

func (r *EdgeHostPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy oramav1alpha1.EdgeHostPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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

func (r *EdgeHostPolicyReconciler) resolve(ctx context.Context, policy *oramav1alpha1.EdgeHostPolicy) (oramav1alpha1.EdgeHostPolicyStatus, error) {
	notReady := func(message string) oramav1alpha1.EdgeHostPolicyStatus {
		return oramav1alpha1.EdgeHostPolicyStatus{Message: message}
	}
	var provider oramav1alpha1.EdgeProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: policy.Spec.ProviderRef.Name}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return notReady(fmt.Sprintf("EdgeProvider %q does not exist", policy.Spec.ProviderRef.Name)), nil
		}
		return policy.Status, err
	}
	if provider.Spec.Type != "cloudflare" || provider.Spec.Cloudflare == nil {
		return notReady(fmt.Sprintf("EdgeProvider %q has no supported type", provider.Name)), nil
	}
	concrete := &oramav1alpha1.CloudflareHostPolicy{ObjectMeta: metav1.ObjectMeta{Name: policy.Name, Namespace: policy.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, concrete, func() error {
		concrete.Labels = policy.Labels
		concrete.Spec.AccountID = provider.Spec.Cloudflare.AccountID
		concrete.Spec.APITokenSecretRef = provider.Spec.Cloudflare.APITokenSecretRef
		concrete.Spec.Hosts = append([]string(nil), policy.Spec.Hosts...)
		concrete.Spec.OriginTLS = policy.Spec.OriginTLS
		concrete.Spec.RedirectToHTTPS = policy.Spec.RedirectToHTTPS
		concrete.Spec.RuleRefPrefix = edgeHostRuleRefPrefix(policy.Namespace, policy.Name)
		return controllerutil.SetControllerReference(policy, concrete, r.Scheme)
	}); err != nil {
		return policy.Status, err
	}
	if concrete.Status.ObservedGeneration != concrete.Generation || concrete.Status.Message == "" {
		return notReady("waiting for the provider to apply the rules"), nil
	}
	return oramav1alpha1.EdgeHostPolicyStatus{Ready: concrete.Status.Ready, Message: concrete.Status.Message}, nil
}

func (r *EdgeHostPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.EdgeHostPolicy{}).
		Owns(&oramav1alpha1.CloudflareHostPolicy{}).
		Watches(&oramav1alpha1.EdgeProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("edge-host-policy").
		Complete(r)
}

func (r *EdgeHostPolicyReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var policies oramav1alpha1.EdgeHostPolicyList
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
