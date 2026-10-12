/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testSentryDSN = "https://public@o1.ingest.sentry.io/42"

// withSentry turns on engine Sentry with the DSN in Secret engine-sentry, key dsn.
func withSentry(environment string) func(*oramav1alpha1.OramaCloudV1) {
	return func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Sentry = &oramav1alpha1.EngineSentrySpec{
			DSNSecretRef: oramav1alpha1.SentryDSNSecretRef{Name: "engine-sentry", Key: "dsn"},
			Environment:  environment,
		}
	}
}

// logSection is the log section of an OramaCore config.
func logSection(t *testing.T, name, cfg string) map[string]any {
	t.Helper()
	log, ok := parseEngineConfig(t, name, cfg)["log"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no log section:\n%s", name, cfg)
	}
	return log
}

// runRenderConfig runs the render-config init container's command against the side's template with
// env set, as the pod would, and returns the rendered config.
func runRenderConfig(t *testing.T, oc *oramav1alpha1.OramaCloudV1, s engineSide, env ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-"+s.configKey), []byte(s.template), 0o600); err != nil {
		t.Fatal(err)
	}
	render := renderConfigInit(oc, s)
	script := strings.NewReplacer("/tmpl/", dir+"/in-", "/etc/oramacore/", dir+"/out-").Replace(render.Command[2])
	cmd := exec.Command(render.Command[0], render.Command[1], script)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render command: %v\n%s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "out-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(rendered)
}

func TestEngineSentryDSNFromSecretAndSideEnvironments(t *testing.T) {
	oc := sideImagesOC(withSentry(""))
	cl, _ := reconcileOCV1(t, oc)
	cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
	for _, c := range []struct{ key, side, sts string }{
		{writerConfigKey, "writer", "v1-writer"},
		{readerConfigKey, "reader", "v1-reader"},
	} {
		if !strings.Contains(cm.Data[c.key], "  sentry_dns: '__SENTRY_DSN__'\n") {
			t.Errorf("%s lacks the DSN placeholder:\n%s", c.key, cm.Data[c.key])
		}
		log := logSection(t, c.key, cm.Data[c.key])
		if want := "us-east-" + c.side; log["sentry_environment"] != want {
			t.Errorf("%s sentry_environment = %v, want %s", c.key, log["sentry_environment"], want)
		}
		wantLevels := map[string]any{"oramacore": "info", "orama_js_pool": "info"}
		if !reflect.DeepEqual(log["levels"], wantLevels) {
			t.Errorf("%s levels = %v, want %v", c.key, log["levels"], wantLevels)
		}

		render := renderInit(t, getObj(t, cl, c.sts, &appsv1.StatefulSet{}))
		var ref *corev1.SecretKeySelector
		for _, e := range render.Env {
			if e.Name == sentryDSNEnv && e.ValueFrom != nil {
				ref = e.ValueFrom.SecretKeyRef
			}
		}
		if ref == nil || ref.Name != "engine-sentry" || ref.Key != "dsn" {
			t.Errorf("%s render-config SENTRY_DSN = %+v, want engine-sentry/dsn", c.sts, ref)
		}
	}
	if strings.Contains(cm.Data[writerConfigKey]+cm.Data[readerConfigKey], testSentryDSN) {
		t.Error("the DSN must never sit in the ConfigMap")
	}

	// The init container substitutes the DSN; an empty Secret value leaves Sentry off.
	reader := engineSideFor(oc, "reader")
	got := logSection(t, "rendered reader", runRenderConfig(t, oc, reader, "SENTRY_DSN="+testSentryDSN, "POD_NAME=v1-reader-0"))
	if got["sentry_dns"] != testSentryDSN {
		t.Errorf("rendered sentry_dns = %v, want the Secret's DSN", got["sentry_dns"])
	}
	got = logSection(t, "rendered reader", runRenderConfig(t, oc, reader, "SENTRY_DSN=", "POD_NAME=v1-reader-0"))
	if _, ok := got["sentry_dns"]; ok {
		t.Errorf("an empty DSN must drop sentry_dns, got %v", got)
	}

	// An explicit environment is the prefix of both sides' environments.
	named := sideImagesOC(withSentry("production"))
	if env := logSection(t, "writer", engineSideFor(named, "writer").template)["sentry_environment"]; env != "production-writer" {
		t.Errorf("writer environment = %v, want production-writer", env)
	}
}

