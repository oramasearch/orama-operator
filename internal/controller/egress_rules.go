/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The policy half of EgressRule (operator#470). A CIDR rule is one egress allowance from its
// source pods to that range. A DNS-name rule never opens a path to the destination: the source may
// reach only the workspace's zetta-tunnel egress gateway, the gateway may receive only from the
// source, and the gateway alone may reach public addresses on the rule's ports. The operator's own
// publicInternet rule carries a workspace's internetEgress unchanged, under its original policy
// name allow-internet-https. Every allowance is added to the workspace NetworkSandbox, which renders
// and owns the NetworkPolicy objects and keeps its mandatory metadata deny.

const (
	// maxEgressRulePorts is the gateway's per-rule port limit, applied to every rule.
	maxEgressRulePorts = 64
	// workspaceInternetRuleName is the EgressRule carrying a workspace's internetEgress.
	workspaceInternetRuleName = "internet-https"
)

// egressRuleNeverReachable is always removed from a CIDR rule, whatever it names. Cluster addresses
// are added at render time. An explicit CIDR rule never reaches what public-internet egress never reaches
// (workspacePublicIPExceptions): private (RFC 1918), CGNAT, ULA, loopback, link-local and metadata,
// documentation, multicast and reserved ranges, and every cluster address (LSR-217,
// orama-operator#812, #780, #764). A range inside one of them is refused; a wider range is opened
// minus all of them. On an overlay network the pod and Service ranges are private addresses, so they
// are excluded even when no node reports them.
var egressRuleNeverReachable = workspacePublicIPExceptions

func sortedInt32(values []int32) []int32 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}

// parseEgressCIDR accepts a canonical prefix. Lake's cidr column is Postgres cidr, whose text is
// already canonical; a host address without a length is a single address.
func parseEgressCIDR(value string) (netip.Prefix, error) {
	if !strings.Contains(value, "/") {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("cidr %q is not an address range", value)
		}
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("cidr %q is not an address range", value)
	}
	if prefix.Masked() != prefix {
		return netip.Prefix{}, fmt.Errorf("cidr %q has host bits set; use %s", value, prefix.Masked())
	}
	return prefix, nil
}

// normalizeEgressDNSName returns the exact name the gateway authorizes: ASCII, lower case, one
// final dot removed, no wildcard and never an IP literal (an address is a CIDR rule).
func normalizeEgressDNSName(value string) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if name == "" || len(name) > 253 {
		return "", errors.New("dns_name must be a host name of at most 253 characters")
	}
	if net.ParseIP(name) != nil {
		return "", errors.New("dns_name must be a name; an address is a cidr rule")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", errors.New("dns_name must be a fully qualified host name")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("dns_name label %q is invalid", label)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("dns_name %q may hold only letters, digits, hyphens and dots; wildcards are not supported", value)
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return "", errors.New("dns_name must not end in a numeric label")
	}
	return name, nil
}

func validateEgressRuleSpec(spec *oramav1alpha1.EgressRuleSpec) error {
	destinations := 0
	if spec.CIDR != "" {
		destinations++
		if _, err := parseEgressCIDR(spec.CIDR); err != nil {
			return err
		}
	}
	if spec.DNSName != "" {
		destinations++
		normalized, err := normalizeEgressDNSName(spec.DNSName)
		if err != nil {
			return err
		}
		if normalized != spec.DNSName {
			return fmt.Errorf("dnsName must be written as %q", normalized)
		}
	}
	if spec.PublicInternet {
		destinations++
	}
	if destinations != 1 {
		return errors.New("exactly one of cidr, dnsName and publicInternet is required")
	}
	if (spec.SourceKind == "") != (spec.SourceID == "") {
		return errors.New("sourceKind and sourceId are set together")
	}
	if len(spec.Ports) == 0 || len(spec.Ports) > maxEgressRulePorts {
		return fmt.Errorf("ports must list between 1 and %d ports", maxEgressRulePorts)
	}
	seen := map[int32]bool{}
	for _, port := range spec.Ports {
		if port < 1 || port > 65535 || seen[port] {
			return errors.New("ports must be distinct and between 1 and 65535")
		}
		seen[port] = true
	}
	return nil
}

// egressRuleAllowanceNames are the sandbox allowances one rule owns.
func egressRuleAllowanceNames(rule *oramav1alpha1.EgressRule) []string {
	switch {
	case rule.Spec.PublicInternet:
		if rule.Spec.RuleID == "" && rule.Name == workspaceInternetRuleName {
			return []string{workspaceInternetName}
		}
		return []string{rule.Name}
	case rule.Spec.DNSName != "":
		return []string{rule.Name, rule.Name + "-gateway-ingress", rule.Name + "-gateway-egress"}
	default:
		return []string{rule.Name}
	}
}

