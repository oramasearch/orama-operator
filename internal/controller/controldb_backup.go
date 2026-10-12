/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// CNPG Barman object-store backups of the OramaCloudV1 control DB (spec.components.controlDB.backup).
// resolveControlDBBackup (controldb.go) validates the ControlDBBackupSpec with
// resolveControlDBBackupConfig; reconcileControlDB renders controlDBBarmanBackup into the Cluster spec
// and owns a ScheduledBackup built by newControlDBScheduledBackup/controlDBScheduledBackupSpec; and
// status.go reports controlDBBackupStatus as the BackupReady condition. Lake keeps its own copy in
// lake_controller.go.

var (
	controlDBScheduledBackupGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup"}
	controlDBBackupGVK          = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"}
	controlDBBackupListGVK      = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "BackupList"}
)

const (
	defaultControlDBBackupSchedule  = "0 0 3 * * *"
	defaultControlDBBackupRetention = "30d"
)

// controlDBBackupConfig is a validated ControlDBBackupSpec with its defaults applied. Enabled && !Valid
// carries the Reason/Message the owner reports; nothing is configured in CNPG until Valid.
type controlDBBackupConfig struct {
	Enabled    bool
	Valid      bool
	Reason     string
	Message    string
	Bucket     string
	Path       string
	Endpoint   string
	Schedule   string
	Retention  string
	SecretName string
}

// resolveControlDBBackupConfig validates enabled backup settings without an admission webhook. Invalid
// user configuration is returned as Reason/Message rather than an error so the owner can report it
// in status. defaultPath is the bucket prefix used when the spec leaves it empty.
func resolveControlDBBackupConfig(ctx context.Context, c client.Reader, ns string, backup oramav1alpha1.ControlDBBackupSpec, defaultPath string) (controlDBBackupConfig, error) {
	config := controlDBBackupConfig{Enabled: backup.Enabled != nil && *backup.Enabled}
	if !config.Enabled {
		return config, nil
	}
	if backup.Destination.Bucket == "" {
		config.Reason = "BackupBucketRequired"
		config.Message = "spec.components.controlDB.backup.destination.bucket is required when backups are enabled"
		return config, nil
	}
	if backup.SecretRef.Name == "" {
		config.Reason = "BackupSecretRequired"
		config.Message = "spec.components.controlDB.backup.secretRef is required when backups are enabled"
		return config, nil
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backup.SecretRef.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			config.Reason = "BackupSecretNotFound"
			config.Message = fmt.Sprintf("backup Secret %q was not found in namespace %q", backup.SecretRef.Name, ns)
			return config, nil
		}
		return config, err
	}
	for _, key := range []string{"s3ApiEndpoint", "accessKeyId", "secretAccessKey"} {
		if len(secret.Data[key]) == 0 {
			config.Reason = "InvalidBackupSecret"
			config.Message = fmt.Sprintf("backup Secret %q must contain a non-empty %s key", backup.SecretRef.Name, key)
			return config, nil
		}
	}

	config.Valid = true
	config.Bucket = backup.Destination.Bucket
	config.Path = backup.Destination.Path
	if config.Path == "" {
		config.Path = defaultPath
	}
	config.Endpoint = backup.Destination.EndpointURL
	if config.Endpoint == "" {
		config.Endpoint = string(secret.Data["s3ApiEndpoint"])
	}
	config.Schedule = backup.Schedule
	if config.Schedule == "" {
		config.Schedule = defaultControlDBBackupSchedule
	}
	config.Retention = backup.Retention
	if config.Retention == "" {
		config.Retention = defaultControlDBBackupRetention
	}
	config.SecretName = backup.SecretRef.Name
	return config, nil
}

// destinationPath is the Barman destinationPath (s3://<bucket>/<prefix>).
func (c controlDBBackupConfig) destinationPath() string {
	return fmt.Sprintf("s3://%s/%s", strings.Trim(c.Bucket, "/"), strings.Trim(c.Path, "/"))
}

