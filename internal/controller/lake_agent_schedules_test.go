// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testScheduleID     = "77777777-7777-4777-8777-777777777777"
	testScheduleSlug   = "as-777777777777"
	testScheduleName   = "prod-as-777777777777"
	testScheduleIDB    = "88888888-8888-4888-8888-888888888888"
	testScheduleSlugB  = "as-888888888888"
	testScheduleNameB  = "prod-as-888888888888"
	testScheduleRunUID = "99999999-9999-4999-8999-999999999999"
)

func testSchedule(id, slug, cron, timeZone string) LakeAgentSchedule {
	return LakeAgentSchedule{ID: id, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testServiceNamespace, AgentID: "a-1", Cron: cron, TimeZone: timeZone,
		Input: `"the weekly churn check"`, Slug: slug, Desired: serviceDesiredPresent, Generation: 1}
}

func (f *fakeServiceStore) scheduleStatus(t *testing.T, id string) LakeAgentScheduleStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, schedule := range f.schedules {
		if schedule.ID == id && schedule.Status != nil {
			return *schedule.Status
		}
	}
	t.Fatalf("schedule %s has no status", id)
	return LakeAgentScheduleStatus{}
}

func (f *fakeServiceStore) runs() []LakeAgentScheduleRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LakeAgentScheduleRun(nil), f.runUpserts...)
}

func TestEachAgentScheduleIsItsOwnCronJobWithItsOwnCredential(t *testing.T) {
	store := &fakeServiceStore{schedules: []LakeAgentSchedule{
		testSchedule(testScheduleID, testScheduleSlug, "0 9 * * 1", ""),
		testSchedule(testScheduleIDB, testScheduleSlugB, "30 2 * * *", "America/Toronto"),
	}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	ctx := context.Background()

	var weekly, nightly batchv1.CronJob
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleName}, &weekly); err != nil {
		t.Fatalf("weekly CronJob: %v", err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleNameB}, &nightly); err != nil {
		t.Fatalf("a second schedule of the same agent is a second CronJob: %v", err)
	}
	if weekly.Spec.Schedule != "0 9 * * 1" || *weekly.Spec.TimeZone != "Etc/UTC" ||
		nightly.Spec.Schedule != "30 2 * * *" || *nightly.Spec.TimeZone != "America/Toronto" {
		t.Fatalf("schedules %q %q / %q %q", weekly.Spec.Schedule, *weekly.Spec.TimeZone, nightly.Spec.Schedule, *nightly.Spec.TimeZone)
	}
	if weekly.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Fatal("a schedule never runs over its own previous run")
	}
	job := weekly.Spec.JobTemplate.Spec
	if *job.BackoffLimit != 0 || job.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("a scheduled run is never retried behind the person's back")
	}
	for key, lake := range map[string]string{"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": "prod"} {
		if job.Template.Labels[key] == lake || weekly.Spec.JobTemplate.Labels[key] == lake {
			t.Fatalf("a scheduled pod carries lake's selector label %s=%s", key, lake)
		}
	}
	if job.Template.Labels[agentScheduleIDLabel] != testScheduleID {
		t.Fatal("a scheduled Job carries its schedule's id")
	}
	env := job.Template.Spec.Containers[0].Env
	if envValue(t, env, "AGENT_INPUT").Value != `"the weekly churn check"` || envValue(t, env, "AGENT_MODE").Value != "job" ||
		envValue(t, env, "LAKE_TOKEN").ValueFrom.SecretKeyRef.Name != testScheduleName+"-credential" {
		t.Fatalf("scheduled env %v", env)
	}

	var credential corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleName + "-credential"}, &credential); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	token := string(credential.Data["token"])
	if !strings.HasPrefix(token, "las."+testScheduleID+".") || len(token) != len("las.")+36+1+32 {
		t.Fatalf("credential %q", token)
	}
	status := store.scheduleStatus(t, testScheduleID)
	if status.Phase != AgentPhaseReady || status.CredentialSHA256 != credentialDigest(token) {
		t.Fatalf("status %+v", status)
	}
	for _, variable := range env {
		if strings.Contains(variable.Value, token) {
			t.Fatal("the credential is never a plain env value")
		}
	}
	h.poll()
	_ = h.client.Get(ctx, client.ObjectKeyFromObject(&credential), &credential)
	if string(credential.Data["token"]) != token {
		t.Fatal("a schedule's credential is generated once")
	}
}

