/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// prod1LLM is Prod 1's provider block (orama-deploy oramacore/production/config/).
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

// prod1LLMFragment is what prod1LLM renders under ai_server, on both sides.
const prod1LLMFragment = `  llm:
    local: false
    host: "https://api.groq.com/openai/v1"
    model: "openai/gpt-oss-120b"
    api_key: '__GROQ_API_KEY__'
  remote_llms:
    - provider: openai
      api_key: '__OPENAI_API_KEY__'
      default_model: "gpt-5.2"
    - provider: google
      api_key: '__GOOGLE_API_KEY__'
      default_model: "gemini-2.0-flash-001"
    - provider: groq
      api_key: '__GROQ_API_KEY__'
      default_model: "openai/gpt-oss-120b"
    - provider: anthropic
      api_key: '__ANTHROPIC_API_KEY__'
      default_model: "claude-sonnet-4-5"
`

// prod1AIServerLLM is prod1LLM's ai_server LLM settings as parsed YAML.
var prod1AIServerLLM = map[string]any{
	"llm": map[string]any{"local": false, "host": "https://api.groq.com/openai/v1", "model": "openai/gpt-oss-120b", "api_key": "__GROQ_API_KEY__"},
	"remote_llms": []any{
		map[string]any{"provider": "openai", "api_key": "__OPENAI_API_KEY__", "default_model": "gpt-5.2"},
		map[string]any{"provider": "google", "api_key": "__GOOGLE_API_KEY__", "default_model": "gemini-2.0-flash-001"},
		map[string]any{"provider": "groq", "api_key": "__GROQ_API_KEY__", "default_model": "openai/gpt-oss-120b"},
		map[string]any{"provider": "anthropic", "api_key": "__ANTHROPIC_API_KEY__", "default_model": "claude-sonnet-4-5"},
	},
	"selector": map[string]any{"provider": "openai", "model": "gpt-5.2"},
}

// engineKeys is an engine Secret with distinctive values, so a test can look for them.
var engineKeys = map[string]string{
	"MASTER_API_KEY": "master-value-0001", "GROQ_API_KEY": "groq-value-0002", "OPENAI_API_KEY": "openai-value-0003",
	"ANTHROPIC_API_KEY": "anthropic-value-0004", "GOOGLE_API_KEY": "google-value-0005", "HF_TOKEN": "hf-value-0006",
}

func secretOf(name string, data map[string]string) *corev1.Secret {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system"}, Data: map[string][]byte{}}
	for k, v := range data {
		sec.Data[k] = []byte(v)
	}
	return sec
}

func engineSecret() *corev1.Secret { return secretOf("v1-engine-secret", engineKeys) }

// aiServerLLM picks the LLM settings out of a parsed config's ai_server.
func aiServerLLM(t *testing.T, doc string) map[string]any {
	t.Helper()
	ai, _ := parseYAML(t, doc)["ai_server"].(map[string]any)
	emb, _ := ai["embeddings"].(map[string]any)
	got := map[string]any{"llm": ai["llm"], "remote_llms": ai["remote_llms"]}
	if sel, ok := emb["automatic_embeddings_selector"]; ok {
		got["selector"] = sel
	}
	return got
}

// run is one full Reconcile of the stored CR named v1.
func (h *engineHarness) run() error {
	_, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1", Namespace: "orama-system"}})
	return err
}

// stored reads the CR back.
func (h *engineHarness) stored() *oramav1alpha1.OramaCloudV1 {
	h.t.Helper()
	oc := &oramav1alpha1.OramaCloudV1{}
	if !h.get("v1", oc) {
		h.t.Fatal("CR v1 not found")
	}
	return oc
}

// updateCR changes the stored CR's spec.
func (h *engineHarness) updateCR(mut func(*oramav1alpha1.OramaCloudV1)) {
	h.t.Helper()
	oc := h.stored()
	mut(oc)
	if err := h.cl.Update(context.Background(), oc); err != nil {
		h.t.Fatal(err)
	}
}

func (h *engineHarness) setSecret(name, key, value string) {
	h.t.Helper()
	sec := &corev1.Secret{}
	if !h.get(name, sec) {
		h.t.Fatalf("Secret %s not found", name)
	}
	sec.Data[key] = []byte(value)
	if err := h.cl.Update(context.Background(), sec); err != nil {
		h.t.Fatal(err)
	}
}

