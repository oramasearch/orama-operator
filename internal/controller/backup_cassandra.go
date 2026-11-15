/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Cassandra in the backup system (operator#734), through Medusa (Apache-2.0), which K8ssandra already
// runs beside each Cassandra node: nothing is added to the operator image.
//
// The schedule owns Medusa's storage: a Secret <schedule>-medusa with the bucket's scoped keys in the
// ini shape Medusa reads (key `credentials`), and the K8ssandraCluster's spec.medusa (s3_compatible,
// the bucket, the schedule's path as the prefix, the restricted security context the workspace
// namespace needs). The Lake writes only its own fields of the K8ssandraCluster, so spec.medusa is the
// schedule's alone; adding it restarts the Cassandra pods once, which is how K8ssandra adds Medusa's
// container. Each Backup is a MedusaBackupJob of the cluster's datacenter, owned by it: every node
// snapshots at the same time and Medusa uploads the files. A restore is a MedusaRestoreJob of that
// backup into the target's datacenter, which must have the same number of nodes and the same Medusa
// storage (the restore writes it), and which Medusa empties and refills. Medusa verifies each file it
// downloads against the hash it recorded; there is no row count for Cassandra.

var (
	medusaBackupJobGVK  = schema.GroupVersionKind{Group: "medusa.k8ssandra.io", Version: "v1alpha1", Kind: "MedusaBackupJob"}
	medusaRestoreJobGVK = schema.GroupVersionKind{Group: "medusa.k8ssandra.io", Version: "v1alpha1", Kind: "MedusaRestoreJob"}
)

// +kubebuilder:rbac:groups=medusa.k8ssandra.io,resources=medusabackupjobs;medusarestorejobs;medusabackups,verbs=get;list;watch;create;update;patch;delete

func medusaSecretName(schedule string) string { return schedule + "-medusa" }

func init() {
	backupDrivers[oramav1alpha1.BackupDatastoreCassandra] = cassandraDriver{}
	restoreDrivers[oramav1alpha1.BackupDatastoreCassandra] = cassandraDriver{}
}

type cassandraDriver struct{}

func newK8ssandraCluster(namespace, name string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(k8ssandraClusterGVK)
	cluster.SetNamespace(namespace)
	cluster.SetName(name)
	return cluster
}

// cassandraDatacenterName is the CassandraDatacenter a K8ssandraCluster's first datacenter makes.
func cassandraDatacenterName(cluster *unstructured.Unstructured) string {
	datacenters, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "cassandra", "datacenters")
	if len(datacenters) == 0 {
		return ""
	}
	dc, _ := datacenters[0].(map[string]interface{})
	name, _, _ := unstructured.NestedString(dc, "metadata", "name")
	return name
}

// medusaSpec is spec.medusa for a bucket and prefix, its keys in credentialsSecret.
func medusaSpec(bucket *corev1.Secret, prefix, credentialsSecret string) map[string]interface{} {
	host := string(bucket.Data["endpoint"])
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Hostname()
	}
	region := string(bucket.Data["region"])
	if region == "" {
		region = "auto"
	}
	return map[string]interface{}{
		"storageProperties": map[string]interface{}{
			"storageProvider":  "s3_compatible",
			"bucketName":       string(bucket.Data["bucket"]),
			"prefix":           prefix,
			"host":             host,
			"port":             int64(443),
			"secure":           true,
			"region":           region,
			"storageSecretRef": map[string]interface{}{"name": credentialsSecret},
		},
		"securityContext":        restrictedContainerSecurityContext(),
		"containerResources":     serviceResources("50m", "256Mi", "500m", "512Mi"),
		"initContainerResources": serviceResources("50m", "128Mi", "500m", "512Mi"),
	}
}

