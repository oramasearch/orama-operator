/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// LakeRestoreRehearsalReconciler runs isolated Postgres restore and upgrade rehearsals and a
// controlled failover rehearsal against a replicated Lake control DB.
type LakeRestoreRehearsalReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

const (
	lakeRehearsalRequeue       = 10 * time.Second
	lakeRehearsalVerifyImage   = "postgres:18-alpine"
	cnpgHealthyPhase           = "Cluster in healthy state"
	postgresLifecycleGapNote   = "one or more lifecycle procedures have not been tested"
	postgresSingleInstanceNote = "failover is unavailable for a single-instance cluster"
)

// +kubebuilder:rbac:groups=orama.cloud,resources=lakerestorerehearsals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakerestorerehearsals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=get;list;watch

// Reconcile advances one rehearsal through a durable, idempotent state machine.
func (r *LakeRestoreRehearsalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rehearsal oramav1alpha1.LakeRestoreRehearsal
	if err := r.Get(ctx, req.NamespacedName, &rehearsal); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if rehearsal.Status.Phase == "Succeeded" || rehearsal.Status.Phase == "Failed" {
		return ctrl.Result{}, r.cleanupRehearsal(ctx, &rehearsal)
	}

	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: rehearsal.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finishRehearsal(ctx, &rehearsal, false, fmt.Sprintf("Lake %q was not found", rehearsal.Spec.LakeRef))
		}
		return ctrl.Result{}, err
	}
	if message := validateRehearsal(&rehearsal, &lake); message != "" {
		return r.finishRehearsal(ctx, &rehearsal, false, message)
	}
	if rehearsal.Status.StartedAt == nil {
		return r.startRehearsal(ctx, &rehearsal)
	}

	switch rehearsal.Spec.Operation {
	case "restore", "upgrade":
		return r.reconcileScratchRehearsal(ctx, &rehearsal, &lake)
	case "failover":
		return r.reconcileFailoverRehearsal(ctx, &rehearsal, &lake)
	default:
		return r.finishRehearsal(ctx, &rehearsal, false, fmt.Sprintf("unsupported operation %q", rehearsal.Spec.Operation))
	}
}

func validateRehearsal(rehearsal *oramav1alpha1.LakeRestoreRehearsal, lake *oramav1alpha1.Lake) string {
	if rehearsal.Spec.Service != "postgres" {
		return fmt.Sprintf("unsupported service %q", rehearsal.Spec.Service)
	}
	if !lakePostgresEnabled(lake) {
		return "control-plane Postgres is disabled"
	}
	if rehearsal.Spec.Operation == "upgrade" && rehearsal.Spec.TargetImage == "" {
		return "spec.targetImage is required for an upgrade rehearsal"
	}
	if rehearsal.Spec.Operation == "failover" {
		instances := int32(1)
		if lake.Spec.Postgres.Instances != nil {
			instances = *lake.Spec.Postgres.Instances
		}
		if instances < 2 {
			return "failover requires spec.postgres.instances of at least 2"
		}
	}
	return ""
}

func (r *LakeRestoreRehearsalReconciler) startRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
) (ctrl.Result, error) {
	now := metav1.Now()
	desired := rehearsal.Status
	desired.Phase = "Running"
	desired.Message = "rehearsal accepted"
	desired.StartedAt = &now
	desired.ObservedGeneration = rehearsal.Generation
	if rehearsal.Spec.Operation == "restore" || rehearsal.Spec.Operation == "upgrade" {
		desired.ScratchNamespace = lakeRehearsalNamespace(rehearsal.Name)
	}
	rehearsal.Status = desired
	if err := r.Status().Update(ctx, rehearsal); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
}