// podTemplates is the writer's and readers' pod templates, keyed by side.
func (h *engineHarness) podTemplates() map[string]corev1.PodTemplateSpec {
	return map[string]corev1.PodTemplateSpec{"writer": h.writer().Spec.Template, "reader": h.reader().Spec.Template}
}

func TestLLMProd1RendersOnBothSides(t *testing.T) {
	for name, sides := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, engineSecret())
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
				prod1LLM(o)
				if sides != nil {
					sides(o)
				}
			})
			h.reconcile(oc)
			cm := h.engineConfig()
			for side, spec := range h.podTemplates() {
				key := renderSource(t, spec.Spec)
				tmpl := cm.Data[key]
				if got := aiServerLLM(t, tmpl); !reflect.DeepEqual(got, prod1AIServerLLM) {
					t.Errorf("%s (%s) ai_server LLM settings = %v, want Prod 1's", side, key, got)
				}
				if !strings.Contains(tmpl, prod1LLMFragment) {
					t.Errorf("%s (%s) lacks the Prod 1 LLM fragment:\n%s", side, key, tmpl)
				}
				if !strings.Contains(tmpl, "    total_threads: 8\n    automatic_embeddings_selector:\n      provider: openai\n      model: \"gpt-5.2\"\n") {
					t.Errorf("%s (%s): the selector is not under ai_server.embeddings", side, key)
				}
			}
			for k, v := range cm.Data {
				for env, val := range engineKeys {
					if strings.Contains(v, val) {
						t.Errorf("ConfigMap key %s holds the value of %s", k, env)
					}
				}
			}
		})
	}
}

// A CR without spec.llm, or with an empty provider list, renders the documented defaults; an
// omitted selector stays omitted, and the spec is not filled in.
func TestLLMDefaults(t *testing.T) {
	want := map[string]any{"llm": prod1AIServerLLM["llm"], "remote_llms": prod1AIServerLLM["remote_llms"]}
	for name, llm := range map[string]oramav1alpha1.LLMSpec{
		"no spec.llm":         {},
		"empty provider list": {Providers: []oramav1alpha1.LLMProvider{}, Default: ""},
	} {
		for mode, sides := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
			t.Run(name+" "+mode, func(t *testing.T) {
				oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
					o.Spec.LLM = llm
					if sides != nil {
						sides(o)
					}
				})
				h := newEngineHarness(t, oc)
				if err := h.run(); err != nil {
					t.Fatal(err)
				}
				cm := h.engineConfig()
				for side, spec := range h.podTemplates() {
					if got := aiServerLLM(t, cm.Data[renderSource(t, spec.Spec)]); !reflect.DeepEqual(got, want) {
						t.Errorf("%s ai_server LLM settings = %v, want the defaults without a selector", side, got)
					}
				}
				shared := &corev1.ConfigMap{}
				h.get("v1-config", shared)
				if shared.Data["LLM_PROVIDERS"] != "openai,google,groq,anthropic" || shared.Data["LLM_DEFAULT"] != "groq" {
					t.Errorf("LLM_PROVIDERS/LLM_DEFAULT = %q/%q", shared.Data["LLM_PROVIDERS"], shared.Data["LLM_DEFAULT"])
				}
				if got := h.stored().Spec.LLM; len(got.Providers) != 0 || got.Default != "" || got.AutomaticEmbeddingsSelector != nil {
					t.Errorf("the reconcile filled in spec.llm: %+v", got)
				}
			})
		}
	}
}

// The engine Secret gains GOOGLE_API_KEY without losing the values already in it.
func TestLLMGoogleKeySeededWithoutReplacingValues(t *testing.T) {
	existing := map[string]string{}
	for k, v := range engineKeys {
		if k != "GOOGLE_API_KEY" {
			existing[k] = v
		}
	}
	h := newEngineHarness(t, secretOf("v1-engine-secret", existing))
	h.reconcile(engineOC(nil))
	sec := &corev1.Secret{}
	h.get("v1-engine-secret", sec)
	if got := string(sec.Data["GOOGLE_API_KEY"]); got != "dev-google-api-key" {
		t.Errorf("GOOGLE_API_KEY = %q, want the development placeholder", got)
	}
	for k, v := range existing {
		if string(sec.Data[k]) != v {
			t.Errorf("%s was replaced", k)
		}
	}
}

