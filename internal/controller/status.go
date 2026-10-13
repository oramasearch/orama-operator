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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
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

	// Workloads sharing a component label count as one component, their replicas summed. That is
	// normally one workload each; while the readers move from a Deployment to a StatefulSet (#141)
	// both exist, and the reader is Ready only once both are fully available.
	type replicaCount struct{ ready, want int32 }
	workloads := map[string]replicaCount{}
	add := func(name string, ready int32, want *int32) {
		c := workloads[name]
		c.ready += ready
		c.want += replicasOf(want)
		workloads[name] = c
	}
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, ns, owned); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			add(componentName(d.Labels, d.Name), d.Status.ReadyReplicas, d.Spec.Replicas)
		}
	}
	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts, ns, owned); err == nil {
		for i := range sts.Items {
			s := &sts.Items[i]
			add(componentName(s.Labels, s.Name), s.Status.ReadyReplicas, s.Spec.Replicas)
		}
	}
	for name, c := range workloads {
		comps = append(comps, oramav1alpha1.ComponentStatus{
			Name:   name,
			Ready:  c.want > 0 && c.ready >= c.want,
			Detail: fmt.Sprintf("%d/%d", c.ready, c.want),
		})
	}

	// CNPG control DB (unstructured Cluster). Best-effort — not-found counts as not-ready.
	if oc.Spec.Components.ControlDB.Enabled {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(cnpgClusterGVK)
		key := client.ObjectKey{Namespace: oc.Namespace, Name: controlDBClusterName(oc)}
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
// componentsReady ("n/total"), the per-component list, a standard Ready condition, and the engine
// snapshots' status and SnapshotsReady condition from this pass. Returns whether the stack is fully
// ready. Writes the status subresource ONLY on a real transition.
func (r *OramaCloudV1Reconciler) applyReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, snapshots engineSnapshots) bool {
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

	conditions := []metav1.Condition{{Type: "Ready", Status: condStatus, Reason: reason, Message: msg}}
	backupCondition, err := r.controlDBBackupCondition(ctx, oc)
	otherChanged := false // a removed condition
	switch {
	case err != nil:
		log.FromContext(ctx).Error(err, "read control DB backup status")
	case backupCondition != nil:
		conditions = append(conditions, *backupCondition)
	default: // backups off: drop a BackupReady left from when they were on
		otherChanged = meta.RemoveStatusCondition(&oc.Status.Conditions, "BackupReady")
	}
	// status.engineSnapshots and SnapshotsReady (#316); both dropped while snapshots are off.
	if !equality.Semantic.DeepEqual(snapshots.statuses, oc.Status.EngineSnapshots) {
		oc.Status.EngineSnapshots = snapshots.statuses
		otherChanged = true
	}
	if snapshots.condition != nil {
		conditions = append(conditions, *snapshots.condition)
	} else if meta.RemoveStatusCondition(&oc.Status.Conditions, engineSnapshotsConditionType) {
		otherChanged = true
	}
	r.setStatus(ctx, oc, phase, fmt.Sprintf("%d/%d", ready, total), comps, otherChanged, conditions...)
	return allReady
}

// controlDBBackupCondition is the BackupReady condition for spec.components.controlDB.backup: True
// once the ScheduledBackup exists and the newest CNPG Backup completed; False with the cause for a
// pending, failed or misconfigured backup. nil when backups are off, so the condition is dropped.
func (r *OramaCloudV1Reconciler) controlDBBackupCondition(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*metav1.Condition, error) {
	backup, err := r.resolveControlDBBackup(ctx, oc)
	if err != nil || !backup.Enabled {
		return nil, err
	}
	_, _, cond, err := controlDBBackupStatus(ctx, r.Client, oc.Namespace, controlDBClusterName(oc), controlDBScheduledBackupName(oc), oc.Generation, backup)
	if err != nil {
		return nil, err
	}
	return &cond, nil
}

// setStatus writes phase + componentsReady + the per-component list + the merged conditions — but
// only when something actually changed, so a steady state produces NO control-plane traffic even
// though reconcile runs on every child event. This is the anti-flap guard. otherChanged
// reports that the caller already changed oc.Status (removed a condition), which also needs a write.
func (r *OramaCloudV1Reconciler) setStatus(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, phase, componentsReady string, comps []oramav1alpha1.ComponentStatus, otherChanged bool, conds ...metav1.Condition) {
	changed := otherChanged || oc.Status.Phase != phase ||
		oc.Status.ComponentsReady != componentsReady ||
		oc.Status.ObservedGeneration != oc.Generation ||
		!componentsEqual(oc.Status.Components, comps)
	for _, cond := range conds {
		cond.ObservedGeneration = oc.Generation
		if meta.SetStatusCondition(&oc.Status.Conditions, cond) {
			changed = true
		}
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
