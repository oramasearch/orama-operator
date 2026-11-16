// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testOperatorNamespace = "orama-system"

func zoneTokenSecret(name, zoneID string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lake"},
		Data: map[string][]byte{"token": []byte("test-token"), "zoneID": []byte(zoneID)}}
}

// lakeLiveSet is lake-prod's shape: one Lake on each of two production clusters, both in the
// lakelive.example zone through the token the set copies.
func lakeLiveSet() *oramav1alpha1.LakeSet {
	return &oramav1alpha1.LakeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-prod"},
		Spec: oramav1alpha1.LakeSetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(`{"image":"oramasearch/lake:v0.4.0","ingressEnabled":true,"dnsSecretRef":{"name":"cloudflare-api-token-lake-live"}}`),
			Secrets:  []oramav1alpha1.SetSecret{{Name: "cloudflare-api-token-lake-live", Namespace: "lake"}},
			Clusters: []oramav1alpha1.SetMember{
				{ClusterRef: "example-production-eu", Name: "shared", Spec: rawJSON(`{"host":"eu.lakelive.example"}`)},
				{ClusterRef: "example-production-us", Name: "shared-us", Spec: rawJSON(`{"host":"us.lakelive.example"}`)},
			},
		}},
	}
}

func lakeLiveObjects() []client.Object {
	objects := append(managedCluster("example-production-eu", "swedencentral", productionPurpose),
		managedCluster("example-production-us", "eastus", productionPurpose)...)
	return append(objects, zoneTokenSecret("cloudflare-api-token-lake-live", "zone-1"))
}

func reconcileLakeSetAt(t *testing.T, c client.Client, members *fakeMembers, name string, now func() time.Time) error {
	t.Helper()
	r := &LakeSetReconciler{Client: c, Scheme: clusterTestScheme(t), Applier: members, OperatorNamespace: testOperatorNamespace, Now: now}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	return err
}

// copiedOriginPull is the zone origin-pull Secret in one applied member's Secrets, or nil.
func copiedOriginPull(req memberRequest) *corev1.Secret {
	for _, secret := range req.Secrets {
		if secret.Name == originPullSecretName("zone-1") {
			return secret
		}
	}
	return nil
}

func managingOriginPull(t *testing.T, c client.Client) corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testOperatorNamespace, Name: originPullSecretName("zone-1")}, &secret); err != nil {
		t.Fatalf("the managing cluster's zone CA: %v", err)
	}
	return secret
}

// The managing cluster creates the zone's CA once, and every member serving the zone receives the
// same CA and client certificate, without the CA key. A later pass keeps the CA; a renewal reaches
// every member with the same new client certificate and the same CA.
func TestALakeSetCopiesOneZoneCAToEveryMemberServingTheZone(t *testing.T) {
	c := setTestClient(t, append(lakeLiveObjects(), lakeLiveSet())...)
	members := &fakeMembers{}
	if err := reconcileLakeSetAt(t, c, members, "lake-prod", nil); err != nil {
		t.Fatal(err)
	}
	source := managingOriginPull(t, c)
	if len(source.Data["ca.key"]) == 0 || source.Labels[secretPurposeLabel] != productionPurpose {
		t.Fatalf("managing Secret: labels %v, keys %d", source.Labels, len(source.Data))
	}
	if len(members.applied) != 2 {
		t.Fatalf("applied %d members", len(members.applied))
	}
	eu, us := copiedOriginPull(members.applied[0]), copiedOriginPull(members.applied[1])
	if eu == nil || us == nil {
		t.Fatalf("a member serving the zone did not receive its CA: %v, %v", eu, us)
	}
	for _, copy := range []*corev1.Secret{eu, us} {
		if string(copy.Data["ca.crt"]) != string(source.Data["ca.crt"]) || string(copy.Data["tls.crt"]) != string(source.Data["tls.crt"]) ||
			string(copy.Data["tls.key"]) != string(source.Data["tls.key"]) {
			t.Fatal("a member received a different CA or client certificate")
		}
		if _, found := copy.Data["ca.key"]; found {
			t.Fatal("the CA key left the managing cluster")
		}
		if copy.Labels[originPullDistributedLabel] != "distributed" {
			t.Fatalf("labels = %v", copy.Labels)
		}
	}

	if err := reconcileLakeSetAt(t, c, members, "lake-prod", nil); err != nil {
		t.Fatal(err)
	}
	if again := managingOriginPull(t, c); string(again.Data["ca.crt"]) != string(source.Data["ca.crt"]) || again.ResourceVersion != source.ResourceVersion {
		t.Fatal("a second pass changed the zone's CA Secret")
	}

	later := func() time.Time { return time.Now().Add(originPullLeafLifetime - originPullRenewBefore + time.Hour) }
	members.applied = nil
	if err := reconcileLakeSetAt(t, c, members, "lake-prod", later); err != nil {
		t.Fatal(err)
	}
	renewed := managingOriginPull(t, c)
	if string(renewed.Data["ca.crt"]) != string(source.Data["ca.crt"]) || string(renewed.Data["tls.crt"]) == string(source.Data["tls.crt"]) {
		t.Fatal("renewal must keep the CA and replace the client certificate")
	}
	for _, req := range members.applied {
		if copy := copiedOriginPull(req); copy == nil || string(copy.Data["tls.crt"]) != string(renewed.Data["tls.crt"]) {
			t.Fatalf("member %s did not receive the renewed client certificate", req.Name)
		}
	}
}