func TestEngineSentryCombinedConfigEnvironmentPerSide(t *testing.T) {
	oc := fullStackOCV1(withSentry("production"))
	for _, side := range []string{"writer", "reader"} {
		s := engineSideFor(oc, side)
		if s.configKey != combinedConfigKey {
			t.Fatalf("%s config key = %s, want the combined config", side, s.configKey)
		}
		log := logSection(t, side, runRenderConfig(t, oc, s, "SENTRY_DSN="+testSentryDSN))
		if want := "production-" + side; log["sentry_environment"] != want || log["sentry_dns"] != testSentryDSN {
			t.Errorf("%s rendered log = %v, want environment %s and the DSN", side, log, want)
		}
	}
}

func TestEngineWithoutSentryRendersNoDSN(t *testing.T) {
	for name, oc := range map[string]*oramav1alpha1.OramaCloudV1{"combined": fullStackOCV1(nil), "side-only": sideImagesOC(nil)} {
		cl, _ := reconcileOCV1(t, oc)
		cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
		for key, cfg := range cm.Data {
			if key == "envoy.yaml" {
				continue
			}
			log := logSection(t, name+" "+key, cfg)
			for _, k := range []string{"sentry_dns", "sentry_environment"} {
				if _, ok := log[k]; ok {
					t.Errorf("%s %s renders %s without spec.components.engine.sentry", name, key, k)
				}
			}
			if log["levels"] == nil {
				t.Errorf("%s %s has no log levels", name, key)
			}
		}
		for _, sts := range []string{"v1-writer", "v1-reader"} {
			render := renderInit(t, getObj(t, cl, sts, &appsv1.StatefulSet{}))
			if strings.Contains(render.Command[2], "SENTRY") || strings.Contains(render.Command[2], "sentry") {
				t.Errorf("%s %s render-config mentions Sentry: %s", name, sts, render.Command[2])
			}
			for _, e := range render.Env {
				if e.Name == sentryDSNEnv {
					t.Errorf("%s %s render-config has %s", name, sts, sentryDSNEnv)
				}
			}
		}
	}
}

func TestEngineSentryChangeRollsPods(t *testing.T) {
	cl, r := reconcileOCV1(t, sideImagesOC(nil))
	hashes := func() [2]string {
		return [2]string{
			getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"],
			getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"],
		}
	}
	before := hashes()
	reconcileAgain(t, cl, r, withSentry(""))
	withDefault := hashes()
	reconcileAgain(t, cl, r, withSentry("production"))
	renamed := hashes()
	for i, side := range []string{"writer", "reader"} {
		if withDefault[i] == before[i] {
			t.Errorf("turning Sentry on did not roll the %s", side)
		}
		if renamed[i] == withDefault[i] {
			t.Errorf("changing the Sentry environment did not roll the %s", side)
		}
	}
}

