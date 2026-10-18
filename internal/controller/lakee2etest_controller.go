/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeE2ETargetRequeue = 15 * time.Second
	// lakeE2ELabel marks a test's Jobs, so older runs' Jobs can be found and removed.
	lakeE2ELabel = "orama.cloud/lake-e2e"
	// lakeE2EDefaultTimeout bounds one run: the suite waits on workspaces, services and agent turns.
	lakeE2EDefaultTimeout = int64(3 * 60 * 60)
	// lakeE2EMaxListed caps the failed and skipped steps kept in status.
	lakeE2EMaxListed = 50
)

// LakeE2ETestReconciler runs lake's end-to-end suite (the oramasearch/lake-e2e runner) as a Job
// against a deployed Lake, usually a LakeSet member, one Job per spec generation, and reports each
// run's result in the cluster-scoped test resource.
type LakeE2ETestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads the finished run's pod (its termination message, or its log when the runner
	// ended without one). A nil clientset leaves the counts empty and the phase from the Job.
	Clientset kubernetes.Interface
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakee2etests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakee2etests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get

// Reconcile starts a run for each new spec generation once its Lake is ready, follows the run's
// Job, and records its result. A finished run is never started again for the same generation.
func (r *LakeE2ETestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var test oramav1alpha1.LakeE2ETest
	if err := r.Get(ctx, req.NamespacedName, &test); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := test.Status.DeepCopy()
	lakeName := lakeE2ETarget(&test)
	status.Target = lakeName

	// A run already finished for this generation stays as it is, even after its Job is gone.
	if status.ObservedGeneration == test.Generation && (status.Phase == "Succeeded" || status.Phase == "Failed") {
		return r.writeStatus(ctx, &test, status, ctrl.Result{})
	}

	jobName := lakeE2EJobName(&test)
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: lakeName}, &lake); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.pending(status, &test, fmt.Sprintf("waiting for target Lake %s", lakeName))
		return r.writeStatus(ctx, &test, status, ctrl.Result{RequeueAfter: lakeE2ETargetRequeue})
	}
	if member := test.Spec.Member; member != nil {
		want := "LakeSet." + member.LakeSet
		if got := lake.Labels[managedBySetLabel]; got != want {
			r.pending(status, &test, fmt.Sprintf("Lake %s is not a member of LakeSet %s (its %s label is %q)", lakeName, member.LakeSet, managedBySetLabel, got))
			return r.writeStatus(ctx, &test, status, ctrl.Result{RequeueAfter: lakeE2ETargetRequeue})
		}
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: jobName}, job)
	switch {
	case apierrors.IsNotFound(err):
		if !lakeReadyForE2E(&lake) {
			r.pending(status, &test, fmt.Sprintf("waiting for target Lake %s to be ready", lakeName))
			return r.writeStatus(ctx, &test, status, ctrl.Result{RequeueAfter: lakeE2ETargetRequeue})
		}
		job = r.runnerJob(&test, &lake, namespace, jobName)
		if err := controllerutil.SetControllerReference(&test, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		*status = oramav1alpha1.LakeE2ETestStatus{
			Phase:              "Running",
			Message:            fmt.Sprintf("run %d started against Lake %s", test.Generation, lakeName),
			JobName:            jobName,
			Target:             lakeName,
			ObservedGeneration: test.Generation,
			RunID:              lakeE2ERunID(&test),
			StartedAt:          &now,
		}
		return r.writeStatus(ctx, &test, status, ctrl.Result{})
	case err != nil:
		return ctrl.Result{}, err
	}

	status.JobName = jobName
	status.ObservedGeneration = test.Generation
	status.RunID = lakeE2ERunID(&test)
	if status.StartedAt == nil {
		started := job.CreationTimestamp
		status.StartedAt = &started
	}
	finished, succeeded := lakeE2EJobFinished(job)
	if !finished {
		status.Phase = "Running"
		status.Message = fmt.Sprintf("run %d running against Lake %s", test.Generation, lakeName)
		return r.writeStatus(ctx, &test, status, ctrl.Result{})
	}

	result, read := r.runResult(ctx, namespace, jobName)
	if read {
		status.Pass, status.Fail, status.Skip = result.Pass, result.Fail, result.Skip
		status.Failed, status.Skipped = capSteps(result.Failed), capSteps(result.Skipped)
		status.LakeRevision = result.Revision
	}
	completed := metav1.Now()
	if job.Status.CompletionTime != nil {
		completed = *job.Status.CompletionTime
	}
	status.CompletedAt = &completed
	status.Phase, status.Message = runVerdict(succeeded, read, result)
	if err := r.removeOlderRuns(ctx, &test, namespace, jobName); err != nil {
		return ctrl.Result{}, err
	}
	return r.writeStatus(ctx, &test, status, ctrl.Result{})
}

