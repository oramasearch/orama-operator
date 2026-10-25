/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A store restore rehearsal (service: store) proves that the operator's CSI snapshots of Lake's
// store volume (lake_volume_snapshots.go) restore to a readable volume. Lake's store holds the
// transparency log, TUF metadata and the instance registry; nothing else backs it up.
//
// It never touches the live claim, the live snapshot or its content. The selected snapshot's cloud
// handle is bound to a new, pre-provisioned VolumeSnapshotContent with deletionPolicy Retain, so
// deleting it afterwards leaves the cloud snapshot in place. A VolumeSnapshot bound to that content,
// a claim restored from it and a read-only verifier Job all live in the rehearsal's scratch
// namespace and are deleted when the rehearsal finishes. The restored disk goes with its claim.

const (
	storeRehearsalClaim    = "store"
	storeRehearsalSnapshot = "store"
	storeRehearsalDeadline = int64(3600)
	// The UID and GID Lake runs as (lake_controller.go), so the verifier can read the files Lake
	// wrote with owner-only permissions.
	storeRehearsalUser = int64(10001)
)

// storeRehearsalScript reads every file of the restored store, not only its metadata, so a disk
// that mounts but whose blocks do not restore fails. It reports through the termination message.
const storeRehearsalScript = `
report() { printf '%s' "$1" > /dev/termination-log; echo "$1"; }
cd /restore
if ! find . -xdev -path ./lost+found -prune -o -type f -print > /tmp/files; then
  report "the restored store could not be listed"; exit 1
fi
files=$(wc -l < /tmp/files | tr -d ' ')
if [ "$files" -eq 0 ]; then
  report "the restored store has no files"; exit 1
fi
if ! find . -xdev -path ./lost+found -prune -o -type f -exec cat {} + > /dev/null; then
  report "a file of the restored store could not be read"; exit 1
fi
kib=$(du -sk -x . | cut -f1)
top=$(ls -1A | grep -v '^lost+found$' | head -20 | tr '\n' ' ')
report "files=$files kib=$kib top=${top% }"
`

func newStoreRehearsalSnapshot(rehearsal *oramav1alpha1.LakeRestoreRehearsal) *unstructured.Unstructured {
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetName(storeRehearsalSnapshot)
	snapshot.SetNamespace(rehearsal.Status.ScratchNamespace)
	return snapshot
}

// storeRehearsalContentName is the cluster-scoped VolumeSnapshotContent the rehearsal pre-provisions.
func storeRehearsalContentName(name string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return "lake-rehearsal-store-" + hash
}

func newStoreRehearsalContent(rehearsal *oramav1alpha1.LakeRestoreRehearsal) *unstructured.Unstructured {
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	content.SetName(storeRehearsalContentName(rehearsal.Name))
	return content
}

