/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// A Lake on a host the customer owns (orama-operator#947, docs/lake/custom-domains.md).
//
// The operator never writes the customer's DNS. It publishes in status.customDomain the records
// the customer creates (a CNAME that routes the host to this cluster and a TXT record that proves
// they control the name), checks both by DNS lookup, and routes the host only once both were seen.
// The host gets its own Ingress, without the Cloudflare edge settings of the Lake's own Ingress
// (the customer's users do not come through Orama's Cloudflare zone), and its own Let's Encrypt
// certificate, issued with HTTP-01 and renewed by cert-manager. Once that certificate is issued the
// Lake's public host becomes the customer's host:
//
//	LAKE_PUBLIC_HOST  = spec.customDomain.host  every link, callback, passkey and Clerk origin
//	LAKE_HOST_ALIASES = spec.host               still served; lake sends browser pages to the
//	                                            public host and serves the API as before
//	LAKE_CONTROL_HOST = spec.host               the Lake's name on the orama_control rows, which
//	                                            the operator also keys on (lakeRowHost), so it
//	                                            never changes with the custom domain
//
// Every cluster runs the same code: the only inputs are the Lake's own spec and DNS.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeCustomDomainConditionType = "CustomDomainReady"

	lakeCustomDomainPending  = "Pending"
	lakeCustomDomainVerified = "Verified"
	lakeCustomDomainRouted   = "Routed"

	// The ownership challenge: a TXT record at _orama-verification.<host> holding
	// orama-verification=<token>. The token is derived from the Lake's UID and the host, so it is
	// stable across reconciles and differs for every Lake and host.
	lakeCustomDomainTXTLabel  = "_orama-verification."
	lakeCustomDomainTXTPrefix = "orama-verification="

	// lakeCustomDomainLabel marks the customer host's Certificate and its Secret with the Lake.
	lakeCustomDomainLabel = "orama.cloud/custom-domain"

	// How often DNS is looked up again: soon while the customer is still creating the records or
	// the certificate is being issued, rarely once the host is routed (only to keep Records
	// current; a routed host is never unrouted by a failed lookup).
	lakeCustomDomainCheckPending = time.Minute
	lakeCustomDomainCheckRouted  = 10 * time.Minute
)

// DomainResolver looks up the customer's DNS records. *net.Resolver implements it; tests substitute
// a fake.
type DomainResolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

func (r *LakeReconciler) domainResolver() DomainResolver {
	if r.DomainResolver != nil {
		return r.DomainResolver
	}
	return net.DefaultResolver
}

// lakeCustomDomainClock remembers, per Lake, the last DNS lookup and what it saw, so a steady Lake
// looks DNS up only every lakeCustomDomainCheck*.
type lakeCustomDomainClock struct {
	mu   sync.Mutex
	last map[types.UID]lakeCustomDomainLookup
}

type lakeCustomDomainLookup struct {
	key       string
	at        time.Time
	routing   bool
	ownership bool
	clerk     map[string]bool
}

func (c *lakeCustomDomainClock) get(uid types.UID, key string, now time.Time, every time.Duration) (lakeCustomDomainLookup, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, found := c.last[uid]
	if !found || last.key != key || now.Sub(last.at) >= every {
		return lakeCustomDomainLookup{}, false
	}
	return last, true
}

func (c *lakeCustomDomainClock) put(uid types.UID, lookup lakeCustomDomainLookup) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.UID]lakeCustomDomainLookup{}
	}
	c.last[uid] = lookup
}

// lakeCustomDomainHost is spec.customDomain.host, lower-cased, or empty without it.
func lakeCustomDomainHost(lake *oramav1alpha1.Lake) string {
	if lake.Spec.CustomDomain == nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(lake.Spec.CustomDomain.Host)), ".")
}

// lakeCustomDomainState is spec.customDomain's status when it is for the current host, else nil.
func lakeCustomDomainState(lake *oramav1alpha1.Lake) *oramav1alpha1.LakeCustomDomainStatus {
	host := lakeCustomDomainHost(lake)
	if host == "" || lake.Status.CustomDomain == nil || lake.Status.CustomDomain.Host != host {
		return nil
	}
	return lake.Status.CustomDomain
}

