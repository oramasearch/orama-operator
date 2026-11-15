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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// assertIngressTLS is the check every operator Ingress builder's test runs on the rendered object:
// the issuer and redirect annotations are set and every rule host has exactly one TLS entry, whose
// Secret is the host's ingressTLSSecretName.
func assertIngressTLS(t *testing.T, ing *networkingv1.Ingress, issuer string) {
	t.Helper()
	if got := ing.Annotations[annotationCertManagerIssuer]; got != issuer {
		t.Errorf("%s: %s = %q, want %q", ing.Name, annotationCertManagerIssuer, got, issuer)
	}
	for _, other := range []string{annotationCertManagerClusterIssuer, annotationCertManagerIssuerKind, annotationCertManagerIssuerGroup} {
		if v, ok := ing.Annotations[other]; ok {
			t.Errorf("%s: contradictory issuer annotation %s=%q remains", ing.Name, other, v)
		}
	}
	if got := ing.Annotations[annotationNginxSSLRedirect]; got != "true" {
		t.Errorf("%s: %s = %q, want \"true\"", ing.Name, annotationNginxSSLRedirect, got)
	}
	secretFor := map[string]string{}
	for _, entry := range ing.Spec.TLS {
		if entry.SecretName == "" {
			t.Errorf("%s: TLS entry %v has no Secret", ing.Name, entry.Hosts)
		}
		for _, host := range entry.Hosts {
			if _, dup := secretFor[host]; dup {
				t.Errorf("%s: host %s is in two TLS entries", ing.Name, host)
			}
			secretFor[host] = entry.SecretName
		}
	}
	if len(ing.Spec.Rules) == 0 {
		t.Errorf("%s: no rules", ing.Name)
	}
	for _, rule := range ing.Spec.Rules {
		if got, want := secretFor[rule.Host], ingressTLSSecretName(rule.Host); got != want {
			t.Errorf("%s: host %s has TLS Secret %q, want %q", ing.Name, rule.Host, got, want)
		}
	}
}

func tlsTestIngress(hosts ...string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "v1-dev-dashboard", Namespace: "v1-dev"}}
	for _, h := range hosts {
		ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{Host: h})
	}
	return ing
}

func TestApplyIngressTLSCoversEveryHostOnce(t *testing.T) {
	ing := tlsTestIngress("app.oramadev.net", "legacy.oramadev.net", "app.oramadev.net")
	if err := applyIngressTLS(ing, "v1-dev-letsencrypt"); err != nil {
		t.Fatal(err)
	}
	assertIngressTLS(t, ing, "v1-dev-letsencrypt")
	want := []networkingv1.IngressTLS{
		{Hosts: []string{"app.oramadev.net"}, SecretName: "app.oramadev.net-tls"},
		{Hosts: []string{"legacy.oramadev.net"}, SecretName: "legacy.oramadev.net-tls"},
	}
	if !equality.Semantic.DeepEqual(ing.Spec.TLS, want) {
		t.Fatalf("TLS = %+v, want %+v (one entry per distinct host, in rule order)", ing.Spec.TLS, want)
	}

	// Removing a host removes its certificate request and keeps the other host's Secret.
	ing.Spec.Rules = ing.Spec.Rules[:1]
	if err := applyIngressTLS(ing, "v1-dev-letsencrypt"); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(ing.Spec.TLS, want[:1]) {
		t.Fatalf("after host removal TLS = %+v, want %+v", ing.Spec.TLS, want[:1])
	}
}

func TestApplyIngressTLSIsIdempotent(t *testing.T) {
	ing := tlsTestIngress("app.oramadev.net", "docs.oramadev.net")
	if err := applyIngressTLS(ing, "v1-dev-letsencrypt"); err != nil {
		t.Fatal(err)
	}
	first := ing.DeepCopy()
	if err := applyIngressTLS(ing, "v1-dev-letsencrypt"); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(first, ing) {
		t.Fatalf("a second apply changed the Ingress:\n%+v\n%+v", first, ing)
	}
}

