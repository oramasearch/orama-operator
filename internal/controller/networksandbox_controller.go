/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	networkSandboxName              = "network-sandbox"
	networkSandboxDefaultDenyName   = "default-deny"
	networkSandboxCommentAnnotation = "orama.cloud/network-policy-comment"
)

var ciliumNetworkPolicyGVK = schema.GroupVersionKind{
	Group:   "cilium.io",
	Version: "v2",
	Kind:    "CiliumNetworkPolicy",
}

// NetworkSandboxReconciler renders one namespace's explicit policy declaration.
type NetworkSandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=networksandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=networksandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders the desired policies and writes status only when its stable summary changes.
func (r *NetworkSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	policyCount, ciliumCount, err := reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, &sandbox)
	condition := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: sandbox.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "RenderFailed"
		condition.Message = err.Error()
	} else {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "PoliciesReady"
		condition.Message = fmt.Sprintf("rendered %d NetworkPolicies and %d CiliumNetworkPolicies", policyCount, ciliumCount)
	}
	if statusErr := r.updateStatus(ctx, &sandbox, policyCount, ciliumCount, condition); statusErr != nil {
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("render policies: %v; update status: %w", err, statusErr)
		}
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

func (r *NetworkSandboxReconciler) updateStatus(
	ctx context.Context,
	sandbox *oramav1alpha1.NetworkSandbox,
	policyCount int32,
	ciliumCount int32,
	condition metav1.Condition,
) error {
	desired := oramav1alpha1.NetworkSandboxStatus{
		ObservedGeneration:    sandbox.Generation,
		NetworkPolicies:       policyCount,
		CiliumNetworkPolicies: ciliumCount,
		Conditions:            append([]metav1.Condition(nil), sandbox.Status.Conditions...),
	}
	apimeta.SetStatusCondition(&desired.Conditions, condition)
	if apiequality.Semantic.DeepEqual(sandbox.Status, desired) {
		return nil
	}
	sandbox.Status = desired
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return fmt.Errorf("update NetworkSandbox status: %w", err)
	}
	return nil
}

// SetupWithManager registers the controller unconditionally. CiliumNetworkPolicy is deliberately
// not watched: clusters without the optional Cilium CRD must still start, and current sandboxes use
// ordinary NetworkPolicy for every expressible path.
func (r *NetworkSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.NetworkSandbox{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

func reconcileNetworkSandboxPolicies(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
) (int32, int32, error) {
	desiredNames := map[string]bool{networkSandboxDefaultDenyName: true}
	if err := reconcileSandboxNetworkPolicy(ctx, kube, scheme, sandbox, defaultDenyNetworkPolicy(sandbox)); err != nil {
		return 0, 0, err
	}

	policies := append([]oramav1alpha1.NetworkSandboxPolicy(nil), sandbox.Spec.Policies...)
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	for _, allowance := range policies {
		if allowance.Name == networkSandboxDefaultDenyName {
			return 1, 0, fmt.Errorf("policy %q is reserved for the mandatory default deny", allowance.Name)
		}
		if desiredNames[allowance.Name] {
			return 1, 0, fmt.Errorf("policy name %q is duplicated", allowance.Name)
		}
		desiredNames[allowance.Name] = true
		policy := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:        allowance.Name,
				Namespace:   sandbox.Namespace,
				Labels:      copyStringMap(sandbox.Spec.PolicyLabels),
				Annotations: map[string]string{networkSandboxCommentAnnotation: allowance.Comment},
			},
			Spec: *allowance.Spec.DeepCopy(),
		}
		if err := reconcileSandboxNetworkPolicy(ctx, kube, scheme, sandbox, policy); err != nil {
			return int32(len(desiredNames) - 1), 0, err
		}
	}
	if err := deleteStaleSandboxNetworkPolicies(ctx, kube, sandbox, desiredNames); err != nil {
		return int32(len(desiredNames)), 0, err
	}

	ciliumCount, err := reconcileSandboxCiliumPolicies(ctx, kube, scheme, sandbox)
	return int32(len(desiredNames)), ciliumCount, err
}

func defaultDenyNetworkPolicy(sandbox *oramav1alpha1.NetworkSandbox) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkSandboxDefaultDenyName,
			Namespace: sandbox.Namespace,
			Labels:    copyStringMap(sandbox.Spec.PolicyLabels),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