func tcpPolicyPorts(ports []int32) []networkingv1.NetworkPolicyPort {
	sorted := sortedUniqueInt32(sortedInt32(append([]int32(nil), ports...)))
	out := make([]networkingv1.NetworkPolicyPort, 0, len(sorted))
	for _, port := range sorted {
		out = append(out, workspaceTCPPort(port))
	}
	return out
}

// cidrRulePeers is the rule's range minus every range public egress excludes (egressRuleNeverReachable)
// and every cluster address inside it. A range that lies wholly inside an excluded range allows
// nothing and is refused.
func cidrRulePeers(rule netip.Prefix, cluster []string) ([]networkingv1.NetworkPolicyPeer, error) {
	var except []string
	for _, value := range append(append([]string(nil), egressRuleNeverReachable...), cluster...) {
		excluded, err := netip.ParsePrefix(value)
		if err != nil || excluded.Addr().Is4() != rule.Addr().Is4() || !excluded.Overlaps(rule) {
			continue
		}
		if excluded.Bits() <= rule.Bits() {
			return nil, fmt.Errorf("cidr %s lies inside %s, which no egress rule may reach", rule, excluded)
		}
		except = append(except, excluded.Masked().String())
	}
	sort.Strings(except)
	return []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: rule.String(), Except: sortedUnique(except)}}}, nil
}

// egressGatewaySelector selects the workspace's egress gateway pods.
func egressGatewaySelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{egressGatewayComponentLabel: egressGatewayComponent}}
}

// egressRuleAllowances renders the allowances of one present rule.
func egressRuleAllowances(
	ctx context.Context,
	reader client.Reader,
	rule *oramav1alpha1.EgressRule,
	source metav1.LabelSelector,
) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	names := egressRuleAllowanceNames(rule)
	ports := tcpPolicyPorts(rule.Spec.Ports)
	describe := "every workload in the workspace"
	if rule.Spec.SourceKind != "" {
		describe = rule.Spec.SourceKind + " " + rule.Spec.SourceID
	}
	egress := func(name string, selector metav1.LabelSelector, peers []networkingv1.NetworkPolicyPeer, ports []networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
		return networkPolicy(name, selector, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil,
			[]networkingv1.NetworkPolicyEgressRule{{To: peers, Ports: ports}})
	}
	switch {
	case rule.Spec.PublicInternet:
		peers, err := publicInternetPeers(ctx, reader)
		if err != nil {
			return nil, err
		}
		comment := "This workspace explicitly enables HTTPS to public addresses while excluding private, metadata, API, pod and node ranges."
		if names[0] != workspaceInternetName {
			comment = fmt.Sprintf("EgressRule %s lets %s reach public addresses on its ports.", rule.Name, describe)
		}
		return []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(comment, egress(names[0], source, peers, ports))}, nil
	case rule.Spec.CIDR != "":
		prefix, err := parseEgressCIDR(rule.Spec.CIDR)
		if err != nil {
			return nil, err
		}
		cluster, err := clusterAddressExceptions(ctx, reader)
		if err != nil {
			return nil, err
		}
		peers, err := cidrRulePeers(prefix, cluster)
		if err != nil {
			return nil, egressRuleInvalidError{err}
		}
		comment := fmt.Sprintf("EgressRule %s lets %s reach %s on its ports.", rule.Name, describe, prefix)
		return []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(comment, egress(names[0], source, peers, ports))}, nil
	default:
		peers, err := publicInternetPeers(ctx, reader)
		if err != nil {
			return nil, err
		}
		gateway := egressGatewaySelector()
		gatewayPorts := tcpPolicyPorts([]int32{egressGatewayPort})
		return []oramav1alpha1.NetworkSandboxPolicy{
			sandboxAllowance(
				fmt.Sprintf("EgressRule %s lets %s reach %s only through the workspace egress gateway.", rule.Name, describe, rule.Spec.DNSName),
				egress(names[0], source, []networkingv1.NetworkPolicyPeer{{PodSelector: gateway.DeepCopy()}}, gatewayPorts),
			),
			sandboxAllowance(
				fmt.Sprintf("The egress gateway accepts %s for EgressRule %s.", describe, rule.Name),
				networkPolicy(names[1], gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: source.DeepCopy()}}, Ports: gatewayPorts}},
					nil),
			),
			sandboxAllowance(
				fmt.Sprintf("The egress gateway reaches public addresses on the ports of EgressRule %s after authorizing %s by name.", rule.Name, rule.Spec.DNSName),
				egress(names[2], gateway, peers, ports),
			),
		}, nil
	}
}

// egressRuleInvalidError marks a rule that can never be rendered as written.
type egressRuleInvalidError struct{ err error }

func (e egressRuleInvalidError) Error() string { return e.err.Error() }
func (e egressRuleInvalidError) Unwrap() error { return e.err }
