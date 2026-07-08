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
func secretName(oc *oramav1alpha1.OramaCloudV1) string    { return oc.Name + "-secrets" }

// reconcileBootstrap creates the shared config ConfigMap + dummy-value Secret every
// component consumes via envFrom, so a fresh install is self-bootstrapping (no manual
// secret pre-creation). The ConfigMap (non-sensitive endpoints/settings) is
// reconciled every pass; the Secret is **create-if-absent** so real / rotated /
// ExternalSecrets-managed values are never clobbered.
func (r *OramaCloudV1Reconciler) reconcileBootstrap(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	n := oc.Name

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName(oc), Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(oc, "config")
		cm.Data = map[string]string{
			"REGION":               oc.Spec.Region.Name,
			"ROLE":                 oc.Spec.Region.Role,
			"POSTGRES_HOST":        n + "-controldb-rw", // CNPG read/write Service
			"POSTGRES_PORT":        "5432",
			"POSTGRES_DB":          controlDBName,
			"CLICKHOUSE_HOST":      n + "-analytics",
			"CLICKHOUSE_PORT":      "8123",
			"REDIS_HOST":           n + "-cache",
			"REDIS_PORT":           "6379",
			"RABBITMQ_HOST":        n + "-broker",
			"RABBITMQ_PORT":        "5672",
			"TYPESENSE_HOST":       n + "-search",
			"TYPESENSE_PORT":       "8108",
			"TYPESENSE_DATA_DIR":   "/data",
			"TYPESENSE_ENABLE_CORS": "true",
			"DISCO_URL":            "http://" + n + "-disco:8080",
			"ORAMACORE_WRITER_URL": "http://" + n + "-writer:8080",
			"ORAMACORE_READER_URL": "http://" + n + "-reader",
			"EMBEDDINGS_URL":       "http://" + n + "-disco:50051",
			"KC_HTTP_ENABLED":      "true",
			"KC_PROXY_HEADERS":     "xforwarded",
			"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
		}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}

	// Secret: create-if-absent with DUMMY dev values. Real deployments override these
	// (edit in place, or point spec.secrets at ExternalSecrets) — the operator won't
	// overwrite an existing secret.
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: secretName(oc)}, &corev1.Secret{}); err == nil {
		return nil // exists — leave it
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(oc), Namespace: oc.Namespace, Labels: labelsFor(oc, "secrets")},
		StringData: map[string]string{
			// ⚠ DUMMY placeholders for a self-contained dev install. Override in prod.
			"MASTER_API_KEY":              "dev-master-api-key",
			"TYPESENSE_API_KEY":           "dev-typesense-api-key",
			"POSTGRES_PASSWORD":           "dev-postgres-password",
			"REDIS_PASSWORD":              "dev-redis-password",
			"RABBITMQ_PASSWORD":           "dev-rabbitmq-password",
			"KC_BOOTSTRAP_ADMIN_PASSWORD": "dev-keycloak-password",
			"JWT_PRIVATE_KEY":             "dev-jwt-private-key-REPLACE-ME",
			"JWT_PUBLIC_KEY":              "dev-jwt-public-key-REPLACE-ME",
			"GROQ_API_KEY":                "dev-groq-api-key",
			"OPENAI_API_KEY":              "dev-openai-api-key",
			"ANTHROPIC_API_KEY":           "dev-anthropic-api-key",
			"CLOUDFLARE_API_TOKEN":        "dev-cloudflare-api-token",
		},
	}
	if err := controllerutil.SetControllerReference(oc, sec, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, sec)
}

// envFromShared feeds the shared ConfigMap + Secret to a component's containers.
func envFromShared(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvFromSource {
	return []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(oc)}}},
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName(oc)}}},
	}
}
