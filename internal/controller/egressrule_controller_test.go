/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testEgressNameRuleID = "a0000000-0000-4000-8000-00000000000a"
	testEgressCIDRRuleID = "b0000000-0000-4000-8000-00000000000b"
	testEgressSourceID   = "c0000000-0000-4000-8000-00000000000c"
	testEgressNamespace  = "workspace"
	testSourceIP         = "10.244.0.17"
	testBystanderIP      = "10.244.0.18"
	testGatewayIP        = "10.244.0.19"
)

type egressFixture struct {
	ctx        context.Context
	kube       client.Client
	lake       *oramav1alpha1.Lake
	reconciler *EgressRuleReconciler
}

func newEgressFixture(t *testing.T, objects ...client.Object) *egressFixture {
	t.Helper()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")},
		Spec:       oramav1alpha1.LakeSpec{PullSecretName: "dockerhub"},
	}
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: testEgressNamespace, UID: types.UID("sandbox-uid")},
	}
	if err := controllerutil.SetControllerReference(lake, sandbox, scheme); err != nil {
		t.Fatal(err)
	}
	pod := func(name, ip string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testEgressNamespace, Labels: labels},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}},
		}
	}
	all := []client.Object{
		lake, sandbox,
		pod("source", testSourceIP, map[string]string{serviceIDLabel: testEgressSourceID}),
		pod("bystander", testBystanderIP, map[string]string{serviceIDLabel: testConnectionTargetID}),
		pod("gateway", testGatewayIP, egressGatewayLabels()),
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Spec:       corev1.NodeSpec{PodCIDR: "10.244.0.0/24", PodCIDRs: []string{"10.244.0.0/24"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.224.0.4"},
			}},
		},
	}
	all = append(all, objects...)
	all = append(all, defaultKubernetesAPIObjects(objects)...)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.EgressRule{}, &appsv1.Deployment{}).
		WithObjects(all...).
		Build()
	return &egressFixture{
		ctx:        context.Background(),
		kube:       kube,
		lake:       lake,
		reconciler: &EgressRuleReconciler{Client: kube, Scheme: scheme},
	}
}

func (f *egressFixture) add(t *testing.T, name string, spec oramav1alpha1.EgressRuleSpec) {
	t.Helper()
	spec.LakeRef = f.lake.Name
	if spec.WorkspaceID == "" {
		spec.WorkspaceID = testWorkspaceID
	}
	if spec.Desired == "" {
		spec.Desired = oramav1alpha1.EgressRuleDesiredPresent
	}
	if spec.Generation == 0 {
		spec.Generation = 1
	}
	rule := &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testEgressNamespace}, Spec: spec}
	if err := controllerutil.SetControllerReference(f.lake, rule, f.kube.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(f.ctx, rule); err != nil {
		t.Fatal(err)
	}
}

func (f *egressFixture) reconcile(t *testing.T, name string) oramav1alpha1.EgressRule {
	t.Helper()
	request := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testEgressNamespace, Name: name}}
	for pass := 0; pass < 2; pass++ {
		if _, err := f.reconciler.Reconcile(f.ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	var rule oramav1alpha1.EgressRule
	if err := f.kube.Get(f.ctx, request.NamespacedName, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return rule
		}
		t.Fatal(err)
	}
	return rule
}

func (f *egressFixture) policy(t *testing.T, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	var policy networkingv1.NetworkPolicy
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: name}, &policy); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return &policy
}

func (f *egressFixture) snapshot(t *testing.T) (egressGatewaySnapshot, bool) {
	t.Helper()
	var config corev1.ConfigMap
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: egressGatewayName}, &config); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return egressGatewaySnapshot{}, false
		}
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(config.Data[egressGatewayRulesKey]))
	decoder.DisallowUnknownFields()
	var snapshot egressGatewaySnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatalf("snapshot %q: %v", config.Data[egressGatewayRulesKey], err)
	}
	return snapshot, true
}

func (f *egressFixture) markGatewayReady(t *testing.T) {
	t.Helper()
	var deployment appsv1.Deployment
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: egressGatewayName}, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	if err := f.kube.Status().Update(f.ctx, &deployment); err != nil {
		t.Fatal(err)
	}
}

func nameRule() oramav1alpha1.EgressRuleSpec {
	return oramav1alpha1.EgressRuleSpec{
		RuleID:     testEgressNameRuleID,
		SourceKind: oramav1alpha1.EgressRuleSourceService,
		SourceID:   testEgressSourceID,
		DNSName:    "api.example.com",
		Ports:      []int32{443, 8443},
	}
}

