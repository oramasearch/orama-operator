package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Workspace isolation on the network is policy (operator#699): the NetworkSandbox renders
// CiliumNetworkPolicy and the workspace, app, build and environment controllers render Kubernetes
// NetworkPolicy. On a cluster whose dataplane does not enforce them those objects are accepted and
// isolate nothing. So before a Lake runs tenant code, the operator reads from the Kubernetes API
// that Cilium is there and running, and refuses tenant code where it is not. Nothing turns the
// check off: every cluster runs it, and a cluster passes it only by running Cilium.
const (
	lakeNetworkPolicyConditionType = "NetworkPolicyEnforced"

	// ciliumPolicyCRDName is the kind the NetworkSandbox renders. Without it no Cilium policy exists.
	ciliumPolicyCRDName = "ciliumnetworkpolicies.cilium.io"
	// ciliumAgentNamespace and ciliumAgentName are the Cilium agent DaemonSet, the process that
	// enforces policy on each node. AKS (Azure CNI powered by Cilium) and the upstream chart both
	// install it as kube-system/cilium; read on orama-production-cluster-01 on 2026-11-14.
	ciliumAgentNamespace = "kube-system"
	ciliumAgentName      = "cilium"

	reasonNetworkPolicyEnforced          = "CiliumEnforcing"
	reasonCiliumPolicyKindMissing        = "CiliumPolicyKindMissing"
	reasonCiliumAgentMissing             = "CiliumAgentMissing"
	reasonCiliumAgentNotReady            = "CiliumAgentNotReady"
	reasonNetworkPolicyCheckFailed       = "NetworkPolicyCheckFailed"
	reasonTenantNetworkPolicyNotEnforced = "NetworkPolicyNotEnforced"
)

// The check reads (cluster-wide, already in the chart ClusterRole):
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// networkPolicyEnforcement is the result of one check: whether the cluster enforces network policy,
// and the condition that says so.
type networkPolicyEnforcement struct {
	Enforced  bool
	Condition metav1.Condition
}

// networkPolicyEnforcementCheck reads whether the cluster enforces network policy.
type networkPolicyEnforcementCheck func(ctx context.Context, reader client.Reader, generation int64) networkPolicyEnforcement

// defaultNetworkPolicyEnforcementCheck is the check a LakeReconciler runs when its
// NetworkPolicyCheck is nil. Production always uses ciliumNetworkPolicyEnforcement; the package's
// tests replace this variable once (lake_network_policy_enforcement_fixture_test.go), because their
// fake clients hold no Cilium.
var defaultNetworkPolicyEnforcementCheck networkPolicyEnforcementCheck = ciliumNetworkPolicyEnforcement

// ciliumNetworkPolicyEnforcement passes when the CiliumNetworkPolicy kind is served and the Cilium
// agent DaemonSet schedules at least one pod and has at least one ready. It does not ask for every
// agent to be ready: while a new node joins, its agent starts after it, and AKS keeps tenant pods
// off such a node (the node.cilium.io/agent-not-ready taint) until the agent runs. Requiring
// ready == desired would stop every tenant workload on each scale-up.
func ciliumNetworkPolicyEnforcement(ctx context.Context, reader client.Reader, generation int64) networkPolicyEnforcement {
	refuse := func(reason, message string) networkPolicyEnforcement {
		return networkPolicyEnforcement{Condition: metav1.Condition{
			Type: lakeNetworkPolicyConditionType, Status: metav1.ConditionFalse, ObservedGeneration: generation,
			Reason: reason, Message: message + "; tenant workloads are not started or changed until Cilium enforces network policy",
		}}
	}
	crd := &metav1.PartialObjectMetadata{}
	crd.SetGroupVersionKind(crdGVK)
	if err := reader.Get(ctx, client.ObjectKey{Name: ciliumPolicyCRDName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return refuse(reasonCiliumPolicyKindMissing, fmt.Sprintf("the %s CustomResourceDefinition is not installed", ciliumPolicyCRDName))
		}
		return refuse(reasonNetworkPolicyCheckFailed, fmt.Sprintf("cannot read the %s CustomResourceDefinition: %v", ciliumPolicyCRDName, err))
	}
	var agent appsv1.DaemonSet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ciliumAgentNamespace, Name: ciliumAgentName}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return refuse(reasonCiliumAgentMissing, fmt.Sprintf("the Cilium agent DaemonSet %s/%s does not exist", ciliumAgentNamespace, ciliumAgentName))
		}
		return refuse(reasonNetworkPolicyCheckFailed, fmt.Sprintf("cannot read the Cilium agent DaemonSet %s/%s: %v", ciliumAgentNamespace, ciliumAgentName, err))
	}
	desired, ready := agent.Status.DesiredNumberScheduled, agent.Status.NumberReady
	if desired == 0 || ready == 0 {
		return refuse(reasonCiliumAgentNotReady, fmt.Sprintf("the Cilium agent DaemonSet %s/%s has %d of %d agents ready", ciliumAgentNamespace, ciliumAgentName, ready, desired))
	}
	return networkPolicyEnforcement{Enforced: true, Condition: metav1.Condition{
		Type: lakeNetworkPolicyConditionType, Status: metav1.ConditionTrue, ObservedGeneration: generation,
		Reason: reasonNetworkPolicyEnforced,
		Message: fmt.Sprintf("Cilium enforces network policy: %s is installed and %d of %d agents are ready",
			ciliumPolicyCRDName, ready, desired),
	}}
}

