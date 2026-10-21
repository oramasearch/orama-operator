/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Origin TLS for every Lake Ingress (operator#578), on the model of Cloud v1's (#457,
// ingress_tls.go).
//
//   - Each Lake with a dnsSecretRef owns a namespaced cert-manager Issuer in its namespace, Let's
//     Encrypt with DNS-01 through that Secret's `token`, so each environment uses its own zone's
//     token. Every certificate a Lake's hosts need is issued there, where the credential is.
//   - The Lake's own Ingress (host, apexHost) and its wildcard Ingresses (*.<apps wildcard>,
//     *.<preview wildcard>) request their certificates through ingress-shim (applyIngressTLS).
//   - An app or environment host under one of those wildcards uses the wildcard's certificate; any
//     other host gets its own Certificate in the Lake namespace, owned by the LakeApp or
//     LakeEnvironment. Either way the certificate's Secret is copied next to the Ingress, in the
//     workspace namespace: a Kubernetes Ingress can name only a Secret in its own namespace, and
//     Orama's Cloudflare token must never be put in a workspace namespace.
//   - Each Lake also owns an EdgeZonePolicy for its zone. Its status gates two Ingress settings:
//     the HTTP to HTTPS redirect (only when the edge reaches origins over TLS, or it would loop),
//     and the requirement for the edge's client certificate (only once the edge presents one).

// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers;certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=edgeproviders;edgezonepolicies,verbs=get;list;watch;create;update;patch;delete

// certManagerCertificateGVK is cert-manager's Certificate, written unstructured like the Issuer.
var certManagerCertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

const (
	annotationNginxAuthTLSSecret       = "nginx.ingress.kubernetes.io/auth-tls-secret"
	annotationNginxAuthTLSVerifyClient = "nginx.ingress.kubernetes.io/auth-tls-verify-client"
	annotationNginxAuthTLSVerifyDepth  = "nginx.ingress.kubernetes.io/auth-tls-verify-depth"
	annotationNginxPermanentRedirect   = "nginx.ingress.kubernetes.io/permanent-redirect"

	// lakeTLSOwnerLabel marks a per-host Certificate with the LakeApp or LakeEnvironment it is for,
	// and a copied certificate Secret with the Ingress it serves.
	lakeTLSOwnerLabel = "orama.cloud/tls-owner"
	// lakeTLSCopyLabel marks a Secret the operator copied from a Lake's certificate.
	lakeTLSCopyLabel = "orama.cloud/tls-copy"
)

// errLakeNoOriginCredential is returned for a Lake host that needs a certificate when the Lake has
// no dnsSecretRef. There is no HTTP-only fallback.
var errLakeNoOriginCredential = errors.New("origin TLS needs spec.dnsSecretRef (key \"token\") for the Let's Encrypt DNS-01 Issuer")

func lakeHasDNSCredential(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.DNSSecretRef != nil && lake.Spec.DNSSecretRef.Name != ""
}

// deleteIfKindExists is deleteIfExists for a kind whose CRD may be absent (cert-manager's, or the
// edge kinds on a cluster mid-rollout): no CRD means no object to delete.
func deleteIfKindExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := deleteIfExists(ctx, c, obj); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	return nil
}

// lakeOriginIssuerName is the Lake's cert-manager Issuer.
func lakeOriginIssuerName(lake *oramav1alpha1.Lake) string {
	return "lake-" + lake.Name + "-letsencrypt"
}

// lakeOriginIssuer is the Issuer a Lake's Ingress builders use, or errLakeNoOriginCredential.
func lakeOriginIssuer(lake *oramav1alpha1.Lake) (string, error) {
	if !lakeHasDNSCredential(lake) {
		return "", errLakeNoOriginCredential
	}
	return lakeOriginIssuerName(lake), nil
}

// lakeEdgeName names the Lake's EdgeProvider and EdgeZonePolicy.
func lakeEdgeName(lake *oramav1alpha1.Lake) string { return "lake-" + lake.Name + "-edge" }

