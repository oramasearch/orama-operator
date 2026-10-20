/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// An OramaCloudV1Set's migrations (operator#654). Each declared migration is applied as an
// OramaCloudV1Migration in its member's namespace on its member's cluster, its Secrets copied first,
// and its status is read back into the set's. One removed from the set is deleted there, and its
// Secrets once it is gone. A member leaving the set, and the set itself, wait for the migrations at
// their place to be gone: a stack deleted first would skip the migration's removal guard.

var migrationKind = oramav1alpha1.GroupVersion.WithKind("OramaCloudV1Migration")

const migrationCRD = "oramacloudv1migrations.orama.cloud"

// setMigrations reconciles one OramaCloudV1Set's migrations during one pass of the set.
type setMigrations struct {
	set      *oramav1alpha1.OramaCloudV1Set
	observed *oramav1alpha1.OramaCloudV1SetStatus // as read, so status is written only on a change

	// What this pass found: the migrations still being removed, what the set could not do (for its
	// message), the first failure (returned, so it is retried), and whether a migration is still
	// moving (so the set looks again soon).
	removing []oramav1alpha1.SetMigrationStatus
	problems []string
	err      error
	busy     bool
}

func (m *setMigrations) fail(entry *oramav1alpha1.SetMigrationStatus, err error) {
	m.refuse(entry, err.Error())
	if m.err == nil {
		m.err = fmt.Errorf("migration %s into %s: %w", entry.Name, entry.Member, err)
	}
}

// refuse reports what the set could not do with a migration. One already applied keeps the phase
// it last reported: what is on its cluster is left as it was.
func (m *setMigrations) refuse(entry *oramav1alpha1.SetMigrationStatus, message string) {
	if entry.Cluster == "" {
		entry.Step, entry.Phase = "", oramav1alpha1.MigrationPhaseBlocked
	}
	entry.Message = message
	m.problems = append(m.problems, fmt.Sprintf("migration %s into %s: %s", entry.Name, entry.Member, message))
}

func (m *setMigrations) wait(entry *oramav1alpha1.SetMigrationStatus, message string) {
	if entry.Cluster == "" {
		entry.Step, entry.Phase = "", oramav1alpha1.MigrationPhasePending
	}
	entry.Message = message
	m.busy = true
}

// restore puts the migrations' status as it was read into base, a copy of the set.
func (m *setMigrations) restore(base managedSet) {
	set := base.(*oramav1alpha1.OramaCloudV1Set)
	set.Status.Migrations, set.Status.MigrationSummary = m.observed.Migrations, m.observed.MigrationSummary
}

func (m *setMigrations) memberNamed(name string) (oramav1alpha1.SetMember, bool) {
	for _, member := range m.set.Spec.Clusters {
		if member.Name == name {
			return member, true
		}
	}
	return oramav1alpha1.SetMember{}, false
}

func migrationPlace(cluster, namespace string) string { return cluster + "/" + namespace }

// removeUndeclared deletes the migrations the set applied and no longer declares, and keeps
// reporting each until it is gone. One being removed holds only itself, and its member if that is
// leaving too. It returns the places (cluster/namespace) that still have a migration of this set.
func (m *setMigrations) removeUndeclared(ctx context.Context, r *managedSetReconciler) map[string]bool {
	declared := map[string]bool{}
	for _, migration := range m.set.Spec.Migrations {
		declared[migration.Name] = true
	}
	var undeclared []oramav1alpha1.SetMigrationStatus
	at := map[string]bool{}
	for _, entry := range m.observed.Migrations {
		switch {
		case entry.Cluster == "":
			// Never applied: there is nothing on any cluster.
		case declared[entry.Name]:
			at[migrationPlace(entry.Cluster, entry.Namespace)] = true
		default:
			undeclared = append(undeclared, entry)
		}
	}
	m.removing = m.remove(ctx, r, undeclared, m.secretsInUse(r.kind))
	for _, entry := range m.removing {
		at[migrationPlace(entry.Cluster, entry.Namespace)] = true
	}
	return at
}

