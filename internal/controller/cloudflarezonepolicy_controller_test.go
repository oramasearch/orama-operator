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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeCloudflareZone is the zone settings and DNS endpoints, recording every change.
type fakeCloudflareZone struct {
	mu       sync.Mutex
	settings map[string]string
	records  []cfDNSRecord
	// name is the zone's name, answered at GET /zones/zone-1; empty answers 404.
	name string
	// deny maps "METHOD setting" (or "GET dns_records", "GET zone", "GET certificate_packs",
	// "POST order", "POST order subscription", "GET origin_tls_client_auth",
	// "POST origin_tls_client_auth") to a refusal.
	deny    map[string]bool
	patches []string
	// packs are the zone's certificate packs; orders records every order's hosts.
	packs  []cfCertificatePack
	orders [][]string
	// clientCerts are the zone-level origin-pull certificates; zoneAOP is their enablement.
	clientCerts []cfZoneClientCert
	zoneAOP     bool
	// uploadStatus is the status of an uploaded certificate (default active).
	uploadStatus string
	nextCert     int
}

func (z *fakeCloudflareZone) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		z.mu.Lock()
		defer z.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"success":false}`, http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/zones/zone-1/")
		switch {
		case r.URL.Path == "/zones/zone-1" && r.Method == http.MethodGet:
			if z.deny["GET zone"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if z.name == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "zone-1", "name": z.name}})
		case path == "ssl/certificate_packs" && r.Method == http.MethodGet:
			if z.deny["GET certificate_packs"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("status") != "all" {
				t.Errorf("certificate packs listed without status=all")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": z.packs, "result_info": map[string]int{"page": 1, "total_pages": 1}})
		case path == "ssl/certificate_packs/order" && r.Method == http.MethodPost:
			if z.deny["POST order"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if z.deny["POST order subscription"] {
				http.Error(w, `{"success":false,"errors":[{"code":1450,"message":"no subscription"}]}`, http.StatusBadRequest)
				return
			}
			var order cfCertificatePackOrder
			_ = json.NewDecoder(r.Body).Decode(&order)
			if order.Type != "advanced" || order.ValidationMethod != "txt" || order.ValidityDays != edgeCertificateValidityDays || order.CertificateAuthority == "" {
				t.Errorf("order = %+v", order)
			}
			z.orders = append(z.orders, order.Hosts)
			z.packs = append(z.packs, cfCertificatePack{ID: fmt.Sprintf("pack-%d", len(z.packs)), Type: "advanced", Hosts: order.Hosts, Status: "initializing"})
			_ = json.NewEncoder(w).Encode(map[string]any{"result": z.packs[len(z.packs)-1]})
		case path == "dns_records" && r.Method == http.MethodGet:
			if z.deny["GET dns_records"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("proxied") != "true" {
				t.Errorf("records listed without proxied=true")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": z.records, "result_info": map[string]int{"page": 1, "total_pages": 1}})
		case path == "origin_tls_client_auth/settings":
			if z.deny[r.Method+" origin_tls_client_auth"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if r.Method == http.MethodPut {
				var body map[string]bool
				_ = json.NewDecoder(r.Body).Decode(&body)
				z.zoneAOP = body["enabled"]
				z.patches = append(z.patches, fmt.Sprintf("zone_aop=%v", z.zoneAOP))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]bool{"enabled": z.zoneAOP}})
		case path == "origin_tls_client_auth":
			if z.deny[r.Method+" origin_tls_client_auth"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if r.Method == http.MethodPost {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if !strings.Contains(body["private_key"], "PRIVATE KEY") {
					t.Errorf("upload without a private key")
				}
				z.nextCert++
				status := z.uploadStatus
				if status == "" {
					status = "active"
				}
				cert := cfZoneClientCert{ID: fmt.Sprintf("cert-%d", z.nextCert), Certificate: body["certificate"], Status: status}
				z.clientCerts = append(z.clientCerts, cert)
				z.patches = append(z.patches, "upload="+cert.ID)
				_ = json.NewEncoder(w).Encode(map[string]any{"result": cert})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": z.clientCerts})
		case strings.HasPrefix(path, "origin_tls_client_auth/") && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(path, "origin_tls_client_auth/")
			for i, cert := range z.clientCerts {
				if cert.ID == id {
					z.clientCerts = append(z.clientCerts[:i], z.clientCerts[i+1:]...)
					z.patches = append(z.patches, "delete="+id)
					_ = json.NewEncoder(w).Encode(map[string]any{"result": cert})
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(path, "settings/"):
			name := strings.TrimPrefix(path, "settings/")
			if z.deny[r.Method+" "+name] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			value, ok := z.settings[name]
			if !ok {
				http.Error(w, `{"success":false}`, http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPatch {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				value = body["value"]
				z.settings[name] = value
				z.patches = append(z.patches, name+"="+value)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": name, "value": value}})
		default:
			http.NotFound(w, r)
		}
	})
}

// fakeEdgeProber answers from tables keyed by the probed name.
type fakeEdgeProber struct {
	originInvalid map[string]bool // handshake works, certificate invalid
	originNoTLS   map[string]bool // no TLS at all
	edgeInvalid   map[string]bool
	// clientCAs, when set, answers ClientCAs; nil means no origin asks for a client certificate.
	clientCAs func(name string) (bool, [][]byte, error)
}

func (p fakeEdgeProber) ClientCAs(_ context.Context, _, name string) (bool, [][]byte, error) {
	if p.originNoTLS[name] {
		return false, nil, errors.New("connection refused")
	}
	if p.clientCAs == nil {
		return false, nil, nil
	}
	return p.clientCAs(name)
}

func (p fakeEdgeProber) Origin(_ context.Context, _, name string) (bool, error) {
	if p.originNoTLS[name] {
		return false, errors.New("connection refused")
	}
	if p.originInvalid[name] {
		return true, errors.New("x509: certificate signed by unknown authority")
	}
	return true, nil
}

func (p fakeEdgeProber) Edge(_ context.Context, name string) error {
	if p.edgeInvalid[name] {
		return errors.New("remote error: tls: handshake failure")
	}
	return nil
}

func newZonePolicyFixture(t *testing.T, zone *fakeCloudflareZone, prober edgeProber) (client.Client, *CloudflareZonePolicyReconciler, func() oramav1alpha1.CloudflareZonePolicy) {
	t.Helper()
	server := httptest.NewServer(zone.handler(t))
	t.Cleanup(server.Close)
	s := newLakeScheme(t)
	policy := &oramav1alpha1.CloudflareZonePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "lake", Generation: 1},
		Spec: oramav1alpha1.CloudflareZonePolicySpec{
			ZoneID: "zone-1", APITokenSecretRef: oramav1alpha1.EdgeProviderSecretKeyRef{Name: "cf", Key: "token"},
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "lake"}, Data: map[string][]byte{"token": []byte("test-token")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(policy, secret).WithStatusSubresource(policy).Build()
	r := &CloudflareZonePolicyReconciler{Client: c, Scheme: s, APIBase: server.URL, Prober: prober}
	get := func() oramav1alpha1.CloudflareZonePolicy {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: "edge"}}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		var out oramav1alpha1.CloudflareZonePolicy
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "edge"}, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return c, r, get
}

func zoneRecords() []cfDNSRecord {
	return []cfDNSRecord{
		{Type: "A", Name: "eu.lakelive.example", Content: "11.2.3.7", Proxied: true},
		{Type: "A", Name: "lakelive.example", Content: "11.2.3.7", Proxied: true},
		{Type: "A", Name: "*.eu.lakelive.example", Content: "11.2.3.7", Proxied: true},
	}
}

func conditionStatus(policy oramav1alpha1.CloudflareZonePolicy, kind string) string {
	for _, c := range policy.Status.Conditions {
		if c.Type == kind {
			return string(c.Status)
		}
	}
	return ""
}

// Full (strict) waits for every proxied host's origin certificate; until then the zone goes no
// further than Full, which needs only that every origin speaks TLS. Authenticated origin pulls
// follow Full; Always Use HTTPS waits for every edge certificate (the wildcard's here).
func TestZonePolicyWaitsForOriginCertificatesBeforeStrict(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "flexible", "tls_client_auth": "off", "always_use_https": "off", "ssl_automatic_mode": "auto"},
		records:  zoneRecords(),
	}
	prober := fakeEdgeProber{
		originInvalid: map[string]bool{"orama-edge-probe.eu.lakelive.example": true},
		edgeInvalid:   map[string]bool{"orama-edge-probe.eu.lakelive.example": true},
	}
	_, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()

	if zone.settings["ssl"] != "full" || zone.settings["ssl_automatic_mode"] != "custom" || zone.settings["tls_client_auth"] != "on" {
		t.Errorf("settings = %v", zone.settings)
	}
	if zone.settings["always_use_https"] != "off" {
		t.Error("Always Use HTTPS turned on while a host has no edge certificate")
	}
	st := policy.Status
	if st.OriginEncryption != "full" || !st.AuthenticatedOriginPulls || st.AlwaysUseHTTPS || st.Phase != edgePhaseProgressing {
		t.Errorf("status = %+v", st)
	}
	if conditionStatus(policy, edgeConditionOriginTLSStrict) != "False" || conditionStatus(policy, edgeConditionOriginCertificates) != "False" {
		t.Errorf("conditions = %+v", st.Conditions)
	}
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "*.eu.lakelive.example" || st.Hosts[0].Origin == "" || st.Hosts[0].Edge == "" {
		t.Errorf("hosts = %+v", st.Hosts)
	}
	// No origin asks for a client certificate yet, so the zone already presents its own, and the
	// origins verify against the operator's CA alone.
	if !st.ZoneOriginPulls || st.OriginClientCAPEM == "" || strings.Contains(st.OriginClientCAPEM, cloudflareOriginPullCA) {
		t.Error("status does not carry the operator's client CA alone")
	}
}

// Once every certificate is valid the zone reaches Full (strict) and Always Use HTTPS, and a
// steady zone writes nothing more.
func TestZonePolicyReachesStrictAndIsSteady(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "full", "tls_client_auth": "off", "always_use_https": "off"},
		records:  zoneRecords(),
	}
	_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	first := reconcile()
	if zone.settings["ssl"] != "strict" || zone.settings["tls_client_auth"] != "on" || zone.settings["always_use_https"] != "on" {
		t.Errorf("settings = %v", zone.settings)
	}
	if first.Status.Phase != edgePhaseReady || first.Status.OriginEncryption != "strict" || len(first.Status.Hosts) != 0 {
		t.Errorf("status = %+v", first.Status)
	}
	patches := len(zone.patches)
	second := reconcile()
	if len(zone.patches) != patches {
		t.Errorf("a steady zone was changed again: %v", zone.patches[patches:])
	}
	if second.ResourceVersion != first.ResourceVersion {
		t.Error("a steady zone rewrote its status")
	}
}

// The operator never weakens a setting: a strict zone whose host loses its certificate is
// reported, not downgraded.
func TestZonePolicyNeverDowngrades(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "strict", "tls_client_auth": "on", "always_use_https": "on"},
		records:  zoneRecords(),
	}
	prober := fakeEdgeProber{originNoTLS: map[string]bool{"eu.lakelive.example": true}}
	_, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	if len(zone.patches) != 0 {
		t.Errorf("settings changed: %v", zone.patches)
	}
	if policy.Status.OriginEncryption != "strict" || conditionStatus(policy, edgeConditionOriginCertificates) != "False" {
		t.Errorf("status = %+v", policy.Status)
	}
}

// A token without the permission is reported by name, and the last observed settings are kept so
// the Lakes keep their redirect and client-certificate check.
func TestZonePolicyNamesTheMissingPermission(t *testing.T) {
	for _, tc := range []struct {
		name, deny, want string
	}{
		{"edit", "PATCH ssl", cfPermissionSettingsEdit},
		{"read", "GET ssl", cfPermissionSettingsRead},
		{"dns", "GET dns_records", cfPermissionDNSRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone := &fakeCloudflareZone{
				settings: map[string]string{"ssl": "full", "tls_client_auth": "on", "always_use_https": "off"},
				records:  zoneRecords(),
				deny:     map[string]bool{tc.deny: true},
			}
			_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
			policy := reconcile()
			if policy.Status.Phase != edgePhaseFailed || !strings.Contains(policy.Status.Message, tc.want) {
				t.Errorf("status = %s %q, want the permission %q named", policy.Status.Phase, policy.Status.Message, tc.want)
			}
			if conditionStatus(policy, edgeConditionCredentials) != "False" {
				t.Errorf("conditions = %+v", policy.Status.Conditions)
			}
		})
	}
	// Kept observations: a zone read once, then refused.
	zone := &fakeCloudflareZone{settings: map[string]string{"ssl": "strict", "tls_client_auth": "on", "always_use_https": "on"}, records: zoneRecords()}
	_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	if policy := reconcile(); policy.Status.Phase != edgePhaseReady {
		t.Fatalf("status = %+v", policy.Status)
	}
	zone.mu.Lock()
	zone.deny = map[string]bool{"GET ssl": true}
	zone.mu.Unlock()
	policy := reconcile()
	if policy.Status.Phase != edgePhaseFailed || policy.Status.OriginEncryption != "strict" || !policy.Status.AuthenticatedOriginPulls {
		t.Errorf("a refused read forgot the zone: %+v", policy.Status)
	}
}

// The abstract kind resolves to the concrete one through its provider and copies its status.
func TestEdgeZonePolicyResolvesToCloudflare(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	provider := &oramav1alpha1.EdgeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "lake"},
		Spec: oramav1alpha1.EdgeProviderSpec{Type: "cloudflare", Cloudflare: &oramav1alpha1.CloudflareEdgeProviderSpec{
			APITokenSecretRef: oramav1alpha1.EdgeProviderSecretKeyRef{Name: "cf", Key: "token"},
		}},
	}
	policy := &oramav1alpha1.EdgeZonePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "lake", UID: "policy-uid"},
		Spec:       oramav1alpha1.EdgeZonePolicySpec{ProviderRef: corev1.LocalObjectReference{Name: "edge"}, ZoneID: "zone-1"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(provider, policy).
		WithStatusSubresource(policy, &oramav1alpha1.CloudflareZonePolicy{}).Build()
	r := &EdgeZonePolicyReconciler{Client: c, Scheme: s}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "lake", Name: "edge"}}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var concrete oramav1alpha1.CloudflareZonePolicy
	if err := c.Get(ctx, request.NamespacedName, &concrete); err != nil {
		t.Fatalf("concrete policy: %v", err)
	}
	if concrete.Spec.ZoneID != "zone-1" || concrete.Spec.APITokenSecretRef.Name != "cf" || len(concrete.OwnerReferences) != 1 {
		t.Errorf("concrete = %+v", concrete)
	}
	concrete.Status = oramav1alpha1.EdgeZonePolicyStatus{Phase: edgePhaseReady, OriginEncryption: "strict", AuthenticatedOriginPulls: true}
	if err := c.Status().Update(ctx, &concrete); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, policy); err != nil {
		t.Fatal(err)
	}
	if policy.Status.Phase != edgePhaseReady || policy.Status.OriginEncryption != "strict" || !policy.Status.AuthenticatedOriginPulls {
		t.Errorf("abstract status = %+v", policy.Status)
	}
}

func TestEdgeProbeNames(t *testing.T) {
	for in, want := range map[string]string{"eu.lakelive.example": "eu.lakelive.example", "*.eu.lakelive.example": "orama-edge-probe.eu.lakelive.example"} {
		if got := edgeProbeName(in); got != want {
			t.Errorf("edgeProbeName(%q) = %q, want %q", in, got, want)
		}
	}
	if fmt.Sprint(edgeOriginEncrypted("flexible"), edgeOriginEncrypted("full"), edgeOriginEncrypted("strict"), edgeOriginEncrypted("")) != "false true true false" {
		t.Error("only full and strict reach the origin over TLS")
	}
}

// The zone's name is reported for the Lakes' per-host choice of ACME solver (operator#632). A
// token that may not read it is named, the zone's settings are still applied, and a name read
// before is kept, so the Lakes never fall back from what they had.
func TestZonePolicyReportsTheZoneName(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "strict", "tls_client_auth": "on", "always_use_https": "on"},
		records:  zoneRecords(),
		name:     "Lake.Live",
	}
	_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{})
	policy := reconcile()
	if policy.Status.Zone != "lakelive.example" || conditionStatus(policy, edgeConditionZoneName) != "True" || policy.Status.Phase != edgePhaseReady {
		t.Fatalf("status = %+v", policy.Status)
	}

	zone.mu.Lock()
	zone.deny = map[string]bool{"GET zone": true}
	zone.mu.Unlock()
	policy = reconcile()
	if policy.Status.Zone != "lakelive.example" {
		t.Errorf("a refused read forgot the zone's name: %+v", policy.Status)
	}
	if policy.Status.Phase != edgePhaseReady {
		t.Errorf("a refused name read changed the phase: %s %q", policy.Status.Phase, policy.Status.Message)
	}
	for _, c := range policy.Status.Conditions {
		if c.Type == edgeConditionZoneName && (c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, cfPermissionZoneRead)) {
			t.Errorf("ZoneName condition = %+v, want False naming %q", c, cfPermissionZoneRead)
		}
	}

	// Never read: no name, and the settings are still applied.
	fresh := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "full", "tls_client_auth": "off", "always_use_https": "off"},
		records:  zoneRecords(),
		deny:     map[string]bool{"GET zone": true},
	}
	_, _, reconcileFresh := newZonePolicyFixture(t, fresh, fakeEdgeProber{})
	policy = reconcileFresh()
	if policy.Status.Zone != "" || fresh.settings["ssl"] != "strict" || conditionStatus(policy, edgeConditionZoneName) != "False" {
		t.Errorf("status = %+v settings = %v", policy.Status, fresh.settings)
	}
}

// lake#798: an apps wildcard two levels below the apex (*.eu.lakelive.example) has no Universal SSL
// certificate, so the policy orders an Advanced Certificate covering it, once: a pending pack is
// not ordered again, a host one level below the apex is never ordered, and a zone whose hosts all
// have edge certificates makes no certificate calls.
func TestZonePolicyOrdersEdgeCertificatesForDeepHosts(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "full", "tls_client_auth": "on", "always_use_https": "off"},
		records: append(zoneRecords(),
			cfDNSRecord{Type: "A", Name: "us.lakelive.example", Content: "20.0.0.1", Proxied: true},
			cfDNSRecord{Type: "A", Name: "app.us.lakelive.example", Content: "20.0.0.1", Proxied: true}),
		name:  "lakelive.example",
		packs: []cfCertificatePack{{ID: "universal", Type: "universal", Hosts: []string{"lakelive.example", "*.lakelive.example"}, Status: "active"}},
	}
	prober := fakeEdgeProber{edgeInvalid: map[string]bool{
		"orama-edge-probe.eu.lakelive.example": true, "app.us.lakelive.example": true, "us.lakelive.example": true,
	}}
	_, r, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	want := "[[lakelive.example *.eu.lakelive.example eu.lakelive.example] [lakelive.example app.us.lakelive.example]]"
	if got := fmt.Sprint(zone.orders); got != want {
		t.Fatalf("orders = %s, want %s", got, want)
	}
	if conditionStatus(policy, edgeConditionEdgeCertificateOrders) != "False" || zone.settings["always_use_https"] != "off" {
		t.Errorf("status = %+v, settings = %v", policy.Status, zone.settings)
	}
	if !strings.Contains(policy.Status.Message, "edge certificate ordered for *.eu.lakelive.example, app.us.lakelive.example") {
		t.Errorf("message = %q", policy.Status.Message)
	}

	// The packs are pending: nothing is ordered again.
	policy = reconcile()
	if len(zone.orders) != 2 || !strings.Contains(policy.Status.Message, "is ordered and not yet serving") {
		t.Errorf("orders = %v, message = %q", zone.orders, policy.Status.Message)
	}

	// A pack that failed validation no longer counts: the host is ordered again.
	zone.mu.Lock()
	zone.packs[1].Status = "validation_timed_out"
	zone.mu.Unlock()
	reconcile()
	if len(zone.orders) != 3 || fmt.Sprint(zone.orders[2]) != "[lakelive.example *.eu.lakelive.example eu.lakelive.example]" {
		t.Errorf("orders = %v", zone.orders)
	}

	// Once the edge serves every host, Always Use HTTPS follows and no certificate API is called.
	zone.mu.Lock()
	zone.deny = map[string]bool{"GET certificate_packs": true, "POST order": true}
	zone.mu.Unlock()
	r.Prober = fakeEdgeProber{}
	policy = reconcile()
	if zone.settings["always_use_https"] != "on" || conditionStatus(policy, edgeConditionEdgeCertificateOrders) != "True" {
		t.Errorf("status = %+v, settings = %v", policy.Status, zone.settings)
	}
}

// A Lake's app hosts have their own proxied records under its apps wildcard, and "*.x" sorts before
// them. The wildcard's pack, ordered first in the pass, covers them, so one reconcile places exactly
// one order instead of one per app host.
func TestZonePolicyWildcardOrderCoversItsAppHosts(t *testing.T) {
	zone := &fakeCloudflareZone{
		settings: map[string]string{"ssl": "strict", "tls_client_auth": "on", "always_use_https": "off"},
		records: append(zoneRecords(),
			cfDNSRecord{Type: "A", Name: "alpha.eu.lakelive.example", Content: "11.2.3.7", Proxied: true},
			cfDNSRecord{Type: "A", Name: "beta.eu.lakelive.example", Content: "11.2.3.7", Proxied: true},
			cfDNSRecord{Type: "A", Name: "gamma.eu.lakelive.example", Content: "11.2.3.7", Proxied: true}),
		name:  "lakelive.example",
		packs: []cfCertificatePack{{ID: "universal", Type: "universal", Hosts: []string{"lakelive.example", "*.lakelive.example"}, Status: "active"}},
	}
	prober := fakeEdgeProber{edgeInvalid: map[string]bool{
		"orama-edge-probe.eu.lakelive.example": true, "alpha.eu.lakelive.example": true, "beta.eu.lakelive.example": true, "gamma.eu.lakelive.example": true,
	}}
	_, _, reconcile := newZonePolicyFixture(t, zone, prober)
	policy := reconcile()
	if got, want := fmt.Sprint(zone.orders), "[[lakelive.example *.eu.lakelive.example eu.lakelive.example]]"; got != want {
		t.Fatalf("orders = %s, want %s", got, want)
	}
	if !strings.Contains(policy.Status.Message, "edge certificate ordered for *.eu.lakelive.example;") {
		t.Errorf("message = %q", policy.Status.Message)
	}
	// The next pass finds the pending pack covering every host and orders nothing.
	reconcile()
	if len(zone.orders) != 1 {
		t.Errorf("orders = %v", zone.orders)
	}
}

// A token that may not order certificates, or a zone without Advanced Certificate Manager, is
// reported on the condition; the zone's other settings still advance.
func TestZonePolicyReportsARefusedCertificateOrder(t *testing.T) {
	for _, tc := range []struct{ deny, want string }{
		{"GET certificate_packs", cfPermissionCertificatesEdit},
		{"POST order", cfPermissionCertificatesEdit},
		{"POST order subscription", "Advanced Certificate Manager"},
	} {
		t.Run(tc.deny, func(t *testing.T) {
			zone := &fakeCloudflareZone{
				settings: map[string]string{"ssl": "flexible", "tls_client_auth": "off", "always_use_https": "off"},
				records:  zoneRecords(),
				name:     "lakelive.example",
				deny:     map[string]bool{tc.deny: true},
			}
			_, _, reconcile := newZonePolicyFixture(t, zone, fakeEdgeProber{edgeInvalid: map[string]bool{"orama-edge-probe.eu.lakelive.example": true}})
			policy := reconcile()
			if conditionStatus(policy, edgeConditionEdgeCertificateOrders) != "False" || !strings.Contains(policy.Status.Message, tc.want) {
				t.Errorf("message = %q, want %q named", policy.Status.Message, tc.want)
			}
			if policy.Status.Phase != edgePhaseProgressing || zone.settings["ssl"] != "strict" || zone.settings["tls_client_auth"] != "on" {
				t.Errorf("status = %s, settings = %v", policy.Status.Phase, zone.settings)
			}
		})
	}
}

func TestEdgeCertificateHelpers(t *testing.T) {
	for name, want := range map[string]int{"lakelive.example": 0, "eu.lakelive.example": 1, "*.lakelive.example": 1, "*.eu.lakelive.example": 2, "a.b.lakelive.example": 2, "example.com": -1, "notlakelive.example": -1} {
		if got := edgeHostDepth(name, "lakelive.example"); got != want {
			t.Errorf("edgeHostDepth(%q) = %d, want %d", name, got, want)
		}
	}
	for _, tc := range []struct {
		pack, name string
		want       bool
	}{
		{"*.eu.lakelive.example", "*.eu.lakelive.example", true},
		{"*.eu.lakelive.example", "app.eu.lakelive.example", true},
		{"*.lakelive.example", "*.eu.lakelive.example", false},
		{"*.lakelive.example", "app.eu.lakelive.example", false},
		{"eu.lakelive.example", "*.eu.lakelive.example", false},
	} {
		if got := edgeCertificateCovers(tc.pack, tc.name); got != tc.want {
			t.Errorf("edgeCertificateCovers(%q, %q) = %v", tc.pack, tc.name, got)
		}
	}
	for status, want := range map[string]bool{"active": true, "pending_validation": true, "initializing": true, "deleted": false, "expired": false, "validation_timed_out": false, "issuance_timed_out": false} {
		if got := edgeCertificatePackLive(status); got != want {
			t.Errorf("edgeCertificatePackLive(%q) = %v", status, got)
		}
	}
}