// lakeCustomDomainRoutedHost is the customer host once it is routed with an issued certificate:
// from then on it is the Lake's public host. Empty before.
func lakeCustomDomainRoutedHost(lake *oramav1alpha1.Lake) string {
	if state := lakeCustomDomainState(lake); state != nil && state.Phase == lakeCustomDomainRouted {
		return state.Host
	}
	return ""
}

// lakeCustomDomainVerifiedHost is the customer host once its records were found (Verified or
// Routed): Clerk allows its origin from then on, so sign-in works the moment it is routed.
func lakeCustomDomainVerifiedHost(lake *oramav1alpha1.Lake) string {
	if state := lakeCustomDomainState(lake); state != nil && state.VerifiedTime != nil {
		return state.Host
	}
	return ""
}

// lakeCustomDomainToken is the ownership challenge's token for the Lake and host.
func lakeCustomDomainToken(lake *oramav1alpha1.Lake, host string) string {
	sum := sha256.Sum256([]byte("orama-lake-custom-domain\x00" + string(lake.UID) + "\x00" + host))
	return hex.EncodeToString(sum[:16])
}

// lakeCustomDomainOwnershipRecord is the TXT record that proves the customer controls host.
func lakeCustomDomainOwnershipRecord(lake *oramav1alpha1.Lake, host string) oramav1alpha1.LakeCustomerDNSRecord {
	return oramav1alpha1.LakeCustomerDNSRecord{
		Purpose: "ownership",
		Type:    "TXT",
		Name:    lakeCustomDomainTXTLabel + host,
		Value:   lakeCustomDomainTXTPrefix + lakeCustomDomainToken(lake, host),
	}
}

// lakeCustomDomainOriginName is the DNS-only record in the Lake's own zone that the customer's
// CNAME points at, so the customer's record never needs to change when the load balancer does:
// <the Lake's host without its zone, dots as dashes>.origin.<zone>, for example
// acme.origin.example.com for acme.example.com. Empty without a DNS credential, or when the zone
// cannot be told from the host.
func lakeCustomDomainOriginName(lake *oramav1alpha1.Lake) string {
	if !lakeHasDNSCredential(lake) || lake.Spec.Host == "" {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(lake.Spec.Host), ".")
	zone, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	label := strings.ReplaceAll(strings.TrimSuffix(strings.TrimSuffix(host, zone), "."), ".", "-")
	if label == "" {
		label = "apex"
	}
	if len(label) > 63 {
		sum := sha256.Sum256([]byte(host))
		label = "h" + hex.EncodeToString(sum[:8])
	}
	return label + ".origin." + zone
}

func lakeCustomDomainOriginRecord(lake *oramav1alpha1.Lake) *oramav1alpha1.CloudflareDNSRecord {
	return &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{
		Name: "lake-" + lake.Name + "-custom-domain-origin", Namespace: lakeNamespace(lake),
	}}
}

func lakeCustomDomainIngressName(lake *oramav1alpha1.Lake) string {
	return "lake-" + lake.Name + "-custom-domain"
}

// lakeCustomDomainRoutingRecord is the record that sends the customer's host to this cluster: a
// CNAME to the origin record when the Lake has a DNS credential, else straight to the ingress load
// balancer (a CNAME to its host name, or an A or AAAA record for its address). ok is false while the
// load balancer has no address.
func lakeCustomDomainRoutingRecord(lake *oramav1alpha1.Lake, host, loadBalancer string) (oramav1alpha1.LakeCustomerDNSRecord, bool) {
	record := oramav1alpha1.LakeCustomerDNSRecord{Purpose: "routing", Name: host}
	if loadBalancer == "" {
		return record, false
	}
	if origin := lakeCustomDomainOriginName(lake); origin != "" {
		record.Type, record.Value = "CNAME", origin
		return record, true
	}
	switch ip := net.ParseIP(loadBalancer); {
	case ip == nil:
		record.Type = "CNAME"
	case ip.To4() != nil:
		record.Type = "A"
	default:
		record.Type = "AAAA"
	}
	record.Value = loadBalancer
	return record, true
}

