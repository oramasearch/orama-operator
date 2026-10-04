/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Workspace volume growth (operator#259, lake migration 0069). Lake's workspaces.storage_gi is the
// desired size of each of the workspace Zetta's volumes; it never decreases (lake's trigger). The
// StatefulSet's claim template is immutable, so the operator grows each existing claim in place
// and keeps the OramaZettaCluster's storage.size in step for the record and for new clusters; a
// replica added later starts at the template's size and is grown by the same claim patch.
const (
	WorkspaceStoragePending  = "Pending"
	WorkspaceStorageResizing = "Resizing"
	WorkspaceStorageReady    = "Ready"
	WorkspaceStorageFailed   = "Failed"

	gibibyte = int64(1) << 30

	defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"
)

// workspaceStorageGi is the row's desired size in GiB. A row without one (a fake, or a schema
// before 0069) falls back to the Lake's spec.workspaces.storage.size, rounded up to whole GiB.
func workspaceStorageGi(lake *oramav1alpha1.Lake, row *LakeWorkspaceRow) int64 {
	if row.StorageGi > 0 {
		return int64(row.StorageGi)
	}
	size := lake.Spec.Workspaces.Storage.Size
	if size == "" {
		size = "5Gi"
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return 5
	}
	return (quantity.Value() + gibibyte - 1) / gibibyte
}

func gibQuantity(gi int64) resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dGi", gi))
}

// workspaceZettaStorage is the OramaZettaCluster's storage for a workspace: the Lake's storage
// class, and the larger of the desired size and the size already recorded (it never shrinks).
func workspaceZettaStorage(lake *oramav1alpha1.Lake, row *LakeWorkspaceRow, current oramav1alpha1.OramaZettaStorage, creating bool) oramav1alpha1.OramaZettaStorage {
	storage := current
	if creating {
		storage = lake.Spec.Workspaces.Storage
	} else if row.StorageGi <= 0 {
		// No desired size from lake: an existing cluster keeps what it has, whatever the Lake's
		// default says now.
		return storage
	}
	desired := gibQuantity(workspaceStorageGi(lake, row))
	recorded, err := resource.ParseQuantity(storage.Size)
	if creating || err != nil || recorded.Cmp(desired) < 0 {
		storage.Size = desired.String()
	}
	return storage
}

// workspaceStorageObservation is what the operator writes in the workspace row's storage fields.
type workspaceStorageObservation struct {
	ObservedGi int32
	Phase      string
	Message    string
}

// workspaceZettaClaims lists the data claims of a workspace Zetta's StatefulSet
// (data-<cluster>-zetta-<ordinal>), by name.
func workspaceZettaClaims(ctx context.Context, reader client.Reader, namespace, clusterName string) ([]corev1.PersistentVolumeClaim, error) {
	var claims corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &claims, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	prefix := "data-" + clusterName + "-zetta-"
	var found []corev1.PersistentVolumeClaim
	for i := range claims.Items {
		if strings.HasPrefix(claims.Items[i].Name, prefix) && claims.Items[i].DeletionTimestamp.IsZero() {
			found = append(found, claims.Items[i])
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found, nil
}

// expandableClass reports whether a claim's storage class allows volume expansion, and the class
// name it judged (the cluster default when the claim names none).
func (r *LakeWorkspacesReconciler) expandableClass(ctx context.Context, claim *corev1.PersistentVolumeClaim) (bool, string, error) {
	name := ""
	if claim.Spec.StorageClassName != nil {
		name = *claim.Spec.StorageClassName
	}
	if name == "" {
		var classes storagev1.StorageClassList
		if err := r.apiReader().List(ctx, &classes); err != nil {
			return false, "", err
		}
		for i := range classes.Items {
			if classes.Items[i].Annotations[defaultStorageClassAnnotation] == "true" {
				name = classes.Items[i].Name
				break
			}
		}
		if name == "" {
			return false, "", nil
		}
	}
	var class storagev1.StorageClass
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, name, nil
		}
		return false, name, err
	}
	return class.AllowVolumeExpansion != nil && *class.AllowVolumeExpansion, name, nil
}

