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
	"strconv"

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
	networkSandboxMetadataDenyName  = "deny-cloud-metadata"
	networkSandboxCommentAnnotation = "orama.cloud/network-policy-comment"
)

// cloudMetadataDenyCIDRs are the addresses every untrusted pod is denied on every port with a
// Cilium egressDeny: the cloud instance metadata service, Azure's host-service address (WireServer)
// and the IPv6 metadata counterpart. On AKS-managed Cilium an ipBlock exclusion alone did not stop
// metadata traffic (docs/infrastructure/network-sandbox.md#mandatory-cloud-metadata-deny).
func cloudMetadataDenyCIDRs() []interface{} {
	return []interface{}{"169.254.169.254/32", "168.63.129.16/32", "fd00:ec2::254/128"}
}

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
	if !sandbox.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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

// SetupWithManager watches both policy kinds so deletion and drift repair the mandatory denies.
// CiliumNetworkPolicy support is required for every sandbox, including on AKS-managed Cilium.
func (r *NetworkSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ciliumPolicy := &unstructured.Unstructured{}
	ciliumPolicy.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.NetworkSandbox{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(ciliumPolicy).
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
	// Install the explicit metadata deny before any allowances. NetworkPolicy ipBlock exclusions
	// alone do not block Azure metadata traffic on AKS-managed Cilium.
	ciliumCount, err := reconcileSandboxCiliumPolicies(ctx, kube, scheme, sandbox)
	if err != nil {
		return 1, ciliumCount, err
	}

	policies := append([]oramav1alpha1.NetworkSandboxPolicy(nil), sandbox.Spec.Policies...)
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	for _, allowance := range policies {
		if allowance.Name == networkSandboxDefaultDenyName {
			return int32(len(desiredNames)), ciliumCount, fmt.Errorf("policy %q is reserved for the mandatory default deny", allowance.Name)
		}
		if desiredNames[allowance.Name] {
			return int32(len(desiredNames)), ciliumCount, fmt.Errorf("policy name %q is duplicated", allowance.Name)
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
		policy.Spec.PodSelector = intersectSandboxSelectors(policy.Spec.PodSelector, sandbox.Spec.PodSelector)
		if sandboxMeshEnrolled(sandbox) && isConnectionAllowanceName(allowance.Name) {
			meshOnlyPorts(&policy.Spec)
			policy.Annotations[networkSandboxCommentAnnotation] = allowance.Comment + " Enrolled in the mesh: only the mesh's mutual-TLS port " + strconv.Itoa(zettaTunnelMeshInboundPort) + " is allowed."
		}
		if err := reconcileSandboxNetworkPolicy(ctx, kube, scheme, sandbox, policy); err != nil {
			return int32(len(desiredNames) - 1), ciliumCount, err
		}
	}
	if err := deleteStaleSandboxNetworkPolicies(ctx, kube, sandbox, desiredNames); err != nil {
		return int32(len(desiredNames)), ciliumCount, err
	}
	return int32(len(desiredNames)), ciliumCount, nil
}

func defaultDenyNetworkPolicy(sandbox *oramav1alpha1.NetworkSandbox) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkSandboxDefaultDenyName,
			Namespace: sandbox.Namespace,
			Labels:    copyStringMap(sandbox.Spec.PolicyLabels),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *sandbox.Spec.PodSelector.DeepCopy(),
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// intersectSandboxSelectors preserves both sets of requirements, including contradictory ones.
// In particular, an allowance cannot replace an owner's label with another owner's value.
// Keeping an empty scope unchanged preserves live workspace policy specs exactly.
func intersectSandboxSelectors(pods, scope metav1.LabelSelector) metav1.LabelSelector {
	result := pods.DeepCopy()
	keys := make([]string, 0, len(scope.MatchLabels))
	for key := range scope.MatchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := scope.MatchLabels[key]
		if existing, found := result.MatchLabels[key]; found && existing != value {
			result.MatchExpressions = append(result.MatchExpressions, metav1.LabelSelectorRequirement{
				Key: key, Operator: metav1.LabelSelectorOpIn, Values: []string{value},
			})
			continue
		}
		if result.MatchLabels == nil {
			result.MatchLabels = map[string]string{}
		}
		result.MatchLabels[key] = value
	}
	result.MatchExpressions = append(result.MatchExpressions, scope.DeepCopy().MatchExpressions...)
	return *result
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
	metadataDeny := map[string]interface{}{
		"egressDeny": []interface{}{
			map[string]interface{}{
				"toCIDR": cloudMetadataDenyCIDRs(),
			},
		},
	}
	if err := reconcileSandboxCiliumPolicy(ctx, kube, scheme, sandbox, networkSandboxMetadataDenyName,
		"Deny cloud metadata and host-service addresses regardless of other allowances.", metadataDeny); err != nil {
		return 0, err
	}
	policies := append([]oramav1alpha1.NetworkSandboxCiliumPolicy(nil), sandbox.Spec.CiliumPolicies...)
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	desiredNames := map[string]bool{networkSandboxMetadataDenyName: true}
	for _, allowance := range policies {
		if allowance.Name == networkSandboxMetadataDenyName {
			return int32(len(desiredNames)), fmt.Errorf("Cilium policy %q is reserved for the mandatory metadata deny", allowance.Name)
		}
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
		if err := reconcileSandboxCiliumPolicy(ctx, kube, scheme, sandbox, allowance.Name, allowance.Comment, spec); err != nil {
			return int32(len(desiredNames) - 1), err
		}
	}
	return int32(len(desiredNames)), deleteStaleSandboxCiliumPolicies(ctx, kube, sandbox, desiredNames)
}

func reconcileSandboxCiliumPolicy(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
	name, comment string,
	spec map[string]interface{},
) error {
	if err := scopeCiliumSandboxPolicy(spec, sandbox.Spec.PodSelector); err != nil {
		return fmt.Errorf("scope CiliumNetworkPolicy %q: %w", name, err)
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	policy.SetName(name)
	policy.SetNamespace(sandbox.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, kube, policy, func() error {
		if owner := metav1.GetControllerOf(policy); owner != nil && !metav1.IsControlledBy(policy, sandbox) {
			return fmt.Errorf("CiliumNetworkPolicy %s/%s is controlled by %s %s", policy.GetNamespace(), policy.GetName(), owner.Kind, owner.Name)
		}
		policy.SetLabels(copyStringMap(sandbox.Spec.PolicyLabels))
		policy.SetAnnotations(map[string]string{networkSandboxCommentAnnotation: comment})
		policy.Object["spec"] = spec
		delete(policy.Object, "specs")
		return controllerutil.SetControllerReference(sandbox, policy, scheme)
	})
	if err != nil {
		return fmt.Errorf("reconcile CiliumNetworkPolicy %s/%s: %w", sandbox.Namespace, name, err)
	}
	return nil
}

func deleteStaleSandboxCiliumPolicies(
	ctx context.Context,
	kube client.Client,
	sandbox *oramav1alpha1.NetworkSandbox,
	desiredNames map[string]bool,
) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicyList"})
	if err := kube.List(ctx, list, client.InNamespace(sandbox.Namespace)); err != nil {
		return fmt.Errorf("list CiliumNetworkPolicies in %s: %w", sandbox.Namespace, err)
	}
	for i := range list.Items {
		policy := &list.Items[i]
		if desiredNames[policy.GetName()] || !metav1.IsControlledBy(policy, sandbox) {
			continue
		}
		if err := kube.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale CiliumNetworkPolicy %s/%s: %w", policy.GetNamespace(), policy.GetName(), err)
		}
	}
	return nil
}

func scopeCiliumSandboxPolicy(spec map[string]interface{}, scope metav1.LabelSelector) error {
	if _, present := spec["nodeSelector"]; present {
		return fmt.Errorf("nodeSelector is not allowed in a pod sandbox")
	}
	var pods metav1.LabelSelector
	if raw, present := spec["endpointSelector"]; present {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &pods); err != nil {
			return fmt.Errorf("decode endpointSelector: %w", err)
		}
	}
	// Cilium's k8s label source pins the scope to pod labels, not another identity source.
	kubernetesScope := metav1.LabelSelector{}
	if scope.MatchLabels != nil {
		kubernetesScope.MatchLabels = map[string]string{}
	}
	for key, value := range scope.MatchLabels {
		kubernetesScope.MatchLabels["k8s:"+key] = value
	}
	for _, expression := range scope.DeepCopy().MatchExpressions {
		expression.Key = "k8s:" + expression.Key
		kubernetesScope.MatchExpressions = append(kubernetesScope.MatchExpressions, expression)
	}
	pods = intersectSandboxSelectors(pods, kubernetesScope)
	selector, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pods)
	if err != nil {
		return err
	}
	spec["endpointSelector"] = selector
	return nil
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
