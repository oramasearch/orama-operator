/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testEgressSourcelessRuleID = "d0000000-0000-4000-8000-00000000000d"
	testEgressOtherRuleID      = "e0000000-0000-4000-8000-00000000000e"
	testEgressFailedRuleID     = "f0000000-0000-4000-8000-00000000000f"
)

func zettaEgressAllow(t *testing.T, reader client.Reader, namespace, serviceID string) string {
	t.Helper()
	allow, err := lakeZettaEgressAllow(context.Background(), reader, namespace, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return allow
}

// A Zetta's allowlist is what its workspace's NetworkSandbox renders for it (operator#724): every
// EgressRule with no source or with the Zetta as its source, once the rule's allowance is in the
// sandbox, and nothing for a rule that selects another workload, failed to render, or is absent.
func TestZettaEgressMirrorsTheRulesTheSandboxRendered(t *testing.T) {
	f := newEgressFixture(t)
	if got := zettaEgressAllow(t, f.kube, testEgressNamespace, testEgressSourceID); got != "" {
		t.Fatalf("a namespace with no allowances gave %q, want deny", got)
	}

	nameRuleName := lakeEgressRuleName(testEgressNameRuleID)
	f.add(t, nameRuleName, nameRule())
	f.add(t, lakeEgressRuleName(testEgressCIDRRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressCIDRRuleID, SourceKind: oramav1alpha1.EgressRuleSourceService, SourceID: testEgressSourceID,
		CIDR: "203.0.113.7/32", Ports: []int32{5432},
	})
	f.add(t, lakeEgressRuleName(testEgressSourcelessRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressSourcelessRuleID, CIDR: "2001:db8::/32", Ports: []int32{8443, 443},
	})
	f.add(t, lakeEgressRuleName(testEgressOtherRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressOtherRuleID, SourceKind: oramav1alpha1.EgressRuleSourceService, SourceID: testConnectionTargetID,
		CIDR: "198.51.100.0/24", Ports: []int32{443},
	})
	// Inside the pod range: the controller refuses it, so the sandbox never carries it.
	f.add(t, lakeEgressRuleName(testEgressFailedRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressFailedRuleID, CIDR: "10.244.0.16/28", Ports: []int32{80},
	})
	f.add(t, workspaceInternetRuleName, oramav1alpha1.EgressRuleSpec{PublicInternet: true, Ports: []int32{443}})
	for _, name := range []string{
		nameRuleName, lakeEgressRuleName(testEgressCIDRRuleID), lakeEgressRuleName(testEgressSourcelessRuleID),
		lakeEgressRuleName(testEgressOtherRuleID), lakeEgressRuleName(testEgressFailedRuleID), workspaceInternetRuleName,
	} {
		f.reconcile(t, name)
	}
	if rule := f.reconcile(t, lakeEgressRuleName(testEgressFailedRuleID)); rule.Status.Phase != oramav1alpha1.EgressRulePhaseFailed {
		t.Fatalf("the pod-range rule is %q, want Failed", rule.Status.Phase)
	}

	want := "*:443,203.0.113.7/32:5432,[2001:db8::/32]:443,[2001:db8::/32]:8443,api.example.com:443,api.example.com:8443"
	if got := zettaEgressAllow(t, f.kube, testEgressNamespace, testEgressSourceID); got != want {
		t.Fatalf("Zetta allowlist\n got %q\nwant %q", got, want)
	}
	// A Zetta that is not the source of any rule gets only the workspace-wide ones.
	if got := zettaEgressAllow(t, f.kube, testEgressNamespace, ""); got != "*:443,[2001:db8::/32]:443,[2001:db8::/32]:8443" {
		t.Fatalf("workspace-wide allowlist = %q", got)
	}

	// An absent rule leaves the sandbox, and the allowlist with it.
	var rule oramav1alpha1.EgressRule
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: nameRuleName}, &rule); err != nil {
		t.Fatal(err)
	}
	rule.Spec.Desired = oramav1alpha1.EgressRuleDesiredAbsent
	rule.Spec.Generation = 2
	if err := f.kube.Update(f.ctx, &rule); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, nameRuleName)
	if got := zettaEgressAllow(t, f.kube, testEgressNamespace, testEgressSourceID); strings.Contains(got, "api.example.com") {
		t.Fatalf("an absent rule is still allowed: %q", got)
	}
}

