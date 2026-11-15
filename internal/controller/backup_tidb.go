/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TiDB in the backup system (operator#735), through BR (Apache-2.0) as TiDB Operator runs it: a
// Backup is a TiDB Operator Backup (pingcap.com/v1alpha1, backupType full) of the TidbCluster to
// <bucket>/<path>/<backup>/, and a restore is a TiDB Operator Restore of that path into the target
// TidbCluster. BR writes from TiKV straight to the bucket, so TiKV's pods (and the backup pod TiDB
// Operator starts) reach public HTTPS for the run. BR refuses to restore a table that already exists,
// and checks every table's checksum after it restores, which is the verification. TiDB Operator's
// backup pod reports to its Backup through a ServiceAccount tidb-backup-manager, which the operator
// keeps in the namespace with the Role its docs give it.
//
// Point-in-time restore (BR log backup) was to be evaluated: it needs a log task running all the time
// with TiKV's egress open, so it is not used. A restore is to a full backup's commit timestamp.

var (
	tidbBackupGVK  = schema.GroupVersionKind{Group: "pingcap.com", Version: "v1alpha1", Kind: "Backup"}
	tidbRestoreGVK = schema.GroupVersionKind{Group: "pingcap.com", Version: "v1alpha1", Kind: "Restore"}
)

const tidbBackupManager = "tidb-backup-manager"

// +kubebuilder:rbac:groups=pingcap.com,resources=backups;restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;update;patch,resourceNames=tidb-backup-manager
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;update;patch,resourceNames=tidb-backup-manager

func init() {
	backupDrivers[oramav1alpha1.BackupDatastoreTiDB] = tidbDriver{}
	restoreDrivers[oramav1alpha1.BackupDatastoreTiDB] = tidbDriver{}
}

type tidbDriver struct{}

// ensureTiDBBackupManager keeps the ServiceAccount, Role and RoleBinding TiDB Operator's backup
// and restore pods use to report to their resource.
func ensureTiDBBackupManager(ctx context.Context, c client.Client, namespace string) error {
	labels := map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: tidbBackupManager, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, account, func() error {
		account.Labels = labels
		return nil
	}); err != nil {
		return err
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: tidbBackupManager, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		role.Labels = labels
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			{APIGroups: []string{"pingcap.com"}, Resources: []string{"backups", "restores"}, Verbs: []string{"get", "list", "watch", "update"}},
		}
		return nil
	}); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: tidbBackupManager, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, binding, func() error {
		binding.Labels = labels
		binding.RoleRef = rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: tidbBackupManager}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: tidbBackupManager, Namespace: namespace}}
		return nil
	})
	return err
}

// tidbStorage is the s3 block of a TiDB Operator Backup or Restore.
func tidbStorage(bucket *corev1.Secret, prefix, secretName string) map[string]interface{} {
	region := string(bucket.Data["region"])
	if region == "" || region == "auto" {
		region = "us-east-1" // R2 accepts it as "auto"; BR wants an AWS region name
	}
	return map[string]interface{}{
		"provider":       "aws",
		"endpoint":       string(bucket.Data["endpoint"]),
		"bucket":         string(bucket.Data["bucket"]),
		"prefix":         prefix,
		"region":         region,
		"secretName":     secretName,
		"forcePathStyle": true,
	}
}

// tidbPod is the restricted pod security context of TiDB Operator's backup and restore pods.
func tidbPod() map[string]interface{} {
	pod := restrictedPodSecurityContext()
	pod["runAsUser"], pod["runAsGroup"], pod["fsGroup"] = int64(65532), int64(65532), int64(65532)
	return pod
}

