// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Customer stores (lake#196; the contract is lake's docs/control-plane/storage.md, migration
// 0028_stores). They follow the managed-services model: lake writes the stores table and never
// talks to Kubernetes; on the services poll, after the services, the operator makes each present
// store exist, writes its connection to <slug>.json in lake-<lake>-services beside the services'
// connections, deletes absent ones, and writes what it observed to store_status. The pairs it runs:
//   - the cluster volume: a PersistentVolumeClaim <lake>-<slug> on the StorageClass the Lake's
//     services use;
//   - the customer's own bucket (r2, s3, b2, gcs, azure; lake#664, lake_stores_buckets.go): a
//     LakeSealedSecret <lake>-<slug> in the Lake namespace that delivers the workspace's credential
//     from lake's secret store. The bucket itself is the customer's and is never touched;
//   - the Lake-run pairs (zetta, lake): kept by Lake in the workspace, nothing in Kubernetes.
//
// Every other type × provider pair (the network filesystems) is reported Failed.
//
// These are a customer's stores. The Lake's own libliquid store root (reconcileLakeStore,
// lake-<lake>-store) is a different thing and is not touched here.
const (
	storeTypeVolume      = "volume"
	storeProviderCluster = "cluster"

	// storeIDLabel is the ownership key of a store's resource, with the Lake as its controller.
	storeIDLabel = "orama.cloud/store-id"

	storeDefaultSizeGi = 10
	// storeMaxSizeGi guards against a row written outside lake's own checks.
	storeMaxSizeGi = 1024

	storeReadyUnboundMessage = "created; binds when a service mounts it"
	storeReadyBoundMessage   = "bound"
)

var storeSlugPattern = regexp.MustCompile(`^st-[0-9a-f]{12}$`)

// errStoresTableMissing: lake's schema has not reached 0028_stores yet. Stores are skipped and
// services go on.
var errStoresTableMissing = errors.New("the stores table does not exist yet (lake schema before 0028_stores)")

// LakeStore is one row of lake's stores table joined to its store_status row.
type LakeStore struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	Type               string
	Provider           string
	Name               string
	Slug               string
	// Spec is the raw jsonb spec: a volume's {"size_gi", "region", "public"}, a bucket's
	// {"public", "bucket", "endpoint", "region", "prefix", "credential"}, a Lake-run store's
	// {"public"} (and "repository_id" for provider lake).
	Spec       string
	Desired    string
	Generation int64
	// Status is the current store_status row, nil when the operator has written none.
	Status *LakeStoreStatus
}

// LakeStoreStatus is one store_status row, written only by the operator.
type LakeStoreStatus struct {
	StoreID            string
	Phase              string
	Message            string
	Endpoint           string
	ObservedGeneration int64
}

type lakeStoreSpec struct {
	SizeGi int64
}

// lakeStoresPass is what one pass did with the stores: the slugs whose connection the services
// Secret keeps, the fresh connections, and the result of every present store.
type lakeStoresPass struct {
	// read is false when the stores table could not be read; the Secret then keeps every store
	// key it has, since a failed read must never remove a connection lake depends on.
	read        bool
	present     map[string]bool
	credentials map[string][]byte
	stores      []*LakeStore
	results     map[string]serviceResult
	changing    bool
}

func storeResourceName(lake, slug string) string { return lake + "-" + slug }

// storeVolumeEndpoint is what store_status.endpoint carries for a cluster volume: the claim, as
// <namespace>/<name>. A volume has no network address; the claim is how a service mounts it.
func storeVolumeEndpoint(namespace, claim string) string { return namespace + "/" + claim }

func storeUnavailableMessage(st *LakeStore) string {
	return fmt.Sprintf("%s %s stores are not available on this lake yet", st.Provider, st.Type)
}

func foreignStoreMessage(namespace, name string) string {
	return fmt.Sprintf("PersistentVolumeClaim %s/%s exists and is not this Lake's store", namespace, name)
}

// storeStorageClass is the StorageClass of the Lake's services (spec.workspaces.storage
// .storageClassName, which every Zetta service's data volumes use); "" is the cluster default.
func storeStorageClass(lake *oramav1alpha1.Lake) string {
	return lake.Spec.Workspaces.Storage.StorageClassName
}

