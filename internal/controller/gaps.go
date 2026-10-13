/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Gap-fill components surfaced by the techops-docs gap analysis (operator issues #5–#7).
// Airbyte (#4) is a platform dependency installed via its Helm chart (helmfile), and the
// QStash-equivalent queue (#8) is config wiring (workers → the in-cluster RabbitMQ) — neither
// is a reconciled component here.

// analyticsProcessorConfigYAML renders the config the `server` binary reads (lib.rs). It
// connects to ClickHouse + Postgres at startup, so these are the real endpoints.
// Secret values are __PLACEHOLDER__s the render init container substitutes — never in the CM.
// sources become its `oramacores`, pulled in order on every POST /import.
func analyticsProcessorConfigYAML(oc *oramav1alpha1.OramaCloudV1, sources []oramav1alpha1.AnalyticsImportSource) string {
	chURL := "http://" + oc.Name + "-analytics:8123"
	chUser := "default"
	if analyticsExternal(oc) {
		chURL = oc.Spec.Components.Analytics.Endpoint
		chUser = "__CLICKHOUSE_USER__"
	}
	var oramacores strings.Builder
	for _, s := range sources {
		fmt.Fprintf(&oramacores, "  - name: %s\n    api_url: %s\n    api_key: __MASTER_API_KEY__\n    enabled: %t\n", s.Name, s.URL, s.Enabled)
	}
	return fmt.Sprintf(`clickhouse:
  url: %q
  user: %q
  password: __CLICKHOUSE_PASSWORD__
  database: default
  search_table: oramacore_analytics_search_raw
  interaction_table: oramacore_analytics_interaction_raw
  aggregated_search_table: oramacore_analytics_search_aggregated
create_tables: true
oramacores:
%spostgres:
  url: __PG_URI__
  schema: analytics
  runs_table: runs
  delta: 60m
`, chURL, chUser, oramacores.String())
}

// unscheduledAnalyticsSources is the processor's config without scheduled imports: the
// load-balanced reader Service and the writer, as before scheduled imports existed. Nothing in the
// cluster calls /import in this mode.
func unscheduledAnalyticsSources(oc *oramav1alpha1.OramaCloudV1) []oramav1alpha1.AnalyticsImportSource {
	return []oramav1alpha1.AnalyticsImportSource{
		{Name: "oramacore-reader", URL: "http://" + oc.Name + "-reader/v1/analytics", Enabled: true},
		{Name: "oramacore-writer", URL: "http://" + oc.Name + "-writer:8080/v1/analytics", Enabled: true},
	}
}

// analyticsImportSources lists every reader ordinal once, through the headless governing Service.
// Ordinals below live have a pod and are pulled. Ordinals from live up to claimed have only a data
// PVC left (a scale-down retains it) and are listed disabled: GET /v1/analytics erases what it
// returns, and those events wait on the PVC until the ordinal's pod returns and is pulled again.
// The processor aborts a run at the first source it cannot reach (analytics-processor
// importer.rs, import_analytics), so a source without a pod must be disabled, not left to fail.
// The writer follows only when import.includeWriter is set on a primary (the only region with one).
func analyticsImportSources(oc *oramav1alpha1.OramaCloudV1, live, claimed int32) []oramav1alpha1.AnalyticsImportSource {
	n := oc.Name
	var sources []oramav1alpha1.AnalyticsImportSource
	for ordinal := int32(0); ordinal < max(live, claimed); ordinal++ {
		pod := fmt.Sprintf("%s-reader-%d", n, ordinal)
		sources = append(sources, oramav1alpha1.AnalyticsImportSource{
			Name:    pod,
			URL:     fmt.Sprintf("http://%s.%s-reader-headless.%s.svc:8080/v1/analytics", pod, n, oc.Namespace),
			Enabled: ordinal < live,
		})
	}
	if oc.Spec.Components.AnalyticsProcessor.Import.IncludeWriter && oc.Spec.Region.Role == "primary" {
		sources = append(sources, oramav1alpha1.AnalyticsImportSource{
			Name: n + "-writer", URL: "http://" + n + "-writer:8080/v1/analytics", Enabled: true,
		})
	}
	return sources
}

// readerImportSources reads the reader StatefulSet and its data PVCs and returns the scheduled
// import's sources. Ordinals with a pod are those below the larger of the desired and the current
// replica count (a scale-down's terminating pods still count); ordinals with a PVC
// (data-<cr>-reader-<ordinal>) stay listed after their pod is gone.
func (r *OramaCloudV1Reconciler) readerImportSources(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]oramav1alpha1.AnalyticsImportSource, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-reader"}, sts); err != nil {
		return nil, err
	}
	if sts.Spec.ServiceName != oc.Name+"-reader-headless" {
		return nil, fmt.Errorf("analytics import requires reader StatefulSet serviceName %s-reader-headless; legacy readers need a separately reviewed migration before enabling imports", oc.Name)
	}
	live := oc.Spec.Components.Engine.Reader.Replicas
	if live == 0 {
		live = 3
	}
	if sts.Spec.Replicas != nil {
		if *sts.Spec.Replicas < 1 {
			return nil, fmt.Errorf("analytics import requires at least one reader replica")
		}
		live = *sts.Spec.Replicas
	}
	live = max(live, sts.Status.Replicas)
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(oc.Namespace)); err != nil {
		return nil, err
	}
	claimed, prefix := int32(0), "data-"+oc.Name+"-reader-"
	for _, c := range claims.Items {
		rest, ok := strings.CutPrefix(c.Name, prefix)
		if !ok {
			continue
		}
		if ordinal, err := strconv.ParseInt(rest, 10, 32); err == nil && ordinal >= 0 && rest == strconv.FormatInt(ordinal, 10) {
			claimed = max(claimed, int32(ordinal)+1)
		}
	}
	return analyticsImportSources(oc, live, claimed), nil
}

