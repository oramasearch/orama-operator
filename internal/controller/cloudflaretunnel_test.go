// SPDX-License-Identifier: MIT

package controller

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeCloudflare records every call and keeps the objects a customer's account would hold.
type fakeCloudflare struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string]map[string]any
	tunnels  map[string]bool
	records  map[string]bool
	apps     map[string]bool
	tokenHdr []string
	// The zone's records by id: name and comment. A test seeds one to stand for a record the
	// tunnel did not create (operator#585).
	zone map[string]map[string]any
}

func newFakeCloudflare() *fakeCloudflare {
	return &fakeCloudflare{bodies: map[string]map[string]any{}, tunnels: map[string]bool{}, records: map[string]bool{}, apps: map[string]bool{}, zone: map[string]map[string]any{}}
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.tokenHdr = append(f.tokenHdr, r.Header.Get("Authorization"))
	var body map[string]any
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &body)
		f.bodies[key] = body
	}
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}
	const account = "/client/v4/accounts/acct"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == account+"/cfd_tunnel":
		reply([]any{})
	case r.Method == http.MethodPost && r.URL.Path == account+"/cfd_tunnel":
		f.tunnels["cf-tunnel"] = true
		reply(map[string]any{"id": "cf-tunnel", "name": body["name"]})
	case r.Method == http.MethodGet && r.URL.Path == account+"/cfd_tunnel/cf-tunnel/token":
		reply("connector-token")
	case r.Method == http.MethodPut && r.URL.Path == account+"/cfd_tunnel/cf-tunnel/configurations":
		reply(map[string]any{})
	case r.Method == http.MethodGet && r.URL.Path == "/client/v4/zones/zone/dns_records":
		found := []any{}
		for id, record := range f.zone {
			if name := r.URL.Query().Get("name"); name == "" || record["name"] == name {
				found = append(found, map[string]any{"id": id, "name": record["name"], "content": record["content"], "comment": record["comment"]})
			}
		}
		reply(found)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/client/v4/zones/zone/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/client/v4/zones/zone/dns_records/")
		record, ok := f.zone[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}
		reply(map[string]any{"id": id, "name": record["name"], "content": record["content"], "comment": record["comment"]})
	case r.Method == http.MethodPost && r.URL.Path == "/client/v4/zones/zone/dns_records":
		f.records["dns-record"] = true
		f.zone["dns-record"] = map[string]any{"name": body["name"], "content": body["content"], "comment": body["comment"]}
		reply(map[string]any{"id": "dns-record", "name": body["name"], "content": body["content"]})
	case r.Method == http.MethodGet && r.URL.Path == account+"/access/apps":
		reply([]any{})
	case r.Method == http.MethodPost && r.URL.Path == account+"/access/apps":
		f.apps["access-app"] = true
		reply(map[string]any{"id": "access-app", "domain": body["domain"]})
	case r.Method == http.MethodDelete && r.URL.Path == account+"/access/apps/access-app":
		delete(f.apps, "access-app")
		reply(map[string]any{"id": "access-app"})
	case r.Method == http.MethodDelete && r.URL.Path == "/client/v4/zones/zone/dns_records/dns-record":
		delete(f.records, "dns-record")
		delete(f.zone, "dns-record")
		reply(map[string]any{"id": "dns-record"})
	case r.Method == http.MethodDelete && r.URL.Path == account+"/cfd_tunnel/cf-tunnel/connections":
		reply(map[string]any{})
	case r.Method == http.MethodDelete && r.URL.Path == account+"/cfd_tunnel/cf-tunnel":
		delete(f.tunnels, "cf-tunnel")
		reply(map[string]any{"id": "cf-tunnel"})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false}`))
	}
}

func TestCloudflareTunnelUsesOnlyTheCustomersAccountAndAdmitsOnlyThePerson(t *testing.T) {
	api := newFakeCloudflare()
	server := httptest.NewServer(api)
	defer server.Close()
	provider := &oramav1alpha1.TunnelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: defaultTunnelProviderName, Namespace: testTunnelNS},
		Spec: oramav1alpha1.TunnelProviderSpec{
			Type: oramav1alpha1.TunnelProviderCloudflare, CustomerID: testServiceCustomer,
			Cloudflare: &oramav1alpha1.CloudflareTunnelProviderSpec{
				AccountID: "acct", ZoneID: "zone", Domain: "dev.customer.example",
				APITokenSecretRef: oramav1alpha1.TunnelProviderSecretKeyRef{Name: "cloudflare", Key: "token"},
			},
		},
	}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare", Namespace: testTunnelNS},
		Data: map[string][]byte{"token": []byte("customer-token")}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/24"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.224.0.4"}}}}
	f := newTunnelFixture(t, provider, token, node)
	cloudflareR := &CloudflareTunnelReconciler{Client: f.kube, Scheme: f.scheme, APIBase: server.URL + "/client/v4", Now: f.tunnelR.Now}
	providerR := &TunnelProviderReconciler{Client: f.kube, Scheme: f.scheme}
	f.store.grants = []LakeTunnelGrant{f.grant()}
	run := func() {
		t.Helper()
		for i := 0; i < 4; i++ {
			f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
			if _, err := providerR.Reconcile(f.ctx, f.request(defaultTunnelProviderName)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tunnelR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID))); err != nil {
				t.Fatal(err)
			}
			if _, err := cloudflareR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID))); err != nil {
				t.Fatal(err)
			}
			if _, err := f.connR.Reconcile(f.ctx, f.request(tunnelConnectionName(testTunnelGrantID))); err != nil {
				t.Fatal(err)
			}
		}
	}
	run()
	var tunnel oramav1alpha1.Tunnel
	f.get(client.ObjectKey{Namespace: testTunnelNS, Name: lakeTunnelName(testTunnelGrantID)}, &tunnel)
	if tunnel.Spec.ProviderRef != defaultTunnelProviderName || tunnel.Spec.Subject != "dev@example.com" {
		t.Fatalf("Tunnel = %+v", tunnel.Spec)
	}
	var zetta oramav1alpha1.ZettaTunnelList
	if err := f.kube.List(f.ctx, &zetta); err != nil || len(zetta.Items) != 0 {
		t.Fatalf("a Cloudflare grant made %d ZettaTunnels (%v)", len(zetta.Items), err)
	}
	for _, header := range api.tokenHdr {
		if header != "Bearer customer-token" {
			t.Fatalf("a Cloudflare call used %q, not the customer's token", header)
		}
	}
	hostname := "tunnel-" + testTunnelGrantID + ".dev.customer.example"
	app := api.bodies["POST /client/v4/accounts/acct/access/apps"]
	policies, _ := json.Marshal(app["policies"])
	if app["domain"] != hostname || app["session_duration"] != "120m" ||
		string(policies) != `[{"decision":"allow","include":[{"email":{"email":"dev@example.com"}}],"name":"grant `+testTunnelGrantID+`"}]` {
		t.Fatalf("Access application = %v", app)
	}
	config, _ := json.Marshal(api.bodies["PUT /client/v4/accounts/acct/cfd_tunnel/cf-tunnel/configurations"])
	if !strings.Contains(string(config), `"service":"tcp://db-rw.workspace.svc.cluster.local:5432"`) ||
		!strings.Contains(string(config), `"service":"http_status:404"`) {
		t.Fatalf("tunnel configuration = %s", config)
	}
	if record := api.bodies["POST /client/v4/zones/zone/dns_records"]; record["content"] != "cf-tunnel.cfargotunnel.com" || record["proxied"] != true {
		t.Fatalf("DNS record = %v", record)
	}
	var connector appsv1.Deployment
	if !f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &connector) {
		t.Fatal("no cloudflared connector")
	}
	if connector.Spec.Template.Spec.Containers[0].Image != DefaultCloudflaredImage ||
		connector.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name != cloudflareTunnelTokenSecret(testTunnelGrantID) {
		t.Fatalf("connector = %+v", connector.Spec.Template.Spec.Containers[0])
	}
	egress, ok := f.policy(cloudflareTunnelEgressAllowance(testTunnelGrantID))
	if !ok {
		t.Fatal("the connector has no egress to Cloudflare")
	}
	for _, peer := range egress.Spec.Egress[0].To {
		if peer.IPBlock == nil || (peer.IPBlock.CIDR == "0.0.0.0/0" && !strings.Contains(strings.Join(peer.IPBlock.Except, ","), "10.0.0.0/8")) {
			t.Fatalf("connector egress reaches private addresses: %+v", peer)
		}
	}
	f.markAvailable(testTunnelNS, zettaTunnelEndpointName(testTunnelGrantID))
	run()
	ready := f.store.last(t)
	if ready.Phase != oramav1alpha1.TunnelPhaseReady || ready.Endpoint != hostname || ready.RelayCAPEM != "" {
		t.Fatalf("ready status = %+v", ready)
	}

	// Expiry removes the connector and then every object in the customer's account.
	f.setNow(f.now.Add(3 * time.Hour))
	cloudflareR.Now = f.tunnelR.Now
	run()
	if len(api.tunnels)+len(api.records)+len(api.apps) != 0 {
		t.Fatalf("left in the customer's account: tunnels %v records %v apps %v", api.tunnels, api.records, api.apps)
	}
	if f.get(client.ObjectKey{Namespace: testTunnelNS, Name: zettaTunnelEndpointName(testTunnelGrantID)}, &connector) {
		t.Fatal("the connector survived expiry")
	}
	if last := f.store.last(t); last.Phase != oramav1alpha1.TunnelPhaseExpired {
		t.Fatalf("expired status = %+v", last)
	}
	var condition metav1.Condition
	var current oramav1alpha1.TunnelProvider
	f.get(client.ObjectKeyFromObject(provider), &current)
	for _, c := range current.Status.Conditions {
		condition = c
	}
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("provider condition = %+v", condition)
	}
}

func TestCloudflareSessionNeverOutlivesTheGrant(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		remaining time.Duration
		want      string
	}{{30 * time.Hour, "720m"}, {90*time.Minute + 30*time.Second, "90m"}, {10 * time.Second, "1m"}} {
		if got := cloudflareSessionDuration(now.Add(c.remaining), now); got != c.want {
			t.Fatalf("remaining %v: %s, want %s", c.remaining, got, c.want)
		}
	}
}

func TestTunnelLeafIsReissuedWhenItsNamesOrIssuerChange(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ca, err := newTunnelCA("test CA", now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newTunnelCA("other CA", now)
	if err != nil {
		t.Fatal(err)
	}
	request := tunnelLeafRequest{CommonName: "relay", DNSNames: []string{"relay.svc"}, IPs: []net.IP{net.ParseIP("20.1.2.3")}, Server: true}
	cert, key, err := issueTunnelLeaf(ca, request, now)
	if err != nil {
		t.Fatal(err)
	}
	if !tunnelLeafCurrent(cert, key, ca.CertPEM, request, now) {
		t.Fatal("a fresh leaf is not current")
	}
	moved := request
	moved.IPs = []net.IP{net.ParseIP("20.1.2.4")}
	if tunnelLeafCurrent(cert, key, ca.CertPEM, moved, now) {
		t.Fatal("a leaf for the old address is still current")
	}
	if tunnelLeafCurrent(cert, key, other.CertPEM, request, now) {
		t.Fatal("a leaf from another CA is current")
	}
	clientOnly := request
	clientOnly.Server, clientOnly.Client = false, true
	if tunnelLeafCurrent(cert, key, ca.CertPEM, clientOnly, now) {
		t.Fatal("a server leaf is current for a client request")
	}
	if tunnelLeafCurrent(cert, key, ca.CertPEM, request, now.Add(tunnelLeafValidity-tunnelLeafRenewBefore+time.Hour)) {
		t.Fatal("a leaf near expiry is current")
	}
	_, otherKey, _ := issueTunnelLeaf(ca, request, now)
	if tunnelLeafCurrent(cert, otherKey, ca.CertPEM, request, now) {
		t.Fatal("a certificate with another key is current")
	}
	bundle := pemBundle(ca.CertPEM, other.CertPEM, ca.CertPEM)
	certs, err := pemCertificates(bundle)
	if err != nil || len(certs) != 2 {
		t.Fatalf("bundle has %d certificates (%v), want the two distinct CAs", len(certs), err)
	}
	if string(pemBundle(other.CertPEM, ca.CertPEM)) != string(bundle) {
		t.Fatal("the bundle depends on input order")
	}
}

// cloudflareTunnelRun sets up one Cloudflare grant against api and returns the fixture, a function
// running every reconciler four times (errors from the Cloudflare reconciler collected, not fatal),
// and the grant's hostname.
func cloudflareTunnelRun(t *testing.T, api *fakeCloudflare, apiURL string) (*tunnelFixture, func() []error, string) {
	t.Helper()
	provider := &oramav1alpha1.TunnelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: defaultTunnelProviderName, Namespace: testTunnelNS},
		Spec: oramav1alpha1.TunnelProviderSpec{
			Type: oramav1alpha1.TunnelProviderCloudflare, CustomerID: testServiceCustomer,
			Cloudflare: &oramav1alpha1.CloudflareTunnelProviderSpec{
				AccountID: "acct", ZoneID: "zone", Domain: "dev.customer.example",
				APITokenSecretRef: oramav1alpha1.TunnelProviderSecretKeyRef{Name: "cloudflare", Key: "token"},
			},
		},
	}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare", Namespace: testTunnelNS},
		Data: map[string][]byte{"token": []byte("customer-token")}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/24"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.224.0.4"}}}}
	f := newTunnelFixture(t, provider, token, node)
	cloudflareR := &CloudflareTunnelReconciler{Client: f.kube, Scheme: f.scheme, APIBase: apiURL + "/client/v4", Now: f.tunnelR.Now}
	providerR := &TunnelProviderReconciler{Client: f.kube, Scheme: f.scheme}
	f.store.grants = []LakeTunnelGrant{f.grant()}
	run := func() []error {
		t.Helper()
		var errs []error
		cloudflareR.Now = f.tunnelR.Now
		for i := 0; i < 4; i++ {
			f.lakeR.reconcileLakeTunnels(f.ctx, f.lake, f.store)
			if _, err := providerR.Reconcile(f.ctx, f.request(defaultTunnelProviderName)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tunnelR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID))); err != nil {
				t.Fatal(err)
			}
			if _, err := cloudflareR.Reconcile(f.ctx, f.request(lakeTunnelName(testTunnelGrantID))); err != nil {
				errs = append(errs, err)
			}
			if _, err := f.connR.Reconcile(f.ctx, f.request(tunnelConnectionName(testTunnelGrantID))); err != nil {
				t.Fatal(err)
			}
		}
		return errs
	}
	return f, run, "tunnel-" + testTunnelGrantID + ".dev.customer.example"
}

// operator#585: a record already at the tunnel's hostname that does not carry the tunnel's marker
// is never repointed or adopted, and the tunnel creates nothing there.
func TestCloudflareTunnelNeverAdoptsARecordItDidNotCreate(t *testing.T) {
	api := newFakeCloudflare()
	server := httptest.NewServer(api)
	defer server.Close()
	_, run, hostname := cloudflareTunnelRun(t, api, server.URL)
	api.zone["prod-record"] = map[string]any{"name": hostname, "content": "app.company.example", "comment": "Prod 1"}
	errs := run()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "operator#585") {
		t.Fatalf("errors = %v, want a refusal naming operator#585", errs)
	}
	for _, call := range api.calls {
		if call == "POST /client/v4/zones/zone/dns_records" || strings.HasPrefix(call, "PUT /client/v4/zones/zone/dns_records") ||
			strings.HasPrefix(call, "DELETE /client/v4/zones/zone/dns_records") {
			t.Fatalf("the tunnel changed the zone: %s", call)
		}
	}
	if record := api.zone["prod-record"]; record["content"] != "app.company.example" || record["comment"] != "Prod 1" {
		t.Fatalf("the existing record changed: %v", record)
	}
}

// operator#585: the tunnel's own record carries its marker, and teardown deletes it only while the
// marker is still there; a record someone else has since taken over is left in place.
func TestCloudflareTunnelDeletesOnlyARecordStillCarryingItsMarker(t *testing.T) {
	api := newFakeCloudflare()
	server := httptest.NewServer(api)
	defer server.Close()
	f, run, _ := cloudflareTunnelRun(t, api, server.URL)
	if errs := run(); len(errs) != 0 {
		t.Fatal(errs)
	}
	record, ok := api.zone["dns-record"]
	if !ok || record["comment"] != "orama-operator:"+testTunnelNS+"/"+lakeTunnelName(testTunnelGrantID) {
		t.Fatalf("created record = %v, want the tunnel's ownership marker", record)
	}
	record["comment"] = "taken over by hand"
	f.setNow(f.now.Add(3 * time.Hour))
	if errs := run(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := api.zone["dns-record"]; !ok {
		t.Fatal("teardown deleted a record that no longer carries the tunnel's marker")
	}
}
