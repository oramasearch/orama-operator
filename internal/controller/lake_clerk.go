/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// A Lake's Clerk instance (operator#541, docs/lake/clerk.md).
//
// Lake.spec.clerk names a Secret holding one Clerk instance's Backend API secret key. With it the
// operator keeps the instance configured for lake, through Clerk's Backend API and the Svix
// application Clerk delivers webhooks with, and gives the Lake what it reads:
//
//   - the `lake` JWT template lake's browser sign-in asks for (lake docs/api/clerk.md);
//   - the Lake's host (and apex) among the instance's allowed origins, added, never removed,
//     because several Lakes may share an instance;
//   - a webhook endpoint at https://<host>/identity/clerk-webhook for the user events, its
//     signing secret given to the Lake as LAKE_CLERK_WEBHOOK_SECRET (lake#765);
//   - LAKE_CLERK_PUBLISHABLE_KEY, derived from the instance's Frontend API host;
//   - on a production instance, the DNS records Clerk asks for, as CloudflareDNSRecords.
//
// Every step reads the current state first and changes only what differs. Clerk's Platform API
// (applications, instances, sign-up mode) is a private beta the Orama workspace has no access
// to, so applications and instances are created once in the Clerk dashboard, with invite-only
// sign-up and verified email sign-in; docs/lake/clerk.md lists those steps.
//
// A Lake without spec.clerk is not touched: no request to Clerk, no new object, no new env var.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// Clerk's Backend API (https://clerk.com/docs/reference/backend-api, spec
	// https://clerk.com/docs/reference/spec/bapi/latest.json, version 2026-08-20).
	clerkBackendAPIBase = "https://api.clerk.com/v1"

	lakeClerkWebhookPath   = "/identity/clerk-webhook"
	lakeClerkTemplateName  = "lake"
	lakeClerkTokenSeconds  = 300
	lakeClerkConditionType = "ClerkReady"
	lakeClerkPurpose       = "clerk"

	// A configured instance is checked again this often, so a setting changed in the dashboard is
	// put back; a failed pass is retried sooner.
	lakeClerkRecheck = time.Hour
	lakeClerkRetry   = time.Minute
)

// The Clerk events lake handles at /identity/clerk-webhook (lake#765).
var lakeClerkWebhookEvents = []string{"session.revoked", "user.created", "user.deleted", "user.updated"}

// lakeClerkTemplateClaims is the `lake` JWT template lake verifies (lake docs/api/clerk.md).
func lakeClerkTemplateClaims() map[string]any {
	return map[string]any{
		"aud":   "lake",
		"email": "{{user.primary_email_address}}",
		"name":  "{{user.full_name}}",
	}
}

// lakeClerkSecretName is the Lake-owned Secret holding what the operator derived and fetched:
// publishableKey, issuer and webhookSecret.
func lakeClerkSecretName(name string) string { return "lake-" + name + "-clerk" }

// lakeClerkClock remembers, per Lake, the last configuration pass and what it was for, so a
// steady Lake makes no request to Clerk between rechecks.
type lakeClerkClock struct {
	mu   sync.Mutex
	last map[types.UID]lakeClerkPass
}

type lakeClerkPass struct {
	digest string
	at     time.Time
	ok     bool
}

func (c *lakeClerkClock) due(uid types.UID, digest string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pass, found := c.last[uid]
	if !found || pass.digest != digest {
		return true
	}
	if pass.ok {
		return now.Sub(pass.at) >= lakeClerkRecheck
	}
	return now.Sub(pass.at) >= lakeClerkRetry
}

func (c *lakeClerkClock) record(uid types.UID, digest string, now time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.UID]lakeClerkPass{}
	}
	c.last[uid] = lakeClerkPass{digest: digest, at: now, ok: ok}
}

