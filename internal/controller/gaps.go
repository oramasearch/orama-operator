/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Gap-fill components surfaced by the techops-docs gap analysis (operator issues #5–#7).
// The QStash-equivalent queue (#8) is config wiring (workers → the in-cluster RabbitMQ), not a
// reconciled component here.

// The ClickHouse tables analytics-processor creates at start (create_tables) and writes to.
const (
	analyticsSearchTable      = "oramacore_analytics_search_raw"
	analyticsInteractionTable = "oramacore_analytics_interaction_raw"
	analyticsAggregatedTable  = "oramacore_analytics_search_aggregated"
)

// analyticsProcessorConfigYAML renders the config the `server` binary reads (config.rs). It
// connects to ClickHouse + Postgres at startup, so these are the real in-cluster endpoints.
// Secret values are __PLACEHOLDER__s the render init container substitutes — never in the CM —
// inside single-quoted scalars, so renderEscFunc keeps every character of a value. oramacores lists
// every reader ordinal once (analyticsSources), each with the analytics key. It is the same whether
// the import is enabled, disabled or suspended: the server reads it only when POST /import runs.
func analyticsProcessorConfigYAML(oc *oramav1alpha1.OramaCloudV1) string {
	n := oc.Name
	var sources strings.Builder
	for _, src := range analyticsSources(oc) {
		fmt.Fprintf(&sources, `  - name: %s
    api_url: %s
    api_key: '%s'
    enabled: true
`, src.name, src.url, keyPlaceholder(analyticsKeyEnvName))
	}
	return fmt.Sprintf(`clickhouse:
  url: http://%s-analytics:8123
  user: default
  password: '__CLICKHOUSE_PASSWORD__'
  database: default
  search_table: %s
  interaction_table: %s
  aggregated_search_table: %s
create_tables: true
oramacores:
%spostgres:
  url: '__PG_URI__'
  schema: analytics
  runs_table: runs
  delta: 60m
`, n, analyticsSearchTable, analyticsInteractionTable, analyticsAggregatedTable, sources.String())
}

// analyticsSource is one oramacores entry: a reader's GET /v1/analytics.
type analyticsSource struct{ name, url string }

// analyticsSources is the readers analytics-processor imports from: reader ordinals
// 0..readerReplicas-1, each once, by its own address through <cr>-reader-headless (the readers'
// governing Service), never the load-balanced <cr>-reader. GET /v1/analytics returns and erases a
// reader's events, so a load-balanced call would drain whichever pod it landed on. Only readers
// record search events (reader_side.analytics); the writer has none. The list comes from the spec,
// not the live StatefulSet; a negative count gives none, and guardAnalyticsImport rejects it.
func analyticsSources(oc *oramav1alpha1.OramaCloudV1) []analyticsSource {
	var sources []analyticsSource
	for i := range readerReplicas(oc) {
		name := fmt.Sprintf("%s-reader-%d", oc.Name, i)
		sources = append(sources, analyticsSource{name: name, url: fmt.Sprintf("http://%s.%s.%s.svc:%d/v1/analytics",
			name, readerHeadlessServiceName(oc), oc.Namespace, oramacorePort)})
	}
	return sources
}

// The analytics import's fixed schedule and its helper image. There is no per-stack schedule.
// spec.images.overrides["analytics-import"] replaces the image, so a set can pin it by digest; the
// default is a full reference, not resolved against the stack's registry and tag.
const (
	analyticsImportSchedule  = "*/5 * * * *"
	analyticsImportTimeZone  = "Etc/UTC"
	analyticsImportComponent = "analytics-import"
	analyticsImportImage     = pinnedCurlImage
)

// analyticsImportScript is what each import Job runs: POST /import, then POST /aggregate2, the
// processor route that runs the same aggregation as Prod 1's aggregator binary
// (analytics-processor.aggregator.timer, every five minutes beside the importer). The aggregation
// fills oramacore_analytics_search_aggregated, which the dashboard's top terms, no-results and totals
// read. It runs even when the import fails, as Prod 1's two independent timers do, so readers an
// earlier failed run did import are still aggregated; the Job fails when either call fails. Each
// call has its own request deadline, together inside the Job's 300 s, and neither retries.
func analyticsImportScript(oc *oramav1alpha1.OramaCloudV1) string {
	const curl = "curl --fail --silent --show-error --connect-timeout 10 --request POST --write-out '\\n'"
	base := fmt.Sprintf("http://%s-analytics-processor:8080", oc.Name)
	return fmt.Sprintf(`rc=0
%[1]s --max-time 180 %[2]s/import || rc=1
%[1]s --max-time 110 %[2]s/aggregate2 || rc=1
exit $rc
`, curl, base)
}

// analyticsImportName is the import CronJob, <cr>-analytics-import.
func analyticsImportName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-analytics-import" }

// guardAnalyticsImport removes <cr>-analytics-import unless the processor and its import are both
// enabled and the import's prerequisites hold, and returns the failing prerequisite or a cleanup
// error. It never creates the CronJob (reconcileAnalyticsImport does). Reconcile runs it right after
// reading the CR, so an earlier component's failure cannot leave an old schedule running;
// reconcileAnalyticsProcessor runs it again for direct callers. A disabled processor reports no
// import error: its step removes the processor too. Suspension does not skip the checks.
func (r *OramaCloudV1Reconciler) guardAnalyticsImport(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	p := oc.Spec.Components.AnalyticsProcessor
	if !p.Enabled || !p.Import.Enabled {
		return r.teardownByComponent(ctx, oc, "analytics-import")
	}
	if err := analyticsImportPrerequisites(oc); err != nil {
		if cleanupErr := r.teardownByComponent(ctx, oc, "analytics-import"); cleanupErr != nil {
			return cleanupErr
		}
		return err
	}
	return nil
}

// analyticsImportPrerequisites names what an enabled import needs and does not have: the engine
// (the readers), the in-cluster ClickHouse it writes to, the control DB holding its run log, and a
// reader count of zero (the default, 3) or more.
func analyticsImportPrerequisites(oc *oramav1alpha1.OramaCloudV1) error {
	c := oc.Spec.Components
	var missing []string
	if !c.Engine.Enabled {
		missing = append(missing, "engine")
	}
	if !c.Analytics.Enabled || c.Analytics.Mode == "external" {
		missing = append(missing, "analytics (the in-cluster ClickHouse)")
	}
	if !c.ControlDB.Enabled {
		missing = append(missing, "controlDB")
	}
	if len(missing) > 0 {
		return fmt.Errorf("analyticsProcessor.import.enabled requires %s enabled; its CronJob %s is removed",
			strings.Join(missing, ", "), analyticsImportName(oc))
	}
	if n := c.Engine.Reader.Replicas; n < 0 {
		return fmt.Errorf("analyticsProcessor.import.enabled: engine.reader.replicas is %d, an invalid reader count; its CronJob %s is removed",
			n, analyticsImportName(oc))
	}
	return nil
}

// reconcileAnalyticsProcessor deploys analytics-processor (#5): the worker that pulls the readers'
// /v1/analytics into ClickHouse when POST /import is called, plus the import CronJob that calls it
// (reconcileAnalyticsImport) and the ClickHouse retention Job (reconcileAnalyticsRetention). Its
// `server` binary reads a YAML config FILE (CONFIG_PATH) and connects to ClickHouse + Postgres at
// startup — so the operator renders that file via an init container and mounts it; the template's
// hash is on the pod, so a changed reader count rolls it. The init container takes each value by
// secretKeyRef: the analytics key from <cr>-analytics-api-key (#386), the ClickHouse password from
// the component Secret and the Postgres URI of its own role, analytics_processor, from
// <cr>-controldb-analytics-processor (#823; the owner's <cr>-controldb-app on a read-tier replica,
// analyticsProcessorPGURIEnv). Neither
// container loads the component Secret whole, so a MASTER_API_KEY an older operator seeded there
// never reaches the pod: the processor holds no master key.
//
// The image must carry the bridge's listener patch (orama-cloud-v1-bridge
// build/images/analytics-processor/Dockerfile), which binds src/bin/server.rs to 0.0.0.0:8080.
// Upstream binds 127.0.0.1:8888, which neither the Service, the probes nor the import Job can reach,
// so every import would fail to connect.
func (r *OramaCloudV1Reconciler) reconcileAnalyticsProcessor(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.Components.AnalyticsProcessor.Enabled {
		if err := r.teardownByComponent(ctx, oc, "analytics-import", "analytics-retention", "analytics-processor"); err != nil {
			return err
		}
		return r.removeUnlabeledProcessorConfig(ctx, oc)
	}
	if err := r.guardAnalyticsImport(ctx, oc); err != nil {
		return err
	}
	// The processor signs in to the control DB as its own role (#823): until that role's grants
	// apply, the processor is left as it is.
	if ready, err := r.analyticsProcessorRoleReady(ctx, oc); err != nil || !ready {
		return err
	}
	config := analyticsProcessorConfigYAML(oc)
	img := componentImage(oc, "analytics-processor", "analytics-processor")
	cmName := analyticsProcessorConfigName(oc)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		// Labelled so the component teardown removes it with the Deployment.
		cm.Labels = labelsFor(oc, "analytics-processor")
		cm.Data = map[string]string{"config.yaml": config}
		return controllerutil.SetControllerReference(oc, cm, r.Scheme)
	}); err != nil {
		return err
	}
	clickhouseSecret := clickhouseAuthSecretName(oc)
	// The shared, non-secret ConfigMap only.
	envFrom := []corev1.EnvFromSource{sharedConfigEnvFrom(oc)}
	render := corev1.Container{
		Name:  "render-config",
		Image: img,
		Command: []string{"sh", "-c", renderEscFunc + "sed" +
			renderSubstitutions([]string{"CLICKHOUSE_PASSWORD", analyticsKeyEnvName, "PG_URI"}) +
			" /tmpl/config.yaml > /etc/analytics-processor/config.yaml"},
		EnvFrom: envFrom,
		Env: []corev1.EnvVar{
			analyticsKeyEnv(oc),
			clickhousePasswordEnv(oc),
			analyticsProcessorPGURIEnv(oc, "PG_URI"),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config-tmpl", MountPath: "/tmpl"},
			{Name: "rendered-config", MountPath: "/etc/analytics-processor"},
		},
	}
	sum := sha256.Sum256([]byte(config))
	if err := r.reconcileApp(ctx, oc, appDef{
		component:      "analytics-processor",
		image:          img,
		port:           8080,
		env:            []corev1.EnvVar{{Name: "CONFIG_PATH", Value: "/etc/analytics-processor/config.yaml"}},
		envFrom:        envFrom,
		resources:      resourcesOr(oc.Spec.Components.AnalyticsProcessor.Resources, resourceReqs("128Mi", "50m", "512Mi")),
		initContainers: []corev1.Container{render},
		// The unrendered template's hash: a changed reader count or address rolls the processor.
		podAnnotations: map[string]string{configHashKey: hex.EncodeToString(sum[:])},
		// Every Secret the render init reads, so a changed analytics key, ClickHouse or Postgres
		// password rolls the processor.
		credentialSecrets: []string{analyticsKeySecretName(oc), clickhouseSecret, analyticsProcessorDBSecret(oc)},
		volumes: []corev1.Volume{
			{Name: "config-tmpl", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
			{Name: "rendered-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		volumeMounts: []corev1.VolumeMount{{Name: "rendered-config", MountPath: "/etc/analytics-processor"}},
	}); err != nil {
		return err
	}
	if err := r.reconcileAnalyticsRetention(ctx, oc); err != nil {
		return err
	}
	if !oc.Spec.Components.AnalyticsProcessor.Import.Enabled {
		return nil
	}
	return r.reconcileAnalyticsImport(ctx, oc)
}

func analyticsProcessorConfigName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-analytics-processor-config"
}

// removeUnlabeledProcessorConfig deletes <cr>-analytics-processor-config when the CR controls it.
// Earlier operator versions created it without labels, so the component teardown cannot find it. A
// ConfigMap of that name the CR does not control is left alone.
func (r *OramaCloudV1Reconciler) removeUnlabeledProcessorConfig(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: analyticsProcessorConfigName(oc)}, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(cm, oc) {
		return nil
	}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("teardown analytics-processor/%s: %w", cm.Name, err)
	}
	return nil
}

