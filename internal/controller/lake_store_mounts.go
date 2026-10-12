/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Store mounts (lake#666, lake migration 0080_store_mounts; the contract is lake's
// docs/control-plane/storage.md#mounts). lake records in store_mounts which services and apps mount
// a store; the operator mounts each into the pods it builds for that workload:
//   - a volume (volume/cluster) is its PersistentVolumeClaim, mounted at mount_path, read-only when
//     read_only is set. It is ReadWriteOnce, so lake lets one workload mount it;
//   - a cloud bucket (r2, s3, b2, gcs, azure) is its connection as environment variables,
//     STORE_<NAME>_BUCKET, _ENDPOINT, _REGION, _PREFIX and the credential fields from the Secret
//     its LakeSealedSecret delivered (lake_stores_buckets.go), copied into the workload's
//     namespace;
//   - a Lake-run store is not mountable (lake refuses it) and is skipped here.
//
// The workloads whose pods the operator builds: apps (LakeApp) and the wrapper service kinds
// (sqlite, duckdb, polars; lake_service_wrapper.go). Any change to a workload's mounts, or a new
// credential version, changes its pod template and so rolls its pods.
const (
	StoreMountKindService = "service"
	StoreMountKindApp     = "app"

	// storeCredentialsCopyLabel marks a bucket credential copied into a workspace namespace for the
	// services that mount it.
	storeCredentialsCopyLabel = "orama.cloud/store-credentials"
	// storeCredentialsAnnotation on a pod template is the digest of the bucket credentials the
	// pods read, so a new credential version rolls them.
	storeCredentialsAnnotation = "orama.cloud/store-credentials-digest"

	storeVolumePrefix = "store-"
)

// storeBucketCredentialFields are the Secret keys a bucket's credential may carry, and the
// environment suffix each is given. Each is optional: S3 protocol stores carry the first three,
// Azure the last two.
var storeBucketCredentialFields = []struct{ key, suffix string }{
	{"access_key_id", "ACCESS_KEY_ID"},
	{"secret_access_key", "SECRET_ACCESS_KEY"},
	{"session_token", "SESSION_TOKEN"},
	{"account_key", "ACCOUNT_KEY"},
	{"sas_token", "SAS_TOKEN"},
}

// storeMountPathPattern is lake's store_mount_path_check.
var storeMountPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)

// errStoreMountsTableMissing: lake's schema has not reached 0080_store_mounts yet. Nothing is
// mounted and no used space is reported; everything else goes on.
var errStoreMountsTableMissing = errors.New("the store_mounts table does not exist yet (lake schema before 0080_store_mounts)")

// storeMountsState is what one services pass knows of the store mounts: read (the table exists
// and both it and the stores were read), unknown (the table exists but a read failed), or neither
// (a schema before 0080: nothing is mounted).
type storeMountsState struct {
	read    bool
	unknown bool
	mounts  []LakeStoreMount
}

// LakeStoreMount is one store_mounts row joined to its store.
type LakeStoreMount struct {
	StoreID    string
	ObjectKind string
	ObjectID   string
	MountPath  string
	ReadOnly   bool
	Slug       string
	Type       string
	Provider   string
	Name       string
	Desired    string
}

// workloadStoreMount is one store resolved for one workload: what its pods get.
type workloadStoreMount struct {
	StoreID  string
	Name     string
	Slug     string
	Type     string
	Provider string
	// A volume's claim (in the workload's namespace), path and mode.
	Claim     string
	MountPath string
	ReadOnly  bool
	// A bucket's connection and the delivered Secret, in the Lake namespace, holding its
	// credential fields.
	Bucket   string
	Endpoint string
	Region   string
	Prefix   string
	Secret   string
}

func (m workloadStoreMount) volume() bool { return m.Type == storeTypeVolume }

