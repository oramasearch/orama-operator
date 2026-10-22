/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// fakeRulesets is Cloudflare's zone lookup and Rulesets API for a few zones, recording every
// request.
type fakeRulesets struct {
	mu        sync.Mutex
	zones     map[string]string                // zone name -> ID
	rulesets  map[string]map[string]*cfRuleset // zone ID -> phase -> entrypoint
	nextID    int
	requests  []string // "METHOD path"
	bodies    []string // the body of every write
	failWrite int      // HTTP status for every DELETE, when non-zero
}

func newFakeRulesets() *fakeRulesets {
	return &fakeRulesets{
		zones:    map[string]string{"orama.ing": "zone-ing", "lake.live": "zone-live"},
		rulesets: map[string]map[string]*cfRuleset{},
	}
}

func (f *fakeRulesets) id(kind string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", kind, f.nextID)
}

// seed puts an entrypoint with rules in a zone's phase.
func (f *fakeRulesets) seed(zoneID, phase string, rules ...cfRule) {
	if f.rulesets[zoneID] == nil {
		f.rulesets[zoneID] = map[string]*cfRuleset{}
	}
	f.rulesets[zoneID][phase] = &cfRuleset{ID: f.id("rs"), Name: "default", Kind: "zone", Phase: phase, Rules: rules}
}

func (f *fakeRulesets) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeRulesets) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests, f.bodies = nil, nil
}

