/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const lakeE2ETargetRequeue = 15 * time.Second

// LakeE2ETestReconciler runs lake's signup and liquid sync suite as a Job against a deployed Lake
// and reflects the Job result in the cluster-scoped test resource.
type LakeE2ETestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakee2etests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakee2etests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile waits for the target Lake and creates one immutable e2e Job in its workload namespace.
func (r *LakeE2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var test oramav1alpha1.LakeE2ETest
	if err := r.Get(ctx, req.NamespacedName, &test); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: test.Spec.TargetRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setStatus(
				ctx,
				&test,
				"Pending",
				fmt.Sprintf("waiting for target Lake %s", test.Spec.TargetRef),
				"",
				ctrl.Result{RequeueAfter: lakeE2ETargetRequeue},
			)
		}
		return ctrl.Result{}, err
	}

	if !lakeReadyForE2E(&lake) {
		return r.setStatus(
			ctx,
			&test,
			"Pending",
			fmt.Sprintf("waiting for target Lake %s to be ready", test.Spec.TargetRef),
			"",
			ctrl.Result{RequeueAfter: lakeE2ETargetRequeue},
		)
	}

	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	jobName := test.Name + "-e2e"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		if job.CreationTimestamp.IsZero() {
			pullSecretName := test.Spec.PullSecretName
			if pullSecretName == "" {
				pullSecretName = lake.Spec.PullSecretName
			}
			var pullSecrets []corev1.LocalObjectReference
			if pullSecretName != "" {
				pullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
			}
			runID := string(test.UID)
			if runID == "" {
				runID = test.Name
			}
			automountServiceAccountToken := false
			job.Labels = map[string]string{
				"app.kubernetes.io/managed-by": "orama-operator",
				"orama.cloud/lake-e2e":         test.Name,
			}
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(1),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						AutomountServiceAccountToken: &automountServiceAccountToken,
						ImagePullSecrets:             pullSecrets,
						RestartPolicy:                corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:  "e2e",
							Image: test.Spec.Image,
							Env: []corev1.EnvVar{
								{
									Name:  "LAKE_E2E_BASE_URL",
									Value: fmt.Sprintf("http://lake-%s.%s.svc.cluster.local", lake.Name, namespace),
								},
								{
									Name:  "LAKE_E2E_RUN_ID",
									Value: runID,
								},
							},
						}},
					},
				},
			}
		}
		return controllerutil.SetControllerReference(&test, job, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	phase := "Running"
	message := "lake e2e job running"
	if job.Status.Succeeded > 0 {
		phase = "Succeeded"
		message = "lake e2e suite passed"
	} else if job.Status.Failed > 0 {
		phase = "Failed"
		message = "lake e2e suite failed; see the Job pod logs"
	}
	return r.setStatus(ctx, &test, phase, message, jobName, ctrl.Result{})
}

func lakeReadyForE2E(lake *oramav1alpha1.Lake) bool {
	return lake.Status.Phase == "Ready" &&
		lake.Status.ReadyReplicas > 0 &&
		lake.Status.PostgresReady &&
		lake.Status.SchemaPhase == "Applied"
}

func (r *LakeE2ETestReconciler) setStatus(
	ctx context.Context,
	test *oramav1alpha1.LakeE2ETest,
	phase string,
	message string,
	jobName string,
	result ctrl.Result,
) (ctrl.Result, error) {
	if test.Status.Phase == phase &&
		test.Status.Message == message &&
		test.Status.JobName == jobName {
		return result, nil
	}
	test.Status.Phase = phase
	test.Status.Message = message
	test.Status.JobName = jobName
	if err := r.Status().Update(ctx, test); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *LakeE2ETestReconciler) requestsForLake(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	lake, ok := object.(*oramav1alpha1.Lake)
	if !ok {
		return nil
	}
	var tests oramav1alpha1.LakeE2ETestList
	if err := r.List(ctx, &tests); err != nil {
		logf.FromContext(ctx).Error(err, "list LakeE2ETests for target Lake", "lake", lake.Name)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tests.Items))
	for i := range tests.Items {
		if tests.Items[i].Spec.TargetRef != lake.Name {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: tests.Items[i].Name},
		})
	}
	return requests
}

// SetupWithManager registers the Lake test and target readiness watches.
func (r *LakeE2ETestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeE2ETest{}).
		Owns(&batchv1.Job{}).
		Watches(
			&oramav1alpha1.Lake{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForLake),
		).
		Named("lake-e2e-test").
		Complete(r)
}
