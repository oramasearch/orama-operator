/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	oramacorePort  = 8080
	envoyPort      = 80
	envoyAdminPort = 9901
	envoyImage     = "envoyproxy/envoy:v1.31.2"

	// OramaCore handles SIGTERM gracefully (drains HTTP, then force-commits BOTH
	// sides to disk — oramacore/src/lib.rs, web_server/mod.rs). The writer's commit
	// flushes indexes (HNSW/FST/BM25), so it needs far more than the k8s default 30s
	// grace period or SIGKILL corrupts the flush. Readers only re-commit read state.
	writerGracePeriodSeconds int64 = 300
	readerGracePeriodSeconds int64 = 60
)

// envoyConfig is the static Envoy bootstrap that matches prod: listen :80, proxy to
// the local OramaCore on 127.0.0.1:8080 (see techops-docs Envoy topology). Envoy answers
// /metrics with 404 itself, so OramaCore's Prometheus endpoint is never served through the
// reader Service or the public reader host (#314); inside the cluster it stays on the pod's
// :8080. Envoy takes the first route that matches, so that route comes before the catch-all.
// Envoy reads this only at start: reconcileReaders hashes it into the reader pod template.
const envoyConfig = `admin:
  address: { socket_address: { address: 127.0.0.1, port_value: 9901 } }
static_resources:
  listeners:
  - name: listener_0
    address: { socket_address: { address: 0.0.0.0, port_value: 80 } }
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: ingress_http
          route_config:
            virtual_hosts:
            - name: local
              domains: ["*"]
              routes:
              - match: { path: "/metrics" }
                direct_response: { status: 404 }
              - match: { prefix: "/" }
                route: { cluster: oramacore, timeout: 60s }
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: oramacore
    connect_timeout: 30s
    type: STATIC
    load_assignment:
      cluster_name: oramacore
      endpoints:
      - lb_endpoints:
        - endpoint: { address: { socket_address: { address: 127.0.0.1, port_value: 8080 } } }
`

// reconcileEngine reconciles the OramaCore engine: writer (primary only) + readers,
// with an Envoy sidecar on readers, plus a shared config ConfigMap and Services.
func (r *OramaCloudV1Reconciler) reconcileEngine(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.Engine.Enabled {
		// The public endpoints go first: their Ingresses are deleted only while this CR controls them.
		if err := r.reconcileEngineIngresses(ctx, oc); err != nil {
			return err
		}
		return r.teardownByComponent(ctx, oc, "engine", "embeddings", "writer", "reader")
	}
	if err := validateEngineImages(oc); err != nil {
		return err
	}
	// The engine does not run without a real master key (#307): until spec.masterAPIKeySecretRef
	// resolves to one, the writer and readers are held at zero replicas and Reconcile reports the
	// cause in Ready (validateMasterAPIKey). Their templates, volumes and credentials are kept.
	if cond, err := r.validateMasterAPIKey(ctx, oc); err != nil {
		return err
	} else if cond != nil {
		return r.holdEngine(ctx, oc)
	}
	// Everything the engine templates depend on is checked before the first engine object is
	// written, so invalid LLM or writer JWT settings or a missing key leave the running engine as it is.
	if err := validateLLM(oc); err != nil {
		return err
	}
	if err := validateWriterJWT(oc); err != nil {
		return err
	}
	if _, err := r.ensureComponentSecret(ctx, oc, "engine"); err != nil {
		return fmt.Errorf("engine secret: %w", err)
	}
	llmSecretHash, err := r.engineLLMSecretHash(ctx, oc)
	if err != nil {
		return err
	}
	credentialsHashes := map[string]string{}
	for _, side := range []string{"writer", "reader"} {
		if credentialsHashes[side], err = r.credentialsHash(ctx, oc, engineCredentialSecrets(oc, engineSideFor(oc, side))...); err != nil {
			return err
		}
	}
	if err := r.reconcileEngineConfig(ctx, oc); err != nil {
		return fmt.Errorf("engine config: %w", err)
	}
	if err := r.reconcileEmbeddings(ctx, oc); err != nil {
		return fmt.Errorf("embeddings: %w", err)
	}
	if oc.Spec.Region.Role == "primary" {
		if err := r.reconcileWriter(ctx, oc, llmSecretHash, credentialsHashes["writer"]); err != nil {
			return fmt.Errorf("writer: %w", err)
		}
	}
	if err := r.reconcileReaders(ctx, oc, llmSecretHash, credentialsHashes["reader"], envoyConfig); err != nil {
		return fmt.Errorf("readers: %w", err)
	}
	return r.reconcileEngineIngresses(ctx, oc)
}

