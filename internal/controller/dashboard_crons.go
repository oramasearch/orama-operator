/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The dashboard's scheduled calls and the e2e canary (spec.components.dashboard.crons, operator#17).
//
// Prod 1 runs the dashboard's three crons on Vercel (vercel.json) and the e2e canary and its prune as
// systemd timers on prod-oramacore-crons-us-east-2. Here each is an owned CronJob in the stack. The
// dashboard crons GET the in-cluster dashboard Service, never the public hostname, so a run cannot
// reach Prod 1 while DNS moves. Everything is created suspended: the two stacks must never crawl,
// bill or probe at once, and the cutover unsuspends them through the set file.
//
// Configuration errors (no Secret source for CRON_SECRET, a missing Secret or key, a same-name object
// this CR does not control) fail the dashboard step, which reports Ready=False with the reason
// ReconcileError. Run outcomes show in Job status and logs, not in the stack's readiness.

const (
	dashboardCronsComponent = "dashboard-crons"
	e2eCanaryComponent      = "e2e-canary"
	// dashboardCronsImage is the curl image when spec.images.overrides["dashboard-crons"] is unset: a
	// full reference, not resolved against the stack's registry and tag. It runs as curl_user, which
	// is uid 100, gid 101 in this image; the numbers are set explicitly so runAsNonRoot can check them.
	dashboardCronsImage = "curlimages/curl:8.10.1"
	curlUID, curlGID    = 100, 101
	// The canary image runs as root by default; the pod runs it as this conventional non-root user.
	canaryUID = 65532
)

// dashboardCron is one of the dashboard's scheduled calls: the CronJob suffix, the route and Prod 1's
// schedule (dashboard vercel.json). The schedules are fixed for parity with Prod 1.
type dashboardCron struct{ name, path, schedule string }

var dashboardCrons = []dashboardCron{
	{"crawler", "/api/private/v1/run-crawler-cron", "*/1 * * * *"},
	{"index-gc", "/api/private/v1/run-index-gc-cron", "0 * * * *"},
	{"consumption-sync", "/api/billing/queue-consumption-sync", "0 */6 * * *"},
}

// e2eCanaryScript runs the canary, then the prune once a day; see the script's header.
//
//go:embed assets/e2e-canary.sh
var e2eCanaryScript string

// errCronsNeedDashboard is the reconcile's copy of the CRD rule, for objects that bypass admission.
var errCronsNeedDashboard = errors.New("dashboard.crons requires the dashboard enabled; set dashboard.crons.enabled to false as well")

// dashboardCronSecret returns the Secret key the dashboard's own CRON_SECRET comes from, so the
// CronJobs send the bearer the dashboard checks (dashboardEnvSecretKey).
func dashboardCronSecret(oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) (*corev1.SecretKeySelector, error) {
	return dashboardEnvSecretKey(oc, sources, "CRON_SECRET", "dashboard.crons", "the CronJobs")
}

// dashboardEnvSecretKey returns the Secret key the dashboard's own variable name comes from, so a
// workload that calls the dashboard (reader, e.g. "the CronJobs") sends the value the dashboard
// checks, with no second reference to keep in step. dashboard.env wins when it names the variable
// and must then be a secretKeyRef; otherwise the last dashboard.envFrom source holding it after its
// prefix wins and must be a Secret. Any other winner (a literal, a ConfigMap, a fieldRef) is an
// error even if an earlier Secret also holds the key. field names the setting in errors. The
// operator's own sources carry none of these variables. sources are dashboardEnvSources' result; an
// absent optional source has nil Data.
func dashboardEnvSecretKey(oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource, name, field, reader string) (*corev1.SecretKeySelector, error) {
	for _, e := range overrideEnv(nil, oc.Spec.Components.Dashboard.Env) {
		if e.Name != name {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			return nil, fmt.Errorf("%s: dashboard.env sets %s, but not from a Secret key; %s read it from the same Secret as the dashboard, so use a secretKeyRef", field, name, reader)
		}
		ref := e.ValueFrom.SecretKeyRef
		return &corev1.SecretKeySelector{LocalObjectReference: ref.LocalObjectReference, Key: ref.Key}, nil
	}
	for i := len(sources) - 1; i >= 0; i-- {
		src := sources[i]
		key, ok := strings.CutPrefix(name, src.Prefix)
		if !ok {
			continue
		}
		if _, has := src.Data[key]; !has {
			continue
		}
		if src.Kind != "Secret" {
			return nil, fmt.Errorf("%s: the dashboard takes %s from %s %s (dashboard.envFrom[%d]); it must come from a Secret", field, name, src.Kind, src.Name, i)
		}
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: src.Name}, Key: key}, nil
	}
	return nil, fmt.Errorf("%s needs %s: give the dashboard a dashboard.envFrom Secret holding it, or a dashboard.env secretKeyRef", field, name)
}

