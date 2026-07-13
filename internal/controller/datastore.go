/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// dataStoreDef describes a single-node stateful dependency run as a StatefulSet.
// For light/medium profiles this is a fine in-cluster default; heavy/production
// should swap in the dedicated operator (Altinity ClickHouse, RabbitMQ cluster
// operator) — tracked in docs/components.md.
type dataStoreDef struct {
	component string
	image     string
	ports     []corev1.ContainerPort
	dataPath  string
	env       []corev1.EnvVar
	args      []string
	// resources caps the container (requests/limits). Right-sizing (#42).
	resources *corev1.ResourceRequirements
	// configFiles are rendered into a `<name>-config` ConfigMap and mounted at configMount — the
	// BINARY-AWARE half of right-sizing: we translate our resource intent into the binary's own
	// config (e.g. a ClickHouse config.d overlay) so it actually adheres to the limit instead of
	// assuming it owns the whole node.
	configFiles map[string]string
	configMount string
	// gracePeriodSeconds is terminationGracePeriodSeconds. 0 → a store default (60s) — headroom for
	// the engine's SIGTERM flush (fsync the AOF, checkpoint parts, persist queues) so k8s doesn't
	// SIGKILL mid-write and corrupt state. Workload hardening (#18).
	gracePeriodSeconds int64
}

// reconcileDataStore provisions (or skips) one data store per its DataStoreSpec.
func (r *OramaCloudV1Reconciler) reconcileDataStore(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, spec oramav1alpha1.DataStoreSpec, def dataStoreDef) error {
	if spec.Mode == "external" {
		return nil // the app points at an external endpoint — nothing to tear down
	}
	if !spec.Enabled {
		return r.teardownByComponent(ctx, oc, def.component) // disable ≠ leave running (#23)
	}
	// "central" placement: only the primary runs it; read tiers reference the endpoint.
	if spec.Placement == "central" && oc.Spec.Region.Role == "read" {
		return nil
	}
	return r.provisionStatefulSet(ctx, oc, def)
}

// provisionStatefulSet unconditionally provisions a single-node StatefulSet + PVC +
// Service for a def (used by data stores and by ingestion's Mongo).
func (r *OramaCloudV1Reconciler) provisionStatefulSet(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, def dataStoreDef) error {
	if _, err := r.ensureComponentSecret(ctx, oc, def.component); err != nil {
		return err
	}
	name := oc.Name + "-" + def.component
	lbls := labelsFor(oc, def.component)
	// Workload hardening (#18): startup gates liveness/readiness so a slow data load isn't killed;
	// readiness gates Service routing; liveness restarts a hung store. TCP on the store's first port
	// (health paths differ per store; a wrong HTTP liveness path would restart-loop a healthy pod).
	var readiness, liveness, startup *corev1.Probe
	if len(def.ports) > 0 {
		p := def.ports[0].ContainerPort
		readiness, liveness, startup = tcpReadiness(p), tcpLiveness(p), tcpStartup(p)
	}
	// Requests on every container; nil → a modest default (heavy stores like ClickHouse pass their
	// own, or the CR overrides per component).
	res := def.resources
	if res == nil {
		res = resourceReqs("128Mi", "50m", "512Mi")
	}
	grace := def.gracePeriodSeconds
	if grace == 0 {
		grace = 60
	}

	volumes := []corev1.Volume{}
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: def.dataPath}}
	// Binary-aware right-sizing (#42): render the def's config files into a ConfigMap + mount them
	// so the binary is instructed to respect our resource intent.
	if len(def.configFiles) > 0 {
		cmName := name + "-config"
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = lbls
			cm.Data = def.configFiles
			return controllerutil.SetControllerReference(oc, cm, r.Scheme)
		}); err != nil {
			return err
		}
		volumes = append(volumes, corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}})
		// Mount each file via subPath so we ADD to (not replace) the image's config dir — mounting
		// a whole ConfigMap at e.g. ClickHouse's config.d would hide its shipped
		// docker_related_config.xml (which sets listen_host 0.0.0.0) and break external access.
		fnames := make([]string, 0, len(def.configFiles))
		for f := range def.configFiles {
			fnames = append(fnames, f)
		}
		sort.Strings(fnames)
		for _, f := range fnames {
			mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: def.configMount + "/" + f, SubPath: f, ReadOnly: true})
		}
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = lbls
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.ServiceName = name
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		c := corev1.Container{
			Name:            def.component,
			Image:           storeImage(oc, def.component, def.image),
			Ports:           def.ports,
			Env:             def.env,
			EnvFrom:         envFromFor(oc, def.component),
			Args:            def.args,
			Resources:       *res,
			StartupProbe:    startup,
			ReadinessProbe:  readiness,
			LivenessProbe:   liveness,
			SecurityContext: dataStoreSecurityContext(), // root-entrypoint chown needs CHOWN/SETUID (#18)
			VolumeMounts:    mounts,
		}
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				ImagePullSecrets:              imagePullSecrets(oc),
				Containers:                    []corev1.Container{c},
				Volumes:                       volumes,
				TerminationGracePeriodSeconds: int64Ptr(grace),
			},
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataPVC()}
		return controllerutil.SetControllerReference(oc, sts, r.Scheme)
	}); err != nil {
		return err
	}

	svcPorts := make([]corev1.ServicePort, 0, len(def.ports))
	for _, p := range def.ports {
		svcPorts = append(svcPorts, corev1.ServicePort{Name: p.Name, Port: p.ContainerPort, TargetPort: intstr.FromInt32(p.ContainerPort)})
	}
	if err := r.upsertService(ctx, oc, name, def.component, svcPorts); err != nil {
		return err
	}
	// Single-node stores are singletons → PDB with maxUnavailable 0: block casual eviction so a node
	// drain/autoscaler can't silently take the store (and its data-plane) down; node maintenance
	// handles it consciously (#18).
	return r.upsertPDB(ctx, oc, name, def.component, 0)
}