// holdEngine scales the writer StatefulSet and reader Deployment that exist to zero replicas,
// changing nothing else, so their volumes stay and the next valid reconcile restores the configured
// counts. A fresh stack gets no writer or readers.
func (r *OramaCloudV1Reconciler) holdEngine(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	writer, reader := &appsv1.StatefulSet{}, &appsv1.Deployment{}
	for _, w := range []struct {
		obj      client.Object
		name     string
		replicas **int32
	}{
		{writer, oc.Name + "-writer", &writer.Spec.Replicas},
		{reader, oc.Name + "-reader", &reader.Spec.Replicas},
	} {
		if err := r.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: w.name}, w.obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(w.obj, oc) || replicasOf(*w.replicas) == 0 {
			continue
		}
		base := w.obj.DeepCopyObject().(client.Object)
		*w.replicas = int32Ptr(0)
		if err := r.Patch(ctx, w.obj, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("hold %s at zero replicas: %w", w.name, err)
		}
	}
	return nil
}

// engineCredentialSecrets is the credential Secrets a side's render-config reads: <cr>-broker-admin,
// and the master API key Secret where the side's template holds master_api_key.
func engineCredentialSecrets(oc *oramav1alpha1.OramaCloudV1, side engineSide) []string {
	names := []string{brokerAdminSecretName(oc)}
	if side.readsMasterAPIKey() {
		name, _ := masterAPIKeyRef(oc)
		names = append(names, name)
	}
	return names
}

func engineConfigName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-engine-config" }

// Keys of the engine ConfigMap's OramaCore config templates. The combined config is for an image
// that runs both OramaCore sides; the writer and reader configs are for side-only images (#308).
const (
	combinedConfigKey = "config.yaml"
	writerConfigKey   = "writer.yaml"
	readerConfigKey   = "reader.yaml"
)

// engineSide is what a writer or reader pod takes from the engine's image selection: the OramaCore
// image it runs, and the ConfigMap key and unrendered template of the config it renders. The
// ConfigMap, the render-config init container and the config hash all read it from here.
type engineSide struct {
	image     string
	configKey string
	template  string
}

// readsMasterAPIKey reports whether the side's template holds the master key placeholder: the
// writer's and the combined config do (writer_side.master_api_key), the reader-only config does not.
func (s engineSide) readsMasterAPIKey() bool {
	return strings.Contains(s.template, keyPlaceholder(masterAPIKeyEnvName))
}

// engineSideFor resolves the "writer" or "reader" side. With side-only images each side renders its
// own config; otherwise both run the combined image and render the combined config.
func engineSideFor(oc *oramav1alpha1.OramaCloudV1, side string) engineSide {
	if !engineSidesSeparated(oc) {
		return engineSide{image: oramacoreImage(oc), configKey: combinedConfigKey, template: oramacoreConfigYAML(oc)}
	}
	if side == "writer" {
		return engineSide{image: oramacoreSideImage(oc, side), configKey: writerConfigKey, template: oramacoreWriterConfigYAML(oc)}
	}
	return engineSide{image: oramacoreSideImage(oc, side), configKey: readerConfigKey, template: oramacoreReaderConfigYAML(oc)}
}

// engineSidesSeparated reports whether the writer and readers run side-only OramaCore images
// (spec.images.overrides oramacore-writer and oramacore-reader). The combined oramacore image starts
// both sides in every pod: each reader also runs a writer, and the writer builds a reader index.
func engineSidesSeparated(oc *oramav1alpha1.OramaCloudV1) bool {
	o := oc.Spec.Images.Overrides
	return o["oramacore-writer"] != "" && o["oramacore-reader"] != ""
}

// validateEngineImages refuses a single side-only image: the other side would keep the combined
// image and config, so the stack would be neither separated nor as before. A disabled engine is not
// checked, so a leftover override cannot block its teardown.
func validateEngineImages(oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.Engine.Enabled {
		return nil
	}
	o := oc.Spec.Images.Overrides
	if (o["oramacore-writer"] == "") != (o["oramacore-reader"] == "") {
		return fmt.Errorf("spec.images.overrides needs both oramacore-writer and oramacore-reader, or neither")
	}
	return nil
}

