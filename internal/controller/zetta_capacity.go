/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// Elastic capacity — the first slice of operator#69 (Zetta → operator → Karpenter → nodes → more Zetta
// nodes). When spec.capacity.karpenter.enabled, the operator OWNS a Karpenter NodePool and pins the
// Zetta pods onto it, so raising spec.replicas past current node capacity leaves pods Pending →
// Karpenter provisions matching nodes just-in-time → the new pods schedule and join the CRDT cluster.
//
// The NodePool is referenced via `unstructured` (like the CNPG Cluster in controldb.go) so the operator
// stays decoupled from Karpenter's Go types/version, and so a cluster without Karpenter installed is a
// graceful no-op rather than a build/runtime coupling. Karpenter model (see the vendored
// docs/research/karpenter-manual.md): NodePool → provider NodeClass → NodeClaim → Node.
const (
	// zettaCapacityTaintKey taints Zetta nodes so ONLY tolerating pods (the Zetta nodes) land there —
	// the pool is dedicated to Zetta, per the manual's isolated-pool recipe.
	zettaCapacityTaintKey = "orama.cloud/zetta"
	// zettaPoolLabel is stamped on the NodePool's node template (→ every provisioned node) and used as
	// the Zetta pods' nodeSelector, so the pods schedule only onto this pool's nodes.
	zettaPoolLabel = "orama.cloud/zetta-pool"
	// karpenterNodePoolLabel is the well-known label Karpenter puts on every node it provisions from a
	// NodePool — the authoritative key for counting this cluster's provisioned nodes.
	karpenterNodePoolLabel = "karpenter.sh/nodepool"
	// doNotDisruptAnnotation, on a Zetta pod, tells Karpenter's disruption controller never to
	// voluntarily evict it (consolidation/drift) — quorum-safety for a stateful CRDT peer. Operator-driven
	// drain-safe scale-down is a later slice of operator#69.
	doNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	// capacityFinalizer guards deletion of the CLUSTER-SCOPED NodePool. A namespaced CR cannot own a
	// cluster-scoped object via ownerReferences (k8s GC would treat the owner as absent and delete it), so
	// the NodePool is label-identified and cleaned up here on capacity-disable or CR delete.
	capacityFinalizer = "orama.cloud/karpenter-capacity"

	capacityConditionType = "CapacityReady"
)

// karpenterNodePoolGVK is the Karpenter NodePool CRD (generic, cloud-neutral). Provisioned as an
// unstructured object so the operator has no compile-time dependency on Karpenter's types.
var karpenterNodePoolGVK = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}

// capacityEnabled reports whether operator-managed Karpenter capacity is requested.
func capacityEnabled(z *oramav1alpha1.OramaZettaCluster) bool {
	return z.Spec.Capacity != nil && z.Spec.Capacity.Karpenter != nil && z.Spec.Capacity.Karpenter.Enabled
}

// zettaNodePoolName is the deterministic, cluster-scoped NodePool name for a cluster. It is namespace +
// name qualified because NodePools share one cluster-wide namespace-less space across every operator CR.
func zettaNodePoolName(z *oramav1alpha1.OramaZettaCluster) string {
	return z.Namespace + "-" + z.Name + "-zetta"
}

