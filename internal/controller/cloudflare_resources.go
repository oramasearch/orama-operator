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

// ---- CloudflareR2Bucket ----

const cfR2Finalizer = "orama.cloud/cloudflare-r2-cleanup"

type CloudflareR2BucketReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets/finalizers,verbs=update

func (r *CloudflareR2BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b oramav1alpha1.CloudflareR2Bucket
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token, _, accountID, err := cfCredsFromSecret(ctx, r.Client, b.Namespace, b.Spec.SecretRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !b.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&b, cfR2Finalizer) {
			if token != "" && accountID != "" && b.Status.Created {
				if err := cfDeleteR2Bucket(ctx, token, accountID, b.Spec.Name); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&b, cfR2Finalizer)
			if err := r.Update(ctx, &b); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&b, cfR2Finalizer) {
		controllerutil.AddFinalizer(&b, cfR2Finalizer)
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
	if token == "" || accountID == "" {
		return ctrl.Result{}, nil
	}
	if err := cfEnsureR2Bucket(ctx, token, accountID, b.Spec.Name); err != nil {
		// Written only when it changes, so a standing failure produces no writes.
		if b.Status.Ready || b.Status.Message != err.Error() {
			b.Status.Ready, b.Status.Message = false, err.Error()
			if statusErr := r.Status().Update(ctx, &b); statusErr != nil {
				return settle(ctrl.Result{}, statusErr)
			}
		}
		// The error is returned, so it is logged and retried until its cause is fixed. For R2 the
		// usual cause is the account: R2 not enabled, or a token without Workers R2 Storage: Edit.
		return ctrl.Result{}, err
	}
	if !b.Status.Created || !b.Status.Ready {
		b.Status.Created, b.Status.Ready, b.Status.Message = true, true, "reconciled"
		if err := r.Status().Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *CloudflareR2BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareR2Bucket{}).Complete(r)
}

// cfEnsureR2Bucket creates the bucket if absent (GET first, so a pre-existing bucket is fine).
func cfEnsureR2Bucket(ctx context.Context, token, accountID, name string) error {
	getURL := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s", cfAPIBase, accountID, name)
	if err := cfDo(ctx, token, http.MethodGet, getURL, nil, nil); err == nil {
		return nil // already exists
	}
	createURL := fmt.Sprintf("%s/accounts/%s/r2/buckets", cfAPIBase, accountID)
	return cfDo(ctx, token, http.MethodPost, createURL, map[string]interface{}{"name": name}, nil)
}

func cfDeleteR2Bucket(ctx context.Context, token, accountID, name string) error {
	url := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s", cfAPIBase, accountID, name)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}