// reconcileEngineConfig owns <cr>-engine-config: envoy.yaml plus either the combined config.yaml or
// the side-only writer.yaml and reader.yaml. Data is replaced whole, so switching modes drops the
// keys of the other mode.
func (r *OramaCloudV1Reconciler) reconcileEngineConfig(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: engineConfigName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(oc, "engine")
		cm.Data = map[string]string{"envoy.yaml": envoyConfig}
		for _, side := range []string{"writer", "reader"} {
			s := engineSideFor(oc, side)
			cm.Data[s.configKey] = s.template
		}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	})
	return err
}

// oramacoreConfigYAML renders the combined OramaCore config (http / log / writer_side / reader_side /
// ai_server, matching oramacore/config-docker.yaml) for an image that runs both sides. The writer
// replicates to the readers over a RabbitMQ stream on the <cr>-broker Service, and both sides reach
// the shared <cr>-embeddings ai-server.
//
// Every engine config is a template. master_api_key is a __MASTER_API_KEY__ placeholder (this string
// lives in a ConfigMap), the broker user and password __BROKER_USERNAME__ and __BROKER_PASSWORD__,
// and each LLM provider's key a __<PROVIDER>_API_KEY__ one: the render-config init container
// substitutes the real values at pod start, so no credential ever sits in the ConfigMap. OramaCore
// can't take them from env — config's ORAMACORE_ override splits on "_" and can't reach the nested
// writer_side.master_api_key.
func oramacoreConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return engineCommonYAML() + writerSideYAML(oc) + readerSideYAML(oc) + aiServerYAML(oc)
}

// oramacoreWriterConfigYAML is the writer-only image's config: no reader_side.
func oramacoreWriterConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return engineCommonYAML() + writerSideYAML(oc) + aiServerYAML(oc)
}

// oramacoreReaderConfigYAML is the reader-only image's config: no writer_side.
func oramacoreReaderConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return engineCommonYAML() + readerSideYAML(oc) + aiServerYAML(oc)
}

// engineCommonYAML is the http and log sections every OramaCore config carries.
func engineCommonYAML() string {
	return `http:
  host: 0.0.0.0
  port: 8080
  allow_cors: true
  with_prometheus: true
log: {}
`
}

// writerSideYAML is the writer_side section. The writer publishes to the RabbitMQ stream
// oramacore-operations on the broker Service :5552 (#41); OramaCore creates the stream on connect and
// the broker advertises the Service host so reconnect works. Both sides authenticate as the broker
// admin user from <cr>-broker-admin (#307; per-service users are #99). Its hooks block is hooksYAML,
// the same as the readers'. With engine.writer.jwt set it ends with the dashboard JWKS provider
// (writerJWTYAML), so the combined and writer-only configs carry it and the reader-only config does not.
func writerSideYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return fmt.Sprintf(`writer_side:
  output:
    type: rabbitmq
    host: %s
    port: 5552
    user: '__BROKER_USERNAME__'
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-writer
    producer_name: write
%s  master_api_key: '__MASTER_API_KEY__'
  config:
    data_dir: /app/.data/writer
    embedding_queue_limit: 50000
    insert_batch_commit_size: 50000000
    default_embedding_model: BGESmall
    javascript_queue_limit: 500000
    commit_interval: 1m
`, oc.Name+"-broker", hooksYAML(oc)) + writerJWTYAML(oc)
}

// readerSideYAML is the reader_side section: the readers consume the writer's stream from the same
// broker Service, and run customer hooks under the same hooksYAML block as the writer.
func readerSideYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return fmt.Sprintf(`reader_side:
  input:
    type: rabbitmq
    host: %s
    port: 5552
    user: '__BROKER_USERNAME__'
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-reader
    consumer_name: reader
%s  config:
    data_dir: /app/.data/reader
    insert_batch_commit_size: 50000000
    commit_interval: 1m
`, oc.Name+"-broker", hooksYAML(oc))
}

