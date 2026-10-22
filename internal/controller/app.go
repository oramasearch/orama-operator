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
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// tcpReadiness is a universal readiness probe — the port accepting connections. Used
// for components whose HTTP health path varies (data stores, ported apps); the engine
// uses richer HTTP /health probes.
func tcpReadiness(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		TimeoutSeconds:      3,
		FailureThreshold:    6,
	}
}

// tcpLiveness restarts a HUNG pod — the port stopped accepting connections. Deliberately MORE
// LENIENT than readiness (longer period, no short initial delay because the startup probe already
// guards the boot) so a briefly-busy app isn't killed. TCP, not HTTP: the app's health path varies
// and a wrong liveness path would restart-loop a healthy pod — the engine uses richer HTTP /health.
// Issue #18.
func tcpLiveness(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		PeriodSeconds:    20,
		TimeoutSeconds:   3,
		FailureThreshold: 3,
	}
}

// tcpStartup gives a slow-booting app time before liveness/readiness kick in, so a slow boot
// (index-builder's ~29-var wiring, Keycloak's start-dev, a store loading data) isn't killed as
// "unhealthy". ~5 min budget (30 × 10s); once it passes, liveness/readiness take over. Issue #18.
func tcpStartup(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		PeriodSeconds:    10,
		TimeoutSeconds:   3,
		FailureThreshold: 30,
	}
}

// drainPreStop delays SIGTERM briefly so kube-proxy/ingress remove the pod from Service endpoints
// BEFORE its process exits — otherwise a rolling update drops in-flight requests to a pod that's
// already gone. An exec `sleep` (not the native SleepAction) keeps this portable: SleepAction is
// only enabled-by-default from k8s 1.30, and every app image the operator runs ships a shell. Issue
// #18.
func drainPreStop() *corev1.Lifecycle {
	return &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "sleep 5"}},
		},
	}
}

// appDef describes a stateless app run as a Deployment (+ Service, + optional Ingress).
type appDef struct {
	component string
	image     string
	port      int32
	replicas  int32
	// recreate stops the previous pod before replacing a worker whose claims are process-local.
	recreate    bool
	env         []corev1.EnvVar
	args        []string
	ingressHost string
	// ingressExtraHosts are further names the Ingress answers, to the same backend (no DNS).
	ingressExtraHosts []string
	// Optional config-file rendering: an init container renders a config file (from a
	// ConfigMap template with secret placeholders) into a shared volume the app reads via
	// CONFIG_PATH. initContainers/volumes are added to the pod; volumeMounts to the app container.
	initContainers []corev1.Container
	volumes        []corev1.Volume
	volumeMounts   []corev1.VolumeMount
	// securityContext (optional) — a POD-level securityContext, e.g. an fsGroup so a non-root
	// image can write its mounted volume. Composes with the per-CONTAINER hardening baseline
	// (hardenedSecurityContext) that reconcileApp applies to every container (#18).
	securityContext *corev1.PodSecurityContext
	// resources caps this app's container (requests + memory limit). nil → a light default
	// (resourceReqs) so every container still gets scheduler requests. Workload hardening (#18).
	resources *corev1.ResourceRequirements
	// gracePeriodSeconds is terminationGracePeriodSeconds. 0 → the stateless-app default (30s).
	// Stateful/slow-draining apps set a larger value. Workload hardening (#18).
	gracePeriodSeconds int64
	// credentialSecrets are the Secrets the app's containers, init containers included, read by
	// secretKeyRef (broker admin, cache auth, analytics key, master API key, ...). Their data hash is
	// on the pod template, so a changed credential rolls the app (#307).
	credentialSecrets []string
	// podAnnotations go on the pod template, so a change to one rolls the Deployment. An app whose
	// envFrom value only takes effect at process start uses a hash of it to roll on change.
	podAnnotations map[string]string
	// extraEnvFrom is appended after the operator's envFrom sources (envFromFor), so its keys win
	// over them; envHash, when set, is the pod-template annotation dashboardEnvHashAnnotation that
	// rolls the pods when a referenced Secret/ConfigMap changes. The dashboard's user env (#311).
	extraEnvFrom []corev1.EnvFromSource
	// envFrom replaces the main container's envFromFor default (the shared ConfigMap and the
	// component Secret) when set, for an app that must not load its component Secret whole.
	envFrom []corev1.EnvFromSource
	envHash string
}