// lakeE2ETarget is the Lake a test runs against: the member's Lake, named after the member, or
// spec.targetRef.
func lakeE2ETarget(test *oramav1alpha1.LakeE2ETest) string {
	if test.Spec.Member != nil {
		return test.Spec.Member.Name
	}
	return test.Spec.TargetRef
}

// lakeE2EJobName is `<test>-e2e-<generation>`: one Job per run, so a changed spec runs again.
func lakeE2EJobName(test *oramav1alpha1.LakeE2ETest) string {
	suffix := fmt.Sprintf("-e2e-%d", test.Generation)
	name := test.Name
	if len(name)+len(suffix) > 63 {
		name = strings.TrimRight(name[:63-len(suffix)], "-.")
	}
	return name + suffix
}

// lakeE2ERunID names the run's test data: eight characters that differ per run, then the test's
// UID. The runner names what it creates after the first eight.
func lakeE2ERunID(test *oramav1alpha1.LakeE2ETest) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", test.UID, test.Generation, test.Spec.RunID)))
	id := string(test.UID)
	if id == "" {
		id = test.Name
	}
	return hex.EncodeToString(sum[:4]) + "-" + id
}

func (r *LakeE2ETestReconciler) pending(status *oramav1alpha1.LakeE2ETestStatus, test *oramav1alpha1.LakeE2ETest, message string) {
	// A run of an earlier generation keeps its result until this generation's run starts.
	if status.ObservedGeneration != test.Generation {
		*status = oramav1alpha1.LakeE2ETestStatus{Target: status.Target}
	}
	status.Phase = "Pending"
	status.Message = message
}

func (r *LakeE2ETestReconciler) runnerJob(test *oramav1alpha1.LakeE2ETest, lake *oramav1alpha1.Lake, namespace, jobName string) *batchv1.Job {
	pullSecretName := test.Spec.PullSecretName
	if pullSecretName == "" {
		pullSecretName = lake.Spec.PullSecretName
	}
	var pullSecrets []corev1.LocalObjectReference
	if pullSecretName != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
	}
	timeout := lakeE2EDefaultTimeout
	if test.Spec.TimeoutSeconds != nil {
		timeout = *test.Spec.TimeoutSeconds
	}
	env := []corev1.EnvVar{
		{Name: "LAKE_E2E_BASE_URL", Value: fmt.Sprintf("http://lake-%s.%s.svc.cluster.local", lake.Name, namespace)},
		{Name: "LAKE_E2E_RUN_ID", Value: lakeE2ERunID(test)},
		// Chromium and Node share the container's memory limit.
		{Name: "NODE_OPTIONS", Value: "--max-old-space-size=1024"},
	}
	// A person's browser reaches the Lake at its public host; the session cookie is Secure, so the
	// browser keeps it only over https.
	if lake.Spec.IngressEnabled && lake.Spec.Host != "" {
		env = append(env, corev1.EnvVar{Name: "LAKE_E2E_PUBLIC_URL", Value: "https://" + lake.Spec.Host})
	}
	env = append(env, test.Spec.Env...)
	automount := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "orama-operator",
				lakeE2ELabel:                   test.Name,
			},
		},
		Spec: batchv1.JobSpec{
			// A failed run is a result, not a crash to retry: a retry would test against the
			// first attempt's leftovers.
			BackoffLimit:          int32Ptr(0),
			ActiveDeadlineSeconds: &timeout,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{lakeE2ELabel: test.Name}},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &automount,
					ImagePullSecrets:             pullSecrets,
					RestartPolicy:                corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "e2e",
						Image: test.Spec.Image,
						Env:   env,
						// The runner writes its summary (counts, failed and skipped steps) here.
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					}},
				},
			},
		},
	}
}

func lakeE2EJobFinished(job *batchv1.Job) (finished, succeeded bool) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return true, true
		case batchv1.JobFailed:
			return true, false
		}
	}
	if job.Status.Succeeded > 0 {
		return true, true
	}
	return false, false
}