// reconcileAnalyticsImport owns <cr>-analytics-import, the CronJob that sends POST /import and then
// POST /aggregate2 to the processor every five minutes (UTC) (analyticsImportScript). Each import
// drains the readers' logs, so nothing may overlap or repeat one: Forbid keeps scheduled Jobs from
// running together, and there are no Job retries (backoffLimit 0) and no request retries (curl
// without --retry). A failed run stays visible in the Job's status and the processor's logs; the
// next scheduled run is the next attempt. The processor visits its sources in order and stops at the
// first error, so a failed run may have drained only some readers, and a reader that is not Ready
// (the headless Service publishes no address for it) blocks every reader after it until it is. The
// pod has no credentials: neither route takes any, and no service-account token is mounted. import.suspend is copied to spec.suspend, which stops new Jobs
// but not one already running. Only the fields set here are managed, so a steady reconcile, with
// the API's defaults in place, makes no write.
func (r *OramaCloudV1Reconciler) reconcileAnalyticsImport(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: analyticsImportName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cron, func() error {
		lbls := labelsFor(oc, analyticsImportComponent)
		cron.Labels = lbls
		cron.Spec.Schedule = analyticsImportSchedule
		cron.Spec.TimeZone = strPtr(analyticsImportTimeZone)
		cron.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cron.Spec.Suspend = boolPtr(oc.Spec.Components.AnalyticsProcessor.Import.Suspend)
		cron.Spec.StartingDeadlineSeconds = int64Ptr(60)
		cron.Spec.SuccessfulJobsHistoryLimit = int32Ptr(1)
		cron.Spec.FailedJobsHistoryLimit = int32Ptr(1)
		job := &cron.Spec.JobTemplate.Spec
		job.BackoffLimit = int32Ptr(0)
		job.ActiveDeadlineSeconds = int64Ptr(300)
		job.TTLSecondsAfterFinished = int32Ptr(3600)
		job.Template.Labels = lbls
		pod := &job.Template.Spec
		pod.RestartPolicy = corev1.RestartPolicyNever
		pod.AutomountServiceAccountToken = boolPtr(false)
		pod.ImagePullSecrets = imagePullSecrets(oc)
		pod.Containers = []corev1.Container{{
			Name:            "import",
			Image:           storeImage(oc, analyticsImportComponent, analyticsImportImage),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", analyticsImportScript(oc)},
			Resources:       *resourceReqs("32Mi", "10m", "64Mi"),
			SecurityContext: hardenedSecurityContext(),
			// The API's defaults, set so a steady reconcile compares equal.
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		}}
		return controllerutil.SetControllerReference(oc, cron, r.Scheme)
	})
	return err
}

