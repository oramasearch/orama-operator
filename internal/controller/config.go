/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// llmProviderNames is the shared LLM_PROVIDERS value: the names of the effective provider list
// (llmProviders), the same list the engine config renders. End users pick one per query.
func llmProviderNames(oc *oramav1alpha1.OramaCloudV1) string {
	provs := llmProviders(oc)
	names := make([]string, 0, len(provs))
	for _, p := range provs {
		names = append(names, p.Name)
	}
	return strings.Join(names, ",")
}

// llmDefault is spec.llm.default, or groq when it is empty.
func llmDefault(oc *oramav1alpha1.OramaCloudV1) string {
	if oc.Spec.LLM.Default != "" {
		return oc.Spec.LLM.Default
	}
	return "groq"
}

// jwtIssuer is the one issuer of the stack's write tokens: the shared ConfigMap's JWT_ISSUER, which
// the dashboard signs with, and the writer's only accepted issuer (writerJWTYAML). spec.jwtIssuer, or
// orama-cloud when empty, which validateWriterJWT allows only while engine.writer.jwt is unset.
func jwtIssuer(oc *oramav1alpha1.OramaCloudV1) string {
	if oc.Spec.JWTIssuer != "" {
		return oc.Spec.JWTIssuer
	}
	return "orama-cloud"
}

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
			"REGION":                oc.Spec.Region.Name,
			"ROLE":                  oc.Spec.Region.Role,
			"LLM_PROVIDERS":         llmProviderNames(oc), // multiple; users pick per query
			"LLM_DEFAULT":           llmDefault(oc),
			"POSTGRES_HOST":         n + "-controldb-rw",
			"POSTGRES_PORT":         "5432",
			"POSTGRES_DB":           controlDBName,
			"CLICKHOUSE_HOST":       n + "-analytics",
			"CLICKHOUSE_PORT":       "8123",
			"REDIS_HOST":            n + "-cache",
			"REDIS_PORT":            "6379",
			"RABBITMQ_HOST":         n + "-broker",
			"RABBITMQ_PORT":         "5672",
			"TYPESENSE_HOST":        n + "-search",
			"TYPESENSE_PORT":        "8108",
			"TYPESENSE_DATA_DIR":    "/data",
			"TYPESENSE_ENABLE_CORS": "true",
			"DISCO_URL":             "http://" + n + "-disco:8080",
			"ORAMACORE_WRITER_URL":  "http://" + n + "-writer:8080",
			"ORAMACORE_READER_URL":  "http://" + n + "-reader",
			"EMBEDDINGS_URL":        "http://" + n + "-disco:50051",
			// #8 QStash-equivalent: the in-cluster RabbitMQ (a true QStash HTTP shim is a follow-up).
			"QSTASH_URL":                  "http://" + n + "-broker:5672",
			"KC_HTTP_ENABLED":             "true",
			"KC_PROXY_HEADERS":            "xforwarded",
			"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
			"JWT_ISSUER":                  jwtIssuer(oc),
		}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	})
	if err != nil {
		return err
	}
	if err := r.ensureCredentialSecrets(ctx, oc); err != nil {
		return err
	}
	if err := r.pruneRetiredComponentCredentials(ctx, oc); err != nil {
		return err
	}
	// The engine's broker users' passwords (#99), before the engine step hashes them, so a fresh
	// stack's writer and readers are written once, not again when the broker step creates them.
	if storeLocal(oc, oc.Spec.Components.Broker) {
		if err := r.ensureBrokerSecrets(ctx, oc); err != nil {
			return fmt.Errorf("broker users Secret: %w", err)
		}
	}
	return r.ensureJWTSecrets(ctx, oc)
}

// ensureJWTSecrets generates a real RSA-2048 keypair (once, create-if-absent) for the
// OramaCore writer JWT. Least-privilege: <cr>-jwt holds private+public (the dashboard
// signs with it); <cr>-jwt-pub holds public only (the engine verifies). An existing pair is
// never rotated; a missing <cr>-jwt-pub is recovered from it (ensureJWTPublicSecret).
func (r *OramaCloudV1Reconciler) ensureJWTSecrets(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	jwtName := oc.Name + "-jwt"
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: jwtName}, existing); err == nil {
		return r.ensureJWTPublicSecret(ctx, oc, existing)
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	// dashboard: private + public (it signs the writer JWT). Data, not StringData, so every reader,
	// including a fake client, sees the keys under Data.
	jwt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: jwtName, Namespace: oc.Namespace, Labels: labelsFor(oc, "jwt")},
		Data:       map[string][]byte{"JWT_PRIVATE_KEY": privPEM, "JWT_PUBLIC_KEY": pubPEM},
	}
	if err := controllerutil.SetControllerReference(oc, jwt, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, jwt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// Another reconcile created the pair first: derive the public Secret from the winning pair,
		// not from the one generated here, which is discarded.
		winner := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(jwt), winner); err != nil {
			return err
		}
		jwt = winner
	}
	return r.ensureJWTPublicSecret(ctx, oc, jwt)
}

