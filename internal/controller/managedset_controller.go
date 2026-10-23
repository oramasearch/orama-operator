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
	// The label a set puts on each instance it creates: its cluster's purpose tag (nonprod,
	// production). The set writes it on every pass, so it is the set's to decide: an instance's own
	// reconciler reads it (a production Cloud v1 dashboard needs its production authentication
	// settings, operator#794), and an edit by hand is put back on the next pass.
	instancePurposeLabel = "orama.cloud/purpose"
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
	// prepare computes, once per pass, what every member's spec takes from the whole set (a Lake's
	// region list). A *setError refuses the set: nothing is applied. Nil for a kind that needs none.
	prepare func(template map[string]any, members []resolvedMember) (any, error)
	// finish sets what a member's spec takes from where it runs and from prepare, after the merge.
	finish func(spec map[string]any, namespace string, cluster *oramav1alpha1.KubernetesCluster, shared any) error
	// observe reads an instance as its cluster reports it.
	observe func(obj *unstructured.Unstructured) (ready bool, version, message string)
}

var lakeSetKind = setKind{
	setKindName: "LakeSet",
	instance:    oramav1alpha1.GroupVersion.WithKind("Lake"),
	crd:         "lakes.orama.cloud", namespacePrefix: "lake-",
	prepare: prepareLakeRegions,
	finish: func(spec map[string]any, namespace string, cluster *oramav1alpha1.KubernetesCluster, shared any) error {
		// A Lake names its own namespace, and every member has its own.
		if set, ok := spec["namespace"].(string); ok && set != "" && set != namespace {
			return fmt.Errorf("spec.namespace is %q, but a member's namespace is set by the member (%q): remove it from the template and the member's spec, or set the member's namespace field", set, namespace)
		}
		spec["namespace"] = namespace
		return finishLakeRegion(spec, cluster, shared)
	},
	observe: func(obj *unstructured.Unstructured) (bool, string, string) {
		version, _, _ := unstructured.NestedString(obj.Object, "spec", "image")
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		ready, message := conditionOf(obj, "Ready")
		// The Ready condition says only that the Deployment is not up; the condition that says
		// why (a store that cannot be provisioned, Zetta not ready) is another one. All that are
		// not met are reported. BackupReady is left out: backups being off is a setting.
		for _, kind := range []string{"StoreReady", "ZettaReady"} {
			if met, why := conditionOf(obj, kind); !met && why != "" && !ready {
				message = strings.TrimPrefix(message+"; "+why, "; ")
			}
		}
		return phase == "Ready" && ready, version, firstNonEmpty(message, phaseText(phase))
	},
}

var oramaCloudV1SetKind = setKind{
	setKindName: "OramaCloudV1Set",
	instance:    oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"),
	crd:         "oramacloudv1s.orama.cloud", namespaced: true, namespacePrefix: "v1-",
	finish: func(spec map[string]any, _ string, cluster *oramav1alpha1.KubernetesCluster, _ any) error {
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
	// Purpose is the member's cluster's purpose tag, written as the instancePurposeLabel.
	Purpose string
	Spec    map[string]any
	Secrets []*corev1.Secret // already named as they are to be on the member's cluster
}

// memberState is what the cluster has.
type memberState struct {
	// MissingCRD is true when the cluster does not have the instance's kind: its operator is not
	// installed. Nothing was applied.
	MissingCRD bool
	// Object is the instance as read back after the apply.
	Object *unstructured.Unstructured
}

// leafMemberApplier applies, reads and deletes one instance, one OramaCloudV1Set migration, or one
// LakeSet e2e test, on a
// cluster reached by a kubeconfig. The set controllers hold one as an injected dependency; tests
// give a fake.
type leafMemberApplier interface {
	ApplyMember(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error)
	DeleteMember(ctx context.Context, kubeconfig []byte, req memberRequest) error
	// ApplyMigration applies an OramaCloudV1Migration and its Secrets as ApplyMember applies an
	// instance. A refused apply returns the migration the cluster still has with the error.
	ApplyMigration(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error)
	// DeleteMigration deletes one, and once it is gone the Secrets named in req.Secrets. While its
	// finalizer keeps it, the state's Object is the migration; nil means it is gone.
	DeleteMigration(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error)
	// ApplyE2ETest applies a LakeSet's LakeE2ETest (cluster-scoped, no Secrets) as ApplyMember
	// applies an instance.
	ApplyE2ETest(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error)
	// DeleteE2ETest deletes one this set applied; its Job goes with it. One that is gone, or not
	// this set's, is left alone.
	DeleteE2ETest(ctx context.Context, kubeconfig []byte, req memberRequest) error
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
	case apierrors.IsNotFound(err):
		existing = nil
	default:
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
		sec.Name, sec.Namespace, sec.Labels = source.Name, req.Namespace, source.Labels
		sec.Type, sec.Data = source.Type, source.Data
		if err := c.Patch(ctx, sec, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
			return memberState{}, fmt.Errorf("the cluster refused Secret %s/%s: %w", req.Namespace, source.Name, err)
		}
	}
	desired := memberObject(req)
	desired.SetLabels(memberLabels(req))
	desired.Object["spec"] = req.Spec
	if err := c.Patch(ctx, desired, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
		// What the cluster has is left as it was, and returned with the refusal.
		return memberState{Object: existing}, fmt.Errorf("the cluster refused %s %s: %w", req.Kind.Kind, req.Name, err)
	}
	return memberState{Object: desired}, nil
}