// reconcileApp provisions a Deployment + Service (+ Ingress for ingressHost and ingressExtraHosts
// if ingressHost is set).
func (r *OramaCloudV1Reconciler) reconcileApp(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, def appDef) error {
	if _, err := r.ensureComponentSecret(ctx, oc, def.component); err != nil {
		return err
	}
	name := oc.Name + "-" + def.component
	lbls := labelsFor(oc, def.component)
	replicas := def.replicas
	if replicas == 0 {
		replicas = 1
	}
	// Workload hardening (#18): every container gets requests (scheduler needs them). nil → a light
	// default; heavy apps pass a bigger one (or the CR overrides it per component).
	res := def.resources
	if res == nil {
		res = resourceReqs("128Mi", "50m", "512Mi")
	}
	grace := def.gracePeriodSeconds
	if grace == 0 {
		grace = 30 // stateless-app default; a stateful app passes more
	}
	// Harden the operator's init/render/wait containers to the same baseline as the main container.
	initContainers := def.initContainers
	hardenAll(initContainers)
	envFrom := def.envFrom
	if envFrom == nil {
		envFrom = envFromFor(oc, def.component)
	}
	var annotations map[string]string
	if len(def.credentialSecrets) > 0 {
		hash, err := r.credentialsHash(ctx, oc, def.credentialSecrets...)
		if err != nil {
			return err
		}
		annotations = map[string]string{credentialsHashKey: hash}
	}
	for k, v := range def.podAnnotations {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[k] = v
	}
	// No env-hash annotation without a hash, so an app with no referenced env keeps its template.
	if def.envHash != "" {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[dashboardEnvHashAnnotation] = def.envHash
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(replicas)
		if def.recreate {
			dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		}
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		podSpec := corev1.PodSpec{
			ImagePullSecrets:              imagePullSecrets(oc),
			SecurityContext:               def.securityContext,
			InitContainers:                initContainers,
			Volumes:                       def.volumes,
			TerminationGracePeriodSeconds: int64Ptr(grace),
			Containers: []corev1.Container{{
				Name:  def.component,
				Image: def.image,
				// Dev image tags (e.g. v1-sandbox) are MUTABLE — the same tag is re-pushed on
				// each rebuild. Default IfNotPresent would keep a stale cached image, so pull
				// Always to pick up the latest build (build-manifest.tsv records the digest).
				ImagePullPolicy: corev1.PullAlways,
				Args:            def.args,
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: def.port}},
				Env:             def.env,
				EnvFrom:         append(envFrom, def.extraEnvFrom...),
				VolumeMounts:    def.volumeMounts,
				Resources:       *res,
				// Startup gates liveness/readiness so a slow boot isn't killed; readiness gates
				// Service routing; liveness restarts a hung pod (#18).
				StartupProbe:    tcpStartup(def.port),
				ReadinessProbe:  tcpReadiness(def.port),
				LivenessProbe:   tcpLiveness(def.port),
				SecurityContext: containerSecurityContext(def.port),
				// Drain from Service endpoints before exit so a rolling update doesn't drop requests.
				Lifecycle: drainPreStop(),
			}},
		}
		// Spread replicas across nodes when there's more than one (soft — a single-node sandbox
		// still schedules; a real cluster honors it).
		if replicas > 1 {
			podSpec.TopologySpreadConstraints = spreadConstraints(lbls)
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls, Annotations: annotations},
			Spec:       podSpec,
		}
		return controllerutil.SetControllerReference(oc, dep, r.Scheme)
	}); err != nil {
		return err
	}
	if err := r.upsertService(ctx, oc, name, def.component,
		[]corev1.ServicePort{{Name: "http", Port: def.port, TargetPort: intstr.FromInt32(def.port)}}); err != nil {
		return err
	}
	// PDB on every app (#18): a singleton blocks casual eviction (maxUnavailable 0 — node
	// maintenance handles it consciously); a replicated set rolls one at a time (maxUnavailable 1).
	maxUnavailable := 0
	if replicas > 1 {
		maxUnavailable = 1
	}
	if err := r.upsertPDB(ctx, oc, name, def.component, maxUnavailable); err != nil {
		return err
	}
	if def.ingressHost != "" {
		return r.upsertIngress(ctx, oc, name, def.component, ingressSpec(oc, name, def.ingressHost, def.ingressExtraHosts, def.port))
	}
	return nil
}

