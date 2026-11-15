/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// OramaCloudV1Reconciler reconciles an OramaCloudV1 object into the full v1 stack.
type OramaCloudV1Reconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	APIReader         client.Reader
	OperatorNamespace string
	Recorder          record.EventRecorder
}

// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets;persistentvolumeclaims;serviceaccounts;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1migrations,verbs=get;list;watch

// Reconcile provisions enabled components in dependency order, applies storage intent, and
// rolls up workload readiness independently of storage configuration acceptance.
func (r *OramaCloudV1Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	l := log.FromContext(ctx)

	var oc oramav1alpha1.OramaCloudV1
	if err := r.Get(ctx, req.NamespacedName, &oc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("reconciling OramaCloudV1", "region", oc.Spec.Region.Name, "role", oc.Spec.Region.Role, "profile", oc.Spec.Profile)
	// A disabled or invalid analytics import loses its schedule first, before any other check or
	// component step can end the reconcile early and leave it running (guardAnalyticsImport).
	if err := r.guardAnalyticsImport(ctx, &oc); err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "analytics-import", err)
	}
	// Invalid input changes nothing else: check it before the first child resource is written.
	if err := validateIngressHosts(&oc); err != nil {
		// Only a spec change can fix it, and that triggers a reconcile: report it, don't requeue.
		r.setStatus(ctx, &oc, "Degraded", oc.Status.ComponentsReady, oc.Status.Components, nil, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "InvalidIngressHosts", Message: err.Error(),
		})
		return ctrl.Result{}, nil
	}
	if err := validateEngineImages(&oc); err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "engine", err)
	}
	// A volume that cannot reach its declared storage in place, and whose storage approves data
	// deletion, is replaced before any component is reconciled again, so nothing is created on a
	// claim being deleted (#915).
	proceed, err := r.reconcileVolumeReplacement(ctx, &oc)
	if err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "volume-replacement", err)
	}
	if !proceed {
		return ctrl.Result{RequeueAfter: volumeReplacementRequeue}, nil
	}
	// A missing or placeholder master key is configuration, not a failure: the rest of the stack
	// reconciles, the engine is held at zero (reconcileEngine), and Ready names the cause until a
	// Secret event or the readiness requeue finds it fixed.
	masterKey, err := r.validateMasterAPIKey(ctx, &oc)
	if err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "master API key", err)
	}
	if err := r.reconcileNetworkSandbox(ctx, &oc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile network sandbox: %w", err)
	}

	// Keep the shared reconciler immutable: simultaneous reconciles may use different policies.
	var rejectedImages []string
	imagesReported := false
	local := *r
	r = &local
	if oc.Spec.Images.RequireDigests {
		r.Client = imageEnforcingClient{Client: r.Client, violations: &rejectedImages}
	}
	defer func() {
		if imagesReported {
			return
		}
		if err := r.reportCloudV1Images(ctx, &oc, rejectedImages...); err != nil && reconcileErr == nil {
			reconcileErr = r.reconcileFailed(ctx, &oc, "images", err)
		}
	}()

	// Reconcile each component in order. Add new component reconcilers to this list.
	steps := []struct {
		name string
		fn   func(context.Context, *oramav1alpha1.OramaCloudV1) error
	}{
		{"ephemeral-storage", r.reconcileV1StorageDefaults},
		{"bootstrap", r.reconcileBootstrap},    // shared config + generated credentials first
		{"originTLS", r.reconcileOriginIssuer}, // the Issuer every Ingress below requests from (#457)
		{"engine", r.reconcileEngine},
		{"controlDB", r.reconcileControlDB},
		{"pgadmin", r.reconcilePgAdmin}, // pgAdmin4 web (admin UI for the control-DB)
		{"cache", r.reconcileCache},
		{"redis-http", r.reconcileRedisHTTP}, // the Upstash REST shim in front of the cache (#562)
		{"broker", r.reconcileBroker},
		{"analytics", r.reconcileAnalytics},
		{"search", r.reconcileSearch},
		{"hpes", r.reconcileHPES}, // index-builder's embeddings server, on CPU, no public host (#574)
		{"ingestion", r.reconcileIngestion},
		{"workers", r.reconcileWorkers},
		{"dashboard", r.reconcileDashboard},
		{"docs", r.reconcileDocs},
		{"disco", r.reconcileDisco},
		{"analytics-processor", r.reconcileAnalyticsProcessor},
		// oramasearch-app (api.askorama.ai, the legacy "old dashboard") is REMOVED — it just
		// redirects to the new app, so we don't run it in-cluster (this also moots the MySQL-vs-
		// CNPG mismatch, operator#35). The reconciler + toggle remain for reference but are unwired.
		{"search-workers", r.reconcileSearchWorkers},
		{"idp", r.reconcileIdP},
		{"cloudflare", r.reconcileCloudflare},
		{"backups", r.reconcileV1Backups}, // the backup bucket and schedules (operator#740)
		{"demoSeed", r.reconcileDemoSeed},
		// Scrape configuration, alert rules and dashboards on the platform's kube-prometheus-stack
		// and Perses (operator#15); a cluster without them yet is not an error.
		{"monitoring", r.reconcileMonitoring},
		// Last, so a configuration error in it (a missing AWS Secret) never keeps another component
		// from being reconciled (operator#446).
		{"import-consumer", r.reconcileImportConsumer},
	}
	for _, s := range steps {
		if err := s.fn(ctx, &oc); err != nil {
			return ctrl.Result{}, r.reconcileFailed(ctx, &oc, s.name, err)
		}
	}

	// Roll child readiness up into the phase (#37): Ready once every owned workload is ready,
	// else Provisioning. Requeue while still coming up so the phase advances as pods go ready —
	// Owns() events cover most transitions; the requeue is also the backstop for CNPG readiness,
	// PVC creation and storage retries even after workloads become Ready.
	managedStorage := r.reconcileStorage(ctx, &oc)
	// Enforced legacy image failures must precede readiness; otherwise each pass would flip a
	// steady stack from Ready/Provisioning to Degraded and back before reporting the same error.
	imagesReported = true
	if err := r.reportCloudV1Images(ctx, &oc, rejectedImages...); err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "images", err)
	}
	if r.applyReadiness(ctx, &oc, masterKey) && !managedStorage {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
}