// reconcileWorkspaceStorage grows each claim of the workspace's Zetta to the desired size and
// observes it. It never shrinks a claim: a desired size below a claim's request is ignored and
// reported. A class without expansion, or an API refusal of the patch (a quota, for example), is
// Failed with the reason. changing is true while a claim's capacity has not reached its request.
func (r *LakeWorkspacesReconciler) reconcileWorkspaceStorage(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeWorkspaceRow,
	namespace string,
	cluster *oramav1alpha1.OramaZettaCluster,
) (workspaceStorageObservation, bool, error) {
	clusterName := cluster.Name
	desiredGi := workspaceStorageGi(lake, row)
	if recorded, err := resource.ParseQuantity(cluster.Spec.Storage.Size); row.StorageGi <= 0 && err == nil {
		desiredGi = (recorded.Value() + gibibyte - 1) / gibibyte
	}
	desired := gibQuantity(desiredGi)
	claims, err := workspaceZettaClaims(ctx, r.apiReader(), namespace, clusterName)
	if err != nil {
		return workspaceStorageObservation{}, true, err
	}
	if len(claims) == 0 {
		return workspaceStorageObservation{
			Phase:   WorkspaceStoragePending,
			Message: fmt.Sprintf("waiting for the workspace Zetta's volume claim (%d GiB)", desiredGi),
		}, false, nil
	}
	var failures, refused, resizing []string
	smallest := int64(-1)
	for i := range claims {
		claim := &claims[i]
		requested := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		switch {
		case requested.Cmp(desired) < 0:
			expandable, className, err := r.expandableClass(ctx, claim)
			if err != nil {
				return workspaceStorageObservation{}, true, err
			}
			if !expandable {
				failures = append(failures, fmt.Sprintf("storage class %q of claim %s does not allow volume expansion; it stays at %s",
					className, claim.Name, requested.String()))
				break
			}
			patch := client.MergeFrom(claim.DeepCopy())
			if claim.Spec.Resources.Requests == nil {
				claim.Spec.Resources.Requests = corev1.ResourceList{}
			}
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = desired
			if err := r.Patch(ctx, claim, patch); err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
					claim.Spec.Resources.Requests[corev1.ResourceStorage] = requested
					failures = append(failures, fmt.Sprintf("growing claim %s to %d GiB was refused: %s",
						claim.Name, desiredGi, err.Error()))
					break
				}
				return workspaceStorageObservation{}, true, err
			}
		case requested.Cmp(desired) > 0:
			refused = append(refused, fmt.Sprintf("%s at %s", claim.Name, requested.String()))
		}
		capacity := claim.Status.Capacity[corev1.ResourceStorage]
		target := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if capacity.Cmp(target) < 0 {
			note := claim.Name
			for _, condition := range claim.Status.Conditions {
				if condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending && condition.Status == corev1.ConditionTrue {
					note += " (file system resize pending)"
				}
			}
			resizing = append(resizing, note)
		}
		if gi := capacity.Value() / gibibyte; smallest < 0 || gi < smallest {
			smallest = gi
		}
	}
	observation := workspaceStorageObservation{ObservedGi: int32(max(smallest, 0))}
	switch {
	case len(failures) > 0:
		observation.Phase = WorkspaceStorageFailed
		observation.Message = strings.Join(failures, "; ")
	case len(resizing) > 0:
		observation.Phase = WorkspaceStorageResizing
		observation.Message = fmt.Sprintf("growing %s to %d GiB", strings.Join(resizing, ", "), desiredGi)
	default:
		observation.Phase = WorkspaceStorageReady
		observation.Message = fmt.Sprintf("%d volume(s) at %d GiB or more", len(claims), desiredGi)
	}
	if len(refused) > 0 {
		observation.Message += fmt.Sprintf("; storage_gi %d is below %s, and volumes never shrink",
			desiredGi, strings.Join(refused, ", "))
	}
	return observation, len(resizing) > 0, nil
}