// reconcileLakeOriginIssuer keeps the Lake's Let's Encrypt Issuer while it has a dnsSecretRef.
func reconcileLakeOriginIssuer(ctx context.Context, c client.Client, scheme *runtime.Scheme, lake *oramav1alpha1.Lake, labels map[string]string) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	issuer.SetName(lakeOriginIssuerName(lake))
	issuer.SetNamespace(lakeNamespace(lake))
	if !lakeHasDNSCredential(lake) {
		return deleteIfKindExists(ctx, c, issuer)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, issuer, func() error {
		issuer.SetLabels(labels)
		issuer.Object["spec"] = map[string]any{
			"acme": map[string]any{
				"server":              letsEncryptDirectory,
				"privateKeySecretRef": map[string]any{"name": lakeOriginIssuerName(lake) + "-account"},
				"solvers": []any{map[string]any{
					"dns01": map[string]any{
						"cloudflare": map[string]any{
							"apiTokenSecretRef": map[string]any{"name": lake.Spec.DNSSecretRef.Name, "key": "token"},
						},
					},
				}},
			},
		}
		return controllerutil.SetControllerReference(lake, issuer, scheme)
	})
	return err
}

// reconcileLakeEdgePolicy keeps the Lake's EdgeProvider and EdgeZonePolicy for the zone of its
// dnsSecretRef (keys `token` and `zoneID`), and removes them without one.
func reconcileLakeEdgePolicy(ctx context.Context, c client.Client, scheme *runtime.Scheme, lake *oramav1alpha1.Lake, labels map[string]string) error {
	ns, name := lakeNamespace(lake), lakeEdgeName(lake)
	provider := &oramav1alpha1.EdgeProvider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	policy := &oramav1alpha1.EdgeZonePolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if !lakeHasDNSCredential(lake) {
		if err := deleteIfKindExists(ctx, c, policy); err != nil {
			return err
		}
		return deleteIfKindExists(ctx, c, provider)
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: lake.Spec.DNSSecretRef.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// The DNS records wait for the same Secret and report it; the policy is written once
			// the Secret exists (the Lake is re-reconciled on its own schedule).
			return nil
		}
		return fmt.Errorf("read spec.dnsSecretRef %q for the zone policy: %w", lake.Spec.DNSSecretRef.Name, err)
	}
	zoneID := strings.TrimSpace(string(secret.Data["zoneID"]))
	if zoneID == "" {
		return fmt.Errorf("spec.dnsSecretRef %q has no zoneID, so the zone policy cannot name the zone", lake.Spec.DNSSecretRef.Name)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, provider, func() error {
		provider.Labels = labels
		provider.Spec = oramav1alpha1.EdgeProviderSpec{Type: "cloudflare", Cloudflare: &oramav1alpha1.CloudflareEdgeProviderSpec{
			AccountID:         strings.TrimSpace(string(secret.Data["accountID"])),
			APITokenSecretRef: oramav1alpha1.EdgeProviderSecretKeyRef{Name: lake.Spec.DNSSecretRef.Name, Key: "token"},
		}}
		return controllerutil.SetControllerReference(lake, provider, scheme)
	}); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, policy, func() error {
		policy.Labels = labels
		policy.Spec = oramav1alpha1.EdgeZonePolicySpec{ProviderRef: corev1.LocalObjectReference{Name: name}, ZoneID: zoneID}
		return controllerutil.SetControllerReference(lake, policy, scheme)
	})
	return err
}

// lakeEdge is what a Lake's EdgeZonePolicy says about its zone, as its Ingresses use it.
type lakeEdge struct {
	// originEncrypted: the edge reaches origins over TLS, so the origin may redirect HTTP.
	originEncrypted bool
	// clientCA is set when the edge presents its client certificate, so origins may require it.
	clientCA string
	// noEdgeCertificate are the zone's proxied record names (a wildcard as "*.x") the edge has no
	// valid certificate for. A host there is not redirected to HTTPS: the visitor could not follow.
	noEdgeCertificate map[string]bool
}