func isGoneOrUnknownKind(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

func (r *LakeRestoreRehearsalReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// storeSnapshotSource is the selected snapshot as the restore needs it.
type storeSnapshotSource struct {
	name         string
	className    string
	driver       string
	handle       string
	restoreSize  resource.Quantity
	storageClass *string
	accessModes  []corev1.PersistentVolumeAccessMode
}

func (r *LakeRestoreRehearsalReconciler) reconcileStoreRehearsal(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (ctrl.Result, error) {
	source, waiting, failure, err := r.selectStoreSnapshot(ctx, rehearsal, lake)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failure != "" {
		return r.finishRehearsal(ctx, rehearsal, false, failure)
	}
	if waiting {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	if err := r.ensureScratchNamespace(ctx, rehearsal); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureStoreRehearsalContent(ctx, rehearsal, source); err != nil {
		return ctrl.Result{}, err
	}
	snapshot, err := r.ensureStoreRehearsalSnapshot(ctx, rehearsal, source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if message := snapshotErrorMessage(snapshot); message != "" {
		return r.finishRehearsal(ctx, rehearsal, false, "the rehearsal's VolumeSnapshot failed: "+message)
	}
	if ready, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse"); !ready {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	if err := r.ensureStoreRehearsalClaim(ctx, rehearsal, source); err != nil {
		return ctrl.Result{}, err
	}
	job, err := r.ensureStoreVerificationJob(ctx, rehearsal)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job.Status.Failed == 0 && job.Status.Succeeded == 0 {
		return ctrl.Result{RequeueAfter: lakeRehearsalRequeue}, nil
	}
	evidence, err := r.verifierReport(ctx, rehearsal.Status.ScratchNamespace, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	rehearsal.Status.Evidence = evidence
	if job.Status.Failed > 0 {
		message := "store verification Job failed"
		if evidence != "" {
			message += ": " + evidence
		}
		return r.finishRehearsal(ctx, rehearsal, false, message)
	}
	return r.finishRehearsal(ctx, rehearsal, true,
		fmt.Sprintf("store restore rehearsal succeeded using VolumeSnapshot %s", rehearsal.Status.SelectedSnapshot))
}

// selectStoreSnapshot pins the snapshot once (status.selectedSnapshot) and resolves its content.
// A missing kind or snapshot is a terminal failure, not a wait: nothing would ever appear.
func (r *LakeRestoreRehearsalReconciler) selectStoreSnapshot(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	lake *oramav1alpha1.Lake,
) (storeSnapshotSource, bool, string, error) {
	namespace := lakeNamespace(lake)
	claimName := lakeStoreClaimName(lake)
	selected := rehearsal.Status.SelectedSnapshot
	from := rehearsal.Spec.From
	if selected == "" && from != "" && from != "latest" {
		selected = from
	}
	snapshots := &LakeVolumeSnapshotsReconciler{Client: r.Client, APIReader: r.reader()}
	var snapshot *unstructured.Unstructured
	if selected == "" {
		taken, err := snapshots.claimSnapshots(ctx, lake, namespace, snapshotStoreTarget, claimName)
		if meta.IsNoMatchError(err) {
			return storeSnapshotSource{}, false, "the cluster has no VolumeSnapshot kind (snapshot.storage.k8s.io)", nil
		}
		if err != nil {
			return storeSnapshotSource{}, false, "", err
		}
		for i := len(taken) - 1; i >= 0; i-- {
			if taken[i].ready() {
				snapshot = taken[i].object
				break
			}
		}
		if snapshot == nil {
			return storeSnapshotSource{}, false, fmt.Sprintf("no ready VolumeSnapshot of the store claim %s/%s", namespace, claimName), nil
		}
		rehearsal.Status.SelectedSnapshot = snapshot.GetName()
		rehearsal.Status.Message = "selected VolumeSnapshot " + snapshot.GetName()
		return storeSnapshotSource{}, true, "", r.Status().Update(ctx, rehearsal)
	}
	snapshot = &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: selected}, snapshot); err != nil {
		if isGoneOrUnknownKind(err) {
			return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshot %s/%s was not found", namespace, selected), nil
		}
		return storeSnapshotSource{}, false, "", err
	}
	sourceClaim, _, _ := unstructured.NestedString(snapshot.Object, "spec", "source", "persistentVolumeClaimName")
	if !ownSnapshot(snapshot, lake, snapshotStoreTarget) || sourceClaim != claimName {
		return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshot %s/%s is not an operator snapshot of the store claim %s", namespace, selected, claimName), nil
	}
	if ready, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse"); !ready {
		return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshot %s/%s is not ready to use", namespace, selected), nil
	}
	if rehearsal.Status.SelectedSnapshot == "" {
		rehearsal.Status.SelectedSnapshot = selected
		rehearsal.Status.Message = "selected VolumeSnapshot " + selected
		return storeSnapshotSource{}, true, "", r.Status().Update(ctx, rehearsal)
	}

	contentName, _, _ := unstructured.NestedString(snapshot.Object, "status", "boundVolumeSnapshotContentName")
	if contentName == "" {
		return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshot %s/%s is bound to no content", namespace, selected), nil
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := r.reader().Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshotContent %s was not found", contentName), nil
		}
		return storeSnapshotSource{}, false, "", err
	}
	handle, _, _ := unstructured.NestedString(content.Object, "status", "snapshotHandle")
	driver, _, _ := unstructured.NestedString(content.Object, "spec", "driver")
	if handle == "" || driver == "" {
		return storeSnapshotSource{}, false, fmt.Sprintf("VolumeSnapshotContent %s has no snapshot handle or driver", contentName), nil
	}
	className, _, _ := unstructured.NestedString(snapshot.Object, "spec", "volumeSnapshotClassName")
	var claim corev1.PersistentVolumeClaim
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return storeSnapshotSource{}, false, fmt.Sprintf("the store claim %s/%s does not exist", namespace, claimName), nil
		}
		return storeSnapshotSource{}, false, "", err
	}
	size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if restore, found, _ := unstructured.NestedFieldNoCopy(snapshot.Object, "status", "restoreSize"); found {
		if parsed, err := resource.ParseQuantity(fmt.Sprint(restore)); err == nil && parsed.Cmp(size) > 0 {
			size = parsed
		}
	}
	if size.IsZero() {
		return storeSnapshotSource{}, false, fmt.Sprintf("neither VolumeSnapshot %s/%s nor the store claim states a size", namespace, selected), nil
	}
	accessModes := claim.Spec.AccessModes
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return storeSnapshotSource{
		name: selected, className: className, driver: driver, handle: handle, restoreSize: size,
		storageClass: claim.Spec.StorageClassName, accessModes: accessModes,
	}, false, "", nil
}