func TestAScheduledRunIsReportedOnceItsOutcomeIsReadable(t *testing.T) {
	store := &fakeServiceStore{schedules: []LakeAgentSchedule{testSchedule(testScheduleID, testScheduleSlug, "0 9 * * 1", "")}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	ctx := context.Background()

	// What the CronJob controller would start: a Job from the template.
	var cron batchv1.CronJob
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleName}, &cron); err != nil {
		t.Fatal(err)
	}
	jobName := testScheduleName + "-29000000"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: testServiceNamespace, UID: types.UID(testScheduleRunUID),
			Labels: cron.Spec.JobTemplate.Labels},
		Spec: cron.Spec.JobTemplate.Spec,
	}
	if err := h.client.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 1
	if err := h.client.Status().Update(ctx, job); err != nil {
		_ = h.client.Update(ctx, job)
	}
	h.poll()
	runs := store.runs()
	if len(runs) != 1 || runs[0].ID != testScheduleRunUID || runs[0].Phase != AgentPhaseRunning || runs[0].JobName != jobName {
		t.Fatalf("a running scheduled Job is reported: %+v", runs)
	}
	if !h.changing {
		t.Fatal("a run under way keeps the poll fast")
	}
	h.poll()
	if len(store.runs()) != 1 {
		t.Fatal("an unchanged run is not written again")
	}

	job.Status.Active, job.Status.Succeeded = 0, 1
	if err := h.client.Status().Update(ctx, job); err != nil {
		_ = h.client.Update(ctx, job)
	}
	h.poll()
	if runs := store.runs(); runs[len(runs)-1].Phase != AgentPhaseRunning {
		t.Fatalf("a success whose pod outcome is not readable yet is not final: %+v", runs[len(runs)-1])
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-x", Namespace: testServiceNamespace, Labels: map[string]string{"job-name": jobName}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"reply":"3 at risk","exit":0}`}}}}},
	}
	if err := h.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Status().Update(ctx, pod); err != nil {
		_ = h.client.Update(ctx, pod)
	}
	h.poll()
	runs = store.runs()
	last := runs[len(runs)-1]
	if last.Phase != AgentPhaseSucceeded || last.ExitCode == nil || *last.ExitCode != 0 ||
		!strings.Contains(last.Result, "3 at risk") || last.FinishedAt == nil {
		t.Fatalf("finished run %+v", last)
	}
	written := len(runs)
	h.poll()
	if len(store.runs()) != written {
		t.Fatal("a finished run is written once")
	}
	if status := store.scheduleStatus(t, testScheduleID); status.Phase != AgentPhaseReady || status.CredentialSHA256 == "" {
		t.Fatalf("the schedule stays Ready with its credential between runs: %+v", status)
	}
}

func TestARemovedScheduleLosesItsCronJobAndCredential(t *testing.T) {
	store := &fakeServiceStore{schedules: []LakeAgentSchedule{testSchedule(testScheduleID, testScheduleSlug, "0 9 * * 1", "")}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	ctx := context.Background()
	store.mu.Lock()
	store.schedules[0].Desired = serviceDesiredAbsent
	store.schedules[0].Generation = 2
	store.mu.Unlock()
	h.poll()
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleName}, &batchv1.CronJob{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the CronJob is deleted, got %v", err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testScheduleName + "-credential"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the credential is deleted, got %v", err)
	}
	status := store.scheduleStatus(t, testScheduleID)
	if status.Phase != AgentPhaseStopped || status.CredentialSHA256 != "" || status.ObservedGeneration != 2 {
		t.Fatalf("status %+v", status)
	}
	writes := len(store.scheduleUpserts)
	h.poll()
	if len(store.scheduleUpserts) != writes {
		t.Fatal("a stopped schedule is left alone")
	}
}

func TestAScheduleWithoutAValidSlugFailsAndOneWithoutAnImageRunsOnTheDefault(t *testing.T) {
	bad := testSchedule(testScheduleID, "not-a-slug", "0 9 * * 1", "")
	store := &fakeServiceStore{schedules: []LakeAgentSchedule{bad}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	if status := store.scheduleStatus(t, testScheduleID); status.Phase != AgentPhaseFailed {
		t.Fatalf("status %+v", status)
	}
	// A Lake that states no agent image schedules on the default (operator#539).
	store = &fakeServiceStore{schedules: []LakeAgentSchedule{testSchedule(testScheduleID, testScheduleSlug, "0 9 * * 1", "")}}
	h = newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	if status := store.scheduleStatus(t, testScheduleID); status.Phase == AgentPhaseFailed {
		t.Fatalf("status %+v", status)
	}
}