// lakeCustomDomainCheckRouting is whether host leads to the routing record's target: its canonical
// name is the target, or it resolves to an address the target resolves to (a provider that
// flattens CNAMEs, or a proxy in front, still passes when it ends at the same addresses).
func lakeCustomDomainCheckRouting(ctx context.Context, resolver DomainResolver, host string, record oramav1alpha1.LakeCustomerDNSRecord) bool {
	normalize := func(name string) string { return strings.TrimSuffix(strings.ToLower(name), ".") }
	if record.Type == "CNAME" {
		if cname, err := resolver.LookupCNAME(ctx, host); err == nil && normalize(cname) == normalize(record.Value) {
			return true
		}
	}
	want := []string{record.Value}
	if net.ParseIP(record.Value) == nil {
		addresses, err := resolver.LookupHost(ctx, record.Value)
		if err != nil {
			return false
		}
		want = addresses
	}
	got, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return false
	}
	for _, address := range got {
		if slices.Contains(want, address) {
			return true
		}
	}
	return false
}

// lakeCustomDomainCheckTXT is whether name holds the TXT text value.
func lakeCustomDomainCheckTXT(ctx context.Context, resolver DomainResolver, name, value string) bool {
	texts, err := resolver.LookupTXT(ctx, name)
	if err != nil {
		return false
	}
	return slices.Contains(texts, value)
}

// lakeCustomDomainCheckRecord checks one record a Clerk instance on the customer's domain asks for.
func lakeCustomDomainCheckRecord(ctx context.Context, resolver DomainResolver, record oramav1alpha1.LakeCustomerDNSRecord) bool {
	switch record.Type {
	case "TXT":
		return lakeCustomDomainCheckTXT(ctx, resolver, record.Name, record.Value)
	default:
		return lakeCustomDomainCheckRouting(ctx, resolver, record.Name, record)
	}
}

