// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// originTLSLake is a production-shaped Lake: a region host, the apex, a preview wildcard and the
// lakelive.example zone's token Secret.
func originTLSLake() (*oramav1alpha1.Lake, *corev1.Secret) {
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake", Image: "oramasearch/lake:test", IngressEnabled: true,
			Host: "eu.lakelive.example", ApexHost: "lakelive.example", PreviewWildcard: "preview.lakelive.example",
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
	if len(got) != 2 || got["eu.lakelive.example"] != "eu.lakelive.example-tls" || got["lakelive.example"] != "lakelive.example-tls" {
		t.Errorf("main ingress TLS = %+v", main.Spec.TLS)
	}
	if main.Annotations[annotationCertManagerIssuer] != "lake-shared-letsencrypt" {
		t.Errorf("main ingress issuer = %+v", main.Annotations)
	}

	for name, host := range map[string]string{
		"lake-shared-apps-wildcard":    "*.eu.lakelive.example",
		"lake-shared-preview-wildcard": "*.preview.lakelive.example",
	} {
		ing := getIngress(t, c, "lake", name)
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != host {
			t.Errorf("%s rules = %+v", name, ing.Spec.Rules)
		}
		if tlsSecretsByHost(ing)[host] != ingressTLSSecretName(host) || ing.Annotations[annotationCertManagerIssuer] != "lake-shared-letsencrypt" {
			t.Errorf("%s TLS = %+v annotations %+v", name, ing.Spec.TLS, ing.Annotations)
		}
		if ing.Annotations[annotationNginxPermanentRedirect] != "https://eu.lakelive.example/" {
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
	policy.Status.Hosts = []oramav1alpha1.EdgeZoneHostStatus{{Name: "*.eu.lakelive.example", Edge: "TLS handshake failed"}}
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

// issuerSolvers reads the Lake's Issuer's ACME solvers.
func issuerSolvers(t *testing.T, c client.Client, lake *oramav1alpha1.Lake) []any {
	t.Helper()
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: lakeNamespace(lake), Name: lakeOriginIssuerName(lake)}, issuer); err != nil {
		t.Fatalf("issuer: %v", err)
	}
	solvers, _, _ := unstructured.NestedSlice(issuer.Object, "spec", "acme", "solvers")
	return solvers
}

// A Lake without a dnsSecretRef (an install that brings its own DNS) is no longer refused: its
// hosts are issued through HTTP-01 by the Lake's Issuer, through the Lake's ingress class. It has
// no wildcard Ingress (a wildcard needs DNS-01) and no zone policy (operator#632).
func TestLakeWithoutADNSCredentialIssuesThroughHTTP01(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake, _ := originTLSLake()
	lake.Spec.DNSSecretRef = nil
	lake.Spec.IngressClassName = "internal-nginx"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	reconcileLake(t, c, s, "shared")

	main := getIngress(t, c, "lake", "lake-shared")
	got := tlsSecretsByHost(main)
	if len(got) != 2 || got["eu.lakelive.example"] != "eu.lakelive.example-tls" || got["lakelive.example"] != "lakelive.example-tls" {
		t.Errorf("main ingress TLS = %+v", main.Spec.TLS)
	}
	if main.Annotations[annotationCertManagerIssuer] != "lake-shared-letsencrypt" {
		t.Errorf("main ingress issuer = %+v", main.Annotations)
	}
	solvers := issuerSolvers(t, c, lake)
	if len(solvers) != 1 {
		t.Fatalf("solvers = %v, want HTTP-01 alone", solvers)
	}
	class, _, _ := unstructured.NestedString(solvers[0].(map[string]any), "http01", "ingress", "ingressClassName")
	if class != "internal-nginx" {
		t.Errorf("HTTP-01 solver = %v, want the Lake's ingress class", solvers[0])
	}
	if _, found := solvers[0].(map[string]any)["dns01"]; found {
		t.Errorf("a DNS-01 solver without a credential: %v", solvers[0])
	}
	for _, name := range []string{"lake-shared-apps-wildcard", "lake-shared-preview-wildcard"} {
		var ing networkingv1.Ingress
		if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: name}, &ing); !apierrors.IsNotFound(err) {
			t.Errorf("%s exists without a DNS-01 credential: %v", name, err)
		}
	}
	var policy oramav1alpha1.EdgeZonePolicy
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &policy); !apierrors.IsNotFound(err) {
		t.Errorf("a zone policy without a credential: %v", err)
	}
	if main.Annotations[annotationNginxSSLRedirect] != "false" {
		t.Errorf("redirect without an observed edge: %+v", main.Annotations)
	}
}

