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

	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Scheduled CSI snapshots of the engine's data claims (operator#316). On Prod 2 the writer's claim
// is the only copy of every index, and a reader rebuilds from the replication stream only while the
// stream still holds the history, so both are snapshotted, as Prod 1 snapshots its EBS volumes
// daily and keeps 7. The reconciler takes the snapshots itself on the schedule, timed from the
// newest existing snapshot of each claim (so a restart neither skips nor repeats a run), names each
// <claim>-<UTC timestamp>, and prunes by the same rules as Lake's snapshots (decision 39), in its
// own code: one VolumeSnapshotClass with deletionPolicy Retain, a pruned snapshot's content switched
// to Delete first. Restore is a runbook (docs/components.md).

const (
	engineSnapshotsDefaultSchedule = "0 3 * * *"
	engineSnapshotsDefaultKeep     = 7
	engineSnapshotsDefaultClass    = "orama-cloud-v1-retain"
	// engineSnapshotsPendingPoll is how soon a pass looks again while a snapshot is not ready.
	engineSnapshotsPendingPoll = 30 * time.Second

	engineSnapshotsConditionType = "SnapshotsReady"
)

// engineSnapshots is one pass's outcome: the per-claim status, the SnapshotsReady condition (nil
// when snapshots are off, so the condition is dropped) and when to look again (0: no need).
type engineSnapshots struct {
	statuses  []oramav1alpha1.EngineSnapshotStatus
	condition *metav1.Condition
	wait      time.Duration
}

// engineSnapshotConfig is spec.components.engine.snapshots with its defaults applied.
type engineSnapshotConfig struct {
	schedule cron.Schedule
	keep     int
	class    string
}

// engineSnapshotParser accepts exactly five fields; descriptors such as @daily are refused.
var engineSnapshotParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// resolveEngineSnapshots answers the snapshot config, or nil when snapshots are off. A schedule
// that is not a 5-field UTC cron expression is an error, which fails reconcile.
func resolveEngineSnapshots(oc *oramav1alpha1.OramaCloudV1) (*engineSnapshotConfig, error) {
	spec := oc.Spec.Components.Engine.Snapshots
	if !oc.Spec.Components.Engine.Enabled || spec == nil || !spec.Enabled || !oc.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	expr := spec.Schedule
	if expr == "" {
		expr = engineSnapshotsDefaultSchedule
	}
	const want = "a 5-field cron expression (minute hour day-of-month month day-of-week), evaluated in UTC"
	if strings.Contains(expr, "TZ=") {
		return nil, fmt.Errorf("spec.components.engine.snapshots.schedule %q must be %s, without a time zone", expr, want)
	}
	schedule, err := engineSnapshotParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("spec.components.engine.snapshots.schedule %q must be %s: %v", expr, want, err)
	}
	if schedule.Next(time.Now().UTC()).IsZero() {
		return nil, fmt.Errorf("spec.components.engine.snapshots.schedule %q never fires", expr)
	}
	cfg := &engineSnapshotConfig{schedule: schedule, keep: engineSnapshotsDefaultKeep, class: engineSnapshotsDefaultClass}
	if spec.Keep > 0 {
		cfg.keep = int(spec.Keep)
	}
	if spec.VolumeSnapshotClassName != "" {
		cfg.class = spec.VolumeSnapshotClassName
	}
	return cfg, nil
}

func (r *OramaCloudV1Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// engineClaim is one engine data claim and the component it belongs to.
type engineClaim struct{ name, component string }

// engineSnapshotClaims lists the data claims that exist: the writer's, and one per reader up to the
// reader StatefulSet's current replica count (which an HPA may have changed).
func (r *OramaCloudV1Reconciler) engineSnapshotClaims(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]engineClaim, error) {
	candidates := []engineClaim{{fmt.Sprintf("data-%s-writer-0", oc.Name), "writer"}}
	var readers appsv1.StatefulSet
	switch err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-reader"}, &readers); {
	case err == nil:
		for i := range replicasOf(readers.Spec.Replicas) {
			candidates = append(candidates, engineClaim{fmt.Sprintf("data-%s-reader-%d", oc.Name, i), "reader"})
		}
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	var claims []engineClaim
	for _, c := range candidates {
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: c.name}, &pvc)
		if apierrors.IsNotFound(err) {
			continue // not created yet
		}
		if err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, nil
}

