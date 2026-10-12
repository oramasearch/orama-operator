/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
	if jwt := oc.Spec.Components.Engine.Writer.JWT; jwt != nil {
		u, err := url.Parse(jwt.JWKSURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || jwt.Audience == "" {
			return fmt.Errorf("engine.writer.jwt requires an HTTP(S) jwksURL without credentials and a nonempty audience")
		}
	}
	if err := validateEngineImages(oc); err != nil {
		return err
	}
	if err := validateLLM(oc); err != nil {
		return err
	}
	if _, err := r.ensureComponentSecret(ctx, oc, "engine"); err != nil {
		return fmt.Errorf("engine secret: %w", err)
	}
	if err := r.reconcileEngineConfig(ctx, oc); err != nil {
		return fmt.Errorf("engine config: %w", err)
	}
	if err := r.retireEmbeddings(ctx, oc); err != nil {
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
	return r.reconcileEngineIngresses(ctx, oc)
}

func engineConfigName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-engine-config" }

// Keys of the engine ConfigMap's OramaCore config templates. combinedConfigKey is the config of an
// image that runs both OramaCore sides; writerConfigKey and readerConfigKey are the side-only
// configs used when the writer and readers run side-only images (#308).
const (
	combinedConfigKey = "config.yaml"
	writerConfigKey   = "writer.yaml"
	readerConfigKey   = "reader.yaml"

	// podNamePlaceholder in a side-only template becomes the pod's own name: the render-config
	// init container substitutes it from the downward API (POD_NAME).
	podNamePlaceholder = "__POD_NAME__"
)

// engineSide is what the writer and the reader pods differ in: the OramaCore image they run, the
// config template (and its ConfigMap key) their render-config init container renders, and how long
// their startup probe waits for OramaCore to answer.
type engineSide struct {
	image          string
	configKey      string
	template       string
	startupTimeout time.Duration
}

// Engine startup and preflight defaults, matching Prod 1: its reader unit allows 20 minutes for the
// port to open and 40 for the unit to start (orama-deploy systemd units), its units set
// LimitNOFILE=65535, and its reader hosts set vm.max_map_count=4194304.
const (
	defaultWriterStartupTimeout = 20 * time.Minute
	defaultReaderStartupTimeout = 40 * time.Minute
	engineStartupPeriodSeconds  = 10
	defaultMinMaxMapCount       = 4194304
	defaultMinOpenFiles         = 65535
)

// engineSideFor resolves the writer ("writer") or reader ("reader") side. With side-only images
// (engineSidesSeparated) each side renders its own config; otherwise both render today's combined
// config, whose image also starts the other side.
func engineSideFor(oc *oramav1alpha1.OramaCloudV1, side string) engineSide {
	s := engineSide{image: oramacoreSideImage(oc, side), configKey: combinedConfigKey, template: oramacoreConfigYAML(oc)}
	s.startupTimeout = durationOr(oc.Spec.Components.Engine.Reader.StartupTimeout, defaultReaderStartupTimeout)
	if side == "writer" {
		s.startupTimeout = durationOr(oc.Spec.Components.Engine.Writer.StartupTimeout, defaultWriterStartupTimeout)
	}
	if !engineSidesSeparated(oc) {
		return s
	}
	if side == "writer" {
		s.configKey, s.template = writerConfigKey, oramacoreWriterConfigYAML(oc)
	} else {
		s.configKey, s.template = readerConfigKey, oramacoreReaderConfigYAML(oc)
	}
	return s
}

// engineSidesSeparated reports whether the writer and readers run side-only OramaCore images
// (spec.images.overrides oramacore-writer and oramacore-reader, #308). OramaCore built with both
// Cargo features starts every compiled-in side, so with the shared oramacore image each reader
// also runs a writer and the writer also builds a reader index.
func engineSidesSeparated(oc *oramav1alpha1.OramaCloudV1) bool {
	o := oc.Spec.Images.Overrides
	return o["oramacore-writer"] != "" && o["oramacore-reader"] != ""
}

// validateEngineImages refuses a single side-only image: the other side would keep the combined
// image and config, so the stack would be neither separated nor as before.
func validateEngineImages(oc *oramav1alpha1.OramaCloudV1) error {
	o := oc.Spec.Images.Overrides
	if (o["oramacore-writer"] == "") != (o["oramacore-reader"] == "") {
		return fmt.Errorf("spec.images.overrides needs both oramacore-writer and oramacore-reader, or neither")
	}
	return nil
}

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

// streamClient names an engine pod's connection to the oramacore-operations stream. Both names are
// labels only (client_provided_name and the producer/consumer name): a reader's stream position is
// the offset kept under its data_dir, so a new name never replays the stream.
type streamClient struct{ connection, endpoint string }

// The combined config keeps the names it always had, so existing stacks render the same config.
var (
	combinedWriterClient = streamClient{connection: "oramacore-writer", endpoint: "write"}
	combinedReaderClient = streamClient{connection: "oramacore-reader", endpoint: "reader"}
)

// oramacoreConfigYAML renders the combined OramaCore config (http / log / writer_side / reader_side /
// ai_server, matching oramacore/config-docker.yaml) that an image with both sides needs: OramaCore
// requires both side sections when both are compiled in.
//
// Every config is a template. master_api_key, the LLM providers' api_key fields and the broker
// user and password are placeholders such as __MASTER_API_KEY__ (this string lives in a ConfigMap):
// the render-config init container substitutes the real values from Secrets at pod start, so no
// credential ever sits in the ConfigMap.
// OramaCore can't take them from env — config's ORAMACORE_ override splits on "_" and can't reach
// nested keys such as writer_side.master_api_key or a remote_llms entry.
func oramacoreConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	return engineCommonYAML() + writerSideYAML(oc, combinedWriterClient) + readerSideYAML(oc, combinedReaderClient) + aiServerYAML(oc, oc.Spec.Components.Engine.Writer.GPU)
}

