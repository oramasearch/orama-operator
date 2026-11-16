// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The datastore operators that run lake's managed services reconcile pods inside a workspace
// namespace, which is default-deny (operator#268). Each allowance below is one flow an operator or
// an operand cannot work without, selected by the operand's own pod labels and the operator's
// namespace and pod labels. The namespaces are the ones helmfile.yaml installs into, and the pod
// labels are the ones those charts put on the operator Deployments.

// workspaceOperatorPeer selects a datastore operator's pods in its own namespace.
type workspaceOperatorPeer struct {
	namespace string
	pods      metav1.LabelSelector
}

var (
	workspaceCNPGOperator = workspaceOperatorPeer{
		namespace: "cnpg-system",
		pods: metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name": "cloudnative-pg",
		}},
	}
	workspaceAltinityOperator = workspaceOperatorPeer{
		namespace: "clickhouse-system",
		pods: metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name": "altinity-clickhouse-operator",
		}},
	}
	workspaceTiDBOperator = workspaceOperatorPeer{
		namespace: "tidb-admin",
		pods: metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name":      "tidb-operator",
			"app.kubernetes.io/component": "controller-manager",
		}},
	}
	// The K8ssandra chart runs two Deployments that both call the Cassandra management API:
	// cass-operator starts and drains the nodes, k8ssandra-operator manages keyspaces.
	workspaceK8ssandraOperator = workspaceOperatorPeer{
		namespace: "k8ssandra-operator",
		pods: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "app.kubernetes.io/name",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"cass-operator", "k8ssandra-operator"},
		}}},
	}
	workspaceValkeyOperator = workspaceOperatorPeer{
		namespace: "valkey-operator-system",
		pods: metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name": "valkey-operator",
		}},
	}
)

// workspaceOperatorIngress lets one datastore operator reach one kind of operand pod on the ports
// it calls.
type workspaceOperatorIngress struct {
	name    string
	comment string
	pods    metav1.LabelSelector
	from    workspaceOperatorPeer
	ports   []int32
}

func labelExists(key string) metav1.LabelSelectorRequirement {
	return metav1.LabelSelectorRequirement{Key: key, Operator: metav1.LabelSelectorOpExists}
}

func tidbComponentPods(component string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/component": component,
		tidbManagedByLabel:            tidbManagedBy,
	}}
}

// workspaceOperatorIngresses is every operator-to-operand flow, one NetworkPolicy each.
var workspaceOperatorIngresses = []workspaceOperatorIngress{
	{
		// Status extraction, promotion and switchover go to the instance manager's status port;
		// the operator also connects to Postgres itself.
		name:    "allow-cnpg-operator-ingress",
		comment: "CloudNativePG needs the instance-manager status port and Postgres port to manage workspace Postgres clusters.",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{"cnpg.io/podRole": "instance"},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists("cnpg.io/cluster")},
		},
		from:  workspaceCNPGOperator,
		ports: []int32{8000, 5432},
	},
	{
		// The operator queries every ClickHouse host over HTTP to decide when it is ready and to
		// propagate schema.
		name:    "allow-clickhouse-operator-ingress",
		comment: "The Altinity operator needs ClickHouse HTTP to observe readiness and propagate schema.",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{clickHouseAppLabel: clickHouseAppValue},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(clickHouseInstallationLabel)},
		},
		from:  workspaceAltinityOperator,
		ports: []int32{clickHouseHTTPPort},
	},
	{
		// A replicated installation points at a Keeper; the operator creates the cluster's root
		// path there over the ZooKeeper protocol.
		name:    "allow-clickhouse-keeper-operator-ingress",
		comment: "The Altinity operator needs the Keeper client port to create replicated ClickHouse paths.",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{clickHouseKeeperAppLabel: clickHouseAppValue},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(clickHouseKeeperLabel)},
		},
		from:  workspaceAltinityOperator,
		ports: []int32{clickHouseKeeperClientPort},
	},
	{
		// PD's client API: members, health and store state.
		name:    "allow-tidb-pd-operator-ingress",
		comment: "The TiDB operator needs PD's client API for member, health and store state.",
		pods:    tidbComponentPods("pd"),
		from:    workspaceTiDBOperator,
		ports:   []int32{tidbPDClientPort},
	},
	{
		// TiKV's status port: leader counts during an upgrade.
		name:    "allow-tidb-tikv-operator-ingress",
		comment: "The TiDB operator needs TiKV's status port to count leaders during upgrades.",
		pods:    tidbComponentPods("tikv"),
		from:    workspaceTiDBOperator,
		ports:   []int32{tidbTiKVStatusPort},
	},
	{
		// TiDB's status port for health, and its SQL port, where the operator sets the root
		// password that createPassword asks for.
		name:    "allow-tidb-operator-ingress",
		comment: "The TiDB operator needs TiDB SQL and status ports for health and root-password setup.",
		pods:    tidbComponentPods("tidb"),
		from:    workspaceTiDBOperator,
		ports:   []int32{tidbPort, tidbStatusPort},
	},
	{
		// The Cassandra management API. cass-operator starts every node through it
		// (MGMT_API_EXPLICIT_START), so a node never starts without this flow.
		name:    "allow-cassandra-operator-ingress",
		comment: "The K8ssandra operators need the Cassandra management API to start, drain and manage nodes.",
		pods: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(cassandraClusterLabel)},
		},
		from:  workspaceK8ssandraOperator,
		ports: []int32{cassandraManagementPort},
	},
	{
		// The operator forms the cluster (CLUSTER MEET, slot assignment, replication) and reads
		// its state over the Valkey port of each node.
		name:    "allow-valkey-operator-ingress",
		comment: "The Valkey operator needs each node's Valkey port to form and observe the cluster.",
		pods: metav1.LabelSelector{
			MatchLabels:      map[string]string{workspaceManagedByLabel: valkeyManagedBy},
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(valkeyClusterLabel)},
		},
		from:  workspaceValkeyOperator,
		ports: []int32{valkeyPort},
	},
}