// requireSecretKeys fails unless the Secret exists in oc's namespace and each key is present and
// non-empty. field names the setting in the error; no value is ever included.
func (r *OramaCloudV1Reconciler) requireSecretKeys(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, field, name string, keys ...string) error {
	data, found, err := r.readEnvObject(ctx, oc.Namespace, "Secret", name)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if !found {
		return fmt.Errorf("%s: Secret %s/%s does not exist", field, oc.Namespace, name)
	}
	for _, k := range keys {
		if data[k] == "" {
			return fmt.Errorf("%s: Secret %s/%s has no non-empty key %s", field, oc.Namespace, name, k)
		}
	}
	return nil
}

// dashboardCronScript calls url with the bearer from CRON_SECRET. The header goes to curl on stdin
// (printf is a shell builtin in busybox ash and dash), so the value is in no process argument, and
// tracing is off. Only the status is logged. curl --fail fails on 4xx/5xx, connection errors and the
// timeout; a 3xx is refused by the status check and never followed. url is a DNS name and a fixed
// path, so single quotes are enough.
func dashboardCronScript(url string) string {
	return `set +x
[ -n "${CRON_SECRET:-}" ] || { echo 'CRON_SECRET is empty' >&2; exit 1; }
code="$(printf 'Authorization: Bearer %s\n' "$CRON_SECRET" |
  curl --header @- --fail --silent --show-error \
    --connect-timeout 10 --max-time 300 --output /dev/null \
    --write-out '%{http_code}' \
    '` + url + `')"
result=$?
printf 'HTTP %s\n' "$code"
[ "$result" -eq 0 ] || exit "$result"
case "$code" in 2[0-9][0-9]) exit 0 ;; *) exit 1 ;; esac
`
}

// e2eCanaryActive reports whether oc asks for the canary CronJob.
func e2eCanaryActive(oc *oramav1alpha1.OramaCloudV1) bool {
	d := oc.Spec.Components.Dashboard
	return d.Enabled && d.Crons.Enabled && d.Crons.E2ECanary != nil
}

// suspended reads a suspend field the CRD defaults to true; nil (an object built in Go, or one that
// bypassed defaulting) is suspended too.
func suspended(s *bool) bool { return s == nil || *s }

