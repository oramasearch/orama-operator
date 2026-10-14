/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// An OramaCloudV1Set connects its read members to their primary (operator#396). Nothing is stated
// by hand: the primary's spec.remoteReaders comes from the read members' clusters' egress IPs, and a
// read member's spec.peers from the primary's status.remoteRead, with the primary's credentials
// Secret copied from its cluster into the read member's namespace.

// memberRole is a member's region.role, primary when unset (the CRD default).
func memberRole(p *memberPass) string {
	region, _ := p.spec["region"].(map[string]any)
	if role, _ := region["role"].(string); role != "" {
		return role
	}
	return "primary"
}

func memberNames(passes []*memberPass) string {
	names := make([]string, 0, len(passes))
	for _, p := range passes {
		names = append(names, p.resolved.member.Name+" on "+p.resolved.member.ClusterRef)
	}
	return strings.Join(names, ", ")
}

// hostRange is an address as a single-host CIDR.
func hostRange(address string) string {
	if ip := net.ParseIP(address); ip != nil && ip.To4() == nil {
		return address + "/128"
	}
	return address + "/32"
}

// linkReadMembers checks the set's roles and writes the primary's remoteReaders. A set with read
// members has exactly one primary; a set without any is left as it is. A read member whose cluster
// has not reported its egress IPs waits, since the primary would not accept it.
func linkReadMembers(passes []*memberPass) error {
	var primaries, reads []*memberPass
	for _, p := range passes {
		if p.spec == nil {
			continue // its spec could not be read; it has failed already
		}
		switch memberRole(p) {
		case "read":
			reads = append(reads, p)
		case "primary":
			primaries = append(primaries, p)
		}
	}
	if len(reads) == 0 {
		return nil
	}
	if len(primaries) != 1 {
		return &setError{"InvalidRoles", fmt.Sprintf("read members (%s) need exactly one primary in the set to read from, and it has %d%s: nothing is applied",
			memberNames(reads), len(primaries), strings.TrimSuffix(" ("+memberNames(primaries)+")", " ()"))}
	}
	primary := primaries[0]
	var ranges []string
	for _, p := range reads {
		p.source = primary
		if p.resolved.cluster == nil {
			continue // it waits for its cluster to exist
		}
		ips := p.resolved.cluster.Status.EgressIPs
		if len(ips) == 0 && !p.done {
			p.wait(fmt.Sprintf("waiting for KubernetesCluster %s to report its egress IPs, which primary %s must allow", p.resolved.cluster.Name, primary.resolved.member.Name))
		}
		for _, ip := range ips {
			ranges = append(ranges, hostRange(ip))
		}
	}
	slices.Sort(ranges)
	ranges = slices.Compact(ranges)
	if len(ranges) > 0 {
		allowed := make([]any, 0, len(ranges))
		for _, r := range ranges {
			allowed = append(allowed, r)
		}
		primary.spec["remoteReaders"] = map[string]any{"allowedSourceRanges": allowed}
	}
	return nil
}

// connectReadMember writes a read member's peers from what its primary, applied just before it,
// publishes, and adds the primary's credentials Secret to the Secrets copied to it. Until the
// primary publishes, the member waits and is not applied.
func connectReadMember(ctx context.Context, r *managedSetReconciler, p *memberPass) {
	primary := p.source
	name := primary.resolved.member.Name + " on " + primary.resolved.member.ClusterRef
	if primary.state.Object == nil {
		p.wait("waiting for primary " + name + " to be applied")
		return
	}
	published, _, _ := unstructured.NestedStringMap(primary.state.Object.Object, "status", "remoteRead")
	stream, secretName, controlDB := published["brokerStreamEndpoint"], published["credentialsSecret"], published["controlDBEndpoint"]
	if stream == "" || secretName == "" {
		p.wait("waiting for primary " + name + " to publish its replication stream (status.remoteRead)")
		return
	}
	replicate, _, _ := unstructured.NestedBool(p.spec, "components", "controlDB", "replicate")
	if replicate && controlDB == "" {
		p.wait("waiting for primary " + name + " to publish its control database (status.remoteRead.controlDBEndpoint), which controlDB.replicate streams from")
		return
	}
	source, err := r.applier.ReadSecret(ctx, primary.kubeconfig, primary.resolved.namespace, secretName)
	if err != nil {
		p.fail(fmt.Errorf("primary %s: %w", name, err))
		return
	}
	if source == nil {
		p.wait(fmt.Sprintf("waiting for Secret %s/%s on primary %s", primary.resolved.namespace, secretName, name))
		return
	}
	// cnpg.io/reload makes CNPG pick up a rotated replication certificate in the copy.
	copied := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: source.Data}
	copied.Name, copied.Labels = secretName, map[string]string{"cnpg.io/reload": "true"}
	p.req.Secrets = append(p.req.Secrets, copied)

	peer := map[string]any{
		"name":                 primary.resolved.member.Name,
		"brokerStreamEndpoint": stream,
		"credentialsSecretRef": map[string]any{"name": secretName},
	}
	if primary.resolved.cluster != nil {
		peer["region"] = primary.resolved.cluster.Spec.Region
	}
	if replicate {
		peer["postgresEndpoint"] = controlDB
	}
	p.spec["peers"] = []any{peer}
}