// memberLabels is the labels a set writes on an instance: whose it is, and what its cluster is for.
func memberLabels(req memberRequest) map[string]string {
	labels := map[string]string{managedBySetLabel: req.Owner}
	if req.Purpose != "" {
		labels[instancePurposeLabel] = req.Purpose
	}
	return labels
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
	// migrations copies Prod 1 teams into an OramaCloudV1Set's members; nil for a LakeSet.
	migrations *setMigrations
	// e2eTests runs lake's end-to-end suite against a LakeSet's members; nil for an
	// OramaCloudV1Set.
	e2eTests *setE2ETests
	// productionSources is what every production set copies, read once per pass (managedset_secret_scope.go).
	productionSources map[string]string
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
		namespace := r.kind.namespaceOf(member)
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

// namespaceOf is a member's own namespace on its cluster.
func (k setKind) namespaceOf(member oramav1alpha1.SetMember) string {
	return firstNonEmpty(member.Namespace, k.namespacePrefix+member.Name)
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
func (r *managedSetReconciler) secretsFor(ctx context.Context, spec *oramav1alpha1.ManagedSetSpec, resolved resolvedMember) ([]*corev1.Secret, error) {
	scope, err := r.scopeFor(ctx, resolved, nil)
	if err != nil {
		return nil, err
	}
	return r.copySecrets(ctx, scope, spec.Secrets, resolved.member.Secrets)
}

// secretTarget is a SetSecret's name on the member's cluster.
func secretTarget(s oramav1alpha1.SetSecret) string { return firstNonEmpty(s.As, s.Name) }

// copySecrets reads the named Secrets on this cluster, each named as it is to be on the member's
// cluster. One in a later list replaces one of the same name there in an earlier list.
// Every one is checked against scope first: a Secret that may not reach the member's cluster is
// refused, and nothing is copied.
func (r *managedSetReconciler) copySecrets(ctx context.Context, scope secretScope, lists ...[]oramav1alpha1.SetSecret) ([]*corev1.Secret, error) {
	chosen := map[string]oramav1alpha1.SetSecret{}
	var order []string
	for _, list := range lists {
		for _, ref := range list {
			if _, seen := chosen[secretTarget(ref)]; !seen {
				order = append(order, secretTarget(ref))
			}
			chosen[secretTarget(ref)] = ref
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
		if err := scope.refuse(ref, name, source); err != nil {
			return nil, err
		}
		copied := &corev1.Secret{Type: source.Type, Data: source.Data}
		copied.Name = name
		secrets = append(secrets, copied)
	}
	return secrets, nil
}

// owner is the managedBySetLabel value on what the set applies.
func (r *managedSetReconciler) owner(set managedSet) string {
	return r.kind.setKindName + "." + set.GetName()
}

func (r *managedSetReconciler) request(set managedSet, resolved resolvedMember) memberRequest {
	req := memberRequest{
		Kind: r.kind.instance, CRD: r.kind.crd, Name: resolved.member.Name, Namespace: resolved.namespace,
		Owner: r.owner(set),
	}
	if r.kind.namespaced {
		req.ObjectNamespace = resolved.namespace
	}
	if resolved.cluster != nil {
		req.Purpose = resolved.cluster.Spec.Tags[clusterPurposeTag]
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
	var shared any
	if err != nil {
		err = &setError{"InvalidTemplate", err.Error()}
	} else if members, status.Purpose, err = r.resolve(ctx, spec); err == nil && r.kind.prepare != nil {
		shared, err = r.kind.prepare(template, members)
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

	// Migrations the set no longer declares are deleted first: a member leaving the set waits for
	// the ones at its place.
	var migrationsAt map[string]bool
	if r.migrations != nil {
		migrationsAt = r.migrations.removeUndeclared(ctx, r)
	}
	if r.e2eTests != nil {
		r.e2eTests.removeUndeclared(ctx, r)
	}
	// Members that were in the set and no longer are.
	held, err := r.release(ctx, set, members, observed, migrationsAt)
	if err != nil {
		setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDegraded, "RemovalFailed", err.Error())
		if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
			return settle(ctrl.Result{}, statusErr)
		}
		return ctrl.Result{}, err
	}

	// Members are independent: each is applied on every pass, and none waits for another. One
	// member failing or starting slowly does not hold back the rest.
	statuses := make([]oramav1alpha1.SetMemberStatus, 0, len(members))
	var ready int32
	var notReady []string
	var firstErr error
	for _, resolved := range members {
		entry := oramav1alpha1.SetMemberStatus{Cluster: resolved.member.ClusterRef, Name: resolved.member.Name, Namespace: resolved.namespace}
		for _, previous := range observed.MemberStatuses {
			if previous.Cluster == entry.Cluster && previous.Name == entry.Name {
				entry.Version = previous.Version
			}
		}
		isReady, err := r.applyMember(ctx, set, template, shared, resolved, &entry)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if isReady {
			ready++
		} else {
			line := resolved.member.Name + " on " + resolved.member.ClusterRef
			if entry.Message != "" {
				line += " (" + entry.Message + ")"
			}
			notReady = append(notReady, line)
		}
		statuses = append(statuses, entry)
	}
	status.MemberStatuses = append(statuses, held...)
	status.Members, status.ReadyMembers = int32(len(members)), ready
	status.ObservedGeneration = set.GetGeneration()

	phase, reason, requeue := setPhaseProgressing, "MembersNotReady", leafOperatorPollInterval
	message := fmt.Sprintf("%d of %d members are Ready; not Ready: %s", ready, len(members), strings.Join(notReady, "; "))
	switch {
	case firstErr != nil:
		phase, reason, message = clusterPhaseDegraded, "MemberFailed", firstErr.Error()
	case int(ready) == len(members):
		phase, reason, message, requeue = clusterPhaseReady, "AllMembersReady", "", leafOperatorSteadyRecheck
	}
	if r.migrations != nil {
		// After the members, since a migration needs its stack. They do not change the set's phase;
		// what the set could not do with one is added to its message, and returned.
		r.migrations.apply(ctx, r, members, statuses)
		if problems := r.migrations.problems; len(problems) > 0 {
			message = strings.TrimPrefix(message+"; "+strings.Join(problems, "; "), "; ")
		}
		if r.migrations.busy {
			requeue = leafOperatorPollInterval
		}
		if firstErr == nil {
			firstErr = r.migrations.err
		}
	}
	if r.e2eTests != nil {
		// After the members, since a run needs its Lake. Like migrations, they do not change the
		// set's phase; what the set could not do with one is added to its message, and returned.
		r.e2eTests.apply(ctx, r, members, statuses)
		if problems := r.e2eTests.problems; len(problems) > 0 {
			message = strings.TrimPrefix(message+"; "+strings.Join(problems, "; "), "; ")
		}
		if r.e2eTests.busy {
			requeue = leafOperatorPollInterval
		}
		if firstErr == nil {
			firstErr = r.e2eTests.err
		}
	}
	setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), phase, reason, message)
	if err := r.writeStatus(ctx, set, observed); err != nil {
		return settle(ctrl.Result{}, err)
	}
	return settle(ctrl.Result{RequeueAfter: requeue}, firstErr)
}

// applyMember brings one member to its settings and fills in its status. It returns whether the
// member is Ready, and an error only for something that failed (a wait is not an error).
func (r *managedSetReconciler) applyMember(ctx context.Context, set managedSet, template map[string]any, shared any, resolved resolvedMember, entry *oramav1alpha1.SetMemberStatus) (bool, error) {
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
	merged, err := memberSpec(template, resolved)
	if err != nil {
		return fail(err)
	}
	if err := r.kind.finish(merged, resolved.namespace, resolved.cluster, shared); err != nil {
		return fail(err)
	}
	req := r.request(set, resolved)
	req.Spec = merged
	if req.Secrets, err = r.secretsFor(ctx, set.SetSpec(), resolved); err != nil {
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

// memberSpec is a member's spec: its own settings merged over the template, as a copy through
// JSON so nothing shared with the template is written.
func memberSpec(template map[string]any, resolved resolvedMember) (map[string]any, error) {
	var override map[string]any
	if resolved.member.Spec != nil {
		var err error
		if override, err = jsonObject(resolved.member.Spec.Raw, "the member's spec"); err != nil {
			return nil, err
		}
	}
	merged := mergeSpec(template, override)
	raw, _ := json.Marshal(merged)
	_ = json.Unmarshal(raw, &merged)
	return merged, nil
}

// release handles members that were in the set's status and are no longer in its spec. Under
// Retain they are let go; under Delete their instances are deleted from their clusters, each once
// no migration is left at its place (migrationsAt, by cluster/namespace). It returns the members
// still waiting for theirs, which stay in the status until they are deleted.
func (r *managedSetReconciler) release(ctx context.Context, set managedSet, members []resolvedMember, observed *oramav1alpha1.ManagedSetStatus, migrationsAt map[string]bool) ([]oramav1alpha1.SetMemberStatus, error) {
	if set.SetSpec().DeletionPolicy != oramav1alpha1.SetDeletionDelete {
		return nil, nil
	}
	current := map[string]bool{}
	for _, resolved := range members {
		current[resolved.member.ClusterRef+"/"+resolved.member.Name] = true
	}
	var held []oramav1alpha1.SetMemberStatus
	for _, previous := range observed.MemberStatuses {
		if current[previous.Cluster+"/"+previous.Name] {
			continue
		}
		if migrationsAt[previous.Cluster+"/"+previous.Namespace] {
			previous.Phase, previous.Message = clusterPhaseDeleting, "waiting for its migrations to be removed first"
			held = append(held, previous)
			continue
		}
		if err := r.deleteMember(ctx, set, previous); err != nil {
			return nil, err
		}
	}
	return held, nil
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
	// Migrations go first and the members wait for them: a stack deleted under its migration would
	// skip the migration's removal and its guard for a switched team.
	if r.migrations != nil {
		if waiting := r.migrations.removeAll(ctx, r); waiting != "" || r.migrations.err != nil {
			reason, message := "RemovingMigrations", "waiting for its migrations to be removed: "+waiting
			if r.migrations.err != nil {
				reason, message = "DeleteFailed", r.migrations.err.Error()
			}
			setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDeleting, reason, message)
			if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
				return settle(ctrl.Result{}, statusErr)
			}
			return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.migrations.err)
		}
	}
	// E2E runs go before the members too: a run left behind would wait for a Lake that is gone.
	if r.e2eTests != nil {
		if waiting := r.e2eTests.removeAll(ctx, r); waiting != "" || r.e2eTests.err != nil {
			reason, message := "RemovingE2ETests", "waiting for its e2e tests to be removed: "+waiting
			if r.e2eTests.err != nil {
				reason, message = "DeleteFailed", r.e2eTests.err.Error()
			}
			setClusterStatus(&status.Phase, &status.Message, &status.Conditions, set.GetGeneration(), clusterPhaseDeleting, reason, message)
			if statusErr := r.writeStatus(ctx, set, observed); statusErr != nil {
				return settle(ctrl.Result{}, statusErr)
			}
			return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.e2eTests.err)
		}
	}
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
	// The set as read: this one with the status it had. A patch of what changed, with no version
	// precondition: this controller is the only writer of this status, so a read that is one write
	// behind must not make the write fail.
	base := set.DeepCopyObject().(managedSet)
	*base.SetStatus() = *observed.DeepCopy()
	if r.migrations != nil {
		r.migrations.restore(base)
	}
	if r.e2eTests != nil {
		r.e2eTests.restore(base)
	}
	if reflect.DeepEqual(base, set) {
		return nil
	}
	return r.Status().Patch(ctx, set, client.MergeFrom(base))
}

// LakeSetReconciler installs Lake instances onto managed clusters (operator#391), and runs lake's
// end-to-end suite against them (operator#589).
type LakeSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Applier reaches a member's cluster. Nil means the real one; tests inject a fake.
	Applier leafMemberApplier
}

// OramaCloudV1SetReconciler installs OramaCloudV1 instances onto managed clusters (operator#391),
// and copies Prod 1 teams into them (operator#654).
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
	return (&managedSetReconciler{Client: r.Client, kind: lakeSetKind, applier: applierOr(r.Applier),
		e2eTests: &setE2ETests{set: &set, observed: set.Status.DeepCopy()}}).reconcile(ctx, &set)
}

func (r *OramaCloudV1SetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set oramav1alpha1.OramaCloudV1Set
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return (&managedSetReconciler{Client: r.Client, kind: oramaCloudV1SetKind, applier: applierOr(r.Applier),
		migrations: &setMigrations{set: &set, observed: set.Status.DeepCopy()}}).reconcile(ctx, &set)
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