// A Connection from the Zetta opens its target's Services by cluster name, on the Service ports
// that lead to a pod port the Connection opens. The namespace's same-namespace allowance, while it
// lasts, is every name in the namespace.
func TestZettaEgressMirrorsTheZettasConnections(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake, sandbox, connection := ownedConnectionObjects(t)
	connection.Spec.Ports = []int32{5432, 8080}
	target := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: connection.Namespace, Labels: map[string]string{serviceIDLabel: testConnectionTargetID}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "postgres", Port: 5432},
			{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)},
			{Name: "metrics", Port: 9187, TargetPort: intstr.FromInt32(9187)},
			{Name: "named", Port: 7000, TargetPort: intstr.FromString("named")},
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(lake, sandbox, connection, target).Build()
	internal := "*.workspace.svc.cluster.local"
	if got := zettaEgressAllow(t, kube, "workspace", testConnectionSourceID); got != internal {
		t.Fatalf("before the Connection is rendered: %q, want only %q", got, internal)
	}
	reconciler := &ConnectionReconciler{Client: kube, Scheme: scheme}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}); err != nil {
		t.Fatal(err)
	}
	want := internal + ",target.workspace.svc.cluster.local:5432,target.workspace.svc.cluster.local:80"
	if got := zettaEgressAllow(t, kube, "workspace", testConnectionSourceID); got != want {
		t.Fatalf("Zetta allowlist\n got %q\nwant %q", got, want)
	}
	if got := zettaEgressAllow(t, kube, "workspace", testEgressSourceID); got != internal {
		t.Fatalf("another Zetta gained the Connection: %q", got)
	}

	// With no declared ports the Connection opens every Service port.
	var stored oramav1alpha1.Connection
	if err := kube.Get(ctx, client.ObjectKeyFromObject(connection), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Ports = nil
	if err := kube.Update(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	want = internal + ",target.workspace.svc.cluster.local:5432,target.workspace.svc.cluster.local:7000," +
		"target.workspace.svc.cluster.local:80,target.workspace.svc.cluster.local:9187"
	if got := zettaEgressAllow(t, kube, "workspace", testConnectionSourceID); got != want {
		t.Fatalf("every-port allowlist\n got %q\nwant %q", got, want)
	}
}

// A range that holds link-local addresses (the cloud metadata address among them) is written
// without them, as the policy renders it; IPv6 ranges are bracketed for the engine's parser.
func TestZettaEgressCutsLinkLocalOutOfARange(t *testing.T) {
	entries := egressRuleZettaEntries(&oramav1alpha1.EgressRuleSpec{CIDR: "0.0.0.0/0", Ports: []int32{80}})
	if len(entries) != 16 {
		t.Fatalf("0.0.0.0/0 minus 169.254.0.0/16 = %d prefixes, want 16: %v", len(entries), entries)
	}
	contains := func(address string) bool {
		for _, entry := range entries {
			prefix := netip.MustParsePrefix(strings.TrimSuffix(entry, ":80"))
			if prefix.Contains(netip.MustParseAddr(address)) {
				return true
			}
		}
		return false
	}
	for _, address := range []string{"169.254.169.254", "169.254.0.1"} {
		if contains(address) {
			t.Errorf("%s is still allowed", address)
		}
	}
	for _, address := range []string{"0.0.0.1", "8.8.8.8", "169.253.255.255", "169.255.0.0", "255.255.255.255"} {
		if !contains(address) {
			t.Errorf("%s was cut out", address)
		}
	}
	six := egressRuleZettaEntries(&oramav1alpha1.EgressRuleSpec{CIDR: "::/0", Ports: []int32{443}})
	for _, entry := range six {
		if !strings.HasPrefix(entry, "[") || !strings.HasSuffix(entry, "]:443") {
			t.Fatalf("IPv6 entry %q is not bracketed", entry)
		}
		prefix := netip.MustParsePrefix(strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]:443"))
		if prefix.Overlaps(netip.MustParsePrefix("fe80::/10")) {
			t.Fatalf("IPv6 entry %q holds link-local addresses", entry)
		}
	}
	if len(six) != 10 {
		t.Fatalf("::/0 minus fe80::/10 = %d prefixes, want 10", len(six))
	}
	if got := egressRuleZettaEntries(&oramav1alpha1.EgressRuleSpec{CIDR: "169.254.169.254/32", Ports: []int32{80}}); len(got) != 0 {
		t.Fatalf("a range inside link-local admits %v", got)
	}
	if got := egressRuleZettaEntries(&oramav1alpha1.EgressRuleSpec{CIDR: "203.0.113.7", Ports: []int32{443, 80}}); strings.Join(got, ",") != "203.0.113.7/32:80,203.0.113.7/32:443" {
		t.Fatalf("a single address = %v", got)
	}
}

// A failed read never changes the running value (that would roll the StatefulSet), and a list too
// long for one environment variable is refused in favour of deny, never widened.
func TestZettaEgressFailsClosedAndNeverFlaps(t *testing.T) {
	scheme := newLakeScheme(t)
	broken := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("API server unavailable")
		},
	}).Build()
	allow, err := workspaceZettaEgressAllow(context.Background(), broken, "workspace", testEgressSourceID, "api.example.com:443", true)
	if err == nil || allow != "api.example.com:443" {
		t.Fatalf("a failed read gave %q, %v; want the running value and the error", allow, err)
	}
	if allow, err = workspaceZettaEgressAllow(context.Background(), broken, "workspace", testEgressSourceID, "", false); err == nil || allow != "" {
		t.Fatalf("a failed read for a new cluster gave %q, %v; want deny", allow, err)
	}

	var entries []string
	for i := 0; len(strings.Join(entries, ",")) <= maxZettaEgressAllowBytes; i++ {
		entries = append(entries, "host-"+strconv.Itoa(i)+"-"+strings.Repeat("x", 40)+".example.com:443")
	}
	if allow, err := joinZettaEgressEntries(entries); allow != "" || !errors.Is(err, errZettaEgressTooLong) {
		t.Fatalf("an over-long list gave %d bytes, %v", len(allow), err)
	}
	if allow, err := joinZettaEgressEntries([]string{"b.example.com:443", "a.example.com:443", "b.example.com:443"}); err != nil ||
		allow != "a.example.com:443,b.example.com:443" {
		t.Fatalf("the list is not sorted and de-duplicated: %q, %v", allow, err)
	}
}

