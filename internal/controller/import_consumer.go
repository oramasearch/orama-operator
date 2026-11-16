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

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The import consumer (spec.components.importConsumer, operator#446).
//
// On Prod 1 an EventBridge rule starts one Fargate task of oramasearch/airbyte-fargate-consumer for
// each object written to the import bucket; the task reads S3_BUCKET and S3_KEY, asks the dashboard
// where the object goes (get-source), writes it to the engine with the project write key, and reports
// crawl and file status back, sending PRIVATE_API_KEY as x-api-key on every dashboard call. Here the
// same image runs as one Deployment in its bucket-worker mode (importConsumerArgs): it lists the
// stack's own R2 uploads bucket (<cr>-uploads), runs that same per-object import for each object
// against this stack's own dashboard, and deletes the object once imported.
//
// Configuration errors (no dashboard, no Cloudflare, no PRIVATE_API_KEY source, a missing Secret or
// key) fail the step, which reports Ready=False with the reason ReconcileError. It is the last step,
// so it never keeps another component from being reconciled.

const (
	importConsumerComponent = "import-consumer"
	// importConsumerPort is the worker's health listener; the probes check it accepts connections.
	importConsumerPort int32 = 8080
	// Let in-flight imports finish on rollout. Objects interrupted by shutdown remain in the
	// bucket for the next worker process to discover.
	importConsumerGracePeriod = 600
	// importConsumerWorkerOverheadMi is the worker process's own share of the memory limit; the rest
	// is split between the import processes (importConsumerHeapMi).
	importConsumerWorkerOverheadMi = 128
	// importConsumerMinHeapMi is the smallest heap an import process is given.
	importConsumerMinHeapMi = 64
)

// importConsumerArgs start the image's bucket worker; Deno is the image's entrypoint.
var importConsumerArgs = []string{"run", "-A", "src/bucket-worker.ts"}

// errImportConsumerNeedsDashboard is returned while the consumer is enabled without the dashboard: it
// calls the stack's own dashboard and sends the dashboard's PRIVATE_API_KEY.
var errImportConsumerNeedsDashboard = errors.New("importConsumer requires the dashboard enabled: it calls this stack's dashboard with the dashboard's PRIVATE_API_KEY")

// importConsumerResources is the default: two import processes of the Fargate task's 512 MiB each,
// plus the worker.
func importConsumerResources() *corev1.ResourceRequirements {
	return resourceReqs("256Mi", "100m", "1536Mi")
}

// importConsumerHeapMi is each import process's V8 heap cap in MiB: three quarters of what the memory
// limit leaves after the worker, split between the concurrent imports, so the processes together stay
// under the container's limit (principle 8). No limit leaves the default resources' cap.
func importConsumerHeapMi(res *corev1.ResourceRequirements, concurrency int32) int64 {
	limit, ok := res.Limits[corev1.ResourceMemory]
	if !ok {
		limit = importConsumerResources().Limits[corev1.ResourceMemory]
	}
	if concurrency < 1 {
		concurrency = 1
	}
	limitMi := limit.Value() / (1024 * 1024)
	heap := (limitMi - importConsumerWorkerOverheadMi) * 3 / 4 / int64(concurrency)
	return max(heap, importConsumerMinHeapMi)
}

// validateImportConsumer is the reconcile's copy of the CRD rules, for objects that bypass admission.
func validateImportConsumer(oc *oramav1alpha1.OramaCloudV1) error {
	ic := oc.Spec.Components.ImportConsumer
	if !oc.Spec.Components.Dashboard.Enabled {
		return errImportConsumerNeedsDashboard
	}
	if !oc.Spec.Cloudflare.Enabled {
		return errors.New("importConsumer requires cloudflare.enabled: it reads the stack's R2 uploads bucket")
	}
	var missing []string
	if ic.Image == "" {
		missing = append(missing, "image")
	}
	if len(missing) > 0 {
		return fmt.Errorf("importConsumer is enabled but has no %v", missing)
	}
	return nil
}

// reconcileImportConsumer runs or removes the consumer Deployment.
func (r *OramaCloudV1Reconciler) reconcileImportConsumer(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	ic := oc.Spec.Components.ImportConsumer
	if !ic.Enabled {
		return r.teardownByComponent(ctx, oc, importConsumerComponent)
	}
	if err := validateImportConsumer(oc); err != nil {
		return err
	}
	sources, err := r.dashboardEnvSources(ctx, oc)
	if err != nil {
		return fmt.Errorf("importConsumer: %w", err)
	}
	apiKey, err := dashboardEnvSecretKey(oc, sources, "PRIVATE_API_KEY", "importConsumer", "the import consumer")
	if err != nil {
		return err
	}
	if err := r.requireSecretKeys(ctx, oc, "importConsumer PRIVATE_API_KEY", apiKey.Name, apiKey.Key); err != nil {
		return err
	}
	uploadsSecret := oc.Name + "-uploads-r2"
	if err := r.requireSecretKeys(ctx, oc, "importConsumer uploads bucket", uploadsSecret, "endpoint", "bucket", "region", "accessKeyId", "secretAccessKey"); err != nil {
		return err
	}

	res := resourcesOr(ic.Resources, importConsumerResources())
	concurrency := ic.Concurrency
	if concurrency < 1 {
		concurrency = 2
	}
	env := append(uploadsBucketEnv(oc, importConsumerUploadsEnv), []corev1.EnvVar{
		{Name: "NODE_ENV", Value: "production"},
		{Name: "POLL_INTERVAL_SECONDS", Value: "10"},
		// The in-cluster dashboard Service, never the public hostname, so the consumer cannot reach
		// Prod 1 while DNS moves.
		{Name: "DASHBOARD_URL", Value: "http://" + oc.Name + "-dashboard:3000"},
		{Name: "PRIVATE_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: apiKey}},
		{Name: "WORKER_CONCURRENCY", Value: strconv.Itoa(int(concurrency))},
		{Name: "IMPORT_MAX_OLD_SPACE_MB", Value: strconv.FormatInt(importConsumerHeapMi(res, concurrency), 10)},
		{Name: "PORT", Value: strconv.Itoa(int(importConsumerPort))},
	}...)
	return r.reconcileApp(ctx, oc, appDef{
		component: importConsumerComponent,
		image:     ic.Image,
		port:      importConsumerPort,
		// Duplicate-import protection is local to one worker process.
		replicas: 1,
		recreate: true,
		args:     importConsumerArgs,
		env:      env,
		// Nothing from the shared ConfigMap or the component Secret: the consumer reads only the
		// values above.
		envFrom:            []corev1.EnvFromSource{},
		credentialSecrets:  []string{uploadsSecret, apiKey.Name},
		resources:          res,
		gracePeriodSeconds: importConsumerGracePeriod,
	})
}