// oramacoreWriterConfigYAML is the writer-only config (no reader_side). The writer is a singleton, so
// its stream connection is named <cr>-writer.
func oramacoreWriterConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	name := oc.Name + "-writer"
	return engineCommonYAML() + writerSideYAML(oc, streamClient{connection: name, endpoint: name}) + aiServerYAML(oc, oc.Spec.Components.Engine.Writer.GPU)
}

// oramacoreReaderConfigYAML is the reader-only config (no writer_side). Each reader names its stream
// connection after its pod (<cr>-reader-<ordinal>), so broker connections tell the readers apart.
// Readers never get a GPU, so their embeddings run on CPU only, as in Prod 1's reader config.
func oramacoreReaderConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	pod := streamClient{connection: podNamePlaceholder, endpoint: podNamePlaceholder}
	return engineCommonYAML() + readerSideYAML(oc, pod) + aiServerYAML(oc, false)
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

// writerSideYAML is the writer_side section. The writer replicates to the readers over the RabbitMQ
// stream oramacore-operations on the broker Service :5552 (#41); OramaCore creates the stream on
// connect and the broker advertises the Service host so reconnect works. Both sides authenticate as
// the broker admin user from <cr>-broker-admin (#307; per-service users are #99). Its hooks block is
// hooksYAML, the same as the reader side's (#306).
func writerSideYAML(oc *oramav1alpha1.OramaCloudV1, client streamClient) string {
	jwt := ""
	if cfg := oc.Spec.Components.Engine.Writer.JWT; cfg != nil {
		jwt = fmt.Sprintf("  jwt:\n    providers:\n      - name: dashboard\n        jwks_url: %q\n        issuers: [%q]\n        audiences: [%q]\n        refresh_interval: 1h\n", cfg.JWKSURL, jwtIssuer(oc), cfg.Audience)
	}
	return fmt.Sprintf(`writer_side:
  output:
    type: rabbitmq
    host: %s
    port: 5552
    user: '__BROKER_USERNAME__'
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: %s
    producer_name: %s
%s  master_api_key: '__MASTER_API_KEY__'
%s  config:
    data_dir: /app/.data/writer
    embedding_queue_limit: 50000
    insert_batch_commit_size: 50000000
    default_embedding_model: BGESmall
    javascript_queue_limit: 500000
    commit_interval: 1m
`, oc.Name+"-broker", client.connection, client.endpoint, hooksYAML(oc), jwt)
}