// With a dnsSecretRef the solver is chosen per host: until the zone's name is observed, DNS-01
// alone for every name (as before HTTP-01 existed); once it is, DNS-01 for the zone and every name
// under it, wildcards included, and HTTP-01 only for names outside it. A name in the zone is never
// moved off DNS-01: cert-manager always prefers a solver whose dnsZones selector matches.
func TestLakeIssuerChoosesTheSolverPerHost(t *testing.T) {
	s := newLakeScheme(t)
	lake, secret := originTLSLake()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}, &oramav1alpha1.EdgeZonePolicy{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	reconcileLake(t, c, s, "shared")

	solvers := issuerSolvers(t, c, lake)
	if len(solvers) != 1 {
		t.Fatalf("zone unknown: solvers = %v, want DNS-01 alone", solvers)
	}
	if _, found := solvers[0].(map[string]any)["selector"]; found {
		t.Errorf("zone unknown: DNS-01 is limited by a selector: %v", solvers[0])
	}

	var policy oramav1alpha1.EdgeZonePolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.Zone = "Lake.Live."
	if err := c.Status().Update(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, s, "shared")
	solvers = issuerSolvers(t, c, lake)
	if len(solvers) != 2 {
		t.Fatalf("zone known: solvers = %v, want DNS-01 for the zone and HTTP-01 for the rest", solvers)
	}
	dns01 := solvers[0].(map[string]any)
	zones, _, _ := unstructured.NestedStringSlice(dns01, "selector", "dnsZones")
	ref, _, _ := unstructured.NestedStringMap(dns01, "dns01", "cloudflare", "apiTokenSecretRef")
	if len(zones) != 1 || zones[0] != "lakelive.example" || ref["name"] != "cloudflare-api-token-lake-live" || ref["key"] != "token" {
		t.Errorf("DNS-01 solver = %v", dns01)
	}
	http01 := solvers[1].(map[string]any)
	if _, found := http01["selector"]; found {
		t.Errorf("HTTP-01 has a selector, so it could compete with DNS-01: %v", http01)
	}
	if class, _, _ := unstructured.NestedString(http01, "http01", "ingress", "ingressClassName"); class != "nginx" {
		t.Errorf("HTTP-01 solver = %v", http01)
	}
	// Every Lake host and both wildcards are in the zone, so they stay on DNS-01.
	for _, host := range []string{"eu.lakelive.example", "lakelive.example", "*.eu.lakelive.example", "*.preview.lakelive.example"} {
		if !dnsZoneCovers("lakelive.example", host) {
			t.Errorf("%s is not solved by DNS-01", host)
		}
	}
	if dnsZoneCovers("lakelive.example", "shop.example.com") || dnsZoneCovers("lakelive.example", "notlakelive.example") {
		t.Error("a name outside the zone is solved by DNS-01")
	}
}

