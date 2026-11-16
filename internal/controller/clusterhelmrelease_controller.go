// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	clusterHelmReleaseFinalizer = "orama.cloud/cluster-helm-release-cleanup"
	clusterHelmReleaseWorkers   = 8
)

// ClusterHelmReleaseReconciler keeps one chart release on a managed cluster at its pinned version
// (operator#363). The management cluster only.
type ClusterHelmReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Installer reaches the cluster. Nil means the Helm installer; tests inject a fake.
	Installer leafReleaseInstaller
}

//+kubebuilder:rbac:groups=orama.cloud,resources=clusterhelmreleases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=clusterhelmreleases/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=clusterhelmreleases/finalizers,verbs=update
//+kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ClusterHelmReleaseReconciler) installer() leafReleaseInstaller {
	if r.Installer == nil {
		r.Installer = newHelmLeafInstaller()
	}
	return r.Installer
}

func (r *ClusterHelmReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rel oramav1alpha1.ClusterHelmRelease
	if err := r.Get(ctx, req.NamespacedName, &rel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observed := rel.Status.DeepCopy()

	if !rel.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &rel, observed)
	}
	if !controllerutil.ContainsFinalizer(&rel, clusterHelmReleaseFinalizer) {
		controllerutil.AddFinalizer(&rel, clusterHelmReleaseFinalizer)
		if err := r.Update(ctx, &rel); err != nil {
			return settle(ctrl.Result{}, err)
		}
	}

	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, rel.Spec.ClusterRef)
	if err != nil {
		return r.fail(ctx, &rel, observed, "ClusterUnreadable", err)
	}
	if waiting == "" {
		waiting, err = r.waitingOnDependency(ctx, &rel)
		if err != nil {
			return r.fail(ctx, &rel, observed, "DependencyUnreadable", err)
		}
	}
	if waiting != "" {
		// Not an error: the watches on KubernetesCluster and on the other releases bring this
		// one back when what it waits for changes.
		setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
			rel.Generation, clusterPhasePending, "Waiting", waiting)
		return settle(ctrl.Result{}, r.writeStatus(ctx, &rel, observed))
	}

	source := chartSource{Repository: rel.Spec.Chart.Repository, Name: rel.Spec.Chart.Name, Version: rel.Spec.Chart.Version}
	if ref := rel.Spec.Chart.CredentialsSecretRef; ref != nil {
		if source.User, source.Pass, err = r.chartCredentials(ctx, *ref, source); err != nil {
			return r.fail(ctx, &rel, observed, "CredentialsUnusable", err)
		}
	}
	var values map[string]any
	if rel.Spec.Values != nil && len(rel.Spec.Values.Raw) > 0 {
		if err := json.Unmarshal(rel.Spec.Values.Raw, &values); err != nil {
			return r.fail(ctx, &rel, observed, "InvalidValues", fmt.Errorf("spec.values must be an object: %w", err))
		}
	}

	var pullSecret *corev1.Secret
	if ref := rel.Spec.ImagePullSecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				err = fmt.Errorf("image pull Secret %s/%s does not exist on this cluster", ref.Namespace, ref.Name)
			}
			return r.fail(ctx, &rel, observed, "PullSecretUnavailable", err)
		}
		// The rule OramaOperator keeps for its pull Secret (orama-operator#832).
		scope, err := operatorSecretScope(ctx, r.Client, rel.Spec.ClusterRef)
		if err != nil {
			return r.fail(ctx, &rel, observed, "ClusterUnreadable", err)
		}
		copyName := firstNonEmpty(rel.Spec.ImagePullSecretAs, ref.Name)
		if refused := scope.refuse(oramav1alpha1.SetSecret{Name: ref.Name, Namespace: ref.Namespace}, copyName, secret); refused != nil {
			setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
				rel.Generation, clusterPhaseDegraded, "ProductionCredential", refused.Error())
			return settle(ctrl.Result{RequeueAfter: leafOperatorSteadyRecheck}, r.writeStatus(ctx, &rel, observed))
		}
		pullSecret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: copyName}, Type: secret.Type, Data: secret.Data}
	}

	state, err := r.installer().EnsureRelease(ctx, kubeconfig, helmReleaseRequest{
		Release: rel.Spec.ReleaseName, Namespace: rel.Spec.Namespace, Chart: source, Values: values,
		RequiredCRDs: rel.Spec.RequiredCRDs, CRDURLs: rel.Spec.CRDURLs, PullSecret: pullSecret,
	})
	rel.Status.ChartVersion, rel.Status.AppVersion = state.ChartVersion, state.AppVersion
	rel.Status.Revision, rel.Status.ReleaseStatus = int32(state.Revision), state.Status
	rel.Status.Workloads, rel.Status.ReadyWorkloads = int32(state.Workloads), int32(state.ReadyWorkloads)
	if err != nil {
		return r.fail(ctx, &rel, observed, "InstallFailed", err)
	}
	rel.Status.ObservedGeneration = rel.Generation

	switch {
	case len(state.MissingCRDs) > 0:
		setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
			rel.Generation, clusterPhasePending, "WaitingForPrerequisites",
			"not installed: the cluster lacks CRDs this chart needs first ("+strings.Join(state.MissingCRDs, ", ")+")")
		return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.writeStatus(ctx, &rel, observed))
	case len(state.NotReady) > 0:
		// Each workload that is not rolled out, and what its pods report, so it can be corrected
		// from here.
		setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
			rel.Generation, operatorPhaseInstalling, "WorkloadsNotReady", strings.Join(state.NotReady, "; "))
		return settle(ctrl.Result{RequeueAfter: leafOperatorPollInterval}, r.writeStatus(ctx, &rel, observed))
	}
	setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
		rel.Generation, clusterPhaseReady, "Deployed", "")
	return settle(ctrl.Result{RequeueAfter: leafOperatorSteadyRecheck}, r.writeStatus(ctx, &rel, observed))
}

