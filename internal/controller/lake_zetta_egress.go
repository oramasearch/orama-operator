/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A Lake-owned Zetta's own egress allowlist (operator#724, defense in depth for zetta#75).
//
// The engine denies every outbound connection except what ZETTA_EGRESS_ALLOW lists (zetta's
// docs/governance/egress-policy.md). The namespace's NetworkSandbox stays the source of truth: the
// allowlist is read from the allowances the sandbox carries, never kept as a second list, so the
// engine refuses what the network policy refuses. A path the sandbox has not rendered (a Failed or
// Pending EgressRule or Connection) adds nothing, and a path that cannot be written in the engine's
// syntax adds the narrowest entries that still admit what the policy admits, or none.
//
// What is mirrored, for a Zetta in a workspace namespace whose Lake service id is serviceID:
//
//   - an EgressRule with no source, or with source service serviceID, whose allowance is in the
//     sandbox: a dnsName rule gives <dnsName>:<port>, a cidr rule <cidr>:<port> (link-local ranges,
//     the cloud metadata address among them, cut out as the policy does), and the operator's
//     publicInternet rule (the workspace's internetEgress) *:<port>, which is *:443;
//   - a Connection whose source is service serviceID and whose egress allowance is in the sandbox:
//     <service>.<namespace>.svc.cluster.local:<port> for each target Kubernetes Service port whose
//     pod port the Connection opens (the name every endpoint the operator publishes uses);
//   - while the namespace still has its same-namespace allowance (before every initial Connection
//     is enforced): *.<namespace>.svc.cluster.local.
//
// Not mirrored, because a Zetta never dials them through its HTTP client: cluster DNS, the Lake
// API, and other nodes of its own cluster (peer links are not egress).
//
// Where the engine's syntax is wider than the policy (it has no "except"): *:443 admits in-cluster
// names on 443 and a cidr entry keeps the pod, node and API server addresses the policy cuts out.
// Those addresses change as nodes come and go, and on an image that reads the value from
// ZETTA_EGRESS_ALLOW every change of it rolls the Zetta's StatefulSet, so the policy alone keeps
// excluding them.
//
// Where the value goes: the cluster's ConfigMap <cluster>-zetta-egress, mounted as the file
// ZETTA_EGRESS_ALLOW_FILE names, which an image with zetta#1332 re-reads without a restart; any
// other image gets it as ZETTA_EGRESS_ALLOW (zetta_egress_file.go).

// zettaEgressAllowEnv is the engine's allowlist variable.
const zettaEgressAllowEnv = "ZETTA_EGRESS_ALLOW"

// lakeDeploymentEgressAllow is the lake Deployment's own ZETTA_EGRESS_ALLOW (lake#1097). lake installs it
// as the policy of zetta's client, which it uses only for in-cluster services: the bus Zetta and the
// workspace Zettas (port 4800), agent services and service wrappers (8080). Workspace namespaces are
// created at runtime, so they are matched by suffix rather than listed, which would roll lake on
// every new workspace; nothing outside the cluster is admitted, and the namespace's NetworkPolicy
// still decides which in-cluster services lake reaches.
const lakeDeploymentEgressAllow = "*.svc.cluster.local"

// zettaEgressDeny is the allowlist that admits nothing but loopback.
const zettaEgressDeny = ""

// maxZettaEgressAllowBytes keeps the variable far below Linux's 128 KiB limit on one environment
// string; a longer list would stop the container from starting at all.
const maxZettaEgressAllowBytes = 64 << 10

// zettaEgressLinkLocal is cut out of every cidr entry: the policy never lets a rule reach it, and
// it holds the cloud metadata address. Loopback needs no cut: the engine admits it under every
// policy, and it never leaves the pod.
var zettaEgressLinkLocal = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10")}

// withZettaEgressAllow is env with ZETTA_EGRESS_ALLOW=allow added, unless the spec's env names it:
// a stated allowlist is the spec's own and is kept.
func withZettaEgressAllow(env []corev1.EnvVar, allow string) []corev1.EnvVar {
	if findEnvVar(env, zettaEgressAllowEnv) != nil {
		return env
	}
	return append(env, corev1.EnvVar{Name: zettaEgressAllowEnv, Value: allow})
}