func analyticsExternal(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Components.Analytics.Mode == "external"
}

func validateExternalAnalytics(oc *oramav1alpha1.OramaCloudV1) error {
	a := oc.Spec.Components.Analytics
	u, err := url.Parse(a.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return fmt.Errorf("analytics.endpoint must be an HTTP(S) URL without credentials")
	}
	if a.CredentialsSecretRef == nil || a.CredentialsSecretRef.Name == "" {
		return fmt.Errorf("analytics.credentialsSecretRef is required for external ClickHouse (CLICKHOUSE_USER and CLICKHOUSE_PASSWORD)")
	}
	return nil
}

func secretKeyEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
	}}}
}

// reconcileAnalyticsProcessor deploys analytics-processor (#5): the worker that pulls
// /v1/analytics into ClickHouse. Its `server` binary reads a YAML config FILE (CONFIG_PATH) and
// connects to ClickHouse + Postgres at startup — so the operator renders that file via an init
// container (ClickHouse password from the component Secret; the master key from the master API key
// Secret; the control-DB URI from the CNPG or external credentials Secret) and mounts it. (The image is also
// patched at build to bind 0.0.0.0:8080 instead of its hardcoded 127.0.0.1:8888.)
func (r *OramaCloudV1Reconciler) reconcileAnalyticsProcessor(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.AnalyticsProcessor.Enabled {
		return r.teardownByComponent(ctx, oc, "analytics-import", "analytics-processor")
	}
	// Remove a previous schedule before reporting invalid prerequisites; it must stop running.
	if !oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
		if err := r.teardownByComponent(ctx, oc, "analytics-import"); err != nil {
			return err
		}
	}
	sources := unscheduledAnalyticsSources(oc)
	if oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
		var err error
		if err = analyticsImportPrerequisites(oc); err == nil {
			sources, err = r.readerImportSources(ctx, oc)
		}
		if err != nil {
			if cleanupErr := r.teardownByComponent(ctx, oc, "analytics-import"); cleanupErr != nil {
				return cleanupErr
			}
			return err
		}
	}
	if analyticsExternal(oc) {
		if err := validateExternalAnalytics(oc); err != nil {
			if cleanupErr := r.teardownByComponent(ctx, oc, "analytics-import"); cleanupErr != nil {
				return cleanupErr
			}
			return err
		}
	}
	config := analyticsProcessorConfigYAML(oc, sources)
	img := componentImage(oc, "analytics-processor", "analytics-processor")
	cmName := oc.Name + "-analytics-processor-config"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(oc, "analytics-processor")
		cm.Data = map[string]string{"config.yaml": config}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}
	render := corev1.Container{
		Name:  "render-config",
		Image: img,
		Command: []string{"sh", "-c",
			// esc makes a value safe as a sed replacement: a URI's query string carries `&`, which sed
			// would otherwise expand to the matched placeholder.
			`esc() { printf '%s' "$1" | sed -e 's/[\\&#]/\\&/g'; }; ` +
				`sed -e "s#__CLICKHOUSE_PASSWORD__#$(esc "${CLICKHOUSE_PASSWORD}")#g" -e "s#__MASTER_API_KEY__#$(esc "${MASTER_API_KEY}")#g" -e "s#__CLICKHOUSE_USER__#$(esc "${CLICKHOUSE_USER}")#g" -e "s#__PG_URI__#$(esc "${PG_URI}")#g" /tmpl/config.yaml > /etc/analytics-processor/config.yaml`},
		EnvFrom: envFromFor(oc, "analytics-processor"),
		// The control-DB URI from the CNPG or external Secret (same key in both modes).
		Env: []corev1.EnvVar{pgURIEnv(oc, "PG_URI"), masterAPIKeyEnv(oc)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config-tmpl", MountPath: "/tmpl"},
			{Name: "rendered-config", MountPath: "/etc/analytics-processor"},
		},
	}
	if analyticsExternal(oc) {
		ref := oc.Spec.Components.Analytics.CredentialsSecretRef.Name
		render.Env = append(render.Env, secretKeyEnv("CLICKHOUSE_USER", ref, "CLICKHOUSE_USER"), secretKeyEnv("CLICKHOUSE_PASSWORD", ref, "CLICKHOUSE_PASSWORD"))
	}
	if err := r.reconcileApp(ctx, oc, appDef{
		component:      "analytics-processor",
		configHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(config))),
		image:          img,
		port:           8080,
		env:            []corev1.EnvVar{{Name: "CONFIG_PATH", Value: "/etc/analytics-processor/config.yaml"}},
		resources:      resourcesOr(oc.Spec.Components.AnalyticsProcessor.Resources, resourceReqs("128Mi", "50m", "512Mi")),
		initContainers: []corev1.Container{render},
		volumes: []corev1.Volume{
			{Name: "config-tmpl", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
			{Name: "rendered-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		volumeMounts: []corev1.VolumeMount{{Name: "rendered-config", MountPath: "/etc/analytics-processor"}},
	}); err != nil {
		return err
	}
	if oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
		return r.reconcileAnalyticsImport(ctx, oc)
	}
	return nil
}