// workspaceOramaOperatorIngresses are the flows the Orama operator itself opens into a workspace's
// datastores, one NetworkPolicy each, from its own pods in its own namespace (the reconciler's
// OperatorNamespace; from is filled in by workspaceOramaOperatorIngressPolicy).
var workspaceOramaOperatorIngresses = []workspaceOperatorIngress{
	{
		// The operator signs in as root over TiDB's SQL port to keep the tenant user
		// (operator#771).
		name:    "allow-orama-operator-tidb-sql",
		comment: "The Orama operator signs in to TiDB as root to keep the tenant user (operator#771).",
		pods:    tidbComponentPods("tidb"),
		ports:   []int32{tidbPort},
	},
	{
		// The operator signs in as the generated superuser over CQL to keep the tenant role; the
		// management API has no GRANT (operator#771).
		name:    "allow-orama-operator-cassandra-cql",
		comment: "The Orama operator signs in to Cassandra as the superuser to keep the tenant role (operator#771).",
		pods: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists(cassandraClusterLabel)},
		},
		ports: []int32{cassandraPort},
	},
}

// workspaceOramaOperatorPeer selects the Orama operator's pods in operatorNamespace.
func workspaceOramaOperatorPeer(operatorNamespace string) workspaceOperatorPeer {
	return workspaceOperatorPeer{
		namespace: operatorNamespace,
		pods:      metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "orama-operator"}},
	}
}

func workspaceOramaOperatorIngressPolicy(allowance workspaceOperatorIngress, operatorNamespace, namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	allowance.from = workspaceOramaOperatorPeer(operatorNamespace)
	return workspaceOperatorIngressPolicy(allowance, namespace, labels)
}

// workspaceAPIServerClient is a kind of operand pod that reads the Kubernetes API with its own
// ServiceAccount.
type workspaceAPIServerClient struct {
	name    string
	comment string
	pods    metav1.LabelSelector
}

// workspaceAPIServerClients is every operand that needs the Kubernetes API, one NetworkPolicy each.
var workspaceAPIServerClients = []workspaceAPIServerClient{
	{
		// The CNPG instance manager, and the initdb and join Jobs, read their Cluster, hold the
		// primary lease and report status through the API.
		name:    "allow-cnpg-apiserver-egress",
		comment: "CloudNativePG instances and bootstrap jobs need the Kubernetes API for their Cluster, leases and status.",
		pods: metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{labelExists("cnpg.io/cluster")},
		},
	},
	{
		// Strimzi's Topic and User Operators watch KafkaTopic and KafkaUser and write the user's
		// credential Secret, which the Kafka service's connection is read from.
		name:    "allow-strimzi-apiserver-egress",
		comment: "Strimzi entity operators need the Kubernetes API for topics, users and credential Secrets.",
		pods: metav1.LabelSelector{MatchLabels: map[string]string{
			strimziKindLabel:            "Kafka",
			"strimzi.io/component-type": "entity-operator",
		}},
	},
	{
		// TiDB's discovery service reads the TidbCluster to tell each PD how to join.
		name:    "allow-tidb-discovery-apiserver-egress",
		comment: "TiDB discovery needs the Kubernetes API to read its TidbCluster and tell PD how to join.",
		pods:    tidbComponentPods("discovery"),
	},
}

