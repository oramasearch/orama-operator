/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// llmSecretHashKey is the pod-template annotation that holds the digest of every Secret the engine
// takes LLM keys from, so a rotated key rolls the writer and readers.
const llmSecretHashKey = "orama.cloud/llm-secret-hash"

// defaultLLMProviders is the provider set a CR without spec.llm.providers gets: Prod 1's providers
// and models (orama-deploy oramacore/production/config/, identical for writer and reader).
var defaultLLMProviders = []oramav1alpha1.LLMProvider{
	{Name: "openai", Models: []string{"gpt-5.2"}},
	{Name: "google", Models: []string{"gemini-2.0-flash-001"}},
	{Name: "groq", Models: []string{"openai/gpt-oss-120b"}},
	{Name: "anthropic", Models: []string{"claude-sonnet-4-5"}},
}

// llmBaseURLs are OramaCore's built-in OpenAI-compatible base URLs (v1.2.39, src/ai/llms.rs,
// LLMService::try_new), keyed by the remote providers the operator renders. remote_llms entries
// leave url out so OramaCore uses these; ai_server.llm.host has no built-in value, so the default
// provider's URL is rendered there.
var llmBaseURLs = map[string]string{
	"groq":      "https://api.groq.com/openai/v1",
	"openai":    "https://api.openai.com/v1",
	"google":    "https://generativelanguage.googleapis.com/v1beta/openai",
	"anthropic": "https://api.anthropic.com/v1",
}

// llmProviders is the effective provider list: spec.llm.providers, or defaultLLMProviders when it is
// empty. Rendering, validation, key resolution and the Secret watch all read it, never the spec.
func llmProviders(oc *oramav1alpha1.OramaCloudV1) []oramav1alpha1.LLMProvider {
	if len(oc.Spec.LLM.Providers) == 0 {
		return defaultLLMProviders
	}
	return oc.Spec.LLM.Providers
}

// llmKeyEnv is the variable that holds a provider's key in the render-config init container, and
// the key of <cr>-engine-secret it comes from by default: GROQ_API_KEY, GOOGLE_API_KEY, ...
func llmKeyEnv(provider string) string { return strings.ToUpper(provider) + "_API_KEY" }

// keyPlaceholder is the ConfigMap stand-in for the value of env, substituted at pod start.
func keyPlaceholder(env string) string { return "__" + env + "__" }

// validateLLM refuses an LLM specification the engine can't run with, before any engine object is
// written. Messages name the field, never a value from a Secret.
func validateLLM(oc *oramav1alpha1.OramaCloudV1) error {
	provs := llmProviders(oc)
	names := make([]string, 0, len(provs))
	for i, p := range provs {
		field := fmt.Sprintf("spec.llm.providers[%d]", i)
		switch {
		case p.Name == "vllm":
			return fmt.Errorf("%s: vllm (in-cluster vLLM) is not supported yet; remove it", field)
		case llmBaseURLs[p.Name] == "":
			return fmt.Errorf("%s: unsupported provider %q (supported: anthropic, google, groq, openai)", field, p.Name)
		case slices.Contains(names, p.Name):
			return fmt.Errorf("%s: provider %s is listed twice", field, p.Name)
		case len(p.Models) == 0 || p.Models[0] == "":
			return fmt.Errorf("%s (%s): the first model is the provider's default model and must be set", field, p.Name)
		case p.APIKeySecretRef != nil && p.APIKeySecretRef.Name == "":
			return fmt.Errorf("%s (%s): apiKeySecretRef.name is empty", field, p.Name)
		}
		names = append(names, p.Name)
	}
	if d := llmDefault(oc); !slices.Contains(names, d) {
		return fmt.Errorf("spec.llm.default %q is not one of the providers (%s)", d, strings.Join(names, ", "))
	}
	if sel := oc.Spec.LLM.AutomaticEmbeddingsSelector; sel != nil {
		if !slices.Contains(names, sel.Provider) {
			return fmt.Errorf("spec.llm.automaticEmbeddingsSelector.provider %q is not one of the providers (%s)", sel.Provider, strings.Join(names, ", "))
		}
		if sel.Model == "" {
			return fmt.Errorf("spec.llm.automaticEmbeddingsSelector.model is empty")
		}
	}
	return nil
}

