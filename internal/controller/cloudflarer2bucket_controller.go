/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// CloudflareR2Bucket: the bucket, its deletion policy, and credentials scoped to it (operator#453).
//
// With spec.credentials the operator mints an account-owned API token whose one policy grants
// "Workers R2 Storage Bucket Item Write" on this bucket alone, and writes the S3 credentials R2
// derives from it (access key id = token id, secret = SHA-256 of the token value;
// https://developers.cloudflare.com/r2/api/tokens/) to a Secret owned by the bucket resource. The
// token value is shown once by Cloudflare, so a lost Secret means a new token: the old one is
// revoked first. Deleting the resource revokes the token always, and deletes the bucket only with
// deletionPolicy Delete.

const (
	cfR2Finalizer = "orama.cloud/cloudflare-r2-cleanup"
	// cfR2ObjectWritePermission is the permission group a bucket token gets, looked up by name
	// because Cloudflare's ids are per API surface.
	cfR2ObjectWritePermission = "Workers R2 Storage Bucket Item Write"
	// r2TokenIDAnnotation records on the credentials Secret which token it was derived from.
	r2TokenIDAnnotation = "orama.cloud/r2-token-id"
)

// CloudflareR2BucketReconciler reconciles CloudflareR2Bucket.
type CloudflareR2BucketReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIBase is the Cloudflare API base URL; empty means the public API. Tests point it at a fake.
	APIBase string
}

//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets/finalizers,verbs=update

func (r *CloudflareR2BucketReconciler) apiBase() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return cfAPIBase
}

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
			if token != "" && accountID != "" {
				if b.Status.CredentialsTokenID != "" {
					if err := r.revokeToken(ctx, token, accountID, b.Status.CredentialsTokenID); err != nil {
						return ctrl.Result{}, err
					}
				}
				if b.Status.Created && b.Spec.DeletionPolicy == oramav1alpha1.BucketDeletionDelete {
					if err := cfDeleteR2Bucket(ctx, r.apiBase(), token, accountID, b.Spec.Name); err != nil {
						return ctrl.Result{}, err
					}
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
	desired := b.Status
	err = cfEnsureR2Bucket(ctx, r.apiBase(), token, accountID, b.Spec.Name)
	if err == nil {
		desired.Created = true
		err = r.reconcileCredentials(ctx, &b, &desired, token, accountID)
	}
	if err != nil {
		// Written only when it changes, so a standing failure produces no writes. The error is
		// returned, so it is logged and retried until its cause is fixed. For R2 the usual cause is
		// the account: R2 not enabled, a token without Workers R2 Storage: Edit, or (for
		// spec.credentials) a token that may not create API tokens.
		desired.Ready, desired.Message = false, err.Error()
	} else {
		desired.Ready, desired.Message = true, "reconciled"
	}
	if desired != b.Status {
		b.Status = desired
		if statusErr := r.Status().Update(ctx, &b); statusErr != nil {
			return settle(ctrl.Result{}, errors.Join(err, statusErr))
		}
	}
	return ctrl.Result{}, err
}

// reconcileCredentials keeps the bucket-scoped token and its Secret, or removes both when
// spec.credentials is unset. It updates status in place.
func (r *CloudflareR2BucketReconciler) reconcileCredentials(ctx context.Context, b *oramav1alpha1.CloudflareR2Bucket, status *oramav1alpha1.CloudflareR2BucketStatus, token, accountID string) error {
	if b.Spec.Credentials == nil {
		if status.CredentialsTokenID != "" {
			if err := r.revokeToken(ctx, token, accountID, status.CredentialsTokenID); err != nil {
				return err
			}
		}
		if status.CredentialsSecret != "" {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: status.CredentialsSecret, Namespace: b.Namespace}}
			if err := deleteIfExists(ctx, r.Client, secret); err != nil {
				return err
			}
		}
		status.CredentialsTokenID, status.CredentialsSecret = "", ""
		return nil
	}

	name := b.Spec.Credentials.SecretName
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && !metav1.IsControlledBy(&secret, b) {
		return fmt.Errorf("Secret %s/%s exists and is not this bucket's; choose another spec.credentials.secretName", b.Namespace, name)
	}
	if err == nil && status.CredentialsTokenID != "" && secret.Annotations[r2TokenIDAnnotation] == status.CredentialsTokenID {
		if status.CredentialsSecret != name {
			status.CredentialsSecret = name
		}
		return nil
	}
	// No Secret, or one from another token: its value cannot be read back, so mint a new token
	// and revoke the old one.
	if status.CredentialsTokenID != "" {
		if err := r.revokeToken(ctx, token, accountID, status.CredentialsTokenID); err != nil {
			return err
		}
		status.CredentialsTokenID = ""
	}
	id, value, err := r.mintBucketToken(ctx, token, accountID, b.Spec.Name)
	if err != nil {
		return err
	}
	status.CredentialsTokenID = id
	sum := sha256.Sum256([]byte(value))
	secret = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &secret, func() error {
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[r2TokenIDAnnotation] = id
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			"accessKeyId":     []byte(id),
			"secretAccessKey": []byte(hex.EncodeToString(sum[:])),
			"endpoint":        []byte(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)),
			"bucket":          []byte(b.Spec.Name),
			"region":          []byte("auto"),
		}
		return controllerutil.SetControllerReference(b, &secret, r.Scheme)
	}); err != nil {
		return err
	}
	status.CredentialsSecret = name
	return nil
}

