/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// componentReadiness lists the operator-owned workloads for this CR and reports how many are
// ready vs total, plus the names not yet ready. It covers Deployments + StatefulSets (the bulk of
// the stack) and the CNPG control DB (unstructured). The component reconcilers only CREATE
// resources; this roll-up over their LIVE status is what advances the CR phase from Provisioning
// to Ready (#37).
func (r *OramaCloudV1Reconciler) componentReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (ready, total int, notReady []string) {
	owned := client.MatchingLabels{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"app.kubernetes.io/instance":   oc.Name,
	}
	ns := client.InNamespace(oc.Namespace)

	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, ns, owned); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			total++
			if want := replicasOf(d.Spec.Replicas); want > 0 && d.Status.ReadyReplicas >= want {
				ready++
			} else {
				notReady = append(notReady, "deploy/"+d.Name)
			}
		}
	}

	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts, ns, owned); err == nil {
		for i := range sts.Items {
			s := &sts.Items[i]
			total++
			if want := replicasOf(s.Spec.Replicas); want > 0 && s.Status.ReadyReplicas >= want {
				ready++
			} else {
				notReady = append(notReady, "sts/"+s.Name)
			}
		}
	}

	// CNPG control DB (unstructured Cluster). Best-effort — if CNPG isn't installed or the cluster
	// isn't found yet, it counts as not-ready rather than failing the roll-up.
	if oc.Spec.Components.ControlDB.Enabled {
		total++
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(cnpgClusterGVK)
		key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb"}
		if err := r.Get(ctx, key, cluster); err == nil && cnpgClusterReady(cluster) {
			ready++
		} else {
			notReady = append(notReady, "cnpg/"+oc.Name+"-controldb")
		}
	}

	sort.Strings(notReady)
	return ready, total, notReady
}

func replicasOf(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// cnpgClusterReady reports whether a CNPG Cluster has at least one ready instance.
func cnpgClusterReady(c *unstructured.Unstructured) bool {
	readyInstances, found, err := unstructured.NestedInt64(c.Object, "status", "readyInstances")
	return err == nil && found && readyInstances >= 1
}

// applyReadiness computes the readiness roll-up and updates the CR status: phase (Ready once every
// owned workload is ready, else Provisioning), componentsReady ("n/total"), and a standard Ready
// condition. Returns whether the stack is fully ready.
func (r *OramaCloudV1Reconciler) applyReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) bool {
	ready, total, notReady := r.componentReadiness(ctx, oc)
	allReady := ready == total // total==0 (nothing enabled) is vacuously ready

	phase, condStatus, reason := "Provisioning", metav1.ConditionFalse, "ComponentsNotReady"
	msg := fmt.Sprintf("%d/%d components ready; waiting on: %s", ready, total, strings.Join(notReady, ", "))
	if allReady {
		phase, condStatus, reason = "Ready", metav1.ConditionTrue, "AllComponentsReady"
		msg = fmt.Sprintf("all %d components ready", total)
	}

	r.setStatus(ctx, oc, phase, fmt.Sprintf("%d/%d", ready, total), metav1.Condition{
		Type: "Ready", Status: condStatus, Reason: reason, Message: msg,
	})
	return allReady
}

// setStatus best-effort updates phase + componentsReady and merges a condition, writing the
// status subresource only when something actually changed.
func (r *OramaCloudV1Reconciler) setStatus(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, phase, componentsReady string, cond metav1.Condition) {
	cond.ObservedGeneration = oc.Generation
	changed := oc.Status.Phase != phase ||
		oc.Status.ComponentsReady != componentsReady ||
		oc.Status.ObservedGeneration != oc.Generation
	if meta.SetStatusCondition(&oc.Status.Conditions, cond) {
		changed = true
	}
	if !changed {
		return
	}
	oc.Status.Phase = phase
	oc.Status.ComponentsReady = componentsReady
	oc.Status.ObservedGeneration = oc.Generation
	if err := r.Status().Update(ctx, oc); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "phase", phase)
	}
}