// ensureJWTPublicSecret creates <cr>-jwt-pub from the signing Secret jwt when it is missing, so an
// interrupted first run or a deleted public Secret heals on the next reconcile. It copies only
// JWT_PUBLIC_KEY (from Data, or StringData on the just-created object) and leaves an existing public
// Secret as it is: it neither rotates nor repairs keys.
func (r *OramaCloudV1Reconciler) ensureJWTPublicSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, jwt *corev1.Secret) error {
	name := jwt.Name + "-pub"
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	publicKey := jwt.Data["JWT_PUBLIC_KEY"]
	if len(publicKey) == 0 {
		publicKey = []byte(jwt.StringData["JWT_PUBLIC_KEY"])
	}
	if len(publicKey) == 0 {
		return fmt.Errorf("secret %s has no JWT_PUBLIC_KEY to recover %s from; restore it, the operator does not rotate the pair", jwt.Name, name)
	}
	pub := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: labelsFor(oc, "jwt")},
		Data:       map[string][]byte{"JWT_PUBLIC_KEY": publicKey},
	}
	if err := controllerutil.SetControllerReference(oc, pub, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pub); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// componentSecretData returns the DUMMY dev credentials a component needs — scoped so
// each component's secret holds only its own keys (derived from the production connection
// maps). Returns nil for components that need no secret. Real
// deployments override these (edit in place, or source from ExternalSecrets).
//
// MASTER_API_KEY is not seeded here: every consumer takes it from spec.masterAPIKeySecretRef through
// an explicit env entry (masterAPIKeyEnv), which wins over a value an older Secret still holds. The
// broker and Redis passwords are generated into <cr>-broker-admin and <cr>-cache-auth (#307).
//
// NOTE: the Cloudflare API token is intentionally NOT here — it's needed only by the
// operator (the CF DNS reconciler), read from spec.cloudflare.apiTokenSecretRef, and
// a dummy value would be useless (the CF API would reject it).
func componentSecretData(component string) map[string]string {
	switch component {
	case "engine": // OramaCore: LLM providers, model downloads, JWT verify (public only)
		return map[string]string{
			// JWT_PUBLIC_KEY comes from the generated <cr>-jwt-pub secret (verify only).
			"GROQ_API_KEY":      "dev-groq-api-key",
			"OPENAI_API_KEY":    "dev-openai-api-key",
			"ANTHROPIC_API_KEY": "dev-anthropic-api-key",
			"GOOGLE_API_KEY":    "dev-google-api-key",
			"HF_TOKEN":          "dev-huggingface-token",
		}
	case "search":
		return map[string]string{}
	case "cache":
		// The Redis password is generated into <cr>-cache-auth (credentials.go), not seeded here.
		return map[string]string{}
	case "broker":
		// The admin credentials are generated into <cr>-broker-admin and rendered into the broker's
		// config by an init container (reconcileBroker), not seeded here.
		return map[string]string{}
	case "analytics":
		return map[string]string{}
	case "analytics-processor": // pulls /v1/analytics -> ClickHouse
		return map[string]string{}
	case "disco":
		return map[string]string{}
	case "keycloak":
		return map[string]string{"KC_BOOTSTRAP_ADMIN_PASSWORD": "dev-keycloak-password"}
	case "pgadmin": // pgAdmin4 web login (PGADMIN_DEFAULT_EMAIL is non-secret, set in the Deployment env)
		return map[string]string{}
	case "index-builder": // writes Typesense, embeds via HPES, Ably progress, CF KV, decrypter, DISCO
		// ORAMA_EMBEDDINGS_BEARER is not seeded: it comes from the generated <cr>-hpes (#574), and
		// HARVEST_SIGN from the generated <cr>-harvest-sign it shares with harvest-master.
		// index-builder's EnvSchema (src/utils.ts) requires 27 vars at boot; the secret
		// carries the credential-shaped ones (envFrom exposes each key as its env var). The
		// external ones (Apify/Ably) are dummies unless referenced/provisioned (operator#747).
		// The Ably key and Cloudflare pair are not seeded when their references are set
		// (componentKeyRefs). Non-secret URLs/constants are set as plain env in reconcileIngestion.
		return map[string]string{
			"ABLY_API_KEY":          "sandbox.dummy:sandboxdummyablysecret000000",
			"APIFY_TOKEN":           "dev-apify-token",
			"CLOUDFLARE_API_TOKEN":  "dev-cloudflare-token",
			"CLOUDFLARE_ACCOUNT_ID": "dev-cloudflare-account",
		}
	case "harvest": // crawler worker (one-shot): S3 exports, Ably, job-signing
		return map[string]string{
			"AWS_ACCESS_KEY_ID":     "dev",
			"AWS_SECRET_ACCESS_KEY": "dev-secret",
			"ABLY_API_KEY":          "sandbox.dummy:sandboxdummyablysecret000000",
		}
	case "harvest-master": // crawler master (job API): worker-image pull creds
		// HARVEST_SIGNATURE and NOTIFICATION_SIGNATURE are not seeded: they come from the generated
		// <cr>-harvest-sign and <cr>-harvest-notification.
		return map[string]string{
			"GITHUB_PAT": "dev-github-pat",
			"GHCR_TOKEN": "dev-ghcr-token",
		}
	case "dashboard": // app.example.com control plane — the widest secret surface
		return map[string]string{
			// JWT_PRIVATE_KEY + JWT_PUBLIC_KEY come from the generated <cr>-jwt secret
			// (the dashboard signs the OramaCore writer JWT); JWT_ISSUER is in the config.
			// POSTGRES_PASSWORD is sourced from the CNPG-generated <cr>-controldb-app Secret via
			// an explicit env in reconcileDashboard (#32) — not the dummy.
			"FIREBASE_SERVICE_ACCOUNT": "dev-firebase-service-account-json",
			"STRIPE_SECRET_KEY":        "dev-stripe-secret-key",
			"STRIPE_WEBHOOK_SECRET":    "dev-stripe-webhook-secret",
			"RESEND_API_KEY":           "dev-resend-api-key",
			"SLACK_TOKEN":              "dev-slack-token",
		}
	case "workers": // the CF workers bundle — RAG answer path + telemetry
		return map[string]string{
			"OPENAI_API_KEY":    "dev-openai-api-key",
			"GROQ_API_KEY":      "dev-groq-api-key",
			"ANTHROPIC_API_KEY": "dev-anthropic-api-key",
			"QSTASH_TOKEN":      "dev-qstash-token",
			"SENTRY_DSN":        "dev-sentry-dsn",
		}
	default:
		return nil // no secret (e.g. selenium, harvest-mongo)
	}
}