func (r *LakeRestoreRehearsalReconciler) reconcileScratchRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (ctrl.Result, error) {
	namespace := lakeNamespace(lake)
	lakeReconciler := LakeReconciler{Client: r.Client, Scheme: r.Scheme}
	backupConfig, err := lakeReconciler.resolveLakeBackupConfig(ctx, lake, namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !backupConfig.Enabled || !backupConfig.Valid {
		message := backupConfig.Message
		if message == "" {
			message = "control-plane object-store backups are not enabled"
		}
		return r.finishRehearsal(ctx, rehearsal, false, message)
	}

	backup, waiting, err := r.selectRehearsalBackup(ctx, rehearsal, lake, namespace)
	if err != nil {
		return r.finishRehearsal(ctx, rehearsal, false, err.Error())
	}
	if waiting {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	sourceImage, waiting, err := r.sourcePostgresImage(ctx, lake, namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if waiting {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	if err := r.ensureScratchResources(ctx, rehearsal, lake, backupConfig, backup, sourceImage); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
		}
		return ctrl.Result{}, err
	}

	cluster, err := r.getScratchCluster(ctx, rehearsal)
	if err != nil {
		return ctrl.Result{}, err
	}
	if message := cnpgClusterFailure(cluster); message != "" {
		return r.finishRehearsal(ctx, rehearsal, false, message)
	}
	if !cnpgClusterReady(cluster) {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	if rehearsal.Spec.Operation == "upgrade" {
		updated, err := r.ensureScratchUpgrade(ctx, rehearsal, lake, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if updated || !cnpgUpgradeComplete(cluster, rehearsal.Spec.TargetImage) {
			return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
		}
	}

	job, err := r.ensureScratchVerificationJob(ctx, rehearsal, lake)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.finishFromJob(ctx, rehearsal, job)
}

func (r *LakeRestoreRehearsalReconciler) selectRehearsalBackup(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
	namespace string,
) (*unstructured.Unstructured, bool, error) {
	selectedName := rehearsal.Status.SelectedBackup
	from := rehearsal.Spec.From
	if from == "" {
		from = "latest"
	}
	if selectedName == "" && from != "latest" {
		selectedName = from
	}
	if selectedName != "" {
		backup := &unstructured.Unstructured{}
		backup.SetGroupVersionKind(cnpgBackupGVK)
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selectedName}, backup); err != nil {
			return nil, false, fmt.Errorf("get Backup %q: %w", selectedName, err)
		}
		if err := validateSelectedBackup(backup, lake); err != nil {
			return nil, false, err
		}
		if rehearsal.Status.SelectedBackup == "" {
			return nil, true, r.recordSelectedBackup(ctx, rehearsal, backup.GetName())
		}
		return backup, false, nil
	}

	backups := &unstructured.UnstructuredList{}
	backups.SetGroupVersionKind(cnpgBackupListGVK)
	if err := r.List(ctx, backups, client.InNamespace(namespace), client.MatchingLabels{
		"cnpg.io/cluster": lakeControlDB(lake.Name),
	}); err != nil {
		return nil, false, err
	}
	completed := make([]unstructured.Unstructured, 0, len(backups.Items))
	for _, backup := range backups.Items {
		phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
		if phase == "completed" {
			completed = append(completed, backup)
		}
	}
	if len(completed) == 0 {
		return nil, true, nil
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].GetCreationTimestamp().After(completed[j].GetCreationTimestamp().Time)
	})
	return nil, true, r.recordSelectedBackup(ctx, rehearsal, completed[0].GetName())
}

func validateSelectedBackup(backup *unstructured.Unstructured, lake *oramav1alpha1.Lake) error {
	if backup.GetLabels()["cnpg.io/cluster"] != lakeControlDB(lake.Name) {
		return fmt.Errorf("Backup %q does not belong to %s", backup.GetName(), lakeControlDB(lake.Name))
	}
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	if phase != "completed" {
		return fmt.Errorf("Backup %q is not completed", backup.GetName())
	}
	backupID, _, _ := unstructured.NestedString(backup.Object, "status", "backupId")
	if backupID == "" {
		return fmt.Errorf("Backup %q has no status.backupId", backup.GetName())
	}
	return nil
}