// ensureStoreRehearsalContent pre-provisions the content that gives the scratch namespace the
// selected cloud snapshot. Its deletionPolicy is Retain, always: deleting it must never delete
// the cloud snapshot the live store's snapshot is bound to.
func (r *LakeRestoreRehearsalReconciler) ensureStoreRehearsalContent(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	source storeSnapshotSource,
) error {
	content := newStoreRehearsalContent(rehearsal)
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(content), content); err == nil {
		if !metav1.IsControlledBy(content, rehearsal) {
			return fmt.Errorf("VolumeSnapshotContent %s is not controlled by this rehearsal", content.GetName())
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	content.SetLabels(rehearsalLabels(rehearsal))
	spec := map[string]interface{}{
		"deletionPolicy": snapshotRetainPolicy,
		"driver":         source.driver,
		"source":         map[string]interface{}{"snapshotHandle": source.handle},
		"volumeSnapshotRef": map[string]interface{}{
			"name":      storeRehearsalSnapshot,
			"namespace": rehearsal.Status.ScratchNamespace,
		},
	}
	if source.className != "" {
		spec["volumeSnapshotClassName"] = source.className
	}
	content.Object["spec"] = spec
	if err := controllerutil.SetControllerReference(rehearsal, content, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, content)
}

func (r *LakeRestoreRehearsalReconciler) ensureStoreRehearsalSnapshot(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	source storeSnapshotSource,
) (*unstructured.Unstructured, error) {
	snapshot := newStoreRehearsalSnapshot(rehearsal)
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); err == nil {
		if !metav1.IsControlledBy(snapshot, rehearsal) {
			return nil, fmt.Errorf("VolumeSnapshot %s/%s is not controlled by this rehearsal", snapshot.GetNamespace(), snapshot.GetName())
		}
		return snapshot, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	snapshot.SetLabels(rehearsalLabels(rehearsal))
	spec := map[string]interface{}{
		"source": map[string]interface{}{"volumeSnapshotContentName": storeRehearsalContentName(rehearsal.Name)},
	}
	if source.className != "" {
		spec["volumeSnapshotClassName"] = source.className
	}
	snapshot.Object["spec"] = spec
	if err := controllerutil.SetControllerReference(rehearsal, snapshot, r.Scheme); err != nil {
		return nil, err
	}
	return snapshot, r.Create(ctx, snapshot)
}

func snapshotErrorMessage(snapshot *unstructured.Unstructured) string {
	message, _, _ := unstructured.NestedString(snapshot.Object, "status", "error", "message")
	return message
}

func (r *LakeRestoreRehearsalReconciler) ensureStoreRehearsalClaim(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
	source storeSnapshotSource,
) error {
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: storeRehearsalClaim, Namespace: rehearsal.Status.ScratchNamespace}}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(claim), claim); err == nil {
		if !metav1.IsControlledBy(claim, rehearsal) {
			return fmt.Errorf("claim %s/%s is not controlled by this rehearsal", claim.Namespace, claim.Name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	group := volumeSnapshotGVK.Group
	claim.Labels = rehearsalLabels(rehearsal)
	claim.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes:      source.accessModes,
		StorageClassName: source.storageClass,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: source.restoreSize},
		},
		DataSource: &corev1.TypedLocalObjectReference{
			APIGroup: &group,
			Kind:     volumeSnapshotGVK.Kind,
			Name:     storeRehearsalSnapshot,
		},
	}
	if err := controllerutil.SetControllerReference(rehearsal, claim, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, claim)
}