// storeImage: an override wins, else the default upstream image.
func storeImage(oc *oramav1alpha1.OramaCloudV1, component, def string) string {
	if o := oc.Spec.Images.Overrides[component]; o != "" {
		return o
	}
	return def
}

// ---- per-store reconcilers (wired into the reconcile step-list) ----

func (r *OramaCloudV1Reconciler) reconcileCache(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Cache, dataStoreDef{
		component: "cache",
		image:     "redis:7-alpine",
		ports:     []corev1.ContainerPort{{Name: "redis", ContainerPort: 6379}},
		dataPath:  "/data",
		args:      []string{"redis-server", "--appendonly", "yes"},
		// Right-size (#18): a GENEROUS memory limit — the binary-side cap (Redis `maxmemory` +
		// eviction policy, derived from this number) is #42; until then keep headroom so the cache
		// isn't OOM-killed. Override via spec.components.cache.resources.
		resources: resourcesOr(oc.Spec.Components.Cache.Resources, resourceReqs("128Mi", "50m", "1Gi")),
	})
}

// Broker admin credentials (management HTTP API + AMQP default user). A sandbox dummy today; a
// single source of truth so rabbitmqConf (mounted into the pod) and the <cr>-broker-admin Secret
// (read by the RabbitMQTopology reconciler) can never drift. Prod overrides both by editing the
// Secret + rendering the password into rabbitmq.conf via an init container (the #41 follow-up).
const (
	brokerAdminUser     = "orama"
	brokerAdminPassword = "dev-rabbitmq-password"
	// The replication stream OramaCore's writer produces to and the reader consumes from (#41).
	brokerOperationsStream = "oramacore-operations"
	// Stream args OramaCore sets when IT declares oramacore-operations on connect. The operator MUST
	// declare the stream with the SAME args — RabbitMQ rejects a redeclare whose args differ
	// ("inequivalent arg '<x>'"), which otherwise fails the whole RabbitMQTopology reconcile before it
	// creates the reader/writer users, so the reader can never auth to the stream. Both confirmed
	// against a live AKS cluster (`rabbitmqctl list_queues name arguments`): x-max-length-bytes (5 GB
	// retention) + x-queue-leader-locator (least-leaders). x-queue-type=stream is added unconditionally.
	brokerOperationsStreamMaxBytes      = 5_000_000_000
	brokerOperationsStreamLeaderLocator = "least-leaders"
)

