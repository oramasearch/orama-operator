/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// prod1LLM is Prod 1's provider set (orama-deploy oramacore/production/config/*.config.yaml).
func prod1LLM(o *oramav1alpha1.OramaCloudV1) {
	o.Spec.LLM = oramav1alpha1.LLMSpec{
		Default: "groq",
		Providers: []oramav1alpha1.LLMProvider{
			{Name: "openai", Models: []string{"gpt-5.2"}},
			{Name: "google", Models: []string{"gemini-2.0-flash-001"}},
			{Name: "groq", Models: []string{"openai/gpt-oss-120b"}},
			{Name: "anthropic", Models: []string{"claude-sonnet-4-5"}},
		},
		AutomaticEmbeddingsSelector: &oramav1alpha1.LLMModelRef{Provider: "openai", Model: "gpt-5.2"},
	}
}

// aiServerOf parses a config and returns its ai_server section.
func aiServerOf(t *testing.T, cfg string) map[string]any {
	t.Helper()
	return parseEngineConfig(t, "engine config", cfg)["ai_server"].(map[string]any)
}

func TestLLMConfigMatchesProd1(t *testing.T) {
	for _, side := range []string{"writer", "reader"} {
		for _, oc := range []*oramav1alpha1.OramaCloudV1{newOCV1(prod1LLM), sideImagesOC(prod1LLM)} {
			ai := aiServerOf(t, engineSideFor(oc, side).template)
			if got, want := ai["llm"], map[string]any{
				"local": false, "host": "https://api.groq.com/openai/v1", "model": "openai/gpt-oss-120b", "api_key": "__GROQ_API_KEY__",
			}; !reflect.DeepEqual(got, want) {
				t.Errorf("%s llm = %v, want %v", side, got, want)
			}
			want := []any{
				map[string]any{"provider": "openai", "api_key": "__OPENAI_API_KEY__", "default_model": "gpt-5.2"},
				map[string]any{"provider": "google", "api_key": "__GOOGLE_API_KEY__", "default_model": "gemini-2.0-flash-001"},
				map[string]any{"provider": "groq", "api_key": "__GROQ_API_KEY__", "default_model": "openai/gpt-oss-120b"},
				map[string]any{"provider": "anthropic", "api_key": "__ANTHROPIC_API_KEY__", "default_model": "claude-sonnet-4-5"},
			}
			if got := ai["remote_llms"]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s remote_llms = %v, want %v", side, got, want)
			}
			sel := ai["embeddings"].(map[string]any)["automatic_embeddings_selector"]
			if want := map[string]any{"provider": "openai", "model": "gpt-5.2"}; !reflect.DeepEqual(sel, want) {
				t.Errorf("%s automatic_embeddings_selector = %v, want %v", side, sel, want)
			}
		}
	}
}

func TestLLMConfigDefaultsWithoutSpec(t *testing.T) {
	oc := newOCV1(nil)
	if err := validateLLM(oc); err != nil {
		t.Fatalf("a CR without spec.llm must be valid: %v", err)
	}
	ai := aiServerOf(t, oramacoreConfigYAML(oc))
	llm := ai["llm"].(map[string]any)
	if llm["host"] != "https://api.groq.com/openai/v1" || llm["model"] != "llama-3.3-70b-versatile" || llm["api_key"] != "__GROQ_API_KEY__" {
		t.Errorf("default llm = %v, want Groq llama-3.3-70b-versatile", llm)
	}
	var got []string
	for _, e := range ai["remote_llms"].([]any) {
		m := e.(map[string]any)
		got = append(got, m["provider"].(string)+"="+m["default_model"].(string))
	}
	if want := []string{"groq=llama-3.3-70b-versatile", "openai=gpt-5.2", "anthropic=claude-sonnet-4-5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("default remote_llms = %v, want %v", got, want)
	}
	if _, ok := ai["embeddings"].(map[string]any)["automatic_embeddings_selector"]; ok {
		t.Error("no selector was set, but one is rendered")
	}
}

func TestLLMURLOverride(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.LLM.Providers = []oramav1alpha1.LLMProvider{{Name: "groq", Models: []string{"m"}, URL: "https://llm.example.com/v1"}}
	})
	ai := aiServerOf(t, oramacoreConfigYAML(oc))
	if host := ai["llm"].(map[string]any)["host"]; host != "https://llm.example.com/v1" {
		t.Errorf("llm.host = %v, want the url override", host)
	}
	if url := ai["remote_llms"].([]any)[0].(map[string]any)["url"]; url != "https://llm.example.com/v1" {
		t.Errorf("remote_llms url = %v, want the override", url)
	}
}

