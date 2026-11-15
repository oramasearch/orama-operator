/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A LakeSet's restore rehearsals (orama-operator#127). Every file in config/clusters is applied to
// the managing cluster, but a LakeRestoreRehearsal runs on the cluster of the Lake it rehearses,
// next to its backups and snapshots. So the set carries the rehearsal: each declared entry is
// applied as a cluster-scoped LakeRestoreRehearsal on its member's cluster with spec.lakeRef set
// to the member's Lake, and its result is read back into the set's status. One removed from the
// set is deleted there; its controller deletes whatever scratch resources it still has.

var lakeRehearsalKind = oramav1alpha1.GroupVersion.WithKind("LakeRestoreRehearsal")

const lakeRehearsalCRD = "lakerestorerehearsals.orama.cloud"

// setRehearsals reconciles one LakeSet's restore rehearsals during one pass of the set.
type setRehearsals struct {
	set      *oramav1alpha1.LakeSet
	observed *oramav1alpha1.LakeSetStatus // as read, so status is written only on a change

	removing []oramav1alpha1.SetRestoreRehearsalStatus // removed from the set and not gone yet
	problems []string                                  // what the set could not do, for its message
	err      error                                     // the first failure, returned so it is retried
	busy     bool                                      // a rehearsal is still moving: look again soon
}

func (e *setRehearsals) refuse(entry *oramav1alpha1.SetRestoreRehearsalStatus, message string) {
	if entry.Cluster == "" || entry.Phase == "" {
		entry.Phase = e2ePhaseBlocked
	}
	entry.Message = message
	e.problems = append(e.problems, fmt.Sprintf("restore rehearsal %s of %s: %s", entry.Name, entry.Member, message))
}

func (e *setRehearsals) fail(entry *oramav1alpha1.SetRestoreRehearsalStatus, err error) {
	e.refuse(entry, err.Error())
	if e.err == nil {
		e.err = fmt.Errorf("restore rehearsal %s of %s: %w", entry.Name, entry.Member, err)
	}
}

func (e *setRehearsals) wait(entry *oramav1alpha1.SetRestoreRehearsalStatus, message string) {
	if entry.Cluster == "" {
		entry.Phase = e2ePhasePending
	}
	entry.Message = message
	e.busy = true
}

// restore puts the rehearsals' status as it was read into base, a copy of the set.
func (e *setRehearsals) restore(base managedSet) {
	base.(*oramav1alpha1.LakeSet).Status.RestoreRehearsals = e.observed.RestoreRehearsals
}

func (e *setRehearsals) request(r *managedSetReconciler, name, namespace string) memberRequest {
	// Cluster-scoped: ObjectNamespace stays empty. Namespace is the member's, which exists already.
	return memberRequest{Kind: lakeRehearsalKind, CRD: lakeRehearsalCRD, Name: name, Namespace: namespace, Owner: r.owner(e.set)}
}

// removeUndeclared deletes the rehearsals the set applied and no longer declares, and keeps
// reporting each until it is gone.
func (e *setRehearsals) removeUndeclared(ctx context.Context, r *managedSetReconciler) {
	declared := map[string]bool{}
	for _, rehearsal := range e.set.Spec.RestoreRehearsals {
		declared[rehearsal.Name] = true
	}
	for _, entry := range e.observed.RestoreRehearsals {
		if entry.Cluster == "" || declared[entry.Name] {
			continue
		}
		if gone := e.delete(ctx, r, &entry, entry.Cluster); !gone {
			e.removing = append(e.removing, entry)
		}
	}
}

