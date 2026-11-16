// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// defaultVolumeSize is the data volume of a component whose storage.size is unset; a reader's
// default is readerDataSize.
const defaultVolumeSize = "10Gi"

// volumeSize is storage.size, or fallback when it is unset.
func volumeSize(storage *oramav1alpha1.VolumeSpec, fallback string) resource.Quantity {
	if storage != nil && storage.Size != nil {
		return storage.Size.DeepCopy()
	}
	return resource.MustParse(fallback)
}

// dataPVC is a StatefulSet's "data" claim template: ReadWriteOnce, storage.size (fallback when
// unset) and storage.storageClassName when set. The API makes templates immutable, so callers set
// it only before creation; reconcileStatefulStorage grows the claims made from it.
func dataPVC(storage *oramav1alpha1.VolumeSpec, fallback string) corev1.PersistentVolumeClaim {
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: volumeSize(storage, fallback)}},
		},
	}
	if storage != nil && storage.StorageClassName != nil {
		class := *storage.StorageClassName
		claim.Spec.StorageClassName = &class
	}
	return claim
}

func controlDBStorage(storage *oramav1alpha1.VolumeSpec) map[string]interface{} {
	size := volumeSize(storage, defaultVolumeSize)
	result := map[string]interface{}{"size": size.String()}
	if storage != nil && storage.StorageClassName != nil {
		result["storageClass"] = *storage.StorageClassName
	}
	return result
}

// storageResult collects one line per component and resource. A non-empty problem
// (ShrinkNotSupported, StorageClassChangeNotSupported, StorageUpdateFailed, ClaimsPending)
// prefixes its line and makes StorageApplied False.
type storageResult struct {
	messages []string
	failed   bool
}

func (s *storageResult) add(component, problem, message string) {
	if problem != "" {
		s.failed = true
		message = problem + ": " + message
	}
	s.messages = append(s.messages, component+": "+message)
}

// reconcileStorage applies storage independently of workload readiness. It reports through its
// own StorageApplied condition rather than Ready, so a refused shrink or a resize in progress never
// marks a serving stack not Ready. The return value keeps the readiness backstop running even when
// workloads are Ready, including for late/retained PVCs.
// controlDBVolume names the control DB's volume, CNPG's, in managedVolumes.
const controlDBVolume = "controlDB"

// managedVolume is one locally managed data volume: a StatefulSet's "data" claims
// (<cr>-<component>), or the control DB's CNPG storage.
type managedVolume struct {
	component string
	storage   *oramav1alpha1.VolumeSpec
	fallback  string
}

// managedVolumes lists the stack's locally managed volumes: the primary's writer, the readers of an
// enabled engine, the in-cluster analytics/broker/cache/search stores this region runs, HARVEST's
// Mongo while HARVEST runs, and the control DB. External stores and disabled components have none.
func managedVolumes(oc *oramav1alpha1.OramaCloudV1) []managedVolume {
	var volumes []managedVolume
	if oc.Spec.Components.Engine.Enabled && oc.Spec.Region.Role == "primary" {
		volumes = append(volumes, managedVolume{"writer", oc.Spec.Components.Engine.Writer.Storage, defaultVolumeSize})
	}
	if oc.Spec.Components.Engine.Enabled {
		volumes = append(volumes, managedVolume{"reader", oc.Spec.Components.Engine.Reader.Storage, readerDataSize})
	}
	for _, store := range []struct {
		name string
		spec oramav1alpha1.DataStoreSpec
	}{
		{"analytics", oc.Spec.Components.Analytics}, {"broker", oc.Spec.Components.Broker},
		{"cache", oc.Spec.Components.Cache}, {"search", oc.Spec.Components.Search},
	} {
		if !store.spec.Enabled || store.spec.Mode == "external" || (store.spec.Placement == "central" && oc.Spec.Region.Role == "read") {
			continue
		}
		volumes = append(volumes, managedVolume{store.name, store.spec.Storage, defaultVolumeSize})
	}
	if ing := oc.Spec.Components.Ingestion; ing.Enabled && ing.Harvest {
		volumes = append(volumes, managedVolume{"harvest-mongo", ing.HarvestMongoStorage, defaultVolumeSize})
	}
	if db := oc.Spec.Components.ControlDB; db.Enabled && db.Mode != "external" {
		volumes = append(volumes, managedVolume{controlDBVolume, db.Storage, defaultVolumeSize})
	}
	return volumes
}