// waitingOnDependency names the first dependency that is not Ready, or returns "".
func (r *ClusterHelmReleaseReconciler) waitingOnDependency(ctx context.Context, rel *oramav1alpha1.ClusterHelmRelease) (string, error) {
	names := append([]string{}, rel.Spec.DependsOn...)
	sort.Strings(names)
	for _, name := range names {
		if name == rel.Name {
			return "spec.dependsOn names this resource itself", nil
		}
		var dependency oramav1alpha1.ClusterHelmRelease
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &dependency); err != nil {
			if apierrors.IsNotFound(err) {
				return "ClusterHelmRelease " + name + ", which this depends on, does not exist", nil
			}
			return "", err
		}
		if dependency.Spec.ClusterRef != rel.Spec.ClusterRef {
			return "ClusterHelmRelease " + name + ", which this depends on, is for cluster " + dependency.Spec.ClusterRef + ", not " + rel.Spec.ClusterRef, nil
		}
		if dependency.Status.Phase != clusterPhaseReady || dependency.Status.ObservedGeneration != dependency.Generation {
			phase := dependency.Status.Phase
			if phase == "" {
				phase = clusterPhasePending
			}
			return "ClusterHelmRelease " + name + ", which this depends on, is " + phase + ", not Ready", nil
		}
	}
	return "", nil
}

// chartCredentials reads a private chart's credentials: a registry credentials Secret with an
// entry for the repository's host, or a Secret with username and password keys.
func (r *ClusterHelmReleaseReconciler) chartCredentials(ctx context.Context, ref oramav1alpha1.ClusterSecretReference, source chartSource) (string, string, error) {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("chart credentials Secret %s/%s does not exist on this cluster", ref.Namespace, ref.Name)
		}
		return "", "", err
	}
	if user, pass := string(sec.Data["username"]), string(sec.Data["password"]); user != "" && pass != "" {
		return user, pass, nil
	}
	return registryCredentials(sec, chartRepositoryHost(source.Repository))
}

func chartRepositoryHost(repository string) string {
	for _, prefix := range []string{"oci://", "https://", "http://"} {
		repository = strings.TrimPrefix(repository, prefix)
	}
	return strings.SplitN(repository, "/", 2)[0]
}

