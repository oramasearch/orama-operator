// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

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
				if b.Status.CustomDomain != "" {
					if err := r.detachCustomDomain(ctx, token, accountID, b.Spec.Name, b.Status.CustomDomain); err != nil {
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
		err = r.reconcileBucketConfiguration(ctx, &b, token, accountID)
	}
	if err == nil {
		err = r.reconcileCredentials(ctx, &b, &desired, token, accountID)
	}
	if err == nil {
		err = r.reconcileCustomDomain(ctx, &b, &desired, token, accountID)
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
	if err == nil && b.Spec.CustomDomain != nil && desired.CustomDomainStatus != r2CustomDomainActive {
		// Cloudflare verifies the host and issues its certificate on its own; look again until both
		// are active.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, err
}

const r2CustomDomainActive = "ownership=active ssl=active"

type cfR2CORSRule struct {
	Allowed struct {
		Origins []string `json:"origins"`
		Methods []string `json:"methods"`
		Headers []string `json:"headers,omitempty"`
	} `json:"allowed"`
	ExposeHeaders []string `json:"exposeHeaders,omitempty"`
	MaxAgeSeconds int32    `json:"maxAgeSeconds,omitempty"`
}

type cfR2CORS struct {
	Rules []cfR2CORSRule `json:"rules"`
}

type cfR2LifecycleTransition struct {
	Condition struct {
		Type   string `json:"type"`
		MaxAge int64  `json:"maxAge,omitempty"`
		Date   string `json:"date,omitempty"`
	} `json:"condition"`
}

type cfR2LifecycleRule struct {
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	Conditions struct {
		Prefix string `json:"prefix"`
	} `json:"conditions"`
	DeleteObjectsTransition         *cfR2LifecycleTransition `json:"deleteObjectsTransition,omitempty"`
	AbortMultipartUploadsTransition *cfR2LifecycleTransition `json:"abortMultipartUploadsTransition,omitempty"`
	StorageClassTransitions         []json.RawMessage        `json:"storageClassTransitions,omitempty"`
}

// canonicalCORS compares effective rules, ignoring provider-assigned IDs, set ordering, and
// omitted versus empty optional arrays. Header names are case-insensitive.
func canonicalCORS(config cfR2CORS) []string {
	set := func(values []string, headers bool) []string {
		values = slices.Clone(values)
		if headers {
			for i := range values {
				values[i] = strings.ToLower(values[i])
			}
		}
		slices.Sort(values)
		return slices.Compact(values)
	}
	rules := make([]string, 0, len(config.Rules))
	for _, rule := range config.Rules {
		rule.Allowed.Origins = set(rule.Allowed.Origins, false)
		rule.Allowed.Methods = set(rule.Allowed.Methods, false)
		rule.Allowed.Headers = set(rule.Allowed.Headers, true)
		rule.ExposeHeaders = set(rule.ExposeHeaders, true)
		encoded, _ := json.Marshal(rule)
		rules = append(rules, string(encoded))
	}
	slices.Sort(rules)
	return slices.Compact(rules)
}

// reconcileBucketConfiguration touches only explicitly requested settings. Other lifecycle rules
// are kept verbatim because only orama-expire belongs to the operator.
func (r *CloudflareR2BucketReconciler) reconcileBucketConfiguration(ctx context.Context, b *oramav1alpha1.CloudflareR2Bucket, token, accountID string) error {
	base := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s", r.apiBase(), accountID, b.Spec.Name)
	if b.Spec.CORS != nil {
		desired := cfR2CORS{Rules: make([]cfR2CORSRule, 0, len(b.Spec.CORS))}
		for _, rule := range b.Spec.CORS {
			converted := cfR2CORSRule{MaxAgeSeconds: rule.MaxAgeSeconds}
			converted.Allowed.Origins, converted.Allowed.Methods, converted.Allowed.Headers = rule.AllowedOrigins, rule.AllowedMethods, rule.AllowedHeaders
			desired.Rules = append(desired.Rules, converted)
		}
		var current struct {
			Result cfR2CORS `json:"result"`
		}
		// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/cors/methods/get/
		if err := cfDo(ctx, token, http.MethodGet, base+"/cors", nil, &current); err != nil && !cfR2ConfigurationMissing(err) {
			return err
		}
		if !slices.Equal(canonicalCORS(current.Result), canonicalCORS(desired)) {
			// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/cors/methods/update/
			if err := cfDo(ctx, token, http.MethodPut, base+"/cors", desired, nil); err != nil {
				return err
			}
		}
	}
	if b.Spec.ExpireAfterDays != nil {
		desired := cfR2LifecycleRule{ID: "orama-expire", Enabled: true, DeleteObjectsTransition: &cfR2LifecycleTransition{}}
		desired.DeleteObjectsTransition.Condition.Type = "Age"
		desired.DeleteObjectsTransition.Condition.MaxAge = int64(*b.Spec.ExpireAfterDays) * 86400
		var current struct {
			Result struct {
				Rules []json.RawMessage `json:"rules"`
			} `json:"result"`
		}
		// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/lifecycle/methods/get/
		if err := cfDo(ctx, token, http.MethodGet, base+"/lifecycle", nil, &current); err != nil && !cfR2ConfigurationMissing(err) {
			return err
		}
		index := -1
		for i, raw := range current.Result.Rules {
			var rule cfR2LifecycleRule
			if err := json.Unmarshal(raw, &rule); err != nil {
				return err
			}
			if rule.ID != desired.ID {
				continue
			}
			if len(rule.StorageClassTransitions) == 0 {
				rule.StorageClassTransitions = nil
			}
			if reflect.DeepEqual(rule, desired) {
				return nil
			}
			index = i
			break
		}
		raw, err := json.Marshal(desired)
		if err != nil {
			return err
		}
		if index < 0 {
			current.Result.Rules = append(current.Result.Rules, raw)
		} else {
			current.Result.Rules[index] = raw
		}
		// https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/lifecycle/methods/update/
		return cfDo(ctx, token, http.MethodPut, base+"/lifecycle", current.Result, nil)
	}
	return nil
}

// cfR2ConfigurationMissing recognizes buckets with no CORS or lifecycle configuration yet.
func cfR2ConfigurationMissing(err error) bool {
	var apiErr *cfAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
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

// cfR2CustomDomain is one custom domain as the R2 API reports it.
type cfR2CustomDomain struct {
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
	MinTLS  string `json:"minTLS"`
	ZoneID  string `json:"zoneId"`
	Status  struct {
		Ownership string `json:"ownership"`
		SSL       string `json:"ssl"`
	} `json:"status"`
}

// reconcileCustomDomain attaches spec.customDomain to the bucket, keeps it enabled at the asked
// minimum TLS version, and detaches a domain the operator attached once the spec drops or changes
// it. https://developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/custom/
func (r *CloudflareR2BucketReconciler) reconcileCustomDomain(ctx context.Context, b *oramav1alpha1.CloudflareR2Bucket, status *oramav1alpha1.CloudflareR2BucketStatus, token, accountID string) error {
	want := b.Spec.CustomDomain
	if status.CustomDomain != "" && (want == nil || want.Domain != status.CustomDomain) {
		if err := r.detachCustomDomain(ctx, token, accountID, b.Spec.Name, status.CustomDomain); err != nil {
			return err
		}
		status.CustomDomain, status.CustomDomainStatus = "", ""
	}
	if want == nil {
		return nil
	}
	minTLS := want.MinTLS
	if minTLS == "" {
		minTLS = "1.2"
	}
	base := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s/domains/custom", r.apiBase(), accountID, b.Spec.Name)
	var current struct {
		Result cfR2CustomDomain `json:"result"`
	}
	err := cfDo(ctx, token, http.MethodGet, base+"/"+want.Domain, nil, &current)
	var apiErr *cfAPIError
	switch {
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		body := map[string]any{"domain": want.Domain, "zoneId": want.ZoneID, "enabled": true, "minTLS": minTLS}
		if err := cfDo(ctx, token, http.MethodPost, base, body, nil); err != nil {
			return err
		}
		current.Result = cfR2CustomDomain{Domain: want.Domain, Enabled: true, MinTLS: minTLS}
		current.Result.Status.Ownership, current.Result.Status.SSL = "pending", "pending"
	case err != nil:
		return err
	case !current.Result.Enabled || current.Result.MinTLS != minTLS:
		if err := cfDo(ctx, token, http.MethodPut, base+"/"+want.Domain, map[string]any{"enabled": true, "minTLS": minTLS}, nil); err != nil {
			return err
		}
	}
	status.CustomDomain = want.Domain
	status.CustomDomainStatus = fmt.Sprintf("ownership=%s ssl=%s", orUnknown(current.Result.Status.Ownership), orUnknown(current.Result.Status.SSL))
	return nil
}

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// detachCustomDomain removes a custom domain from the bucket; one already gone is fine.
func (r *CloudflareR2BucketReconciler) detachCustomDomain(ctx context.Context, token, accountID, bucket, domain string) error {
	err := cfDo(ctx, token, http.MethodDelete, fmt.Sprintf("%s/accounts/%s/r2/buckets/%s/domains/custom/%s", r.apiBase(), accountID, bucket, domain), nil, nil)
	var apiErr *cfAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
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