func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// workspaceZettaEgressAllow is the allowlist of a Zetta in a workspace namespace, or, when the
// namespace cannot be read, the value the cluster already runs with (current, when known), so a
// failed read never changes what it may reach; the error is returned for the caller to log.
func workspaceZettaEgressAllow(
	ctx context.Context,
	reader client.Reader,
	namespace, serviceID string,
	current string, known bool,
) (string, error) {
	allow, err := lakeZettaEgressAllow(ctx, reader, namespace, serviceID)
	if err == nil || errors.Is(err, errZettaEgressTooLong) {
		return allow, err
	}
	if known {
		return current, err
	}
	return zettaEgressDeny, err
}

// lakeZettaEgressAllow renders ZETTA_EGRESS_ALLOW for a Zetta in a workspace namespace from the
// namespace's NetworkSandbox and the EgressRules and Connections whose allowances it carries.
func lakeZettaEgressAllow(ctx context.Context, reader client.Reader, namespace, serviceID string) (string, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return zettaEgressDeny, nil
		}
		return "", fmt.Errorf("read the NetworkSandbox for the Zetta egress allowlist: %w", err)
	}
	rendered := make(map[string]bool, len(sandbox.Spec.Policies))
	for _, policy := range sandbox.Spec.Policies {
		rendered[policy.Name] = true
	}
	var entries []string
	if rendered[workspaceInternalName] {
		entries = append(entries, "*."+namespace+".svc.cluster.local")
	}

	var rules oramav1alpha1.EgressRuleList
	if err := reader.List(ctx, &rules, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list EgressRules for the Zetta egress allowlist: %w", err)
	}
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.Desired != oramav1alpha1.EgressRuleDesiredPresent || !rule.DeletionTimestamp.IsZero() ||
			!rendered[egressRuleAllowanceNames(rule)[0]] || !zettaEgressSourceSelects(rule.Spec.SourceKind, rule.Spec.SourceID, serviceID) {
			continue
		}
		entries = append(entries, egressRuleZettaEntries(&rule.Spec)...)
	}

	var connections oramav1alpha1.ConnectionList
	if err := reader.List(ctx, &connections, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list Connections for the Zetta egress allowlist: %w", err)
	}
	for i := range connections.Items {
		connection := &connections.Items[i]
		egressName, _ := connectionPolicyNames(connection)
		if serviceID == "" || connection.Spec.Desired != oramav1alpha1.ConnectionDesiredPresent ||
			!connection.DeletionTimestamp.IsZero() || !rendered[egressName] ||
			connection.Spec.SourceKind != oramav1alpha1.ConnectionSourceService || connection.Spec.SourceID != serviceID {
			continue
		}
		connectionEntries, err := connectionZettaEntries(ctx, reader, connection)
		if err != nil {
			return "", err
		}
		entries = append(entries, connectionEntries...)
	}
	return joinZettaEgressEntries(entries)
}

// zettaEgressSourceSelects reports whether a rule with this source applies to the Zetta whose Lake
// service id is serviceID: no source is every workload in the workspace.
func zettaEgressSourceSelects(kind, id, serviceID string) bool {
	if kind == "" {
		return true
	}
	return serviceID != "" && kind == oramav1alpha1.EgressRuleSourceService && id == serviceID
}

// egressRuleZettaEntries is one rendered rule in the engine's syntax.
func egressRuleZettaEntries(spec *oramav1alpha1.EgressRuleSpec) []string {
	ports := sortedUniqueInt32(sortedInt32(append([]int32(nil), spec.Ports...)))
	var hosts []string
	switch {
	case spec.PublicInternet:
		hosts = []string{"*"}
	case spec.DNSName != "":
		name, err := normalizeEgressDNSName(spec.DNSName)
		if err != nil {
			return nil
		}
		hosts = []string{name}
	case spec.CIDR != "":
		prefix, err := parseEgressCIDR(spec.CIDR)
		if err != nil {
			return nil
		}
		for _, part := range subtractPrefixes(prefix, zettaEgressLinkLocal) {
			hosts = append(hosts, zettaEgressNetwork(part))
		}
	}
	entries := make([]string, 0, len(hosts)*len(ports))
	for _, host := range hosts {
		for _, port := range ports {
			entries = append(entries, host+":"+strconv.Itoa(int(port)))
		}
	}
	return entries
}

// zettaEgressNetwork writes a range the way the engine parses it: an IPv6 range is bracketed,
// because a port follows.
func zettaEgressNetwork(prefix netip.Prefix) string {
	if prefix.Addr().Is6() {
		return "[" + prefix.String() + "]"
	}
	return prefix.String()
}