// reconcileLakeCustomDomain keeps spec.customDomain: the origin record, the lookups, and, once the
// records are verified, the host's Ingress and certificate. It returns the status to publish (nil
// without spec.customDomain) and the condition. It runs before the Deployment, which reads the
// result through lake.Status.CustomDomain (lakeEnv).
func (r *LakeReconciler) reconcileLakeCustomDomain(ctx context.Context, lake *oramav1alpha1.Lake, ns, svcName string, labels map[string]string) (*oramav1alpha1.LakeCustomDomainStatus, *metav1.Condition, error) {
	host := lakeCustomDomainHost(lake)
	// Removal is a transition from a published status, so a Lake that never had a custom domain
	// makes no request for one (anti-flap: a steady Lake writes nothing). Everything removed here
	// is also owned by the Lake, so deleting the Lake deletes it regardless.
	if host == "" || !lake.Spec.IngressEnabled || lake.Spec.Host == "" {
		if lake.Status.CustomDomain == nil {
			return nil, nil, nil
		}
		return nil, nil, r.removeLakeCustomDomain(ctx, lake, ns, "")
	}
	// A host changed: what was made for the old one goes.
	if lake.Status.CustomDomain != nil && lake.Status.CustomDomain.Host != host {
		if err := r.removeLakeCustomDomain(ctx, lake, ns, host); err != nil {
			return nil, nil, err
		}
	}

	condition := &metav1.Condition{Type: lakeCustomDomainConditionType, ObservedGeneration: lake.Generation}
	previous := lakeCustomDomainState(lake)
	state := &oramav1alpha1.LakeCustomDomainStatus{Host: host, Phase: lakeCustomDomainPending}
	if previous != nil {
		state.VerifiedTime = previous.VerifiedTime
	}

	// The route the customer's CNAME points at.
	var main networkingv1.Ingress
	loadBalancer := ""
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: svcName}, &main); err == nil {
		loadBalancer = ingressLoadBalancerTarget(&main)
	} else if !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	if err := r.reconcileLakeCustomDomainOrigin(ctx, lake, labels, loadBalancer); err != nil {
		return nil, nil, fmt.Errorf("origin record: %w", err)
	}
	routing, routable := lakeCustomDomainRoutingRecord(lake, host, loadBalancer)
	ownership := lakeCustomDomainOwnershipRecord(lake, host)
	clerk, err := r.lakeClerkCustomerRecords(ctx, lake, ns)
	if err != nil {
		return nil, nil, err
	}

	// DNS, at most every lakeCustomDomainCheck*.
	key := strings.Join([]string{host, routing.Type, routing.Value, ownership.Value, fmt.Sprint(clerk)}, "\x00")
	every := lakeCustomDomainCheckPending
	if previous != nil && previous.Phase == lakeCustomDomainRouted {
		every = lakeCustomDomainCheckRouted
	}
	now := time.Now()
	lookup, cached := r.customDomainClock.get(lake.UID, key, now, every)
	if !cached {
		resolver := r.domainResolver()
		lookup = lakeCustomDomainLookup{key: key, at: now, clerk: map[string]bool{}}
		lookup.routing = routable && lakeCustomDomainCheckRouting(ctx, resolver, host, routing)
		lookup.ownership = lakeCustomDomainCheckTXT(ctx, resolver, ownership.Name, ownership.Value)
		for _, record := range clerk {
			lookup.clerk[record.Type+" "+record.Name] = lakeCustomDomainCheckRecord(ctx, resolver, record)
		}
		r.customDomainClock.put(lake.UID, lookup)
	}
	if routable {
		routing.Found = lookup.routing
		state.Records = append(state.Records, routing)
	}
	ownership.Found = lookup.ownership
	state.Records = append(state.Records, ownership)
	for _, record := range clerk {
		record.Found = lookup.clerk[record.Type+" "+record.Name]
		state.Records = append(state.Records, record)
	}

	if state.VerifiedTime == nil && lookup.routing && lookup.ownership {
		verified := metav1.NewTime(now.UTC().Truncate(time.Second))
		state.VerifiedTime = &verified
	}
	// Nothing is served on the host before it is verified: no Ingress and no certificate exist
	// for it until then (a changed host's were removed above).
	if state.VerifiedTime == nil {
		condition.Status = metav1.ConditionFalse
		if !routable {
			condition.Reason = "WaitingForLoadBalancer"
			state.Message = "waiting for the ingress load balancer's address, which the customer's CNAME points at"
		} else {
			var missing []string
			if !lookup.routing {
				missing = append(missing, fmt.Sprintf("%s %s -> %s", routing.Type, routing.Name, routing.Value))
			}
			if !lookup.ownership {
				missing = append(missing, fmt.Sprintf("TXT %s = %s", ownership.Name, ownership.Value))
			}
			condition.Reason = "RecordsNotFound"
			state.Message = "the customer creates these records in their zone: " + strings.Join(missing, "; ")
		}
		condition.Message = state.Message
		return state, condition, nil
	}

	// Verified: route the host and issue its certificate.
	state.Phase = lakeCustomDomainVerified
	if err := r.reconcileLakeCustomDomainCertificate(ctx, lake, ns, host); err != nil {
		return nil, nil, fmt.Errorf("certificate for %s: %w", host, err)
	}
	if err := r.reconcileLakeCustomDomainIngress(ctx, lake, ns, svcName, labels, host); err != nil {
		return nil, nil, fmt.Errorf("ingress for %s: %w", host, err)
	}
	ready, notAfter, why, err := r.lakeCustomDomainCertificateStatus(ctx, ns, host)
	if err != nil {
		return nil, nil, err
	}
	state.CertificateReady, state.CertificateNotAfter = ready, notAfter
	if !ready {
		condition.Status, condition.Reason = metav1.ConditionFalse, "CertificatePending"
		state.Message = "the records are verified; waiting for the certificate (HTTP-01 on http://" + host + "/.well-known/acme-challenge/)"
		if why != "" {
			state.Message += ": " + why
		}
		condition.Message = state.Message
		return state, condition, nil
	}
	state.Phase = lakeCustomDomainRouted
	condition.Status, condition.Reason = metav1.ConditionTrue, "Routed"
	condition.Message = "https://" + host + " is the Lake's public host; https://" + lake.Spec.Host + " is kept as an alias"
	var unseen []string
	for _, record := range state.Records {
		if !record.Found {
			unseen = append(unseen, record.Type+" "+record.Name)
		}
	}
	if len(unseen) > 0 {
		state.Message = "the last lookup did not find: " + strings.Join(unseen, ", ")
	}
	return state, condition, nil
}

