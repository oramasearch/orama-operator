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
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// thirdPartyStack is stackOC with Prod 1's providers keyed from v1-llm-keys, the workers' Sentry DSN
// from v1-sentry, and the Cloudflare token Secret cloudflare-api-token.
func thirdPartyStack() *oramav1alpha1.OramaCloudV1 {
	return stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		for i := range o.Spec.LLM.Providers {
			o.Spec.LLM.Providers[i].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "v1-llm-keys"}
		}
		o.Spec.Components.Workers.SentryDSNSecretRef = &oramav1alpha1.SecretKeyRef{Name: "v1-sentry", Key: "dsn"}
		o.Spec.Cloudflare.APITokenSecretRef = &corev1.LocalObjectReference{Name: "cloudflare-api-token"}
	})
}

func thirdPartySecrets() []client.Object {
	return []client.Object{
		engineSecret(),
		secretOf("v1-llm-keys", map[string]string{
			"OPENAI_API_KEY": "sk-openai-real", "GOOGLE_API_KEY": "google-real", "GROQ_API_KEY": "groq-real", "ANTHROPIC_API_KEY": "anthropic-real",
		}),
		secretOf("v1-sentry", map[string]string{"dsn": "https://key@sentry.example/1"}),
		secretOf("cloudflare-api-token", map[string]string{"token": "cf-token-real", "accountID": "cf-account-real", "zoneID": "cf-zone"}),
	}
}

// secretRef returns the secretKeyRef of the env entry named name, or nil.
func secretRef(env []corev1.EnvVar, name string) *corev1.SecretKeySelector {
	for _, e := range env {
		if e.Name == name && e.ValueFrom != nil {
			return e.ValueFrom.SecretKeyRef
		}
	}
	return nil
}