// Keys formerly seeded into component Secrets now live in generated credential Secrets. Prune only
// the old placeholder values on upgrade; preserve any user-supplied value in these Secrets.
func retiredComponentCredentialKeys(component string) []string {
	switch component {
	case "search":
		return []string{"TYPESENSE_API_KEY"}
	case "analytics", "analytics-processor":
		return []string{"CLICKHOUSE_PASSWORD"}
	case "disco", "workers":
		return []string{"CLICKHOUSE_PASSWORD", "DISCO_SECRET"}
	case "pgadmin":
		return []string{"PGADMIN_DEFAULT_PASSWORD"}
	case "dashboard":
		return []string{"CLICKHOUSE_PASSWORD", "CRON_SECRET", "REDIS_CACHE_TOKEN", "SCRAPER_SIGNATURE"}
	case "index-builder":
		return []string{"TYPESENSE_API_KEY", "API_SECRET", "DEPLOYMENT_API_API_KEY", "DECRYPTER_API_SECRET_BASE64", "HARVEST_SIGN", "ORAMA_EMBEDDINGS_BEARER", "DISCO_SECRET_KEY"}
	case "harvest":
		return []string{"HARVEST_SIGN"}
	case "harvest-master":
		return []string{"HARVEST_SIGNATURE", "NOTIFICATION_SIGNATURE"}
	}
	return nil
}

