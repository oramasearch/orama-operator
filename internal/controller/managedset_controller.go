/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	managedSetFinalizer = "orama.cloud/managed-set-cleanup"

	setPhaseProgressing = "Progressing"

	// The label a set puts on each instance it creates: <Kind>.<set name>. A set does not touch
	// an instance that lacks its label.
	managedBySetLabel = "orama.cloud/managed-by-set"
	// The tag on a KubernetesCluster that says what the cluster is for.
	clusterPurposeTag = "purpose"
)

// managedSet is what LakeSet and OramaCloudV1Set have in common.
type managedSet interface {
	client.Object
	SetSpec() *oramav1alpha1.ManagedSetSpec
	SetStatus() *oramav1alpha1.ManagedSetStatus
}

// setKind describes the kind of instance a set manages.
type setKind struct {
	setKindName     string // LakeSet
	instance        schema.GroupVersionKind
	crd             string // the instance kind's CRD, which must exist on a member's cluster
	namespaced      bool   // whether the instance is a namespaced resource
	namespacePrefix string
	// finish sets what a member's spec takes from where it runs, after the merge.
	finish func(spec map[string]any, namespace string, cluster *oramav1alpha1.KubernetesCluster) error
	// observe reads an instance as its cluster reports it.
	observe func(obj *unstructured.Unstructured) (ready bool, version, message string)
}

var lakeSetKind = setKind{
	setKindName: "LakeSet",
	instance:    oramav1alpha1.GroupVersion.WithKind("Lake"),
	crd:         "lakes.orama.cloud", namespacePrefix: "lake-",
	finish: func(spec map[string]any, namespace string, _ *oramav1alpha1.KubernetesCluster) error {
		// A Lake names its own namespace, and every member has its own.
		if set, ok := spec["namespace"].(string); ok && set != "" && set != namespace {
			return fmt.Errorf("spec.namespace is %q, but a member's namespace is set by the member (%q): remove it from the template and the member's spec, or set the member's namespace field", set, namespace)
		}
		spec["namespace"] = namespace
		return nil
	},
	observe: func(obj *unstructured.Unstructured) (bool, string, string) {
		version, _, _ := unstructured.NestedString(obj.Object, "spec", "image")
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		ready, message := conditionOf(obj, "Ready")
		return phase == "Ready" && ready, version, firstNonEmpty(message, phaseText(phase))
	},
}

var oramaCloudV1SetKind = setKind{
	setKindName: "OramaCloudV1Set",
	instance:    oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"),
	crd:         "oramacloudv1s.orama.cloud", namespaced: true, namespacePrefix: "v1-",
	finish: func(spec map[string]any, _ string, cluster *oramav1alpha1.KubernetesCluster) error {
		// The region is the cluster's. It is never stated, and cannot be stated differently.
		region, _ := spec["region"].(map[string]any)
		if region == nil {
			region = map[string]any{}
		}
		if set, ok := region["name"].(string); ok && set != "" && set != cluster.Spec.Region {
			return fmt.Errorf("spec.region.name is %q, but the instance runs on %s, which is in %s: the region comes from the cluster, remove it", set, cluster.Name, cluster.Spec.Region)
		}
		region["name"] = cluster.Spec.Region
		spec["region"] = region
		return nil
	},
	observe: func(obj *unstructured.Unstructured) (bool, string, string) {
		version, _, _ := unstructured.NestedString(obj.Object, "spec", "images", "tag")
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
		_, message := conditionOf(obj, "Ready")
		return phase == "Ready" && observed == obj.GetGeneration(), version, firstNonEmpty(message, phaseText(phase))
	},
}

func phaseText(phase string) string {
	if phase == "" {
		return "the cluster's operator has not reported on it yet"
	}
	return "the cluster reports it as " + phase
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// conditionOf reads a condition of an instance: whether it is True for the instance's current
// generation, and its message when it is not.
func conditionOf(obj *unstructured.Unstructured, kind string) (bool, string) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != kind {
			continue
		}
		observed, _ := condition["observedGeneration"].(int64)
		message, _ := condition["message"].(string)
		if condition["status"] == "True" && observed == obj.GetGeneration() {
			return true, ""
		}
		if observed != obj.GetGeneration() {
			return false, "the cluster's operator has not reconciled the latest settings yet"
		}
		return false, message
	}
	return false, ""
}