// readerSideYAML is the reader_side section. Each reader persists its index and the last stream
// offset it applied under config.data_dir, which sits on the reader's PVC (#141), so a replaced
// reader resumes from that offset instead of replaying the stream from 0.
//
// analytics (#55) turns on OramaCore's search-event log: without it the reader records nothing and
// GET /v1/analytics answers 500, so analytics-processor imports zero rows. Its api_key is the same
// __MASTER_API_KEY__ placeholder, because that is the key the processor polls with
// (analyticsProcessorConfigYAML). metadata_from_headers mirrors the prod reader configs: the
// Cloudflare request headers become per-event metadata. hooks is hooksYAML, as on the writer side.
func readerSideYAML(oc *oramav1alpha1.OramaCloudV1, client streamClient) string {
	return fmt.Sprintf(`reader_side:
  input:
    type: rabbitmq
    host: %s
    port: 5552
    user: '__BROKER_USERNAME__'
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: %s
    consumer_name: %s
%s  config:
    data_dir: /app/.data/reader
    insert_batch_commit_size: 50000000
    commit_interval: 1m
  analytics:
    api_key: '__MASTER_API_KEY__'
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
`, oc.Name+"-broker", client.connection, client.endpoint, hooksYAML(oc))
}

// engineModelsCacheDir is where OramaCore's in-process embeddings keep the models they download from
// Hugging Face: on the pod's data PVC, so a restart reuses them instead of downloading again (and a
// Hugging Face outage does not block a restart). OramaCore's default, /tmp/fastembed_cache, is lost
// with the container.
const engineModelsCacheDir = "/app/.data/models"

// aiServerYAML is the ai_server section. OramaCore computes embeddings in-process (fastembed/ONNX
// through its embedded Python); since 1.2 it reaches no embeddings server, so the section carries
// no host, port or scheme. gpu lists CUDAExecutionProvider before CPUExecutionProvider, as Prod 1's
// writer does, so a pod without a usable GPU falls back to CPU. The embeddings settings otherwise
// match Prod 1 (default_model_group small, models loaded on demand). The LLM settings (llm,
// remote_llms and the automatic embeddings selector) come from spec.llm (#304, engine_llm.go).
func aiServerYAML(oc *oramav1alpha1.OramaCloudV1, gpu bool) string {
	providers := "      - CPUExecutionProvider\n"
	if gpu {
		providers = "      - CUDAExecutionProvider\n" + providers
	}
	return fmt.Sprintf(`ai_server:
  models_cache_dir: %s
  total_threads: 12
  embeddings:
    default_model_group: small
    dynamically_load_models: true
    execution_providers:
%s    total_threads: 8
%s`, engineModelsCacheDir, providers, llmSelectorYAML(oc)+llmYAML(oc))
}

// retireEmbeddings deletes the <cr>-embeddings Deployment and Service that operators before #305 ran
// as a shared ai-server. OramaCore never used it (it embeds in-process and its ai_server config has
// no host), and on a GPU writer it held the GPU. Only objects this CR controls are deleted; a
// foreign object of the same name is left alone.
func (r *OramaCloudV1Reconciler) retireEmbeddings(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-embeddings"}
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		if err := r.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, oc) {
			continue
		}
		if err := r.deleteCloudV1Owned(ctx, oc, obj); err != nil {
			return err
		}
	}
	return nil
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

