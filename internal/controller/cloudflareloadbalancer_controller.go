/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cfLBFinalizer tears the Cloudflare Load Balancer, its Pools, and its Monitor down (in that order)
// before the CR is released, so deleting the owning OramaCloudV1 cascades to real external cleanup.
const cfLBFinalizer = "orama.cloud/cloudflare-lb-cleanup"

// cfLBRequeue is the steady-state resync — long enough that a healthy LB is quiet, short enough to
// keep the reflected per-pool health reasonably fresh. Status is still written ONLY on a real change
// (anti-flap, principle #7), so a steady LB produces zero control-plane WRITE traffic.
const cfLBRequeue = 5 * time.Minute

// CloudflareLoadBalancerReconciler owns a Cloudflare global Load Balancer as a reconciled CRD
// (principle #3): it creates/updates a Monitor + Pools + Load Balancer via the CF Load Balancing API
// and holds a finalizer for external cleanup, so the operator never mutates Cloudflare from the main
// loop. Same "external resource as an owned CRD" shape as CloudflareDNSRecord (epic #31 / #73).
type CloudflareLoadBalancerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflareloadbalancers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflareloadbalancers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflareloadbalancers/finalizers,verbs=update

func (r *CloudflareLoadBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lb oramav1alpha1.CloudflareLoadBalancer
	if err := r.Get(ctx, req.NamespacedName, &lb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, zoneID, accountID, err := cfCredsFromSecret(ctx, r.Client, lb.Namespace, lb.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Deletion: run the finalizer (delete LB → pools → monitor from Cloudflare), then release.
	if !lb.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&lb, cfLBFinalizer) {
			if token != "" && zoneID != "" && accountID != "" {
				if err := r.teardown(ctx, token, zoneID, accountID, &lb); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&lb, cfLBFinalizer)
			if err := r.Update(ctx, &lb); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Register the finalizer before creating any external state.
	if !controllerutil.ContainsFinalizer(&lb, cfLBFinalizer) {
		controllerutil.AddFinalizer(&lb, cfLBFinalizer)
		if err := r.Update(ctx, &lb); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Safe no-op until the Cloudflare secret is present (token + zoneID + accountID). Surface it as a
	// clear condition rather than an error so the loop stays green and doesn't back off aggressively.
	if token == "" || zoneID == "" || accountID == "" {
		r.setNotReady(ctx, &lb, "WaitingForCredentials",
			"waiting for the Cloudflare secret ("+lb.Spec.SecretRef+") with token+zoneID+accountID")
		return ctrl.Result{}, nil
	}

	// Reconcile the external objects. On a Load-Balancing-not-enabled / entitlement error, surface a
	// clear condition and requeue gently — never crash-loop (principle #7 anti-flap; graceful CF errors).
	desired, err := r.reconcileExternal(ctx, token, zoneID, accountID, &lb)
	if err != nil {
		if isCloudflareLBUnavailable(err) {
			logf.FromContext(ctx).Info("Cloudflare Load Balancing is not enabled on this account — "+
				"enable it in the dashboard (Delivery & performance → Load Balancing), then reconcile will proceed",
				"cloudflareloadbalancer", req.NamespacedName.String())
			r.setNotReady(ctx, &lb, "LoadBalancingNotEnabled",
				"Cloudflare Load Balancing is a paid add-on that must be enabled on the account first: "+err.Error())
			return ctrl.Result{RequeueAfter: cfLBRequeue}, nil
		}
		r.setNotReady(ctx, &lb, "ReconcileError", err.Error())
		return ctrl.Result{}, err
	}

	r.setReady(ctx, &lb, desired)
	return ctrl.Result{RequeueAfter: cfLBRequeue}, nil
}

func (r *CloudflareLoadBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareLoadBalancer{}).Complete(r)
}

// reconcileExternal makes the CF API idempotent: ensure the Monitor, ensure each Pool (adopting or
// creating, then PATCHing only the fields we manage so server-managed fields are not clobbered),
// ensure the Load Balancer, prune pools that left the spec, and read back per-pool health. Returns
// the computed status so the caller can apply it atomically under the anti-flap guard.
func (r *CloudflareLoadBalancerReconciler) reconcileExternal(ctx context.Context, token, zoneID, accountID string, lb *oramav1alpha1.CloudflareLoadBalancer) (oramav1alpha1.CloudflareLoadBalancerStatus, error) {
	tag := cfLBTag(lb)
	status := oramav1alpha1.CloudflareLoadBalancerStatus{ObservedGeneration: lb.Generation}

	// 1) Monitor (account-scoped). Adopt the existing one by our description tag if not yet recorded.
	monitorID, err := cfEnsureMonitor(ctx, token, accountID, lb.Status.MonitorID, tag, &lb.Spec.Monitor)
	if err != nil {
		return status, err
	}
	status.MonitorID = monitorID

	// 2) Pools (account-scoped). Name → CF id, preserving previously-recorded ids for adoption/PATCH.
	prevPoolIDs := map[string]string{}
	for _, p := range lb.Status.Pools {
		if p.ID != "" {
			prevPoolIDs[p.Name] = p.ID
		}
	}
	poolIDByName := map[string]string{}
	for i := range lb.Spec.Pools {
		p := &lb.Spec.Pools[i]
		id, err := cfEnsurePool(ctx, token, accountID, prevPoolIDs[p.Name], cfPoolName(lb, p.Name), tag, monitorID, p)
		if err != nil {
			return status, err
		}
		poolIDByName[p.Name] = id
	}

	// 3) Load Balancer (zone-scoped). default_pools = spec order; fallback = named pool (or last).
	defaultPools := make([]string, 0, len(lb.Spec.Pools))
	for _, p := range lb.Spec.Pools {
		defaultPools = append(defaultPools, poolIDByName[p.Name])
	}
	fallback := ""
	if lb.Spec.FallbackPool != "" {
		fallback = poolIDByName[lb.Spec.FallbackPool]
	}
	if fallback == "" && len(defaultPools) > 0 {
		fallback = defaultPools[len(defaultPools)-1]
	}
	regionPools := map[string][]string{}
	for region, names := range lb.Spec.RegionPools {
		ids := make([]string, 0, len(names))
		for _, n := range names {
			if id := poolIDByName[n]; id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			regionPools[region] = ids
		}
	}
	lbID, err := cfEnsureLoadBalancer(ctx, token, zoneID, lb.Status.LoadBalancerID, tag, &lb.Spec, defaultPools, fallback, regionPools)
	if err != nil {
		return status, err
	}
	status.LoadBalancerID = lbID

	// 4) Prune pools that left the spec (safe now the LB no longer references them). Best-effort.
	for name, id := range prevPoolIDs {
		if _, keep := poolIDByName[name]; !keep {
			if err := cfDeletePool(ctx, token, accountID, id); err != nil && !isCloudflareNotFound(err) {
				logf.FromContext(ctx).Error(err, "failed to prune orphaned Cloudflare pool", "pool", name, "id", id)
			}
		}
	}

	// 5) Per-pool health (best-effort — a health read must never fail the reconcile).
	names := make([]string, 0, len(poolIDByName))
	for n := range poolIDByName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		id := poolIDByName[n]
		status.Pools = append(status.Pools, oramav1alpha1.LoadBalancerPoolStatus{
			Name: n, ID: id, Health: cfPoolHealth(ctx, token, accountID, id),
		})
	}
	return status, nil
}

// teardown deletes the Load Balancer, then the Pools, then the Monitor (dependency order). 404s are
// treated as success so a partially-created or already-deleted LB still releases the finalizer.
func (r *CloudflareLoadBalancerReconciler) teardown(ctx context.Context, token, zoneID, accountID string, lb *oramav1alpha1.CloudflareLoadBalancer) error {
	if id := lb.Status.LoadBalancerID; id != "" {
		if err := cfDeleteLoadBalancer(ctx, token, zoneID, id); err != nil && !isCloudflareNotFound(err) {
			return err
		}
	}
	for _, p := range lb.Status.Pools {
		if p.ID == "" {
			continue
		}
		if err := cfDeletePool(ctx, token, accountID, p.ID); err != nil && !isCloudflareNotFound(err) {
			return err
		}
	}
	if id := lb.Status.MonitorID; id != "" {
		if err := cfDeleteMonitor(ctx, token, accountID, id); err != nil && !isCloudflareNotFound(err) {
			return err
		}
	}
	return nil
}

// ---- status helpers (anti-flap: write the subresource ONLY on a real transition) ----

func (r *CloudflareLoadBalancerReconciler) setReady(ctx context.Context, lb *oramav1alpha1.CloudflareLoadBalancer, desired oramav1alpha1.CloudflareLoadBalancerStatus) {
	desired.Message = fmt.Sprintf("load balancer %s reconciled (%d pool(s))", lb.Spec.Hostname, len(desired.Pools))
	r.applyStatus(ctx, lb, desired, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: desired.Message,
	})
}