// A CA that cannot be read fails the members that need it and is never replaced.
func TestALakeSetNeverReplacesAnUnreadableZoneCA(t *testing.T) {
	broken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: originPullSecretName("zone-1"), Namespace: testOperatorNamespace},
		Data: map[string][]byte{"ca.crt": []byte("not a certificate"), "ca.key": []byte("x")}}
	c := setTestClient(t, append(lakeLiveObjects(), lakeLiveSet(), broken)...)
	members := &fakeMembers{}
	if err := reconcileLakeSetAt(t, c, members, "lake-prod", nil); err == nil {
		t.Fatal("an unreadable CA must fail the members")
	}
	if len(members.applied) != 0 {
		t.Fatal("a member was applied without its zone CA")
	}
	if got := managingOriginPull(t, c); string(got.Data["ca.crt"]) != "not a certificate" {
		t.Fatal("the unreadable CA was replaced")
	}
	if set := getLakeSet(t, c, "lake-prod"); !strings.Contains(set.Status.Message, "ca.crt is not a CA certificate") {
		t.Fatalf("message = %q", set.Status.Message)
	}
}

// A zone's CA made for production members never reaches a nonprod member, even one whose token
// names the same zone.
func TestAProductionZoneCANeverReachesANonprodMember(t *testing.T) {
	objects := append(lakeLiveObjects(), lakeLiveSet())
	objects = append(objects, managedCluster("example-nonprod", "westus2", "nonprod")...)
	objects = append(objects, zoneTokenSecret("cloudflare-api-token", "zone-1"), &oramav1alpha1.LakeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-nonprod"},
		Spec: oramav1alpha1.LakeSetSpec{ManagedSetSpec: oramav1alpha1.ManagedSetSpec{
			Template: *rawJSON(`{"image":"oramasearch/lake:v0.4.0","dnsSecretRef":{"name":"cloudflare-api-token"}}`),
			Secrets:  []oramav1alpha1.SetSecret{{Name: "cloudflare-api-token", Namespace: "lake"}},
			Clusters: []oramav1alpha1.SetMember{{ClusterRef: "example-nonprod", Name: "dev"}},
		}},
	})
	c := setTestClient(t, objects...)
	if err := reconcileLakeSetAt(t, c, &fakeMembers{}, "lake-prod", nil); err != nil {
		t.Fatal(err)
	}
	members := &fakeMembers{}
	_ = reconcileLakeSetAt(t, c, members, "lake-nonprod", nil)
	if len(members.applied) != 0 {
		t.Fatal("the production zone CA was copied into a nonprod member")
	}
	if set := getLakeSet(t, c, "lake-nonprod"); !strings.Contains(set.Status.Message, "never copied there") {
		t.Fatalf("message = %q", set.Status.Message)
	}
}

