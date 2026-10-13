/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	oramaOperatorFinalizer = "orama.cloud/leaf-operator-cleanup"

	operatorPhaseInstalling = "Installing"

	defaultLeafOperatorNamespace = "orama-system"
	defaultLeafImageRepository   = "oramasearch/orama-operator"
	defaultLeafChartRepository   = "registry-1.docker.io/oramasearch/orama-operator-chart"

	// While the leaf's operator is starting it is looked at often; once ready, occasionally, to
	// notice a pod that stopped being ready or a release changed by hand.
	leafOperatorPollInterval  = 15 * time.Second
	leafOperatorSteadyRecheck = 5 * time.Minute
)

// OramaOperatorReconciler installs the Orama operator onto a cluster this one manages and keeps
// it at the version its resource names (operator#363). The management cluster only.
type OramaOperatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Installer reaches the leaf. Nil means the Helm installer; tests inject a fake.
	Installer leafOperatorInstaller
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramaoperators,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramaoperators/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramaoperators/finalizers,verbs=update
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *OramaOperatorReconciler) installer() leafOperatorInstaller {
	if r.Installer == nil {
		r.Installer = newHelmLeafInstaller()
	}
	return r.Installer
}

func (r *OramaOperatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var operator oramav1alpha1.OramaOperator
	if err := r.Get(ctx, req.NamespacedName, &operator); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := operator.Status.DeepCopy()
	namespace := operator.Spec.Namespace
	if namespace == "" {
		namespace = defaultLeafOperatorNamespace
	}

	if !operator.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &operator, observed, namespace)
	}

	// Registered before anything is installed on the leaf.
	if !controllerutil.ContainsFinalizer(&operator, oramaOperatorFinalizer) {
		controllerutil.AddFinalizer(&operator, oramaOperatorFinalizer)
		if err := r.Update(ctx, &operator); err != nil {
			return settle(ctrl.Result{}, err)
		}
	}

	kubeconfig, waiting, err := r.clusterKubeconfig(ctx, operator.Spec.ClusterRef)
	if err != nil {
		return r.fail(ctx, &operator, observed, "ClusterUnreadable", err)
	}
	if waiting != "" {
		// Not an error: the cluster is still being created. The watch on KubernetesCluster brings
		// this resource back when the cluster changes.
		setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
			operator.Generation, clusterPhasePending, "WaitingForCluster", waiting)
		return settle(ctrl.Result{}, r.writeStatus(ctx, &operator, observed))
	}

	pullSecret := &corev1.Secret{}
	ref := operator.Spec.ImagePullSecretRef
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, pullSecret); err != nil {
		if apierrors.IsNotFound(err) {
			err = fmt.Errorf("image pull Secret %s/%s does not exist on this cluster", ref.Namespace, ref.Name)
		}
		return r.fail(ctx, &operator, observed, "PullSecretUnavailable", err)
	}
	chartRepository := operator.Spec.ChartRepository
	if chartRepository == "" {
		chartRepository = defaultLeafChartRepository
	}
	chartRepository = strings.TrimPrefix(chartRepository, "oci://")
	user, pass, err := registryCredentials(pullSecret, strings.SplitN(chartRepository, "/", 2)[0])
	if err != nil {
		return r.fail(ctx, &operator, observed, "PullSecretUnusable", err)
	}

	var extra map[string]any
	if operator.Spec.Values != nil && len(operator.Spec.Values.Raw) > 0 {
		if err := json.Unmarshal(operator.Spec.Values.Raw, &extra); err != nil {
			return r.fail(ctx, &operator, observed, "InvalidValues", fmt.Errorf("spec.values must be an object: %w", err))
		}
	}
	imageRepository := operator.Spec.ImageRepository
	if imageRepository == "" {
		imageRepository = defaultLeafImageRepository
	}
	chartVersion := leafChartVersion(operator.Spec.Version)

	state, err := r.installer().Ensure(ctx, kubeconfig, leafOperatorRequest{
		Namespace:  namespace,
		Version:    operator.Spec.Version,
		Chart:      chartSource{Repository: chartRepository, Version: chartVersion, User: user, Pass: pass},
		Values:     leafOperatorValues(extra, imageRepository, operator.Spec.Version, pullSecret.Name),
		PullSecret: pullSecret,
	})
	observeLeafState(&operator.Status, state)
	if err != nil {
		return r.fail(ctx, &operator, observed, "InstallFailed", err)
	}
	operator.Status.ObservedGeneration = operator.Generation
	if len(state.MissingCRDs) > 0 {
		// Not installed, and not an error of this resource: this operator version watches kinds
		// the cluster does not have yet, and would not start. It waits for them.
		setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
			operator.Generation, clusterPhasePending, "WaitingForPrerequisites",
			"not installed: the cluster lacks CRDs this operator version needs before it can start ("+
				strings.Join(state.MissingCRDs, ", ")+"); install what provides them with a ClusterHelmRelease")
		return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.writeStatus(ctx, &operator, observed))
	}

	wantImage := imageRepository + ":" + operator.Spec.Version
	if state.Replicas > 0 && state.ReadyReplicas >= state.Replicas && state.Image == wantImage {
		setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
			operator.Generation, clusterPhaseReady, "Running", "")
		return settle(ctrl.Result{RequeueAfter: leafOperatorSteadyRecheck}, r.writeStatus(ctx, &operator, observed))
	}
	message := fmt.Sprintf("%d of %d operator pods are ready on the cluster", state.ReadyReplicas, state.Replicas)
	reason := "PodsNotReady"
	if state.PodProblem != "" {
		// The pod's own account of what is wrong, so it can be corrected from here.
		message += ": " + state.PodProblem
		reason = "PodsFailing"
	}
	setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
		operator.Generation, operatorPhaseInstalling, reason, message)
	return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.writeStatus(ctx, &operator, observed))
}

