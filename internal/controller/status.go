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

const componentLabel = "app.kubernetes.io/component"

// componentReadiness returns a per-component readiness list for the operator-owned workloads —
// one entry per DEPLOYABLE component (Deployment / StatefulSet, keyed by its component label) plus
// the CNPG control DB. Middle granularity: component-level, not per-pod. Sorted by name so the
// list is stable and never triggers a status write just from map/list ordering.
func (r *OramaCloudV1Reconciler) componentReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) []oramav1alpha1.ComponentStatus {
	owned := client.MatchingLabels{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"app.kubernetes.io/instance":   oc.Name,
	}
	ns := client.InNamespace(oc.Namespace)
	var comps []oramav1alpha1.ComponentStatus

	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, ns, owned); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			want := replicasOf(d.Spec.Replicas)
			comps = append(comps, oramav1alpha1.ComponentStatus{
				Name:   componentName(d.Labels, d.Name),
				Ready:  want > 0 && d.Status.ReadyReplicas >= want,
				Detail: fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, want),
			})
		}
	}

	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts, ns, owned); err == nil {
		for i := range sts.Items {
			s := &sts.Items[i]
			want := replicasOf(s.Spec.Replicas)
			comps = append(comps, oramav1alpha1.ComponentStatus{
				Name:   componentName(s.Labels, s.Name),
				Ready:  want > 0 && s.Status.ReadyReplicas >= want,
				Detail: fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, want),
			})
		}
	}

	// CNPG control DB (unstructured Cluster). Best-effort — not-found counts as not-ready.
	if oc.Spec.Components.ControlDB.Enabled {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(cnpgClusterGVK)
		key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb"}
		ready, detail := false, "0"
		if err := r.Get(ctx, key, cluster); err == nil {
			ri, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
			inst, _, _ := unstructured.NestedInt64(cluster.Object, "status", "instances")
			ready, detail = ri >= 1, fmt.Sprintf("%d/%d", ri, inst)
		}
		comps = append(comps, oramav1alpha1.ComponentStatus{Name: "controldb", Ready: ready, Detail: detail})
	}

	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps
}

func componentName(labels map[string]string, fallback string) string {
	if c := labels[componentLabel]; c != "" {
		return c
	}
	return fallback
}

func replicasOf(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// applyReadiness rolls the per-component states up into the CR status: phase (Ready when every
// component is ready; Degraded when one regressed after having been Ready; else Progressing),
// componentsReady ("n/total"), the per-component list, and a standard Ready condition. Returns
// whether the stack is fully ready. Writes the status subresource ONLY on a real transition.
func (r *OramaCloudV1Reconciler) applyReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) bool {
	comps := r.componentReadiness(ctx, oc)
	ready, notReady := 0, []string(nil)
	for _, c := range comps {
		if c.Ready {
			ready++
		} else {
			notReady = append(notReady, c.Name)
		}
	}
	total := len(comps)
	allReady := ready == total // total==0 (nothing enabled) is vacuously ready

	// Progressing (coming up) vs Degraded (was Ready, a component regressed) — decided from the
	// existing Ready condition, so no extra state and it only flips on a genuine transition.
	wasReady := meta.IsStatusConditionTrue(oc.Status.Conditions, "Ready")
	phase, condStatus, reason := "Progressing", metav1.ConditionFalse, "ComponentsProgressing"
	msg := fmt.Sprintf("%d/%d ready; waiting on: %s", ready, total, strings.Join(notReady, ", "))
	switch {
	case allReady:
		phase, condStatus, reason = "Ready", metav1.ConditionTrue, "AllComponentsReady"
		msg = fmt.Sprintf("all %d components ready", total)
	case wasReady:
		phase, reason = "Degraded", "ComponentsDegraded"
		msg = fmt.Sprintf("%d/%d ready; degraded: %s", ready, total, strings.Join(notReady, ", "))
	}

	r.setStatus(ctx, oc, phase, fmt.Sprintf("%d/%d", ready, total), comps, metav1.Condition{
		Type: "Ready", Status: condStatus, Reason: reason, Message: msg,
	})
	return allReady
}

// setStatus writes phase + componentsReady + the per-component list + a merged condition — but
// only when something actually changed, so a steady state produces NO control-plane traffic even
// though reconcile runs on every child event. This is the anti-flap guard.
func (r *OramaCloudV1Reconciler) setStatus(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, phase, componentsReady string, comps []oramav1alpha1.ComponentStatus, cond metav1.Condition) {
	cond.ObservedGeneration = oc.Generation
	changed := oc.Status.Phase != phase ||
		oc.Status.ComponentsReady != componentsReady ||
		oc.Status.ObservedGeneration != oc.Generation ||
		!componentsEqual(oc.Status.Components, comps)
	if meta.SetStatusCondition(&oc.Status.Conditions, cond) {
		changed = true
	}
	if !changed {
		return
	}
	oc.Status.Phase = phase
	oc.Status.ComponentsReady = componentsReady
	oc.Status.Components = comps
	oc.Status.ObservedGeneration = oc.Generation
	if err := r.Status().Update(ctx, oc); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "phase", phase)
	}
}

// componentsEqual reports whether two component lists are identical (both are name-sorted).
func componentsEqual(a, b []oramav1alpha1.ComponentStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