// analyticsImportPrerequisites validates the spec before a schedule is created; the reader
// topology is checked by readerImportSources.
func analyticsImportPrerequisites(oc *oramav1alpha1.OramaCloudV1) error {
	c := oc.Spec.Components
	if !c.Engine.Enabled || !c.Analytics.Enabled || !c.ControlDB.Enabled {
		return fmt.Errorf("analyticsProcessor.import requires engine, analytics and controlDB enabled")
	}
	if len(strings.Fields(analyticsImportSchedule(oc))) != 5 {
		return fmt.Errorf("analyticsProcessor.import.schedule must have five cron fields")
	}
	if _, err := cron.ParseStandard(analyticsImportSchedule(oc)); err != nil {
		return fmt.Errorf("analyticsProcessor.import.schedule: %w", err)
	}
	return nil
}

// analyticsImportStatus is status.analyticsImport: the sources the processor is configured with
// and the import CronJob's last successful run. nil while imports are off.
func (r *OramaCloudV1Reconciler) analyticsImportStatus(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*oramav1alpha1.AnalyticsImportStatus, error) {
	if !oc.Spec.Components.AnalyticsProcessor.Enabled || !oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
		return nil, nil
	}
	sources, err := r.readerImportSources(ctx, oc)
	if err != nil {
		return nil, err
	}
	status := &oramav1alpha1.AnalyticsImportStatus{Sources: sources}
	schedule := &batchv1.CronJob{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-analytics-import"}, schedule); {
	case err == nil:
		status.LastSuccessfulTime = schedule.Status.LastSuccessfulTime
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	return status, nil
}

