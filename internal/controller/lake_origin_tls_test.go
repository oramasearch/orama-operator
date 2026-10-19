/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// originTLSLake is a production-shaped Lake: a region host, the apex, a preview wildcard and the
// lake.live zone's token Secret.
func originTLSLake() (*oramav1alpha1.Lake, *corev1.Secret) {
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake", Image: "oramasearch/lake:test", IngressEnabled: true,
			Host: "eu.lake.live", ApexHost: "lake.live", PreviewWildcard: "preview.lake.live",
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token-lake-live"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-api-token-lake-live", Namespace: "lake"},
		Data:       map[string][]byte{"token": []byte("t"), "zoneID": []byte("zone-lake-live"), "accountID": []byte("acct")},
	}
	return lake, secret
}

func getIngress(t *testing.T, c client.Client, ns, name string) *networkingv1.Ingress {
	t.Helper()
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &ing); err != nil {
		t.Fatalf("ingress %s/%s: %v", ns, name, err)
	}
	return &ing
}

func tlsSecretsByHost(ing *networkingv1.Ingress) map[string]string {
	out := map[string]string{}
	for _, entry := range ing.Spec.TLS {
		for _, host := range entry.Hosts {
			out[host] = entry.SecretName
		}
	}
	return out
}

func setEdgeStatus(t *testing.T, c client.Client, lake *oramav1alpha1.Lake, mode string, pulls bool) {
	t.Helper()
	var policy oramav1alpha1.EdgeZonePolicy
	key := types.NamespacedName{Namespace: lakeNamespace(lake), Name: lakeEdgeName(lake)}
	if err := c.Get(context.Background(), key, &policy); err != nil {
		t.Fatalf("edge zone policy: %v", err)
	}
	policy.Status.OriginEncryption = mode
	policy.Status.AuthenticatedOriginPulls = pulls
	policy.Status.OriginClientCAPEM = cloudflareOriginPullCA
	if err := c.Status().Update(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
}

// Every host of the Lake's own Ingress (host and apex) and both wildcards get a certificate from
// the Lake's Issuer, whose DNS-01 credential is the Lake's dnsSecretRef; the zone policy names the
// zone of that Secret.
func TestLakeOriginTLSCoversEveryHostAndWildcard(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake, secret := originTLSLake()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}, &oramav1alpha1.EdgeZonePolicy{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	reconcileLake(t, c, s, "shared")

	main := getIngress(t, c, "lake", "lake-shared")
	got := tlsSecretsByHost(main)
	if len(got) != 2 || got["eu.lake.live"] != "eu.lake.live-tls" || got["lake.live"] != "lake.live-tls" {
		t.Errorf("main ingress TLS = %+v", main.Spec.TLS)
	}
	if main.Annotations[annotationCertManagerIssuer] != "lake-shared-letsencrypt" {
		t.Errorf("main ingress issuer = %+v", main.Annotations)
	}

	for name, host := range map[string]string{
		"lake-shared-apps-wildcard":    "*.eu.lake.live",
		"lake-shared-preview-wildcard": "*.preview.lake.live",
	} {
		ing := getIngress(t, c, "lake", name)
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != host {
			t.Errorf("%s rules = %+v", name, ing.Spec.Rules)
		}
		if tlsSecretsByHost(ing)[host] != ingressTLSSecretName(host) || ing.Annotations[annotationCertManagerIssuer] != "lake-shared-letsencrypt" {
			t.Errorf("%s TLS = %+v annotations %+v", name, ing.Spec.TLS, ing.Annotations)
		}
		if ing.Annotations[annotationNginxPermanentRedirect] != "https://eu.lake.live/" {
			t.Errorf("%s does not send unknown names to the Lake: %+v", name, ing.Annotations)
		}
	}

	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-shared-letsencrypt"}, issuer); err != nil {
		t.Fatalf("issuer: %v", err)
	}
	solvers, _, _ := unstructured.NestedSlice(issuer.Object, "spec", "acme", "solvers")
	if len(solvers) != 1 {
		t.Fatalf("solvers = %v", solvers)
	}
	ref, _, _ := unstructured.NestedStringMap(solvers[0].(map[string]any), "dns01", "cloudflare", "apiTokenSecretRef")
	if ref["name"] != "cloudflare-api-token-lake-live" || ref["key"] != "token" {
		t.Errorf("issuer credential = %v, want the Lake's dnsSecretRef token", ref)
	}

	var policy oramav1alpha1.EdgeZonePolicy
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &policy); err != nil {
		t.Fatalf("edge zone policy: %v", err)
	}
	var provider oramav1alpha1.EdgeProvider
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &provider); err != nil {
		t.Fatalf("edge provider: %v", err)
	}
	if policy.Spec.ZoneID != "zone-lake-live" || policy.Spec.ProviderRef.Name != "lake-shared-edge" ||
		provider.Spec.Cloudflare == nil || provider.Spec.Cloudflare.APITokenSecretRef.Name != "cloudflare-api-token-lake-live" {
		t.Errorf("policy %+v provider %+v", policy.Spec, provider.Spec)
	}
}

