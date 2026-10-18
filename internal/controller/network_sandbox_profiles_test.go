/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type failingSandboxDiscoveryReader struct {
	client.Reader
}

func (r failingSandboxDiscoveryReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if key.Namespace == "default" && key.Name == "kubernetes" {
		return apierrors.NewServiceUnavailable("test API discovery failure")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func sandboxDiscoveryObjects() []client.Object {
	return []client.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				ClusterIP:  "10.96.0.1",
				ClusterIPs: []string{"10.96.0.1", "fd00:96::1"},
				Ports:      []corev1.ServicePort{{Port: 443}},
			},
		},
		&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Subsets: []corev1.EndpointSubset{{
				Addresses:         []corev1.EndpointAddress{{IP: "20.237.72.161"}, {IP: "2603:1030:20e:3::10"}},
				NotReadyAddresses: []corev1.EndpointAddress{{IP: "20.237.72.162"}, {IP: "2603:1030:20e:3::11"}},
				Ports:             []corev1.EndpointPort{{Port: 6443}},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Spec: corev1.NodeSpec{
				PodCIDR:  "11.244.0.0/24",
				PodCIDRs: []string{"11.244.0.0/24", "2603:1030:20e:42::/64"},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "20.51.1.20"},
				{Type: corev1.NodeInternalIP, Address: "2603:1030:20e:3::20"},
			}},
		},
	}
}

func allowanceByName(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox, name string) oramav1alpha1.NetworkSandboxPolicy {
	t.Helper()
	for _, allowance := range sandbox.Spec.Policies {
		if allowance.Name == name {
			return allowance
		}
	}
	t.Fatalf("allowance %q not found", name)
	return oramav1alpha1.NetworkSandboxPolicy{}
}

func assertJustifiedNarrowAllowances(t *testing.T, sandbox *oramav1alpha1.NetworkSandbox) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, allowance := range sandbox.Spec.Policies {
		if strings.TrimSpace(allowance.Comment) == "" {
			t.Errorf("allowance %q has no component justification", allowance.Name)
		}
		if _, ok := seen[allowance.Name]; ok {
			t.Errorf("duplicate allowance %q", allowance.Name)
		}
		seen[allowance.Name] = struct{}{}
		for _, ingress := range allowance.Spec.Ingress {
			if len(ingress.From) == 0 {
				t.Errorf("allowance %q has unrestricted ingress", allowance.Name)
			}
		}
		for _, egress := range allowance.Spec.Egress {
			if len(egress.To) == 0 {
				t.Errorf("allowance %q has unrestricted egress", allowance.Name)
			}
		}
	}
}

func TestLakeNetworkSandboxListsOnlyJustifiedNarrowPaths(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", UID: types.UID("lake-uid")},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake-system",
			Image:          "oramasearch/lake:test",
			IngressEnabled: true,
			Host:           "lake.example.test",
		},
	}
	objects := append([]client.Object{lake}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &LakeReconciler{
		Client:            kube,
		Scheme:            scheme,
		APIReader:         kube,
		OperatorNamespace: "orama-system",
	}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, map[string]string{"test": "lake"}); err != nil {
		t.Fatalf("reconcile Lake NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)
	for _, name := range []string{
		"allow-lake-to-controldb",
		"allow-lake-to-zetta",
		"allow-lake-to-workspaces",
		"allow-lake-apiserver-egress",
		"allow-cnpg-apiserver-egress",
		"allow-lake-public-https",
		"allow-ingress-nginx",
	} {
		allowanceByName(t, &sandbox, name)
	}
	var deny networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxDefaultDenyName}, &deny); err != nil {
		t.Fatalf("default deny was not rendered: %v", err)
	}
}