// The render script fills every placeholder with its value, whatever characters the value holds,
// and never runs any of it.
func TestLLMRenderScriptSubstitutesSafely(t *testing.T) {
	values := map[string]string{
		"MASTER_API_KEY":    `m&a#s\ter'"$(touch pwned-master)`,
		"OPENAI_API_KEY":    "sk-`touch pwned-openai`;exit 1|&",
		"GOOGLE_API_KEY":    `AIza\n\t$HOME${PATH}\\`,
		"GROQ_API_KEY":      `gsk_'quoted' "double" #hash &amp \1`,
		"ANTHROPIC_API_KEY": `sk-ant-*?[]~!{}<> ''`,
	}
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { prod1LLM(o); withSideImages(o) })
	h := newEngineHarness(t, engineSecret())
	h.reconcile(oc)
	cm := h.engineConfig()
	for side, spec := range h.podTemplates() {
		dir := t.TempDir()
		tmpl, out := filepath.Join(dir, "tmpl"), filepath.Join(dir, "etc")
		for _, d := range []string{tmpl, out} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		key := renderSource(t, spec.Spec)
		if err := os.WriteFile(filepath.Join(tmpl, key), []byte(cm.Data[key]), 0o644); err != nil {
			t.Fatal(err)
		}
		script := renderInit(t, spec.Spec).Command[2]
		script = strings.ReplaceAll(script, "/tmpl/", tmpl+"/")
		script = strings.ReplaceAll(script, "/etc/oramacore/", out+"/")
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}
		for k, v := range values {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s render-config: %v: %s", side, err, b)
		}
		if matches, _ := filepath.Glob(filepath.Join(dir, "pwned-*")); len(matches) > 0 {
			t.Fatalf("%s render-config executed a key value: %v", side, matches)
		}
		b, err := os.ReadFile(filepath.Join(out, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		rendered := parseYAML(t, string(b))
		ai := rendered["ai_server"].(map[string]any)
		if got := ai["llm"].(map[string]any)["api_key"]; got != values["GROQ_API_KEY"] {
			t.Errorf("%s ai_server.llm.api_key = %q, want the default provider's key", side, got)
		}
		for _, e := range ai["remote_llms"].([]any) {
			entry := e.(map[string]any)
			if got, want := entry["api_key"], values[llmKeyEnv(entry["provider"].(string))]; got != want {
				t.Errorf("%s remote_llms %s api_key = %q, want %q", side, entry["provider"], got, want)
			}
		}
		if side == "writer" {
			if got := rendered["writer_side"].(map[string]any)["master_api_key"]; got != values["MASTER_API_KEY"] {
				t.Errorf("writer master_api_key = %q, want %q", got, values["MASTER_API_KEY"])
			}
		}
		if strings.Contains(string(b), "__") {
			t.Errorf("%s rendered config keeps a placeholder", side)
		}
		for _, v := range values {
			if strings.Contains(renderInit(t, spec.Spec).Command[2], v) {
				t.Errorf("%s render script holds a key value", side)
			}
		}
	}
}

// A provider with apiKeySecretRef keeps its placeholder in the template and binds its variable to
// the referenced Secret key; an omitted key means <PROVIDER>_API_KEY.
func TestLLMCustomKeySecretWiring(t *testing.T) {
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys", Key: "openai"}
		o.Spec.LLM.Providers[3].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys"}
	})
	h := newEngineHarness(t, engineSecret(), secretOf("llm-keys", map[string]string{"openai": "custom-openai", "ANTHROPIC_API_KEY": "custom-anthropic"}))
	h.reconcile(oc)
	want := []corev1.EnvVar{
		{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "llm-keys"}, Key: "openai"}}},
		{Name: "ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "llm-keys"}, Key: "ANTHROPIC_API_KEY"}}},
	}
	// The credentials come first: the master key (both sides run the combined config) and the
	// broker admin user and password.
	want = append([]corev1.EnvVar{secretKeyEnv("MASTER_API_KEY", testMasterSecret, "MASTER_API_KEY"),
		secretKeyEnv("BROKER_USERNAME", "v1-broker-admin", "username"), secretKeyEnv("BROKER_PASSWORD", "v1-broker-admin", "password")}, want...)
	cm := h.engineConfig()
	for side, spec := range h.podTemplates() {
		init := renderInit(t, spec.Spec)
		if !reflect.DeepEqual(init.Env, want) {
			t.Errorf("%s render-config env = %+v, want %+v", side, init.Env, want)
		}
		if !reflect.DeepEqual(init.EnvFrom, envFromFor(oc, "engine")) {
			t.Errorf("%s render-config lost the engine envFrom", side)
		}
		if !strings.Contains(cm.Data[renderSource(t, spec.Spec)], "api_key: '__OPENAI_API_KEY__'") {
			t.Errorf("%s template lost the OpenAI placeholder", side)
		}
	}
}

