/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/robfig/cron/v3"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Postgres in the backup system (operator#731), through CloudNativePG's plugin interface (CNPG-I)
// and the Barman Cloud Plugin (cloudnative-pg/plugin-barman-cloud, Apache-2.0; it runs Barman,
// GPL-3.0, in its own sidecar image, unmodified).
//
// Decided under Steve's 2026-11-14 delegation: the Barman Cloud Plugin, because it is CloudNativePG's
// own maintained replacement for the in-tree barmanObjectStore (removed in 1.31.0), it keeps every
// existing backup readable (the same Barman layout), and a plugin of our own on pgBackRest or WAL-G
// would be a new private repository, image and maintenance burden. GPL-3.0 applies to the Barman
// image, which we ship unmodified as its own container; its notice and source go with any install we
// distribute (docs/backups.md, "Postgres").
//
// Continuous WAL archiving runs in the plugin's sidecar beside Postgres, so a Postgres backup is not a
// Job: the BackupSchedule owns the plugin's archive configuration (a barmancloud.cnpg.io ObjectStore
// <schedule>-archive pointing at <bucket>/<path>, with a copy of the bucket's scoped credentials, and a
// NetworkPolicy letting the Cluster's pods reach public HTTPS, both lasting as long as the schedule);
// the owner of the CNPG Cluster names that ObjectStore in the Cluster's spec.plugins; and each Backup
// asks CloudNativePG for a base backup (Backup, method plugin) and reads its outcome. Retention is
// Barman's: the ObjectStore keeps a recovery window of keep ticks, and deleting a Backup deletes the
// CNPG Backup resource, never Barman's files. A restore bootstraps a new Cluster from the archive.

const (
	barmanCloudPlugin     = "barman-cloud.cloudnative-pg.io"
	postgresArchiveSuffix = "-archive"
	postgresBackupIDAnnot = "orama.cloud/postgres-backup-id"
	postgresArchiveAnnot  = "orama.cloud/postgres-archive"
	postgresServerAnnot   = "orama.cloud/postgres-server"
	postgresPoll          = 30 * time.Second
)

var barmanObjectStoreGVK = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}

// +kubebuilder:rbac:groups=barmancloud.cnpg.io,resources=objectstores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete

// postgresArchiveName is the ObjectStore a schedule owns; an owner names it in its Cluster.
func postgresArchiveName(schedule string) string { return schedule + postgresArchiveSuffix }

// barmanArchivePlugins is the spec.plugins of a Cluster that archives its WAL to the named
// ObjectStore.
func barmanArchivePlugins(objectStore string) []interface{} {
	return []interface{}{map[string]interface{}{
		"name":          barmanCloudPlugin,
		"isWALArchiver": true,
		"parameters":    map[string]interface{}{"barmanObjectName": objectStore},
	}}
}

// barmanArchivePluginInstalled reports whether the Barman Cloud Plugin's ObjectStore kind exists on
// the cluster. Until its chart is installed (helmfile.yaml and every platform.yaml, docs/backups.md
// "Platform"), owners leave their Clusters as they are, because a Cluster naming a plugin that is
// not running does not archive.
// It is a variable so tests, whose fake clients know no CRD, can say the plugin is installed.
var barmanArchivePluginInstalled = func(c client.Client) bool {
	_, err := c.RESTMapper().RESTMapping(barmanObjectStoreGVK.GroupKind(), barmanObjectStoreGVK.Version)
	return err == nil
}

// newBarmanObjectStore is the ObjectStore for a destination: every WAL segment and base backup goes
// under destinationPath, gzip-compressed, signed with the keys in credentialsSecret.
func newBarmanObjectStore(namespace, name, destinationPath, endpoint, credentialsSecret, retention string) *unstructured.Unstructured {
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(barmanObjectStoreGVK)
	store.SetNamespace(namespace)
	store.SetName(name)
	store.Object["spec"] = barmanObjectStoreSpec(destinationPath, endpoint, credentialsSecret, retention)
	return store
}

func barmanObjectStoreSpec(destinationPath, endpoint, credentialsSecret, retention string) map[string]interface{} {
	spec := map[string]interface{}{
		"configuration": map[string]interface{}{
			"destinationPath": destinationPath,
			"endpointURL":     endpoint,
			"s3Credentials": map[string]interface{}{
				"accessKeyId":     map[string]interface{}{"name": credentialsSecret, "key": "accessKeyId"},
				"secretAccessKey": map[string]interface{}{"name": credentialsSecret, "key": "secretAccessKey"},
			},
			"wal":  map[string]interface{}{"compression": "gzip"},
			"data": map[string]interface{}{"compression": "gzip"},
		},
	}
	if retention != "" {
		spec["retentionPolicy"] = retention
	}
	return spec
}

