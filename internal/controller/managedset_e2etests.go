// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A LakeSet's end-to-end runs (orama-operator#589). Every file in config/clusters is applied to
// the managing cluster, but a LakeE2ETest runs on the cluster of the Lake it tests. So the set
// carries the test: each declared entry is applied as a cluster-scoped LakeE2ETest on its member's
// cluster, with spec.member naming this set and the member, and its result is read back into the
// set's status. One removed from the set is deleted there; its Job goes with it.

var lakeE2EKind = oramav1alpha1.GroupVersion.WithKind("LakeE2ETest")

const (
	lakeE2ECRD = "lakee2etests.orama.cloud"

	e2ePhasePending   = "Pending"
	e2ePhaseRunning   = "Running"
	e2ePhaseSucceeded = "Succeeded"
	e2ePhaseFailed    = "Failed"
	e2ePhaseBlocked   = "Blocked"
	e2ePhaseRemoving  = "Removing"
)

// setE2ETests reconciles one LakeSet's end-to-end runs during one pass of the set.
type setE2ETests struct {
	set      *oramav1alpha1.LakeSet
	observed *oramav1alpha1.LakeSetStatus // as read, so status is written only on a change

	removing []oramav1alpha1.SetE2ETestStatus // removed from the set and not gone yet
	problems []string                         // what the set could not do, for its message
	err      error                            // the first failure, returned so it is retried
	busy     bool                             // a run is still moving: look again soon
}

func (e *setE2ETests) refuse(entry *oramav1alpha1.SetE2ETestStatus, message string) {
	if entry.Cluster == "" {
		entry.Phase = e2ePhaseBlocked
	}
	entry.Message = message
	e.problems = append(e.problems, fmt.Sprintf("e2e test %s against %s: %s", entry.Name, entry.Member, message))
}

func (e *setE2ETests) fail(entry *oramav1alpha1.SetE2ETestStatus, err error) {
	e.refuse(entry, err.Error())
	if e.err == nil {
		e.err = fmt.Errorf("e2e test %s against %s: %w", entry.Name, entry.Member, err)
	}
}

func (e *setE2ETests) wait(entry *oramav1alpha1.SetE2ETestStatus, message string) {
	if entry.Cluster == "" {
		entry.Phase = e2ePhasePending
	}
	entry.Message = message
	e.busy = true
}

// restore puts the runs' status as it was read into base, a copy of the set.
func (e *setE2ETests) restore(base managedSet) {
	set := base.(*oramav1alpha1.LakeSet)
	set.Status.E2ETests, set.Status.E2ESummary = e.observed.E2ETests, e.observed.E2ESummary
}

func (e *setE2ETests) request(r *managedSetReconciler, name, cluster, namespace string) memberRequest {
	// Cluster-scoped: ObjectNamespace stays empty. Namespace is the member's, which exists already.
	return memberRequest{Kind: lakeE2EKind, CRD: lakeE2ECRD, Name: name, Namespace: namespace, Owner: r.owner(e.set)}
}

// removeUndeclared deletes the runs the set applied and no longer declares, and keeps reporting
// each until it is gone.
func (e *setE2ETests) removeUndeclared(ctx context.Context, r *managedSetReconciler) {
	declared := map[string]bool{}
	for _, test := range e.set.Spec.E2ETests {
		declared[test.Name] = true
	}
	for _, entry := range e.observed.E2ETests {
		if entry.Cluster == "" || declared[entry.Name] {
			continue
		}
		if gone := e.delete(ctx, r, &entry, entry.Cluster); !gone {
			e.removing = append(e.removing, entry)
		}
	}
}

// removeAll deletes every run of a set being deleted. It returns what is still being removed; the
// set keeps its finalizer until nothing is.
func (e *setE2ETests) removeAll(ctx context.Context, r *managedSetReconciler) string {
	entries := slices.Clone(e.observed.E2ETests)
	// A declared run is deleted even when the set never recorded applying it: a pass may have
	// applied it and failed to write the status.
	for _, test := range e.set.Spec.E2ETests {
		if slices.ContainsFunc(entries, func(entry oramav1alpha1.SetE2ETestStatus) bool { return entry.Name == test.Name && entry.Cluster != "" }) {
			continue
		}
		for _, member := range e.set.Spec.Clusters {
			if member.Name == test.Member {
				entries = append(entries, oramav1alpha1.SetE2ETestStatus{Name: test.Name, Member: test.Member, Cluster: member.ClusterRef})
			}
		}
	}
	var present []oramav1alpha1.SetE2ETestStatus
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
	e.set.Status.E2ETests, e.set.Status.E2ESummary = present, ""
	return strings.Join(waiting, "; ")
}

// delete deletes one run on a cluster. It reports whether it is gone.
func (e *setE2ETests) delete(ctx context.Context, r *managedSetReconciler, entry *oramav1alpha1.SetE2ETestStatus, cluster string) bool {
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
	if err := r.applier.DeleteE2ETest(ctx, kubeconfig, e.request(r, entry.Name, cluster, "")); err != nil {
		entry.Phase = e2ePhaseRemoving
		e.fail(entry, err)
		return false
	}
	return true
}