// mergeSpec lays a member's settings over the template: a field wins, an object is merged field
// by field, and a list replaces the template's list whole.
func mergeSpec(template, override map[string]any) map[string]any {
	merged := make(map[string]any, len(template)+len(override))
	for key, value := range template {
		merged[key] = value
	}
	for key, value := range override {
		base, baseIsMap := merged[key].(map[string]any)
		over, overIsMap := value.(map[string]any)
		if baseIsMap && overIsMap {
			merged[key] = mergeSpec(base, over)
			continue
		}
		merged[key] = value
	}
	return merged
}

func jsonObject(raw []byte, what string) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", what, err)
	}
	if object == nil {
		object = map[string]any{}
	}
	return object, nil
}

// memberRequest is one instance a managed cluster should have.
type memberRequest struct {
	Kind      schema.GroupVersionKind
	CRD       string
	Name      string
	Namespace string // the member's own namespace: created, and where its Secrets go
	// ObjectNamespace is the instance's metadata.namespace; empty for a cluster-scoped kind.
	ObjectNamespace string
	Owner           string // the managedBySetLabel value
	Spec            map[string]any
	Secrets         []*corev1.Secret // already named as they are to be on the member's cluster
}

// memberState is what the cluster has.
type memberState struct {
	// MissingCRD is true when the cluster does not have the instance's kind: its operator is not
	// installed. Nothing was applied.
	MissingCRD bool
	// Object is the instance as read back after the apply.
	Object *unstructured.Unstructured
}

// leafMemberApplier applies, reads and deletes one instance on a cluster reached by a kubeconfig.
// The set controllers hold one as an injected dependency; tests give a fake.
type leafMemberApplier interface {
	ApplyMember(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error)
	DeleteMember(ctx context.Context, kubeconfig []byte, req memberRequest) error
}

type clientMemberApplier struct{}

// errNotManagedBySet is returned when an instance of the same name exists and was not created by
// this set. A set never takes one over.
var errNotManagedBySet = errors.New("exists and is not managed by this set")

func memberObject(req memberRequest) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(req.Kind)
	obj.SetName(req.Name)
	obj.SetNamespace(req.ObjectNamespace)
	return obj
}

func (clientMemberApplier) ApplyMember(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	_, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return memberState{}, err
	}
	return applyMemberWith(ctx, c, req)
}

// applyMemberWith is ApplyMember against a client for the member's cluster.
func applyMemberWith(ctx context.Context, c client.Client, req memberRequest) (memberState, error) {
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
		if owner := existing.GetLabels()[managedBySetLabel]; owner != req.Owner {
			return memberState{Object: existing}, fmt.Errorf("%s %s on the cluster %w (its %s label is %q)", req.Kind.Kind, req.Name, errNotManagedBySet, managedBySetLabel, owner)
		}
	case !apierrors.IsNotFound(err):
		return memberState{}, fmt.Errorf("%s %s cannot be read on the cluster: %w", req.Kind.Kind, req.Name, err)
	}
	// The namespace, then the Secrets, then the instance: an instance whose pull secret or
	// credentials are not there yet starts failing.
	if err := applyNamespaceAndPullSecret(ctx, c, req.Namespace, nil); err != nil {
		return memberState{}, err
	}
	for _, source := range req.Secrets {
		sec := &corev1.Secret{}
		sec.APIVersion, sec.Kind = "v1", "Secret"
		sec.Name, sec.Namespace = source.Name, req.Namespace
		sec.Type, sec.Data = source.Type, source.Data
		if err := c.Patch(ctx, sec, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
			return memberState{}, fmt.Errorf("the cluster refused Secret %s/%s: %w", req.Namespace, source.Name, err)
		}
	}
	desired := memberObject(req)
	desired.SetLabels(map[string]string{managedBySetLabel: req.Owner})
	desired.Object["spec"] = req.Spec
	if err := c.Patch(ctx, desired, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
		return memberState{}, fmt.Errorf("the cluster refused %s %s: %w", req.Kind.Kind, req.Name, err)
	}
	return memberState{Object: desired}, nil
}

// DeleteMember deletes an instance this set created. One that is gone, or that is not this
// set's, is left alone.
func (clientMemberApplier) DeleteMember(ctx context.Context, kubeconfig []byte, req memberRequest) error {
	_, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return err
	}
	return deleteMemberWith(ctx, c, req)
}

