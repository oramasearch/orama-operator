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
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// dataStoreDef describes a single-node stateful dependency run as a StatefulSet.
// For light/medium profiles this is a fine in-cluster default; heavy/production
// should swap in the dedicated operator (Altinity ClickHouse, RabbitMQ cluster
// operator) — tracked in docs/components.md.
type dataStoreDef struct {
	component string
	storage   *oramav1alpha1.VolumeSpec
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
	// configTemplates are config files holding __VAR__ placeholders, one per templateEnv entry
	// (#307). They join the `<name>-config` ConfigMap; a render-config init container substitutes
	// each placeholder from its variable into an emptyDir, and the rendered file is mounted read-only
	// at configMount (subPath), so a credential never sits in the ConfigMap or the pod spec.
	configTemplates map[string]string
	templateEnv     []corev1.EnvVar
	// credentialSecrets are the Secrets the store reads credentials from; their data hash is on the
	// pod template, so a changed credential rolls the store.
	credentialSecrets []string
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
	def.storage = spec.Storage
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
	var initContainers []corev1.Container
	annotations := map[string]string{}
	// Binary-aware right-sizing (#42): render the def's config files into a ConfigMap + mount them
	// so the binary is instructed to respect our resource intent.
	if len(def.configFiles) > 0 || len(def.configTemplates) > 0 {
		cmName := name + "-config"
		data := map[string]string{}
		maps.Copy(data, def.configFiles)
		maps.Copy(data, def.configTemplates)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = lbls
			cm.Data = data
			return controllerutil.SetControllerReference(oc, cm, r.Scheme)
		}); err != nil {
			return err
		}
		volumes = append(volumes, corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}})
		// Mount each file via subPath so we ADD to (not replace) the image's config dir — mounting
		// a whole ConfigMap at e.g. ClickHouse's config.d would hide its shipped
		// docker_related_config.xml (which sets listen_host 0.0.0.0) and break external access.
		for _, f := range slices.Sorted(maps.Keys(def.configFiles)) {
			mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: def.configMount + "/" + f, SubPath: f, ReadOnly: true})
		}
		if len(def.configTemplates) > 0 {
			templates := slices.Sorted(maps.Keys(def.configTemplates))
			volumes = append(volumes, renderedConfigVolume())
			for _, f := range templates {
				mounts = append(mounts, corev1.VolumeMount{Name: "rendered-config", MountPath: def.configMount + "/" + f, SubPath: f, ReadOnly: true})
			}
			initContainers = append(initContainers, storeRenderConfigInit(oc, templates, def.templateEnv))
			// The init container renders only at pod start, so a template change must roll the pod.
			annotations[configHashKey] = configFilesHash(data)
		}
	}
	if len(def.credentialSecrets) > 0 {
		hash, err := r.credentialsHash(ctx, oc, def.credentialSecrets...)
		if err != nil {
			return err
		}
		annotations[credentialsHashKey] = hash
	}
	if len(annotations) == 0 {
		annotations = nil
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
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: annotations},
			Spec: corev1.PodSpec{
				ImagePullSecrets:              imagePullSecrets(oc),
				InitContainers:                initContainers,
				Containers:                    []corev1.Container{c},
				Volumes:                       volumes,
				TerminationGracePeriodSeconds: int64Ptr(grace),
			},
		}
		// Preserve the fetched template in full, including admission-populated fields.
		if sts.ResourceVersion == "" {
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataPVC(def.storage, defaultVolumeSize)}
		}
		boundV1PodStorage(&sts.Spec.Template.Spec)
		cloudV1PodIdentity(&sts.Spec.Template.Spec, name)
		if err := ensureCloudV1ServiceAccount(ctx, r.Client, r.Scheme, oc, name, lbls); err != nil {
			return err
		}
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