// A changed model rolls both sides through the config hash.
func TestLLMModelChangeRollsBothSides(t *testing.T) {
	for name, sides := range map[string]func(*oramav1alpha1.OramaCloudV1){"combined": nil, "side-only": withSideImages} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, engineSecret())
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
				prod1LLM(o)
				if sides != nil {
					sides(o)
				}
			})
			h.reconcile(oc)
			before := map[string]string{}
			for side, spec := range h.podTemplates() {
				before[side] = spec.Annotations[configHashKey]
			}
			oc.Spec.LLM.Providers[1].Models = []string{"gemini-2.5-flash"}
			h.reconcile(oc)
			cm := h.engineConfig()
			for side, spec := range h.podTemplates() {
				got := spec.Annotations[configHashKey]
				if got == before[side] || got != templateSHA256(cm.Data[renderSource(t, spec.Spec)]) {
					t.Errorf("%s config hash = %q (was %q), want the new template's", side, got, before[side])
				}
			}
		})
	}
}

// A rotated key in the engine Secret or an apiKeySecretRef Secret rolls both sides; an unrelated
// Secret, a metadata-only edit or an unchanged reconcile does not.
func TestLLMSecretChangeRollsBothSides(t *testing.T) {
	h := newEngineHarness(t, engineSecret(), secretOf("llm-keys", map[string]string{"openai": "custom-openai"}),
		secretOf("unrelated", map[string]string{"X": "1"}))
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		withSideImages(o)
		o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys", Key: "openai"}
	})
	h.reconcile(oc)
	snapshot := func() (string, map[string]string) {
		t.Helper()
		versions := map[string]string{"writer": h.writer().ResourceVersion, "reader": h.reader().ResourceVersion}
		w, r := h.writer().Spec.Template.Annotations[llmSecretHashKey], h.reader().Spec.Template.Annotations[llmSecretHashKey]
		if w == "" || w != r {
			t.Fatalf("Secret hash writer %q, reader %q: want one identical digest", w, r)
		}
		return w, versions
	}
	hash, versions := snapshot()

	steady := func(what string) {
		t.Helper()
		h.reconcile(oc)
		got, gotVersions := snapshot()
		if got != hash || !reflect.DeepEqual(gotVersions, versions) {
			t.Errorf("%s changed the engine pod templates", what)
		}
	}
	rolled := func(what string) {
		t.Helper()
		h.reconcile(oc)
		got, gotVersions := snapshot()
		if got == hash {
			t.Errorf("%s did not change the Secret hash", what)
		}
		if gotVersions["writer"] == versions["writer"] || gotVersions["reader"] == versions["reader"] {
			t.Errorf("%s did not update both workloads", what)
		}
		hash, versions = got, gotVersions
	}

	steady("an unchanged reconcile")
	h.setSecret("unrelated", "X", "2")
	steady("an unrelated Secret change")
	sec := &corev1.Secret{}
	h.get("v1-engine-secret", sec)
	sec.Labels = map[string]string{"touched": "yes"}
	sec.Annotations = map[string]string{"note": "metadata only"}
	if err := h.cl.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	steady("a metadata-only edit")

	h.setSecret("v1-engine-secret", "GROQ_API_KEY", "groq-rotated")
	rolled("rotating GROQ_API_KEY in the engine Secret")
	h.setSecret("llm-keys", "openai", "custom-openai-rotated")
	rolled("rotating the apiKeySecretRef key")
	steady("a second unchanged reconcile")
}