func (r *LakeRestoreRehearsalReconciler) ensureStoreVerificationJob(
	ctx context.Context,
	rehearsal *oramav1alpha1.LakeRestoreRehearsal,
) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "verify", Namespace: rehearsal.Status.ScratchNamespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		if !metav1.IsControlledBy(job, rehearsal) {
			return nil, fmt.Errorf("verification Job %s is not controlled by this rehearsal", job.Name)
		}
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = rehearsalLabels(rehearsal)
	job.Spec = storeVerificationJobSpec()
	if err := controllerutil.SetControllerReference(rehearsal, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, r.Create(ctx, job)
}

// storeVerificationJobSpec mounts the restored claim read-only in a pod at the restricted Pod
// Security level, with no service account token.
func storeVerificationJobSpec() batchv1.JobSpec {
	backoff := int32(0)
	deadline := storeRehearsalDeadline
	user := storeRehearsalUser
	noToken := false
	nonRoot := true
	noEscalation := false
	readOnlyRoot := true
	return batchv1.JobSpec{
		BackoffLimit:          &backoff,
		ActiveDeadlineSeconds: &deadline,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "lake-store-rehearsal"}},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: &noToken,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   &nonRoot,
					RunAsUser:      &user,
					RunAsGroup:     &user,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:                     "verify",
					Image:                    lakeRehearsalVerifyImage,
					Command:                  []string{"/bin/sh", "-ec", storeRehearsalScript},
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &noEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRoot,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "restore", MountPath: "/restore", ReadOnly: true},
						{Name: "tmp", MountPath: "/tmp"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "restore", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: storeRehearsalClaim, ReadOnly: true,
					}}},
					{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		},
	}
}

// verifierReport reads the verifier pod's termination message. The Job is deleted when the
// rehearsal finishes, so this is the evidence that outlives it.
func (r *LakeRestoreRehearsalReconciler) verifierReport(ctx context.Context, namespace string, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "verify" && status.State.Terminated != nil {
				message := strings.TrimSpace(status.State.Terminated.Message)
				if len(message) > 1024 {
					message = message[:1024]
				}
				return message, nil
			}
		}
	}
	return "", nil
}

// deleteStoreRehearsalContent deletes the pre-provisioned content once its snapshot is gone. It
// is Retain, so the cloud snapshot stays; the check refuses to delete anything else.
func (r *LakeRestoreRehearsalReconciler) deleteStoreRehearsalContent(ctx context.Context, rehearsal *oramav1alpha1.LakeRestoreRehearsal) error {
	content := newStoreRehearsalContent(rehearsal)
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(content), content); err != nil {
		if isGoneOrUnknownKind(err) {
			return nil
		}
		return err
	}
	policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy")
	if !metav1.IsControlledBy(content, rehearsal) || policy != snapshotRetainPolicy {
		return fmt.Errorf("VolumeSnapshotContent %s is not this rehearsal's Retain content; not deleted", content.GetName())
	}
	return client.IgnoreNotFound(r.Delete(ctx, content))
}