// ingressSpec is the spec of a component's Ingress named name: the stack's ingress class, one rule
// for host and then one per extra host in order, each with a Prefix / path to the Service name on
// port, and origin TLS for every host (originTLS). upsertIngress writes it; the engine's /metrics
// deny companions narrow the same spec (metricsBlockIngressSpec), so the two cannot drift apart.
func ingressSpec(oc *oramav1alpha1.OramaCloudV1, name, host string, extraHosts []string, port int32) networkingv1.IngressSpec {
	pathType := networkingv1.PathTypePrefix
	rules := make([]networkingv1.IngressRule, 0, 1+len(extraHosts))
	for _, h := range append([]string{host}, extraHosts...) {
		rules = append(rules, networkingv1.IngressRule{
			Host: h,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: name,
								Port: networkingv1.ServiceBackendPort{Number: port},
							},
						},
					}},
				},
			},
		})
	}
	return networkingv1.IngressSpec{
		// Bind to the helmfile-installed ingress-nginx controller. Without an ingressClassName the
		// controller ignores the Ingress (no ADDRESS assigned → nothing routes). Configurable via
		// spec.components.ingressClassName (default "nginx").
		IngressClassName: strPtr(ingressClassName(oc)),
		Rules:            rules,
		TLS:              originTLS(rules),
	}
}

// upsertIngress writes a component's Ingress from spec (ingressSpec). The rules are replaced whole,
// so a removed extra host loses its route and certificate, and applyIngressTLS requests origin TLS
// from the stack's Issuer for every host. It fails, leaving any existing Ingress as it is, while the
// stack has no Cloudflare token for the Issuer.
func (r *OramaCloudV1Reconciler) upsertIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component string, spec networkingv1.IngressSpec) error {
	issuer, err := stackIngressIssuer(oc)
	if err != nil {
		return err
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = labelsFor(oc, component)
		ing.Spec.IngressClassName = spec.IngressClassName
		ing.Spec.Rules = spec.Rules
		// Origin TLS from the stack's Issuer, after the rules: every host gets a certificate (#457).
		if err := applyIngressTLS(ing, issuer); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(oc, ing, r.Scheme)
	})
	return err
}

// validateIngressHosts refuses Ingress host settings that would make two rules claim one name or
// leave extra hosts without the primary host they alias: an extra host without host, an empty or
// over-long extra host, and any name (case-insensitive) repeated across the host and extraHosts of
// dashboard, docs, oramasearchApp, engine.writer and engine.reader, enabled or not. Reconcile calls
// it before writing any child resource. The error names both conflicting field paths.
// dnsHostName is the CRD's pattern for host and each extraHosts entry: a lowercase DNS name with at
// least one dot.
var dnsHostName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

