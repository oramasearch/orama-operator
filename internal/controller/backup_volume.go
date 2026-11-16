/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// Volume-backed datastores (operator#738, operator#737): a volume claim (a Lake's store) or a wrapper
// service's data claim (SQLite, DuckDB, Polars). The backup controller first takes a CSI
// VolumeSnapshot <job>-snap of the claim, waits until it is ready, makes a claim <job>-source from it,
// and only then starts the backup Job, which mounts that claim read-only at /source as the claim's
// files' owner (spec.datastore.volume.runAsUser). Both are owned by the Backup and deleted when it
// finishes; the workload never stops and its own claim is never mounted twice. A restore creates its
// target claim (target.name, which must not exist), as big and of the same class as the source was,
// and mounts it at /target; a scratch one is deleted afterwards.

const (
	volumeSourceMount     = "/source"
	volumeTargetMount     = "/target"
	volumeSizeAnnot       = "orama.cloud/volume-size"
	volumeClassAnnot      = "orama.cloud/volume-storage-class"
	volumeDefaultRunAs    = int64(65532)
	volumeSnapshotTimeout = "the snapshot is not ready yet"
)

// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

func volumeBacked(kind oramav1alpha1.BackupDatastoreKind) bool {
	switch kind {
	case oramav1alpha1.BackupDatastoreVolume, oramav1alpha1.BackupDatastoreSQLite, oramav1alpha1.BackupDatastoreDuckDB, oramav1alpha1.BackupDatastorePolars:
		return true
	}
	return false
}

func volumeSnapshotClass(store oramav1alpha1.BackupDatastoreRef) string {
	if store.Volume != nil && store.Volume.VolumeSnapshotClassName != "" {
		return store.Volume.VolumeSnapshotClassName
	}
	return snapshotDefaultClass
}

// prepareVolumeCopy makes the snapshot and the claim made from it, and returns the claim's name, or
// what it is waiting for.
func prepareVolumeCopy(ctx context.Context, c client.Client, scheme *runtime.Scheme, b *oramav1alpha1.Backup, jobName string) (string, string, error) {
	var source corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.Datastore.Name}, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("volume claim %s does not exist", b.Spec.Datastore.Name), nil
		}
		return "", "", err
	}
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetNamespace(b.Namespace)
	snapshot.SetName(jobName + "-snap")
	if err := c.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); apierrors.IsNotFound(err) {
		snapshot.SetLabels(map[string]string{backupNameLabel: dnsLabelValue(b.Name), "app.kubernetes.io/managed-by": backupOperatorManagedTag})
		snapshot.Object["spec"] = map[string]interface{}{
			"volumeSnapshotClassName": volumeSnapshotClass(b.Spec.Datastore),
			"source":                  map[string]interface{}{"persistentVolumeClaimName": source.Name},
		}
		if err := controllerutil.SetControllerReference(b, snapshot, scheme); err != nil {
			return "", "", err
		}
		if err := c.Create(ctx, snapshot); err != nil && !apierrors.IsAlreadyExists(err) {
			if isKindNotInstalled(err) {
				return "", "the CSI snapshot kinds (snapshot.storage.k8s.io) are not installed on this cluster", nil
			}
			return "", "", err
		}
		return "", "taking a snapshot of " + source.Name, nil
	} else if err != nil {
		if isKindNotInstalled(err) {
			return "", "the CSI snapshot kinds (snapshot.storage.k8s.io) are not installed on this cluster", nil
		}
		return "", "", err
	}
	if ready, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse"); !ready {
		message, _, _ := unstructured.NestedString(snapshot.Object, "status", "error", "message")
		if message != "" {
			return "", "the snapshot failed: " + message, nil
		}
		return "", volumeSnapshotTimeout, nil
	}
	size := source.Spec.Resources.Requests[corev1.ResourceStorage]
	if capacity, ok := source.Status.Capacity[corev1.ResourceStorage]; ok {
		size = capacity
	}
	clone := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-source", Namespace: b.Namespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(clone), clone); apierrors.IsNotFound(err) {
		group := "snapshot.storage.k8s.io"
		clone.Labels = map[string]string{backupNameLabel: dnsLabelValue(b.Name), "app.kubernetes.io/managed-by": backupOperatorManagedTag}
		clone.Spec = corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: source.Spec.StorageClassName,
			DataSource:       &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: snapshot.GetName()},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		}
		if err := controllerutil.SetControllerReference(b, clone, scheme); err != nil {
			return "", "", err
		}
		if err := c.Create(ctx, clone); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	// What a restore needs to make a claim like this one.
	annotations := b.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	class := ""
	if source.Spec.StorageClassName != nil {
		class = *source.Spec.StorageClassName
	}
	if annotations[volumeSizeAnnot] != size.String() || annotations[volumeClassAnnot] != class {
		annotations[volumeSizeAnnot], annotations[volumeClassAnnot] = size.String(), class
		b.SetAnnotations(annotations)
		if err := c.Update(ctx, b); err != nil {
			return "", "", err
		}
	}
	return clone.Name, "", nil
}

