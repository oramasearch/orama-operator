/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The Zetta grace counts from status.suspendedSince, and a Lake that is not suspended has none.
func TestLakeSuspensionZettaGrace(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	lake := &oramav1alpha1.Lake{}
	if lakeSuspendedSince(lake, now) != nil || lakeZettasSuspended(lake, now) {
		t.Fatal("a Lake that is not suspended has no suspension")
	}
	lake.Spec.Suspended = true
	since := lakeSuspendedSince(lake, now)
	if since == nil || !since.Time.Equal(now) {
		t.Fatalf("suspendedSince = %v, want %v", since, now)
	}
	lake.Status.SuspendedSince = since
	if kept := lakeSuspendedSince(lake, now.Add(time.Hour)); !kept.Time.Equal(now) {
		t.Fatalf("suspendedSince moved to %v; it must stay the first time seen", kept)
	}
	if lakeZettasSuspended(lake, now.Add(lakeSuspensionZettaGrace-time.Second)) {
		t.Fatal("the Zettas stopped before their grace ended")
	}
	if wait := lakeSuspensionRequeue(lake, now); wait != lakeSuspensionZettaGrace {
		t.Fatalf("requeue %v, want the whole grace", wait)
	}
	if !lakeZettasSuspended(lake, now.Add(lakeSuspensionZettaGrace)) {
		t.Fatal("the Zettas still run after their grace")
	}
	lake.Spec.Suspended = false
	if lakeSuspendedSince(lake, now) != nil || lakeZettasSuspended(lake, now) {
		t.Fatal("a renewed Lake keeps its suspension")
	}
}

// A suspension scales Deployments to zero and suspends unfinished Jobs and CronJobs, keeping what
// they had; the end of the suspension gives exactly that back.
func TestLakeSuspensionRoundTripsEachKind(t *testing.T) {
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(3)}}
	if !suspendDeployment(deployment, true) || *deployment.Spec.Replicas != 0 || deployment.Annotations[lakeSuspendedReplicasAnnotation] != "3" {
		t.Fatalf("suspended Deployment: %+v", deployment)
	}
	if suspendDeployment(deployment, true) {
		t.Fatal("a second suspension pass changed the Deployment again")
	}
	if !suspendDeployment(deployment, false) || *deployment.Spec.Replicas != 3 || deployment.Annotations[lakeSuspendedReplicasAnnotation] != "" {
		t.Fatalf("restored Deployment: %+v", deployment)
	}
	if suspendDeployment(deployment, false) {
		t.Fatal("a Deployment that was never suspended was changed")
	}

	job := &batchv1.Job{}
	if !suspendJob(job, true) || !*job.Spec.Suspend || job.Annotations[lakeSuspendedAnnotation] != "false" {
		t.Fatalf("suspended Job: %+v", job)
	}
	if !suspendJob(job, false) || *job.Spec.Suspend || job.Annotations[lakeSuspendedAnnotation] != "" {
		t.Fatalf("restored Job: %+v", job)
	}
	done := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	if suspendJob(done, true) {
		t.Fatal("a finished Job was suspended")
	}

	cronJob := &batchv1.CronJob{Spec: batchv1.CronJobSpec{Suspend: boolPtr(true)}}
	if !suspendCronJob(cronJob, true) || cronJob.Annotations[lakeSuspendedAnnotation] != "true" {
		t.Fatalf("suspended CronJob: %+v", cronJob)
	}
	if !suspendCronJob(cronJob, false) || !*cronJob.Spec.Suspend {
		t.Fatal("a CronJob its owner had suspended was resumed by the end of the suspension")
	}
}

