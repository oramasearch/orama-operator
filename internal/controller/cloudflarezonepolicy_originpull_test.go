/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
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

func strictZone() *fakeCloudflareZone {
	return &fakeCloudflareZone{
		settings: map[string]string{"ssl": "strict", "tls_client_auth": "on", "always_use_https": "on"},
		records:  zoneRecords(),
	}
}

// zoneCA reads the zone's operator CA from its Secret in the fixture's namespace.
func zoneCA(t *testing.T, c client.Client) (*x509.Certificate, corev1.Secret) {
	t.Helper()
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: originPullSecretName("zone-1")}, &secret); err != nil {
		t.Fatalf("origin-pull Secret: %v", err)
	}
	ca, err := parsePEMCertificate(string(secret.Data["ca.crt"]))
	if err != nil {
		t.Fatal(err)
	}
	return ca, secret
}

func globalCASubject(t *testing.T) []byte {
	t.Helper()
	ca, err := parsePEMCertificate(cloudflareOriginPullCA)
	if err != nil {
		t.Fatal(err)
	}
	return ca.RawSubject
}

// A zone whose origins ask for no client certificate yet: the operator makes a CA and a leaf,
// uploads the leaf, turns zone-level pulls on, and publishes its CA alone. A steady zone writes
// nothing more.
func TestZoneOriginPullsUploadsTheOperatorsCertificate(t *testing.T) {
	zone := strictZone()
	c, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	first := reconcile()
	ca, secret := zoneCA(t, c)
	if !ca.IsCA || len(secret.Data["tls.key"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatalf("secret = %v", secret.Data)
	}
	if len(zone.clientCerts) != 1 || !zone.zoneAOP || zone.clientCerts[0].Certificate != string(secret.Data["tls.crt"]) {
		t.Fatalf("zone certs = %+v, enabled = %v", zone.clientCerts, zone.zoneAOP)
	}
	leaf, _ := parsePEMCertificate(zone.clientCerts[0].Certificate)
	if leaf.CheckSignatureFrom(ca) != nil || leaf.IsCA || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("uploaded leaf is not a client certificate of the CA")
	}
	st := first.Status
	if st.Phase != edgePhaseReady || !st.ZoneOriginPulls || !st.AuthenticatedOriginPulls || st.OriginClientCAPEM != string(secret.Data["ca.crt"]) {
		t.Fatalf("status = %+v", st)
	}
	if conditionStatus(first, edgeConditionZoneOriginPulls) != "True" {
		t.Fatalf("conditions = %+v", st.Conditions)
	}
	patches := len(zone.patches)
	second := reconcile()
	if len(zone.patches) != patches || second.ResourceVersion != first.ResourceVersion {
		t.Fatalf("a steady zone was changed: %v", zone.patches[patches:])
	}
}

// An origin that requires Cloudflare's global certificate and does not accept the operator's CA
// keeps the zone on global pulls: no upload, no switch, and the bundle accepts both CAs. Once the
// origin accepts the operator's CA too, the zone switches.
func TestZoneOriginPullsWaitUntilEveryOriginTrustsTheCA(t *testing.T) {
	zone := strictZone()
	trusting := false
	var c client.Client
	prober := fakeEdgeProber{clientCAs: func(name string) (bool, [][]byte, error) {
		acceptable := [][]byte{globalCASubject(t)}
		if trusting && name == "eu.lake.live" {
			ca, _ := zoneCA(t, c)
			acceptable = append(acceptable, ca.RawSubject)
		}
		if name == "lake.live" {
			return false, nil, nil // an origin that asks for no client certificate
		}
		return name == "eu.lake.live", acceptable, nil
	}}
	c, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	if len(zone.clientCerts) != 0 || zone.zoneAOP {
		t.Fatalf("switched while an origin does not trust the CA: %v", zone.patches)
	}
	st := policy.Status
	if st.ZoneOriginPulls || !st.AuthenticatedOriginPulls || st.Phase != edgePhaseProgressing {
		t.Fatalf("status = %+v", st)
	}
	_, secret := zoneCA(t, c)
	if st.OriginClientCAPEM != cloudflareOriginPullCA+string(secret.Data["ca.crt"]) {
		t.Fatalf("bundle does not accept both CAs")
	}
	if conditionStatus(policy, edgeConditionZoneOriginPulls) != "False" || !strings.Contains(st.Message, "not yet: eu.lake.live") {
		t.Fatalf("message = %q", st.Message)
	}
	trusting = true
	policy = reconcile()
	if len(zone.clientCerts) != 1 || !zone.zoneAOP || !policy.Status.ZoneOriginPulls || policy.Status.OriginClientCAPEM != string(secret.Data["ca.crt"]) {
		t.Fatalf("did not switch once every origin trusts the CA: %+v", policy.Status)
	}
}