// withVolume mounts a claim in a run's Job at path, as the claim's files' owner.
func withVolume(job *batchv1.Job, claim, path string, readOnly bool, source *oramav1alpha1.BackupVolumeSource) {
	user, group := volumeDefaultRunAs, volumeDefaultRunAs
	if source != nil && source.RunAsUser != nil {
		user = *source.RunAsUser
		group = user
	}
	if source != nil && source.RunAsGroup != nil {
		group = *source.RunAsGroup
	}
	pod := &job.Spec.Template.Spec
	pod.SecurityContext.RunAsUser, pod.SecurityContext.RunAsGroup = int64Ptr(user), int64Ptr(group)
	if !readOnly {
		pod.SecurityContext.FSGroup = int64Ptr(group)
	}
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly},
	}})
	container := &pod.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: path, ReadOnly: readOnly})
	container.Env = append(container.Env, corev1.EnvVar{Name: backup.EnvVolumePath, Value: path})
	container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
}

// ensureRestoreClaim creates a volume restore's target claim, sized and classed like the backup's
// source. It returns why it cannot, if it cannot.
func ensureRestoreClaim(ctx context.Context, c client.Client, rs *oramav1alpha1.BackupRestore, b *oramav1alpha1.Backup) (string, error) {
	claim := &corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.Target.Name}, claim)
	if err == nil {
		if claim.Labels[backupRestoreLabel] != dnsLabelValue(rs.Name) {
			return fmt.Sprintf("volume claim %s already exists: a volume restore creates its target", claim.Name), nil
		}
		return "", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	size, err := resource.ParseQuantity(b.Annotations[volumeSizeAnnot])
	if err != nil {
		return fmt.Sprintf("Backup %s does not record its volume's size", b.Name), nil
	}
	claim = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: rs.Spec.Target.Name, Namespace: rs.Namespace,
			Labels: map[string]string{backupRestoreLabel: dnsLabelValue(rs.Name), "app.kubernetes.io/managed-by": backupOperatorManagedTag}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if class := b.Annotations[volumeClassAnnot]; class != "" {
		claim.Spec.StorageClassName = &class
	}
	return "", client.IgnoreAlreadyExists(c.Create(ctx, claim))
}

// deleteRestoreClaim deletes a scratch volume restore's claim, only the one it created.
func deleteRestoreClaim(ctx context.Context, c client.Client, rs *oramav1alpha1.BackupRestore) error {
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.Target.Name}, claim); err != nil {
		return client.IgnoreNotFound(err)
	}
	if claim.Labels[backupRestoreLabel] != dnsLabelValue(rs.Name) {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, claim))
}

// cleanupVolumeCopy deletes the snapshot and the claim made from it.
func cleanupVolumeCopy(ctx context.Context, c client.Client, namespace, jobName string) error {
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetNamespace(namespace)
	snapshot.SetName(jobName + "-snap")
	for _, object := range []client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-source", Namespace: namespace}},
		snapshot,
	} {
		if err := c.Delete(ctx, object); err != nil && !isGoneOrUnknownKind(err) {
			return err
		}
	}
	return nil
}