func analyticsImportSchedule(oc *oramav1alpha1.OramaCloudV1) string {
	if s := oc.Spec.Components.AnalyticsProcessor.Import.Schedule; s != "" {
		return s
	}
	return "*/5 * * * *"
}

// The bridge analytics-processor image patches upstream's localhost listener to 0.0.0.0:8080.
// Use that verified Service contract; no pod exec or Kubernetes API access is needed by the Job.
func (r *OramaCloudV1Reconciler) reconcileAnalyticsImport(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	name := oc.Name + "-analytics-import"
	job := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		job.Labels = labelsFor(oc, "analytics-import")
		// Preserve API-defaulted fields outside our intent so a steady schedule makes no writes.
		job.Spec.Schedule = analyticsImportSchedule(oc)
		job.Spec.TimeZone = strPtr("Etc/UTC")
		job.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		job.Spec.StartingDeadlineSeconds = int64Ptr(60)
		job.Spec.SuccessfulJobsHistoryLimit, job.Spec.FailedJobsHistoryLimit = int32Ptr(1), int32Ptr(1)
		jobSpec := &job.Spec.JobTemplate.Spec
		jobSpec.BackoffLimit, jobSpec.ActiveDeadlineSeconds, jobSpec.TTLSecondsAfterFinished = int32Ptr(0), int64Ptr(300), int32Ptr(3600)
		pod := &jobSpec.Template.Spec
		jobSpec.Template.Labels = labelsFor(oc, "analytics-import")
		pod.RestartPolicy, pod.AutomountServiceAccountToken = corev1.RestartPolicyNever, boolPtr(false)
		pod.ImagePullSecrets = imagePullSecrets(oc)
		pod.Containers = []corev1.Container{{
			Name: "import", Image: "curlimages/curl:8.10.1", ImagePullPolicy: corev1.PullIfNotPresent,
			Args:                   []string{"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "240", "--request", "POST", "http://" + oc.Name + "-analytics-processor:8080/import"},
			TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			SecurityContext: hardenedSecurityContext(), Resources: *resourceReqs("32Mi", "10m", "64Mi"),
		}}

		return controllerutil.SetControllerReference(oc, job, r.Scheme)
	})
	return err
}

// reconcileOramasearchApp deploys the legacy api.askorama.ai app (#7).
// Deprecated: UNWIRED from the reconcile step-list (operator#35) — the legacy app just redirects
// to the new app, so we don't run it in-cluster (this also moots the MySQL-vs-CNPG mismatch). Kept
// for reference; not called.
func (r *OramaCloudV1Reconciler) reconcileOramasearchApp(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	a := oc.Spec.Components.OramasearchApp
	if !a.Enabled {
		return r.teardownByComponent(ctx, oc, "oramasearch-app")
	}
	return r.reconcileApp(ctx, oc, appDef{
		component:   "oramasearch-app",
		image:       componentImage(oc, "oramasearch-app", "oramasearch-app"),
		port:        3000,
		ingressHost: a.Host,
		resources:   resourcesOr(a.Resources, resourceReqs("128Mi", "50m", "512Mi")),
	})
}

// searchWorkerNames are index-manager's hosted-search SERVING workers (#6) — distinct from the
// index-BUILDER the ingestion component already deploys. They're Cloudflare Workers, so (like the
// orama-workers bundle) each still needs its CF bindings mapped to in-cluster equivalents; this
// reconciles a Deployment + Service per worker as the scaffold.
var searchWorkerNames = []string{
	"index-shared-worker",
	"large-index-worker",
	"openai-decrypter",
	"telemetry-worker",
}

func (r *OramaCloudV1Reconciler) reconcileSearchWorkers(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.SearchWorkers.Enabled {
		return r.teardownByComponent(ctx, oc, searchWorkerNames...)
	}
	for _, w := range searchWorkerNames {
		if err := r.reconcileApp(ctx, oc, appDef{
			component: w,
			image:     componentImage(oc, w, w),
			port:      8080,
			resources: resourcesOr(oc.Spec.Components.SearchWorkers.Resources, resourceReqs("64Mi", "25m", "256Mi")),
		}); err != nil {
			return err
		}
	}
	return nil
}