func TestSecretDataHash(t *testing.T) {
	a := secretOf("a", map[string]string{"K1": "v1", "K2": "v2"})
	b := secretOf("b", map[string]string{"K": "v"})
	base := secretDataHash(a, b)
	if secretDataHash(b, a) != base || secretDataHash(a, b, a) != base {
		t.Error("the hash depends on order or duplicates")
	}
	moved := secretOf("a", map[string]string{"K1": "v1", "K2": "v2"})
	moved.Labels, moved.ResourceVersion = map[string]string{"x": "y"}, "42"
	if secretDataHash(moved, b) != base {
		t.Error("metadata changed the hash")
	}
	// The encoding is unambiguous: moving bytes between key and value changes the digest.
	if secretDataHash(secretOf("a", map[string]string{"K1v": "1"})) == secretDataHash(secretOf("a", map[string]string{"K1": "v1"})) {
		t.Error("key/value boundary is ambiguous")
	}
	other := secretOf("b", map[string]string{"K": "v"})
	other.Namespace = "elsewhere"
	if secretDataHash(a, other) == base {
		t.Error("the namespace is not part of the hash")
	}
}

func TestRequestsForEngineLLMSecret(t *testing.T) {
	ref := func(name string) *oramav1alpha1.LLMKeySecretRef { return &oramav1alpha1.LLMKeySecretRef{Name: name} }
	cr := func(ns, name string, enabled bool, refs ...*oramav1alpha1.LLMKeySecretRef) *oramav1alpha1.OramaCloudV1 {
		oc := &oramav1alpha1.OramaCloudV1{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		oc.Spec.Components.Engine.Enabled = enabled
		for i, r := range refs {
			oc.Spec.LLM.Providers = append(oc.Spec.LLM.Providers, oramav1alpha1.LLMProvider{
				Name: []string{"openai", "google", "groq"}[i], Models: []string{"m"}, APIKeySecretRef: r})
		}
		return oc
	}
	objs := []client.Object{
		cr("orama-system", "v1", true, ref("shared-keys"), ref("shared-keys"), ref("v1-engine-secret")),
		cr("orama-system", "v2", true, ref("shared-keys")),
		cr("orama-system", "v3", false, ref("shared-keys")),
		cr("orama-system", "v4", true), // defaults: no custom Secret
		cr("other", "v1", true, ref("shared-keys")),
	}
	s := ocv1TestScheme(t)
	h := newEngineHarness(t, objs...)
	r := &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
	eh := handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)

	cases := []struct {
		ns, name string
		want     []string
	}{
		{"orama-system", "shared-keys", []string{"orama-system/v1", "orama-system/v2"}},
		{"orama-system", "v1-engine-secret", []string{"orama-system/v1"}},
		{"orama-system", "v4-engine-secret", []string{"orama-system/v4"}},
		{"orama-system", "v3-engine-secret", nil},
		{"orama-system", "unrelated", nil},
		{"other", "shared-keys", []string{"other/v1"}},
		{"other", "v2-engine-secret", nil},
		{"elsewhere", "shared-keys", nil},
	}
	for _, tc := range cases {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tc.ns, Name: tc.name}}
		for kind, fire := range map[string]func(workqueue.RateLimitingInterface){
			"create": func(q workqueue.RateLimitingInterface) {
				eh.Create(context.Background(), event.CreateEvent{Object: sec}, q)
			},
			"update": func(q workqueue.RateLimitingInterface) {
				eh.Update(context.Background(), event.UpdateEvent{ObjectOld: sec, ObjectNew: sec}, q)
			},
			"delete": func(q workqueue.RateLimitingInterface) {
				eh.Delete(context.Background(), event.DeleteEvent{Object: sec}, q)
			},
		} {
			q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
			fire(q)
			var got []string
			for q.Len() > 0 {
				item, _ := q.Get()
				got = append(got, item.(reconcile.Request).String())
				q.Done(item)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s %s/%s enqueued %v, want %v", kind, tc.ns, tc.name, got, tc.want)
			}
		}
		// The map function itself returns each CR once, even when it reads the Secret twice.
		reqs := r.requestsForSecret(context.Background(), sec)
		if len(reqs) != len(tc.want) {
			t.Errorf("%s/%s mapped to %v, want %d requests", tc.ns, tc.name, reqs, len(tc.want))
		}
	}
}