// removeAll deletes every rehearsal of a set being deleted. It returns what is still being
// removed; the set keeps its finalizer until nothing is.
func (e *setRehearsals) removeAll(ctx context.Context, r *managedSetReconciler) string {
	entries := slices.Clone(e.observed.RestoreRehearsals)
	// A declared rehearsal is deleted even when the set never recorded applying it: a pass may
	// have applied it and failed to write the status.
	for _, rehearsal := range e.set.Spec.RestoreRehearsals {
		if slices.ContainsFunc(entries, func(entry oramav1alpha1.SetRestoreRehearsalStatus) bool {
			return entry.Name == rehearsal.Name && entry.Cluster != ""
		}) {
			continue
		}
		for _, member := range e.set.Spec.Clusters {
			if member.Name == rehearsal.Member {
				entries = append(entries, oramav1alpha1.SetRestoreRehearsalStatus{Name: rehearsal.Name, Member: rehearsal.Member, Cluster: member.ClusterRef})
			}
		}
	}
	var present []oramav1alpha1.SetRestoreRehearsalStatus
	var waiting []string
	for _, entry := range entries {
		if entry.Cluster == "" {
			continue
		}
		if gone := e.delete(ctx, r, &entry, entry.Cluster); !gone {
			present = append(present, entry)
			waiting = append(waiting, fmt.Sprintf("%s on %s (%s)", entry.Name, entry.Cluster, entry.Message))
		}
	}
	e.set.Status.RestoreRehearsals = present
	return strings.Join(waiting, "; ")
}

// delete deletes one rehearsal on a cluster. It reports whether it is gone.
func (e *setRehearsals) delete(ctx context.Context, r *managedSetReconciler, entry *oramav1alpha1.SetRestoreRehearsalStatus, cluster string) bool {
	var kc oramav1alpha1.KubernetesCluster
	switch err := r.Get(ctx, types.NamespacedName{Name: cluster}, &kc); {
	case apierrors.IsNotFound(err):
		return true // the cluster is gone, and everything that was on it
	case err != nil:
		entry.Phase = e2ePhaseRemoving
		e.fail(entry, err)
		return false
	case !kc.DeletionTimestamp.IsZero():
		return true
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, cluster)
	if err != nil || waiting != "" {
		entry.Phase, entry.Message = e2ePhaseRemoving, waiting
		e.busy = true
		if err != nil {
			e.fail(entry, err)
		}
		return false
	}
	if err := r.applier.DeleteRehearsal(ctx, kubeconfig, e.request(r, entry.Name, "")); err != nil {
		entry.Phase = e2ePhaseRemoving
		e.fail(entry, err)
		return false
	}
	return true
}

// apply brings each declared rehearsal to what the set says, once its member has been applied
// (members and statuses are this pass's, index for index), and reports them with those being
// removed.
func (e *setRehearsals) apply(ctx context.Context, r *managedSetReconciler, members []resolvedMember, statuses []oramav1alpha1.SetMemberStatus) {
	var entries []oramav1alpha1.SetRestoreRehearsalStatus
	for _, rehearsal := range e.set.Spec.RestoreRehearsals {
		entry := oramav1alpha1.SetRestoreRehearsalStatus{}
		if i := slices.IndexFunc(e.observed.RestoreRehearsals, func(o oramav1alpha1.SetRestoreRehearsalStatus) bool { return o.Name == rehearsal.Name }); i >= 0 {
			entry = *e.observed.RestoreRehearsals[i].DeepCopy()
		}
		entry.Name, entry.Member = rehearsal.Name, rehearsal.Member
		if i := slices.IndexFunc(members, func(m resolvedMember) bool { return m.member.Name == rehearsal.Member }); i < 0 {
			e.refuse(&entry, "it names no member of this set")
		} else {
			e.applyOne(ctx, r, rehearsal, members[i], statuses[i], &entry)
		}
		switch entry.Phase {
		case e2ePhasePending, e2ePhaseRunning, e2ePhaseRemoving:
			e.busy = true
		}
		entries = append(entries, entry)
	}
	e.set.Status.RestoreRehearsals = append(entries, e.removing...)
}