// parseStoreSpec reads spec {size_gi, region, public}. Only size_gi matters to a cluster volume.
func parseStoreSpec(raw string) (lakeStoreSpec, error) {
	spec := lakeStoreSpec{SizeGi: storeDefaultSizeGi}
	if strings.TrimSpace(raw) == "" {
		return spec, nil
	}
	var fields struct {
		SizeGi *int64 `json:"size_gi"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return spec, fmt.Errorf("spec is not a JSON object with an integer size_gi: %v", err)
	}
	if fields.SizeGi != nil {
		if *fields.SizeGi < 1 || *fields.SizeGi > storeMaxSizeGi {
			return spec, fmt.Errorf("spec.size_gi must be between 1 and %d", storeMaxSizeGi)
		}
		spec.SizeGi = *fields.SizeGi
	}
	return spec, nil
}

// storeAvailable reports whether the operator runs this type × provider pair.
func storeAvailable(st *LakeStore) bool {
	return storeIsVolume(st) || storeIsBucket(st) || storeIsLakeRun(st)
}

func storeIsVolume(st *LakeStore) bool {
	return st.Type == storeTypeVolume && st.Provider == storeProviderCluster
}

// validateStore rejects a row the operator will not act on. The table's own constraints make most
// of these unreachable; they are checked again because a wrong name here would touch another
// resource.
func validateStore(lake *oramav1alpha1.Lake, st *LakeStore) error {
	switch {
	case !storeSlugPattern.MatchString(st.Slug):
		return fmt.Errorf("slug must match %s", storeSlugPattern.String())
	case st.CustomerID == "":
		return fmt.Errorf("customer_id is required")
	case st.WorkspaceID == "" || st.WorkspaceNamespace == "":
		return fmt.Errorf("workspace_id and its reconciled namespace are required")
	case len(validation.IsDNS1123Label(st.WorkspaceNamespace)) != 0:
		return fmt.Errorf("workspace namespace is invalid")
	case len(validation.IsDNS1123Subdomain(storeResourceName(lake.Name, st.Slug))) > 0:
		return fmt.Errorf("lake name %q is too long for store resource names", lake.Name)
	}
	return nil
}

// storeOwned: controlled by this Lake and labelled with this store's id.
func storeOwned(lake *oramav1alpha1.Lake, obj metav1.Object, st *LakeStore) bool {
	return metav1.IsControlledBy(obj, lake) && obj.GetLabels()[storeIDLabel] == st.ID
}

// reconcileLakeStores applies one read of the stores table: absent stores first, then present
// stores oldest first. It writes no status of a present store: that waits for the services Secret,
// since a store is Ready only once its connection is there (finishLakeStores).
func (r *LakeReconciler) reconcileLakeStores(ctx context.Context, lake *oramav1alpha1.Lake, credentialNamespace string, labels map[string]string, store LakeServiceStore, stores []LakeStore, read bool) lakeStoresPass {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	pass := lakeStoresPass{read: read, present: map[string]bool{}, credentials: map[string][]byte{}, results: map[string]serviceResult{}}
	for i := range stores {
		st := &stores[i]
		switch st.Desired {
		case serviceDesiredAbsent:
			// History: lake hides it and the operator skips it.
			if st.Status != nil && st.Status.Phase == ServicePhaseDeleted {
				continue
			}
			pass.changing = true
			if st.WorkspaceID == "" || len(validation.IsDNS1123Label(st.WorkspaceNamespace)) != 0 {
				result := serviceResult{phase: ServicePhaseFailed, message: "workspace_id and its reconciled namespace are required"}
				if err := writeStoreStatus(ctx, store, st, result); err != nil {
					log.Error(err, "store status write deferred", "store", st.ID)
				}
				continue
			}
			result, err := r.deleteCustomerStore(ctx, lake, st.WorkspaceNamespace, credentialNamespace, st)
			if err != nil {
				log.Error(err, "store deletion deferred", "store", st.ID, "slug", st.Slug)
				continue
			}
			if err := writeStoreStatus(ctx, store, st, result); err != nil {
				log.Error(err, "store status write deferred", "store", st.ID)
			}
		case serviceDesiredPresent:
			pass.present[st.Slug] = true
			pass.stores = append(pass.stores, st)
			workspaceLabels := withLabels(labels, map[string]string{workspaceLabel: st.WorkspaceID})
			result, err := r.reconcileCustomerStore(ctx, lake, st.WorkspaceNamespace, credentialNamespace, workspaceLabels, st)
			if err != nil {
				log.Error(err, "store reconcile deferred", "store", st.ID, "slug", st.Slug)
				pass.changing = true
				continue
			}
			pass.results[st.ID] = result
			if result.credential != nil {
				pass.credentials[st.Slug] = result.credential
			}
		default:
			result := serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("desired must be %s or %s", serviceDesiredPresent, serviceDesiredAbsent)}
			if err := writeStoreStatus(ctx, store, st, result); err != nil {
				log.Error(err, "store status write deferred", "store", st.ID)
			}
		}
	}
	return pass
}

// finishLakeStores writes the status of every present store once the services Secret has been
// written: Ready only when the store's connection is in it.
func finishLakeStores(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, pass *lakeStoresPass, written map[string]bool) {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	for _, st := range pass.stores {
		result, found := pass.results[st.ID]
		if !found {
			continue
		}
		if result.resourceReady {
			if written[st.Slug] {
				result.phase = ServicePhaseReady
			} else {
				result.phase, result.message = ServicePhaseProvisioning, "delivering the connection"
			}
		}
		if result.phase == ServicePhaseProvisioning {
			pass.changing = true
		}
		if err := writeStoreStatus(ctx, store, st, result); err != nil {
			log.Error(err, "store status write deferred", "store", st.ID)
		}
	}
}

// writeStoreStatus upserts store_status only when phase, message, endpoint or the observed
// generation differ from the row already there, so a steady store produces no database writes.
func writeStoreStatus(ctx context.Context, store LakeServiceStore, st *LakeStore, result serviceResult) error {
	next := LakeStoreStatus{
		StoreID:            st.ID,
		Phase:              result.phase,
		Message:            result.message,
		Endpoint:           result.endpoint,
		ObservedGeneration: st.Generation,
	}
	if st.Status != nil && *st.Status == next {
		return nil
	}
	if err := store.UpsertStoreStatus(ctx, next); err != nil {
		return err
	}
	st.Status = &next
	return nil
}

// reconcileCustomerStore makes one present store exist and reports what it observed. namespace is
// the store's workspace namespace (a volume's claim); lakeNamespace is the Lake's own, where a
// bucket's credential is delivered beside the services Secret.
func (r *LakeReconciler) reconcileCustomerStore(ctx context.Context, lake *oramav1alpha1.Lake, namespace, lakeNamespace string, labels map[string]string, st *LakeStore) (serviceResult, error) {
	if err := validateStore(lake, st); err != nil {
		return serviceResult{phase: ServicePhaseFailed, message: err.Error()}, nil
	}
	switch {
	case storeIsVolume(st):
		spec, err := parseStoreSpec(st.Spec)
		if err != nil {
			return serviceResult{phase: ServicePhaseFailed, message: err.Error()}, nil
		}
		return r.reconcileVolumeStore(ctx, lake, namespace, labels, st, spec)
	case storeIsBucket(st):
		spec, err := parseBucketSpec(st.Spec)
		if err != nil {
			return serviceResult{phase: ServicePhaseFailed, message: err.Error()}, nil
		}
		return r.reconcileBucketStore(ctx, lake, lakeNamespace, labels, st, spec)
	case storeIsLakeRun(st):
		return lakeRunStoreResult(st)
	}
	return serviceResult{phase: ServicePhaseFailed, message: storeUnavailableMessage(st)}, nil
}

// storeClaimLabels are the Lake's labels plus the Lake and the store id.
func storeClaimLabels(lake *oramav1alpha1.Lake, labels map[string]string, st *LakeStore) map[string]string {
	out := make(map[string]string, len(labels)+2)
	for key, value := range labels {
		out[key] = value
	}
	out[serviceLakeLabel] = lake.Name
	out[storeIDLabel] = st.ID
	out[workspaceLabel] = st.WorkspaceID
	return out
}

// reconcileVolumeStore creates the cluster volume's claim once. An existing claim is never
// rewritten: its size, class and access mode are what they were at creation, and a claim is never
// shrunk. The store is resource-ready as soon as the claim exists: AKS managed-csi binds on first
// use (WaitForFirstConsumer), so Pending is the expected state until a service mounts it.
func (r *LakeReconciler) reconcileVolumeStore(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, st *LakeStore, spec lakeStoreSpec) (serviceResult, error) {
	name := storeResourceName(lake.Name, st.Slug)
	endpoint := storeVolumeEndpoint(namespace, name)
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, claim); err == nil {
		if !storeOwned(lake, claim, st) {
			return serviceResult{phase: ServicePhaseFailed, message: foreignStoreMessage(namespace, name)}, nil
		}
		if claim.DeletionTimestamp != nil {
			return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the previous volume to be deleted"}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	} else {
		claim = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: storeClaimLabels(lake, labels, st),
				Annotations: map[string]string{serviceCustomerAnnotation: st.CustomerID}},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(spec.SizeGi<<30, resource.BinarySI),
				}},
			},
		}
		if class := storeStorageClass(lake); class != "" {
			claim.Spec.StorageClassName = &class
		}
		if err := controllerutil.SetControllerReference(lake, claim, r.Scheme); err != nil {
			return serviceResult{}, err
		}
		if err := r.Create(ctx, claim); err != nil {
			return serviceResult{}, fmt.Errorf("create store volume %s/%s: %w", namespace, name, err)
		}
	}

	message := storeReadyUnboundMessage
	if claim.Status.Phase == corev1.ClaimBound {
		message = storeReadyBoundMessage
	}
	// The class and size are the claim's own: the class the API server's default admission chose
	// when none was asked for, and the size it was created with.
	class := ""
	if claim.Spec.StorageClassName != nil {
		class = *claim.Spec.StorageClassName
	}
	size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	credential, err := json.Marshal(struct {
		Type         string `json:"type"`
		Provider     string `json:"provider"`
		Claim        string `json:"claim"`
		Namespace    string `json:"namespace"`
		SizeGi       int64  `json:"size_gi"`
		StorageClass string `json:"storage_class"`
	}{storeTypeVolume, storeProviderCluster, name, namespace, size.Value() >> 30, class})
	if err != nil {
		return serviceResult{}, err
	}
	return serviceResult{phase: ServicePhaseProvisioning, message: message, endpoint: endpoint, resourceReady: true, credential: credential}, nil
}

// deleteCustomerStore deletes an absent store: for a cluster volume its claim, and so its data
// (lake asks for a typed confirmation first); then its connection. It reports Deleted only once
// the claim is gone; until then every poll repeats the same steps. Only a claim this Lake owns for
// this store is ever deleted: a foreign claim of the same name is left in place.
func (r *LakeReconciler) deleteCustomerStore(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	credentialNamespace string,
	st *LakeStore,
) (serviceResult, error) {
	if !storeSlugPattern.MatchString(st.Slug) {
		return serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("slug must match %s", storeSlugPattern.String())}, nil
	}
	deleted := serviceResult{phase: ServicePhaseDeleted, message: "store deleted"}
	if storeIsBucket(st) {
		return r.deleteBucketStore(ctx, lake, credentialNamespace, st)
	}
	if storeIsVolume(st) {
		name := storeResourceName(lake.Name, st.Slug)
		key := client.ObjectKey{Namespace: namespace, Name: name}
		claim := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, key, claim); err == nil {
			if !storeOwned(lake, claim, st) {
				deleted.message = "store deleted; " + foreignStoreMessage(namespace, name) + " and was left in place"
			} else if claim.DeletionTimestamp == nil {
				if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
					return serviceResult{}, fmt.Errorf("delete store volume %s: %w", key, err)
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return serviceResult{}, err
		}
		if err := r.removeServiceCredential(ctx, lake, credentialNamespace, st.Slug); err != nil {
			return serviceResult{}, err
		}
		remaining := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, key, remaining); err == nil {
			if storeOwned(lake, remaining, st) {
				return serviceResult{phase: ServicePhaseDeleting, endpoint: storeVolumeEndpoint(namespace, name), message: "deleting the volume"}, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return serviceResult{}, err
		}
		return deleted, nil
	}
	// A Lake-run store has nothing in Kubernetes (Lake removes its contents itself at delete_store),
	// and an unavailable pair never had a resource: only a connection could be left.
	if err := r.removeServiceCredential(ctx, lake, credentialNamespace, st.Slug); err != nil {
		return serviceResult{}, err
	}
	return deleted, nil
}