// analyticsRetentionDays is how long the in-cluster ClickHouse keeps analytics rows: decision 4 of
// the Cloud v1 migration (orama-bridge docs/projects/orama-cloud-v1/decisions-2026-10-16.md, 16 Oct),
// analytics start fresh at the cutover and keep 30 days.
const analyticsRetentionDays = 30

// analyticsRetentionTables is each processor table with the column its TTL counts from.
var analyticsRetentionTables = []struct{ table, column string }{
	{analyticsSearchTable, "timestamp"},
	{analyticsInteractionTable, "timestamp"},
	{analyticsAggregatedTable, "created_at"},
}

// analyticsRetentionHashKey is the retention Job's annotation: the hash of its script, so a changed
// retention replaces the Job and runs again.
const analyticsRetentionHashKey = "orama.cloud/retention-hash"

// analyticsRetentionScript waits for the processor's three tables (it creates them at start) by
// failing until they exist, then sets each one's TTL to analyticsRetentionDays where
// system.tables shows another. A second run changes nothing. ClickHouse prints the TTL as
// `<column> + toIntervalDay(<days>)`.
func analyticsRetentionScript(oc *oramav1alpha1.OramaCloudV1) string {
	var b strings.Builder
	fmt.Fprintf(&b, `set -eu
ch() { clickhouse-client --host %s-analytics --port 9000 --user default --password "$CLICKHOUSE_PASSWORD" --query "$1"; }
`, oc.Name)
	for _, t := range analyticsRetentionTables {
		fmt.Fprintf(&b, `if [ "$(ch "EXISTS TABLE default.%[1]s")" != 1 ]; then
  echo "default.%[1]s does not exist yet: analytics-processor creates it when it starts" >&2; exit 1
fi
`, t.table)
	}
	for _, t := range analyticsRetentionTables {
		fmt.Fprintf(&b, `case "$(ch "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = '%[1]s'") " in
  *"TTL %[2]s + toIntervalDay(%[3]d) "*) echo "default.%[1]s keeps %[3]d days" ;;
  *) ch "ALTER TABLE default.%[1]s MODIFY TTL %[2]s + INTERVAL %[3]d DAY"; echo "default.%[1]s now keeps %[3]d days" ;;
esac
`, t.table, t.column, analyticsRetentionDays)
	}
	return b.String()
}