func TestApplyIngressTLSKeepsOtherAnnotationsAndReplacesIssuer(t *testing.T) {
	ing := tlsTestIngress("app.oramadev.net")
	ing.Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/proxy-body-size": "64m",
		"nginx.ingress.kubernetes.io/server-snippet":  "location = /metrics { return 403; }",
		annotationCertManagerClusterIssuer:            "old-cluster-issuer",
		annotationCertManagerIssuerKind:               "OriginIssuer",
		annotationCertManagerIssuerGroup:              "cert-manager.k8s.cloudflare.com",
		annotationCertManagerIssuer:                   "old-issuer",
	}
	if err := applyIngressTLS(ing, "v1-dev-letsencrypt"); err != nil {
		t.Fatal(err)
	}
	assertIngressTLS(t, ing, "v1-dev-letsencrypt")
	for k, v := range map[string]string{
		"nginx.ingress.kubernetes.io/proxy-body-size": "64m",
		"nginx.ingress.kubernetes.io/server-snippet":  "location = /metrics { return 403; }",
	} {
		if ing.Annotations[k] != v {
			t.Errorf("annotation %s = %q, want it kept as %q", k, ing.Annotations[k], v)
		}
	}
	// An issuer change keeps the Secret names, so cert-manager reissues into the same Secrets.
	if ing.Spec.TLS[0].SecretName != "app.oramadev.net-tls" {
		t.Errorf("issuer change renamed the Secret to %q", ing.Spec.TLS[0].SecretName)
	}
}

func TestApplyIngressTLSRejectsIncompleteInput(t *testing.T) {
	for name, tc := range map[string]struct {
		ing    *networkingv1.Ingress
		issuer string
	}{
		"no issuer":  {tlsTestIngress("app.oramadev.net"), ""},
		"no rules":   {tlsTestIngress(), "v1-dev-letsencrypt"},
		"empty host": {tlsTestIngress("app.oramadev.net", ""), "v1-dev-letsencrypt"},
	} {
		if err := applyIngressTLS(tc.ing, tc.issuer); err == nil {
			t.Errorf("%s: applyIngressTLS accepted it; it must fail rather than render HTTP only", name)
		}
	}
}

func TestIngressTLSSecretName(t *testing.T) {
	if got := ingressTLSSecretName("app.oramadev.net"); got != "app.oramadev.net-tls" {
		t.Errorf("got %q", got)
	}
	if got := ingressTLSSecretName("*.apps.oramadev.net"); got != "wildcard.apps.oramadev.net-tls" {
		t.Errorf("wildcard: got %q", got)
	}

	// A host whose `<host>-tls` would pass 253 characters gets a hashed, still stable, name.
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 59)
	got := ingressTLSSecretName(long)
	if len(got) > 253 {
		t.Fatalf("len = %d, want <= 253", len(got))
	}
	if got != ingressTLSSecretName(long) {
		t.Error("long name is not deterministic")
	}
	if !strings.HasSuffix(got, "-tls") || strings.Contains(got, ".-") || strings.Contains(got, "--") {
		t.Errorf("long name %q is malformed", got)
	}
	other := long[:len(long)-1] + "e"
	if ingressTLSSecretName(other) == got {
		t.Error("two long hosts sharing a prefix got the same Secret")
	}
}

func originTLSStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Name, o.Namespace = "v1-dev", "v1-dev"
		// The dashboard renders nothing without a master key reference (#307).
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
		o.Spec.Cloudflare = oramav1alpha1.CloudflareSpec{
			Enabled: true, APITokenSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token"},
		}
		if mut != nil {
			mut(o)
		}
	})
}

func getOriginIssuer(t *testing.T, cl client.Client) (*unstructured.Unstructured, error) {
	t.Helper()
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	err := cl.Get(context.Background(), types.NamespacedName{Name: "v1-dev-letsencrypt", Namespace: "v1-dev"}, issuer)
	return issuer, err
}