// durationOr is d when it is set and positive, otherwise def.
func durationOr(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

// startupFailureThreshold is the startup probe failureThreshold that gives OramaCore at least
// timeout to answer when the probe runs every periodSeconds.
func startupFailureThreshold(timeout time.Duration, periodSeconds int32) int32 {
	period := time.Duration(periodSeconds) * time.Second
	return int32(max(1, (timeout+period-1)/period))
}

// preflightInit checks the node and process limits OramaCore needs before the writer or a reader
// starts, failing with a message that names the setting: vm.max_map_count against
// spec.components.engine.minMaxMapCount (a reader keeps every mapping it opens until it restarts, and
// mmap fails with ENOMEM at the limit whatever the free memory) and the open-files limit against
// spec.components.engine.minOpenFiles. vm.max_map_count is not namespaced, so a pod can only check
// it; the node pool sets it (docs/install.md). The open-files limit comes from the container
// runtime, which gives every container in the pod the same one. A field set to 0 drops its check;
// with both at 0 there is no preflight container. It runs as nobody on a read-only root filesystem.
func preflightInit(oc *oramav1alpha1.OramaCloudV1) (corev1.Container, bool) {
	e := oc.Spec.Components.Engine
	var checks []string
	if n := int64Or(e.MinMaxMapCount, defaultMinMaxMapCount); n > 0 {
		v := strconv.FormatInt(n, 10)
		checks = append(checks, `m=$(cat /proc/sys/vm/max_map_count); if [ "$m" -lt `+v+` ]; then `+
			`echo "preflight: vm.max_map_count is $m, below the required `+v+`. Set it on the node pool `+
			`(AKS linuxOSConfig.sysctls.vmMaxMapCount), or set spec.components.engine.minMaxMapCount: 0 `+
			`to skip this check." >&2; fail=1; fi`)
	}
	if n := int64Or(e.MinOpenFiles, defaultMinOpenFiles); n > 0 {
		v := strconv.FormatInt(n, 10)
		checks = append(checks, `n=$(ulimit -n); if [ "$n" != unlimited ] && [ "$n" -lt `+v+` ]; then `+
			`echo "preflight: the open-files limit (ulimit -n) is $n, below the required `+v+`. Raise the `+
			`container runtime's LimitNOFILE on the node, or set spec.components.engine.minOpenFiles: 0 `+
			`to skip this check." >&2; fail=1; fi`)
	}
	if len(checks) == 0 {
		return corev1.Container{}, false
	}
	sc := hardenedSecurityContext() // workload hardening baseline (#18)
	sc.RunAsNonRoot, sc.RunAsUser, sc.ReadOnlyRootFilesystem = boolPtr(true), int64Ptr(65534), boolPtr(true)
	return corev1.Container{
		Name:            "preflight",
		Image:           "busybox:1.37",
		Command:         []string{"sh", "-c", "fail=0; " + strings.Join(checks, "; ") + "; exit $fail"},
		SecurityContext: sc,
	}, true
}

// int64Or is *p when p is set, otherwise def.
func int64Or(p *int64, def int64) int64 {
	if p == nil {
		return def
	}
	return *p
}

// engineInitContainers are the writer and reader init containers, in order: the broker wait, the
// preflight checks when any is enabled, then render-config.
func engineInitContainers(oc *oramav1alpha1.OramaCloudV1, s engineSide) []corev1.Container {
	inits := []corev1.Container{waitForBrokerInit(oc)}
	if c, ok := preflightInit(oc); ok {
		inits = append(inits, c)
	}
	return append(inits, renderConfigInit(oc, s))
}

// renderConfigInit substitutes the secret placeholders into the side's config template (the
// ConfigMap key s.configKey) and writes the rendered file to the shared rendered-config emptyDir that
// the OramaCore container reads via CONFIG_PATH: MASTER_API_KEY (writer_side.master_api_key and
// reader_side.analytics.api_key) from the master API key Secret, BROKER_USERNAME and BROKER_PASSWORD
// from <cr>-broker-admin, and each LLM provider's <PROVIDER>_API_KEY from the engine Secret via
// envFrom, or from a provider's apiKeySecretRef via an env entry that overrides it. This
// keeps the credentials out of the ConfigMap while leaving the reviewable config in one. Values may
// hold any character but a newline (renderConfigScript escapes them).
//
// A side-only template also carries __POD_NAME__, filled from the downward API so each reader names
// its stream connection after itself.
func renderConfigInit(oc *oramav1alpha1.OramaCloudV1, s engineSide) corev1.Container {
	var env []corev1.EnvVar
	if s.configKey != combinedConfigKey {
		env = []corev1.EnvVar{{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}
	}
	env = append(env, masterAPIKeyEnv(oc))
	env = append(env, brokerCredentialEnv(oc)...)
	env = append(env, llmKeySecretEnv(oc)...)
	return corev1.Container{
		Name:    "render-config",
		Image:   "busybox:1.37",
		Command: []string{"sh", "-c", renderConfigScript(oc, s)},
		Env:     env,
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
	side := engineSideFor(oc, "writer")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.ServiceName = name
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc, side)
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
		// OramaCore computes embeddings in-process, so the GPU belongs to the writer (#305).
		// Clone the limits so a spec-provided Resources is never mutated.
		if w.GPU {
			c.Resources.Limits = maps.Clone(c.Resources.Limits)
			if c.Resources.Limits == nil {
				c.Resources.Limits = corev1.ResourceList{}
			}
			c.Resources.Limits[resourceNvidiaGPU] = resource.MustParse("1")
		}
		nodeSelector, tolerations := writerPlacement(oc)
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: engineConfigAnnotations(side)},
			Spec: corev1.PodSpec{
				ImagePullSecrets:              imagePullSecrets(oc),
				NodeSelector:                  nodeSelector,
				Tolerations:                   tolerations,
				InitContainers:                engineInitContainers(oc, side),
				Containers:                    []corev1.Container{c},
				Volumes:                       []corev1.Volume{configVolume(oc), renderedConfigVolume()},
				TerminationGracePeriodSeconds: int64Ptr(writerGracePeriodSeconds),
			},
		}
		setVolumeClaimTemplates(sts, dataPVC(w.Storage))
		applyPVCRetention(oc, sts)
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

// reconcileReaders runs the readers as a StatefulSet with one `data` PVC per reader (#141). The
// reader keeps its index and the last replication-stream offset it applied on that volume, so a
// rescheduled or replaced reader resumes where it stopped. With the emptyDir readers used before,
// every new reader replayed the stream from offset 0, and once the stream outgrew its 5 GB
// retention (brokerOperationsStreamMaxBytes) a new reader silently served a partial index.
// Pods are managed in parallel (readers are independent) and spec.components.engine.reader.hpa,
// when set, scales the StatefulSet through an owned HorizontalPodAutoscaler.
func (r *OramaCloudV1Reconciler) reconcileReaders(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-reader"
	lbls := labelsFor(oc, "reader")
	rd := oc.Spec.Components.Engine.Reader
	replicas := rd.Replicas
	if replicas == 0 {
		replicas = 3
	}
	useEnvoy := oc.Spec.Components.Engine.Envoy
	side := engineSideFor(oc, "reader")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		// With an HPA the autoscaler owns the replica count after creation; writing it here would
		// fight every scale decision.
		if rd.HPA == nil || sts.Spec.Replicas == nil {
			sts.Spec.Replicas = int32Ptr(replicas)
		}
		if sts.ResourceVersion == "" {
			sts.Spec.ServiceName = name
			if oc.Spec.Components.AnalyticsProcessor.Enabled && oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
				sts.Spec.ServiceName = name + "-headless"
			}
		}
		sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := oramacoreContainer(oc, side)
		if rd.Resources != nil {
			c.Resources = *rd.Resources
		} else {
			// Right-size (#42): readers are query-serving; cgroup-aware tokio sizes threads to this
			// CPU limit. Override via spec.components.engine.reader.resources.
			c.Resources = *resourceCaps("2Gi", "1", "512Mi", "250m")
		}
		containers := []corev1.Container{c}
		if useEnvoy {
			containers = append(containers, envoySidecar())
		}
		nodeSelector, tolerations := podPlacement(oc)
		podSpec := corev1.PodSpec{
			ImagePullSecrets:              imagePullSecrets(oc),
			NodeSelector:                  nodeSelector,
			Tolerations:                   tolerations,
			InitContainers:                engineInitContainers(oc, side),
			Containers:                    containers,
			Volumes:                       []corev1.Volume{configVolume(oc), renderedConfigVolume()},
			TerminationGracePeriodSeconds: int64Ptr(readerGracePeriodSeconds),
		}
		// Spread readers across nodes when there's more than one (soft — a single-node sandbox still
		// schedules; a real cluster honors it). #18.
		if replicas > 1 || rd.HPA != nil {
			podSpec.TopologySpreadConstraints = spreadConstraints(lbls)
		}
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: engineConfigAnnotations(side)},
			Spec:       podSpec,
		}
		setVolumeClaimTemplates(sts, dataPVC(rd.Storage))
		applyPVCRetention(oc, sts)
		return controllerutil.SetControllerReference(oc, sts, r.Scheme)
	}); err != nil {
		return err
	}
	if sts.Spec.ServiceName == name+"-headless" {
		if err := r.reconcileReaderHeadlessService(ctx, oc); err != nil {
			return err
		}
	}
	if err := r.retireReaderDeployment(ctx, oc, sts); err != nil {
		return err
	}
	if err := r.reconcileReaderHPA(ctx, oc, name, lbls, replicas); err != nil {
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

// retireReaderDeployment removes the emptyDir reader Deployment that CRs reconciled before #141
// ran under the same name. It is deleted only once every StatefulSet reader is Ready, so the
// reader Service (which selects both by their shared labels) keeps serving through the switch;
// the new readers build their index from the stream while the old ones still answer queries.
func (r *OramaCloudV1Reconciler) retireReaderDeployment(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sts *appsv1.StatefulSet) error {
	old := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: sts.Name}, old); err != nil {
		return client.IgnoreNotFound(err)
	}
	if want := replicasOf(sts.Spec.Replicas); want == 0 || sts.Status.ReadyReplicas < want {
		return nil // the StatefulSet isn't serving yet; keep the old readers up
	}
	log.FromContext(ctx).Info("retiring the emptyDir reader Deployment; StatefulSet readers are ready", "name", sts.Name)
	return r.deleteCloudV1Owned(ctx, oc, old)
}

