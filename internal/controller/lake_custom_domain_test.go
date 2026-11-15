/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// fakeDNS is the customer's zone as the public DNS answers it.
type fakeDNS struct {
	mu     sync.Mutex
	cnames map[string]string
	txts   map[string][]string
	hosts  map[string][]string
}

func newFakeDNS() *fakeDNS {
	return &fakeDNS{cnames: map[string]string{}, txts: map[string][]string{}, hosts: map[string][]string{}}
}

var errNoSuchHost = errors.New("no such host")

func (f *fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cname, ok := f.cnames[host]; ok {
		return cname + ".", nil
	}
	return "", errNoSuchHost
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if texts, ok := f.txts[name]; ok {
		return texts, nil
	}
	return nil, errNoSuchHost
}

func (f *fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addresses, ok := f.hosts[host]; ok {
		return addresses, nil
	}
	return nil, errNoSuchHost
}

func (f *fakeDNS) set(update func(*fakeDNS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func customDomainTestLake(name string) *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: oramav1alpha1.LakeSpec{
			Image:          "oramasearch/lake:test",
			Namespace:      "lake-" + name,
			IngressEnabled: true,
			Host:           name + ".lakelive.example",
			DNSSecretRef:   &corev1.LocalObjectReference{Name: "cloudflare-lake-live"},
			CustomDomain:   &oramav1alpha1.LakeCustomDomainSpec{Host: "data.example.com"},
		},
	}
}

func TestCustomDomainRecordsAreDerivedFromTheLake(t *testing.T) {
	lake := customDomainTestLake("acme")
	lake.UID = "uid-1"
	if got := lakeCustomDomainOriginName(lake); got != "acme.origin.lakelive.example" {
		t.Errorf("origin name = %q", got)
	}
	nested := customDomainTestLake("x")
	nested.Spec.Host = "dev.lake.nonprod.example"
	if got := lakeCustomDomainOriginName(nested); got != "dev-lake.origin.nonprod.example" {
		t.Errorf("origin name for a nested host = %q", got)
	}
	routing, ok := lakeCustomDomainRoutingRecord(lake, "data.example.com", "203.0.113.10")
	if !ok || routing.Type != "CNAME" || routing.Value != "acme.origin.lakelive.example" || routing.Name != "data.example.com" {
		t.Errorf("routing record with a DNS credential = %+v", routing)
	}
	if _, ok := lakeCustomDomainRoutingRecord(lake, "data.example.com", ""); ok {
		t.Error("a routing record was offered before the load balancer had an address")
	}
	onPrem := lake.DeepCopy()
	onPrem.Spec.DNSSecretRef = nil
	if routing, _ := lakeCustomDomainRoutingRecord(onPrem, "data.example.com", "203.0.113.10"); routing.Type != "A" || routing.Value != "203.0.113.10" {
		t.Errorf("routing record without a DNS credential = %+v", routing)
	}
	if routing, _ := lakeCustomDomainRoutingRecord(onPrem, "data.example.com", "lb.example.net"); routing.Type != "CNAME" || routing.Value != "lb.example.net" {
		t.Errorf("routing record to a load balancer name = %+v", routing)
	}
	ownership := lakeCustomDomainOwnershipRecord(lake, "data.example.com")
	if ownership.Name != "_orama-verification.data.example.com" || !strings.HasPrefix(ownership.Value, "orama-verification=") {
		t.Errorf("ownership record = %+v", ownership)
	}
	other := lake.DeepCopy()
	other.UID = "uid-2"
	if lakeCustomDomainOwnershipRecord(other, "data.example.com").Value == ownership.Value {
		t.Error("two Lakes were given the same ownership token")
	}
}