// reconcileZettaCapacity is the elastic-capacity step. It returns the CapacityReady condition to fold
// into the CR status (nil ⇒ capacity is off, drop the condition) and the count of provisioned nodes.
//
// It also owns the capacity finalizer: added while capacity is on (so the cluster-scoped NodePool is
// cleaned up on delete), removed once the NodePool is gone. Deletion is handled by the caller
// (finalizeZettaCapacity) before the CR's owned children are GC'd.
func (r *OramaZettaClusterReconciler) reconcileZettaCapacity(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (*metav1.Condition, int32, error) {
	log := logf.FromContext(ctx)

	if !capacityEnabled(z) {
		// Disabled (or cleared). The NodePool/PDB only ever exist while the finalizer is present, so a
		// cluster that never used capacity does ZERO capacity API calls here (anti-flap). If the
		// finalizer IS present, capacity was on: tear down and release it.
		if controllerutil.ContainsFinalizer(z, capacityFinalizer) {
			if err := r.teardownZettaCapacity(ctx, z); err != nil {
				return nil, 0, err
			}
			controllerutil.RemoveFinalizer(z, capacityFinalizer)
			if err := r.Update(ctx, z); err != nil {
				return nil, 0, err
			}
		}
		return nil, 0, nil // no condition — a cluster that never uses capacity keeps a clean status
	}

	kc := z.Spec.Capacity.Karpenter

	// Guard: a NodePool needs a provider NodeClass to launch nodes; we can't invent cloud specifics.
	if kc.NodeClassRef.Name == "" {
		return blockedCondition("Blocked",
			"spec.capacity.karpenter.nodeClassRef.name is required — reference the platform's EC2NodeClass (EKS) or AKSNodeClass (AKS)"), 0, nil
	}

	// Detect Karpenter WITHOUT starting an informer for a possibly-absent CRD: ask the RESTMapper. If
	// the NodePool kind has no REST mapping, Karpenter (or AKS NAP) isn't installed — no-op, don't crash.
	if _, err := r.RESTMapper().RESTMapping(karpenterNodePoolGVK.GroupKind(), karpenterNodePoolGVK.Version); err != nil {
		if apimeta.IsNoMatchError(err) {
			log.Info("elastic capacity requested but Karpenter is not installed — no-op",
				"nodePoolGVK", karpenterNodePoolGVK.String())
			return blockedCondition("KarpenterNotInstalled",
				"Karpenter NodePool CRD (karpenter.sh/v1) not found — install Karpenter (EKS) or enable Node Auto Provisioning (AKS)"), 0, nil
		}
		return nil, 0, err
	}

	// Ensure the finalizer BEFORE creating the cluster-scoped NodePool, so a delete that races the
	// create still cleans the pool up.
	if !controllerutil.ContainsFinalizer(z, capacityFinalizer) {
		controllerutil.AddFinalizer(z, capacityFinalizer)
		if err := r.Update(ctx, z); err != nil {
			return nil, 0, err
		}
	}

	poolReady, err := r.reconcileZettaNodePool(ctx, z)
	if err != nil {
		return nil, 0, err
	}
	if err := r.reconcileZettaCapacityPDB(ctx, z); err != nil {
		return nil, 0, err
	}

	provisioned := r.zettaProvisionedNodes(ctx, z)

	if poolReady {
		return &metav1.Condition{
			Type: capacityConditionType, Status: metav1.ConditionTrue, Reason: "NodePoolReady",
			Message: fmt.Sprintf("Karpenter NodePool %s ready; %d node(s) provisioned", zettaNodePoolName(z), provisioned),
		}, provisioned, nil
	}
	return &metav1.Condition{
		Type: capacityConditionType, Status: metav1.ConditionFalse, Reason: "NodePoolProgressing",
		Message: fmt.Sprintf("Karpenter NodePool %s not yet ready", zettaNodePoolName(z)),
	}, provisioned, nil
}

func blockedCondition(reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: capacityConditionType, Status: metav1.ConditionFalse, Reason: reason, Message: msg}
}

// reconcileZettaNodePool ensures the operator-owned Karpenter NodePool exists and matches spec. It is
// created via CreateOrUpdate so the write is a no-op when nothing changed (anti-flap — principle #7).
// Returns whether the NodePool reports Ready. No ownerReference: the NodePool is cluster-scoped and a
// namespaced CR cannot own it (see capacityFinalizer); it is identified by labels + cleaned up by the
// finalizer instead.
func (r *OramaZettaClusterReconciler) reconcileZettaNodePool(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (bool, error) {
	kc := z.Spec.Capacity.Karpenter
	name := zettaNodePoolName(z)

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(karpenterNodePoolGVK)
	pool.SetName(name)

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pool, func() error {
		// MERGE our labels into whatever is already there (don't clobber Karpenter-added metadata) — a
		// wholesale SetLabels would drop server labels and flap the object (anti-flap, principle #7).
		labels := pool.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["app.kubernetes.io/name"] = "orama-zetta"
		labels["app.kubernetes.io/instance"] = z.Name
		labels["app.kubernetes.io/component"] = "zetta-capacity"
		labels["app.kubernetes.io/managed-by"] = "orama-cloud-v1-operator"
		// Owner identity (the NodePool has no ownerReference; these are how the finalizer/teardown find
		// the pool that belongs to this CR).
		labels["orama.cloud/owner-namespace"] = z.Namespace
		labels["orama.cloud/owner-name"] = z.Name
		pool.SetLabels(labels)

		// Node-template requirements: OS + arch + capacity type, plus optional instance-type pinning.
		// All keys are cloud-neutral well-known labels, so the same NodePool works on EKS and AKS.
		reqs := []interface{}{
			requirement("kubernetes.io/os", "In", "linux"),
			requirement("kubernetes.io/arch", "In", defaultStr(kc.Arch, "amd64")),
			requirement("karpenter.sh/capacity-type", "In", defaultStr(kc.CapacityType, "on-demand")),
		}
		if len(kc.InstanceTypes) > 0 {
			reqs = append(reqs, requirement("node.kubernetes.io/instance-type", "In", kc.InstanceTypes...))
		}

		template := map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{
					zettaPoolLabel:                 z.Name,
					"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
				},
			},
			"spec": map[string]interface{}{
				"nodeClassRef": map[string]interface{}{
					"group": defaultStr(kc.NodeClassRef.Group, "karpenter.k8s.aws"),
					"kind":  defaultStr(kc.NodeClassRef.Kind, "EC2NodeClass"),
					"name":  kc.NodeClassRef.Name,
				},
				"taints": []interface{}{
					map[string]interface{}{
						"key":    zettaCapacityTaintKey,
						"value":  z.Name,
						"effect": "NoSchedule",
					},
				},
				// Stateful CRDT nodes must not be force-cycled by node expiry.
				"expireAfter":            "Never",
				"terminationGracePeriod": "1h",
				"requirements":           reqs,
			},
		}

		// MERGE our managed keys into the existing spec so any server-side defaults we don't set (e.g.
		// spec.weight) survive and the object doesn't flap between our write and the API server's
		// defaulting. We fully own template/limits/disruption (every subkey Karpenter cares about is set).
		spec, _, _ := unstructured.NestedMap(pool.Object, "spec")
		if spec == nil {
			spec = map[string]interface{}{}
		}
		spec["template"] = template
		// Cost/scale guardrail — Karpenter stops provisioning once any cap is hit.
		spec["limits"] = map[string]interface{}{
			"cpu":    defaultStr(kc.Limits.CPU, "16"),
			"memory": defaultStr(kc.Limits.Memory, "64Gi"),
			"nodes":  fmt.Sprintf("%d", capacityNodeLimit(kc.Limits.Nodes)),
		}
		// Quorum-safe disruption: reclaim ONLY genuinely empty nodes (never evict a live peer for
		// underutilization), and cap churn to one node at a time.
		spec["disruption"] = map[string]interface{}{
			"consolidationPolicy": "WhenEmpty",
			"consolidateAfter":    "5m",
			"budgets": []interface{}{
				map[string]interface{}{"nodes": "1"},
			},
		}
		pool.Object["spec"] = spec
		return nil
	}); err != nil {
		return false, err
	}

	// Ready = the NodePool's own Ready condition is True (Karpenter validated the NodeClass etc.).
	return nodePoolReady(pool), nil
}

