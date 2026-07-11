/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// zettaE2ERunnerImage is the default runner: the oramasearch/zetta-e2e image that BAKES the suite
// (tests/e2e/zetta-e2e.sh from the zetta repo) as its ENTRYPOINT. Unlike the OramaCloudV1 suite —
// which is embedded in the operator via go:embed — the Zetta suite ships in the image so it evolves
// with the engine repo; this reconciler only triggers it and wires the cluster addressing via env.
const zettaE2ERunnerImage = "oramasearch/zetta-e2e:v1-sandbox"

// ZettaClusterE2ETestReconciler runs the live-cluster zetta e2e suite as a Job against a deployed
// OramaZettaCluster and reflects the Job's result in the test's status.
type ZettaClusterE2ETestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=zettaclustere2etests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=zettaclustere2etests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *ZettaClusterE2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t oramav1alpha1.ZettaClusterE2ETest
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The suite addresses the target cluster by its per-pod headless DNS, so the target must exist to
	// know its replica count. If it isn't there yet, park in Pending and retry — don't create a Job.
	var target oramav1alpha1.OramaZettaCluster
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.TargetRef}, &target); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setStatus(ctx, &t, "Pending",
				fmt.Sprintf("waiting for target OramaZettaCluster %s", t.Spec.TargetRef), "",
				ctrl.Result{RequeueAfter: 15 * time.Second})
		}
		return ctrl.Result{}, err
	}

	replicas := target.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}

	// The e2e contract (tests/e2e/zetta-e2e.sh): pin the writer to node-0 via per-pod DNS, list the
	// remaining ordinals as read peers, and pass the requested test levels. These env names are the
	// exact contract the script reads — do NOT rename.
	baseURL := fmt.Sprintf("http://%s-zetta-0.%s-zetta-headless.%s.svc.cluster.local:%d",
		t.Spec.TargetRef, t.Spec.TargetRef, t.Namespace, zettaAPIPort)
	var peers []string
	for i := int32(1); i < replicas; i++ {
		peers = append(peers, fmt.Sprintf("%s-zetta-%d.%s-zetta-headless.%s.svc.cluster.local:%d",
			t.Spec.TargetRef, i, t.Spec.TargetRef, t.Namespace, zettaAPIPort))
	}
	peerURLs := strings.Join(peers, " ") // empty when replicas == 1
	levels := t.Spec.Levels
	if levels == "" {
		levels = "2,3"
	}

	env := []corev1.EnvVar{
		{Name: "ZETTA_E2E_BASE_URL", Value: baseURL},
		{Name: "ZETTA_E2E_PEER_URLS", Value: peerURLs},
		{Name: "ZETTA_E2E_LEVELS", Value: levels},
	}
	// When the target requires an MCP bearer, hand the suite the same token (Secret <ref>-zetta-mcp
	// key `token`). Best-effort: the token authenticates-but-denies today (zetta#733), which is fine.
	if target.Spec.MCPTokenAuth {
		env = append(env, corev1.EnvVar{Name: "ZETTA_E2E_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: t.Spec.TargetRef + "-zetta-mcp"},
				Key:                  "token",
			}}})
	}

	// The runner image's ENTRYPOINT runs the baked suite — do NOT set a Command, only Env.
	runner := t.Spec.Image
	if runner == "" {
		runner = zettaE2ERunnerImage
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
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/zetta-e2e": t.Name}
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(1),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy:    corev1.RestartPolicyNever,
						ImagePullSecrets: pull,
						Containers: []corev1.Container{{
							Name:  "e2e",
							Image: runner,
							Env:   env,
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
	phase, msg := "Running", "zetta e2e job running"
	switch {
	case job.Status.Succeeded > 0:
		phase, msg = "Succeeded", "zetta e2e suite passed"
	case job.Status.Failed > 0:
		phase, msg = "Failed", "zetta e2e suite failed — see the Job's pod logs"
	}
	return r.setStatus(ctx, &t, phase, msg, jobName, ctrl.Result{})
}

// setStatus writes the test's status subresource ONLY when Phase or JobName actually changed
// (anti-flap — operator principle #7), then returns the given result.
func (r *ZettaClusterE2ETestReconciler) setStatus(ctx context.Context, t *oramav1alpha1.ZettaClusterE2ETest, phase, msg, jobName string, res ctrl.Result) (ctrl.Result, error) {
	if t.Status.Phase != phase || t.Status.JobName != jobName {
		t.Status.Phase, t.Status.Message, t.Status.JobName = phase, msg, jobName
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

func (r *ZettaClusterE2ETestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ZettaClusterE2ETest{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
