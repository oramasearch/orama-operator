/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// An OramaCloudV1 stack's datastores in the backup system (operator#740, docs/backups.md). With
// Cloudflare on and its token set, the stack owns a CloudflareR2Bucket <stack>-backups
// (deletionPolicy Retain, credentials scoped to it) and a BackupSchedule, daily at 03:00 UTC keeping
// 7, for each in-cluster datastore that holds data nothing else can rebuild:
//
//   - the control DB (Postgres, the CNPG Cluster <stack>-controldb);
//   - analytics (ClickHouse, the StatefulSet <stack>-analytics), with a connection Secret
//     <stack>-analytics-backup-connection from the stack's generated ClickHouse password.
//
// The cache (Redis) holds what the stack rebuilds, the broker (RabbitMQ) is a transport like Kafka
// (operator#733), and the engine is rebuilt from its sources; none of them gets a schedule. A late
// or failed backup is the stack's BackupsOnSchedule condition, which does not change its phase.

const (
	v1BackupsConditionType = "BackupsOnSchedule"
	v1BackupComponent      = "backup"
	v1BackupOwnerLabel     = "orama.cloud/backup-owner"
)

func v1BackupBucketName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-backups" }

// v1BackupsEnabled: the bucket needs the stack's Cloudflare account token.
func v1BackupsEnabled(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Cloudflare.Enabled && oc.Spec.Cloudflare.APITokenSecretRef != nil
}

// v1BackupTargets is what of the stack is backed up.
func v1BackupTargets(oc *oramav1alpha1.OramaCloudV1) []oramav1alpha1.BackupDatastoreRef {
	var targets []oramav1alpha1.BackupDatastoreRef
	if oc.Spec.Components.ControlDB.Enabled {
		targets = append(targets, oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastorePostgres, Name: oc.Name + "-controldb"})
	}
	analytics := oc.Spec.Components.Analytics
	if analytics.Enabled && analytics.Mode != "external" && !(analytics.Placement == "central" && oc.Spec.Region.Role == "read") {
		targets = append(targets, oramav1alpha1.BackupDatastoreRef{
			Kind: oramav1alpha1.BackupDatastoreClickHouse, Name: oc.Name + "-analytics",
			ConnectionSecretRef: &oramav1alpha1.LocalSecretReference{Name: oc.Name + "-analytics-backup-connection"},
			PodSelector:         map[string]string{"app.kubernetes.io/instance": oc.Name, "app.kubernetes.io/component": "analytics"},
		})
	}
	return targets
}

// reconcileV1Backups writes the bucket, the schedules and their connection Secrets, and deletes the
// schedules of datastores no longer backed up. Backups and the bucket's objects are never deleted
// here.
func (r *OramaCloudV1Reconciler) reconcileV1Backups(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	want := map[string]bool{}
	if v1BackupsEnabled(oc) {
		bucket := &oramav1alpha1.CloudflareR2Bucket{ObjectMeta: metav1.ObjectMeta{Name: v1BackupBucketName(oc), Namespace: oc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bucket, func() error {
			bucket.Spec.SecretRef = oc.Spec.Cloudflare.APITokenSecretRef.Name
			bucket.Spec.Name = v1BackupBucketName(oc)
			bucket.Spec.DeletionPolicy = oramav1alpha1.BucketDeletionRetain
			bucket.Spec.Credentials = &oramav1alpha1.CloudflareR2BucketCredentials{SecretName: v1BackupBucketName(oc) + "-s3"}
			return controllerutil.SetControllerReference(oc, bucket, r.Scheme)
		}); err != nil {
			return err
		}
		for _, target := range v1BackupTargets(oc) {
			if target.ConnectionSecretRef != nil {
				if err := r.ensureV1BackupConnection(ctx, oc, target.ConnectionSecretRef.Name); err != nil {
					return err
				}
			}
			schedule := &oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: target.Name + "-backup", Namespace: oc.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, schedule, func() error {
				schedule.Labels = map[string]string{v1BackupOwnerLabel: oc.Name, "app.kubernetes.io/component": v1BackupComponent}
				schedule.Spec = oramav1alpha1.BackupScheduleSpec{
					Datastore: target,
					Bucket:    oramav1alpha1.BackupBucketRef{Kind: "CloudflareR2Bucket", Name: v1BackupBucketName(oc)},
					Schedule:  backupScheduleDefaultCron,
					Keep:      backupScheduleDefaultKeep,
				}
				return controllerutil.SetControllerReference(oc, schedule, r.Scheme)
			}); err != nil {
				return err
			}
			want[schedule.Name] = true
		}
	}
	schedules, err := r.v1BackupSchedules(ctx, oc)
	if err != nil {
		return err
	}
	for i := range schedules {
		if !want[schedules[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &schedules[i])); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureV1BackupConnection writes the analytics connection in the shape a backup Job reads: the
// ClickHouse HTTP interface of <stack>-analytics as the default user with the stack's password.
func (r *OramaCloudV1Reconciler) ensureV1BackupConnection(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string) error {
	var auth corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: clickhouseAuthSecretName(oc)}, &auth); err != nil {
		return client.IgnoreNotFound(err) // generated by the bootstrap step; the next pass writes it
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labelsFor(oc, v1BackupComponent)
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			"host":     []byte(fmt.Sprintf("%s-analytics.%s.svc.cluster.local", oc.Name, oc.Namespace)),
			"port":     []byte("8123"),
			"username": []byte("default"),
			"password": append([]byte(nil), auth.Data["CLICKHOUSE_PASSWORD"]...),
		}
		return controllerutil.SetControllerReference(oc, secret, r.Scheme)
	})
	return err
}

func (r *OramaCloudV1Reconciler) v1BackupSchedules(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]oramav1alpha1.BackupSchedule, error) {
	var list oramav1alpha1.BackupScheduleList
	if err := r.List(ctx, &list, client.InNamespace(oc.Namespace), client.MatchingLabels{v1BackupOwnerLabel: oc.Name}); err != nil {
		return nil, err
	}
	owned := list.Items[:0]
	for _, schedule := range list.Items {
		if metav1.IsControlledBy(&schedule, oc) {
			owned = append(owned, schedule)
		}
	}
	return owned, nil
}

// v1BackupsCondition is the stack's BackupsOnSchedule condition.
func (r *OramaCloudV1Reconciler) v1BackupsCondition(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (metav1.Condition, error) {
	condition := metav1.Condition{Type: v1BackupsConditionType}
	if !v1BackupsEnabled(oc) {
		condition.Status, condition.Reason = metav1.ConditionFalse, "NotConfigured"
		condition.Message = "spec.cloudflare is off or has no apiTokenSecretRef: there is no backup bucket, so nothing is backed up"
		return condition, nil
	}
	return backupSchedulesCondition(condition, func() ([]oramav1alpha1.BackupSchedule, error) { return r.v1BackupSchedules(ctx, oc) })
}
