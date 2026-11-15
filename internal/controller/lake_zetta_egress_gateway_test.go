/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The usage of zetta PR #1516 lists both gateway variables.
const testZettaUsageGateway = "  --egress-gateway HOST[:PORT]  (or ZETTA_EGRESS_GATEWAY)\n" +
	"  --egress-gateway-route LIST   (or ZETTA_EGRESS_GATEWAY_ROUTES)\n"

// operator#1179: the routes are the namespace's name rules for the Zetta, once rendered; a CIDR
// rule, the public-internet rule and a rule for another workload are not routes, and a namespace
// without a name rule gets no gateway.
func TestZettaEgressGatewayRoutesAreTheNameRules(t *testing.T) {
	f := newEgressFixture(t)
	none, err := lakeZettaEgressGateway(context.Background(), f.kube, testEgressNamespace, testEgressSourceID)
	if err != nil || none != (zettaEgressGateway{}) {
		t.Fatalf("no rules: %+v, %v", none, err)
	}
	nameRuleName := lakeEgressRuleName(testEgressNameRuleID)
	f.add(t, nameRuleName, nameRule())
	f.add(t, lakeEgressRuleName(testEgressCIDRRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressCIDRRuleID, CIDR: "203.0.113.7/32", Ports: []int32{5432},
	})
	f.add(t, lakeEgressRuleName(testEgressOtherRuleID), oramav1alpha1.EgressRuleSpec{
		RuleID: testEgressOtherRuleID, SourceKind: oramav1alpha1.EgressRuleSourceService, SourceID: testConnectionTargetID,
		DNSName: "other.example.com", Ports: []int32{443},
	})
	f.add(t, workspaceInternetRuleName, oramav1alpha1.EgressRuleSpec{PublicInternet: true, Ports: []int32{443}})
	for _, name := range []string{nameRuleName, lakeEgressRuleName(testEgressCIDRRuleID), lakeEgressRuleName(testEgressOtherRuleID), workspaceInternetRuleName} {
		f.reconcile(t, name)
	}
	gateway, err := lakeZettaEgressGateway(context.Background(), f.kube, testEgressNamespace, testEgressSourceID)
	if err != nil {
		t.Fatal(err)
	}
	if gateway.routes != "api.example.com:443,api.example.com:8443" ||
		gateway.address != "zetta-tunnel-egress."+testEgressNamespace+".svc.cluster.local:7445" {
		t.Fatalf("gateway = %+v", gateway)
	}
}

// The ConfigMap carries the gateway only with routes, and the node gets the variables only for an
// image whose usage lists both.
func TestTheEgressGatewayReachesOnlyAnImageThatReadsIt(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	cluster := newZettaTestCluster()
	cluster.Spec.EgressAllowConfigMap = zettaEgressConfigMapName(cluster.Name)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	gateway := zettaEgressGateway{address: "zetta-tunnel-egress.ws.svc.cluster.local:7445", routes: "api.example.com:443"}
	if err := reconcileZettaEgressConfigMapWithGateway(ctx, cl, scheme, cluster, "api.example.com:443", gateway); err != nil {
		t.Fatal(err)
	}
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: "z0-zetta-egress"}
	if err := cl.Get(ctx, key, &configMap); err != nil {
		t.Fatal(err)
	}
	if configMap.Data["gateway"] != gateway.address || configMap.Data["gatewayRoutes"] != gateway.routes {
		t.Fatalf("ConfigMap = %v", configMap.Data)
	}
	if err := reconcileZettaEgressConfigMapWithGateway(ctx, cl, scheme, cluster, "", zettaEgressGateway{}); err != nil {
		t.Fatal(err)
	}
	_ = cl.Get(ctx, key, &configMap)
	if _, found := configMap.Data["gateway"]; found {
		t.Fatal("the gateway stayed without routes")
	}

	features := parseZettaImageFeatures("z:1516", testZettaUsageGateway, 0)
	if !features.EgressGateway {
		t.Fatal("the usage lists both variables")
	}
	if parseZettaImageFeatures("z:old", "  (or ZETTA_EGRESS_GATEWAY)\n", 0).EgressGateway {
		t.Fatal("an image listing only one variable")
	}
	cluster.Spec.Image = "z:1516"
	if !zettaImageRoutesThroughGateway(cluster, features) || zettaImageRoutesThroughGateway(cluster, parseZettaImageFeatures("z:other", testZettaUsageGateway, 0)) {
		t.Fatal("the answer counts only for spec.image")
	}

	reads := renderZettaWithEgress(t, cluster, zettaEgressRender{configMap: "z0-zetta-egress", gateway: gateway, gatewayEnv: true})
	env := reads.Spec.Template.Spec.Containers[0].Env
	if got := findEnv(env, "ZETTA_EGRESS_GATEWAY"); got == nil || got.Value != gateway.address {
		t.Fatalf("ZETTA_EGRESS_GATEWAY = %+v", got)
	}
	if got := findEnv(env, "ZETTA_EGRESS_GATEWAY_ROUTES"); got == nil || got.Value != gateway.routes {
		t.Fatalf("ZETTA_EGRESS_GATEWAY_ROUTES = %+v", got)
	}
	older := renderZettaWithEgress(t, cluster, zettaEgressRender{configMap: "z0-zetta-egress", gateway: gateway})
	if findEnv(older.Spec.Template.Spec.Containers[0].Env, "ZETTA_EGRESS_GATEWAY") != nil {
		t.Fatal("an image that does not read the variables got them")
	}
	if _, ignored := zettaCustomEnv([]corev1.EnvVar{{Name: "ZETTA_EGRESS_GATEWAY", Value: "evil:1"}, {Name: "ZETTA_EGRESS_GATEWAY_ROUTES", Value: "x:443"}}); len(ignored) != 2 {
		t.Fatal("spec.env must not set the gateway")
	}
}