// ensureEngineSnapshotClass answers the CSI driver of the class snapshots name, creating the default
// class (disk.csi.azure.com, deletionPolicy Retain) when it is absent. problem explains a class the
// operator cannot use: no snapshot CRDs, a named class that does not exist, or one that would
// delete its contents with the snapshot. No snapshot is taken then.
func (r *OramaCloudV1Reconciler) ensureEngineSnapshotClass(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string) (driver, problem string, err error) {
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeSnapshotClassGVK)
	err = r.Get(ctx, client.ObjectKey{Name: name}, class)
	switch {
	case meta.IsNoMatchError(err):
		return "", "the snapshot.storage.k8s.io CRDs are not installed", nil
	case apierrors.IsNotFound(err) && name == engineSnapshotsDefaultClass:
		class = &unstructured.Unstructured{}
		class.SetGroupVersionKind(volumeSnapshotClassGVK)
		class.SetName(name)
		class.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "orama-cloud-v1-operator"})
		class.Object["driver"] = snapshotDefaultCSI
		class.Object["deletionPolicy"] = snapshotRetainPolicy
		if err := r.Create(ctx, class); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", "", err
		}
		log.FromContext(ctx).Info("created the engine volume snapshot class", "class", name)
		return snapshotDefaultCSI, "", nil
	case apierrors.IsNotFound(err):
		return "", fmt.Sprintf("VolumeSnapshotClass %q does not exist", name), nil
	case err != nil:
		return "", "", err
	}
	if policy, _, _ := unstructured.NestedString(class.Object, "deletionPolicy"); policy != snapshotRetainPolicy {
		return "", fmt.Sprintf("VolumeSnapshotClass %q has deletionPolicy %s; engine snapshots need %s so they outlive the snapshot object",
			name, policy, snapshotRetainPolicy), nil
	}
	driver, _, _ = unstructured.NestedString(class.Object, "driver")
	return driver, "", nil
}

// ownEngineSnapshot reports whether this CR's reconciler took the snapshot: a hand-made snapshot,
// or another CR's, is never pruned or counted.
func ownEngineSnapshot(object *unstructured.Unstructured, oc *oramav1alpha1.OramaCloudV1) bool {
	labels := object.GetLabels()
	return labels["app.kubernetes.io/managed-by"] == "orama-cloud-v1-operator" &&
		labels["app.kubernetes.io/instance"] == oc.Name && labels["app.kubernetes.io/name"] == appName
}

// engineSnapshotsByClaim lists this CR's live snapshots in one call and groups them by source
// claim, each oldest first by the instant the operator took it.
func (r *OramaCloudV1Reconciler) engineSnapshotsByClaim(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (map[string][]takenSnapshot, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := r.List(ctx, list, client.InNamespace(oc.Namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"app.kubernetes.io/instance":   oc.Name,
	}); err != nil {
		return nil, err
	}
	byClaim := map[string][]takenSnapshot{}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetDeletionTimestamp() != nil || !ownEngineSnapshot(item, oc) {
			continue
		}
		claim, _, _ := unstructured.NestedString(item.Object, "spec", "source", "persistentVolumeClaimName")
		takenAt := item.GetCreationTimestamp().Time
		if at, err := time.Parse(time.RFC3339Nano, item.GetAnnotations()[snapshotAtAnnotation]); err == nil {
			takenAt = at
		}
		byClaim[claim] = append(byClaim[claim], takenSnapshot{object: item, takenAt: takenAt.UTC()})
	}
	for _, snapshots := range byClaim {
		sort.Slice(snapshots, func(i, j int) bool {
			if !snapshots[i].takenAt.Equal(snapshots[j].takenAt) {
				return snapshots[i].takenAt.Before(snapshots[j].takenAt)
			}
			return snapshots[i].object.GetName() < snapshots[j].object.GetName()
		})
	}
	return byClaim, nil
}