// removeAll deletes every migration of a set being deleted. It returns what is still being removed,
// for the set's message; the set keeps its finalizer until nothing is.
func (m *setMigrations) removeAll(ctx context.Context, r *managedSetReconciler) string {
	entries := slices.Clone(m.observed.Migrations)
	// A declared migration is deleted even when the set never recorded applying it, with the
	// Secrets it lists: a pass may have applied it and failed to write the status.
	for _, migration := range m.set.Spec.Migrations {
		i := slices.IndexFunc(entries, func(entry oramav1alpha1.SetMigrationStatus) bool { return entry.Name == migration.Name })
		if i < 0 {
			member, ok := m.memberNamed(migration.Member)
			if !ok {
				continue
			}
			entries = append(entries, oramav1alpha1.SetMigrationStatus{Name: migration.Name, Member: migration.Member,
				Cluster: member.ClusterRef, Namespace: r.kind.namespaceOf(member)})
			i = len(entries) - 1
		}
		entries[i].Secrets = sortedUnion(entries[i].Secrets, secretTargets(migration.Secrets))
	}
	entries = slices.DeleteFunc(entries, func(entry oramav1alpha1.SetMigrationStatus) bool { return entry.Cluster == "" })
	present := m.remove(ctx, r, entries, nil)
	m.set.Status.Migrations, m.set.Status.MigrationSummary = present, ""
	waiting := make([]string, 0, len(present))
	for _, entry := range present {
		waiting = append(waiting, fmt.Sprintf("%s into %s (%s)", entry.Name, entry.Member, strings.TrimSuffix(entry.Phase+": "+entry.Message, ": ")))
	}
	return strings.Join(waiting, "; ")
}

// remove deletes migrations the set applied and returns those not gone yet, as their clusters
// report them. Once one is gone its Secrets go too, except those another migration at its place
// still lists, or that inUse holds there (the stack's own, a declared migration's).
func (m *setMigrations) remove(ctx context.Context, r *managedSetReconciler, entries []oramav1alpha1.SetMigrationStatus, inUse map[string]map[string]bool) []oramav1alpha1.SetMigrationStatus {
	var present []oramav1alpha1.SetMigrationStatus
	for i, entry := range entries {
		place := migrationPlace(entry.Cluster, entry.Namespace)
		keep := map[string]bool{}
		for name := range inUse[place] {
			keep[name] = true
		}
		for _, other := range append(slices.Clone(present), entries[i+1:]...) {
			if migrationPlace(other.Cluster, other.Namespace) == place {
				for _, name := range other.Secrets {
					keep[name] = true
				}
			}
		}
		gone, err := m.delete(ctx, r, &entry, keep)
		if err != nil {
			m.fail(&entry, err)
		}
		if !gone {
			m.busy = true
			present = append(present, entry)
		}
	}
	return present
}

// delete deletes one migration on its cluster, and once it is gone the Secrets copied for it that
// keep does not hold. It reports whether the migration is gone.
func (m *setMigrations) delete(ctx context.Context, r *managedSetReconciler, entry *oramav1alpha1.SetMigrationStatus, keep map[string]bool) (bool, error) {
	var cluster oramav1alpha1.KubernetesCluster
	switch err := r.Get(ctx, types.NamespacedName{Name: entry.Cluster}, &cluster); {
	case apierrors.IsNotFound(err):
		return true, nil // the cluster is gone, and everything that was on it
	case err != nil:
		return false, err
	case !cluster.DeletionTimestamp.IsZero():
		return true, nil
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, entry.Cluster)
	if err != nil || waiting != "" {
		if waiting != "" {
			entry.Phase, entry.Message = oramav1alpha1.MigrationPhaseRemoving, waiting
		}
		return false, err
	}
	req := memberRequest{Kind: migrationKind, CRD: migrationCRD, Name: entry.Name, Namespace: entry.Namespace,
		ObjectNamespace: entry.Namespace, Owner: r.owner(m.set)}
	for _, name := range entry.Secrets {
		if !keep[name] {
			secret := &corev1.Secret{}
			secret.Name = name
			req.Secrets = append(req.Secrets, secret)
		}
	}
	state, err := r.applier.DeleteMigration(ctx, kubeconfig, req)
	if err != nil || state.Object == nil {
		return err == nil, err
	}
	entry.Step, entry.Phase, entry.Message = observeMigration(state.Object)
	return false, nil
}