// controlDBBarmanBackup renders the Cluster spec.backup block for a valid config: a Barman object store
// with gzip WAL archiving and the configured retention.
func controlDBBarmanBackup(c controlDBBackupConfig) map[string]interface{} {
	return map[string]interface{}{
		"barmanObjectStore": map[string]interface{}{
			"destinationPath": c.destinationPath(),
			"endpointURL":     c.Endpoint,
			"s3Credentials": map[string]interface{}{
				"accessKeyId":     map[string]interface{}{"name": c.SecretName, "key": "accessKeyId"},
				"secretAccessKey": map[string]interface{}{"name": c.SecretName, "key": "secretAccessKey"},
			},
			"wal": map[string]interface{}{"compression": "gzip"},
		},
		"retentionPolicy": c.Retention,
	}
}

// newControlDBScheduledBackup returns an empty ScheduledBackup addressed by name/namespace (for
// Get, Delete, and CreateOrUpdate).
func newControlDBScheduledBackup(name, ns string) *unstructured.Unstructured {
	backup := &unstructured.Unstructured{}
	backup.SetGroupVersionKind(controlDBScheduledBackupGVK)
	backup.SetName(name)
	backup.SetNamespace(ns)
	return backup
}

// controlDBScheduledBackupSpec is the ScheduledBackup spec for a Cluster: the configured schedule, an
// immediate first backup, and Backups owned by the ScheduledBackup.
func controlDBScheduledBackupSpec(c controlDBBackupConfig, clusterName string) map[string]interface{} {
	return map[string]interface{}{
		"schedule":             c.Schedule,
		"backupOwnerReference": "self",
		"cluster":              map[string]interface{}{"name": clusterName},
		"immediate":            true,
	}
}

// controlDBBackupStatus reports a BackupReady condition for a Cluster's backups: True only when the
// ScheduledBackup exists and the newest CNPG Backup completed. It also returns that Backup's
// completion time. Disabled or invalid configs report False with their reason.
func controlDBBackupStatus(ctx context.Context, c client.Reader, ns, clusterName, scheduledBackupName string, generation int64, config controlDBBackupConfig) (bool, *metav1.Time, metav1.Condition, error) {
	condition := metav1.Condition{Type: "BackupReady", ObservedGeneration: generation}
	if !config.Enabled {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupDisabled", "control-plane backups are disabled"
		return false, nil, condition, nil
	}
	if !config.Valid {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, config.Reason, config.Message
		return false, nil, condition, nil
	}

	scheduledBackup := newControlDBScheduledBackup(scheduledBackupName, ns)
	if err := c.Get(ctx, client.ObjectKeyFromObject(scheduledBackup), scheduledBackup); err != nil {
		if apierrors.IsNotFound(err) {
			condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ScheduledBackupPending", "waiting for the CNPG ScheduledBackup"
			return false, nil, condition, nil
		}
		return false, nil, condition, err
	}

	backups := &unstructured.UnstructuredList{}
	backups.SetGroupVersionKind(controlDBBackupListGVK)
	if err := c.List(ctx, backups, client.InNamespace(ns), client.MatchingLabels{"cnpg.io/cluster": clusterName}); err != nil {
		return false, nil, condition, err
	}
	if len(backups.Items) == 0 {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupPending", "waiting for the first CNPG Backup"
		return false, nil, condition, nil
	}
	newest := &backups.Items[0]
	for i := 1; i < len(backups.Items); i++ {
		if backups.Items[i].GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
			newest = &backups.Items[i]
		}
	}
	phase, _, _ := unstructured.NestedString(newest.Object, "status", "phase")
	lastBackupTime, err := controlDBBackupCompletionTime(newest)
	if err != nil {
		return false, nil, condition, err
	}
	if phase == "completed" {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "BackupCompleted", "newest CNPG Backup completed"
		return true, lastBackupTime, condition, nil
	}
	condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BackupNotCompleted", fmt.Sprintf("newest CNPG Backup phase is %q", phase)
	return false, lastBackupTime, condition, nil
}

func controlDBBackupCompletionTime(backup *unstructured.Unstructured) (*metav1.Time, error) {
	stoppedAt, found, err := unstructured.NestedString(backup.Object, "status", "stoppedAt")
	if err != nil || !found || stoppedAt == "" {
		return nil, err
	}
	parsed, err := time.Parse(time.RFC3339, stoppedAt)
	if err != nil {
		return nil, fmt.Errorf("parse CNPG Backup status.stoppedAt: %w", err)
	}
	return &metav1.Time{Time: parsed}, nil
}