func (r *CloudflareLoadBalancerReconciler) setNotReady(ctx context.Context, lb *oramav1alpha1.CloudflareLoadBalancer, reason, msg string) {
	// Preserve any ids/pools already discovered; only the condition + message move.
	desired := oramav1alpha1.CloudflareLoadBalancerStatus{
		LoadBalancerID:     lb.Status.LoadBalancerID,
		MonitorID:          lb.Status.MonitorID,
		Pools:              lb.Status.Pools,
		ObservedGeneration: lb.Generation,
		Message:            msg,
	}
	r.applyStatus(ctx, lb, desired, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})
}

// applyStatus deep-compares the computed status (minus conditions) and merges the condition; it
// calls Status().Update ONLY when something actually changed, so a steady LB is silent.
func (r *CloudflareLoadBalancerReconciler) applyStatus(ctx context.Context, lb *oramav1alpha1.CloudflareLoadBalancer, desired oramav1alpha1.CloudflareLoadBalancerStatus, cond metav1.Condition) {
	cond.ObservedGeneration = lb.Generation
	changed := lb.Status.LoadBalancerID != desired.LoadBalancerID ||
		lb.Status.MonitorID != desired.MonitorID ||
		lb.Status.ObservedGeneration != desired.ObservedGeneration ||
		lb.Status.Message != desired.Message ||
		!reflect.DeepEqual(lb.Status.Pools, desired.Pools)
	// Carry existing conditions forward so SetStatusCondition can detect a real change.
	desired.Conditions = lb.Status.Conditions
	if apimeta.SetStatusCondition(&desired.Conditions, cond) {
		changed = true
	}
	if !changed {
		return
	}
	lb.Status = desired
	if err := r.Status().Update(ctx, lb); err != nil {
		logf.FromContext(ctx).Error(err, "status update failed", "cloudflareloadbalancer", lb.Name)
	}
}

