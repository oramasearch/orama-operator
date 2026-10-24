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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A storage rebuild (spec.storageRebuild, operator#915) moves a stack to volumes it was not created
// with. A claim's storage class and a StatefulSet's claim template cannot change, and a StatefulSet's
// claims outlive it, so the only way onto new volumes is to delete the old ones. The rebuild deletes
// the stack's data StatefulSets, their claims, the control DB and what recorded work against it, then
// hands back to the normal reconcile, which creates them all again from the spec.

const (
	// storageRebuildCondition reports a requested rebuild: Refused, Deleting, or Completed.
	storageRebuildCondition = "StorageRebuild"
	rebuildPhaseDeleting    = "Deleting"
	rebuildPhaseCompleted   = "Completed"
	// storageRebuildRequeue paces the passes that wait for the old objects to go.
	storageRebuildRequeue = 10 * time.Second
)

// rebuildDataComponents are the components whose StatefulSet keeps a "data" claim.
var rebuildDataComponents = []string{"writer", "reader", "analytics", "broker", "cache", "search", "harvest-mongo"}

// reconcileStorageRebuild runs before any component is reconciled. It returns true when the normal
// reconcile may go on, and false while a rebuild is deleting the old storage, so that no StatefulSet
// or control DB is created again on a claim the rebuild is about to delete.
func (r *OramaCloudV1Reconciler) reconcileStorageRebuild(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	current := oc.Status.StorageRebuild
	if current == nil || current.Phase != rebuildPhaseDeleting {
		req := oc.Spec.StorageRebuild
		if req == nil || (current != nil && current.ID == req.ID) {
			return true, nil
		}
		refusal, err := r.storageRebuildRefusal(ctx, oc, req)
		if err != nil {
			return false, err
		}
		if refusal != "" {
			r.setStatus(ctx, oc, oc.Status.Phase, oc.Status.ComponentsReady, oc.Status.Components, nil, metav1.Condition{
				Type: storageRebuildCondition, Status: metav1.ConditionFalse, Reason: "Refused",
				Message: fmt.Sprintf("rebuild %s refused: %s", req.ID, refusal),
			})
			return true, nil
		}
		claims, err := r.rebuildClaims(ctx, oc)
		if err != nil {
			return false, err
		}
		// The record is written before anything is deleted: a rebuild deletes no claim it did not
		// record, and a restarted operator finishes the same rebuild from it.
		oc.Status.StorageRebuild = &oramav1alpha1.StorageRebuildStatus{ID: req.ID, Phase: rebuildPhaseDeleting, Claims: claims}
		meta.SetStatusCondition(&oc.Status.Conditions, metav1.Condition{
			Type: storageRebuildCondition, Status: metav1.ConditionFalse, Reason: rebuildPhaseDeleting,
			Message:            fmt.Sprintf("rebuild %s: deleting %d claims and the workloads that use them", req.ID, len(claims)),
			ObservedGeneration: oc.Generation,
		})
		if err := r.Status().Update(ctx, oc); err != nil {
			return false, fmt.Errorf("record storage rebuild %s: %w", req.ID, err)
		}
	}

	rebuild := oc.Status.StorageRebuild
	remaining, err := r.deleteRebuildStorage(ctx, oc, rebuild.Claims)
	if err != nil {
		return false, err
	}
	if len(remaining) > 0 {
		message := fmt.Sprintf("rebuild %s: waiting for %s to be deleted", rebuild.ID, strings.Join(remaining, ", "))
		r.setStatus(ctx, oc, "Progressing", oc.Status.ComponentsReady, oc.Status.Components, nil,
			metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "StorageRebuilding", Message: message},
			metav1.Condition{Type: storageRebuildCondition, Status: metav1.ConditionFalse, Reason: rebuildPhaseDeleting, Message: message},
		)
		return false, nil
	}
	rebuild.Phase = rebuildPhaseCompleted
	meta.SetStatusCondition(&oc.Status.Conditions, metav1.Condition{
		Type: storageRebuildCondition, Status: metav1.ConditionTrue, Reason: rebuildPhaseCompleted,
		Message:            fmt.Sprintf("rebuild %s deleted %d claims; the stack is built again from its spec", rebuild.ID, len(rebuild.Claims)),
		ObservedGeneration: oc.Generation,
	})
	if err := r.Status().Update(ctx, oc); err != nil {
		return false, fmt.Errorf("complete storage rebuild %s: %w", rebuild.ID, err)
	}
	return true, nil
}

// storageRebuildRefusal says why a request may not start, or "" when it may.
func (r *OramaCloudV1Reconciler) storageRebuildRefusal(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, req *oramav1alpha1.StorageRebuildSpec) (string, error) {
	if req.InstanceUID != string(oc.UID) {
		return fmt.Sprintf("instanceUID %s is not this OramaCloudV1's uid %s", req.InstanceUID, oc.UID), nil
	}
	if req.DataLoss != "DiscardAll" {
		return "dataLoss must be DiscardAll", nil
	}
	var migrations oramav1alpha1.OramaCloudV1MigrationList
	if err := r.List(ctx, &migrations, client.InNamespace(oc.Namespace)); err != nil {
		return "", fmt.Errorf("list migrations: %w", err)
	}
	if n := len(migrations.Items); n > 0 {
		return fmt.Sprintf("%d OramaCloudV1Migration(s) in namespace %s may have copied customer data into this stack", n, oc.Namespace), nil
	}
	return "", nil
}

