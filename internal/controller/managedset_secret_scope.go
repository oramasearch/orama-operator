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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Production credentials never reach a non-production cluster (orama-operator#787, #789; findings
// LSR-038 and LSR-040 of the Lake pre-launch security review; orama-operator#832). A set copies
// Secrets from the managing cluster into its members' namespaces, and an OramaOperator copies its
// pull Secret to the cluster it installs onto, so the copy is where the boundary is kept. What is
// production is read from the resources, never from a flag: the `purpose` tag of each
// KubernetesCluster, the sets and OramaOperators that copy a Secret, the field of a migration that
// names it, and a label on the Secret itself.
//
// Nothing else copies a Secret from the managing cluster to a managed one: a ClusterHelmRelease's
// chart credentials and a KubernetesCluster's cloud credentials are read on the managing cluster
// only, and its kubeconfig is written there.

const (
	// secretPurposeLabel on a Secret on the managing cluster names the one cluster purpose it may be
	// copied into (`nonprod`, `production`). A Secret with it is never copied into a member whose
	// cluster has another purpose.
	// The same key as instancePurposeLabel, which a set writes on the instances it creates.
	secretPurposeLabel = instancePurposeLabel
	// productionPurpose is the `purpose` tag of a production cluster.
	productionPurpose = "production"
)

// secretScope decides which Secrets on the managing cluster may be copied into one member, or to
// one cluster an OramaOperator installs onto.
type secretScope struct {
	// purpose is the target cluster's `purpose` tag.
	purpose string
	// operator is set when the copy is an OramaOperator's pull Secret rather than a set's; it only
	// changes the wording of a refusal.
	operator bool
	// production maps namespace/name of every Secret production copies to what copies it, worded
	// to follow the Secret's name ("is copied into production members by LakeSet lake-prod").
	production map[string]string
	// prod1 holds the names, on the member's cluster, that a migration reads as its Prod 1 source
	// credentials: the database URL, the engine master key and the settings IAM key.
	prod1 map[string]bool
}

// refuse says why source, copied as target, may not be copied into the member, or nil. A refusal is
// a *setError: only an edit (of the set, or of the Secret's label) changes it.
func (s secretScope) refuse(ref oramav1alpha1.SetSecret, target string, source *corev1.Secret) error {
	key := ref.Namespace + "/" + ref.Name
	purpose := firstNonEmpty(s.purpose, "unknown")
	label := source.Labels[secretPurposeLabel]
	if label != "" && label != s.purpose {
		where := "the member's cluster"
		if s.operator {
			where = "the target cluster"
		}
		return purposeRefusal("Secret %s is labelled %s=%s and %s is %s: it is never copied there", key, secretPurposeLabel, label, where, purpose)
	}
	if s.purpose == productionPurpose {
		return nil
	}
	if by, found := s.production[key]; found {
		if s.operator {
			target := "a " + s.purpose + " cluster"
			if s.purpose == "" {
				target = "a cluster with no purpose tag"
			}
			return purposeRefusal("Secret %s %s, so it is a production credential and is never copied to %s: give this cluster its own pull Secret (imagePullSecretRef, copied under the old name with imagePullSecretAs)", key, by, target)
		}
		return purposeRefusal("Secret %s %s, so it is a production credential and is never copied into a %s member: give this set its own Secret", key, by, purpose)
	}
	if s.prod1[target] && label != s.purpose {
		return purposeRefusal("Secret %s is this migration's Prod 1 source credential, which is production, and is never copied into a %s member; a Secret standing in for Prod 1 (a fixture) is copied only when labelled %s=%s", key, purpose, secretPurposeLabel, purpose)
	}
	return nil
}

func purposeRefusal(format string, args ...any) error {
	return &setError{"ProductionCredential", fmt.Sprintf(format, args...)}
}

// scopeFor is the scope of one member, with prod1 the names a migration into it reads as Prod 1
// credentials (nil for the member's own Secrets).
func (r *managedSetReconciler) scopeFor(ctx context.Context, resolved resolvedMember, prod1 map[string]bool) (secretScope, error) {
	scope := secretScope{prod1: prod1}
	if resolved.cluster != nil {
		scope.purpose = resolved.cluster.Spec.Tags[clusterPurposeTag]
	}
	if scope.purpose == productionPurpose {
		return scope, nil
	}
	if r.productionSources == nil {
		sources, err := productionSecretSources(ctx, r.Client)
		if err != nil {
			return secretScope{}, err
		}
		r.productionSources = sources
	}
	scope.production = r.productionSources
	return scope, nil
}

// operatorSecretScope is the scope of the cluster an OramaOperator installs onto: its purpose tag
// and, unless it is production, every Secret production copies. A cluster that does not exist yet
// has no purpose, so it is not production.
func operatorSecretScope(ctx context.Context, r client.Reader, clusterName string) (secretScope, error) {
	scope := secretScope{operator: true}
	var cluster oramav1alpha1.KubernetesCluster
	switch err := r.Get(ctx, types.NamespacedName{Name: clusterName}, &cluster); {
	case err == nil:
		scope.purpose = cluster.Spec.Tags[clusterPurposeTag]
	case !apierrors.IsNotFound(err):
		return secretScope{}, err
	}
	if scope.purpose == productionPurpose {
		return scope, nil
	}
	sources, err := productionSecretSources(ctx, r)
	if err != nil {
		return secretScope{}, err
	}
	scope.production = sources
	return scope, nil
}

// productionSecretSources lists every Secret (namespace/name) that production copies, with what
// copies it, worded to follow the Secret's name: a production set's set, member or migration
// Secret, and a production OramaOperator's pull Secret. A set is production when one of its
// members' clusters is tagged production; a set whose clusters cannot be read is taken at its last
// reported purpose. An OramaOperator is production when its cluster is tagged production. A
// LakeOrderSource's memberSecrets are copied into every Lake ordered from its set, so they are
// production when that set is, before the first order is made (orama-operator#790, LSR-041).
func productionSecretSources(ctx context.Context, r client.Reader) (map[string]string, error) {
	sources := map[string]string{}
	purposes := map[string]string{}
	isProduction := func(spec *oramav1alpha1.ManagedSetSpec, reported string) (bool, error) {
		for _, member := range spec.Clusters {
			purpose, read := purposes[member.ClusterRef]
			if !read {
				var cluster oramav1alpha1.KubernetesCluster
				switch err := r.Get(ctx, types.NamespacedName{Name: member.ClusterRef}, &cluster); {
				case err == nil:
					purpose = cluster.Spec.Tags[clusterPurposeTag]
				case !apierrors.IsNotFound(err):
					return false, err
				}
				purposes[member.ClusterRef] = purpose
			}
			if purpose == productionPurpose {
				return true, nil
			}
		}
		return reported == productionPurpose, nil
	}
	add := func(by string, lists ...[]oramav1alpha1.SetSecret) {
		for _, list := range lists {
			for _, ref := range list {
				if _, found := sources[ref.Namespace+"/"+ref.Name]; !found {
					sources[ref.Namespace+"/"+ref.Name] = "is copied into production members by " + by
				}
			}
		}
	}
	addSet := func(by string, spec *oramav1alpha1.ManagedSetSpec, reported string, extra ...[]oramav1alpha1.SetSecret) (bool, error) {
		production, err := isProduction(spec, reported)
		if err != nil || !production {
			return false, err
		}
		add(by, spec.Secrets)
		for _, member := range spec.Clusters {
			add(by, member.Secrets)
		}
		add(by, extra...)
		return true, nil
	}

	var lakeSets oramav1alpha1.LakeSetList
	if err := r.List(ctx, &lakeSets); err != nil {
		return nil, fmt.Errorf("list LakeSets to find production Secrets: %w", err)
	}
	var v1Sets oramav1alpha1.OramaCloudV1SetList
	if err := r.List(ctx, &v1Sets); err != nil {
		return nil, fmt.Errorf("list OramaCloudV1Sets to find production Secrets: %w", err)
	}
	// In name order, so the set named in a refusal is always the same one.
	sort.Slice(lakeSets.Items, func(i, j int) bool { return lakeSets.Items[i].Name < lakeSets.Items[j].Name })
	sort.Slice(v1Sets.Items, func(i, j int) bool { return v1Sets.Items[i].Name < v1Sets.Items[j].Name })
	var orderSources oramav1alpha1.LakeOrderSourceList
	if err := r.List(ctx, &orderSources); err != nil {
		return nil, fmt.Errorf("list LakeOrderSources to find production Secrets: %w", err)
	}
	sort.Slice(orderSources.Items, func(i, j int) bool { return orderSources.Items[i].Name < orderSources.Items[j].Name })
	productionLakeSets := map[string]bool{}
	for index := range lakeSets.Items {
		set := &lakeSets.Items[index]
		production, err := addSet("LakeSet "+set.Name, &set.Spec.ManagedSetSpec, set.Status.Purpose)
		if err != nil {
			return nil, err
		}
		productionLakeSets[set.Name] = production
	}
	for index := range orderSources.Items {
		source := &orderSources.Items[index]
		if productionLakeSets[source.Spec.LakeSetRef] {
			add("LakeOrderSource "+source.Name, source.Spec.MemberSecrets)
		}
	}
	for index := range v1Sets.Items {
		set := &v1Sets.Items[index]
		var migrationSecrets [][]oramav1alpha1.SetSecret
		for _, migration := range set.Spec.Migrations {
			migrationSecrets = append(migrationSecrets, migration.Secrets)
		}
		if _, err := addSet("OramaCloudV1Set "+set.Name, &set.Spec.ManagedSetSpec, set.Status.Purpose, migrationSecrets...); err != nil {
			return nil, err
		}
	}
	var operators oramav1alpha1.OramaOperatorList
	if err := r.List(ctx, &operators); err != nil {
		return nil, fmt.Errorf("list OramaOperators to find production Secrets: %w", err)
	}
	sort.Slice(operators.Items, func(i, j int) bool { return operators.Items[i].Name < operators.Items[j].Name })
	for index := range operators.Items {
		operator := &operators.Items[index]
		production, err := isProduction(&oramav1alpha1.ManagedSetSpec{Clusters: []oramav1alpha1.SetMember{{ClusterRef: operator.Spec.ClusterRef}}}, "")
		if err != nil {
			return nil, err
		}
		ref := operator.Spec.ImagePullSecretRef
		if _, found := sources[ref.Namespace+"/"+ref.Name]; production && !found {
			sources[ref.Namespace+"/"+ref.Name] = "is the pull Secret of production cluster " + operator.Spec.ClusterRef + " (OramaOperator " + operator.Name + ")"
		}
	}
	return sources, nil
}

// prod1SourceCredentials is the names of the Secrets a migration's spec reads as its Prod 1 source
// credentials. Prod 1 is production, so these are production credentials wherever they are copied.
func prod1SourceCredentials(spec map[string]any) map[string]bool {
	names := map[string]bool{}
	source, _ := spec["source"].(map[string]any)
	for _, path := range [][]string{{"databaseSecretRef"}, {"masterKeySecretRef"}, {"settings", "credentialsSecretRef"}} {
		node := source
		for _, step := range path {
			node, _ = node[step].(map[string]any)
		}
		if name, _ := node["name"].(string); strings.TrimSpace(name) != "" {
			names[name] = true
		}
	}
	return names
}