// aiServerYAML is the ai_server section both sides share. The embeddings ai-server is the SHARED
// <cr>-embeddings Deployment (#14), reached by its Service DNS name — not a per-pod 127.0.0.1
// sidecar — so the model loads into RAM once, not once per engine pod. The LLM settings come from
// spec.llm (engine_llm.go).
func aiServerYAML(oc *oramav1alpha1.OramaCloudV1) string {
	ep := "CPUExecutionProvider"
	if oc.Spec.Components.Engine.Writer.GPU {
		ep = "CUDAExecutionProvider"
	}
	return fmt.Sprintf(`ai_server:
  scheme: http
  host: %s
  port: 50051
  api_key: ""
  max_connections: 15
  total_threads: 12
  embeddings:
    default_model_group: small
    dynamically_load_models: false
    execution_providers:
      - %s
    total_threads: 8
`, oc.Name+"-embeddings", ep) + llmSelectorYAML(oc) + llmYAML(oc)
}

// embeddingsContainer is the OramaCore ai-server (gRPC embeddings on :50051). It runs as ONE
// shared Deployment (reconcileEmbeddings, #14) that the writer + all readers reach over its
// Service — matching OramaCore's reference architecture (config-docker.yaml points
// ai_server.host at a standalone `python-ai-server`), not a per-pod sidecar.
func embeddingsContainer(oc *oramav1alpha1.OramaCloudV1) corev1.Container {
	return corev1.Container{
		Name:  "embeddings",
		Image: embeddingsImage(oc),
		Ports: []corev1.ContainerPort{{Name: "grpc", ContainerPort: 50051}},
		// The ai-server preloads the embedding model(s) into RAM, so it's one of the heavy ones the
		// audit (#18) warns NOT to blanket-OOM — a generous memory limit + a real request so the
		// scheduler sees it. Deeper embeddings-memory tuning is #14; on a GPU writer the GPU limit is
		// added on top in reconcileEmbeddings.
		Resources:       *resourceReqs("512Mi", "250m", "4Gi"),
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
	}
}

// reconcileEmbeddings runs the ai-server as a single SHARED Deployment + Service instead of a
// sidecar in every engine pod (#14): the embedding model loads into RAM once, not once per
// writer/reader. The GPU (when the engine is GPU) belongs HERE — the model inference is the GPU
// consumer, OramaCore itself is CPU — so writer.gpu provisions this Deployment on the GPU node
// and the writer/reader pods stay CPU-only.
func (r *OramaCloudV1Reconciler) reconcileEmbeddings(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-embeddings"
	lbls := labelsFor(oc, "embeddings")
	w := oc.Spec.Components.Engine.Writer

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(1)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := embeddingsContainer(oc)
		var nodeSelector map[string]string
		var tolerations []corev1.Toleration
		if w.GPU {
			if c.Resources.Limits == nil {
				c.Resources.Limits = corev1.ResourceList{}
			}
			c.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse("1")
			// Pin to the GPU node the writer would otherwise have used.
			nodeSelector = w.NodeSelector
			tolerations = w.Tolerations
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				ImagePullSecrets: imagePullSecrets(oc),
				NodeSelector:     nodeSelector,
				Tolerations:      tolerations,
				Containers:       []corev1.Container{c},
			},
		}
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	return r.upsertService(ctx, oc, name, "embeddings",
		[]corev1.ServicePort{{Name: "grpc", Port: 50051, TargetPort: intstr.FromInt32(50051)}})
}

// waitForBrokerInit blocks OramaCore startup until the broker's stream listener (:5552) is up.
// OramaCore's RabbitMQ *producer* does NOT retry — it exits 1 with "Cannot create RabbitMQ sender:
// Connection refused" if the broker isn't ready — so gate on it instead of crash-looping (#41).
func waitForBrokerInit(oc *oramav1alpha1.OramaCloudV1) corev1.Container {
	broker := oc.Name + "-broker"
	return corev1.Container{
		Name:            "wait-for-broker",
		Image:           "busybox:1.37",
		Command:         []string{"sh", "-c", "until nc -z " + broker + " 5552; do echo 'waiting for broker stream :5552'; sleep 2; done"},
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
	}
}