// reconcileReaderHPA owns a HorizontalPodAutoscaler on the reader StatefulSet when
// spec.components.engine.reader.hpa is set, and removes it when unset. Defaults: minReplicas =
// reader.replicas, maxReplicas = 2 × minReplicas, targetCPUUtilization = 70%.
func (r *OramaCloudV1Reconciler) reconcileReaderHPA(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string, lbls map[string]string, replicas int32) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	spec := oc.Spec.Components.Engine.Reader.HPA
	if spec == nil {
		return deleteIfExists(ctx, r.Client, hpa)
	}
	minReplicas := spec.MinReplicas
	if minReplicas == 0 {
		minReplicas = replicas
	}
	maxReplicas := spec.MaxReplicas
	if maxReplicas == 0 {
		maxReplicas = 2 * minReplicas
	}
	maxReplicas = max(maxReplicas, minReplicas)
	cpu := spec.TargetCPUUtilization
	if cpu == 0 {
		cpu = 70
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.Labels = lbls
		hpa.Spec = autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: name},
			MinReplicas:    int32Ptr(minReplicas),
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: int32Ptr(cpu)},
				},
			}},
		}
		return controllerutil.SetControllerReference(oc, hpa, r.Scheme)
	})
	return err
}

// ---- shared builders ----