// postgresRetention is Barman's recovery window for a schedule: keep ticks of its cron, in whole
// days, at least one.
func postgresRetention(cronText string, keep int32) string {
	if keep < 1 {
		keep = backupScheduleDefaultKeep
	}
	days := int64(keep)
	if schedule, err := cron.ParseStandard(cronText); err == nil {
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		first := schedule.Next(start)
		interval := schedule.Next(first).Sub(first)
		days = int64((time.Duration(keep)*interval + 24*time.Hour - 1) / (24 * time.Hour))
	}
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("%dd", days)
}

// ensurePostgresArchive writes a Postgres schedule's archive: the bucket credentials copy, the
// ObjectStore and the egress of the Cluster's pods. It returns a reason and message when it cannot
// yet, for the schedule's Ready condition.
func (r *BackupScheduleReconciler) ensurePostgresArchive(ctx context.Context, s *oramav1alpha1.BackupSchedule, cronText string) (string, string, error) {
	if !barmanArchivePluginInstalled(r.Client) {
		return "ArchivePluginNotInstalled", "the Barman Cloud Plugin (barmancloud.cnpg.io ObjectStore) is not installed on this cluster: see docs/backups.md, Platform", nil
	}
	bucket, reason, message, err := resolveBackupBucket(ctx, r.Client, s.Namespace, s.Spec.Bucket)
	if err != nil || reason != "" {
		return reason, message, err
	}
	name := postgresArchiveName(s.Name)
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-s3", Namespace: s.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, credentials, func() error {
		if credentials.ResourceVersion != "" && !metav1.IsControlledBy(credentials, s) {
			return fmt.Errorf("Secret %s/%s exists and is not this schedule's", s.Namespace, credentials.Name)
		}
		credentials.Labels = map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag, backupScheduleLabel: dnsLabelValue(s.Name)}
		credentials.Type = corev1.SecretTypeOpaque
		credentials.Data = map[string][]byte{
			"accessKeyId":     append([]byte(nil), bucket.Data["accessKeyId"]...),
			"secretAccessKey": append([]byte(nil), bucket.Data["secretAccessKey"]...),
		}
		return controllerutil.SetControllerReference(s, credentials, r.Scheme)
	}); err != nil {
		return "", "", err
	}
	destination := fmt.Sprintf("s3://%s/%s/", string(bucket.Data["bucket"]), backupPrefix(s.Namespace, s.Spec.Datastore, s.Spec.Path))
	store := newBarmanObjectStore(s.Namespace, name, destination, string(bucket.Data["endpoint"]), credentials.Name, postgresRetention(cronText, s.Spec.Keep))
	desiredSpec := store.Object["spec"]
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, store, func() error {
		if store.GetResourceVersion() != "" && !metav1.IsControlledBy(store, s) {
			return fmt.Errorf("ObjectStore %s/%s exists and is not this schedule's", s.Namespace, name)
		}
		store.Object["spec"] = desiredSpec
		return controllerutil.SetControllerReference(s, store, r.Scheme)
	}); err != nil {
		return "", "", err
	}
	// The sidecar beside every Postgres instance ships each WAL segment as it is written, so the
	// Cluster's pods reach public HTTPS for as long as the schedule exists.
	tcp, https := corev1.ProtocolTCP, intstr.FromInt32(443)
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name + "-egress", Namespace: s.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		if policy.ResourceVersion != "" && !metav1.IsControlledBy(policy, s) {
			return fmt.Errorf("NetworkPolicy %s/%s exists and is not this schedule's", s.Namespace, policy.Name)
		}
		policy.Labels = map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag, backupScheduleLabel: dnsLabelValue(s.Name)}
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: datastorePodSelector(s.Spec.Datastore)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: publicHTTPSExcept}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
			}},
		}
		return controllerutil.SetControllerReference(s, policy, r.Scheme)
	})
	return "", "", err
}

// backupDriver backs up a kind through its own operator's resources instead of a Job.
type backupDriver interface {
	backup(ctx context.Context, r *BackupReconciler, b *oramav1alpha1.Backup, bucket *corev1.Secret, prefix string) (backupOutcome, error)
}

// backupOutcome is where a driver's backup is.
type backupOutcome struct {
	runOutcome
	location string
	size     int64
	// annotations are recorded on the Backup when it succeeds (what a restore needs).
	annotations map[string]string
}

