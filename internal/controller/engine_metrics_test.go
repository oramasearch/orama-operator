/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Envoy answers /metrics with 404 before its catch-all route to OramaCore, the engine ConfigMap
// carries that bootstrap, and OramaCore keeps serving Prometheus on its own port.
func TestEnvoyRefusesMetricsBeforeCatchAll(t *testing.T) {
	doc := parseYAML(t, envoyConfig)
	listener := doc["static_resources"].(map[string]any)["listeners"].([]any)[0].(map[string]any)
	filter := listener["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	host := filter["typed_config"].(map[string]any)["route_config"].(map[string]any)["virtual_hosts"].([]any)[0].(map[string]any)
	routes := host["routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("routes = %v, want the /metrics refusal then the catch-all", routes)
	}
	metrics, catchAll := routes[0].(map[string]any), routes[1].(map[string]any)
	if !reflect.DeepEqual(metrics["match"], map[string]any{"path": "/metrics"}) ||
		fmt.Sprint(metrics["direct_response"]) != "map[status:404]" || metrics["route"] != nil {
		t.Errorf("first route = %v, want path /metrics answered 404 with no upstream", metrics)
	}
	if !reflect.DeepEqual(catchAll["match"], map[string]any{"prefix": "/"}) ||
		catchAll["route"].(map[string]any)["cluster"] != "oramacore" {
		t.Errorf("second route = %v, want prefix / to the oramacore cluster", catchAll)
	}

	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		h := newEngineHarness(t)
		oc := engineOC(mut)
		if err := h.r.reconcileEngineConfig(context.Background(), oc); err != nil {
			t.Fatal(err)
		}
		cm := h.engineConfig()
		if cm.Data["envoy.yaml"] != envoyConfig {
			t.Errorf("%s: envoy.yaml is not envoyConfig", name)
		}
		for _, side := range []string{"writer", "reader"} {
			key := engineSideFor(oc, side).configKey
			if parseYAML(t, cm.Data[key])["http"].(map[string]any)["with_prometheus"] != true {
				t.Errorf("%s: %s turns OramaCore's Prometheus endpoint off", name, key)
			}
		}
	}
}

// Envoy's bootstrap is part of the Envoy readers' config hash, and of nothing else: an Envoy-only
// change rolls them and leaves the writer, and readers without Envoy, as they were.
func TestEnvoyBootstrapRollsOnlyEnvoyReaders(t *testing.T) {
	ctx := context.Background()
	changed := envoyConfig + "# changed\n"
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		for _, envoy := range []bool{true, false} {
			h := newEngineHarness(t)
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.Envoy = envoy
				if mut != nil {
					mut(o)
				}
			})
			h.reconcile(oc)
			cm := h.engineConfig()
			writer, reader := h.writer(), h.reader()
			writerKey, readerKey := engineSideFor(oc, "writer").configKey, engineSideFor(oc, "reader").configKey
			// The writer's hash is its template's alone, byte for byte as before #314.
			if got := writer.Spec.Template.Annotations[configHashKey]; got != templateSHA256(cm.Data[writerKey]) {
				t.Errorf("%s envoy=%v: writer hash = %s, want the SHA-256 of %s", name, envoy, got, writerKey)
			}
			want := templateSHA256(cm.Data[readerKey])
			if envoy {
				want = sha256Hex([]byte(cm.Data[readerKey]), []byte(envoyConfig))
			}
			if got := reader.Spec.Template.Annotations[configHashKey]; got != want {
				t.Errorf("%s envoy=%v: reader hash = %s, want %s", name, envoy, got, want)
			}

			// An unchanged reconcile leaves both workloads alone.
			h.reconcile(oc)
			if h.writer().ResourceVersion != writer.ResourceVersion || h.reader().ResourceVersion != reader.ResourceVersion {
				t.Errorf("%s envoy=%v: an unchanged reconcile rewrote a workload", name, envoy)
			}

			// Only the bootstrap changes.
			llmHash := reader.Spec.Template.Annotations[llmSecretHashKey]
			if err := h.r.reconcileReaders(ctx, oc, llmHash, h.reader().Spec.Template.Annotations[credentialsHashKey], changed); err != nil {
				t.Fatal(err)
			}
			if moved := h.reader().Spec.Template.Annotations[configHashKey] != reader.Spec.Template.Annotations[configHashKey]; moved != envoy {
				t.Errorf("%s envoy=%v: a new bootstrap changed the reader hash = %v", name, envoy, moved)
			}
			if !envoy && h.reader().ResourceVersion != reader.ResourceVersion {
				t.Errorf("%s: a new bootstrap rewrote readers that run no Envoy", name)
			}
			if !reflect.DeepEqual(h.writer().Spec.Template, writer.Spec.Template) {
				t.Errorf("%s envoy=%v: a new bootstrap changed the writer pod template", name, envoy)
			}
			if err := h.r.reconcileReaders(ctx, oc, llmHash, h.reader().Spec.Template.Annotations[credentialsHashKey], envoyConfig); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(h.reader().Spec.Template, reader.Spec.Template) {
				t.Errorf("%s envoy=%v: the bootstrap restored did not restore the reader pod template", name, envoy)
			}
		}
	}
}

