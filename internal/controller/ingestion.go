/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// reconcileIngestion provisions the ingest path: index-builder (Cloud Run -> a plain
// Deployment) and, when enabled, HARVEST (master + Mongo record store + Selenium grid).
func (r *OramaCloudV1Reconciler) reconcileIngestion(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	ing := oc.Spec.Components.Ingestion
	if !ing.Enabled {
		return r.teardownByComponent(ctx, oc, "index-builder", "harvest-master", "harvest-mongo", "selenium")
	}

	// index-builder orchestrator (was GCP Cloud Run). Its EnvSchema requires ~29 vars at boot;
	// most are in-cluster URLs + constants (indexBuilderEnv), the credential-shaped ones come via
	// the component Secret (envFrom), and the CF KV namespace IDs come from the owned
	// CloudflareKVNamespace CRs' status (dummy until Cloudflare provisions them). Boot connects to
	// nothing eagerly (Redis/PG/DISCO/Ably are per-build), so present-and-valid env → healthy.
	// It needs the master key, so it waits for spec.masterAPIKeySecretRef (masterAPIKeySecret).
	if masterKey, ok := masterAPIKeySecret(oc); ok {
		// Referenced Cloudflare credentials (#571) and Ably key (#747) are checked before the
		// Deployment is touched. HARVEST's future worker launcher remains separate (#12).
		refs := componentKeyRefs(oc, "index-builder")
		if err := r.checkKeyRefs(ctx, oc, refs); err != nil {
			return err
		}
		if err := r.reconcileApp(ctx, oc, appDef{
			component:         "index-builder",
			image:             componentImage(oc, "index-builder", "index-builder"),
			port:              8080,
			env:               append(r.indexBuilderEnv(ctx, oc), keyRefEnv(refs)...),
			credentialSecrets: append([]string{cacheAuthSecretName(oc), masterKey, hpesSecretName(oc), harvestSignSecretName(oc)}, keyRefSecrets(refs)...),
			resources:         resourcesOr(ing.Resources, resourceReqs("256Mi", "100m", "768Mi")),
		}); err != nil {
			return err
		}
	}

	if !ing.Harvest {
		return nil
	}

	// HARVEST crawler MASTER (Node/Express job API on :8080). The worker is a per-crawl one-shot
	// the master spawns — running the worker as a Deployment CrashLoops (operator#12), so we run
	// the master. Its worker-spawning (Podman → WORKER_IMAGE) still needs a k8s-Jobs port to crawl,
	// so it gets no PODMAN_API_HOST (reconcileHarvestMaster).
	// It signs in to the broker as harvest (#824, LSR-134), so it is written only once the topology
	// reports that user on the broker; until then a running harvest-master keeps its template.
	n := oc.Name
	ready, err := r.brokerUsersReady(ctx, oc)
	if err != nil {
		return err
	}
	if ready {
		if err := r.reconcileHarvestMaster(ctx, oc, n); err != nil {
			return err
		}
	}

	// HARVEST's record store (Mongo). Single-node StatefulSet + PVC + Service. Mongo 7 detects the
	// cgroup memory limit for its WiredTiger cache sizing, so this limit is binary-aware (#18).
	if err := r.provisionStatefulSet(ctx, oc, dataStoreDef{
		component: "harvest-mongo",
		image:     "mongo:7@sha256:c53852c00c23ca9f23fc39690350ffa9a80e3642fe8f96879726d8da0abe80a7",
		ports:     []corev1.ContainerPort{{Name: "mongo", ContainerPort: 27017}},
		dataPath:  "/data/db",
		resources: resourceReqs("256Mi", "50m", "1Gi"),
	}); err != nil {
		return err
	}

	// Selenium grid for JS-rendered crawls. Chromium is memory-heavy → a roomier limit than an app.
	return r.reconcileApp(ctx, oc, appDef{
		component: "selenium",
		image:     storeImage(oc, "selenium", "selenium/standalone-chromium:latest@sha256:c7c17cd79c62819a8329df00e610dbc4073c279da034a6f37135d454eb0d8870"),
		port:      4444,
		resources: resourceReqs("512Mi", "100m", "1536Mi"),
	})
}

const (
	// harvestSignKey is the key of <cr>-harvest-sign: HARVEST's control key, which index-builder
	// sends as "Authorization: Bearer" (its HARVEST_SIGN) and harvest-master checks (its
	// HARVEST_SIGNATURE, at least 32 characters). One value under two names.
	harvestSignKey = "HARVEST_SIGN"
	// harvestNotificationKey is the key of <cr>-harvest-notification: harvest-master's private HMAC
	// key for job status notifications (at least 32 characters). It is never sent, and no other
	// component holds it: the receiver of a notification is the URL in the job's own configuration.
	harvestNotificationKey = "NOTIFICATION_SIGNATURE"
)

// harvestSignSecretName is the generated Secret holding HARVEST's control key (ensureCredentialSecrets).
func harvestSignSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-harvest-sign" }

// harvestNotificationSecretName is the generated Secret holding harvest-master's notification
// signing key (ensureCredentialSecrets).
func harvestNotificationSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-harvest-notification"
}