// edgeCovers is whether the edge has a certificate for host, as far as the zone policy saw.
func (e lakeEdge) edgeCovers(host string) bool {
	if e.noEdgeCertificate[host] {
		return false
	}
	if _, parent, ok := strings.Cut(host, "."); ok && e.noEdgeCertificate["*."+parent] {
		return false
	}
	return true
}

// readLakeEdge reads the Lake's EdgeZonePolicy. A Lake without one, or one not yet observed,
// gets neither the redirect nor the client-certificate requirement: both are only safe once the
// zone is known to allow them.
func readLakeEdge(ctx context.Context, c client.Client, lake *oramav1alpha1.Lake) (lakeEdge, error) {
	if !lakeHasDNSCredential(lake) {
		return lakeEdge{}, nil
	}
	var policy oramav1alpha1.EdgeZonePolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeEdgeName(lake)}, &policy); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return lakeEdge{}, nil
		}
		return lakeEdge{}, err
	}
	edge := lakeEdge{originEncrypted: edgeOriginEncrypted(policy.Status.OriginEncryption)}
	if edge.originEncrypted && policy.Status.AuthenticatedOriginPulls {
		edge.clientCA = policy.Status.OriginClientCAPEM
	}
	for _, host := range policy.Status.Hosts {
		if host.Edge != "" {
			if edge.noEdgeCertificate == nil {
				edge.noEdgeCertificate = map[string]bool{}
			}
			edge.noEdgeCertificate[host.Name] = true
		}
	}
	return edge, nil
}

// applyLakeEdgeAnnotations sets the redirect and the client-certificate requirement on a Lake
// Ingress from the zone's state. caSecret is "<namespace>/<name>" of the Secret holding the
// edge's CA (reconcileEdgeClientCA), or empty. Call it after applyIngressTLS, which sets the
// redirect unconditionally.
func applyLakeEdgeAnnotations(ing *networkingv1.Ingress, edge lakeEdge, caSecret string) {
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	// ingress-nginx redirects every host that has a certificate unless told not to, so "false" is
	// written, not left out. The redirect is per Ingress, so one host the edge cannot serve over
	// HTTPS keeps it off for the Ingress.
	redirect := edge.originEncrypted
	for _, rule := range ing.Spec.Rules {
		redirect = redirect && edge.edgeCovers(rule.Host)
	}
	ing.Annotations[annotationNginxSSLRedirect] = fmt.Sprintf("%t", redirect)
	if edge.clientCA != "" && caSecret != "" {
		ing.Annotations[annotationNginxAuthTLSSecret] = caSecret
		ing.Annotations[annotationNginxAuthTLSVerifyClient] = "on"
		ing.Annotations[annotationNginxAuthTLSVerifyDepth] = "1"
		return
	}
	delete(ing.Annotations, annotationNginxAuthTLSSecret)
	delete(ing.Annotations, annotationNginxAuthTLSVerifyClient)
	delete(ing.Annotations, annotationNginxAuthTLSVerifyDepth)
}

// reconcileEdgeClientCA keeps a Secret with the edge's client CA (`ca.crt`, the key ingress-nginx
// reads) while the zone has authenticated origin pulls, and returns "<namespace>/<name>". mutate
// sets the Secret's labels, annotations and owner.
func reconcileEdgeClientCA(ctx context.Context, c client.Client, namespace, name string, edge lakeEdge, mutate func(*corev1.Secret) error) (string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if edge.clientCA == "" {
		return "", deleteIfExists(ctx, c, secret)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{"ca.crt": []byte(edge.clientCA)}
		return mutate(secret)
	}); err != nil {
		return "", err
	}
	return namespace + "/" + name, nil
}

// lakeWildcardFor is the wildcard certificate host that covers host for this Lake ("*.<suffix>"),
// or "" when none does. Only one label under the apps or preview wildcard is covered, as a
// certificate covers it, and only while the Lake serves those wildcards (lakeServesWildcards).
func lakeWildcardFor(lake *oramav1alpha1.Lake, host string) string {
	if !lakeServesWildcards(lake) {
		return ""
	}
	for _, suffix := range lakeWildcardSuffixes(lake) {
		if label, ok := strings.CutSuffix(host, "."+suffix); ok && label != "" && !strings.Contains(label, ".") {
			return "*." + suffix
		}
	}
	return ""
}