// reconcileLakeCustomDomainOrigin keeps the DNS-only origin record in the Lake's zone, pointing at
// the ingress load balancer. Without a DNS credential, or before the load balancer has an address,
// there is none.
func (r *LakeReconciler) reconcileLakeCustomDomainOrigin(ctx context.Context, lake *oramav1alpha1.Lake, labels map[string]string, loadBalancer string) error {
	name := lakeCustomDomainOriginName(lake)
	record := lakeCustomDomainOriginRecord(lake)
	if name == "" || loadBalancer == "" {
		return deleteIfExists(ctx, r.Client, record)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, record, func() error {
		record.Labels = withLabels(labels, map[string]string{"orama.cloud/dns-purpose": "custom-domain-origin"})
		record.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
		record.Spec.Name = name
		record.Spec.Content = loadBalancer
		// DNS only: the customer's users reach the origin directly, with the customer's certificate.
		record.Spec.Proxied = false
		return controllerutil.SetControllerReference(lake, record, r.Scheme)
	})
	return err
}

// reconcileLakeCustomDomainCertificate keeps the host's Certificate in the Lake namespace, issued by
// the Lake's Issuer. The Issuer solves exactly this name with HTTP-01 (lakeOriginSolvers), so it is
// issued whatever the Lake's own zone is.
func (r *LakeReconciler) reconcileLakeCustomDomainCertificate(ctx context.Context, lake *oramav1alpha1.Lake, ns, host string) error {
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	certificate.SetName(ingressTLSSecretName(host))
	certificate.SetNamespace(ns)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, certificate, func() error {
		certificate.SetLabels(map[string]string{lakeCustomDomainLabel: lake.Name, serviceLakeLabel: lake.Name})
		certificate.Object["spec"] = map[string]any{
			"secretName": ingressTLSSecretName(host),
			"dnsNames":   []any{host},
			"issuerRef":  map[string]any{"name": lakeOriginIssuerName(lake), "kind": "Issuer", "group": "cert-manager.io"},
			"privateKey": map[string]any{"rotationPolicy": "Always"},
			// The Secret carries the same label, so removing the domain finds and deletes it.
			"secretTemplate": map[string]any{"labels": map[string]any{lakeCustomDomainLabel: lake.Name}},
		}
		return controllerutil.SetControllerReference(lake, certificate, r.Scheme)
	})
	return err
}

// lakeCustomDomainCertificateStatus reads the host's Certificate: whether it is Ready, when it
// expires, and the reason cert-manager gives while it is not.
func (r *LakeReconciler) lakeCustomDomainCertificateStatus(ctx context.Context, ns, host string) (bool, *metav1.Time, string, error) {
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ingressTLSSecretName(host)}, certificate); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil, "", nil
		}
		return false, nil, "", err
	}
	var notAfter *metav1.Time
	if text, found, _ := unstructured.NestedString(certificate.Object, "status", "notAfter"); found {
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			value := metav1.NewTime(parsed.UTC())
			notAfter = &value
		}
	}
	conditions, _, _ := unstructured.NestedSlice(certificate.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		if condition["status"] == "True" {
			return true, notAfter, "", nil
		}
		message, _ := condition["message"].(string)
		return false, notAfter, message, nil
	}
	return false, notAfter, "", nil
}

// reconcileLakeCustomDomainIngress keeps the host's own Ingress: the whole host to the lake Service,
// HTTPS with the host's certificate and a redirect from HTTP. It has none of the Lake Ingress's
// Cloudflare edge settings: the customer's users do not come through Orama's zone, so requiring
// Cloudflare's client certificate would refuse every one of them.
func (r *LakeReconciler) reconcileLakeCustomDomainIngress(ctx context.Context, lake *oramav1alpha1.Lake, ns, svcName string, labels map[string]string, host string) error {
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: lakeCustomDomainIngressName(lake), Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = labels
		ing.Annotations = lakeIngressAnnotations()
		ing.Annotations[annotationNginxSSLRedirect] = "true"
		ing.Spec.IngressClassName = strPtr(ingressClassOrDefault(lake.Spec.IngressClassName))
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &prefix,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName, Port: networkingv1.ServiceBackendPort{Number: lakeServicePort}}},
				}},
			}},
		}}
		ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: ingressTLSSecretName(host)}}
		return controllerutil.SetControllerReference(lake, ing, r.Scheme)
	})
	return err
}

