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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func productionLakeSet(members ...oramav1alpha1.SetMember) *oramav1alpha1.LakeSet {
	return &oramav1alpha1.LakeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-prod"},
		Spec: oramav1alpha1.LakeSetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(`{"image":"oramasearch/lake:v0.1.0","ingressEnabled":true,"dnsSecretRef":{"name":"cloudflare-api-token-lake-live"}}`),
			Clusters: members,
		}},
	}
}

func productionClusters() []client.Object {
	return append(managedCluster("example-production-eu", "swedencentral", "production"),
		managedCluster("example-production-us", "eastus", "production")...)
}

// Decision 64: one LakeSet manages every region. Each member's region is its cluster's, every
// labelled member gets the same region list, and the apex host stays on the member that states it.
func TestALakeSetGivesEachRegionItsClusterRegionAndTheRegionList(t *testing.T) {
	set := productionLakeSet(
		oramav1alpha1.SetMember{ClusterRef: "example-production-eu", Name: "shared",
			Spec: rawJSON(`{"host":"eu.lakelive.example","region":{"label":"Europe"},"apexHost":"lakelive.example"}`)},
		oramav1alpha1.SetMember{ClusterRef: "example-production-us", Name: "shared-us",
			Spec: rawJSON(`{"host":"us.lakelive.example","region":{"label":"United States"}}`)},
		oramav1alpha1.SetMember{ClusterRef: "example-production-us", Name: "customer-1",
			Spec: rawJSON(`{"host":"customer-1.lakelive.example"}`)},
	)
	members := &fakeMembers{ready: map[string]bool{}}
	c := setTestClient(t, append(productionClusters(), set)...)
	if _, err := reconcileLakeSet(t, c, members, "lake-prod"); err != nil {
		t.Fatal(err)
	}
	if len(members.applied) != 3 {
		t.Fatalf("applied %d members", len(members.applied))
	}
	want := []oramav1alpha1.LakeRegion{
		{Name: "swedencentral", Label: "Europe", Host: "eu.lakelive.example", Latitude: "60.67488", Longitude: "17.14127"},
		{Name: "eastus", Label: "United States", Host: "us.lakelive.example", Latitude: "37.3719", Longitude: "-79.8164"},
	}
	eu, us, dedicated := members.applied[0].Spec, members.applied[1].Spec, members.applied[2].Spec
	for _, spec := range []map[string]any{eu, us} {
		raw, _ := json.Marshal(spec["regions"])
		var got []oramav1alpha1.LakeRegion
		if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("regions:\n got %s\nwant %+v", raw, want)
		}
	}
	if region := eu["region"].(map[string]any); region["name"] != "swedencentral" || region["label"] != "Europe" {
		t.Fatalf("eu region: %+v", region)
	}
	if region := us["region"].(map[string]any); region["name"] != "eastus" {
		t.Fatalf("us region: %+v", region)
	}
	if eu["apexHost"] != "lakelive.example" || us["apexHost"] != nil {
		t.Fatalf("apex: eu %v, us %v", eu["apexHost"], us["apexHost"])
	}
	if _, found := dedicated["region"]; found {
		t.Fatalf("a member that states no region is left as it is: %+v", dedicated)
	}
	if _, found := dedicated["regions"]; found {
		t.Fatalf("a member without a region gets no picker: %+v", dedicated)
	}
}

// A set whose region list cannot be right is refused whole, and nothing is applied.
func TestALakeSetRefusesARegionListThatCannotBeRight(t *testing.T) {
	cases := map[string]struct {
		members []oramav1alpha1.SetMember
		objects []client.Object
		want    string
	}{
		"two apex hosts": {members: []oramav1alpha1.SetMember{
			{ClusterRef: "example-production-eu", Name: "shared", Spec: rawJSON(`{"host":"eu.lakelive.example","apexHost":"lakelive.example"}`)},
			{ClusterRef: "example-production-us", Name: "shared-us", Spec: rawJSON(`{"host":"us.lakelive.example","apexHost":"lakelive.example"}`)},
		}, want: "at most one member serves the apex host, and these all set spec.apexHost: shared (lakelive.example), shared-us (lakelive.example)"},
		"a region with no host": {members: []oramav1alpha1.SetMember{
			{ClusterRef: "example-production-eu", Name: "shared", Spec: rawJSON(`{"region":{"label":"Europe"}}`)},
		}, want: `member shared has the region label "Europe" but no spec.host`},
		"one region twice (separate environments)": {members: []oramav1alpha1.SetMember{
			{ClusterRef: "example-production-eu", Name: "dev", Spec: rawJSON(`{"host":"lake-dev.example","region":{"label":"Dev"}}`)},
			{ClusterRef: "example-production-eu", Name: "staging", Spec: rawJSON(`{"host":"lake-staging.example","region":{"label":"Staging"}}`)},
		}, want: "members dev and staging both have a region label and both run in swedencentral"},
		"the list stated by hand": {members: []oramav1alpha1.SetMember{
			{ClusterRef: "example-production-eu", Name: "shared", Spec: rawJSON(`{"host":"eu.lakelive.example","regions":[]}`)},
		}, want: "member shared states spec.regions, which the set computes"},
		"a region with no known location": {members: []oramav1alpha1.SetMember{
			{ClusterRef: "example-production-asia", Name: "shared", Spec: rawJSON(`{"host":"jp.lakelive.example","region":{"label":"Japan"}}`)},
		}, objects: managedCluster("example-production-asia", "japaneast", "production"),
			want: `member shared runs on example-production-asia, in region "japaneast", whose location the operator does not know`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			members := &fakeMembers{ready: map[string]bool{}}
			objects := append(append(productionClusters(), tc.objects...), productionLakeSet(tc.members...))
			c := setTestClient(t, objects...)
			if _, err := reconcileLakeSet(t, c, members, "lake-prod"); err != nil {
				t.Fatal(err)
			}
			if len(members.applied) != 0 {
				t.Fatalf("a refused set applied %d members", len(members.applied))
			}
			if set := getLakeSet(t, c, "lake-prod"); set.Status.Phase != clusterPhaseDegraded || !strings.Contains(set.Status.Message, tc.want) {
				t.Fatalf("status %q: %q, want it to contain %q", set.Status.Phase, set.Status.Message, tc.want)
			}
		})
	}
}

