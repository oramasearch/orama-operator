/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A Lake's managed services in the backup system (operator#740, docs/lake/backups.md). With
// spec.backups the Lake owns:
//
//   - a CloudflareR2Bucket lake-<name>-backups in its namespace, deletionPolicy Retain, with
//     credentials scoped to it and allowed for every workspace namespace that has a schedule;
//   - per managed service, in the service's workspace namespace, a BackupSchedule
//     <datastore>-backup and, for a kind whose backup Job talks to the server, a connection Secret
//     <datastore>-backup-connection in the shape the Job reads (host, port, username, password,
//     database), derived from the service's connection document.
//
// The connection comes from the services Secret the Lake already writes, which comes from lake's
// control Postgres (decision 51: the operator talks only to Postgres). A schedule that is not
// ready, a failed newest backup or a late one is appended to the service's service_status message
// and summarised in the Lake's BackupsOnSchedule condition. Deleting a service deletes its schedule
// and connection Secret; its Backups are only labelled, so they and their objects stay.

const (
	lakeBackupsConditionType = "BackupsOnSchedule"
	lakeBackupComponent      = "backup"
	// lakeBackupNoteLimit bounds what the backup state adds to a service_status message.
	lakeBackupNoteLimit = 400
)

func lakeBackupBucketName(lake string) string        { return "lake-" + lake + "-backups" }
func lakeBackupCredentialsSecret(lake string) string { return lakeBackupBucketName(lake) + "-s3" }
func lakeBackupScheduleName(datastore string) string { return datastore + "-backup" }
func lakeBackupConnectionSecret(datastore string) string {
	return lakeBackupScheduleName(datastore) + "-connection"
}

// lakeBackupTarget is one datastore of a service that gets a BackupSchedule.
type lakeBackupTarget struct {
	kind oramav1alpha1.BackupDatastoreKind
	// name is the datastore's name in its namespace: the upstream resource, the CNPG Cluster, the
	// OramaZettaCluster or the volume claim.
	name string
	// connection: the kind's backup Job reads the server's connection.
	connection  bool
	podSelector map[string]string
	volume      *oramav1alpha1.BackupVolumeSource
}

// lakeServiceBackupTargets is what of a service is backed up, by kind.
func lakeServiceBackupTargets(lake *oramav1alpha1.Lake, svc *LakeService) []lakeBackupTarget {
	name := serviceResourceName(lake.Name, svc.Slug)
	in := catalogService{lake: lake, svc: svc, namespace: svc.WorkspaceNamespace, name: name}
	switch svc.Kind {
	case serviceKindZetta:
		if svc.WorkspaceZetta {
			name = workspaceZettaClusterName(lake, svc.WorkspaceID)
		}
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreZetta, name: name, connection: true}}
	case serviceKindPostgres:
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastorePostgres, name: name}}
	case serviceKindClickHouse:
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreClickHouse, name: name, connection: true, podSelector: clickHouseServiceKind{}.podSelector(in)}}
	case serviceKindTiDB:
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreTiDB, name: name, podSelector: map[string]string{"app.kubernetes.io/instance": name}}}
	case serviceKindCassandra:
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreCassandra, name: name, podSelector: cassandraServiceKind{}.podSelector(in)}}
	case serviceKindValkey:
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreValkey, name: name, connection: true, podSelector: valkeyServiceKind{}.podSelector(in)}}
	case serviceKindSQLite, serviceKindDuckDB, serviceKindPolars:
		// Each data claim data-<name>-<i>, copied from a CSI snapshot as the wrapper's own user
		// (operator#737). Polars keeps datasets per worker, so every worker's claim has a schedule.
		kind := map[string]oramav1alpha1.BackupDatastoreKind{serviceKindSQLite: oramav1alpha1.BackupDatastoreSQLite,
			serviceKindDuckDB: oramav1alpha1.BackupDatastoreDuckDB, serviceKindPolars: oramav1alpha1.BackupDatastorePolars}[svc.Kind]
		instances := int32(1)
		if wrapper, _ := catalogKinds[svc.Kind].(wrapperServiceKind); wrapper.multiInstance {
			if spec, err := parseServiceSpec(svc.Spec); err == nil && spec.Instances > 0 {
				instances = spec.Instances
			}
		}
		user := wrapperRunAsUser
		var targets []lakeBackupTarget
		for i := int32(0); i < instances; i++ {
			targets = append(targets, lakeBackupTarget{kind: kind, name: fmt.Sprintf("%s-%s-%d", wrapperDataVolume, name, i),
				volume: &oramav1alpha1.BackupVolumeSource{VolumeSnapshotClassName: snapshotDefaultClass, RunAsUser: &user, RunAsGroup: &user}})
		}
		return targets
	case serviceKindKafka:
		// The schedule states Kafka's decision (operator#733) instead of running.
		return []lakeBackupTarget{{kind: oramav1alpha1.BackupDatastoreKafka, name: name}}
	}
	return nil
}

