/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for operator#396: an OramaCloudV1Set connects its read members to their primary.

const (
	euCluster = "orama-production-cluster-02"
	usCluster = "orama-production-cluster-03"
)

// productionClusters are the EU and US clusters; the US one reports egressIPs.
func productionClusters(egressIPs ...string) []client.Object {
	us := managedCluster(usCluster, "eastus", "production")
	us[0].(*oramav1alpha1.KubernetesCluster).Status.EgressIPs = egressIPs
	return append(managedCluster(euCluster, "swedencentral", "production"), us...)
}

func primaryAndReadSet(template, euSpec, usSpec string) *oramav1alpha1.OramaCloudV1Set {
	return &oramav1alpha1.OramaCloudV1Set{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-prod"},
		Spec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(template),
			Clusters: []oramav1alpha1.SetMember{
				{ClusterRef: euCluster, Name: "v1-eu", Namespace: "v1-eu", Spec: rawJSON(euSpec)},
				{ClusterRef: usCluster, Name: "v1-us", Namespace: "v1-us", Spec: rawJSON(usSpec)},
			},
		},
	}
}

func reconcileV1Set(t *testing.T, c client.Client, members *fakeMembers) error {
	t.Helper()
	members.applied = nil
	r := &OramaCloudV1SetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1-prod"}})
	return err
}

