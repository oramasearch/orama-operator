/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// One origin-pull CA per zone, whatever the number of clusters serving it (operator#633,
// option (a)).
//
// A zone such as example.com is served by several clusters, and Cloudflare presents one client
// certificate per zone. So every cluster serving the zone must trust, and upload, a leaf of the same
// CA. The managing cluster generates it: a LakeSet reads each member's zone from the zoneID of the
// member's dnsSecretRef (a Secret the set copies), creates the zone's Secret
// orama-edge-origin-pull-<zone ID> in the operator's namespace once, renews its client certificate
// before it expires, and copies the CA certificate and client certificate (never the CA key) into
// the member's namespace with the Secrets the set names, before the Lake. The member's Lake points
// its EdgeZonePolicy at the copy (lakeOriginPullSecretRef), and the member cluster's operator uses
// it and never mints a CA of its own for the zone.
//
// On the managing cluster the copied Secret is the operator's own per-cluster Secret, so a Lake
// that no set manages there presents and trusts the same CA.
//
// This is a copy the set makes on its own rather than an entry in `secrets:`: the CA must exist
// before anything could name it, its name depends on the zone, it must reach exactly the members
// serving the zone and no other, and nobody creates or copies it by hand.

// lakeOriginPullSecrets adds the zone's origin-pull Secret to what a LakeSet copies to a member.
// A member without a dnsSecretRef the set copies, or whose Secret has no zoneID, gets none: its Lake
// reports the missing credential itself.
func lakeOriginPullSecrets(ctx context.Context, r *managedSetReconciler, spec map[string]any, copied []*corev1.Secret, resolved resolvedMember) ([]*corev1.Secret, error) {
	ref, _, _ := unstructured.NestedString(spec, "dnsSecretRef", "name")
	if ref == "" {
		return copied, nil
	}
	var dns *corev1.Secret
	for _, secret := range copied {
		if secret.Name == ref {
			dns = secret
		}
	}
	if dns == nil {
		return copied, nil
	}
	zoneID := strings.TrimSpace(string(dns.Data["zoneID"]))
	if zoneID == "" {
		return copied, nil
	}
	namespace := r.operatorNamespace
	if namespace == "" {
		return nil, fmt.Errorf("the operator's namespace (POD_NAMESPACE) is not set, so the zone's origin-pull CA has nowhere to live")
	}
	scope, err := r.scopeFor(ctx, resolved, nil)
	if err != nil {
		return nil, err
	}
	// The first set to need the zone's CA labels it with its member's purpose, so it never reaches a
	// cluster of another purpose (a zone is either nonprod or production).
	var labels map[string]string
	if scope.purpose != "" {
		labels = map[string]string{secretPurposeLabel: scope.purpose}
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	source, _, err := ensureOriginPullSecret(ctx, r.Client, namespace, zoneID, labels, now)
	if err != nil {
		return nil, fmt.Errorf("the zone's origin-pull CA: %w", err)
	}
	name := originPullSecretName(zoneID)
	if err := scope.refuse(oramav1alpha1.SetSecret{Name: name, Namespace: namespace}, name, source); err != nil {
		return nil, err
	}
	out := make([]*corev1.Secret, 0, len(copied)+1)
	for _, secret := range copied {
		// Only the operator writes this name: a Secret the set names under it is replaced.
		if secret.Name != name {
			out = append(out, secret)
		}
	}
	return append(out, distributedOriginPullCopy(source)), nil
}
