/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Ingress extra hosts (operator#313): each component's Ingress answers host and every extra host
// with the same backend, origin TLS covers all of them, and only host gets a DNS record.

const (
	aliasDashboardHost = "app-alias-dev.oramadev.net"
	aliasDocsHost      = "docs-alias-dev.oramadev.net"
	aliasWriterHost    = "writer-alias-dev.oramadev.net"
	aliasReaderHost    = "reader-alias-dev.oramadev.net"
	publicDocsHost     = "docs-dev.oramadev.net"
)

// extraHostsHarness is a fake cluster that counts object writes and status writes apart.
type extraHostsHarness struct {
	t            *testing.T
	cl           client.Client
	r            *OramaCloudV1Reconciler
	writes       int
	statusWrites int
}

func newExtraHostsHarness(t *testing.T, objs ...client.Object) *extraHostsHarness {
	t.Helper()
	s := ocv1TestScheme(t)
	h := &extraHostsHarness{t: t}
	h.cl = fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1{}, &networkingv1.Ingress{}, &oramav1alpha1.CloudflareDNSRecord{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				h.writes++
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				h.writes++
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				h.writes++
				return c.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				h.writes++
				return c.Delete(ctx, obj, opts...)
			},
			DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
				h.writes++
				return c.DeleteAllOf(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				h.statusWrites++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				h.statusWrites++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.r = &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
	return h
}

func (h *extraHostsHarness) get(name string, obj client.Object) bool {
	h.t.Helper()
	err := h.cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "v1-dev"}, obj)
	if err != nil && client.IgnoreNotFound(err) != nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

func (h *extraHostsHarness) ingress(component string) *networkingv1.Ingress {
	h.t.Helper()
	ing := &networkingv1.Ingress{}
	if !h.get("v1-dev-"+component, ing) {
		h.t.Fatalf("no %s Ingress", component)
	}
	return ing
}

// publish runs the steps that own the public endpoints: the engine, dashboard and docs (each
// renders its own Ingress), then the DNS records.
func (h *extraHostsHarness) publish(oc *oramav1alpha1.OramaCloudV1) {
	h.t.Helper()
	ctx := context.Background()
	for _, step := range []func(context.Context, *oramav1alpha1.OramaCloudV1) error{
		h.r.reconcileEngine, h.r.reconcileDashboard, h.r.reconcileDocs, h.r.reconcileCloudflare,
	} {
		if err := step(ctx, oc); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *extraHostsHarness) setAddress(component, addr string) {
	h.t.Helper()
	ing := h.ingress(component)
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: addr}}
	if err := h.cl.Status().Update(context.Background(), ing); err != nil {
		h.t.Fatal(err)
	}
}

// records maps each DNS record's name to its spec.
func (h *extraHostsHarness) records() map[string]oramav1alpha1.CloudflareDNSRecordSpec {
	h.t.Helper()
	var list oramav1alpha1.CloudflareDNSRecordList
	if err := h.cl.List(context.Background(), &list, client.InNamespace("v1-dev")); err != nil {
		h.t.Fatal(err)
	}
	got := map[string]oramav1alpha1.CloudflareDNSRecordSpec{}
	for _, rec := range list.Items {
		got[rec.Name] = rec.Spec
	}
	return got
}

// extraHostsStack is publicStack with docs published too, every engine key present, and the given
// extra hosts on the dashboard, docs, writer and reader.
func extraHostsStack(dashboard, docs, writer, reader []string) *oramav1alpha1.OramaCloudV1 {
	return publicStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Docs = oramav1alpha1.ToggleWithHost{Enabled: true, Host: publicDocsHost}
		o.Spec.Components.Dashboard.ExtraHosts = dashboard
		o.Spec.Components.Docs.ExtraHosts = docs
		o.Spec.Components.Engine.Writer.ExtraHosts = writer
		o.Spec.Components.Engine.Reader.ExtraHosts = reader
	})
}

// engineKeySecret is CR v1-dev's engine Secret with every key.
func engineKeySecret() *corev1.Secret {
	sec := secretOf("v1-dev-engine-secret", engineKeys)
	sec.Namespace = "v1-dev"
	return sec
}

// engineMasterSecret is the master API key Secret originTLSStack references, so the engine is not
// held at zero replicas (#307).
func engineMasterSecret() *corev1.Secret {
	sec := secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey})
	sec.Namespace = "v1-dev"
	return sec
}

func ruleHosts(ing *networkingv1.Ingress) []string {
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts
}