func TestOramaCloudEngineEgressIsRestrictedToRequiredDestinations(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	// A resource stored before admission refused controlDB.mode external must still get no
	// path to a database outside the cluster.
	stack.Spec.Components.ControlDB.Mode = "external"
	objects := append([]client.Object{stack}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &OramaCloudV1Reconciler{
		Client:            kube,
		Scheme:            scheme,
		APIReader:         kube,
		OperatorNamespace: "orama-system",
	}
	if err := reconciler.reconcileNetworkSandbox(ctx, stack); err != nil {
		t.Fatalf("reconcile Cloud v1 NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, &sandbox)
	// Airbyte was removed from Cloud v1 (#563): no allowance may reach its namespace or API port.
	airbyteNamespace := namespaceNameSelector("airbyte")
	for _, allowance := range sandbox.Spec.Policies {
		var peers []networkingv1.NetworkPolicyPeer
		var ports []networkingv1.NetworkPolicyPort
		for _, rule := range allowance.Spec.Egress {
			peers, ports = append(peers, rule.To...), append(ports, rule.Ports...)
		}
		for _, rule := range allowance.Spec.Ingress {
			peers, ports = append(peers, rule.From...), append(ports, rule.Ports...)
		}
		for _, peer := range peers {
			if reflect.DeepEqual(peer.NamespaceSelector, airbyteNamespace) {
				t.Errorf("%s selects namespace airbyte", allowance.Name)
			}
		}
		for _, port := range ports {
			if port.Port != nil && port.Port.IntVal == 8001 {
				t.Errorf("%s allows port 8001", allowance.Name)
			}
		}
	}
	for _, component := range []string{"writer", "reader"} {
		engineLabels := labelsFor(stack, component)
		var enginePolicyNames []string
		for _, allowance := range sandbox.Spec.Policies {
			if len(allowance.Spec.Egress) > 0 && selects(t, allowance.Spec.PodSelector, engineLabels) {
				enginePolicyNames = append(enginePolicyNames, allowance.Name)
			}
		}
		if !reflect.DeepEqual(enginePolicyNames, []string{
			"allow-dns-egress",
			"allow-engine-egress",
			"allow-engine-public-internet",
		}) {
			t.Fatalf("%s egress policies = %v", component, enginePolicyNames)
		}
	}

	internal := allowanceByName(t, &sandbox, "allow-engine-egress")
	if len(internal.Spec.Egress) != 2 {
		t.Fatalf("engine internal egress = %+v", internal.Spec.Egress)
	}
	if !selects(t, *internal.Spec.Egress[0].To[0].PodSelector, labelsFor(stack, "broker")) ||
		!reflect.DeepEqual(policyPorts(t, internal.Spec.Egress[0].Ports), []int32{5552}) {
		t.Fatalf("engine broker rule = %+v", internal.Spec.Egress[0])
	}
	if !selects(t, *internal.Spec.Egress[1].To[0].PodSelector, labelsFor(stack, "embeddings")) ||
		!reflect.DeepEqual(policyPorts(t, internal.Spec.Egress[1].Ports), []int32{50051}) {
		t.Fatalf("engine embeddings rule = %+v", internal.Spec.Egress[1])
	}

	dns := allowanceByName(t, &sandbox, "allow-dns-egress")
	if len(dns.Spec.Egress) != 1 || len(dns.Spec.Egress[0].Ports) != 2 {
		t.Fatalf("engine DNS rule = %+v", dns.Spec.Egress)
	}
	protocols := map[corev1.Protocol]bool{}
	for _, port := range dns.Spec.Egress[0].Ports {
		if port.Protocol == nil || port.Port == nil || port.Port.IntVal != 53 {
			t.Fatalf("engine DNS port = %+v", port)
		}
		protocols[*port.Protocol] = true
	}
	if !protocols[corev1.ProtocolTCP] || !protocols[corev1.ProtocolUDP] {
		t.Fatalf("engine DNS protocols = %v", protocols)
	}

	engine := allowanceByName(t, &sandbox, "allow-engine-public-internet")
	if len(engine.Spec.Egress) != 1 || len(engine.Spec.Egress[0].Ports) != 1 {
		t.Fatalf("engine public rule must allow every public URL TCP port: %+v", engine.Spec.Egress)
	}
	publicPort := engine.Spec.Egress[0].Ports[0]
	if publicPort.Protocol == nil || *publicPort.Protocol != corev1.ProtocolTCP || publicPort.Port != nil || publicPort.EndPort != nil {
		t.Fatalf("engine public port restriction = %+v", publicPort)
	}
	excluded := map[string]bool{}
	cidrs := map[string]bool{}
	for _, peer := range engine.Spec.Egress[0].To {
		if peer.IPBlock == nil {
			t.Fatalf("engine public peer is not an IP block: %+v", peer)
		}
		cidrs[peer.IPBlock.CIDR] = true
		for _, value := range peer.IPBlock.Except {
			excluded[value] = true
		}
	}
	if !cidrs["0.0.0.0/0"] || !cidrs["::/0"] || len(cidrs) != 2 {
		t.Fatalf("engine public CIDRs = %v", cidrs)
	}
	for _, value := range []string{
		"10.0.0.0/8",
		"100.64.0.0/10",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"11.244.0.0/24",
		"2603:1030:20e:42::/64",
		"20.51.1.20/32",
		"2603:1030:20e:3::20/128",
		"20.237.72.161/32",
		"20.237.72.162/32",
		"2603:1030:20e:3::10/128",
		"2603:1030:20e:3::11/128",
	} {
		if !excluded[value] {
			t.Errorf("engine public rule does not exclude %s", value)
		}
	}
	for _, name := range []string{
		"allow-dashboard-egress",
		"allow-cnpg-apiserver-egress",
		"allow-dashboard-ingress-nginx",
		"allow-docs-ingress-nginx",
	} {
		allowanceByName(t, &sandbox, name)
	}
	for _, allowance := range sandbox.Spec.Policies {
		if strings.Contains(allowance.Name, "external-postgres") {
			t.Fatalf("stored controlDB.mode external opened %q", allowance.Name)
		}
	}
}

// The writer reaches the dashboard's port 3000 only while its JWT provider fetches the JWKS from this
// stack's dashboard Service, and readers never do.
func TestWriterJWKSOpensOnlyWriterToDashboard(t *testing.T) {
	jwks := func(u string) func(*oramav1alpha1.OramaCloudV1) {
		return func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.JWT.JWKSURL = u }
	}
	cases := []struct {
		name string
		oc   *oramav1alpha1.OramaCloudV1
		open bool
	}{
		{"unset", engineOC(nil), false},
		{"in-cluster default", jwtOC([]string{"https://writer.example"}, nil), true},
		{"qualified service", jwtOC([]string{"https://writer.example"}, jwks("http://v1-dashboard.orama-system.svc.cluster.local:3000/api/.well-known/jwks.json")), true},
		{"external", jwtOC([]string{"https://writer.example"}, jwks("https://app.example.com/api/.well-known/jwks.json")), false},
		{"other namespace", jwtOC([]string{"https://writer.example"}, jwks("http://v1-dashboard.elsewhere:3000/api/.well-known/jwks.json")), false},
	}
	names := []string{"allow-writer-jwks-egress", "allow-dashboard-jwks-ingress"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := ocv1TestScheme(t)
			objects := append([]client.Object{tc.oc}, sandboxDiscoveryObjects()...)
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reconciler := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube}
			if err := reconciler.reconcileNetworkSandbox(ctx, tc.oc); err != nil {
				t.Fatal(err)
			}
			var sandbox oramav1alpha1.NetworkSandbox
			if err := kube.Get(ctx, client.ObjectKey{Namespace: tc.oc.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
				t.Fatal(err)
			}
			assertJustifiedNarrowAllowances(t, &sandbox)
			if !tc.open {
				for _, allowance := range sandbox.Spec.Policies {
					if slices.Contains(names, allowance.Name) {
						t.Fatalf("%s rendered for a JWKS outside this stack's dashboard", allowance.Name)
					}
				}
				return
			}
			writer, reader, dashboard := labelsFor(tc.oc, "writer"), labelsFor(tc.oc, "reader"), labelsFor(tc.oc, "dashboard")

			egress := allowanceByName(t, &sandbox, names[0])
			if !selects(t, egress.Spec.PodSelector, writer) || selects(t, egress.Spec.PodSelector, reader) ||
				len(egress.Spec.Ingress) != 0 || len(egress.Spec.Egress) != 1 || len(egress.Spec.Egress[0].To) != 1 {
				t.Fatalf("writer JWKS egress = %+v", egress.Spec)
			}
			to := egress.Spec.Egress[0]
			if to.To[0].NamespaceSelector != nil || !selects(t, *to.To[0].PodSelector, dashboard) ||
				selects(t, *to.To[0].PodSelector, writer) ||
				!reflect.DeepEqual(policyPorts(t, to.Ports), []int32{3000}) {
				t.Fatalf("writer JWKS egress rule = %+v", to)
			}

			ingress := allowanceByName(t, &sandbox, names[1])
			if !selects(t, ingress.Spec.PodSelector, dashboard) || selects(t, ingress.Spec.PodSelector, writer) ||
				len(ingress.Spec.Egress) != 0 || len(ingress.Spec.Ingress) != 1 || len(ingress.Spec.Ingress[0].From) != 1 {
				t.Fatalf("dashboard JWKS ingress = %+v", ingress.Spec)
			}
			from := ingress.Spec.Ingress[0]
			if from.From[0].NamespaceSelector != nil || !selects(t, *from.From[0].PodSelector, writer) ||
				selects(t, *from.From[0].PodSelector, reader) ||
				!reflect.DeepEqual(policyPorts(t, from.Ports), []int32{3000}) {
				t.Fatalf("dashboard JWKS ingress rule = %+v", from)
			}
		})
	}
}