func hasIPBlock(policy *networkingv1.NetworkPolicy) bool {
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

// TestNameRuleAllowsTheSourceOnlyTheGateway is the policy half of #470's acceptance: the source of
// a DNS-name rule may reach the gateway and nothing else, so a name the gateway does not authorize,
// one resolving to an allowed name's address, and an IP literal all have no path.
func TestNameRuleAllowsTheSourceOnlyTheGateway(t *testing.T) {
	f := newEgressFixture(t)
	name := lakeEgressRuleName(testEgressNameRuleID)
	f.add(t, name, nameRule())
	rule := f.reconcile(t, name)
	if rule.Status.Phase != oramav1alpha1.EgressRulePhasePending || !strings.Contains(rule.Status.Message, "gateway") {
		t.Fatalf("status before the gateway is ready = %+v", rule.Status)
	}

	source := f.policy(t, name)
	if source == nil || source.Spec.PodSelector.MatchLabels[serviceIDLabel] != testEgressSourceID || hasIPBlock(source) ||
		len(source.Spec.Egress) != 1 || len(source.Spec.Egress[0].To) != 1 ||
		source.Spec.Egress[0].To[0].NamespaceSelector != nil ||
		!reflect.DeepEqual(source.Spec.Egress[0].To[0].PodSelector.MatchLabels, map[string]string{egressGatewayComponentLabel: egressGatewayComponent}) ||
		len(source.Spec.Egress[0].Ports) != 1 || source.Spec.Egress[0].Ports[0].Port.IntVal != egressGatewayPort {
		t.Fatalf("source egress = %#v", source)
	}
	ingress := f.policy(t, name+"-gateway-ingress")
	if ingress == nil || ingress.Spec.PodSelector.MatchLabels[egressGatewayComponentLabel] != egressGatewayComponent ||
		ingress.Spec.Ingress[0].From[0].PodSelector.MatchLabels[serviceIDLabel] != testEgressSourceID ||
		ingress.Spec.Ingress[0].Ports[0].Port.IntVal != egressGatewayPort {
		t.Fatalf("gateway ingress = %#v", ingress)
	}
	gatewayEgress := f.policy(t, name+"-gateway-egress")
	if gatewayEgress == nil || gatewayEgress.Spec.PodSelector.MatchLabels[egressGatewayComponentLabel] != egressGatewayComponent ||
		!hasIPBlock(gatewayEgress) || len(gatewayEgress.Spec.Egress[0].Ports) != 2 {
		t.Fatalf("gateway egress = %#v", gatewayEgress)
	}
	// The gateway reaches only public addresses: never private, metadata, nodes or pods.
	excluded := map[string]bool{}
	for _, peer := range gatewayEgress.Spec.Egress[0].To {
		for _, cidr := range peer.IPBlock.Except {
			excluded[cidr] = true
		}
	}
	for _, cidr := range []string{"10.0.0.0/8", "169.254.0.0/16", "10.244.0.0/24", "10.224.0.4/32"} {
		if !excluded[cidr] {
			t.Errorf("gateway egress does not exclude %s", cidr)
		}
	}

	// The snapshot authorizes exactly the source's address for the name on the rule's ports.
	snapshot, found := f.snapshot(t)
	want := egressGatewaySnapshot{Rules: []egressGatewayRule{{
		ID:        testEgressNameRuleID,
		Workspace: testEgressNamespace,
		SourceIPs: []string{testSourceIP},
		DNSName:   "api.example.com",
		Ports:     []int32{443, 8443},
	}}}
	if !found || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}

	// The gateway is zetta-tunnel's egress role exactly as its contract specifies.
	var deployment appsv1.Deployment
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: egressGatewayName}, &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	wantArgs := []string{
		"egress", "--rules", "/etc/zetta-tunnel/egress/rules.json", "--listen", "0.0.0.0:7445",
		"--health-listen", "0.0.0.0:8080", "--sni-port", "443", "--audit", "-",
	}
	if container.Image != "oramasearch/zetta-tunnel:dev-933d6fc" || !reflect.DeepEqual(container.Args, wantArgs) ||
		len(container.Env) != 0 || deployment.Spec.Template.Spec.ImagePullSecrets[0].Name != "dockerhub" ||
		*deployment.Spec.Template.Spec.AutomountServiceAccountToken || !*container.SecurityContext.ReadOnlyRootFilesystem ||
		*deployment.Spec.Template.Spec.SecurityContext.RunAsUser != 65532 || !metav1.IsControlledBy(&deployment, f.lake) {
		t.Fatalf("gateway deployment = %#v", deployment.Spec.Template.Spec)
	}
	var service corev1.Service
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: egressGatewayName}, &service); err != nil {
		t.Fatal(err)
	}
	if len(service.Spec.Ports) != 2 || service.Spec.Ports[0].Port != 443 || service.Spec.Ports[0].TargetPort.IntVal != 7445 {
		t.Fatalf("gateway service = %#v", service.Spec)
	}

	f.markGatewayReady(t)
	rule = f.reconcile(t, name)
	if rule.Status.Phase != oramav1alpha1.EgressRulePhaseReady || !strings.Contains(rule.Status.Message, egressGatewayAddress(testEgressNamespace)) {
		t.Fatalf("status = %+v", rule.Status)
	}
}

