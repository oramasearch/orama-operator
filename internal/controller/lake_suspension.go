/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A suspended Lake (spec.suspended, operator#862) runs no compute and keeps all of its data. A
// LakeOrderSource sets it while lake's cloud_order_desired_state says the Lake's order is not paid
// for (lake#972); anyone else may set it too, with the same effect.
//
// At once:
//   - the lake Deployment runs no replica, and no control row is acted on, so no tenant workload
//     starts and nothing missing is made again;
//   - every Deployment the Lake controls (library and launched agents, served models, the rest) is
//     scaled to zero, its replica count kept in lakeSuspendedReplicasAnnotation;
//   - every unfinished Job (builds, methodology runs, agent launches) and every CronJob the Lake
//     controls is suspended, marked with lakeSuspendedAnnotation;
//   - every LakeApp and LakeEnvironment of the Lake runs no replica (their own reconcilers read
//     the Lake).
//
// After lakeSuspensionZettaGrace from status.suspendedSince, every OramaZettaCluster the Lake
// controls (its own, its workspaces', its Zetta services) is suspended too: the StatefulSets go
// to zero and their claims stay. The grace keeps the data readable for a renewal that comes soon,
// and keeps a Lake whose payment is late from losing its Zettas' warm state at once.
//
// Nothing is deleted. Volumes, claims, Secrets, Services, the control Postgres, managed services
// (CNPG, K8ssandra, TiDB, Valkey, Kafka, ClickHouse), namespaces, hosts and names all stay. When
// spec.suspended is false again, every object carrying a mark gets back exactly what it had, and
// the control rows are acted on as before.

// lakeSuspensionZettaGrace is how long a suspended Lake's Zettas keep running. The same on every
// cluster, with no setting.
const lakeSuspensionZettaGrace = 72 * time.Hour

const (
	// lakeSuspendedReplicasAnnotation keeps the replica count a Deployment had when its Lake was
	// suspended.
	lakeSuspendedReplicasAnnotation = "orama.cloud/suspended-replicas"
	// lakeSuspendedAnnotation marks a Job or CronJob the operator suspended for its Lake; its
	// value is the spec.suspend it had ("true" or "false").
	lakeSuspendedAnnotation = "orama.cloud/suspended-from"
)

// lakeSuspendedSince is the status.suspendedSince a Lake should have at now.
func lakeSuspendedSince(lake *oramav1alpha1.Lake, now time.Time) *metav1.Time {
	if !lake.Spec.Suspended {
		return nil
	}
	if lake.Status.SuspendedSince != nil {
		return lake.Status.SuspendedSince
	}
	since := metav1.NewTime(now.UTC().Truncate(time.Second))
	return &since
}

// lakeZettasSuspended is whether a Lake's Zettas are stopped at now: it is suspended, and its
// grace has passed since status.suspendedSince.
func lakeZettasSuspended(lake *oramav1alpha1.Lake, now time.Time) bool {
	if !lake.Spec.Suspended || lake.Status.SuspendedSince == nil {
		return false
	}
	return !now.Before(lake.Status.SuspendedSince.Add(lakeSuspensionZettaGrace))
}

// lakeSuspensionReadiness is the readiness entry of a suspended Lake.
func lakeSuspensionReadiness(lake *oramav1alpha1.Lake, now time.Time) lakeComponentReadiness {
	if lakeZettasSuspended(lake, now) {
		return lakeComponentReadiness{"Progressing", "Suspended",
			"suspended: no compute runs, its Zettas included; data, volumes and names are kept until spec.suspended is false"}
	}
	message := "suspended: no lake process, agent, app, build or run is running; its Zettas keep running for the grace"
	if lake.Status.SuspendedSince != nil {
		message += " until " + lake.Status.SuspendedSince.Add(lakeSuspensionZettaGrace).UTC().Format(time.RFC3339)
	}
	return lakeComponentReadiness{"Progressing", "Suspended", message}
}

// lakeSuspensionRequeue is when a suspended Lake must be looked at again for its Zetta grace to
// end on time; zero when nothing is pending.
func lakeSuspensionRequeue(lake *oramav1alpha1.Lake, now time.Time) time.Duration {
	if !lake.Spec.Suspended || lake.Status.SuspendedSince == nil || lakeZettasSuspended(lake, now) {
		return 0
	}
	wait := lake.Status.SuspendedSince.Add(lakeSuspensionZettaGrace).Sub(now)
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// reconcileLakeSuspension stops what the Lake controls while it is suspended, and gives back what
// a suspension took once it is not. It only scales and suspends; it never deletes. The lake
// Deployment itself follows spec.suspended through its replica count and is skipped here.
func (r *LakeReconciler) reconcileLakeSuspension(ctx context.Context, lake *oramav1alpha1.Lake, now time.Time) error {
	suspended := lake.Spec.Suspended
	lakeDeployment := client.ObjectKey{Namespace: lakeNamespace(lake), Name: "lake-" + lake.Name}
	var errs []error

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments); err != nil {
		errs = append(errs, fmt.Errorf("list Deployments: %w", err))
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if !metav1.IsControlledBy(deployment, lake) || client.ObjectKeyFromObject(deployment) == lakeDeployment {
			continue
		}
		before := deployment.DeepCopy()
		if !suspendDeployment(deployment, suspended) {
			continue
		}
		if err := r.Patch(ctx, deployment, client.MergeFrom(before)); err != nil {
			errs = append(errs, fmt.Errorf("Deployment %s: %w", client.ObjectKeyFromObject(deployment), err))
		}
	}

	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil {
		errs = append(errs, fmt.Errorf("list Jobs: %w", err))
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, lake) {
			continue
		}
		before := job.DeepCopy()
		if !suspendJob(job, suspended) {
			continue
		}
		if err := r.Patch(ctx, job, client.MergeFrom(before)); err != nil {
			errs = append(errs, fmt.Errorf("Job %s: %w", client.ObjectKeyFromObject(job), err))
		}
	}

	var cronJobs batchv1.CronJobList
	if err := r.List(ctx, &cronJobs); err != nil {
		errs = append(errs, fmt.Errorf("list CronJobs: %w", err))
	}
	for i := range cronJobs.Items {
		cronJob := &cronJobs.Items[i]
		if !metav1.IsControlledBy(cronJob, lake) {
			continue
		}
		before := cronJob.DeepCopy()
		if !suspendCronJob(cronJob, suspended) {
			continue
		}
		if err := r.Patch(ctx, cronJob, client.MergeFrom(before)); err != nil {
			errs = append(errs, fmt.Errorf("CronJob %s: %w", client.ObjectKeyFromObject(cronJob), err))
		}
	}

	// Every Zetta the Lake controls: its own (also set in reconcileLakeZetta), its workspaces'
	// and its Zetta services'.
	zettasSuspended := lakeZettasSuspended(lake, now)
	children, err := r.lakeZettaChildren(ctx, lake)
	if err != nil {
		errs = append(errs, fmt.Errorf("list OramaZettaClusters: %w", err))
	}
	for i := range children {
		cluster := &children[i]
		if cluster.Spec.Suspended == zettasSuspended || !cluster.DeletionTimestamp.IsZero() {
			continue
		}
		before := cluster.DeepCopy()
		cluster.Spec.Suspended = zettasSuspended
		if err := r.Patch(ctx, cluster, client.MergeFrom(before)); err != nil {
			errs = append(errs, fmt.Errorf("OramaZettaCluster %s: %w", client.ObjectKeyFromObject(cluster), err))
		}
	}
	return errors.Join(errs...)
}