func (r *CloudflareR2BucketReconciler) mintBucketToken(ctx context.Context, token, accountID, bucket string) (string, string, error) {
	var groups struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodGet, fmt.Sprintf("%s/accounts/%s/tokens/permission_groups", r.apiBase(), accountID), nil, &groups); err != nil {
		return "", "", err
	}
	groupID := ""
	for _, g := range groups.Result {
		if g.Name == cfR2ObjectWritePermission {
			groupID = g.ID
		}
	}
	if groupID == "" {
		return "", "", fmt.Errorf("the account offers no %q permission group to this token", cfR2ObjectWritePermission)
	}
	body := map[string]any{
		"name": fmt.Sprintf("orama-operator r2 %s", bucket),
		"policies": []map[string]any{{
			"effect":            "allow",
			"permission_groups": []map[string]string{{"id": groupID}},
			"resources":         map[string]string{fmt.Sprintf("com.cloudflare.edge.r2.bucket.%s_default_%s", accountID, bucket): "*"},
		}},
	}
	var created struct {
		Result struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := cfDo(ctx, token, http.MethodPost, fmt.Sprintf("%s/accounts/%s/tokens", r.apiBase(), accountID), body, &created); err != nil {
		return "", "", err
	}
	if created.Result.ID == "" || created.Result.Value == "" {
		return "", "", fmt.Errorf("cloudflare created a bucket token without an id or value")
	}
	return created.Result.ID, created.Result.Value, nil
}

// revokeToken deletes a bucket token; one already gone is fine.
func (r *CloudflareR2BucketReconciler) revokeToken(ctx context.Context, token, accountID, id string) error {
	err := cfDo(ctx, token, http.MethodDelete, fmt.Sprintf("%s/accounts/%s/tokens/%s", r.apiBase(), accountID, id), nil, nil)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (r *CloudflareR2BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&oramav1alpha1.CloudflareR2Bucket{}).Owns(&corev1.Secret{}).Complete(r)
}

// cfEnsureR2Bucket creates the bucket if absent (GET first, so a pre-existing bucket is fine: a
// Retain bucket whose resource is re-created is adopted).
func cfEnsureR2Bucket(ctx context.Context, base, token, accountID, name string) error {
	getURL := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s", base, accountID, name)
	if err := cfDo(ctx, token, http.MethodGet, getURL, nil, nil); err == nil {
		return nil // already exists
	}
	createURL := fmt.Sprintf("%s/accounts/%s/r2/buckets", base, accountID)
	return cfDo(ctx, token, http.MethodPost, createURL, map[string]interface{}{"name": name}, nil)
}

func cfDeleteR2Bucket(ctx context.Context, base, token, accountID, name string) error {
	url := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s", base, accountID, name)
	return cfDo(ctx, token, http.MethodDelete, url, nil, nil)
}
