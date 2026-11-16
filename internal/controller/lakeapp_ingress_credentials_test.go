// SPDX-License-Identifier: MIT

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type appIngressCredentialConfig struct {
	ProxySetHeaders map[string]string `json:"proxySetHeaders"`
	Config          map[string]string `json:"config"`
}

// Read the actual platform values: the fixture must also fail if either installation path loses
// the header configuration. The existing platform sync tests compare every other chart value.
func appIngressCredentialConfigs(t *testing.T) map[string]appIngressCredentialConfig {
	t.Helper()
	var helmfile struct {
		Releases []struct {
			Name   string `json:"name"`
			Values []struct {
				Controller appIngressCredentialConfig `json:"controller"`
			} `json:"values"`
		} `json:"releases"`
	}
	read := func(path string, into any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, into); err != nil {
			t.Fatal(err)
		}
	}
	read("../../helmfile.yaml", &helmfile)
	configs := map[string]appIngressCredentialConfig{}
	for _, release := range helmfile.Releases {
		if release.Name == "ingress-nginx" {
			for _, values := range release.Values {
				configs["helmfile.yaml"] = values.Controller
			}
		}
	}
	paths, err := filepath.Glob(filepath.Join(clustersDir(), "*", "platform.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("platform paths: %v", err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, document := range strings.Split(string(data), "\n---\n") {
			var release struct {
				Spec struct {
					Chart  struct{ Name string } `json:"chart"`
					Values struct {
						Controller appIngressCredentialConfig `json:"controller"`
					} `json:"values"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(document), &release); err != nil {
				t.Fatal(err)
			}
			if release.Spec.Chart.Name == "ingress-nginx" {
				configs[path] = release.Spec.Values.Controller
			}
		}
	}
	if len(configs) != len(paths)+1 {
		t.Fatalf("missing ingress configuration: got %d, want %d", len(configs), len(paths)+1)
	}
	return configs
}

type appIngressMapEntry struct {
	key, value string
}

type appIngressMap struct {
	source  string
	entries []appIngressMapEntry
}

// This isolated ingress fixture renders the chart's proxySetHeaders into upstream requests and
// evaluates the small nginx map subset used here. It never handles real credentials or hosts.
func appIngressFixtureHeaders(t *testing.T, config appIngressCredentialConfig, ingress string, headers http.Header) http.Header {
	t.Helper()
	maps := map[string]appIngressMap{}
	mapPattern := regexp.MustCompile(`(?s)map\s+("[^"]*"|\$\w+)\s+(\$\w+)\s*\{(.*?)\n\s*\}`)
	tokens := regexp.MustCompile(`"[^"]*"|[^\s;]+`)
	for _, match := range mapPattern.FindAllStringSubmatch(config.Config["http-snippet"], -1) {
		definition := appIngressMap{source: strings.Trim(match[1], `"`)}
		scanner := bufio.NewScanner(strings.NewReader(match[3]))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := tokens.FindAllString(line, -1)
			if len(fields) != 2 || !strings.HasSuffix(line, ";") {
				t.Fatalf("unsupported nginx map entry: %s", line)
			}
			definition.entries = append(definition.entries, appIngressMapEntry{
				key: strings.Trim(fields[0], `"`), value: strings.Trim(fields[1], `"`),
			})
		}
		maps[match[2]] = definition
	}
	variables := regexp.MustCompile(`\$\w+`)
	var evaluate func(string, int) string
	evaluate = func(expression string, depth int) string {
		if depth > 8 {
			t.Fatal("cyclic nginx variable")
		}
		return variables.ReplaceAllStringFunc(expression, func(variable string) string {
			switch variable {
			case "$ingress_name":
				return ingress
			case "$http_authorization":
				return headers.Get("Authorization")
			case "$http_cookie":
				return strings.Join(headers.Values("Cookie"), "; ")
			}
			definition, ok := maps[variable]
			if !ok {
				t.Fatalf("undefined nginx variable %s", variable)
			}
			source := evaluate(definition.source, depth+1)
			fallback := ""
			for _, entry := range definition.entries {
				if entry.key == "default" {
					fallback = entry.value
					continue
				}
				matches := source == entry.key
				if strings.HasPrefix(entry.key, "~") {
					pattern := strings.TrimPrefix(entry.key, "~")
					if strings.HasPrefix(pattern, "*") {
						pattern = "(?i)" + strings.TrimPrefix(pattern, "*")
					}
					matches = regexp.MustCompile(pattern).MatchString(source)
				}
				if matches {
					return evaluate(entry.value, depth+1)
				}
			}
			return evaluate(fallback, depth+1)
		})
	}
	result := headers.Clone()
	for header, expression := range config.ProxySetHeaders {
		value := evaluate(expression, 0)
		result.Del(header)
		// nginx suppresses headers whose proxy_set_header value is empty.
		if value != "" {
			result.Set(header, value)
		}
	}
	return result
}

func TestLakeAppIngressCredentials(t *testing.T) {
	lake := testWorkspaceLake()
	apps := isolationTestApps(lake)
	apps["standalone"] = &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "custom-host", Namespace: "lake"}}
	cookieCases := []struct {
		name    string
		cookies []string
		want    string
	}{
		{"app-session", []string{"lake_app_session=app-only; theme=dark"}, "lake_app_session=app-only; theme=dark"},
		{"no-cookie", nil, ""},
		{"control-only", []string{"lake_session=dummy-control"}, ""},
		{"control-first", []string{"lake_session=dummy-control; theme=dark"}, ""},
		{"control-last", []string{"theme=dark; lake_session=dummy-control"}, ""},
		{"host-session", []string{"theme=dark; __Host-lake_session=dummy-control; lake_app_session=app-only"}, ""},
		{"secure-session", []string{"__Secure-lake_session=dummy-control"}, ""},
		{"repeated", []string{"lake_session=; theme=dark; lake_session=dummy-control"}, ""},
		{"split-headers", []string{"theme=dark", "lake_session=dummy-control"}, ""},
		{"case-and-spaces", []string{"theme=dark;\t __HOST-LAKE_SESSION =dummy-control"}, ""},
		{"comma-separated", []string{"theme=dark,lake_session=dummy-control"}, ""},
		{"unrelated-name", []string{"my_lake_session=ordinary; lake_session_extra=ordinary"}, "my_lake_session=ordinary; lake_session_extra=ordinary"},
	}
	for where, config := range appIngressCredentialConfigs(t) {
		t.Run(where, func(t *testing.T) {
			for name, app := range apps {
				for _, auth := range []string{"", oramav1alpha1.LakeAppAuthLake, oramav1alpha1.LakeAppAuthPublic} {
					app.Spec.Auth = auth
					for _, tc := range cookieCases {
						t.Run(name+"/"+auth+"/"+tc.name, func(t *testing.T) {
							headers := http.Header{"Authorization": {"Bearer dummy-control", "Bearer second-dummy"}, "Cookie": tc.cookies}
							got := appIngressFixtureHeaders(t, config, lakeAppWorkloadObjectName(app), headers)
							if len(got.Values("Authorization")) != 0 || got.Get("Cookie") != tc.want {
								t.Fatalf("upstream headers: %v; want Cookie %q and no Authorization", got, tc.want)
							}
						})
					}
				}
			}
			// These names follow the non-app reconcilers: Lake's API/console and its wildcard
			// handlers start with lake-, environments use environment, Cloud v1 and Zetta have
			// component suffixes. A CR named app cannot turn them into a tenant app location.
			for _, ingress := range []string{"lake-app", "lake-app-apps-wildcard", "lake-app-preview-wildcard", environmentWorkloadName(nil), "app-dashboard", "app-docs", "app-writer", "app-reader", "app-zetta", "app-zetta-landing", ""} {
				headers := http.Header{"Authorization": {"Bearer dummy-control"}, "Cookie": {"lake_session=dummy-control; lake_app_session=app-only"}}
				got := appIngressFixtureHeaders(t, config, ingress, headers)
				if got.Get("Authorization") != headers.Get("Authorization") || got.Get("Cookie") != headers.Get("Cookie") {
					t.Errorf("non-app ingress %q lost credentials: %v", ingress, got)
				}
			}
		})
	}
}