// lakeClerkState is what the Deployment and status need from a Lake's Clerk instance.
type lakeClerkState struct {
	PublishableKey string
	WebhookSecret  bool // the Lake-owned Secret holds webhookSecret
	// Condition is ClerkReady; nil when the Lake has no spec.clerk.
	Condition *metav1.Condition
}

func (r *LakeReconciler) clerkAPI() string {
	if r.ClerkAPIBase != "" {
		return strings.TrimSuffix(r.ClerkAPIBase, "/")
	}
	return clerkBackendAPIBase
}

// lakeClerkOrigins are the origins the instance must allow: the Lake's host and its apex.
func lakeClerkOrigins(lake *oramav1alpha1.Lake) []string {
	var origins []string
	for _, host := range []string{lake.Spec.Host, lakeApexHost(lake)} {
		if host != "" {
			origins = append(origins, "https://"+host)
		}
	}
	return origins
}

func lakeClerkWebhookURL(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Host == "" {
		return ""
	}
	return "https://" + lake.Spec.Host + lakeClerkWebhookPath
}

// reconcileLakeClerk keeps the Lake's Clerk instance configured and returns what the Lake reads
// from it. A Clerk failure never fails the Lake: it is reported in condition ClerkReady and the
// Lake keeps the keys it already had.
func (r *LakeReconciler) reconcileLakeClerk(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) (lakeClerkState, error) {
	if lake.Spec.Clerk == nil {
		// Removing spec.clerk removes what it made. On a Lake that never had it these find
		// nothing and write nothing.
		if err := deleteIfExists(ctx, r.Client, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeClerkSecretName(lake.Name), Namespace: ns}}); err != nil {
			return lakeClerkState{}, err
		}
		return lakeClerkState{}, r.pruneLakeClerkDNS(ctx, lake, ns, nil)
	}
	condition := metav1.Condition{
		Type: lakeClerkConditionType, Status: metav1.ConditionTrue, Reason: "Configured",
		ObservedGeneration: lake.Generation,
	}
	state := lakeClerkState{Condition: &condition}
	fail := func(reason, message string) {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, reason, message
	}

	secretKey, publishableOverride, err := r.lakeClerkSecretKey(ctx, lake, ns)
	if err != nil {
		return state, err
	}
	switch {
	case secretKey == "":
		fail("SecretKeyMissing", fmt.Sprintf("Secret %s/%s has no secretKey", ns, lake.Spec.Clerk.SecretKeySecretRef.Name))
	default:
		digest := lakeClerkDigest(lake, secretKey, publishableOverride)
		now := time.Now()
		if r.clerkClock.due(lake.UID, digest, now) {
			result, err := r.configureLakeClerk(ctx, lake, ns, labels, secretKey, publishableOverride)
			r.clerkClock.record(lake.UID, digest, now, err == nil)
			if err != nil {
				fail("ClerkAPIError", err.Error())
			} else {
				condition.Message = result
			}
		} else if previous := lakeClerkPreviousCondition(lake); previous != nil {
			condition.Status, condition.Reason, condition.Message = previous.Status, previous.Reason, previous.Message
		}
	}

	var owned corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeClerkSecretName(lake.Name)}, &owned); err != nil && !apierrors.IsNotFound(err) {
		return state, err
	}
	state.PublishableKey = string(owned.Data["publishableKey"])
	state.WebhookSecret = len(owned.Data["webhookSecret"]) > 0
	if condition.Status == metav1.ConditionTrue && condition.Message == "" {
		condition.Message = "the Clerk instance is configured"
	}
	return state, nil
}

func lakeClerkPreviousCondition(lake *oramav1alpha1.Lake) *metav1.Condition {
	for i := range lake.Status.Conditions {
		if lake.Status.Conditions[i].Type == lakeClerkConditionType {
			return &lake.Status.Conditions[i]
		}
	}
	return nil
}

