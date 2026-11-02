/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"sort"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The egress gateway's per-rule denied_cidrs (operator#1136, zetta-tunnel docs/egress.md): the
// gateway refuses private and special-use destinations by itself, and denied_cidrs adds the
// cluster's own addresses that sit in otherwise public ranges. The list comes only from what the
// API server reports, never from a tenant's fields. The workspace NetworkSandbox stays the second
// layer and keeps its own exceptions.

// egressGatewayMaxDeniedCIDRs is the gateway's limit per rule. A longer list is invalid and makes
// the gateway refuse the whole snapshot.
const egressGatewayMaxDeniedCIDRs = 256

// egressGatewayDeniedCIDRs reads the cluster inventory the gateway must never connect to: the
// Kubernetes API Service and its endpoints, every node address and node pod range (the same
// inventory the NetworkPolicy exceptions use) and every ServiceCIDR. An unreadable inventory is an
// error, so the caller publishes nothing and the last published snapshot stays.
func egressGatewayDeniedCIDRs(ctx context.Context, reader client.Reader) ([]string, error) {
	inventory, err := clusterAddressExceptions(ctx, reader)
	if err != nil {
		return nil, err
	}
	// networking.k8s.io/v1 ServiceCIDR is newer than the vendored k8s.io/api, so it is read
	// unstructured (uncached; a cluster has one or two). A cluster that does not serve it has no
	// list to read; the Kubernetes API Service address is in the inventory either way.
	serviceCIDRs := &unstructured.UnstructuredList{}
	serviceCIDRs.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "ServiceCIDRList"})
	if err := reader.List(ctx, serviceCIDRs); err != nil && !apimeta.IsNoMatchError(err) {
		return nil, fmt.Errorf("list ServiceCIDRs for the egress gateway: %w", err)
	}
	for i := range serviceCIDRs.Items {
		cidrs, _, err := unstructured.NestedStringSlice(serviceCIDRs.Items[i].Object, "spec", "cidrs")
		if err != nil {
			return nil, fmt.Errorf("read ServiceCIDR %s: %w", serviceCIDRs.Items[i].GetName(), err)
		}
		inventory = append(inventory, cidrs...)
	}
	return deniedCIDRList(inventory)
}

// deniedCIDRList turns inventory addresses and ranges into the list the gateway accepts: canonical
// CIDRs (explicit prefix length, no host bits, IPv4-mapped IPv6 written as IPv4), without the
// ranges the gateway always refuses, with no duplicate or nested entry, and at most
// egressGatewayMaxDeniedCIDRs long. A longer list is widened, never cut: two neighbouring entries
// become their smallest common supernet until it fits, so every inventory address stays denied.
// Leaving out the always-refused ranges also keeps the list stable while privately addressed nodes
// come and go: a changed list closes every session of every rule.
func deniedCIDRList(values []string) ([]string, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := canonicalPrefix(value)
		if err != nil {
			return nil, err
		}
		if !egressGatewayRefusesAlways(prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	prefixes = collapsePrefixes(prefixes)
	for len(prefixes) > egressGatewayMaxDeniedCIDRs {
		prefixes = collapsePrefixes(widenClosestPair(prefixes))
	}
	result := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		result[i] = prefix.String()
	}
	return result, nil
}

// canonicalPrefix parses an address or a CIDR into its masked network, unmapping IPv4-mapped IPv6.
func canonicalPrefix(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		address, addressErr := netip.ParseAddr(value)
		if addressErr != nil || address.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("cluster inventory value %q is not an address or a CIDR", value)
		}
		prefix = netip.PrefixFrom(address, address.BitLen())
	}
	if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

// egressGatewayRefusesAlways reports whether the prefix lies wholly inside a range the gateway
// refuses whatever its denied_cidrs say. workspacePublicIPExceptions is a subset of the gateway's
// own list (docs/egress.md, Resolved destination policy).
func egressGatewayRefusesAlways(prefix netip.Prefix) bool {
	for _, value := range workspacePublicIPExceptions {
		if prefixContains(netip.MustParsePrefix(value), prefix) {
			return true
		}
	}
	return false
}

// collapsePrefixes sorts the prefixes, drops each one another contains and merges sibling halves
// into their parent, so the same set of addresses has one shortest canonical list.
func collapsePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	sort.Slice(prefixes, func(i, j int) bool {
		if order := prefixes[i].Addr().Compare(prefixes[j].Addr()); order != 0 {
			return order < 0
		}
		return prefixes[i].Bits() < prefixes[j].Bits()
	})
	var kept []netip.Prefix
	for _, prefix := range prefixes {
		// Sorted by start address, a prefix can only be inside the last one kept.
		if len(kept) > 0 && prefixContains(kept[len(kept)-1], prefix) {
			continue
		}
		kept = append(kept, prefix)
		for len(kept) >= 2 {
			low, high := kept[len(kept)-2], kept[len(kept)-1]
			if low.Bits() != high.Bits() || low.Bits() == 0 || low.Addr().BitLen() != high.Addr().BitLen() {
				break
			}
			parent := netip.PrefixFrom(low.Addr(), low.Bits()-1).Masked()
			if parent.Addr() != low.Addr() || !parent.Contains(high.Addr()) {
				break
			}
			kept = append(kept[:len(kept)-2], parent)
		}
	}
	return kept
}

// widenClosestPair replaces the neighbouring pair of one address family with the longest common
// prefix by that common prefix. In a sorted list without nesting, the closest pair is neighbours.
func widenClosestPair(prefixes []netip.Prefix) []netip.Prefix {
	best, bestBits := -1, -1
	for i := 0; i+1 < len(prefixes); i++ {
		if bits := commonPrefixBits(prefixes[i], prefixes[i+1]); bits > bestBits {
			best, bestBits = i, bits
		}
	}
	if best < 0 {
		return prefixes
	}
	widened := netip.PrefixFrom(prefixes[best].Addr(), bestBits).Masked()
	return append(append(append([]netip.Prefix(nil), prefixes[:best]...), widened), prefixes[best+2:]...)
}

// commonPrefixBits is the length of the smallest prefix that holds both, or -1 across families.
func commonPrefixBits(a, b netip.Prefix) int {
	if a.Addr().BitLen() != b.Addr().BitLen() {
		return -1
	}
	for bits := min(a.Bits(), b.Bits()); bits > 0; bits-- {
		if netip.PrefixFrom(a.Addr(), bits).Masked().Contains(b.Addr()) {
			return bits
		}
	}
	return 0
}

func prefixContains(outer, inner netip.Prefix) bool {
	return outer.Addr().BitLen() == inner.Addr().BitLen() && outer.Bits() <= inner.Bits() && outer.Contains(inner.Addr())
}