// Every invalid input fails the actual reconcile with Ready=False before an engine object changes,
// on a fresh stack and on a running one, and names no key value.
func TestLLMInvalidInputsLeaveEngineUntouched(t *testing.T) {
	type tc struct {
		spec   func(*oramav1alpha1.OramaCloudV1)
		secret func(*engineHarness)
		want   string
	}
	cases := map[string]tc{
		"unknown default": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Default = "openai-ish" },
			want: `spec.llm.default "openai-ish" is not one of the providers`},
		"default not listed": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers = o.Spec.LLM.Providers[:2] },
			want: `spec.llm.default "groq" is not one of the providers (openai, google)`},
		"unknown provider": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers[1].Name = "mistral" },
			want: `spec.llm.providers[1]: unsupported provider "mistral"`},
		"duplicate provider": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers[3].Name = "openai" },
			want: "spec.llm.providers[3]: provider openai is listed twice"},
		"no models": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers[0].Models = nil },
			want: "spec.llm.providers[0] (openai): the first model"},
		"empty first model": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.Providers[2].Models = []string{"", "x"} },
			want: "spec.llm.providers[2] (groq): the first model"},
		"vllm": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers = append(o.Spec.LLM.Providers, oramav1alpha1.LLMProvider{Name: "vllm", Models: []string{"q"}})
		}, want: "spec.llm.providers[4]: vllm (in-cluster vLLM) is not supported yet"},
		"selector provider not listed": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers = o.Spec.LLM.Providers[1:]
		}, want: `spec.llm.automaticEmbeddingsSelector.provider "openai" is not one of the providers`},
		"empty selector model": {spec: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.LLM.AutomaticEmbeddingsSelector.Model = "" },
			want: "spec.llm.automaticEmbeddingsSelector.model is empty"},
		"empty Secret reference name": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{}
		}, want: "spec.llm.providers[0] (openai): apiKeySecretRef.name is empty"},
		"missing Secret": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "absent"}
		}, want: "LLM key Secret orama-system/absent does not exist"},
		"absent custom key": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys", Key: "nope"}
		}, want: "LLM key Secret orama-system/llm-keys has no key nope (for OPENAI_API_KEY)"},
		// The engine Secret's provider keys are seeded when absent, so an absent key is a custom
		// Secret's: here the omitted key, GOOGLE_API_KEY.
		"absent omitted key": {spec: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.LLM.Providers[1].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys"}
		}, want: "LLM key Secret orama-system/llm-keys has no key GOOGLE_API_KEY (for GOOGLE_API_KEY)"},
		"empty fallback key": {secret: func(h *engineHarness) { h.setSecret("v1-engine-secret", "GROQ_API_KEY", "") },
			want: "LLM key Secret orama-system/v1-engine-secret key GROQ_API_KEY (for GROQ_API_KEY) is empty"},
		"line break in a fallback key": {secret: func(h *engineHarness) { h.setSecret("v1-engine-secret", "GROQ_API_KEY", "groq-line\nbreak") },
			want: "LLM key Secret orama-system/v1-engine-secret key GROQ_API_KEY (for GROQ_API_KEY) contains a line break"},
		"carriage return in a custom key": {secret: func(h *engineHarness) { h.setSecret("llm-keys", "openai", "custom\rvalue") },
			spec: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys", Key: "openai"}
			}, want: "LLM key Secret orama-system/llm-keys key openai (for OPENAI_API_KEY) contains a line break"},
	}
	secretValues := []string{"custom-openai", "custom\rvalue", "groq-line\nbreak"}
	for _, v := range engineKeys {
		secretValues = append(secretValues, v)
	}
	for name, c := range cases {
		for _, existing := range []bool{false, true} {
			label := name + " on a fresh stack"
			if existing {
				label = name + " on a running stack"
			}
			t.Run(label, func(t *testing.T) {
				h := newEngineHarness(t, engineOC(func(o *oramav1alpha1.OramaCloudV1) { prod1LLM(o); withSideImages(o) }),
					engineSecret(), secretOf("llm-keys", map[string]string{"openai": "custom-openai"}))
				var cm *corev1.ConfigMap
				var sts *appsv1.StatefulSet
				var dep *appsv1.Deployment
				if existing {
					if err := h.run(); err != nil {
						t.Fatal(err)
					}
					cm, sts, dep = h.engineConfig(), h.writer(), h.reader()
				}
				if c.secret != nil {
					c.secret(h)
				}
				if c.spec != nil {
					h.updateCR(c.spec)
				}
				if err := h.run(); err == nil {
					t.Fatal("Reconcile accepted the invalid input")
				}
				ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
				if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileError" ||
					!strings.HasPrefix(ready.Message, "engine: ") || !strings.Contains(ready.Message, c.want) {
					t.Fatalf("Ready = %+v, want False/ReconcileError containing %q", ready, c.want)
				}
				for _, v := range secretValues {
					if strings.Contains(ready.Message, v) {
						t.Errorf("the Ready message holds a key value")
					}
				}
				if !existing {
					for _, n := range []string{"v1-engine-config", "v1-writer", "v1-reader"} {
						for _, obj := range []client.Object{&corev1.ConfigMap{}, &appsv1.StatefulSet{}, &appsv1.Deployment{}} {
							if h.get(n, obj) {
								t.Errorf("%T %s was created by a refused reconcile", obj, n)
							}
						}
					}
					return
				}
				if got := h.engineConfig(); got.ResourceVersion != cm.ResourceVersion {
					t.Error("the refused reconcile changed the engine ConfigMap")
				}
				if got := h.writer(); got.ResourceVersion != sts.ResourceVersion {
					t.Error("the refused reconcile changed the writer StatefulSet")
				}
				if got := h.reader(); got.ResourceVersion != dep.ResourceVersion {
					t.Error("the refused reconcile changed the reader Deployment")
				}
			})
		}
	}
}

