/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	pixieFinalizer      = "orama.cloud/pixie"
	pixieRequeue        = 5 * time.Minute
	pixieReadyCondition = "Ready"
	vizierCASecret      = "pixie-vizier-ca"
)

// PixieVizierReconciler runs Vizier on its own cluster from a PixieVizier the managing cluster
// wrote (operator#15, #456): the operator's certificates and signing key, the vendored Vizier with
// auto-update off, and a default-deny sandbox. It changes nothing until the managing cluster has
// delivered this cluster's own deploy key and the hub's CA.
type PixieVizierReconciler struct {
	client.Client
	// APIReader reads the Kubernetes API Service and Endpoints for the sandbox, uncached.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Now       func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=pixieviziers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=pixieviziers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=pixieviziers/finalizers,verbs=update
// +kubebuilder:rbac:groups=px.dev,resources=viziers,verbs=get;list;watch;create;update;patch;delete

func (r *PixieVizierReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PixieVizierReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *PixieVizierReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vz oramav1alpha1.PixieVizier
	if err := r.Get(ctx, req.NamespacedName, &vz); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !vz.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&vz, pixieFinalizer) {
			return ctrl.Result{}, nil
		}
		// Namespaced objects go with their owner; the cluster-scoped RBAC is removed here.
		if err := prunePixieObjects(ctx, r.Client, vz.Namespace, pixieManagedVizier, nil); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&vz, pixieFinalizer)
		return ctrl.Result{}, r.Update(ctx, &vz)
	}
	if !controllerutil.ContainsFinalizer(&vz, pixieFinalizer) {
		controllerutil.AddFinalizer(&vz, pixieFinalizer)
		if err := r.Update(ctx, &vz); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	status, alertingErr, err := r.reconcile(ctx, &vz)
	if statusErr := r.setStatus(ctx, &vz, status, alertingErr); statusErr != nil && err == nil {
		err = statusErr
	}
	return ctrl.Result{RequeueAfter: pixieRequeue}, err
}

func (r *PixieVizierReconciler) reconcile(ctx context.Context, vz *oramav1alpha1.PixieVizier) (oramav1alpha1.PixieVizierStatus, error, error) {
	if vz.Namespace != oramav1alpha1.PixieVizierNamespace {
		return pixieVizierStatus(oramav1alpha1.ObservabilityPhaseRefused, "a PixieVizier runs in namespace "+oramav1alpha1.PixieVizierNamespace), nil, nil
	}
	// The cluster's alert routing and rules first: they need nothing of Pixie, so a cluster pages
	// before its agents can run (the release, its key or the hub may still be missing).
	alertingErr := reconcileObservabilityAlerting(ctx, r.Client, r.Scheme, vz, vz.Spec.ClusterName, vz.Spec.AlertRoutes, vizierAlertRules(vz))
	if alertingErr != nil && !errors.Is(alertingErr, errMonitoringCRDsMissing) {
		return oramav1alpha1.PixieVizierStatus{}, nil, alertingErr
	}
	localHub := !isPeerImportAddress(vz.Spec.CloudAddress)
	// The sandbox first, so no pod here runs without it.
	policies, cilium, err := vizierSandboxPolicies(ctx, r.reader(), vz, localHub)
	if err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	if err := reconcilePixieSandbox(ctx, r.Client, r.Scheme, vz, policies, cilium); err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	// Then the hub's window onto this cluster's metrics, which like the alerting needs nothing of
	// Pixie: the hub reads a cluster whose agents cannot run yet.
	federation, err := r.reconcileMetricsFederation(ctx, vz, localHub)
	if err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	if vz.Spec.Release == nil {
		return pixieVizierStatus(oramav1alpha1.ObservabilityPhasePending, "waiting for an installable provider release (#455); alert routing and metrics federation are written"), alertingErr, nil
	}
	if problems := pixieReleaseProblems(vz.Spec.Release); len(problems) > 0 {
		return pixieVizierStatus(oramav1alpha1.ObservabilityPhaseDegraded, problems[0]), alertingErr, nil
	}
	for _, need := range []struct{ name, key, what string }{
		{vz.Spec.PullSecretName, corev1.DockerConfigJsonKey, "the registry credentials"},
		{vz.Spec.DeployKeySecretName, "deploy-key", "this cluster's deploy key"},
		{vz.Spec.CloudCASecretName, "ca.crt", "the hub's CA"},
	} {
		var secret corev1.Secret
		found, err := getIfExists(ctx, r.Client, vz.Namespace, need.name, &secret)
		if err != nil {
			return oramav1alpha1.PixieVizierStatus{}, nil, err
		}
		if !found || len(secret.Data[need.key]) == 0 {
			return pixieVizierStatus(oramav1alpha1.ObservabilityPhasePending,
				fmt.Sprintf("waiting for %s (Secret %s key %s), which the managing cluster writes", need.what, need.name, need.key)), alertingErr, nil
		}
	}
	certsHash, err := r.ensureSecrets(ctx, vz)
	if err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	objects, err := renderVizier(vizierRender{Vizier: vz, CertsHash: certsHash})
	if err != nil {
		return pixieVizierStatus(oramav1alpha1.ObservabilityPhaseDegraded, err.Error()), alertingErr, nil
	}
	if err := applyPixieObjects(ctx, r.Client, r.Scheme, vz, pixieManagedVizier, objects); err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	components, err := pixieWorkloadStatus(ctx, r.Client, r.reader(), vz.Namespace, append(objects, federation...))
	if err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	}
	phase, ready, message := summarizeComponents(components)
	status := oramav1alpha1.PixieVizierStatus{Phase: phase, Ready: ready, Components: components, Message: message}
	var cluster corev1.Secret
	if found, err := getIfExists(ctx, r.Client, vz.Namespace, vizierClusterSecret, &cluster); err != nil {
		return oramav1alpha1.PixieVizierStatus{}, nil, err
	} else if found {
		status.ClusterID = string(cluster.Data["cluster-id"])
	}
	return status, alertingErr, nil
}

