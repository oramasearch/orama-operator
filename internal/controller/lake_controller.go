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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// LakeReconciler reconciles a Lake object into a Namespace, a Deployment and a
// Service. Hello-world stage: the Deployment runs whatever spec.image says, which
// defaults to an HTTP echo until the lake binary exists.
type LakeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	lakeEchoPort   = 5678 // hashicorp/http-echo's listen port
	lakeServicePort = 80
)

// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings the cluster to the state described by the Lake.
func (r *LakeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, req.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ns := lake.Spec.Namespace
	if ns == "" {
		ns = "lake-dev"
	}
	image := lake.Spec.Image
	if image == "" {
		image = "hashicorp/http-echo:1.0"
	}
	replicas := int32(1)
	if lake.Spec.Replicas != nil {
		replicas = *lake.Spec.Replicas
	}

	// Namespace: create if missing. Not owner-ref'd — a namespace outliving the Lake
	// is safer than a Lake deletion taking a namespace with it.
	if err := r.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{}); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create namespace %q: %w", ns, err)
		}
		log.Info("created namespace", "namespace", ns)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "lake",
		"app.kubernetes.io/instance":   lake.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
	name := "lake-" + lake.Name

	// Deployment.
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = &replicas
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Template.ObjectMeta.Labels = labels
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "lake",
			Image: image,
			Args:  []string{"-listen=:5678", "-text=hello from lake " + lake.Name},
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: lakeEchoPort}},
		}}
		return controllerutil.SetControllerReference(&lake, deploy, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}

	// Service.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "http", Port: lakeServicePort, TargetPort: intstr.FromInt(lakeEchoPort),
		}}
		return controllerutil.SetControllerReference(&lake, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	// Status: mirror the Deployment, and only write when it changed (anti-flap).
	phase := "Pending"
	if deploy.Status.ReadyReplicas >= replicas && replicas > 0 {
		phase = "Ready"
	}
	if lake.Status.ReadyReplicas != deploy.Status.ReadyReplicas || lake.Status.Phase != phase {
		lake.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		lake.Status.Phase = phase
		if err := r.Status().Update(ctx, &lake); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Lake{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("lake").
		Complete(r)
}
