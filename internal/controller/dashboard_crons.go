/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The dashboard's scheduled calls (spec.components.dashboard.crons, operator#17). On Prod 1 Vercel
// runs them from vercel.json; here each is a CronJob that GETs the in-cluster dashboard Service (never
// the public hostname, so a run cannot reach Prod 1 while DNS moves) with the dashboard's CRON_SECRET.

// dashboardCronsComponent labels the CronJobs, apart from the dashboard's own children.
const dashboardCronsComponent = "dashboard-crons"

// dashboardCron is one scheduled call: the CronJob suffix, the route and its schedule.
type dashboardCron struct{ name, path, schedule string }

// dashboardCrons returns the three calls with any per-entry schedule override applied.
func dashboardCrons(oc *oramav1alpha1.OramaCloudV1) []dashboardCron {
	s := oc.Spec.Components.Dashboard.Crons.Schedules
	return []dashboardCron{
		{"crawler", "/api/private/v1/run-crawler-cron", cmp.Or(s.Crawler, "*/1 * * * *")},
		{"index-gc", "/api/private/v1/run-index-gc-cron", cmp.Or(s.IndexGC, "0 * * * *")},
		{"consumption-sync", "/api/billing/queue-consumption-sync", cmp.Or(s.ConsumptionSync, "0 */6 * * *")},
	}
}

// dashboardCronSecret is the Secret key the CronJobs read CRON_SECRET from: crons.secretRef, else the
// Secret key the dashboard itself reads CRON_SECRET from (operator#311's envFrom/env). sources are the
// dashboard.envFrom sources reconcileDashboard already read.
func dashboardCronSecret(oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) (*corev1.SecretKeySelector, error) {
	if ref := oc.Spec.Components.Dashboard.Crons.SecretRef; ref != nil {
		if ref.Name == "" || ref.Key == "" {
			return nil, fmt.Errorf("dashboard.crons.secretRef needs a name and a key")
		}
		return ref, nil
	}
	operatorEnv, err := dashboardOperatorEnv(oc)
	if err != nil {
		return nil, err
	}
	check, isSecret, found := dashboardCredentialSource(oc, operatorEnv, sources, "CRON_SECRET")
	switch {
	case !found:
		return nil, fmt.Errorf("dashboard.crons needs CRON_SECRET: provide it to the dashboard through a dashboard.envFrom Secret or a dashboard.env secretKeyRef, or set dashboard.crons.secretRef")
	case !isSecret:
		return nil, fmt.Errorf("dashboard.crons: the dashboard's CRON_SECRET is not a Secret key; move it into a Secret or set dashboard.crons.secretRef")
	}
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: check.secret}, Key: check.key}, nil
}

// dashboardCronScript sends CRON_SECRET as a header read from stdin, so the value never appears in
// the process arguments, and lets curl --fail turn a non-2xx response into a failed Job.
func dashboardCronScript(url string) string {
	return `[ -n "${CRON_SECRET:-}" ] || { echo "CRON_SECRET is empty" >&2; exit 1; }
printf 'Authorization: Bearer %s\n' "$CRON_SECRET" | curl --header @- --fail --silent --show-error --connect-timeout 10 --max-time 300 --output /dev/null ` + url + "\n"
}

// reconcileDashboardCrons creates, updates or removes the dashboard's CronJobs. It runs after the
// dashboard Deployment so the Service it calls exists.
func (r *OramaCloudV1Reconciler) reconcileDashboardCrons(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) error {
	c := oc.Spec.Components.Dashboard.Crons
	if !c.Enabled {
		return r.teardownDashboardCrons(ctx, oc)
	}
	crons := dashboardCrons(oc)
	for _, dc := range crons {
		if len(strings.Fields(dc.schedule)) != 5 {
			return fmt.Errorf("dashboard.crons %s schedule %q must have five cron fields", dc.name, dc.schedule)
		}
		if _, err := cron.ParseStandard(dc.schedule); err != nil {
			return fmt.Errorf("dashboard.crons %s schedule: %w", dc.name, err)
		}
	}
	secret, err := dashboardCronSecret(oc, sources)
	if err != nil {
		return err
	}
	suspend := c.Suspend == nil || *c.Suspend
	for _, dc := range crons {
		if err := r.reconcileDashboardCron(ctx, oc, dc, secret, suspend); err != nil {
			return fmt.Errorf("dashboard cron %s: %w", dc.name, err)
		}
	}
	return nil
}

// reconcileDashboardCron writes one CronJob, following the analytics import's shape (gaps.go).
func (r *OramaCloudV1Reconciler) reconcileDashboardCron(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, dc dashboardCron, secret *corev1.SecretKeySelector, suspend bool) error {
	job := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-dashboard-cron-" + dc.name, Namespace: oc.Namespace}}
	url := "http://" + oc.Name + "-dashboard:3000" + dc.path
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		job.Labels = labelsFor(oc, dashboardCronsComponent)
		nodes, tolerations := podPlacement(oc)
		// Set only our intent so API-defaulted fields survive and a steady schedule makes no writes.
		job.Spec.Schedule = dc.schedule
		job.Spec.TimeZone = strPtr("Etc/UTC")
		job.Spec.Suspend = boolPtr(suspend)
		job.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		job.Spec.StartingDeadlineSeconds = int64Ptr(60)
		job.Spec.SuccessfulJobsHistoryLimit, job.Spec.FailedJobsHistoryLimit = int32Ptr(1), int32Ptr(3)
		jobSpec := &job.Spec.JobTemplate.Spec
		jobSpec.BackoffLimit, jobSpec.ActiveDeadlineSeconds, jobSpec.TTLSecondsAfterFinished = int32Ptr(0), int64Ptr(330), int32Ptr(3600)
		jobSpec.Template.Labels = labelsFor(oc, dashboardCronsComponent)
		pod := &jobSpec.Template.Spec
		pod.RestartPolicy, pod.AutomountServiceAccountToken = corev1.RestartPolicyNever, boolPtr(false)
		pod.NodeSelector, pod.Tolerations, pod.ImagePullSecrets = nodes, tolerations, imagePullSecrets(oc)
		pod.Containers = []corev1.Container{{
			Name: "call", Image: "curlimages/curl:8.10.1", ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{"/bin/sh", "-c", dashboardCronScript(url)},
			Env: []corev1.EnvVar{{Name: "CRON_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: secret.LocalObjectReference, Key: secret.Key, Optional: secret.Optional,
			}}}},
			TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			SecurityContext: hardenedSecurityContext(), Resources: *resourceReqs("32Mi", "10m", "64Mi"),
		}}
		return controllerutil.SetControllerReference(oc, job, r.Scheme)
	})
	return err
}

// teardownDashboardCrons deletes the CronJobs this CR owns and leaves any other CronJob alone, even
// one carrying the same labels.
func (r *OramaCloudV1Reconciler) teardownDashboardCrons(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	var list batchv1.CronJobList
	if err := r.List(ctx, &list, client.InNamespace(oc.Namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"app.kubernetes.io/instance":   oc.Name,
		"app.kubernetes.io/component":  dashboardCronsComponent,
	}); err != nil {
		return fmt.Errorf("teardown dashboard crons: list: %w", err)
	}
	for i := range list.Items {
		if !metav1.IsControlledBy(&list.Items[i], oc) {
			continue
		}
		if err := r.deleteCloudV1Owned(ctx, oc, &list.Items[i]); err != nil {
			return fmt.Errorf("teardown dashboard cron %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}
