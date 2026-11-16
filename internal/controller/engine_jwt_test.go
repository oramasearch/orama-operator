// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testJWTIssuer = "https://app-dev.nonprod.example"

// jwtProvider is a writer_side.jwt provider as OramaCore reads it.
type jwtProvider struct {
	Name            string   `json:"name"`
	JWKSURL         string   `json:"jwks_url"`
	Issuers         []string `json:"issuers"`
	Audiences       []string `json:"audiences"`
	RefreshInterval string   `json:"refresh_interval"`
}

// jwtProviders parses the writer_side.jwt providers out of an OramaCore config, and reports whether
// writer_side has a jwt key at all.
func jwtProviders(t *testing.T, config string) ([]jwtProvider, bool) {
	t.Helper()
	var cfg struct {
		Writer map[string]any `json:"writer_side"`
	}
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		t.Fatalf("config does not parse as YAML: %v", err)
	}
	raw, ok := cfg.Writer["jwt"]
	if !ok {
		return nil, false
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var jwt struct {
		Providers []jwtProvider `json:"providers"`
	}
	if err := yaml.UnmarshalStrict(b, &jwt); err != nil {
		t.Fatalf("writer_side.jwt does not match the provider shape: %v", err)
	}
	return jwt.Providers, true
}

// jwtOC is a primary CR named v1 with the engine and dashboard enabled, the test issuer, and a writer
// verifying tokens for audiences.
func jwtOC(audiences []string, mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Enabled = true
		o.Spec.JWTIssuer = testJWTIssuer
		o.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{Audiences: audiences}
		if mut != nil {
			mut(o)
		}
	})
}

// The worked example of #384: duplicates collapse in first-occurrence order, the JWKS URL defaults to
// the in-cluster dashboard Service, and the issuer is the shared one.
func TestWriterJWTRendersProvider(t *testing.T) {
	oc := jwtOC([]string{"https://writer.a.example", "https://writer.b.example", "https://writer.a.example"}, nil)
	if err := validateWriterJWT(oc); err != nil {
		t.Fatal(err)
	}
	p, ok := jwtProviders(t, oramacoreWriterConfigYAML(oc))
	want := []jwtProvider{{
		Name:            "dashboard",
		JWKSURL:         "http://v1-dashboard:3000/api/.well-known/jwks.json",
		Issuers:         []string{testJWTIssuer},
		Audiences:       []string{"https://writer.a.example", "https://writer.b.example"},
		RefreshInterval: "1h",
	}}
	if !ok || !reflect.DeepEqual(p, want) {
		t.Fatalf("providers = %+v, want %+v", p, want)
	}

	// Strings are kept exactly: no trimming, sorting or URL normalization.
	exact := []string{"https://writer.b.example/", "HTTPS://Writer.A.example", "https://writer.b.example"}
	if got := writerJWTAudiences(&oramav1alpha1.WriterJWTSpec{Audiences: exact}); !slices.Equal(got, exact) {
		t.Errorf("writerJWTAudiences = %v, want %v", got, exact)
	}

	explicit := "https://app.cloudv1.example/api/.well-known/jwks.json"
	oc.Spec.Components.Engine.Writer.JWT.JWKSURL = explicit
	if p, _ := jwtProviders(t, oramacoreWriterConfigYAML(oc)); len(p) != 1 || p[0].JWKSURL != explicit {
		t.Errorf("explicit jwksURL not rendered as given: %+v", p)
	}
}

// Issuer, URL and audiences are quoted scalars: YAML-significant characters survive parsing unchanged.
func TestWriterJWTQuotesUserStrings(t *testing.T) {
	issuer := `iss "quoted" \ back: #hash 'single' ü`
	audiences := []string{`https://w.example/a: b, c]`, `x" ]\n`, "tab\there"}
	jwks := `https://jwks.example/keys?a=1&b="2"#frag`
	oc := jwtOC(audiences, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.JWTIssuer = issuer
		o.Spec.Components.Engine.Writer.JWT.JWKSURL = jwks
	})
	if err := validateWriterJWT(oc); err != nil {
		t.Fatal(err)
	}
	p, _ := jwtProviders(t, oramacoreConfigYAML(oc))
	if len(p) != 1 || p[0].JWKSURL != jwks || !slices.Equal(p[0].Issuers, []string{issuer}) || !slices.Equal(p[0].Audiences, audiences) {
		t.Fatalf("providers = %+v", p)
	}
}