// lakeServesWildcards is whether the Lake renders its wildcard Ingresses: it has a public host and
// a DNS credential to issue wildcard certificates with (wildcards need DNS-01).
func lakeServesWildcards(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.IngressEnabled && lake.Spec.Host != "" && lakeHasDNSCredential(lake)
}

// lakeWildcardSuffixes are the apps wildcard and, when set, the preview wildcard.
func lakeWildcardSuffixes(lake *oramav1alpha1.Lake) []string {
	var suffixes []string
	if apps := lakeAppsWildcard(lake); apps != "" {
		suffixes = append(suffixes, apps)
	}
	if preview := strings.TrimPrefix(lake.Spec.PreviewWildcard, "*."); preview != "" && preview != lakeAppsWildcard(lake) {
		suffixes = append(suffixes, preview)
	}
	return suffixes
}

// lakeTLSOwnerValue is a label value naming the LakeApp or LakeEnvironment a certificate is for.
func lakeTLSOwnerValue(kind, name string) string {
	return fmt.Sprintf("%s-%x", strings.ToLower(kind), sha256.Sum256([]byte(name)))[:40]
}

// reconcileLakeHostCertificates gives every host a certificate in the Lake namespace and returns
// each host's certificate Secret there. A host under a wildcard the Lake serves uses that
// wildcard's Secret; any other host gets a Certificate named ingressTLSSecretName(host), owned by
// owner and labelled with ownerValue. The owner's Certificates for hosts it no longer has are
// deleted. A Certificate another owner already holds is never shared: the host gets no source, so
// the owner's Ingress serves no certificate for it, rather than another tenant's key (#776).
func reconcileLakeHostCertificates(ctx context.Context, c client.Client, scheme *runtime.Scheme, lake *oramav1alpha1.Lake, owner client.Object, ownerValue string, hosts []string) (map[string]string, error) {
	issuer, err := lakeOriginIssuer(lake)
	if err != nil {
		hosts = nil // nothing can be issued; the owner's Certificates are still pruned below
	}
	ns := lakeNamespace(lake)
	secrets := make(map[string]string, len(hosts))
	keep := map[string]bool{}
	for _, host := range hosts {
		if wildcard := lakeWildcardFor(lake, host); wildcard != "" {
			secrets[host] = ingressTLSSecretName(wildcard)
			continue
		}
		name := ingressTLSSecretName(host)
		certificate := &unstructured.Unstructured{}
		certificate.SetGroupVersionKind(certManagerCertificateGVK)
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, certificate); err == nil {
			if certificate.GetLabels()[lakeTLSOwnerLabel] != ownerValue {
				continue // another app or environment holds this host's certificate; never copy its key
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		secrets[host] = name
		keep[name] = true
		certificate = &unstructured.Unstructured{}
		certificate.SetGroupVersionKind(certManagerCertificateGVK)
		certificate.SetName(name)
		certificate.SetNamespace(ns)
		if _, err := controllerutil.CreateOrUpdate(ctx, c, certificate, func() error {
			certificate.SetLabels(map[string]string{lakeTLSOwnerLabel: ownerValue, serviceLakeLabel: lake.Name})
			certificate.Object["spec"] = map[string]any{
				"secretName": name,
				"dnsNames":   []any{host},
				"issuerRef":  map[string]any{"name": issuer, "kind": "Issuer", "group": "cert-manager.io"},
				"privateKey": map[string]any{"rotationPolicy": "Always"},
			}
			return controllerutil.SetControllerReference(owner, certificate, scheme)
		}); err != nil {
			return nil, fmt.Errorf("certificate for %s: %w", host, err)
		}
	}
	var existing unstructured.UnstructuredList
	existing.SetGroupVersionKind(certManagerCertificateGVK.GroupVersion().WithKind("CertificateList"))
	if err := c.List(ctx, &existing, client.InNamespace(ns), client.MatchingLabels{lakeTLSOwnerLabel: ownerValue}); err != nil {
		if meta.IsNoMatchError(err) {
			return secrets, nil
		}
		return nil, err
	}
	for i := range existing.Items {
		if !keep[existing.Items[i].GetName()] {
			if err := c.Delete(ctx, &existing.Items[i]); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
		}
	}
	return secrets, nil
}

// copiedTLSSecretName is the name of a certificate's copy next to an Ingress: one per Ingress and
// host, so removing one Ingress never removes a Secret another still names.
func copiedTLSSecretName(ingressName, host string) string {
	return fmt.Sprintf("%s-tls-%x", ingressName, sha256.Sum256([]byte(host)))[:len(ingressName)+5+12]
}

// copyLakeTLSSecrets copies each host's certificate Secret from the Lake namespace into namespace,
// for the Ingress ingressName, and returns the TLS entries for it: one per host, naming its copy.
// A certificate not issued yet is not copied (the entry still names the copy, and ingress-nginx
// serves no certificate and no redirect for the host until it exists); a renewed one is copied
// again. A host with no source (another owner holds its certificate) gets no copy, and a copy made
// for it before is deleted, so another tenant's key never stays in this namespace (#776). Copies of hosts the Ingress no longer has are deleted. mutate sets labels, annotations
// and owner on each copy.
func copyLakeTLSSecrets(ctx context.Context, c client.Client, lake *oramav1alpha1.Lake, namespace, ingressName string, hosts []string, sources map[string]string, mutate func(*corev1.Secret)) ([]networkingv1.IngressTLS, error) {
	ns := lakeNamespace(lake)
	owner := lakeTLSOwnerValue("ingress", namespace+"/"+ingressName)
	keep := map[string]bool{}
	var tls []networkingv1.IngressTLS
	for _, host := range hosts {
		name := copiedTLSSecretName(ingressName, host)
		tls = append(tls, networkingv1.IngressTLS{Hosts: []string{host}, SecretName: name})
		if sources[host] == "" {
			continue
		}
		keep[name] = true
		var source corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: sources[host]}, &source); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if len(source.Data[corev1.TLSCertKey]) == 0 || len(source.Data[corev1.TLSPrivateKeyKey]) == 0 {
			continue
		}
		copy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, c, copy, func() error {
			mutate(copy)
			if copy.Labels == nil {
				copy.Labels = map[string]string{}
			}
			copy.Labels[lakeTLSCopyLabel] = "true"
			copy.Labels[lakeTLSOwnerLabel] = owner
			copy.Type = corev1.SecretTypeTLS
			copy.Data = map[string][]byte{
				corev1.TLSCertKey:       source.Data[corev1.TLSCertKey],
				corev1.TLSPrivateKeyKey: source.Data[corev1.TLSPrivateKeyKey],
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("copy certificate for %s: %w", host, err)
		}
	}
	if err := deleteLakeTLSCopies(ctx, c, namespace, ingressName, keep); err != nil {
		return nil, err
	}
	return tls, nil
}

// deleteLakeTLSCopies deletes the certificate copies made for an Ingress, except keep.
func deleteLakeTLSCopies(ctx context.Context, c client.Client, namespace, ingressName string, keep map[string]bool) error {
	var copies corev1.SecretList
	if err := c.List(ctx, &copies, client.InNamespace(namespace), client.MatchingLabels{
		lakeTLSCopyLabel: "true", lakeTLSOwnerLabel: lakeTLSOwnerValue("ingress", namespace+"/"+ingressName),
	}); err != nil {
		return err
	}
	for i := range copies.Items {
		if !keep[copies.Items[i].Name] {
			if err := c.Delete(ctx, &copies.Items[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// isLakeCertificateSecret is whether a Secret may be a Lake certificate whose renewal must be
// copied again: ingressTLSSecretName names end in -tls.
func isLakeCertificateSecret(object client.Object) bool {
	return strings.HasSuffix(object.GetName(), "-tls") && object.GetLabels()[lakeTLSCopyLabel] == ""
}
