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
// same image runs as one Deployment in its queue-worker mode (importConsumerArgs): it long-polls an
// SQS queue fed by the bucket's object-created events and runs that same per-object import for each
// event, against this stack's own dashboard. The queue, the bucket's event delivery to it and the IAM
// user are AWS resources the operator neither creates nor changes (docs/cloud-v1-import-consumer.md).
//
// Configuration errors (no dashboard, no PRIVATE_API_KEY source, a missing Secret or key) fail the
// step, which reports Ready=False with the reason ReconcileError. It is the last step, so it never
// keeps another component from being reconciled.

const (
	importConsumerComponent = "import-consumer"
	// importConsumerPort is the worker's health listener; the probes check it accepts connections.
	importConsumerPort int32 = 8080
	// The keys the AWS credentials Secret must hold, passed to the AWS SDK under the same names.
	awsAccessKeyIDKey     = "AWS_ACCESS_KEY_ID"
	awsSecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY"
	// importConsumerGracePeriod lets in-flight imports finish on a rollout: the worker stops receiving
	// on SIGTERM and waits for its imports. One cut short is not deleted from the queue, so SQS hands
	// it out again once its visibility timeout passes.
	importConsumerGracePeriod = 600
	// importConsumerWorkerOverheadMi is the worker process's own share of the memory limit; the rest
	// is split between the import processes (importConsumerHeapMi).
	importConsumerWorkerOverheadMi = 128
	// importConsumerMinHeapMi is the smallest heap an import process is given.
	importConsumerMinHeapMi = 64
)

// importConsumerArgs start the image's queue worker (src/sqs-worker.ts) instead of its default
// one-object command (src/main.ts), which Prod 1's Fargate task still runs. Deno is the image's
// entrypoint.
var importConsumerArgs = []string{"run", "-A", "src/sqs-worker.ts"}

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
	var missing []string
	if ic.Image == "" {
		missing = append(missing, "image")
	}
	if ic.QueueURL == "" {
		missing = append(missing, "queueURL")
	}
	if ic.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if ic.AWSCredentialsSecretRef == nil || ic.AWSCredentialsSecretRef.Name == "" {
		missing = append(missing, "awsCredentialsSecretRef")
	}
	if len(missing) > 0 {
		return fmt.Errorf("importConsumer is enabled but has no %v", missing)
	}
	return nil
}

// importConsumerAWSSecret is the AWS credentials Secret an enabled consumer reads, or "".
func importConsumerAWSSecret(oc *oramav1alpha1.OramaCloudV1) string {
	ic := oc.Spec.Components.ImportConsumer
	if !ic.Enabled || ic.AWSCredentialsSecretRef == nil {
		return ""
	}
	return ic.AWSCredentialsSecretRef.Name
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
	awsSecret := ic.AWSCredentialsSecretRef.Name
	if err := r.requireSecretKeys(ctx, oc, "importConsumer.awsCredentialsSecretRef", awsSecret, awsAccessKeyIDKey, awsSecretAccessKeyKey); err != nil {
		return err
	}

	res := resourcesOr(ic.Resources, importConsumerResources())
	concurrency := ic.Concurrency
	if concurrency < 1 {
		concurrency = 2
	}
	region := ic.Region
	if region == "" {
		region = "us-east-1"
	}
	awsKey := func(env, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: awsSecret}, Key: key}}}
	}
	env := []corev1.EnvVar{
		{Name: "NODE_ENV", Value: "production"},
		{Name: "SQS_QUEUE_URL", Value: ic.QueueURL},
		{Name: "S3_BUCKET", Value: ic.Bucket},
		{Name: "AWS_REGION", Value: region},
		awsKey(awsAccessKeyIDKey, awsAccessKeyIDKey),
		awsKey(awsSecretAccessKeyKey, awsSecretAccessKeyKey),
		// The in-cluster dashboard Service, never the public hostname, so the consumer cannot reach
		// Prod 1 while DNS moves.
		{Name: "DASHBOARD_URL", Value: "http://" + oc.Name + "-dashboard:3000"},
		{Name: "PRIVATE_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: apiKey}},
		{Name: "WORKER_CONCURRENCY", Value: strconv.Itoa(int(concurrency))},
		{Name: "IMPORT_MAX_OLD_SPACE_MB", Value: strconv.FormatInt(importConsumerHeapMi(res, concurrency), 10)},
		{Name: "PORT", Value: strconv.Itoa(int(importConsumerPort))},
	}
	replicas := ic.Replicas
	if replicas < 1 {
		replicas = 1
	}
	return r.reconcileApp(ctx, oc, appDef{
		component: importConsumerComponent,
		image:     ic.Image,
		port:      importConsumerPort,
		replicas:  replicas,
		args:      importConsumerArgs,
		env:       env,
		// Nothing from the shared ConfigMap or the component Secret: the consumer reads only the
		// values above.
		envFrom:            []corev1.EnvFromSource{},
		credentialSecrets:  []string{awsSecret, apiKey.Name},
		resources:          res,
		gracePeriodSeconds: importConsumerGracePeriod,
	})
}
