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
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The two Rulesets API phases a host policy writes to (operator#527).
//
// Evaluation order, from Cloudflare's Rules documentation ("Execution order"): in a phase whose
// actions do not terminate (http_config_settings, action set_config), every matching rule applies
// and "the last change made by rules in the same phase will win", so our Full (strict) rule is
// appended last and a later rule that sets ssl to something else for one of our hosts overrides
// it. A redirect terminates evaluation ("Cloudflare will always use the URL redirect of the first
// rule that matches"), so our redirect rule is inserted first and an earlier redirect that matches
// one of our hosts overrides it. Either way the other rule is reported, never moved.
const (
	cfPhaseConfig   = "http_config_settings"
	cfPhaseRedirect = "http_request_dynamic_redirect"

	cfHostPolicyFinalizer = "orama.cloud/cloudflare-hostpolicy-cleanup"

	// The token permissions a host policy needs, as the dashboard names them (the API permission
	// groups are "Zone Read", "Config Settings Write" and "Dynamic URL Redirects Write").
	cfPermissionZoneRead    = "Zone > Zone > Read"
	cfPermissionConfigRules = "Zone > Config Rules > Edit"
	cfPermissionRedirects   = "Zone > Single Redirect > Edit"
)

// cfRule is one rule of a Rulesets API ruleset.
type cfRule struct {
	ID               string          `json:"id,omitempty"`
	Ref              string          `json:"ref,omitempty"`
	Description      string          `json:"description,omitempty"`
	Expression       string          `json:"expression"`
	Action           string          `json:"action"`
	ActionParameters json.RawMessage `json:"action_parameters,omitempty"`
	Enabled          *bool           `json:"enabled,omitempty"`
	Position         *cfRulePosition `json:"position,omitempty"`
}

type cfRulePosition struct {
	Index int `json:"index,omitempty"`
}

func (r cfRule) enabled() bool { return r.Enabled == nil || *r.Enabled }

type cfRuleset struct {
	ID    string   `json:"id,omitempty"`
	Name  string   `json:"name,omitempty"`
	Kind  string   `json:"kind,omitempty"`
	Phase string   `json:"phase,omitempty"`
	Rules []cfRule `json:"rules"`
}

type cfRulesetResponse struct {
	Result cfRuleset `json:"result"`
}

// cfHostSet renders hosts as a sorted Rules-language set, so one spec always gives one expression.
func cfHostSet(hosts []string) string {
	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, host := range sorted {
		quoted[i] = `"` + host + `"`
	}
	return "{" + strings.Join(quoted, " ") + "}"
}

func cfRawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// hostPolicyRule is the rule a policy wants in phase for hosts.
func hostPolicyRule(phase, prefix string, hosts []string) cfRule {
	enabled := true
	if phase == cfPhaseConfig {
		return cfRule{
			Ref:              prefix + "-tls",
			Description:      "orama-operator " + prefix + ": origin encryption Full (strict)",
			Expression:       "http.host in " + cfHostSet(hosts),
			Action:           "set_config",
			ActionParameters: cfRawJSON(map[string]string{"ssl": "strict"}),
			Enabled:          &enabled,
		}
	}
	return cfRule{
		Ref:         prefix + "-https",
		Description: "orama-operator " + prefix + ": redirect HTTP to HTTPS",
		Expression:  `(http.request.scheme eq "http" and http.host in ` + cfHostSet(hosts) + `)`,
		Action:      "redirect",
		ActionParameters: cfRawJSON(map[string]any{"from_value": map[string]any{
			"status_code":           301,
			"preserve_query_string": true,
			"target_url":            map[string]string{"expression": `concat("https://", http.host, http.request.uri.path)`},
		}}),
		Enabled: &enabled,
	}
}

// cfSameJSON compares two JSON documents by value, ignoring key order and spacing.
func cfSameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(x, y)
}

// cfRuleEqual is whether a live rule already does what the desired one asks.
func cfRuleEqual(live, desired cfRule) bool {
	return live.Expression == desired.Expression && live.Action == desired.Action &&
		live.Description == desired.Description && live.enabled() == desired.enabled() &&
		cfSameJSON(live.ActionParameters, desired.ActionParameters)
}