func deleteMemberWith(ctx context.Context, c client.Client, req memberRequest) error {
	existing := memberObject(req)
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		if apierrors.IsNotFound(err) || apimetaNoMatch(err) {
			return nil
		}
		return fmt.Errorf("%s %s cannot be read on the cluster: %w", req.Kind.Kind, req.Name, err)
	}
	if existing.GetLabels()[managedBySetLabel] != req.Owner {
		return nil
	}
	if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("the cluster refused to delete %s %s: %w", req.Kind.Kind, req.Name, err)
	}
	return nil
}

func apimetaNoMatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no matches for kind")
}

// managedSetReconciler is the controller both set kinds share.
type managedSetReconciler struct {
	client.Client
	kind    setKind
	applier leafMemberApplier
}

// resolvedMember is a member with everything worked out from the set and its cluster.
type resolvedMember struct {
	member    oramav1alpha1.SetMember
	namespace string
	cluster   *oramav1alpha1.KubernetesCluster
}

// setError is a problem with the set as written: it is reported and not retried, because only an
// edit fixes it.
type setError struct{ reason, message string }

func (e *setError) Error() string { return e.message }

// resolve checks the set as a whole before anything is applied: its members are distinct, each
// has its own namespace on its cluster, and every cluster is for the same purpose.
func (r *managedSetReconciler) resolve(ctx context.Context, spec *oramav1alpha1.ManagedSetSpec) ([]resolvedMember, string, error) {
	members := make([]resolvedMember, 0, len(spec.Clusters))
	names, namespaces := map[string]bool{}, map[string]string{}
	purposes := map[string][]string{}
	var purpose string
	for _, member := range spec.Clusters {
		namespace := member.Namespace
		if namespace == "" {
			namespace = r.kind.namespacePrefix + member.Name
		}
		if key := member.ClusterRef + "/" + member.Name; names[key] {
			return nil, "", &setError{"DuplicateMember", fmt.Sprintf("two members are named %s on cluster %s", member.Name, member.ClusterRef)}
		} else {
			names[key] = true
		}
		if other, taken := namespaces[member.ClusterRef+"/"+namespace]; taken {
			return nil, "", &setError{"SharedNamespace", fmt.Sprintf("members %s and %s would share namespace %s on cluster %s: each instance needs its own", other, member.Name, namespace, member.ClusterRef)}
		}
		namespaces[member.ClusterRef+"/"+namespace] = member.Name

		resolved := resolvedMember{member: member, namespace: namespace}
		var cluster oramav1alpha1.KubernetesCluster
		switch err := r.Get(ctx, types.NamespacedName{Name: member.ClusterRef}, &cluster); {
		case err == nil:
			resolved.cluster = &cluster
			p := cluster.Spec.Tags[clusterPurposeTag]
			if p == "" {
				return nil, "", &setError{"NoPurpose", fmt.Sprintf("KubernetesCluster %s has no %q tag, so it is not known whether it is nonprod or production: nothing is applied", cluster.Name, clusterPurposeTag)}
			}
			purposes[p] = append(purposes[p], cluster.Name)
			purpose = p
		case !apierrors.IsNotFound(err):
			return nil, "", err
		}
		members = append(members, resolved)
	}
	if len(purposes) > 1 {
		var parts []string
		for p, clusters := range purposes {
			sort.Strings(clusters)
			parts = append(parts, p+" ("+strings.Join(dedupe(clusters), ", ")+")")
		}
		sort.Strings(parts)
		return nil, "", &setError{"MixedPurpose", "a set never combines clusters of different purposes, and this one names " + strings.Join(parts, " and ") + ": nothing is applied"}
	}
	return members, purpose, nil
}

func dedupe(sorted []string) []string {
	var out []string
	for i, value := range sorted {
		if i == 0 || value != sorted[i-1] {
			out = append(out, value)
		}
	}
	return out
}