// reconcileMetricsFederation keeps the read-only proxy a hub on another cluster federates this
// cluster's metrics through (observability_metrics.go), once the pull Secret its image needs is
// here, and removes it when the hub is on this cluster, which reads the Prometheus directly.
func (r *PixieVizierReconciler) reconcileMetricsFederation(ctx context.Context, vz *oramav1alpha1.PixieVizier, localHub bool) ([]client.Object, error) {
	var objects []client.Object
	if !localHub {
		var pull corev1.Secret
		found, err := getIfExists(ctx, r.Client, vz.Namespace, vz.Spec.PullSecretName, &pull)
		if err != nil {
			return nil, err
		}
		if found {
			if objects, err = metricsFederationObjects(); err != nil {
				return nil, err
			}
			for _, object := range objects {
				stampPixieObject(object, pixieManagedMetrics, vz.Namespace, vz.Spec.PullSecretName)
			}
		}
	}
	return objects, applyPixieObjects(ctx, r.Client, r.Scheme, vz, pixieManagedMetrics, objects)
}

// ensureSecrets mints Vizier's CA and service certificates and its signing key, and returns the
// hash that rolls the pods when a certificate or the hub's CA changes.
func (r *PixieVizierReconciler) ensureSecrets(ctx context.Context, vz *oramav1alpha1.PixieVizier) (string, error) {
	labels := map[string]string{pixieManagedLabel: pixieManagedVizier}
	now := r.now()
	ca, err := ensurePixieCA(ctx, r.Client, r.Scheme, vz, vizierCASecret, "Pixie Vizier CA "+vz.Spec.ClusterName, labels, now)
	if err != nil {
		return "", err
	}
	names := vizierDNSNames(vz.Namespace)
	if _, err := ensurePixieLeaf(ctx, r.Client, r.Scheme, vz, ca, pixieLeaf{
		Secret: vizierServiceTLSSecret, CertKey: "server.crt", KeyKey: "server.key", CAKey: "ca.crt",
		Request: tunnelLeafRequest{CommonName: "pixie-vizier", DNSNames: names, Server: true, Client: true},
	}, labels, now); err != nil {
		return "", err
	}
	certs, err := ensurePixieLeaf(ctx, r.Client, r.Scheme, vz, ca, pixieLeaf{
		Secret: vizierServiceTLSSecret, CertKey: "client.crt", KeyKey: "client.key", CAKey: "ca.crt",
		Request: tunnelLeafRequest{CommonName: "pixie-vizier-client", DNSNames: names, Server: true, Client: true},
	}, labels, now)
	if err != nil {
		return "", err
	}
	// cluster-id and cluster-name are written by the cloud connector when it registers; the
	// operator only mints the signing key and never touches the rest.
	if _, err := ensureMintedSecret(ctx, r.Client, r.Scheme, vz, vizierClusterSecret, corev1.SecretTypeOpaque, labels,
		randomKey("jwt-signing-key", 32)); err != nil {
		return "", err
	}
	var hubCA corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: vz.Namespace, Name: vz.Spec.CloudCASecretName}, &hubCA); err != nil {
		return "", err
	}
	return secretsHash(certs, &hubCA), nil
}

// isPeerImportAddress reports whether the hub is reached through a PeerConnection import Service
// in Vizier's own namespace rather than on this cluster.
func isPeerImportAddress(address string) bool {
	return len(address) > len("peer-") && address[:len("peer-")] == "peer-"
}

func pixieVizierStatus(phase, message string) oramav1alpha1.PixieVizierStatus {
	return oramav1alpha1.PixieVizierStatus{Phase: phase, Message: message}
}

// setStatus writes status only when it changed (principle 7).
func (r *PixieVizierReconciler) setStatus(ctx context.Context, vz *oramav1alpha1.PixieVizier, observed oramav1alpha1.PixieVizierStatus, alertingErr error) error {
	if observed.Phase == "" {
		return nil
	}
	desired := observed
	desired.ObservedGeneration = vz.Generation
	desired.Conditions = append([]metav1.Condition(nil), vz.Status.Conditions...)
	apimeta.SetStatusCondition(&desired.Conditions, pixieCondition(observed.Phase, observed.Message, vz.Generation))
	apimeta.SetStatusCondition(&desired.Conditions, alertingCondition(alertingErr, vz.Generation))
	if apiequality.Semantic.DeepEqual(vz.Status, desired) {
		return nil
	}
	vz.Status = desired
	return r.Status().Update(ctx, vz)
}

// pixieCondition is the Ready condition for a phase. Its reason is the phase, so a steady phase
// keeps the condition unchanged.
func pixieCondition(phase, message string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if phase == oramav1alpha1.ObservabilityPhaseReady {
		status = metav1.ConditionTrue
		message = "every component is ready"
	}
	return metav1.Condition{Type: pixieReadyCondition, Status: status, Reason: phase, Message: message, ObservedGeneration: generation}
}

func (r *PixieVizierReconciler) viziersInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() != oramav1alpha1.PixieVizierNamespace {
		return nil
	}
	var list oramav1alpha1.PixieVizierList
	if err := r.List(ctx, &list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

func (r *PixieVizierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("pixievizier").
		For(&oramav1alpha1.PixieVizier{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.viziersInNamespace)).
		Complete(r)
}