func tlsHosts(ing *networkingv1.Ingress) []string {
	var hosts []string
	for _, entry := range ing.Spec.TLS {
		hosts = append(hosts, entry.Hosts...)
	}
	return hosts
}

// assertRoutes checks that ing has one rule per host, in order, each with the single Prefix /
// path to service:port, and a certificate for every host.
func assertRoutes(t *testing.T, ing *networkingv1.Ingress, service string, port int32, hosts ...string) {
	t.Helper()
	if got := ruleHosts(ing); !slices.Equal(got, hosts) {
		t.Fatalf("%s rule hosts = %v, want %v", ing.Name, got, hosts)
	}
	pathType := networkingv1.PathTypePrefix
	want := &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
		Path: "/", PathType: &pathType,
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: service, Port: networkingv1.ServiceBackendPort{Number: port},
		}},
	}}}
	for _, rule := range ing.Spec.Rules {
		if !reflect.DeepEqual(rule.HTTP, want) {
			t.Errorf("%s rule %s = %+v, want / to %s:%d", ing.Name, rule.Host, rule.HTTP, service, port)
		}
	}
	if got := tlsHosts(ing); !slices.Equal(got, hosts) {
		t.Errorf("%s TLS hosts = %v, want %v", ing.Name, got, hosts)
	}
	for _, entry := range ing.Spec.TLS {
		if entry.SecretName != ingressTLSSecretName(entry.Hosts[0]) {
			t.Errorf("%s TLS %v Secret = %s, want %s", ing.Name, entry.Hosts, entry.SecretName, ingressTLSSecretName(entry.Hosts[0]))
		}
	}
	assertIngressTLS(t, ing, "v1-dev-letsencrypt")
}

// The cutover example: the dashboard answers app.orama.ing then app.orama.com, both to its Service
// on 3000, with a certificate for each.
func TestDashboardIngressAnswersExtraHosts(t *testing.T) {
	oc := publicStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app.orama.ing", ExtraHosts: []string{"app.orama.com"}}}
	})
	h := newExtraHostsHarness(t)
	if err := h.r.reconcileDashboard(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	assertRoutes(t, h.ingress("dashboard"), "v1-dev-dashboard", 3000, "app.orama.ing", "app.orama.com")
}

// Docs, writer and reader each forward their own extra hosts to their own Service and port.
func TestComponentIngressesAnswerTheirOwnExtraHosts(t *testing.T) {
	oc := extraHostsStack([]string{aliasDashboardHost}, []string{aliasDocsHost}, []string{aliasWriterHost}, []string{aliasReaderHost})
	h := newExtraHostsHarness(t, engineKeySecret(), engineMasterSecret())
	h.publish(oc)
	assertRoutes(t, h.ingress("dashboard"), "v1-dev-dashboard", 3000, publicDashboardHost, aliasDashboardHost)
	assertRoutes(t, h.ingress("docs"), "v1-dev-docs", 3000, publicDocsHost, aliasDocsHost)
	assertRoutes(t, h.ingress("writer"), "v1-dev-writer", oramacorePort, publicWriterHost, aliasWriterHost)
	assertRoutes(t, h.ingress("reader"), "v1-dev-reader", envoyPort, publicReaderHost, aliasReaderHost)
}

// Nil and empty extra hosts render the single-host Ingress: one rule, one certificate.
func TestNoExtraHostsRendersTheSingleHostIngress(t *testing.T) {
	render := func(extra []string) *networkingv1.Ingress {
		oc := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.ExtraHosts = extra })
		h := newExtraHostsHarness(t)
		if err := h.r.reconcileDashboard(context.Background(), oc); err != nil {
			t.Fatal(err)
		}
		return h.ingress("dashboard")
	}
	pathType := networkingv1.PathTypePrefix
	want := networkingv1.IngressSpec{
		IngressClassName: strPtr(defaultIngressClassName),
		TLS:              []networkingv1.IngressTLS{{Hosts: []string{publicDashboardHost}, SecretName: publicDashboardHost + "-tls"}},
		Rules: []networkingv1.IngressRule{{Host: publicDashboardHost, IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: "v1-dev-dashboard", Port: networkingv1.ServiceBackendPort{Number: 3000},
				}},
			}}},
		}}},
	}
	nilIng, emptyIng := render(nil), render([]string{})
	for name, ing := range map[string]*networkingv1.Ingress{"nil": nilIng, "empty": emptyIng} {
		if !reflect.DeepEqual(ing.Spec, want) {
			t.Errorf("%s extraHosts: spec = %+v, want %+v", name, ing.Spec, want)
		}
	}
	if !reflect.DeepEqual(nilIng.Annotations, emptyIng.Annotations) || !reflect.DeepEqual(nilIng.Labels, emptyIng.Labels) {
		t.Error("nil and empty extraHosts render different metadata")
	}
}

