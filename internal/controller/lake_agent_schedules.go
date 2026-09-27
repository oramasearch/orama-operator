/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The cron schedules of lake's user-created Python agents (lake#244; the contract is lake's
// docs/api/agent-launches.md and this repo's docs/lake/agents.md). An agent may have any number;
// each is a row of lake's agent_cron_schedules table (migration 0031), read on the services poll,
// and each present one is its own CronJob, <lake>-<slug>, whose Job template is a job launch's.
// Every run authenticates with the schedule's credential, las.<schedule id>.<secret>, which the
// operator generates into a Secret; it writes only the SHA-256 into agent_cron_schedule_status,
// and one agent_cron_runs row per Job the CronJob starts.
const (
	agentScheduleIDLabel = "orama.cloud/agent-schedule-id"

	agentScheduleStartingDeadline = int64(300)
	agentScheduleHistory          = int32(3)
)

var agentScheduleSlugPattern = regexp.MustCompile(`^as-[0-9a-f]{12}$`)

// errAgentSchedulesTableMissing: lake's schema has not reached 0031_agent_cron_schedules yet.
var errAgentSchedulesTableMissing = errors.New("the agent_cron_schedules table does not exist yet (lake schema before 0031_agent_cron_schedules)")

// LakeAgentSchedule is one row of lake's agent_cron_schedules joined to its status row, with the
// phase of each run already written (by run id) so a finished run is written once.
type LakeAgentSchedule struct {
	ID         string
	CustomerID string
	AgentID    string
	Cron       string
	TimeZone   string
	// Input is the raw jsonb input every run is asked to do.
	Input      string
	Slug       string
	Desired    string
	Generation int64
	Status     *LakeAgentScheduleStatus
	RunPhases  map[string]string
}

// LakeAgentScheduleStatus is one agent_cron_schedule_status row, written only by the operator.
type LakeAgentScheduleStatus struct {
	ScheduleID         string
	Phase              string
	Message            string
	LastScheduleTime   *time.Time
	CredentialSHA256   string
	ObservedGeneration int64
}

func (s LakeAgentScheduleStatus) equal(other LakeAgentScheduleStatus) bool {
	sameLast := (s.LastScheduleTime == nil && other.LastScheduleTime == nil) ||
		(s.LastScheduleTime != nil && other.LastScheduleTime != nil && s.LastScheduleTime.Equal(*other.LastScheduleTime))
	return s.ScheduleID == other.ScheduleID && s.Phase == other.Phase && s.Message == other.Message && sameLast &&
		s.CredentialSHA256 == other.CredentialSHA256 && s.ObservedGeneration == other.ObservedGeneration
}

// LakeAgentScheduleRun is one agent_cron_runs row: one Job a schedule's CronJob started.
type LakeAgentScheduleRun struct {
	ID         string
	ScheduleID string
	JobName    string
	Phase      string
	Message    string
	ExitCode   *int32
	Result     string
	StartedAt  time.Time
	FinishedAt *time.Time
}

func agentScheduleName(lake, slug string) string { return lake + "-" + slug }

func agentScheduleCredentialSecret(lake, slug string) string {
	return agentScheduleName(lake, slug) + "-credential"
}

func agentPhaseFinal(phase string) bool {
	return phase == AgentPhaseSucceeded || phase == AgentPhaseFailed
}

