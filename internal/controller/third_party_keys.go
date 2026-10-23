/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Third-party keys from their own Secret references (operator#571). A third-party key (an LLM
// provider's, Sentry's, Cloudflare's) used to sit in the operator-seeded <cr>-<component>-secret
// beside keys that only have to agree inside the stack, so giving a stack a real one meant copying the
// whole component Secret, internal keys included. Now each such key can name its own Secret and key:
//
//   - workers: OPENAI_API_KEY, GROQ_API_KEY and ANTHROPIC_API_KEY from the same
//     spec.llm.providers[].apiKeySecretRef the engine reads, and SENTRY_DSN from
//     spec.components.workers.sentryDSNSecretRef;
//   - index-builder: CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID from `token` and `accountID` of
//     spec.cloudflare.apiTokenSecretRef, the token that already creates the KV namespaces it writes;
//     ABLY_API_KEY from spec.components.ingestion.ablyKeySecretRef for deployment progress;
//   - dashboard: STRIPE_SECRET_KEY from spec.components.dashboard.stripeSecretKeySecretRef, a Stripe
//     restricted key with only the permissions the dashboard uses (#802; dashboard_production.go).
//
// Each reference renders an explicit secretKeyRef env entry, which wins over the component Secret's
// envFrom, and its Secret joins the workload's credentialSecrets, so a changed value rolls it. The
// operator never creates or changes a referenced Secret: a missing Secret, key or value fails the
// component's step, naming the field, the Secret and the key, never a value. componentSecretData
// stops seeding a key once its reference is set (ensureComponentSecret).

// keyRef is one third-party key a component takes from a referenced Secret.
type keyRef struct {
	env, secret, key string
	// field is the spec field that names the reference, for messages.
	field string
}

// workersLLMProviders are the LLM providers whose keys the workers read (OPENAI_API_KEY,
// GROQ_API_KEY, ANTHROPIC_API_KEY; the RAG answer path). Google's key is the engine's alone.
var workersLLMProviders = []string{"openai", "groq", "anthropic"}

// componentKeyRefs is the third-party keys component takes from referenced Secrets, in a stable
// order. A component without any returns nil.
func componentKeyRefs(oc *oramav1alpha1.OramaCloudV1, component string) []keyRef {
	var refs []keyRef
	switch component {
	case "workers":
		for i, p := range llmProviders(oc) {
			if p.APIKeySecretRef == nil || p.APIKeySecretRef.Name == "" || !slices.Contains(workersLLMProviders, p.Name) {
				continue
			}
			env := llmKeyEnv(p.Name)
			refs = append(refs, keyRef{env: env, secret: p.APIKeySecretRef.Name, key: env,
				field: fmt.Sprintf("spec.llm.providers[%d] (%s).apiKeySecretRef", i, p.Name)})
		}
		if ref := oc.Spec.Components.Workers.SentryDSNSecretRef; ref != nil {
			refs = append(refs, keyRef{env: "SENTRY_DSN", secret: ref.Name, key: ref.Key,
				field: "spec.components.workers.sentryDSNSecretRef"})
		}
	case "index-builder":
		if ref := oc.Spec.Components.Ingestion.AblyKeySecretRef; ref != nil {
			refs = append(refs, keyRef{env: "ABLY_API_KEY", secret: ref.Name, key: ref.Key,
				field: "spec.components.ingestion.ablyKeySecretRef"})
		}
		if ref := oc.Spec.Cloudflare.APITokenSecretRef; ref != nil && ref.Name != "" {
			const field = "spec.cloudflare.apiTokenSecretRef"
			refs = append(refs,
				keyRef{env: "CLOUDFLARE_API_TOKEN", secret: ref.Name, key: "token", field: field},
				keyRef{env: "CLOUDFLARE_ACCOUNT_ID", secret: ref.Name, key: "accountID", field: field})
		}
	case "dashboard":
		if ref := oc.Spec.Components.Dashboard.StripeSecretKeySecretRef; ref != nil {
			refs = append(refs, keyRef{env: "STRIPE_SECRET_KEY", secret: ref.Name, key: ref.Key,
				field: "spec.components.dashboard.stripeSecretKeySecretRef"})
		}
	}
	return refs
}

// keyRefEnv is the explicit env entries for refs, each a required secretKeyRef.
func keyRefEnv(refs []keyRef) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(refs))
	for _, r := range refs {
		env = append(env, secretKeyEnv(r.env, r.secret, r.key))
	}
	return env
}

// keyRefSecrets is the distinct Secrets refs name, in order.
func keyRefSecrets(refs []keyRef) []string {
	var names []string
	for _, r := range refs {
		if !slices.Contains(names, r.secret) {
			names = append(names, r.secret)
		}
	}
	return names
}

// checkKeyRefs reads every Secret refs name, uncached, and fails on a missing Secret, a missing key
// or an empty value, naming the variable, the field, the Secret and the key. It runs before the
// workload is written, so a broken reference leaves the running template as it is.
func (r *OramaCloudV1Reconciler) checkKeyRefs(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, refs []keyRef) error {
	read := map[string]*corev1.Secret{}
	for _, ref := range refs {
		sec, ok := read[ref.secret]
		if !ok {
			sec = &corev1.Secret{}
			if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: ref.secret}, sec); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("%s: Secret %s/%s (%s) does not exist", ref.env, oc.Namespace, ref.secret, ref.field)
				}
				return fmt.Errorf("%s: read Secret %s/%s: %w", ref.env, oc.Namespace, ref.secret, err)
			}
			read[ref.secret] = sec
		}
		v, ok := sec.Data[ref.key]
		switch {
		case !ok:
			return fmt.Errorf("%s: Secret %s/%s (%s) has no key %s", ref.env, oc.Namespace, ref.secret, ref.field, ref.key)
		case len(v) == 0:
			return fmt.Errorf("%s: Secret %s/%s (%s) key %s is empty", ref.env, oc.Namespace, ref.secret, ref.field, ref.key)
		}
	}
	return nil
}

// keyRefSecretsReadBy is the referenced third-party key Secrets of oc's enabled workers,
// index-builder and dashboard, for the Secret watch (secretsReadBy).
func keyRefSecretsReadBy(oc *oramav1alpha1.OramaCloudV1) []string {
	var names []string
	if oc.Spec.Components.Workers.Enabled {
		names = append(names, keyRefSecrets(componentKeyRefs(oc, "workers"))...)
	}
	if oc.Spec.Components.Ingestion.Enabled {
		names = append(names, keyRefSecrets(componentKeyRefs(oc, "index-builder"))...)
	}
	if oc.Spec.Components.Dashboard.Enabled {
		names = append(names, keyRefSecrets(componentKeyRefs(oc, "dashboard"))...)
	}
	return names
}