func (r *LakeRestoreRehearsalReconciler) recordSelectedBackup(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	name string,
) error {
	rehearsal.Status.SelectedBackup = name
	rehearsal.Status.Message = fmt.Sprintf("selected Backup %s", name)
	return r.Status().Update(ctx, rehearsal)
}

func (r *LakeRestoreRehearsalReconciler) sourcePostgresImage(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
) (string, bool, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeControlDB(lake.Name)}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", true, nil
		}
		return "", false, err
	}
	image, _, _ := unstructured.NestedString(cluster.Object, "status", "image")
	if image == "" {
		return "", true, nil
	}
	return image, false, nil
}

func (r *LakeRestoreRehearsalReconciler) ensureScratchResources(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
	backupConfig lakeBackupConfig,
	backup *unstructured.Unstructured,
	sourceImage string,
) error {
	if err := r.ensureScratchNamespace(ctx, rehearsal); err != nil {
		return err
	}
	if err := r.copyRehearsalSecret(ctx, rehearsal, lakeNamespace(lake), backupConfig.SecretName, "object-store"); err != nil {
		return err
	}
	if err := r.copyRehearsalSecret(ctx, rehearsal, lakeNamespace(lake), lakeControlDB(lake.Name)+"-app", "source-db"); err != nil {
		return err
	}
	return r.ensureScratchCluster(ctx, rehearsal, lake, backupConfig, backup, sourceImage)
}

func (r *LakeRestoreRehearsalReconciler) ensureScratchNamespace(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
) error {
	namespace := &corev1.Namespace{}
	key := client.ObjectKey{Name: rehearsal.Status.ScratchNamespace}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, key, namespace); err == nil {
		if !metav1.IsControlledBy(namespace, rehearsal) {
			return fmt.Errorf("scratch namespace %q is not controlled by this rehearsal", namespace.Name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	namespace.Name = rehearsal.Status.ScratchNamespace
	namespace.Labels = rehearsalLabels(rehearsal)
	if err := controllerutil.SetControllerReference(rehearsal, namespace, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, namespace)
}

func (r *LakeRestoreRehearsalReconciler) copyRehearsalSecret(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	sourceNamespace string,
	sourceName string,
	targetName string,
) error {
	var source corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: sourceNamespace, Name: sourceName}, &source); err != nil {
		return err
	}
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      targetName,
		Namespace: rehearsal.Status.ScratchNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
		target.Labels = rehearsalLabels(rehearsal)
		target.Type = source.Type
		target.Data = source.Data
		return controllerutil.SetControllerReference(rehearsal, target, r.Scheme)
	})
	return err
}

