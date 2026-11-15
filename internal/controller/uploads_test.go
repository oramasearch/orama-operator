// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestStackUploadsBucket(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		cloudflare, dashboard bool
	}{
		{"enabled", true, true}, {"no Cloudflare", false, true}, {"no dashboard", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Cloudflare.Enabled = tc.cloudflare
				o.Spec.Cloudflare.APITokenSecretRef = &corev1.LocalObjectReference{Name: "cloudflare"}
				o.Spec.Components.Dashboard.Enabled = tc.dashboard
				o.Spec.Components.Dashboard.Host = "app.example"
				o.Spec.Components.Dashboard.ExtraHosts = []string{"extra.example", "other.example"}
				o.Spec.Components.Ingestion.Enabled = true
			})
			h := newEngineHarness(t, oc)
			if err := h.r.reconcileCloudflare(context.Background(), oc); err != nil {
				t.Fatal(err)
			}
			var bucket oramav1alpha1.CloudflareR2Bucket
			found := h.get("v1-uploads", &bucket)
			if found != (tc.cloudflare && tc.dashboard) {
				t.Fatalf("uploads bucket exists = %v", found)
			}
			if !found {
				return
			}
			spec := bucket.Spec
			if !metav1.IsControlledBy(&bucket, oc) || spec.SecretRef != "cloudflare" || spec.Name != "v1-uploads" || spec.DeletionPolicy != oramav1alpha1.BucketDeletionDelete || spec.Credentials == nil || spec.Credentials.SecretName != "v1-uploads-r2" || spec.ExpireAfterDays == nil || *spec.ExpireAfterDays != 7 {
				t.Fatalf("uploads bucket = %+v", bucket)
			}
			want := []oramav1alpha1.CloudflareR2CORSRule{{AllowedOrigins: []string{"https://app.example", "https://extra.example", "https://other.example"}, AllowedMethods: []string{"PUT"}, AllowedHeaders: []string{"*"}, MaxAgeSeconds: 3600}}
			if !reflect.DeepEqual(spec.CORS, want) {
				t.Fatalf("CORS = %+v", spec.CORS)
			}
			var legacy oramav1alpha1.CloudflareR2Bucket
			if !h.get("v1-legacy-indexes", &legacy) || legacy.Spec.CORS != nil || legacy.Spec.ExpireAfterDays != nil || legacy.Spec.Credentials != nil {
				t.Fatal("legacy bucket changed")
			}
			version := bucket.ResourceVersion
			if err := h.r.reconcileCloudflare(context.Background(), oc); err != nil {
				t.Fatal(err)
			}
			h.get("v1-uploads", &bucket)
			if version != bucket.ResourceVersion {
				t.Fatal("steady reconcile rewrote uploads bucket")
			}
		})
	}
}

// Assert each mapping independently of the consumer code that chooses its variable set.
func TestUploadsBucketEnvMappings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names uploadsEnvNames
		want  []string
	}{
		{"dashboard", dashboardUploadsEnv, []string{"S3_ENDPOINT", "S3_BUCKET", "S3_BUCKET_REGION", "AWS_ACCESS_KEY_VALUE", "AWS_SECRET_ACCESS_KEY_VALUE"}},
		{"crawler", crawlerUploadsEnv, []string{"AWS_ENDPOINT_URL_S3", "S3_BUCKET_NAME", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}},
		{"consumer", importConsumerUploadsEnv, []string{"S3_ENDPOINT", "S3_BUCKET", "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := uploadsBucketEnv(newOCV1(nil), tc.names)
			keys := []string{"endpoint", "bucket", "region", "accessKeyId", "secretAccessKey"}
			if len(env) != 5 {
				t.Fatalf("env count = %d", len(env))
			}
			for i, variable := range env {
				if variable.Name != tc.want[i] || variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
					t.Fatalf("invalid env %s", variable.Name)
				}
				ref := variable.ValueFrom.SecretKeyRef
				if ref.Name != "v1-uploads-r2" || ref.Key != keys[i] || (ref.Optional != nil && *ref.Optional) {
					t.Fatalf("%s reference = %+v", variable.Name, ref)
				}
			}
		})
	}
}

func TestDashboardUploadsOverrideEnvFromAndRotate(t *testing.T) {
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-placeholders")}
	})
	oc.Spec.Cloudflare.Enabled = true
	placeholders := secretOf("dashboard-placeholders", map[string]string{
		"S3_ENDPOINT": "placeholder", "S3_BUCKET": "placeholder", "S3_BUCKET_REGION": "placeholder",
		"AWS_ACCESS_KEY_VALUE": "placeholder", "AWS_SECRET_ACCESS_KEY_VALUE": "placeholder",
	})
	h := newEngineHarness(t, oc, placeholders, icUploads("first"))
	h.mustRun()
	dep := h.dashboard()
	container := dep.Spec.Template.Spec.Containers[0]
	if !slices.ContainsFunc(container.EnvFrom, func(source corev1.EnvFromSource) bool {
		return source.SecretRef != nil && source.SecretRef.Name == placeholders.Name
	}) {
		t.Fatal("missing placeholder envFrom")
	}
	env := envByName(container)
	for _, variable := range dashboardUploadsBucketEnv(oc) {
		if !reflect.DeepEqual(env[variable.Name], variable) {
			t.Fatalf("%s does not override envFrom with the uploads Secret", variable.Name)
		}
	}
	before := dep.Spec.Template.Annotations[credentialsHashKey]
	secret := &corev1.Secret{}
	h.get(icUploadsSecret, secret)
	secret.Data["secretAccessKey"] = []byte("rotated")
	if err := h.cl.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	if h.dashboard().Spec.Template.Annotations[credentialsHashKey] == before {
		t.Fatal("bucket credential rotation did not roll dashboard")
	}
}

// Optional bucket references keep dashboard pods startable before provisioning succeeds.
func TestDashboardUploadsSecretMayBeAbsent(t *testing.T) {
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dashboard-placeholders")}
	})
	oc.Spec.Cloudflare.Enabled = true
	h := newEngineHarness(t, oc, secretOf("dashboard-placeholders", map[string]string{"S3_ENDPOINT": "placeholder"}))
	h.mustRun()
	dep := h.dashboard()
	env := envByName(dep.Spec.Template.Spec.Containers[0])
	for _, name := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_BUCKET_REGION", "AWS_ACCESS_KEY_VALUE", "AWS_SECRET_ACCESS_KEY_VALUE"} {
		variable := env[name]
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s has no Secret reference", name)
		}
		ref := variable.ValueFrom.SecretKeyRef
		if ref.Name != "v1-uploads-r2" || ref.Optional == nil || !*ref.Optional {
			t.Fatalf("%s must be an optional uploads Secret reference", name)
		}
	}
	before := dep.Spec.Template.Annotations[credentialsHashKey]
	if err := h.cl.Create(context.Background(), icUploads("first")); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	after := h.dashboard().Spec.Template.Annotations[credentialsHashKey]
	if before == after {
		t.Fatal("dashboard did not roll when bucket credentials appeared")
	}
	h.mustRun()
	if h.dashboard().Spec.Template.Annotations[credentialsHashKey] != after {
		t.Fatal("unchanged bucket credentials rolled dashboard again")
	}
}