// lakeClerkDigest changes whenever what a pass would ask Clerk for changes, so the next
// reconcile reconfigures at once instead of waiting for the recheck.
func lakeClerkDigest(lake *oramav1alpha1.Lake, secretKey, publishableOverride string) string {
	dns := ""
	if lake.Spec.DNSSecretRef != nil {
		dns = lake.Spec.DNSSecretRef.Name
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		secretKey, publishableOverride, lake.Spec.Host, lakeApexHost(lake), dns,
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func (r *LakeReconciler) lakeClerkSecretKey(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (string, string, error) {
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lake.Spec.Clerk.SecretKeySecretRef.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(secret.Data["secretKey"])), strings.TrimSpace(string(secret.Data["publishableKey"])), nil
}

// configureLakeClerk is one pass over the instance. It returns a one-line summary for the
// condition.
func (r *LakeReconciler) configureLakeClerk(
	ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string, secretKey, publishableOverride string,
) (string, error) {
	api := r.clerkAPI()

	// GET /v1/instance: the instance's environment type and allowed origins.
	var instance clerkBAPIInstance
	if err := cfDo(ctx, secretKey, http.MethodGet, api+"/instance", nil, &instance); err != nil {
		return "", fmt.Errorf("reading the instance: %w", err)
	}
	// GET /v1/domains: the primary domain's Frontend API URL and, on production, its DNS targets.
	var domains clerkBAPIDomains
	if err := cfDo(ctx, secretKey, http.MethodGet, api+"/domains", nil, &domains); err != nil {
		return "", fmt.Errorf("reading the instance's domains: %w", err)
	}
	primary := domains.primary()
	if primary == nil {
		return "", errors.New("the instance reports no primary domain")
	}
	publishable := publishableOverride
	if publishable == "" {
		publishable = clerkPublishableKey(instance.EnvironmentType, primary.FrontendAPIURL)
	}

	if err := clerkEnsureJWTTemplate(ctx, api, secretKey); err != nil {
		return "", err
	}
	if err := clerkEnsureAllowedOrigins(ctx, api, secretKey, instance.AllowedOrigins, lakeClerkOrigins(lake)); err != nil {
		return "", err
	}
	webhookSecret, endpointID := "", ""
	if hook := lakeClerkWebhookURL(lake); hook != "" {
		var err error
		if webhookSecret, endpointID, err = clerkEnsureWebhook(ctx, api, secretKey, r.SvixAPIBase, hook, "lake "+lake.Name); err != nil {
			return "", err
		}
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeClerkSecretName(lake.Name), Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["publishableKey"] = []byte(publishable)
		secret.Data["issuer"] = []byte(strings.TrimSuffix(primary.FrontendAPIURL, "/"))
		if webhookSecret != "" {
			secret.Data["webhookSecret"] = []byte(webhookSecret)
		} else {
			delete(secret.Data, "webhookSecret")
		}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	}); err != nil {
		return "", err
	}

	summary := fmt.Sprintf("%s instance on %s", instance.EnvironmentType, primary.Name)
	if endpointID != "" {
		summary += ", webhook " + endpointID
	}
	if instance.EnvironmentType != "production" {
		return summary, r.pruneLakeClerkDNS(ctx, lake, ns, nil)
	}
	written, manual, err := r.reconcileLakeClerkDNS(ctx, lake, ns, labels, primary.targets())
	if err != nil {
		return "", err
	}
	summary += fmt.Sprintf(", %d DNS record(s)", written)
	if len(manual) > 0 {
		return "", fmt.Errorf("%s; Clerk says these DNS records need a person: %s", summary, strings.Join(manual, ", "))
	}
	return summary, nil
}

// ── Lake-owned DNS records for a production instance ─────────────────────────

func lakeClerkDNSRecordName(lake *oramav1alpha1.Lake, target clerkDNSTarget) string {
	sum := sha256.Sum256([]byte(target.RecordType + " " + target.Host))
	return fmt.Sprintf("lake-%s-clerk-%x", lake.Name, sum[:4])
}

// reconcileLakeClerkDNS writes each DNS record Clerk asks for as a CloudflareDNSRecord with the
// Lake's dnsSecretRef: not proxied (Clerk must see its own targets) and Retain, because every
// Lake sharing the instance writes the same records and deleting one Lake must not take them
// from the others. It returns how many it wrote and those Clerk says a person must resolve.
func (r *LakeReconciler) reconcileLakeClerkDNS(
	ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string, targets []clerkDNSTarget,
) (int, []string, error) {
	if lake.Spec.DNSSecretRef == nil || lake.Spec.DNSSecretRef.Name == "" {
		if len(targets) == 0 {
			return 0, nil, nil
		}
		return 0, nil, errors.New("the production instance needs DNS records and spec.dnsSecretRef is not set")
	}
	keep := map[string]bool{}
	var manual []string
	for _, target := range targets {
		if target.AutomationDisposition == "manual_resolution_required" {
			manual = append(manual, target.RecordType+" "+target.Host)
			continue
		}
		name := lakeClerkDNSRecordName(lake, target)
		keep[name] = true
		record := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, record, func() error {
			record.Labels = withLabels(labels, map[string]string{"orama.cloud/dns-purpose": lakeClerkPurpose})
			record.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
			record.Spec.Name = strings.TrimSuffix(target.Host, ".")
			record.Spec.Type = target.RecordType
			record.Spec.Content = target.Value
			record.Spec.Proxied = false
			record.Spec.DeletionPolicy = "Retain"
			return controllerutil.SetControllerReference(lake, record, r.Scheme)
		}); err != nil {
			return 0, nil, err
		}
	}
	return len(keep), manual, r.pruneLakeClerkDNS(ctx, lake, ns, keep)
}

