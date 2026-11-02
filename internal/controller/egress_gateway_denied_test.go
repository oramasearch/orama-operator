/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestDeniedCIDRListIsCanonical(t *testing.T) {
	got, err := deniedCIDRList([]string{
		"20.237.72.161",          // an address becomes a host prefix
		"20.237.72.161/32",       // duplicate
		"11.244.3.7/24",          // host bits are masked
		"11.244.3.9",             // inside the range above
		"::ffff:198.51.99.7",     // IPv4-mapped IPv6 is written as IPv4
		"::ffff:198.51.99.0/120", // and so is a mapped range
		"2603:1030:20e:42::5/64",
		"10.224.0.4",      // private: the gateway refuses it itself
		"10.244.0.0/16",   // private
		"fd00::1",         // unique-local
		"169.254.169.254", // link-local, the metadata address
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"11.244.3.0/24", "20.237.72.161/32", "198.51.99.0/24", "2603:1030:20e:42::/64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("denied CIDRs = %v, want %v", got, want)
	}
	for _, value := range got {
		prefix := netip.MustParsePrefix(value)
		if prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			t.Fatalf("%s is not canonical", value)
		}
	}
}

func TestDeniedCIDRListMergesSiblingsAndRefusesGarbage(t *testing.T) {
	got, err := deniedCIDRList([]string{"11.0.0.0/25", "11.0.0.128/25", "11.0.1.0/24"})
	if err != nil || !reflect.DeepEqual(got, []string{"11.0.0.0/23"}) {
		t.Fatalf("siblings = %v, %v", got, err)
	}
	if got, err := deniedCIDRList(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty inventory = %#v, %v", got, err)
	}
	for _, value := range []string{"", "node-1", "11.0.0.0/33", "fe80::1%eth0"} {
		if _, err := deniedCIDRList([]string{value}); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}

// More inventory than the gateway accepts is widened, never cut: the list fits, and every
// inventory address is still inside it.
func TestDeniedCIDRListWidensPastTheGatewayLimit(t *testing.T) {
	var inventory []string
	for i := 0; i < 600; i++ {
		// Public node addresses two apart, so no two are siblings and nothing merges losslessly.
		inventory = append(inventory, fmt.Sprintf("11.%d.%d.%d", i/250, (i%250)/2, (i%2)*2+1))
	}
	inventory = append(inventory, "2603:1030::1", "2603:1030::3")
	got, err := deniedCIDRList(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > egressGatewayMaxDeniedCIDRs {
		t.Fatalf("%d entries, want 1 to %d", len(got), egressGatewayMaxDeniedCIDRs)
	}
	var prefixes []netip.Prefix
	for _, value := range got {
		prefix := netip.MustParsePrefix(value)
		if prefix != prefix.Masked() {
			t.Fatalf("%s has host bits", value)
		}
		prefixes = append(prefixes, prefix)
	}
	for _, value := range inventory {
		address := netip.MustParseAddr(value)
		covered := false
		for _, prefix := range prefixes {
			covered = covered || prefix.Contains(address)
		}
		if !covered {
			t.Fatalf("%s is no longer denied", value)
		}
	}
	again, _ := deniedCIDRList(inventory)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("widening is not deterministic")
	}
}

var serviceCIDRListKind = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "ServiceCIDRList"}

// The inventory is the API server's: its endpoints, the nodes, their pod ranges and every
// ServiceCIDR. A cluster that does not serve ServiceCIDR still has an inventory; any other read
// error has none.
func TestEgressGatewayDeniedCIDRsReadsTheClusterInventory(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"11.244.0.0/24"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.224.0.4"},
			{Type: corev1.NodeExternalIP, Address: "52.1.2.3"},
			{Type: corev1.NodeHostName, Address: "node-1"},
		}},
	}
	build := func(list func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error) client.Client {
		return fake.NewClientBuilder().
			WithScheme(newLakeScheme(t)).
			WithObjects(append([]client.Object{node}, defaultKubernetesAPIObjects(nil)...)...).
			WithInterceptorFuncs(interceptor.Funcs{List: list}).
			Build()
	}
	serviceCIDRs := func(cidrs ...string) func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return func(ctx context.Context, kube client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			items, ok := list.(*unstructured.UnstructuredList)
			if !ok || items.GroupVersionKind() != serviceCIDRListKind {
				return kube.List(ctx, list, options...)
			}
			item := unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "networking.k8s.io/v1", "kind": "ServiceCIDR",
				"metadata": map[string]any{"name": "kubernetes"},
				"spec":     map[string]any{"cidrs": toAnySlice(cidrs)},
			}}
			items.Items = []unstructured.Unstructured{item}
			return nil
		}
	}

	got, err := egressGatewayDeniedCIDRs(context.Background(), build(serviceCIDRs("10.0.0.0/16", "12.0.0.0/16")))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"11.244.0.0/24", "12.0.0.0/16", "20.237.72.161/32", "52.1.2.3/32"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("denied CIDRs = %v, want %v", got, want)
	}

	notServed := func(ctx context.Context, kube client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
		if items, ok := list.(*unstructured.UnstructuredList); ok && items.GroupVersionKind() == serviceCIDRListKind {
			return &apimeta.NoKindMatchError{GroupKind: serviceCIDRListKind.GroupKind(), SearchedVersions: []string{"v1"}}
		}
		return kube.List(ctx, list, options...)
	}
	if got, err := egressGatewayDeniedCIDRs(context.Background(), build(notServed)); err != nil ||
		!reflect.DeepEqual(got, []string{"11.244.0.0/24", "20.237.72.161/32", "52.1.2.3/32"}) {
		t.Fatalf("without ServiceCIDR = %v, %v", got, err)
	}

	failing := func(ctx context.Context, kube client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
		if _, ok := list.(*corev1.NodeList); ok {
			return errors.New("cache not synced")
		}
		return kube.List(ctx, list, options...)
	}
	if got, err := egressGatewayDeniedCIDRs(context.Background(), build(failing)); err == nil {
		t.Fatalf("an unreadable node list gave %v", got)
	}
}