func retiredCredentialPlaceholder(key string, value []byte) bool {
	switch key {
	case "DISCO_SECRET", "DISCO_SECRET_KEY":
		return string(value) == legacyDiscoSecret
	case "PGADMIN_DEFAULT_PASSWORD":
		return string(value) == legacyPgAdminPassword
	}
	if strings.HasPrefix(string(value), "dev-") {
		return true
	}
	if key == "DECRYPTER_API_SECRET_BASE64" {
		decoded, err := base64.StdEncoding.DecodeString(string(value))
		return err == nil && strings.HasPrefix(string(decoded), "dev-")
	}
	return false
}

// pruneRetiredComponentCredentials removes obsolete copies even when a component is disabled or
// has no running workload (the harvest worker is one example). It never creates a component Secret.
func (r *OramaCloudV1Reconciler) pruneRetiredComponentCredentials(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	for _, component := range secretComponents {
		keys := retiredComponentCredentialKeys(component)
		if len(keys) == 0 {
			continue
		}
		sec := &corev1.Secret{}
		name := componentSecretName(oc, component)
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read component Secret %s/%s: %w", oc.Namespace, name, err)
		}
		changed := false
		for _, key := range keys {
			if value, found := sec.Data[key]; found && retiredCredentialPlaceholder(key, value) {
				delete(sec.Data, key)
				changed = true
			}
		}
		if changed {
			if err := r.Update(ctx, sec); err != nil {
				return fmt.Errorf("prune retired keys from component Secret %s/%s: %w", oc.Namespace, name, err)
			}
		}
	}
	return nil
}

// ensureComponentSecret creates a component's scoped secret (create-if-absent, dummy
// values) if the component needs one. Returns whether the component has a secret. A key the
// component takes from its own Secret reference (componentKeyRefs, #571) is not seeded: the
// explicit secretKeyRef wins over envFrom, and a dev-* value beside it would only mislead.
func (r *OramaCloudV1Reconciler) ensureComponentSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string) (bool, error) {
	data := componentSecretData(component)
	if data == nil {
		return false, nil
	}
	for _, ref := range componentKeyRefs(oc, component) {
		delete(data, ref.env)
	}
	// A production dashboard takes these from the user's own Secrets only (dashboard_production.go,
	// #794): no placeholder is seeded for them.
	if component == "dashboard" && !dashboardSeedsAuthPlaceholders(oc) {
		for _, name := range dashboardProductionUnseeded() {
			delete(data, name)
		}
	}
	name := componentSecretName(oc, component)
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, existing); err == nil {
		// Exists — seed any MISSING keys (newly-required dummy defaults) WITHOUT overwriting
		// existing values, so real/rotated secrets survive but new required keys get added
		// (e.g. when a component gains env on an operator upgrade).
		missing := map[string]string{}
		for k, v := range data {
			if _, ok := existing.Data[k]; !ok {
				missing[k] = v
			}
		}
		if len(missing) == 0 {
			return true, nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		for k, v := range missing {
			existing.Data[k] = []byte(v)
		}
		return true, r.Update(ctx, existing)
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	// Data, not StringData: the reconcile reads the values back (engineLLMSecretHash) and every
	// reader, including a fake client, sees the same bytes.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: labelsFor(oc, component)},
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		sec.Data[k] = []byte(v)
	}
	if err := controllerutil.SetControllerReference(oc, sec, r.Scheme); err != nil {
		return false, err
	}
	return true, r.Create(ctx, sec)
}

// envFromFor feeds the shared ConfigMap and (if present) the component's own secret.
func envFromFor(oc *oramav1alpha1.OramaCloudV1, component string) []corev1.EnvFromSource {
	src := []corev1.EnvFromSource{sharedConfigEnvFrom(oc)}
	if componentSecretData(component) != nil {
		src = append(src, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: componentSecretName(oc, component)}},
		})
	}
	// JWT keypair (generated): the engine verifies (public only); the dashboard signs.
	switch component {
	case "engine":
		src = append(src, secretEnvFrom(oc.Name+"-jwt-pub"))
	case "dashboard":
		src = append(src, secretEnvFrom(oc.Name+"-jwt"))
	}
	return src
}

// sharedConfigEnvFrom is the shared, non-secret <cr>-config ConfigMap as an env source.
func sharedConfigEnvFrom(oc *oramav1alpha1.OramaCloudV1) corev1.EnvFromSource {
	return corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(oc)}}}
}

func secretEnvFrom(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}
}