// resolveStoreMounts is the stores one workload mounts, by store name. A row the operator cannot
// honour is left out: a store that is absent, of another workspace or customer, not mountable, a
// volume without a valid path, or a second store at the same path.
func resolveStoreMounts(lake *oramav1alpha1.Lake, stores []LakeStore, mounts []LakeStoreMount, kind, id, customerID, workspaceID string) []workloadStoreMount {
	byID := make(map[string]*LakeStore, len(stores))
	for i := range stores {
		byID[stores[i].ID] = &stores[i]
	}
	var out []workloadStoreMount
	for _, mount := range mounts {
		if mount.ObjectKind != kind || mount.ObjectID != id {
			continue
		}
		st := byID[mount.StoreID]
		if st == nil || st.Desired != serviceDesiredPresent || st.WorkspaceID != workspaceID || st.CustomerID != customerID ||
			validateStore(lake, st) != nil {
			continue
		}
		resolved := workloadStoreMount{StoreID: st.ID, Name: st.Name, Slug: st.Slug, Type: st.Type, Provider: st.Provider}
		switch {
		case storeIsVolume(st):
			clean := path.Clean(mount.MountPath)
			if !storeMountPathPattern.MatchString(mount.MountPath) || clean == "/" || clean != mount.MountPath {
				continue
			}
			resolved.Claim = storeResourceName(lake.Name, st.Slug)
			resolved.MountPath = mount.MountPath
			resolved.ReadOnly = mount.ReadOnly
		case storeIsBucket(st):
			spec, err := parseBucketSpec(st.Spec)
			if err != nil {
				continue
			}
			resolved.Bucket, resolved.Endpoint, resolved.Region, resolved.Prefix = spec.Bucket, spec.Endpoint, spec.Region, spec.Prefix
			resolved.Secret = storeResourceName(lake.Name, st.Slug)
		default:
			continue
		}
		out = append(out, resolved)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// withoutReservedPaths drops a volume whose path is one the workload's own container already
// mounts, or that an earlier store took.
func withoutReservedPaths(mounts []workloadStoreMount, reserved ...string) []workloadStoreMount {
	taken := map[string]bool{}
	for _, value := range reserved {
		taken[value] = true
	}
	out := make([]workloadStoreMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.volume() {
			if taken[mount.MountPath] {
				continue
			}
			taken[mount.MountPath] = true
		}
		out = append(out, mount)
	}
	return out
}

// storeEnvPrefix is STORE_<NAME>_: the store name upper-cased, "-" to "_".
func storeEnvPrefix(name string) string {
	return "STORE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
}

// storeVolumeName is the pod volume of a mounted volume store.
func storeVolumeName(slug string) string { return storeVolumePrefix + slug }

// storeMountsPodParts is what the mounts add to a pod: the volumes, the container's volume mounts
// and its environment. secretName names, per store id, the Secret in the pod's namespace that
// holds a bucket's credential fields.
func storeMountsPodParts(mounts []workloadStoreMount, secretName func(workloadStoreMount) string) ([]corev1.Volume, []corev1.VolumeMount, []corev1.EnvVar) {
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	var env []corev1.EnvVar
	optional := true
	for _, mount := range mounts {
		if mount.volume() {
			volumes = append(volumes, corev1.Volume{Name: storeVolumeName(mount.Slug), VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: mount.Claim, ReadOnly: mount.ReadOnly}}})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: storeVolumeName(mount.Slug), MountPath: mount.MountPath, ReadOnly: mount.ReadOnly})
			continue
		}
		prefix := storeEnvPrefix(mount.Name)
		env = append(env,
			corev1.EnvVar{Name: prefix + "BUCKET", Value: mount.Bucket},
			corev1.EnvVar{Name: prefix + "ENDPOINT", Value: mount.Endpoint},
			corev1.EnvVar{Name: prefix + "REGION", Value: mount.Region},
			corev1.EnvVar{Name: prefix + "PREFIX", Value: mount.Prefix},
		)
		secret := secretName(mount)
		for _, field := range storeBucketCredentialFields {
			env = append(env, corev1.EnvVar{Name: prefix + field.suffix, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: field.key, Optional: &optional}}})
		}
	}
	return volumes, volumeMounts, env
}