// A provider that flattens the CNAME, or a proxy in front, still passes when the name ends at the
// origin's address.
func TestCustomDomainRoutingAcceptsAFlattenedCNAME(t *testing.T) {
	dns := newFakeDNS()
	record := oramav1alpha1.LakeCustomerDNSRecord{Type: "CNAME", Name: "example.com", Value: "acme.origin.lakelive.example"}
	ctx := context.Background()
	if lakeCustomDomainCheckRouting(ctx, dns, "example.com", record) {
		t.Fatal("an absent record passed")
	}
	dns.set(func(f *fakeDNS) {
		f.hosts["acme.origin.lakelive.example"] = []string{"203.0.113.10"}
		f.hosts["example.com"] = []string{"203.0.113.10"}
	})
	if !lakeCustomDomainCheckRouting(ctx, dns, "example.com", record) {
		t.Error("a flattened CNAME to the origin's address did not pass")
	}
	dns.set(func(f *fakeDNS) { f.hosts["example.com"] = []string{"198.51.100.1"} })
	if lakeCustomDomainCheckRouting(ctx, dns, "example.com", record) {
		t.Error("a name at another address passed")
	}
}

// The customer's host is solved with HTTP-01 whatever the Lake's own zone is: before its zone is
// read (DNS-01 alone otherwise) and after.
func TestTheIssuerSolvesTheCustomerHostWithHTTP01(t *testing.T) {
	lake := customDomainTestLake("acme")
	for _, zone := range []string{"", "lakelive.example"} {
		solvers := lakeOriginSolvers(lake, zone)
		first, _ := solvers[0].(map[string]any)
		selector, _ := first["selector"].(map[string]any)
		if _, http01 := first["http01"]; !http01 || !reflect.DeepEqual(selector["dnsNames"], []any{"data.example.com"}) {
			t.Errorf("zone %q: first solver = %+v, want HTTP-01 for data.example.com", zone, first)
		}
	}
	lake.Spec.CustomDomain = nil
	if solvers := lakeOriginSolvers(lake, ""); len(solvers) != 1 {
		t.Errorf("without a custom domain the solvers changed: %+v", solvers)
	}
}

// A Clerk production instance on the customer's domain asks for records in the customer's zone:
// those are the customer's to create, the rest the operator writes.
func TestClerkRecordsInTheCustomerZoneAreLeftToTheCustomer(t *testing.T) {
	lake := customDomainTestLake("acme")
	ours, customer := splitClerkDNSTargets(lake, []clerkDNSTarget{
		{Host: "clerk.example.com", Value: "frontend-api.clerk.services", RecordType: "CNAME"},
		{Host: "clk._domainkey.example.com.", Value: "dkim1.x.clerk.services", RecordType: "CNAME"},
		{Host: "clerk.acme.lakelive.example", Value: "frontend-api.clerk.services", RecordType: "CNAME"},
	})
	if len(ours) != 1 || ours[0].Host != "clerk.acme.lakelive.example" {
		t.Errorf("records the operator writes = %+v", ours)
	}
	if len(customer) != 2 || customer[0].Purpose != "clerk" || customer[1].Name != "clk._domainkey.example.com" {
		t.Errorf("records for the customer = %+v", customer)
	}
	lake.Spec.CustomDomain = nil
	if ours, customer := splitClerkDNSTargets(lake, []clerkDNSTarget{{Host: "clerk.example.com"}}); len(ours) != 1 || len(customer) != 0 {
		t.Error("without a custom domain a Clerk record was left to the customer")
	}
}

func TestClerkAllowsTheCustomerHostOnceVerified(t *testing.T) {
	lake := customDomainTestLake("acme")
	if got := lakeClerkOrigins(lake); !reflect.DeepEqual(got, []string{"https://acme.lakelive.example"}) {
		t.Errorf("origins before verification = %v", got)
	}
	verified := metav1.Now()
	lake.Status.CustomDomain = &oramav1alpha1.LakeCustomDomainStatus{Host: "data.example.com", Phase: lakeCustomDomainVerified, VerifiedTime: &verified}
	if got := lakeClerkOrigins(lake); !reflect.DeepEqual(got, []string{"https://acme.lakelive.example", "https://data.example.com"}) {
		t.Errorf("origins once verified = %v", got)
	}
}