// A member without a zone credential the set copies gets no origin-pull Secret.
func TestALakeSetCopiesNoZoneCAWithoutAZone(t *testing.T) {
	c := setTestClient(t, append(nonprodObjects(), nonprodLakeSet())...)
	members := &fakeMembers{}
	if err := reconcileLakeSetAt(t, c, members, "lake-nonprod", nil); err != nil {
		t.Fatal(err)
	}
	for _, req := range members.applied {
		for _, secret := range req.Secrets {
			if strings.HasPrefix(secret.Name, "orama-edge-origin-pull-") {
				t.Fatalf("member %s received %s", req.Name, secret.Name)
			}
		}
	}
}

// pemSubjects are the subjects of every certificate in a PEM bundle: what an origin configured
// with it lists in its CertificateRequest.
func pemSubjects(t *testing.T, bundle string) [][]byte {
	t.Helper()
	var subjects [][]byte
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return subjects
		}
		cert, err := parsePEMCertificate(string(pem.EncodeToMemory(block)))
		if err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, cert.RawSubject)
	}
}

// twoClusterZone is one zone served by the managing cluster (local Secret) and a member cluster.
// The member's origins are eu.lakelive.example and *.eu.lakelive.example, the managing cluster's lakelive.example; each
// origin requires a client certificate and accepts the bundle its own cluster's policy publishes.
type twoClusterZone struct {
	zone                   *fakeCloudflareZone
	managing, member       client.Client
	managingR, memberR     *CloudflareZonePolicyReconciler
	managingGet, memberGet func() oramav1alpha1.CloudflareZonePolicy
}

func newTwoClusterZone(t *testing.T, distributed bool) *twoClusterZone {
	t.Helper()
	tz := &twoClusterZone{zone: strictZone()}
	bundleOf := func(c client.Client) [][]byte {
		var policy oramav1alpha1.CloudflareZonePolicy
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "edge"}, &policy)
		if policy.Status.OriginClientCAPEM == "" {
			return pemSubjects(t, cloudflareOriginPullCA)
		}
		return pemSubjects(t, policy.Status.OriginClientCAPEM)
	}
	prober := fakeEdgeProber{clientCAs: func(name string) (bool, [][]byte, error) {
		if strings.Contains(name, "eu.lakelive.example") {
			return true, bundleOf(tz.member), nil
		}
		return true, bundleOf(tz.managing), nil
	}}
	tz.managing, tz.managingR, tz.managingGet = newZonePolicyFixture(t, tz.zone, prober)
	tz.managingR.OperatorNamespace = testOperatorNamespace
	tz.member, tz.memberR, tz.memberGet = newZonePolicyFixture(t, tz.zone, prober)
	tz.memberR.OperatorNamespace = testOperatorNamespace
	if distributed {
		tz.distribute(t, nil)
		var policy oramav1alpha1.CloudflareZonePolicy
		key := types.NamespacedName{Namespace: "lake", Name: "edge"}
		if err := tz.member.Get(context.Background(), key, &policy); err != nil {
			t.Fatal(err)
		}
		policy.Spec.OriginPullSecretRef = &corev1.LocalObjectReference{Name: originPullSecretName("zone-1")}
		if err := tz.member.Update(context.Background(), &policy); err != nil {
			t.Fatal(err)
		}
	}
	return tz
}

// distribute runs the LakeSet's copy against the managing cluster and writes what it would apply
// into the member cluster, as applyMemberWith does.
func (tz *twoClusterZone) distribute(t *testing.T, now func() time.Time) {
	t.Helper()
	r := &managedSetReconciler{Client: tz.managing, operatorNamespace: testOperatorNamespace, now: now}
	resolved := resolvedMember{cluster: &oramav1alpha1.KubernetesCluster{Spec: oramav1alpha1.KubernetesClusterSpec{Tags: map[string]string{clusterPurposeTag: productionPurpose}}}}
	spec := map[string]any{"dnsSecretRef": map[string]any{"name": "cf"}}
	secrets, err := lakeOriginPullSecrets(context.Background(), r, spec, []*corev1.Secret{zoneTokenSecret("cf", "zone-1")}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	copy := secrets[len(secrets)-1]
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: copy.Name, Namespace: "lake", Labels: copy.Labels}, Type: copy.Type, Data: copy.Data}
	var existing corev1.Secret
	if err := tz.member.Get(context.Background(), client.ObjectKeyFromObject(target), &existing); err == nil {
		existing.Data = copy.Data
		if err := tz.member.Update(context.Background(), &existing); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := tz.member.Create(context.Background(), target); err != nil {
		t.Fatal(err)
	}
}