func TestEnginePodsCarryPrometheusAnnotations(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	want := map[string]string{"prometheus.io/scrape": "true", "prometheus.io/port": "8080", "prometheus.io/path": "/metrics"}
	for _, name := range []string{"v1-writer", "v1-reader"} {
		got := getObj(t, cl, name, &appsv1.StatefulSet{}).Spec.Template.Annotations
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s pod annotation %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
}

func TestPublicEngineIngressesDoNotServeMetrics(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		e := &o.Spec.Components.Engine
		e.Writer.Host = "writer.prod2.cluster.orama.ing"
		e.Writer.IngressHostsSpec = oramav1alpha1.IngressHostsSpec{ExtraHosts: []string{"writer.orama.com"}, TLSSecretName: "writer-cert"}
		e.Reader.Host = "collections.orama.ing"
		e.Reader.ExtraHosts = []string{"collections.orama.com"}
	})
	cl, r := reconcileOCV1(t, oc)
	for _, c := range []struct{ name, host, extra string }{
		{"v1-writer", "writer.prod2.cluster.orama.ing", "writer.orama.com"},
		{"v1-reader", "collections.orama.ing", "collections.orama.com"},
	} {
		// The public Ingress itself routes only Prefix "/" (no /metrics rule of its own).
		for _, rule := range getObj(t, cl, c.name, &networkingv1.Ingress{}).Spec.Rules {
			if p := rule.HTTP.Paths; len(p) != 1 || p[0].Path != "/" {
				t.Errorf("%s %s paths = %+v, want only /", c.name, rule.Host, p)
			}
		}
		// Every one of its hosts has an Exact /metrics rule that ingress-nginx denies to everyone.
		block := getObj(t, cl, c.name+"-metrics", &networkingv1.Ingress{})
		if block.Annotations[metricsDenyAnnotation] != "0.0.0.0/0,::/0" {
			t.Errorf("%s-metrics annotations = %v, want a deny-all source range", c.name, block.Annotations)
		}
		var hosts []string
		for _, rule := range block.Spec.Rules {
			hosts = append(hosts, rule.Host)
			p := rule.HTTP.Paths
			if len(p) != 1 || p[0].Path != "/metrics" || p[0].PathType == nil || *p[0].PathType != networkingv1.PathTypeExact {
				t.Errorf("%s-metrics %s paths = %+v, want Exact /metrics", c.name, rule.Host, p)
			}
		}
		if want := []string{c.host, c.extra}; !reflect.DeepEqual(hosts, want) {
			t.Errorf("%s-metrics hosts = %v, want %v", c.name, hosts, want)
		}
	}

	// Envoy in front of each reader answers /metrics itself, before the catch-all route to OramaCore.
	var envoy struct {
		StaticResources struct {
			Listeners []struct {
				FilterChains []struct {
					Filters []struct {
						TypedConfig struct {
							RouteConfig struct {
								VirtualHosts []struct {
									Routes []struct {
										Match          map[string]string `json:"match"`
										DirectResponse *struct {
											Status int `json:"status"`
										} `json:"direct_response"`
										Route map[string]any `json:"route"`
									} `json:"routes"`
								} `json:"virtual_hosts"`
							} `json:"route_config"`
						} `json:"typed_config"`
					} `json:"filters"`
				} `json:"filter_chains"`
			} `json:"listeners"`
		} `json:"static_resources"`
	}
	cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
	if err := yaml.Unmarshal([]byte(cm.Data["envoy.yaml"]), &envoy); err != nil {
		t.Fatal(err)
	}
	routes := envoy.StaticResources.Listeners[0].FilterChains[0].Filters[0].TypedConfig.RouteConfig.VirtualHosts[0].Routes
	if len(routes) != 2 || routes[0].Match["path"] != "/metrics" || routes[0].DirectResponse == nil ||
		routes[0].DirectResponse.Status != 404 || routes[0].Route != nil || routes[1].Match["prefix"] != "/" {
		t.Errorf("envoy routes = %+v, want /metrics answered 404 before the / route", routes)
	}

	// Removing a host removes its /metrics block with it.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Host = ""
		o.Spec.Components.Engine.Writer.IngressHostsSpec = oramav1alpha1.IngressHostsSpec{}
	})
	if !absent(t, cl, "v1-writer-metrics", &networkingv1.Ingress{}) || !absent(t, cl, "v1-writer", &networkingv1.Ingress{}) {
		t.Error("the writer's Ingresses outlived its host")
	}
}