// Adding extra hosts adds their routes and certificates; removing them takes both away, back to
// the original Ingress. A repeated reconcile writes nothing. The CR's lists are not modified.
func TestExtraHostsAddAndRemove(t *testing.T) {
	oc := extraHostsStack(nil, nil, nil, nil)
	h := newExtraHostsHarness(t, engineKeySecret(), engineMasterSecret())
	h.publish(oc)
	original := h.ingress("dashboard").Spec

	extras := []string{"b.oramadev.net", "a.oramadev.net"}
	oc.Spec.Components.Dashboard.ExtraHosts = extras
	oc.Spec.Components.Engine.Writer.ExtraHosts = []string{aliasWriterHost}
	h.publish(oc)
	assertRoutes(t, h.ingress("dashboard"), "v1-dev-dashboard", 3000, publicDashboardHost, "b.oramadev.net", "a.oramadev.net")
	assertRoutes(t, h.ingress("writer"), "v1-dev-writer", oramacorePort, publicWriterHost, aliasWriterHost)
	if !slices.Equal(extras, []string{"b.oramadev.net", "a.oramadev.net"}) {
		t.Errorf("the CR's extraHosts changed to %v", extras)
	}
	before := h.writes
	h.publish(oc)
	if h.writes != before {
		t.Errorf("an unchanged reconcile wrote %d times", h.writes-before)
	}

	oc.Spec.Components.Dashboard.ExtraHosts = []string{"a.oramadev.net"}
	oc.Spec.Components.Engine.Writer.ExtraHosts = nil
	h.publish(oc)
	assertRoutes(t, h.ingress("dashboard"), "v1-dev-dashboard", 3000, publicDashboardHost, "a.oramadev.net")
	assertRoutes(t, h.ingress("writer"), "v1-dev-writer", oramacorePort, publicWriterHost)

	oc.Spec.Components.Dashboard.ExtraHosts = []string{}
	h.publish(oc)
	if got := h.ingress("dashboard").Spec; !reflect.DeepEqual(got, original) {
		t.Errorf("after clearing extraHosts spec = %+v, want the original %+v", got, original)
	}
	before = h.writes
	h.publish(oc)
	if h.writes != before {
		t.Errorf("an unchanged reconcile wrote %d times", h.writes-before)
	}
}

// Only the primary hosts get DNS records, whatever the extra hosts are, and adding or removing
// extras leaves the records as they were. With DNS unmanaged, extras get no record either.
func TestExtraHostsGetNoDNSRecord(t *testing.T) {
	oc := extraHostsStack([]string{aliasDashboardHost}, []string{aliasDocsHost}, []string{aliasWriterHost}, []string{aliasReaderHost})
	h := newExtraHostsHarness(t, engineKeySecret(), engineMasterSecret())
	h.publish(oc)
	h.setAddress("dashboard", "192.0.2.9")
	h.setAddress("writer", "192.0.2.10")
	h.setAddress("reader", "192.0.2.11")
	h.publish(oc)

	want := map[string]oramav1alpha1.CloudflareDNSRecordSpec{
		"v1-dev-dashboard-dns": {SecretRef: "cloudflare-api-token", Name: publicDashboardHost, Content: "192.0.2.9", Proxied: true},
		"v1-dev-writer-dns":    {SecretRef: "cloudflare-api-token", Name: publicWriterHost, Content: "192.0.2.10", Proxied: true},
		"v1-dev-reader-dns":    {SecretRef: "cloudflare-api-token", Name: publicReaderHost, Content: "192.0.2.11", Proxied: true},
	}
	assertRecords := func(when string) {
		t.Helper()
		got := h.records()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: records = %+v, want %+v", when, got, want)
		}
		for _, spec := range got {
			if strings.Contains(spec.Name, "alias") {
				t.Errorf("%s: record for extra host %s", when, spec.Name)
			}
		}
	}
	assertRecords("with extras")

	oc.Spec.Components.Dashboard.ExtraHosts = []string{aliasDashboardHost, "app-alias2-dev.oramadev.net"}
	oc.Spec.Components.Engine.Writer.ExtraHosts = nil
	h.publish(oc)
	assertRecords("after changing extras")

	unmanaged := extraHostsStack([]string{aliasDashboardHost}, []string{aliasDocsHost}, []string{aliasWriterHost}, []string{aliasReaderHost})
	unmanaged.Spec.Cloudflare.ManageDNS = false
	h2 := newExtraHostsHarness(t, engineKeySecret(), engineMasterSecret())
	h2.publish(unmanaged)
	h2.setAddress("dashboard", "192.0.2.9")
	h2.publish(unmanaged)
	if got := h2.records(); len(got) != 0 {
		t.Errorf("manageDNS off: records = %+v, want none", got)
	}
}