// requirement builds a Karpenter NodeSelectorRequirement entry.
func requirement(key, op string, values ...string) map[string]interface{} {
	vals := make([]interface{}, 0, len(values))
	for _, v := range values {
		vals = append(vals, v)
	}
	return map[string]interface{}{"key": key, "operator": op, "values": vals}
}

// nodePoolReady reads the NodePool's status.conditions for a Ready=True entry.
func nodePoolReady(pool *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(pool.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t == "Ready" {
			s, _, _ := unstructured.NestedString(cm, "status")
			return s == "True"
		}
	}
	return false
}

// reconcileZettaCapacityPDB protects the Zetta StatefulSet from concurrent voluntary disruptions during
// node churn — at most one Zetta peer down at a time. Namespaced, so it is owner-referenced (GC'd with
// the CR); combined with the pods' do-not-disrupt annotation this keeps scale/consolidation quorum-safe.
func (r *OramaZettaClusterReconciler) reconcileZettaCapacityPDB(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	lbls := zettaLabels(z)
	maxUnavailable := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: z.Name + "-zetta", Namespace: z.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = lbls
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		return controllerutil.SetControllerReference(z, pdb, r.Scheme)
	})
	return err
}

// zettaProvisionedNodes counts k8s nodes Karpenter has provisioned from this cluster's NodePool. Uses
// the uncached APIReader — a one-shot list on the periodic requeue — so the operator doesn't hold a
// cluster-wide Node informer just for a status count.
func (r *OramaZettaClusterReconciler) zettaProvisionedNodes(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) int32 {
	reader := r.reader()
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels{karpenterNodePoolLabel: zettaNodePoolName(z)}); err != nil {
		logf.FromContext(ctx).V(1).Info("could not list provisioned nodes for capacity status", "err", err.Error())
		return 0
	}
	return int32(len(nodes.Items))
}

// teardownZettaCapacity removes the operator-owned capacity resources — the cluster-scoped NodePool
// (tolerating a missing Karpenter CRD or an already-gone pool) and the PDB. Called on capacity-disable
// and on CR deletion (finalizeZettaCapacity).
func (r *OramaZettaClusterReconciler) teardownZettaCapacity(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(karpenterNodePoolGVK)
	pool.SetName(zettaNodePoolName(z))
	if err := r.Delete(ctx, pool); err != nil && !apimeta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return err
	}
	// The PDB is owner-referenced (GC'd with the CR), but on a plain disable we prune it eagerly.
	return deleteIfExists(ctx, r.Client, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: z.Name + "-zetta", Namespace: z.Namespace}})
}

// finalizeZettaCapacity is the delete-path cleanup: drop the cluster-scoped NodePool then release the
// finalizer so the CR (and its owner-GC'd children) can be removed.
func (r *OramaZettaClusterReconciler) finalizeZettaCapacity(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	if !controllerutil.ContainsFinalizer(z, capacityFinalizer) {
		return nil
	}
	if err := r.teardownZettaCapacity(ctx, z); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(z, capacityFinalizer)
	return r.Update(ctx, z)
}

// capacityNodeLimit clamps the node cap to a sane floor (never 0, which would forbid all provisioning).
func capacityNodeLimit(n int32) int32 {
	if n < 1 {
		return 3
	}
	return n
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// reader returns the uncached APIReader when wired (in-cluster), else the cached client (make run/tests).
func (r *OramaZettaClusterReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
