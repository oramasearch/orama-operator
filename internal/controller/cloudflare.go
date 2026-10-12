/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cfAPIBase is the Cloudflare API root. A var only so tests can point it at an httptest server.
var cfAPIBase = "https://api.cloudflare.com/client/v4"

// cfLBBillingOnce ensures the Load-Balancing "enable the paid feature" guidance is logged only once
// per operator process (not on every reconcile).
var cfLBBillingOnce sync.Once

// reconcileCloudflare models this cluster's public DNS records as OWNED
// CloudflareDNSRecord CR (epic #31): the child controller makes the CF API call and holds a
// finalizer for external cleanup, so the operator never mutates Cloudflare from the main loop.
// DNS creation waits for enabled + manageDNS + a resolvable ingress target; obsolete owned
// records are finalized. The child waits for its Secret (token+zoneID) before provisioning.
func (r *OramaCloudV1Reconciler) reconcileCloudflare(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cf := oc.Spec.Cloudflare
	// DNS follows each enabled public endpoint, with finalizer cleanup through the child CR.
	for _, endpoint := range []struct {
		component, host string
		enabled         bool
	}{
		{"dashboard", oc.Spec.Components.Dashboard.Host, oc.Spec.Components.Dashboard.Enabled},
		{"reader", oc.Spec.Components.Engine.Reader.Host, oc.Spec.Components.Engine.Enabled},
		{"writer", oc.Spec.Components.Engine.Writer.Host, oc.Spec.Components.Engine.Enabled && oc.Spec.Region.Role == "primary"},
	} {
		rec := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-" + endpoint.component + "-dns", Namespace: oc.Namespace}}
		if !cf.Enabled || !cf.ManageDNS || cf.APITokenSecretRef == nil || !endpoint.enabled || endpoint.host == "" {
			if err := r.deleteCloudV1Owned(ctx, oc, rec); err != nil {
				return err
			}
			continue
		}
		target, err := r.componentIngressTarget(ctx, oc, endpoint.component)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		} // Preserve a serving record while an ingress is waiting for its address.
		zoneID := cfZoneIDForHost(cf.ZoneIDs, endpoint.host)
		// Changing an externally provisioned name/credential reference must finalize the old
		// child first; otherwise the DNS controller's name-based upsert can orphan its old record.
		// A zone change is not finalized here: the child refuses to move an existing record between
		// zones and reports ready=false, so no record is deleted or recreated.
		if err := r.Get(ctx, client.ObjectKeyFromObject(rec), rec); err == nil {
			if !rec.DeletionTimestamp.IsZero() {
				continue
			}
			if rec.Status.RecordID != "" && (rec.Spec.Name != endpoint.host || rec.Spec.SecretRef != cf.APITokenSecretRef.Name) {
				if err := r.deleteCloudV1Owned(ctx, oc, rec); err != nil {
					return err
				}
				continue
			}
		} else if err := client.IgnoreNotFound(err); err != nil {
			return err
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
			rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = cf.APITokenSecretRef.Name, endpoint.host, target, true
			rec.Spec.ZoneID = zoneID
			return controllerutil.SetControllerReference(oc, rec, r.Scheme)
		}); err != nil {
			return err
		}
	}
	if !cf.Enabled || cf.APITokenSecretRef == nil {
		return nil
	}
	secretName := cf.APITokenSecretRef.Name

	// KV namespaces + R2 bucket the ingestion path (index-builder) consumes.
	if oc.Spec.Components.Ingestion.Enabled {
		for _, kv := range []struct{ name, title string }{
			{oc.Name + "-kv-dumps", oc.Name + "-dumps"},
			{oc.Name + "-kv-api-keys", oc.Name + "-api-keys"},
		} {
			obj := &oramav1alpha1.CloudflareKVNamespace{ObjectMeta: metav1.ObjectMeta{Name: kv.name, Namespace: oc.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
				obj.Spec.SecretRef, obj.Spec.Title = secretName, kv.title
				return controllerutil.SetControllerReference(oc, obj, r.Scheme)
			}); err != nil {
				return err
			}
		}
		bucket := &oramav1alpha1.CloudflareR2Bucket{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-legacy-indexes", Namespace: oc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bucket, func() error {
			bucket.Spec.SecretRef, bucket.Spec.Name = secretName, oc.Name+"-legacy-indexes"
			return controllerutil.SetControllerReference(oc, bucket, r.Scheme)
		}); err != nil {
			return err
		}
	}

	// Cloudflare Load Balancing (Pools / Monitors / Load Balancers) is a PAID add-on that must be
	// enabled on the account before its API will accept requests — the operator cannot enable billing
	// for you. Surface clear guidance in the log so users know the one manual step to take (as seen in
	// the dashboard: Delivery & performance → Load Balancing). Logged once per process to avoid noise.
	//
	// The Load Balancer / Pool / Monitor objects are now modelled as an owned CloudflareLoadBalancer
	// CRD reconciled by CloudflareLoadBalancerReconciler (operator#73). The main loop does NOT create
	// that CR here because its origins are the cluster's live wan-accessible nodes — populating the
	// pool from the live node set is tracked separately (operator#74 / zetta#739). Apply a
	// CloudflareLoadBalancer directly (see config/samples) until that wiring lands.
	if cf.ManageLoadBalancers {
		cfLBBillingOnce.Do(func() {
			logf.FromContext(ctx).Info(
				"spec.cloudflare.manageLoadBalancers is enabled, but Cloudflare Load Balancing is a PAID "+
					"feature that must be activated on the account FIRST — the operator cannot enable billing. "+
					"Enable it in the Cloudflare dashboard (Delivery & performance → Load Balancing → Create / "+
					"Purchase additional features), then the operator will reconcile Pools, Monitors, and Load "+
					"Balancers for the configured hosts. Docs: https://developers.cloudflare.com/load-balancing/",
				"component", "cloudflare", "manageLoadBalancers", true, "account", "see cloudflare-api-token accountID")
		})
	}
	return nil
}

// cfZoneIDForHost picks the zone ID whose zone name is the longest suffix of host (the host itself
// or a parent domain), so app.orama.com maps to orama.com rather than com. Empty means no entry
// matches and the record falls back to its Secret's zoneID.
func cfZoneIDForHost(zoneIDs map[string]string, host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	best, bestLen := "", -1
	for zone, id := range zoneIDs {
		zone = strings.TrimSuffix(strings.ToLower(zone), ".")
		if zone == "" || len(zone) <= bestLen {
			continue
		}
		if host == zone || strings.HasSuffix(host, "."+zone) {
			best, bestLen = id, len(zone)
		}
	}
	return best
}

// componentIngressTarget returns this component's assigned ingress address.
func (r *OramaCloudV1Reconciler) componentIngressTarget(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string) (string, error) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-" + component}, ing); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" {
			return lb.Hostname, nil
		}
		if lb.IP != "" {
			return lb.IP, nil
		}
	}
	return "", nil
}

// cfAPIError is a non-2xx Cloudflare API response. It carries the HTTP status + raw body so callers
// can distinguish cases (e.g. a 404 on delete, or a 403/entitlement error when Load Balancing is not
// enabled on the account) via errors.As, while Error() keeps the original human-readable format so
// existing callers that only propagate/log the error are unaffected.
type cfAPIError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *cfAPIError) Error() string {
	return fmt.Sprintf("cloudflare API %s %s: %d %s", e.Method, e.URL, e.StatusCode, e.Body)
}

func cfDo(ctx context.Context, token, method, url string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &cfAPIError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