// backupDrivers are the kinds backed up through their own operator. Their objects in the bucket
// belong to that operator's retention, so deleting a Backup deletes only the resource.
var backupDrivers = map[oramav1alpha1.BackupDatastoreKind]backupDriver{
	oramav1alpha1.BackupDatastorePostgres: postgresDriver{},
}

func init() {
	restoreDrivers[oramav1alpha1.BackupDatastorePostgres] = postgresDriver{}
}

type postgresDriver struct{}

// clusterArchive is the ObjectStore a Cluster archives its WAL to, from its spec.plugins.
func clusterArchive(cluster *unstructured.Unstructured) string {
	plugins, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	for _, item := range plugins {
		plugin, _ := item.(map[string]interface{})
		if plugin["name"] != barmanCloudPlugin {
			continue
		}
		if archiver, _ := plugin["isWALArchiver"].(bool); !archiver {
			continue
		}
		name, _, _ := unstructured.NestedString(plugin, "parameters", "barmanObjectName")
		return name
	}
	return ""
}

func (postgresDriver) backup(ctx context.Context, r *BackupReconciler, b *oramav1alpha1.Backup, bucket *corev1.Secret, _ string) (backupOutcome, error) {
	cluster := newCNPGCluster(b.Namespace, b.Spec.Datastore.Name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		if apierrors.IsNotFound(err) || isKindNotInstalled(err) {
			return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("CloudNativePG Cluster %s does not exist", b.Spec.Datastore.Name), requeue: backupWaitPoll}}, nil
		}
		return backupOutcome{}, err
	}
	archive := clusterArchive(cluster)
	if archive == "" {
		return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("Cluster %s does not archive through the Barman Cloud Plugin yet (spec.plugins); its owner names the schedule's ObjectStore", cluster.GetName()), requeue: backupWaitPoll}}, nil
	}
	cnpg := &unstructured.Unstructured{}
	cnpg.SetGroupVersionKind(cnpgBackupGVK)
	cnpg.SetNamespace(b.Namespace)
	cnpg.SetName(dnsLabel(b.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(cnpg), cnpg); apierrors.IsNotFound(err) {
		cnpg.SetLabels(map[string]string{backupNameLabel: dnsLabelValue(b.Name)})
		cnpg.Object["spec"] = map[string]interface{}{
			"cluster":             map[string]interface{}{"name": cluster.GetName()},
			"method":              "plugin",
			"pluginConfiguration": map[string]interface{}{"name": barmanCloudPlugin},
		}
		if err := controllerutil.SetControllerReference(b, cnpg, r.Scheme); err != nil {
			return backupOutcome{}, err
		}
		if err := r.Create(ctx, cnpg); err != nil && !apierrors.IsAlreadyExists(err) {
			return backupOutcome{}, err
		}
		return backupOutcome{runOutcome: runOutcome{message: "CloudNativePG Backup " + cnpg.GetName() + " requested", requeue: postgresPoll}}, nil
	} else if err != nil {
		return backupOutcome{}, err
	}
	phase, _, _ := unstructured.NestedString(cnpg.Object, "status", "phase")
	switch phase {
	case "completed":
		id, _, _ := unstructured.NestedString(cnpg.Object, "status", "backupId")
		server, _, _ := unstructured.NestedString(cnpg.Object, "status", "serverName")
		if server == "" {
			server = cluster.GetName()
		}
		location := fmt.Sprintf("s3://%s/%s/%s/base/%s/", string(bucket.Data["bucket"]), backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path), server, id)
		return backupOutcome{
			runOutcome:  runOutcome{done: true, succeeded: true, reason: "BackupSucceeded", message: fmt.Sprintf("CloudNativePG base backup %s of %s completed; WAL is archived continuously to %s", id, cluster.GetName(), archive)},
			location:    location,
			annotations: map[string]string{postgresBackupIDAnnot: id, postgresArchiveAnnot: archive, postgresServerAnnot: server},
		}, nil
	case "failed":
		message, _, _ := unstructured.NestedString(cnpg.Object, "status", "error")
		return backupOutcome{runOutcome: runOutcome{done: true, reason: "BackupFailed", message: "CloudNativePG Backup failed: " + message}}, nil
	}
	if phase == "" {
		phase = "pending"
	}
	return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("CloudNativePG Backup %s is %s", cnpg.GetName(), phase), requeue: postgresPoll}}, nil
}