// llmYAML is ai_server.llm and ai_server.remote_llms, indented under ai_server. With local: false
// OramaCore sends every request that names no provider to llm.host as an OpenAI-compatible API, so
// llm is the default provider's base URL, first model and key. Each provider then gets a remote_llms
// entry for requests that name it. Keys are single-quoted placeholders: renderConfigScript fills
// them, and a single-quoted scalar keeps every character but a line break literal.
func llmYAML(oc *oramav1alpha1.OramaCloudV1) string {
	var b strings.Builder
	provs := llmProviders(oc)
	if i := slices.IndexFunc(provs, func(p oramav1alpha1.LLMProvider) bool { return p.Name == llmDefault(oc) }); i >= 0 {
		p := provs[i]
		fmt.Fprintf(&b, "  llm:\n    local: false\n    host: %q\n    model: %q\n    api_key: '%s'\n",
			llmBaseURLs[p.Name], p.Models[0], keyPlaceholder(llmKeyEnv(p.Name)))
	}
	b.WriteString("  remote_llms:\n")
	for _, p := range provs {
		fmt.Fprintf(&b, "    - provider: %s\n      api_key: '%s'\n      default_model: %q\n",
			p.Name, keyPlaceholder(llmKeyEnv(p.Name)), p.Models[0])
	}
	return b.String()
}

// llmSelectorYAML is ai_server.embeddings.automatic_embeddings_selector, indented under embeddings,
// or "" when spec.llm.automaticEmbeddingsSelector is unset.
func llmSelectorYAML(oc *oramav1alpha1.OramaCloudV1) string {
	sel := oc.Spec.LLM.AutomaticEmbeddingsSelector
	if sel == nil {
		return ""
	}
	return fmt.Sprintf("    automatic_embeddings_selector:\n      provider: %s\n      model: %q\n", sel.Provider, sel.Model)
}

// llmKeySource is where one provider's key comes from: the Secret name and key, in the CR's
// namespace, and the render-config variable it fills.
type llmKeySource struct {
	env, secret, key string
	custom           bool
}

// llmKeySources resolves every effective provider's key: <PROVIDER>_API_KEY in its apiKeySecretRef
// Secret, or in <cr>-engine-secret.
func llmKeySources(oc *oramav1alpha1.OramaCloudV1) []llmKeySource {
	var srcs []llmKeySource
	for _, p := range llmProviders(oc) {
		env := llmKeyEnv(p.Name)
		ref := p.APIKeySecretRef
		if ref == nil {
			srcs = append(srcs, llmKeySource{env: env, secret: componentSecretName(oc, "engine"), key: env})
			continue
		}
		srcs = append(srcs, llmKeySource{env: env, secret: ref.Name, key: env, custom: true})
	}
	return srcs
}

// llmKeySecretEnv is the render-config env for providers whose key comes from apiKeySecretRef. An
// explicit entry overrides the same-named variable the engine Secret's envFrom supplies.
func llmKeySecretEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, s := range llmKeySources(oc) {
		if !s.custom {
			continue
		}
		env = append(env, corev1.EnvVar{Name: s.env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s.secret}, Key: s.key}}})
	}
	return env
}

// renderConfigScript is the engine render-config init container's shell script: one sed expression
// per key placeholder the side's template holds (renderEnvs), reading each value from its quoted
// environment variable. The script never holds a key value, and nothing in it evaluates or logs one.
func renderConfigScript(oc *oramav1alpha1.OramaCloudV1, side engineSide) string {
	return renderEscFunc + "sed" + renderSubstitutions(renderEnvs(oc, side)) + " /tmpl/" + side.configKey + " > /etc/oramacore/config.yaml"
}

// renderEnvs is the variables an engine side's render-config fills: the master key where the
// side's template holds writer_side.master_api_key, the broker admin user and password, the
// analytics key where it holds reader_side.analytics, and each provider's key.
func renderEnvs(oc *oramav1alpha1.OramaCloudV1, side engineSide) []string {
	var envs []string
	if side.readsMasterAPIKey() {
		envs = append(envs, masterAPIKeyEnvName)
	}
	envs = append(envs, brokerUserEnv, brokerPasswordEnv)
	if side.readsAnalyticsKey() {
		envs = append(envs, analyticsKeyEnvName)
	}
	for _, p := range llmProviders(oc) {
		envs = append(envs, llmKeyEnv(p.Name))
	}
	return envs
}

// renderEscFunc defines the shell function esc, which makes a value safe as a sed replacement
// (backslash, ampersand and the # delimiter) and inside a single-quoted YAML scalar (it doubles a
// single quote). Every render-config script starts with it.
const renderEscFunc = `esc() { printf '%s' "$1" | sed -e "s/'/''/g" -e 's/[\\&#]/\\&/g'; }; `

// renderSubstitutions is the sed arguments that replace each __VAR__ placeholder with the escaped
// value of VAR.
func renderSubstitutions(envs []string) string {
	var b strings.Builder
	for _, env := range envs {
		fmt.Fprintf(&b, ` -e "s#%s#$(esc "${%s}")#g"`, keyPlaceholder(env), env)
	}
	return b.String()
}