// renderConfigInit fills the key placeholders of the pod's template (side.configKey) and writes the
// rendered file to the shared rendered-config emptyDir that the OramaCore container reads via
// CONFIG_PATH, so the credentials stay out of the ConfigMap while the reviewable config lives in one.
// Explicit env entries, which take precedence over envFrom, carry MASTER_API_KEY from
// spec.masterAPIKeySecretRef where the side reads it, the broker credentials from
// <cr>-broker-admin, and a provider's key from its apiKeySecretRef; the other provider keys come
// from the engine Secret (envFrom). See renderConfigScript and llmKeySecretEnv.
func renderConfigInit(oc *oramav1alpha1.OramaCloudV1, side engineSide) corev1.Container {
	var env []corev1.EnvVar
	if side.readsMasterAPIKey() {
		env = append(env, masterAPIKeyEnv(oc))
	}
	env = append(env, brokerCredentialEnv(oc)...)
	return corev1.Container{
		Name:    "render-config",
		Image:   "busybox:1.37",
		Command: []string{"sh", "-c", renderConfigScript(oc, side)},
		Env:     append(env, llmKeySecretEnv(oc)...),
		EnvFrom: envFromFor(oc, "engine"),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/tmpl"},
			{Name: "rendered-config", MountPath: "/etc/oramacore"},
		},
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
	}
}

// renderedConfigVolume is the emptyDir the init container writes the rendered config.yaml
// into and the OramaCore container reads from (CONFIG_PATH=/etc/oramacore/config.yaml).
func renderedConfigVolume() corev1.Volume {
	return corev1.Volume{Name: "rendered-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// reconcileWriter owns the writer StatefulSet, Service and PDB. llmSecretHash and credentialsHash
// (engineLLMSecretHash, credentialsHash) are resolved before any engine object is written.
func (r *OramaCloudV1Reconciler) reconcileWriter(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, llmSecretHash, credentialsHash string) error {
	name := oc.Name + "-writer"
	lbls := labelsFor(oc, "writer")
	w := oc.Spec.Components.Engine.Writer
	side := engineSideFor(oc, "writer")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.ServiceName = name
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc, "writer", writerStartupTimeout)
		if w.Resources != nil {
			c.Resources = *w.Resources
		} else {
			// Right-size (#42): default caps so the writer can't consume the node. OramaCore's
			// Rust/tokio runtime is cgroup-aware — it sizes its worker threads to this CPU limit and
			// backs off under the memory limit — so the container limit IS the binary-aware lever
			// (unlike ClickHouse, which ignores the cgroup for its caches). The config's
			// embedding_queue_limit / insert_batch_commit_size bound in-flight indexing memory on
			// top. Override via spec.components.engine.writer.resources.
			c.Resources = *resourceCaps("4Gi", "2", "1Gi", "500m")
		}
		// The GPU belongs to the shared embeddings Deployment (#14), not the writer — OramaCore
		// is CPU; the embedding model is the GPU consumer (see reconcileEmbeddings).
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: engineConfigAnnotations(side, "", llmSecretHash, credentialsHash)},
			Spec: corev1.PodSpec{
				ImagePullSecrets:              imagePullSecrets(oc),
				NodeSelector:                  w.NodeSelector,
				Tolerations:                   w.Tolerations,
				InitContainers:                []corev1.Container{waitForBrokerInit(oc), renderConfigInit(oc, side)},
				Containers:                    []corev1.Container{c},
				Volumes:                       []corev1.Volume{configVolume(oc), renderedConfigVolume()},
				TerminationGracePeriodSeconds: int64Ptr(writerGracePeriodSeconds),
			},
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataPVC()}
		return controllerutil.SetControllerReference(oc, sts, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.upsertService(ctx, oc, name, "writer",
		[]corev1.ServicePort{{Name: "http", Port: oramacorePort, TargetPort: intstr.FromInt32(oramacorePort)}}); err != nil {
		return err
	}
	// The writer is a singleton: block voluntary disruption (drains/autoscaler) so it
	// isn't casually evicted. Node maintenance must consciously handle it (its
	// graceful shutdown persists data, but availability drops during any move).
	return r.upsertPDB(ctx, oc, name, "writer", 0)
}