// pruneLakeClerkDNS deletes the Lake's Clerk DNS records not in keep. Their policy is Retain, so
// Cloudflare keeps the record; only the resource goes.
func (r *LakeReconciler) pruneLakeClerkDNS(ctx context.Context, lake *oramav1alpha1.Lake, ns string, keep map[string]bool) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := r.List(ctx, &records, client.InNamespace(ns), client.MatchingLabels{
		"app.kubernetes.io/instance": lake.Name,
		"orama.cloud/dns-purpose":    lakeClerkPurpose,
	}); err != nil {
		return err
	}
	for i := range records.Items {
		if keep[records.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &records.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ── Clerk Backend API ─────────────────────────────────────────────────────────
// Every path and payload below is from Clerk's Backend API spec, version 2026-08-20
// (https://clerk.com/docs/reference/spec/bapi/latest.json), authenticated with the instance's
// secret key as a bearer token.

// clerkBAPIInstance is GET /v1/instance (operationId GetInstance, schema Instance).
type clerkBAPIInstance struct {
	ID              string   `json:"id"`
	EnvironmentType string   `json:"environment_type"`
	AllowedOrigins  []string `json:"allowed_origins"`
}

// clerkBAPIDomains is GET /v1/domains (operationId ListDomains, schema Domains).
type clerkBAPIDomains struct {
	Data []clerkBAPIDomain `json:"data"`
}

type clerkBAPIDomain struct {
	Name           string           `json:"name"`
	IsSatellite    bool             `json:"is_satellite"`
	FrontendAPIURL string           `json:"frontend_api_url"`
	DNSTargets     []clerkDNSTarget `json:"dns_targets"`
	CNAMETargets   []clerkDNSTarget `json:"cname_targets"`
}

// clerkDNSTarget is schema DNSTarget (and the legacy CNameTarget, which has no record_type).
type clerkDNSTarget struct {
	Host                  string `json:"host"`
	Value                 string `json:"value"`
	RecordType            string `json:"record_type"`
	AutomationDisposition string `json:"automation_disposition"`
	Required              bool   `json:"required"`
}

func (d clerkBAPIDomains) primary() *clerkBAPIDomain {
	for i := range d.Data {
		if !d.Data[i].IsSatellite {
			return &d.Data[i]
		}
	}
	return nil
}

// targets are the domain's DNS records: dns_targets, which the spec says to use alone when
// present, else the legacy CNAME-only cname_targets.
func (d clerkBAPIDomain) targets() []clerkDNSTarget {
	if d.DNSTargets != nil {
		return d.DNSTargets
	}
	out := make([]clerkDNSTarget, 0, len(d.CNAMETargets))
	for _, target := range d.CNAMETargets {
		target.RecordType = "CNAME"
		out = append(out, target)
	}
	return out
}

// clerkPublishableKey derives an instance's publishable key from its Frontend API URL: pk_live_
// (production) or pk_test_, then base64 of the host and a trailing "$". lake parses it the same
// way (lake include/lake/api/clerk.hpp) and issuerFromPublishableKey reverses it.
func clerkPublishableKey(environmentType, frontendAPIURL string) string {
	host := frontendAPIURL
	if parsed, err := url.Parse(frontendAPIURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	prefix := "pk_test_"
	if environmentType == "production" {
		prefix = "pk_live_"
	}
	return prefix + base64.StdEncoding.EncodeToString([]byte(host+"$"))
}

// clerkBAPIJWTTemplate is schema JWTTemplate.
type clerkBAPIJWTTemplate struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Claims   map[string]any `json:"claims"`
	Lifetime int64          `json:"lifetime"`
}

// clerkEnsureJWTTemplate keeps the `lake` template: GET /v1/jwt_templates (ListJWTTemplates),
// then POST /v1/jwt_templates (CreateJWTTemplate) when it is missing or PATCH
// /v1/jwt_templates/{template_id} (UpdateJWTTemplate) when its claims or lifetime differ.
func clerkEnsureJWTTemplate(ctx context.Context, api, secretKey string) error {
	var raw json.RawMessage
	if err := cfDo(ctx, secretKey, http.MethodGet, api+"/jwt_templates?limit=500", nil, &raw); err != nil {
		return fmt.Errorf("listing JWT templates: %w", err)
	}
	// Without paginated=true the response is an array; with it, {data, total_count}.
	var templates []clerkBAPIJWTTemplate
	if err := json.Unmarshal(raw, &templates); err != nil {
		var page struct {
			Data []clerkBAPIJWTTemplate `json:"data"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("reading JWT templates: %w", err)
		}
		templates = page.Data
	}
	want := map[string]any{"name": lakeClerkTemplateName, "claims": lakeClerkTemplateClaims(), "lifetime": lakeClerkTokenSeconds}
	for _, template := range templates {
		if template.Name != lakeClerkTemplateName {
			continue
		}
		if reflect.DeepEqual(template.Claims, lakeClerkTemplateClaims()) && template.Lifetime == lakeClerkTokenSeconds {
			return nil
		}
		if err := cfDo(ctx, secretKey, http.MethodPatch, api+"/jwt_templates/"+url.PathEscape(template.ID), want, nil); err != nil {
			return fmt.Errorf("updating JWT template %s: %w", lakeClerkTemplateName, err)
		}
		return nil
	}
	if err := cfDo(ctx, secretKey, http.MethodPost, api+"/jwt_templates", want, nil); err != nil {
		return fmt.Errorf("creating JWT template %s: %w", lakeClerkTemplateName, err)
	}
	return nil
}

// clerkEnsureAllowedOrigins adds the missing origins with PATCH /v1/instance (UpdateInstance,
// allowed_origins replaces the list, so the current one is sent with them). Origins already
// there stay: another Lake on the same instance may need them.
func clerkEnsureAllowedOrigins(ctx context.Context, api, secretKey string, current, want []string) error {
	have := map[string]bool{}
	for _, origin := range current {
		have[origin] = true
	}
	merged := append([]string{}, current...)
	for _, origin := range want {
		if !have[origin] {
			have[origin] = true
			merged = append(merged, origin)
		}
	}
	if len(merged) == len(current) {
		return nil
	}
	if err := cfDo(ctx, secretKey, http.MethodPatch, api+"/instance", map[string]any{"allowed_origins": merged}, nil); err != nil {
		return fmt.Errorf("adding allowed origins: %w", err)
	}
	return nil
}

// ── Webhooks: Clerk's Svix application ───────────────────────────────────────
// Clerk delivers webhooks through Svix (https://clerk.com/docs/guides/development/webhooks/overview).
// The Backend API manages only the instance's Svix application: POST /v1/webhooks/svix_url
// (GenerateSvixAuthURL) returns an App Portal link for it, and POST /v1/webhooks/svix
// (CreateSvixApp) creates it first when the instance has none. The link carries an App Portal
// token scoped to that one application (https://docs.svix.com/app-portal), which is what Clerk's
// dashboard uses to manage endpoints through Svix's API (https://api.svix.com/docs, Endpoint):
// GET/POST /api/v1/app/{app_id}/endpoint, PATCH /api/v1/app/{app_id}/endpoint/{endpoint_id},
// GET /api/v1/app/{app_id}/endpoint/{endpoint_id}/secret.

type svixPortalKey struct {
	AppID     string `json:"appId"`
	Token     string `json:"token"`
	ServerURL string `json:"serverUrl"`
}

type svixEndpoint struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	Disabled   bool     `json:"disabled"`
}

// clerkSvixPortal returns the instance's Svix App Portal link, creating the Svix application
// when the instance has none.
func clerkSvixPortal(ctx context.Context, api, secretKey string) (string, error) {
	var link struct {
		SvixURL string `json:"svix_url"`
	}
	err := cfDo(ctx, secretKey, http.MethodPost, api+"/webhooks/svix_url", nil, &link)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
		// No Svix application yet: create it, which also returns the link.
		err = cfDo(ctx, secretKey, http.MethodPost, api+"/webhooks/svix", nil, &link)
	}
	if err != nil {
		return "", fmt.Errorf("opening the instance's webhooks: %w", err)
	}
	return link.SvixURL, nil
}

// parseSvixPortalURL reads the App Portal key from a link's fragment, #key=<base64 JSON>.
func parseSvixPortalURL(link string) (svixPortalKey, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return svixPortalKey{}, err
	}
	values, _ := url.ParseQuery(parsed.Fragment)
	encoded := values.Get("key")
	if encoded == "" {
		return svixPortalKey{}, errors.New("the webhook portal link has no key")
	}
	var data []byte
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err = encoding.DecodeString(encoded); err == nil {
			break
		}
	}
	if err != nil {
		return svixPortalKey{}, fmt.Errorf("the webhook portal key is not base64: %w", err)
	}
	var key svixPortalKey
	if err := json.Unmarshal(data, &key); err != nil {
		return svixPortalKey{}, fmt.Errorf("the webhook portal key is not JSON: %w", err)
	}
	if key.AppID == "" || key.Token == "" {
		return svixPortalKey{}, errors.New("the webhook portal key has no application or token")
	}
	return key, nil
}

// svixAPIBase is where a token's application lives: the key's server URL, else the region its
// token ends with (the Svix SDKs' rule), else the default.
func svixAPIBase(key svixPortalKey) string {
	if key.ServerURL != "" {
		return strings.TrimSuffix(key.ServerURL, "/")
	}
	parts := strings.Split(key.Token, ".")
	switch parts[len(parts)-1] {
	case "us", "eu", "in", "ca", "au":
		return "https://api." + parts[len(parts)-1] + ".svix.com"
	}
	return "https://api.svix.com"
}

func sameEvents(a, b []string) bool {
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

// clerkEnsureWebhook keeps one endpoint at hookURL for lake's events, adopting one already at
// that URL, and returns its signing secret and id. svixOverride replaces the Svix API's address
// (tests).
func clerkEnsureWebhook(ctx context.Context, api, secretKey, svixOverride, hookURL, description string) (string, string, error) {
	link, err := clerkSvixPortal(ctx, api, secretKey)
	if err != nil {
		return "", "", err
	}
	key, err := parseSvixPortalURL(link)
	if err != nil {
		return "", "", err
	}
	base := svixAPIBase(key)
	if svixOverride != "" {
		base = strings.TrimSuffix(svixOverride, "/")
	}
	endpoints := base + "/api/v1/app/" + url.PathEscape(key.AppID) + "/endpoint"

	var list struct {
		Data     []svixEndpoint `json:"data"`
		Done     bool           `json:"done"`
		Iterator *string        `json:"iterator"`
	}
	var found *svixEndpoint
	next := endpoints + "?limit=250"
	for {
		list.Data, list.Iterator = nil, nil
		if err := cfDo(ctx, key.Token, http.MethodGet, next, nil, &list); err != nil {
			return "", "", fmt.Errorf("listing webhook endpoints: %w", err)
		}
		for i := range list.Data {
			if list.Data[i].URL == hookURL {
				found = &list.Data[i]
				break
			}
		}
		if found != nil || list.Done || list.Iterator == nil || *list.Iterator == "" {
			break
		}
		next = endpoints + "?limit=250&iterator=" + url.QueryEscape(*list.Iterator)
	}

	events := append([]string{}, lakeClerkWebhookEvents...)
	switch {
	case found == nil:
		var created svixEndpoint
		body := map[string]any{"url": hookURL, "description": description, "eventTypes": events}
		if err := cfDo(ctx, key.Token, http.MethodPost, endpoints, body, &created); err != nil {
			return "", "", fmt.Errorf("creating the webhook endpoint: %w", err)
		}
		found = &created
	case !sameEvents(found.EventTypes, events) || found.Disabled:
		body := map[string]any{"eventTypes": events, "disabled": false}
		if err := cfDo(ctx, key.Token, http.MethodPatch, endpoints+"/"+url.PathEscape(found.ID), body, nil); err != nil {
			return "", "", fmt.Errorf("updating the webhook endpoint: %w", err)
		}
	}
	var secret struct {
		Key string `json:"key"`
	}
	if err := cfDo(ctx, key.Token, http.MethodGet, endpoints+"/"+url.PathEscape(found.ID)+"/secret", nil, &secret); err != nil {
		return "", "", fmt.Errorf("reading the webhook signing secret: %w", err)
	}
	if secret.Key == "" {
		return "", "", errors.New("the webhook endpoint has no signing secret")
	}
	return secret.Key, found.ID, nil
}

// lakeClerkEnv is the Lake's Clerk variables. Both names are reserved whenever spec.clerk is
// set (lakeEnv drops them from spec.env); each is given once the operator has its value.
func lakeClerkEnv(lake *oramav1alpha1.Lake, state lakeClerkState) []corev1.EnvVar {
	if lake.Spec.Clerk == nil {
		return nil
	}
	var env []corev1.EnvVar
	if state.PublishableKey != "" {
		env = append(env, corev1.EnvVar{Name: "LAKE_CLERK_PUBLISHABLE_KEY", Value: state.PublishableKey})
	}
	if state.WebhookSecret {
		env = append(env, corev1.EnvVar{Name: "LAKE_CLERK_WEBHOOK_SECRET", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: lakeClerkSecretName(lake.Name)},
				Key:                  "webhookSecret",
			},
		}})
	}
	return env
}

var lakeClerkEnvNames = []string{"LAKE_CLERK_PUBLISHABLE_KEY", "LAKE_CLERK_WEBHOOK_SECRET"}