// ---- Cloudflare Load Balancing API ----
//
// Monitors + Pools are account-scoped (/accounts/{account}/load_balancers/...); the Load Balancer is
// zone-scoped (/zones/{zone}/load_balancers). Reuses cfDo + cfAPIBase from cloudflare.go.

// cfLBTag is a stable description tag written onto the Monitor/Pool/LB so the operator can adopt
// objects it created before (e.g. after a status loss) instead of duplicating them.
func cfLBTag(lb *oramav1alpha1.CloudflareLoadBalancer) string {
	return "orama-operator:" + lb.Namespace + "/" + lb.Name
}

// cfPoolName is the Cloudflare pool name — namespaced by the CR so pools never collide across CRs in
// the same account.
func cfPoolName(lb *oramav1alpha1.CloudflareLoadBalancer, poolName string) string {
	return lb.Name + "-" + poolName
}

func cfMonitorBody(tag string, m *oramav1alpha1.HealthMonitor) map[string]interface{} {
	typ := strings.ToLower(m.Type)
	if typ == "" {
		typ = "https"
	}
	body := map[string]interface{}{
		"type":             typ,
		"description":      tag,
		"expected_codes":   orDefault(m.ExpectedCodes, "200"),
		"interval":         orDefaultInt(m.Interval, 60),
		"timeout":          orDefaultInt(m.Timeout, 5),
		"retries":          orDefaultInt(m.Retries, 2),
		"follow_redirects": m.FollowRedirects,
	}
	if typ == "http" || typ == "https" {
		body["method"] = orDefault(m.Method, "GET")
		body["path"] = orDefault(m.Path, "/v1/server_info")
	}
	if m.Port > 0 {
		body["port"] = m.Port
	}
	if m.ExpectedBody != "" {
		body["expected_body"] = m.ExpectedBody
	}
	return body
}