func getV1Set(t *testing.T, c client.Client) *oramav1alpha1.OramaCloudV1Set {
	t.Helper()
	var set oramav1alpha1.OramaCloudV1Set
	if err := c.Get(context.Background(), types.NamespacedName{Name: "v1-prod"}, &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

func appliedMember(members *fakeMembers, name string) *memberRequest {
	for i := range members.applied {
		if members.applied[i].Name == name {
			return &members.applied[i]
		}
	}
	return nil
}

func memberStatus(set *oramav1alpha1.OramaCloudV1Set, name string) oramav1alpha1.SetMemberStatus {
	for _, m := range set.Status.MemberStatuses {
		if m.Name == name {
			return m
		}
	}
	return oramav1alpha1.SetMemberStatus{}
}

// The read member waits for its primary to publish, then is applied with a peer naming the
// primary's endpoints and a copy of its credentials Secret. The primary allows the read member's
// cluster's egress IPs.
func TestASetConnectsAReadMemberToItsPrimary(t *testing.T) {
	set := primaryAndReadSet(`{"profile":"light","region":{"role":"primary"}}`, `{}`,
		`{"region":{"role":"read"},"components":{"controlDB":{"replicate":true}}}`)
	c := setTestClient(t, append(productionClusters("20.1.1.2", "20.1.1.1"), set)...)
	members := &fakeMembers{ready: map[string]bool{"v1-eu": true}}

	// The primary has published nothing yet.
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	eu := appliedMember(members, "v1-eu")
	readers, _ := eu.Spec["remoteReaders"].(map[string]any)
	if ranges, _ := readers["allowedSourceRanges"].([]any); len(ranges) != 2 || ranges[0] != "20.1.1.1/32" || ranges[1] != "20.1.1.2/32" {
		t.Fatalf("the primary allows the read member's egress IPs, sorted: %+v", eu.Spec["remoteReaders"])
	}
	if appliedMember(members, "v1-us") != nil {
		t.Fatal("a read member was applied before its primary published")
	}
	us := memberStatus(getV1Set(t, c), "v1-us")
	if us.Phase != clusterPhasePending || us.Message != "waiting for primary v1-eu on "+euCluster+" to publish its replication stream (status.remoteRead)" {
		t.Fatalf("read member status: %+v", us)
	}

	// Published, but its Secret is not readable yet.
	members.published = map[string]map[string]any{"v1-eu": {
		"brokerStreamEndpoint": "20.30.40.50:5551", "controlDBEndpoint": "20.30.40.60:5432", "credentialsSecret": "v1-eu-remote-read"}}
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if us = memberStatus(getV1Set(t, c), "v1-us"); !strings.Contains(us.Message, "waiting for Secret v1-eu/v1-eu-remote-read") {
		t.Fatalf("read member status: %+v", us)
	}

	members.secrets = map[string]*corev1.Secret{"kubeconfig-of-" + euCluster + " v1-eu/v1-eu-remote-read": {
		ObjectMeta: metav1.ObjectMeta{Name: "v1-eu-remote-read", Namespace: "v1-eu"}, Data: map[string][]byte{"MASTER_API_KEY": []byte("eu-key")}}}
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	read := appliedMember(members, "v1-us")
	if read == nil {
		t.Fatal("the read member was not applied once its primary published")
	}
	peers, _ := read.Spec["peers"].([]any)
	if len(peers) != 1 {
		t.Fatalf("peers: %+v", read.Spec["peers"])
	}
	peer := peers[0].(map[string]any)
	if peer["name"] != "v1-eu" || peer["region"] != "swedencentral" || peer["brokerStreamEndpoint"] != "20.30.40.50:5551" ||
		peer["postgresEndpoint"] != "20.30.40.60:5432" || peer["credentialsSecretRef"].(map[string]any)["name"] != "v1-eu-remote-read" {
		t.Fatalf("peer: %+v", peer)
	}
	if read.Spec["region"].(map[string]any)["name"] != "eastus" {
		t.Fatalf("the read member's region is still its cluster's: %+v", read.Spec["region"])
	}
	copied := read.Secrets[len(read.Secrets)-1]
	if copied.Name != "v1-eu-remote-read" || string(copied.Data["MASTER_API_KEY"]) != "eu-key" || copied.Labels["cnpg.io/reload"] != "true" {
		t.Fatalf("the primary's credentials Secret is copied to the read member: %+v", copied)
	}

	// Steady: no status write.
	members.ready["v1-us"] = true
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	before := getV1Set(t, c).ResourceVersion
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if getV1Set(t, c).ResourceVersion != before {
		t.Fatal("a steady set wrote its status")
	}
}

// A read member that does not replicate its control DB does not wait for a control-DB endpoint and
// gets no postgresEndpoint; one that does waits until the primary publishes it.
func TestAReadMemberThatReplicatesWaitsForTheControlDBEndpoint(t *testing.T) {
	set := primaryAndReadSet(`{}`, `{}`, `{"region":{"role":"read"},"components":{"controlDB":{"replicate":true}}}`)
	c := setTestClient(t, append(productionClusters("20.1.1.1"), set)...)
	members := &fakeMembers{published: map[string]map[string]any{"v1-eu": {"brokerStreamEndpoint": "20.30.40.50:5551", "credentialsSecret": "v1-eu-remote-read"}}}
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if us := memberStatus(getV1Set(t, c), "v1-us"); !strings.Contains(us.Message, "to publish its control database") {
		t.Fatalf("read member status: %+v", us)
	}
}

// A read member whose cluster has not reported its egress IPs waits: the primary would not let it in.
func TestAReadMemberWaitsForItsClustersEgressIPs(t *testing.T) {
	set := primaryAndReadSet(`{}`, `{}`, `{"region":{"role":"read"}}`)
	c := setTestClient(t, append(productionClusters(), set)...)
	members := &fakeMembers{}
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if _, stated := appliedMember(members, "v1-eu").Spec["remoteReaders"]; stated {
		t.Fatal("the primary allows a read member whose addresses are unknown")
	}
	us := memberStatus(getV1Set(t, c), "v1-us")
	if us.Phase != clusterPhasePending || us.Message != "waiting for KubernetesCluster "+usCluster+" to report its egress IPs, which primary v1-eu must allow" {
		t.Fatalf("read member status: %+v", us)
	}
}

// A set with read members has exactly one primary; one without is left as it is.
func TestASetRefusesReadMembersWithoutExactlyOnePrimary(t *testing.T) {
	for name, tc := range map[string]struct{ template, eu, us, message string }{
		"two primaries": {`{}`, `{}`, `{}`, ""},
		"no primary":    {`{"region":{"role":"read"}}`, `{}`, `{}`, "need exactly one primary in the set to read from, and it has 0: nothing is applied"},
	} {
		t.Run(name, func(t *testing.T) {
			set := primaryAndReadSet(tc.template, tc.eu, tc.us)
			c := setTestClient(t, append(productionClusters("20.1.1.1"), set)...)
			members := &fakeMembers{}
			if err := reconcileV1Set(t, c, members); err != nil {
				t.Fatal(err)
			}
			got := getV1Set(t, c)
			if tc.message == "" {
				for _, m := range members.applied {
					if _, stated := m.Spec["remoteReaders"]; stated || m.Spec["peers"] != nil {
						t.Fatalf("a set without read members writes no cross-cluster fields: %+v", m.Spec)
					}
				}
				if len(members.applied) != 2 {
					t.Fatalf("both primaries are applied, got %d", len(members.applied))
				}
				return
			}
			if len(members.applied) != 0 || got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, tc.message) {
				t.Fatalf("applied %d, status %q %q", len(members.applied), got.Status.Phase, got.Status.Message)
			}
		})
	}

	// Two primaries and a read member.
	set := &oramav1alpha1.OramaCloudV1Set{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-prod"},
		Spec: oramav1alpha1.ManagedSetSpec{Template: *rawJSON(`{}`), Clusters: []oramav1alpha1.SetMember{
			{ClusterRef: euCluster, Name: "v1-eu"}, {ClusterRef: euCluster, Name: "v1-eu-2"},
			{ClusterRef: usCluster, Name: "v1-us", Spec: rawJSON(`{"region":{"role":"read"}}`)},
		}},
	}
	c := setTestClient(t, append(productionClusters("20.1.1.1"), set)...)
	members := &fakeMembers{}
	if err := reconcileV1Set(t, c, members); err != nil {
		t.Fatal(err)
	}
	if got := getV1Set(t, c); len(members.applied) != 0 || got.Status.Phase != clusterPhaseDegraded ||
		!strings.Contains(got.Status.Message, "it has 2 (v1-eu on "+euCluster+", v1-eu-2 on "+euCluster+")") {
		t.Fatalf("applied %d, status %q %q", len(members.applied), got.Status.Phase, got.Status.Message)
	}
}

// The set owns remoteReaders and peers: a template or member that states one is refused.
func TestASetRefusesHandStatedCrossClusterFields(t *testing.T) {
	for field, set := range map[string]*oramav1alpha1.OramaCloudV1Set{
		"remoteReaders": primaryAndReadSet(`{"remoteReaders":{"allowedSourceRanges":["1.2.3.4/32"]}}`, `{}`, `{"region":{"role":"read"}}`),
		"peers":         primaryAndReadSet(`{}`, `{}`, `{"region":{"role":"read"},"peers":[{"name":"v1-eu"}]}`),
	} {
		t.Run(field, func(t *testing.T) {
			c := setTestClient(t, append(productionClusters("20.1.1.1"), set)...)
			members := &fakeMembers{}
			err := reconcileV1Set(t, c, members)
			if err == nil || !strings.Contains(err.Error(), "spec."+field+" is written by the set") {
				t.Fatalf("want a stated %s refused, got %v", field, err)
			}
			if appliedMember(members, "v1-us") != nil {
				t.Fatal("the member that states it was applied")
			}
		})
	}
}