// reconcileAgentSchedule makes one present schedule a CronJob, reports its runs, and returns its
// status and whether a run is still under way.
func (r *LakeReconciler) reconcileAgentSchedule(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, store LakeServiceStore, schedule *LakeAgentSchedule) (LakeAgentScheduleStatus, bool, error) {
	status := LakeAgentScheduleStatus{ScheduleID: schedule.ID, ObservedGeneration: schedule.Generation}
	switch {
	case !agentScheduleSlugPattern.MatchString(schedule.Slug):
		status.Phase, status.Message = AgentPhaseFailed, "slug must match "+agentScheduleSlugPattern.String()
		return status, false, nil
	case schedule.Cron == "":
		status.Phase, status.Message = AgentPhaseFailed, "the schedule has no cron expression"
		return status, false, nil
	case lake.Spec.Agents.RuntimeImage == "":
		status.Phase, status.Message = AgentPhaseFailed, "agents are not enabled on this Lake (spec.agents.runtimeImage)"
		return status, false, nil
	}
	credential, err := r.reconcileAgentCredential(ctx, lake, namespace, labels,
		agentScheduleCredentialSecret(lake.Name, schedule.Slug), agentScheduleIDLabel, schedule.ID, "las.")
	if err != nil {
		return status, true, err
	}
	status.CredentialSHA256 = credentialDigest(credential)

	name := agentScheduleName(lake.Name, schedule.Slug)
	scheduleLabels := agentLabels(labels, "agent-runtime", name, map[string]string{agentScheduleIDLabel: schedule.ID})
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cron, func() error {
		if !cron.CreationTimestamp.IsZero() && cron.Labels[agentScheduleIDLabel] != schedule.ID {
			return fmt.Errorf("CronJob %s/%s exists and is not this schedule's", namespace, name)
		}
		timeZone := schedule.TimeZone
		if timeZone == "" {
			timeZone = "Etc/UTC"
		}
		deadline, starting := agentJobDeadline(lake), agentScheduleStartingDeadline
		backoff, history := int32(0), agentScheduleHistory
		env := agentRunEnv(lake, namespace, agentScheduleCredentialSecret(lake.Name, schedule.Slug),
			schedule.CustomerID, schedule.AgentID, agentLaunchModeJob, schedule.Input)
		cron.Labels = scheduleLabels
		cron.Spec.Schedule = schedule.Cron
		cron.Spec.TimeZone = &timeZone
		cron.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cron.Spec.StartingDeadlineSeconds = &starting
		cron.Spec.SuccessfulJobsHistoryLimit = &history
		cron.Spec.FailedJobsHistoryLimit = &history
		cron.Spec.JobTemplate = batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: scheduleLabels},
			Spec: batchv1.JobSpec{
				BackoffLimit:          &backoff,
				ActiveDeadlineSeconds: &deadline,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: scheduleLabels},
					Spec:       agentJobPod(lake, env),
				},
			},
		}
		return controllerutil.SetControllerReference(lake, cron, r.Scheme)
	}); apierrors.IsInvalid(err) {
		// The API server refused the cron or the time zone: nothing to retry until lake changes it.
		status.Phase, status.Message = AgentPhaseFailed, "Kubernetes refused the schedule: "+err.Error()
		return status, false, nil
	} else if err != nil {
		return status, true, fmt.Errorf("reconcile agent CronJob %s: %w", name, err)
	}
	status.Phase, status.Message = AgentPhaseReady, "scheduled"
	if last := cron.Status.LastScheduleTime; last != nil {
		at := last.UTC().Truncate(time.Second)
		status.LastScheduleTime = &at
	}
	running, err := r.reportAgentScheduleRuns(ctx, namespace, store, schedule)
	if err != nil {
		return status, true, err
	}
	return status, running, nil
}

// reportAgentScheduleRuns writes a run row for every Job of the schedule whose phase changed. A
// run already written as finished is never written again.
func (r *LakeReconciler) reportAgentScheduleRuns(ctx context.Context, namespace string, store LakeServiceStore, schedule *LakeAgentSchedule) (bool, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{agentScheduleIDLabel: schedule.ID}); err != nil {
		return true, err
	}
	running := false
	for i := range jobs.Items {
		job := &jobs.Items[i]
		id := string(job.UID)
		if agentPhaseFinal(schedule.RunPhases[id]) {
			continue
		}
		run := r.agentScheduleRun(ctx, namespace, schedule.ID, job)
		running = running || !agentPhaseFinal(run.Phase)
		if schedule.RunPhases[id] == run.Phase && !agentPhaseFinal(run.Phase) {
			continue
		}
		if err := store.UpsertAgentScheduleRun(ctx, run); err != nil {
			return true, err
		}
		if schedule.RunPhases == nil {
			schedule.RunPhases = map[string]string{}
		}
		schedule.RunPhases[id] = run.Phase
	}
	return running, nil
}