// lakeE2EResult is the runner's summary: its termination message.
type lakeE2EResult struct {
	Pass     int32                           `json:"pass"`
	Fail     int32                           `json:"fail"`
	Skip     int32                           `json:"skip"`
	Revision string                          `json:"revision"`
	Failed   []oramav1alpha1.LakeE2ETestStep `json:"failed"`
	Skipped  []oramav1alpha1.LakeE2ETestStep `json:"skipped"`
}

// lakeE2ESummaryRe matches the runner's last log line, when there is no termination message.
var lakeE2ESummaryRe = regexp.MustCompile(`Lake e2e: pass=([0-9]+) fail=([0-9]+) skip=([0-9]+)`)

// runResult reads the finished run's summary from its pod: the termination message the runner
// writes, or, when the runner ended without one, the summary line of its log.
func (r *LakeE2ETestReconciler) runResult(ctx context.Context, namespace, jobName string) (lakeE2EResult, bool) {
	if r.Clientset == nil {
		return lakeE2EResult{}, false
	}
	pods, err := r.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return lakeE2EResult{}, false
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name != "e2e" || container.State.Terminated == nil {
				continue
			}
			if result, ok := parseLakeE2EResult(container.State.Terminated.Message); ok {
				return result, true
			}
		}
		raw, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "e2e"}).DoRaw(ctx)
		if err == nil {
			if result, ok := parseLakeE2EResult(string(raw)); ok {
				return result, true
			}
		}
	}
	return lakeE2EResult{}, false
}

// parseLakeE2EResult reads the runner's JSON summary, or the last summary line of a log.
func parseLakeE2EResult(text string) (lakeE2EResult, bool) {
	var result lakeE2EResult
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &result) == nil {
		return result, true
	}
	matches := lakeE2ESummaryRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return lakeE2EResult{}, false
	}
	last := matches[len(matches)-1]
	pass, _ := strconv.Atoi(last[1])
	fail, _ := strconv.Atoi(last[2])
	skip, _ := strconv.Atoi(last[3])
	return lakeE2EResult{Pass: int32(pass), Fail: int32(fail), Skip: int32(skip)}, true
}

func capSteps(steps []oramav1alpha1.LakeE2ETestStep) []oramav1alpha1.LakeE2ETestStep {
	if len(steps) > lakeE2EMaxListed {
		steps = steps[:lakeE2EMaxListed]
	}
	if len(steps) == 0 {
		return nil
	}
	return steps
}

// runVerdict is the phase and message of a finished run.
func runVerdict(succeeded, read bool, result lakeE2EResult) (string, string) {
	if !read {
		if succeeded {
			return "Succeeded", "the runner passed but left no result; see the Job's pod log"
		}
		return "Failed", "the runner ended without a result (it crashed, ran out of time or memory); see the Job's pod log"
	}
	counts := fmt.Sprintf("%d passed, %d failed, %d skipped", result.Pass, result.Fail, result.Skip)
	if succeeded && result.Fail == 0 {
		return "Succeeded", counts
	}
	if len(result.Failed) > 0 {
		return "Failed", fmt.Sprintf("%s; first failure: %s: %s", counts, result.Failed[0].Step, result.Failed[0].Reason)
	}
	return "Failed", counts
}

// removeOlderRuns deletes this test's Jobs of earlier runs once a newer run has finished.
func (r *LakeE2ETestReconciler) removeOlderRuns(ctx context.Context, test *oramav1alpha1.LakeE2ETest, namespace, current string) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{lakeE2ELabel: test.Name}); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == current || !metav1.IsControlledBy(job, test) {
			continue
		}
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func lakeReadyForE2E(lake *oramav1alpha1.Lake) bool {
	return lake.Status.Phase == "Ready" &&
		lake.Status.ReadyReplicas > 0 &&
		lake.Status.PostgresReady &&
		lake.Status.SchemaPhase == "Applied"
}

// writeStatus writes the status subresource only when it changed (principle 7: a steady test
// makes no writes).
func (r *LakeE2ETestReconciler) writeStatus(
	ctx context.Context,
	test *oramav1alpha1.LakeE2ETest,
	status *oramav1alpha1.LakeE2ETestStatus,
	result ctrl.Result,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&test.Status, status) {
		return result, nil
	}
	test.Status = *status
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
		if lakeE2ETarget(&tests.Items[i]) != lake.Name {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: tests.Items[i].Name},
		})
	}
	return requests
}

// SetupWithManager registers the Lake test, its Jobs and target readiness watches.
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
