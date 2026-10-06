/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
// the local OramaCore on 127.0.0.1:8080 (see techops-docs Envoy topology).
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
		return r.teardownByComponent(ctx, oc, "engine", "embeddings", "writer", "reader")
	}
	if _, err := r.ensureComponentSecret(ctx, oc, "engine"); err != nil {
		return fmt.Errorf("engine secret: %w", err)
	}
	if err := r.reconcileEngineConfig(ctx, oc); err != nil {
		return fmt.Errorf("engine config: %w", err)
	}
	if err := r.reconcileEmbeddings(ctx, oc); err != nil {
		return fmt.Errorf("embeddings: %w", err)
	}
	if oc.Spec.Region.Role == "primary" {
		if err := r.reconcileWriter(ctx, oc); err != nil {
			return fmt.Errorf("writer: %w", err)
		}
	}
	if err := r.reconcileReaders(ctx, oc); err != nil {
		return fmt.Errorf("readers: %w", err)
	}
	return nil
}

func engineConfigName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-engine-config" }

func (r *OramaCloudV1Reconciler) reconcileEngineConfig(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: engineConfigName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(oc, "engine")
		cm.Data = map[string]string{
			"envoy.yaml":  envoyConfig,
			"config.yaml": oramacoreConfigYAML(oc),
		}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	})
	return err
}

// oramacoreConfigYAML renders a VALID OramaCore config (http / writer_side /
// reader_side / ai_server) matching oramacore/config-docker.yaml. The embeddings
// ai-server is a co-located sidecar at 127.0.0.1:50051. writer/reader use in-memory
// transport (single-node); RabbitMQ replication is a follow-up.
//
// master_api_key is a __MASTER_API_KEY__ placeholder here (this string lives in a
// ConfigMap): the render-config init container substitutes the real value from the
// engine Secret at pod start, so no credential ever sits in the ConfigMap. OramaCore
// can't take it from env — config's ORAMACORE_ override splits on "_" and can't reach
// the nested writer_side.master_api_key.
//
// reader_side.analytics (#55) turns on OramaCore's search-event log: without it the reader
// records nothing and GET /v1/analytics answers 500, so analytics-processor imports zero rows.
// Its api_key is the same __MASTER_API_KEY__ placeholder, because that is the key the
// processor polls with (analyticsProcessorConfigYAML). metadata_from_headers mirrors the prod
// reader configs: the Cloudflare request headers become per-event metadata.
func oramacoreConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	ep := "CPUExecutionProvider"
	if oc.Spec.Components.Engine.Writer.GPU {
		ep = "CUDAExecutionProvider"
	}
	// The embeddings ai-server is a SHARED Deployment (#14), reached by its Service DNS name —
	// not a per-pod 127.0.0.1 sidecar — so the model loads into RAM once, not once per engine pod.
	embHost := oc.Name + "-embeddings"
	// writer→reader replication over a RabbitMQ stream (#41): the broker Service on 5552. OramaCore
	// creates the stream on connect; the broker advertises the Service host so reconnect works.
	brokerHost := oc.Name + "-broker"
	return fmt.Sprintf(`http:
  host: 0.0.0.0
  port: 8080
  allow_cors: true
  with_prometheus: true
log: {}
writer_side:
  output:
    type: rabbitmq
    host: %s
    port: 5552
    user: orama
    password: dev-rabbitmq-password
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-writer
    producer_name: write
  hooks:
    select_embeddings_properties:
      check_interval: 60s
      max_idle_time: 20s
      instances_count_per_code: 3
      queue_capacity: 100
      max_execution_time: 1s
      max_startup_time: 100ms
  master_api_key: "__MASTER_API_KEY__"
  config:
    data_dir: /app/.data/writer
    embedding_queue_limit: 50000
    insert_batch_commit_size: 50000000
    default_embedding_model: BGESmall
    javascript_queue_limit: 500000
    commit_interval: 1m
reader_side:
  input:
    type: rabbitmq
    host: %s
    port: 5552
    user: orama
    password: dev-rabbitmq-password
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-reader
    consumer_name: reader
  config:
    data_dir: /app/.data/reader
    insert_batch_commit_size: 50000000
    commit_interval: 1m
  analytics:
    api_key: "__MASTER_API_KEY__"
    metadata_from_headers:
      - header: user-agent
        metadata_key: ua
      - header: CF-Connecting-IP
        metadata_key: ip
      - header: CF-IPCountry
        metadata_key: country
      - header: continent
        metadata_key: continent
      - header: colo
        metadata_key: colo
      - header: latitude
        metadata_key: latitude
      - header: longitude
        metadata_key: longitude
ai_server:
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
  llm:
    local: false
    port: 8000
    host: 127.0.0.1
    model: "Qwen/Qwen2.5-3B-Instruct"
  remote_llms:
    - provider: groq
      api_key: "__GROQ_API_KEY__"
      default_model: llama-3.3-70b-versatile
`, brokerHost, brokerHost, embHost, ep)
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

