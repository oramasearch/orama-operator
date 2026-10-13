/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for the dashboard's scheduled calls (operator#17).

var wantDashboardCrons = map[string]struct{ path, schedule string }{
	"v1-dashboard-cron-crawler":          {"/api/private/v1/run-crawler-cron", "*/1 * * * *"},
	"v1-dashboard-cron-index-gc":         {"/api/private/v1/run-index-gc-cron", "0 * * * *"},
	"v1-dashboard-cron-consumption-sync": {"/api/billing/queue-consumption-sync", "0 */6 * * *"},
}

// dashboardWithCrons is a dashboard CR whose user env Secret carries CRON_SECRET, with crons as given.
func dashboardWithCrons(crons oramav1alpha1.DashboardCronsSpec) *oramav1alpha1.OramaCloudV1 {
	oc := dashboardWithUserEnv(userEnvFrom(), nil)
	oc.Spec.Components.Dashboard.Crons = crons
	return oc
}

// cronClient is a fake client holding the user env Secret plus objs, and a reconciler over it.
func cronClient(t *testing.T, objs ...client.Object) (client.Client, *OramaCloudV1Reconciler) {
	t.Helper()
	s := ocv1TestScheme(t)
	objs = append(objs, userEnvSecret(map[string]string{"CRON_SECRET": "not-a-real-secret"}))
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return cl, &OramaCloudV1Reconciler{Client: cl, Scheme: s}
}

func TestDashboardCronsEnabledCreatesThreeSuspendedCronJobs(t *testing.T) {
	cl, r := cronClient(t)
	if err := r.reconcileDashboard(context.Background(), dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true})); err != nil {
		t.Fatal(err)
	}
	for name, want := range wantDashboardCrons {
		cj := getObj(t, cl, name, &batchv1.CronJob{})
		if cj.Spec.Schedule != want.schedule {
			t.Errorf("%s schedule = %q, want %q", name, cj.Spec.Schedule, want.schedule)
		}
		if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
			t.Errorf("%s suspend = %v, want true", name, cj.Spec.Suspend)
		}
		if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
			t.Errorf("%s concurrencyPolicy = %q", name, cj.Spec.ConcurrencyPolicy)
		}
		c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
		script := strings.Join(c.Command, " ")
		if url := "http://v1-dashboard:3000" + want.path; !strings.Contains(script, url) {
			t.Errorf("%s command %q does not call %s", name, script, url)
		}
		if !strings.Contains(script, "--fail") || !strings.Contains(script, "--header @-") {
			t.Errorf("%s command %q must fail on non-2xx and read the header from stdin", name, script)
		}
		if strings.Contains(script, "not-a-real-secret") {
			t.Errorf("%s command carries the secret value", name)
		}
		if len(c.Env) != 1 || c.Env[0].Name != "CRON_SECRET" || c.Env[0].Value != "" || c.Env[0].ValueFrom == nil ||
			c.Env[0].ValueFrom.SecretKeyRef == nil || c.Env[0].ValueFrom.SecretKeyRef.Name != userDashboardEnv ||
			c.Env[0].ValueFrom.SecretKeyRef.Key != "CRON_SECRET" {
			t.Errorf("%s env = %+v, want CRON_SECRET from secretKeyRef %s/CRON_SECRET", name, c.Env, userDashboardEnv)
		}
		if !metav1.IsControlledBy(cj, dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{})) {
			t.Errorf("%s is not controlled by the CR", name)
		}
	}
}

func TestDashboardCronsUnsuspendAndDisable(t *testing.T) {
	foreign := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: "orama-system",
		Labels: labelsFor(newOCV1(nil), dashboardCronsComponent)}}
	cl, r := cronClient(t, foreign)
	ctx := context.Background()

	if err := r.reconcileDashboard(ctx, dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true, Suspend: boolPtr(false)})); err != nil {
		t.Fatal(err)
	}
	for name := range wantDashboardCrons {
		if cj := getObj(t, cl, name, &batchv1.CronJob{}); cj.Spec.Suspend == nil || *cj.Spec.Suspend {
			t.Errorf("%s suspend = %v, want false", name, cj.Spec.Suspend)
		}
	}

	if err := r.reconcileDashboard(ctx, dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{})); err != nil {
		t.Fatal(err)
	}
	for name := range wantDashboardCrons {
		if !absent(t, cl, name, &batchv1.CronJob{}) {
			t.Errorf("%s survived disabling the crons", name)
		}
	}
	if absent(t, cl, foreign.Name, &batchv1.CronJob{}) {
		t.Error("disabling the crons deleted a CronJob the CR does not own")
	}
}

func TestDashboardCronsScheduleOverrideReplacesOnlyThatEntry(t *testing.T) {
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Crons.Schedules.IndexGC = "30 * * * *"
	})
	got := map[string]string{}
	for _, c := range dashboardCrons(oc) {
		got[c.name] = c.schedule
	}
	want := map[string]string{"crawler": "*/1 * * * *", "index-gc": "30 * * * *", "consumption-sync": "0 */6 * * *"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s schedule = %q, want %q", k, got[k], v)
		}
	}
}

func TestDashboardCronsSecretSource(t *testing.T) {
	override := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "crons"}, Key: "token"}
	oc := dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true, SecretRef: override})
	if ref, err := dashboardCronSecret(oc, nil); err != nil || ref.Name != "crons" || ref.Key != "token" {
		t.Errorf("secretRef override = %+v, %v", ref, err)
	}

	// Without a source for CRON_SECRET the reconcile fails instead of creating CronJobs that 401.
	noSecret := dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true})
	noSecret.Spec.Components.Dashboard.EnvFrom = nil
	if _, err := dashboardCronSecret(noSecret, nil); err == nil {
		t.Error("crons without a CRON_SECRET source were accepted")
	}

	// A literal CRON_SECRET in dashboard.env cannot be referenced by a CronJob.
	literal := dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true})
	literal.Spec.Components.Dashboard.Env = []corev1.EnvVar{{Name: "CRON_SECRET", Value: "x"}}
	if _, err := dashboardCronSecret(literal, nil); err == nil {
		t.Error("a literal CRON_SECRET was accepted")
	}
}

func TestDashboardCronsRejectBadScheduleAndDisabledDashboard(t *testing.T) {
	_, r := cronClient(t)
	bad := dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true,
		Schedules: oramav1alpha1.DashboardCronSchedules{Crawler: "every minute"}})
	if err := r.reconcileDashboard(context.Background(), bad); err == nil {
		t.Error("an invalid schedule was accepted")
	}
	off := dashboardWithCrons(oramav1alpha1.DashboardCronsSpec{Enabled: true})
	off.Spec.Components.Dashboard.Enabled = false
	if err := r.reconcileDashboard(context.Background(), off); err == nil {
		t.Error("crons with the dashboard disabled were accepted")
	}
}