func (r *LakeRestoreRehearsalReconciler) ensureScratchCluster(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
	backupConfig lakeBackupConfig,
	backup *unstructured.Unstructured,
	sourceImage string,
) error {
	cluster := newScratchCluster(rehearsal)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	backupID, _, _ := unstructured.NestedString(backup.Object, "status", "backupId")
	instances := int64(1)
	if rehearsal.Spec.Operation == "upgrade" && lake.Spec.Postgres.Instances != nil {
		instances = int64(*lake.Spec.Postgres.Instances)
	}
	storage := map[string]interface{}{"size": lakePostgresStorageSize(lake)}
	if lake.Spec.Postgres.StorageClassName != "" {
		storage["storageClass"] = lake.Spec.Postgres.StorageClassName
	}
	sourceName := lakeControlDB(lake.Name)
	cluster.SetLabels(rehearsalLabels(rehearsal))
	cluster.Object["spec"] = map[string]interface{}{
		"instances": instances,
		"imageName": sourceImage,
		"storage":   storage,
		"bootstrap": map[string]interface{}{
			"recovery": map[string]interface{}{
				"source": sourceName,
				"recoveryTarget": map[string]interface{}{
					"backupID": backupID,
				},
			},
		},
		"externalClusters": []interface{}{
			map[string]interface{}{
				"name": sourceName,
				"barmanObjectStore": map[string]interface{}{
					"destinationPath": fmt.Sprintf("s3://%s/%s", strings.Trim(backupConfig.Bucket, "/"), strings.Trim(backupConfig.Path, "/")),
					"endpointURL":     backupConfig.Endpoint,
					"serverName":      sourceName,
					"s3Credentials": map[string]interface{}{
						"accessKeyId": map[string]interface{}{
							"name": "object-store",
							"key":  "accessKeyId",
						},
						"secretAccessKey": map[string]interface{}{
							"name": "object-store",
							"key":  "secretAccessKey",
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(rehearsal, cluster, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, cluster)
}

func (r *LakeRestoreRehearsalReconciler) getScratchCluster(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
) (*unstructured.Unstructured, error) {
	cluster := newScratchCluster(rehearsal)
	err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
	return cluster, err
}

func (r *LakeRestoreRehearsalReconciler) ensureScratchUpgrade(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
	cluster *unstructured.Unstructured,
) (bool, error) {
	image, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	if image == rehearsal.Spec.TargetImage {
		return false, nil
	}
	method := "restart"
	if lake.Spec.Postgres.Instances != nil && *lake.Spec.Postgres.Instances > 1 {
		method = "switchover"
	}
	if err := unstructured.SetNestedField(cluster.Object, rehearsal.Spec.TargetImage, "spec", "imageName"); err != nil {
		return false, err
	}
	if err := unstructured.SetNestedField(cluster.Object, "unsupervised", "spec", "primaryUpdateStrategy"); err != nil {
		return false, err
	}
	if err := unstructured.SetNestedField(cluster.Object, method, "spec", "primaryUpdateMethod"); err != nil {
		return false, err
	}
	return true, r.Update(ctx, cluster)
}

func cnpgUpgradeComplete(cluster *unstructured.Unstructured, targetImage string) bool {
	image, _, _ := unstructured.NestedString(cluster.Object, "status", "image")
	return image == targetImage && cnpgClusterReady(cluster)
}

func (r *LakeRestoreRehearsalReconciler) ensureScratchVerificationJob(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "verify",
		Namespace: rehearsal.Status.ScratchNamespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = rehearsalLabels(rehearsal)
	job.Spec = verificationJobSpec(
		lakeControlDB(lake.Name)+"-rw."+lakeNamespace(lake)+".svc.cluster.local",
		"postgres-rw."+rehearsal.Status.ScratchNamespace+".svc.cluster.local",
		"source-db",
		true,
	)
	if err := controllerutil.SetControllerReference(rehearsal, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, r.Create(ctx, job)
}

func (r *LakeRestoreRehearsalReconciler) reconcileFailoverRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (ctrl.Result, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name)}
	if err := r.Get(ctx, key, cluster); err != nil {
		return ctrl.Result{}, err
	}
	if message := cnpgClusterFailure(cluster); message != "" {
		return r.finishRehearsal(ctx, rehearsal, false, message)
	}
	if !cnpgClusterReady(cluster) {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	current, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	if rehearsal.Status.TargetPrimary == "" {
		return r.selectFailoverTarget(ctx, rehearsal, cluster, current)
	}
	if current != rehearsal.Status.TargetPrimary {
		if current != rehearsal.Status.PreviousPrimary {
			message := fmt.Sprintf("primary changed unexpectedly from %s to %s", rehearsal.Status.PreviousPrimary, current)
			return r.finishRehearsal(ctx, rehearsal, false, message)
		}
		if err := r.requestCNPGSwitchover(ctx, cluster, rehearsal.Status.TargetPrimary); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}

	job, err := r.ensureFailoverVerificationJob(ctx, rehearsal, lake)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.finishFromJob(ctx, rehearsal, job)
}

func (r *LakeRestoreRehearsalReconciler) selectFailoverTarget(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	cluster *unstructured.Unstructured,
	current string,
) (ctrl.Result, error) {
	healthy, _, _ := unstructured.NestedStringSlice(cluster.Object, "status", "instancesStatus", "healthy")
	sort.Strings(healthy)
	target := ""
	for _, instance := range healthy {
		if instance != current {
			target = instance
			break
		}
	}
	if current == "" || target == "" {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	rehearsal.Status.PreviousPrimary = current
	rehearsal.Status.TargetPrimary = target
	rehearsal.Status.Message = fmt.Sprintf("selected %s to replace primary %s", target, current)
	if err := r.Status().Update(ctx, rehearsal); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
}

func (r *LakeRestoreRehearsalReconciler) requestCNPGSwitchover(
	ctx context.Context,
	cluster *unstructured.Unstructured,
	target string,
) error {
	desired, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	if desired == target {
		return nil
	}
	base := cluster.DeepCopy()
	if err := unstructured.SetNestedField(cluster.Object, target, "status", "targetPrimary"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(cluster.Object, metav1.Now().UTC().Format(time.RFC3339), "status", "targetPrimaryTimestamp"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(cluster.Object, "Switchover in progress", "status", "phase"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(cluster.Object, fmt.Sprintf("Switching over to %s", target), "status", "phaseReason"); err != nil {
		return err
	}
	return r.Status().Patch(ctx, cluster, client.MergeFrom(base))
}

func (r *LakeRestoreRehearsalReconciler) ensureFailoverVerificationJob(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      lakeRehearsalJobName(rehearsal.Name),
		Namespace: lakeNamespace(lake),
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = rehearsalLabels(rehearsal)
	job.Spec = verificationJobSpec(
		lakeControlDB(lake.Name)+"-rw",
		"",
		lakeControlDB(lake.Name)+"-app",
		false,
	)
	if err := controllerutil.SetControllerReference(rehearsal, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, r.Create(ctx, job)
}

func (r *LakeRestoreRehearsalReconciler) finishFromJob(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	job *batchv1.Job,
) (ctrl.Result, error) {
	if job.Status.Failed > 0 {
		return r.finishRehearsal(ctx, rehearsal, false, "verification Job failed")
	}
	if job.Status.Succeeded == 0 {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	message := fmt.Sprintf("%s rehearsal succeeded", rehearsal.Spec.Operation)
	if rehearsal.Status.SelectedBackup != "" {
		message = fmt.Sprintf("%s using Backup %s", message, rehearsal.Status.SelectedBackup)
	}
	return r.finishRehearsal(ctx, rehearsal, true, message)
}

func (r *LakeRestoreRehearsalReconciler) finishRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	succeeded bool,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	if rehearsal.Spec.LakeRef != "" {
		if err := r.recordLakeRehearsal(ctx, rehearsal, succeeded, message, now); err != nil {
			return ctrl.Result{}, err
		}
	}
	desired := rehearsal.Status
	desired.Phase = "Failed"
	if succeeded {
		desired.Phase = "Succeeded"
	}
	desired.Message = message
	desired.CompletedAt = &now
	desired.ObservedGeneration = rehearsal.Generation
	if !apiequality.Semantic.DeepEqual(rehearsal.Status, desired) {
		rehearsal.Status = desired
		if err := r.Status().Update(ctx, rehearsal); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, r.cleanupRehearsal(ctx, rehearsal)
}

func (r *LakeRestoreRehearsalReconciler) recordLakeRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	succeeded bool,
	message string,
	completed metav1.Time,
) error {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: rehearsal.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	before := lake.DeepCopy()
	services, _ := lakeDataServiceStatus(&lake)
	postgres := &services[0]
	if succeeded {
		switch rehearsal.Spec.Operation {
		case "restore":
			postgres.Backup = "tested"
			postgres.Restore = "tested"
		case "upgrade":
			postgres.Upgrade = "tested"
		case "failover":
			postgres.Failover = "tested"
		}
		postgres.Notes = message
	} else {
		markPostgresRehearsalUntested(postgres, rehearsal.Spec.Operation)
		postgres.Notes = fmt.Sprintf("last %s rehearsal failed: %s", rehearsal.Spec.Operation, message)
	}
	if postgres.Failover == "none" && !strings.Contains(postgres.Notes, postgresSingleInstanceNote) {
		postgres.Notes += "; " + postgresSingleInstanceNote
	}
	postgres.LastVerified = &completed
	postgres.Managed = postgresDataServiceManaged(postgres, &lake)
	lake.Status.DataServices = services
	lake.Status.DataServicesSummary = dataServiceSummary(services)
	if apiequality.Semantic.DeepEqual(before.Status, lake.Status) {
		return nil
	}
	return r.Status().Patch(ctx, &lake, client.MergeFrom(before))
}

func markPostgresRehearsalUntested(service *oramav1alpha1.LakeDataServiceStatus, operation string) {
	switch operation {
	case "restore":
		if service.Backup != "none" {
			service.Backup = "untested"
		}
		if service.Restore != "none" {
			service.Restore = "untested"
		}
	case "upgrade":
		if service.Upgrade != "none" {
			service.Upgrade = "untested"
		}
	case "failover":
		if service.Failover != "none" {
			service.Failover = "untested"
		}
	}
}

func (r *LakeRestoreRehearsalReconciler) cleanupRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
) error {
	if rehearsal.Status.ScratchNamespace != "" {
		objects := []client.Object{
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "verify", Namespace: rehearsal.Status.ScratchNamespace}},
			newScratchCluster(rehearsal),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source-db", Namespace: rehearsal.Status.ScratchNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "object-store", Namespace: rehearsal.Status.ScratchNamespace}},
		}
		for _, object := range objects {
			if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rehearsal.Status.ScratchNamespace}}
		if err := r.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if rehearsal.Spec.Operation == "failover" && rehearsal.Spec.LakeRef != "" {
		var lake oramav1alpha1.Lake
		if err := r.Get(ctx, client.ObjectKey{Name: rehearsal.Spec.LakeRef}, &lake); err == nil {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:      lakeRehearsalJobName(rehearsal.Name),
				Namespace: lakeNamespace(&lake),
			}}
			if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func cnpgClusterReady(cluster *unstructured.Unstructured) bool {
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	current, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	return instances > 0 && ready >= instances && phase == cnpgHealthyPhase && current != ""
}

func cnpgClusterFailure(cluster *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	lower := strings.ToLower(phase)
	if strings.Contains(lower, "unrecoverable") || strings.Contains(lower, "failed") || strings.Contains(lower, "error") {
		return fmt.Sprintf("CNPG Cluster entered terminal phase %q", phase)
	}
	return ""
}

func verificationJobSpec(sourceHost string, restoredHost string, secretName string, compare bool) batchv1.JobSpec {
	backoff := int32(0)
	deadline := int64(600)
	script := postgresConnectionVerificationScript
	if compare {
		script = postgresRestoreVerificationScript
	}
	env := []corev1.EnvVar{
		{Name: "SOURCE_HOST", Value: sourceHost},
		{Name: "PGUSER", ValueFrom: rehearsalSecretKey(secretName, "username")},
		{Name: "PGPASSWORD", ValueFrom: rehearsalSecretKey(secretName, "password")},
		{Name: "QUERY", Value: postgresVerificationQuery},
	}
	if restoredHost != "" {
		env = append(env, corev1.EnvVar{Name: "RESTORED_HOST", Value: restoredHost})
	}
	return batchv1.JobSpec{
		BackoffLimit:          &backoff,
		ActiveDeadlineSeconds: &deadline,
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "verify",
					Image:   lakeRehearsalVerifyImage,
					Command: []string{"/bin/sh", "-ec", script},
					Env:     env,
				}},
			},
		},
	}
}

func rehearsalSecretKey(name string, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}}
}

const postgresVerificationQuery = `
SELECT 'agent_guard=' || count(*) FROM agent_guard
UNION ALL SELECT 'settings_guard=' || count(*) FROM settings_guard
UNION ALL SELECT 'platform_guard=' || count(*) FROM platform_guard
UNION ALL SELECT 'identity_exit_guard=' || count(*) FROM identity_exit_guard
UNION ALL SELECT 'token_attenuation_guard=' || count(*) FROM token_attenuation_guard
UNION ALL SELECT 'session_lookup_guard=' || count(*) FROM session_lookup_guard
UNION ALL SELECT 'custodial_identity_guard=' || count(*) FROM custodial_identity_guard
UNION ALL SELECT 'services_guard=' || count(*) FROM services_guard
UNION ALL SELECT 'service_catalog_guard=' || count(*) FROM service_catalog_guard
UNION ALL SELECT 'sqlite_services_guard=' || count(*) FROM sqlite_services_guard
UNION ALL SELECT 'auth_app_guard=' || count(*) FROM auth_app_guard
UNION ALL SELECT 'drive_guard=' || count(*) FROM drive_guard
UNION ALL SELECT 'forge_guard=' || count(*) FROM forge_guard
UNION ALL SELECT 'identity_core_guard=' || count(*) FROM identity_core_guard
UNION ALL SELECT 'identity_mfa_guard=' || count(*) FROM identity_mfa_guard
UNION ALL SELECT 'metering_guard=' || count(*) FROM metering_guard
UNION ALL SELECT 'offline_recovery_guard=' || count(*) FROM offline_recovery_guard
UNION ALL SELECT 'pki_guard=' || count(*) FROM pki_guard
UNION ALL SELECT 'secrets_guard=' || count(*) FROM secrets_guard
UNION ALL SELECT 'sso_federation_guard=' || count(*) FROM sso_federation_guard
UNION ALL SELECT 'schema_version=' || max(version) FROM schema_migrations
ORDER BY 1;
`

const postgresRestoreVerificationScript = `
psql --host="$SOURCE_HOST" --dbname=lake --tuples-only --no-align --command "$QUERY" > /tmp/source
psql --host="$RESTORED_HOST" --dbname=lake --tuples-only --no-align --command "$QUERY" > /tmp/restored
diff -u /tmp/source /tmp/restored
`

const postgresConnectionVerificationScript = `
psql --host="$SOURCE_HOST" --dbname=lake --tuples-only --no-align --command "SELECT max(version) FROM schema_migrations"
`

func newScratchCluster(rehearsal *oramav1alpha1.LakeRestoreRehearsal) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("postgres")
	cluster.SetNamespace(rehearsal.Status.ScratchNamespace)
	return cluster
}

func rehearsalLabels(rehearsal *oramav1alpha1.LakeRestoreRehearsal) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lake-postgres-rehearsal",
		"app.kubernetes.io/instance":   rehearsal.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
}

func lakeNamespace(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Namespace == "" {
		return "lake-dev"
	}
	return lake.Spec.Namespace
}

func lakePostgresStorageSize(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Postgres.StorageSize == "" {
		return "50Gi"
	}
	return lake.Spec.Postgres.StorageSize
}

func lakeRehearsalNamespace(name string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	base := strings.ToLower(strings.ReplaceAll(name, ".", "-"))
	const prefix = "lake-rehearsal-"
	maximumBaseLength := 63 - len(prefix) - 1 - len(hash)
	if len(base) > maximumBaseLength {
		base = base[:maximumBaseLength]
	}
	base = strings.Trim(base, "-")
	return prefix + base + "-" + hash
}

func lakeRehearsalJobName(name string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return "lake-rehearsal-" + hash
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakeRestoreRehearsalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeRestoreRehearsal{}).
		Owns(&batchv1.Job{}).
		Owns(newScratchCluster(&oramav1alpha1.LakeRestoreRehearsal{})).
		Named("lake-restore-rehearsal").
		Complete(r)
}