// rabbitmqConf renders the broker config (#41): enable the stream listener on 5552 and set
// advertised_host to the broker Service — for our SINGLE-node broker the Service routes to the one
// pod, so the stream client reconnects correctly (the pod-DNS "advertised host" gotcha only bites
// multi-node clusters). default_user/pass live here (a sandbox dummy) rather than the
// RABBITMQ_DEFAULT_USER env, because a mounted rabbitmq.conf and that env conflict; prod should
// render the password from the broker Secret via an init container (like the engine config).
func rabbitmqConf(advertisedHost string) string {
	return "default_user = " + brokerAdminUser + "\n" +
		"default_pass = " + brokerAdminPassword + "\n" +
		"stream.listeners.tcp.1 = 5552\n" +
		"stream.advertised_host = " + advertisedHost + "\n" +
		"stream.advertised_port = 5552\n"
}

func (r *OramaCloudV1Reconciler) reconcileBroker(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	advertised := oc.Name + "-broker." + oc.Namespace + ".svc.cluster.local"
	if err := r.reconcileDataStore(ctx, oc, oc.Spec.Components.Broker, dataStoreDef{
		component: "broker",
		image:     "rabbitmq:3.13-management",
		ports: []corev1.ContainerPort{
			{Name: "amqp", ContainerPort: 5672},
			{Name: "management", ContainerPort: 15672},
			{Name: "stream", ContainerPort: 5552}, // rabbitmq_stream — OramaCore writer→reader replication (#41)
		},
		dataPath: "/var/lib/rabbitmq",
		// Enable the stream plugin + expose 5552 so OramaCore replicates over a stream — our OWN
		// topology (no vendor operator, #41). subPath-mounted so we add to /etc/rabbitmq, not clobber it.
		configMount: "/etc/rabbitmq",
		configFiles: map[string]string{
			"enabled_plugins": "[rabbitmq_management,rabbitmq_stream].\n",
			"rabbitmq.conf":   rabbitmqConf(advertised),
		},
		// Right-size (#18): RabbitMQ 3.13 detects the cgroup memory limit and derives its
		// vm_memory_high_watermark from it, so this limit is binary-aware. Generous default; override
		// via spec.components.broker.resources.
		resources: resourcesOr(oc.Spec.Components.Broker.Resources, resourceReqs("256Mi", "50m", "1Gi")),
	}); err != nil {
		return err
	}
	// Model the broker's vhost/users/permissions/streams as an owned, reconciled RabbitMQTopology
	// CRD (#40/#41) — declarative topology instead of OramaCore creating the stream as an app-boot
	// side effect and every service sharing the admin user.
	return r.reconcileBrokerTopology(ctx, oc)
}

// brokerAdminSecretName / brokerUsersSecretName name the Secrets the broker topology depends on.
func brokerAdminSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-broker-admin" }
func brokerUsersSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-broker-users" }

// reconcileBrokerTopology creates + owns a RabbitMQTopology declaring the broker's vhost, the
// least-privilege per-service users (oramacore-writer / oramacore-reader) with stream-scoped
// permissions, and the oramacore-operations replication stream. It only manages topology where the
// broker actually runs in-cluster; elsewhere it removes the CR (whose finalizer cleans the broker).
//
// Per the current decision the engine still authenticates as the shared admin user, so the
// per-service users are declared-but-unused for now — the topology makes least-privilege ready to
// adopt; wiring the engine's writer/reader to these users is the fast-follow (operator#40).
func (r *OramaCloudV1Reconciler) reconcileBrokerTopology(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	spec := oc.Spec.Components.Broker
	brokerLocal := spec.Mode != "external" && spec.Enabled &&
		!(spec.Placement == "central" && oc.Spec.Region.Role == "read")

	topo := &oramav1alpha1.RabbitMQTopology{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-broker", Namespace: oc.Namespace}}
	if !brokerLocal {
		// No in-cluster broker here → no topology to own (deleting the CR runs its finalizer).
		return deleteIfExists(ctx, r.Client, topo)
	}
	if err := r.ensureBrokerSecrets(ctx, oc); err != nil {
		return err
	}
	usersSecret := brokerUsersSecretName(oc)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, topo, func() error {
		topo.Labels = labelsFor(oc, "broker")
		topo.Spec = oramav1alpha1.RabbitMQTopologySpec{
			BrokerService:  oc.Name + "-broker",
			ManagementPort: 15672,
			AdminSecretRef: brokerAdminSecretName(oc),
			VHosts:         []string{"/"},
			Users: []oramav1alpha1.RabbitMQUser{
				{Name: "oramacore-writer", PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: usersSecret, Key: "writer-password"}},
				{Name: "oramacore-reader", PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: usersSecret, Key: "reader-password"}},
			},
			Permissions: []oramav1alpha1.RabbitMQPermission{
				// writer produces to (and may declare) the stream; reader consumes from (and may
				// declare) it. Both scoped to the exact stream — least-privilege (principle #4).
				{User: "oramacore-writer", VHost: "/", Configure: "^" + brokerOperationsStream + "$", Write: "^" + brokerOperationsStream + "$", Read: "^" + brokerOperationsStream + "$"},
				{User: "oramacore-reader", VHost: "/", Configure: "^" + brokerOperationsStream + "$", Write: "^$", Read: "^" + brokerOperationsStream + "$"},
			},
			// Declare the stream with the SAME retention OramaCore uses on its own stream-protocol
			// Create (x-max-length-bytes = 5 GB) — RabbitMQ rejects a redeclare whose properties differ,
			// which would fail this reconcile before the reader/writer users are created (the reader then
			// can't auth to the stream). Value confirmed against a live cluster. Bounds growth too (#8).
			Streams: []oramav1alpha1.RabbitMQStream{{Name: brokerOperationsStream, VHost: "/", MaxLengthBytes: brokerOperationsStreamMaxBytes, LeaderLocator: brokerOperationsStreamLeaderLocator}},
		}
		return controllerutil.SetControllerReference(oc, topo, r.Scheme)
	})
	return err
}

