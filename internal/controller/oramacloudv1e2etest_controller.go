/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"encoding/json"
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

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// e2e smoke-test fixture (sandbox): a throwaway collection + index the suite writes to.
// e2eRWKey is a **test-only** collection read/write key (NOT the master key) — safe to carry
// in a plain ConfigMap. The master key (used to create the collection AND to run the admin/
// analytics assertions) comes from the target's spec.masterAPIKeySecretRef at pod start and never
// lands in a ConfigMap.
const (
	e2eCollectionID = "e2e-test"
	e2eIndexID      = "e2e-index"
	e2eRWKey        = "e2e-rw-key"
	e2eConfigPath   = "/etc/e2e/config.json"
	e2eScriptPath   = "/etc/e2e/functional.sh"
	// e2eComponent labels the runner's pod as a component of the target stack, so the stack's
	// network sandbox applies default-deny to it and admits it through its e2e allowances.
	e2eComponent = "e2e"
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

// e2eLibScript holds the suite's HTTP and assertion helpers, sourced by both suite scripts.
//
//go:embed assets/e2e-lib.sh
var e2eLibScript string

// e2eControlPlaneScript checks the control plane around the engine: the dashboard's JWKS, write
// tokens, RBAC and CSRF guards (operator#63), and the in-cluster edge workers' auth (operator#61).
// The functional suite sources it.
//
//go:embed assets/e2e-control-plane.sh
var e2eControlPlaneScript string

// e2eFixtureKeys are the dashboard fixture Secret's keys and the variables the suite reads them
// from. The first three are required; readerSessionCookie is optional.
var e2eFixtureKeys = []struct {
	key, env string
	optional bool
}{
	{"collectionID", "DASHBOARD_FIXTURE_COLLECTION_ID", false},
	{"indexID", "DASHBOARD_FIXTURE_INDEX_ID", false},
	{"apiKey", "DASHBOARD_FIXTURE_API_KEY", false},
	{"readerSessionCookie", "DASHBOARD_FIXTURE_READER_SESSION", true},
}

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
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch

// e2eTargetWait is how often a test waiting on its target, or the target's master API key
// reference, checks again.
const e2eTargetWait = 20 * time.Second

// e2eWorker is one in-cluster edge worker Service the suite probes.
type e2eWorker struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// e2eConfig is the suite's config.json.
type e2eConfig struct {
	Cluster struct {
		WriterURL string `json:"writerURL"`
		ReadURL   string `json:"readURL"`
	} `json:"cluster"`
	CollectionID string `json:"collectionID"`
	IndexID      string `json:"indexID"`
	APIKey       string `json:"apiKey"`
	// DashboardURL is the target's in-cluster dashboard, or "" when it runs none.
	DashboardURL string `json:"dashboardURL"`
	// WriterJWT reports whether the target's writer verifies the dashboard's write tokens.
	WriterJWT bool `json:"writerJWT"`
	// Workers are the target's in-cluster edge worker Services.
	Workers []e2eWorker `json:"workers"`
}

// e2eConfigJSON renders the config the suite reads (monitor-test, config.rs): the target's
// engine Service URLs + the throwaway collection/index + the test collection key. readURL hits
// the reader Service (:80 → on-box Envoy → OramaCore :8080); writerURL the writer (:8080). With the
// target at hand it also names the control-plane Services the stack runs: the dashboard, whether the
// writer verifies its write tokens, and the edge workers. Nothing in it is a secret.
func e2eConfigJSON(t *oramav1alpha1.OramaCloudV1E2ETest, target *oramav1alpha1.OramaCloudV1) string {
	name := t.Spec.TargetRef
	cfg := e2eConfig{CollectionID: e2eCollectionID, IndexID: e2eIndexID, APIKey: e2eRWKey, Workers: []e2eWorker{}}
	cfg.Cluster.WriterURL = "http://" + name + "-writer:8080"
	cfg.Cluster.ReadURL = "http://" + name + "-reader"
	if target != nil {
		c := target.Spec.Components
		if c.Dashboard.Enabled {
			cfg.DashboardURL = "http://" + name + "-dashboard:3000"
		}
		cfg.WriterJWT = c.Engine.Writer.JWT != nil
		if c.Workers.Enabled {
			cfg.Workers = append(cfg.Workers, e2eWorker{Name: "orama-workers", URL: "http://" + name + "-workers:8080"})
		}
		if c.SearchWorkers.Enabled {
			for _, w := range searchWorkerNames {
				cfg.Workers = append(cfg.Workers, e2eWorker{Name: w, URL: "http://" + name + "-" + w + ":8080"})
			}
		}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil { // a struct of strings, bools and slices always marshals
		panic(err)
	}
	return string(out) + "\n"
}

// e2eFixtureEnv reads the dashboard fixture Secret into the runner's environment, or nothing when
// the test names none.
func e2eFixtureEnv(t *oramav1alpha1.OramaCloudV1E2ETest) []corev1.EnvVar {
	ref := t.Spec.DashboardFixtureSecretRef
	if ref == nil || ref.Name == "" {
		return nil
	}
	env := make([]corev1.EnvVar, 0, len(e2eFixtureKeys))
	for _, k := range e2eFixtureKeys {
		sel := &corev1.SecretKeySelector{LocalObjectReference: *ref, Key: k.key}
		if k.optional {
			sel.Optional = boolPtr(true)
		}
		env = append(env, corev1.EnvVar{Name: k.env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: sel}})
	}
	return env
}

// e2eFixtureMissing says what the dashboard fixture Secret lacks, or "" when it has every required key.
func (r *OramaCloudV1E2ETestReconciler) e2eFixtureMissing(ctx context.Context, t *oramav1alpha1.OramaCloudV1E2ETest) (string, error) {
	ref := t.Spec.DashboardFixtureSecretRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	s := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: ref.Name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("waiting for dashboard fixture Secret %s/%s", t.Namespace, ref.Name), nil
		}
		return "", err
	}
	for _, k := range e2eFixtureKeys {
		if !k.optional && len(s.Data[k.key]) == 0 {
			return fmt.Sprintf("waiting for dashboard fixture Secret %s/%s to hold a non-empty %s", t.Namespace, ref.Name, k.key), nil
		}
	}
	return "", nil
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
	jobName := t.Name + "-e2e"

	// The runner takes MASTER_API_KEY from the target's spec.masterAPIKeySecretRef, read when the
	// Job is created; there is no fallback Secret. Until the target exists and names one, wait.
	// An existing Job is immutable and keeps the reference it was created with.
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: t.Namespace}}
	var masterKey corev1.EnvVar
	err := r.Get(ctx, client.ObjectKeyFromObject(job), job)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// The target names the control-plane Services in the config and gives the runner's pod its
	// stack labels. A running Job keeps its spec when the target is gone.
	var target *oramav1alpha1.OramaCloudV1
	found := &oramav1alpha1.OramaCloudV1{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.TargetRef}, found); {
	case err == nil:
		target = found
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	if !exists {
		if target == nil {
			return r.e2eWaiting(ctx, &t, fmt.Sprintf("waiting for OramaCloudV1 %s/%s", t.Namespace, t.Spec.TargetRef))
		}
		if _, ok := masterAPIKeySecret(target); !ok {
			return r.e2eWaiting(ctx, &t, fmt.Sprintf("waiting for OramaCloudV1 %s/%s to set spec.masterAPIKeySecretRef", t.Namespace, t.Spec.TargetRef))
		}
		if msg, err := r.e2eFixtureMissing(ctx, &t); err != nil {
			return ctrl.Result{}, err
		} else if msg != "" {
			return r.e2eWaiting(ctx, &t, msg)
		}
		masterKey = masterAPIKeyEnv(target)
	}

	// 1. The suite's config.json (plain — the collection key is a test-only credential) + the
	//    embedded functional suite script.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: t.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"config.json":      e2eConfigJSON(&t, target),
			"functional.sh":    e2eFunctionalScript,
			"lib.sh":           e2eLibScript,
			"control-plane.sh": e2eControlPlaneScript,
		}
		return controllerutil.SetControllerReference(&t, cm, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. The Job: a single runner container that installs curl+jq and runs the embedded
	//    functional suite (which self-bootstraps a clean-slate collection/index before asserting).
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Job spec is immutable after creation — only populate it on create.
		if !exists {
			var pull []corev1.LocalObjectReference
			if t.Spec.PullSecretName != "" {
				pull = []corev1.LocalObjectReference{{Name: t.Spec.PullSecretName}}
			}
			configVol := corev1.VolumeMount{Name: "e2e-config", MountPath: "/etc/e2e", ReadOnly: true}
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "orama.cloud/e2e": t.Name}
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(1),
				Template: corev1.PodTemplateSpec{
					// A component of the target stack: inside its network sandbox, admitted by the e2e allowances.
					ObjectMeta: metav1.ObjectMeta{Labels: e2eRunnerLabels(target, t.Name)},
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
							// master key (admin + analytics assertions) comes from the target's reference.
							Command: []string{"sh", "-c",
								"apk add --no-cache curl jq >/dev/null 2>&1 || apk add --no-cache curl jq; exec sh " + e2eScriptPath},
							Env: append([]corev1.EnvVar{
								{Name: "CONFIG_PATH", Value: e2eConfigPath},
								masterKey,
							}, e2eFixtureEnv(&t)...),
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

// e2eRunnerLabels are the runner pod's labels: the target stack's, with component e2e, plus the test's name.
func e2eRunnerLabels(target *oramav1alpha1.OramaCloudV1, test string) map[string]string {
	l := labelsFor(target, e2eComponent)
	l["orama.cloud/e2e"] = test
	return l
}

// e2eWaiting records phase Pending with msg (only when it changed) and checks again later.
func (r *OramaCloudV1E2ETestReconciler) e2eWaiting(ctx context.Context, t *oramav1alpha1.OramaCloudV1E2ETest, msg string) (ctrl.Result, error) {
	if t.Status.Phase != "Pending" || t.Status.Message != msg {
		t.Status.Phase, t.Status.Message = "Pending", msg
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: e2eTargetWait}, nil
}

func (r *OramaCloudV1E2ETestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaCloudV1E2ETest{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