// apply brings each declared run to what the set says, once its member has been applied (members
// and statuses are this pass's, index for index), and reports them with those being removed.
func (e *setE2ETests) apply(ctx context.Context, r *managedSetReconciler, members []resolvedMember, statuses []oramav1alpha1.SetMemberStatus) {
	var entries []oramav1alpha1.SetE2ETestStatus
	var summary []string
	for _, test := range e.set.Spec.E2ETests {
		entry := oramav1alpha1.SetE2ETestStatus{}
		if i := slices.IndexFunc(e.observed.E2ETests, func(o oramav1alpha1.SetE2ETestStatus) bool { return o.Name == test.Name }); i >= 0 {
			entry = *e.observed.E2ETests[i].DeepCopy()
		}
		entry.Name, entry.Member = test.Name, test.Member
		if i := slices.IndexFunc(members, func(m resolvedMember) bool { return m.member.Name == test.Member }); i < 0 {
			e.refuse(&entry, "it names no member of this set")
		} else {
			e.applyOne(ctx, r, test, members[i], statuses[i], &entry)
		}
		switch entry.Phase {
		case e2ePhasePending, e2ePhaseRunning, e2ePhaseRemoving:
			e.busy = true
		}
		line := entry.Name + " " + entry.Phase
		if entry.Phase == e2ePhaseSucceeded || entry.Phase == e2ePhaseFailed {
			line += fmt.Sprintf(" %d/%d/%d", entry.Pass, entry.Fail, entry.Skip)
		}
		summary = append(summary, line)
		entries = append(entries, entry)
	}
	e.set.Status.E2ETests = append(entries, e.removing...)
	e.set.Status.E2ESummary = strings.Join(summary, ", ")
}

func (e *setE2ETests) applyOne(ctx context.Context, r *managedSetReconciler, test oramav1alpha1.SetE2ETest, resolved resolvedMember, member oramav1alpha1.SetMemberStatus, entry *oramav1alpha1.SetE2ETestStatus) {
	cluster := resolved.member.ClusterRef
	spec, err := jsonObject(test.Spec.Raw, "spec.e2eTests["+test.Name+"].spec")
	if err != nil {
		e.refuse(entry, err.Error())
		return
	}
	if _, set := spec["targetRef"]; set {
		e.refuse(entry, "spec.targetRef is set, but the target is the member: remove it")
		return
	}
	want := map[string]any{"lakeSet": e.set.Name, "name": resolved.member.Name}
	if got, set := spec["member"]; set && !reflect.DeepEqual(got, want) {
		e.refuse(entry, fmt.Sprintf("spec.member is %v, but the set fills it in as lakeSet %s, name %s: remove it", got, e.set.Name, resolved.member.Name))
		return
	}
	spec["member"] = want
	// The member moved to another cluster: the run there is deleted before one is applied here.
	if entry.Cluster != "" && entry.Cluster != cluster {
		if gone := e.delete(ctx, r, entry, entry.Cluster); !gone {
			return
		}
		*entry = oramav1alpha1.SetE2ETestStatus{Name: entry.Name, Member: entry.Member}
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
	req := e.request(r, test.Name, cluster, resolved.namespace)
	req.Spec = spec
	state, err := r.applier.ApplyE2ETest(ctx, kubeconfig, req)
	if state.MissingCRD {
		e.wait(entry, fmt.Sprintf("cluster %s does not have the %s kind (%s): its Orama operator predates it", cluster, lakeE2EKind.Kind, lakeE2ECRD))
		return
	}
	if err != nil {
		e.fail(entry, err)
		return
	}
	entry.Cluster = cluster
	if state.Object != nil {
		observeE2ETest(state.Object, entry)
	}
}

// observeE2ETest reads a LakeE2ETest's latest run as its cluster reports it. A result recorded for
// an earlier generation is not this spec's: it is reported as Pending until the new run starts.
func observeE2ETest(obj *unstructured.Unstructured, entry *oramav1alpha1.SetE2ETestStatus) {
	str := func(field string) string {
		value, _, _ := unstructured.NestedString(obj.Object, "status", field)
		return value
	}
	num := func(field string) int32 {
		value, _, _ := unstructured.NestedInt64(obj.Object, "status", field)
		return int32(value)
	}
	observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	phase := str("phase")
	*entry = oramav1alpha1.SetE2ETestStatus{Name: entry.Name, Member: entry.Member, Cluster: entry.Cluster}
	switch {
	case phase == "":
		entry.Phase, entry.Message = e2ePhasePending, phaseText(phase)
	case phase != e2ePhasePending && observed != obj.GetGeneration():
		entry.Phase = e2ePhasePending
		entry.Message = fmt.Sprintf("the cluster's operator has not started the run for the latest settings yet (the previous run was %s)", phase)
	default:
		entry.Phase, entry.Message = phase, str("message")
		entry.JobName, entry.RunID, entry.LakeRevision = str("jobName"), str("runID"), str("lakeRevision")
		entry.Pass, entry.Fail, entry.Skip = num("pass"), num("fail"), num("skip")
	}
}

func (a clientMemberApplier) ApplyE2ETest(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	return a.ApplyMember(ctx, kubeconfig, req)
}

func (a clientMemberApplier) DeleteE2ETest(ctx context.Context, kubeconfig []byte, req memberRequest) error {
	return a.DeleteMember(ctx, kubeconfig, req)
}
