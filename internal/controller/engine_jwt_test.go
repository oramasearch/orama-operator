/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// jwtProvider is the writer_side.jwt provider as OramaCore reads it.
type jwtProvider struct {
	JWKSURL            string `json:"jwks_url"`
	Issuers, Audiences []string
}

// renderedJWTProviders parses the writer JWT providers out of an OramaCore config.
func renderedJWTProviders(t *testing.T, config string) []jwtProvider {
	t.Helper()
	var cfg struct {
		Writer struct {
			JWT struct{ Providers []jwtProvider }
		} `json:"writer_side"`
	}
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Writer.JWT.Providers
}

// writerJWTOC is a full stack whose writer verifies dashboard tokens for jwt.
func writerJWTOC(jwt oramav1alpha1.WriterJWTSpec, mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.JWTIssuer = "https://app.orama.ing"
		o.Spec.Components.Engine.Writer.JWT = &jwt
		if mut != nil {
			mut(o)
		}
	})
}

func TestWriterJWTRendersAudienceUnion(t *testing.T) {
	for _, tc := range []struct {
		name string
		jwt  oramav1alpha1.WriterJWTSpec
		want []string
	}{
		{"two audiences", oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.a.example", "https://writer.b.example"}},
			[]string{"https://writer.a.example", "https://writer.b.example"}},
		{"audience and audiences", oramav1alpha1.WriterJWTSpec{Audience: "https://writer.a.example", Audiences: []string{"https://writer.b.example", "https://writer.a.example"}},
			[]string{"https://writer.a.example", "https://writer.b.example"}},
		{"deprecated audience alone", oramav1alpha1.WriterJWTSpec{Audience: "https://writer.a.example"},
			[]string{"https://writer.a.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := renderedJWTProviders(t, oramacoreConfigYAML(writerJWTOC(tc.jwt, nil)))
			if len(p) != 1 || !slices.Equal(p[0].Audiences, tc.want) {
				t.Fatalf("providers = %+v, want audiences %v", p, tc.want)
			}
		})
	}
}

func TestWriterJWTDefaultsToInClusterJWKS(t *testing.T) {
	oc := writerJWTOC(oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.a.example"}}, nil)
	cl, _ := reconcileOCV1(t, oc)
	cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
	p := renderedJWTProviders(t, cm.Data[combinedConfigKey])
	if len(p) != 1 || p[0].JWKSURL != "http://v1-dashboard:3000/api/.well-known/jwks.json" {
		t.Fatalf("providers = %+v", p)
	}
	// The default URL names a Service and port the operator actually serves the dashboard on.
	svc := getObj(t, cl, "v1-dashboard", &corev1.Service{})
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 3000 {
		t.Fatalf("dashboard Service ports = %+v", svc.Spec.Ports)
	}

	explicit := writerJWTOC(oramav1alpha1.WriterJWTSpec{JWKSURL: "https://app.orama.ing/api/.well-known/jwks.json", Audiences: []string{"https://writer.a.example"}}, nil)
	if p := renderedJWTProviders(t, oramacoreConfigYAML(explicit)); p[0].JWKSURL != "https://app.orama.ing/api/.well-known/jwks.json" {
		t.Fatalf("explicit jwksURL not kept: %+v", p)
	}
}

func TestWriterJWTRefusesUnverifiableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		oc   *oramav1alpha1.OramaCloudV1
		want string
	}{
		{"empty issuer", writerJWTOC(oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.a.example"}},
			func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = "" }),
			"engine.writer.jwt requires spec.jwtIssuer"},
		{"no audience", writerJWTOC(oramav1alpha1.WriterJWTSpec{}, nil),
			"requires at least one audience"},
		{"no jwksURL without the dashboard", writerJWTOC(oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.a.example"}},
			func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Enabled = false }),
			"jwksURL is required when the dashboard is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.oc).WithStatusSubresource(tc.oc).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tc.oc)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Reconcile error = %v, want %q", err, tc.want)
			}
			ready := apimeta.FindStatusCondition(getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{}).Status.Conditions, "Ready")
			if ready == nil || !strings.Contains(ready.Message, tc.want) {
				t.Fatalf("Ready condition = %+v", ready)
			}
		})
	}
}

func TestWriterJWTIssuerMatchesDashboard(t *testing.T) {
	oc := writerJWTOC(oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.a.example"}}, nil)
	cl, _ := reconcileOCV1(t, oc)
	p := renderedJWTProviders(t, getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{}).Data[combinedConfigKey])
	if len(p) != 1 || len(p[0].Issuers) != 1 || p[0].Issuers[0] != oc.Spec.JWTIssuer {
		t.Fatalf("writer issuers = %+v", p)
	}
	if got := getObj(t, cl, "v1-config", &corev1.ConfigMap{}).Data["JWT_ISSUER"]; got != p[0].Issuers[0] {
		t.Errorf("shared ConfigMap JWT_ISSUER = %q, writer issuers[0] = %q", got, p[0].Issuers[0])
	}
	env := getObj(t, cl, "v1-dashboard", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Env
	if i := slices.IndexFunc(env, func(e corev1.EnvVar) bool { return e.Name == "JWT_ISSUER" }); i < 0 || env[i].Value != p[0].Issuers[0] {
		t.Errorf("dashboard JWT_ISSUER env does not match writer issuers[0] %q", p[0].Issuers[0])
	}
}
