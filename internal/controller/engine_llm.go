/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// defaultLLMProviders is the provider set a CR without spec.llm.providers gets: Groq with the model
// the operator always rendered, plus OpenAI and Anthropic with Prod 1's models.
var defaultLLMProviders = []oramav1alpha1.LLMProvider{
	{Name: "groq", Models: []string{"llama-3.3-70b-versatile"}},
	{Name: "openai", Models: []string{"gpt-5.2"}},
	{Name: "anthropic", Models: []string{"claude-sonnet-4-5"}},
}

// llmBaseURLs are OramaCore's built-in OpenAI-compatible API base URLs (oramacore src/ai/llms.rs,
// LLMService::try_new). remote_llms entries without a url use them; ai_server.llm.host has no
// built-in default, so the default provider's URL is rendered there.
var llmBaseURLs = map[string]string{
	"groq":      "https://api.groq.com/openai/v1",
	"openai":    "https://api.openai.com/v1",
	"google":    "https://generativelanguage.googleapis.com/v1beta/openai",
	"anthropic": "https://api.anthropic.com/v1",
}

// llmProviders is spec.llm.providers, or defaultLLMProviders when it is empty.
func llmProviders(oc *oramav1alpha1.OramaCloudV1) []oramav1alpha1.LLMProvider {
	if len(oc.Spec.LLM.Providers) == 0 {
		return defaultLLMProviders
	}
	return oc.Spec.LLM.Providers
}

// llmKeyEnv is the variable that holds a provider's API key in the render-config init container,
// and the engine Secret key it comes from by default: GROQ_API_KEY, OPENAI_API_KEY, ...
func llmKeyEnv(provider string) string { return strings.ToUpper(provider) + "_API_KEY" }

// keyPlaceholder is the ConfigMap stand-in for the secret value of env, substituted at pod start.
func keyPlaceholder(env string) string { return "__" + env + "__" }

// validateLLM refuses an LLM spec OramaCore can't run with, before any config is rendered: a
// provider listed twice or with no models, a default or selector provider that is not listed, and
// vllm (in-cluster vLLM is not rendered yet, and OramaCore has no remote provider of that name).
func validateLLM(oc *oramav1alpha1.OramaCloudV1) error {
	provs := llmProviders(oc)
	names := make([]string, 0, len(provs))
	for _, p := range provs {
		switch {
		case p.Name == "vllm":
			return fmt.Errorf("spec.llm.providers: vllm (in-cluster vLLM) is not supported yet; remove it")
		case slices.Contains(names, p.Name):
			return fmt.Errorf("spec.llm.providers lists %s twice", p.Name)
		case len(p.Models) == 0 || p.Models[0] == "":
			return fmt.Errorf("spec.llm.providers: %s lists no models; its first model is its default model", p.Name)
		}
		names = append(names, p.Name)
	}
	if d := llmDefault(oc); !slices.Contains(names, d) {
		return fmt.Errorf("spec.llm.default %q is not one of spec.llm.providers (%s)", d, strings.Join(names, ", "))
	}
	if sel := oc.Spec.LLM.AutomaticEmbeddingsSelector; sel != nil && !slices.Contains(names, sel.Provider) {
		return fmt.Errorf("spec.llm.automaticEmbeddingsSelector.provider %q is not one of spec.llm.providers (%s)", sel.Provider, strings.Join(names, ", "))
	}
	return nil
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

// llmYAML is ai_server.llm and ai_server.remote_llms. With local: false OramaCore sends every
// request that names no provider to llm.host as an OpenAI-compatible API, so llm is the default
// provider's base URL, first model and key. Each provider then gets a remote_llms entry for
// requests that name it. Keys are single-quoted placeholders: renderConfigInit substitutes them, and
// a single-quoted YAML scalar keeps any character but a newline literal (esc doubles a quote).
func llmYAML(oc *oramav1alpha1.OramaCloudV1) string {
	var b strings.Builder
	provs := llmProviders(oc)
	for _, p := range provs {
		if p.Name != llmDefault(oc) {
			continue
		}
		host := p.URL
		if host == "" {
			host = llmBaseURLs[p.Name]
		}
		fmt.Fprintf(&b, "  llm:\n    local: false\n    host: %q\n    model: %q\n    api_key: '%s'\n",
			host, p.Models[0], keyPlaceholder(llmKeyEnv(p.Name)))
	}
	b.WriteString("  remote_llms:\n")
	for _, p := range provs {
		fmt.Fprintf(&b, "    - provider: %s\n      api_key: '%s'\n      default_model: %q\n",
			p.Name, keyPlaceholder(llmKeyEnv(p.Name)), p.Models[0])
		if p.URL != "" {
			fmt.Fprintf(&b, "      url: %q\n", p.URL)
		}
	}
	return b.String()
}

// llmKeySecretEnv is the render-config env for providers whose key comes from apiKeySecretRef. It
// overrides the same-named variable the engine Secret's envFrom supplies.
func llmKeySecretEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, p := range llmProviders(oc) {
		ref := p.APIKeySecretRef
		if ref == nil {
			continue
		}
		key := ref.Key
		if key == "" {
			key = llmKeyEnv(p.Name)
		}
		env = append(env, secretKeyEnv(llmKeyEnv(p.Name), ref.Name, key))
	}
	return env
}

// engineKeyEnvs are the variables whose values replace their placeholders in the engine config:
// the master key, one key per LLM provider, and the Sentry DSN when spec.components.engine.sentry
// is set.
func engineKeyEnvs(oc *oramav1alpha1.OramaCloudV1) []string {
	envs := []string{"MASTER_API_KEY"}
	for _, p := range llmProviders(oc) {
		envs = append(envs, llmKeyEnv(p.Name))
	}
	if oc.Spec.Components.Engine.Sentry != nil {
		envs = append(envs, sentryDSNEnv)
	}
	return envs
}

// renderConfigScript is the render-config init container's shell command: one sed expression per
// secret placeholder (and __POD_NAME__ for side-only configs, and the Sentry expressions of
// sentryRenderExprs). esc makes a value safe both as a sed
// replacement (backslash, ampersand and the # delimiter) and inside the template's single-quoted YAML
// (it doubles a single quote).
func renderConfigScript(oc *oramav1alpha1.OramaCloudV1, s engineSide) string {
	var b strings.Builder
	b.WriteString(`esc() { printf '%s' "$1" | sed -e "s/'/''/g" -e 's/[\\&#]/\\&/g'; }; sed`)
	for _, env := range engineKeyEnvs(oc) {
		fmt.Fprintf(&b, ` -e "s#%s#$(esc "${%s}")#g"`, keyPlaceholder(env), env)
	}
	if s.configKey != combinedConfigKey {
		b.WriteString(` -e "s#` + podNamePlaceholder + `#${POD_NAME}#g"`)
	}
	b.WriteString(sentryRenderExprs(oc, s))
	b.WriteString(" /tmpl/" + s.configKey + " > /etc/oramacore/config.yaml")
	return b.String()
}