// reconcileDashboardCrons writes the three dashboard CronJobs and then the canary, after the
// dashboard's Deployment and Service. The crons' own inputs are checked before the first CronJob is
// written; the canary's are checked in reconcileE2ECanary, so a missing canary Secret never keeps
// the dashboard crons from being written.
func (r *OramaCloudV1Reconciler) reconcileDashboardCrons(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) error {
	c := oc.Spec.Components.Dashboard.Crons
	secret, err := dashboardCronSecret(oc, sources)
	if err != nil {
		return err
	}
	if err := r.requireSecretKeys(ctx, oc, "dashboard.crons CRON_SECRET", secret.Name, secret.Key); err != nil {
		return err
	}
	image := dashboardCronsImage
	if o := oc.Spec.Images.Overrides[dashboardCronsComponent]; o != "" {
		image = o
	}
	for _, dc := range dashboardCrons {
		url := "http://" + oc.Name + "-dashboard:3000" + dc.path
		err := r.upsertCronJob(ctx, oc, oc.Name+"-dashboard-cron-"+dc.name, dashboardCronsComponent, dc.schedule, suspended(c.Suspend), 330, func(pod *corev1.PodSpec) {
			ctr := onlyContainer(pod, "call")
			ctr.Image = image
			ctr.ImagePullPolicy = corev1.PullIfNotPresent
			ctr.Command = []string{"/bin/sh", "-c", dashboardCronScript(url)}
			ctr.Env = []corev1.EnvVar{{Name: "CRON_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: secret}}}
			ctr.Resources = *resourceReqs("32Mi", "10m", "64Mi")
			ctr.SecurityContext = nonRootSecurityContext(curlUID, curlGID)
		})
		if err != nil {
			return fmt.Errorf("dashboard cron %s: %w", dc.name, err)
		}
	}

	if !e2eCanaryActive(oc) {
		return r.deleteControlled(ctx, oc, e2eCanaryComponent)
	}
	return r.reconcileE2ECanary(ctx, oc)
}

// reconcileE2ECanary checks the canary's Secret, then writes its state volume and its CronJob, which
// follows crons.suspend like the dashboard crons. One CronJob runs the canary
// and then, once a day, the prune, so the two never run at once against test-state.json: the prune
// keeps the titles the canary tracks there, and a prune racing a canary run would delete the probe
// the canary has written but not yet recorded. ForbidConcurrent keeps runs from overlapping.
func (r *OramaCloudV1Reconciler) reconcileE2ECanary(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	crons := oc.Spec.Components.Dashboard.Crons
	spec := crons.E2ECanary
	if err := r.requireSecretKeys(ctx, oc, "dashboard.crons.e2eCanary.secretRef", spec.SecretRef.Name, "collectionID", "indexID", "apiKey"); err != nil {
		return err
	}
	pvcName := oc.Name + "-e2e-canary-state"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if err := refuseForeign(oc, "PersistentVolumeClaim", pvc); err != nil {
			return err
		}
		pvc.Labels = labelsFor(oc, e2eCanaryComponent)
		if pvc.ResourceVersion == "" { // the claim's spec is fixed once created
			pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
		}
		return controllerutil.SetControllerReference(oc, pvc, r.Scheme)
	}); err != nil {
		return fmt.Errorf("e2e canary state: %w", err)
	}

	key := func(env, k string) corev1.EnvVar {
		return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: spec.SecretRef, Key: k}}}
	}
	// The prune's run can take ten minutes (Prod 1's TimeoutSec=600) on top of the canary's.
	err := r.upsertCronJob(ctx, oc, oc.Name+"-e2e-canary", e2eCanaryComponent, "*/5 * * * *", suspended(crons.Suspend), 900, func(pod *corev1.PodSpec) {
		pod.SecurityContext = &corev1.PodSecurityContext{
			FSGroup:             int64Ptr(canaryUID),
			FSGroupChangePolicy: ptrTo(corev1.FSGroupChangeOnRootMismatch),
		}
		pod.Volumes = []corev1.Volume{
			{Name: "state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}}},
			// The rendered config holds the private API key: memory only, never the state volume.
			{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptrTo(resource.MustParse("1Mi"))}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptrTo(resource.MustParse("64Mi"))}}},
		}
		ctr := onlyContainer(pod, "canary")
		ctr.Image = spec.Image
		ctr.ImagePullPolicy = corev1.PullIfNotPresent
		ctr.Command = []string{"/bin/sh", "-c", e2eCanaryScript}
		ctr.Env = []corev1.EnvVar{
			key("E2E_CANARY_COLLECTION_ID", "collectionID"),
			key("E2E_CANARY_INDEX_ID", "indexID"),
			key("E2E_CANARY_API_KEY", "apiKey"),
			{Name: "E2E_CANARY_JWT_URL", Value: "http://" + oc.Name + "-dashboard:3000/api/user/jwt"},
			{Name: "E2E_CANARY_READER_URL", Value: "http://" + oc.Name + "-reader"},
			{Name: "E2E_CANARY_STATE_DIR", Value: "/state"},
			{Name: "E2E_CANARY_RUN_DIR", Value: "/run/canary"},
			{Name: "TMPDIR", Value: "/tmp"},
			{Name: "RUST_LOG", Value: "monitor-e2e=info,warn"}, // Prod 1's oramacore.test-e2e.service
		}
		ctr.VolumeMounts = []corev1.VolumeMount{
			{Name: "state", MountPath: "/state"}, {Name: "run", MountPath: "/run/canary"}, {Name: "tmp", MountPath: "/tmp"},
		}
		ctr.Resources = *resourceReqs("64Mi", "10m", "256Mi")
		ctr.SecurityContext = nonRootSecurityContext(canaryUID, canaryUID)
	})
	if err != nil {
		return fmt.Errorf("e2e canary: %w", err)
	}
	return nil
}

