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
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// cfLBBillingOnce ensures the Load-Balancing "enable the paid feature" guidance is logged only once
// per operator process (not on every reconcile).
var cfLBBillingOnce sync.Once

// reconcileCloudflare models this stack's Cloudflare resources as OWNED child CRs (epic #31): the
// child controllers make the CF API calls and hold finalizers for external cleanup, so the operator
// never mutates Cloudflare from the main loop. The public endpoints' DNS records are reconciled
// first, so turning Cloudflare, manageDNS or the token off removes them before the early return.
func (r *OramaCloudV1Reconciler) reconcileCloudflare(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if err := r.reconcilePublicDNS(ctx, oc); err != nil {
		return err
	}
	cf := oc.Spec.Cloudflare
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
			// The index-builder's working bucket, rebuilt from the engine: it goes with the stack,
			// as it always has (operator#453 made the policy explicit).
			bucket.Spec.DeletionPolicy = oramav1alpha1.BucketDeletionDelete
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

// publicDNSSecret is the token Secret the stack's DNS records name, or "" while the operator manages
// no DNS: Cloudflare or manageDNS off, or no token reference. The child controller reads the
// Secret's `token` and `zoneID` itself; the parent never copies credential values into a child.
func publicDNSSecret(oc *oramav1alpha1.OramaCloudV1) string {
	cf := oc.Spec.Cloudflare
	if !cf.Enabled || !cf.ManageDNS || cf.APITokenSecretRef == nil {
		return ""
	}
	return cf.APITokenSecretRef.Name
}

// reconcilePublicDNS keeps one CloudflareDNSRecord, <cr>-<component>-dns, for each published
// endpoint: the dashboard, the writer and the readers. Every host is a record in the zone of the
// token Secret's `zoneID`.
func (r *OramaCloudV1Reconciler) reconcilePublicDNS(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	dashboardHost := ""
	if d := oc.Spec.Components.Dashboard; d.Enabled {
		dashboardHost = d.Host
	}
	secret := publicDNSSecret(oc)
	for _, endpoint := range []struct{ component, host string }{
		{"dashboard", dashboardHost},
		{"writer", writerPublicHost(oc)},
		{"reader", readerPublicHost(oc)},
	} {
		if err := r.reconcileEndpointDNS(ctx, oc, endpoint.component, endpoint.host, secret); err != nil {
			return fmt.Errorf("%s DNS: %w", endpoint.component, err)
		}
	}
	return nil
}

// reconcileEndpointDNS converges one endpoint's record. host or secret empty means the record should
// not exist. The external record follows its child under the DNS controller's create-only rules, so
// a child that already has a Cloudflare record (status.recordID) is never repointed at another host
// or Secret: it is deleted with its spec intact, its finalizer removes the record with the old
// credentials, and a later reconcile creates the replacement. A child being deleted is left alone
// until its deletion event. Without an Ingress address nothing is created and an existing child
// keeps its content.
func (r *OramaCloudV1Reconciler) reconcileEndpointDNS(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component, host, secret string) error {
	rec := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-" + component + "-dns", Namespace: oc.Namespace}}
	if host == "" || secret == "" {
		return r.deletePublicEndpointOwned(ctx, oc, rec)
	}
	exists, err := r.refuseForeignPublicEndpoint(ctx, oc, rec)
	if err != nil {
		return err
	}
	if exists {
		if !rec.DeletionTimestamp.IsZero() {
			return nil
		}
		if rec.Status.RecordID != "" && (rec.Spec.Name != host || rec.Spec.SecretRef != secret) {
			return r.deletePublicEndpointOwned(ctx, oc, rec)
		}
	}
	target, err := r.componentIngressTarget(ctx, oc, component)
	if err != nil || target == "" {
		return err
	}
	desired := oramav1alpha1.CloudflareDNSRecordSpec{SecretRef: secret, Name: host, Content: target, Proxied: true}
	if !exists {
		created := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: rec.Name, Namespace: rec.Namespace}, Spec: desired}
		if err := controllerutil.SetControllerReference(oc, created, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, created)
	}
	if rec.Spec == desired {
		return nil
	}
	// Spec only, under optimistic locking: the child's status, record ID and finalizer stay its
	// controller's.
	patch := client.MergeFromWithOptions(rec.DeepCopy(), client.MergeFromWithOptimisticLock{})
	rec.Spec = desired
	return r.Patch(ctx, rec, patch)
}

// componentIngressTarget returns the first load-balancer address of Ingress <cr>-<component>
// (hostname before IP within one entry), or "" while the Ingress is absent, being deleted or has no
// address yet. Other read errors are returned.
func (r *OramaCloudV1Reconciler) componentIngressTarget(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string) (string, error) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-" + component}, ing); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if !ing.DeletionTimestamp.IsZero() {
		return "", nil
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