// Deleting an apiKeySecretRef Secret fails the reconcile and keeps the templates; recreating it is
// mapped back to the CR, and the next reconcile recovers.
func TestLLMCustomSecretDeletedThenRecreated(t *testing.T) {
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		prod1LLM(o)
		o.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "llm-keys", Key: "openai"}
	})
	keys := secretOf("llm-keys", map[string]string{"openai": "custom-openai"})
	h := newEngineHarness(t, oc, engineSecret(), keys.DeepCopy())
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	sts, dep := h.writer(), h.reader()
	if err := h.cl.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-keys", Namespace: "orama-system"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.run(); err == nil {
		t.Fatal("Reconcile succeeded without the referenced Secret")
	}
	if h.writer().ResourceVersion != sts.ResourceVersion || h.reader().ResourceVersion != dep.ResourceVersion {
		t.Error("a missing Secret changed the engine workloads")
	}
	if ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}

	recreated := keys.DeepCopy()
	recreated.Data["openai"] = []byte("custom-openai-new")
	if err := h.cl.Create(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	if reqs := h.r.requestsForSecret(context.Background(), recreated); len(reqs) != 1 || reqs[0].Name != "v1" {
		t.Fatalf("recreating the Secret mapped to %v, want v1", reqs)
	}
	if err := h.run(); err != nil {
		t.Fatalf("Reconcile after recreating the Secret: %v", err)
	}
	if ready := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready"); ready != nil && ready.Reason == "ReconcileError" {
		t.Errorf("Ready = %+v, want the error cleared", ready)
	}
	for side, spec := range h.podTemplates() {
		if spec.Annotations[llmSecretHashKey] == sts.Spec.Template.Annotations[llmSecretHashKey] {
			t.Errorf("%s did not roll to the recreated key", side)
		}
	}
}

// A disabled engine tears down without any LLM key; a read region rolls only its readers.
func TestLLMDisabledAndReadRegion(t *testing.T) {
	t.Run("disabled engine needs no key", func(t *testing.T) {
		h := newEngineHarness(t, engineSecret())
		oc := engineOC(prod1LLM)
		h.reconcile(oc)
		oc.Spec.Components.Engine.Enabled = false
		oc.Spec.LLM.Providers[0].APIKeySecretRef = &oramav1alpha1.LLMKeySecretRef{Name: "absent"}
		oc.Spec.LLM.Default = "nobody"
		h.reconcile(oc)
		if h.get("v1-writer", &appsv1.StatefulSet{}) || h.get("v1-reader", &appsv1.Deployment{}) || h.get("v1-engine-config", &corev1.ConfigMap{}) {
			t.Error("the disabled engine survived teardown")
		}
	})
	t.Run("read region", func(t *testing.T) {
		h := newEngineHarness(t, engineSecret())
		oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read"; prod1LLM(o) })
		h.reconcile(oc)
		before := h.reader().Spec.Template.Annotations[llmSecretHashKey]
		h.setSecret("v1-engine-secret", "ANTHROPIC_API_KEY", "anthropic-rotated")
		h.reconcile(oc)
		if h.reader().Spec.Template.Annotations[llmSecretHashKey] == before {
			t.Error("the reader did not roll on a rotated key")
		}
		if h.get("v1-writer", &appsv1.StatefulSet{}) {
			t.Error("a read region created a writer")
		}
	})
}