// The workers take their LLM keys from the engine's providers' Secrets and SENTRY_DSN from
// sentryDSNSecretRef, index-builder its Cloudflare token and account from cloudflare.apiTokenSecretRef;
// each explicit entry wins over the component Secret's envFrom, none is seeded with a dev-* value, a
// changed value rolls the workload, and a missing Secret or key fails the step, naming it (#571).
func TestThirdPartyKeysComeFromTheirOwnSecrets(t *testing.T) {
	h := newEngineHarness(t, append(thirdPartySecrets(), thirdPartyStack())...)
	h.mustRun()

	for _, tc := range []struct {
		component, deployment string
		want                  map[string][2]string // env -> Secret, key
		effective             map[string]string
		unseeded              []string
		kept                  []string
	}{
		{"workers", "v1-workers",
			map[string][2]string{
				"OPENAI_API_KEY": {"v1-llm-keys", "OPENAI_API_KEY"}, "GROQ_API_KEY": {"v1-llm-keys", "GROQ_API_KEY"},
				"ANTHROPIC_API_KEY": {"v1-llm-keys", "ANTHROPIC_API_KEY"}, "SENTRY_DSN": {"v1-sentry", "dsn"},
			},
			map[string]string{"OPENAI_API_KEY": "sk-openai-real", "GROQ_API_KEY": "groq-real",
				"ANTHROPIC_API_KEY": "anthropic-real", "SENTRY_DSN": "https://key@sentry.example/1"},
			[]string{"OPENAI_API_KEY", "GROQ_API_KEY", "ANTHROPIC_API_KEY", "SENTRY_DSN"},
			[]string{"QSTASH_TOKEN", "DISCO_SECRET"}},
		{"index-builder", "v1-index-builder",
			map[string][2]string{
				"CLOUDFLARE_API_TOKEN": {"cloudflare-api-token", "token"}, "CLOUDFLARE_ACCOUNT_ID": {"cloudflare-api-token", "accountID"},
			},
			map[string]string{"CLOUDFLARE_API_TOKEN": "cf-token-real", "CLOUDFLARE_ACCOUNT_ID": "cf-account-real"},
			[]string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID"},
			[]string{"ABLY_API_KEY", "APIFY_TOKEN"}},
	} {
		t.Run(tc.component, func(t *testing.T) {
			dep := &appsv1.Deployment{}
			if !h.get(tc.deployment, dep) {
				t.Fatalf("Deployment %s not found", tc.deployment)
			}
			c := containerIn(t, dep.Spec.Template.Spec, tc.component)
			for env, want := range tc.want {
				ref := secretRef(c.Env, env)
				if ref == nil || ref.Name != want[0] || ref.Key != want[1] {
					t.Errorf("%s = %+v, want a secretKeyRef to %s/%s", env, ref, want[0], want[1])
				}
			}
			// The explicit entry wins over the component Secret's envFrom, which still exists.
			if !slices.ContainsFunc(c.EnvFrom, func(s corev1.EnvFromSource) bool {
				return s.SecretRef != nil && s.SecretRef.Name == componentSecretName(h.stored(), tc.component)
			}) {
				t.Fatalf("%s no longer loads its component Secret", tc.component)
			}
			got := h.effectiveEnv(c)
			for env, want := range tc.effective {
				if got[env] != want {
					t.Errorf("effective %s is not the referenced value", env)
				}
			}
			sec := h.secret(componentSecretName(h.stored(), tc.component))
			for _, k := range tc.unseeded {
				if _, ok := sec.Data[k]; ok {
					t.Errorf("%s is seeded into %s although its reference is set", k, sec.Name)
				}
			}
			for _, k := range tc.kept {
				if _, ok := sec.Data[k]; !ok {
					t.Errorf("%s is missing from %s; only referenced keys stop being seeded", k, sec.Name)
				}
			}
		})
	}

	// Google's key is the engine's alone: the workers do not read it.
	workers := &appsv1.Deployment{}
	h.get("v1-workers", workers)
	if secretRef(containerIn(t, workers.Spec.Template.Spec, "workers").Env, "GOOGLE_API_KEY") != nil {
		t.Error("the workers read GOOGLE_API_KEY")
	}

	// A changed referenced value rolls the workload that reads it, and only that one.
	hashes := func() (string, string) {
		w, ib := &appsv1.Deployment{}, &appsv1.Deployment{}
		h.get("v1-workers", w)
		h.get("v1-index-builder", ib)
		return w.Spec.Template.Annotations[credentialsHashKey], ib.Spec.Template.Annotations[credentialsHashKey]
	}
	w0, ib0 := hashes()
	h.setSecret("v1-sentry", "dsn", "https://rotated@sentry.example/1")
	h.mustRun()
	w1, ib1 := hashes()
	if w1 == w0 || ib1 != ib0 {
		t.Errorf("rotating the Sentry DSN: workers hash moved %v (want true), index-builder moved %v (want false)", w1 != w0, ib1 != ib0)
	}
	h.setSecret("cloudflare-api-token", "token", "cf-token-rotated")
	h.mustRun()
	w2, ib2 := hashes()
	if ib2 == ib1 || w2 != w1 {
		t.Errorf("rotating the Cloudflare token: index-builder hash moved %v (want true), workers moved %v (want false)", ib2 != ib1, w2 != w1)
	}

	// The Secret watch maps each referenced Secret back to the CR.
	for _, name := range []string{"v1-llm-keys", "v1-sentry", "cloudflare-api-token"} {
		if !slices.Contains(secretsReadBy(h.stored()), name) {
			t.Errorf("secretsReadBy does not include %s", name)
		}
	}

	// A missing key or Secret fails the step, naming the variable, the field, the Secret and the key,
	// and leaves the running template as it was.
	sentry := h.secret("v1-sentry")
	delete(sentry.Data, "dsn")
	if err := h.cl.Update(context.Background(), sentry); err != nil {
		t.Fatal(err)
	}
	h.mustFail("SENTRY_DSN: Secret orama-system/v1-sentry (spec.components.workers.sentryDSNSecretRef) has no key dsn")
	if w, _ := hashes(); w != w2 {
		t.Error("a failed reference changed the workers template")
	}
	if c := h.readyCondition(); c == nil || c.Status != "False" || !strings.Contains(c.Message, "workers: SENTRY_DSN") {
		t.Errorf("Ready = %+v, want it to name workers: SENTRY_DSN", c)
	}
	h.put(secretOf("v1-sentry", map[string]string{"dsn": "https://key@sentry.example/1"}))
	cf := h.secret("cloudflare-api-token")
	delete(cf.Data, "accountID")
	if err := h.cl.Update(context.Background(), cf); err != nil {
		t.Fatal(err)
	}
	h.mustFail("CLOUDFLARE_ACCOUNT_ID: Secret orama-system/cloudflare-api-token (spec.cloudflare.apiTokenSecretRef) has no key accountID")
	if err := h.cl.Delete(context.Background(), h.secret("cloudflare-api-token")); err != nil {
		t.Fatal(err)
	}
	h.mustFail("CLOUDFLARE_API_TOKEN: Secret orama-system/cloudflare-api-token (spec.cloudflare.apiTokenSecretRef) does not exist")
}

// Without references nothing changes: the workers and index-builder read every key from their
// component Secrets, which are seeded as before.
func TestThirdPartyKeysWithoutReferencesKeepTheComponentSecret(t *testing.T) {
	h := newEngineHarness(t, stackOC(nil), engineSecret())
	h.mustRun()
	for comp, keys := range map[string][]string{
		"workers":       {"OPENAI_API_KEY", "GROQ_API_KEY", "ANTHROPIC_API_KEY", "SENTRY_DSN"},
		"index-builder": {"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID"},
	} {
		dep := &appsv1.Deployment{}
		if !h.get("v1-"+comp, dep) {
			t.Fatalf("Deployment v1-%s not found", comp)
		}
		c := containerIn(t, dep.Spec.Template.Spec, comp)
		sec := h.secret("v1-" + comp + "-secret")
		for _, k := range keys {
			if secretRef(c.Env, k) != nil {
				t.Errorf("%s: %s has an explicit reference without one in the spec", comp, k)
			}
			if _, ok := sec.Data[k]; !ok {
				t.Errorf("%s: %s is not seeded", comp, k)
			}
		}
	}
}