// metricsCompanion is Ingress <cr>-<component>-metrics, if it exists.
func (h *publicHarness) metricsCompanion(component string) (*networkingv1.Ingress, bool) {
	ing := &networkingv1.Ingress{}
	return ing, h.get("v1-dev-"+component+"-metrics", ing)
}

// assertMetricsCompanion checks that the main Ingress of component serves only Prefix / on hosts,
// and that its companion denies Exact /metrics to every source on the same hosts, class, TLS,
// Service and port, labelled for component and controlled by the CR.
func assertMetricsCompanion(t *testing.T, h *publicHarness, component string, port int32, hosts ...string) {
	t.Helper()
	main, ok := h.ingress(component)
	if !ok {
		t.Fatalf("no %s Ingress", component)
	}
	service := "v1-dev-" + component
	assertRoutes(t, main, service, port, hosts...)
	companion, ok := h.metricsCompanion(component)
	if !ok {
		t.Fatalf("no %s metrics companion", component)
	}
	if companion.Namespace != "v1-dev" {
		t.Errorf("%s namespace = %s", companion.Name, companion.Namespace)
	}
	if !reflect.DeepEqual(companion.Annotations, map[string]string{annotationNginxDenylistSourceRange: "0.0.0.0/0,::/0"}) {
		t.Errorf("%s annotations = %v, want only the deny-all source range (no certificate request)", companion.Name, companion.Annotations)
	}
	if !reflect.DeepEqual(companion.Labels, labelsFor(h.oc, component)) {
		t.Errorf("%s labels = %v", companion.Name, companion.Labels)
	}
	if owner := metav1.GetControllerOf(companion); owner == nil || owner.UID != h.oc.UID || owner.Kind != "OramaCloudV1" {
		t.Errorf("%s controller = %+v, want OramaCloudV1 %s", companion.Name, owner, h.oc.UID)
	}
	if *companion.Spec.IngressClassName != *main.Spec.IngressClassName {
		t.Errorf("%s class = %s, want %s", companion.Name, *companion.Spec.IngressClassName, *main.Spec.IngressClassName)
	}
	if !reflect.DeepEqual(companion.Spec.TLS, main.Spec.TLS) {
		t.Errorf("%s TLS = %v, want the main Ingress's %v", companion.Name, companion.Spec.TLS, main.Spec.TLS)
	}
	if got := ruleHosts(companion); !slices.Equal(got, hosts) {
		t.Errorf("%s hosts = %v, want %v", companion.Name, got, hosts)
	}
	exact := networkingv1.PathTypeExact
	want := &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
		Path: "/metrics", PathType: &exact,
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: service, Port: networkingv1.ServiceBackendPort{Number: port},
		}},
	}}}
	for _, rule := range companion.Spec.Rules {
		if !reflect.DeepEqual(rule.HTTP, want) {
			t.Errorf("%s rule %s = %+v, want only Exact /metrics to %s:%d", companion.Name, rule.Host, rule.HTTP, service, port)
		}
	}
}

// Every public engine Ingress has a deny-all /metrics companion on its host and every extra host,
// sharing its TLS Secrets, and a second reconcile writes nothing.
func TestMetricsCompanionsShadowPublicEngineIngresses(t *testing.T) {
	for name, extra := range map[string][2][]string{
		"hosts only":  {nil, nil},
		"extra hosts": {{"writer-alt.oramadev.net"}, {"reader-alt.oramadev.net", "reader-alt2.oramadev.net"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.Writer.ExtraHosts = extra[0]
				o.Spec.Components.Engine.Reader.ExtraHosts = extra[1]
			}))
			h.mustPublish()
			assertMetricsCompanion(t, h, "writer", oramacorePort, append([]string{publicWriterHost}, extra[0]...)...)
			assertMetricsCompanion(t, h, "reader", envoyPort, append([]string{publicReaderHost}, extra[1]...)...)
			h.steady()
		})
	}
}

