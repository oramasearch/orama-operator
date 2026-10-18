/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func readyLake(name string, labels map[string]string) *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake-" + name,
			Image:          "oramasearch/lake:sha-1234",
			PullSecretName: "target-pull-secret",
			IngressEnabled: true,
			Host:           name + ".lake.example",
		},
		Status: oramav1alpha1.LakeStatus{Phase: "Ready", ReadyReplicas: 1, PostgresReady: true, SchemaPhase: "Applied"},
	}
}

func memberTest(generation int64) *oramav1alpha1.LakeE2ETest {
	return &oramav1alpha1.LakeE2ETest{
		ObjectMeta: metav1.ObjectMeta{Name: "gate", UID: types.UID("run-uid"), Generation: generation},
		Spec: oramav1alpha1.LakeE2ETestSpec{
			Member: &oramav1alpha1.LakeE2ETestMember{LakeSet: "lake-nonprod", Name: "testing"},
			Image:  "oramasearch/lake-e2e:dev-abc1234",
		},
	}
}

type lakeE2EFixture struct {
	client     client.Client
	clientset  *kubefake.Clientset
	reconciler *LakeE2ETestReconciler
	request    ctrl.Request
}

func newLakeE2EFixture(t *testing.T, objects ...client.Object) *lakeE2EFixture {
	t.Helper()
	scheme := newLakeE2ETestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.LakeE2ETest{}, &oramav1alpha1.Lake{}, &batchv1.Job{}).
		Build()
	clientset := kubefake.NewSimpleClientset()
	return &lakeE2EFixture{
		client:     c,
		clientset:  clientset,
		reconciler: &LakeE2ETestReconciler{Client: c, Scheme: scheme, Clientset: clientset},
		request:    ctrl.Request{NamespacedName: types.NamespacedName{Name: "gate"}},
	}
}

func (f *lakeE2EFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := f.reconciler.Reconcile(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *lakeE2EFixture) test(t *testing.T) *oramav1alpha1.LakeE2ETest {
	t.Helper()
	var test oramav1alpha1.LakeE2ETest
	if err := f.client.Get(context.Background(), f.request.NamespacedName, &test); err != nil {
		t.Fatal(err)
	}
	return &test
}

func (f *lakeE2EFixture) jobs(t *testing.T) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := f.client.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	return jobs.Items
}

// finish marks a run's Job finished and gives the clientset its pod with the runner's summary.
func (f *lakeE2EFixture) finish(t *testing.T, namespace, name string, succeeded bool, message string) {
	t.Helper()
	job := &batchv1.Job{}
	if err := f.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, job); err != nil {
		t.Fatal(err)
	}
	condition := batchv1.JobComplete
	if !succeeded {
		condition = batchv1.JobFailed
		job.Status.Failed = 1
	} else {
		job.Status.Succeeded = 1
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
	if err := f.client.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: namespace, Labels: map[string]string{"job-name": name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "e2e",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
	if _, err := f.clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestLakeE2ETestWaitsForTarget(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1))
	if result := f.reconcile(t); result.RequeueAfter != 15*time.Second {
		t.Fatalf("requeue = %s", result.RequeueAfter)
	}
	if status := f.test(t).Status; status.Phase != "Pending" || status.JobName != "" || status.Target != "testing" {
		t.Fatalf("status = %+v", status)
	}
	if jobs := f.jobs(t); len(jobs) != 0 {
		t.Fatalf("created %d Jobs without a target", len(jobs))
	}
}

// A member's Lake must carry its set's label: a Lake of the same name that the set does not
// manage is never tested in its place.
func TestLakeE2ETestRefusesALakeTheSetDoesNotManage(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1), readyLake("testing", map[string]string{managedBySetLabel: "LakeSet.lake-prod"}))
	f.reconcile(t)
	status := f.test(t).Status
	if status.Phase != "Pending" || !strings.Contains(status.Message, "not a member of LakeSet lake-nonprod") {
		t.Fatalf("status = %+v", status)
	}
	if jobs := f.jobs(t); len(jobs) != 0 {
		t.Fatalf("created %d Jobs against another set's Lake", len(jobs))
	}
}

