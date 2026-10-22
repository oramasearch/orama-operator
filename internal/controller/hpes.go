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

// HPES (embeddings-server-py) inside the stack (#574; bridge decisions of 16 October, 3, and 17
// October, 5 and 9). index-builder embeds documents through it; Prod 1 calls it at ai.orama.run. It
// is not the engine's embeddings sidecar (embeddingsImage, oramacore-ai-server), which is a different
// program and stays as it is.
//
// It runs on CPU, on the stack's ordinary nodes, whenever ingestion runs: no toggle of its own, no
// GPU request, no Ingress, no DNS record and no public host. index-builder is its only client.

const (
	hpesComponent = "hpes"
	hpesPort      = 8787
	// hpesTokenKey is the key of <cr>-hpes holding the bearer index-builder sends and HPES checks.
	hpesTokenKey = "token"
	// hpesDefaultRepo is the image repository componentImage resolves without an override. Production
	// pins oramasearch/embeddings-server-py:cpu-<sha> by digest through spec.images.overrides["hpes"]
	// (embeddings-server-py#9 builds it).
	hpesDefaultRepo = "embeddings-server-py"
	// hpesModelsGroup is the server's model group: every model a copied Typesense index can name,
	// as Prod 1's systemd unit loads (python3 main.py --models-group=all start_server).
	hpesModelsGroup = "all"
	// hpesThreads is the server's TOTAL_THREADS (it must be even; half of it sizes each of its two
	// thread pools), matched to the CPU request so the server does not assume it owns the node.
	hpesThreads = "4"

	// index-builder's HPES client (index-manager apps/index-builder/src/embeddings/oramaai.ts).
	embeddingsURLEnv    = "ORAMA_EMBEDDINGS_URL"
	embeddingsBearerEnv = "ORAMA_EMBEDDINGS_BEARER"
)

// hpesSecretName is the generated Secret holding the bearer (ensureCredentialSecrets).
func hpesSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-" + hpesComponent
}

// hpesRuns reports whether HPES runs: wherever ingestion runs.
func hpesRuns(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Components.Ingestion.Enabled
}

// hpesURL is the endpoint index-builder posts to. The client posts to ORAMA_EMBEDDINGS_URL exactly as
// given (fetchEmbeddings), and decodes base64 float32 vectors when the URL ends in _b64. The server's
// /v1/embeddings_b64 answers {embeddings, dimensions}, which is what that client reads; its bare
// /v1/embeddings answers an OpenAI-shaped {data: [...]}, which the client does not read, and / is not
// a route. Both base64 float32 and JSON carry the same float32 values.
func hpesURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "http://" + oc.Name + "-" + hpesComponent + ":8787/v1/embeddings_b64"
}

// reconcileHPES runs embeddings-server-py as a Deployment and Service <cr>-hpes on 8787. The server
// refuses any request whose Authorization is not "Bearer <API_KEY>", its /health route included, so
// the probes stay TCP on 8787: the server loads every model before it listens, so an open port means
// the models are loaded. The gRPC listener the server also starts (50051) is not exposed. A changed
// bearer rolls it (credentialSecrets). Disabling ingestion removes it and keeps the bearer.
func (r *OramaCloudV1Reconciler) reconcileHPES(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !hpesRuns(oc) {
		return r.teardownByComponent(ctx, oc, hpesComponent)
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: hpesComponent,
		image:     componentImage(oc, hpesComponent, hpesDefaultRepo),
		port:      hpesPort,
		env: []corev1.EnvVar{
			secretKeyEnv("API_KEY", hpesSecretName(oc), hpesTokenKey),
			{Name: "EXECUTION_PROVIDERS", Value: "CPUExecutionProvider"},
			{Name: "MODELS_GROUP", Value: hpesModelsGroup},
			{Name: "HOST", Value: "0.0.0.0"},
			{Name: "HTTP_PORT", Value: "8787"},
			{Name: "TOTAL_THREADS", Value: hpesThreads},
		},
		credentialSecrets: []string{hpesSecretName(oc)},
		// The six models of the "all" group (bge small/base/large, multilingual-e5 small/base/large)
		// are about 1.4 billion float32 parameters, ~5.7 GB loaded, plus the ONNX and torch runtime.
		resources: resourcesOr(oc.Spec.Components.Ingestion.HPESResources, resourceReqs("7Gi", "2", "10Gi")),
	})
}