func validateIngressHosts(oc *oramav1alpha1.OramaCloudV1) error {
	c := oc.Spec.Components
	claimed := map[string]string{}
	for _, endpoint := range []struct {
		path       string
		host       string
		extraHosts []string
	}{
		{"spec.components.dashboard", c.Dashboard.Host, c.Dashboard.ExtraHosts},
		{"spec.components.docs", c.Docs.Host, c.Docs.ExtraHosts},
		{"spec.components.oramasearchApp", c.OramasearchApp.Host, c.OramasearchApp.ExtraHosts},
		{"spec.components.engine.writer", c.Engine.Writer.Host, c.Engine.Writer.ExtraHosts},
		{"spec.components.engine.reader", c.Engine.Reader.Host, c.Engine.Reader.ExtraHosts},
	} {
		if endpoint.host == "" {
			if len(endpoint.extraHosts) > 0 {
				return fmt.Errorf("%s.extraHosts requires %s.host", endpoint.path, endpoint.path)
			}
			continue
		}
		names := append([]string{endpoint.host}, endpoint.extraHosts...)
		for i, name := range names {
			path := endpoint.path + ".host"
			if i > 0 {
				path = fmt.Sprintf("%s.extraHosts[%d]", endpoint.path, i-1)
				if name == "" || len(name) > 253 {
					return fmt.Errorf("%s must be 1 to 253 characters, got %d", path, len(name))
				}
			}
			key := strings.ToLower(name)
			if first, ok := claimed[key]; ok {
				return fmt.Errorf("%s and %s are both %q", first, path, name)
			}
			// The CRD refuses this too; checked here for objects that bypassed admission, before any
			// Ingress or certificate is written for the name.
			if i > 0 && !dnsHostName.MatchString(name) {
				return fmt.Errorf("%s %q must be a lowercase DNS name with at least one dot", path, name)
			}
			claimed[key] = path
		}
	}
	return nil
}

// ---- workers + dashboard reconcilers ----

// The dashboard variables the operator derives (#572). NEXT_PUBLIC_APP_URL takes effect at runtime
// only in an image built without it: next build inlines a NEXT_PUBLIC_* value set at build time.
const (
	dashboardAppURLEnv           = "NEXT_PUBLIC_APP_URL"
	dashboardPlacementClusterEnv = "ORAMACORE_PLACEMENT_CLUSTER_ID"
)

