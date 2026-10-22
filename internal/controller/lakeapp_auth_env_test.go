package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// app-runtime trusts X-Orama-User only when APP_AUTH is exactly lake, because only then does the
// Ingress replace the client's copy with Lake's forward-auth answer (app-runtime#11). The pod must
// be told its real auth mode, and nothing the app's owner configures may change it.
func TestALakeAppPodIsToldItsAuthModeAndCannotOverrideIt(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "lake"}}
	for _, test := range []struct {
		auth string
		want string
	}{
		{auth: "", want: oramav1alpha1.LakeAppAuthLake},
		{auth: oramav1alpha1.LakeAppAuthLake, want: oramav1alpha1.LakeAppAuthLake},
		{auth: oramav1alpha1.LakeAppAuthPublic, want: oramav1alpha1.LakeAppAuthPublic},
		// Never "protected" unless the Ingress really puts forward auth in front of the app.
		{auth: "Lake", want: oramav1alpha1.LakeAppAuthPublic},
	} {
		for _, mode := range []string{oramav1alpha1.LakeAppModeRuntime, oramav1alpha1.LakeAppModeImage} {
			app := &oramav1alpha1.LakeApp{
				ObjectMeta: metav1.ObjectMeta{Name: "dashboard", Namespace: "lake"},
				Spec: oramav1alpha1.LakeAppSpec{
					Mode: mode,
					Port: 8080,
					Auth: test.auth,
					Env:  []oramav1alpha1.LakeEnvironmentVariable{{Name: "APP_AUTH", Value: "lake"}},
				},
			}
			secrets := lakeAppSecrets{SecretEnv: []corev1.EnvVar{secretEnv("APP_AUTH", "owner-secret", "auth")}}
			env := lakeAppManagedEnv(lake, app, secrets)
			found := 0
			for _, variable := range env {
				if variable.Name != "APP_AUTH" {
					continue
				}
				found++
				if variable.Value != test.want || variable.ValueFrom != nil {
					t.Fatalf("auth %q mode %s: APP_AUTH = %+v, want %q", test.auth, mode, variable, test.want)
				}
			}
			if found != 1 {
				t.Fatalf("auth %q mode %s: APP_AUTH appears %d times, want once", test.auth, mode, found)
			}
			if lakeAppAuth(app) != test.want {
				t.Fatalf("auth %q: lakeAppAuth = %q, want %q", test.auth, lakeAppAuth(app), test.want)
			}
		}
	}
}