func observeLeafState(status *oramav1alpha1.OramaOperatorStatus, state leafOperatorState) {
	status.InstalledVersion = state.InstalledVersion
	status.ChartVersion = state.ChartVersion
	status.Revision = int32(state.Revision)
	status.ReleaseStatus = state.ReleaseStatus
	status.Image = state.Image
	status.Replicas = state.Replicas
	status.ReadyReplicas = state.ReadyReplicas
}

// clusterKubeconfig returns the admin kubeconfig of the named KubernetesCluster. When the cluster
// is not ready to be reached it returns a sentence saying what is being waited for.
func (r *OramaOperatorReconciler) clusterKubeconfig(ctx context.Context, name string) ([]byte, string, error) {
	return managedClusterKubeconfig(ctx, r.Client, name)
}

// managedClusterKubeconfig is shared by every controller that acts on a managed cluster.
func managedClusterKubeconfig(ctx context.Context, r client.Client, name string) ([]byte, string, error) {
	var cluster oramav1alpha1.KubernetesCluster
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "KubernetesCluster " + name + " does not exist", nil
		}
		return nil, "", err
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return nil, "KubernetesCluster " + name + " is being deleted", nil
	}
	ref := cluster.Status.KubeconfigSecretRef
	if cluster.Status.Phase != clusterPhaseReady || ref == nil {
		phase := cluster.Status.Phase
		if phase == "" {
			phase = clusterPhasePending
		}
		return nil, "KubernetesCluster " + name + " is " + phase + ", not Ready", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("kubeconfig Secret %s/%s of KubernetesCluster %s does not exist", ref.Namespace, ref.Name, name)
		}
		return nil, "", err
	}
	kubeconfig := sec.Data[kubeconfigSecretKey]
	if len(kubeconfig) == 0 {
		return nil, "", fmt.Errorf("kubeconfig Secret %s/%s has no %q key", ref.Namespace, ref.Name, kubeconfigSecretKey)
	}
	return kubeconfig, "", nil
}

// finalize uninstalls the operator from the leaf, then releases the resource. When the cluster is
// gone or going, or was never reachable, there is nothing to uninstall.
func (r *OramaOperatorReconciler) finalize(ctx context.Context, operator *oramav1alpha1.OramaOperator, observed *oramav1alpha1.OramaOperatorStatus, namespace string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(operator, oramaOperatorFinalizer) {
		return ctrl.Result{}, nil
	}
	kubeconfig, waiting, err := r.clusterKubeconfig(ctx, operator.Spec.ClusterRef)
	if err != nil {
		return r.fail(ctx, operator, observed, "ClusterUnreadable", err)
	}
	if waiting == "" {
		setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
			operator.Generation, clusterPhaseDeleting, "Uninstalling", "uninstalling the operator from the cluster")
		if err := r.installer().Remove(ctx, kubeconfig, namespace); err != nil {
			return r.fail(ctx, operator, observed, "UninstallFailed", err)
		}
	}
	controllerutil.RemoveFinalizer(operator, oramaOperatorFinalizer)
	return settle(ctrl.Result{}, r.Update(ctx, operator))
}

// fail records why a reconcile stopped and returns the error, so it is logged and retried.
func (r *OramaOperatorReconciler) fail(ctx context.Context, operator *oramav1alpha1.OramaOperator, observed *oramav1alpha1.OramaOperatorStatus, reason string, err error) (ctrl.Result, error) {
	phase := clusterPhaseDegraded
	if !operator.DeletionTimestamp.IsZero() {
		phase = clusterPhaseDeleting
	}
	setClusterStatus(&operator.Status.Phase, &operator.Status.Message, &operator.Status.Conditions,
		operator.Generation, phase, reason, err.Error())
	if statusErr := r.writeStatus(ctx, operator, observed); statusErr != nil {
		return settle(ctrl.Result{}, statusErr)
	}
	return ctrl.Result{}, err
}

// writeStatus patches the status only when it differs from what was read.
func (r *OramaOperatorReconciler) writeStatus(ctx context.Context, operator *oramav1alpha1.OramaOperator, observed *oramav1alpha1.OramaOperatorStatus) error {
	if reflect.DeepEqual(&operator.Status, observed) {
		return nil
	}
	// A patch of what changed, with no version precondition: this controller is the only writer
	// of this status, so a read that is one write behind must not make the write fail.
	base := operator.DeepCopy()
	base.Status = *observed.DeepCopy()
	return r.Status().Patch(ctx, operator, client.MergeFrom(base))
}

func (r *OramaOperatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A cluster becoming Ready (or going away) reconciles the operators installed onto it.
	forCluster := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var operators oramav1alpha1.OramaOperatorList
		if err := r.List(ctx, &operators); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, operator := range operators.Items {
			if operator.Spec.ClusterRef == obj.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: operator.Name}})
			}
		}
		return requests
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaOperator{}).
		Watches(&oramav1alpha1.KubernetesCluster{}, forCluster).
		Complete(r)
}