// Unset engine.writer.jwt renders no jwt block, as before; the issuer falls back to orama-cloud only
// when spec.jwtIssuer is empty.
func TestWriterJWTUnset(t *testing.T) {
	for _, tc := range []struct {
		issuer, want string
	}{{"", "orama-cloud"}, {testJWTIssuer, testJWTIssuer}} {
		oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = tc.issuer })
		h := newEngineHarness(t)
		if err := h.r.reconcileBootstrap(context.Background(), oc); err != nil {
			t.Fatal(err)
		}
		h.reconcile(oc)
		for key, doc := range h.engineConfig().Data {
			if key == "envoy.yaml" {
				continue
			}
			if _, ok := jwtProviders(t, doc); ok {
				t.Errorf("issuer %q: %s has a writer_side.jwt block without engine.writer.jwt", tc.issuer, key)
			}
		}
		cm := &corev1.ConfigMap{}
		h.get("v1-config", cm)
		if got := cm.Data["JWT_ISSUER"]; got != tc.want {
			t.Errorf("spec.jwtIssuer %q: JWT_ISSUER = %q, want %q", tc.issuer, got, tc.want)
		}
	}
	if got := oramacoreConfigYAML(engineOC(nil)); got != combinedConfigFixture {
		t.Error("the combined config without engine.writer.jwt is not the fixture")
	}
}

// The dashboard signs with the shared ConfigMap's JWT_ISSUER and the writer accepts issuers[0]: one
// value, from one function.
func TestWriterJWTIssuerMatchesDashboard(t *testing.T) {
	oc := jwtOC([]string{"https://writer.a.example"}, nil)
	h := newEngineHarness(t)
	ctx := context.Background()
	if err := h.r.reconcileBootstrap(ctx, oc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(oc)
	if err := h.r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	h.get("v1-config", cm)
	p, _ := jwtProviders(t, h.engineConfig().Data[combinedConfigKey])
	if len(p) != 1 || len(p[0].Issuers) != 1 || p[0].Issuers[0] != cm.Data["JWT_ISSUER"] || p[0].Issuers[0] != testJWTIssuer {
		t.Fatalf("writer issuers = %+v, shared JWT_ISSUER = %q", p, cm.Data["JWT_ISSUER"])
	}

	dep := &appsv1.Deployment{}
	h.get("v1-dashboard", dep)
	c := dep.Spec.Template.Spec.Containers[0]
	if !slices.ContainsFunc(c.EnvFrom, func(e corev1.EnvFromSource) bool {
		return e.ConfigMapRef != nil && e.ConfigMapRef.Name == "v1-config"
	}) {
		t.Error("the dashboard does not read the shared ConfigMap through envFrom")
	}
	if slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "JWT_ISSUER" }) {
		t.Error("the dashboard has a second, explicit JWT_ISSUER env value")
	}
}

// Every refusal fails the whole reconcile with Ready=False, reason ReconcileError, naming the field.
func TestWriterJWTRefusals(t *testing.T) {
	long := "https://" + strings.Repeat("a", 121) // 129 characters
	valid := []string{"https://writer.a.example"}
	withURL := func(u string) func(*oramav1alpha1.OramaCloudV1) {
		return func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.JWT.JWKSURL = u }
	}
	for _, tc := range []struct {
		name string
		oc   *oramav1alpha1.OramaCloudV1
		want string
	}{
		{"empty issuer", jwtOC(valid, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = "" }), "requires spec.jwtIssuer"},
		{"issuer too long", jwtOC(valid, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = long }), "spec.jwtIssuer is longer than 128"},
		{"jwt: {}", jwtOC(nil, nil), "jwt.audiences needs at least one entry"},
		{"empty audiences", jwtOC([]string{}, nil), "jwt.audiences needs at least one entry"},
		{"empty item with a valid one", jwtOC([]string{"https://writer.a.example", ""}, nil), "jwt.audiences[1] must be 1 to 128"},
		{"audience too long", jwtOC([]string{long}, nil), "jwt.audiences[0] must be 1 to 128"},
		{"no jwksURL, dashboard disabled", jwtOC(valid, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Enabled = false }),
			"jwt.jwksURL is required when the dashboard is disabled"},
		{"jwksURL without a host", jwtOC(valid, withURL("http:///jwks")), "jwt.jwksURL must be"},
		{"jwksURL with an empty hostname", jwtOC(valid, withURL("http://:3000/jwks")), "jwt.jwksURL must be"},
		{"jwksURL with another scheme", jwtOC(valid, withURL("ftp://dashboard/jwks")), "jwt.jwksURL must be"},
		{"jwksURL that does not parse", jwtOC(valid, withURL("http://dash board%zz/jwks")), "jwt.jwksURL must be"},
		{"jwksURL with credentials", jwtOC(valid, withURL("https://user:pass@app.cloudv1.example/api/.well-known/jwks.json")), "jwt.jwksURL must be"},
		{"jwksURL with a user only", jwtOC(valid, withURL("https://user@app.cloudv1.example/jwks")), "jwt.jwksURL must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, tc.oc)
			err := h.run()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Reconcile error = %v, want it to contain %q", err, tc.want)
			}
			got := h.stored()
			ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			if got.Status.Phase != "Degraded" || ready == nil || ready.Status != metav1.ConditionFalse ||
				ready.Reason != "ReconcileError" || !strings.HasPrefix(ready.Message, "engine: ") || !strings.Contains(ready.Message, tc.want) {
				t.Errorf("phase %q, Ready = %+v", got.Status.Phase, ready)
			}
			// Refused before any engine object is written.
			if h.get("v1-engine-config", &corev1.ConfigMap{}) || h.get("v1-writer", &appsv1.StatefulSet{}) {
				t.Error("a refused writer JWT config wrote engine objects")
			}
		})
	}
}