// subtractPrefixes is prefix with every excluded range inside or around it removed, as the
// fewest prefixes that cover exactly what is left. A prefix inside an excluded range is gone.
func subtractPrefixes(prefix netip.Prefix, excluded []netip.Prefix) []netip.Prefix {
	parts := []netip.Prefix{prefix.Masked()}
	for _, cut := range excluded {
		var next []netip.Prefix
		for _, part := range parts {
			next = append(next, subtractPrefix(part, cut)...)
		}
		parts = next
	}
	sort.Slice(parts, func(i, j int) bool {
		if c := parts[i].Addr().Compare(parts[j].Addr()); c != 0 {
			return c < 0
		}
		return parts[i].Bits() < parts[j].Bits()
	})
	return parts
}

func subtractPrefix(part, cut netip.Prefix) []netip.Prefix {
	if part.Addr().Is4() != cut.Addr().Is4() || !part.Overlaps(cut) {
		return []netip.Prefix{part}
	}
	if cut.Bits() <= part.Bits() {
		return nil
	}
	// Split part in halves until the half holding cut is cut itself; keep every other half.
	var kept []netip.Prefix
	for part.Bits() < cut.Bits() {
		low := netip.PrefixFrom(part.Addr(), part.Bits()+1)
		high := netip.PrefixFrom(prefixUpperHalf(part), part.Bits()+1)
		if low.Contains(cut.Addr()) {
			kept = append(kept, high)
			part = low
		} else {
			kept = append(kept, low)
			part = high
		}
	}
	return kept
}

// prefixUpperHalf is the first address of the upper half of prefix.
func prefixUpperHalf(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Addr().AsSlice()
	bit := prefix.Bits()
	bytes[bit/8] |= 0x80 >> (bit % 8)
	address, _ := netip.AddrFromSlice(bytes)
	return address
}

// connectionZettaEntries is one rendered Connection from a Zetta: the target's Kubernetes Services
// by their cluster name, on each Service port that leads to a pod port the Connection opens. A
// named target port cannot be compared with declared numeric ports and is left out.
func connectionZettaEntries(ctx context.Context, reader client.Reader, connection *oramav1alpha1.Connection) ([]string, error) {
	target, err := lakeWorkloadSelector(ctx, reader, connection.Spec.LakeRef, connection.Namespace,
		connection.Spec.TargetKind, connection.Spec.TargetID)
	if err != nil {
		// The sandbox carries the path, so the target existed when it was rendered; a target that
		// cannot be resolved now admits nothing until it can.
		var pending connectionPendingError
		if errors.As(err, &pending) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve Connection %s target for the Zetta egress allowlist: %w", connection.Name, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&target)
	if err != nil {
		return nil, err
	}
	services, err := targetServices(ctx, reader, connection.Namespace, target, selector)
	if err != nil {
		return nil, fmt.Errorf("list Connection %s target Services for the Zetta egress allowlist: %w", connection.Name, err)
	}
	declared := map[int32]bool{}
	for _, port := range connection.Spec.Ports {
		declared[port] = true
	}
	var entries []string
	for i := range services {
		service := &services[i]
		host := service.Name + "." + service.Namespace + ".svc.cluster.local"
		if service.Namespace == "" {
			host = service.Name + "." + connection.Namespace + ".svc.cluster.local"
		}
		for _, servicePort := range service.Spec.Ports {
			if servicePort.Protocol != "" && servicePort.Protocol != corev1.ProtocolTCP {
				continue
			}
			if len(declared) > 0 {
				targetPort := servicePort.TargetPort
				if targetPort.Type == intstr.Int && targetPort.IntVal == 0 {
					targetPort = intstr.FromInt32(servicePort.Port)
				}
				if targetPort.Type != intstr.Int || !declared[targetPort.IntVal] {
					continue
				}
			}
			entries = append(entries, host+":"+strconv.Itoa(int(servicePort.Port)))
		}
	}
	return entries, nil
}

// errZettaEgressTooLong reports an allowlist too long for one environment variable.
var errZettaEgressTooLong = errors.New("the Zetta egress allowlist is longer than one environment variable may be")

// joinZettaEgressEntries is the sorted, de-duplicated list, comma-separated so the same paths
// always give the same value and never roll the StatefulSet. A list too long to start the
// container is refused in favour of deny, with errZettaEgressTooLong: the policy still holds, and
// nothing is widened.
func joinZettaEgressEntries(entries []string) (string, error) {
	sort.Strings(entries)
	value := strings.Join(sortedUnique(entries), ",")
	if len(value) > maxZettaEgressAllowBytes {
		return zettaEgressDeny, fmt.Errorf("%w (%d bytes, %d entries); the engine denies all egress", errZettaEgressTooLong, len(value), len(entries))
	}
	return value, nil
}
