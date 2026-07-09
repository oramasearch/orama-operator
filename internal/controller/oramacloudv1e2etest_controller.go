/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// e2e smoke-test fixture (sandbox): a throwaway collection + index the suite writes to.
// e2eRWKey is a **test-only** collection read/write key (NOT the master key) — safe to carry
// in a plain ConfigMap. The master key (used only to CREATE the collection) is sourced from
// the target's engine Secret at pod start and never lands in a ConfigMap.
const (
	e2eCollectionID = "e2e-test"
	e2eIndexID      = "e2e-index"
	e2eRWKey        = "e2e-rw-key"
	e2eConfigPath   = "/etc/e2e/config.json"
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

// e2eBootstrapScript creates the throwaway collection + index the suite writes to, idempotently.
// The collection is created with the target's MASTER_API_KEY (auth), assigning our test key as
// both read + write key; the index is created with that test key. EXPLICIT embedding of the
// "title" field (`"embedding":["title"]` → IndexEmbeddingsCalculation::Properties) so the suite's
// vector search works via the embeddings server directly — NOT "automatic", which invokes
// OramaCore's LLM field-selector and panics in async-openai 0.28.3 (CannotCloneRequestError on a
// streaming request's retry-clone) regardless of a valid key. See operator#41. Re-runs tolerate
// "already exists".
func e2eBootstrapScript(t *oramav1alpha1.OramaCloudV1E2ETest) string {
	w := "http://" + t.Spec.TargetRef + "-writer:8080"
	return fmt.Sprintf(`set -eu
W=%q
echo "e2e-bootstrap: create collection %s (idempotent)"
curl -fsS -X POST "$W/v1/collections/create" \
  -H "Authorization: Bearer $MASTER_API_KEY" -H 'Content-Type: application/json' \
  -d '{"id":%q,"description":"e2e smoke","read_api_key":%q,"write_api_key":%q,"language":"english"}' \
  || echo "  collection create non-2xx (already exists?) — continuing"
echo "e2e-bootstrap: create index %s (idempotent)"
curl -fsS -X POST "$W/v1/collections/%s/indexes/create" \
  -H "Authorization: Bearer %s" -H 'Content-Type: application/json' \
  -d '{"id":%q,"embedding":["title"]}' \
  || echo "  index create non-2xx (already exists?) — continuing"
`, w, e2eCollectionID, e2eCollectionID, e2eRWKey, e2eRWKey, e2eIndexID, e2eCollectionID, e2eRWKey, e2eIndexID)
}

func (r *OramaCloudV1E2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t oramav1alpha1.OramaCloudV1E2ETest
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	image := t.Spec.Image
	if image == "" {
		image = "oramasearch/oramacore-test-e2e:latest"
	}
	cmName := t.Name + "-e2e-config"
	jobName := t.Name + "-e2e"

	// 1. The suite's config.json (plain — the collection key is a test-only credential).
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: t.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"config.json": e2eConfigJSON(&t)}
		return controllerutil.SetControllerReference(&t, cm, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. The Job: bootstrap init container (create collection+index) → suite container.
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: t.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Job spec is immutable after creation — only populate it on create.
		if job.CreationTimestamp.IsZero() {
			var pull []corev1.LocalObjectReference
			if t.Spec.PullSecretName != "" {
				pull = []corev1.LocalObjectReference{{Name: t.Spec.PullSecretName}}
			}
			// The master key to create the collection comes from the target's engine Secret.
			masterKey := corev1.EnvVar{Name: "MASTER_API_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: t.Spec.TargetRef + "-engine-secret"},
					Key:                  "MASTER_API_KEY",
				}}}
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
						InitContainers: []corev1.Container{{
							Name:    "bootstrap",
							Image:   "curlimages/curl:8.10.1",
							Command: []string{"sh", "-c", e2eBootstrapScript(&t)},
							Env:     []corev1.EnvVar{masterKey},
						}},
						Containers: []corev1.Container{{
							Name:         "e2e",
							Image:        image,
							Args:         t.Spec.Args,
							Env:          []corev1.EnvVar{{Name: "CONFIG_PATH", Value: e2eConfigPath}},
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