// Extras never publish what is not published: a disabled component, a disabled engine and a read
// region's writer get no Ingress, and the deprecated oramasearchApp stays inactive.
func TestExtraHostsKeepLifecycleRules(t *testing.T) {
	cases := map[string]struct {
		mut    func(*oramav1alpha1.OramaCloudV1)
		absent []string
	}{
		"disabled dashboard and docs": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.Enabled = false
			o.Spec.Components.Docs.Enabled = false
		}, []string{"dashboard", "docs"}},
		"disabled engine": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Enabled = false
		}, []string{"writer", "reader"}},
		"read region": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Region.Role = "read"
		}, []string{"writer"}},
		"deprecated oramasearchApp": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.OramasearchApp = oramav1alpha1.ToggleWithHost{Enabled: true, Host: "api.oramadev.net", ExtraHosts: []string{"api-alias.oramadev.net"}}
		}, []string{"oramasearch-app"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			oc := extraHostsStack([]string{aliasDashboardHost}, []string{aliasDocsHost}, []string{aliasWriterHost}, []string{aliasReaderHost})
			c.mut(oc)
			if err := validateIngressHosts(oc); err != nil {
				t.Fatal(err)
			}
			h := newExtraHostsHarness(t, engineKeySecret(), engineMasterSecret())
			h.publish(oc)
			for _, component := range c.absent {
				if h.get("v1-dev-"+component, &networkingv1.Ingress{}) {
					t.Errorf("%s has an Ingress", component)
				}
			}
		})
	}
}

func TestValidateIngressHosts(t *testing.T) {
	long := strings.Repeat("a", 250) + ".net"
	cases := map[string]struct {
		mut  func(*oramav1alpha1.ComponentsSpec)
		want string
	}{
		"no hosts": {func(*oramav1alpha1.ComponentsSpec) {}, ""},
		"an extra host that is not a lowercase DNS name": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Host: "app.orama.ing", ExtraHosts: []string{"App.Orama.com"}}}
		}, `spec.components.dashboard.extraHosts[0] "App.Orama.com" must be a lowercase DNS name with at least one dot`},
		"distinct hosts and extras": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Host: "app.orama.ing", ExtraHosts: []string{"app.orama.com"}}}
			c.Docs = oramav1alpha1.ToggleWithHost{Host: "docs.orama.ing", ExtraHosts: []string{"docs.orama.com"}}
			c.Engine.Writer.Host, c.Engine.Writer.ExtraHosts = "writer.orama.ing", []string{"writer.orama.com"}
			c.Engine.Reader.Host, c.Engine.Reader.ExtraHosts = "reader.orama.ing", []string{"reader.orama.com"}
		}, ""},
		"primary and primary": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard.Host, c.Docs.Host = "app.orama.ing", "APP.orama.ing"
		}, `spec.components.dashboard.host and spec.components.docs.host are both "APP.orama.ing"`},
		"primary and its own extra, case variant": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Host: "app.orama.ing", ExtraHosts: []string{"APP.orama.ing"}}}
		}, `spec.components.dashboard.host and spec.components.dashboard.extraHosts[0] are both "APP.orama.ing"`},
		"repeated extra in one list": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Host: "app.orama.ing", ExtraHosts: []string{"alias.oramadev.net", "ALIAS.oramadev.net"}}}
		}, `spec.components.dashboard.extraHosts[0] and spec.components.dashboard.extraHosts[1] are both "ALIAS.oramadev.net"`},
		"extra and a disabled component's primary": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app.orama.ing", ExtraHosts: []string{"app.orama.com"}}}
			c.Docs = oramav1alpha1.ToggleWithHost{Enabled: false, Host: "app.orama.com"}
		}, `spec.components.dashboard.extraHosts[0] and spec.components.docs.host are both "app.orama.com"`},
		"extra and extra across components": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Docs = oramav1alpha1.ToggleWithHost{Host: "docs.orama.ing", ExtraHosts: []string{"x.oramadev.net"}}
			c.Engine.Reader.Host, c.Engine.Reader.ExtraHosts = "reader.orama.ing", []string{"y.oramadev.net", "X.oramadev.net"}
		}, `spec.components.docs.extraHosts[0] and spec.components.engine.reader.extraHosts[1] are both "X.oramadev.net"`},
		"deprecated oramasearchApp": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Dashboard.Host = "app.orama.ing"
			c.OramasearchApp = oramav1alpha1.ToggleWithHost{Host: "api.orama.ing", ExtraHosts: []string{"app.orama.ing"}}
		}, `spec.components.dashboard.host and spec.components.oramasearchApp.extraHosts[0] are both "app.orama.ing"`},
		"writer extra and reader primary with the engine disabled": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Engine.Enabled = false
			c.Engine.Writer.Host, c.Engine.Writer.ExtraHosts = "writer.orama.ing", []string{"reader.orama.ing"}
			c.Engine.Reader.Host = "reader.orama.ing"
		}, `spec.components.engine.writer.extraHosts[0] and spec.components.engine.reader.host are both "reader.orama.ing"`},
		"extras without a primary": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Engine.Writer.ExtraHosts = []string{"writer-alias.oramadev.net"}
		}, "spec.components.engine.writer.extraHosts requires spec.components.engine.writer.host"},
		"empty extra": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Docs = oramav1alpha1.ToggleWithHost{Host: "docs.orama.ing", ExtraHosts: []string{"docs.orama.com", ""}}
		}, "spec.components.docs.extraHosts[1] must be 1 to 253 characters, got 0"},
		"too-long extra": {func(c *oramav1alpha1.ComponentsSpec) {
			c.Engine.Reader.Host, c.Engine.Reader.ExtraHosts = "reader.orama.ing", []string{long}
		}, "spec.components.engine.reader.extraHosts[0] must be 1 to 253 characters, got 254"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { c.mut(&o.Spec.Components) })
			err := validateIngressHosts(oc)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || err.Error() != c.want):
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}