// oramacoreContainer is the OramaCore container of a writer or reader pod, running the side's image.
func oramacoreContainer(oc *oramav1alpha1.OramaCloudV1, s engineSide) corev1.Container {
	// OramaCore serves GET /health at root (oramacore/src/web_server/api/mod.rs;
	// also the prod ALB/Envoy health path).
	health := func() *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromInt32(oramacorePort)}}}
	}
	// startup gates liveness/readiness so a slow index-load boot isn't killed: a restart would begin
	// the load again, so the probe allows the side's whole startupTimeout.
	startup := health()
	startup.PeriodSeconds = engineStartupPeriodSeconds
	startup.FailureThreshold = startupFailureThreshold(s.startupTimeout, engineStartupPeriodSeconds)
	readiness := health()
	readiness.PeriodSeconds, readiness.TimeoutSeconds, readiness.FailureThreshold = 10, 3, 3
	liveness := health()
	liveness.PeriodSeconds, liveness.TimeoutSeconds, liveness.FailureThreshold = 15, 3, 3

	return corev1.Container{
		Name:  "oramacore",
		Image: s.image,
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

// defaultDataVolumeSize sizes a component's data PVC when its storage.size is unset.
const defaultDataVolumeSize = "10Gi"

// dataPVC is the `data` volumeClaimTemplate for a StatefulSet, sized by the component's storage
// spec (default 10Gi on the cluster default StorageClass).
func dataPVC(v oramav1alpha1.VolumeSpec) corev1.PersistentVolumeClaim {
	size := resource.MustParse(defaultDataVolumeSize)
	if v.Size != nil {
		size = *v.Size
	}
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if v.StorageClassName != "" {
		pvc.Spec.StorageClassName = strPtr(v.StorageClassName)
	}
	return pvc
}

// setVolumeClaimTemplates sets a StatefulSet's volumeClaimTemplates only when the operator creates
// it. The API server refuses any later change to them, and a refused update would fail every
// reconcile step after this one, so an existing StatefulSet keeps the templates it was created
// with. Resizing a running component's volume is a PVC expansion; see docs/components.md.
func setVolumeClaimTemplates(sts *appsv1.StatefulSet, pvcs ...corev1.PersistentVolumeClaim) {
	if sts.ResourceVersion == "" {
		sts.Spec.VolumeClaimTemplates = pvcs
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

// engineConfigAnnotations carries the hash of the side's config template, because an init-rendered
// config needs a rollout to apply: a change to the JWT or LLM settings rolls the pods that render
// it. Side-only configs are hashed separately, so a change rolls only the side it affects. Key
// values are not in the template, so rotating a key in its Secret needs a manual rollout restart.
func engineConfigAnnotations(s engineSide) map[string]string {
	return map[string]string{"orama.cloud/config-hash": fmt.Sprintf("%x", sha256.Sum256([]byte(s.template)))}
}

func (r *OramaCloudV1Reconciler) reconcileReaderHeadlessService(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-reader-headless"
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelsFor(oc, "reader")
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = labelsFor(oc, "reader")
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: oramacorePort, TargetPort: intstr.FromInt32(oramacorePort)}}
		return controllerutil.SetControllerReference(oc, svc, r.Scheme)
	})
	return err
}
