/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

const (
	sealedSecretLabel          = "lake.orama.cloud/sealed"
	sealedSecretContentMAC     = "lake.orama.cloud/content-mac"
	serviceAccountTokenPath    = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	lakeMintSealedSecretRoute  = "/v1/mint_sealed_secret"
	dynamicRenewalDefaultRatio = 3
)

// LakeSealedSecretReconciler decrypts cluster-bound envelopes into owned Secrets.
type LakeSealedSecretReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	HTTPClient          *http.Client
	LakeURL             string
	ServiceAccountToken string
	TokenPath           string
	Now                 func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=lakesealedsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakesealedsecrets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=lakesealedsecrets/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile delivers the declared secret without persisting plaintext outside the Secret.
func (r *LakeSealedSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	resource := &oramav1alpha1.LakeSealedSecret{}
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if reason, message := validateLakeSealedSecret(resource); reason != "" {
		return r.fail(ctx, resource, reason, message)
	}
	lake := &oramav1alpha1.Lake{}
	if err := r.Get(ctx, client.ObjectKey{Name: resource.Spec.LakeRef.Name}, lake); err != nil {
		return ctrl.Result{}, fmt.Errorf("get Lake %q: %w", resource.Spec.LakeRef.Name, err)
	}
	owned, ownerResult, err := r.ensureLakeOwner(ctx, resource, lake)
	if err != nil || !owned {
		return ownerResult, err
	}
	if lake.Status.Sealing == nil || lake.Status.Sealing.KID == "" {
		return ctrl.Result{}, errors.New("Lake sealing status is not published")
	}
	keySecret, err := getLakeSealingKeySecret(ctx, r.Client, lake)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Lake sealing key: %w", err)
	}

	now := r.now()
	produced := &corev1.Secret{}
	producedKey := client.ObjectKey{Namespace: resource.Namespace, Name: resource.Spec.Template.Name}
	producedErr := r.Get(ctx, producedKey, produced)
	if producedErr != nil && !apierrors.IsNotFound(producedErr) {
		return ctrl.Result{}, producedErr
	}
	producedExists := producedErr == nil
	if !producedExists {
		produced.Name = producedKey.Name
		produced.Namespace = producedKey.Namespace
	}
	if producedExists && !metav1.IsControlledBy(produced, resource) {
		return r.fail(ctx, resource, "SecretOwnershipConflict", fmt.Sprintf("Secret %s is not controlled by this resource", producedKey))
	}
	if err := r.deleteStaleProducedSecrets(ctx, resource); err != nil {
		return ctrl.Result{}, err
	}

	records, lease, deliveryResult, err := r.recordsForDelivery(
		ctx,
		resource,
		lake,
		keySecret,
		produced,
		producedExists,
		now,
	)
	if err != nil || deliveryResult != nil {
		if deliveryResult != nil {
			return *deliveryResult, err
		}
		return ctrl.Result{}, err
	}
	if len(records) == 0 {
		return r.fail(ctx, resource, "NoData", "no sealed data was provided")
	}

	plaintext, kid, privateKey, err := unsealRecordSet(records, keySecret, sealedSecretAAD{
		Cluster:   lake.Status.Sealing.Cluster,
		Namespace: resource.Namespace,
		Name:      resource.Spec.Template.Name,
	})
	if err != nil {
		return r.fail(ctx, resource, "UnsealFailed", err.Error())
	}
	changed, err := r.reconcileProducedSecret(ctx, resource, produced, plaintext, privateKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ready(ctx, resource, kid, lease, changed, now); err != nil {
		return ctrl.Result{}, err
	}
	if resource.Spec.Source.Type == "dynamic" && lease != nil {
		return ctrl.Result{RequeueAfter: lease.RenewAt.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *LakeSealedSecretReconciler) ensureLakeOwner(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	lake *oramav1alpha1.Lake,
) (bool, ctrl.Result, error) {
	if metav1.IsControlledBy(resource, lake) {
		return true, ctrl.Result{}, nil
	}
	if owner := metav1.GetControllerOf(resource); owner != nil {
		result, err := r.fail(ctx, resource, "OwnershipConflict", "resource already has a controller owner")
		return false, result, err
	}
	if err := controllerutil.SetControllerReference(lake, resource, r.Scheme); err != nil {
		return false, ctrl.Result{}, err
	}
	if err := r.Update(ctx, resource); err != nil {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{Requeue: true}, nil
}

func (r *LakeSealedSecretReconciler) recordsForDelivery(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	lake *oramav1alpha1.Lake,
	keySecret *corev1.Secret,
	produced *corev1.Secret,
	producedExists bool,
	now time.Time,
) (map[string]string, *oramav1alpha1.LakeSealedSecretLeaseStatus, *ctrl.Result, error) {
	if resource.Spec.Source.Type == "literal" {
		return resource.Spec.Data, nil, nil, nil
	}
	lease := resource.Status.Lease
	validSecret := producedExists && r.remoteSecretValid(produced, keySecret, resource)
	needsMint := !validSecret || resource.Status.ObservedGeneration != resource.Generation ||
		resource.Status.KID != lake.Status.Sealing.KID
	if resource.Spec.Source.Type == "dynamic" && lease != nil && !now.Before(lease.RenewAt.Time) {
		needsMint = true
	}
	if !needsMint {
		result := ctrl.Result{}
		if resource.Spec.Source.Type == "dynamic" && lease != nil {
			result.RequeueAfter = lease.RenewAt.Sub(now)
		}
		return nil, nil, &result, nil
	}
	minted, err := r.mintSealedSecret(ctx, lake, resource)
	if err != nil {
		if _, statusErr := r.fail(ctx, resource, "MintFailed", err.Error()); statusErr != nil {
			return nil, nil, nil, statusErr
		}
		return nil, nil, nil, err
	}
	return minted.Data, minted.Lease, nil, nil
}

func (r *LakeSealedSecretReconciler) reconcileProducedSecret(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	produced *corev1.Secret,
	plaintext map[string][]byte,
	privateKey []byte,
) (bool, error) {
	secretType := resource.Spec.Template.Type
	if secretType == "" {
		secretType = corev1.SecretTypeOpaque
	}
	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, produced, func() error {
		produced.Name = resource.Spec.Template.Name
		produced.Namespace = resource.Namespace
		if produced.Labels == nil {
			produced.Labels = map[string]string{}
		}
		if produced.Annotations == nil {
			produced.Annotations = map[string]string{}
		}
		produced.Labels[sealedSecretLabel] = "true"
		produced.Annotations[sealedSecretContentMAC] = secretContentMAC(privateKey, plaintext)
		produced.Type = secretType
		produced.Data = plaintext
		return controllerutil.SetControllerReference(resource, produced, r.Scheme)
	})
	if err != nil {
		return false, fmt.Errorf("reconcile produced Secret: %w", err)
	}
	return operation != controllerutil.OperationResultNone, nil
}

func validateLakeSealedSecret(resource *oramav1alpha1.LakeSealedSecret) (string, string) {
	if resource.Spec.LakeRef.Name == "" {
		return "LakeRefRequired", "spec.lakeRef.name is required"
	}
	if resource.Spec.Template.Name == "" {
		return "SecretNameRequired", "spec.template.name is required"
	}
	switch resource.Spec.Source.Type {
	case "literal":
		if len(resource.Spec.Data) == 0 {
			return "NoData", "literal sources require spec.data"
		}
		if resource.Spec.Source.Lake != nil || resource.Spec.Source.Dynamic != nil {
			return "SourceInvalid", "literal source cannot include lake or dynamic configuration"
		}
	case "lake":
		if resource.Spec.Source.Lake == nil {
			return "LakeSourceRequired", "lake sources require spec.source.lake"
		}
		if resource.Spec.Source.Dynamic != nil || len(resource.Spec.Data) > 0 {
			return "SourceInvalid", "lake source cannot include data or dynamic configuration"
		}
	case "dynamic":
		if resource.Spec.Source.Dynamic == nil {
			return "DynamicSourceRequired", "dynamic sources require spec.source.dynamic"
		}
		if resource.Spec.Source.Dynamic.TTL.Duration <= 0 {
			return "DynamicTTLRequired", "dynamic sources require a positive ttl"
		}
		if resource.Spec.Source.Lake != nil || len(resource.Spec.Data) > 0 {
			return "SourceInvalid", "dynamic source cannot include data or lake configuration"
		}
	default:
		return "SourceTypeInvalid", fmt.Sprintf("unsupported source type %q", resource.Spec.Source.Type)
	}
	return "", ""
}

func unsealRecordSet(
	records map[string]string,
	keySecret *corev1.Secret,
	expectedAAD sealedSecretAAD,
) (map[string][]byte, string, []byte, error) {
	plaintext := make(map[string][]byte, len(records))
	setKID := ""
	var setPrivateKey []byte
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		envelope, err := decodeSealedSecretRecord(records[key])
		if err != nil {
			return nil, "", nil, fmt.Errorf("data[%q]: %w", key, err)
		}
		if setKID != "" && envelope.KID != setKID {
			return nil, "", nil, fmt.Errorf("sealed data uses multiple kids %q and %q", setKID, envelope.KID)
		}
		privateKey, publicKey, err := loadLakeSealingKey(keySecret, envelope.KID)
		if err != nil {
			return nil, "", nil, fmt.Errorf("data[%q]: %w", key, err)
		}
		if sealingKID(publicKey) != envelope.KID {
			return nil, "", nil, fmt.Errorf("data[%q]: public key does not match kid %q", key, envelope.KID)
		}
		value, kid, err := unsealSecretRecord(records[key], privateKey, publicKey, expectedAAD)
		if err != nil {
			return nil, "", nil, fmt.Errorf("data[%q]: %w", key, err)
		}
		setKID = kid
		setPrivateKey = privateKey
		plaintext[key] = value
	}
	return plaintext, setKID, setPrivateKey, nil
}

func secretContentMAC(privateKey []byte, data map[string][]byte) string {
	mac := hmac.New(sha256.New, privateKey)
	_, _ = io.WriteString(mac, "orama lake sealed secret content mac v1")
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = io.WriteString(mac, lengthPrefix(len(key)))
		_, _ = io.WriteString(mac, key)
		_, _ = io.WriteString(mac, lengthPrefix(len(data[key])))
		_, _ = mac.Write(data[key])
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func (r *LakeSealedSecretReconciler) remoteSecretValid(
	secret *corev1.Secret,
	keySecret *corev1.Secret,
	resource *oramav1alpha1.LakeSealedSecret,
) bool {
	secretType := resource.Spec.Template.Type
	if secretType == "" {
		secretType = corev1.SecretTypeOpaque
	}
	if secret.Type != secretType || secret.Labels[sealedSecretLabel] != "true" {
		return false
	}
	return r.secretContentMACValid(secret, keySecret, resource.Status.KID)
}

func lengthPrefix(length int) string {
	return fmt.Sprintf("%d:", length)
}

func (r *LakeSealedSecretReconciler) secretContentMACValid(
	secret *corev1.Secret,
	keySecret *corev1.Secret,
	kid string,
) bool {
	privateKey, _, err := loadLakeSealingKey(keySecret, kid)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(secret.Annotations[sealedSecretContentMAC])
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(secretContentMAC(privateKey, secret.Data))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

type lakeMintRequest struct {
	Attestation lakeMintAttestation                  `json:"attestation"`
	Recipient   lakeMintRecipient                    `json:"recipient"`
	Source      oramav1alpha1.LakeSealedSecretSource `json:"source"`
}

type lakeMintAttestation struct {
	Type      string   `json:"type"`
	Token     string   `json:"token"`
	Audiences []string `json:"audiences"`
}

type lakeMintRecipient struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	KID       string `json:"kid"`
	PublicKey string `json:"public_key"`
}

type lakeMintResult struct {
	Data  map[string]string                          `json:"data"`
	Lease *oramav1alpha1.LakeSealedSecretLeaseStatus `json:"lease,omitempty"`
}

type lakeMintResponse struct {
	OK    bool          `json:"ok"`
	Value lakeMintValue `json:"value"`
	Error *lakeAPIError `json:"error,omitempty"`
}

type lakeMintValue struct {
	Data  map[string]string `json:"data"`
	Lease *lakeMintLease    `json:"lease,omitempty"`
}

type lakeMintLease struct {
	ID        string     `json:"id"`
	ExpiresAt time.Time  `json:"expires_at"`
	RenewAt   *time.Time `json:"renew_at,omitempty"`
}

type lakeAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *LakeSealedSecretReconciler) mintSealedSecret(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	resource *oramav1alpha1.LakeSealedSecret,
) (lakeMintResult, error) {
	token, err := r.serviceAccountToken()
	if err != nil {
		return lakeMintResult{}, err
	}
	requestBody := lakeMintRequest{
		Attestation: lakeMintAttestation{
			Type:      "kubernetes",
			Token:     token,
			Audiences: []string{"lake"},
		},
		Recipient: lakeMintRecipient{
			Cluster:   lake.Status.Sealing.Cluster,
			Namespace: resource.Namespace,
			Name:      resource.Spec.Template.Name,
			KID:       lake.Status.Sealing.KID,
			PublicKey: lake.Status.Sealing.PublicKey,
		},
		Source: resource.Spec.Source,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return lakeMintResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.lakeBaseURL(lake)+lakeMintSealedSecretRoute, bytes.NewReader(body))
	if err != nil {
		return lakeMintResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.httpClient().Do(request)
	if err != nil {
		return lakeMintResult{}, fmt.Errorf("call lake mint_sealed_secret: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return lakeMintResult{}, fmt.Errorf("lake mint_sealed_secret returned %s", response.Status)
	}
	decoded := lakeMintResponse{}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&decoded); err != nil {
		return lakeMintResult{}, fmt.Errorf("decode lake mint_sealed_secret response: %w", err)
	}
	if !decoded.OK {
		if decoded.Error == nil {
			return lakeMintResult{}, errors.New("lake mint_sealed_secret failed without an error")
		}
		return lakeMintResult{}, fmt.Errorf("lake %s: %s", decoded.Error.Code, decoded.Error.Message)
	}
	result := lakeMintResult{Data: decoded.Value.Data}
	if len(result.Data) == 0 {
		return lakeMintResult{}, errors.New("lake mint_sealed_secret returned no data")
	}
	if resource.Spec.Source.Type == "dynamic" {
		if decoded.Value.Lease == nil {
			return lakeMintResult{}, errors.New("lake mint_sealed_secret returned no dynamic lease")
		}
		renewAt := time.Time{}
		if decoded.Value.Lease.RenewAt != nil {
			renewAt = decoded.Value.Lease.RenewAt.UTC()
		}
		if renewAt.IsZero() {
			ttl := resource.Spec.Source.Dynamic.TTL.Duration
			renewAt = decoded.Value.Lease.ExpiresAt.Add(-ttl / dynamicRenewalDefaultRatio)
		}
		if !decoded.Value.Lease.ExpiresAt.After(r.now()) {
			return lakeMintResult{}, errors.New("lake mint_sealed_secret returned an expired lease")
		}
		if !renewAt.After(r.now()) || !renewAt.Before(decoded.Value.Lease.ExpiresAt) {
			return lakeMintResult{}, errors.New("lake mint_sealed_secret returned an invalid renewal time")
		}
		result.Lease = &oramav1alpha1.LakeSealedSecretLeaseStatus{
			ID:        decoded.Value.Lease.ID,
			ExpiresAt: metav1.NewTime(decoded.Value.Lease.ExpiresAt.UTC()),
			RenewAt:   metav1.NewTime(renewAt.UTC()),
		}
	}
	return result, nil
}

func (r *LakeSealedSecretReconciler) deleteStaleProducedSecrets(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
) error {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(resource.Namespace)); err != nil {
		return fmt.Errorf("list produced Secrets: %w", err)
	}
	for index := range secrets.Items {
		secret := &secrets.Items[index]
		if !metav1.IsControlledBy(secret, resource) || secret.Name == resource.Spec.Template.Name {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale produced Secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
	}
	return nil
}

func (r *LakeSealedSecretReconciler) requestsForLake(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	lake, ok := object.(*oramav1alpha1.Lake)
	if !ok {
		return nil
	}
	var resources oramav1alpha1.LakeSealedSecretList
	if err := r.List(ctx, &resources); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range resources.Items {
		resource := &resources.Items[index]
		if resource.Spec.LakeRef.Name != lake.Name {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(resource)})
	}
	return requests
}

func (r *LakeSealedSecretReconciler) serviceAccountToken() (string, error) {
	if r.ServiceAccountToken != "" {
		return r.ServiceAccountToken, nil
	}
	path := r.TokenPath
	if path == "" {
		path = serviceAccountTokenPath
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read projected service-account token: %w", err)
	}
	token := strings.TrimSpace(string(value))
	if token == "" {
		return "", errors.New("projected service-account token is empty")
	}
	return token, nil
}

func (r *LakeSealedSecretReconciler) lakeBaseURL(lake *oramav1alpha1.Lake) string {
	if r.LakeURL != "" {
		return strings.TrimRight(r.LakeURL, "/")
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	return fmt.Sprintf("http://lake-%s.%s.svc.cluster.local", lake.Name, namespace)
}

func (r *LakeSealedSecretReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (r *LakeSealedSecretReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *LakeSealedSecretReconciler) ready(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	kid string,
	lease *oramav1alpha1.LakeSealedSecretLeaseStatus,
	secretChanged bool,
	now time.Time,
) error {
	desired := resource.Status.DeepCopy()
	desired.Phase = "Ready"
	desired.KID = kid
	desired.Lease = lease
	desired.ObservedGeneration = resource.Generation
	if secretChanged || desired.UnsealedAt == nil {
		unsealedAt := metav1.NewTime(now)
		desired.UnsealedAt = &unsealedAt
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Delivered",
		Message:            "sealed data was delivered to the owned Secret",
		ObservedGeneration: resource.Generation,
	})
	if apiequality.Semantic.DeepEqual(resource.Status, *desired) {
		return nil
	}
	resource.Status = *desired
	return r.Status().Update(ctx, resource)
}

func (r *LakeSealedSecretReconciler) fail(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	reason string,
	message string,
) (ctrl.Result, error) {
	desired := resource.Status.DeepCopy()
	desired.Phase = "Error"
	desired.ObservedGeneration = resource.Generation
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: resource.Generation,
	})
	if apiequality.Semantic.DeepEqual(resource.Status, *desired) {
		return ctrl.Result{}, nil
	}
	resource.Status = *desired
	if err := r.Status().Update(ctx, resource); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the LakeSealedSecret controller.
func (r *LakeSealedSecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.LakeSealedSecret{}).
		Owns(&corev1.Secret{}).
		Watches(&oramav1alpha1.Lake{}, handler.EnqueueRequestsFromMapFunc(r.requestsForLake)).
		Named("lakesealedsecret").
		Complete(r)
}