// storeRenderConfigInit renders a store's config templates from the ConfigMap (/tmpl) into the
// rendered-config emptyDir (/rendered), filling each __VAR__ placeholder from the variable of the
// same name (renderSubstitutions; values are escaped, never evaluated).
func storeRenderConfigInit(oc *oramav1alpha1.OramaCloudV1, files []string, env []corev1.EnvVar) corev1.Container {
	vars := make([]string, 0, len(env))
	for _, e := range env {
		vars = append(vars, e.Name)
	}
	var b strings.Builder
	b.WriteString(renderEscFunc)
	for i, f := range files {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "sed%s /tmpl/%s > /rendered/%s", renderSubstitutions(vars), f, f)
	}
	return corev1.Container{
		Name:    "render-config",
		Image:   storeImage(oc, "busybox", "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"),
		Command: []string{"sh", "-c", b.String()},
		Env:     env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/tmpl"},
			{Name: "rendered-config", MountPath: "/rendered"},
		},
		SecurityContext: hardenedSecurityContext(), // workload hardening baseline (#18)
	}
}

// configFilesHash is the hex SHA-256 of a store's config files, in name order.
func configFilesHash(files map[string]string) string {
	h := sha256.New()
	for _, f := range slices.Sorted(maps.Keys(files)) {
		h.Write([]byte(f + "\x00" + files[f] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// storeImage: an override wins, else the default upstream image.
func storeImage(oc *oramav1alpha1.OramaCloudV1, component, def string) string {
	if o := oc.Spec.Images.Overrides[component]; o != "" {
		return o
	}
	return def
}

// ---- per-store reconcilers (wired into the reconcile step-list) ----

// reconcileCache runs Redis with a password (#307): --requirepass takes $(REDIS_PASSWORD), which the
// kubelet expands from <cr>-cache-auth, so the pod spec names the variable, not the value. Every
// in-cluster client gets the same variable and a URL that references it (redisURL).
func (r *OramaCloudV1Reconciler) reconcileCache(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Cache, dataStoreDef{
		component:         "cache",
		image:             "redis:7-alpine@sha256:892022f9f18c26c92c30e15194d132f90044af87b94a491a75429bbc3982eab1",
		ports:             []corev1.ContainerPort{{Name: "redis", ContainerPort: 6379}},
		dataPath:          "/data",
		env:               []corev1.EnvVar{redisPasswordEnvVar(oc)},
		args:              []string{"redis-server", "--appendonly", "yes", "--requirepass", "$(" + redisPasswordEnv + ")"},
		credentialSecrets: []string{cacheAuthSecretName(oc)},
		// Right-size (#18): a GENEROUS memory limit — the binary-side cap (Redis `maxmemory` +
		// eviction policy, derived from this number) is #42; until then keep headroom so the cache
		// isn't OOM-killed. Override via spec.components.cache.resources.
		resources: resourcesOr(oc.Spec.Components.Cache.Resources, resourceReqs("128Mi", "50m", "1Gi")),
	})
}

// The broker's admin credentials live in <cr>-broker-admin (keys username, password), generated
// once by ensureCredentialSecrets. That Secret is the only source: the broker's rabbitmq.conf and
// boot definitions, the engine config and harvest-master's AMQP URL all take the values from it at
// pod start (#307), and the RabbitMQTopology reconciler reads it for the management API.
const (
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

// rabbitmqConfTemplate is the broker config template (#41): enable the stream listener on 5552 and
// set advertised_host to the broker Service — for our SINGLE-node broker the Service routes to the
// one pod, so the stream client reconnects correctly (the pod-DNS "advertised host" gotcha only bites
// multi-node clusters). default_user/default_pass are placeholders the render-config init container
// fills from <cr>-broker-admin (they live in the file rather than the RABBITMQ_DEFAULT_USER env,
// because a mounted rabbitmq.conf and that env conflict). RabbitMQ applies them only to an empty
// data directory, so the same credentials are also imported from credentials.json on every boot.
func rabbitmqConfTemplate(advertisedHost string) string {
	return "default_user = " + keyPlaceholder(brokerUserEnv) + "\n" +
		"default_pass = " + keyPlaceholder(brokerPasswordEnv) + "\n" +
		"definitions.import_backend = local_filesystem\n" +
		"definitions.local.path = /etc/rabbitmq/" + brokerDefinitionsFile + "\n" +
		"stream.listeners.tcp.1 = 5552\n" +
		"stream.advertised_host = " + advertisedHost + "\n" +
		"stream.advertised_port = 5552\n"
}

// brokerDefinitionsFile is the boot definitions file RabbitMQ imports on every start. Importing a
// user that exists updates its password (rabbit_auth_backend_internal:put_user), so a changed
// <cr>-broker-admin password reaches a broker whose data volume already holds the old one. It holds
// only the admin user, the / vhost and its permissions: streams and per-service users stay with
// RabbitMQTopology.
const brokerDefinitionsFile = "credentials.json"

// brokerDefinitionsTemplate is brokerDefinitionsFile with the admin credentials as placeholders.
func brokerDefinitionsTemplate() string {
	u, p := keyPlaceholder(brokerUserEnv), keyPlaceholder(brokerPasswordEnv)
	return `{
  "users": [{"name": "` + u + `", "password": "` + p + `", "tags": ["administrator"]}],
  "vhosts": [{"name": "/"}],
  "permissions": [{"user": "` + u + `", "vhost": "/", "configure": ".*", "write": ".*", "read": ".*"}]
}
`
}

func (r *OramaCloudV1Reconciler) reconcileBroker(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	advertised := oc.Name + "-broker." + oc.Namespace + ".svc.cluster.local"
	if err := r.reconcileDataStore(ctx, oc, oc.Spec.Components.Broker, dataStoreDef{
		component: "broker",
		image:     "rabbitmq:3.13-management@sha256:e582c0bc7766f3342496d8485efb5a1df782b5ce3886ad017e2eaae442311f69",
		ports: []corev1.ContainerPort{
			{Name: "amqp", ContainerPort: 5672},
			{Name: "management", ContainerPort: 15672},
			{Name: "stream", ContainerPort: 5552}, // rabbitmq_stream — OramaCore writer→reader replication (#41)
		},
		dataPath: "/var/lib/rabbitmq",
		// Enable the stream plugin + expose 5552 so OramaCore replicates over a stream — our OWN
		// topology (no vendor operator, #41). subPath-mounted so we add to /etc/rabbitmq, not clobber it.
		configMount: "/etc/rabbitmq",
		configFiles: map[string]string{"enabled_plugins": "[rabbitmq_management,rabbitmq_stream].\n"},
		configTemplates: map[string]string{
			"rabbitmq.conf":       rabbitmqConfTemplate(advertised),
			brokerDefinitionsFile: brokerDefinitionsTemplate(),
		},
		templateEnv:       brokerCredentialEnv(oc),
		credentialSecrets: []string{brokerAdminSecretName(oc)},
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

// The per-service broker users (#99, #824, principle 4). The writer signs in as brokerWriterUser and
// every reader pod, of either image, as brokerReaderUser, which may not write to the replication
// stream (#795). The engine users' passwords are the keys of <cr>-broker-users (brokerUserPasswordKey);
// harvest-master's is <cr>-broker-harvest.
const (
	brokerWriterUser = "oramacore-writer"
	brokerReaderUser = "oramacore-reader"
	// brokerHarvestUser is harvest-master's broker user (#824): HARVEST's per-crawl queues
	// only. Its password is <cr>-broker-harvest's password key.
	brokerHarvestUser = "harvest"
)

// brokerReaderLocalStream is where the writer that the combined OramaCore image starts in every
// reader pod produces (#795). Nothing addresses that writer and no reader consumes this
// stream: it exists so a reader never needs write permission on the replication stream.
const brokerReaderLocalStream = "oramacore-reader-local"

// brokerHarvestSecretName is the Secret holding harvest-master's broker password (key password). It
// is its own Secret, not a key of <cr>-broker-users, so a change to it rolls only harvest-master and
// adding it rolls neither the writer nor the readers.
func brokerHarvestSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-broker-harvest"
}

// brokerUserPasswordKey is the <cr>-broker-users key holding a broker user's password.
func brokerUserPasswordKey(user string) string {
	if user == brokerReaderUser {
		return "reader-password"
	}
	return "writer-password"
}

// harvestQueuePattern matches HARVEST's per-crawl queues, <workerID>-<indexID>-queue (HARVEST
// worker/src/models/scrapping_metadata.py), and nothing OramaCore uses.
const harvestQueuePattern = `[A-Za-z0-9_-]+-[A-Za-z0-9_-]+-queue`

// brokerUserPermissions is what each user may do on the / vhost, as RabbitMQ regular expressions over
// resource names:
//   - oramacore-writer declares (configure), publishes to and reads the replication stream;
//   - oramacore-reader declares and reads the replication stream, and declares and publishes to
//     oramacore-reader-local, where the writer the combined image starts in a reader produces (#795);
//     it may not publish to the replication stream;
//   - harvest declares, consumes and deletes HARVEST's per-crawl queues and publishes to them through
//     the default exchange (authorized as amq.default), as harvest-master and the workers do (#824).
//
// None may touch any other queue, stream or exchange.
func brokerUserPermissions() []oramav1alpha1.RabbitMQPermission {
	stream := "^" + brokerOperationsStream + "$"
	local := "^" + brokerReaderLocalStream + "$"
	queues := "^" + harvestQueuePattern + "$"
	return []oramav1alpha1.RabbitMQPermission{
		{User: brokerWriterUser, VHost: "/", Configure: stream, Write: stream, Read: stream},
		{User: brokerReaderUser, VHost: "/", Configure: "^(" + brokerOperationsStream + "|" + brokerReaderLocalStream + ")$", Write: local, Read: stream},
		{User: brokerHarvestUser, VHost: "/", Configure: queues, Write: `^(amq\.default|` + harvestQueuePattern + `)$`, Read: queues},
	}
}

// engineBrokerPasswordEnv carries the password of the side's broker user from <cr>-broker-users into
// render-config as BROKER_PASSWORD, which fills the template's __BROKER_PASSWORD__.
func engineBrokerPasswordEnv(oc *oramav1alpha1.OramaCloudV1, side engineSide) corev1.EnvVar {
	return secretKeyEnv(brokerPasswordEnv, brokerUsersSecretName(oc), brokerUserPasswordKey(side.brokerUser))
}

// brokerUsersReady reports whether the engine may be written: where the broker runs in this cluster,
// its RabbitMQTopology must report Ready for its current generation with every user applied, so the
// writer's and readers' users exist on the broker with the passwords in <cr>-broker-users. Where the
// broker runs elsewhere, the stack's set supplies <cr>-broker-users and there is nothing to wait for.
func (r *OramaCloudV1Reconciler) brokerUsersReady(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	if !storeLocal(oc, oc.Spec.Components.Broker) {
		return true, nil
	}
	topo := &oramav1alpha1.RabbitMQTopology{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-broker"}, topo); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return brokerTopologyUsersApplied(topo), nil
}

// brokerTopologyUsersApplied reports whether a RabbitMQTopology's status says every user of its
// current spec is on the broker: Ready for the current generation, with as many users as declared.
func brokerTopologyUsersApplied(topo *oramav1alpha1.RabbitMQTopology) bool {
	return topo.Status.ObservedGeneration == topo.Generation &&
		topo.Status.Users == len(topo.Spec.Users) &&
		apimeta.IsStatusConditionTrue(topo.Status.Conditions, "Ready")
}

// reconcileBrokerTopology creates + owns a RabbitMQTopology declaring the broker's vhost, the
// least-privilege per-service users (oramacore-writer / oramacore-reader) with stream-scoped
// permissions, and the oramacore-operations replication stream. It only manages topology where the
// broker actually runs in-cluster; elsewhere it removes the CR (whose finalizer cleans the broker).
// The engine signs in as these users (#99); the admin user in <cr>-broker-admin is for the broker
// itself and this topology's management calls.
func (r *OramaCloudV1Reconciler) reconcileBrokerTopology(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	topo := &oramav1alpha1.RabbitMQTopology{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-broker", Namespace: oc.Namespace}}
	if !storeLocal(oc, oc.Spec.Components.Broker) {
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
				{Name: brokerWriterUser, PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: usersSecret, Key: brokerUserPasswordKey(brokerWriterUser)}},
				{Name: brokerReaderUser, PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: usersSecret, Key: brokerUserPasswordKey(brokerReaderUser)}},
				{Name: brokerHarvestUser, PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: brokerHarvestSecretName(oc), Key: "password"}},
			},
			Permissions: brokerUserPermissions(),
			// Declare the streams with the SAME retention OramaCore uses on its own stream-protocol
			// Create (x-max-length-bytes = 5 GB) — RabbitMQ rejects a redeclare whose properties differ,
			// which would fail this reconcile before the reader/writer users are created (the reader then
			// can't auth to the stream). Value confirmed against a live cluster. Bounds growth too (#8).
			// oramacore-reader-local is declared by the combined image's readers (#795) with the same
			// Create, so it takes the same arguments.
			Streams: []oramav1alpha1.RabbitMQStream{
				{Name: brokerOperationsStream, VHost: "/", MaxLengthBytes: brokerOperationsStreamMaxBytes, LeaderLocator: brokerOperationsStreamLeaderLocator},
				{Name: brokerReaderLocalStream, VHost: "/", MaxLengthBytes: brokerOperationsStreamMaxBytes, LeaderLocator: brokerOperationsStreamLeaderLocator},
			},
		}
		return controllerutil.SetControllerReference(oc, topo, r.Scheme)
	})
	return err
}

// ensureBrokerSecrets provisions the per-service user passwords the topology declares (random,
// generated once; create-if-absent so they survive reconciles): <cr>-broker-users for the engine and
// <cr>-broker-harvest for harvest-master. An existing Secret is never changed. The admin Secret comes
// from ensureCredentialSecrets in the bootstrap step.
func (r *OramaCloudV1Reconciler) ensureBrokerSecrets(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	harvestPass, err := randHex(24)
	if err != nil {
		return err
	}
	if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: brokerHarvestSecretName(oc), Namespace: oc.Namespace, Labels: labelsFor(oc, "broker")},
		Data:       map[string][]byte{"password": []byte(harvestPass)},
	}); err != nil {
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
		Data: map[string][]byte{
			brokerUserPasswordKey(brokerWriterUser): []byte(writerPass),
			brokerUserPasswordKey(brokerReaderUser): []byte(readerPass),
		},
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

// clickhouseImage is the analytics ClickHouse server's default image. The analytics retention Job
// runs its clickhouse-client (reconcileAnalyticsRetention).
const clickhouseImage = "clickhouse/clickhouse-server:24.8@sha256:1ffa82edee000a42c09313bd9f1293d94c570aee74babc1b3ca9983a35fa597b"

func (r *OramaCloudV1Reconciler) reconcileAnalytics(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Analytics, dataStoreDef{
		component:         "analytics",
		env:               []corev1.EnvVar{clickhousePasswordEnv(oc)},
		credentialSecrets: []string{clickhouseAuthSecretName(oc)},
		image:             clickhouseImage,
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
	// TYPESENSE_DATA_DIR / _ENABLE_CORS come from the shared ConfigMap.
	return r.reconcileDataStore(ctx, oc, oc.Spec.Components.Search, dataStoreDef{
		component:         "search",
		env:               []corev1.EnvVar{secretKeyEnv("TYPESENSE_API_KEY", typesenseAuthSecretName(oc), "TYPESENSE_API_KEY")},
		credentialSecrets: []string{typesenseAuthSecretName(oc)},
		image:             "typesense/typesense:0.25.2@sha256:1ddbb28a52b0b8be31237ec718df1c9bc895c5626b26c10fa3f340f684e81488",
		ports:             []corev1.ContainerPort{{Name: "http", ContainerPort: 8108}},
		dataPath:          "/data",
		// Right-size (#18): generous default; override via spec.components.search.resources.
		resources: resourcesOr(oc.Spec.Components.Search.Resources, resourceReqs("256Mi", "50m", "1Gi")),
	})
}
