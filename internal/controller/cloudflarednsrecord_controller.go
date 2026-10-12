/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cfDNSFinalizer ensures the Cloudflare record is deleted from CF before the CR is released,
// so tearing down the owning OramaCloudV1 cascades to real external cleanup.
const cfDNSFinalizer = "orama.cloud/cloudflare-dns-cleanup"

// cfDNSConflictRequeue is how often a record blocked by a foreign Cloudflare record is retried,
// so it converges once the foreign record is removed without hammering the API meanwhile.
const cfDNSConflictRequeue = 5 * time.Minute

// CloudflareDNSRecordReconciler provisions a Cloudflare DNS record via the CF API and reflects
// the assigned record ID in status. It is the reference "external resource as an owned CRD"
// controller (epic #31): create/update on reconcile, delete-from-Cloudflare on finalize.
//
// It never changes a DNS record it did not create, and never changes an existing record beyond
// what it did before ownership comments (operator#312):
//   - A record it creates carries the comment cfDNSOwnerMarker and is recorded in
//     status.ownerComment; such a record is owned only while it still carries the comment.
//   - A legacy record (created before ownership comments) is owned through status.recordID. It is
//     never given a comment, is rewritten only when its type, content, proxying or TTL differs
//     from the spec, and is deleted with its object by ID, exactly as before.
//   - Any other A, AAAA or CNAME record with the same name blocks the reconcile with Ready=false.
//   - A record is never moved between zones: if the spec resolves to a zone other than
//     status.zoneID, nothing is sent to Cloudflare and Ready is false.
type CloudflareDNSRecordReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Events on the CR (e.g. a finalizer that left a foreign record alone). Optional.
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *CloudflareDNSRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec oramav1alpha1.CloudflareDNSRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, secretZoneID, err := r.cfCreds(ctx, rec.Namespace, rec.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	// zoneID is where the spec wants the record; recordZoneID is where the existing record lives
	// (a legacy status without zoneID means the Secret's zone, where it was created).
	zoneID := secretZoneID
	if rec.Spec.ZoneID != "" {
		zoneID = rec.Spec.ZoneID
	}
	recordZoneID := rec.Status.ZoneID
	if recordZoneID == "" {
		recordZoneID = secretZoneID
	}
	marker := cfDNSOwnerMarker(&rec)

	// Deletion: run the finalizer (delete the record from Cloudflare if still ours), then release.
	if !rec.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) {
			if token != "" && recordZoneID != "" && rec.Status.RecordID != "" {
				deleted, err := cfDeleteOwnedDNSRecord(ctx, token, recordZoneID, rec.Status.RecordID, rec.Status.OwnerComment)
				if err != nil {
					return ctrl.Result{}, err
				}
				if !deleted {
					r.event(&rec, corev1.EventTypeWarning, "RecordNotOwned", fmt.Sprintf(
						"Cloudflare record %s no longer carries the ownership comment %q; left it in place", rec.Status.RecordID, rec.Status.OwnerComment))
				}
			}
			controllerutil.RemoveFinalizer(&rec, cfDNSFinalizer)
			if err := r.Update(ctx, &rec); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Register the finalizer before creating any external state.
	if !controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) {
		controllerutil.AddFinalizer(&rec, cfDNSFinalizer)
		if err := r.Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Safe no-op until the Cloudflare secret is present (token+zoneID).
	if token == "" || zoneID == "" {
		return ctrl.Result{}, nil
	}

	// Never move an existing record to another zone: that would delete it in one zone and create
	// it in the other. Leave it untouched and say why.
	if rec.Status.RecordID != "" && recordZoneID != zoneID {
		msg := fmt.Sprintf("zone change refused: record %s is in zone %s but the spec now resolves to zone %s; "+
			"the operator does not move existing records between zones, so nothing was changed in either zone",
			rec.Status.RecordID, recordZoneID, zoneID)
		return r.notReady(ctx, &rec, msg)
	}

	legacyID := ""
	if rec.Status.OwnerComment == "" {
		legacyID = rec.Status.RecordID
	}
	owned, err := cfUpsertDNSRecord(ctx, token, zoneID, &rec.Spec, legacyID, marker)
	var conflict *cfDNSConflictError
	if errors.As(err, &conflict) {
		return r.notReady(ctx, &rec, conflict.Error())
	}
	if err != nil {
		rec.Status.Ready, rec.Status.Message = false, err.Error()
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{}, err
	}
	ownerComment := ""
	if owned.Comment == marker {
		ownerComment = marker
	}
	if rec.Status.RecordID != owned.ID || rec.Status.ZoneID != zoneID || rec.Status.OwnerComment != ownerComment ||
		!rec.Status.Ready || rec.Status.Message != "reconciled" {
		rec.Status.RecordID, rec.Status.ZoneID, rec.Status.OwnerComment = owned.ID, zoneID, ownerComment
		rec.Status.Ready, rec.Status.Message = true, "reconciled"
		if err := r.Status().Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// notReady records a reconcile that was refused without touching Cloudflare (a foreign record or a
// zone change) and retries later, so it converges once the cause is removed.
func (r *CloudflareDNSRecordReconciler) notReady(ctx context.Context, rec *oramav1alpha1.CloudflareDNSRecord, msg string) (ctrl.Result, error) {
	if rec.Status.Ready || rec.Status.Message != msg {
		rec.Status.Ready, rec.Status.Message = false, msg
		if err := r.Status().Update(ctx, rec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: cfDNSConflictRequeue}, nil
}

// event records a Kubernetes Event on the CR when a recorder is configured.
func (r *CloudflareDNSRecordReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, message)
	}
}

// cfCreds reads the Cloudflare token + zoneID from the referenced Secret (nil-safe: a missing
// secret is a no-op, not an error, so the record waits until it's configured).
func (r *CloudflareDNSRecordReconciler) cfCreds(ctx context.Context, ns, name string) (token, zoneID string, err error) {
	if name == "" {
		return "", "", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(sec.Data["token"]), string(sec.Data["zoneID"]), nil
}

func (r *CloudflareDNSRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareDNSRecord{}).Complete(r)
}

// cfDNSOwnerMarker is the Cloudflare record comment the operator puts on every record it creates,
// naming the CloudflareDNSRecord that owns it.
func cfDNSOwnerMarker(rec *oramav1alpha1.CloudflareDNSRecord) string {
	return "orama-operator:" + rec.Namespace + "/" + rec.Name
}

// cfDNSRecord is the subset of a Cloudflare DNS record the reconciler reads.
type cfDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

// cfDNSConflictError reports a same-name record this object does not own; the reconciler leaves it
// untouched and surfaces the message in status.
type cfDNSConflictError struct{ Record cfDNSRecord }

func (e *cfDNSConflictError) Error() string {
	return fmt.Sprintf("conflict: Cloudflare already has a %s record %s (id %s) that this object does not own; not modifying it",
		e.Record.Type, e.Record.Name, e.Record.ID)
}

// cfDNSAddressTypes are the record types that cannot share a name with this object's A or CNAME
// record without one of them being repointed; other types (TXT, MX, ...) at the name are ignored.
var cfDNSAddressTypes = map[string]bool{"A": true, "AAAA": true, "CNAME": true}

// cfUpsertDNSRecord creates or updates the proxied DNS record this object owns and returns it.
// It lists the records with the spec's name: a record is ours when its ID is legacyID (a record
// created before ownership comments, found through status.recordID) or its comment equals marker.
// Any other A, AAAA or CNAME record is a conflict (*cfDNSConflictError) and nothing is written.
// An owned record is rewritten only when its type, content, proxying or TTL differs, and the
// rewrite keeps whatever comment it has, so a legacy record never gains one. A new record is
// created with marker as its comment. (Reuses cfDo + cfAPIBase from cloudflare.go.)
func cfUpsertDNSRecord(ctx context.Context, token, zoneID string, spec *oramav1alpha1.CloudflareDNSRecordSpec, legacyID, marker string) (cfDNSRecord, error) {
	recType := spec.Type
	if recType == "" {
		recType = "A"
		if net.ParseIP(spec.Content) == nil {
			recType = "CNAME"
		}
	}
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?%s", cfAPIBase, zoneID, url.Values{"name": {spec.Name}}.Encode())
	var listResp struct {
		Result []cfDNSRecord `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
		return cfDNSRecord{}, err
	}
	var owned *cfDNSRecord
	for i := range listResp.Result {
		existing := &listResp.Result[i]
		switch {
		case legacyID != "" && existing.ID == legacyID:
			owned = existing
		case existing.Comment == marker:
			if owned == nil {
				owned = existing
			}
		case cfDNSAddressTypes[existing.Type]:
			return cfDNSRecord{}, &cfDNSConflictError{Record: *existing}
		}
	}

	body := map[string]interface{}{"type": recType, "name": spec.Name, "content": spec.Content, "ttl": 1, "proxied": spec.Proxied}
	if owned != nil {
		if owned.Type == recType && owned.Content == spec.Content && owned.Proxied == spec.Proxied && owned.TTL == 1 {
			return *owned, nil
		}
		// PUT replaces the whole record, so send its current comment to leave that untouched.
		body["comment"] = owned.Comment
		putURL := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, owned.ID)
		if err := cfDo(ctx, token, http.MethodPut, putURL, body, nil); err != nil {
			return cfDNSRecord{}, err
		}
		return *owned, nil
	}
	body["comment"] = marker
	var resp struct {
		Result cfDNSRecord `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID), body, &resp); err != nil {
		return cfDNSRecord{}, err
	}
	return cfDNSRecord{ID: resp.Result.ID, Comment: marker}, nil
}

// cfDeleteOwnedDNSRecord deletes recordID from zoneID if this object still owns it. ownerComment is
// status.ownerComment: when set, the record is fetched first and deleted only while it still
// carries that comment (false, nothing deleted, otherwise); when empty, the record is a legacy one
// and is deleted by ID, as the finalizer always did. A record that is already gone counts as deleted.
func cfDeleteOwnedDNSRecord(ctx context.Context, token, zoneID, recordID, ownerComment string) (bool, error) {
	recordURL := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	gone := func(err error) bool {
		var apiErr *cfAPIError
		return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
	}
	if ownerComment != "" {
		var getResp struct {
			Result cfDNSRecord `json:"result"`
		}
		if err := cfDo(ctx, token, http.MethodGet, recordURL, nil, &getResp); err != nil {
			if gone(err) {
				return true, nil
			}
			return false, err
		}
		if getResp.Result.Comment != ownerComment {
			return false, nil
		}
	}
	if err := cfDo(ctx, token, http.MethodDelete, recordURL, nil, nil); err != nil && !gone(err) {
		return false, err
	}
	return true, nil
}