// validHostsStack is CR v1-dev with the dashboard and docs published and the engine disabled.
func validHostsStack() *oramav1alpha1.OramaCloudV1 {
	return originTLSStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Generation = 1
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: publicDashboardHost}}
		o.Spec.Components.Docs = oramav1alpha1.ToggleWithHost{Enabled: true, Host: publicDocsHost}
	})
}

func (h *extraHostsHarness) run() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1-dev", Namespace: "v1-dev"}})
	if err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (h *extraHostsHarness) stored() *oramav1alpha1.OramaCloudV1 {
	h.t.Helper()
	oc := &oramav1alpha1.OramaCloudV1{}
	if !h.get("v1-dev", oc) {
		h.t.Fatal("CR v1-dev not found")
	}
	return oc
}

func (h *extraHostsHarness) updateSpec(mut func(*oramav1alpha1.OramaCloudV1)) {
	h.t.Helper()
	oc := h.stored()
	mut(oc)
	oc.Generation++
	if err := h.cl.Update(context.Background(), oc); err != nil {
		h.t.Fatal(err)
	}
}

// versions maps every object the reconcile writes to its resourceVersion.
func (h *extraHostsHarness) versions() map[string]string {
	h.t.Helper()
	got := map[string]string{}
	for _, list := range []client.ObjectList{
		&oramav1alpha1.NetworkSandboxList{}, &corev1.ConfigMapList{}, &corev1.SecretList{}, &corev1.ServiceList{},
		&networkingv1.IngressList{}, &oramav1alpha1.CloudflareDNSRecordList{},
	} {
		if err := h.cl.List(context.Background(), list, client.InNamespace("v1-dev")); err != nil {
			h.t.Fatal(err)
		}
		_ = meta.EachListItem(list, func(obj runtime.Object) error {
			o := obj.(client.Object)
			got[reflect.TypeOf(o).Elem().Name()+"/"+o.GetName()] = o.GetResourceVersion()
			return nil
		})
	}
	return got
}

func assertInvalidHosts(t *testing.T, oc *oramav1alpha1.OramaCloudV1, msg string) {
	t.Helper()
	if oc.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", oc.Status.Phase)
	}
	ready := meta.FindStatusCondition(oc.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidIngressHosts" || ready.Message != msg {
		t.Errorf("Ready = %+v, want False/InvalidIngressHosts %q", ready, msg)
	}
}