// secretsInUse is, by place, the Secrets the set's members and declared migrations copy there: a
// removed migration's Secret of one of those names stays.
func (m *setMigrations) secretsInUse(kind setKind) map[string]map[string]bool {
	inUse := map[string]map[string]bool{}
	add := func(place string, names []string) {
		if inUse[place] == nil {
			inUse[place] = map[string]bool{}
		}
		for _, name := range names {
			inUse[place][name] = true
		}
	}
	spec := &m.set.Spec
	for _, member := range spec.Clusters {
		add(migrationPlace(member.ClusterRef, kind.namespaceOf(member)), secretTargets(slices.Concat(spec.Secrets, member.Secrets)))
	}
	declared := map[string]bool{}
	for _, migration := range spec.Migrations {
		declared[migration.Name] = true
		if member, ok := m.memberNamed(migration.Member); ok {
			add(migrationPlace(member.ClusterRef, kind.namespaceOf(member)), secretTargets(migration.Secrets))
		}
	}
	// What a declared migration copied before its list changed is still its own.
	for _, entry := range m.observed.Migrations {
		if declared[entry.Name] {
			add(migrationPlace(entry.Cluster, entry.Namespace), entry.Secrets)
		}
	}
	return inUse
}

// apply brings each declared migration to what the set says, once its member has been applied
// (members and statuses are this pass's, index for index), and reports them with those still
// being removed.
func (m *setMigrations) apply(ctx context.Context, r *managedSetReconciler, members []resolvedMember, statuses []oramav1alpha1.SetMemberStatus) {
	spec := &m.set.Spec
	refusals := migrationSecretRefusals(spec)
	var entries []oramav1alpha1.SetMigrationStatus
	succeeded := 0
	for _, migration := range spec.Migrations {
		entry := oramav1alpha1.SetMigrationStatus{}
		if i := slices.IndexFunc(m.observed.Migrations, func(e oramav1alpha1.SetMigrationStatus) bool { return e.Name == migration.Name }); i >= 0 {
			entry = *m.observed.Migrations[i].DeepCopy()
		}
		entry.Name, entry.Member = migration.Name, migration.Member
		i := slices.IndexFunc(members, func(resolved resolvedMember) bool { return resolved.member.Name == migration.Member })
		switch {
		case i < 0:
			m.refuse(&entry, "it names no member of this set")
		case refusals[migration.Name] != "":
			m.refuse(&entry, refusals[migration.Name])
		default:
			m.applyOne(ctx, r, migration, members[i], statuses[i], &entry)
		}
		switch entry.Phase {
		case oramav1alpha1.MigrationPhaseSucceeded:
			succeeded++
		case oramav1alpha1.MigrationPhasePending, oramav1alpha1.MigrationPhaseRunning, oramav1alpha1.MigrationPhaseRemoving:
			m.busy = true
		}
		entries = append(entries, entry)
	}
	m.set.Status.Migrations = append(entries, m.removing...)
	m.set.Status.MigrationSummary = ""
	if len(spec.Migrations) > 0 {
		m.set.Status.MigrationSummary = fmt.Sprintf("%d/%d Succeeded", succeeded, len(spec.Migrations))
	}
}