// tenantNetworkPolicyCacheTTL is how long one check's result is reused by the controllers that
// start tenant code. They reconcile often; the check is two API reads, and Cilium does not come
// and go within seconds.
const tenantNetworkPolicyCacheTTL = 30 * time.Second

var tenantNetworkPolicyCache struct {
	sync.Mutex
	at     time.Time
	result networkPolicyEnforcement
}

// tenantNetworkPolicyEnforcement is the check every controller that starts tenant code runs before
// it does (the Lake's control-row sync, LakeApps, LakeWorkspaces, LakeApp, LakeEnvironment). Each
// runs it itself rather than reading the Lake's condition, so a Lake whose status is not written
// yet starts nothing unchecked.
func tenantNetworkPolicyEnforcement(ctx context.Context, reader client.Reader) networkPolicyEnforcement {
	tenantNetworkPolicyCache.Lock()
	defer tenantNetworkPolicyCache.Unlock()
	if !tenantNetworkPolicyCache.at.IsZero() && time.Since(tenantNetworkPolicyCache.at) < tenantNetworkPolicyCacheTTL {
		return tenantNetworkPolicyCache.result
	}
	result := defaultNetworkPolicyEnforcementCheck(ctx, reader, 0)
	// A failed read is not cached, so the next reconcile asks again.
	if result.Condition.Reason != reasonNetworkPolicyCheckFailed {
		tenantNetworkPolicyCache.at, tenantNetworkPolicyCache.result = time.Now(), result
	}
	return result
}

// lakeNetworkPolicyEnforcement runs the Lake reconciler's check, with the Lake's generation on
// the condition.
func (r *LakeReconciler) lakeNetworkPolicyEnforcement(ctx context.Context, lake *oramav1alpha1.Lake) networkPolicyEnforcement {
	var result networkPolicyEnforcement
	if r.NetworkPolicyCheck != nil {
		result = r.NetworkPolicyCheck(ctx, lakeAPIReader(r), lake.Generation)
	} else {
		result = tenantNetworkPolicyEnforcement(ctx, lakeAPIReader(r))
	}
	result.Condition.ObservedGeneration = lake.Generation
	return result
}

// tenantWorkloadsRefused is the message a tenant-code controller reports when the check fails,
// or "" when it passes.
func tenantWorkloadsRefused(ctx context.Context, reader client.Reader, lake *oramav1alpha1.Lake) string {
	result := tenantNetworkPolicyEnforcement(ctx, reader)
	if result.Enforced {
		return ""
	}
	return fmt.Sprintf("Lake %q runs no tenant workloads: %s (%s)", lake.Name, result.Condition.Message, result.Condition.Reason)
}