const (
	clickHouseKeeperClientPort = 2181
	tidbPDClientPort           = 2379
	tidbStatusPort             = 10080
	tidbTiKVStatusPort         = 20180
	cassandraManagementPort    = 8080
	workspaceAPIServerHTTPS    = 443
)

func workspaceOperatorIngressPolicy(
	allowance workspaceOperatorIngress,
	namespace string,
	labels map[string]string,
) *networkingv1.NetworkPolicy {
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(allowance.ports))
	for _, port := range allowance.ports {
		ports = append(ports, workspaceTCPPort(port))
	}
	operatorPods := allowance.from.pods
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(allowance.name, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: allowance.pods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: namespaceNameSelector(allowance.from.namespace),
					PodSelector:       &operatorPods,
				}},
				Ports: ports,
			}},
		},
	}
}

// workspaceAPIServerTarget is where pods reach the Kubernetes API: the default/kubernetes Service
// address and the addresses and ports behind it.
type workspaceAPIServerTarget struct {
	cidrs []string
	ports []int32
}

// readWorkspaceAPIServerTarget reads the default/kubernetes Service and Endpoints. A CNI may judge
// the connection before or after the Service address is translated, so both the Service address
// and every endpoint address are allowed, on the Service port and on each endpoint port. The
// endpoints are required: an egress rule with no destination would allow every destination.
func (r *LakeWorkspacesReconciler) readWorkspaceAPIServerTarget(ctx context.Context) (workspaceAPIServerTarget, error) {
	return readKubernetesAPIServerTarget(ctx, r.apiReader())
}

func workspaceAPIServerTargetFrom(
	kubernetes *corev1.Service,
	endpoints *corev1.Endpoints,
) (workspaceAPIServerTarget, error) {
	var endpointCIDRs, cidrs []string
	ports := map[int32]bool{}
	for _, port := range kubernetes.Spec.Ports {
		if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
			ports[port.Port] = true
		}
	}
	if len(ports) == 0 {
		ports[workspaceAPIServerHTTPS] = true
	}
	for _, subset := range endpoints.Subsets {
		for _, address := range append(subset.Addresses, subset.NotReadyAddresses...) {
			endpointCIDRs = appendAddressException(endpointCIDRs, address.IP)
		}
		for _, port := range subset.Ports {
			if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
				ports[port.Port] = true
			}
		}
	}
	if len(endpointCIDRs) == 0 {
		return workspaceAPIServerTarget{}, fmt.Errorf("the Kubernetes API endpoints list no address")
	}
	cidrs = append(cidrs, endpointCIDRs...)
	for _, ip := range append([]string{kubernetes.Spec.ClusterIP}, kubernetes.Spec.ClusterIPs...) {
		cidrs = appendAddressException(cidrs, ip)
	}
	sort.Strings(cidrs)
	target := workspaceAPIServerTarget{cidrs: sortedUnique(cidrs)}
	for port := range ports {
		target.ports = append(target.ports, port)
	}
	sort.Slice(target.ports, func(i, j int) bool { return target.ports[i] < target.ports[j] })
	return target, nil
}

func workspaceAPIServerEgressPolicy(
	apiClient workspaceAPIServerClient,
	target workspaceAPIServerTarget,
	namespace string,
	labels map[string]string,
) *networkingv1.NetworkPolicy {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(target.cidrs))
	for _, cidr := range target.cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(target.ports))
	for _, port := range target.ports {
		ports = append(ports, workspaceTCPPort(port))
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: workspacePolicyMeta(apiClient.name, namespace, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: apiClient.pods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers, Ports: ports}},
		},
	}
}

// reconcileWorkspaceAPIServerPolicies writes one egress policy per API client. When the API
// address cannot be read, the policies already in place stay as they are and the error is
// returned, so the workspace retries.
func (r *LakeWorkspacesReconciler) reconcileWorkspaceAPIServerPolicies(
	ctx context.Context,
	namespace string,
	labels map[string]string,
) error {
	target, err := r.readWorkspaceAPIServerTarget(ctx)
	if err != nil {
		return fmt.Errorf("workspace Kubernetes API egress: %w", err)
	}
	for _, apiClient := range workspaceAPIServerClients {
		if err := r.reconcileWorkspaceNetworkPolicy(ctx, labels, workspaceAPIServerEgressPolicy(apiClient, target, namespace, labels)); err != nil {
			return err
		}
	}
	return nil
}
