/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func policyPorts(t *testing.T, ports []networkingv1.NetworkPolicyPort) []int32 {
	t.Helper()
	var out []int32
	for _, port := range ports {
		if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil || port.EndPort != nil {
			t.Fatalf("port %#v is not one TCP port", port)
		}
		out = append(out, port.Port.IntVal)
	}
	return out
}

func selects(t *testing.T, selector metav1.LabelSelector, podLabels map[string]string) bool {
	t.Helper()
	parsed, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Matches(labels.Set(podLabels))
}

func TestWorkspaceAPIServerTarget(t *testing.T) {
	target, err := workspaceAPIServerTargetFrom(testKubernetesService(), testKubernetesEndpoints("20.237.72.161", 443))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.cidrs, []string{"10.0.0.1/32", "20.237.72.161/32"}) ||
		!reflect.DeepEqual(target.ports, []int32{443}) {
		t.Fatalf("AKS target = %+v", target)
	}

	// A self-hosted API server serves on 6443 behind the Service's 443.
	endpoints := testKubernetesEndpoints("192.168.123.175", 6443)
	endpoints.Subsets[0].NotReadyAddresses = []corev1.EndpointAddress{{IP: "fd00::5"}}
	target, err = workspaceAPIServerTargetFrom(testKubernetesService(), endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.cidrs, []string{"10.0.0.1/32", "192.168.123.175/32", "fd00::5/128"}) ||
		!reflect.DeepEqual(target.ports, []int32{443, 6443}) {
		t.Fatalf("self-hosted target = %+v", target)
	}

	// No endpoint address must never become an egress rule with no destination.
	if _, err := workspaceAPIServerTargetFrom(testKubernetesService(), &corev1.Endpoints{}); err == nil {
		t.Fatal("an empty endpoint list produced a target")
	}
}

// TestWorkspaceNetworkSandboxPreservesLegacyPolicyNamesAndSpecs is the live-migration guard for
// issue #465: moving policy ownership under NetworkSandbox must not change what an existing
// policy allows. The expected objects are rendered by the pre-sandbox policy functions.
// Review separately authorized six new Lake-only service ingress policies, exhaustively checked
// by TestWorkspaceServiceIngressPairsWithLakeEgress; no legacy policy comparison is removed.
func TestWorkspaceNetworkSandboxPreservesLegacyPolicyNamesAndSpecs(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	lake.Spec.Workspaces.Overrides = map[string]oramav1alpha1.LakeWorkspaceOverrideSpec{
		testWorkspaceID: {InternetEgress: true},
	}
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	// internetEgress is now the EgressRule internet-https (operator#470): its controller renders
	// allow-internet-https, which must stay exactly the legacy policy below, and a second workspace
	// pass must keep it.
	renderWorkspaceEgressRules(t, kube, testWorkspaceName)
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)

	labels := workspaceLabels(lake, &store.rows[0])
	target, err := workspaceAPIServerTargetFrom(testKubernetesService(), testKubernetesEndpoints("20.237.72.161", 443))
	if err != nil {
		t.Fatal(err)
	}
	expected := []*networkingv1.NetworkPolicy{
		workspaceDefaultDenyPolicy(testWorkspaceName, labels),
		workspaceInternalPolicy(testWorkspaceName, labels),
		workspaceLakeIngressPolicy(lake, "orama-system", testWorkspaceName, labels),
		workspaceDNSPolicy(testWorkspaceName, labels),
		workspaceLakeEgressPolicy(lake, testWorkspaceName, labels),
	}
	for _, allowance := range workspaceOperatorIngresses {
		expected = append(expected, workspaceOperatorIngressPolicy(allowance, testWorkspaceName, labels))
	}
	// operator#469: the operator signs in to workspace Postgres to keep credential roles.
	expected = append(expected, workspaceOperatorPostgresPolicy("orama-system", testWorkspaceName, labels))
	// operator#1172: the operator lists Valkey and Kafka users with its own identity.
	for _, allowance := range workspaceOperatorAuditIngresses {
		expected = append(expected, workspaceOperatorAuditIngressPolicy(allowance, "orama-system", testWorkspaceName, labels))
	}
	for _, apiClient := range workspaceAPIServerClients {
		expected = append(expected, workspaceAPIServerEgressPolicy(apiClient, target, testWorkspaceName, labels))
	}
	internetPolicy, err := reconciler.workspaceInternetPolicy(ctx, testWorkspaceName, labels)
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, internetPolicy)

	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	wantAllowances := len(expected) - 1 + 6
	if len(sandbox.Spec.Policies) != wantAllowances {
		t.Fatalf("sandbox allowances = %d, want %d", len(sandbox.Spec.Policies), wantAllowances)
	}
	comments := map[string]string{}
	for _, allowance := range sandbox.Spec.Policies {
		comments[allowance.Name] = allowance.Comment
	}
	for _, want := range expected {
		var got networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: want.Name}, &got); err != nil {
			t.Fatalf("policy %s: %v", want.Name, err)
		}
		if !reflect.DeepEqual(got.Spec, want.Spec) {
			t.Errorf("policy %s spec changed\ngot:  %#v\nwant: %#v", want.Name, got.Spec, want.Spec)
		}
		if want.Name != networkSandboxDefaultDenyName && comments[want.Name] == "" {
			t.Errorf("policy %s has no component comment", want.Name)
		}
	}
}

