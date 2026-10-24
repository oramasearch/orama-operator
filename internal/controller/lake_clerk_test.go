/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeClerk is one Clerk instance's Backend API (the paths of spec 2026-08-20) and the Svix
// application its webhooks go through. It records every request.
type fakeClerk struct {
	mu        sync.Mutex
	t         *testing.T
	secretKey string
	envType   string
	frontend  string
	targets   []map[string]any
	origins   []string
	templates []map[string]any
	svixApp   bool
	endpoints []map[string]any
	requests  []string
	svixBase  string // set once the server runs
}

func (f *fakeClerk) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

func (f *fakeClerk) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// writes are the requests that change something in Clerk or Svix. POST svix_url only opens a
// portal session, and exchanging its one-time token only signs in to it.
func writes(requests []string) []string {
	var out []string
	for _, request := range requests {
		if strings.HasPrefix(request, "GET ") || request == "POST /v1/webhooks/svix_url" ||
			request == "POST /svix/api/v1/auth/one-time-token/" {
			continue
		}
		out = append(out, request)
	}
	return out
}

func (f *fakeClerk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	if r.Method == http.MethodPost && r.URL.Path == "/svix/api/v1/auth/one-time-token/" {
		if body["oneTimeToken"] != "ott_svix" {
			http.Error(w, "bad one-time token", http.StatusUnauthorized)
			return
		}
		reply(map[string]any{"token": "appsk_portal.us"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/svix/") {
		if r.Header.Get("Authorization") != "Bearer appsk_portal.us" {
			http.Error(w, "bad portal token", http.StatusUnauthorized)
			return
		}
		const endpoints = "/svix/api/v1/app/app_svix/endpoint"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == endpoints:
			reply(map[string]any{"data": f.endpoints, "done": true, "iterator": nil})
		case r.Method == http.MethodPost && r.URL.Path == endpoints:
			body["id"] = fmt.Sprintf("ep_%d", len(f.endpoints)+1)
			body["disabled"] = false
			f.endpoints = append(f.endpoints, body)
			reply(body)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, endpoints+"/"):
			id := strings.TrimPrefix(r.URL.Path, endpoints+"/")
			for _, ep := range f.endpoints {
				if ep["id"] == id {
					for k, v := range body {
						ep[k] = v
					}
				}
			}
			reply(map[string]any{})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secret"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, endpoints+"/"), "/secret")
			reply(map[string]any{"key": "whsec_" + id})
		default:
			http.NotFound(w, r)
		}
		return
	}

	if r.Header.Get("Authorization") != "Bearer "+f.secretKey {
		http.Error(w, `{"errors":[{"code":"authentication_invalid"}]}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/instance":
		reply(map[string]any{"object": "instance", "id": "ins_1", "environment_type": f.envType, "allowed_origins": f.origins})
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/instance":
		f.origins = nil
		for _, o := range body["allowed_origins"].([]any) {
			f.origins = append(f.origins, o.(string))
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/domains":
		domain := map[string]any{"object": "domain", "id": "dmn_1", "name": strings.TrimPrefix(f.frontend, "https://clerk."),
			"is_satellite": false, "frontend_api_url": f.frontend, "development_origin": ""}
		if f.targets != nil {
			domain["dns_targets"] = f.targets
		}
		reply(map[string]any{"data": []any{domain}, "total_count": 1})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/jwt_templates":
		reply(f.templates)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/jwt_templates":
		body["id"] = "jtmp_1"
		f.templates = append(f.templates, body)
		reply(body)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/jwt_templates/"):
		for i, tmpl := range f.templates {
			if tmpl["id"] == strings.TrimPrefix(r.URL.Path, "/v1/jwt_templates/") {
				body["id"] = tmpl["id"]
				f.templates[i] = body
			}
		}
		reply(body)
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/webhooks/svix_url" || r.URL.Path == "/v1/webhooks/svix"):
		if r.URL.Path == "/v1/webhooks/svix_url" && !f.svixApp {
			http.Error(w, `{"errors":[{"code":"svix_app_missing"}]}`, http.StatusBadRequest)
			return
		}
		f.svixApp = true
		// Clerk's real link: a one-time token and the region, exchanged at Svix for the portal token.
		key, _ := json.Marshal(map[string]string{"appId": "app_svix", "oneTimeToken": "ott_svix", "region": "us"})
		reply(map[string]any{"svix_url": "https://app.svix.com/login#key=" + base64.StdEncoding.EncodeToString(key)})
	default:
		f.t.Errorf("unexpected Clerk request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func newFakeClerk(t *testing.T, envType string) (*fakeClerk, *httptest.Server) {
	f := &fakeClerk{t: t, secretKey: "sk_test_lake", envType: envType,
		frontend: "https://above-seahorse-7535.clerk.accounts.dev", origins: []string{"http://localhost:3000"}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.svixBase = server.URL + "/svix"
	return f, server
}

func clerkTestLake(name, ns, host string) *oramav1alpha1.Lake {
	one := int32(1)
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: ns, Image: "oramasearch/lake:test", Replicas: &one, IngressEnabled: true, Host: host,
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-lake-live"},
			Env:          []corev1.EnvVar{{Name: "LAKE_CLERK_PUBLISHABLE_KEY", Value: "pk_test_handmade"}},
			Clerk:        &oramav1alpha1.LakeClerkSpec{SecretKeySecretRef: corev1.LocalObjectReference{Name: "clerk-secret-key"}},
		},
	}
}

type clerkHarness struct {
	t     *testing.T
	c     client.Client
	r     *LakeReconciler
	lake  string
	ns    string
	clerk *fakeClerk
}

func newClerkHarness(t *testing.T, lake *oramav1alpha1.Lake, clerk *fakeClerk, server *httptest.Server, objects ...client.Object) *clerkHarness {
	s := newLakeScheme(t)
	ns := lake.Spec.Namespace
	objects = append(objects, lake)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	h := &clerkHarness{t: t, c: c, lake: lake.Name, ns: ns, clerk: clerk}
	h.r = &LakeReconciler{Client: c, Scheme: s, ClerkAPIBase: server.URL + "/v1"}
	if clerk != nil {
		h.r.SvixAPIBase = clerk.svixBase
	}
	return h
}

func (h *clerkHarness) reconcile() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: h.lake}}); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func (h *clerkHarness) env() map[string]corev1.EnvVar {
	h.t.Helper()
	var d appsv1.Deployment
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: "lake-" + h.lake}, &d); err != nil {
		h.t.Fatal(err)
	}
	out := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e
	}
	return out
}

func (h *clerkHarness) getLake() *oramav1alpha1.Lake {
	h.t.Helper()
	var lake oramav1alpha1.Lake
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: h.lake}, &lake); err != nil {
		h.t.Fatal(err)
	}
	return &lake
}

func (h *clerkHarness) clerkRecords() []oramav1alpha1.CloudflareDNSRecord {
	h.t.Helper()
	var list oramav1alpha1.CloudflareDNSRecordList
	if err := h.c.List(context.Background(), &list, client.InNamespace(h.ns), client.MatchingLabels{"orama.cloud/dns-purpose": "clerk"}); err != nil {
		h.t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Spec.Name < list.Items[j].Spec.Name })
	return list.Items
}

func clerkKeySecret(ns string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "clerk-secret-key", Namespace: ns},
		Data: map[string][]byte{"secretKey": []byte("sk_test_lake")}}
}

// A Lake without spec.clerk does not change: no request reaches Clerk, the Deployment's env is
// exactly lakeEnv's (spec.env's LAKE_CLERK_PUBLISHABLE_KEY untouched, no webhook secret), and no
// Secret, DNS record or condition is added.
func TestALakeWithoutClerkIsUntouched(t *testing.T) {
	clerk, server := newFakeClerk(t, "development")
	lake := clerkTestLake("plain", "lake-plain", "plain.lake.example")
	lake.Spec.Clerk = nil
	h := newClerkHarness(t, lake, nil, server)
	h.r.SvixAPIBase = server.URL + "/svix"
	h.reconcile()
	h.reconcile()

	if got := clerk.log(); len(got) != 0 {
		t.Fatalf("a Lake without spec.clerk called Clerk: %v", got)
	}
	var d appsv1.Deployment
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: "lake-plain"}, &d); err != nil {
		t.Fatal(err)
	}
	current := h.getLake()
	sealing := lakeSealingMaterial{KID: current.Status.Sealing.KID}
	sealing.PublicKey, _ = base64.RawURLEncoding.DecodeString(current.Status.Sealing.PublicKey)
	if got, want := d.Spec.Template.Spec.Containers[0].Env, lakeEnv(current, sealing); !reflect.DeepEqual(got, want) {
		t.Errorf("env differs from lakeEnv:\n got %+v\nwant %+v", got, want)
	}
	env := h.env()
	if env["LAKE_CLERK_PUBLISHABLE_KEY"].Value != "pk_test_handmade" {
		t.Errorf("spec.env's LAKE_CLERK_PUBLISHABLE_KEY = %+v, want it kept", env["LAKE_CLERK_PUBLISHABLE_KEY"])
	}
	if _, found := env["LAKE_CLERK_WEBHOOK_SECRET"]; found {
		t.Error("LAKE_CLERK_WEBHOOK_SECRET given to a Lake without spec.clerk")
	}
	if apimeta.FindStatusCondition(current.Status.Conditions, "ClerkReady") != nil {
		t.Error("ClerkReady condition on a Lake without spec.clerk")
	}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: "lake-plain-clerk"}, &corev1.Secret{}); err == nil {
		t.Error("a Clerk Secret was created")
	}
	if got := h.clerkRecords(); len(got) != 0 {
		t.Errorf("Clerk DNS records were created: %v", got)
	}
}

func TestAClerkDevelopmentInstanceIsConfiguredForLakeAndTheLakeGetsItsKeys(t *testing.T) {
	clerk, server := newFakeClerk(t, "development")
	lake := clerkTestLake("dev", "lake-dev", "lake-dev.oramadev.net")
	h := newClerkHarness(t, lake, clerk, server, clerkKeySecret("lake-dev"))
	h.reconcile()

	wantWrites := []string{
		"POST /v1/jwt_templates",
		"PATCH /v1/instance",
		"POST /v1/webhooks/svix",
		"POST /svix/api/v1/app/app_svix/endpoint",
	}
	if got := writes(clerk.log()); !reflect.DeepEqual(got, wantWrites) {
		t.Fatalf("writes = %v, want %v", got, wantWrites)
	}
	clerk.mu.Lock()
	template := clerk.templates[0]
	if template["name"] != "lake" || template["lifetime"] != float64(300) ||
		!reflect.DeepEqual(template["claims"], map[string]any{"aud": "lake", "email": "{{user.primary_email_address}}", "name": "{{user.full_name}}"}) {
		t.Errorf("template = %v", template)
	}
	if want := []string{"http://localhost:3000", "https://lake-dev.oramadev.net"}; !reflect.DeepEqual(clerk.origins, want) {
		t.Errorf("allowed origins = %v, want %v (existing kept)", clerk.origins, want)
	}
	endpoint := clerk.endpoints[0]
	if endpoint["url"] != "https://lake-dev.oramadev.net/identity/clerk-webhook" ||
		!reflect.DeepEqual(endpoint["eventTypes"], []any{"session.ended", "session.removed", "session.revoked", "user.created", "user.deleted", "user.updated"}) {
		t.Errorf("endpoint = %v", endpoint)
	}
	clerk.mu.Unlock()

	env := h.env()
	pk := env["LAKE_CLERK_PUBLISHABLE_KEY"].Value
	if !strings.HasPrefix(pk, "pk_test_") || issuerFromPublishableKey(pk) != "https://above-seahorse-7535.clerk.accounts.dev" {
		t.Errorf("LAKE_CLERK_PUBLISHABLE_KEY = %q (spec.env's must be replaced)", pk)
	}
	if ref := env["LAKE_CLERK_WEBHOOK_SECRET"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "lake-dev-clerk" || ref.SecretKeyRef.Key != "webhookSecret" {
		t.Errorf("LAKE_CLERK_WEBHOOK_SECRET = %+v", env["LAKE_CLERK_WEBHOOK_SECRET"])
	}
	if ref := env["LAKE_CLERK_SECRET_KEY"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "clerk-secret-key" || ref.SecretKeyRef.Key != "secretKey" {
		t.Errorf("LAKE_CLERK_SECRET_KEY = %+v, want the Lake's own spec.clerk.secretKeySecretRef", env["LAKE_CLERK_SECRET_KEY"])
	}
	var owned corev1.Secret
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-dev-clerk"}, &owned); err != nil {
		t.Fatal(err)
	}
	if string(owned.Data["webhookSecret"]) != "whsec_ep_1" || len(owned.OwnerReferences) != 1 {
		t.Errorf("owned Secret = %v owners %v", owned.Data, owned.OwnerReferences)
	}
	condition := apimeta.FindStatusCondition(h.getLake().Status.Conditions, "ClerkReady")
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("ClerkReady = %+v", condition)
	}
	if got := h.clerkRecords(); len(got) != 0 {
		t.Errorf("a development instance got DNS records: %v", got)
	}

	// A steady Lake does not call Clerk again before the recheck.
	clerk.reset()
	h.reconcile()
	if got := clerk.log(); len(got) != 0 {
		t.Errorf("a steady Lake called Clerk: %v", got)
	}

	// A fresh pass (a restarted operator) reads everything and changes nothing.
	h.r.clerkClock = lakeClerkClock{}
	h.reconcile()
	if got := writes(clerk.log()); len(got) != 0 {
		t.Errorf("a repeated pass wrote %v", got)
	}

	// Removing spec.clerk removes what it made and gives spec.env back its variable.
	current := h.getLake()
	current.Spec.Clerk = nil
	if err := h.c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	clerk.reset()
	h.reconcile()
	if got := clerk.log(); len(got) != 0 {
		t.Errorf("removing spec.clerk called Clerk: %v", got)
	}
	env = h.env()
	if env["LAKE_CLERK_PUBLISHABLE_KEY"].Value != "pk_test_handmade" {
		t.Errorf("after removing spec.clerk LAKE_CLERK_PUBLISHABLE_KEY = %+v", env["LAKE_CLERK_PUBLISHABLE_KEY"])
	}
	if _, found := env["LAKE_CLERK_WEBHOOK_SECRET"]; found {
		t.Error("LAKE_CLERK_WEBHOOK_SECRET kept after removing spec.clerk")
	}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "lake-dev", Name: "lake-dev-clerk"}, &corev1.Secret{}); err == nil {
		t.Error("the Clerk Secret was kept")
	}
	if apimeta.FindStatusCondition(h.getLake().Status.Conditions, "ClerkReady") != nil {
		t.Error("ClerkReady kept after removing spec.clerk")
	}
}

func TestAProductionInstanceAdoptsItsWebhookAndWritesClerksDNS(t *testing.T) {
	clerk, server := newFakeClerk(t, "production")
	clerk.secretKey = "sk_live_lake"
	clerk.frontend = "https://clerk.lake.live"
	clerk.targets = []map[string]any{
		{"host": "clerk.lake.live", "value": "frontend-api.clerk.services", "record_type": "CNAME", "required": true, "automation_disposition": "safe_to_create"},
		{"host": "accounts.lake.live", "value": "accounts.clerk.services", "record_type": "CNAME", "required": true},
		{"host": "clk._domainkey.lake.live", "value": "dkim1.example.clerk.services", "record_type": "CNAME", "required": true},
	}
	// The instance is already partly configured: the template with an old lifetime, the origin,
	// and an endpoint at the Lake's URL with fewer events.
	clerk.templates = []map[string]any{{"id": "jtmp_old", "name": "lake", "lifetime": 60,
		"claims": map[string]any{"aud": "lake", "email": "{{user.primary_email_address}}", "name": "{{user.full_name}}"}}}
	clerk.origins = []string{"https://eu.lake.live", "https://lake.live"}
	clerk.svixApp = true
	clerk.endpoints = []map[string]any{{"id": "ep_existing", "url": "https://eu.lake.live/identity/clerk-webhook", "eventTypes": []any{"user.created"}, "disabled": false}}

	lake := clerkTestLake("shared", "lake-shared", "eu.lake.live")
	lake.Spec.ApexHost = "lake.live"
	secret := clerkKeySecret("lake-shared")
	secret.Data["secretKey"] = []byte("sk_live_lake")
	h := newClerkHarness(t, lake, clerk, server, secret)
	h.reconcile()

	wantWrites := []string{"PATCH /v1/jwt_templates/jtmp_old", "PATCH /svix/api/v1/app/app_svix/endpoint/ep_existing"}
	if got := writes(clerk.log()); !reflect.DeepEqual(got, wantWrites) {
		t.Fatalf("writes = %v, want %v (origins already there, endpoint adopted)", got, wantWrites)
	}
	if pk := h.env()["LAKE_CLERK_PUBLISHABLE_KEY"].Value; !strings.HasPrefix(pk, "pk_live_") || issuerFromPublishableKey(pk) != "https://clerk.lake.live" {
		t.Errorf("publishable key = %q", pk)
	}
	records := h.clerkRecords()
	if len(records) != 3 {
		t.Fatalf("DNS records = %d, want 3", len(records))
	}
	for _, rec := range records {
		if rec.Spec.Proxied || rec.Spec.DeletionPolicy != "Retain" || rec.Spec.SharedOwner != "clerk/ins_1" || rec.Spec.Type != "CNAME" ||
			rec.Spec.SecretRef != "cloudflare-lake-live" || len(rec.OwnerReferences) != 1 {
			t.Errorf("record %s = %+v owners %v", rec.Name, rec.Spec, rec.OwnerReferences)
		}
	}
	if records[0].Spec.Name != "accounts.lake.live" || records[1].Spec.Name != "clerk.lake.live" {
		t.Errorf("record names = %s, %s", records[0].Spec.Name, records[1].Spec.Name)
	}

	// Clerk dropping a target removes its resource on the next pass.
	clerk.mu.Lock()
	clerk.targets = clerk.targets[:2]
	clerk.mu.Unlock()
	h.r.clerkClock = lakeClerkClock{}
	h.reconcile()
	if got := h.clerkRecords(); len(got) != 2 {
		t.Errorf("after Clerk dropped a target: %d records", len(got))
	}
}

func TestAClerkFailureIsReportedAndDoesNotStopTheLake(t *testing.T) {
	clerk, server := newFakeClerk(t, "development")
	clerk.secretKey = "sk_test_other" // the Lake's key is refused
	lake := clerkTestLake("dev", "lake-dev", "lake-dev.oramadev.net")
	h := newClerkHarness(t, lake, clerk, server, clerkKeySecret("lake-dev"))
	h.reconcile()
	condition := apimeta.FindStatusCondition(h.getLake().Status.Conditions, "ClerkReady")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ClerkAPIError" {
		t.Fatalf("ClerkReady = %+v", condition)
	}
	env := h.env()
	if _, found := env["LAKE_CLERK_PUBLISHABLE_KEY"]; found {
		t.Errorf("a key was given before Clerk answered: %+v", env["LAKE_CLERK_PUBLISHABLE_KEY"])
	}

	// No secret key at all is reported too.
	h2 := newClerkHarness(t, clerkTestLake("nokey", "lake-nokey", "nokey.example"), clerk, server)
	h2.reconcile()
	condition = apimeta.FindStatusCondition(h2.getLake().Status.Conditions, "ClerkReady")
	if condition == nil || condition.Reason != "SecretKeyMissing" {
		t.Fatalf("ClerkReady without a key = %+v", condition)
	}
}

func TestClerkKeysAndSvixAddressesAreDerivedAsClerkAndSvixDo(t *testing.T) {
	pk := clerkPublishableKey("development", "https://above-seahorse-7535.clerk.accounts.dev")
	if pk != "pk_test_"+base64.StdEncoding.EncodeToString([]byte("above-seahorse-7535.clerk.accounts.dev$")) {
		t.Errorf("pk = %s", pk)
	}
	if got := issuerFromPublishableKey(clerkPublishableKey("production", "https://clerk.lake.live")); got != "https://clerk.lake.live" {
		t.Errorf("issuer = %s", got)
	}
	for token, want := range map[string]string{
		"appsk_x.eu": "https://api.eu.svix.com", "appsk_x.us": "https://api.us.svix.com", "appsk_x": "https://api.svix.com",
	} {
		if got := svixAPIBase(svixPortalKey{Token: token}); got != want {
			t.Errorf("svixAPIBase(%s) = %s, want %s", token, got, want)
		}
	}
	if got := svixAPIBase(svixPortalKey{Token: "appsk_x.eu", ServerURL: "https://svix.internal/"}); got != "https://svix.internal" {
		t.Errorf("serverUrl not preferred: %s", got)
	}
	key, _ := json.Marshal(map[string]string{"appId": "app_1", "token": "appsk_t"})
	parsed, err := parseSvixPortalURL("https://app.svix.com/login#key=" + base64.RawURLEncoding.EncodeToString(key))
	if err != nil || parsed.AppID != "app_1" || parsed.Token != "appsk_t" {
		t.Errorf("parsed = %+v, %v", parsed, err)
	}
	clerkKey, _ := json.Marshal(map[string]string{"appId": "app_2", "oneTimeToken": "ott_2", "region": "eu"})
	parsed, err = parseSvixPortalURL("https://app.svix.com/login?primaryColor=%23000#key=" + base64.StdEncoding.EncodeToString(clerkKey))
	if err != nil || parsed.AppID != "app_2" || parsed.OneTimeToken != "ott_2" || svixAPIBase(parsed) != "https://api.eu.svix.com" {
		t.Errorf("Clerk's link: parsed = %+v, %v, base %s", parsed, err, svixAPIBase(parsed))
	}
	if _, err := parseSvixPortalURL("https://app.svix.com/login"); err == nil {
		t.Error("a link without a key was accepted")
	}
}

// A Retain record leaves Cloudflare alone when its resource is deleted: Lakes sharing a Clerk
// production instance all write its records.
func TestARetainedDNSRecordStaysInCloudflare(t *testing.T) {
	cloudflare := &fakeCloudflareDNS{records: map[string]cfDNSRecord{}}
	server := httptest.NewServer(cloudflare)
	defer server.Close()
	ctx := context.Background()
	rec := &oramav1alpha1.CloudflareDNSRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-shared-clerk-1", Namespace: "lake-shared"},
		Spec: oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: "cf", Name: "clerk.lake.live", Type: "CNAME",
			Content: "frontend-api.clerk.services", Proxied: false, DeletionPolicy: "Retain"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "lake-shared"},
		Data: map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-1")}}
	c := fake.NewClientBuilder().WithScheme(newLakeScheme(t)).WithObjects(rec, secret).WithStatusSubresource(rec).Build()
	r := &CloudflareDNSRecordReconciler{Client: c, Scheme: c.Scheme(), APIBase: server.URL}
	key := types.NamespacedName{Namespace: "lake-shared", Name: "lake-shared-clerk-1"}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	cloudflare.mu.Lock()
	for _, record := range cloudflare.records {
		if record.Proxied {
			t.Errorf("record proxied = %v, want false", record.Proxied)
		}
	}
	cloudflare.mu.Unlock()
	if err := c.Get(ctx, key, rec); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if got := cloudflare.names(); len(got) != 1 {
		t.Errorf("Cloudflare records after deleting a Retain resource = %v, want the record kept", got)
	}
}