// analyticsRetentionName is the retention Job, <cr>-analytics-retention.
func analyticsRetentionName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-analytics-retention"
}

// reconcileAnalyticsRetention owns <cr>-analytics-retention, the Job that sets the processor's
// ClickHouse tables to keep analyticsRetentionDays (analyticsRetentionScript), which also bounds the
// ClickHouse volume. It runs clickhouse-client from the stack's ClickHouse image as the ClickHouse
// default user, its password by secretKeyRef from the analytics component Secret. The Job retries
// (backoffLimit) while the tables do not exist yet; one that still fails, or that ran an older
// script, is deleted (foreground, so two never run together) and created again on the next
// reconcile. A succeeded Job is kept as the record: the status's analytics-retention entry reads
// applied, pending or failed from it. Without the in-cluster ClickHouse there is nothing to set.
// The operator has no ClickHouse schema kind; a ClickHouseSchema kind could later replace this Job.
func (r *OramaCloudV1Reconciler) reconcileAnalyticsRetention(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !analyticsRetentionWanted(oc) {
		return r.teardownByComponent(ctx, oc, "analytics-retention")
	}
	script := analyticsRetentionScript(oc)
	if err := ensureCloudV1ServiceAccount(ctx, r.Client, r.Scheme, oc, analyticsRetentionName(oc), labelsFor(oc, "analytics-retention")); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(script))
	hash := hex.EncodeToString(sum[:])
	credentialHash, err := r.credentialsHash(ctx, oc, clickhouseAuthSecretName(oc))
	if err != nil {
		return err
	}
	job := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: analyticsRetentionName(oc)}, job)
	switch {
	case apierrors.IsNotFound(err):
		job = analyticsRetentionJob(oc, script, hash, credentialHash)
		if err := controllerutil.SetControllerReference(oc, job, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, job)
	case err != nil:
		return err
	case !metav1.IsControlledBy(job, oc):
		return fmt.Errorf("Job %s is not controlled by OramaCloudV1 %s", job.Name, oc.Name)
	case !job.DeletionTimestamp.IsZero():
		return nil // its deletion event reconciles again
	case job.Annotations[analyticsRetentionHashKey] != hash || job.Annotations[credentialsHashKey] != credentialHash || jobFailed(job):
		policy := metav1.DeletePropagationForeground
		err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy,
			Preconditions: &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}})
		return client.IgnoreNotFound(err)
	}
	return nil
}