// reconcileHarvestMaster owns the harvest-master Deployment and Service.
//
// PODMAN_API_HOST is not set: harvest-master spawns crawler worker containers through a Podman
// (Docker-compatible) API, and none runs in the cluster, because the worker is not spawned here yet
// (operator#12). The operator does not point it at an endpoint that does not exist.
func (r *OramaCloudV1Reconciler) reconcileHarvestMaster(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, n string) error {
	return r.reconcileApp(ctx, oc, appDef{
		component: "harvest-master",
		image:     componentImage(oc, "harvest-master", "harvest-master"),
		port:      8080,
		// AMQP_HOST and REDIS_HOST are URLs whose credentials are $(VAR) references to the env
		// entries before them, expanded by the kubelet from <cr>-broker-harvest and <cr>-cache-auth.
		env: append(harvestBrokerEnv(oc), redisPasswordEnvVar(oc),
			secretKeyEnv("HARVEST_SIGNATURE", harvestSignSecretName(oc), harvestSignKey),
			secretKeyEnv(harvestNotificationKey, harvestNotificationSecretName(oc), harvestNotificationKey),
			corev1.EnvVar{Name: "APP_ENV", Value: "production"},
			corev1.EnvVar{Name: "EXPRESS_PORT", Value: "8080"},
			corev1.EnvVar{Name: "MONGODB_URL", Value: "mongodb://" + n + "-harvest-mongo:27017/harvest"},
			corev1.EnvVar{Name: "AMQP_HOST", Value: amqpURL(oc)},
			corev1.EnvVar{Name: "REDIS_HOST", Value: redisURL(oc)},
			corev1.EnvVar{Name: "MASTER_URI", Value: "http://" + n + "-harvest-master:8080"},
			corev1.EnvVar{Name: "WORKER_IMAGE", Value: componentImage(oc, "harvest", "harvest")},
		),
		credentialSecrets: []string{brokerHarvestSecretName(oc), cacheAuthSecretName(oc), harvestSignSecretName(oc), harvestNotificationSecretName(oc)},
		resources:         resourceReqs("128Mi", "50m", "512Mi"),
	})
}

// indexBuilderEnv provides the non-secret half of index-builder's EnvSchema (in-cluster URLs +
// constants + the CF KV namespace IDs from the owned KV CRs). Credential-shaped vars come via the
// component Secret (envFrom), except MASTER_API_KEY (spec.masterAPIKeySecretRef), the Redis
// password (<cr>-cache-auth), which REDIS_URL references, the HPES bearer (<cr>-hpes, #574) and
// HARVEST's control key (<cr>-harvest-sign). Nothing connects eagerly at boot, so
// present+valid env → healthy; external endpoints (Apify) are placeholders until real provisioning.
func (r *OramaCloudV1Reconciler) indexBuilderEnv(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	n := oc.Name
	return []corev1.EnvVar{
		masterAPIKeyEnv(oc),
		redisPasswordEnvVar(oc), // before REDIS_URL, which references it
		{Name: "NODE_ENV", Value: "production"},
		{Name: "TELEMETRY_WORKER_NAME", Value: "telemetry-worker"},
		{Name: "SENTRY_DSN", Value: ""}, // empty disables Sentry
		{Name: "SMALL_INDEX_COUNT_THRESHOLD", Value: "1000"},
		{Name: "BASE_TMP_DIR", Value: "/tmp/index-builder"},
		{Name: "CLOUDFLARE_BASE_URL", Value: cfAPIBase},
		{Name: "CLOUDFLARE_DUMPS_KV_NAMESPACE_ID", Value: r.kvNamespaceID(ctx, oc, n+"-kv-dumps")},
		{Name: "CLOUDFLARE_API_KEYS_KV_NAMESPACE_ID", Value: r.kvNamespaceID(ctx, oc, n+"-kv-api-keys")},
		{Name: "DEPLOYMENT_API_BASE_URL", Value: "http://" + n + "-dashboard:3000"},
		{Name: "APIFY_ACT_ENDPOINT", Value: "https://api.apify.com/v2/acts/placeholder/runs"},
		{Name: "APIFY_DATASET_ENDPOINT", Value: "https://api.apify.com/v2/datasets/placeholder/items"},
		{Name: "REDIS_URL", Value: redisURL(oc)},
		pgURIEnv(oc, "DATABASE_URL"), // CNPG-generated URI (real password), not a placeholder (#32)
		{Name: "DISCO_URL", Value: "http://" + n + "-disco:8080"},
		{Name: "DISCO_PEERS", Value: ""},
		// ORAMA_EMBEDDINGS_URL and ORAMA_EMBEDDINGS_BEARER: the stack's own HPES (#574).
		{Name: embeddingsURLEnv, Value: hpesURL(oc)},
		secretKeyEnv(embeddingsBearerEnv, hpesSecretName(oc), hpesTokenKey),
		{Name: "DECRYPTER_BASE_URL", Value: "http://" + n + "-openai-decrypter:8080"},
		{Name: "HARVEST_MASTER_ENDPOINT", Value: "http://" + n + "-harvest-master:8080"},
		secretKeyEnv("HARVEST_SIGN", harvestSignSecretName(oc), harvestSignKey), // = harvest-master's HARVEST_SIGNATURE
		{Name: "REST_API_AUTHORIZATION_HEADER_PRIVATE_KEY_PATH", Value: "/etc/index-builder/rest-api-private-key.pem"},
	}
}

// kvNamespaceID returns the provisioned CF KV namespace ID from the owned CloudflareKVNamespace
// CR's status, or a placeholder until Cloudflare provisions it (the sandbox has no CF token).
func (r *OramaCloudV1Reconciler) kvNamespaceID(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string) string {
	kv := &oramav1alpha1.CloudflareKVNamespace{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, kv); err == nil && kv.Status.NamespaceID != "" {
		return kv.Status.NamespaceID
	}
	return "placeholder-kv-namespace-id"
}
