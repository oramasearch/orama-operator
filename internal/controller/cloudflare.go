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
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// reconcileCloudflare models this cluster's dashboard DNS record as an OWNED
// CloudflareDNSRecord CR (epic #31): the child controller makes the CF API call and holds a
// finalizer for external cleanup, so the operator never mutates Cloudflare from the main loop.
// SAFE no-op unless fully configured (enabled + manageDNS + a resolvable ingress target); the
// child is itself a no-op until its Secret (token+zoneID) exists.
func (r *OramaCloudV1Reconciler) reconcileCloudflare(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cf := oc.Spec.Cloudflare
	if !cf.Enabled || cf.APITokenSecretRef == nil {
		return nil
	}
	secretName := cf.APITokenSecretRef.Name

	// Dashboard DNS record (when manageDNS + a resolvable ingress target).
	if cf.ManageDNS {
		if host := oc.Spec.Components.Dashboard.Host; host != "" {
			if target := r.ingressTarget(ctx, oc); target != "" {
				rec := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-dashboard-dns", Namespace: oc.Namespace}}
				if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
					rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = secretName, host, target, true
					return controllerutil.SetControllerReference(oc, rec, r.Scheme)
				}); err != nil {
					return err
				}
			}
		}
	}

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
	return nil
}

// ingressTarget returns the external address of the dashboard Ingress, if assigned.
func (r *OramaCloudV1Reconciler) ingressTarget(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) string {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-dashboard"}, ing); err != nil {
		return ""
	}
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" {
			return lb.Hostname
		}
		if lb.IP != "" {
			return lb.IP
		}
	}
	return ""
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
		return fmt.Errorf("cloudflare API %s %s: %d %s", method, url, resp.StatusCode, string(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
