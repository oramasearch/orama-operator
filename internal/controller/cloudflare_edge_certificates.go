/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Edge certificates for hosts Universal SSL does not cover (lake#798).
//
// Cloudflare's Universal SSL certificate covers the zone apex and one level below it
// (oramadev.net, *.oramadev.net). A Lake's apps wildcard is its host, so app hosts are two levels
// below the apex (<app>.lake-testing.oramadev.net, <app>.eu.lake.live) and the edge refuses their
// TLS handshake (alert 40). For every proxied host more than one level below the apex whose edge
// has no valid certificate, the zone policy orders an Advanced Certificate (an Advanced
// Certificate Manager certificate pack) covering it, unless a pack in the zone already covers it.
// Cloudflare validates and renews the pack itself (TXT validation on a zone it serves DNS for).
// Packs are never deleted by the operator.

const (
	edgeConditionEdgeCertificateOrders = "EdgeCertificateOrders"

	cfPermissionCertificatesEdit = "Zone > SSL and Certificates > Edit"

	// edgeCertificateAuthority and edgeCertificateValidityDays are the order's settings. Cloudflare
	// renews a pack before it expires, so the validity only sets how often.
	edgeCertificateAuthority    = "lets_encrypt"
	edgeCertificateValidityDays = 90
	// edgeCertificateOrderLimit bounds the orders one reconcile places.
	edgeCertificateOrderLimit = 10
)

type cfCertificatePack struct {
	ID     string   `json:"id"`
	Type   string   `json:"type"`
	Hosts  []string `json:"hosts"`
	Status string   `json:"status"`
}

type cfCertificatePackList struct {
	Result     []cfCertificatePack `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cfCertificatePackOrder struct {
	Type                 string   `json:"type"`
	Hosts                []string `json:"hosts"`
	ValidationMethod     string   `json:"validation_method"`
	ValidityDays         int      `json:"validity_days"`
	CertificateAuthority string   `json:"certificate_authority"`
	CloudflareBranding   bool     `json:"cloudflare_branding"`
}

// certificatePacks lists every pack in the zone, of every status.
func (r *CloudflareZonePolicyReconciler) certificatePacks(ctx context.Context, token, zoneID string) ([]cfCertificatePack, error) {
	var packs []cfCertificatePack
	for page := 1; page <= 50; page++ {
		var out cfCertificatePackList
		url := fmt.Sprintf("%s/zones/%s/ssl/certificate_packs?status=all&per_page=50&page=%d", r.api(), zoneID, page)
		if err := cfDo(ctx, token, http.MethodGet, url, nil, &out); err != nil {
			return nil, err
		}
		packs = append(packs, out.Result...)
		if out.ResultInfo.TotalPages <= page {
			break
		}
	}
	return packs, nil
}

func (r *CloudflareZonePolicyReconciler) orderCertificatePack(ctx context.Context, token, zoneID string, hosts []string) error {
	order := cfCertificatePackOrder{
		Type: "advanced", Hosts: hosts, ValidationMethod: "txt",
		ValidityDays: edgeCertificateValidityDays, CertificateAuthority: edgeCertificateAuthority,
	}
	return cfDo(ctx, token, http.MethodPost, fmt.Sprintf("%s/zones/%s/ssl/certificate_packs/order", r.api(), zoneID), order, nil)
}

// edgeHostDepth is how many labels name has below apex: 0 for the apex, 1 for x.apex and *.apex,
// 2 for *.x.apex. -1 when name is not in the zone.
func edgeHostDepth(name, apex string) int {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == apex {
		return 0
	}
	if !strings.HasSuffix(name, "."+apex) {
		return -1
	}
	return strings.Count(strings.TrimSuffix(name, "."+apex), ".") + 1
}

// edgeCertificatePackLive is whether a pack is, or may still become, an edge certificate. Deleted,
// expired, failed and timed-out packs do not count, so the host is ordered again.
func edgeCertificatePackLive(status string) bool {
	switch {
	case status == "deleted", status == "expired", status == "inactive",
		strings.Contains(status, "timed_out"), strings.Contains(status, "failed"):
		return false
	}
	return true
}

// edgeCertificateCovers is whether a pack host covers name: the same name, or a wildcard one level
// above a plain name.
func edgeCertificateCovers(packHost, name string) bool {
	packHost, name = strings.ToLower(packHost), strings.ToLower(name)
	if packHost == name {
		return true
	}
	if strings.HasPrefix(packHost, "*.") && !strings.HasPrefix(name, "*.") {
		if i := strings.IndexByte(name, '.'); i > 0 {
			return name[i:] == packHost[1:]
		}
	}
	return false
}

// edgeCertificateHosts is what a pack for name lists: the apex (Cloudflare requires it), name, and
// for a wildcard the name it is a wildcard of, so the bare host is covered as well.
func edgeCertificateHosts(name, apex string) []string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	hosts := []string{apex, name}
	if strings.HasPrefix(name, "*.") {
		hosts = append(hosts, name[2:])
	}
	return hosts
}

// edgeCertificateCandidates are the proxied hosts whose edge has no valid certificate and that
// Universal SSL cannot cover: more than one level below the apex. Sorted, deduplicated.
func edgeCertificateCandidates(checks []edgeHostCheck, apex string) []string {
	seen := map[string]bool{}
	var names []string
	for _, check := range checks {
		name := strings.ToLower(strings.TrimSuffix(check.status.Name, "."))
		if check.status.Edge == "" || seen[name] || edgeHostDepth(name, apex) < 2 {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// orderEdgeCertificates orders a pack for every candidate host no live pack covers, and reports
// the result as the EdgeCertificateOrders condition. It is called only when a proxied host has no
// valid edge certificate, so a zone whose hosts are all covered makes no certificate API calls.
// apex is the zone's name as observe read it (zoneName), or the last observed one when that read
// was refused; empty when it has never been read.
func (r *CloudflareZonePolicyReconciler) orderEdgeCertificates(ctx context.Context, token, zoneID, apex string, zoneNameErr error, checks []edgeHostCheck) metav1.Condition {
	condition := func(ok bool, reason, message string) metav1.Condition {
		status := metav1.ConditionTrue
		if !ok {
			status = metav1.ConditionFalse
		}
		return metav1.Condition{Type: edgeConditionEdgeCertificateOrders, Status: status, Reason: reason, Message: message}
	}
	if apex == "" {
		message := "the zone's name is unknown, so hosts Universal SSL does not cover cannot be found"
		if zoneNameErr != nil {
			message = cfErrorMessage("read the zone's name", zoneNameErr, cfPermissionZoneRead)
		}
		return condition(false, "ZoneUnreadable", message)
	}
	candidates := edgeCertificateCandidates(checks, apex)
	if len(candidates) == 0 {
		return condition(true, "NothingToOrder", "every host without an edge certificate is one Universal SSL covers")
	}
	packs, err := r.certificatePacks(ctx, token, zoneID)
	if err != nil {
		return condition(false, "CertificatesUnreadable", cfErrorMessage("list the zone's edge certificates", err, cfPermissionCertificatesEdit))
	}
	var pending, ordered []string
	for _, name := range candidates {
		covered := false
		for _, pack := range packs {
			if !edgeCertificatePackLive(pack.Status) {
				continue
			}
			for _, host := range pack.Hosts {
				if edgeCertificateCovers(host, name) {
					covered = true
				}
			}
		}
		if covered {
			pending = append(pending, name)
			continue
		}
		if len(ordered) >= edgeCertificateOrderLimit {
			break
		}
		if err := r.orderCertificatePack(ctx, token, zoneID, edgeCertificateHosts(name, apex)); err != nil {
			message := cfErrorMessage("order an edge certificate for "+name, err, cfPermissionCertificatesEdit)
			if !cfDenied(err) {
				message += " (an Advanced Certificate needs the zone to have Advanced Certificate Manager)"
			}
			return condition(false, "OrderRefused", message)
		}
		logf.FromContext(ctx).Info("edge certificate ordered", "zone", zoneID, "host", name)
		ordered = append(ordered, name)
	}
	if len(ordered) > 0 {
		return condition(false, "Ordered", fmt.Sprintf("edge certificate ordered for %s; Cloudflare validates and deploys it", strings.Join(ordered, ", ")))
	}
	return condition(false, "Pending", fmt.Sprintf("edge certificate for %s is ordered and not yet serving", strings.Join(pending, ", ")))
}