// finalize uninstalls the release, unless another release still depends on this one or the
// cluster is gone or going, in which case there is nothing to uninstall.
func (r *ClusterHelmReleaseReconciler) finalize(ctx context.Context, rel *oramav1alpha1.ClusterHelmRelease, observed *oramav1alpha1.ClusterHelmReleaseStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rel, clusterHelmReleaseFinalizer) {
		return ctrl.Result{}, nil
	}
	var all oramav1alpha1.ClusterHelmReleaseList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	var dependents []string
	for _, other := range all.Items {
		if other.Name == rel.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		for _, name := range other.Spec.DependsOn {
			if name == rel.Name {
				dependents = append(dependents, other.Name)
			}
		}
	}
	if len(dependents) > 0 {
		sort.Strings(dependents)
		setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
			rel.Generation, clusterPhaseDeleting, "InUse",
			"cannot be uninstalled while these depend on it: "+strings.Join(dependents, ", "))
		// A dependent being deleted brings this back through the watch.
		return settle(ctrl.Result{}, r.writeStatus(ctx, rel, observed))
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, r.Client, rel.Spec.ClusterRef)
	if err != nil {
		return r.fail(ctx, rel, observed, "ClusterUnreadable", err)
	}
	if waiting == "" {
		if err := r.installer().RemoveRelease(ctx, kubeconfig, rel.Spec.Namespace, rel.Spec.ReleaseName); err != nil {
			return r.fail(ctx, rel, observed, "UninstallFailed", err)
		}
	}
	controllerutil.RemoveFinalizer(rel, clusterHelmReleaseFinalizer)
	return settle(ctrl.Result{}, r.Update(ctx, rel))
}

// fail records why a reconcile stopped and returns the error, so it is logged and retried.
func (r *ClusterHelmReleaseReconciler) fail(ctx context.Context, rel *oramav1alpha1.ClusterHelmRelease, observed *oramav1alpha1.ClusterHelmReleaseStatus, reason string, err error) (ctrl.Result, error) {
	phase := clusterPhaseDegraded
	if !rel.DeletionTimestamp.IsZero() {
		phase = clusterPhaseDeleting
	}
	setClusterStatus(&rel.Status.Phase, &rel.Status.Message, &rel.Status.Conditions,
		rel.Generation, phase, reason, err.Error())
	if statusErr := r.writeStatus(ctx, rel, observed); statusErr != nil {
		return settle(ctrl.Result{}, statusErr)
	}
	return ctrl.Result{}, err
}

// writeStatus patches the status only when it differs from what was read.
func (r *ClusterHelmReleaseReconciler) writeStatus(ctx context.Context, rel *oramav1alpha1.ClusterHelmRelease, observed *oramav1alpha1.ClusterHelmReleaseStatus) error {
	if reflect.DeepEqual(&rel.Status, observed) {
		return nil
	}
	// A patch of what changed, with no version precondition: this controller is the only writer
	// of this status, so a read that is one write behind must not make the write fail.
	base := rel.DeepCopy()
	base.Status = *observed.DeepCopy()
	return r.Status().Patch(ctx, rel, client.MergeFrom(base))
}

func (r *ClusterHelmReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A cluster becoming Ready reconciles the releases installed onto it.
	forCluster := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var all oramav1alpha1.ClusterHelmReleaseList
		if err := r.List(ctx, &all); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, rel := range all.Items {
			if rel.Spec.ClusterRef == obj.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: rel.Name}})
			}
		}
		return requests
	})
	// A release changing reconciles those that depend on it, and those it depends on (which may
	// be waiting to be uninstalled).
	forRelease := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		changed, ok := obj.(*oramav1alpha1.ClusterHelmRelease)
		if !ok {
			return nil
		}
		var all oramav1alpha1.ClusterHelmReleaseList
		if err := r.List(ctx, &all); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, name := range changed.Spec.DependsOn {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		}
		for _, rel := range all.Items {
			for _, name := range rel.Spec.DependsOn {
				if name == changed.Name {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: rel.Name}})
				}
			}
		}
		return requests
	})
	// Several at once: an install can spend minutes in a chart's hooks, and one cluster's slow
	// release must not hold up every other release on every cluster.
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: clusterHelmReleaseWorkers}).
		For(&oramav1alpha1.ClusterHelmRelease{}).
		Watches(&oramav1alpha1.KubernetesCluster{}, forCluster).
		Watches(&oramav1alpha1.ClusterHelmRelease{}, forRelease).
		Complete(r)
}