func (tidbDriver) backup(ctx context.Context, r *BackupReconciler, b *oramav1alpha1.Backup, bucket *corev1.Secret, prefix string) (backupOutcome, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(tidbClusterGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.Datastore.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) || isKindNotInstalled(err) {
			return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("TidbCluster %s does not exist", b.Spec.Datastore.Name), requeue: backupWaitPoll}}, nil
		}
		return backupOutcome{}, err
	}
	jobName := backupJobName(b.Name)
	keys, err := tidbRunKeys(ctx, r.Client, r.Scheme, b, jobName, bucket)
	if err != nil {
		return backupOutcome{}, err
	}
	if err := ensureTiDBBackupManager(ctx, r.Client, b.Namespace); err != nil {
		return backupOutcome{}, err
	}
	if err := ensureTiDBRunPolicies(ctx, r.Client, r.Scheme, b, jobName, b.Spec.Datastore); err != nil {
		return backupOutcome{}, err
	}
	backupCR := &unstructured.Unstructured{}
	backupCR.SetGroupVersionKind(tidbBackupGVK)
	backupCR.SetNamespace(b.Namespace)
	backupCR.SetName(dnsLabel(b.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(backupCR), backupCR); apierrors.IsNotFound(err) {
		backupCR.SetLabels(map[string]string{backupNameLabel: dnsLabelValue(b.Name)})
		backupCR.Object["spec"] = map[string]interface{}{
			"backupType":                   "full",
			"br":                           map[string]interface{}{"cluster": cluster.GetName(), "clusterNamespace": b.Namespace},
			"s3":                           tidbStorage(bucket, prefix, keys),
			"serviceAccount":               tidbBackupManager,
			"automountServiceAccountToken": true,
			"podSecurityContext":           tidbPod(),
			"cleanPolicy":                  "Retain",
		}
		if err := controllerutil.SetControllerReference(b, backupCR, r.Scheme); err != nil {
			return backupOutcome{}, err
		}
		if err := r.Create(ctx, backupCR); err != nil && !apierrors.IsAlreadyExists(err) {
			return backupOutcome{}, err
		}
		return backupOutcome{runOutcome: runOutcome{message: "TiDB Operator Backup " + backupCR.GetName() + " requested", requeue: postgresPoll}}, nil
	} else if err != nil {
		return backupOutcome{}, err
	}
	phase, _, _ := unstructured.NestedString(backupCR.Object, "status", "phase")
	switch phase {
	case "Complete":
		size, _, _ := unstructured.NestedInt64(backupCR.Object, "status", "backupSize")
		commit, _, _ := unstructured.NestedString(backupCR.Object, "status", "commitTs")
		return backupOutcome{
			runOutcome: runOutcome{done: true, succeeded: true, reason: "BackupSucceeded", message: fmt.Sprintf("BR full backup of %s at commit %s", cluster.GetName(), commit)},
			location:   fmt.Sprintf("s3://%s/%s/", string(bucket.Data["bucket"]), prefix),
			size:       size,
		}, nil
	case "Failed", "Invalid":
		return backupOutcome{runOutcome: runOutcome{done: true, reason: "BackupFailed", message: "TiDB Operator Backup " + phase + ": " + tidbConditionMessage(backupCR)}}, nil
	}
	if phase == "" {
		phase = "Scheduled"
	}
	return backupOutcome{runOutcome: runOutcome{message: "TiDB Operator Backup " + backupCR.GetName() + " is " + phase, requeue: postgresPoll}}, nil
}

func tidbConditionMessage(obj *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var messages []string
	for _, item := range conditions {
		condition, _ := item.(map[string]interface{})
		if message, _ := condition["message"].(string); message != "" {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return "no message"
	}
	return messages[len(messages)-1]
}

// tidbRunKeys writes the bucket's scoped keys as access_key and secret_key, owned by the run (a
// Backup or a BackupRestore), and returns the Secret's name.
func tidbRunKeys(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, jobName string, bucket *corev1.Secret) (string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-tidb-s3", Namespace: owner.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, owner) {
			return fmt.Errorf("Secret %s/%s exists and is not this run's", secret.Namespace, secret.Name)
		}
		secret.Labels = map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			"access_key": append([]byte(nil), bucket.Data["accessKeyId"]...),
			"secret_key": append([]byte(nil), bucket.Data["secretAccessKey"]...),
		}
		return controllerutil.SetControllerReference(owner, secret, scheme)
	})
	return secret.Name, err
}