// takeEngineSnapshot creates the snapshot of claim for now. An object of the same name that is not
// this CR's snapshot of the claim is reported, never adopted.
func (r *OramaCloudV1Reconciler) takeEngineSnapshot(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, claim engineClaim, class string, now time.Time) (takenSnapshot, string, error) {
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	snapshot.SetNamespace(oc.Namespace)
	snapshot.SetName(snapshotName(claim.name, now))
	snapshot.SetLabels(labelsFor(oc, claim.component))
	snapshot.SetAnnotations(map[string]string{
		snapshotAtAnnotation: now.Format(time.RFC3339Nano),
		snapshotClaimAnnot:   claim.name,
	})
	snapshot.Object["spec"] = map[string]interface{}{
		"volumeSnapshotClassName": class,
		"source":                  map[string]interface{}{"persistentVolumeClaimName": claim.name},
	}
	if err := r.Create(ctx, snapshot); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return takenSnapshot{}, "", err
		}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(volumeSnapshotGVK)
		if err := r.Get(ctx, client.ObjectKeyFromObject(snapshot), existing); err != nil {
			return takenSnapshot{}, "", err
		}
		source, _, _ := unstructured.NestedString(existing.Object, "spec", "source", "persistentVolumeClaimName")
		if existing.GetDeletionTimestamp() != nil || !ownEngineSnapshot(existing, oc) || source != claim.name {
			return takenSnapshot{}, fmt.Sprintf("VolumeSnapshot %s/%s exists and is not this claim's snapshot; not adopted",
				oc.Namespace, snapshot.GetName()), nil
		}
		return takenSnapshot{object: existing, takenAt: now}, "", nil
	}
	log.FromContext(ctx).Info("took an engine volume snapshot", "claim", claim.name, "snapshot", snapshot.GetName())
	return takenSnapshot{object: snapshot, takenAt: now}, "", nil
}

