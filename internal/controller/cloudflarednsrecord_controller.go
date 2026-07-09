/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
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
			if token != "" && zoneID != "" && rec.Status.RecordID != "" {
				if err := cfDeleteDNSRecord(ctx, token, zoneID, rec.Status.RecordID); err != nil {
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

	id, err := cfUpsertDNSRecordID(ctx, token, zoneID, &rec.Spec)
	if err != nil {
		rec.Status.Ready, rec.Status.Message = false, err.Error()
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{}, err
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
func cfUpsertDNSRecordID(ctx context.Context, token, zoneID string, spec *oramav1alpha1.CloudflareDNSRecordSpec) (string, error) {
	recType := spec.Type
	if recType == "" {
		recType = "A"
		if net.ParseIP(spec.Content) == nil {
			recType = "CNAME"
		}
	}
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=%s&name=%s", cfAPIBase, zoneID, recType, spec.Name)
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
		url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, listResp.Result[0].ID)
		if err := cfDo(ctx, token, http.MethodPut, url, body, &resp); err != nil {
			return "", err
		}
		return listResp.Result[0].ID, nil
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	if err := cfDo(ctx, token, http.MethodPost, url, body, &resp); err != nil {
		return "", err
	}
	return resp.Result.ID, nil
}

func cfDeleteDNSRecord(ctx context.Context, token, zoneID, recordID string) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}
