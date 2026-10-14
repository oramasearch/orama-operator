/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// These are the client endpoints projected into Lake's services Secret. Both isolated ends
// use the same vendor pod selectors and ports; operator-management ports are not client ports.
var workspaceServicePaths = []struct {
	kind  string
	pods  metav1.LabelSelector
	ports []int32
}{
	{
		kind: "postgres",
		pods: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists("cnpg.io/cluster")},
		},
		ports: []int32{servicePostgresPort},
	},
	{
		kind: "kafka",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{strimziKindLabel: "Kafka"},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(strimziClusterLabel)},
		},
		ports: []int32{kafkaListenerPort},
	},
	{
		kind: "clickhouse",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{clickHouseAppLabel: clickHouseAppValue},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(clickHouseInstallationLabel)},
		},
		ports: []int32{clickHouseHTTPPort, clickHouseNativePort},
	},
	{
		kind:  "tidb",
		pods:  tidbComponentPods("tidb"),
		ports: []int32{tidbPort},
	},
	{
		kind: "cassandra",
		pods: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(cassandraClusterLabel)},
		},
		ports: []int32{cassandraPort},
	},
	{
		kind: "valkey",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{workspaceManagedByLabel: valkeyManagedBy},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(valkeyClusterLabel)},
		},
		ports: []int32{valkeyPort},
	},
}

func workspaceLakeServiceIngressAllowances(lake *oramav1alpha1.Lake) []oramav1alpha1.NetworkSandboxPolicy {
	lakePods := lakePodSelector(lake)
	peer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: namespaceNameSelector(lakeNamespace(lake)),
		PodSelector:       &lakePods,
	}
	policies := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(workspaceServicePaths))
	for _, path := range workspaceServicePaths {
		policies = append(policies, sandboxAllowance(
			"The owning Lake needs workspace "+path.kind+" client ingress for Data / Connections and service_connection checks using its services Secret.",
			networkPolicy(
				"allow-lake-service-"+path.kind+"-ingress",
				path.pods,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{peer}, Ports: tcpPorts(path.ports...)}},
				nil,
			),
		))
	}
	return policies
}