// dnsZoneCovers is cert-manager's dnsZones rule: the zone itself and every name under it.
func dnsZoneCovers(zone, name string) bool {
	name = strings.TrimPrefix(name, "*.")
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// An app under the apps wildcard uses the wildcard's certificate, copied next to its Ingress; an
// app with its own host gets a Certificate in the Lake namespace from the Lake's Issuer.
func TestLakeAppIngressGetsOriginTLS(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake, secret := originTLSLake()
	wildcardSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wildcard.eu.lakelive.example-tls", Namespace: "lake"},
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

	if err := r.reconcileLakeAppIngress(ctx, lake, app, ns, "shop.eu.lakelive.example"); err != nil {
		t.Fatalf("reconcile app ingress: %v", err)
	}
	ing := getIngress(t, c, ns, workload)
	copyName := tlsSecretsByHost(ing)["shop.eu.lakelive.example"]
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

	// The app's Secret pass keeps the Ingress's certificate copy and edge client CA. Deleting them
	// there made each pass delete and recreate them, and the edge answered 526 in between.
	if err := r.deleteStaleLakeAppSecrets(ctx, app, ns, map[string]bool{}); err != nil {
		t.Fatalf("delete stale app secrets: %v", err)
	}
	for _, name := range []string{copyName, workload + "-edge-client-ca"} {
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Secret{}); err != nil {
			t.Errorf("the app's Secret pass deleted %s: %v", name, err)
		}
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
	if err := r.reconcileLakeAppIngress(ctx, lake, app, ns, "shop.eu.lakelive.example"); err != nil {
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
			Host: "checkout.lakelive.example", Domains: []oramav1alpha1.LakeEnvironmentDomain{{Host: "shop.lakelive.example"}},
			Image: testEnvironmentImage, Replicas: 1, Port: 8080,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, secret, environment).
		WithStatusSubresource(environment, &networkingv1.Ingress{}).Build()
	r := &LakeEnvironmentReconciler{Client: c, Scheme: scheme}
	namespace := lakeEnvironmentNamespace(environment)
	if _, err := r.reconcileEnvironmentIngress(ctx, lake, environment, namespace, "checkout.lakelive.example"); err != nil {
		t.Fatalf("reconcile environment ingress: %v", err)
	}
	ing := getIngress(t, c, namespace, environmentWorkloadName(environment))
	got := tlsSecretsByHost(ing)
	if len(got) != 2 || got["checkout.lakelive.example"] == "" || got["shop.lakelive.example"] == "" {
		t.Errorf("environment ingress TLS = %+v", ing.Spec.TLS)
	}
	for _, host := range []string{"checkout.lakelive.example", "shop.lakelive.example"} {
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
	if got := lakeWildcardFor(lake, "7ac4c59b92e4.preview.lakelive.example"); got != "*.preview.lakelive.example" {
		t.Errorf("preview wildcard = %q", got)
	}
	if got := lakeWildcardFor(lake, "a.b.preview.lakelive.example"); got != "" {
		t.Errorf("two labels under a wildcard are not covered by it, got %q", got)
	}
}

// Apps and environments of a Lake without a dnsSecretRef get certificates (operator#632): each host
// its own Certificate from the Lake's Issuer, which solves it through HTTP-01. A preview host is
// left on HTTP: no wildcard covers it, and one certificate per preview would exhaust Let's
// Encrypt's weekly limit for the Lake's domain.
func TestLakeWithoutADNSCredentialIssuesAppAndEnvironmentHosts(t *testing.T) {
	ctx := context.Background()
	lake, _ := originTLSLake()
	lake.Spec.DNSSecretRef = nil

	s := newLakeScheme(t)
	app := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "lake", UID: "app-uid"}, Spec: oramav1alpha1.LakeAppSpec{LakeRef: "shared"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, app).WithStatusSubresource(&networkingv1.Ingress{}).Build()
	r := &LakeAppReconciler{Client: c, Scheme: s}
	if err := r.reconcileLakeAppIngress(ctx, lake, app, "ws-1", "shop.eu.lakelive.example"); err != nil {
		t.Fatalf("reconcile app ingress: %v", err)
	}
	if tlsSecretsByHost(getIngress(t, c, "ws-1", lakeAppWorkloadObjectName(app)))["shop.eu.lakelive.example"] == "" {
		t.Error("the app's Ingress has no TLS entry")
	}
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "shop.eu.lakelive.example-tls"}, certificate); err != nil {
		t.Fatalf("app certificate: %v", err)
	}
	if issuer, _, _ := unstructured.NestedString(certificate.Object, "spec", "issuerRef", "name"); issuer != "lake-shared-letsencrypt" {
		t.Errorf("app certificate issuer = %q", issuer)
	}

	scheme := newLakeEnvironmentScheme(t)
	preview := &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-7", Namespace: "lake", UID: "preview-uid"},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef: "shared", Repository: "repo-1", Environment: "pr-7", Kind: oramav1alpha1.LakeEnvironmentKindPreview,
			Change: "7ac4c59b92e4", Domains: []oramav1alpha1.LakeEnvironmentDomain{{Host: "shop.example.com"}},
			Image: testEnvironmentImage, Replicas: 1, Port: 8080,
		},
	}
	ec := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, preview).WithStatusSubresource(preview, &networkingv1.Ingress{}).Build()
	er := &LakeEnvironmentReconciler{Client: ec, Scheme: scheme}
	namespace := lakeEnvironmentNamespace(preview)
	previewHost := "7ac4c59b92e4.preview.lakelive.example"
	if _, err := er.reconcileEnvironmentIngress(ctx, lake, preview, namespace, previewHost); err != nil {
		t.Fatalf("reconcile environment ingress: %v", err)
	}
	got := tlsSecretsByHost(getIngress(t, ec, namespace, environmentWorkloadName(preview)))
	if len(got) != 1 || got["shop.example.com"] == "" {
		t.Errorf("preview ingress TLS = %+v, want the custom domain only", got)
	}
	if err := ec.Get(ctx, types.NamespacedName{Namespace: "lake", Name: ingressTLSSecretName("shop.example.com")}, certificate); err != nil {
		t.Errorf("custom domain certificate: %v", err)
	}
	if err := ec.Get(ctx, types.NamespacedName{Namespace: "lake", Name: ingressTLSSecretName(previewHost)}, certificate); !apierrors.IsNotFound(err) {
		t.Errorf("a per-preview certificate was requested: %v", err)
	}
}