// cfEnsureMonitor creates (or adopts + PATCHes) the health monitor and returns its id.
func cfEnsureMonitor(ctx context.Context, token, accountID, knownID, tag string, m *oramav1alpha1.HealthMonitor) (string, error) {
	body := cfMonitorBody(tag, m)
	id := knownID
	if id == "" {
		// Adopt an existing monitor carrying our tag before creating a new one.
		listURL := fmt.Sprintf("%s/accounts/%s/load_balancers/monitors", cfAPIBase, accountID)
		var listResp struct {
			Result []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"result"`
		}
		if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
			return "", err
		}
		for _, mon := range listResp.Result {
			if mon.Description == tag {
				id = mon.ID
				break
			}
		}
	}
	if id != "" {
		// PATCH only the fields we manage — leaves server-managed / unspecified fields intact.
		url := fmt.Sprintf("%s/accounts/%s/load_balancers/monitors/%s", cfAPIBase, accountID, id)
		return id, cfDo(ctx, token, http.MethodPatch, url, body, nil)
	}
	createURL := fmt.Sprintf("%s/accounts/%s/load_balancers/monitors", cfAPIBase, accountID)
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, createURL, body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

func cfDeleteMonitor(ctx context.Context, token, accountID, id string) error {
	url := fmt.Sprintf("%s/accounts/%s/load_balancers/monitors/%s", cfAPIBase, accountID, id)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}

func cfPoolBody(cfName, tag, monitorID string, pool *oramav1alpha1.OriginPool) map[string]interface{} {
	origins := make([]map[string]interface{}, 0, len(pool.Origins))
	for _, o := range pool.Origins {
		origins = append(origins, map[string]interface{}{
			"name":    o.Name,
			"address": o.Address,
			"enabled": o.Enabled,
			"weight":  parseWeight(o.Weight),
		})
	}
	body := map[string]interface{}{
		"name":            cfName,
		"description":     tag,
		"enabled":         pool.Enabled,
		"minimum_origins": orDefaultInt(pool.MinimumOrigins, 1),
		"origins":         origins,
	}
	if pool.UseMonitor && monitorID != "" {
		body["monitor"] = monitorID
	}
	return body
}

// cfEnsurePool creates (or adopts + PATCHes) a pool and returns its id. Adoption is by pool name
// (unique per account). PATCH replaces the origins array (we own it) but leaves other pool fields —
// notification settings, check regions, etc. — untouched (merge, don't clobber).
func cfEnsurePool(ctx context.Context, token, accountID, knownID, cfName, tag, monitorID string, pool *oramav1alpha1.OriginPool) (string, error) {
	body := cfPoolBody(cfName, tag, monitorID, pool)
	id := knownID
	if id == "" {
		listURL := fmt.Sprintf("%s/accounts/%s/load_balancers/pools?per_page=100", cfAPIBase, accountID)
		var listResp struct {
			Result []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
			return "", err
		}
		for _, p := range listResp.Result {
			if p.Name == cfName {
				id = p.ID
				break
			}
		}
	}
	if id != "" {
		url := fmt.Sprintf("%s/accounts/%s/load_balancers/pools/%s", cfAPIBase, accountID, id)
		return id, cfDo(ctx, token, http.MethodPatch, url, body, nil)
	}
	createURL := fmt.Sprintf("%s/accounts/%s/load_balancers/pools", cfAPIBase, accountID)
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, createURL, body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

func cfDeletePool(ctx context.Context, token, accountID, id string) error {
	url := fmt.Sprintf("%s/accounts/%s/load_balancers/pools/%s", cfAPIBase, accountID, id)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}

// cfPoolHealth reads a pool's health endpoint (best-effort) and reduces it to healthy | unhealthy |
// unknown. A pool is "healthy" if any Cloudflare PoP reports it healthy; "unknown" if there is no
// data yet (freshly created, or the read failed). Never returns an error — health must not fail
// the reconcile.
func cfPoolHealth(ctx context.Context, token, accountID, id string) string {
	url := fmt.Sprintf("%s/accounts/%s/load_balancers/pools/%s/health", cfAPIBase, accountID, id)
	var resp struct {
		Result struct {
			PopHealth map[string]struct {
				Healthy bool `json:"healthy"`
			} `json:"pop_health"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, url, nil, &resp); err != nil {
		return "unknown"
	}
	if len(resp.Result.PopHealth) == 0 {
		return "unknown"
	}
	for _, pop := range resp.Result.PopHealth {
		if pop.Healthy {
			return "healthy"
		}
	}
	return "unhealthy"
}

func cfLoadBalancerBody(tag string, spec *oramav1alpha1.CloudflareLoadBalancerSpec, defaultPools []string, fallback string, regionPools map[string][]string) map[string]interface{} {
	body := map[string]interface{}{
		"name":            spec.Hostname,
		"description":     tag,
		"default_pools":   defaultPools,
		"fallback_pool":   fallback,
		"proxied":         spec.Proxied,
		"steering_policy": cfSteeringPolicy(spec.SteeringPolicy),
		"enabled":         true,
	}
	if spec.SessionAffinity != "" && spec.SessionAffinity != "none" {
		body["session_affinity"] = spec.SessionAffinity
	}
	if len(regionPools) > 0 {
		body["region_pools"] = regionPools
	}
	return body
}

// cfEnsureLoadBalancer creates (or adopts + PATCHes) the zone Load Balancer and returns its id.
// Adoption is by name (the hostname is unique per zone).
func cfEnsureLoadBalancer(ctx context.Context, token, zoneID, knownID, tag string, spec *oramav1alpha1.CloudflareLoadBalancerSpec, defaultPools []string, fallback string, regionPools map[string][]string) (string, error) {
	body := cfLoadBalancerBody(tag, spec, defaultPools, fallback, regionPools)
	id := knownID
	if id == "" {
		listURL := fmt.Sprintf("%s/zones/%s/load_balancers", cfAPIBase, zoneID)
		var listResp struct {
			Result []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
			return "", err
		}
		for _, l := range listResp.Result {
			if l.Name == spec.Hostname {
				id = l.ID
				break
			}
		}
	}
	if id != "" {
		url := fmt.Sprintf("%s/zones/%s/load_balancers/%s", cfAPIBase, zoneID, id)
		return id, cfDo(ctx, token, http.MethodPatch, url, body, nil)
	}
	createURL := fmt.Sprintf("%s/zones/%s/load_balancers", cfAPIBase, zoneID)
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, createURL, body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

func cfDeleteLoadBalancer(ctx context.Context, token, zoneID, id string) error {
	url := fmt.Sprintf("%s/zones/%s/load_balancers/%s", cfAPIBase, zoneID, id)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}

// cfSteeringPolicy maps the CRD's friendly steering value to the Cloudflare API enum. The only
// non-identity mapping is dynamic → dynamic_latency (Cloudflare's name for latency-based steering).
func cfSteeringPolicy(p string) string {
	switch p {
	case "dynamic":
		return "dynamic_latency"
	case "", "geo", "off", "proximity", "random":
		if p == "" {
			return "geo"
		}
		return p
	default:
		return p
	}
}

// isCloudflareLBUnavailable reports whether an error is the "Load Balancing is not enabled / not
// entitled on this account" case, so the reconciler can surface a clear condition and requeue gently
// instead of crash-looping. Cloudflare signals this with a 403 (forbidden/entitlement) or a body
// mentioning the subscription/entitlement.
func isCloudflareLBUnavailable(err error) bool {
	var apiErr *cfAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusForbidden {
		return true
	}
	body := strings.ToLower(apiErr.Body)
	if strings.Contains(body, "load balanc") &&
		(strings.Contains(body, "subscription") || strings.Contains(body, "entitle") ||
			strings.Contains(body, "not enabled") || strings.Contains(body, "enable") ||
			strings.Contains(body, "billing") || strings.Contains(body, "upgrade")) {
		return true
	}
	return false
}

// isCloudflareNotFound reports whether an error is a 404 (so a delete of an already-gone object, or
// of an object that never existed, is treated as success during teardown / prune).
func isCloudflareNotFound(err error) bool {
	var apiErr *cfAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// ---- small helpers ----

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// parseWeight parses a decimal weight string into a float clamped to [0,1]; unparseable or empty
// weights default to 1 (equal weighting), matching Cloudflare's default.
func parseWeight(s string) float64 {
	if s == "" {
		return 1
	}
	w, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 1
	}
	if w < 0 {
		return 0
	}
	if w > 1 {
		return 1
	}
	return w
}