// reconcileServiceBackup writes a present, ready service's schedules (and connection Secrets), and
// returns what its backups say that the service_status message should carry: empty when every
// schedule is ready, on schedule and its newest backup succeeded.
func (r *LakeReconciler) reconcileServiceBackup(ctx context.Context, lake *oramav1alpha1.Lake, svc *LakeService, credential []byte) (string, error) {
	if lake.Spec.Backups == nil || svc.WorkspaceNamespace == "" {
		return "", nil
	}
	var document map[string]interface{}
	if len(credential) > 0 {
		if err := json.Unmarshal(credential, &document); err != nil {
			return "", fmt.Errorf("read the connection of service %s: %w", svc.ID, err)
		}
	}
	var notes []string
	for _, target := range lakeServiceBackupTargets(lake, svc) {
		schedule, err := r.ensureLakeBackupSchedule(ctx, lake, svc, target, document)
		if err != nil {
			return "", err
		}
		if note := backupScheduleNote(schedule); note != "" {
			notes = append(notes, note)
		}
	}
	note := strings.Join(notes, "; ")
	if len(note) > lakeBackupNoteLimit {
		note = note[:lakeBackupNoteLimit] + "…"
	}
	return note, nil
}

func (r *LakeReconciler) ensureLakeBackupSchedule(ctx context.Context, lake *oramav1alpha1.Lake, svc *LakeService, target lakeBackupTarget, document map[string]interface{}) (*oramav1alpha1.BackupSchedule, error) {
	namespace := svc.WorkspaceNamespace
	datastore := oramav1alpha1.BackupDatastoreRef{Kind: target.kind, Name: target.name, PodSelector: target.podSelector, Volume: target.volume}
	if target.connection {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeBackupConnectionSecret(target.name), Namespace: namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, lake) {
				return fmt.Errorf("Secret %s/%s exists and is not this Lake's", namespace, secret.Name)
			}
			setServiceMetadata(secret, lake, svc)
			secret.Labels[serviceComponentLabel] = lakeBackupComponent
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = backupConnectionData(document)
			return controllerutil.SetControllerReference(lake, secret, r.Scheme)
		}); err != nil {
			return nil, err
		}
		datastore.ConnectionSecretRef = &oramav1alpha1.LocalSecretReference{Name: secret.Name}
	}
	schedule := &oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: lakeBackupScheduleName(target.name), Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, schedule, func() error {
		if schedule.ResourceVersion != "" && !metav1.IsControlledBy(schedule, lake) {
			return fmt.Errorf("BackupSchedule %s/%s exists and is not this Lake's", namespace, schedule.Name)
		}
		setServiceMetadata(schedule, lake, svc)
		schedule.Labels[serviceComponentLabel] = lakeBackupComponent
		keep := lake.Spec.Backups.Keep
		if keep < 1 {
			keep = backupScheduleDefaultKeep
		}
		cron := lake.Spec.Backups.Schedule
		if cron == "" {
			cron = backupScheduleDefaultCron
		}
		schedule.Spec = oramav1alpha1.BackupScheduleSpec{
			Datastore: datastore,
			Bucket:    oramav1alpha1.BackupBucketRef{Kind: "CloudflareR2Bucket", Name: lakeBackupBucketName(lake.Name), Namespace: lakeNamespace(lake)},
			Schedule:  cron,
			Keep:      keep,
		}
		return controllerutil.SetControllerReference(lake, schedule, r.Scheme)
	})
	return schedule, err
}

// backupScheduleNote is what a schedule's status says that its owner should show: not ready, the
// newest backup failed, or late. Empty when all is well, so a steady service writes nothing.
func backupScheduleNote(schedule *oramav1alpha1.BackupSchedule) string {
	conditions := schedule.Status.Conditions
	if c := apimeta.FindStatusCondition(conditions, backupScheduleConditionReady); c != nil && c.Status == metav1.ConditionFalse {
		return fmt.Sprintf("no backup (%s): %s", c.Reason, c.Message)
	}
	if c := apimeta.FindStatusCondition(conditions, backupScheduleConditionLast); c != nil && c.Status == metav1.ConditionFalse {
		return fmt.Sprintf("backup failed (%s): %s", c.Reason, c.Message)
	}
	if c := apimeta.FindStatusCondition(conditions, backupScheduleConditionOnSched); c != nil && c.Status == metav1.ConditionFalse {
		return fmt.Sprintf("backup late: %s", c.Message)
	}
	return ""
}