// secretsFor reads the Secrets a member gets: the set's, with the member's own replacing any of
// the same name on the member's cluster.
func (r *managedSetReconciler) secretsFor(ctx context.Context, spec *oramav1alpha1.ManagedSetSpec, member oramav1alpha1.SetMember) ([]*corev1.Secret, error) {
	target := func(s oramav1alpha1.SetSecret) string { return firstNonEmpty(s.As, s.Name) }
	chosen := map[string]oramav1alpha1.SetSecret{}
	var order []string
	for _, list := range [][]oramav1alpha1.SetSecret{spec.Secrets, member.Secrets} {
		for _, ref := range list {
			if _, seen := chosen[target(ref)]; !seen {
				order = append(order, target(ref))
			}
			chosen[target(ref)] = ref
		}
	}
	secrets := make([]*corev1.Secret, 0, len(order))
	for _, name := range order {
		ref := chosen[name]
		source := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, source); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("Secret %s/%s does not exist on this cluster", ref.Namespace, ref.Name)
			}
			return nil, err
		}
		copied := &corev1.Secret{Type: source.Type, Data: source.Data}
		copied.Name = name
		secrets = append(secrets, copied)
	}
	return secrets, nil
}

func (r *managedSetReconciler) request(set managedSet, resolved resolvedMember) memberRequest {
	req := memberRequest{
		Kind: r.kind.instance, CRD: r.kind.crd, Name: resolved.member.Name, Namespace: resolved.namespace,
		Owner: r.kind.setKindName + "." + set.GetName(),
	}
	if r.kind.namespaced {
		req.ObjectNamespace = resolved.namespace
	}
	return req
}

func (r *managedSetReconciler) reconcile(ctx context.Context, set managedSet) (ctrl.Result, error) {
	spec, status := set.SetSpec(), set.SetStatus()
	observed := status.DeepCopy()

	if !set.GetDeletionTimestamp().IsZero() {
		return r.finalize(ctx, set, observed)
	}
	if !controllerutil.ContainsFinalizer(set, managedSetFinalizer) {
		controllerutil.AddFinalizer(set, managedSetFinalizer)
		if err := r.Update(ctx, set); err != nil {
			return settle(ctrl.Result{}, err)
		}
	}

	template, err := jsonObject(spec.Template.Raw, "spec.template")
	var members []resolvedMember
	if err == nil {
		members, status.Purpose, err = r.resolve(ctx, spec)
	} else {
		err = &setError{"InvalidTemplate", err.Error()}
	}
	var refused *setError
	if errors.As(err, &refused) {
		// The set as written cannot be applied. Nothing is touched on any cluster.
		status.Members, status.ReadyMembers = int32(len(spec.Clusters)), 0
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDegraded, refused.reason, refused.message)
		status.ObservedGeneration = set.GetGeneration()
		return settle(ctrl.Result{}, r.writeStatus(ctx, set, observed))
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Members that were in the set and no longer are.
	if err := r.release(ctx, set, members, observed); err != nil {
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDegraded, "RemovalFailed", err.Error())
		if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
			return settle(ctrl.Result{}, statusErr)
		}
		return ctrl.Result{}, err
	}

	statuses := make([]oramav1alpha1.SetMemberStatus, 0, len(members))
	var ready int32
	var blockedBy string
	var firstErr error
	for _, resolved := range members {
		entry := oramav1alpha1.SetMemberStatus{Cluster: resolved.member.ClusterRef, Name: resolved.member.Name, Namespace: resolved.namespace}
		for _, previous := range observed.MemberStatuses {
			if previous.Cluster == entry.Cluster && previous.Name == entry.Name {
				entry.Version = previous.Version
			}
		}
		if blockedBy != "" {
			// Ordered rollout: a member is not touched until the one before it is Ready.
			entry.Phase, entry.Message = clusterPhasePending, "waiting for "+blockedBy+" to be Ready"
			statuses = append(statuses, entry)
			continue
		}
		isReady, err := r.applyMember(ctx, set, template, resolved, &entry)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if isReady {
			ready++
		} else {
			blockedBy = resolved.member.Name + " on " + resolved.member.ClusterRef
		}
		statuses = append(statuses, entry)
	}
	status.MemberStatuses = statuses
	status.Members, status.ReadyMembers = int32(len(members)), ready
	status.ObservedGeneration = set.GetGeneration()

	switch {
	case firstErr != nil:
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDegraded, "MemberFailed", firstErr.Error())
		if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
			return settle(ctrl.Result{}, statusErr)
		}
		return ctrl.Result{}, firstErr
	case int(ready) == len(members):
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseReady, "AllMembersReady", "")
		return settle(ctrl.Result{RequeueAfter: leafOperatorSteadyRecheck}, r.writeStatus(ctx, set, observed))
	}
	message := fmt.Sprintf("%d of %d members are Ready; %s is not", ready, len(members), blockedBy)
	for _, entry := range statuses {
		if entry.Phase != clusterPhaseReady && entry.Message != "" && !strings.HasPrefix(entry.Message, "waiting for ") {
			message += ": " + entry.Message
			break
		}
	}
	setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), setPhaseProgressing, "MembersNotReady", message)
	return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.writeStatus(ctx, set, observed))
}