// rebuildClaims lists the stack's data claims, sorted by name: each data StatefulSet's
// data-<statefulset>-<ordinal>, including retained ordinals, and every claim of the control DB's
// CNPG Cluster. A claim another controller owns is refused rather than recorded.
func (r *OramaCloudV1Reconciler) rebuildClaims(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]oramav1alpha1.RebuildClaim, error) {
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	controlDB := oc.Name + "-controldb"
	var recorded []oramav1alpha1.RebuildClaim
	for i := range claims.Items {
		claim := &claims.Items[i]
		statefulSet, ok := rebuildDataClaimStatefulSet(oc, claim.Name)
		switch {
		case ok:
			if owner := metav1.GetControllerOf(claim); owner != nil && !(owner.Kind == "StatefulSet" && owner.Name == statefulSet) {
				return nil, fmt.Errorf("claim %s is controlled by %s %s, not this stack", claim.Name, owner.Kind, owner.Name)
			}
		case claim.Labels[cnpgClusterLabel] == controlDB:
		default:
			continue
		}
		recorded = append(recorded, oramav1alpha1.RebuildClaim{Name: claim.Name, UID: string(claim.UID)})
	}
	sort.Slice(recorded, func(i, j int) bool { return recorded[i].Name < recorded[j].Name })
	return recorded, nil
}

// rebuildDataClaimStatefulSet returns the data StatefulSet a claim name belongs to.
func rebuildDataClaimStatefulSet(oc *oramav1alpha1.OramaCloudV1, name string) (string, bool) {
	for _, component := range rebuildDataComponents {
		statefulSet := oc.Name + "-" + component
		if _, ok := claimOrdinal(name, "data-"+statefulSet+"-"); ok {
			return statefulSet, true
		}
	}
	return "", false
}

// deleteRebuildStorage asks for every object the rebuild removes to be deleted, and returns those
// still present. Workloads go in the foreground, so their pods are gone before they are; a claim is
// deleted only while it has its recorded UID, and kubelet's pvc-protection keeps it until no pod
// uses it. Each pass repeats the deletes it needs, so a pass interrupted at any point resumes.
func (r *OramaCloudV1Reconciler) deleteRebuildStorage(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, claims []oramav1alpha1.RebuildClaim) ([]string, error) {
	var remaining []string
	foreground := client.PropagationPolicy(metav1.DeletePropagationForeground)
	// remove deletes one object the stack controls, once, and counts it until it is gone.
	remove := func(kind string, obj client.Object) error {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				return nil
			}
			return fmt.Errorf("get %s %s: %w", kind, obj.GetName(), err)
		}
		if !metav1.IsControlledBy(obj, oc) {
			return fmt.Errorf("%s %s is not controlled by this OramaCloudV1", kind, obj.GetName())
		}
		remaining = append(remaining, kind+" "+obj.GetName())
		if obj.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, obj, foreground); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s %s: %w", kind, obj.GetName(), err)
			}
		}
		return nil
	}

	for _, component := range rebuildDataComponents {
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-" + component, Namespace: oc.Namespace}}
		if err := remove("StatefulSet", sts); err != nil {
			return nil, err
		}
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(oc.Name + "-controldb")
	cluster.SetNamespace(oc.Namespace)
	if err := remove("Cluster", cluster); err != nil {
		return nil, err
	}

	// What recorded work against the old database or disks: the PostgresSchemas (whose migration
	// Jobs go with them) and the Jobs this stack owns, such as the c1 row's and analytics
	// retention's. The normal reconcile creates each again and runs it on the new volumes.
	var schemas oramav1alpha1.PostgresSchemaList
	if err := r.List(ctx, &schemas, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list PostgresSchemas: %w", err)
	}
	for i := range schemas.Items {
		if metav1.IsControlledBy(&schemas.Items[i], oc) {
			if err := remove("PostgresSchema", &schemas.Items[i]); err != nil {
				return nil, err
			}
		}
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list Jobs: %w", err)
	}
	for i := range jobs.Items {
		if metav1.IsControlledBy(&jobs.Items[i], oc) {
			if err := remove("Job", &jobs.Items[i]); err != nil {
				return nil, err
			}
		}
	}

	for _, recorded := range claims {
		claim := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: recorded.Name}, claim); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get claim %s: %w", recorded.Name, err)
		}
		if string(claim.UID) != recorded.UID {
			continue // a new claim with the recorded name: the old one is gone
		}
		remaining = append(remaining, "claim "+recorded.Name)
		if claim.DeletionTimestamp == nil {
			uid := types.UID(recorded.UID)
			if err := r.Delete(ctx, claim, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return nil, fmt.Errorf("delete claim %s: %w", recorded.Name, err)
			}
		}
	}
	return remaining, nil
}