func TestLakeE2ETestRunsAgainstAMemberAndReportsItsResult(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1), readyLake("testing", map[string]string{managedBySetLabel: "LakeSet.lake-nonprod"}))
	f.reconcile(t)
	job := &batchv1.Job{}
	if err := f.client.Get(context.Background(), types.NamespacedName{Namespace: "lake-testing", Name: "gate-e2e-1"}, job); err != nil {
		t.Fatal(err)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "LakeE2ETest" {
		t.Fatalf("owner references = %+v", job.OwnerReferences)
	}
	if *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != lakeE2EDefaultTimeout {
		t.Fatalf("backoff %d, deadline %v", *job.Spec.BackoffLimit, job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "target-pull-secret" {
		t.Fatalf("image pull secrets = %+v", pod.ImagePullSecrets)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("automountServiceAccountToken = %v", pod.AutomountServiceAccountToken)
	}
	container := pod.Containers[0]
	if container.Image != "oramasearch/lake-e2e:dev-abc1234" || container.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("container = %+v", container)
	}
	if container.Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("memory limit = %s", container.Resources.Limits.Memory())
	}
	env := map[string]string{}
	for _, value := range container.Env {
		env[value.Name] = value.Value
	}
	if env["LAKE_E2E_BASE_URL"] != "http://lake-testing.lake-testing.svc.cluster.local" ||
		env["LAKE_E2E_PUBLIC_URL"] != "https://testing.lake.example" ||
		!strings.HasSuffix(env["LAKE_E2E_RUN_ID"], "-run-uid") || len(env["LAKE_E2E_RUN_ID"]) != len("01234567-run-uid") {
		t.Fatalf("env = %+v", env)
	}
	status := f.test(t).Status
	if status.Phase != "Running" || status.JobName != "gate-e2e-1" || status.ObservedGeneration != 1 || status.StartedAt == nil || status.RunID != env["LAKE_E2E_RUN_ID"] {
		t.Fatalf("status = %+v", status)
	}

	f.finish(t, "lake-testing", "gate-e2e-1", false,
		`{"pass":180,"fail":2,"skip":3,"revision":"51fe838b","failed":[{"step":"account.mfa","reason":"the otpauth secret is not base32"},{"step":"repositories.pages","reason":"/protection could not load"}],"skipped":[{"step":"ship.services","reason":"LAKE_E2E_QUICK"}]}`)
	f.reconcile(t)
	status = f.test(t).Status
	if status.Phase != "Failed" || status.Pass != 180 || status.Fail != 2 || status.Skip != 3 || status.LakeRevision != "51fe838b" || status.CompletedAt == nil {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Failed) != 2 || status.Failed[0].Step != "account.mfa" || len(status.Skipped) != 1 || status.Skipped[0].Reason != "LAKE_E2E_QUICK" {
		t.Fatalf("steps = %+v / %+v", status.Failed, status.Skipped)
	}
	if !strings.Contains(status.Message, "180 passed, 2 failed, 3 skipped; first failure: account.mfa") {
		t.Fatalf("message = %q", status.Message)
	}
}