func storeMountsHaveVolume(mounts []workloadStoreMount) bool {
	for _, mount := range mounts {
		if mount.volume() {
			return true
		}
	}
	return false
}

// storeCredentialFields keeps only the credential fields a bucket's Secret may carry.
func storeCredentialFields(data map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for _, field := range storeBucketCredentialFields {
		if value, found := data[field.key]; found {
			out[field.key] = append([]byte(nil), value...)
		}
	}
	return out
}

// storeCredentialsDigest is a stable digest of credential copies, keyed by Secret name.
func storeCredentialsDigest(copies map[string]map[string][]byte) string {
	names := make([]string, 0, len(copies))
	for name := range copies {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		data := copies[name]
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			hash.Write([]byte(key))
			hash.Write([]byte{0})
			hash.Write(data[key])
			hash.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// readStoreCredential reads a bucket store's delivered Secret <lake>-<slug> in the Lake namespace,
// and only that: the Secret must be controlled by a LakeSealedSecret that the Lake controls and that
// carries the store's id. found is false while it has not been delivered.
func readStoreCredential(ctx context.Context, reader client.Reader, lake *oramav1alpha1.Lake, namespace string, mount workloadStoreMount) (map[string][]byte, bool, error) {
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mount.Secret}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "LakeSealedSecret" || owner.Name != mount.Secret {
		return nil, false, fmt.Errorf("Secret %s/%s is not the store's delivered credential", namespace, mount.Secret)
	}
	sealed := &oramav1alpha1.LakeSealedSecret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.Name}, sealed); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if sealed.UID != owner.UID || !metav1.IsControlledBy(sealed, lake) || sealed.Labels[storeIDLabel] != mount.StoreID {
		return nil, false, fmt.Errorf("Secret %s/%s is not the store's delivered credential", namespace, mount.Secret)
	}
	return storeCredentialFields(secret.Data), true, nil
}

// storeCredentialsCopyName is the copy of a bucket's credential in a workspace namespace, for the
// services there that mount it. The suffix keeps it apart from the delivered Secret <lake>-<slug>
// should a workspace namespace ever be the Lake namespace.
func storeCredentialsCopyName(lake, slug string) string {
	return storeResourceName(lake, slug) + "-credentials"
}

// isWrapperServiceKind: the service kinds whose pod template the operator owns, the only ones that
// can mount a store.
func isWrapperServiceKind(kind string) bool {
	_, wrapper := catalogKinds[kind].(wrapperServiceKind)
	return wrapper
}