// removeLakeCustomDomainRoute deletes the host's Ingress (an unverified host serves nothing).
func (r *LakeReconciler) removeLakeCustomDomainRoute(ctx context.Context, lake *oramav1alpha1.Lake, ns string) error {
	return deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: lakeCustomDomainIngressName(lake), Namespace: ns}})
}

// removeLakeCustomDomain removes what spec.customDomain made: the Ingress, every certificate and
// certificate Secret but keep's, and, when keep is empty (the domain is removed, not changed), the
// origin record.
func (r *LakeReconciler) removeLakeCustomDomain(ctx context.Context, lake *oramav1alpha1.Lake, ns, keep string) error {
	if err := r.removeLakeCustomDomainRoute(ctx, lake, ns); err != nil {
		return err
	}
	if keep == "" {
		if err := deleteIfExists(ctx, r.Client, lakeCustomDomainOriginRecord(lake)); err != nil {
			return err
		}
	}
	return r.removeLakeCustomDomainCertificates(ctx, lake, ns, keep)
}

// removeLakeCustomDomainCertificates deletes the Lake's custom-domain Certificates and their Secrets
// for any host but keep. A private key for a host the Lake no longer serves is not kept.
func (r *LakeReconciler) removeLakeCustomDomainCertificates(ctx context.Context, lake *oramav1alpha1.Lake, ns, keep string) error {
	keepName := ""
	if keep != "" {
		keepName = ingressTLSSecretName(keep)
	}
	selector := client.MatchingLabels{lakeCustomDomainLabel: lake.Name}
	var certificates unstructured.UnstructuredList
	certificates.SetGroupVersionKind(certManagerCertificateGVK.GroupVersion().WithKind("CertificateList"))
	if err := r.List(ctx, &certificates, client.InNamespace(ns), selector); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range certificates.Items {
		if certificates.Items[i].GetName() != keepName {
			if err := r.Delete(ctx, &certificates.Items[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(ns), selector); err != nil {
		return err
	}
	for i := range secrets.Items {
		if secrets.Items[i].Name != keepName {
			if err := r.Delete(ctx, &secrets.Items[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// lakeCustomDomainEnv is what a routed customer host changes in the Lake's environment: the public
// host, the Orama host as an alias, and the Lake's unchanging name on the control rows. Nothing
// before the host is routed, so a Lake without spec.customDomain is rendered exactly as before.
func lakeCustomDomainEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	routed := lakeCustomDomainRoutedHost(lake)
	if routed == "" {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "LAKE_PUBLIC_HOST", Value: routed},
		{Name: "LAKE_HOST_ALIASES", Value: lake.Spec.Host},
		{Name: "LAKE_CONTROL_HOST", Value: lakeRowHost(lake)},
	}
}

// lakeClerkCustomerRecords are the DNS records the Lake's Clerk instance asks for in the customer's
// zone, which the Clerk pass leaves to the customer instead of writing them
// (reconcileLakeClerkDNS).
func (r *LakeReconciler) lakeClerkCustomerRecords(ctx context.Context, lake *oramav1alpha1.Lake, ns string) ([]oramav1alpha1.LakeCustomerDNSRecord, error) {
	if lake.Spec.Clerk == nil {
		return nil, nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeClerkSecretName(lake.Name)}, &secret); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	raw := secret.Data[lakeClerkCustomerRecordsKey]
	if len(raw) == 0 {
		return nil, nil
	}
	var records []oramav1alpha1.LakeCustomerDNSRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, nil
	}
	return records, nil
}

// lakeCustomerZone is the registrable domain of the customer's host (example.com for
// data.example.com): a Clerk DNS target in it is the customer's to create.
func lakeCustomerZone(lake *oramav1alpha1.Lake) string {
	host := lakeCustomDomainHost(lake)
	if host == "" {
		return ""
	}
	zone, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return zone
}

// inCustomerZone is whether name is the customer's zone or beneath it.
func inCustomerZone(zone, name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	return zone != "" && (name == zone || strings.HasSuffix(name, "."+zone))
}