// analyticsRetentionWanted reports whether the stack runs the processor on the in-cluster ClickHouse.
func analyticsRetentionWanted(oc *oramav1alpha1.OramaCloudV1) bool {
	c := oc.Spec.Components
	return c.AnalyticsProcessor.Enabled && c.Analytics.Enabled && c.Analytics.Mode != "external"
}

func analyticsRetentionJob(oc *oramav1alpha1.OramaCloudV1, script, hash string, credentialHash ...string) *batchv1.Job {
	lbls := labelsFor(oc, "analytics-retention")
	c := corev1.Container{
		Name:            "retention",
		Image:           storeImage(oc, "analytics", clickhouseImage),
		Command:         []string{"sh", "-c", script},
		Env:             []corev1.EnvVar{clickhousePasswordEnv(oc)},
		Resources:       *resourceReqs("128Mi", "10m", "256Mi"),
		SecurityContext: hardenedSecurityContext(),
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: analyticsRetentionName(oc), Namespace: oc.Namespace, Labels: lbls,
			Annotations: map[string]string{analyticsRetentionHashKey: hash}},
		Spec: batchv1.JobSpec{
			// Retries with the Job's backoff while the processor has not created its tables yet.
			BackoffLimit:          int32Ptr(10),
			ActiveDeadlineSeconds: int64Ptr(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					EnableServiceLinks:           boolPtr(false),
					ServiceAccountName:           analyticsRetentionName(oc),
					ImagePullSecrets:             imagePullSecrets(oc),
					Containers:                   []corev1.Container{c},
				},
			},
		},
	}
	if len(credentialHash) != 0 {
		job.Annotations[credentialsHashKey] = credentialHash[0]
		job.Spec.Template.Annotations = map[string]string{credentialsHashKey: credentialHash[0]}
	}
	boundV1PodStorage(&job.Spec.Template.Spec)
	return job
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
		def := appDef{
			component: w,
			image:     componentImage(oc, w, w),
			port:      8080,
			resources: resourcesOr(oc.Spec.Components.SearchWorkers.Resources, resourceReqs("64Mi", "25m", "256Mi")),
		}
		if w == "openai-decrypter" {
			def.env = []corev1.EnvVar{secretKeyEnv("API_SECRET", decrypterAuthSecretName(oc), "API_SECRET")}
			def.credentialSecrets = []string{decrypterAuthSecretName(oc)}
		}
		if err := r.reconcileApp(ctx, oc, def); err != nil {
			return err
		}
	}
	return nil
}
