/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

func configMapName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-config" }
func componentSecretName(oc *oramav1alpha1.OramaCloudV1, component string) string {
	return oc.Name + "-" + component + "-secret"
}

// reconcileBootstrap creates the shared **non-secret** config ConfigMap every
// component consumes via envFrom. Secrets are NOT put here — each component gets its
// own scoped secret (see componentSecretData / ensureComponentSecret), so no pod
// sees credentials it doesn't need.
func (r *OramaCloudV1Reconciler) reconcileBootstrap(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	n := oc.Name
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(oc, "config")
		cm.Data = map[string]string{
			"REGION":                      oc.Spec.Region.Name,
			"ROLE":                        oc.Spec.Region.Role,
			"POSTGRES_HOST":               n + "-controldb-rw",
			"POSTGRES_PORT":               "5432",
			"POSTGRES_DB":                 controlDBName,
			"CLICKHOUSE_HOST":             n + "-analytics",
			"CLICKHOUSE_PORT":             "8123",
			"REDIS_HOST":                  n + "-cache",
			"REDIS_PORT":                  "6379",
			"RABBITMQ_HOST":               n + "-broker",
			"RABBITMQ_PORT":               "5672",
			"TYPESENSE_HOST":              n + "-search",
			"TYPESENSE_PORT":              "8108",
			"TYPESENSE_DATA_DIR":          "/data",
			"TYPESENSE_ENABLE_CORS":       "true",
			"DISCO_URL":                   "http://" + n + "-disco:8080",
			"ORAMACORE_WRITER_URL":        "http://" + n + "-writer:8080",
			"ORAMACORE_READER_URL":        "http://" + n + "-reader",
			"EMBEDDINGS_URL":              "http://" + n + "-disco:50051",
			"KC_HTTP_ENABLED":             "true",
			"KC_PROXY_HEADERS":            "xforwarded",
			"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
		}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	})
	return err
}

// componentSecretData returns the DUMMY dev credentials a component needs — scoped so
// each component's secret holds only its own keys (derived from the prod connection
// maps in techops-docs). Returns nil for components that need no secret. Real
// deployments override these (edit in place, or source from ExternalSecrets).
//
// NOTE: the Cloudflare API token is intentionally NOT here — it's needed only by the
// operator (the CF DNS reconciler), read from spec.cloudflare.apiTokenSecretRef, and
// a dummy value would be useless (the CF API would reject it).
func componentSecretData(component string) map[string]string {
	switch component {
	case "engine": // OramaCore: master key, LLM providers, model downloads, JWT verify (public only)
		return map[string]string{
			"MASTER_API_KEY":    "dev-master-api-key",
			"JWT_PUBLIC_KEY":    "dev-jwt-public-key-REPLACE-ME",
			"GROQ_API_KEY":      "dev-groq-api-key",
			"OPENAI_API_KEY":    "dev-openai-api-key",
			"ANTHROPIC_API_KEY": "dev-anthropic-api-key",
			"HF_TOKEN":          "dev-huggingface-token",
		}
	case "search":
		return map[string]string{"TYPESENSE_API_KEY": "dev-typesense-api-key"}
	case "cache":
		return map[string]string{"REDIS_PASSWORD": "dev-redis-password"}
	case "broker":
		return map[string]string{"RABBITMQ_DEFAULT_USER": "orama", "RABBITMQ_DEFAULT_PASS": "dev-rabbitmq-password"}
	case "analytics":
		return map[string]string{"CLICKHOUSE_PASSWORD": "dev-clickhouse-password"}
	case "disco":
		return map[string]string{"DISCO_SECRET": "dev-disco-secret", "CLICKHOUSE_PASSWORD": "dev-clickhouse-password"}
	case "keycloak":
		return map[string]string{"KC_BOOTSTRAP_ADMIN_PASSWORD": "dev-keycloak-password"}
	case "index-builder": // writes Typesense, embeds via HPES, live progress via Ably
		return map[string]string{
			"TYPESENSE_API_KEY":       "dev-typesense-api-key",
			"MASTER_API_KEY":          "dev-master-api-key",
			"ORAMA_EMBEDDINGS_BEARER": "dev-hpes-bearer",
			"ABLY_API_KEY":            "dev-ably-api-key",
		}
	case "harvest": // crawler: S3 exports, Ably, job-signing
		return map[string]string{
			"AWS_ACCESS_KEY_ID":     "dev",
			"AWS_SECRET_ACCESS_KEY": "dev-secret",
			"ABLY_API_KEY":          "dev-ably-api-key",
			"HARVEST_SIGN":          "dev-harvest-signature",
		}
	case "dashboard": // app.orama.com control plane — the widest secret surface
		return map[string]string{
			"MASTER_API_KEY":           "dev-master-api-key",
			"JWT_PRIVATE_KEY":          "dev-jwt-private-key-REPLACE-ME", // signs the OramaCore writer JWT
			"JWT_PUBLIC_KEY":           "dev-jwt-public-key-REPLACE-ME",
			"JWT_ISSUER":               "orama-cloud",
			"POSTGRES_PASSWORD":        "dev-postgres-password",
			"CLICKHOUSE_PASSWORD":      "dev-clickhouse-password",
			"REDIS_PASSWORD":           "dev-redis-password",
			"FIREBASE_SERVICE_ACCOUNT": "dev-firebase-service-account-json",
			"STRIPE_SECRET_KEY":        "dev-stripe-secret-key",
			"STRIPE_WEBHOOK_SECRET":    "dev-stripe-webhook-secret",
			"AIRBYTE_CLIENT_SECRET":    "dev-airbyte-client-secret",
			"RESEND_API_KEY":           "dev-resend-api-key",
			"SLACK_TOKEN":              "dev-slack-token",
		}
	case "workers": // the CF workers bundle — RAG answer path + telemetry
		return map[string]string{
			"MASTER_API_KEY":      "dev-master-api-key",
			"OPENAI_API_KEY":      "dev-openai-api-key",
			"GROQ_API_KEY":        "dev-groq-api-key",
			"CLICKHOUSE_PASSWORD": "dev-clickhouse-password",
			"QSTASH_TOKEN":        "dev-qstash-token",
			"DISCO_SECRET":        "dev-disco-secret",
			"SENTRY_DSN":          "dev-sentry-dsn",
		}
	default:
		return nil // no secret (e.g. selenium, harvest-mongo)
	}
}

// ensureComponentSecret creates a component's scoped secret (create-if-absent, dummy
// values) if the component needs one. Returns whether the component has a secret.
func (r *OramaCloudV1Reconciler) ensureComponentSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string) (bool, error) {
	data := componentSecretData(component)
	if data == nil {
		return false, nil
	}
	name := componentSecretName(oc, component)
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, &corev1.Secret{}); err == nil {
		return true, nil // exists — never overwrite
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: labelsFor(oc, component)},
		StringData: data,
	}
	if err := controllerutil.SetControllerReference(oc, sec, r.Scheme); err != nil {
		return false, err
	}
	return true, r.Create(ctx, sec)
}

// envFromFor feeds the shared ConfigMap and (if present) the component's own secret.
func envFromFor(oc *oramav1alpha1.OramaCloudV1, component string) []corev1.EnvFromSource {
	src := []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(oc)}}},
	}
	if componentSecretData(component) != nil {
		src = append(src, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: componentSecretName(oc, component)}},
		})
	}
	return src
}