// upsertCronJob writes the operator's intent onto the named CronJob: the shared run policy (UTC,
// ForbidConcurrent, a 60-second starting deadline so a long suspension leaves no backlog, one
// successful and three failed Jobs kept, no retry, an hour's TTL) plus whatever pod sets. Fields are
// assigned one by one, so the API's defaults survive and a steady spec writes nothing. A same-name
// CronJob this CR does not control is a conflict and is left unchanged.
func (r *OramaCloudV1Reconciler) upsertCronJob(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name, component, schedule string, suspend bool, deadline int64, pod func(*corev1.PodSpec)) error {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		if err := refuseForeign(oc, "CronJob", cj); err != nil {
			return err
		}
		lbls := labelsFor(oc, component)
		cj.Labels = lbls
		cj.Spec.Schedule = schedule
		cj.Spec.TimeZone = strPtr("Etc/UTC")
		cj.Spec.Suspend = boolPtr(suspend)
		cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cj.Spec.StartingDeadlineSeconds = int64Ptr(60)
		cj.Spec.SuccessfulJobsHistoryLimit, cj.Spec.FailedJobsHistoryLimit = int32Ptr(1), int32Ptr(3)
		job := &cj.Spec.JobTemplate.Spec
		job.BackoffLimit, job.ActiveDeadlineSeconds, job.TTLSecondsAfterFinished = int32Ptr(0), int64Ptr(deadline), int32Ptr(3600)
		job.Template.Labels = lbls
		spec := &job.Template.Spec
		spec.RestartPolicy = corev1.RestartPolicyNever
		spec.AutomountServiceAccountToken = boolPtr(false)
		spec.ImagePullSecrets = imagePullSecrets(oc)
		pod(spec)
		boundV1PodStorage(spec)
		return controllerutil.SetControllerReference(oc, cj, r.Scheme)
	})
	return err
}

// refuseForeign fails when obj already exists and oc does not control it, so a same-name object is
// never adopted or modified.
func refuseForeign(oc *oramav1alpha1.OramaCloudV1, kind string, obj client.Object) error {
	if obj.GetResourceVersion() != "" && !metav1.IsControlledBy(obj, oc) {
		return fmt.Errorf("%s %s/%s exists and is not controlled by OramaCloudV1 %s; it is left unchanged", kind, obj.GetNamespace(), obj.GetName(), oc.Name)
	}
	return nil
}

// onlyContainer returns the pod's single container named name, replacing any other set, and keeps
// the API-defaulted fields of an existing one.
func onlyContainer(pod *corev1.PodSpec, name string) *corev1.Container {
	if len(pod.Containers) != 1 || pod.Containers[0].Name != name {
		pod.Containers = []corev1.Container{{Name: name}}
	}
	return &pod.Containers[0]
}

// nonRootSecurityContext is hardenedSecurityContext plus a numeric non-root user and a read-only
// root filesystem.
func nonRootSecurityContext(uid, gid int64) *corev1.SecurityContext {
	sc := hardenedSecurityContext()
	sc.RunAsNonRoot = boolPtr(true)
	sc.RunAsUser, sc.RunAsGroup = int64Ptr(uid), int64Ptr(gid)
	sc.ReadOnlyRootFilesystem = boolPtr(true)
	return sc
}

func ptrTo[T any](v T) *T { return &v }

// teardownDashboardCrons removes the dashboard CronJobs and the canary (its CronJob and state).
func (r *OramaCloudV1Reconciler) teardownDashboardCrons(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if err := r.deleteControlled(ctx, oc, dashboardCronsComponent); err != nil {
		return err
	}
	return r.deleteControlled(ctx, oc, e2eCanaryComponent)
}

// deleteControlled deletes the CronJobs and PersistentVolumeClaims labelled for oc and component that
// oc controls, leaving every other one alone, even one with the same labels. Deleting a CronJob
// removes its Jobs in the background. Not-found is ignored.
func (r *OramaCloudV1Reconciler) deleteControlled(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string) error {
	sel := client.MatchingLabels{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"app.kubernetes.io/instance":   oc.Name,
		"app.kubernetes.io/component":  component,
	}
	for _, list := range []client.ObjectList{&batchv1.CronJobList{}, &corev1.PersistentVolumeClaimList{}} {
		if err := r.List(ctx, list, client.InNamespace(oc.Namespace), sel); err != nil {
			return fmt.Errorf("teardown %s: list: %w", component, err)
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			return err
		}
		for _, it := range items {
			obj := it.(client.Object)
			if !metav1.IsControlledBy(obj, oc) {
				continue
			}
			if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("teardown %s/%s: %w", component, obj.GetName(), err)
			}
		}
	}
	return nil
}

// requestsForE2ECanarySecret maps a Secret to every OramaCloudV1 in its namespace whose canary names
// it, so creating or fixing the Secret clears a failed reconcile without waiting for the backoff. The
// CronJob reads the keys when each run starts, so a changed value needs no rewrite.
func (r *OramaCloudV1Reconciler) requestsForE2ECanarySecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var list oramav1alpha1.OramaCloudV1List
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list OramaCloudV1 for an e2e canary Secret", "name", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if e2eCanaryActive(&list.Items[i]) && list.Items[i].Spec.Components.Dashboard.Crons.E2ECanary.SecretRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}
