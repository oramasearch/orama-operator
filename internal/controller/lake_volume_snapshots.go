/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Scheduled CSI snapshots of lake's volumes (operator#260, decision 54 S13, lake migration 0069).
// Lake's volume_snapshot_policies names each target (`store`, lake's own store volume, or a
// workspace id), a cron schedule in UTC, how many snapshots to keep, and a one-off requested_at.
// The operator takes a VolumeSnapshot of each of the target's claims, names it
// <claim>-<UTC timestamp>, labels it with the target, deletes the oldest beyond keep, and reports
// the newest in volume_snapshot_status. Every snapshot names one VolumeSnapshotClass with
// deletionPolicy Retain, so deleting a snapshot, a workspace or its namespace never deletes the
// snapshot's contents; the operator labels each bound VolumeSnapshotContent with its target so a
// deleted workspace's retained contents can be found for a restore (docs/lake/volume-snapshots.md).
// The operator never talks to a workspace Zetta (decision 51).

const (
	lakeVolumeSnapshotsPoll         = 60 * time.Second
	lakeVolumeSnapshotsChangingPoll = 10 * time.Second

	snapshotTargetLabel   = "orama.cloud/snapshot-target"
	snapshotAtAnnotation  = "orama.cloud/snapshot-at"
	snapshotClaimAnnot    = "orama.cloud/snapshot-claim"
	snapshotNamespaceAnno = "orama.cloud/snapshot-namespace"
	snapshotNameAnnot     = "orama.cloud/snapshot-name"

	snapshotStoreTarget  = "store"
	snapshotTimeLayout   = "20060102t150405z"
	snapshotDefaultClass = "lake-retain"
	snapshotDefaultCSI   = "disk.csi.azure.com"
	snapshotRetainPolicy = "Retain"
)

var (
	volumeSnapshotGVK        = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	volumeSnapshotListGVK    = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList"}
	volumeSnapshotClassGVK   = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}
	volumeSnapshotContentGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
)

// LakeVolumeSnapshotsReconciler reconciles lake's volume_snapshot_policies into VolumeSnapshots.
type LakeVolumeSnapshotsReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	APIReader      client.Reader
	SnapshotStores LakeVolumeSnapshotsStoreOpener
	Events         <-chan event.GenericEvent
	// Now is the clock schedules are judged by; nil means time.Now. Injected for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;list;watch;patch

func lakeSnapshotsEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Snapshots.Enabled && lakePostgresEnabled(lake) && lake.DeletionTimestamp.IsZero()
}

func lakeSnapshotClassName(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Snapshots.ClassName != "" {
		return lake.Spec.Snapshots.ClassName
	}
	return snapshotDefaultClass
}

func lakeSnapshotDriver(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Snapshots.Driver != "" {
		return lake.Spec.Snapshots.Driver
	}
	return snapshotDefaultCSI
}

func (r *LakeVolumeSnapshotsReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *LakeVolumeSnapshotsReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *LakeVolumeSnapshotsReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, request.NamespacedName, &lake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lakeSnapshotsEnabled(&lake) {
		return ctrl.Result{}, nil
	}
	parent := &LakeReconciler{Client: r.Client, Scheme: r.Scheme}
	_, _, schemaStatus := parent.lakeSchemaStatus(ctx, &lake, lakeNamespace(&lake))
	if schemaStatus.phase != "Ready" {
		return ctrl.Result{RequeueAfter: lakeVolumeSnapshotsChangingPoll}, nil
	}
	return ctrl.Result{RequeueAfter: r.pollVolumeSnapshots(ctx, &lake)}, nil
}

// pollVolumeSnapshots runs one pass over every policy and answers when to look again: soon while
// a snapshot is not ready, otherwise at the next due time, at most a minute away.
func (r *LakeVolumeSnapshotsReconciler) pollVolumeSnapshots(ctx context.Context, lake *oramav1alpha1.Lake) time.Duration {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: lakeControlDB(lake.Name) + "-app"}
	if err := r.Get(ctx, key, &secret); err != nil || len(secret.Data["uri"]) == 0 {
		log.V(1).Info("volume snapshots deferred: control DB credential unavailable")
		return lakeVolumeSnapshotsPoll
	}
	passCtx, cancel := context.WithTimeout(ctx, servicePassTimeout)
	defer cancel()
	open := r.SnapshotStores
	if open == nil {
		open = openPgxVolumeSnapshotsStore
	}
	store, err := open(passCtx, string(secret.Data["uri"]))
	if err != nil {
		log.Info("volume snapshots deferred: cannot connect to control DB")
		return lakeVolumeSnapshotsPoll
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = store.Close(closeCtx)
	}()
	policies, err := store.ListVolumeSnapshotPolicies(passCtx)
	if errors.Is(err, errVolumeSnapshotsTableMissing) {
		log.Info("volume snapshots skipped: " + err.Error())
		return lakeVolumeSnapshotsPoll
	}
	if err != nil {
		log.Info("volume snapshots deferred: complete policy list unavailable")
		return lakeVolumeSnapshotsPoll
	}
	return r.syncVolumeSnapshots(passCtx, lake, store, policies)
}