// lakeBackupSchedules lists every BackupSchedule this Lake owns, in every namespace.
func (r *LakeReconciler) lakeBackupSchedules(ctx context.Context, lake *oramav1alpha1.Lake) ([]oramav1alpha1.BackupSchedule, error) {
	var list oramav1alpha1.BackupScheduleList
	if err := r.List(ctx, &list, client.MatchingLabels{serviceLakeLabel: lake.Name, serviceComponentLabel: lakeBackupComponent}); err != nil {
		return nil, err
	}
	owned := list.Items[:0]
	for _, schedule := range list.Items {
		if metav1.IsControlledBy(&schedule, lake) {
			owned = append(owned, schedule)
		}
	}
	return owned, nil
}

// sweepLakeBackups deletes the schedules and connection Secrets of services that are no longer
// present (keep holds the present service ids), or every one when spec.backups is unset, then
// writes the bucket with the workspace namespaces that still have schedules. The Backups stay.
func (r *LakeReconciler) sweepLakeBackups(ctx context.Context, lake *oramav1alpha1.Lake, keep map[string]bool) error {
	schedules, err := r.lakeBackupSchedules(ctx, lake)
	if err != nil {
		return err
	}
	namespaces := map[string]bool{}
	for i := range schedules {
		schedule := &schedules[i]
		if lake.Spec.Backups != nil && keep[schedule.Labels[serviceIDLabel]] {
			if schedule.Namespace != lakeNamespace(lake) {
				namespaces[schedule.Namespace] = true
			}
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, schedule)); err != nil {
			return err
		}
		if ref := schedule.Spec.Datastore.ConnectionSecretRef; ref != nil {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: schedule.Namespace}}
			if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil && metav1.IsControlledBy(secret, lake) {
				if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
					return err
				}
			} else if client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return r.reconcileLakeBackupBucket(ctx, lake, sortedKeys(namespaces))
}

// reconcileLakeBackupBucket keeps the Lake's backup bucket resource, or deletes it when
// spec.backups is unset: the bucket and every backup in it stay (deletionPolicy Retain), and
// setting spec.backups again adopts it.
func (r *LakeReconciler) reconcileLakeBackupBucket(ctx context.Context, lake *oramav1alpha1.Lake, allowedNamespaces []string) error {
	namespace := lakeNamespace(lake)
	bucket := &oramav1alpha1.CloudflareR2Bucket{ObjectMeta: metav1.ObjectMeta{Name: lakeBackupBucketName(lake.Name), Namespace: namespace}}
	if lake.Spec.Backups == nil {
		if err := r.Get(ctx, client.ObjectKeyFromObject(bucket), bucket); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(bucket, lake) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, bucket))
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, bucket, func() error {
		if bucket.ResourceVersion != "" && !metav1.IsControlledBy(bucket, lake) {
			return fmt.Errorf("CloudflareR2Bucket %s/%s exists and is not this Lake's", namespace, bucket.Name)
		}
		if bucket.Labels == nil {
			bucket.Labels = map[string]string{}
		}
		bucket.Labels[serviceLakeLabel] = lake.Name
		bucket.Labels[serviceComponentLabel] = lakeBackupComponent
		bucket.Spec.SecretRef = lake.Spec.Backups.CloudflareSecretRef.Name
		bucket.Spec.Name = lakeBackupBucketName(lake.Name)
		// Backups outlive the Lake, the workspace and the resource (operator#453).
		bucket.Spec.DeletionPolicy = oramav1alpha1.BucketDeletionRetain
		bucket.Spec.Credentials = &oramav1alpha1.CloudflareR2BucketCredentials{
			SecretName:        lakeBackupCredentialsSecret(lake.Name),
			AllowedNamespaces: allowedNamespaces,
		}
		return controllerutil.SetControllerReference(lake, bucket, r.Scheme)
	})
	return err
}

// lakeBackupsCondition summarises the Lake's backups: whether they are configured, whether the
// bucket is ready, and every schedule that is not ready, failed or late (the first few named).
func (r *LakeReconciler) lakeBackupsCondition(ctx context.Context, lake *oramav1alpha1.Lake) (metav1.Condition, error) {
	condition := metav1.Condition{Type: lakeBackupsConditionType, ObservedGeneration: lake.Generation}
	if lake.Spec.Backups == nil {
		condition.Status, condition.Reason = metav1.ConditionFalse, "NotConfigured"
		condition.Message = "spec.backups is not set: no managed service is backed up"
		return condition, nil
	}
	var bucket oramav1alpha1.CloudflareR2Bucket
	if err := r.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeBackupBucketName(lake.Name)}, &bucket); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return condition, err
		}
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BucketNotReady", "the backup bucket resource is not written yet"
		return condition, nil
	}
	if !bucket.Status.Ready {
		message := bucket.Status.Message
		if message == "" {
			message = "not reconciled yet"
		}
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BucketNotReady", "the backup bucket is not ready: "+message
		return condition, nil
	}
	return backupSchedulesCondition(condition, func() ([]oramav1alpha1.BackupSchedule, error) { return r.lakeBackupSchedules(ctx, lake) })
}