// TestRenderConfigSubstitutesLLMKeys runs the init container's real command: every provider
// placeholder is replaced, the apiKeySecretRef key arrives through its own env entry, and values
// holding sed and YAML specials come out unchanged.
func TestRenderConfigSubstitutesLLMKeys(t *testing.T) {
	oc := sideImagesOC(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "openai-key"}
		o.Spec.LLM.Providers[1].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "google-key", Key: "token"}
	})
	side := engineSideFor(oc, "reader")
	render := renderConfigInit(oc, side)

	refs := map[string]string{}
	for _, e := range render.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			refs[e.Name] = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		}
	}
	if want := map[string]string{"OPENAI_API_KEY": "openai-key/OPENAI_API_KEY", "GOOGLE_API_KEY": "google-key/token"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("render-config Secret env = %v, want %v", refs, want)
	}

	keys := map[string]string{
		"MASTER_API_KEY":    `m&a#s\ter`,
		"OPENAI_API_KEY":    `sk-&#\'x`,
		"GOOGLE_API_KEY":    `g\\o#o&g`,
		"GROQ_API_KEY":      `gsk_&&##`,
		"ANTHROPIC_API_KEY": `an'th\ro&pic#`,
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-"+side.configKey), []byte(side.template), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/tmpl/", dir+"/in-", "/etc/oramacore/", dir+"/out-").Replace(render.Command[2])
	cmd := exec.Command(render.Command[0], render.Command[1], script)
	cmd.Env = append(os.Environ(), "POD_NAME=v1-reader-0")
	for k, v := range keys {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render command: %v\n%s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "out-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "_API_KEY__") {
		t.Fatalf("rendered config still holds a placeholder:\n%s", rendered)
	}
	cfg := parseEngineConfig(t, "rendered reader config", string(rendered))
	ai := cfg["ai_server"].(map[string]any)
	if got := ai["llm"].(map[string]any)["api_key"]; got != keys["GROQ_API_KEY"] {
		t.Errorf("llm.api_key = %q, want the Groq key", got)
	}
	for _, e := range ai["remote_llms"].([]any) {
		m := e.(map[string]any)
		if want := keys[llmKeyEnv(m["provider"].(string))]; m["api_key"] != want {
			t.Errorf("%s api_key = %q, want %q", m["provider"], m["api_key"], want)
		}
	}
}

func TestLLMKeysStayOutOfConfigMaps(t *testing.T) {
	secretValues := map[string]string{
		"MASTER_API_KEY": "secret-master-1", "GROQ_API_KEY": "secret-groq-2", "OPENAI_API_KEY": "secret-openai-3",
		"ANTHROPIC_API_KEY": "secret-anthropic-4", "GOOGLE_API_KEY": "secret-google-5", "HF_TOKEN": "secret-hf-6",
	}
	engine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-engine-secret", Namespace: "orama-system"}, Data: map[string][]byte{}}
	for k, v := range secretValues {
		engine.Data[k] = []byte(v)
	}
	ref := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "openai-key", Namespace: "orama-system"},
		Data: map[string][]byte{"OPENAI_API_KEY": []byte("secret-openai-ref-7")}}
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: ref.Name}
	})
	cl, _ := reconcileOCV1(t, oc, engine, ref)

	var cms corev1.ConfigMapList
	if err := cl.List(context.Background(), &cms, client.InNamespace("orama-system")); err != nil {
		t.Fatal(err)
	}
	if len(cms.Items) == 0 {
		t.Fatal("no ConfigMaps rendered")
	}
	for _, cm := range cms.Items {
		for key, data := range cm.Data {
			for _, v := range append(slices.Collect(maps.Values(secretValues)), "secret-openai-ref-7") {
				if strings.Contains(data, v) {
					t.Errorf("ConfigMap %s key %s holds a secret value", cm.Name, key)
				}
			}
		}
	}
}

func TestLLMModelChangeRollsEnginePods(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(prod1LLM))
	hash := func(name string) string {
		return getObj(t, cl, name, &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"]
	}
	writer, reader := hash("v1-writer"), hash("v1-reader")
	if writer == "" || reader == "" {
		t.Fatalf("engine pods carry no config hash: writer %q, reader %q", writer, reader)
	}
	reconcileAgain(t, cl, r, nil)
	if hash("v1-writer") != writer {
		t.Error("an unchanged spec changed the config hash")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers[1].Models = []string{"gemini-2.5-flash"} })
	if hash("v1-writer") == writer || hash("v1-reader") == reader {
		t.Error("changing a provider's model did not roll the engine pods")
	}
}

func TestLLMSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*oramav1alpha1.LLMSpec)
		want string
	}{
		{"unknown default", func(l *oramav1alpha1.LLMSpec) { l.Default = "anthropic"; l.Providers = l.Providers[:1] }, `spec.llm.default "anthropic" is not one of spec.llm.providers (openai)`},
		{"default left out of the default set", func(l *oramav1alpha1.LLMSpec) { l.Providers, l.Default = nil, "google" }, `spec.llm.default "google" is not one of spec.llm.providers (groq, openai, anthropic)`},
		{"no models", func(l *oramav1alpha1.LLMSpec) { l.Providers[1].Models = nil }, "google lists no models"},
		{"listed twice", func(l *oramav1alpha1.LLMSpec) { l.Providers[3].Name = "groq" }, "lists groq twice"},
		{"selector not listed", func(l *oramav1alpha1.LLMSpec) { l.Providers = l.Providers[1:] }, `automaticEmbeddingsSelector.provider "openai" is not one of`},
		{"vllm", func(l *oramav1alpha1.LLMSpec) {
			l.Providers = append(l.Providers, oramav1alpha1.LLMProvider{Name: "vllm", Models: []string{"m"}})
		}, "vllm (in-cluster vLLM) is not supported yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) { prod1LLM(o); tc.mut(&o.Spec.LLM) })
			if err := validateLLM(oc); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validateLLM = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := validateLLM(newOCV1(prod1LLM)); err != nil {
		t.Errorf("the Prod 1 provider set must be valid: %v", err)
	}
}

func TestUnknownDefaultProviderFailsReconcile(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		o.Spec.LLM.Default = "vertex"
	})
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(oc).WithStatusSubresource(oc).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err == nil {
		t.Fatal("reconcile with an unknown default provider succeeded")
	}
	got := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	want := `engine: spec.llm.default "vertex" is not one of spec.llm.providers (openai, google, groq, anthropic)`
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != want {
		t.Fatalf("Ready = %+v, want False with %q", cond, want)
	}
	if !absent(t, cl, "v1-engine-config", &corev1.ConfigMap{}) {
		t.Error("an invalid LLM spec still rendered the engine config")
	}
}