// Every rule of the rendered snapshot carries the same denied_cidrs, never null.
func TestEgressGatewaySnapshotDeniesOnEveryRule(t *testing.T) {
	rule := func(id, name string) *oramav1alpha1.EgressRule {
		return &oramav1alpha1.EgressRule{
			ObjectMeta: metav1.ObjectMeta{Name: "egress-" + id},
			Spec:       oramav1alpha1.EgressRuleSpec{RuleID: id, DNSName: name, Ports: []int32{443}},
		}
	}
	entries := []egressGatewayEntry{
		{rule: rule("10000000-0000-4000-8000-000000000001", "api.example.com"), ips: []string{"10.244.0.17"}},
		{rule: rule("20000000-0000-4000-8000-000000000002", "files.example.com"), ips: []string{"10.244.0.18"}},
	}
	denied := []string{"20.237.72.161/32", "2603:1030:20e:42::/64"}
	snapshot, problems := buildEgressGatewaySnapshot("workspace", entries, denied)
	if len(problems) != 0 || len(snapshot.Rules) != 2 {
		t.Fatalf("snapshot = %+v, problems = %v", snapshot, problems)
	}
	encoded, err := encodeEgressGatewaySnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, entry := range decoded.Rules {
		if string(entry["denied_cidrs"]) != `["20.237.72.161/32","2603:1030:20e:42::/64"]` {
			t.Fatalf("rule %s denied_cidrs = %s", entry["id"], entry["denied_cidrs"])
		}
	}

	snapshot, _ = buildEgressGatewaySnapshot("workspace", entries[:1], nil)
	encoded, _ = encodeEgressGatewaySnapshot(snapshot)
	if !strings.Contains(string(encoded), `"denied_cidrs":[]`) {
		t.Fatalf("an empty inventory is not an empty list: %s", encoded)
	}
}

func toAnySlice(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

// A node that changes what the gateways deny re-renders one name rule per namespace; a node status
// write that changes nothing they deny does not.
func TestNodeInventoryChangeReRendersEveryGateway(t *testing.T) {
	rule := func(namespace, name, dnsName string) *oramav1alpha1.EgressRule {
		spec := oramav1alpha1.EgressRuleSpec{DNSName: dnsName}
		if dnsName == "" {
			spec.CIDR = "11.0.0.0/24"
		}
		return &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: spec}
	}
	kube := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(
		rule("workspace-a", "egress-1", "api.example.com"),
		rule("workspace-a", "egress-2", "files.example.com"),
		rule("workspace-b", "egress-3", ""),
		rule("workspace-c", "egress-4", "api.example.com"),
	).Build()
	requests := (&EgressRuleReconciler{Client: kube}).nameRulesForNode(context.Background(), &corev1.Node{})
	namespaces := map[string]int{}
	for _, request := range requests {
		namespaces[request.Namespace]++
	}
	if !reflect.DeepEqual(namespaces, map[string]int{"workspace-a": 1, "workspace-c": 1}) {
		t.Fatalf("requests = %v", requests)
	}

	before := &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.224.0.4"}}}}
	heartbeat := before.DeepCopy()
	heartbeat.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if nodeInventoryChanged.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: heartbeat}) {
		t.Fatal("a status write that changes no address re-renders the gateways")
	}
	moved := before.DeepCopy()
	moved.Status.Addresses = append(moved.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "52.1.2.3"})
	ranged := before.DeepCopy()
	ranged.Spec.PodCIDRs = []string{"11.244.0.0/24"}
	for _, after := range []*corev1.Node{moved, ranged} {
		if !nodeInventoryChanged.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
			t.Fatalf("a node change to %+v does not re-render the gateways", after)
		}
	}
	if !nodeInventoryChanged.Create(event.CreateEvent{Object: before}) || !nodeInventoryChanged.Delete(event.DeleteEvent{Object: before}) {
		t.Fatal("a node that comes or goes does not re-render the gateways")
	}
}