// An explicit HTTP(S) JWKS URL needs no dashboard; a disabled engine is torn down whatever its JWT says.
func TestWriterJWTAcceptedWithoutDashboardAndIgnoredOnTeardown(t *testing.T) {
	for _, u := range []string{"https://app.cloudv1.example/api/.well-known/jwks.json", "http://10.0.0.5:3000/jwks"} {
		oc := jwtOC([]string{"https://writer.cloudv1.example"}, func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.Enabled = false
			o.Spec.Components.Engine.Writer.JWT.JWKSURL = u
		})
		h := newEngineHarness(t)
		h.reconcile(oc)
		if p, _ := jwtProviders(t, h.engineConfig().Data[combinedConfigKey]); len(p) != 1 || p[0].JWKSURL != u {
			t.Errorf("providers = %+v, want jwks_url %q", p, u)
		}
	}

	h := newEngineHarness(t)
	oc := jwtOC([]string{"https://writer.cloudv1.example"}, nil)
	h.reconcile(oc)
	oc.Spec.Components.Engine.Enabled = false
	oc.Spec.JWTIssuer = ""
	oc.Spec.Components.Engine.Writer.JWT.Audiences = nil
	h.reconcile(oc)
	if h.get("v1-writer", &appsv1.StatefulSet{}) || h.get("v1-engine-config", &corev1.ConfigMap{}) {
		t.Error("an invalid writer JWT config blocked the disabled engine's teardown")
	}
}

// Each JWT change rolls the writer through its config hash and leaves a reader-only config alone;
// the same settings rendered again leave the hash stable.
func TestWriterJWTChangesRollTheWriter(t *testing.T) {
	changes := []struct {
		name string
		mut  func(*oramav1alpha1.OramaCloudV1)
	}{
		{"issuer", func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = "https://app.cloudv1.example" }},
		{"jwksURL", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.JWT.JWKSURL = "https://app.cloudv1.example/api/.well-known/jwks.json"
		}},
		{"audiences", func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.JWT.Audiences = append(o.Spec.Components.Engine.Writer.JWT.Audiences, "https://writer.b.example")
		}},
		{"unset", func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.JWT = nil }},
	}
	for mode, sides := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		for _, ch := range changes {
			t.Run(mode+"/"+ch.name, func(t *testing.T) {
				oc := jwtOC([]string{"https://writer.a.example"}, sides)
				h := newEngineHarness(t)
				h.reconcile(oc)
				writerHash := h.writer().Spec.Template.Annotations[configHashKey]
				readerHash := h.reader().Spec.Template.Annotations[configHashKey]

				h.reconcile(oc.DeepCopy())
				if h.writer().Spec.Template.Annotations[configHashKey] != writerHash {
					t.Fatal("the same JWT settings changed the writer config hash")
				}

				ch.mut(oc)
				h.reconcile(oc)
				writer := h.writer().Spec.Template
				if got := writer.Annotations[configHashKey]; got == writerHash || got != templateSHA256(h.engineConfig().Data[renderSource(t, writer.Spec)]) {
					t.Errorf("changing %s did not roll the writer to its new config", ch.name)
				}
				readerMoved := h.reader().Spec.Template.Annotations[configHashKey] != readerHash
				if sides != nil && readerMoved {
					t.Errorf("changing %s changed the reader-only config hash", ch.name)
				}
			})
		}
	}

	// The writer-only and combined configs carry the provider; the reader-only config never does.
	oc := jwtOC([]string{"https://writer.a.example"}, withSideImages)
	for side, want := range map[string]bool{"writer": true, "reader": false} {
		if _, ok := jwtProviders(t, engineSideFor(oc, side).template); ok != want {
			t.Errorf("side-only %s config has a writer_side.jwt block = %v, want %v", side, ok, want)
		}
	}
	if strings.Contains(oramacoreReaderConfigYAML(oc), "jwks_url") {
		t.Error("the reader-only config names a JWKS URL")
	}
	if _, ok := jwtProviders(t, oramacoreConfigYAML(oc)); !ok {
		t.Error("the combined config has no writer_side.jwt block")
	}
}

