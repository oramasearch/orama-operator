/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// OramaCloudV1E2ETestReconciler runs the oramacore-test-e2e suite as a Job against a deployed
// OramaCloudV1 and reflects the Job's result in the test's status.
type OramaCloudV1E2ETestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1e2etests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1e2etests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *OramaCloudV1E2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t oramav1alpha1.OramaCloudV1E2ETest
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	image := t.Spec.Image
	if image == "" {
		image = "oramasearch/oramacore-test-e2e:latest"
	}
	jobName := t.Name + "-e2e"

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: t.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Job spec is immutable after creation — only populate it on create.
		if job.CreationTimestamp.IsZero() {
			var pull []corev1.LocalObjectReference
			if t.Spec.PullSecretName != "" {
				pull = []corev1.LocalObjectReference{{Name: t.Spec.PullSecretName}}
			}
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/e2e": t.Name}
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(1),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy:    corev1.RestartPolicyNever,
						ImagePullSecrets: pull,
						Containers: []corev1.Container{{
							Name:  "e2e",
							Image: image,
							Args:  t.Spec.Args,
							// Point the suite at the target stack's engine Services.
							Env: []corev1.EnvVar{
								{Name: "ORAMACORE_WRITER_URL", Value: "http://" + t.Spec.TargetRef + "-writer:8080"},
								{Name: "ORAMACORE_READER_URL", Value: "http://" + t.Spec.TargetRef + "-reader"},
							},
						}},
					},
				},
			}
		}
		return controllerutil.SetControllerReference(&t, job, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Reflect the Job's state into status.
	phase, msg := "Running", "e2e job running"
	switch {
	case job.Status.Succeeded > 0:
		phase, msg = "Succeeded", "e2e suite passed"
	case job.Status.Failed > 0:
		phase, msg = "Failed", "e2e suite failed — see the Job's pod logs"
	}
	if t.Status.Phase != phase || t.Status.JobName != jobName {
		t.Status.Phase, t.Status.Message, t.Status.JobName = phase, msg, jobName
		if err := r.Status().Update(ctx, &t); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *OramaCloudV1E2ETestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1E2ETest{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