// ensureMedusaStorage writes a Cassandra schedule's Medusa storage and the cluster's spec.medusa.
func (r *BackupScheduleReconciler) ensureMedusaStorage(ctx context.Context, s *oramav1alpha1.BackupSchedule) (string, string, error) {
	cluster := newK8ssandraCluster(s.Namespace, s.Spec.Datastore.Name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		if isKindNotInstalled(err) {
			return "CassandraOperatorNotInstalled", "K8ssandra (k8ssandra.io K8ssandraCluster) is not installed on this cluster", nil
		}
		if apierrors.IsNotFound(err) {
			return "DatastoreNotFound", fmt.Sprintf("K8ssandraCluster %s does not exist", s.Spec.Datastore.Name), nil
		}
		return "", "", err
	}
	bucket, reason, message, err := resolveBackupBucket(ctx, r.Client, s.Namespace, s.Spec.Bucket)
	if err != nil || reason != "" {
		return reason, message, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: medusaSecretName(s.Name), Namespace: s.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, s) {
			return fmt.Errorf("Secret %s/%s exists and is not this schedule's", s.Namespace, secret.Name)
		}
		secret.Labels = map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag, backupScheduleLabel: dnsLabelValue(s.Name)}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{"credentials": []byte(fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
			bucket.Data["accessKeyId"], bucket.Data["secretAccessKey"]))}
		return controllerutil.SetControllerReference(s, secret, r.Scheme)
	}); err != nil {
		return "", "", err
	}
	desired := medusaSpec(bucket, backupPrefix(s.Namespace, s.Spec.Datastore, s.Spec.Path), secret.Name)
	return "", "", writeMedusaSpec(ctx, r.Client, cluster, desired)
}

// writeMedusaSpec sets spec.medusa only when it differs, so a steady cluster is never rewritten.
func writeMedusaSpec(ctx context.Context, c client.Client, cluster *unstructured.Unstructured, desired map[string]interface{}) error {
	current, _, _ := unstructured.NestedMap(cluster.Object, "spec", "medusa")
	if apiequality.Semantic.DeepEqual(current, desired) {
		return nil
	}
	if err := unstructured.SetNestedMap(cluster.Object, desired, "spec", "medusa"); err != nil {
		return err
	}
	return c.Update(ctx, cluster)
}

// medusaJobOutcome reads a MedusaBackupJob's or MedusaRestoreJob's status: finished once finishTime
// is set, failed when any node is in status.failed.
func medusaJobOutcome(job *unstructured.Unstructured) (done, failed bool, detail string) {
	finish, _, _ := unstructured.NestedString(job.Object, "status", "finishTime")
	failedNodes, _, _ := unstructured.NestedStringSlice(job.Object, "status", "failed")
	finished, _, _ := unstructured.NestedStringSlice(job.Object, "status", "finished")
	message, _, _ := unstructured.NestedString(job.Object, "status", "message")
	if len(failedNodes) > 0 {
		return true, true, fmt.Sprintf("failed on %s %s", strings.Join(failedNodes, ", "), message)
	}
	return finish != "", false, fmt.Sprintf("%d nodes finished", len(finished))
}

func (cassandraDriver) backup(ctx context.Context, r *BackupReconciler, b *oramav1alpha1.Backup, bucket *corev1.Secret, _ string) (backupOutcome, error) {
	cluster := newK8ssandraCluster(b.Namespace, b.Spec.Datastore.Name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		if apierrors.IsNotFound(err) || isKindNotInstalled(err) {
			return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("K8ssandraCluster %s does not exist", b.Spec.Datastore.Name), requeue: backupWaitPoll}}, nil
		}
		return backupOutcome{}, err
	}
	if _, found, _ := unstructured.NestedMap(cluster.Object, "spec", "medusa"); !found {
		return backupOutcome{runOutcome: runOutcome{message: fmt.Sprintf("K8ssandraCluster %s has no Medusa yet; its BackupSchedule writes spec.medusa", cluster.GetName()), requeue: backupWaitPoll}}, nil
	}
	datacenter := cassandraDatacenterName(cluster)
	if datacenter == "" {
		return backupOutcome{runOutcome: runOutcome{done: true, reason: "BackupFailed", message: "the K8ssandraCluster has no datacenter"}}, nil
	}
	// Medusa beside each node uploads the files itself: the nodes reach public HTTPS for the run
	// (the policy's ingress names only the nodes themselves).
	if err := ensureDatastoreRunPolicy(ctx, r.Client, r.Scheme, b, backupJobName(b.Name), b.Spec.Datastore, datastorePodSelector(b.Spec.Datastore), map[string]string{"app.kubernetes.io/managed-by": backupOperatorManagedTag}); err != nil {
		return backupOutcome{}, err
	}
	job := &unstructured.Unstructured{}
	job.SetGroupVersionKind(medusaBackupJobGVK)
	job.SetNamespace(b.Namespace)
	job.SetName(dnsLabel(b.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); apierrors.IsNotFound(err) {
		job.SetLabels(map[string]string{backupNameLabel: dnsLabelValue(b.Name)})
		job.Object["spec"] = map[string]interface{}{"cassandraDatacenter": datacenter, "backupType": "differential"}
		if err := controllerutil.SetControllerReference(b, job, r.Scheme); err != nil {
			return backupOutcome{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return backupOutcome{}, err
		}
		return backupOutcome{runOutcome: runOutcome{message: "MedusaBackupJob " + job.GetName() + " requested", requeue: postgresPoll}}, nil
	} else if err != nil {
		return backupOutcome{}, err
	}
	done, failed, detail := medusaJobOutcome(job)
	switch {
	case failed:
		return backupOutcome{runOutcome: runOutcome{done: true, reason: "BackupFailed", message: "Medusa backup " + detail}}, nil
	case done:
		prefix := backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path)
		return backupOutcome{
			runOutcome: runOutcome{done: true, succeeded: true, reason: "BackupSucceeded", message: fmt.Sprintf("Medusa backup %s of datacenter %s: %s", job.GetName(), datacenter, detail)},
			location:   fmt.Sprintf("s3://%s/%s/", string(bucket.Data["bucket"]), prefix),
		}, nil
	}
	return backupOutcome{runOutcome: runOutcome{message: "Medusa backup " + job.GetName() + ": " + detail, requeue: postgresPoll}}, nil
}