// Only a change to the effective issuer rolls the dashboard.
func TestJWTIssuerRollsTheDashboard(t *testing.T) {
	h := newEngineHarness(t)
	ctx := context.Background()
	oc := jwtOC([]string{"https://writer.a.example"}, nil)
	template := func() corev1.PodTemplateSpec {
		t.Helper()
		if err := h.r.reconcileDashboard(ctx, oc); err != nil {
			t.Fatal(err)
		}
		dep := &appsv1.Deployment{}
		h.get("v1-dashboard", dep)
		return dep.Spec.Template
	}
	before := template()
	if got := before.Annotations[jwtIssuerHashKey]; got != templateSHA256(testJWTIssuer) {
		t.Fatalf("%s = %q, want the SHA-256 of the issuer", jwtIssuerHashKey, got)
	}

	oc.Spec.Components.Engine.Writer.JWT.Audiences = []string{"https://writer.b.example"}
	oc.Spec.Components.Engine.Writer.JWT.JWKSURL = "https://app.cloudv1.example/api/.well-known/jwks.json"
	if !reflect.DeepEqual(template(), before) {
		t.Error("an audience or JWKS URL change changed the dashboard pod template")
	}

	oc.Spec.JWTIssuer = "https://app.cloudv1.example"
	if template().Annotations[jwtIssuerHashKey] == before.Annotations[jwtIssuerHashKey] {
		t.Error("an issuer change did not roll the dashboard")
	}
	oc.Spec.JWTIssuer = ""
	if template().Annotations[jwtIssuerHashKey] != templateSHA256("orama-cloud") {
		t.Error("the dashboard hash does not follow the effective issuer orama-cloud")
	}
}

// The final shape: no CRD default on jwtIssuer, and the audience list alone in the Go type and in
// both CRD copies.
func TestWriterJWTSchemaShape(t *testing.T) {
	var fields []string
	typ := reflect.TypeFor[oramav1alpha1.WriterJWTSpec]()
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Tag.Get("json"))
	}
	if want := []string{"jwksURL,omitempty", "audiences,omitempty"}; !slices.Equal(fields, want) {
		t.Errorf("WriterJWTSpec json fields = %v, want %v", fields, want)
	}

	type prop struct {
		Default    any             `json:"default"`
		MaxLength  *int            `json:"maxLength"`
		Items      *prop           `json:"items"`
		MinLength  *int            `json:"minLength"`
		Properties map[string]prop `json:"properties"`
	}
	var copies [][]byte
	for _, path := range []string{"../../config/crd/bases/orama.cloud_oramacloudv1s.yaml", "../../chart/crds/orama.cloud_oramacloudv1s.yaml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, b)
		var crd struct {
			Spec struct {
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema prop `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatal(err)
		}
		spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		issuer := spec.Properties["jwtIssuer"]
		if issuer.Default != nil || issuer.MaxLength == nil || *issuer.MaxLength != 128 {
			t.Errorf("%s: jwtIssuer = %+v, want maxLength 128 and no default", path, issuer)
		}
		jwt := spec.Properties["components"].Properties["engine"].Properties["writer"].Properties["jwt"]
		var names []string
		for k := range jwt.Properties {
			names = append(names, k)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"audiences", "jwksURL"}) {
			t.Errorf("%s: writer.jwt properties = %v, want audiences and jwksURL", path, names)
		}
		if it := jwt.Properties["audiences"].Items; it == nil || it.MinLength == nil || *it.MinLength != 1 || it.MaxLength == nil || *it.MaxLength != 128 {
			t.Errorf("%s: audiences items = %+v, want 1 to 128 characters", path, it)
		}
	}
	if string(copies[0]) != string(copies[1]) {
		t.Error("chart/crds and config/crd/bases differ")
	}
}