// TestAnIPLiteralNeedsACIDRRule: a name rule never takes an address, and only a CIDR rule opens a
// direct path, minus every cluster address inside its range.
func TestAnIPLiteralNeedsACIDRRule(t *testing.T) {
	for _, literal := range []string{"93.184.216.34", "2001:db8::1", "*.example.com", "localhost", "example.123"} {
		if _, err := normalizeEgressDNSName(literal); err == nil {
			t.Errorf("dns_name %q was accepted", literal)
		}
	}
	if name, err := normalizeEgressDNSName("API.Example.COM."); err != nil || name != "api.example.com" {
		t.Fatalf("normalized = %q, %v", name, err)
	}

	f := newEgressFixture(t)
	name := lakeEgressRuleName(testEgressCIDRRuleID)
	f.add(t, name, oramav1alpha1.EgressRuleSpec{
		RuleID:     testEgressCIDRRuleID,
		SourceKind: oramav1alpha1.EgressRuleSourceService,
		SourceID:   testEgressSourceID,
		CIDR:       "10.224.0.0/16",
		Ports:      []int32{5432},
	})
	rule := f.reconcile(t, name)
	if rule.Status.Phase != oramav1alpha1.EgressRulePhaseReady {
		t.Fatalf("status = %+v", rule.Status)
	}
	policy := f.policy(t, name)
	if policy == nil || len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 {
		t.Fatalf("cidr policy = %#v", policy)
	}
	block := policy.Spec.Egress[0].To[0].IPBlock
	if block.CIDR != "10.224.0.0/16" || !reflect.DeepEqual(block.Except, []string{"10.224.0.4/32"}) ||
		policy.Spec.Egress[0].Ports[0].Port.IntVal != 5432 || policy.Spec.PodSelector.MatchLabels[serviceIDLabel] != testEgressSourceID {
		t.Fatalf("cidr policy = %#v", policy.Spec)
	}
	if _, found := f.snapshot(t); found {
		t.Fatal("a CIDR rule started an egress gateway")
	}

	// A range wholly inside an address no rule may reach is refused.
	metadata := lakeEgressRuleName("d0000000-0000-4000-8000-00000000000d")
	f.add(t, metadata, oramav1alpha1.EgressRuleSpec{RuleID: "d0000000-0000-4000-8000-00000000000d", CIDR: "169.254.169.254/32", Ports: []int32{80}})
	if rule := f.reconcile(t, metadata); rule.Status.Phase != oramav1alpha1.EgressRulePhaseFailed ||
		!strings.Contains(rule.Status.Message, "169.254.0.0/16") || f.policy(t, metadata) != nil {
		t.Fatalf("metadata rule = %+v", rule.Status)
	}
}

func TestRemovingTheLastNameRuleClosesThePathAndRemovesTheGateway(t *testing.T) {
	f := newEgressFixture(t)
	name := lakeEgressRuleName(testEgressNameRuleID)
	f.add(t, name, nameRule())
	f.reconcile(t, name)
	var rule oramav1alpha1.EgressRule
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: name}, &rule); err != nil {
		t.Fatal(err)
	}
	rule.Spec.Desired = oramav1alpha1.EgressRuleDesiredAbsent
	rule.Spec.Generation = 2
	if err := f.kube.Update(f.ctx, &rule); err != nil {
		t.Fatal(err)
	}
	rule = f.reconcile(t, name)
	if rule.Status.Phase != oramav1alpha1.EgressRulePhaseDeleted || rule.Status.ObservedGeneration != 2 {
		t.Fatalf("status = %+v", rule.Status)
	}
	for _, policy := range []string{name, name + "-gateway-ingress", name + "-gateway-egress"} {
		if f.policy(t, policy) != nil {
			t.Fatalf("%s remains after one absent reconcile", policy)
		}
	}
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.ConfigMap{}} {
		err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: egressGatewayName}, object)
		if client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("gateway %T remains: %v", object, err)
		}
	}
}

