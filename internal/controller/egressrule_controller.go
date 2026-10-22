/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	egressRuleFinalizer = "orama.cloud/egress-rule-policy"
	// egressRuleRefresh re-renders a Ready rule so node, pod-range and API addresses that changed
	// are excluded again, as the workspace reconciler did for internetEgress.
	egressRuleRefresh = 5 * time.Minute
)

// EgressRuleReconciler adds a rule's allowances to the workspace NetworkSandbox and, for a DNS
// name, keeps the workspace's egress gateway and its rule snapshot.
type EgressRuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=egressrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=egressrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=egressrules/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=networksandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakeapps;lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes;endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *EgressRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rule oramav1alpha1.EgressRule
	if err := r.Get(ctx, req.NamespacedName, &rule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rule.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&rule, egressRuleFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.remove(ctx, &rule); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&rule, egressRuleFinalizer)
		return ctrl.Result{}, r.Update(ctx, &rule)
	}
	if !controllerutil.ContainsFinalizer(&rule, egressRuleFinalizer) {
		controllerutil.AddFinalizer(&rule, egressRuleFinalizer)
		if err := r.Update(ctx, &rule); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if rule.Spec.Desired == oramav1alpha1.EgressRuleDesiredAbsent {
		if err := r.remove(ctx, &rule); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.updateStatus(ctx, &rule, oramav1alpha1.EgressRulePhaseDeleted, "egress allowances deleted", metav1.ConditionFalse, "Deleted")
	}
	if err := validateEgressRuleSpec(&rule.Spec); err != nil {
		return ctrl.Result{}, r.updateStatus(ctx, &rule, oramav1alpha1.EgressRulePhaseFailed, err.Error(), metav1.ConditionFalse, "Invalid")
	}
	return r.reconcilePresent(ctx, &rule)
}

func (r *EgressRuleReconciler) reconcilePresent(ctx context.Context, rule *oramav1alpha1.EgressRule) (ctrl.Result, error) {
	sandbox, err := lakeWorkspaceSandbox(ctx, r.Client, rule.Spec.LakeRef, rule, "EgressRule")
	if err != nil {
		return r.pendingOrFailed(ctx, rule, "WaitingForSandbox", err)
	}
	source := metav1.LabelSelector{}
	if rule.Spec.SourceKind != "" {
		source, err = lakeWorkloadSelector(ctx, r.Client, rule.Spec.LakeRef, rule.Namespace, rule.Spec.SourceKind, rule.Spec.SourceID)
		if err != nil {
			return r.pendingOrFailed(ctx, rule, "SourceNotReady", err)
		}
	}
	allowances, err := egressRuleAllowances(ctx, r.Client, rule, source)
	if err != nil {
		var invalid egressRuleInvalidError
		if errors.As(err, &invalid) {
			return ctrl.Result{}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseFailed, err.Error(), metav1.ConditionFalse, "Invalid")
		}
		return ctrl.Result{}, err
	}
	sandbox, err = r.replaceAllowances(ctx, sandbox, rule, allowances)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, _, err := reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	if rule.Spec.DNSName == "" {
		return ctrl.Result{RequeueAfter: egressRuleRefresh}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseReady, "egress policy ready", metav1.ConditionTrue, "PolicyReady")
	}
	if rule.Spec.RuleID == "" {
		return ctrl.Result{}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseFailed, "a DNS-name rule needs a Lake rule id", metav1.ConditionFalse, "Invalid")
	}
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: rule.Spec.LakeRef}, &lake); err != nil {
		return ctrl.Result{}, err
	}
	state, err := reconcileEgressGateway(ctx, r.Client, &lake, rule.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if problem := state.Problems[rule.Name]; problem != "" {
		return ctrl.Result{RequeueAfter: egressRuleRefresh}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseFailed, problem, metav1.ConditionFalse, "Conflict")
	}
	if !state.Ready {
		return ctrl.Result{RequeueAfter: connectionCredentialRequeue}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhasePending,
			"waiting for the workspace egress gateway", metav1.ConditionFalse, "GatewayNotReady")
	}
	message := fmt.Sprintf("%s is reachable through %s (TLS on 443, or the ZTEG1 preface on %d)",
		rule.Spec.DNSName, egressGatewayAddress(rule.Namespace), egressGatewayPort)
	return ctrl.Result{RequeueAfter: egressRuleRefresh}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseReady, message, metav1.ConditionTrue, "GatewayReady")
}

func (r *EgressRuleReconciler) pendingOrFailed(ctx context.Context, rule *oramav1alpha1.EgressRule, reason string, err error) (ctrl.Result, error) {
	var pending connectionPendingError
	if errors.As(err, &pending) {
		return ctrl.Result{RequeueAfter: connectionPendingRequeue},
			r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhasePending, pending.Error(), metav1.ConditionFalse, reason)
	}
	return ctrl.Result{}, r.updateStatus(ctx, rule, oramav1alpha1.EgressRulePhaseFailed, err.Error(), metav1.ConditionFalse, "Invalid")
}