// The builder narrows a copy: the main spec it is given is left as it was.
func TestMetricsBlockIngressSpecLeavesMainUnchanged(t *testing.T) {
	oc := publicStack(nil)
	main := ingressSpec(oc, "v1-dev-writer", publicWriterHost, []string{"writer-alt.oramadev.net"}, oramacorePort)
	before := main.DeepCopy()
	companion := metricsBlockIngressSpec(main)
	if !reflect.DeepEqual(main, *before) {
		t.Fatalf("metricsBlockIngressSpec changed its input: %+v", main)
	}
	if companion.Rules[0].HTTP.Paths[0].Path != "/metrics" || main.Rules[0].HTTP.Paths[0].Path != "/" {
		t.Fatalf("companion path %s, main path %s", companion.Rules[0].HTTP.Paths[0].Path, main.Rules[0].HTTP.Paths[0].Path)
	}
	companion.TLS[0].SecretName = "edited"
	if main.TLS[0].SecretName == "edited" {
		t.Fatal("the companion shares the main spec's TLS slice")
	}
}

// The companions are created, updated, repaired and deleted with the Ingress they shadow.
func TestMetricsCompanionsLifecycle(t *testing.T) {
	ctx := context.Background()
	exists := func(t *testing.T, h *publicHarness, component string, want bool) {
		t.Helper()
		_, main := h.ingress(component)
		_, companion := h.metricsCompanion(component)
		if main != want || companion != want {
			t.Errorf("%s: main Ingress exists = %v, companion exists = %v, want %v", component, main, companion, want)
		}
	}

	t.Run("host changes update both", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(nil))
		h.mustPublish()
		h.update(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.Host = "writer-next.oramadev.net"
			o.Spec.Components.Engine.Reader.Host = "reader-next.oramadev.net"
		})
		h.mustPublish()
		assertMetricsCompanion(t, h, "writer", oramacorePort, "writer-next.oramadev.net")
		assertMetricsCompanion(t, h, "reader", envoyPort, "reader-next.oramadev.net")
		h.update(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Reader.ExtraHosts = []string{"reader-alt.oramadev.net"}
		})
		h.mustPublish()
		assertMetricsCompanion(t, h, "reader", envoyPort, "reader-next.oramadev.net", "reader-alt.oramadev.net")
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.ExtraHosts = nil })
		h.mustPublish()
		assertMetricsCompanion(t, h, "reader", envoyPort, "reader-next.oramadev.net")
	})

	t.Run("cleared hosts delete both", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(nil))
		h.mustPublish()
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.Host = "" })
		h.mustPublish()
		exists(t, h, "writer", false)
		exists(t, h, "reader", true)
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "" })
		h.mustPublish()
		exists(t, h, "reader", false)
		h.steady()
	})

	t.Run("read region deletes the writer's", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(nil))
		h.mustPublish()
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read" })
		h.mustPublish()
		exists(t, h, "writer", false)
		assertMetricsCompanion(t, h, "reader", envoyPort, publicReaderHost)
	})

	t.Run("engine disabled then enabled", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(nil))
		h.mustPublish()
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false })
		if err := h.r.reconcileEngine(ctx, h.oc); err != nil {
			t.Fatal(err)
		}
		exists(t, h, "writer", false)
		exists(t, h, "reader", false)
		h.update(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = true })
		h.mustPublish()
		assertMetricsCompanion(t, h, "writer", oramacorePort, publicWriterHost)
		assertMetricsCompanion(t, h, "reader", envoyPort, publicReaderHost)
	})

	t.Run("a deleted companion is recreated", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(nil))
		h.mustPublish()
		for _, component := range []string{"writer", "reader"} {
			companion, _ := h.metricsCompanion(component)
			if err := h.cl.Delete(ctx, companion); err != nil {
				t.Fatal(err)
			}
		}
		h.mustPublish()
		assertMetricsCompanion(t, h, "writer", oramacorePort, publicWriterHost)
		assertMetricsCompanion(t, h, "reader", envoyPort, publicReaderHost)
		h.steady()
	})

	t.Run("a foreign object at the companion's name blocks publishing", func(t *testing.T) {
		foreign := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "v1-dev-writer-metrics", Namespace: "v1-dev"}}
		h := newPublicHarness(t, publicStack(nil), foreign)
		if err := h.r.reconcileEngineIngresses(ctx, h.oc); err == nil || !strings.Contains(err.Error(), "Ingress v1-dev/v1-dev-writer-metrics") {
			t.Fatalf("err = %v, want a conflict naming the companion", err)
		}
		if _, ok := h.ingress("writer"); ok {
			t.Error("the writer was published although its /metrics companion could not be written")
		}
		if ing, _ := h.metricsCompanion("writer"); len(ing.OwnerReferences) != 0 || len(ing.Spec.Rules) != 0 {
			t.Errorf("the foreign companion changed: %+v", ing)
		}
	})

	t.Run("no issuer writes neither", func(t *testing.T) {
		h := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Cloudflare.APITokenSecretRef = nil }))
		if err := h.r.reconcileEngineIngresses(ctx, h.oc); err == nil {
			t.Fatal("published without an Issuer")
		}
		exists(t, h, "writer", false)
		exists(t, h, "reader", false)
	})
}