func TestLakeAppIngressRedirectCredentials(t *testing.T) {
	for where, config := range appIngressCredentialConfigs(t) {
		t.Run(where, func(t *testing.T) {
			// A redirect-following scripted client deliberately retains its dummy bearer. The
			// fake ingress applies the rendered maps only to the selected app location.
			appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(r.Header)
			}))
			defer appServer.Close()
			target, err := url.Parse(appServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			director := proxy.Director
			proxy.Director = func(r *http.Request) {
				director(r)
				r.Header = appIngressFixtureHeaders(t, config, "app", r.Header)
			}
			ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/authorize" {
					http.Redirect(w, r, "/tenant?__lake_code=dummy-code", http.StatusFound)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer ingress.Close()
			request, err := http.NewRequest(http.MethodGet, ingress.URL+"/authorize", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer dummy-control")
			request.Header.Set("Cookie", "lake_session=dummy-control; lake_app_session=app-only")
			client := ingress.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var received http.Header
			if err := json.NewDecoder(response.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			if received.Get("Authorization") != "" || received.Get("Cookie") != "" {
				t.Fatalf("redirect leaked credentials to app: %v", received)
			}
		})
	}
}

// Reconcile the control plane as a CR named app, so the real API/console and wildcard Ingress
// producers, alongside the generated tenant app names above, pin the naming reservation.
func TestLakeIngressKeepsControlCredentials(t *testing.T) {
	scheme := newLakeScheme(t)
	lake, secret := originTLSLake()
	lake.Name = "app"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, secret).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &networkingv1.Ingress{}, &oramav1alpha1.EdgeZonePolicy{}).Build()
	applyLakeSchemaForWorkloadTest(t, c, scheme, lake)
	reconcileLake(t, c, scheme, lake.Name)
	var ingresses networkingv1.IngressList
	if err := c.List(context.Background(), &ingresses); err != nil {
		t.Fatal(err)
	}
	if len(ingresses.Items) != 3 {
		t.Fatalf("want API/console, apps wildcard and preview wildcard ingresses, got %d", len(ingresses.Items))
	}
	for where, config := range appIngressCredentialConfigs(t) {
		for _, ingress := range ingresses.Items {
			headers := http.Header{"Authorization": {"Bearer dummy-control"}, "Cookie": {"lake_session=dummy-control; lake_app_session=app-only"}}
			got := appIngressFixtureHeaders(t, config, ingress.Name, headers)
			if got.Get("Authorization") != headers.Get("Authorization") || got.Get("Cookie") != headers.Get("Cookie") {
				t.Errorf("%s: generated control-plane ingress %q lost credentials: %v", where, ingress.Name, got)
			}
		}
	}
}