// renderConfigInit substitutes the MASTER_API_KEY (from the engine Secret, via envFrom;
// both writer_side.master_api_key and reader_side.analytics.api_key) into the config.yaml template (from the ConfigMap) and writes the rendered file to the
// shared rendered-config emptyDir that the OramaCore container reads via CONFIG_PATH.
// This keeps the credential out of the ConfigMap while leaving the reviewable config in
// one. (The key must be sed-safe — no '#', '&', '\' — which holds for base64url/hex API
// keys; the operator's dev default and prod overrides both satisfy this.)
func renderConfigInit(oc *oramav1alpha1.OramaCloudV1) corev1.Container {
	return corev1.Container{
		Name:    "render-config",
		Image:   "busybox:1.37",
		Command: []string{"sh", "-c", `sed -e "s#__MASTER_API_KEY__#${MASTER_API_KEY}#g" -e "s#__GROQ_API_KEY__#${GROQ_API_KEY}#g" /tmpl/config.yaml > /etc/oramacore/config.yaml`},
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

func (r *OramaCloudV1Reconciler) reconcileWriter(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-writer"
	lbls := labelsFor(oc, "writer")
	w := oc.Spec.Components.Engine.Writer

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.ServiceName = name
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc)
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
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				ImagePullSecrets:              imagePullSecrets(oc),
				NodeSelector:                  w.NodeSelector,
				Tolerations:                   w.Tolerations,
				InitContainers:                []corev1.Container{waitForBrokerInit(oc), renderConfigInit(oc)},
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

func (r *OramaCloudV1Reconciler) reconcileReaders(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-reader"
	lbls := labelsFor(oc, "reader")
	rd := oc.Spec.Components.Engine.Reader
	replicas := rd.Replicas
	if replicas == 0 {
		replicas = 3
	}
	useEnvoy := oc.Spec.Components.Engine.Envoy

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(replicas)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc)
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
			InitContainers:                []corev1.Container{waitForBrokerInit(oc), renderConfigInit(oc)},
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
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
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

func oramacoreContainer(oc *oramav1alpha1.OramaCloudV1) corev1.Container {
	// OramaCore serves GET /health at root (oramacore/src/web_server/api/mod.rs;
	// also the prod ALB/Envoy health path).
	health := func() *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromInt32(oramacorePort)}}}
	}
	// startup gates liveness/readiness so a slow index-load boot isn't killed.
	startup := health()
	startup.PeriodSeconds, startup.FailureThreshold = 10, 30 // ~5 min to become live
	readiness := health()
	readiness.PeriodSeconds, readiness.TimeoutSeconds, readiness.FailureThreshold = 10, 3, 3
	liveness := health()
	liveness.PeriodSeconds, liveness.TimeoutSeconds, liveness.FailureThreshold = 15, 3, 3

	return corev1.Container{
		Name:  "oramacore",
		Image: oramacoreImage(oc),
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: oramacorePort}},
		Env: []corev1.EnvVar{
			{Name: "RUST_LOG", Value: "oramacore=info,warn"},
			// Read the init-container-rendered config (with the real master_api_key)
			// rather than the raw ConfigMap template.
			{Name: "CONFIG_PATH", Value: "/etc/oramacore/config.yaml"},
		},
		EnvFrom:         envFromFor(oc, "engine"),
		StartupProbe:    startup,
		ReadinessProbe:  readiness,
		LivenessProbe:   liveness,
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
		VolumeMounts: []corev1.VolumeMount{
			{Name: "rendered-config", MountPath: "/etc/oramacore"},
			{Name: "data", MountPath: "/app/.data"},
		},
	}
}

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
