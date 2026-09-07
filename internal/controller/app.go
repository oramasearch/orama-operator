/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

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
	component   string
	image       string
	port        int32
	replicas    int32
	env         []corev1.EnvVar
	args        []string
	ingressHost string
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
}

// reconcileApp provisions a Deployment + Service (+ Ingress if ingressHost set).
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

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(replicas)
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
				EnvFrom:         envFromFor(oc, def.component),
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
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
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
		return r.upsertIngress(ctx, oc, name, def.component, def.ingressHost, def.port)
	}
	return nil
}

func (r *OramaCloudV1Reconciler) upsertIngress(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component, host string, port int32) error {
	lbls := labelsFor(oc, component)
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		// Bind to the helmfile-installed ingress-nginx controller. Without an ingressClassName the
		// controller ignores the Ingress (no ADDRESS assigned → nothing routes). Configurable via
		// spec.ingressClassName (default "nginx").
		ing.Spec.IngressClassName = strPtr(ingressClassName(oc))
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
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
		}}
		return controllerutil.SetControllerReference(oc, ing, r.Scheme)
	})
	return err
}

// ---- workers + dashboard reconcilers ----

func (r *OramaCloudV1Reconciler) reconcileWorkers(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	w := oc.Spec.Components.Workers
	if !w.Enabled {
		return r.teardownByComponent(ctx, oc, "workers")
	}
	img := w.Image
	if img == "" {
		img = componentImage(oc, "workers", "orama-workers") // one bundled image for all CF workers
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: "workers", image: img, port: 8080, replicas: w.Replicas,
		resources: resourcesOr(w.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}

func (r *OramaCloudV1Reconciler) reconcileDashboard(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	if !d.Enabled {
		return r.teardownByComponent(ctx, oc, "dashboard")
	}
	img := componentImage(oc, "dashboard", "dashboard")
	// Schema ownership: when the operator manages the control DB in-cluster it also owns the schema —
	// reconcileControlDBMigrations applies the SAME Drizzle schema via an owned PostgresSchema Job
	// (operator#40, supersedes the #34 dashboard init). Running BOTH double-applies the schema and the
	// dashboard's drizzle-kit migrate then dies on the objects PostgresSchema already created (Postgres
	// 42710, "type already exists") — it can't tell, because PostgresSchema applies raw SQL and doesn't
	// touch `__drizzle_migrations`. So only run the dashboard's own db-migrate when the operator does
	// NOT own the schema: an EXTERNAL control DB (no operator-managed PostgresSchema).
	cdb := oc.Spec.Components.ControlDB
	operatorOwnsSchema := cdb.Enabled && cdb.Mode != "external"
	var initContainers []corev1.Container
	if !operatorOwnsSchema {
		// External control DB — the operator applies no PostgresSchema, so the dashboard applies its
		// own Drizzle migrations before it serves (#34). GUARDED as a safe no-op on an image that
		// doesn't ship the migrations (`.drizzle` + `drizzle.config.ts`).
		initContainers = []corev1.Container{{
			Name:    "db-migrate",
			Image:   img,
			Command: []string{"sh", "-c", `if [ -f drizzle.config.ts ] && [ -d .drizzle ]; then npm run db:migrate; else echo "dashboard image ships no migrations yet (operator#34) — skipping"; fi`},
			Env:     []corev1.EnvVar{pgURIEnv(oc, "DATABASE_URL")},
		}}
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "dashboard",
		image:       img,
		port:        3000,
		ingressHost: d.Host,
		// Real control-DB password from the CNPG secret (#32) + DATABASE_URL for Drizzle (#34).
		env:            []corev1.EnvVar{pgPasswordEnv(oc, "POSTGRES_PASSWORD"), pgURIEnv(oc, "DATABASE_URL")},
		initContainers: initContainers,
		// Next.js SSR — a roomier default than the light apps. Override via spec.components.dashboard.resources.
		resources: resourcesOr(d.Resources, resourceReqs("256Mi", "100m", "1Gi")),
	})
}

// reconcileDocs provisions docs.orama.com (Next.js + Fumadocs) as a Deployment + Service
// (+ Ingress if a host is set). No component secret — the docs site has no env coupling.
func (r *OramaCloudV1Reconciler) reconcileDocs(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Docs
	if !d.Enabled {
		return r.teardownByComponent(ctx, oc, "docs")
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "docs",
		image:       componentImage(oc, "docs", "docs"),
		port:        3000,
		ingressHost: d.Host,
		resources:   resourcesOr(d.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}
