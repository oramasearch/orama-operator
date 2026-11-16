// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
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
	// Clientset reads the finished Job's pod logs (corev1 GetLogs — a subresource the controller-runtime
	// client can't read) to parse the suite's pass/fail/skip counts into status. Best-effort: a nil
	// clientset (or an unreadable log) just leaves the coarse Phase and zero counts.
	Clientset kubernetes.Interface
}

//+kubebuilder:rbac:groups=orama.cloud,resources=zettaclustere2etests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=zettaclustere2etests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
//+kubebuilder:rbac:groups=core,resources=pods/log,verbs=get

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
				t.Status.Pass, t.Status.Fail, t.Status.Skip,
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
		peers = append(peers, fmt.Sprintf("http://%s-zetta-%d.%s-zetta-headless.%s.svc.cluster.local:%d",
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
	// Auth wiring — these env names are the exact contract the suite reads (do NOT rename). Auth is ALWAYS
	// enforced by the engine (zetta#799 made it mandatory and removed the unauthenticated/plaintext mode), so
	// ALWAYS run the authz-enforcement assertions (ZETTA_E2E_REQUIRE_AUTH=1) and hand the suite the SEEDED
	// ADMIN token (Secret <ref>-zetta-admin key `token`, always provisioned) — the owner the node seeded, so
	// authenticated calls pass.
	env = append(env,
		corev1.EnvVar{Name: "ZETTA_E2E_REQUIRE_AUTH", Value: "1"},
		corev1.EnvVar{Name: "ZETTA_E2E_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: t.Spec.TargetRef + "-zetta-admin"},
				Key:                  "token",
			}}})

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

	// Best-effort pass/fail/skip counts, parsed from the Job's pod logs once it's terminal. Preserve
	// whatever we already recorded if the summary isn't readable (crash before summary, missing
	// clientset…) — don't zero a count we already have.
	pass, fail, skip := t.Status.Pass, t.Status.Fail, t.Status.Skip
	if phase == "Succeeded" || phase == "Failed" {
		if p, f, s, ok := r.e2eCountsFromLogs(ctx, t.Namespace, jobName); ok {
			pass, fail, skip = p, f, s
		}
	}
	return r.setStatus(ctx, &t, phase, msg, jobName, pass, fail, skip, ctrl.Result{})
}

// e2eSummaryRe matches the suite's final summary line: `Zetta e2e: pass=N fail=N skip=N`.
var e2eSummaryRe = regexp.MustCompile(`Zetta e2e: pass=([0-9]+) fail=([0-9]+) skip=([0-9]+)`)

// e2eCountsFromLogs reads the Job's pod logs (best-effort) and parses that summary line. Returns
// ok=false when the clientset is unavailable, no pod is found, a log can't be read, or the summary
// isn't present (e.g. the suite crashed before printing it). Uses the clientset directly: pods/log is
// a subresource the controller-runtime client can't read, and going direct keeps Pods out of the
// manager's watch cache (no extra informer). BackoffLimit may leave more than one pod — prefer the
// most recently created pod that has a parseable summary.
func (r *ZettaClusterE2ETestReconciler) e2eCountsFromLogs(ctx context.Context, namespace, jobName string) (pass, fail, skip int32, ok bool) {
	if r.Clientset == nil {
		return 0, 0, 0, false
	}
	pods, err := r.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return 0, 0, 0, false
	}
	var newest metav1.Time
	for i := range pods.Items {
		pod := &pods.Items[i]
		if ok && !pod.CreationTimestamp.After(newest.Time) {
			continue // already have counts from a newer pod
		}
		raw, lerr := r.Clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if lerr != nil {
			continue
		}
		m := e2eSummaryRe.FindSubmatch(raw)
		if m == nil {
			continue
		}
		p, _ := strconv.Atoi(string(m[1]))
		f, _ := strconv.Atoi(string(m[2]))
		s, _ := strconv.Atoi(string(m[3]))
		pass, fail, skip, ok, newest = int32(p), int32(f), int32(s), true, pod.CreationTimestamp
	}
	return pass, fail, skip, ok
}

// setStatus writes the test's status subresource ONLY when a tracked field actually changed
// (Phase, JobName, or a pass/fail/skip count) — anti-flap, operator principle #7 — then returns the
// given result.
func (r *ZettaClusterE2ETestReconciler) setStatus(ctx context.Context, t *oramav1alpha1.ZettaClusterE2ETest, phase, msg, jobName string, pass, fail, skip int32, res ctrl.Result) (ctrl.Result, error) {
	if t.Status.Phase != phase || t.Status.JobName != jobName ||
		t.Status.Pass != pass || t.Status.Fail != fail || t.Status.Skip != skip {
		t.Status.Phase, t.Status.Message, t.Status.JobName = phase, msg, jobName
		t.Status.Pass, t.Status.Fail, t.Status.Skip = pass, fail, skip
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
