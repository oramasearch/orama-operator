/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// OramaCloudV1Reconciler reconciles an OramaCloudV1 object into the full v1 stack.
type OramaCloudV1Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	// Reconcile each component in order. Add new component reconcilers to this list.
	steps := []struct {
		name string
		fn   func(context.Context, *oramav1alpha1.OramaCloudV1) error
	}{
		{"bootstrap", r.reconcileBootstrap}, // shared config + dummy secrets first
		{"engine", r.reconcileEngine},
		{"controlDB", r.reconcileControlDB},
		{"cache", r.reconcileCache},
		{"broker", r.reconcileBroker},
		{"analytics", r.reconcileAnalytics},
		{"search", r.reconcileSearch},
		{"ingestion", r.reconcileIngestion},
		{"workers", r.reconcileWorkers},
		{"dashboard", r.reconcileDashboard},
		{"docs", r.reconcileDocs},
		{"disco", r.reconcileDisco},
		{"idp", r.reconcileIdP},
		{"cloudflare", r.reconcileCloudflare},
		{"demoSeed", r.reconcileDemoSeed},
	}
	for _, s := range steps {
		if err := s.fn(ctx, &oc); err != nil {
			l.Error(err, "component reconcile failed", "component", s.name)
			r.setPhase(ctx, &oc, "Degraded")
			return ctrl.Result{}, err
		}
	}

	r.setPhase(ctx, &oc, "Provisioning")
	return ctrl.Result{}, nil
}

// setPhase best-effort updates the CR's status phase.
func (r *OramaCloudV1Reconciler) setPhase(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, phase string) {
	if oc.Status.Phase == phase {
		return
	}
	oc.Status.Phase = phase
	oc.Status.ObservedGeneration = oc.Generation
	if err := r.Status().Update(ctx, oc); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "phase", phase)
	}
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
		Complete(r)
}