// The redirect and the client-certificate requirement follow the zone: neither before the zone is
// observed or while it is flexible (a redirect would loop), the redirect at full, and the
// requirement once the edge presents its certificate.
func TestLakeIngressFollowsTheZone(t *testing.T) {
	s := newLakeScheme(t)
	lake, secret := originTLSLake()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}, &oramav1alpha1.EdgeZonePolicy{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	reconcileLake(t, c, s, "shared")

	names := []string{"lake-shared", "lake-shared-apps-wildcard", "lake-shared-preview-wildcard"}
	check := func(step, redirect string, pulls bool) {
		t.Helper()
		for _, name := range names {
			ing := getIngress(t, c, "lake", name)
			if ing.Annotations[annotationNginxSSLRedirect] != redirect {
				t.Errorf("%s: %s redirect = %q, want %q", step, name, ing.Annotations[annotationNginxSSLRedirect], redirect)
			}
			_, has := ing.Annotations[annotationNginxAuthTLSSecret]
			if has != pulls || (pulls && (ing.Annotations[annotationNginxAuthTLSSecret] != "lake/lake-shared-edge-client-ca" ||
				ing.Annotations[annotationNginxAuthTLSVerifyClient] != "on")) {
				t.Errorf("%s: %s client certificate annotations = %+v, want required=%v", step, name, ing.Annotations, pulls)
			}
		}
	}
	check("unobserved", "false", false)

	setEdgeStatus(t, c, lake, "flexible", true)
	reconcileLake(t, c, s, "shared")
	check("flexible", "false", false)

	setEdgeStatus(t, c, lake, "full", false)
	reconcileLake(t, c, s, "shared")
	check("full", "true", false)

	setEdgeStatus(t, c, lake, "strict", true)
	reconcileLake(t, c, s, "shared")
	check("strict with pulls", "true", true)

	// A wildcard the edge has no certificate for (two levels below the apex) keeps that wildcard's
	// redirect off: a visitor sent to HTTPS could not follow. The Lake's own hosts keep theirs.
	var policy oramav1alpha1.EdgeZonePolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.Hosts = []oramav1alpha1.EdgeZoneHostStatus{{Name: "*.eu.lake.live", Edge: "TLS handshake failed"}}
	if err := c.Status().Update(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "shared")
	if got := getIngress(t, c, "lake", "lake-shared-apps-wildcard").Annotations[annotationNginxSSLRedirect]; got != "false" {
		t.Errorf("uncovered wildcard redirect = %q, want false", got)
	}
	if got := getIngress(t, c, "lake", "lake-shared").Annotations[annotationNginxSSLRedirect]; got != "true" {
		t.Errorf("covered host redirect = %q, want true", got)
	}
	var ca corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge-client-ca"}, &ca); err != nil {
		t.Fatalf("edge CA Secret: %v", err)
	}
	if string(ca.Data["ca.crt"]) != cloudflareOriginPullCA || !strings.Contains(cloudflareOriginPullCA, "BEGIN CERTIFICATE") {
		t.Error("edge CA Secret does not hold the edge's client CA")
	}
}

// A Lake host with no certificate source is refused, not served over HTTP only.
func TestLakeIngressWithoutACertificateSourceIsRefused(t *testing.T) {
	s := newLakeScheme(t)
	lake, _ := originTLSLake()
	lake.Spec.DNSSecretRef = nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	r := &LakeReconciler{Client: c, Scheme: s}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shared"}})
	if err == nil || !strings.Contains(err.Error(), "dnsSecretRef") {
		t.Fatalf("err = %v, want the missing dnsSecretRef named", err)
	}
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "lake-shared"}, &ing); !apierrors.IsNotFound(err) {
		t.Errorf("an HTTP-only ingress was written: %v", err)
	}
}