func (e *setRehearsals) applyOne(ctx context.Context, r *managedSetReconciler, rehearsal oramav1alpha1.SetRestoreRehearsal, resolved resolvedMember, member oramav1alpha1.SetMemberStatus, entry *oramav1alpha1.SetRestoreRehearsalStatus) {
	cluster := resolved.member.ClusterRef
	spec, err := jsonObject(rehearsal.Spec.Raw, "spec.restoreRehearsals["+rehearsal.Name+"].spec")
	if err != nil {
		e.refuse(entry, err.Error())
		return
	}
	// The member's Lake is named after the member on its cluster.
	lakeName := resolved.member.Name
	if got, set := spec["lakeRef"]; set && got != lakeName {
		e.refuse(entry, fmt.Sprintf("spec.lakeRef is %v, but the set fills it in as %s, the member's Lake: remove it", got, lakeName))
		return
	}
	spec["lakeRef"] = lakeName
	// The member moved to another cluster: the rehearsal there is deleted before one is applied here.
	if entry.Cluster != "" && entry.Cluster != cluster {
		if gone := e.delete(ctx, r, entry, entry.Cluster); !gone {
			return
		}
		*entry = oramav1alpha1.SetRestoreRehearsalStatus{Name: entry.Name, Member: entry.Member}
	}
	if member.Phase != clusterPhaseReady && member.Phase != setPhaseProgressing {
		e.wait(entry, strings.TrimSuffix("waiting for its member to be applied: "+member.Message, ": "))
		return
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, cluster)
	if err != nil {
		e.fail(entry, err)
		return
	}
	if waiting != "" {
		e.wait(entry, waiting)
		return
	}
	req := e.request(r, rehearsal.Name, resolved.namespace)
	req.Spec = spec
	state, err := r.applier.ApplyRehearsal(ctx, kubeconfig, req)
	if state.MissingCRD {
		e.wait(entry, fmt.Sprintf("cluster %s does not have the %s kind (%s): its Orama operator predates it", cluster, lakeRehearsalKind.Kind, lakeRehearsalCRD))
		return
	}
	if state.Changed {
		// The rehearsal's spec is immutable; what the cluster has is reported as it is.
		entry.Cluster = cluster
		if state.Object != nil {
			observeRehearsal(state.Object, entry)
		}
		e.refuse(entry, "the rehearsal on the cluster has another spec, and a rehearsal's spec cannot change: add an entry under a new name")
		return
	}
	if err != nil {
		e.fail(entry, err)
		return
	}
	entry.Cluster = cluster
	if state.Object != nil {
		observeRehearsal(state.Object, entry)
	}
}

// observeRehearsal reads a LakeRestoreRehearsal's outcome as its cluster reports it.
func observeRehearsal(obj *unstructured.Unstructured, entry *oramav1alpha1.SetRestoreRehearsalStatus) {
	str := func(field string) string {
		value, _, _ := unstructured.NestedString(obj.Object, "status", field)
		return value
	}
	*entry = oramav1alpha1.SetRestoreRehearsalStatus{Name: entry.Name, Member: entry.Member, Cluster: entry.Cluster}
	entry.Phase, entry.Message, entry.Evidence = str("phase"), str("message"), str("evidence")
	if entry.Phase == "" {
		entry.Phase, entry.Message = e2ePhasePending, "the cluster's operator has not started the rehearsal yet"
	}
	entry.Selected = str("selectedSnapshot")
	if entry.Selected == "" {
		entry.Selected = str("selectedBackup")
	}
	if completed := str("completedAt"); completed != "" {
		var at metav1.Time
		if err := at.UnmarshalQueryParameter(completed); err == nil {
			entry.CompletedAt = &at
		}
	}
}

// rehearsalSpecChanged reports whether the rehearsal on the cluster was applied with another spec.
// Only the fields the set declares are compared: the API server fills in defaults (from: latest).
func rehearsalSpecChanged(existing *unstructured.Unstructured, desired map[string]any) bool {
	if existing == nil {
		return false
	}
	have, _, _ := unstructured.NestedMap(existing.Object, "spec")
	for key, value := range desired {
		if !reflect.DeepEqual(have[key], value) {
			return true
		}
	}
	return false
}

func (a clientMemberApplier) ApplyRehearsal(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	_, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return memberState{}, err
	}
	missing, err := missingCRDs(ctx, c, []string{req.CRD})
	if err != nil {
		return memberState{}, err
	}
	if len(missing) > 0 {
		return memberState{MissingCRD: true}, nil
	}
	existing := memberObject(req)
	switch err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); {
	case err == nil:
		if existing.GetLabels()[managedBySetLabel] == req.Owner && rehearsalSpecChanged(existing, req.Spec) {
			return memberState{Object: existing, Changed: true}, nil
		}
	case apierrors.IsNotFound(err):
	default:
		return memberState{}, fmt.Errorf("%s %s cannot be read on the cluster: %w", req.Kind.Kind, req.Name, err)
	}
	return applyMemberWith(ctx, c, req)
}

func (a clientMemberApplier) DeleteRehearsal(ctx context.Context, kubeconfig []byte, req memberRequest) error {
	return a.DeleteMember(ctx, kubeconfig, req)
}
