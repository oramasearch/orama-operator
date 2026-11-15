// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A volume's storage declares its size and class. Growth is applied in place (storage.go). A claim's
// class, a StatefulSet's claim template and a CNPG Cluster's storage class cannot change, and a claim
// outlives its StatefulSet, so a volume on another class, or larger than declared, reaches its
// declared state only by being replaced (operator#915). With storage.approveDataDeletion the operator
// replaces it: it deletes the StatefulSet and its claims that differ, or the control DB, and the
// ordinary reconcile creates them again from the spec. Every pass works from what is on the cluster,
// so a later change to the declared storage is reconciled the same way.

// volumeReplacementRequeue paces the passes that wait for replaced objects to go.
const volumeReplacementRequeue = 10 * time.Second

// reconcileVolumeReplacement runs before any component is reconciled. It returns true when the
// ordinary reconcile may go on, and false while a volume is being replaced, so that no StatefulSet or
// control DB is created again on a claim being deleted.
func (r *OramaCloudV1Reconciler) reconcileVolumeReplacement(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	var waiting []string
	for _, v := range managedVolumes(oc) {
		if v.storage == nil || !v.storage.ApproveDataDeletion {
			continue
		}
		var pending []string
		var err error
		if v.component == controlDBVolume {
			pending, err = r.replaceControlDB(ctx, oc, v)
		} else {
			pending, err = r.replaceStatefulVolume(ctx, oc, v)
		}
		if err != nil {
			return false, err
		}
		waiting = append(waiting, pending...)
	}
	if len(waiting) == 0 {
		return true, nil
	}
	sort.Strings(waiting)
	message := "replacing volumes that cannot reach their declared storage in place (approveDataDeletion): waiting for " +
		strings.Join(waiting, ", ") + " to be deleted"
	r.setStatus(ctx, oc, "Progressing", oc.Status.ComponentsReady, oc.Status.Components, nil,
		metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ReplacingStorage", Message: message},
		metav1.Condition{Type: "StorageApplied", Status: metav1.ConditionFalse, Reason: "ReplacingStorage", Message: message},
	)
	return false, nil
}

// replaceStatefulVolume deletes a component's StatefulSet and those of its data claims that cannot
// reach the declared storage in place, and returns what is still being deleted. A claim already on
// the declared class and no larger than declared is kept. The StatefulSet goes too when its claim
// template names another class, so new ordinals are never made on the old one.
func (r *OramaCloudV1Reconciler) replaceStatefulVolume(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, v managedVolume) ([]string, error) {
	name := oc.Name + "-" + v.component
	desired := volumeSize(v.storage, v.fallback)
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	var stale []*corev1.PersistentVolumeClaim
	for i := range claims.Items {
		claim := &claims.Items[i]
		ordinal, ok := claimOrdinal(claim.Name, "data-"+name+"-")
		if !ok {
			continue
		}
		if owner := metav1.GetControllerOf(claim); owner != nil &&
			!(owner.Kind == "StatefulSet" && owner.Name == name) && !(owner.Kind == "Pod" && owner.Name == fmt.Sprintf("%s-%d", name, ordinal)) {
			return nil, fmt.Errorf("claim %s is controlled by %s %s, not this stack: it is never replaced", claim.Name, owner.Kind, owner.Name)
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if volumeNeedsReplacement(v.storage, desired, current, claim.Spec.StorageClassName) {
			stale = append(stale, claim)
		}
	}

	// What was set up once on the old volume goes first, on every pass until the old claims are gone,
	// so an interrupted pass never leaves it behind: ClickHouse's retention Job, and analytics-processor,
	// which creates ClickHouse's tables only when it starts.
	if v.component == "analytics" && len(stale) > 0 {
		for _, obj := range []client.Object{
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: analyticsRetentionName(oc), Namespace: oc.Namespace}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-analytics-processor", Namespace: oc.Namespace}},
		} {
			if err := r.deleteOwned(ctx, oc, obj); err != nil {
				return nil, err
			}
		}
	}

	var pending []string
	sts := &appsv1.StatefulSet{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, sts); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("get StatefulSet %s: %w", name, err)
	case len(stale) > 0 || statefulSetTemplateNeedsReplacement(v.storage, sts):
		if !metav1.IsControlledBy(sts, oc) {
			return nil, fmt.Errorf("StatefulSet %s is not controlled by this OramaCloudV1: its volumes are never replaced", name)
		}
		pending = append(pending, "StatefulSet "+name)
		if sts.DeletionTimestamp == nil {
			if err := r.Delete(ctx, sts, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete StatefulSet %s: %w", name, err)
			}
		}
	}
	for _, claim := range stale {
		pending = append(pending, "claim "+claim.Name)
		if claim.DeletionTimestamp == nil {
			// pvc-protection keeps the claim until its pod is gone; the UID makes sure the claim
			// deleted is the one inspected above.
			if err := r.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return nil, fmt.Errorf("delete claim %s: %w", claim.Name, err)
			}
		}
	}
	return pending, nil
}