// An app under the apps wildcard uses the wildcard's certificate, copied next to its Ingress; an
// app with its own host gets a Certificate in the Lake namespace from the Lake's Issuer.
func TestLakeAppIngressGetsOriginTLS(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake, secret := originTLSLake()
	wildcardSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wildcard.eu.lake.live-tls", Namespace: "lake"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	app := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "lake", UID: "app-uid"}, Spec: oramav1alpha1.LakeAppSpec{LakeRef: "shared"}}
	policy := &oramav1alpha1.EdgeZonePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-shared-edge", Namespace: "lake"},
		Status:     oramav1alpha1.EdgeZonePolicyStatus{OriginEncryption: "strict", AuthenticatedOriginPulls: true, OriginClientCAPEM: cloudflareOriginPullCA},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret, wildcardSecret, app, policy).
		WithStatusSubresource(&networkingv1.Ingress{}).Build()
	r := &LakeAppReconciler{Client: c, Scheme: s}
	ns := "ws-1"
	workload := lakeAppWorkloadObjectName(app)

	if err := r.reconcileLakeAppIngress(ctx, lake, app, ns, "shop.eu.lake.live"); err != nil {
		t.Fatalf("reconcile app ingress: %v", err)
	}
	ing := getIngress(t, c, ns, workload)
	copyName := tlsSecretsByHost(ing)["shop.eu.lake.live"]
	if copyName == "" {
		t.Fatalf("app ingress TLS = %+v", ing.Spec.TLS)
	}
	var copied corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: copyName}, &copied); err != nil {
		t.Fatalf("wildcard certificate not copied next to the app: %v", err)
	}
	if string(copied.Data[corev1.TLSCertKey]) != "cert" || copied.Type != corev1.SecretTypeTLS {
		t.Errorf("copy = %+v", copied)
	}
	if ing.Annotations[annotationNginxSSLRedirect] != "true" || ing.Annotations[annotationNginxAuthTLSSecret] != ns+"/"+workload+"-edge-client-ca" {
		t.Errorf("app ingress annotations = %+v", ing.Annotations)
	}
	if _, found := ing.Annotations["nginx.ingress.kubernetes.io/auth-url"]; !found {
		t.Error("the app's Lake auth annotations were lost")
	}

	// Its own host: a Certificate in the Lake namespace, owned by the app; the wildcard copy goes.
	if err := r.reconcileLakeAppIngress(ctx, lake, app, ns, "shop.example.com"); err != nil {
		t.Fatalf("reconcile app ingress: %v", err)
	}
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "shop.example.com-tls"}, certificate); err != nil {
		t.Fatalf("certificate: %v", err)
	}
	dnsNames, _, _ := unstructured.NestedStringSlice(certificate.Object, "spec", "dnsNames")
	issuer, _, _ := unstructured.NestedString(certificate.Object, "spec", "issuerRef", "name")
	if len(dnsNames) != 1 || dnsNames[0] != "shop.example.com" || issuer != "lake-shared-letsencrypt" ||
		len(certificate.GetOwnerReferences()) != 1 || certificate.GetOwnerReferences()[0].Name != "shop" {
		t.Errorf("certificate = %+v", certificate.Object)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: copyName}, &copied); !apierrors.IsNotFound(err) {
		t.Errorf("the old host's copy remains: %v", err)
	}

	// Back under the wildcard: its own Certificate is deleted.
	if err := r.reconcileLakeAppIngress(ctx, lake, app, ns, "shop.eu.lake.live"); err != nil {
		t.Fatalf("reconcile app ingress: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "shop.example.com-tls"}, certificate); !apierrors.IsNotFound(err) {
		t.Errorf("certificate for a host the app no longer has remains: %v", err)
	}
}

// A preview uses the preview wildcard's certificate; a named host and each custom domain get their
// own Certificate.
func TestLakeEnvironmentIngressGetsOriginTLS(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake, secret := originTLSLake()
	environment := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "lake", UID: "environment-uid"},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef: "shared", Repository: "repo-1", Environment: "checkout", Kind: oramav1alpha1.LakeEnvironmentKindNamed,
			Host: "checkout.lake.live", Domains: []oramav1alpha1.LakeEnvironmentDomain{{Host: "shop.lake.live"}},
			Image: testEnvironmentImage, Replicas: 1, Port: 8080,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, secret, environment).
		WithStatusSubresource(environment, &networkingv1.Ingress{}).Build()
	r := &LakeEnvironmentReconciler{Client: c, Scheme: scheme}
	namespace := lakeEnvironmentNamespace(environment)
	if _, err := r.reconcileEnvironmentIngress(ctx, lake, environment, namespace, "checkout.lake.live"); err != nil {
		t.Fatalf("reconcile environment ingress: %v", err)
	}
	ing := getIngress(t, c, namespace, environmentWorkloadName(environment))
	got := tlsSecretsByHost(ing)
	if len(got) != 2 || got["checkout.lake.live"] == "" || got["shop.lake.live"] == "" {
		t.Errorf("environment ingress TLS = %+v", ing.Spec.TLS)
	}
	for _, host := range []string{"checkout.lake.live", "shop.lake.live"} {
		certificate := &unstructured.Unstructured{}
		certificate.SetGroupVersionKind(certManagerCertificateGVK)
		if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: ingressTLSSecretName(host)}, certificate); err != nil {
			t.Errorf("certificate for %s: %v", host, err)
		}
	}
	if ing.Annotations[annotationNginxSSLRedirect] != "false" {
		t.Errorf("redirect before the zone is observed: %+v", ing.Annotations)
	}

	// A preview host is one label under the preview wildcard: the wildcard's certificate.
	if got := lakeWildcardFor(lake, "7ac4c59b92e4.preview.lake.live"); got != "*.preview.lake.live" {
		t.Errorf("preview wildcard = %q", got)
	}
	if got := lakeWildcardFor(lake, "a.b.preview.lake.live"); got != "" {
		t.Errorf("two labels under a wildcard are not covered by it, got %q", got)
	}
}