func (r *OramaCloudV1Reconciler) reconcileWorkers(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	w := oc.Spec.Components.Workers
	if !w.Enabled {
		return r.teardownByComponent(ctx, oc, "workers")
	}
	masterKey, ok := masterAPIKeySecret(oc)
	if !ok {
		return nil
	}
	img := w.Image
	if img == "" {
		img = componentImage(oc, "workers", "orama-workers") // one bundled image for all CF workers
	}
	// Third-party keys from their own Secrets (#571): the LLM providers' apiKeySecretRefs and
	// sentryDSNSecretRef. Checked before the Deployment is touched; each wins over the workers
	// Secret's envFrom, and a changed one rolls the workers.
	refs := componentKeyRefs(oc, "workers")
	if err := r.checkKeyRefs(ctx, oc, refs); err != nil {
		return err
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: "workers", image: img, port: 8080, replicas: w.Replicas,
		env:               append([]corev1.EnvVar{masterAPIKeyEnv(oc)}, keyRefEnv(refs)...),
		credentialSecrets: append([]string{masterKey}, keyRefSecrets(refs)...),
		resources:         resourcesOr(w.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}

func (r *OramaCloudV1Reconciler) reconcileDashboard(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	// Cron cleanup comes first, so a missing or invalid environment never keeps disabled crons running.
	if !d.Enabled || !d.Crons.Enabled {
		if err := r.teardownDashboardCrons(ctx, oc); err != nil {
			return err
		}
	}
	if !d.Enabled {
		if err := r.teardownByComponent(ctx, oc, "dashboard"); err != nil {
			return err
		}
		if d.Crons.Enabled { // admission refuses this; an object that bypassed it is reported
			return errCronsNeedDashboard
		}
		return nil
	}
	masterKey, ok := masterAPIKeySecret(oc)
	if !ok {
		return nil
	}
	// The user's env (dashboard_env.go) is validated, read and hashed before the Deployment is
	// touched, so a refused variable or a missing reference leaves the last valid template running.
	if err := validateDashboardEnv(oc); err != nil {
		return err
	}
	sources, err := r.dashboardEnvSources(ctx, oc)
	if err != nil {
		return err
	}
	envHash, err := r.dashboardEnvHash(ctx, oc, sources)
	if err != nil {
		return err
	}
	// The dashboard signs in to the control DB as its own role (#99): until that role's grants
	// apply, the Deployment is left as it is.
	if ready, err := r.dashboardRoleReady(ctx, oc); err != nil || !ready {
		return err
	}
	img := componentImage(oc, "dashboard", "dashboard")
	// The master key (#307), the dashboard role's control-DB credentials (#99),
	// and, where the cache runs, the serverless-redis-http shim's URL and token for its Upstash
	// client (#562), then the derived values below; then the user's dashboard.env (#311), which may
	// replace any but the owned ones.
	env := append([]corev1.EnvVar{masterAPIKeyEnv(oc)}, dashboardDBEnv(oc)...)
	credentialSecrets := append([]string{masterKey}, dashboardDBSecrets(oc)...)
	if redisHTTPRuns(oc) {
		env = append(env, redisCacheEnv(oc)...)
		credentialSecrets = append(credentialSecrets, redisHTTPSecretName(oc))
	}
	if oc.Spec.Cloudflare.Enabled {
		env = append(env, dashboardUploadsBucketEnv(oc)...)
		credentialSecrets = append(credentialSecrets, oc.Name+"-uploads-r2")
	}
	// Derived values (#572): the public origin from the stack's own host (never an extra host), and
	// the c1 row the cluster-row Job recorded, so new projects go to it. Both are explicit entries, so
	// a value left in a dashboard.envFrom source is ignored; dashboard.env may still replace them.
	if d.Host != "" {
		env = append(env, corev1.EnvVar{Name: dashboardAppURLEnv, Value: "https://" + d.Host})
	}
	rowID, err := r.clusterRowID(ctx, oc)
	if err != nil {
		return err
	}
	if rowID != "" {
		env = append(env, corev1.EnvVar{Name: dashboardPlacementClusterEnv, Value: rowID})
	}
	// The dashboard never migrates the control DB: the operator's PostgresSchema owns the
	// schema (operator#40). A second Drizzle migrate collides with those objects (Postgres 42710).
	if err := r.reconcileApp(ctx, oc, appDef{
		component:         "dashboard",
		image:             img,
		port:              3000,
		ingressHost:       d.Host,
		ingressExtraHosts: d.ExtraHosts,
		env:               overrideEnv(env, d.Env),
		credentialSecrets: credentialSecrets,
		extraEnvFrom:      d.EnvFrom,
		envHash:           envHash,
		// Next.js SSR — a roomier default than the light apps. Override via spec.components.dashboard.resources.
		resources: resourcesOr(d.Resources, resourceReqs("256Mi", "100m", "1Gi")),
		// The dashboard reads JWT_ISSUER from the shared ConfigMap only at start: roll it when the
		// effective issuer changes, so it signs with the issuer the writer verifies.
		podAnnotations: map[string]string{jwtIssuerHashKey: jwtIssuerHash(oc)},
	}); err != nil {
		return err
	}
	// The crons call the dashboard Service written above (dashboard_crons.go).
	if !d.Crons.Enabled {
		return nil
	}
	return r.reconcileDashboardCrons(ctx, oc, sources)
}

// jwtIssuerHashKey is the dashboard pod-template annotation holding jwtIssuerHash.
const jwtIssuerHashKey = "orama.cloud/jwt-issuer-hash"

// jwtIssuerHash is the SHA-256 of the effective issuer, jwtIssuer(oc).
func jwtIssuerHash(oc *oramav1alpha1.OramaCloudV1) string {
	sum := sha256.Sum256([]byte(jwtIssuer(oc)))
	return hex.EncodeToString(sum[:])
}

// reconcileDocs provisions docs.orama.com (Next.js + Fumadocs) as a Deployment + Service
// (+ Ingress if a host is set). No component secret — the docs site has no env coupling.
func (r *OramaCloudV1Reconciler) reconcileDocs(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Docs
	if !d.Enabled {
		return r.teardownByComponent(ctx, oc, "docs")
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:         "docs",
		image:             componentImage(oc, "docs", "docs"),
		port:              3000,
		ingressHost:       d.Host,
		ingressExtraHosts: d.ExtraHosts,
		resources:         resourcesOr(d.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}
