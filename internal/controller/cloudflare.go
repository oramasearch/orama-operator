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
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// reconcileCloudflare reconciles Cloudflare DNS for this cluster's ingress host via
// the CF API — Cloudflare stays the permanent edge/geo-LB, and the operator points
// the dashboard host at this cluster's ingress. It is a SAFE no-op unless fully
// configured (enabled + token secret present + zoneID + manageDNS + a resolvable
// ingress target).
//
// NOTE: Load Balancing (geo-steered pools across regions) is a follow-up, and this
// reconciler has NOT yet been validated against a live Cloudflare account.
func (r *OramaCloudV1Reconciler) reconcileCloudflare(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	l := log.FromContext(ctx)
	cf := oc.Spec.Cloudflare
	if !cf.Enabled || cf.APITokenSecretRef == nil {
		return nil
	}

	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: cf.APITokenSecretRef.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("cloudflare enabled but token secret not found; skipping", "secret", cf.APITokenSecretRef.Name)
			return nil
		}
		return err
	}
	token := string(sec.Data["token"])
	if token == "" || cf.ZoneID == "" || !cf.ManageDNS {
		l.Info("cloudflare not fully configured (token/zoneID/manageDNS); skipping")
		return nil
	}

	host := oc.Spec.Components.Dashboard.Host
	if host == "" {
		return nil
	}
	target := r.ingressTarget(ctx, oc)
	if target == "" {
		l.Info("cloudflare: no ingress address assigned yet; will retry", "host", host)
		return nil
	}
	l.Info("cloudflare: upserting DNS record", "host", host, "target", target)
	return cfUpsertDNSRecord(ctx, token, cf.ZoneID, host, target)
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

// cfUpsertDNSRecord creates or updates a proxied DNS record (name -> target).
func cfUpsertDNSRecord(ctx context.Context, token, zoneID, name, target string) error {
	recType := "A"
	if net.ParseIP(target) == nil {
		recType = "CNAME"
	}
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=%s&name=%s", cfAPIBase, zoneID, recType, name)
	var listResp struct {
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
		return err
	}
	body := map[string]interface{}{"type": recType, "name": name, "content": target, "ttl": 1, "proxied": true}
	if len(listResp.Result) > 0 {
		url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, listResp.Result[0].ID)
		return cfDo(ctx, token, http.MethodPut, url, body, nil)
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	return cfDo(ctx, token, http.MethodPost, url, body, nil)
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