// A member that states a region name other than its cluster's is refused, as for OramaCloudV1Set.
func TestALakeMemberCannotStateAnotherRegion(t *testing.T) {
	set := productionLakeSet(oramav1alpha1.SetMember{ClusterRef: "example-production-eu", Name: "shared",
		Spec: rawJSON(`{"host":"eu.lakelive.example","region":{"name":"eastus","label":"Europe"}}`)})
	members := &fakeMembers{ready: map[string]bool{}}
	c := setTestClient(t, append(productionClusters(), set)...)
	_, err := reconcileLakeSet(t, c, members, "lake-prod")
	if err == nil || !strings.Contains(err.Error(), `spec.region.name is "eastus", but the instance runs on example-production-eu, which is in swedencentral`) {
		t.Fatalf("got %v", err)
	}
}

func TestLakeRegionEnvComesOnlyFromTheSpec(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{
		IngressEnabled: true, Host: "eu.lakelive.example", ApexHost: "lakelive.example",
		Region:  &oramav1alpha1.LakeRegionSpec{Name: "swedencentral", Label: "Europe"},
		Regions: []oramav1alpha1.LakeRegion{{Name: "swedencentral", Label: "Europe", Host: "eu.lakelive.example", Latitude: "60.67488", Longitude: "17.14127"}},
		Env:     []corev1.EnvVar{{Name: "LAKE_REGION", Value: "by-hand"}, {Name: "LAKE_PUBLIC_HOST", Value: "eu.lakelive.example"}},
	}}
	got := map[string][]string{}
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		got[variable.Name] = append(got[variable.Name], variable.Value)
	}
	want := map[string]string{
		"LAKE_REGION": "swedencentral", "LAKE_REGION_LABEL": "Europe", "LAKE_APEX_HOST": "lakelive.example",
		"LAKE_REGIONS":     `[{"name":"swedencentral","label":"Europe","host":"eu.lakelive.example","latitude":"60.67488","longitude":"17.14127"}]`,
		"LAKE_PUBLIC_HOST": "eu.lakelive.example",
	}
	for name, value := range want {
		if len(got[name]) != 1 || got[name][0] != value {
			t.Errorf("%s = %v, want [%s]", name, got[name], value)
		}
	}

	bare := lakeEnv(&oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{ApexHost: "lakelive.example"}}, lakeSealingMaterial{})
	for _, variable := range bare {
		for _, name := range lakeRegionEnvNames {
			if variable.Name == name {
				t.Errorf("a Lake with no region and no ingress got %s=%s", name, variable.Value)
			}
		}
	}
}

// The apex host is a second rule on the Lake's Ingress and its own DNS record; removing it
// removes both.
func TestLakeApexHostGetsAnIngressRuleAndADNSRecord(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "hashicorp/http-echo:1.0", IngressEnabled: true, Host: "eu.lakelive.example", ApexHost: "lakelive.example",
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token-lake-live"},
			TLS:          oramav1alpha1.LakeTLSSpec{SecretName: "lake-origin-tls"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	reconcileLake(t, c, s, "shared")

	var ing networkingv1.Ingress
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-shared"}, &ing); err != nil {
		t.Fatal(err)
	}
	if len(ing.Spec.Rules) != 2 || ing.Spec.Rules[0].Host != "eu.lakelive.example" || ing.Spec.Rules[1].Host != "lakelive.example" ||
		ing.Spec.Rules[1].HTTP.Paths[0].Backend.Service.Name != "lake-shared" {
		t.Fatalf("rules: %+v", ing.Spec.Rules)
	}
	if len(ing.Spec.TLS) != 1 || strings.Join(ing.Spec.TLS[0].Hosts, ",") != "eu.lakelive.example,lakelive.example" {
		t.Fatalf("TLS: %+v", ing.Spec.TLS)
	}

	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "20.91.1.2"}}
	if err := c.Status().Update(ctx, &ing); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "shared")
	var apex oramav1alpha1.CloudflareDNSRecord
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-shared-apex-dns"}, &apex); err != nil {
		t.Fatalf("apex DNS record: %v", err)
	}
	if apex.Spec.Name != "lakelive.example" || apex.Spec.Content != "20.91.1.2" || !apex.Spec.Proxied ||
		apex.Spec.SecretRef != "cloudflare-api-token-lake-live" || len(apex.OwnerReferences) != 1 {
		t.Fatalf("apex DNS record: %+v", apex)
	}

	if err := c.Get(ctx, types.NamespacedName{Name: "shared"}, lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.ApexHost = ""
	if err := c.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "shared")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-shared-apex-dns"}, &apex); err == nil {
		t.Fatal("the apex DNS record outlived the apex host")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake-dev", Name: "lake-shared"}, &ing); err != nil {
		t.Fatal(err)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "eu.lakelive.example" {
		t.Fatalf("rules after removing the apex: %+v", ing.Spec.Rules)
	}
}