func TestReconcileOriginIssuer(t *testing.T) {
	ctx := context.Background()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	oc := originTLSStack(nil)

	if err := r.reconcileOriginIssuer(ctx, oc); err != nil {
		t.Fatal(err)
	}
	issuer, err := getOriginIssuer(t, cl)
	if err != nil {
		t.Fatalf("get Issuer: %v", err)
	}
	acme, _, _ := unstructured.NestedMap(issuer.Object, "spec", "acme")
	if acme["server"] != letsEncryptDirectory {
		t.Errorf("server = %v, want Let's Encrypt production", acme["server"])
	}
	if name, _, _ := unstructured.NestedString(acme, "privateKeySecretRef", "name"); name != "v1-dev-letsencrypt-account" {
		t.Errorf("account key Secret = %q", name)
	}
	solvers, _, _ := unstructured.NestedSlice(acme, "solvers")
	if len(solvers) != 1 {
		t.Fatalf("solvers = %v, want one DNS-01 solver", solvers)
	}
	ref, _, _ := unstructured.NestedStringMap(solvers[0].(map[string]any), "dns01", "cloudflare", "apiTokenSecretRef")
	if ref["name"] != "cloudflare-api-token" || ref["key"] != "token" {
		t.Errorf("DNS-01 token ref = %v, want cloudflare-api-token/token", ref)
	}
	if owners := issuer.GetOwnerReferences(); len(owners) != 1 || owners[0].Name != "v1-dev" {
		t.Errorf("owner references = %v, want the stack", owners)
	}

	// A steady stack writes nothing.
	version := issuer.GetResourceVersion()
	if err := r.reconcileOriginIssuer(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if again, _ := getOriginIssuer(t, cl); again.GetResourceVersion() != version {
		t.Error("a second reconcile updated the unchanged Issuer")
	}

	// Without the token there is no Issuer.
	oc.Spec.Cloudflare.APITokenSecretRef = nil
	if err := r.reconcileOriginIssuer(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getOriginIssuer(t, cl); !apierrors.IsNotFound(err) {
		t.Errorf("Issuer after the token was removed: err = %v, want NotFound", err)
	}
}

// TestCloudV1IngressesRequestOriginTLS renders every Cloud v1 component that has a host through
// its real reconciler and checks the Ingress it leaves behind.
func TestCloudV1IngressesRequestOriginTLS(t *testing.T) {
	ctx := context.Background()
	builders := map[string]struct {
		enable func(*oramav1alpha1.OramaCloudV1)
		run    func(*OramaCloudV1Reconciler, *oramav1alpha1.OramaCloudV1) error
		host   string
	}{
		"v1-dev-dashboard": {
			enable: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app-dev.oramadev.net"}}
			},
			run: func(r *OramaCloudV1Reconciler, o *oramav1alpha1.OramaCloudV1) error {
				return r.reconcileDashboard(ctx, o)
			},
			host: "app-dev.oramadev.net",
		},
		"v1-dev-docs": {
			enable: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Docs = oramav1alpha1.ToggleWithHost{Enabled: true, Host: "docs-dev.oramadev.net"}
			},
			run:  func(r *OramaCloudV1Reconciler, o *oramav1alpha1.OramaCloudV1) error { return r.reconcileDocs(ctx, o) },
			host: "docs-dev.oramadev.net",
		},
		"v1-dev-writer": {
			enable: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine = oramav1alpha1.EngineSpec{Enabled: true, Writer: oramav1alpha1.WriterSpec{Host: "writer-dev.oramadev.net"}}
			},
			run: func(r *OramaCloudV1Reconciler, o *oramav1alpha1.OramaCloudV1) error {
				return r.reconcileEngineIngresses(ctx, o)
			},
			host: "writer-dev.oramadev.net",
		},
		"v1-dev-reader": {
			enable: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine = oramav1alpha1.EngineSpec{Enabled: true, Reader: oramav1alpha1.ReaderSpec{Host: "reader-dev.oramadev.net"}}
			},
			run: func(r *OramaCloudV1Reconciler, o *oramav1alpha1.OramaCloudV1) error {
				return r.reconcileEngineIngresses(ctx, o)
			},
			host: "reader-dev.oramadev.net",
		},
	}
	for name, b := range builders {
		t.Run(name, func(t *testing.T) {
			s := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
			oc := originTLSStack(b.enable)
			if err := b.run(r, oc); err != nil {
				t.Fatal(err)
			}
			ing := &networkingv1.Ingress{}
			if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: "v1-dev"}, ing); err != nil {
				t.Fatalf("get Ingress: %v", err)
			}
			assertIngressTLS(t, ing, "v1-dev-letsencrypt")
			if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != b.host {
				t.Errorf("rules = %+v, want the component's host only", ing.Spec.Rules)
			}
			if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != stackIngressClassName(oc) {
				t.Errorf("class = %v, want it left as the component renders it", ing.Spec.IngressClassName)
			}

			// Reconciling again changes nothing.
			version := ing.ResourceVersion
			if err := b.run(r, oc); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(ing), ing); err != nil {
				t.Fatal(err)
			}
			if ing.ResourceVersion != version {
				t.Error("a second reconcile updated the unchanged Ingress")
			}
		})
	}
}

func TestCloudV1IngressWithoutIssuerCredentialFails(t *testing.T) {
	ctx := context.Background()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	oc := originTLSStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Cloudflare.APITokenSecretRef = nil
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app-dev.oramadev.net"}}
	})
	if err := r.reconcileDashboard(ctx, oc); err == nil || !strings.Contains(err.Error(), "apiTokenSecretRef") {
		t.Fatalf("err = %v, want the missing DNS-01 credential named", err)
	}
	ing := &networkingv1.Ingress{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "v1-dev-dashboard", Namespace: "v1-dev"}, ing); !apierrors.IsNotFound(err) {
		t.Errorf("an HTTP-only Ingress was rendered: err = %v", err)
	}

	// No host, no Ingress: the credential is not needed and nothing fails.
	oc.Spec.Components.Dashboard.Host = ""
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Errorf("dashboard without a host: %v", err)
	}
}