// applyMember brings one member to its settings and fills in its status. It returns whether the
// member is Ready, and an error only for something that failed (a wait is not an error).
func (r *managedSetReconciler) applyMember(ctx context.Context, set managedSet, template map[string]any, resolved resolvedMember, entry *oramav1alpha1.SetMemberStatus) (bool, error) {
	fail := func(err error) (bool, error) {
		entry.Phase, entry.Message = clusterPhaseDegraded, err.Error()
		return false, fmt.Errorf("member %s on %s: %w", resolved.member.Name, resolved.member.ClusterRef, err)
	}
	wait := func(message string) (bool, error) {
		entry.Phase, entry.Message = clusterPhasePending, message
		return false, nil
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, resolved.member.ClusterRef)
	if err != nil {
		return fail(err)
	}
	if waiting != "" || resolved.cluster == nil {
		return wait(firstNonEmpty(waiting, "KubernetesCluster "+resolved.member.ClusterRef+" does not exist"))
	}
	var override map[string]any
	if resolved.member.Spec != nil {
		if override, err = jsonObject(resolved.member.Spec.Raw, "the member's spec"); err != nil {
			return fail(err)
		}
	}
	// A copy through JSON, so nothing shared with the template is written.
	merged := mergeSpec(template, override)
	raw, _ := json.Marshal(merged)
	_ = json.Unmarshal(raw, &merged)
	if err := r.kind.finish(merged, resolved.namespace, resolved.cluster); err != nil {
		return fail(err)
	}
	req := r.request(set, resolved)
	req.Spec = merged
	if req.Secrets, err = r.secretsFor(ctx, set.SetSpec(), resolved.member); err != nil {
		return fail(err)
	}
	state, err := r.applier.ApplyMember(ctx, kubeconfig, req)
	if err != nil {
		return fail(err)
	}
	if state.MissingCRD {
		return wait(fmt.Sprintf("cluster %s does not have the %s kind (%s): its Orama operator is not installed yet", resolved.member.ClusterRef, r.kind.instance.Kind, r.kind.crd))
	}
	isReady, version, message := r.kind.observe(state.Object)
	entry.Version = version
	if isReady {
		entry.Phase, entry.Message = clusterPhaseReady, ""
		return true, nil
	}
	entry.Phase, entry.Message = setPhaseProgressing, message
	return false, nil
}

// release handles members that were in the set's status and are no longer in its spec. Under
// Retain they are let go; under Delete their instances are deleted from their clusters.
func (r *managedSetReconciler) release(ctx context.Context, set managedSet, members []resolvedMember, observed *oramav1alpha1.ManagedSetStatus) error {
	if set.SetSpec().DeletionPolicy != oramav1alpha1.SetDeletionDelete {
		return nil
	}
	current := map[string]bool{}
	for _, resolved := range members {
		current[resolved.member.ClusterRef+"/"+resolved.member.Name] = true
	}
	for _, previous := range observed.MemberStatuses {
		if current[previous.Cluster+"/"+previous.Name] {
			continue
		}
		if err := r.deleteMember(ctx, set, previous); err != nil {
			return err
		}
	}
	return nil
}

func (r *managedSetReconciler) deleteMember(ctx context.Context, set managedSet, member oramav1alpha1.SetMemberStatus) error {
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, member.Cluster)
	if err != nil {
		return err
	}
	if waiting != "" {
		// The cluster is gone or going; there is nothing to delete on it.
		return nil
	}
	req := r.request(set, resolvedMember{member: oramav1alpha1.SetMember{ClusterRef: member.Cluster, Name: member.Name}, namespace: member.Namespace})
	if err := r.applier.DeleteMember(ctx, kubeconfig, req); err != nil {
		return fmt.Errorf("member %s on %s: %w", member.Name, member.Cluster, err)
	}
	return nil
}