// suspendDeployment scales a Lake-controlled Deployment to zero while suspended, keeping its
// replica count, and gives the count back once not. It reports whether it changed anything.
func suspendDeployment(deployment *appsv1.Deployment, suspended bool) bool {
	kept, marked := deployment.Annotations[lakeSuspendedReplicasAnnotation]
	if suspended {
		current := replicasOf(deployment.Spec.Replicas)
		if marked && current == 0 {
			return false
		}
		if !marked {
			if deployment.Annotations == nil {
				deployment.Annotations = map[string]string{}
			}
			deployment.Annotations[lakeSuspendedReplicasAnnotation] = strconv.Itoa(int(current))
		}
		deployment.Spec.Replicas = int32Ptr(0)
		return true
	}
	if !marked {
		return false
	}
	replicas, err := strconv.ParseInt(kept, 10, 32)
	if err != nil || replicas < 0 {
		// Unreadable: the owner's own reconcile sets the count it wants on its next pass.
		replicas = 1
	}
	deployment.Spec.Replicas = int32Ptr(int32(replicas))
	delete(deployment.Annotations, lakeSuspendedReplicasAnnotation)
	return true
}

// suspendJob suspends an unfinished Lake-controlled Job while suspended (Kubernetes stops its
// pods and keeps the Job), and gives back its own spec.suspend once not.
func suspendJob(job *batchv1.Job, suspended bool) bool {
	kept, marked := job.Annotations[lakeSuspendedAnnotation]
	if suspended {
		if marked || jobEnded(job) {
			return false
		}
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[lakeSuspendedAnnotation] = strconv.FormatBool(job.Spec.Suspend != nil && *job.Spec.Suspend)
		job.Spec.Suspend = boolPtr(true)
		return true
	}
	if !marked {
		return false
	}
	job.Spec.Suspend = boolPtr(kept == "true")
	delete(job.Annotations, lakeSuspendedAnnotation)
	return true
}

// suspendCronJob suspends a Lake-controlled CronJob while suspended, and gives back its own
// spec.suspend once not.
func suspendCronJob(cronJob *batchv1.CronJob, suspended bool) bool {
	kept, marked := cronJob.Annotations[lakeSuspendedAnnotation]
	if suspended {
		if marked {
			return false
		}
		if cronJob.Annotations == nil {
			cronJob.Annotations = map[string]string{}
		}
		cronJob.Annotations[lakeSuspendedAnnotation] = strconv.FormatBool(cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend)
		cronJob.Spec.Suspend = boolPtr(true)
		return true
	}
	if !marked {
		return false
	}
	cronJob.Spec.Suspend = boolPtr(kept == "true")
	delete(cronJob.Annotations, lakeSuspendedAnnotation)
	return true
}

// jobEnded is whether a Job has completed or failed, whatever its backoff limit.
func jobEnded(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
