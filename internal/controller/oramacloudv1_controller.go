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
}

// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets;persistentvolumeclaims;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the control loop. It reconciles each enabled component in turn,
// driven by .spec.region.role. Components still TODO: control-plane Postgres (CNPG),
// ClickHouse, RabbitMQ, Redis, Typesense, HARVEST+Mongo, index-builder + workers,
// DISCO, dashboard, IdP shim, Cloudflare (DNS+LB via API), demo seed. See docs/.
func (r *OramaCloudV1Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var oc oramav1alpha1.OramaCloudV1
	if err := r.Get(ctx, req.NamespacedName, &oc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("reconciling OramaCloudV1", "region", oc.Spec.Region.Name, "role", oc.Spec.Region.Role, "profile", oc.Spec.Profile)
	// Invalid input changes nothing: check it before the first child resource is written.
	if err := validateEngineImages(&oc); err != nil {
		return ctrl.Result{}, r.reconcileFailed(ctx, &oc, "engine", err)
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

	// Reconcile each component in order. Add new component reconcilers to this list.
	steps := []struct {
		name string
		fn   func(context.Context, *oramav1alpha1.OramaCloudV1) error
	}{
		{"bootstrap", r.reconcileBootstrap}, // shared config + generated credentials first
		{"engine", r.reconcileEngine},
		{"controlDB", r.reconcileControlDB},
		{"pgadmin", r.reconcilePgAdmin}, // pgAdmin4 web (admin UI for the control-DB)
		{"cache", r.reconcileCache},
		{"broker", r.reconcileBroker},
		{"analytics", r.reconcileAnalytics},
		{"search", r.reconcileSearch},
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
		{"demoSeed", r.reconcileDemoSeed},
	}
	for _, s := range steps {
		if err := s.fn(ctx, &oc); err != nil {
			return ctrl.Result{}, r.reconcileFailed(ctx, &oc, s.name, err)
		}
	}

	// Roll child readiness up into the phase (#37): Ready once every owned workload is ready,
	// else Provisioning. Requeue while still coming up so the phase advances as pods go ready —
	// Owns() events cover most transitions; the requeue is the backstop (e.g. the polled CNPG
	// cluster readiness).
	if r.applyReadiness(ctx, &oc, masterKey) {
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&batchv1.Job{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Owns(&oramav1alpha1.CloudflareKVNamespace{}).
		Owns(&oramav1alpha1.CloudflareR2Bucket{}).
		Owns(&oramav1alpha1.NetworkSandbox{}).
		Owns(&oramav1alpha1.RabbitMQTopology{}).
		Owns(&oramav1alpha1.PostgresSchema{}).
		// Secrets supplied or copied from outside the operator (LLM keys, the master API key, a
		// broker or cache credential) have no owner reference: map them to the CRs that read them,
		// so a changed value rolls their consumers (requestsForSecret).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Complete(r)
}