// ensureTiDBRunPolicies opens, for the run, TiKV's pods (BR writes from there) and TiDB Operator's
// backup or restore pod to public HTTPS, and the cluster to the operator's pod.
func ensureTiDBRunPolicies(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, jobName string, store oramav1alpha1.BackupDatastoreRef) error {
	labels := map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag}
	toolPods := map[string]string{"app.kubernetes.io/managed-by": "backup-operator"}
	if err := ensureDatastoreRunPolicy(ctx, c, scheme, owner, jobName, store, toolPods, labels); err != nil {
		return err
	}
	tool := oramav1alpha1.BackupDatastoreRef{Kind: oramav1alpha1.BackupDatastoreTiDB, Name: store.Name, PodSelector: toolPods}
	return ensureDatastoreRunPolicy(ctx, c, scheme, owner, jobName+"-tool", tool, datastorePodSelector(store), labels)
}

func (tidbDriver) restore(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup, bucket *corev1.Secret) (runOutcome, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(tidbClusterGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.Target.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) || isKindNotInstalled(err) {
			return runOutcome{message: fmt.Sprintf("TidbCluster %s does not exist", rs.Spec.Target.Name), requeue: backupWaitPoll}, nil
		}
		return runOutcome{}, err
	}
	jobName := restoreJobName(rs.Name)
	keys, err := tidbRunKeys(ctx, r.Client, r.Scheme, rs, jobName, bucket)
	if err != nil {
		return runOutcome{}, err
	}
	if err := ensureTiDBBackupManager(ctx, r.Client, rs.Namespace); err != nil {
		return runOutcome{}, err
	}
	if err := ensureTiDBRunPolicies(ctx, r.Client, r.Scheme, rs, jobName, rs.Spec.Target); err != nil {
		return runOutcome{}, err
	}
	restoreCR := &unstructured.Unstructured{}
	restoreCR.SetGroupVersionKind(tidbRestoreGVK)
	restoreCR.SetNamespace(rs.Namespace)
	restoreCR.SetName(dnsLabel(rs.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(restoreCR), restoreCR); apierrors.IsNotFound(err) {
		restoreCR.SetLabels(map[string]string{backupRestoreLabel: dnsLabelValue(rs.Name)})
		prefix := backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path) + "/" + b.Name
		restoreCR.Object["spec"] = map[string]interface{}{
			"backupType":                   "full",
			"br":                           map[string]interface{}{"cluster": cluster.GetName(), "clusterNamespace": rs.Namespace, "checksum": true},
			"s3":                           tidbStorage(bucket, prefix, keys),
			"serviceAccount":               tidbBackupManager,
			"automountServiceAccountToken": true,
			"podSecurityContext":           tidbPod(),
		}
		if err := controllerutil.SetControllerReference(rs, restoreCR, r.Scheme); err != nil {
			return runOutcome{}, err
		}
		if err := r.Create(ctx, restoreCR); err != nil && !apierrors.IsAlreadyExists(err) {
			return runOutcome{}, err
		}
		return runOutcome{message: "TiDB Operator Restore " + restoreCR.GetName() + " requested", requeue: postgresPoll}, nil
	} else if err != nil {
		return runOutcome{}, err
	}
	phase, _, _ := unstructured.NestedString(restoreCR.Object, "status", "phase")
	switch phase {
	case "Complete":
		return runOutcome{done: true, succeeded: true, verified: true, reason: "Restored",
			message:  fmt.Sprintf("BR restored %s into %s", b.Name, cluster.GetName()),
			evidence: "BR checked every restored table's checksum against the backup's (br.checksum)"}, nil
	case "Failed", "Invalid":
		return runOutcome{done: true, reason: "RestoreFailed", message: "TiDB Operator Restore " + phase + ": " + tidbConditionMessage(restoreCR)}, nil
	}
	if phase == "" {
		phase = "Scheduled"
	}
	return runOutcome{message: "TiDB Operator Restore " + restoreCR.GetName() + " is " + phase, requeue: postgresPoll}, nil
}

// cleanup: BR restores only into tables that do not exist; a scratch TidbCluster is its owner's to
// recreate, so nothing here drops data.
func (tidbDriver) cleanup(context.Context, *BackupRestoreReconciler, *oramav1alpha1.BackupRestore) error {
	return nil
}