func (r *OramaCloudV1Reconciler) reconcileStorage(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) bool {
	result := &storageResult{}
	volumes := managedVolumes(oc)
	for _, v := range volumes {
		if v.component == controlDBVolume {
			r.reconcileControlDBStorage(ctx, oc, v.storage, result)
			continue
		}
		r.reconcileStatefulStorage(ctx, oc, v.component, v.storage, v.fallback, result)
	}
	managed := len(volumes) > 0
	sort.Strings(result.messages)
	cond := metav1.Condition{Type: "StorageApplied", Status: metav1.ConditionTrue, Reason: "Applied"}
	if result.failed {
		cond.Status, cond.Reason = metav1.ConditionFalse, "NotApplied"
	}
	cond.Message = strings.Join(result.messages, "; ")
	if cond.Message == "" {
		cond.Message = "No locally managed storage"
	}
	r.setStatus(ctx, oc, oc.Status.Phase, oc.Status.ComponentsReady, oc.Status.Components, nil, cond)
	return managed
}

// storageDifferences reports every unsupported change before any write is attempted. Capacity
// is evidence only: provisioners can round it above a numerically equal requested size.
func storageDifferences(component, target string, desired resource.Quantity, storage *oramav1alpha1.VolumeSpec, current resource.Quantity, class, evidence string, result *storageResult) bool {
	// Neither difference can be applied in place; replacing the volume needs approveDataDeletion
	// (reconcileVolumeReplacement), which this pass only reaches without it.
	const replace = "; replacing the volume erases its data and needs storage.approveDataDeletion"
	rejected := false
	if desired.Cmp(current) < 0 {
		result.add(component, "ShrinkNotSupported", fmt.Sprintf("requested %s, %s requests %s%s%s", desired.String(), target, current.String(), evidence, replace))
		rejected = true
	}
	if storage != nil && storage.StorageClassName != nil && *storage.StorageClassName != class {
		result.add(component, "StorageClassChangeNotSupported", fmt.Sprintf("requested %s, %s uses %q%s", *storage.StorageClassName, target, class, replace))
		rejected = true
	}
	return rejected
}

// claimOrdinal accepts only a complete StatefulSet data claim name, including retained ordinals.
func claimOrdinal(name, prefix string) (int64, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	ordinal, err := strconv.ParseInt(suffix, 10, 32)
	return ordinal, err == nil && ordinal >= 0 && strconv.FormatInt(ordinal, 10) == suffix
}