func (tz *twoClusterZone) rounds(n int) (managing, member oramav1alpha1.CloudflareZonePolicy) {
	for i := 0; i < n; i++ {
		managing = tz.managingGet()
		member = tz.memberGet()
	}
	return managing, member
}

// The point of option (a): two clusters serving one zone, the member with the CA the managing
// cluster distributed, both publish the same CA, so each one's probe finds every origin of the
// zone trusting it, and the zone switches to one client certificate of that CA.
func TestTwoClustersServingOneZonePresentTheSameCA(t *testing.T) {
	tz := newTwoClusterZone(t, true)
	managing, member := tz.rounds(3)
	source := managingOriginPull(t, tz.managing)
	if len(tz.zone.clientCerts) != 1 || !tz.zone.zoneAOP || tz.zone.clientCerts[0].Certificate != string(source.Data["tls.crt"]) {
		t.Fatalf("zone certs = %d, enabled = %v, patches = %v\nmanaging %+v\nmember %+v", len(tz.zone.clientCerts), tz.zone.zoneAOP, tz.zone.patches, managing.Status.Message, member.Status.Message)
	}
	for _, policy := range []oramav1alpha1.CloudflareZonePolicy{managing, member} {
		if !policy.Status.ZoneOriginPulls || !policy.Status.AuthenticatedOriginPulls || policy.Status.OriginClientCAPEM != string(source.Data["ca.crt"]) {
			t.Fatalf("status = %+v", policy.Status)
		}
	}
	var secrets corev1.SecretList
	_ = tz.member.List(context.Background(), &secrets, client.InNamespace(testOperatorNamespace))
	if len(secrets.Items) != 0 {
		t.Fatal("the member cluster minted a CA of its own")
	}
	patches := len(tz.zone.patches)
	tz.rounds(2)
	if len(tz.zone.patches) != patches {
		t.Fatalf("a steady zone was changed: %v", tz.zone.patches[patches:])
	}
}

// Without distribution each cluster has its own CA and neither switches: each one's probe finds
// the other's origins not trusting its CA. This is why the CA is distributed.
func TestTwoClustersWithTheirOwnCAsStayOnGlobalPulls(t *testing.T) {
	tz := newTwoClusterZone(t, false)
	managing, member := tz.rounds(3)
	if len(tz.zone.clientCerts) != 0 || tz.zone.zoneAOP {
		t.Fatalf("switched with two CAs: %v", tz.zone.patches)
	}
	for _, policy := range []oramav1alpha1.CloudflareZonePolicy{managing, member} {
		if policy.Status.ZoneOriginPulls || !strings.Contains(policy.Status.Message, "accepts the zone's origin-pull CA") {
			t.Fatalf("status = %+v", policy.Status)
		}
	}
}

// While a renewed client certificate has reached one cluster and not yet the other, neither undoes
// the other: the newest leaf of the CA is the zone's, the cluster with the older copy neither
// uploads it again nor deletes the newer one.
func TestClustersServingOneZoneNeverUndoEachOthersRenewal(t *testing.T) {
	tz := newTwoClusterZone(t, true)
	tz.rounds(3)
	later := func() time.Time { return time.Now().Add(originPullLeafLifetime - originPullRenewBefore + time.Hour) }
	tz.managingR.Now = later
	tz.managingGet() // renews the managing Secret's leaf and uploads it
	renewed := managingOriginPull(t, tz.managing)
	if len(tz.zone.clientCerts) != 1 || tz.zone.clientCerts[0].Certificate != string(renewed.Data["tls.crt"]) {
		t.Fatalf("zone certs after renewal = %+v", tz.zone.clientCerts)
	}
	patches := len(tz.zone.patches)
	member := tz.memberGet() // still holds the old copy
	if len(tz.zone.patches) != patches || !member.Status.ZoneOriginPulls {
		t.Fatalf("the cluster with the older copy changed the zone: %v, status %+v", tz.zone.patches[patches:], member.Status)
	}
	tz.distribute(t, later)
	tz.rounds(2)
	if len(tz.zone.patches) != patches || len(tz.zone.clientCerts) != 1 {
		t.Fatalf("after the copy arrived: %v", tz.zone.patches[patches:])
	}
}