// replaceControlDB replaces the control DB's CNPG Cluster when its volume cannot reach the declared
// storage in place, and returns what is still being deleted. The class is judged by the Cluster's
// claims, so a Cluster created without a class whose claims are already on the declared one is kept.
// The PostgresSchemas and the c1 row's Job record work done against the old database: they are deleted
// before the Cluster, and again on every pass until it and its claims are gone, so the ordinary
// reconcile creates them again and they run on the new database.
func (r *OramaCloudV1Reconciler) replaceControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, v managedVolume) ([]string, error) {
	name := oc.Name + "-controldb"
	desired := volumeSize(v.storage, v.fallback)
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(oc.Namespace), client.MatchingLabels{cnpgClusterLabel: name}); err != nil {
		return nil, fmt.Errorf("list claims of Cluster %s: %w", name, err)
	}
	var stale []*corev1.PersistentVolumeClaim
	terminating := false
	for i := range claims.Items {
		claim := &claims.Items[i]
		if owner := metav1.GetControllerOf(claim); owner != nil && !(owner.Kind == "Cluster" && owner.Name == name) {
			return nil, fmt.Errorf("claim %s carries the control DB's label but is controlled by %s %s: it is never replaced", claim.Name, owner.Kind, owner.Name)
		}
		if claim.DeletionTimestamp != nil {
			terminating = true
		}
		if volumeNeedsReplacement(v.storage, desired, claim.Spec.Resources.Requests[corev1.ResourceStorage], claim.Spec.StorageClassName) {
			stale = append(stale, claim)
		}
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	getErr := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, cluster)
	exists := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) && !meta.IsNoMatchError(getErr) {
		return nil, fmt.Errorf("get Cluster %s: %w", name, getErr)
	}
	replacing := len(stale) > 0 || terminating
	if exists {
		if !metav1.IsControlledBy(cluster, oc) {
			return nil, fmt.Errorf("Cluster %s is not controlled by this OramaCloudV1: its volume is never replaced", name)
		}
		if cluster.GetDeletionTimestamp() != nil {
			replacing = true
		} else if !replacing {
			// The declared size against the Cluster's; with no claims yet, the class against its spec.
			size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
			current, err := resource.ParseQuantity(size)
			if err != nil {
				return nil, fmt.Errorf("Cluster %s storage size %q: %w", name, size, err)
			}
			shrink := v.storage.Size != nil && desired.Cmp(current) < 0
			class, found, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
			classPtr := &class
			if !found {
				classPtr = nil
			}
			replacing = shrink || (len(claims.Items) == 0 && volumeNeedsReplacement(v.storage, current, current, classPtr))
		}
	}
	if !replacing {
		return nil, nil
	}

	var schemas oramav1alpha1.PostgresSchemaList
	if err := r.List(ctx, &schemas, client.InNamespace(oc.Namespace)); err != nil {
		return nil, fmt.Errorf("list PostgresSchemas: %w", err)
	}
	for i := range schemas.Items {
		if err := r.deleteOwned(ctx, oc, &schemas.Items[i]); err != nil {
			return nil, err
		}
	}
	if err := r.deleteOwned(ctx, oc, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: controlDBClusterRowJobName(oc), Namespace: oc.Namespace}}); err != nil {
		return nil, err
	}

	var pending []string
	if exists {
		pending = append(pending, "Cluster "+name)
		if cluster.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, cluster, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete Cluster %s: %w", name, err)
			}
		}
	}
	// CNPG's ownership usually removes the claims with the Cluster; one left behind that differs from
	// the declared storage is replaced here, so a new Cluster cannot take it back by name. A claim
	// already on the declared storage is kept.
	for i := range claims.Items {
		claim := &claims.Items[i]
		switch {
		case claim.DeletionTimestamp != nil:
			pending = append(pending, "claim "+claim.Name)
		case slices.Contains(stale, claim):
			pending = append(pending, "claim "+claim.Name)
			if err := r.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return nil, fmt.Errorf("delete claim %s: %w", claim.Name, err)
			}
		}
	}
	return pending, nil
}

// volumeNeedsReplacement says whether an existing volume cannot reach the declared storage in place:
// the declared class names another class, or the declared size is smaller. Only what is declared
// counts: an omitted size never replaces a volume grown past the 10Gi default, and an omitted class
// never replaces one on a non-default class.
func volumeNeedsReplacement(storage *oramav1alpha1.VolumeSpec, desired, current resource.Quantity, class *string) bool {
	if storage == nil {
		return false
	}
	if storage.Size != nil && desired.Cmp(current) < 0 {
		return true
	}
	if storage.StorageClassName == nil {
		return false
	}
	return class == nil || *class != *storage.StorageClassName
}

// statefulSetTemplateNeedsReplacement says whether a StatefulSet's data claim template names a class
// other than the declared one, so it would make new claims on the old class.
func statefulSetTemplateNeedsReplacement(storage *oramav1alpha1.VolumeSpec, sts *appsv1.StatefulSet) bool {
	if storage == nil || storage.StorageClassName == nil {
		return false
	}
	for _, template := range sts.Spec.VolumeClaimTemplates {
		if template.Name == "data" {
			return template.Spec.StorageClassName == nil || *template.Spec.StorageClassName != *storage.StorageClassName
		}
	}
	return false
}

// deleteOwned deletes one object this stack controls, if it exists and is not already going; the
// ordinary reconcile creates it again.
func (r *OramaCloudV1Reconciler) deleteOwned(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, oc) || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", obj.GetName(), err)
	}
	return nil
}