// startCustomDomainEnvtest runs a real API server with the operator's CRDs (their CEL rules
// included) and cert-manager's Issuer and Certificate.
func startCustomDomainEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the custom-domain envtest")
	}
	crdPath, err := filepath.Abs("../../chart/crds")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{crdPath}, CRDs: certManagerTestCRDs(), ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	kube, err := client.New(config, client.Options{Scheme: newLakeScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return kube
}

// The API server refuses a custom domain the operator could not serve.
func TestCustomDomainValidationEnvtest(t *testing.T) {
	kube := startCustomDomainEnvtest(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, want string
		edit       func(*oramav1alpha1.Lake)
	}{
		{"no-ingress", "spec.customDomain needs spec.host and spec.ingressEnabled", func(l *oramav1alpha1.Lake) { l.Spec.IngressEnabled = false }},
		{"no-host", "spec.customDomain needs spec.host and spec.ingressEnabled", func(l *oramav1alpha1.Lake) { l.Spec.Host = "" }},
		{"same-host", "spec.customDomain.host must differ", func(l *oramav1alpha1.Lake) { l.Spec.CustomDomain.Host = l.Spec.Host }},
		{"env-host", "remove them from spec.env", func(l *oramav1alpha1.Lake) {
			l.Spec.Env = []corev1.EnvVar{{Name: "LAKE_PUBLIC_HOST", Value: "x.example.com"}}
		}},
		{"bad-host", "spec.customDomain.host", func(l *oramav1alpha1.Lake) { l.Spec.CustomDomain.Host = "Not A Host" }},
	} {
		lake := customDomainTestLake(tc.name)
		tc.edit(lake)
		if err := kube.Create(ctx, lake); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
	if err := kube.Create(ctx, customDomainTestLake("valid")); err != nil {
		t.Errorf("a valid custom domain was refused: %v", err)
	}
}

// pending -> verified -> routed, then removal, against a real API server.
func TestCustomDomainLifecycleEnvtest(t *testing.T) {
	kube := startCustomDomainEnvtest(t)
	ctx := context.Background()
	lake := customDomainTestLake("acme")
	ns := lake.Spec.Namespace
	svc := "lake-" + lake.Name
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	// The Lake's own Ingress, with the load balancer's address the origin record points at.
	prefix := networkingv1.PathTypePrefix
	main := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: ns},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: lake.Spec.Host, IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: 80}}}}}},
		}}}},
	}
	if err := kube.Create(ctx, main); err != nil {
		t.Fatal(err)
	}
	main.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.10"}}
	if err := kube.Status().Update(ctx, main); err != nil {
		t.Fatal(err)
	}

	dns := newFakeDNS()
	reconciler := &LakeReconciler{Client: kube, Scheme: kube.Scheme(), DomainResolver: dns}
	labels := lakeTemplateLabels(lake)
	// reconcile is one pass as Reconcile runs it: the custom domain, then the Deployment reading
	// the fresh state, then the status written.
	reconcile := func() (*oramav1alpha1.LakeCustomDomainStatus, *metav1.Condition, *appsv1.Deployment) {
		t.Helper()
		reconciler.customDomainClock = lakeCustomDomainClock{} // each pass looks DNS up again
		if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
			t.Fatal(err)
		}
		state, condition, err := reconciler.reconcileLakeCustomDomain(ctx, lake, ns, svc, labels)
		if err != nil {
			t.Fatalf("reconcile custom domain: %v", err)
		}
		lake.Status.CustomDomain = state
		if condition != nil {
			apimeta.SetStatusCondition(&lake.Status.Conditions, *condition)
		} else {
			apimeta.RemoveStatusCondition(&lake.Status.Conditions, lakeCustomDomainConditionType)
		}
		deployment, err := reconciler.reconcileLakeDeployment(
			ctx, lake, ns, svc, lake.Spec.Image, svc+"-store", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			labels, 1, lakeDefaultPort, lakeSealingMaterial{KID: "kid", PublicKey: make([]byte, 32)}, lakeClerkState{}, lakePaymentsState{},
		)
		if err != nil {
			t.Fatalf("reconcile deployment: %v", err)
		}
		if err := kube.Status().Update(ctx, lake); err != nil {
			t.Fatalf("status: %v", err)
		}
		return state, condition, deployment
	}
	customIngress := client.ObjectKey{Namespace: ns, Name: lakeCustomDomainIngressName(lake)}
	certificateKey := client.ObjectKey{Namespace: ns, Name: ingressTLSSecretName("data.example.com")}
	getCertificate := func() (*unstructured.Unstructured, error) {
		certificate := &unstructured.Unstructured{}
		certificate.SetGroupVersionKind(certManagerCertificateGVK)
		return certificate, kube.Get(ctx, certificateKey, certificate)
	}

	// Pending: nothing found. The records are published, the origin record exists in our zone,
	// nothing is served on the customer's host, and the Lake keeps its own host.
	state, condition, deployment := reconcile()
	if state.Phase != lakeCustomDomainPending || condition.Status != metav1.ConditionFalse || condition.Reason != "RecordsNotFound" {
		t.Fatalf("pending: %+v %+v", state, condition)
	}
	if len(state.Records) != 2 || state.Records[0].Type != "CNAME" || state.Records[0].Value != "acme.origin.lakelive.example" ||
		state.Records[1].Type != "TXT" || state.Records[0].Found || state.Records[1].Found {
		t.Fatalf("pending records = %+v", state.Records)
	}
	var origin oramav1alpha1.CloudflareDNSRecord
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lakeCustomDomainOriginRecord(lake)), &origin); err != nil {
		t.Fatalf("origin record: %v", err)
	}
	if origin.Spec.Name != "acme.origin.lakelive.example" || origin.Spec.Content != "203.0.113.10" || origin.Spec.Proxied {
		t.Errorf("origin record = %+v, want DNS only to the load balancer", origin.Spec)
	}
	if err := kube.Get(ctx, customIngress, &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
		t.Errorf("an unverified host was routed: %v", err)
	}
	if _, err := getCertificate(); !apierrors.IsNotFound(err) {
		t.Errorf("an unverified host was given a certificate: %v", err)
	}
	assertEnvValueOnce(t, deployment, "LAKE_PUBLIC_HOST", "acme.lakelive.example")
	assertNoEnv(t, deployment, "LAKE_HOST_ALIASES")

	// Only the CNAME: still pending; ownership is required too.
	dns.set(func(f *fakeDNS) { f.cnames["data.example.com"] = "acme.origin.lakelive.example" })
	if state, _, _ = reconcile(); state.Phase != lakeCustomDomainPending || !state.Records[0].Found || state.Records[1].Found {
		t.Fatalf("CNAME alone: %+v", state)
	}

	// Both found: Verified. The host gets its own Ingress, without Cloudflare's client-certificate
	// requirement, and a certificate solved with HTTP-01; the public host does not move yet.
	dns.set(func(f *fakeDNS) {
		f.txts["_orama-verification.data.example.com"] = []string{"unrelated", lakeCustomDomainOwnershipRecord(lake, "data.example.com").Value}
	})
	state, condition, deployment = reconcile()
	if state.Phase != lakeCustomDomainVerified || state.VerifiedTime == nil || condition.Reason != "CertificatePending" {
		t.Fatalf("verified: %+v %+v", state, condition)
	}
	var ingress networkingv1.Ingress
	if err := kube.Get(ctx, customIngress, &ingress); err != nil {
		t.Fatalf("custom host ingress: %v", err)
	}
	if len(ingress.Spec.Rules) != 1 || ingress.Spec.Rules[0].Host != "data.example.com" ||
		len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].SecretName != "data.example.com-tls" {
		t.Errorf("custom host ingress = %+v", ingress.Spec)
	}
	for key := range ingress.Annotations {
		if strings.Contains(key, "auth-tls") {
			t.Errorf("the customer's host requires Cloudflare's client certificate: %s", key)
		}
	}
	certificate, err := getCertificate()
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	if names, _, _ := unstructured.NestedStringSlice(certificate.Object, "spec", "dnsNames"); !reflect.DeepEqual(names, []string{"data.example.com"}) {
		t.Errorf("certificate names = %v", names)
	}
	assertEnvValueOnce(t, deployment, "LAKE_PUBLIC_HOST", "acme.lakelive.example")

	// A later failed lookup does not unverify the host.
	dns.set(func(f *fakeDNS) { delete(f.txts, "_orama-verification.data.example.com") })
	if state, _, _ = reconcile(); state.Phase != lakeCustomDomainVerified || state.Records[1].Found {
		t.Fatalf("verification was undone by a failed lookup: %+v", state)
	}

	// The certificate is issued: Routed. The customer's host is the public host, the Orama host an
	// alias, and the control rows keep the Orama host.
	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	certificate.Object["status"] = map[string]any{
		"notAfter":   notAfter.Format(time.RFC3339),
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
	}
	if err := kube.Update(ctx, certificate); err != nil {
		t.Fatal(err)
	}
	state, condition, deployment = reconcile()
	if state.Phase != lakeCustomDomainRouted || !state.CertificateReady || condition.Status != metav1.ConditionTrue ||
		state.CertificateNotAfter == nil || !state.CertificateNotAfter.Time.Equal(notAfter) {
		t.Fatalf("routed: %+v %+v", state, condition)
	}
	assertEnvValueOnce(t, deployment, "LAKE_PUBLIC_HOST", "data.example.com")
	assertEnvValueOnce(t, deployment, "LAKE_HOST_ALIASES", "acme.lakelive.example")
	assertEnvValueOnce(t, deployment, "LAKE_CONTROL_HOST", "acme.lakelive.example")
	if lakeRowHost(lake) != "acme.lakelive.example" {
		t.Errorf("the operator's control-row key moved to %q", lakeRowHost(lake))
	}
	var stored oramav1alpha1.Lake
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.CustomDomain == nil || stored.Status.CustomDomain.Phase != lakeCustomDomainRouted {
		t.Fatalf("stored status = %+v", stored.Status.CustomDomain)
	}

	// cert-manager's Secret, which carries the custom-domain label from the secretTemplate.
	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "data.example.com-tls", Namespace: ns, Labels: map[string]string{lakeCustomDomainLabel: lake.Name}},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")},
	}
	if err := kube.Create(ctx, tlsSecret); err != nil {
		t.Fatal(err)
	}

	// Removal: the route, the certificate, its key and the origin record go, and the Lake is back
	// on its own host.
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.CustomDomain = nil
	if err := kube.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	state, condition, deployment = reconcile()
	if state != nil || condition != nil {
		t.Fatalf("after removal: %+v %+v", state, condition)
	}
	if err := kube.Get(ctx, customIngress, &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
		t.Errorf("custom host ingress after removal: %v", err)
	}
	if _, err := getCertificate(); !apierrors.IsNotFound(err) {
		t.Errorf("certificate after removal: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(tlsSecret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("certificate key after removal: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lakeCustomDomainOriginRecord(lake)), &oramav1alpha1.CloudflareDNSRecord{}); !apierrors.IsNotFound(err) {
		t.Errorf("origin record after removal: %v", err)
	}
	assertEnvValueOnce(t, deployment, "LAKE_PUBLIC_HOST", "acme.lakelive.example")
	assertNoEnv(t, deployment, "LAKE_HOST_ALIASES")
	assertNoEnv(t, deployment, "LAKE_CONTROL_HOST")
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.CustomDomain != nil || apimeta.FindStatusCondition(stored.Status.Conditions, lakeCustomDomainConditionType) != nil {
		t.Errorf("status after removal = %+v %+v", stored.Status.CustomDomain, stored.Status.Conditions)
	}
}

func assertNoEnv(t *testing.T, deployment *appsv1.Deployment, name string) {
	t.Helper()
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == name {
			t.Fatalf("%s is set (%q), want it absent", name, variable.Value)
		}
	}
}