// ensureBrokerSecrets provisions the two Secrets the topology depends on (create-if-absent so real
// overrides / generated passwords survive reconciles): the admin creds (matching rabbitmqConf) and
// per-service user passwords (random, generated once).
func (r *OramaCloudV1Reconciler) ensureBrokerSecrets(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: brokerAdminSecretName(oc), Namespace: oc.Namespace, Labels: labelsFor(oc, "broker")},
		StringData: map[string]string{"username": brokerAdminUser, "password": brokerAdminPassword},
	}
	if err := r.createSecretIfAbsent(ctx, oc, admin); err != nil {
		return err
	}

	writerPass, err := randHex(16)
	if err != nil {
		return err
	}
	readerPass, err := randHex(16)
	if err != nil {
		return err
	}
	users := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: brokerUsersSecretName(oc), Namespace: oc.Namespace, Labels: labelsFor(oc, "broker")},
		StringData: map[string]string{"writer-password": writerPass, "reader-password": readerPass},
	}
	return r.createSecretIfAbsent(ctx, oc, users)
}

// createSecretIfAbsent owner-references and creates a Secret only if it does not already exist, so
// generated material (or a real override) is never rotated out from under running clients.
func (r *OramaCloudV1Reconciler) createSecretIfAbsent(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sec *corev1.Secret) error {
	if err := r.Get(ctx, client.ObjectKey{Namespace: sec.Namespace, Name: sec.Name}, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := controllerutil.SetControllerReference(oc, sec, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// resourceCaps builds a container ResourceRequirements. The starting point for right-sizing
// (#42): the container is capped AND each binary's own config is derived from the SAME numbers
// (see clickhouseMemoryXML) so it actually adheres instead of assuming it owns the node.
func resourceCaps(memLimit, cpuLimit, memReq, cpuReq string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memLimit), corev1.ResourceCPU: resource.MustParse(cpuLimit)},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memReq), corev1.ResourceCPU: resource.MustParse(cpuReq)},
	}
}

// resourceReqs is the default-sizing helper for the non-engine components (issue #18): CPU+memory
// REQUESTS on every container (the scheduler needs them — unset requests make a node's capacity
// invisible) plus a MEMORY limit only. It deliberately sets NO CPU limit — a CPU limit throttles
// (CFS quota) rather than protecting the node, and the request already gives the scheduler what it
// needs. Memory limits are sized PER COMPONENT at the call sites (a blanket small one OOM-kills the
// heavy ones — embeddings/OramaCore/ClickHouse); the engine keeps resourceCaps (cgroup-aware tokio
// tolerates a CPU limit). Override any of it via the CR's per-component `resources`.
func resourceReqs(memReq, cpuReq, memLimit string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memReq), corev1.ResourceCPU: resource.MustParse(cpuReq)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memLimit)},
	}
}