func TestLakeWorkspaceDatastorePolicies(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)

	get := func(name string) networkingv1.NetworkPolicy {
		t.Helper()
		var policy networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: name}, &policy); err != nil {
			t.Fatalf("policy %s: %v", name, err)
		}
		if policy.Labels[workspaceLabel] != testWorkspaceID {
			t.Fatalf("policy %s labels = %v", name, policy.Labels)
		}
		return policy
	}

	// The CNPG pods from a live workspace namespace: the instance and its initdb Job.
	instance := map[string]string{
		"cnpg.io/cluster": "prod-svc-f85dc7d2ac61", "cnpg.io/podRole": "instance",
		"cnpg.io/instanceName": "prod-svc-f85dc7d2ac61-1", "role": "primary",
	}
	initdb := map[string]string{"cnpg.io/cluster": "prod-svc-f85dc7d2ac61", "cnpg.io/jobRole": "initdb"}
	zetta := map[string]string{"app.kubernetes.io/name": "orama-zetta", "orama.cloud/service-kind": "zetta"}
	app := map[string]string{"app.kubernetes.io/name": "lake-app", "app.kubernetes.io/managed-by": "orama-operator"}

	apiEgress := get("allow-cnpg-apiserver-egress")
	if !selects(t, apiEgress.Spec.PodSelector, instance) || !selects(t, apiEgress.Spec.PodSelector, initdb) ||
		selects(t, apiEgress.Spec.PodSelector, zetta) || selects(t, apiEgress.Spec.PodSelector, app) {
		t.Fatalf("CNPG API egress selects %#v", apiEgress.Spec.PodSelector)
	}
	if !reflect.DeepEqual(apiEgress.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) ||
		len(apiEgress.Spec.Ingress) != 0 || len(apiEgress.Spec.Egress) != 1 {
		t.Fatalf("CNPG API egress spec = %#v", apiEgress.Spec)
	}
	var cidrs []string
	for _, peer := range apiEgress.Spec.Egress[0].To {
		if peer.IPBlock == nil || len(peer.IPBlock.Except) != 0 || peer.PodSelector != nil || peer.NamespaceSelector != nil {
			t.Fatalf("CNPG API egress peer = %#v", peer)
		}
		cidrs = append(cidrs, peer.IPBlock.CIDR)
	}
	if !reflect.DeepEqual(cidrs, []string{"10.0.0.1/32", "20.237.72.161/32"}) ||
		!reflect.DeepEqual(policyPorts(t, apiEgress.Spec.Egress[0].Ports), []int32{443}) {
		t.Fatalf("CNPG API egress = %v on %v", cidrs, apiEgress.Spec.Egress[0].Ports)
	}

	ingress := get("allow-cnpg-operator-ingress")
	if !selects(t, ingress.Spec.PodSelector, instance) || selects(t, ingress.Spec.PodSelector, initdb) ||
		selects(t, ingress.Spec.PodSelector, zetta) {
		t.Fatalf("CNPG operator ingress selects %#v", ingress.Spec.PodSelector)
	}
	if !reflect.DeepEqual(ingress.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) ||
		len(ingress.Spec.Egress) != 0 || len(ingress.Spec.Ingress) != 1 || len(ingress.Spec.Ingress[0].From) != 1 {
		t.Fatalf("CNPG operator ingress spec = %#v", ingress.Spec)
	}
	from := ingress.Spec.Ingress[0].From[0]
	if from.IPBlock != nil || from.NamespaceSelector == nil || from.PodSelector == nil ||
		!reflect.DeepEqual(from.NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": "cnpg-system"}) ||
		!selects(t, *from.PodSelector, map[string]string{"app.kubernetes.io/name": "cloudnative-pg", "app.kubernetes.io/instance": "cnpg"}) {
		t.Fatalf("CNPG operator peer = %#v", from)
	}
	if !reflect.DeepEqual(policyPorts(t, ingress.Spec.Ingress[0].Ports), []int32{8000, 5432}) {
		t.Fatalf("CNPG operator ports = %v", ingress.Spec.Ingress[0].Ports)
	}

	// Every other kind: the operand pod each policy must select, the operator namespace and pod,
	// and the ports.
	for _, tc := range []struct {
		name      string
		pod       map[string]string
		namespace string
		operator  map[string]string
		ports     []int32
	}{
		{"allow-clickhouse-operator-ingress",
			map[string]string{"clickhouse.altinity.com/chi": "svc", "clickhouse.altinity.com/app": "chop"},
			"clickhouse-system", map[string]string{"app.kubernetes.io/name": "altinity-clickhouse-operator"}, []int32{8123}},
		{"allow-clickhouse-keeper-operator-ingress",
			map[string]string{"clickhouse-keeper.altinity.com/chk": "svc", "clickhouse-keeper.altinity.com/app": "chop"},
			"clickhouse-system", map[string]string{"app.kubernetes.io/name": "altinity-clickhouse-operator"}, []int32{2181}},
		{"allow-tidb-pd-operator-ingress",
			map[string]string{"app.kubernetes.io/component": "pd", "app.kubernetes.io/managed-by": "tidb-operator"},
			"tidb-admin", map[string]string{"app.kubernetes.io/name": "tidb-operator", "app.kubernetes.io/component": "controller-manager"}, []int32{2379}},
		{"allow-tidb-tikv-operator-ingress",
			map[string]string{"app.kubernetes.io/component": "tikv", "app.kubernetes.io/managed-by": "tidb-operator"},
			"tidb-admin", map[string]string{"app.kubernetes.io/name": "tidb-operator", "app.kubernetes.io/component": "controller-manager"}, []int32{20180}},
		{"allow-tidb-operator-ingress",
			map[string]string{"app.kubernetes.io/component": "tidb", "app.kubernetes.io/managed-by": "tidb-operator"},
			"tidb-admin", map[string]string{"app.kubernetes.io/name": "tidb-operator", "app.kubernetes.io/component": "controller-manager"}, []int32{4000, 10080}},
		{"allow-cassandra-operator-ingress",
			map[string]string{"cassandra.datastax.com/cluster": "svc", "cassandra.datastax.com/datacenter": "svc"},
			"k8ssandra-operator", map[string]string{"app.kubernetes.io/name": "cass-operator"}, []int32{8080}},
		{"allow-valkey-operator-ingress",
			map[string]string{"valkey.io/cluster": "svc", "app.kubernetes.io/managed-by": "valkey-operator"},
			"valkey-operator-system", map[string]string{"app.kubernetes.io/name": "valkey-operator"}, []int32{6379}},
	} {
		policy := get(tc.name)
		if !selects(t, policy.Spec.PodSelector, tc.pod) || selects(t, policy.Spec.PodSelector, zetta) ||
			selects(t, policy.Spec.PodSelector, app) {
			t.Errorf("%s selects %#v", tc.name, policy.Spec.PodSelector)
			continue
		}
		if len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
			t.Errorf("%s spec = %#v", tc.name, policy.Spec)
			continue
		}
		from := policy.Spec.Ingress[0].From[0]
		if from.NamespaceSelector == nil || from.PodSelector == nil ||
			from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != tc.namespace ||
			!selects(t, *from.PodSelector, tc.operator) || selects(t, *from.PodSelector, app) {
			t.Errorf("%s peer = %#v", tc.name, from)
		}
		if got := policyPorts(t, policy.Spec.Ingress[0].Ports); !reflect.DeepEqual(got, tc.ports) {
			t.Errorf("%s ports = %v, want %v", tc.name, got, tc.ports)
		}
	}
	// Both K8ssandra Deployments call the management API.
	cassandra := get("allow-cassandra-operator-ingress").Spec.Ingress[0].From[0].PodSelector
	if !selects(t, *cassandra, map[string]string{"app.kubernetes.io/name": "k8ssandra-operator"}) {
		t.Error("k8ssandra-operator cannot reach the Cassandra management API")
	}

	for name, tc := range map[string]struct{ in, out map[string]string }{
		"allow-strimzi-apiserver-egress": {
			in: map[string]string{
				"strimzi.io/kind": "Kafka", "strimzi.io/cluster": "svc",
				"strimzi.io/component-type": "entity-operator", "strimzi.io/name": "svc-entity-operator",
			},
			out: map[string]string{"strimzi.io/kind": "Kafka", "strimzi.io/component-type": "kafka"},
		},
		"allow-tidb-discovery-apiserver-egress": {
			in:  map[string]string{"app.kubernetes.io/component": "discovery", "app.kubernetes.io/managed-by": "tidb-operator"},
			out: map[string]string{"app.kubernetes.io/component": "pd", "app.kubernetes.io/managed-by": "tidb-operator"},
		},
	} {
		policy := get(name)
		if !selects(t, policy.Spec.PodSelector, tc.in) || selects(t, policy.Spec.PodSelector, tc.out) {
			t.Errorf("%s selects %#v", name, policy.Spec.PodSelector)
		}
		if !reflect.DeepEqual(policy.Spec.Egress, apiEgress.Spec.Egress) {
			t.Errorf("%s egress = %#v", name, policy.Spec.Egress)
		}
	}

	// No workspace policy may allow every source or every destination, nor select all pods,
	// except the base policies, which allow only same-namespace peers.
	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies, client.InNamespace(testWorkspaceName)); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies.Items {
		for _, rule := range policy.Spec.Ingress {
			if len(rule.From) == 0 {
				t.Errorf("%s allows ingress from everywhere", policy.Name)
			}
		}
		for _, rule := range policy.Spec.Egress {
			if len(rule.To) == 0 {
				t.Errorf("%s allows egress to everywhere", policy.Name)
			}
		}
	}
}

func TestLakeWorkspaceAPIEgressWaitsForEndpoints(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	empty := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}}
	reconciler, kube := newWorkspaceHarness(t, lake, empty)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	if changing := reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows); !changing {
		t.Fatal("a workspace without the API address did not retry")
	}
	for _, apiClient := range workspaceAPIServerClients {
		key := client.ObjectKey{Namespace: testWorkspaceName, Name: apiClient.name}
		if err := kube.Get(ctx, key, &networkingv1.NetworkPolicy{}); client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("policy %s exists without an API address (err %v)", apiClient.name, err)
		}
	}
	var deny networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceDefaultDenyName}, &deny); err != nil {
		t.Fatalf("default-deny was not installed: %v", err)
	}
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: "allow-cnpg-operator-ingress"}
	if err := kube.Get(ctx, key, &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("operator ingress does not depend on the API address: %v", err)
	}
}