// labelEngineSnapshotContent labels the content a ready snapshot is bound to with the CR and
// component and annotates it with its snapshot and claim, so retained contents can be found for a
// restore after the snapshot or its namespace is gone.
func (r *OramaCloudV1Reconciler) labelEngineSnapshotContent(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, snapshot takenSnapshot, claim engineClaim) error {
	name, _, _ := unstructured.NestedString(snapshot.object.Object, "status", "boundVolumeSnapshotContentName")
	if name == "" || !snapshot.ready() {
		return nil
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(volumeSnapshotContentGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		return client.IgnoreNotFound(err)
	}
	if content.GetLabels()["app.kubernetes.io/instance"] == oc.Name {
		return nil
	}
	patch := client.MergeFrom(content.DeepCopy())
	labels := content.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range labelsFor(oc, claim.component) {
		labels[k] = v
	}
	content.SetLabels(labels)
	annotations := content.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[snapshotNamespaceAnno] = snapshot.object.GetNamespace()
	annotations[snapshotNameAnnot] = snapshot.object.GetName()
	annotations[snapshotClaimAnnot] = claim.name
	annotations[snapshotAtAnnotation] = snapshot.takenAt.Format(time.RFC3339Nano)
	content.SetAnnotations(annotations)
	return r.Patch(ctx, content, patch)
}

// reconcileEngineSnapshots runs one pass: for each engine data claim it takes the snapshot that is
// due, prunes beyond keep, and reports the newest. It fails only on an invalid schedule or an API
// error; a class the operator cannot use is reported through SnapshotsReady.
func (r *OramaCloudV1Reconciler) reconcileEngineSnapshots(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (engineSnapshots, error) {
	cfg, err := resolveEngineSnapshots(oc)
	if err != nil || cfg == nil {
		return engineSnapshots{}, err
	}
	notReady := func(reason, message string) engineSnapshots {
		return engineSnapshots{condition: &metav1.Condition{Type: engineSnapshotsConditionType,
			Status: metav1.ConditionFalse, Reason: reason, Message: message}}
	}
	driver, problem, err := r.ensureEngineSnapshotClass(ctx, oc, cfg.class)
	if err != nil {
		return engineSnapshots{}, err
	}
	if problem != "" {
		result := notReady("ClassNotUsable", problem)
		result.statuses = oc.Status.EngineSnapshots
		return result, nil
	}
	claims, err := r.engineSnapshotClaims(ctx, oc)
	if err != nil {
		return engineSnapshots{}, err
	}
	if len(claims) == 0 {
		result := notReady("NoClaims", "no engine data claim exists yet")
		result.wait = engineSnapshotsPendingPoll
		return result, nil
	}
	byClaim, err := r.engineSnapshotsByClaim(ctx, oc)
	if err != nil {
		return engineSnapshots{}, err
	}
	// Whole seconds: the name carries seconds, and status times round-trip through JSON.
	now := r.now().Truncate(time.Second)
	var result engineSnapshots
	soonest := func(d time.Duration) {
		if result.wait == 0 || d < result.wait {
			result.wait = d
		}
	}
	var failed, stale, pending []string
	for _, claim := range claims {
		snapshots := byClaim[claim.name]
		_, due, _ := snapshotDue(snapshots, nil, cfg.schedule, now)
		var notes []string
		if due {
			taken, note, err := r.takeEngineSnapshot(ctx, oc, claim, cfg.class, now)
			if err != nil {
				return engineSnapshots{}, err
			}
			if note != "" {
				notes = append(notes, note)
			} else if len(snapshots) == 0 || snapshots[len(snapshots)-1].object.GetName() != taken.object.GetName() {
				snapshots = append(snapshots, taken)
			}
		}
		kept, pruneNotes, err := r.pruneEngineSnapshots(ctx, snapshots, cfg.keep, driver)
		if err != nil {
			return engineSnapshots{}, err
		}
		notes = append(notes, pruneNotes...)
		status := oramav1alpha1.EngineSnapshotStatus{Claim: claim.name, Component: claim.component}
		if len(kept) == 0 {
			pending = append(pending, claim.name+": no snapshot yet")
			result.statuses = append(result.statuses, status)
			soonest(engineSnapshotsPendingPoll)
			continue
		}
		newest := kept[len(kept)-1]
		if err := r.labelEngineSnapshotContent(ctx, oc, newest, claim); err != nil {
			return engineSnapshots{}, err
		}
		takenAt := metav1.NewTime(newest.takenAt)
		status.SnapshotName = newest.object.GetName()
		status.SnapshotTime = &takenAt
		status.ReadyToUse = newest.ready()
		status.Error = newest.failure()
		result.statuses = append(result.statuses, status)

		// Staleness is judged by the newest READY snapshot: one cut on time that never becomes
		// ready leaves the claim without a usable recent backup.
		var newestReady *takenSnapshot
		for i := range kept {
			if kept[i].ready() {
				newestReady = &kept[i]
			}
		}
		switch {
		case status.Error != "":
			failed = append(failed, fmt.Sprintf("%s: %s failed: %s", claim.name, status.SnapshotName, status.Error))
		case newestReady == nil:
			pending = append(pending, fmt.Sprintf("%s: %s is not ready yet", claim.name, status.SnapshotName))
		case !cfg.staleAfter(newestReady.takenAt).After(now):
			stale = append(stale, fmt.Sprintf("%s: newest ready snapshot %s was taken %s, more than two schedule periods ago",
				claim.name, newestReady.object.GetName(), newestReady.takenAt.Format(time.RFC3339)))
		}
		if len(notes) > 0 {
			log.FromContext(ctx).Info("engine volume snapshots of a claim need attention", "claim", claim.name, "notes", strings.Join(notes, "; "))
		}
		if !newest.ready() && status.Error == "" {
			soonest(engineSnapshotsPendingPoll)
		}
		soonest(max(cfg.schedule.Next(newest.takenAt).Sub(now), time.Second))
	}
	condition := metav1.Condition{Type: engineSnapshotsConditionType, Status: metav1.ConditionTrue, Reason: "SnapshotsCurrent",
		Message: fmt.Sprintf("every engine data claim (%d) has a ready snapshot within two schedule periods", len(claims))}
	switch {
	case len(failed) > 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, "SnapshotFailed"
		condition.Message = strings.Join(append(failed, stale...), "; ")
	case len(stale) > 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, "SnapshotStale"
		condition.Message = strings.Join(stale, "; ")
	case len(pending) > 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, "SnapshotPending"
		condition.Message = strings.Join(pending, "; ")
	}
	result.condition = &condition
	return result, nil
}

// staleAfter is when a snapshot taken at t becomes stale: once two scheduled runs after it have
// come, i.e. two schedule periods have passed.
func (c *engineSnapshotConfig) staleAfter(t time.Time) time.Time {
	return c.schedule.Next(c.schedule.Next(t))
}

// pruneEngineSnapshots bounds one claim's snapshots, which arrive oldest first, by the rules Lake's
// snapshots follow (decision 39). Only readyToUse snapshots count toward keep, and the newest ready
// one is never deleted. Of the failed ones, the newest is kept for diagnosis and older ones are
// deleted; a not-ready one without an error is in progress and is kept unless a newer ready snapshot
// exists, when it is treated as stuck and deleted. Each deletion goes through deleteEngineSnapshot,
// which may decline it with a note; a declined snapshot is kept and the note reported. It answers
// the snapshots kept, oldest first.
func (r *OramaCloudV1Reconciler) pruneEngineSnapshots(ctx context.Context, snapshots []takenSnapshot, keep int, driver string) ([]takenSnapshot, []string, error) {
	var ready, failed []int
	newestReady := -1
	for i, snapshot := range snapshots {
		switch {
		case snapshot.ready():
			ready = append(ready, i)
			newestReady = i
		case snapshotFailed(snapshot):
			failed = append(failed, i)
		}
	}
	doomed := map[int]bool{}
	if len(ready) > keep {
		for _, i := range ready[:len(ready)-keep] {
			doomed[i] = true
		}
	}
	if len(failed) > 1 {
		for _, i := range failed[:len(failed)-1] {
			doomed[i] = true
		}
	}
	for i, snapshot := range snapshots {
		if !snapshot.ready() && !snapshotFailed(snapshot) && newestReady > i {
			doomed[i] = true
		}
	}
	var kept []takenSnapshot
	var notes []string
	for i, snapshot := range snapshots {
		if !doomed[i] || i == newestReady {
			kept = append(kept, snapshot)
			continue
		}
		note, err := r.deleteEngineSnapshot(ctx, snapshot, driver)
		if err != nil {
			return nil, nil, err
		}
		if note != "" {
			notes = append(notes, note)
			kept = append(kept, snapshot)
		}
	}
	return kept, notes, nil
}

// deleteEngineSnapshot deletes one pruned snapshot together with its cloud snapshot. It reads the
// bound VolumeSnapshotContent first and touches only a content bound back to this snapshot by
// driver; it switches that content to deletionPolicy Delete, so the cloud snapshot goes with it, and
// then deletes the snapshot. The class stays Retain, so a snapshot never pruned keeps its contents.
// A snapshot without a bound content is deleted only when it failed (nothing was retained);
// otherwise it is kept, and the returned note says why.
func (r *OramaCloudV1Reconciler) deleteEngineSnapshot(ctx context.Context, snapshot takenSnapshot, driver string) (string, error) {
	name := snapshot.object.GetNamespace() + "/" + snapshot.object.GetName()
	contentName, _, _ := unstructured.NestedString(snapshot.object.Object, "status", "boundVolumeSnapshotContentName")
	if contentName == "" {
		if !snapshotFailed(snapshot) {
			return fmt.Sprintf("%s is not bound to a content yet; not pruned", name), nil
		}
	} else {
		content := &unstructured.Unstructured{}
		content.SetGroupVersionKind(volumeSnapshotContentGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Sprintf("%s names content %s, which does not exist; not pruned", name, contentName), nil
			}
			return "", err
		}
		contentDriver, _, _ := unstructured.NestedString(content.Object, "spec", "driver")
		refName, _, _ := unstructured.NestedString(content.Object, "spec", "volumeSnapshotRef", "name")
		refNamespace, _, _ := unstructured.NestedString(content.Object, "spec", "volumeSnapshotRef", "namespace")
		if contentDriver != driver {
			return fmt.Sprintf("%s is bound to content %s of driver %q, not %q; not pruned", name, contentName, contentDriver, driver), nil
		}
		if refName != snapshot.object.GetName() || refNamespace != snapshot.object.GetNamespace() {
			return fmt.Sprintf("content %s is not bound back to %s; not pruned", contentName, name), nil
		}
		if policy, _, _ := unstructured.NestedString(content.Object, "spec", "deletionPolicy"); policy != "Delete" {
			patch := client.MergeFrom(content.DeepCopy())
			if err := unstructured.SetNestedField(content.Object, "Delete", "spec", "deletionPolicy"); err != nil {
				return "", err
			}
			if err := r.Patch(ctx, content, patch); err != nil {
				return "", err
			}
		}
	}
	if err := r.Delete(ctx, snapshot.object); err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	log.FromContext(ctx).Info("pruned an engine volume snapshot", "snapshot", name)
	return "", nil
}