// reconcileReaders owns the reader Deployment, Service and PDB. llmSecretHash and credentialsHash
// (engineLLMSecretHash, credentialsHash) are resolved before any engine object is written.
// envoyBootstrap is the Envoy bootstrap the engine ConfigMap carries (envoyConfig); with Envoy
// enabled it is part of the readers' config hash, so a new bootstrap rolls them.
func (r *OramaCloudV1Reconciler) reconcileReaders(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, llmSecretHash, credentialsHash, envoyBootstrap string) error {
	name := oc.Name + "-reader"
	lbls := labelsFor(oc, "reader")
	rd := oc.Spec.Components.Engine.Reader
	replicas := rd.Replicas
	if replicas == 0 {
		replicas = 3
	}
	useEnvoy := oc.Spec.Components.Engine.Envoy
	side := engineSideFor(oc, "reader")
	if !useEnvoy {
		envoyBootstrap = ""
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(replicas)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc, "reader", readerStartupTimeout)
		if rd.Resources != nil {
			c.Resources = *rd.Resources
		} else {
			// Right-size (#42): readers are query-serving; cgroup-aware tokio sizes threads to this
			// CPU limit. Override via spec.components.engine.reader.resources.
			c.Resources = *resourceCaps("2Gi", "1", "512Mi", "250m")
		}
		// Readers reach the shared embeddings Deployment over its Service (#14) — no per-pod
		// sidecar, so N readers no longer each load the model into RAM.
		containers := []corev1.Container{c}
		if useEnvoy {
			containers = append(containers, envoySidecar())
		}
		podSpec := corev1.PodSpec{
			ImagePullSecrets:              imagePullSecrets(oc),
			InitContainers:                []corev1.Container{waitForBrokerInit(oc), renderConfigInit(oc, side)},
			Containers:                    containers,
			Volumes:                       []corev1.Volume{configVolume(oc), renderedConfigVolume(), dataEmptyDir()},
			TerminationGracePeriodSeconds: int64Ptr(readerGracePeriodSeconds),
		}
		// Spread readers across nodes when there's more than one (soft — a single-node sandbox still
		// schedules; a real cluster honors it). #18.
		if replicas > 1 {
			podSpec.TopologySpreadConstraints = spreadConstraints(lbls)
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: engineConfigAnnotations(side, envoyBootstrap, llmSecretHash, credentialsHash)},
			Spec:       podSpec,
		}
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	target := int32(oramacorePort)
	if useEnvoy {
		target = envoyPort
	}
	if err := r.upsertService(ctx, oc, name, "reader",
		[]corev1.ServicePort{{Name: "http", Port: envoyPort, TargetPort: intstr.FromInt32(target)}}); err != nil {
		return err
	}
	// Readers can lose one at a time during disruptions (keeps the rest serving).
	return r.upsertPDB(ctx, oc, name, "reader", 1)
}

// ---- shared builders ----

const (
	// Prod 1's startup allowances, per Steve's review of #406: writer 20 min, readers 40 min.
	writerStartupTimeout             = 20 * time.Minute
	readerStartupTimeout             = 40 * time.Minute
	engineStartupPeriodSeconds int32 = 10
)

// engineStartupProbe gates readiness/liveness until startup succeeds or exhausts its budget.
// The budget must be a whole number of periods; the startup tests check the resulting window.
func engineStartupProbe(timeout time.Duration) *corev1.Probe {
	threshold := timeout / (time.Duration(engineStartupPeriodSeconds) * time.Second)
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromInt32(oramacorePort),
		}},
		PeriodSeconds:    engineStartupPeriodSeconds,
		FailureThreshold: int32(threshold),
	}
}

// oramacoreContainer is the OramaCore container of the "writer" or "reader" pod, running that side's
// image (engineSideFor), with that side's startup budget.
func oramacoreContainer(oc *oramav1alpha1.OramaCloudV1, side string, startupTimeout time.Duration) corev1.Container {
	// OramaCore serves GET /health at root (oramacore/src/web_server/api/mod.rs;
	// also the prod ALB/Envoy health path).
	health := func() *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromInt32(oramacorePort)}}}
	}
	readiness := health()
	readiness.PeriodSeconds, readiness.TimeoutSeconds, readiness.FailureThreshold = 10, 3, 3
	liveness := health()
	liveness.PeriodSeconds, liveness.TimeoutSeconds, liveness.FailureThreshold = 15, 3, 3

	return corev1.Container{
		Name:  "oramacore",
		Image: engineSideFor(oc, side).image,
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: oramacorePort}},
		Env: []corev1.EnvVar{
			{Name: "RUST_LOG", Value: "oramacore=info,warn"},
			// Read the init-container-rendered config (with the real credentials) rather than
			// the raw ConfigMap template.
			{Name: "CONFIG_PATH", Value: "/etc/oramacore/config.yaml"},
		},
		EnvFrom:         envFromFor(oc, "engine"),
		StartupProbe:    engineStartupProbe(startupTimeout),
		ReadinessProbe:  readiness,
		LivenessProbe:   liveness,
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
		VolumeMounts: []corev1.VolumeMount{
			{Name: "rendered-config", MountPath: "/etc/oramacore"},
			{Name: "data", MountPath: "/app/.data"},
		},
	}
}