func TestNetworkSandboxPreservesDiscoveredPathsDuringReadFailure(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", UID: types.UID("lake-uid")},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-system", Image: "oramasearch/lake:test"},
	}
	objects := append([]client.Object{lake}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &LakeReconciler{Client: kube, Scheme: scheme, APIReader: kube}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, nil); err != nil {
		t.Fatal(err)
	}
	reconciler.APIReader = failingSandboxDiscoveryReader{Reader: kube}
	if err := reconciler.reconcileLakeNetworkSandbox(ctx, lake, lake.Spec.Namespace, nil); err == nil {
		t.Fatal("reconcile succeeded during Kubernetes API discovery failure")
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: lake.Spec.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"allow-lake-apiserver-egress",
		"allow-cnpg-apiserver-egress",
		"allow-lake-public-https",
		"allow-agents-public-https",
		"allow-controldb-backup-public-https",
	} {
		allowanceByName(t, &sandbox, name)
	}
}

// TestOramaCloudAnalyticsJobsReachTheirTargets checks every path #419 and #421 added has both its
// egress and its ingress allowance (#616): the retention Job to ClickHouse's native port, the import
// CronJob to the analytics processor, and the processor to every reader.
func TestOramaCloudAnalyticsJobsReachTheirTargets(t *testing.T) {
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	stack := newOCV1(nil)
	objects := append([]client.Object{stack}, sandboxDiscoveryObjects()...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
	if err := reconciler.reconcileNetworkSandbox(ctx, stack); err != nil {
		t.Fatalf("reconcile Cloud v1 NetworkSandbox: %v", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	selects := func(sel *metav1.LabelSelector, component string) bool {
		if sel == nil {
			return false
		}
		s, err := metav1.LabelSelectorAsSelector(sel)
		return err == nil && s.Matches(labels.Set(labelsFor(stack, component)))
	}
	hasPort := func(ports []networkingv1.NetworkPolicyPort, port int32) bool {
		for _, p := range ports {
			if p.Port != nil && p.Port.IntVal == port {
				return true
			}
		}
		return false
	}
	for _, path := range []struct {
		from, to string
		port     int32
	}{
		{"analytics-retention", "analytics", 9000},
		{analyticsImportComponent, "analytics-processor", 8080},
		{"analytics-processor", "reader", 8080},
	} {
		var egress, ingress bool
		for _, allowance := range sandbox.Spec.Policies {
			if selects(&allowance.Spec.PodSelector, path.from) {
				for _, rule := range allowance.Spec.Egress {
					for _, peer := range rule.To {
						if selects(peer.PodSelector, path.to) && hasPort(rule.Ports, path.port) {
							egress = true
						}
					}
				}
			}
			if selects(&allowance.Spec.PodSelector, path.to) {
				for _, rule := range allowance.Spec.Ingress {
					for _, peer := range rule.From {
						if selects(peer.PodSelector, path.from) && hasPort(rule.Ports, path.port) {
							ingress = true
						}
					}
				}
			}
		}
		if !egress {
			t.Errorf("no egress allowance from %s to %s:%d", path.from, path.to, path.port)
		}
		if !ingress {
			t.Errorf("no ingress allowance to %s:%d from %s", path.to, path.port, path.from)
		}
	}
}
