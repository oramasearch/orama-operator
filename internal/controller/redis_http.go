/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The serverless-redis-http shim (#562, bridge decisions of 17 October, #7). Prod 1's dashboard
// caches analytics query results through @upstash/redis, which speaks Upstash's REST protocol over
// HTTP (REDIS_CACHE_URL, REDIS_CACHE_TOKEN). The dashboard is not changed: the operator runs
// serverless-redis-http, which implements that protocol, in front of the stack's own Redis.
//
// The shim answers POST /, /pipeline and /multi-exec with a bearer token (the body form
// @upstash/redis uses; it does not implement command paths such as /set/k/v), and GET / without
// one. It runs whenever the cache runs in this cluster: no toggle of its own.

const (
	redisHTTPComponent = "redis-http"
	redisHTTPPort      = 80
	// redisHTTPTokenKey is the key of <cr>-redis-http holding the shim's bearer token.
	redisHTTPTokenKey = "token"

	// redisHTTPImage is hiett/serverless-redis-http 0.0.10 (index digest
	// sha256:65128347949bca511e448fd7238780d624573d74c22b79155a7563db19e9b678, linux/amd64 and
	// linux/arm64), copied without a rebuild into the private oramasearch/serverless-redis-http
	// repository, so the digest is the same. spec.images.overrides["redis-http"] wins.
	redisHTTPImage = "oramasearch/serverless-redis-http@sha256:65128347949bca511e448fd7238780d624573d74c22b79155a7563db19e9b678"

	// The dashboard's @upstash/redis client reads these (src/lib/redis/redis.ts).
	redisCacheURLEnv   = "REDIS_CACHE_URL"
	redisCacheTokenEnv = "REDIS_CACHE_TOKEN"
)

// redisHTTPSecretName is the generated Secret holding the shim's token (ensureCredentialSecrets).
func redisHTTPSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-" + redisHTTPComponent
}

// redisHTTPRuns reports whether the shim runs: wherever the cache runs in this cluster.
func redisHTTPRuns(oc *oramav1alpha1.OramaCloudV1) bool {
	return storeLocal(oc, oc.Spec.Components.Cache)
}

// redisHTTPURL is the shim's in-cluster URL, the Service reconcileApp writes on port 80.
func redisHTTPURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "http://" + oc.Name + "-" + redisHTTPComponent
}

// redisCacheEnv is the dashboard's Upstash client configuration: the shim's URL and its token.
func redisCacheEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: redisCacheURLEnv, Value: redisHTTPURL(oc)},
		secretKeyEnv(redisCacheTokenEnv, redisHTTPSecretName(oc), redisHTTPTokenKey),
	}
}

// reconcileRedisHTTP runs the shim as a Deployment and Service <cr>-redis-http on port 80. It
// connects to the cache with the generated password (SRH_CONNECTION_STRING references the
// REDIS_PASSWORD entry before it) and requires the token from <cr>-redis-http. A changed token or
// Redis password rolls it (credentialSecrets). It has no component Secret of its own.
func (r *OramaCloudV1Reconciler) reconcileRedisHTTP(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !redisHTTPRuns(oc) {
		return r.teardownByComponent(ctx, oc, redisHTTPComponent)
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: redisHTTPComponent,
		image:     storeImage(oc, redisHTTPComponent, redisHTTPImage),
		port:      redisHTTPPort,
		env: []corev1.EnvVar{
			redisPasswordEnvVar(oc),
			{Name: "SRH_MODE", Value: "env"},
			secretKeyEnv("SRH_TOKEN", redisHTTPSecretName(oc), redisHTTPTokenKey),
			{Name: "SRH_CONNECTION_STRING", Value: redisURL(oc)},
		},
		credentialSecrets: []string{cacheAuthSecretName(oc), redisHTTPSecretName(oc)},
		resources:         resourceReqs("64Mi", "50m", "256Mi"),
	})
}