// Invalid hosts on a new CR write only its status: no sandbox, Secret, workload or Ingress. A
// repeated reconcile writes nothing at all.
func TestReconcileRefusesInvalidHostsBeforeAnyChild(t *testing.T) {
	oc := validHostsStack()
	oc.Spec.Components.Dashboard.ExtraHosts = []string{publicDocsHost}
	h := newExtraHostsHarness(t, oc)
	if res := h.run(); !res.IsZero() {
		t.Errorf("result = %+v, want no requeue", res)
	}
	if h.writes != 0 || h.statusWrites != 1 {
		t.Errorf("writes = %d, status writes = %d; want 0 and 1", h.writes, h.statusWrites)
	}
	if got := h.versions(); len(got) != 0 {
		t.Errorf("children after a refused reconcile: %v", got)
	}
	assertInvalidHosts(t, h.stored(), `spec.components.dashboard.extraHosts[0] and spec.components.docs.host are both "docs-dev.oramadev.net"`)

	h.run()
	if h.writes != 0 || h.statusWrites != 1 {
		t.Errorf("a repeated refusal wrote: writes = %d, status writes = %d", h.writes, h.statusWrites)
	}
}

// Invalid hosts on a running stack leave every child as it was; correcting them resumes normal
// reconciliation and replaces the error.
func TestReconcileInvalidHostsKeepChildrenThenRecover(t *testing.T) {
	h := newExtraHostsHarness(t, validHostsStack())
	h.run()
	children := h.versions()
	if _, ok := children["Ingress/v1-dev-dashboard"]; !ok {
		t.Fatalf("the valid reconcile made no dashboard Ingress: %v", children)
	}

	h.updateSpec(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.ExtraHosts = []string{aliasWriterHost}
	})
	h.writes, h.statusWrites = 0, 0
	h.run()
	if h.writes != 0 || h.statusWrites != 1 {
		t.Errorf("writes = %d, status writes = %d; want 0 and 1", h.writes, h.statusWrites)
	}
	if got := h.versions(); !reflect.DeepEqual(got, children) {
		t.Errorf("a refused reconcile changed children:\n got %v\nwant %v", got, children)
	}
	assertInvalidHosts(t, h.stored(), "spec.components.engine.writer.extraHosts requires spec.components.engine.writer.host")

	h.updateSpec(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.ExtraHosts = nil
		o.Spec.Components.Dashboard.ExtraHosts = []string{aliasDashboardHost}
	})
	h.run()
	assertRoutes(t, h.ingress("dashboard"), "v1-dev-dashboard", 3000, publicDashboardHost, aliasDashboardHost)
	ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
	if ready == nil || ready.Reason == "InvalidIngressHosts" {
		t.Errorf("Ready = %+v, want the refusal replaced", ready)
	}
}

// The CRD refuses an extraHosts entry that is not a lowercase DNS name, for every component that
// has the field, so a name such as App.Orama.com never reaches the reconcile.
func TestExtraHostsAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the extraHosts admission envtest")
	}
	server := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases", "orama.cloud_oramacloudv1s.yaml")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	kube, err := client.New(config, client.Options{Scheme: ocv1TestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const namespace = "extrahosts-admission"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	components := map[string][]string{
		"dashboard":     {"dashboard"},
		"docs":          {"docs"},
		"engine.writer": {"engine", "writer"},
		"engine.reader": {"engine", "reader"},
	}
	for name, path := range components {
		for extra, reject := range map[string]bool{"app.orama.com": false, "App.Orama.com": true, "localhost": true, "app.orama.com:443": true} {
			t.Run(name+"/"+extra, func(t *testing.T) {
				leaf := map[string]interface{}{"host": "primary.orama.ing", "extraHosts": []interface{}{extra}}
				var node interface{} = leaf
				for i := len(path) - 1; i >= 0; i-- {
					node = map[string]interface{}{path[i]: node}
				}
				fixture := &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "orama.cloud/v1alpha1", "kind": "OramaCloudV1",
					"metadata": map[string]interface{}{"name": "proof-extra-hosts", "namespace": namespace},
					"spec": map[string]interface{}{
						"region":     map[string]interface{}{"name": "proof", "role": "primary"},
						"components": node,
					},
				}}
				err := kube.Create(ctx, fixture, client.DryRunAll)
				switch {
				case reject && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "extraHosts[0]")):
					t.Fatalf("%q accepted or refused for another reason: %v", extra, err)
				case !reject && err != nil:
					t.Fatalf("%q refused: %v", extra, err)
				}
			})
		}
	}
}
