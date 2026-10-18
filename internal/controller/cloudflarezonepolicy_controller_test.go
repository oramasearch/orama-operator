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
	// deny maps "METHOD setting" (or "GET dns_records") to a 403.
	deny    map[string]bool
	patches []string
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
		case path == "dns_records" && r.Method == http.MethodGet:
			if z.deny["GET dns_records"] {
				http.Error(w, `{"success":false}`, http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("proxied") != "true" {
				t.Errorf("records listed without proxied=true")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": z.records, "result_info": map[string]int{"page": 1, "total_pages": 1}})
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
		{Type: "A", Name: "eu.lake.live", Content: "74.158.44.177", Proxied: true},
		{Type: "A", Name: "lake.live", Content: "74.158.44.177", Proxied: true},
		{Type: "A", Name: "*.eu.lake.live", Content: "74.158.44.177", Proxied: true},
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
		originInvalid: map[string]bool{"orama-edge-probe.eu.lake.live": true},
		edgeInvalid:   map[string]bool{"orama-edge-probe.eu.lake.live": true},
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
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "*.eu.lake.live" || st.Hosts[0].Origin == "" || st.Hosts[0].Edge == "" {
		t.Errorf("hosts = %+v", st.Hosts)
	}
	if st.OriginClientCAPEM != cloudflareOriginPullCA {
		t.Error("status does not carry the edge's client CA")
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
	prober := fakeEdgeProber{originNoTLS: map[string]bool{"eu.lake.live": true}}
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
	for in, want := range map[string]string{"eu.lake.live": "eu.lake.live", "*.eu.lake.live": "orama-edge-probe.eu.lake.live"} {
		if got := edgeProbeName(in); got != want {
			t.Errorf("edgeProbeName(%q) = %q, want %q", in, got, want)
		}
	}
	if fmt.Sprint(edgeOriginEncrypted("flexible"), edgeOriginEncrypted("full"), edgeOriginEncrypted("strict"), edgeOriginEncrypted("")) != "false true true false" {
		t.Error("only full and strict reach the origin over TLS")
	}
}