// remove takes the rule's allowances out of the sandbox and, for a name rule, re-renders the
// gateway snapshot without it, so its sessions are closed.
func (r *EgressRuleReconciler) remove(ctx context.Context, rule *oramav1alpha1.EgressRule) error {
	var sandbox oramav1alpha1.NetworkSandbox
	key := client.ObjectKey{Namespace: rule.Namespace, Name: networkSandboxName}
	if err := r.Get(ctx, key, &sandbox); err == nil {
		updated, err := r.replaceAllowances(ctx, &sandbox, rule, nil)
		if err != nil {
			return err
		}
		if _, _, err := reconcileNetworkSandboxPolicies(ctx, r.Client, r.Scheme, updated); err != nil {
			return err
		}
	} else if client.IgnoreNotFound(err) != nil {
		return err
	}
	if rule.Spec.DNSName == "" {
		return nil
	}
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: rule.Spec.LakeRef}, &lake); err != nil {
		return client.IgnoreNotFound(err)
	}
	// The rule is absent or deleting, so the snapshot no longer carries it.
	_, err := reconcileEgressGateway(ctx, r.Client, &lake, rule.Namespace)
	return err
}

func (r *EgressRuleReconciler) replaceAllowances(
	ctx context.Context,
	sandbox *oramav1alpha1.NetworkSandbox,
	rule *oramav1alpha1.EgressRule,
	allowances []oramav1alpha1.NetworkSandboxPolicy,
) (*oramav1alpha1.NetworkSandbox, error) {
	owned := map[string]bool{}
	for _, name := range egressRuleAllowanceNames(rule) {
		owned[name] = true
	}
	kept := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(sandbox.Spec.Policies)+len(allowances))
	for _, policy := range sandbox.Spec.Policies {
		if !owned[policy.Name] {
			kept = append(kept, policy)
		}
	}
	kept = append(kept, allowances...)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if apiequality.Semantic.DeepEqual(sandbox.Spec.Policies, kept) {
		return sandbox, nil
	}
	sandbox.Spec.Policies = kept
	if err := r.Update(ctx, sandbox); err != nil {
		return nil, fmt.Errorf("update NetworkSandbox egress allowances: %w", err)
	}
	return sandbox, nil
}

func (r *EgressRuleReconciler) updateStatus(
	ctx context.Context,
	rule *oramav1alpha1.EgressRule,
	phase, message string,
	conditionStatus metav1.ConditionStatus,
	reason string,
) error {
	desired := oramav1alpha1.EgressRuleStatus{
		Phase:              phase,
		Message:            message,
		ObservedGeneration: rule.Spec.Generation,
		Conditions:         append([]metav1.Condition(nil), rule.Status.Conditions...),
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rule.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if apiequality.Semantic.DeepEqual(rule.Status, desired) {
		return nil
	}
	rule.Status = desired
	return r.Status().Update(ctx, rule)
}

// rulesInNamespace enqueues every EgressRule in the object's namespace.
func (r *EgressRuleReconciler) rulesInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var rules oramav1alpha1.EgressRuleList
	if err := r.List(ctx, &rules, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(rules.Items))
	for i := range rules.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&rules.Items[i])})
	}
	return requests
}

// nameRulesForPod enqueues the namespace's DNS-name rules when a pod's address may have changed,
// because the gateway snapshot lists source addresses.
func (r *EgressRuleReconciler) nameRulesForPod(ctx context.Context, object client.Object) []reconcile.Request {
	var rules oramav1alpha1.EgressRuleList
	if err := r.List(ctx, &rules, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range rules.Items {
		if rules.Items[i].Spec.DNSName != "" {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&rules.Items[i])})
			// One rule re-renders the whole snapshot.
			break
		}
	}
	return requests
}

func (r *EgressRuleReconciler) rulesForApp(ctx context.Context, object client.Object) []reconcile.Request {
	app, ok := object.(*oramav1alpha1.LakeApp)
	if !ok || app.Spec.Sync == nil || app.Spec.Sync.WorkspaceNamespace == "" {
		return nil
	}
	shadow := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: app.Spec.Sync.WorkspaceNamespace}}
	return r.rulesInNamespace(ctx, shadow)
}

func (r *EgressRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.EgressRule{}).
		Watches(&oramav1alpha1.NetworkSandbox{}, handler.EnqueueRequestsFromMapFunc(r.rulesInNamespace)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.nameRulesForPod)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.nameRulesForPod)).
		Watches(&oramav1alpha1.LakeApp{}, handler.EnqueueRequestsFromMapFunc(r.rulesForApp)).
		Complete(r)
}
