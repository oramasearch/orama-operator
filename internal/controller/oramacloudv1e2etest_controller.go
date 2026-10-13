/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// e2e smoke-test fixture (sandbox): a throwaway collection + index the suite writes to.
// e2eRWKey is a **test-only** collection read/write key (NOT the master key) — safe to carry
// in a plain ConfigMap. The master key (used to create the collection AND to run the admin/
// analytics assertions) is sourced from the target's engine Secret at pod start and never
// lands in a ConfigMap.
const (
	e2eCollectionID = "e2e-test"
	e2eIndexID      = "e2e-index"
	e2eRWKey        = "e2e-rw-key"
	e2eConfigPath   = "/etc/e2e/config.json"
	e2eScriptPath   = "/etc/e2e/functional.sh"
	// e2eRunnerImage is the default runner: a minimal base that provides sh; the Job installs
	// curl + jq into it and runs the mounted functional suite. Pinned for determinism.
	e2eRunnerImage = "alpine:3.20"
)

// e2eFunctionalScript is the comprehensive functional suite (curl + jq) that exercises every
// critical OramaCore function in multiple variations against a live OramaCloudV1. It is embedded
// from source (assets/e2e-functional.sh), shipped in the e2e ConfigMap, and run by the Job.
//
//go:embed assets/e2e-functional.sh
var e2eFunctionalScript string

// OramaCloudV1E2ETestReconciler runs the oramacore-test-e2e suite as a Job against a deployed
// OramaCloudV1 and reflects the Job's result in the test's status.
type OramaCloudV1E2ETestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1e2etests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1e2etests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// e2eConfigJSON renders the config the suite reads (monitor-test, config.rs): the target's
// engine Service URLs + the throwaway collection/index + the test collection key. readURL hits
// the reader Service (:80 → on-box Envoy → OramaCore :8080); writerURL the writer (:8080).
func e2eConfigJSON(t *oramav1alpha1.OramaCloudV1E2ETest) string {
	return fmt.Sprintf(`{
  "cluster": {
    "writerURL": "http://%s-writer:8080",
    "readURL": "http://%s-reader"
  },
  "collectionID": %q,
  "indexID": %q,
  "apiKey": %q
}
`, t.Spec.TargetRef, t.Spec.TargetRef, e2eCollectionID, e2eIndexID, e2eRWKey)
}

func (r *OramaCloudV1E2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t oramav1alpha1.OramaCloudV1E2ETest
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The runner base image provides `sh`; the Job installs curl + jq and runs the mounted suite.
	// spec.Image can override it (e.g. a pre-baked runner) but must contain sh + a package manager.
	runner := t.Spec.Image
	if runner == "" {
		runner = e2eRunnerImage
	}
	cmName := t.Name + "-e2e-config"

	// The target names its master API key Secret (spec.masterAPIKeySecretRef); a target not found
	// yet is assumed to use the default <target>-master-api-key.
	target := &oramav1alpha1.OramaCloudV1{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.TargetRef}, target); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		target.Name = t.Spec.TargetRef
	}
	jobName := t.Name + "-e2e"

	// 1. The suite's config.json (plain — the collection key is a test-only credential) + the
	//    embedded functional suite script.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: t.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"config.json":   e2eConfigJSON(&t),
			"functional.sh": e2eFunctionalScript,
		}
		return controllerutil.SetControllerReference(&t, cm, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. The Job: a single runner container that installs curl+jq and runs the embedded
	//    functional suite (which self-bootstraps a clean-slate collection/index before asserting).
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: t.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Job spec is immutable after creation — only populate it on create.
		if job.CreationTimestamp.IsZero() {
			var pull []corev1.LocalObjectReference
			if t.Spec.PullSecretName != "" {
				pull = []corev1.LocalObjectReference{{Name: t.Spec.PullSecretName}}
			}
			// The master key to create the collection comes from the target's master API key Secret.
			masterKey := masterAPIKeyEnv(target)
			configVol := corev1.VolumeMount{Name: "e2e-config", MountPath: "/etc/e2e", ReadOnly: true}
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/e2e": t.Name}
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(1),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy:    corev1.RestartPolicyNever,
						ImagePullSecrets: pull,
						Volumes: []corev1.Volume{{
							Name: "e2e-config",
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}},
						}},
						Containers: []corev1.Container{{
							Name:  "e2e",
							Image: runner,
							// Install curl + jq into the runner, then exec the mounted suite. The
							// master key (admin + analytics assertions) comes from the engine Secret.
							Command: []string{"sh", "-c",
								"apk add --no-cache curl jq >/dev/null 2>&1 || apk add --no-cache curl jq; exec sh " + e2eScriptPath},
							Env: []corev1.EnvVar{
								{Name: "CONFIG_PATH", Value: e2eConfigPath},
								masterKey,
							},
							VolumeMounts: []corev1.VolumeMount{configVol},
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

	// 3. Reflect the Job's state into status.
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
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
