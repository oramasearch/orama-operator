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
	"strconv"
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

// cfDNSFinalizer cleans up only records still owned by this resource, or tracked legacy IDs.
const cfDNSFinalizer = "orama.cloud/cloudflare-dns-cleanup"

const cfDNSRetryAfter = 5 * time.Minute

// CloudflareDNSRecordReconciler creates marked records and reconciles only owned records.
// Pre-upgrade status IDs retain their legacy ownership without adding a comment.
type CloudflareDNSRecordReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
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
	// Persist the finalizer before creating external state. Patch metadata only: a full
	// update omits proxied=false and could let the CRD default restore true.
	if rec.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) {
		patch := client.MergeFromWithOptions(rec.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(&rec, cfDNSFinalizer)
		if err := r.Patch(ctx, &rec, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !rec.DeletionTimestamp.IsZero() && (!controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) || rec.Status.RecordID == "") {
		return ctrl.Result{}, r.releaseDNSFinalizer(ctx, &rec)
	}
	previous := rec.Status
	token, zoneID, err := r.cfCreds(ctx, rec.Namespace, rec.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if token == "" || zoneID == "" {
		rec.Status.Ready, rec.Status.Message = false, "waiting for Cloudflare credentials (token and zoneID) in spec.secretRef"
		return ctrl.Result{RequeueAfter: cfDNSRetryAfter}, r.updateDNSStatus(ctx, &rec, previous)
	}
	if !rec.DeletionTimestamp.IsZero() {
		deleted, err := cfDeleteOwnedDNSRecord(ctx, token, zoneID, rec.Status.RecordID, rec.Status.OwnerComment)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !deleted && r.Recorder != nil {
			r.Recorder.Eventf(&rec, corev1.EventTypeWarning, "RecordNotOwned", "Cloudflare record %s (%s) no longer carries expected comment %q; left intact", rec.Spec.Name, rec.Status.RecordID, rec.Status.OwnerComment)
		}
		return ctrl.Result{}, r.releaseDNSFinalizer(ctx, &rec)
	}
	legacyID := ""
	marker := rec.Status.OwnerComment
	if marker == "" {
		legacyID = rec.Status.RecordID
		marker = cfDNSOwnerMarker(&rec)
	}
	dns, err := cfUpsertDNSRecord(ctx, token, zoneID, &rec.Spec, legacyID, marker)
	if err != nil {
		rec.Status.Ready, rec.Status.Message = false, err.Error()
		if statusErr := r.updateDNSStatus(ctx, &rec, previous); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		var conflict *cfDNSConflictError
		var invalid *cfDNSRetryError
		if errors.As(err, &conflict) || errors.As(err, &invalid) {
			return ctrl.Result{RequeueAfter: cfDNSRetryAfter}, nil
		}
		return ctrl.Result{}, err
	}
	rec.Status.RecordID, rec.Status.Ready, rec.Status.Message = dns.ID, true, "reconciled"
	if legacyID == "" || dns.ID != legacyID {
		rec.Status.OwnerComment = dns.Comment
	}
	return ctrl.Result{}, r.updateDNSStatus(ctx, &rec, previous)
}

func (r *CloudflareDNSRecordReconciler) updateDNSStatus(ctx context.Context, rec *oramav1alpha1.CloudflareDNSRecord, previous oramav1alpha1.CloudflareDNSRecordStatus) error {
	if rec.Status == previous {
		return nil
	}
	return r.Status().Update(ctx, rec)
}

func (r *CloudflareDNSRecordReconciler) releaseDNSFinalizer(ctx context.Context, rec *oramav1alpha1.CloudflareDNSRecord) error {
	if !controllerutil.ContainsFinalizer(rec, cfDNSFinalizer) {
		return nil
	}
	patch := client.MergeFromWithOptions(rec.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(rec, cfDNSFinalizer)
	return r.Patch(ctx, rec, patch)
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

type cfDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

type cfDNSConflictError struct{ Record cfDNSRecord }

func (e *cfDNSConflictError) Error() string {
	return fmt.Sprintf("Cloudflare DNS conflict: %s %s %s (ID %s) is not owned by this resource", e.Record.Name, e.Record.Type, e.Record.Content, e.Record.ID)
}

// cfDNSRetryError reports a configuration or ownership ambiguity requiring no API writes.
type cfDNSRetryError struct{ message string }

func (e *cfDNSRetryError) Error() string { return e.message }

func cfDNSOwnerMarker(rec *oramav1alpha1.CloudflareDNSRecord) string {
	return "orama-operator:" + rec.Namespace + "/" + rec.Name
}

// cfUpsertDNSRecord reads every name match before writing. Only a successful create or
// a confirmed owned record is returned; foreign address records block every mutation.
func cfUpsertDNSRecord(ctx context.Context, token, zoneID string, spec *oramav1alpha1.CloudflareDNSRecordSpec, legacyID, marker string) (cfDNSRecord, error) {
	endpoint := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	var records []cfDNSRecord
	for page := 1; ; page++ {
		query := url.Values{"name": {spec.Name}, "page": {strconv.Itoa(page)}}
		var response struct {
			Result     []cfDNSRecord `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		if err := cfDo(ctx, token, http.MethodGet, endpoint+"?"+query.Encode(), nil, &response); err != nil {
			return cfDNSRecord{}, err
		}
		records = append(records, response.Result...)
		if page >= response.ResultInfo.TotalPages {
			break
		}
	}
	var owned []cfDNSRecord
	for _, rec := range records {
		if rec.Comment == marker && marker != "" || legacyID != "" && rec.ID == legacyID {
			owned = append(owned, rec)
		} else if rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME" {
			return cfDNSRecord{}, &cfDNSConflictError{Record: rec}
		}
	}
	if len(owned) > 1 {
		return cfDNSRecord{}, &cfDNSRetryError{message: fmt.Sprintf("ambiguous Cloudflare DNS ownership: %d records at %s match this resource", len(owned), spec.Name)}
	}
	desired := cfDNSRecord{Type: spec.Type, Name: spec.Name, Content: spec.Content, Proxied: spec.Proxied, TTL: 1, Comment: marker}
	if desired.Type == "" {
		desired.Type = "A"
		if net.ParseIP(spec.Content) == nil {
			desired.Type = "CNAME"
		}
	}
	if len(owned) == 1 {
		current := owned[0]
		if current.ID == "" {
			return cfDNSRecord{}, fmt.Errorf("Cloudflare returned an owned DNS record without an ID")
		}
		if current.Type == desired.Type && current.Content == desired.Content && current.Proxied == desired.Proxied && current.TTL == desired.TTL {
			return current, nil
		}
		desired.Comment = current.Comment
		if err := cfDo(ctx, token, http.MethodPut, endpoint+"/"+current.ID, desired, nil); err != nil {
			return cfDNSRecord{}, err
		}
		desired.ID = current.ID
		return desired, nil
	}
	// The comment limit applies only to new records, never to legacy reads or cleanup.
	if len(marker) > 100 {
		return cfDNSRecord{}, &cfDNSRetryError{message: fmt.Sprintf("Cloudflare DNS ownership comment is %d characters; maximum is 100 (namespace/name must be at most 85)", len(marker))}
	}
	var response struct {
		Result cfDNSRecord `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, endpoint, desired, &response); err != nil {
		return cfDNSRecord{}, err
	}
	if response.Result.ID == "" {
		return cfDNSRecord{}, fmt.Errorf("Cloudflare DNS create returned an empty record ID")
	}
	desired.ID = response.Result.ID
	return desired, nil
}

// cfDeleteOwnedDNSRecord returns false only when ownership was lost and the record is
// left intact. Legacy IDs intentionally retain their pre-comment cleanup behavior.
func cfDeleteOwnedDNSRecord(ctx context.Context, token, zoneID, recordID, ownerComment string) (bool, error) {
	endpoint := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	if ownerComment != "" {
		var response struct {
			Result cfDNSRecord `json:"result"`
		}
		if err := cfDo(ctx, token, http.MethodGet, endpoint, nil, &response); err != nil {
			var apiErr *cfAPIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return true, nil
			}
			return false, err
		}
		if response.Result.Comment != ownerComment {
			return false, nil
		}
	}
	if err := cfDo(ctx, token, http.MethodDelete, endpoint, nil, nil); err != nil {
		var apiErr *cfAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return true, nil
		}
		return false, err
	}
	return true, nil
}