func (m *setMigrations) applyOne(ctx context.Context, r *managedSetReconciler, migration oramav1alpha1.SetMigration, resolved resolvedMember, member oramav1alpha1.SetMemberStatus, entry *oramav1alpha1.SetMigrationStatus) {
	cluster, namespace := resolved.member.ClusterRef, resolved.namespace
	// Its target is immutable: moving it would mean deleting it, which removes the copied team.
	if entry.Cluster != "" && (entry.Cluster != cluster || entry.Namespace != namespace) {
		m.refuse(entry, fmt.Sprintf("it was applied into namespace %s on %s and a migration does not move: remove this entry, and add one under a new name for member %s", entry.Namespace, entry.Cluster, migration.Member))
		return
	}
	if member.Phase != clusterPhaseReady && member.Phase != setPhaseProgressing {
		m.wait(entry, strings.TrimSuffix("waiting for its member to be applied: "+member.Message, ": "))
		return
	}
	spec, err := jsonObject(migration.Spec.Raw, "spec.migrations["+migration.Name+"].spec")
	if err != nil {
		m.refuse(entry, err.Error())
		return
	}
	if target, ok := spec["targetRef"].(string); ok && target != resolved.member.Name {
		m.refuse(entry, fmt.Sprintf("spec.targetRef is %q, but the target is the member, %s: remove it", target, resolved.member.Name))
		return
	}
	spec["targetRef"] = resolved.member.Name
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, cluster)
	if err != nil {
		m.fail(entry, err)
		return
	}
	if waiting != "" {
		m.wait(entry, waiting)
		return
	}
	req := memberRequest{Kind: migrationKind, CRD: migrationCRD, Name: migration.Name, Namespace: namespace,
		ObjectNamespace: namespace, Owner: r.owner(m.set), Spec: spec}
	if req.Secrets, err = r.copySecrets(ctx, migration.Secrets); err != nil {
		m.fail(entry, err)
		return
	}
	// Labelled, so that only what the set copied is ever deleted.
	for _, secret := range req.Secrets {
		secret.Labels = map[string]string{managedBySetLabel: req.Owner}
	}
	state, err := r.applier.ApplyMigration(ctx, kubeconfig, req)
	if state.MissingCRD {
		m.wait(entry, fmt.Sprintf("cluster %s does not have the %s kind (%s): its Orama operator predates migrations", cluster, migrationKind.Kind, migrationCRD))
		return
	}
	// From here its Secrets may be on the cluster: the place is recorded so they are deleted with it.
	entry.Cluster, entry.Namespace = cluster, namespace
	entry.Secrets = sortedUnion(entry.Secrets, secretTargets(migration.Secrets))
	if state.Object != nil {
		entry.Step, entry.Phase, entry.Message = observeMigration(state.Object)
	}
	if err != nil {
		m.fail(entry, err)
	}
}

// observeMigration reads a migration's step, phase and message as its cluster reports them.
func observeMigration(obj *unstructured.Unstructured) (string, string, string) {
	step, _, _ := unstructured.NestedString(obj.Object, "status", "step")
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(obj.Object, "status", "message")
	observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	switch {
	case !obj.GetDeletionTimestamp().IsZero():
		if phase != oramav1alpha1.MigrationPhaseBlocked && phase != oramav1alpha1.MigrationPhaseFailed {
			phase = oramav1alpha1.MigrationPhaseRemoving
		}
	case phase == "":
		return step, oramav1alpha1.MigrationPhasePending, phaseText(phase)
	case observed != obj.GetGeneration():
		return step, oramav1alpha1.MigrationPhasePending, "the cluster's operator has not reconciled the latest settings yet"
	}
	return step, phase, message
}

