/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// A Lake-owned Zetta's named egress through the workspace egress gateway (operator#1179, zetta#1331,
// zetta PR #1516).
//
// A DNS-name EgressRule is served by the namespace's egress gateway (zetta-tunnel-egress, port 7445,
// egress_gateway.go): the network policy opens the gateway, never the destination. Zetta now sends
// such a name through the gateway when it is told:
//
//   - ZETTA_EGRESS_GATEWAY: the gateway's HOST:PORT;
//   - ZETTA_EGRESS_GATEWAY_ROUTES: the destination names that go through it. This is its own list,
//     because ZETTA_EGRESS_ALLOW also lists in-cluster Connection names, which must stay direct.
//
// The Lake reconcilers render both from the namespace's name rules that apply to the Zetta (no
// source, or source the Zetta's service) and whose allowance the NetworkSandbox carries, and write
// them into the cluster's egress ConfigMap (keys gateway and gatewayRoutes) beside the allowlist.
// A namespace with no such rule gets neither. The OramaZettaCluster reconciler puts them in the node's
// environment only for an image whose usage lists both variables (status.imageFeatures
// .egressGateway). Zetta reads the routes from its environment only, so a route change rolls the
// nodes. The names stay in ZETTA_EGRESS_ALLOW too: the engine checks the allowlist first.

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// The engine's variables, and what its usage lists when it has them.
	zettaEgressGatewayEnv       = "ZETTA_EGRESS_GATEWAY"
	zettaEgressGatewayRoutesEnv = "ZETTA_EGRESS_GATEWAY_ROUTES"
	// The egress ConfigMap's keys for them.
	zettaEgressGatewayKey       = "gateway"
	zettaEgressGatewayRoutesKey = "gatewayRoutes"
)

// zettaEgressGateway is what a Lake-owned Zetta is told about the namespace's gateway.
type zettaEgressGateway struct {
	// address is HOST:PORT; empty when routes is.
	address string
	// routes are the destination names, name:port, comma-joined and sorted.
	routes string
}

// lakeZettaEgressGateway renders the gateway and its routes for a Zetta in a workspace namespace.
func lakeZettaEgressGateway(ctx context.Context, reader client.Reader, namespace, serviceID string) (zettaEgressGateway, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return zettaEgressGateway{}, nil
		}
		return zettaEgressGateway{}, fmt.Errorf("read the NetworkSandbox for the Zetta egress gateway routes: %w", err)
	}
	rendered := make(map[string]bool, len(sandbox.Spec.Policies))
	for _, policy := range sandbox.Spec.Policies {
		rendered[policy.Name] = true
	}
	var rules oramav1alpha1.EgressRuleList
	if err := reader.List(ctx, &rules, client.InNamespace(namespace)); err != nil {
		return zettaEgressGateway{}, fmt.Errorf("list EgressRules for the Zetta egress gateway routes: %w", err)
	}
	var routes []string
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.DNSName == "" || rule.Spec.RuleID == "" || rule.Spec.Desired != oramav1alpha1.EgressRuleDesiredPresent ||
			!rule.DeletionTimestamp.IsZero() || validateEgressRuleSpec(&rule.Spec) != nil ||
			!rendered[egressRuleAllowanceNames(rule)[0]] || !zettaEgressSourceSelects(rule.Spec.SourceKind, rule.Spec.SourceID, serviceID) {
			continue
		}
		for _, port := range sortedUniqueInt32(sortedInt32(append([]int32(nil), rule.Spec.Ports...))) {
			routes = append(routes, rule.Spec.DNSName+":"+strconv.Itoa(int(port)))
		}
	}
	if len(routes) == 0 {
		return zettaEgressGateway{}, nil
	}
	joined, err := joinZettaEgressEntries(routes)
	if err != nil {
		return zettaEgressGateway{}, err
	}
	return zettaEgressGateway{
		address: egressGatewayAddress(namespace) + ":" + strconv.Itoa(egressGatewayPort),
		routes:  joined,
	}, nil
}

// workspaceZettaEgressGateway is lakeZettaEgressGateway, or, when the namespace cannot be read, what
// the cluster's ConfigMap already says, so a failed read never changes the routes.
func workspaceZettaEgressGateway(
	ctx context.Context,
	reader client.Reader,
	cluster *oramav1alpha1.OramaZettaCluster,
	namespace, serviceID string,
) (zettaEgressGateway, error) {
	gateway, err := lakeZettaEgressGateway(ctx, reader, namespace, serviceID)
	if err == nil {
		return gateway, nil
	}
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: zettaEgressConfigMapName(cluster.Name)}
	if getErr := reader.Get(ctx, key, &configMap); getErr == nil {
		return zettaEgressGateway{
			address: configMap.Data[zettaEgressGatewayKey],
			routes:  configMap.Data[zettaEgressGatewayRoutesKey],
		}, err
	}
	return zettaEgressGateway{}, err
}