func (r *OramaCloudV1Reconciler) reconcileStatefulStorage(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, component string, storage *oramav1alpha1.VolumeSpec, fallback string, result *storageResult) {
	key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-" + component}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, key, sts); err != nil {
		reason := "StorageUpdateFailed"
		if apierrors.IsNotFound(err) {
			reason = "ClaimsPending"
		}
		result.add(component, reason, fmt.Sprintf("StatefulSet %s: %v", key.Name, err))
		return
	}
	if !metav1.IsControlledBy(sts, oc) {
		result.add(component, "StorageUpdateFailed", fmt.Sprintf("StatefulSet %s is not controlled by this OramaCloudV1", sts.Name))
		return
	}
	hasData := false
	for _, template := range sts.Spec.VolumeClaimTemplates {
		if template.Name == "data" {
			hasData = true
		}
	}
	if !hasData {
		result.add(component, "StorageUpdateFailed", fmt.Sprintf("StatefulSet %s has no data claim template", sts.Name))
		return
	}
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(oc.Namespace)); err != nil {
		result.add(component, "StorageUpdateFailed", fmt.Sprintf("list claims for StatefulSet %s: %v", sts.Name, err))
		return
	}
	prefix := "data-" + sts.Name + "-"
	found := map[int64]bool{}
	desired := volumeSize(storage, fallback)
	for i := range claims.Items {
		claim := &claims.Items[i]
		ordinal, matches := claimOrdinal(claim.Name, prefix)
		if !matches {
			continue
		}
		target := "claim " + claim.Name
		// Claims usually have no owner under StatefulSet's default retention policy.
		// Refuse CNPG ownership even without a controller reference, and refuse any
		// conflicting controller rather than adopting its volume.
		foreignOwner := false
		for _, owner := range claim.OwnerReferences {
			if owner.Kind == "Cluster" && strings.HasPrefix(owner.APIVersion, cnpgClusterGVK.Group+"/") {
				foreignOwner = true
			}
		}
		if owner := metav1.GetControllerOf(claim); owner != nil &&
			!(owner.Kind == "StatefulSet" && owner.UID == sts.UID) &&
			!(owner.Kind == "Pod" && owner.Name == fmt.Sprintf("%s-%d", sts.Name, ordinal)) {
			foreignOwner = true
		}
		if foreignOwner {
			result.add(component, "StorageUpdateFailed", target+" is owned by another workload")
			continue
		}
		found[ordinal] = true
		current, exists := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if !exists {
			result.add(component, "StorageUpdateFailed", target+" has no storage request")
			continue
		}
		class := ""
		if claim.Spec.StorageClassName != nil {
			class = *claim.Spec.StorageClassName
		}
		evidence := ""
		if capacity, ok := claim.Status.Capacity[corev1.ResourceStorage]; ok {
			evidence = fmt.Sprintf(" (capacity %s)", capacity.String())
		}
		if storageDifferences(component, target, desired, storage, current, class, evidence, result) {
			continue
		}
		if desired.Cmp(current) > 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = desired.DeepCopy()
			if err := r.Patch(ctx, claim, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				result.add(component, "StorageUpdateFailed", fmt.Sprintf("%s: %v", target, err))
				continue
			}
		}
		result.add(component, "", fmt.Sprintf("%s requests %s", target, desired.String()))
	}
	start := int64(0)
	if sts.Spec.Ordinals != nil {
		start = int64(sts.Spec.Ordinals.Start)
	}
	for ordinal := start; ordinal < start+int64(replicasOf(sts.Spec.Replicas)); ordinal++ {
		if !found[ordinal] {
			result.add(component, "ClaimsPending", fmt.Sprintf("claim %s%d is not present", prefix, ordinal))
		}
	}
}

func (r *OramaCloudV1Reconciler) reconcileControlDBStorage(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, storage *oramav1alpha1.VolumeSpec, result *storageResult) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb"}
	target := "Cluster " + key.Name
	if err := r.Get(ctx, key, cluster); err != nil {
		result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s: %v", target, err))
		return
	}
	if !metav1.IsControlledBy(cluster, oc) {
		result.add("controlDB", "StorageUpdateFailed", target+" is not controlled by this OramaCloudV1")
		return
	}
	size, _, err := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	if err != nil {
		result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s storage size: %v", target, err))
		return
	}
	current, err := resource.ParseQuantity(size)
	if err != nil {
		result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s storage size: %v", target, err))
		return
	}
	class, _, err := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	if err != nil {
		result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s storage class: %v", target, err))
		return
	}
	desired := volumeSize(storage, defaultVolumeSize)
	if storageDifferences("controlDB", target, desired, storage, current, class, "", result) {
		return
	}
	if desired.Cmp(current) > 0 {
		before := cluster.DeepCopy()
		if err := unstructured.SetNestedField(cluster.Object, desired.String(), "spec", "storage", "size"); err != nil {
			result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s: %v", target, err))
			return
		}
		if err := r.Patch(ctx, cluster, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			result.add("controlDB", "StorageUpdateFailed", fmt.Sprintf("%s: %v", target, err))
			return
		}
	}
	result.add("controlDB", "", fmt.Sprintf("%s requests %s", target, desired.String()))
}
