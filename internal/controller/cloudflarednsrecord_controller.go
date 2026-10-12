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
// It only ever changes records it owns: the one whose ID is in status.recordID, or one whose
// Cloudflare comment carries this object's ownership marker (cfDNSOwnerMarker). Any other record
// with the same name blocks the reconcile with Ready=false and is never modified or deleted.
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
	token, zoneID, err := r.cfCreds(ctx, rec.Namespace, rec.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rec.Spec.ZoneID != "" {
		zoneID = rec.Spec.ZoneID
	}
	marker := cfDNSOwnerMarker(&rec)

	// Deletion: run the finalizer (delete the record from Cloudflare if still ours), then release.
	if !rec.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) {
			if token != "" && zoneID != "" && rec.Status.RecordID != "" {
				deleted, err := cfDeleteOwnedDNSRecord(ctx, token, zoneID, rec.Status.RecordID, marker)
				if err != nil {
					return ctrl.Result{}, err
				}
				if !deleted {
					r.event(&rec, corev1.EventTypeWarning, "RecordNotOwned", fmt.Sprintf(
						"Cloudflare record %s no longer carries the ownership comment %q; left it in place", rec.Status.RecordID, marker))
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

	id, err := cfUpsertDNSRecordID(ctx, token, zoneID, &rec.Spec, rec.Status.RecordID, marker)
	var conflict *cfDNSConflictError
	if errors.As(err, &conflict) {
		if rec.Status.Ready || rec.Status.Message != conflict.Error() {
			rec.Status.Ready, rec.Status.Message = false, conflict.Error()
			if err := r.Status().Update(ctx, &rec); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: cfDNSConflictRequeue}, nil
	}
	if err != nil {
		rec.Status.Ready, rec.Status.Message = false, err.Error()
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{}, err
	}
	if rec.Status.RecordID != id || !rec.Status.Ready || rec.Status.Message != "reconciled" {
		rec.Status.RecordID, rec.Status.Ready, rec.Status.Message = id, true, "reconciled"
		if err := r.Status().Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
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

// cfDNSOwnerMarker is the Cloudflare record comment that marks a record as created and owned by
// this CloudflareDNSRecord. Records without it are never modified or deleted (except the legacy
// record whose ID is already in status.recordID, which gains the marker on its next update).
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

// cfUpsertDNSRecordID creates or updates the proxied DNS record this object owns and returns its
// Cloudflare ID. It lists every record with the spec's name (any type): a record is ours when its
// ID is knownID (status.recordID) or its comment equals marker; any other one is a conflict
// (*cfDNSConflictError) and nothing is written. An owned record is updated only when it differs
// from the desired state, and the update sets the marker. (Reuses cfDo + cfAPIBase from cloudflare.go.)
func cfUpsertDNSRecordID(ctx context.Context, token, zoneID string, spec *oramav1alpha1.CloudflareDNSRecordSpec, knownID, marker string) (string, error) {
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
		return "", err
	}
	var owned *cfDNSRecord
	for i := range listResp.Result {
		existing := &listResp.Result[i]
		switch {
		case knownID != "" && existing.ID == knownID:
			owned = existing
		case existing.Comment == marker:
			if owned == nil {
				owned = existing
			}
		default:
			return "", &cfDNSConflictError{Record: *existing}
		}
	}

	body := map[string]interface{}{"type": recType, "name": spec.Name, "content": spec.Content, "ttl": 1, "proxied": spec.Proxied, "comment": marker}
	if owned != nil {
		if owned.Type == recType && owned.Content == spec.Content && owned.Proxied == spec.Proxied && owned.TTL == 1 && owned.Comment == marker {
			return owned.ID, nil
		}
		putURL := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, owned.ID)
		if err := cfDo(ctx, token, http.MethodPut, putURL, body, nil); err != nil {
			return "", err
		}
		return owned.ID, nil
	}
	var resp struct {
		Result cfDNSRecord `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID), body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

// cfDeleteOwnedDNSRecord deletes recordID only while it still carries marker. It reports false,
// without deleting, when the record now belongs to someone else (comment removed or changed); a
// record that is already gone counts as deleted.
func cfDeleteOwnedDNSRecord(ctx context.Context, token, zoneID, recordID, marker string) (bool, error) {
	recordURL := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	var getResp struct {
		Result cfDNSRecord `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, recordURL, nil, &getResp); err != nil {
		var apiErr *cfAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return true, nil
		}
		return false, err
	}
	if getResp.Result.Comment != marker {
		return false, nil
	}
	return true, cfDo(ctx, token, http.MethodDelete, recordURL, nil, nil)
}