// Changing the spec (spec.runID) runs the suite again in a new Job without deleting the test, and
// the earlier run's Job goes once the new run has finished.
func TestLakeE2ETestRerunsOnANewGeneration(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1), readyLake("testing", map[string]string{managedBySetLabel: "LakeSet.lake-nonprod"}))
	f.reconcile(t)
	f.finish(t, "lake-testing", "gate-e2e-1", true, `{"pass":10,"fail":0,"skip":0}`)
	f.reconcile(t)
	first := f.test(t)
	if first.Status.Phase != "Succeeded" || first.Status.Pass != 10 || first.Status.Message != "10 passed, 0 failed, 0 skipped" {
		t.Fatalf("first run status = %+v", first.Status)
	}

	// A finished run is not started again for the same generation, even when its Job is gone.
	if err := f.client.Delete(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "lake-testing", Name: "gate-e2e-1"}}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if jobs := f.jobs(t); len(jobs) != 0 {
		t.Fatalf("a finished run was started again: %d Jobs", len(jobs))
	}

	first.Spec.RunID = "release-v0.2.0"
	first.Generation = 2
	if err := f.client.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	second := f.test(t)
	if second.Status.Phase != "Running" || second.Status.JobName != "gate-e2e-2" || second.Status.ObservedGeneration != 2 || second.Status.Pass != 0 {
		t.Fatalf("second run status = %+v", second.Status)
	}
	if second.Status.RunID == first.Status.RunID {
		t.Fatalf("the second run reuses the first run's id %q", first.Status.RunID)
	}
	f.finish(t, "lake-testing", "gate-e2e-2", true, "lake-e2e: ...\nLake e2e: pass=12 fail=0 skip=1\n")
	f.reconcile(t)
	if status := f.test(t).Status; status.Phase != "Succeeded" || status.Pass != 12 || status.Skip != 1 {
		t.Fatalf("second run result from the log line = %+v", status)
	}
}

// Older runs' Jobs are removed when a newer run finishes.
func TestLakeE2ETestRemovesOlderRunsJobs(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1), readyLake("testing", map[string]string{managedBySetLabel: "LakeSet.lake-nonprod"}))
	f.reconcile(t)
	test := f.test(t)
	test.Generation = 2
	if err := f.client.Update(context.Background(), test); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if jobs := f.jobs(t); len(jobs) != 2 {
		t.Fatalf("want both runs' Jobs while the newer runs, got %d", len(jobs))
	}
	f.finish(t, "lake-testing", "gate-e2e-2", true, `{"pass":1}`)
	f.reconcile(t)
	jobs := f.jobs(t)
	if len(jobs) != 1 || jobs[0].Name != "gate-e2e-2" {
		t.Fatalf("jobs after the newer run finished = %d", len(jobs))
	}
}

// A runner that ends without any result fails the run with a reason that says so.
func TestLakeE2ETestRunnerWithoutAResult(t *testing.T) {
	f := newLakeE2EFixture(t, memberTest(1), readyLake("testing", map[string]string{managedBySetLabel: "LakeSet.lake-nonprod"}))
	f.reconcile(t)
	f.finish(t, "lake-testing", "gate-e2e-1", false, "Killed")
	f.reconcile(t)
	if status := f.test(t).Status; status.Phase != "Failed" || !strings.Contains(status.Message, "without a result") {
		t.Fatalf("status = %+v", status)
	}
}

// spec.targetRef still names a Lake that no set manages.
func TestLakeE2ETestTargetRef(t *testing.T) {
	test := memberTest(1)
	test.Spec.Member = nil
	test.Spec.TargetRef = "hello"
	test.Spec.Env = []corev1.EnvVar{{Name: "LAKE_E2E_GROUPS", Value: "repositories"}}
	lake := readyLake("hello", nil)
	lake.Spec.IngressEnabled = false
	f := newLakeE2EFixture(t, test, lake)
	f.reconcile(t)
	job := &batchv1.Job{}
	if err := f.client.Get(context.Background(), types.NamespacedName{Namespace: "lake-hello", Name: "gate-e2e-1"}, job); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, value := range job.Spec.Template.Spec.Containers[0].Env {
		env[value.Name] = value.Value
	}
	if _, public := env["LAKE_E2E_PUBLIC_URL"]; public || env["LAKE_E2E_GROUPS"] != "repositories" {
		t.Fatalf("env = %+v", env)
	}
}

func TestLakeE2EJobNameFitsALabel(t *testing.T) {
	test := memberTest(12345)
	test.Name = strings.Repeat("a", 70)
	if name := lakeE2EJobName(test); len(name) > 63 || !strings.HasSuffix(name, "-e2e-12345") {
		t.Fatalf("job name %q (%d)", name, len(name))
	}
}

func newLakeE2ETestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		batchv1.AddToScheme,
		oramav1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}