func (f *fakeRulesets) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"success":false}`, http.StatusUnauthorized)
			return
		}
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodGet {
			f.bodies = append(f.bodies, string(body))
		}
		reply := func(rs *cfRuleset) { _ = json.NewEncoder(w).Encode(map[string]any{"result": rs}) }
		if r.URL.Path == "/zones" {
			var result []cfZone
			if id, ok := f.zones[r.URL.Query().Get("name")]; ok {
				result = append(result, cfZone{ID: id, Name: r.URL.Query().Get("name")})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/zones/"), "/")
		if len(parts) < 3 || parts[1] != "rulesets" {
			http.NotFound(w, r)
			return
		}
		zoneID := parts[0]
		if parts[2] == "phases" && len(parts) == 5 {
			phase := parts[3]
			switch r.Method {
			case http.MethodGet:
				rs := f.rulesets[zoneID][phase]
				if rs == nil {
					http.Error(w, `{"success":false}`, http.StatusNotFound)
					return
				}
				reply(rs)
			case http.MethodPut:
				if f.rulesets[zoneID][phase] != nil {
					t.Errorf("PUT on an existing %s entrypoint in %s", phase, zoneID)
				}
				var rs cfRuleset
				_ = json.Unmarshal(body, &rs)
				for i := range rs.Rules {
					rs.Rules[i].ID = f.id("rule")
				}
				f.seed(zoneID, phase, rs.Rules...)
				reply(f.rulesets[zoneID][phase])
			}
			return
		}
		var rs *cfRuleset
		for _, candidate := range f.rulesets[zoneID] {
			if candidate.ID == parts[2] {
				rs = candidate
			}
		}
		if rs == nil || len(parts) < 4 || parts[3] != "rules" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Method == http.MethodPost && len(parts) == 4:
			var rule cfRule
			_ = json.Unmarshal(body, &rule)
			rule.ID = f.id("rule")
			at := len(rs.Rules)
			if rule.Position != nil && rule.Position.Index > 0 {
				at = rule.Position.Index - 1
			}
			rule.Position = nil
			rs.Rules = append(rs.Rules[:at], append([]cfRule{rule}, rs.Rules[at:]...)...)
			reply(rs)
		case len(parts) == 5:
			i := cfFindID(rs.Rules, parts[4])
			if i < 0 {
				http.Error(w, `{"success":false}`, http.StatusNotFound)
				return
			}
			switch r.Method {
			case http.MethodPatch:
				var rule cfRule
				_ = json.Unmarshal(body, &rule)
				rule.ID = rs.Rules[i].ID
				rs.Rules[i] = rule
				reply(rs)
			case http.MethodDelete:
				if f.failWrite != 0 {
					http.Error(w, `{"success":false}`, f.failWrite)
					return
				}
				rs.Rules = append(rs.Rules[:i], rs.Rules[i+1:]...)
				reply(rs)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func cfFindID(rules []cfRule, id string) int {
	for i, rule := range rules {
		if rule.ID == id {
			return i
		}
	}
	return -1
}

func foreignRule(id, ref, expression, action string, params string) cfRule {
	return cfRule{ID: id, Ref: ref, Description: "prod1 " + id, Expression: expression, Action: action, ActionParameters: json.RawMessage(params)}
}

type hostPolicyFixture struct {
	t      *testing.T
	c      client.Client
	r      *CloudflareHostPolicyReconciler
	api    *fakeRulesets
	server *httptest.Server
}

func newHostPolicyFixture(t *testing.T, api *fakeRulesets, spec oramav1alpha1.CloudflareHostPolicySpec) *hostPolicyFixture {
	t.Helper()
	server := httptest.NewServer(api.handler(t))
	t.Cleanup(server.Close)
	s := newLakeScheme(t)
	spec.APITokenSecretRef = oramav1alpha1.EdgeProviderSecretKeyRef{Name: "cf", Key: "token"}
	if spec.RuleRefPrefix == "" {
		spec.RuleRefPrefix = "orama-operator-v1-edge"
	}
	policy := &oramav1alpha1.CloudflareHostPolicy{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "v1", Generation: 1}, Spec: spec}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "v1"}, Data: map[string][]byte{"token": []byte("test-token")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(policy, secret).WithStatusSubresource(policy).Build()
	return &hostPolicyFixture{t: t, c: c, r: &CloudflareHostPolicyReconciler{Client: c, Scheme: s, APIBase: server.URL}, api: api, server: server}
}

func (f *hostPolicyFixture) reconcile() (oramav1alpha1.CloudflareHostPolicy, error) {
	f.t.Helper()
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "v1", Name: "edge"}})
	var out oramav1alpha1.CloudflareHostPolicy
	if getErr := f.c.Get(context.Background(), types.NamespacedName{Namespace: "v1", Name: "edge"}, &out); getErr != nil {
		return out, getErr
	}
	return out, err
}

func (f *hostPolicyFixture) mustReconcile() oramav1alpha1.CloudflareHostPolicy {
	f.t.Helper()
	out, err := f.reconcile()
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return out
}

func (f *hostPolicyFixture) update(mutate func(*oramav1alpha1.CloudflareHostPolicySpec)) {
	f.t.Helper()
	var p oramav1alpha1.CloudflareHostPolicy
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "v1", Name: "edge"}, &p); err != nil {
		f.t.Fatal(err)
	}
	mutate(&p.Spec)
	p.Generation++
	if err := f.c.Update(context.Background(), &p); err != nil {
		f.t.Fatal(err)
	}
}

func TestCloudflareHostPolicyCreatesAMissingEntrypointWithOnlyOurRule(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"reader.orama.ing", "app.orama.ing"}, OriginTLS: "strict"})
	got := f.mustReconcile()
	if !got.Status.Ready || len(got.Status.Zones) != 1 || got.Status.Zones[0].ConfigRuleID == "" || got.Status.Zones[0].RedirectRuleID != "" {
		t.Fatalf("status = %+v", got.Status)
	}
	if w := api.writes(); len(w) != 1 || w[0] != "PUT /zones/zone-ing/rulesets/phases/http_config_settings/entrypoint" {
		t.Fatalf("writes = %v", w)
	}
	rules := api.rulesets["zone-ing"][cfPhaseConfig].Rules
	if len(rules) != 1 || rules[0].Ref != "orama-operator-v1-edge-tls" ||
		rules[0].Expression != `http.host in {"app.orama.ing" "reader.orama.ing"}` || rules[0].Action != "set_config" ||
		!cfSameJSON(rules[0].ActionParameters, json.RawMessage(`{"ssl":"strict"}`)) {
		t.Fatalf("rules = %+v", rules)
	}
	if api.rulesets["zone-ing"][cfPhaseRedirect] != nil {
		t.Fatalf("a redirect entrypoint was created though redirectToHTTPS is off")
	}
}

func TestCloudflareHostPolicyAddsOurRuleBesideForeignRulesWithoutSendingThem(t *testing.T) {
	api := newFakeRulesets()
	foreign := []cfRule{
		foreignRule("f1", "prod1-a", `http.host eq "dashboard.orama.ing"`, "set_config", `{"ssl":"full"}`),
		foreignRule("f2", "prod1-b", `http.host eq "docs.orama.ing"`, "set_config", `{"rocket_loader":false}`),
		foreignRule("f3", "", `http.host eq "status.orama.ing"`, "set_config", `{"ssl":"flexible"}`),
	}
	api.seed("zone-ing", cfPhaseConfig, foreign...)
	api.seed("zone-ing", cfPhaseRedirect,
		foreignRule("r1", "prod1-r", `(http.host eq "old.orama.ing")`, "redirect", `{"from_value":{"status_code":302}}`))
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true})
	got := f.mustReconcile()
	if !got.Status.Ready {
		t.Fatalf("status = %+v", got.Status)
	}
	writes := api.writes()
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "POST /zones/zone-ing/rulesets/") || !strings.HasPrefix(writes[1], "POST /zones/zone-ing/rulesets/") {
		t.Fatalf("writes = %v", writes)
	}
	for _, body := range api.bodies {
		for _, id := range []string{"f1", "f2", "f3", "r1", "prod1", "dashboard.orama.ing", "old.orama.ing"} {
			if strings.Contains(body, id) {
				t.Fatalf("a foreign rule (%s) was sent: %s", id, body)
			}
		}
	}
	config := api.rulesets["zone-ing"][cfPhaseConfig].Rules
	if len(config) != 4 || config[3].Ref != "orama-operator-v1-edge-tls" {
		t.Fatalf("the config rule is not appended last: %+v", config)
	}
	redirect := api.rulesets["zone-ing"][cfPhaseRedirect].Rules
	if len(redirect) != 2 || redirect[0].Ref != "orama-operator-v1-edge-https" || redirect[1].ID != "r1" {
		t.Fatalf("the redirect rule is not first: %+v", redirect)
	}
	if redirect[0].Expression != `(http.request.scheme eq "http" and http.host in {"app.orama.ing"})` {
		t.Fatalf("redirect expression = %s", redirect[0].Expression)
	}
	var params map[string]any
	_ = json.Unmarshal(redirect[0].ActionParameters, &params)
	from := params["from_value"].(map[string]any)
	if from["status_code"].(float64) != 301 || from["preserve_query_string"] != true ||
		from["target_url"].(map[string]any)["expression"] != `concat("https://", http.host, http.request.uri.path)` {
		t.Fatalf("redirect parameters = %s", redirect[0].ActionParameters)
	}
}

func TestCloudflareHostPolicyUnchangedSpecOnlyReads(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true})
	first := f.mustReconcile()
	api.reset()
	second := f.mustReconcile()
	if w := api.writes(); len(w) != 0 {
		t.Fatalf("second reconcile wrote %v", w)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("a steady policy wrote its status again")
	}
}

func TestCloudflareHostPolicyHostAddedPatchesSortedExpression(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"reader.orama.ing"}, OriginTLS: "strict"})
	first := f.mustReconcile()
	api.reset()
	f.update(func(s *oramav1alpha1.CloudflareHostPolicySpec) { s.Hosts = append(s.Hosts, "app.orama.ing") })
	got := f.mustReconcile()
	writes := api.writes()
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "PATCH /zones/zone-ing/rulesets/") || !strings.HasSuffix(writes[0], "/rules/"+first.Status.Zones[0].ConfigRuleID) {
		t.Fatalf("writes = %v", writes)
	}
	rule := api.rulesets["zone-ing"][cfPhaseConfig].Rules[0]
	if rule.Expression != `http.host in {"app.orama.ing" "reader.orama.ing"}` || rule.ID != first.Status.Zones[0].ConfigRuleID {
		t.Fatalf("rule = %+v", rule)
	}
	if !got.Status.Ready || got.Status.ObservedGeneration != 2 {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestCloudflareHostPolicyOneRulePerZonePerPhase(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{
		Hosts: []string{"app.orama.ing", "eu.lake.live", "reader.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true,
	})
	got := f.mustReconcile()
	if !got.Status.Ready || len(got.Status.Zones) != 2 || got.Status.Zones[0].ZoneID != "zone-ing" || got.Status.Zones[1].ZoneID != "zone-live" {
		t.Fatalf("status = %+v", got.Status)
	}
	if got.Status.Zones[1].ZoneName != "lake.live" {
		t.Fatalf("zone name = %q", got.Status.Zones[1].ZoneName)
	}
	for zone, hosts := range map[string]string{"zone-ing": `{"app.orama.ing" "reader.orama.ing"}`, "zone-live": `{"eu.lake.live"}`} {
		for _, phase := range []string{cfPhaseConfig, cfPhaseRedirect} {
			rules := api.rulesets[zone][phase].Rules
			if len(rules) != 1 || !strings.Contains(rules[0].Expression, hosts) {
				t.Fatalf("%s %s rules = %+v", zone, phase, rules)
			}
		}
	}
	if len(api.writes()) != 4 {
		t.Fatalf("writes = %v", api.writes())
	}
}

func TestCloudflareHostPolicyRedirectOffDeletesOnlyTheRedirectRule(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true})
	first := f.mustReconcile()
	api.reset()
	f.update(func(s *oramav1alpha1.CloudflareHostPolicySpec) { s.RedirectToHTTPS = false })
	got := f.mustReconcile()
	writes := api.writes()
	if len(writes) != 1 || writes[0] != fmt.Sprintf("DELETE /zones/zone-ing/rulesets/%s/rules/%s", api.rulesets["zone-ing"][cfPhaseRedirect].ID, first.Status.Zones[0].RedirectRuleID) {
		t.Fatalf("writes = %v", writes)
	}
	if len(api.rulesets["zone-ing"][cfPhaseRedirect].Rules) != 0 || len(api.rulesets["zone-ing"][cfPhaseConfig].Rules) != 1 {
		t.Fatalf("rulesets = %+v", api.rulesets["zone-ing"])
	}
	if !got.Status.Ready || got.Status.Zones[0].RedirectRuleID != "" || got.Status.Zones[0].ConfigRuleID == "" {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestCloudflareHostPolicyNeverTouchesAForeignRuleWithOurExpression(t *testing.T) {
	api := newFakeRulesets()
	same := foreignRule("f1", "someone-else-tls", `http.host in {"app.orama.ing"}`, "set_config", `{"ssl":"strict"}`)
	api.seed("zone-ing", cfPhaseConfig, same)
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict"})
	f.mustReconcile()
	f.update(func(s *oramav1alpha1.CloudflareHostPolicySpec) {
		s.Hosts = []string{"app.orama.ing", "reader.orama.ing"}
	})
	f.mustReconcile()
	f.update(func(s *oramav1alpha1.CloudflareHostPolicySpec) { s.OriginTLS = "" })
	f.mustReconcile()
	for _, w := range api.writes() {
		if strings.HasSuffix(w, "/rules/f1") {
			t.Fatalf("the foreign rule was written: %v", api.writes())
		}
	}
	rules := api.rulesets["zone-ing"][cfPhaseConfig].Rules
	if len(rules) != 1 || rules[0].ID != "f1" || rules[0].Expression != same.Expression {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestCloudflareHostPolicyDeleteKeepsTheFinalizerOnAServerError(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true})
	created := f.mustReconcile()
	if len(created.Finalizers) != 1 || created.Finalizers[0] != cfHostPolicyFinalizer {
		t.Fatalf("finalizers = %v", created.Finalizers)
	}
	if err := f.c.Delete(context.Background(), &created); err != nil {
		t.Fatal(err)
	}
	api.failWrite = http.StatusInternalServerError
	got, err := f.reconcile()
	if err == nil {
		t.Fatalf("a failed delete returned no error")
	}
	if len(got.Finalizers) != 1 || got.Status.Ready {
		t.Fatalf("finalizer dropped or status ready after a 500: %+v %+v", got.Finalizers, got.Status)
	}
	api.failWrite = 0
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("after a successful cleanup the policy should be gone, got %v", err)
	}
	for _, phase := range []string{cfPhaseConfig, cfPhaseRedirect} {
		if n := len(api.rulesets["zone-ing"][phase].Rules); n != 0 {
			t.Fatalf("%s still holds %d rule(s)", phase, n)
		}
	}
}

func TestCloudflareHostPolicyDeleteLeavesForeignRules(t *testing.T) {
	api := newFakeRulesets()
	api.seed("zone-ing", cfPhaseConfig, foreignRule("f1", "prod1-a", `http.host eq "dashboard.orama.ing"`, "set_config", `{"ssl":"full"}`))
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict"})
	created := f.mustReconcile()
	if err := f.c.Delete(context.Background(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatalf("expected the policy to be gone")
	}
	rules := api.rulesets["zone-ing"][cfPhaseConfig].Rules
	if len(rules) != 1 || rules[0].ID != "f1" {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestCloudflareHostPolicyReportsAnOverridingRuleWithoutMovingIt(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true})
	f.mustReconcile()
	// Someone appends a config rule after ours that sets the zone's hosts to Flexible, and puts a
	// redirect before ours.
	config := api.rulesets["zone-ing"][cfPhaseConfig]
	config.Rules = append(config.Rules, foreignRule("late", "", `ends_with(http.host, ".orama.ing")`, "set_config", `{"ssl":"flexible"}`))
	redirect := api.rulesets["zone-ing"][cfPhaseRedirect]
	redirect.Rules = append([]cfRule{foreignRule("early", "", `http.host eq "app.orama.ing"`, "redirect", `{"from_value":{"status_code":302}}`)}, redirect.Rules...)
	api.reset()
	got := f.mustReconcile()
	if got.Status.Ready || got.Status.Zones[0].Ready {
		t.Fatalf("an overridden policy is ready: %+v", got.Status)
	}
	for _, want := range []string{"rule late", "rule early", "app.orama.ing"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Fatalf("message %q does not name %q", got.Status.Message, want)
		}
	}
	if w := api.writes(); len(w) != 0 {
		t.Fatalf("an overriding rule made the operator write: %v", w)
	}
	if config.Rules[1].ID != "late" || redirect.Rules[0].ID != "early" {
		t.Fatalf("foreign rules were moved")
	}
}

func TestCloudflareHostPolicyNamesTheMissingPermission(t *testing.T) {
	api := newFakeRulesets()
	f := newHostPolicyFixture(t, api, oramav1alpha1.CloudflareHostPolicySpec{Hosts: []string{"app.example.com"}, OriginTLS: "strict"})
	got := f.mustReconcile()
	if got.Status.Ready || !strings.Contains(got.Status.Message, "no zone the token can read holds app.example.com") {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestCfRuleMayMatch(t *testing.T) {
	hosts := []string{"app.orama.ing", "reader.orama.ing"}
	for expression, want := range map[string]string{
		`http.host eq "reader.orama.ing"`:                          "reader.orama.ing",
		`http.host in {"x.orama.ing" "app.orama.ing"}`:             "app.orama.ing",
		`ends_with(http.host, ".orama.ing")`:                       "app.orama.ing",
		`http.host eq "orama.ing"`:                                 "",
		`ends_with(http.host, "orama.ing")`:                        "app.orama.ing",
		`http.host contains "reader"`:                              "reader.orama.ing",
		`http.host eq "dashboard.orama.ing"`:                       "",
		`http.request.uri.path eq "/x"`:                            "app.orama.ing",
		`true`:                                                     "app.orama.ing",
		`http.host matches "^.*$"`:                                 "app.orama.ing",
		`http.request.full_uri eq "https://reader.orama.ing/v1/x"`: "reader.orama.ing",
	} {
		if got := cfRuleMayMatch(expression, hosts); got != want {
			t.Errorf("cfRuleMayMatch(%q) = %q, want %q", expression, got, want)
		}
	}
}

func TestEdgeHostRuleRefPrefix(t *testing.T) {
	if got := edgeHostRuleRefPrefix("v1-eu", "edge.hosts"); got != "orama-operator-v1-eu-edge-hosts" {
		t.Fatalf("prefix = %q", got)
	}
	long := edgeHostRuleRefPrefix("a-very-long-namespace-name-for-a-cluster", "an-equally-long-policy-name-that-goes-on")
	if len(long) > cfRuleRefPrefixLimit || !cfHostPolicyRefPattern.MatchString(long) {
		t.Fatalf("long prefix = %q (%d)", long, len(long))
	}
	if long == edgeHostRuleRefPrefix("a-very-long-namespace-name-for-a-cluster", "an-equally-long-policy-name-that-goes-off") {
		t.Fatalf("two long names share a prefix")
	}
}

func TestEdgeHostPolicyCreatesItsCloudflareHostPolicy(t *testing.T) {
	s := newLakeScheme(t)
	policy := &oramav1alpha1.EdgeHostPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "v1", Generation: 3, Labels: map[string]string{"app": "v1"}},
		Spec: oramav1alpha1.EdgeHostPolicySpec{
			ProviderRef: corev1.LocalObjectReference{Name: "cf"}, Hosts: []string{"app.orama.ing"}, OriginTLS: "strict", RedirectToHTTPS: true,
		},
	}
	provider := &oramav1alpha1.EdgeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "cf", Namespace: "v1"},
		Spec: oramav1alpha1.EdgeProviderSpec{Type: "cloudflare", Cloudflare: &oramav1alpha1.CloudflareEdgeProviderSpec{
			AccountID: "acct", APITokenSecretRef: oramav1alpha1.EdgeProviderSecretKeyRef{Name: "cf-token", Key: "token"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(policy, provider).
		WithStatusSubresource(policy, &oramav1alpha1.CloudflareHostPolicy{}).Build()
	r := &EdgeHostPolicyReconciler{Client: c, Scheme: s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "v1", Name: "edge"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var concrete oramav1alpha1.CloudflareHostPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &concrete); err != nil {
		t.Fatal(err)
	}
	if concrete.Spec.AccountID != "acct" || concrete.Spec.APITokenSecretRef.Name != "cf-token" || concrete.Spec.RuleRefPrefix != "orama-operator-v1-edge" ||
		concrete.Spec.OriginTLS != "strict" || !concrete.Spec.RedirectToHTTPS || len(concrete.Spec.Hosts) != 1 || concrete.Labels["app"] != "v1" {
		t.Fatalf("concrete = %+v", concrete)
	}
	if len(concrete.OwnerReferences) != 1 || concrete.OwnerReferences[0].Name != "edge" {
		t.Fatalf("owner = %+v", concrete.OwnerReferences)
	}
	var got oramav1alpha1.EdgeHostPolicy
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Ready || got.Status.ObservedGeneration != 3 || !strings.Contains(got.Status.Message, "waiting") {
		t.Fatalf("status = %+v", got.Status)
	}
	concrete.Status = oramav1alpha1.CloudflareHostPolicyStatus{Ready: true, Message: "the rules are in place in every zone", ObservedGeneration: concrete.Generation}
	if err := c.Status().Update(context.Background(), &concrete); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if !got.Status.Ready || got.Status.Message != "the rules are in place in every zone" {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestEdgeHostPolicyWithoutProviderIsNotReady(t *testing.T) {
	s := newLakeScheme(t)
	policy := &oramav1alpha1.EdgeHostPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "v1"},
		Spec:       oramav1alpha1.EdgeHostPolicySpec{ProviderRef: corev1.LocalObjectReference{Name: "missing"}, Hosts: []string{"app.orama.ing"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(policy).WithStatusSubresource(policy).Build()
	r := &EdgeHostPolicyReconciler{Client: c, Scheme: s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "v1", Name: "edge"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var got oramav1alpha1.EdgeHostPolicy
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Ready || !strings.Contains(got.Status.Message, `EdgeProvider "missing" does not exist`) {
		t.Fatalf("status = %+v", got.Status)
	}
	var concrete oramav1alpha1.CloudflareHostPolicyList
	_ = c.List(context.Background(), &concrete)
	if len(concrete.Items) != 0 {
		t.Fatalf("a concrete policy was created without a provider")
	}
}
