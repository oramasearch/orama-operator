package controller

import (
	"encoding/base64"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestStackInternalCredentialsAreSharedAndStable(t *testing.T) {
	oc := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Search = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		o.Spec.Components.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		o.Spec.Components.SearchWorkers.Enabled = true
		o.Spec.Components.Disco.Enabled = true
	})
	provided := secretOf("v1-typesense-auth", map[string]string{"TYPESENSE_API_KEY": "provided-typesense-key"})
	h := newEngineHarness(t, oc, engineSecret(), provided)
	h.mustRun()

	for _, name := range []string{"v1-clickhouse-auth", "v1-typesense-auth", "v1-cron-secret", "v1-decrypter-auth", "v1-index-builder-auth", "v1-deployment-api-auth"} {
		sec := h.secret(name)
		if name != provided.Name && sec.Labels[componentLabel] != credentialsComponent {
			t.Errorf("%s has component label %q", name, sec.Labels[componentLabel])
		}
		if name != provided.Name && (len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != oc.Name) {
			t.Errorf("%s owner references = %+v", name, sec.OwnerReferences)
		}
		if len(sec.Data) == 0 {
			t.Errorf("%s has no data", name)
		}
	}
	if got := h.secret(provided.Name); !reflect.DeepEqual(got.Data, provided.Data) {
		t.Fatal("reconcile replaced the supplied Typesense key")
	}
	decrypter := h.secret("v1-decrypter-auth")
	decoded, err := base64.StdEncoding.DecodeString(string(decrypter.Data["DECRYPTER_API_SECRET_BASE64"]))
	if err != nil || string(decoded) != string(decrypter.Data["API_SECRET"]) {
		t.Fatal("the decrypter sender and receiver keys disagree")
	}

	templates := h.templates()
	for _, tc := range []struct{ workload, container, env, secret, key string }{
		{"analytics", "analytics", "CLICKHOUSE_PASSWORD", "v1-clickhouse-auth", "CLICKHOUSE_PASSWORD"},
		{"analytics-processor", "render-config", "CLICKHOUSE_PASSWORD", "v1-clickhouse-auth", "CLICKHOUSE_PASSWORD"},
		{"dashboard", "dashboard", "CLICKHOUSE_PASSWORD", "v1-clickhouse-auth", "CLICKHOUSE_PASSWORD"},
		{"workers", "workers", "CLICKHOUSE_PASSWORD", "v1-clickhouse-auth", "CLICKHOUSE_PASSWORD"},
		{"disco", "disco", "CLICKHOUSE_PASSWORD", "v1-clickhouse-auth", "CLICKHOUSE_PASSWORD"},
		{"search", "search", "TYPESENSE_API_KEY", "v1-typesense-auth", "TYPESENSE_API_KEY"},
		{"index-builder", "index-builder", "TYPESENSE_API_KEY", "v1-typesense-auth", "TYPESENSE_API_KEY"},
		{"index-builder", "index-builder", "DECRYPTER_API_SECRET_BASE64", "v1-decrypter-auth", "DECRYPTER_API_SECRET_BASE64"},
		{"openai-decrypter", "openai-decrypter", "API_SECRET", "v1-decrypter-auth", "API_SECRET"},
		{"index-builder", "index-builder", "API_SECRET", "v1-index-builder-auth", "API_SECRET"},
		{"index-builder", "index-builder", "DEPLOYMENT_API_API_KEY", "v1-deployment-api-auth", "DEPLOYMENT_API_API_KEY"},
		{"dashboard", "dashboard", "CRON_SECRET", "v1-cron-secret", "CRON_SECRET"},
	} {
		tmpl, ok := templates[tc.workload]
		if !ok {
			t.Errorf("%s was not rendered", tc.workload)
			continue
		}
		container := containerIn(t, tmpl.Spec, tc.container)
		ref := secretRef(container.Env, tc.env)
		if ref == nil || ref.Name != tc.secret || ref.Key != tc.key {
			t.Errorf("%s %s = %+v, want %s/%s", tc.workload, tc.env, ref, tc.secret, tc.key)
		}
	}

	stable := map[string]*corev1.Secret{}
	for _, name := range []string{"v1-clickhouse-auth", "v1-typesense-auth", "v1-cron-secret", "v1-decrypter-auth", "v1-index-builder-auth", "v1-deployment-api-auth"} {
		stable[name] = h.secret(name).DeepCopy()
	}
	h.mustRun()
	for name, before := range stable {
		if !reflect.DeepEqual(h.secret(name).Data, before.Data) {
			t.Errorf("a second reconcile changed %s", name)
		}
	}
	for _, workload := range []string{"analytics", "analytics-processor", "dashboard", "workers", "disco"} {
		if templates[workload].Annotations[credentialsHashKey] == "" {
			t.Errorf("%s has no credential hash", workload)
		}
	}
	for _, tc := range []struct {
		secret, key string
		consumers   []string
	}{
		{"v1-clickhouse-auth", "CLICKHOUSE_PASSWORD", []string{"analytics", "analytics-processor", "dashboard", "workers", "disco"}},
		{"v1-typesense-auth", "TYPESENSE_API_KEY", []string{"search", "index-builder"}},
		{"v1-cron-secret", "CRON_SECRET", []string{"dashboard"}},
		{"v1-decrypter-auth", "API_SECRET", []string{"index-builder", "openai-decrypter"}},
		{"v1-index-builder-auth", "API_SECRET", []string{"index-builder"}},
		{"v1-deployment-api-auth", "DEPLOYMENT_API_API_KEY", []string{"index-builder"}},
	} {
		before := h.templates()
		value := "updated-" + tc.secret
		h.setSecret(tc.secret, tc.key, value)
		if tc.secret == "v1-decrypter-auth" {
			h.setSecret(tc.secret, "DECRYPTER_API_SECRET_BASE64", base64.StdEncoding.EncodeToString([]byte(value)))
		}
		h.mustRun()
		after := h.templates()
		for _, consumer := range tc.consumers {
			if before[consumer].Annotations[credentialsHashKey] == after[consumer].Annotations[credentialsHashKey] {
				t.Errorf("%s did not roll after %s changed", consumer, tc.secret)
			}
		}
	}
}

func TestDecrypterCredentialPairMustAgree(t *testing.T) {
	provided := secretOf("v1-decrypter-auth", map[string]string{
		"DECRYPTER_API_SECRET_BASE64": base64.StdEncoding.EncodeToString([]byte("sender")),
		"API_SECRET":                  "receiver",
	})
	h := newEngineHarness(t, stackOC(nil), engineSecret(), provided)
	h.mustFail("keys DECRYPTER_API_SECRET_BASE64 and API_SECRET disagree")
	if !reflect.DeepEqual(h.secret(provided.Name).Data, provided.Data) {
		t.Fatal("the invalid supplied Secret was changed")
	}
}