// An origin that cannot be probed blocks the switch: whether it would refuse the edge is unknown.
func TestZoneOriginPullsWaitForAnUnreachableOrigin(t *testing.T) {
	zone := strictZone()
	prober := fakeEdgeProber{clientCAs: func(name string) (bool, [][]byte, error) {
		if name == "lake.live" {
			return false, nil, errors.New("i/o timeout")
		}
		return false, nil, nil
	}}
	_, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	if len(zone.clientCerts) != 0 || zone.zoneAOP || !strings.Contains(policy.Status.Message, "lake.live (no answer)") {
		t.Fatalf("status = %+v, patches = %v", policy.Status, zone.patches)
	}
}

// A leaf close to expiry is replaced: the new one is uploaded while the old one still serves, the
// origins keep verifying against the operator's CA alone, and the old leaf is deleted once the new
// one is active. A certificate of another CA is never deleted.
func TestZoneOriginPullsRenewTheLeaf(t *testing.T) {
	zone := strictZone()
	c, r, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	reconcile()
	_, before := zoneCA(t, c)
	oldID := zone.clientCerts[0].ID

	r.Now = func() time.Time { return time.Now().Add(originPullLeafLifetime - originPullRenewBefore + time.Hour) }
	zone.uploadStatus = "pending_deployment"
	policy := reconcile()
	_, after := zoneCA(t, c)
	if string(after.Data["ca.crt"]) != string(before.Data["ca.crt"]) {
		t.Fatal("renewing the leaf replaced the CA")
	}
	if string(after.Data["tls.crt"]) == string(before.Data["tls.crt"]) || len(zone.clientCerts) != 2 {
		t.Fatalf("the leaf was not renewed: %v", zone.patches)
	}
	if !policy.Status.ZoneOriginPulls || policy.Status.OriginClientCAPEM != string(after.Data["ca.crt"]) {
		t.Fatalf("renewal widened the bundle: %+v", policy.Status)
	}
	zone.clientCerts[1].Status = "active"
	reconcile()
	if len(zone.clientCerts) != 1 || zone.clientCerts[0].ID == oldID {
		t.Fatalf("the old leaf was not deleted: %+v", zone.clientCerts)
	}
}

// A zone that may present a certificate of another CA: the origins must not require one (the bundle
// cannot verify it), the other certificate is never deleted, and the zone is not switched.
func TestZoneOriginPullsLeaveAForeignCertificate(t *testing.T) {
	zone := strictZone()
	otherCA, otherKey, _ := newOriginPullCA("other", time.Now())
	caCert, _ := parsePEMCertificate(otherCA)
	caKey, _ := parsePEMRSAKey(otherKey)
	foreign, _, _ := newOriginPullLeaf("other", caCert, caKey, time.Now())
	zone.clientCerts = []cfZoneClientCert{{ID: "foreign", Certificate: foreign, Status: "active"}}
	zone.zoneAOP = true
	prober := fakeEdgeProber{clientCAs: func(name string) (bool, [][]byte, error) {
		return true, [][]byte{caCert.RawSubject}, nil // the other cluster's origins
	}}
	_, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	if len(zone.clientCerts) != 1 || zone.clientCerts[0].ID != "foreign" {
		t.Fatalf("zone certs = %+v", zone.clientCerts)
	}
	if policy.Status.AuthenticatedOriginPulls || policy.Status.ZoneOriginPulls {
		t.Fatalf("origins would require a certificate the bundle cannot verify: %+v", policy.Status)
	}
}