// restore bootstraps target.name, a new Cluster, from the backup's base backup and the archive's WAL
// up to that backup's consistent point. The new Cluster archives nowhere (no spec.plugins), so it
// never writes into the source's archive. It is labelled with the restore; a scratch one is deleted
// afterwards. Verification is CloudNativePG's: the Cluster comes up only once the base backup and
// every WAL segment up to its end have been read back and replayed.
func (postgresDriver) restore(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup, _ *corev1.Secret) (runOutcome, error) {
	id, archive, server := b.Annotations[postgresBackupIDAnnot], b.Annotations[postgresArchiveAnnot], b.Annotations[postgresServerAnnot]
	if id == "" || archive == "" || server == "" {
		return runOutcome{done: true, reason: "Refused", message: fmt.Sprintf("Backup %s does not record its CloudNativePG backup id and archive", b.Name)}, nil
	}
	target := newCNPGCluster(rs.Namespace, rs.Spec.Target.Name)
	err := r.Get(ctx, client.ObjectKeyFromObject(target), target)
	switch {
	case apierrors.IsNotFound(err):
		source := newCNPGCluster(b.Namespace, b.Spec.Datastore.Name)
		size, image := "10Gi", ""
		if r.Get(ctx, client.ObjectKeyFromObject(source), source) == nil {
			if value, _, _ := unstructured.NestedString(source.Object, "spec", "storage", "size"); value != "" {
				size = value
			}
			image, _, _ = unstructured.NestedString(source.Object, "spec", "imageName")
		}
		target.SetLabels(map[string]string{backupRestoreLabel: dnsLabelValue(rs.Name), "app.kubernetes.io/managed-by": backupOperatorManagedTag})
		spec := map[string]interface{}{
			"instances": int64(1),
			"storage":   map[string]interface{}{"size": size},
			"bootstrap": map[string]interface{}{"recovery": map[string]interface{}{
				"source":         "origin",
				"recoveryTarget": map[string]interface{}{"backupID": id, "targetImmediate": true},
			}},
			"externalClusters": []interface{}{map[string]interface{}{
				"name": "origin",
				"plugin": map[string]interface{}{
					"name":       barmanCloudPlugin,
					"parameters": map[string]interface{}{"barmanObjectName": archive, "serverName": server},
				},
			}},
		}
		if image != "" {
			spec["imageName"] = image
		}
		target.Object["spec"] = spec
		if err := r.Create(ctx, target); err != nil && !apierrors.IsAlreadyExists(err) {
			return runOutcome{}, err
		}
		return runOutcome{message: "recovering Cluster " + target.GetName() + " from base backup " + id, requeue: postgresPoll}, nil
	case err != nil:
		return runOutcome{}, err
	}
	if target.GetLabels()[backupRestoreLabel] != dnsLabelValue(rs.Name) {
		return runOutcome{done: true, reason: "Refused", message: fmt.Sprintf("Cluster %s already exists: a Postgres restore creates its target", target.GetName())}, nil
	}
	ready, _, _ := unstructured.NestedInt64(target.Object, "status", "readyInstances")
	phase, _, _ := unstructured.NestedString(target.Object, "status", "phase")
	if ready < 1 {
		return runOutcome{message: fmt.Sprintf("Cluster %s is recovering: %s", target.GetName(), phase), requeue: postgresPoll}, nil
	}
	return runOutcome{done: true, succeeded: true, verified: true, reason: "Restored",
		message:  fmt.Sprintf("Cluster %s recovered from base backup %s to its consistent point and is ready", target.GetName(), id),
		evidence: fmt.Sprintf("CloudNativePG replayed base backup %s and its WAL from %s; %d instance ready", id, archive, ready)}, nil
}

// cleanup deletes the Cluster a scratch restore created, and only that one.
func (postgresDriver) cleanup(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore) error {
	target := newCNPGCluster(rs.Namespace, rs.Spec.Target.Name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
		return client.IgnoreNotFound(err)
	}
	if target.GetLabels()[backupRestoreLabel] != dnsLabelValue(rs.Name) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, target))
}

// setBarmanArchivePlugin names (or, with objectStore empty, removes) the Barman Cloud Plugin as a
// Cluster's WAL archiver, leaving any other plugin entry as it is.
func setBarmanArchivePlugin(cluster *unstructured.Unstructured, objectStore string) error {
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil {
		return err
	}
	kept := []interface{}{}
	for _, item := range plugins {
		if plugin, _ := item.(map[string]interface{}); plugin["name"] != barmanCloudPlugin {
			kept = append(kept, item)
		}
	}
	if objectStore != "" {
		kept = append(kept, barmanArchivePlugins(objectStore)...)
	}
	if len(kept) == 0 {
		unstructured.RemoveNestedField(cluster.Object, "spec", "plugins")
		return nil
	}
	return unstructured.SetNestedSlice(cluster.Object, kept, "spec", "plugins")
}