func (r *LakeVolumeSnapshotsReconciler) syncVolumeSnapshots(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeVolumeSnapshotsStore,
	policies []LakeVolumeSnapshotPolicy,
) time.Duration {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	classProblem, err := r.ensureSnapshotClass(ctx, lake)
	if err != nil {
		log.Error(err, "volume snapshot class deferred")
		return lakeVolumeSnapshotsChangingPoll
	}
	now := r.now()
	wait := lakeVolumeSnapshotsPoll
	for i := range policies {
		policy := &policies[i]
		status, report, changing, next, err := r.reconcileSnapshotTarget(ctx, lake, policy, classProblem, now)
		if err != nil {
			log.Error(err, "volume snapshots of a target deferred", "target", policy.Target)
			wait = min(wait, lakeVolumeSnapshotsChangingPoll)
			continue
		}
		if changing {
			wait = min(wait, lakeVolumeSnapshotsChangingPoll)
		}
		if !next.IsZero() {
			wait = min(wait, max(next.Sub(now), time.Second))
		}
		if !report || (policy.Status != nil && policy.Status.equal(status)) {
			continue
		}
		if err := store.UpsertVolumeSnapshotStatus(ctx, status); err != nil {
			log.Error(err, "volume snapshot status write deferred", "target", policy.Target)
			wait = min(wait, lakeVolumeSnapshotsChangingPoll)
		}
	}
	return wait
}

// ensureSnapshotClass creates the Lake's VolumeSnapshotClass with deletionPolicy Retain when it is
// absent. A problem the operator cannot fix (no snapshot CRDs, an existing class that deletes its
// contents) is answered as a message every target reports, and no snapshot is taken.
func (r *LakeVolumeSnapshotsReconciler) ensureSnapshotClass(ctx context.Context, lake *oramav1alpha1.Lake) (string, error) {
	name := lakeSnapshotClassName(lake)
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, class)
	switch {
	case meta.IsNoMatchError(err):
		return "the snapshot.storage.k8s.io CRDs are not installed", nil
	case apierrors.IsNotFound(err):
		class = &unstructured.Unstructured{}
		class.SetGroupVersionKind(volumeSnapshotClassGVK)
		class.SetName(name)
		class.SetLabels(map[string]string{workspaceLakeLabel: lake.Name, workspaceManagedByLabel: "orama-operator"})
		class.Object["driver"] = lakeSnapshotDriver(lake)
		class.Object["deletionPolicy"] = snapshotRetainPolicy
		if err := r.Create(ctx, class); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		logf.FromContext(ctx).Info("created the volume snapshot class", "lake", lake.Name, "class", name)
		return "", nil
	case err != nil:
		return "", err
	}
	if policy, _, _ := unstructured.NestedString(class.Object, "deletionPolicy"); policy != snapshotRetainPolicy {
		return fmt.Sprintf("VolumeSnapshotClass %q has deletionPolicy %s; lake's snapshots need %s so they outlive their workspace",
			name, policy, snapshotRetainPolicy), nil
	}
	return "", nil
}

// snapshotClaims is where a target's volumes are: lake's store claim, or the data claims of the
// workspace Zetta. skip is true for a target the operator leaves alone (a deleted or unknown
// workspace: its snapshots and status stay as they are); problem explains a target it cannot
// snapshot yet.
func (r *LakeVolumeSnapshotsReconciler) snapshotClaims(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	policy *LakeVolumeSnapshotPolicy,
) (namespace string, claims []string, skip bool, problem string, err error) {
	if policy.Target == snapshotStoreTarget {
		namespace = lakeNamespace(lake)
		name := lakeStoreClaimName(lake)
		var claim corev1.PersistentVolumeClaim
		if err := r.apiReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &claim); err != nil {
			if apierrors.IsNotFound(err) {
				return namespace, nil, false, fmt.Sprintf("lake's store claim %s/%s does not exist", namespace, name), nil
			}
			return namespace, nil, false, "", err
		}
		return namespace, []string{name}, false, "", nil
	}
	switch policy.WorkspaceDesired {
	case "running", "stopped":
	default:
		return "", nil, true, "", nil
	}
	clusterName := workspaceZettaClusterName(lake, policy.Target)
	if policy.WorkspaceNamespace == "" || clusterName == "" {
		return "", nil, false, "the workspace has no namespace yet", nil
	}
	found, err := workspaceZettaClaims(ctx, r.apiReader(), policy.WorkspaceNamespace, clusterName)
	if err != nil {
		return policy.WorkspaceNamespace, nil, false, "", err
	}
	for i := range found {
		claims = append(claims, found[i].Name)
	}
	if len(claims) == 0 {
		return policy.WorkspaceNamespace, nil, false, "the workspace Zetta has no volume claim yet", nil
	}
	return policy.WorkspaceNamespace, claims, false, "", nil
}

