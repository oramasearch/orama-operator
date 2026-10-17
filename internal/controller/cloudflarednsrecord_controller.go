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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cfDNSFinalizer ensures the Cloudflare record is deleted from CF before the CR is released,
// so tearing down the owning OramaCloudV1 cascades to real external cleanup.
const cfDNSFinalizer = "orama.cloud/cloudflare-dns-cleanup"

// CloudflareDNSRecordReconciler provisions a Cloudflare DNS record via the CF API and reflects
// the assigned record ID in status. It is the reference "external resource as an owned CRD"
// controller (epic #31): create/update on reconcile, delete-from-Cloudflare on finalize.
type CloudflareDNSRecordReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIBase is the Cloudflare API base URL; empty means the public API. Tests point it at a fake.
	APIBase string
}

func (r *CloudflareDNSRecordReconciler) api() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return cfAPIBase
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords/finalizers,verbs=update

func (r *CloudflareDNSRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec oramav1alpha1.CloudflareDNSRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, zoneID, err := r.cfCreds(ctx, rec.Namespace, rec.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Deletion: run the finalizer (delete the record from Cloudflare), then release.
	if !rec.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rec, cfDNSFinalizer) {
			// Retain leaves the record in Cloudflare (a record another resource also keeps).
			if rec.Spec.DeletionPolicy != "Retain" && token != "" && zoneID != "" && rec.Status.RecordID != "" {
				if err := cfDeleteDNSRecord(ctx, r.api(), token, zoneID, rec.Status.RecordID); err != nil {
					return ctrl.Result{}, err
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

	id, err := cfUpsertDNSRecordID(ctx, r.api(), token, zoneID, &rec.Spec)
	if err != nil {
		rec.Status.Ready, rec.Status.Message = false, err.Error()
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{}, err
	}
	// A changed name (or type) is a different record in Cloudflare. The one this resource made
	// before is deleted before the new ID is recorded, so a rename never leaves the old name live
	// (#529). If the delete fails, the old ID stays in status and the next pass retries it.
	if previous := rec.Status.RecordID; previous != "" && previous != id {
		if err := cfDeleteDNSRecord(ctx, r.api(), token, zoneID, previous); err != nil {
			rec.Status.Ready, rec.Status.Message = false, "deleting the previous record "+previous+": "+err.Error()
			_ = r.Status().Update(ctx, &rec)
			return ctrl.Result{}, err
		}
	}
	if rec.Status.RecordID != id || !rec.Status.Ready {
		rec.Status.RecordID, rec.Status.Ready, rec.Status.Message = id, true, "reconciled"
		if err := r.Status().Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
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

// cfUpsertDNSRecordID creates or updates a proxied DNS record and returns its Cloudflare ID.
// (Reuses cfDo + cfAPIBase from cloudflare.go.)
func cfUpsertDNSRecordID(ctx context.Context, api, token, zoneID string, spec *oramav1alpha1.CloudflareDNSRecordSpec) (string, error) {
	recType := spec.Type
	if recType == "" {
		recType = "A"
		if net.ParseIP(spec.Content) == nil {
			recType = "CNAME"
		}
	}
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=%s&name=%s", api, zoneID, recType, spec.Name)
	var listResp struct {
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
		return "", err
	}
	body := map[string]interface{}{"type": recType, "name": spec.Name, "content": spec.Content, "ttl": 1, "proxied": spec.Proxied}
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if len(listResp.Result) > 0 {
		url := fmt.Sprintf("%s/zones/%s/dns_records/%s", api, zoneID, listResp.Result[0].ID)
		if err := cfDo(ctx, token, http.MethodPut, url, body, &resp); err != nil {
			return "", err
		}
		return listResp.Result[0].ID, nil
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records", api, zoneID)
	if err := cfDo(ctx, token, http.MethodPost, url, body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

// cfDeleteDNSRecord deletes a record by ID. A record that is already gone counts as deleted.
func cfDeleteDNSRecord(ctx context.Context, api, token, zoneID, recordID string) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", api, zoneID, recordID)
	err := cfDo(ctx, token, http.MethodDelete, url, nil, nil)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}