// clickhouseMemoryXML instructs ClickHouse to respect its container limit (#42, #16).
//
// The `*_to_ram_ratio` knob reads the **host's** physical RAM, NOT the cgroup limit — on our 30 GiB
// AKS node it computed a ~24 GiB ceiling and ClickHouse was OOMKilled at startup (exit 137) inside its
// 2Gi pod, crashlooping. The correct binary-aware cap (principle #8) is an ABSOLUTE
// `max_server_memory_usage` in bytes, derived from the SAME number as the pod's memory limit, so it
// holds regardless of host RAM. Keep it ~25% under the pod limit for untracked allocations at boot.
//
// Also (#16): enable console logging so `kubectl logs` shows ClickHouse's own logs (it otherwise logs
// only to files under /var/log/clickhouse-server, making OOMs/crashes invisible to the log stream).
//
// Sized against the 4Gi pod limit (below): server cap 3 GiB, leaving ~1 GiB for UNTRACKED startup RSS
// (mmap'd parts, jemalloc arenas, thread stacks) that `max_server_memory_usage` does NOT bound.
// ClickHouse ran fine at 2Gi steady-state but OOMed on restart because loading the accumulated
// analytics parts' metadata spikes RSS past 2Gi — so the pod floor is ~4Gi, not 2Gi. Keep in lockstep.
const clickhouseMemoryXML = `<clickhouse>
    <!-- ABSOLUTE cap (bytes) = 3 GiB, derived from the 4Gi pod limit. NOT the host-relative ratio. -->
    <max_server_memory_usage>3221225472</max_server_memory_usage>
    <!-- Default mark cache is 5 GiB — far too large for a small pod. -->
    <mark_cache_size>268435456</mark_cache_size>
    <!-- Uncompressed cache is rarely useful for analytics; disable it. -->
    <uncompressed_cache_size>0</uncompressed_cache_size>
    <!-- #16: log to stdout so kubectl logs surfaces server logs, not just the entrypoint banner. -->
    <logger>
        <console>1</console>
    </logger>
    <!-- #42: disable ClickHouse's heavy INTERNAL system-log tables. They write parts constantly
         (thousands accumulated over hours here), and merging that backlog on startup blew RSS past the
         pod limit -> OOMKilled crashloop. A self-host analytics store (which holds the oramacore_*
         tables) does not need ClickHouse's own trace/metric/query telemetry. Console logging (above)
         still gives operational logs on stdout. -->
    <asynchronous_metric_log remove="1"/>
    <metric_log remove="1"/>
    <trace_log remove="1"/>
    <text_log remove="1"/>
    <query_log remove="1"/>
    <query_thread_log remove="1"/>
    <query_views_log remove="1"/>
    <part_log remove="1"/>
    <session_log remove="1"/>
    <processors_profile_log remove="1"/>
    <opentelemetry_span_log remove="1"/>
    <crash_log remove="1"/>
</clickhouse>
`

func (r *OramaCloudV1Reconciler) reconcileAnalytics(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Analytics, dataStoreDef{
		component: "analytics",
		image:     "clickhouse/clickhouse-server:24.8",
		ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8123},
			{Name: "native", ContainerPort: 9000},
		},
		dataPath: "/var/lib/clickhouse",
		// Right-size ClickHouse (#42): cap the pod AND derive its own config from that cap so it
		// stops grabbing the whole node (default 5 GiB mark cache + 90% RAM target). 4Gi is the real
		// floor — 2Gi OOMed on restart when loading accumulated part metadata (see clickhouseMemoryXML).
		// The clickhouseMemoryXML absolute cap is derived from this 4Gi — keep them in lockstep if the
		// CR overrides it (spec.components.analytics.resources).
		resources:   resourcesOr(oc.Spec.Components.Analytics.Resources, resourceCaps("4Gi", "1", "1Gi", "100m")),
		configMount: "/etc/clickhouse-server/config.d",
		configFiles: map[string]string{"orama-memory.xml": clickhouseMemoryXML},
	})
}

func (r *OramaCloudV1Reconciler) reconcileSearch(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	// TYPESENSE_DATA_DIR / _ENABLE_CORS come from the shared ConfigMap and
	// TYPESENSE_API_KEY from the shared Secret (via envFrom).
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Search, dataStoreDef{
		component: "search",
		image:     "typesense/typesense:0.25.2",
		ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: 8108}},
		dataPath:  "/data",
		// Right-size (#18): generous default; override via spec.components.search.resources.
		resources: resourcesOr(oc.Spec.Components.Search.Resources, resourceReqs("256Mi", "50m", "1Gi")),
	})
}