// reconcileLakeSuspension stops only what the Lake controls, never the lake Deployment itself
// (spec.suspended sets its replicas), stops the Zettas only after the grace, deletes nothing, and
// gives everything back.
func TestReconcileLakeSuspensionStopsAndRestoresWhatTheLakeControls(t *testing.T) {
	ctx := context.Background()
	s := newLakeScheme(t)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	since := metav1.NewTime(now.Add(-time.Hour))
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: types.UID("lake-uid")},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-acme", Image: "oramasearch/lake:v1", Suspended: true},
		Status:     oramav1alpha1.LakeStatus{SuspendedSince: &since},
	}
	owned := func(obj client.Object) client.Object {
		if err := controllerutil.SetControllerReference(lake, obj, s); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	agent := owned(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ws-1"}, Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(2)}})
	lakeItself := owned(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lake-acme", Namespace: "lake-acme"}, Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(1)}})
	other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ws-1"}, Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(1)}}
	build := owned(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "builds"}})
	schedule := owned(&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ws-1"}})
	zetta := owned(&oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "ws-zetta", Namespace: "ws-1"}, Spec: oramav1alpha1.OramaZettaClusterSpec{Replicas: 1}})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, agent, lakeItself, other, build, schedule, zetta).Build()
	r := &LakeReconciler{Client: c, Scheme: s}

	get := func(obj client.Object, namespace, name string) {
		t.Helper()
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			t.Fatal(err)
		}
	}
	check := func(wantStopped, wantZettaStopped bool) {
		t.Helper()
		var d appsv1.Deployment
		get(&d, "ws-1", "agent")
		if stopped := *d.Spec.Replicas == 0; stopped != wantStopped || (!wantStopped && *d.Spec.Replicas != 2) {
			t.Fatalf("agent replicas %d, stopped want %t", *d.Spec.Replicas, wantStopped)
		}
		get(&d, "lake-acme", "lake-acme")
		if *d.Spec.Replicas != 1 {
			t.Fatal("the suspension pass changed the lake Deployment; spec.suspended sets its replicas")
		}
		get(&d, "ws-1", "other")
		if *d.Spec.Replicas != 1 {
			t.Fatal("a Deployment the Lake does not control was scaled")
		}
		var job batchv1.Job
		get(&job, "builds", "build")
		if suspended := job.Spec.Suspend != nil && *job.Spec.Suspend; suspended != wantStopped {
			t.Fatalf("build suspended %t, want %t", suspended, wantStopped)
		}
		var cron batchv1.CronJob
		get(&cron, "ws-1", "nightly")
		if suspended := cron.Spec.Suspend != nil && *cron.Spec.Suspend; suspended != wantStopped {
			t.Fatalf("schedule suspended %t, want %t", suspended, wantStopped)
		}
		var z oramav1alpha1.OramaZettaCluster
		get(&z, "ws-1", "ws-zetta")
		if z.Spec.Suspended != wantZettaStopped {
			t.Fatalf("workspace Zetta suspended %t, want %t", z.Spec.Suspended, wantZettaStopped)
		}
	}

	if err := r.reconcileLakeSuspension(ctx, lake, now); err != nil {
		t.Fatal(err)
	}
	check(true, false)
	if err := r.reconcileLakeSuspension(ctx, lake, now.Add(lakeSuspensionZettaGrace)); err != nil {
		t.Fatal(err)
	}
	check(true, true)

	lake.Spec.Suspended = false
	lake.Status.SuspendedSince = nil
	if err := r.reconcileLakeSuspension(ctx, lake, now.Add(lakeSuspensionZettaGrace)); err != nil {
		t.Fatal(err)
	}
	check(false, false)
}

// A suspended OramaZettaCluster's StatefulSet runs no node, and its status says so.
func TestSuspendedZettaClusterScalesItsStatefulSetToZero(t *testing.T) {
	s := newLakeScheme(t)
	z := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", UID: types.UID("z-uid")},
		Spec:       oramav1alpha1.OramaZettaClusterSpec{Replicas: 3, Suspended: true},
	}
	r := &OramaZettaClusterReconciler{Scheme: s}
	sts := &appsv1.StatefulSet{}
	if err := r.mutateZettaStatefulSet(z, sts, "z-headless", "z", map[string]string{"app": "z"}, "", nil); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Fatalf("a suspended cluster runs %v nodes, want 0", sts.Spec.Replicas)
	}
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		t.Fatal("a suspended cluster lost its volume claim templates; its data must be kept")
	}
}