// reconcileServiceStoreMounts resolves the stores each present wrapper service mounts onto the
// service (svc.StoreMounts, used by its pod template) and keeps one copy of each mounted bucket's
// credential in the namespaces of the services that mount it, removing copies no service needs.
// mountsRead is false while lake's schema has no store_mounts table: nothing is mounted and no copy
// is removed.
func (r *LakeReconciler) reconcileServiceStoreMounts(ctx context.Context, lake *oramav1alpha1.Lake, lakeNamespace string, labels map[string]string,
	services []LakeService, present []*LakeService, stores []LakeStore, mounts []LakeStoreMount, mountsRead bool) error {
	if !mountsRead {
		return nil
	}
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	type copyKey struct{ namespace, name string }
	wanted := map[copyKey]map[string][]byte{}
	wantedStore := map[copyKey]workloadStoreMount{}
	namespaces := map[string]bool{}
	for i := range services {
		if services[i].WorkspaceNamespace != "" {
			namespaces[services[i].WorkspaceNamespace] = true
		}
	}
	for _, svc := range present {
		svc.StoreMounts, svc.StoreCredentialsDigest = nil, ""
		if !isWrapperServiceKind(svc.Kind) {
			continue
		}
		resolved := withoutReservedPaths(
			resolveStoreMounts(lake, stores, mounts, StoreMountKindService, svc.ID, svc.CustomerID, svc.WorkspaceID),
			wrapperDataDir, wrapperTempDir)
		digest := map[string]map[string][]byte{}
		buckets := false
		for _, mount := range resolved {
			if mount.volume() {
				continue
			}
			buckets = true
			data, found, err := readStoreCredential(ctx, r.Client, lake, lakeNamespace, mount)
			if err != nil {
				log.Info("store credential not copied", "store", mount.StoreID, "err", err.Error())
				continue
			}
			if !found {
				continue
			}
			name := storeCredentialsCopyName(lake.Name, mount.Slug)
			key := copyKey{svc.WorkspaceNamespace, name}
			wanted[key] = data
			wantedStore[key] = mount
			digest[name] = data
		}
		svc.StoreMounts = resolved
		if buckets {
			svc.StoreCredentialsDigest = storeCredentialsDigest(digest)
		}
	}

	var errs []error
	for key, data := range wanted {
		mount := wantedStore[key]
		secret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: key.namespace, Name: key.name}, secret)
		switch {
		case apierrors.IsNotFound(err):
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: key.name, Namespace: key.namespace,
					Labels: withLabels(labels, map[string]string{serviceLakeLabel: lake.Name, storeIDLabel: mount.StoreID, storeCredentialsCopyLabel: "true"})},
				Type: corev1.SecretTypeOpaque,
				Data: data,
			}
			if err := controllerutil.SetControllerReference(lake, secret, r.Scheme); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := r.Create(ctx, secret); err != nil {
				errs = append(errs, fmt.Errorf("copy store credential %s/%s: %w", key.namespace, key.name, err))
			}
		case err != nil:
			errs = append(errs, err)
		case !metav1.IsControlledBy(secret, lake) || secret.Labels[storeIDLabel] != mount.StoreID || secret.Labels[storeCredentialsCopyLabel] != "true":
			errs = append(errs, fmt.Errorf("Secret %s/%s exists and is not this Lake's store credential", key.namespace, key.name))
		case !apiequality.Semantic.DeepEqual(secret.Data, data):
			secret.Data = data
			if err := r.Update(ctx, secret); err != nil {
				errs = append(errs, fmt.Errorf("update store credential %s/%s: %w", key.namespace, key.name, err))
			}
		}
	}
	for namespace := range namespaces {
		var copies corev1.SecretList
		if err := r.List(ctx, &copies, client.InNamespace(namespace),
			client.MatchingLabels{serviceLakeLabel: lake.Name, storeCredentialsCopyLabel: "true"}); err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range copies.Items {
			secret := &copies.Items[i]
			if _, keep := wanted[copyKey{secret.Namespace, secret.Name}]; keep || !metav1.IsControlledBy(secret, lake) {
				continue
			}
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// withStoreMounts adds a wrapper service's mounted stores to its pod template.
func withStoreMounts(template *corev1.PodTemplateSpec, lake *oramav1alpha1.Lake, svc *LakeService) {
	if svc == nil || len(svc.StoreMounts) == 0 {
		return
	}
	volumes, volumeMounts, env := storeMountsPodParts(svc.StoreMounts, func(mount workloadStoreMount) string {
		return storeCredentialsCopyName(lake.Name, mount.Slug)
	})
	template.Spec.Volumes = append(template.Spec.Volumes, volumes...)
	container := &template.Spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, volumeMounts...)
	container.Env = append(container.Env, env...)
	if svc.StoreCredentialsDigest != "" {
		if template.Annotations == nil {
			template.Annotations = map[string]string{}
		}
		template.Annotations[storeCredentialsAnnotation] = svc.StoreCredentialsDigest
	}
}