// certManagerTestCRDs are cert-manager's Issuer and Certificate, schema-free, for envtests that
// reconcile Lake hosts: every cluster runs cert-manager (both installation paths), and since
// operator#632 every Lake host is issued, with or without a dnsSecretRef.
func certManagerTestCRDs() []*apiextensionsv1.CustomResourceDefinition {
	preserve := true
	crd := func(plural, singular, kind string) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + ".cert-manager.io"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "cert-manager.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Singular: singular, Kind: kind, ListKind: kind + "List"},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name: "v1", Served: true, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
							"status": {Type: "object", XPreserveUnknownFields: &preserve},
						},
					}},
				}},
			},
		}
	}
	return []*apiextensionsv1.CustomResourceDefinition{
		crd("issuers", "issuer", "Issuer"),
		crd("certificates", "certificate", "Certificate"),
	}
}

// A Lake a set manages points its zone policy at the zone's distributed origin-pull Secret, which
// the set copies before it applies the Lake; a Lake no set manages leaves it unset, so its
// cluster's operator keeps its own (operator#633).
func TestASetManagedLakeUsesTheDistributedOriginPullSecret(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	for _, managed := range []bool{false, true} {
		lake, secret := originTLSLake()
		if managed {
			lake.Labels = map[string]string{managedBySetLabel: "LakeSet.lake-prod"}
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret).Build()
		if err := reconcileLakeEdgePolicy(ctx, c, s, lake, nil); err != nil {
			t.Fatal(err)
		}
		var policy oramav1alpha1.EdgeZonePolicy
		if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-shared-edge"}, &policy); err != nil {
			t.Fatal(err)
		}
		ref := policy.Spec.OriginPullSecretRef
		switch {
		case managed && (ref == nil || ref.Name != originPullSecretName("zone-lake-live")):
			t.Errorf("a set-managed Lake: ref = %+v", ref)
		case !managed && ref != nil:
			t.Errorf("a Lake no set manages: ref = %+v", ref)
		}
	}
}