// With Envoy off the reader companion still blocks /metrics, on the reader Service's port 80,
// which then targets OramaCore's 8080 directly.
func TestMetricsCompanionWithoutEnvoy(t *testing.T) {
	h := newPublicHarness(t, publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Envoy = false }))
	h.mustPublish()
	assertMetricsCompanion(t, h, "reader", envoyPort, publicReaderHost)
	if err := h.r.reconcileReaders(context.Background(), h.oc, "", "", envoyConfig); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{}
	if !h.get("v1-dev-reader", svc) {
		t.Fatal("no reader Service")
	}
	if p := svc.Spec.Ports; len(p) != 1 || p[0].Port != envoyPort || p[0].TargetPort != intstr.FromInt32(oramacorePort) {
		t.Errorf("reader Service ports = %+v, want 80 to 8080", p)
	}
}

// zetta-tunnel's gateway cannot refuse a source, so it serves no host that a deny companion covers,
// on any Ingress: its Prefix / would forward /metrics. Other Cloud v1 hosts stay served.
func TestIngressGatewaySkipsSourceDeniedHosts(t *testing.T) {
	ctx := context.Background()
	writer := testOperatorIngress("v1", "v1-writer", "OramaCloudV1", "nginx", "writer.example.com", "writer.example.com-tls", "v1-writer", 8080, nil)
	companion := writer.DeepCopy()
	companion.Name = "v1-writer-metrics"
	companion.Annotations = map[string]string{annotationNginxDenylistSourceRange: denyAllSources}
	companion.Spec = metricsBlockIngressSpec(writer.Spec)
	kube := fake.NewClientBuilder().WithScheme(ocv1TestScheme(t)).WithObjects(
		writer, companion,
		testTLSSecret(t, "v1", "writer.example.com-tls", "writer.example.com"),
		testBackend("v1", "v1-writer", 8080, 8080),
		testOperatorIngress("v1", "v1-dashboard", "OramaCloudV1", "nginx", "app.example.com", "app-tls", "v1-dashboard", 3000, nil),
		testTLSSecret(t, "v1", "app-tls", "app.example.com"),
		testBackend("v1", "v1-dashboard", 3000, 3000),
	).Build()

	projection, err := projectIngresses(ctx, kube)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, host := range projection.Document.Hosts {
		hosts = append(hosts, host.Name)
	}
	if !slices.Equal(hosts, []string{"app.example.com"}) {
		t.Fatalf("served hosts = %v, want only the dashboard", hosts)
	}
	for _, key := range []string{"v1/v1-writer", "v1/v1-writer-metrics"} {
		if projection.Served[key] {
			t.Errorf("%s is reported served", key)
		}
		if !slices.ContainsFunc(projection.Skipped, func(s string) bool {
			return strings.HasPrefix(s, key+" writer.example.com: ") && strings.Contains(s, "denylist-source-range")
		}) {
			t.Errorf("skipped = %v, want %s writer.example.com refused for its denylist", projection.Skipped, key)
		}
	}
	if !projection.Served["v1/v1-dashboard"] {
		t.Error("the dashboard is no longer served")
	}
}