// migrationSecretRefusals refuses each migration whose Secrets would change what its stack mounts
// (a set or member Secret of the same name there), or that copies a Secret under a name another
// migration into the same member copies from another source. Two migrations may share one Secret
// from one source.
func migrationSecretRefusals(spec *oramav1alpha1.OramaCloudV1SetSpec) map[string]string {
	refusals := map[string][]string{}
	type use struct{ migration, source string }
	uses := map[string]map[string][]use{} // member → name there → who copies it from where
	for _, migration := range spec.Migrations {
		var memberSecrets []oramav1alpha1.SetSecret
		for _, member := range spec.Clusters {
			if member.Name == migration.Member {
				memberSecrets = member.Secrets
			}
		}
		stack := secretTargets(slices.Concat(spec.Secrets, memberSecrets))
		if uses[migration.Member] == nil {
			uses[migration.Member] = map[string][]use{}
		}
		for _, secret := range migration.Secrets {
			name := secretTarget(secret)
			if slices.Contains(stack, name) {
				refusals[migration.Name] = append(refusals[migration.Name], fmt.Sprintf("Secret %s is one the stack itself gets from the set or its member, and a migration never changes what the stack mounts: copy it under another name with `as`", name))
				continue
			}
			uses[migration.Member][name] = append(uses[migration.Member][name], use{migration.Name, secret.Namespace + "/" + secret.Name})
		}
	}
	for _, byName := range uses {
		for name, list := range byName {
			var sources, migrations []string
			for _, u := range list {
				sources, migrations = append(sources, u.source), append(migrations, u.migration)
			}
			if sources = sortedUnion(nil, sources); len(sources) < 2 {
				continue
			}
			for _, migration := range sortedUnion(nil, migrations) {
				refusals[migration] = append(refusals[migration], fmt.Sprintf("Secret %s is copied from %s by migrations %s: one name has one source", name, strings.Join(sources, " and "), strings.Join(sortedUnion(nil, migrations), ", ")))
			}
		}
	}
	messages := map[string]string{}
	for migration, reasons := range refusals {
		sort.Strings(reasons)
		messages[migration] = strings.Join(reasons, "; ")
	}
	return messages
}

func secretTargets(secrets []oramav1alpha1.SetSecret) []string {
	names := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secretTarget(secret))
	}
	return names
}

// sortedUnion is the distinct values of both, sorted; nil when there are none.
func sortedUnion(a, b []string) []string {
	all := slices.Concat(a, b)
	sort.Strings(all)
	if all = slices.Compact(all); len(all) == 0 {
		return nil
	}
	return all
}

func (a clientMemberApplier) ApplyMigration(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	return a.ApplyMember(ctx, kubeconfig, req)
}

func (clientMemberApplier) DeleteMigration(ctx context.Context, kubeconfig []byte, req memberRequest) (memberState, error) {
	_, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return memberState{}, err
	}
	return deleteMigrationWith(ctx, c, req)
}

// deleteMigrationWith is DeleteMigration against a client for the member's cluster. A migration of
// that name that this set did not apply is left alone and counts as gone.
func deleteMigrationWith(ctx context.Context, c client.Client, req memberRequest) (memberState, error) {
	existing, err := migrationOf(ctx, c, req)
	if err != nil {
		return memberState{}, err
	}
	if existing != nil && existing.GetDeletionTimestamp().IsZero() {
		if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return memberState{}, fmt.Errorf("the cluster refused to delete %s %s: %w", req.Kind.Kind, req.Name, err)
		}
		if existing, err = migrationOf(ctx, c, req); err != nil {
			return memberState{}, err
		}
	}
	if existing != nil {
		return memberState{Object: existing}, nil
	}
	for _, named := range req.Secrets {
		secret := &corev1.Secret{}
		switch err := c.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: named.Name}, secret); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return memberState{}, fmt.Errorf("Secret %s/%s cannot be read on the cluster: %w", req.Namespace, named.Name, err)
		}
		if secret.Labels[managedBySetLabel] != req.Owner {
			continue
		}
		if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return memberState{}, fmt.Errorf("the cluster refused to delete Secret %s/%s: %w", req.Namespace, named.Name, err)
		}
	}
	return memberState{}, nil
}

// migrationOf reads the migration this set applied; nil when there is none.
func migrationOf(ctx context.Context, c client.Client, req memberRequest) (*unstructured.Unstructured, error) {
	existing := memberObject(req)
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		if apierrors.IsNotFound(err) || apimetaNoMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s %s cannot be read on the cluster: %w", req.Kind.Kind, req.Name, err)
	}
	if existing.GetLabels()[managedBySetLabel] != req.Owner {
		return nil, nil
	}
	return existing, nil
}