func TestOneNameRuleLeavingKeepsTheOthersAuthorized(t *testing.T) {
	f := newEgressFixture(t)
	first := lakeEgressRuleName(testEgressNameRuleID)
	f.add(t, first, nameRule())
	second := lakeEgressRuleName("e0000000-0000-4000-8000-00000000000e")
	other := nameRule()
	other.RuleID = "e0000000-0000-4000-8000-00000000000e"
	other.DNSName = "files.example.org"
	other.SourceKind, other.SourceID = "", ""
	other.Ports = []int32{443}
	f.add(t, second, other)
	f.reconcile(t, first)
	f.reconcile(t, second)
	snapshot, _ := f.snapshot(t)
	if len(snapshot.Rules) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// A workspace-wide rule authorizes every workload except the gateway itself.
	for _, entry := range snapshot.Rules {
		if entry.ID == other.RuleID && !reflect.DeepEqual(entry.SourceIPs, []string{testSourceIP, testBystanderIP}) {
			t.Fatalf("workspace-wide sources = %v", entry.SourceIPs)
		}
	}
	var rule oramav1alpha1.EgressRule
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: testEgressNamespace, Name: first}, &rule); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, &rule); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, first)
	snapshot, found := f.snapshot(t)
	if !found || len(snapshot.Rules) != 1 || snapshot.Rules[0].DNSName != "files.example.org" {
		t.Fatalf("snapshot after deleting one rule = %+v", snapshot)
	}
}

func TestSnapshotNeverAuthorizesOneTupleTwice(t *testing.T) {
	rule := func(id, source string, ports ...int32) *oramav1alpha1.EgressRule {
		spec := oramav1alpha1.EgressRuleSpec{RuleID: id, DNSName: "api.example.com", Ports: ports}
		if source != "" {
			spec.SourceKind, spec.SourceID = "app", source
		}
		return &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{Name: "egress-" + id}, Spec: spec}
	}
	wide := rule("10000000-0000-4000-8000-000000000001", "", 443)
	same := rule("20000000-0000-4000-8000-000000000002", testEgressSourceID, 443)
	partial := rule("30000000-0000-4000-8000-000000000003", testEgressSourceID, 443, 8443)
	idle := rule("40000000-0000-4000-8000-000000000004", testEgressSourceID, 9000)
	snapshot, problems := buildEgressGatewaySnapshot("workspace", []egressGatewayEntry{
		{rule: partial, ips: []string{"10.0.0.1"}},
		{rule: same, ips: []string{"10.0.0.1"}},
		{rule: wide, ips: []string{"10.0.0.1", "10.0.0.2"}},
		{rule: idle, ips: nil},
	})
	if len(snapshot.Rules) != 1 || snapshot.Rules[0].ID != wide.Spec.RuleID {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if problems[same.Name] != "" || !strings.Contains(problems[partial.Name], wide.Name) {
		t.Fatalf("problems = %+v", problems)
	}
	seen := map[string]bool{}
	for _, entry := range snapshot.Rules {
		for _, ip := range entry.SourceIPs {
			for _, port := range entry.Ports {
				key := ip + entry.DNSName + string(rune(port))
				if seen[key] {
					t.Fatalf("tuple %s authorized twice", key)
				}
				seen[key] = true
			}
		}
	}
	encoded, err := json.Marshal(egressGatewaySnapshot{Rules: []egressGatewayRule{}})
	if err != nil || string(encoded) != `{"rules":[]}` {
		t.Fatalf("empty snapshot = %s, %v", encoded, err)
	}
}

func TestInternetEgressRuleRendersTheLegacyPolicyAndLeavesWithTheOverride(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	lake.Spec.Workspaces.Overrides = map[string]oramav1alpha1.LakeWorkspaceOverrideSpec{
		testWorkspaceID: {InternetEgress: true},
	}
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	var rule oramav1alpha1.EgressRule
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetRuleName}, &rule); err != nil {
		t.Fatal(err)
	}
	if !rule.Spec.PublicInternet || !reflect.DeepEqual(rule.Spec.Ports, []int32{443}) || rule.Spec.SourceKind != "" ||
		rule.Spec.RuleID != "" || !metav1.IsControlledBy(&rule, lake) {
		t.Fatalf("internet rule = %+v", rule.Spec)
	}
	renderWorkspaceEgressRules(t, kube, testWorkspaceName)
	labels := workspaceLabels(lake, &store.rows[0])
	legacy, err := reconciler.workspaceInternetPolicy(ctx, testWorkspaceName, labels)
	if err != nil {
		t.Fatal(err)
	}
	var rendered networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetName}, &rendered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rendered.Spec, legacy.Spec) {
		t.Fatalf("internetEgress changed what it allows\ngot:  %#v\nwant: %#v", rendered.Spec, legacy.Spec)
	}
	// Another workspace pass keeps it: the rebuild preserves the rule's allowance.
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetName}, &rendered); err != nil {
		t.Fatalf("a workspace pass dropped the internet allowance: %v", err)
	}

	lake.Spec.Workspaces.Overrides = nil
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)
	renderWorkspaceEgressRules(t, kube, testWorkspaceName)
	err = kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetName}, &rendered)
	if client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("internet allowance remains after the override was removed: %v", err)
	}
	err = kube.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: workspaceInternetRuleName}, &rule)
	if client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("internet rule remains after the override was removed: %v", err)
	}
}