// takenSnapshot is one VolumeSnapshot the operator took, with the instant it asked for it.
type takenSnapshot struct {
	object  *unstructured.Unstructured
	takenAt time.Time
}

func (s takenSnapshot) ready() bool {
	ready, _, _ := unstructured.NestedBool(s.object.Object, "status", "readyToUse")
	return ready
}

func (s takenSnapshot) failure() string {
	message, _, _ := unstructured.NestedString(s.object.Object, "status", "error", "message")
	return message
}

// claimSnapshots lists the target's snapshots of one claim, oldest first.
func (r *LakeVolumeSnapshotsReconciler) claimSnapshots(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace, target, claim string,
) ([]takenSnapshot, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := r.apiReader().List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabels{snapshotTargetLabel: target, workspaceLakeLabel: lake.Name}); err != nil {
		return nil, err
	}
	var snapshots []takenSnapshot
	for i := range list.Items {
		item := &list.Items[i]
		source, _, _ := unstructured.NestedString(item.Object, "spec", "source", "persistentVolumeClaimName")
		if source != claim || item.GetDeletionTimestamp() != nil {
			continue
		}
		takenAt := item.GetCreationTimestamp().Time
		if at, err := time.Parse(time.RFC3339Nano, item.GetAnnotations()[snapshotAtAnnotation]); err == nil {
			takenAt = at
		}
		snapshots = append(snapshots, takenSnapshot{object: item, takenAt: takenAt.UTC()})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].takenAt.Equal(snapshots[j].takenAt) {
			return snapshots[i].takenAt.Before(snapshots[j].takenAt)
		}
		return snapshots[i].object.GetName() < snapshots[j].object.GetName()
	})
	return snapshots, nil
}

// snapshotName is <claim>-<UTC timestamp>, within a DNS subdomain's 253 characters.
func snapshotName(claim string, at time.Time) string {
	suffix := "-" + at.UTC().Format(snapshotTimeLayout)
	if len(claim)+len(suffix) > 253 {
		claim = strings.TrimRight(claim[:253-len(suffix)], "-.")
	}
	return claim + suffix
}

func (r *LakeVolumeSnapshotsReconciler) takeSnapshot(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace, target, claim string,
	now time.Time,
) (takenSnapshot, error) {
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetNamespace(namespace)
	snapshot.SetName(snapshotName(claim, now))
	snapshot.SetLabels(map[string]string{
		snapshotTargetLabel:     target,
		workspaceLakeLabel:      lake.Name,
		workspaceManagedByLabel: "orama-operator",
	})
	snapshot.SetAnnotations(map[string]string{
		snapshotAtAnnotation: now.Format(time.RFC3339Nano),
		snapshotClaimAnnot:   claim,
	})
	snapshot.Object["spec"] = map[string]interface{}{
		"volumeSnapshotClassName": lakeSnapshotClassName(lake),
		"source":                  map[string]interface{}{"persistentVolumeClaimName": claim},
	}
	if err := r.Create(ctx, snapshot); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Taken in this second already; the existing one stands.
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(volumeSnapshotGVK)
			if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(snapshot), existing); err != nil {
				return takenSnapshot{}, err
			}
			return takenSnapshot{object: existing, takenAt: now}, nil
		}
		return takenSnapshot{}, err
	}
	logf.FromContext(ctx).Info("took a volume snapshot", "lake", lake.Name, "target", target,
		"snapshot", namespace+"/"+snapshot.GetName())
	return takenSnapshot{object: snapshot, takenAt: now}, nil
}

