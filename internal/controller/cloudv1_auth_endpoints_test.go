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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestCloudV1JWTAndFirebase(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.JWTIssuer = "https://app.orama.ing"
		o.Spec.IDP = oramav1alpha1.IDPSpec{Provider: "firebase", ConfigSecretRef: &corev1.LocalObjectReference{Name: "firebase-server"}}
		o.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{
			JWKSURL: "https://app.orama.ing/api/.well-known/jwks.json", Audience: "https://writer.prod2.cluster.orama.ing",
		}
	})
	cl, r := reconcileOCV1(t, oc)
	var cfg struct {
		Writer struct {
			JWT struct {
				Providers []struct {
					JWKSURL            string `json:"jwks_url"`
					Issuers, Audiences []string
				}
			}
		} `json:"writer_side"`
	}
	if err := yaml.Unmarshal([]byte(oramacoreConfigYAML(oc)), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Writer.JWT.Providers
	if len(p) != 1 || p[0].JWKSURL != oc.Spec.Components.Engine.Writer.JWT.JWKSURL || !slices.Equal(p[0].Issuers, []string{oc.Spec.JWTIssuer}) || !slices.Equal(p[0].Audiences, []string{oc.Spec.Components.Engine.Writer.JWT.Audience}) {
		t.Fatalf("writer JWT provider = %+v", p)
	}
	if got := getObj(t, cl, "v1-config", &corev1.ConfigMap{}).Data["JWT_ISSUER"]; got != oc.Spec.JWTIssuer {
		t.Fatalf("issuer = %q", got)
	}
	dep := getObj(t, cl, "v1-dashboard", &appsv1.Deployment{})
	for _, key := range []string{"FIREBASE_PROJECT_ID", "FIREBASE_CLIENT_EMAIL", "FIREBASE_PRIVATE_KEY"} {
		if !slices.ContainsFunc(dep.Spec.Template.Spec.Containers[0].Env, func(e corev1.EnvVar) bool {
			return e.Name == key && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "firebase-server" && e.ValueFrom.SecretKeyRef.Key == key && e.ValueFrom.SecretKeyRef.Optional == nil
		}) {
			t.Errorf("missing required Secret reference for %s", key)
		}
	}
	writer := getObj(t, cl, "v1-writer", &appsv1.StatefulSet{})
	hash := writer.Spec.Template.Annotations["orama.cloud/config-hash"]
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"] != hash {
		t.Error("unchanged JWT config changed rollout hash")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = "new-issuer" })
	if getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"] == hash {
		t.Error("changed issuer did not roll engine config")
	}

	defaults := newOCV1(nil)
	if jwtIssuer(defaults) != "orama-cloud" || strings.Contains(oramacoreConfigYAML(defaults), "  jwt:") {
		t.Error("default JWT behavior changed")
	}
	if _, exists := componentSecretData("dashboard")["FIREBASE_SERVICE_ACCOUNT"]; exists {
		t.Error("nonfunctional Firebase dummy is still seeded")
	}
	oc.Spec.IDP.ConfigSecretRef = nil
	if err := r.reconcileDashboard(context.Background(), oc); err == nil {
		t.Error("Firebase without a Secret reference must fail visibly")
	}
}

func TestCloudV1JWTRecoversPublicSecretWithoutRotation(t *testing.T) {
	oc := newOCV1(nil)
	scheme := ocv1TestScheme(t)
	jwt := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-jwt", Namespace: oc.Namespace},
		Data: map[string][]byte{"JWT_PRIVATE_KEY": []byte("preserved-private"), "JWT_PUBLIC_KEY": []byte("preserved-public")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(jwt).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
	if err := r.ensureJWTSecrets(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	pub := getObj(t, cl, "v1-jwt-pub", &corev1.Secret{})
	if len(pub.StringData) != 1 || pub.StringData["JWT_PUBLIC_KEY"] != "preserved-public" || !metav1.IsControlledBy(pub, oc) {
		t.Error("public-only Secret did not recover from preserved pair")
	}
	if getObj(t, cl, "v1-jwt", &corev1.Secret{}).ResourceVersion != jwt.ResourceVersion {
		t.Error("recovering public Secret changed signing pair")
	}
	version := pub.ResourceVersion
	if err := r.ensureJWTSecrets(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if getObj(t, cl, pub.Name, &corev1.Secret{}).ResourceVersion != version {
		t.Error("steady key recovery rewrote Secret")
	}
}
