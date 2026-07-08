/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

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
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the control loop. TODO: build out the per-component reconcilers
// (engine writer/readers/envoy, control-plane Postgres via CNPG, ClickHouse,
// RabbitMQ, Redis, Typesense, HARVEST+Mongo, index-builder + workers as Knative
// Services, DISCO, dashboard, IdP shim, demo seed) driven by .spec.region.role.
func (r *OramaCloudV1Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var oc oramav1alpha1.OramaCloudV1
	if err := r.Get(ctx, req.NamespacedName, &oc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("reconciling OramaCloudV1", "region", oc.Spec.Region.Name, "role", oc.Spec.Region.Role)

	// TODO(scaffold): reconcile child components here. See DESIGN.md.

	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller.
func (r *OramaCloudV1Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1{}).
		Complete(r)
}