var cfQuotedLiteral = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// cfRuleMayMatch returns one of hosts that a rule's expression may match, or "". It errs on the
// side of a match: an expression that does not test the host at all, or tests it with a pattern,
// matches every host.
func cfRuleMayMatch(expression string, hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	testsHost := strings.Contains(expression, "http.host") || strings.Contains(expression, "http.request.full_uri")
	if !testsHost || strings.Contains(expression, " matches ") || strings.Contains(expression, " ~ ") || strings.Contains(expression, "wildcard") {
		return hosts[0]
	}
	// A part-of-the-name test (ends_with, contains) may match on any literal the host ends with
	// or contains; otherwise a literal matches its own host only.
	partial := strings.Contains(expression, "ends_with") || strings.Contains(expression, "contains") || strings.Contains(expression, "starts_with")
	for _, m := range cfQuotedLiteral.FindAllStringSubmatch(expression, -1) {
		literal := strings.ToLower(m[1])
		if literal == "" {
			continue
		}
		for _, host := range hosts {
			if host == literal || strings.Contains(literal, "//"+host) || (partial && strings.Contains(host, literal)) {
				return host
			}
		}
	}
	return ""
}

// cfOverridingRule finds a foreign rule that would override ours (rules[ours]) for one of hosts.
func cfOverridingRule(phase string, rules []cfRule, ours int, hosts []string) (cfRule, string, bool) {
	desired := rules[ours]
	for i, rule := range rules {
		if i == ours || !rule.enabled() {
			continue
		}
		switch phase {
		case cfPhaseConfig:
			// Applied after ours, so its ssl wins.
			if i < ours || rule.Action != "set_config" {
				continue
			}
			var params map[string]any
			_ = json.Unmarshal(rule.ActionParameters, &params)
			ssl, sets := params["ssl"]
			if !sets || ssl == "strict" {
				continue
			}
		case cfPhaseRedirect:
			// Evaluated before ours, and a redirect stops evaluation.
			if i > ours || rule.Action != "redirect" || cfSameJSON(rule.ActionParameters, desired.ActionParameters) {
				continue
			}
		}
		if host := cfRuleMayMatch(rule.Expression, hosts); host != "" {
			return rule, host, true
		}
	}
	return cfRule{}, "", false
}

// cfZone is a zone found by name.
type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfZoneList struct {
	Result []cfZone `json:"result"`
}

// cfZoneFinder finds the zone of a host by its longest suffix that is a zone the token can read.
// Lookups are cached for one reconcile.
type cfZoneFinder struct {
	api, token, accountID string
	cache                 map[string]*cfZone
}

func (f *cfZoneFinder) find(ctx context.Context, host string) (cfZone, bool, error) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	for i := 0; i+1 < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		zone, cached := f.cache[candidate]
		if !cached {
			query := url.Values{"name": {candidate}}
			if f.accountID != "" {
				query.Set("account.id", f.accountID)
			}
			var out cfZoneList
			if err := cfDo(ctx, f.token, http.MethodGet, f.api+"/zones?"+query.Encode(), nil, &out); err != nil {
				return cfZone{}, false, err
			}
			zone = nil
			for j := range out.Result {
				if strings.EqualFold(out.Result[j].Name, candidate) {
					zone = &out.Result[j]
					break
				}
			}
			f.cache[candidate] = zone
		}
		if zone != nil {
			return *zone, true, nil
		}
	}
	return cfZone{}, false, nil
}

// CloudflareHostPolicyReconciler applies a CloudflareHostPolicy as at most two rules per zone,
// each identified by its ref: a set_config rule for Full (strict) and a redirect rule for HTTPS. It
// never replaces a phase's ruleset that already exists, never touches a rule whose ref is not its
// own, and never changes a zone setting.
type CloudflareHostPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIBase is the Cloudflare API base URL; empty means the public API. Tests point it at a fake.
	APIBase string
}

func (r *CloudflareHostPolicyReconciler) api() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return cfAPIBase
}

// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarehostpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarehostpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarehostpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *CloudflareHostPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy oramav1alpha1.CloudflareHostPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if policy.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&policy, cfHostPolicyFinalizer) {
		patch := client.MergeFromWithOptions(policy.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(&policy, cfHostPolicyFinalizer)
		if err := r.Patch(ctx, &policy, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	previous := *policy.Status.DeepCopy()

	if !policy.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&policy, cfHostPolicyFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanup(ctx, &policy); err != nil {
			policy.Status.Ready, policy.Status.Message = false, "deleting the policy's rules: "+err.Error()
			if statusErr := r.writeStatus(ctx, &policy, previous); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		patch := client.MergeFromWithOptions(policy.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.RemoveFinalizer(&policy, cfHostPolicyFinalizer)
		return ctrl.Result{}, r.Patch(ctx, &policy, patch)
	}

	token, problem := r.token(ctx, &policy)
	if problem != "" {
		policy.Status.Ready, policy.Status.Message = false, problem
	} else {
		policy.Status = r.apply(ctx, &policy, token)
	}
	policy.Status.ObservedGeneration = policy.Generation
	if err := r.writeStatus(ctx, &policy, previous); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: edgeZoneRecheck}, nil
}

func (r *CloudflareHostPolicyReconciler) writeStatus(ctx context.Context, policy *oramav1alpha1.CloudflareHostPolicy, previous oramav1alpha1.CloudflareHostPolicyStatus) error {
	if equality.Semantic.DeepEqual(previous, policy.Status) {
		return nil
	}
	return r.Status().Update(ctx, policy)
}

func (r *CloudflareHostPolicyReconciler) token(ctx context.Context, policy *oramav1alpha1.CloudflareHostPolicy) (string, string) {
	ref := policy.Spec.APITokenSecretRef
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("token Secret %q does not exist", ref.Name)
		}
		return "", fmt.Sprintf("read token Secret %q: %v", ref.Name, err)
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return "", fmt.Sprintf("token Secret %q has no key %q", ref.Name, ref.Key)
	}
	return token, ""
}

var (
	cfHostPattern          = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
	cfHostPolicyRefPattern = regexp.MustCompile(`^[a-z0-9][-a-z0-9]*$`)
)

// apply converges every zone's rules and returns the status.
func (r *CloudflareHostPolicyReconciler) apply(ctx context.Context, policy *oramav1alpha1.CloudflareHostPolicy, token string) oramav1alpha1.CloudflareHostPolicyStatus {
	if !cfHostPolicyRefPattern.MatchString(policy.Spec.RuleRefPrefix) {
		return oramav1alpha1.CloudflareHostPolicyStatus{Zones: policy.Status.Zones, Message: fmt.Sprintf("ruleRefPrefix %q is not a rule ref", policy.Spec.RuleRefPrefix)}
	}
	finder := &cfZoneFinder{api: r.api(), token: token, accountID: policy.Spec.AccountID, cache: map[string]*cfZone{}}
	type group struct {
		name  string
		hosts []string
	}
	groups := map[string]*group{}
	var problems []string
	lookupFailed := false
	for _, host := range policy.Spec.Hosts {
		// The CRD validates hosts; checked again because each one is written into an expression.
		if !cfHostPattern.MatchString(host) {
			problems = append(problems, fmt.Sprintf("host %q is not a host name", host))
			continue
		}
		zone, found, err := finder.find(ctx, host)
		if err != nil {
			lookupFailed = true
			problems = append(problems, cfErrorMessage("find the zone of "+host, err, cfPermissionZoneRead))
			continue
		}
		if !found {
			problems = append(problems, fmt.Sprintf("no zone the token can read holds %s (it needs %s on the host's zone)", host, cfPermissionZoneRead))
			continue
		}
		g := groups[zone.ID]
		if g == nil {
			g = &group{name: zone.Name}
			groups[zone.ID] = g
		}
		g.hosts = append(g.hosts, host)
	}

	previous := map[string]oramav1alpha1.CloudflareHostPolicyZoneStatus{}
	for _, zone := range policy.Status.Zones {
		previous[zone.ZoneID] = zone
	}
	var zones []oramav1alpha1.CloudflareHostPolicyZoneStatus
	for zoneID, g := range groups {
		zones = append(zones, r.applyZone(ctx, token, policy, zoneID, g.name, g.hosts))
	}
	for zoneID, old := range previous {
		if groups[zoneID] != nil {
			continue
		}
		if lookupFailed {
			// A host whose zone could not be looked up may still be in this zone: keep its rules.
			zones = append(zones, old)
			continue
		}
		if err := r.deleteZoneRules(ctx, token, policy.Spec.RuleRefPrefix, zoneID); err != nil {
			old.Ready, old.Message = false, "deleting rules of a zone with no host left: "+err.Error()
			zones = append(zones, old)
		}
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].ZoneID < zones[j].ZoneID })

	status := oramav1alpha1.CloudflareHostPolicyStatus{Zones: zones, Ready: len(problems) == 0}
	for _, zone := range zones {
		if !zone.Ready {
			status.Ready = false
			name := zone.ZoneName
			if name == "" {
				name = zone.ZoneID
			}
			problems = append(problems, name+": "+zone.Message)
		}
	}
	if status.Ready {
		status.Message = "the rules are in place in every zone"
	} else {
		status.Message = strings.Join(problems, "; ")
	}
	return status
}