// reconcileFailed reports a component's reconcile error as Ready=False (ReconcileError) with phase
// Degraded, leaving the other conditions as they were, and returns the error.
func (r *OramaCloudV1Reconciler) reconcileFailed(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string, err error) error {
	log.FromContext(ctx).Error(err, "component reconcile failed", "component", component)
	r.setStatus(ctx, oc, "Degraded", oc.Status.ComponentsReady, oc.Status.Components, nil, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "ReconcileError",
		Message: fmt.Sprintf("%s: %v", component, err),
	})
	return err
}

// SetupWithManager wires the controller and the child resources it owns.
func (r *OramaCloudV1Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("oramacloudv1")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.LimitRange{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&oramav1alpha1.CloudflareKVNamespace{}).
		Owns(&oramav1alpha1.CloudflareR2Bucket{}).
		Owns(&oramav1alpha1.BackupSchedule{}).
		Owns(&oramav1alpha1.NetworkSandbox{}).
		Owns(&oramav1alpha1.RabbitMQTopology{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		// Secrets supplied or copied from outside the operator (LLM keys, the master API key, a
		// broker or cache credential) have no owner reference: map them to the CRs that read them,
		// so a changed value rolls their consumers (requestsForSecret).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		// Likewise the Secrets and ConfigMaps the dashboard takes its environment from, so a changed,
		// created or deleted one re-hashes and rolls the dashboard (dashboard_env.go).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForDashboardEnv)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.requestsForDashboardEnv)).
		// And the e2e canary's target Secret, so creating or fixing it retries at once (dashboard_crons.go).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForE2ECanarySecret)).
		// The migrations that target a stack turn its migration role's login and its Prod 1 egress on
		// and off (operator#653).
		Watches(&oramav1alpha1.OramaCloudV1Migration{}, handler.EnqueueRequestsFromMapFunc(r.requestsForMigration)).
		Complete(r)
}