// finalize lets the set's instances go (Retain) or deletes them (Delete), then releases the set.
func (r *managedSetReconciler) finalize(ctx context.Context, set managedSet, observed *oramav1alpha1.ManagedSetStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(set, managedSetFinalizer) {
		return ctrl.Result{}, nil
	}
	status := set.SetStatus()
	if set.SetSpec().DeletionPolicy == oramav1alpha1.SetDeletionDelete {
		for _, member := range observed.MemberStatuses {
			if err := r.deleteMember(ctx, set, member); err != nil {
				setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDeleting, "DeleteFailed", err.Error())
				if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
					return settle(ctrl.Result{}, statusErr)
				}
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(set, managedSetFinalizer)
	return settle(ctrl.Result{}, r.Update(ctx, set))
}

// writeStatus patches the status only when it differs from what was read.
func (r *managedSetReconciler) writeStatus(ctx context.Context, set managedSet, observed *oramav1alpha1.ManagedSetStatus) error {
	if reflect.DeepEqual(set.SetStatus(), observed) {
		return nil
	}
	// A patch of what changed, with no version precondition: this controller is the only writer
	// of this status, so a read that is one write behind must not make the write fail.
	base := set.DeepCopyObject().(managedSet)
	*base.SetStatus() = *observed.DeepCopy()
	return r.Status().Patch(ctx, set, client.MergeFrom(base))
}

// LakeSetReconciler installs Lake instances onto managed clusters (operator#391).
type LakeSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Applier reaches a member's cluster. Nil means the real one; tests inject a fake.
	Applier leafMemberApplier
}

// OramaCloudV1SetReconciler installs OramaCloudV1 instances onto managed clusters (operator#391).
type OramaCloudV1SetReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Applier leafMemberApplier
}

//+kubebuilder:rbac:groups=orama.cloud,resources=lakesets;oramacloudv1sets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=lakesets/status;oramacloudv1sets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=lakesets/finalizers;oramacloudv1sets/finalizers,verbs=update
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func applierOr(applier leafMemberApplier) leafMemberApplier {
	if applier == nil {
		return clientMemberApplier{}
	}
	return applier
}

func (r *LakeSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set oramav1alpha1.LakeSet
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return (&managedSetReconciler{Client: r.Client, kind: lakeSetKind, applier: applierOr(r.Applier)}).reconcile(ctx, &set)
}

func (r *OramaCloudV1SetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set oramav1alpha1.OramaCloudV1Set
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return (&managedSetReconciler{Client: r.Client, kind: oramaCloudV1SetKind, applier: applierOr(r.Applier)}).reconcile(ctx, &set)
}

// setsForCluster maps a KubernetesCluster to the sets with a member on it, so a cluster becoming
// Ready reconciles them.
func setsForCluster[L client.ObjectList](c client.Client, list func() L, items func(L) []managedSet) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		sets := list()
		if err := c.List(ctx, sets); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, set := range items(sets) {
			for _, member := range set.SetSpec().Clusters {
				if member.ClusterRef == obj.GetName() {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: set.GetName()}})
					break
				}
			}
		}
		return requests
	})
}

func (r *LakeSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	forCluster := setsForCluster(r.Client, func() *oramav1alpha1.LakeSetList { return &oramav1alpha1.LakeSetList{} },
		func(list *oramav1alpha1.LakeSetList) []managedSet {
			sets := make([]managedSet, 0, len(list.Items))
			for i := range list.Items {
				sets = append(sets, &list.Items[i])
			}
			return sets
		})
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.LakeSet{}).
		Watches(&oramav1alpha1.KubernetesCluster{}, forCluster).Complete(r)
}

func (r *OramaCloudV1SetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	forCluster := setsForCluster(r.Client, func() *oramav1alpha1.OramaCloudV1SetList { return &oramav1alpha1.OramaCloudV1SetList{} },
		func(list *oramav1alpha1.OramaCloudV1SetList) []managedSet {
			sets := make([]managedSet, 0, len(list.Items))
			for i := range list.Items {
				sets = append(sets, &list.Items[i])
			}
			return sets
		})
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.OramaCloudV1Set{}).
		Watches(&oramav1alpha1.KubernetesCluster{}, forCluster).Complete(r)
}