// engineConfigAnnotations is the side's pod-template annotations. The init container renders the
// config only at pod start, so a change to what it renders must roll the pod:
//   - orama.cloud/config-hash is the SHA-256 of the side's unrendered template (placeholders, not
//     keys). With side-only images a change to one side's template rolls only that side; the
//     combined config is hashed the same way, and rolls both. envoyBootstrap is the Envoy sidecar's
//     bootstrap, or "" for a pod without Envoy (the writer, readers with Envoy disabled), whose hash
//     stays the template's alone. Readers with Envoy hash the template and the bootstrap together
//     (sha256Hex), so an Envoy-only change rolls them and not the writer.
//   - orama.cloud/llm-secret-hash is engineLLMSecretHash, identical on both sides, so a rotated key
//     in the engine Secret or an apiKeySecretRef Secret rolls the writer and readers.
//   - orama.cloud/credentials-hash is the side's credentialsHash: <cr>-broker-admin, and the master
//     API key Secret where the side reads it (engineCredentialSecrets).
func engineConfigAnnotations(s engineSide, envoyBootstrap, llmSecretHash, credentialsHash string) map[string]string {
	sum := sha256.Sum256([]byte(s.template))
	hash := hex.EncodeToString(sum[:])
	if envoyBootstrap != "" {
		hash = sha256Hex([]byte(s.template), []byte(envoyBootstrap))
	}
	return map[string]string{
		configHashKey:      hash,
		llmSecretHashKey:   llmSecretHash,
		credentialsHashKey: credentialsHash,
	}
}

// configHashKey is the pod-template annotation holding the hash of the config templates a pod's
// render-config init container renders.
const configHashKey = "orama.cloud/config-hash"

func envoySidecar() corev1.Container {
	return corev1.Container{
		Name:  "envoy",
		Image: envoyImage,
		// Run envoy directly, bypassing the image's /docker-entrypoint.sh. Under the non-root hardened
		// securityContext (#18) that entrypoint's `chown /dev/stdout` fails ("Operation not permitted")
		// and the container exits 1 (the reader then never reaches 2/2). envoy needs no root; -c points
		// at the mounted bootstrap — the same argv the entrypoint would exec, just without the chown.
		Command: []string{"envoy", "-c", "/etc/envoy/envoy.yaml"},
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: envoyPort},
			{Name: "admin", ContainerPort: envoyAdminPort},
		},
		// Envoy binds :80 (privileged) — needs NET_BIND_SERVICE back after drop-ALL (#18).
		SecurityContext: hardenedSecurityContextNetBind(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/etc/envoy/envoy.yaml", SubPath: "envoy.yaml"},
		},
	}
}

func configVolume(oc *oramav1alpha1.OramaCloudV1) corev1.Volume {
	return corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: engineConfigName(oc)},
			},
		},
	}
}

func dataEmptyDir() corev1.Volume {
	return corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func dataPVC() corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
}

func (r *OramaCloudV1Reconciler) upsertService(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component string, ports []corev1.ServicePort) error {
	lbls := labelsFor(oc, component)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = lbls
		svc.Spec.Selector = lbls
		svc.Spec.Ports = ports
		return controllerutil.SetControllerReference(oc, svc, r.Scheme)
	})
	return err
}

func (r *OramaCloudV1Reconciler) upsertPDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component string, maxUnavailable int) error {
	lbls := labelsFor(oc, component)
	mu := intstr.FromInt(maxUnavailable)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = lbls
		pdb.Spec.MaxUnavailable = &mu
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		return controllerutil.SetControllerReference(oc, pdb, r.Scheme)
	})
	return err
}