// engineLLMSecretHash reads every Secret the engine takes LLM keys from, checks that each
// provider's key is there, nonempty and on one line, and returns secretDataHash of those Secrets.
// It runs before any engine object is written, so a missing Secret or key leaves the running
// templates as they are.
func (r *OramaCloudV1Reconciler) engineLLMSecretHash(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		// Uncached: the engine Secret may have been created by this reconcile.
		reader = r.APIReader
	}
	read := map[string]*corev1.Secret{}
	engineSecret := componentSecretName(oc, "engine")
	for _, name := range append([]string{engineSecret}, llmCustomSecretNames(oc)...) {
		if _, ok := read[name]; ok {
			continue
		}
		sec := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("LLM key Secret %s/%s does not exist", oc.Namespace, name)
			}
			return "", fmt.Errorf("read LLM key Secret %s/%s: %w", oc.Namespace, name, err)
		}
		read[name] = sec
	}
	for _, s := range llmKeySources(oc) {
		v, ok := read[s.secret].Data[s.key]
		switch {
		case !ok:
			return "", fmt.Errorf("LLM key Secret %s/%s has no key %s (for %s)", oc.Namespace, s.secret, s.key, s.env)
		case len(v) == 0:
			return "", fmt.Errorf("LLM key Secret %s/%s key %s (for %s) is empty", oc.Namespace, s.secret, s.key, s.env)
		case strings.ContainsAny(string(v), "\r\n"):
			return "", fmt.Errorf("LLM key Secret %s/%s key %s (for %s) contains a line break", oc.Namespace, s.secret, s.key, s.env)
		}
	}
	secrets := make([]*corev1.Secret, 0, len(read))
	for _, sec := range read {
		secrets = append(secrets, sec)
	}
	return secretDataHash(secrets...), nil
}

// llmCustomSecretNames is the distinct Secrets the effective providers' apiKeySecretRefs name.
func llmCustomSecretNames(oc *oramav1alpha1.OramaCloudV1) []string {
	var names []string
	for _, s := range llmKeySources(oc) {
		if s.custom && !slices.Contains(names, s.secret) {
			names = append(names, s.secret)
		}
	}
	return names
}

// secretDataHash is the hex SHA-256 of the data of secrets: namespace, name and every key and value,
// as JSON records sorted by namespace, name and key, each Secret once. Metadata and resource versions
// are left out, so only a change to a value moves it. A pod template that carries it as an
// annotation rolls when one of the Secrets it reads changes.
func secretDataHash(secrets ...*corev1.Secret) string {
	type entry struct {
		Key   string `json:"k"`
		Value []byte `json:"v"`
	}
	type record struct {
		Namespace string  `json:"ns"`
		Name      string  `json:"name"`
		Data      []entry `json:"data"`
	}
	seen := map[types.NamespacedName]bool{}
	var recs []record
	for _, s := range secrets {
		id := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		if seen[id] {
			continue
		}
		seen[id] = true
		rec := record{Namespace: s.Namespace, Name: s.Name, Data: []entry{}}
		for k, v := range s.Data {
			rec.Data = append(rec.Data, entry{Key: k, Value: v})
		}
		sort.Slice(rec.Data, func(i, j int) bool { return rec.Data[i].Key < rec.Data[j].Key })
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Namespace != recs[j].Namespace {
			return recs[i].Namespace < recs[j].Namespace
		}
		return recs[i].Name < recs[j].Name
	})
	b, _ := json.Marshal(recs) // strings and byte slices only: Marshal cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// requestsForSecret maps a Secret event to the OramaCloudV1 resources in its namespace that read
// it: an enabled engine's LLM key Secrets (<cr>-engine-secret, or an apiKeySecretRef), the Secret
// spec.masterAPIKeySecretRef names, <cr>-broker-admin, <cr>-cache-auth and <cr>-analytics-api-key,
// and the control DB's <cr>-controldb-app while analytics-processor runs. Supplied and copied
// Secrets carry no owner reference, so Owns alone would miss them; a Secret several CRs share
// enqueues each of them.
func (r *OramaCloudV1Reconciler) requestsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	var list oramav1alpha1.OramaCloudV1List
	if err := r.List(ctx, &list, client.InNamespace(object.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list OramaCloudV1 for a referenced Secret", "secret", object.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if slices.Contains(secretsReadBy(&list.Items[i]), object.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
		}
	}
	return reqs
}

// secretsReadBy is the Secrets a CR's workloads read that requestsForSecret maps back to it.
func secretsReadBy(oc *oramav1alpha1.OramaCloudV1) []string {
	names := []string{brokerAdminSecretName(oc), cacheAuthSecretName(oc), analyticsKeySecretName(oc)}
	if oc.Spec.Components.AnalyticsProcessor.Enabled {
		names = append(names, controlDBAppSecret(oc))
	}
	if name, _ := masterAPIKeyRef(oc); name != "" {
		names = append(names, name)
	}
	if oc.Spec.Components.Engine.Enabled {
		names = append(names, componentSecretName(oc, "engine"))
		names = append(names, llmCustomSecretNames(oc)...)
	}
	return names
}