// The data plane's allowlist is deny because its namespace's NetworkSandbox lets its pods reach
// cluster DNS and the nodes of its own cluster and nothing else. A new egress allowance for those
// pods must come with a change to lakeDataPlaneZettaEnv, so this test names the ones there are.
func TestTheLakeDataPlaneZettaIsAllowedNothingOutside(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := serviceTestLake()
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	r := &LakeReconciler{Client: kube, Scheme: scheme, OperatorNamespace: "orama-operator"}
	namespace := lakeNamespace(lake)
	if err := r.reconcileLakeNetworkSandbox(ctx, lake, namespace, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	cluster := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name: lakeZettaName(lake.Name), Labels: withLabels(map[string]string{}, lakeInstancePodLabels(lake)),
	}}
	pod := zettaPodLabels(cluster)
	var egress []string
	for _, policy := range sandbox.Spec.Policies {
		if !selectorMatches(t, policy.Spec.PodSelector, pod) || len(policy.Spec.Egress) == 0 {
			continue
		}
		egress = append(egress, policy.Name)
		if policy.Name == "allow-zetta-internal" {
			for _, rule := range policy.Spec.Egress {
				for _, peer := range rule.To {
					if peer.IPBlock != nil || peer.NamespaceSelector != nil || peer.PodSelector == nil || !selectorMatches(t, *peer.PodSelector, pod) {
						t.Fatalf("allow-zetta-internal reaches beyond the cluster's own nodes: %+v", peer)
					}
				}
			}
		}
	}
	sort.Strings(egress)
	want := []string{"allow-zetta-internal", workspaceDNSName}
	sort.Strings(want)
	if strings.Join(egress, ",") != strings.Join(want, ",") {
		t.Fatalf("data-plane Zetta egress allowances = %v; update lakeDataPlaneZettaEnv with any new destination", egress)
	}
	if got := findEnv(lakeDataPlaneZettaEnv(lake), zettaEgressAllowEnv); got == nil || got.Value != "" {
		t.Fatalf("data-plane ZETTA_EGRESS_ALLOW = %+v, want deny", got)
	}
}

// lake#1097: lake installs ZETTA_EGRESS_ALLOW as the policy of zetta's client, which refuses all
// but loopback without it. The operator gives the lake Deployment one in-cluster allowlist, and a
// spec.env value cannot replace it.
func TestTheLakeDeploymentGetsItsOwnZettaEgressAllowlist(t *testing.T) {
	lake := billingTestLake(nil, corev1.EnvVar{Name: zettaEgressAllowEnv, Value: "*"})
	var values []string
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		if variable.Name == zettaEgressAllowEnv {
			values = append(values, variable.Value)
		}
	}
	if len(values) != 1 || values[0] != "*.svc.cluster.local" {
		t.Fatalf("ZETTA_EGRESS_ALLOW = %q, want exactly the operator's \"*.svc.cluster.local\"", values)
	}
}