func (cassandraDriver) restore(ctx context.Context, r *BackupRestoreReconciler, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup, bucket *corev1.Secret) (runOutcome, error) {
	source := newK8ssandraCluster(b.Namespace, b.Spec.Datastore.Name)
	target := newK8ssandraCluster(rs.Namespace, rs.Spec.Target.Name)
	if err := r.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
		if apierrors.IsNotFound(err) || isKindNotInstalled(err) {
			return runOutcome{message: fmt.Sprintf("K8ssandraCluster %s does not exist", rs.Spec.Target.Name), requeue: backupWaitPoll}, nil
		}
		return runOutcome{}, err
	}
	datacenter := cassandraDatacenterName(target)
	if datacenter == "" {
		return runOutcome{done: true, reason: "Refused", message: "the target K8ssandraCluster has no datacenter"}, nil
	}
	// The target reads the same storage: the source's spec.medusa, or one written from the bucket.
	desired := medusaSpec(bucket, backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path), medusaSecretName(lakeBackupScheduleName(b.Spec.Datastore.Name)))
	if r.Get(ctx, client.ObjectKeyFromObject(source), source) == nil {
		if medusa, found, _ := unstructured.NestedMap(source.Object, "spec", "medusa"); found {
			desired = medusa
		}
	}
	if target.GetName() != source.GetName() {
		if err := writeMedusaSpec(ctx, r.Client, target, desired); err != nil {
			return runOutcome{}, err
		}
	}
	job := &unstructured.Unstructured{}
	job.SetGroupVersionKind(medusaRestoreJobGVK)
	job.SetNamespace(rs.Namespace)
	job.SetName(dnsLabel(rs.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); apierrors.IsNotFound(err) {
		job.SetLabels(map[string]string{backupRestoreLabel: dnsLabelValue(rs.Name)})
		job.Object["spec"] = map[string]interface{}{"cassandraDatacenter": datacenter, "backup": dnsLabel(b.Name)}
		if err := controllerutil.SetControllerReference(rs, job, r.Scheme); err != nil {
			return runOutcome{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return runOutcome{}, err
		}
		return runOutcome{message: "MedusaRestoreJob " + job.GetName() + " requested", requeue: postgresPoll}, nil
	} else if err != nil {
		return runOutcome{}, err
	}
	done, failed, detail := medusaJobOutcome(job)
	switch {
	case failed:
		return runOutcome{done: true, reason: "RestoreFailed", message: "Medusa restore " + detail}, nil
	case done:
		return runOutcome{done: true, succeeded: true, verified: true, reason: "Restored",
			message:  fmt.Sprintf("Medusa restored backup %s into datacenter %s: %s", dnsLabel(b.Name), datacenter, detail),
			evidence: "Medusa checked every downloaded file against the hash it recorded at backup: " + detail}, nil
	}
	return runOutcome{message: "Medusa restore " + job.GetName() + ": " + detail, requeue: postgresPoll}, nil
}

// cleanup: Cassandra has nothing a scratch rehearsal could empty that the next restore does not
// replace; Medusa empties the datacenter before it restores.
func (cassandraDriver) cleanup(context.Context, *BackupRestoreReconciler, *oramav1alpha1.BackupRestore) error {
	return nil
}