func (r *CloudflareHostPolicyReconciler) applyZone(ctx context.Context, token string, policy *oramav1alpha1.CloudflareHostPolicy, zoneID, zoneName string, hosts []string) oramav1alpha1.CloudflareHostPolicyZoneStatus {
	sort.Strings(hosts)
	zone := oramav1alpha1.CloudflareHostPolicyZoneStatus{ZoneID: zoneID, ZoneName: zoneName}
	var problems []string
	for _, phase := range []string{cfPhaseConfig, cfPhaseRedirect} {
		desired := hostPolicyRule(phase, policy.Spec.RuleRefPrefix, hosts)
		want := (phase == cfPhaseConfig && policy.Spec.OriginTLS == "strict") || (phase == cfPhaseRedirect && policy.Spec.RedirectToHTTPS)
		var wanted *cfRule
		if want {
			wanted = &desired
		}
		id, problem := r.syncRule(ctx, token, zoneID, phase, desired.Ref, wanted, hosts)
		if phase == cfPhaseConfig {
			zone.ConfigRuleID = id
		} else {
			zone.RedirectRuleID = id
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	zone.Ready = len(problems) == 0
	zone.Message = strings.Join(problems, "; ")
	return zone
}

func cfPhasePermission(phase string) string {
	if phase == cfPhaseConfig {
		return cfPermissionConfigRules
	}
	return cfPermissionRedirects
}

// entrypoint reads a phase's zone entrypoint ruleset; exists is false when the zone has none.
func (r *CloudflareHostPolicyReconciler) entrypoint(ctx context.Context, token, zoneID, phase string) (cfRuleset, bool, error) {
	var out cfRulesetResponse
	err := cfDo(ctx, token, http.MethodGet, fmt.Sprintf("%s/zones/%s/rulesets/phases/%s/entrypoint", r.api(), zoneID, phase), nil, &out)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return cfRuleset{}, false, nil
	}
	if err != nil {
		return cfRuleset{}, false, err
	}
	return out.Result, true, nil
}

func cfFindRef(rules []cfRule, ref string) int {
	for i, rule := range rules {
		if rule.Ref == ref {
			return i
		}
	}
	return -1
}

// syncRule makes phase hold desired (or, when desired is nil, not hold our rule) and returns our
// rule's ID and a problem, if any.
func (r *CloudflareHostPolicyReconciler) syncRule(ctx context.Context, token, zoneID, phase, ref string, desired *cfRule, hosts []string) (string, string) {
	log := logf.FromContext(ctx)
	permission := cfPhasePermission(phase)
	ruleset, exists, err := r.entrypoint(ctx, token, zoneID, phase)
	if err != nil {
		return "", cfErrorMessage("read the "+phase+" rules", err, permission)
	}
	ours := -1
	if exists {
		ours = cfFindRef(ruleset.Rules, ref)
	}
	if desired == nil {
		if ours < 0 {
			return "", ""
		}
		id := ruleset.Rules[ours].ID
		if err := r.deleteRule(ctx, token, zoneID, ruleset.ID, id); err != nil {
			return id, cfErrorMessage("delete our "+phase+" rule", err, permission)
		}
		log.Info("host policy rule deleted", "zone", zoneID, "phase", phase, "ref", ref)
		return "", ""
	}

	var out cfRulesetResponse
	switch {
	case !exists:
		// The zone has no rules in this phase: the entrypoint is created holding only ours. An
		// existing entrypoint is never PUT, which would replace every rule in it.
		body := cfRuleset{Name: "default", Kind: "zone", Phase: phase, Rules: []cfRule{*desired}}
		if err := cfDo(ctx, token, http.MethodPut, fmt.Sprintf("%s/zones/%s/rulesets/phases/%s/entrypoint", r.api(), zoneID, phase), body, &out); err != nil {
			return "", cfErrorMessage("create the "+phase+" entrypoint", err, permission)
		}
		log.Info("host policy entrypoint created", "zone", zoneID, "phase", phase, "ref", ref)
	case ours < 0:
		rule := *desired
		if phase == cfPhaseRedirect {
			rule.Position = &cfRulePosition{Index: 1}
		}
		if err := cfDo(ctx, token, http.MethodPost, fmt.Sprintf("%s/zones/%s/rulesets/%s/rules", r.api(), zoneID, ruleset.ID), rule, &out); err != nil {
			return "", cfErrorMessage("add our "+phase+" rule", err, permission)
		}
		log.Info("host policy rule added", "zone", zoneID, "phase", phase, "ref", ref)
	case !cfRuleEqual(ruleset.Rules[ours], *desired):
		id := ruleset.Rules[ours].ID
		if err := cfDo(ctx, token, http.MethodPatch, fmt.Sprintf("%s/zones/%s/rulesets/%s/rules/%s", r.api(), zoneID, ruleset.ID, id), *desired, &out); err != nil {
			return id, cfErrorMessage("update our "+phase+" rule", err, permission)
		}
		log.Info("host policy rule updated", "zone", zoneID, "phase", phase, "ref", ref)
	default:
		out.Result = ruleset
	}
	ours = cfFindRef(out.Result.Rules, ref)
	if ours < 0 {
		return "", "Cloudflare did not return our " + phase + " rule"
	}
	id := out.Result.Rules[ours].ID
	if other, host, found := cfOverridingRule(phase, out.Result.Rules, ours, hosts); found {
		return id, fmt.Sprintf("rule %s (%q) in %s may match %s and override ours; it is left as it is", other.ID, other.Description, phase, host)
	}
	return id, ""
}

func (r *CloudflareHostPolicyReconciler) deleteRule(ctx context.Context, token, zoneID, rulesetID, ruleID string) error {
	err := cfDo(ctx, token, http.MethodDelete, fmt.Sprintf("%s/zones/%s/rulesets/%s/rules/%s", r.api(), zoneID, rulesetID, ruleID), nil, nil)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// deleteZoneRules deletes our rules in both phases of one zone, each only while its ref is ours.
func (r *CloudflareHostPolicyReconciler) deleteZoneRules(ctx context.Context, token, prefix, zoneID string) error {
	for _, phase := range []string{cfPhaseConfig, cfPhaseRedirect} {
		ref := hostPolicyRule(phase, prefix, nil).Ref
		if _, problem := r.syncRule(ctx, token, zoneID, phase, ref, nil, nil); problem != "" {
			return errors.New(problem)
		}
	}
	return nil
}

// cleanup deletes the policy's rules in every zone it recorded, and in the zones of its current
// hosts (a rule may exist whose status write was lost).
func (r *CloudflareHostPolicyReconciler) cleanup(ctx context.Context, policy *oramav1alpha1.CloudflareHostPolicy) error {
	zones := map[string]bool{}
	for _, zone := range policy.Status.Zones {
		zones[zone.ZoneID] = true
	}
	token, problem := r.token(ctx, policy)
	if problem != "" {
		if len(zones) == 0 {
			// Nothing was recorded, and nothing can be looked up: no rule is known to exist.
			return nil
		}
		return errors.New(problem)
	}
	finder := &cfZoneFinder{api: r.api(), token: token, accountID: policy.Spec.AccountID, cache: map[string]*cfZone{}}
	for _, host := range policy.Spec.Hosts {
		if zone, found, err := finder.find(ctx, host); err == nil && found {
			zones[zone.ID] = true
		}
	}
	ids := make([]string, 0, len(zones))
	for id := range zones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, zoneID := range ids {
		if err := r.deleteZoneRules(ctx, token, policy.Spec.RuleRefPrefix, zoneID); err != nil {
			return fmt.Errorf("zone %s: %w", zoneID, err)
		}
	}
	return nil
}

func (r *CloudflareHostPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareHostPolicy{}).Named("cloudflare-host-policy").Complete(r)
}