func reconcileSandboxNetworkPolicy(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
	desired *networkingv1.NetworkPolicy,
) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, kube, policy, func() error {
		if owner := metav1.GetControllerOf(policy); owner != nil && !metav1.IsControlledBy(policy, sandbox) {
			return fmt.Errorf("NetworkPolicy %s/%s is controlled by %s %s", policy.Namespace, policy.Name, owner.Kind, owner.Name)
		}
		policy.Labels = copyStringMap(desired.Labels)
		policy.Annotations = copyStringMap(desired.Annotations)
		policy.Spec = *desired.Spec.DeepCopy()
		return controllerutil.SetControllerReference(sandbox, policy, scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile NetworkPolicy %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	return nil
}

func deleteStaleSandboxNetworkPolicies(
	ctx context.Context,
	kube client.Client,
	sandbox *oramav1alpha1.NetworkSandbox,
	desired map[string]bool,
) error {
	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies, client.InNamespace(sandbox.Namespace)); err != nil {
		return fmt.Errorf("list NetworkPolicies in %s: %w", sandbox.Namespace, err)
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if desired[policy.Name] || !metav1.IsControlledBy(policy, sandbox) {
			continue
		}
		if err := kube.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}
	return nil
}

func reconcileSandboxCiliumPolicies(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
) (int32, error) {
	policies := append([]oramav1alpha1.NetworkSandboxCiliumPolicy(nil), sandbox.Spec.CiliumPolicies...)
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	desiredNames := map[string]bool{}
	for _, allowance := range policies {
		if desiredNames[allowance.Name] {
			return int32(len(desiredNames)), fmt.Errorf("Cilium policy name %q is duplicated", allowance.Name)
		}
		desiredNames[allowance.Name] = true
		var spec map[string]interface{}
		if err := json.Unmarshal(allowance.Spec.Raw, &spec); err != nil {
			return int32(len(desiredNames) - 1), fmt.Errorf("decode CiliumNetworkPolicy %q spec: %w", allowance.Name, err)
		}
		if spec == nil {
			return int32(len(desiredNames) - 1), fmt.Errorf("CiliumNetworkPolicy %q spec must be a JSON object", allowance.Name)
		}
		desired := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name":      allowance.Name,
				"namespace": sandbox.Namespace,
				"annotations": map[string]interface{}{
					networkSandboxCommentAnnotation: allowance.Comment,
				},
			},
			"spec": spec,
		}}
		desired.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		desired.SetLabels(copyStringMap(sandbox.Spec.PolicyLabels))
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		current.SetName(allowance.Name)
		current.SetNamespace(sandbox.Namespace)
		_, err := controllerutil.CreateOrUpdate(ctx, kube, current, func() error {
			if owner := metav1.GetControllerOf(current); owner != nil && !metav1.IsControlledBy(current, sandbox) {
				return fmt.Errorf("CiliumNetworkPolicy %s/%s is controlled by %s %s", current.GetNamespace(), current.GetName(), owner.Kind, owner.Name)
			}
			current.SetLabels(desired.GetLabels())
			current.SetAnnotations(desired.GetAnnotations())
			current.Object["spec"] = desired.Object["spec"]
			return controllerutil.SetControllerReference(sandbox, current, scheme)
		})
		if err != nil {
			return int32(len(desiredNames) - 1), fmt.Errorf("reconcile CiliumNetworkPolicy %s/%s: %w", sandbox.Namespace, allowance.Name, err)
		}
	}
	if len(policies) == 0 && sandbox.Status.CiliumNetworkPolicies == 0 {
		return 0, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicyList"})
	if err := kube.List(ctx, list, client.InNamespace(sandbox.Namespace)); err != nil {
		return int32(len(desiredNames)), fmt.Errorf("list CiliumNetworkPolicies in %s: %w", sandbox.Namespace, err)
	}
	for i := range list.Items {
		policy := &list.Items[i]
		if desiredNames[policy.GetName()] || !metav1.IsControlledBy(policy, sandbox) {
			continue
		}
		if err := kube.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return int32(len(desiredNames)), fmt.Errorf("delete stale CiliumNetworkPolicy %s/%s: %w", policy.GetNamespace(), policy.GetName(), err)
		}
	}
	return int32(len(desiredNames)), nil
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
