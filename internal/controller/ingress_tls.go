/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Origin TLS for Cloud v1's Ingresses (operator#457).
// Every stack owns one namespaced cert-manager Issuer (Let's Encrypt, DNS-01 through Cloudflare);
// every Ingress the stack renders calls applyIngressTLS inside its CreateOrUpdate mutation, after
// its rules are set. cert-manager's ingress-shim then creates one Certificate per TLS entry and
// renews it; the operator never writes the Certificate or its Secret.

// certManagerIssuerGVK is cert-manager's namespaced Issuer, provisioned unstructured (like CNPG's
// Cluster) so the operator does not depend on cert-manager's Go types.
var certManagerIssuerGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"}

const (
	// letsEncryptDirectory is Let's Encrypt's production ACME directory.
	letsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

	annotationCertManagerIssuer        = "cert-manager.io/issuer"
	annotationCertManagerClusterIssuer = "cert-manager.io/cluster-issuer"
	annotationCertManagerIssuerKind    = "cert-manager.io/issuer-kind"
	annotationCertManagerIssuerGroup   = "cert-manager.io/issuer-group"
	annotationNginxSSLRedirect         = "nginx.ingress.kubernetes.io/ssl-redirect"
)

// originIssuerName is the stack's cert-manager Issuer.
func originIssuerName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-letsencrypt" }

// originIssuerAccountSecret is the Secret cert-manager generates for the stack's ACME account key.
func originIssuerAccountSecret(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-letsencrypt-account"
}

// errNoOriginIssuerCredential is returned while a stack renders an Ingress but has no Cloudflare
// token for DNS-01. There is no HTTP-only fallback: the reconcile fails and says why.
var errNoOriginIssuerCredential = errors.New("origin TLS needs spec.cloudflare.apiTokenSecretRef (key \"token\") for the Let's Encrypt DNS-01 Issuer")

// stackIngressIssuer is the Issuer a stack's Ingress builders pass to applyIngressTLS. Call it
// before CreateOrUpdate, so a missing credential fails the reconcile without touching the Ingress.
func stackIngressIssuer(oc *oramav1alpha1.OramaCloudV1) (string, error) {
	if oc.Spec.Cloudflare.APITokenSecretRef == nil || oc.Spec.Cloudflare.APITokenSecretRef.Name == "" {
		return "", errNoOriginIssuerCredential
	}
	return originIssuerName(oc), nil
}

// reconcileOriginIssuer keeps the stack's Let's Encrypt Issuer while the stack has a Cloudflare
// token, and removes it otherwise. The DNS-01 solver uses the same token Secret the stack's
// Cloudflare resources use (key `token`), whatever spec.cloudflare.enabled says.
func (r *OramaCloudV1Reconciler) reconcileOriginIssuer(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	issuer.SetName(originIssuerName(oc))
	issuer.SetNamespace(oc.Namespace)
	if _, err := stackIngressIssuer(oc); err != nil {
		// Removing the token removes the Issuer, but existing Ingresses keep their
		// cert-manager.io/issuer annotation: their certificates stay served until expiry and renewal
		// fails until the token returns. Meanwhile every Ingress builder fails on the same missing
		// token, so the stack reports Degraded and says why.
		return deleteIfExists(ctx, r.Client, issuer)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(labelsFor(oc, "origin-tls"))
		issuer.Object["spec"] = map[string]any{
			"acme": map[string]any{
				"server":              letsEncryptDirectory,
				"privateKeySecretRef": map[string]any{"name": originIssuerAccountSecret(oc)},
				"solvers": []any{map[string]any{
					"dns01": map[string]any{
						"cloudflare": map[string]any{
							"apiTokenSecretRef": map[string]any{
								"name": oc.Spec.Cloudflare.APITokenSecretRef.Name,
								"key":  "token",
							},
						},
					},
				}},
			},
		}
		return controllerutil.SetControllerReference(oc, issuer, r.Scheme)
	})
	return err
}

// applyIngressTLS requests a certificate from the namespaced Issuer issuerName for every host of
// ing's rules, one TLS entry and one Secret per host (ingressTLSSecretName). It merges the
// cert-manager and nginx redirect annotations into the existing ones, drops any other issuer
// reference, and replaces spec.tls. Call it last in the mutation, so it sees the final rules: a
// host added or removed changes the requested certificates, never the other hosts' Secrets.
func applyIngressTLS(ing *networkingv1.Ingress, issuerName string) error {
	if issuerName == "" {
		return fmt.Errorf("ingress %s: origin TLS needs an issuer", ing.Name)
	}
	if len(ing.Spec.Rules) == 0 {
		return fmt.Errorf("ingress %s: origin TLS needs at least one rule", ing.Name)
	}
	for i, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			return fmt.Errorf("ingress %s: rule %d has no host, so no certificate can cover it", ing.Name, i)
		}
	}
	ing.Spec.TLS = originTLS(ing.Spec.Rules)

	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	delete(ing.Annotations, annotationCertManagerClusterIssuer)
	delete(ing.Annotations, annotationCertManagerIssuerKind)
	delete(ing.Annotations, annotationCertManagerIssuerGroup)
	ing.Annotations[annotationCertManagerIssuer] = issuerName
	ing.Annotations[annotationNginxSSLRedirect] = "true"
	return nil
}

// originTLS is the spec.tls applyIngressTLS writes for rules: one entry per distinct host, in rule
// order, each naming ingressTLSSecretName(host). ingressSpec uses it too, so a built spec carries the
// TLS its Ingress is written with.
func originTLS(rules []networkingv1.IngressRule) []networkingv1.IngressTLS {
	var tls []networkingv1.IngressTLS
	seen := map[string]bool{}
	for _, rule := range rules {
		if rule.Host == "" || seen[rule.Host] {
			continue
		}
		seen[rule.Host] = true
		tls = append(tls, networkingv1.IngressTLS{Hosts: []string{rule.Host}, SecretName: ingressTLSSecretName(rule.Host)})
	}
	return tls
}

// ingressTLSSecretName is the Secret holding host's certificate: `<host>-tls`, with `*` spelled
// `wildcard`. It depends on the host alone, so every Ingress serving a host (for example the
// /metrics deny companions, #314) names the same Secret. A name over 253 characters keeps a
// 240-character prefix and adds the first 8 hex characters of the host's SHA-256.
func ingressTLSSecretName(host string) string {
	base := strings.ReplaceAll(host, "*", "wildcard")
	if name := base + "-tls"; len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(host))
	prefix := strings.TrimRight(base[:240], "-.")
	return prefix + "-" + hex.EncodeToString(sum[:])[:8] + "-tls"
}