// backupSchedulesCondition fills an owner's condition from its schedules: True when every one is
// ready (or refused by decision), on schedule and its newest backup did not fail.
func backupSchedulesCondition(condition metav1.Condition, list func() ([]oramav1alpha1.BackupSchedule, error)) (metav1.Condition, error) {
	schedules, err := list()
	if err != nil {
		return condition, err
	}
	var problems []string
	reason := ""
	for i := range schedules {
		note := backupScheduleNote(&schedules[i])
		if note == "" || isBackupDecisionRefusal(&schedules[i]) {
			continue
		}
		problems = append(problems, schedules[i].Namespace+"/"+schedules[i].Name+": "+note)
		switch {
		case strings.HasPrefix(note, "backup failed"):
			reason = "BackupFailed"
		case strings.HasPrefix(note, "backup late") && reason != "BackupFailed":
			reason = "BackupLate"
		case reason == "":
			reason = "ScheduleNotReady"
		}
	}
	sort.Strings(problems)
	if len(problems) == 0 {
		condition.Status, condition.Reason = metav1.ConditionTrue, "OnSchedule"
		condition.Message = fmt.Sprintf("%d backup schedules on schedule", len(schedules))
		return condition, nil
	}
	condition.Status, condition.Reason = metav1.ConditionFalse, reason
	shown := problems
	if len(shown) > 3 {
		shown = shown[:3]
	}
	condition.Message = fmt.Sprintf("%d of %d schedules need attention: %s", len(problems), len(schedules), strings.Join(shown, "; "))
	if len(condition.Message) > 1024 {
		condition.Message = condition.Message[:1024] + "…"
	}
	return condition, nil
}

// isBackupDecisionRefusal: a schedule refused because its kind has no backup by decision (Kafka,
// operator#733). It is stated on the service, but is not a problem of the owner's backups.
func isBackupDecisionRefusal(schedule *oramav1alpha1.BackupSchedule) bool {
	c := apimeta.FindStatusCondition(schedule.Status.Conditions, backupScheduleConditionReady)
	return c != nil && c.Reason == backupReasonNoBackupByDecision
}

// backupReasonNoBackupByDecision is the Ready reason of a kind with no backup by decision.
const backupReasonNoBackupByDecision = "NoBackupByDecision"

// lakeStoreBackupID is the service-id label of the store's schedule, which no service row has.
const lakeStoreBackupID = "lake-store"

// reconcileLakeStoreBackup keeps the BackupSchedule of the Lake's store volume (operator#738): a
// file-level copy, from a CSI snapshot, to the Lake's backup bucket, as lake's own user so every file
// can be read. The store's CSI snapshots (lake_volume_snapshots.go) stay as they are; this is the copy
// off the cluster's disks.
func (r *LakeReconciler) reconcileLakeStoreBackup(ctx context.Context, lake *oramav1alpha1.Lake, namespace, claim string) error {
	if claim == "" {
		return nil
	}
	schedule := &oramav1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: lakeBackupScheduleName(claim), Namespace: namespace}}
	if lake.Spec.Backups == nil {
		if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), schedule); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(schedule, lake) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, schedule))
	}
	user, group := lakeRunAsUser, lakeRunAsGroup
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, schedule, func() error {
		if schedule.ResourceVersion != "" && !metav1.IsControlledBy(schedule, lake) {
			return fmt.Errorf("BackupSchedule %s/%s exists and is not this Lake's", namespace, schedule.Name)
		}
		schedule.Labels = map[string]string{serviceLakeLabel: lake.Name, serviceComponentLabel: lakeBackupComponent, serviceIDLabel: lakeStoreBackupID}
		keep := lake.Spec.Backups.Keep
		if keep < 1 {
			keep = backupScheduleDefaultKeep
		}
		cron := lake.Spec.Backups.Schedule
		if cron == "" {
			cron = backupScheduleDefaultCron
		}
		schedule.Spec = oramav1alpha1.BackupScheduleSpec{
			Datastore: oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastoreVolume, Name: claim,
				Volume: &oramav1alpha1.BackupVolumeSource{VolumeSnapshotClassName: snapshotDefaultClass, RunAsUser: &user, RunAsGroup: &group}},
			Bucket:   oramav1alpha1.BackupBucketRef{Kind: "CloudflareR2Bucket", Name: lakeBackupBucketName(lake.Name), Namespace: namespace},
			Schedule: cron,
			Keep:     keep,
		}
		return controllerutil.SetControllerReference(lake, schedule, r.Scheme)
	})
	return err
}