// labelSnapshotContent labels the VolumeSnapshotContent a snapshot is bound to with its target and
// source, so its retained contents can be found after the snapshot or its namespace is gone.
func (r *LakeVolumeSnapshotsReconciler) labelSnapshotContent(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	snapshot takenSnapshot,
	target, claim string,
) error {
	name, _, _ := unstructured.NestedString(snapshot.object.Object, "status", "boundVolumeSnapshotContentName")
	if name == "" {
		return nil
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		return client.IgnoreNotFound(err)
	}
	if content.GetLabels()[snapshotTargetLabel] == target {
		return nil
	}
	patch := client.MergeFrom(content.DeepCopy())
	labels := content.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[snapshotTargetLabel] = target
	labels[workspaceLakeLabel] = lake.Name
	content.SetLabels(labels)
	annotations := content.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[snapshotNamespaceAnno] = snapshot.object.GetNamespace()
	annotations[snapshotNameAnnot] = snapshot.object.GetName()
	annotations[snapshotClaimAnnot] = claim
	annotations[snapshotAtAnnotation] = snapshot.takenAt.Format(time.RFC3339Nano)
	content.SetAnnotations(annotations)
	return r.Patch(ctx, content, patch)
}

// reconcileSnapshotTarget takes what is due for one target, prunes beyond keep, and answers the
// status to report. report is false for a target left alone. changing is true while the newest
// snapshot of a claim is not ready. next is when the target is next due.
func (r *LakeVolumeSnapshotsReconciler) reconcileSnapshotTarget(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	policy *LakeVolumeSnapshotPolicy,
	classProblem string,
	now time.Time,
) (status LakeVolumeSnapshotStatus, report bool, changing bool, next time.Time, err error) {
	status = LakeVolumeSnapshotStatus{Target: policy.Target}
	if policy.Status != nil {
		status.SnapshotName = policy.Status.SnapshotName
		status.ReadyToUse = policy.Status.ReadyToUse
		status.SnapshotAt = policy.Status.SnapshotAt
	}
	namespace, claims, skip, problem, err := r.snapshotClaims(ctx, lake, policy)
	if err != nil || skip {
		return status, false, false, time.Time{}, err
	}
	if problem == "" && classProblem != "" {
		problem = classProblem
	}
	schedule, parseErr := cron.ParseStandard(policy.Schedule)
	if problem == "" && parseErr != nil {
		problem = fmt.Sprintf("schedule %q is not a cron expression", policy.Schedule)
	}
	if problem != "" {
		status.Message = problem
		return status, true, false, time.Time{}, nil
	}
	keep := int(policy.Keep)
	if keep < 1 {
		keep = 1
	}
	var newest *takenSnapshot
	allReady := true
	var failures []string
	for _, claim := range claims {
		snapshots, err := r.claimSnapshots(ctx, lake, namespace, policy.Target, claim)
		if err != nil {
			return status, false, true, time.Time{}, err
		}
		due := len(snapshots) == 0
		if !due {
			latest := snapshots[len(snapshots)-1].takenAt
			due = (policy.RequestedAt != nil && policy.RequestedAt.After(latest)) ||
				!schedule.Next(latest).After(now)
		}
		if due {
			taken, err := r.takeSnapshot(ctx, lake, namespace, policy.Target, claim, now)
			if err != nil {
				return status, false, true, time.Time{}, err
			}
			if len(snapshots) == 0 || snapshots[len(snapshots)-1].object.GetName() != taken.object.GetName() {
				snapshots = append(snapshots, taken)
			}
		}
		for len(snapshots) > keep {
			oldest := snapshots[0]
			if err := r.Delete(ctx, oldest.object); err != nil && !apierrors.IsNotFound(err) {
				return status, false, true, time.Time{}, err
			}
			logf.FromContext(ctx).Info("pruned a volume snapshot beyond keep", "lake", lake.Name,
				"target", policy.Target, "snapshot", namespace+"/"+oldest.object.GetName())
			snapshots = snapshots[1:]
		}
		for _, snapshot := range snapshots {
			if err := r.labelSnapshotContent(ctx, lake, snapshot, policy.Target, claim); err != nil {
				return status, false, true, time.Time{}, err
			}
		}
		last := snapshots[len(snapshots)-1]
		if !last.ready() {
			allReady = false
		}
		if message := last.failure(); message != "" {
			failures = append(failures, last.object.GetName()+": "+message)
		}
		if newest == nil || last.takenAt.After(newest.takenAt) {
			newest = &last
		}
		if claimNext := schedule.Next(last.takenAt); next.IsZero() || claimNext.Before(next) {
			next = claimNext
		}
	}
	takenAt := newest.takenAt
	status.SnapshotName = newest.object.GetName()
	status.SnapshotAt = &takenAt
	status.ReadyToUse = allReady
	status.Message = strings.Join(failures, "; ")
	return status, true, !allReady, next, nil
}

func (r *LakeVolumeSnapshotsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.Lake{})
	if r.Events != nil {
		builder = builder.WatchesRawSource(source.Channel(r.Events, &handler.EnqueueRequestForObject{}))
	}
	return builder.Named("lake-volume-snapshots").Complete(r)
}