// agentScheduleRun is what one Job of a schedule shows: as a job launch, finished only once its
// pod's outcome is readable.
func (r *LakeReconciler) agentScheduleRun(ctx context.Context, namespace, scheduleID string, job *batchv1.Job) LakeAgentScheduleRun {
	run := LakeAgentScheduleRun{ID: string(job.UID), ScheduleID: scheduleID, JobName: job.Name, Result: "null",
		StartedAt: job.CreationTimestamp.UTC()}
	if job.Status.StartTime != nil {
		run.StartedAt = job.Status.StartTime.UTC()
	}
	finished := func() {
		at := time.Now().UTC()
		if job.Status.CompletionTime != nil {
			at = job.Status.CompletionTime.UTC()
		}
		run.FinishedAt = &at
	}
	switch {
	case job.Status.Succeeded > 0:
		run.ExitCode, run.Result = r.jobOutcome(ctx, namespace, job.Name)
		if run.ExitCode == nil {
			run.Phase, run.Message = AgentPhaseRunning, "collecting the result"
			return run
		}
		run.Phase, run.Message = AgentPhaseSucceeded, "finished"
		finished()
	case job.Status.Failed > 0:
		run.ExitCode, run.Result = r.jobOutcome(ctx, namespace, job.Name)
		run.Phase, run.Message = AgentPhaseFailed, "the agent did not finish"
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Message != "" {
				run.Message = condition.Message
			}
		}
		finished()
	case job.Status.Active > 0:
		run.Phase, run.Message = AgentPhaseRunning, "running"
	default:
		run.Phase, run.Message = AgentPhasePending, "starting the agent"
	}
	return run
}

// stopAgentSchedule removes an absent schedule's CronJob (and with it its Jobs) and credential.
func (r *LakeReconciler) stopAgentSchedule(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, schedule *LakeAgentSchedule) (LakeAgentScheduleStatus, error) {
	status := LakeAgentScheduleStatus{ScheduleID: schedule.ID, Phase: AgentPhaseStopped, Message: "removed",
		ObservedGeneration: schedule.Generation}
	if schedule.Status != nil {
		status.LastScheduleTime = schedule.Status.LastScheduleTime
	}
	if !agentScheduleSlugPattern.MatchString(schedule.Slug) {
		return status, nil
	}
	background := metav1.DeletePropagationBackground
	objects := []client.Object{
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: agentScheduleName(lake.Name, schedule.Slug), Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentScheduleCredentialSecret(lake.Name, schedule.Slug), Namespace: namespace}},
	}
	for _, object := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return status, err
		}
		if object.GetLabels()[agentScheduleIDLabel] != schedule.ID || !metav1.IsControlledBy(object, lake) {
			continue
		}
		if err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
			return status, err
		}
	}
	return status, nil
}

// reconcileLakeAgentSchedules applies one read of agent_cron_schedules and reports whether any
// schedule is still changing or has a run under way.
func (r *LakeReconciler) reconcileLakeAgentSchedules(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, store LakeServiceStore, schedules []LakeAgentSchedule) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	changing := false
	for i := range schedules {
		schedule := &schedules[i]
		var status LakeAgentScheduleStatus
		var err error
		switch schedule.Desired {
		case serviceDesiredAbsent:
			if schedule.Status != nil && schedule.Status.ObservedGeneration == schedule.Generation &&
				schedule.Status.Phase == AgentPhaseStopped && schedule.Status.CredentialSHA256 == "" {
				continue
			}
			status, err = r.stopAgentSchedule(ctx, lake, namespace, schedule)
		case serviceDesiredPresent:
			var still bool
			status, still, err = r.reconcileAgentSchedule(ctx, lake, namespace, labels, store, schedule)
			changing = changing || still
		default:
			status = LakeAgentScheduleStatus{ScheduleID: schedule.ID, Phase: AgentPhaseFailed,
				Message: "desired must be present or absent", ObservedGeneration: schedule.Generation}
		}
		if err != nil {
			log.Error(err, "agent schedule deferred", "schedule", schedule.ID, "slug", schedule.Slug)
			changing = true
			continue
		}
		if schedule.Status != nil && schedule.Status.equal(status) {
			continue
		}
		if err := store.UpsertAgentScheduleStatus(ctx, status); err != nil {
			log.Error(err, "agent schedule status write deferred", "schedule", schedule.ID)
			continue
		}
		copied := status
		schedule.Status = &copied
	}
	return changing
}
