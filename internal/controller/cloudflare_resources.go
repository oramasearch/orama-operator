/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// cfCredsFromSecret reads the Cloudflare token + zoneID + accountID from a Secret. A missing
// secret is a no-op (empty strings, nil error) so a resource waits until it's configured.
func cfCredsFromSecret(ctx context.Context, c client.Client, ns, name string) (token, zoneID, accountID string, err error) {
	if name == "" {
		return "", "", "", nil
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", nil
		}
		return "", "", "", err
	}
	return string(sec.Data["token"]), string(sec.Data["zoneID"]), string(sec.Data["accountID"]), nil
}

// ---- CloudflareKVNamespace ----

const cfKVFinalizer = "orama.cloud/cloudflare-kv-cleanup"

type CloudflareKVNamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarekvnamespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarekvnamespaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarekvnamespaces/finalizers,verbs=update

func (r *CloudflareKVNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kv oramav1alpha1.CloudflareKVNamespace
	if err := r.Get(ctx, req.NamespacedName, &kv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, _, accountID, err := cfCredsFromSecret(ctx, r.Client, kv.Namespace, kv.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !kv.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&kv, cfKVFinalizer) {
			if token != "" && accountID != "" && kv.Status.NamespaceID != "" {
				if err := cfDeleteKVNamespace(ctx, token, accountID, kv.Status.NamespaceID); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&kv, cfKVFinalizer)
			if err := r.Update(ctx, &kv); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&kv, cfKVFinalizer) {
		controllerutil.AddFinalizer(&kv, cfKVFinalizer)
		if err := r.Update(ctx, &kv); err != nil {
			return ctrl.Result{}, err
		}
	}
	if token == "" || accountID == "" {
		return ctrl.Result{}, nil // not configured yet — safe no-op
	}
	id, err := cfEnsureKVNamespace(ctx, token, accountID, kv.Spec.Title)
	if err != nil {
		// Written only when it changes, so a standing failure produces no writes.
		if kv.Status.Ready || kv.Status.Message != err.Error() {
			kv.Status.Ready, kv.Status.Message = false, err.Error()
			if statusErr := r.Status().Update(ctx, &kv); statusErr != nil {
				return settle(ctrl.Result{}, statusErr)
			}
		}
		return ctrl.Result{}, err
	}
	if kv.Status.NamespaceID != id || !kv.Status.Ready {
		kv.Status.NamespaceID, kv.Status.Ready, kv.Status.Message = id, true, "reconciled"
		if err := r.Status().Update(ctx, &kv); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *CloudflareKVNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareKVNamespace{}).Complete(r)
}

// cfEnsureKVNamespace returns the namespace ID for `title`, creating it if absent (idempotent).
func cfEnsureKVNamespace(ctx context.Context, token, accountID, title string) (string, error) {
	listURL := fmt.Sprintf("%s/accounts/%s/storage/kv/namespaces?per_page=100", cfAPIBase, accountID)
	var listResp struct {
		Result []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, listURL, nil, &listResp); err != nil {
		return "", err
	}
	for _, n := range listResp.Result {
		if n.Title == title {
			return n.ID, nil
		}
	}
	var createResp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	createURL := fmt.Sprintf("%s/accounts/%s/storage/kv/namespaces", cfAPIBase, accountID)
	if err := cfDo(ctx, token, http.MethodPost, createURL, map[string]interface{}{"title": title}, &createResp); err != nil {
		return "", err
	}
	return createResp.Result.ID, nil
}

func cfDeleteKVNamespace(ctx context.Context, token, accountID, id string) error {
	url := fmt.Sprintf("%s/accounts/%s/storage/kv/namespaces/%s", cfAPIBase, accountID, id)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}