// Two policies naming one zone share the zone's CA and leaf, so they upload one certificate.
func TestZoneOriginPullsShareOneCertificatePerZone(t *testing.T) {
	zone := strictZone()
	c, r, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	reconcile()
	second := &oramav1alpha1.CloudflareZonePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-2", Namespace: "lake"},
		Spec:       oramav1alpha1.CloudflareZonePolicySpec{ZoneID: "zone-1", APITokenSecretRef: oramav1alpha1.EdgeProviderSecretKeyRef{Name: "cf", Key: "token"}},
	}
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: "edge-2"}}); err != nil {
		t.Fatal(err)
	}
	if len(zone.clientCerts) != 1 {
		t.Fatalf("two policies uploaded %d certificates", len(zone.clientCerts))
	}
	var got oramav1alpha1.CloudflareZonePolicy
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "edge-2"}, &got)
	if !got.Status.ZoneOriginPulls {
		t.Fatalf("status = %+v", got.Status)
	}
}

// A token without SSL and Certificates is named; global pulls carry on and the zone is not failed
// for it.
func TestZoneOriginPullsNameTheMissingPermission(t *testing.T) {
	zone := strictZone()
	zone.deny = map[string]bool{"GET origin_tls_client_auth": true}
	_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	policy := reconcile()
	st := policy.Status
	if st.Phase != edgePhaseProgressing || !strings.Contains(st.Message, cfPermissionCertificatesRead) || !st.AuthenticatedOriginPulls || st.ZoneOriginPulls {
		t.Fatalf("status = %+v", st)
	}
	if !strings.HasPrefix(st.OriginClientCAPEM, cloudflareOriginPullCA) {
		t.Fatal("the bundle dropped Cloudflare's global CA while the zone presents it")
	}
	zone.deny = map[string]bool{"POST origin_tls_client_auth": true}
	policy = reconcile()
	if policy.Status.Phase != edgePhaseFailed || !strings.Contains(policy.Status.Message, cfPermissionCertificatesEdit) {
		t.Fatalf("status = %+v", policy.Status)
	}
}

// A refused read after the switch keeps the operator's CA as the bundle: going back to Cloudflare's
// global CA would make the origins refuse the zone's own certificate.
func TestZoneOriginPullsKeepTheBundleWhenTheZoneIsUnreadable(t *testing.T) {
	zone := strictZone()
	_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	first := reconcile()
	zone.mu.Lock()
	zone.deny = map[string]bool{"GET ssl": true}
	zone.mu.Unlock()
	policy := reconcile()
	if policy.Status.Phase != edgePhaseFailed || policy.Status.OriginClientCAPEM != first.Status.OriginClientCAPEM {
		t.Fatalf("status = %+v", policy.Status)
	}
}

// The real prober reads the CAs an origin accepts from its CertificateRequest.
func TestProbeClientCAsReadsTheCertificateRequest(t *testing.T) {
	caPEM, _, err := newOriginPullCA("zone-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := parsePEMCertificate(caPEM)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	server.TLS = &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	requested, acceptable, err := probeClientCAs(context.Background(), server.Listener.Addr().String(), "example.com")
	if err != nil || !requested || !clientCAAccepted(acceptable, ca) {
		t.Fatalf("requested=%v acceptable=%d err=%v", requested, len(acceptable), err)
	}

	plain := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer plain.Close()
	requested, _, err = probeClientCAs(context.Background(), plain.Listener.Addr().String(), "example.com")
	if err != nil || requested {
		t.Fatalf("an origin that asks for nothing: requested=%v err=%v", requested, err)
	}
}

func TestOriginPullSecretName(t *testing.T) {
	if got := originPullSecretName("0123abcdEF"); got != "orama-edge-origin-pull-0123abcdef" {
		t.Fatalf("name = %q", got)
	}
	if got := originPullSecretName("bad/zone"); len(got) != len("orama-edge-origin-pull-")+32 {
		t.Fatalf("name = %q", got)
	}
}
