/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Store providers beyond the cluster volume (lake#664; the contract is lake's
// docs/control-plane/storage.md).
//
// A cloud bucket (r2, s3, b2 and gcs over the S3 protocol, azure over Blob REST) is the customer's
// own bucket. The operator never creates, changes or deletes it: it delivers the workspace's
// credential for it, which lives in lake's secret store sealed in the workspace Zetta (decision 58),
// as a LakeSealedSecret <lake>-<slug> in the Lake namespace with a lake source. The operator never
// sees the credential through Postgres; the store's spec only names the secret.
//
// A Lake-run store (zetta: a sealed bucket in the workspace Zetta; lake: a liquid repository of
// the workspace) is kept by Lake itself. There is nothing to create in Kubernetes.
const (
	storeTypeObject     = "object"
	storeTypeFilesystem = "filesystem"
	storeProviderZetta  = "zetta"
	storeProviderLake   = "lake"
	storeProviderAzure  = "azure"

	storeBucketDeliveringMessage = "delivering the credentials from the workspace's secret store"
	storeBucketReadyMessage      = "credentials delivered"
	storeBucketDeletedMessage    = "store removed; the bucket and what is in it stay in your account"
	storeLakeRunReadyMessage     = "kept by Lake in the workspace"
)

// storeCloudProviders are the bucket providers the customer brings their own account for.
var storeCloudProviders = map[string]bool{"r2": true, "s3": true, "b2": true, "gcs": true, storeProviderAzure: true}

// storeIsBucket: an object store in the customer's own cloud account.
func storeIsBucket(st *LakeStore) bool {
	return st.Type == storeTypeObject && storeCloudProviders[st.Provider]
}

// storeIsLakeRun: an object or filesystem store Lake keeps in the workspace.
func storeIsLakeRun(st *LakeStore) bool {
	return (st.Type == storeTypeObject || st.Type == storeTypeFilesystem) &&
		(st.Provider == storeProviderZetta || st.Provider == storeProviderLake)
}

// lakeBucketSpec is a bucket store's spec: where the bucket is and which workspace secret holds
// its credential.
type lakeBucketSpec struct {
	Bucket     string `json:"bucket"`
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region"`
	Prefix     string `json:"prefix"`
	Credential struct {
		MountID string `json:"mount_id"`
		Name    string `json:"name"`
	} `json:"credential"`
}

// parseBucketSpec reads and checks a bucket's spec. lake checks the same on create_store; this
// guards a row written some other way.
func parseBucketSpec(raw string) (lakeBucketSpec, error) {
	var spec lakeBucketSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return spec, fmt.Errorf("spec is not a JSON object with bucket, endpoint and credential: %v", err)
	}
	if strings.TrimSpace(spec.Bucket) == "" {
		return spec, fmt.Errorf("spec.bucket is required")
	}
	endpoint, err := url.Parse(spec.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return spec, fmt.Errorf("spec.endpoint must be an https:// URL")
	}
	if strings.TrimSpace(spec.Credential.MountID) == "" || strings.TrimSpace(spec.Credential.Name) == "" {
		return spec, fmt.Errorf("spec.credential.mount_id and spec.credential.name are required")
	}
	return spec, nil
}

// storeBucketEndpoint is what store_status.endpoint carries for a bucket: <endpoint>/<bucket>.
func storeBucketEndpoint(spec lakeBucketSpec) string {
	return strings.TrimRight(spec.Endpoint, "/") + "/" + spec.Bucket
}

func foreignStoreSecretMessage(namespace, name string) string {
	return fmt.Sprintf("LakeSealedSecret %s/%s exists and is not this Lake's store", namespace, name)
}

// reconcileBucketStore owns the LakeSealedSecret that delivers the bucket's credential and reports
// the store resource-ready once the Secret it produces exists. It never touches the bucket.
func (r *LakeReconciler) reconcileBucketStore(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, st *LakeStore, spec lakeBucketSpec) (serviceResult, error) {
	name := storeResourceName(lake.Name, st.Slug)
	endpoint := storeBucketEndpoint(spec)
	source := oramav1alpha1.LakeSealedSecretSource{Type: "lake",
		Lake: &oramav1alpha1.LakeStaticSecretSource{Mount: spec.Credential.MountID, Name: spec.Credential.Name}}
	template := oramav1alpha1.LakeSealedSecretTemplate{Name: name, Type: corev1.SecretTypeOpaque}

	sealed := &oramav1alpha1.LakeSealedSecret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sealed); err == nil {
		if !storeOwned(lake, sealed, st) {
			return serviceResult{phase: ServicePhaseFailed, message: foreignStoreSecretMessage(namespace, name)}, nil
		}
		if sealed.DeletionTimestamp != nil {
			return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: "waiting for the previous credentials to be removed"}, nil
		}
		// Only a changed reference is written, so a steady store makes no API writes.
		if sealed.Spec.LakeRef.Name != lake.Name || sealed.Spec.Template != template || len(sealed.Spec.Data) != 0 ||
			sealed.Spec.Source.Type != source.Type || sealed.Spec.Source.Lake == nil || *sealed.Spec.Source.Lake != *source.Lake {
			sealed.Spec.LakeRef = corev1.LocalObjectReference{Name: lake.Name}
			sealed.Spec.Template = template
			sealed.Spec.Data = nil
			sealed.Spec.Source = source
			if err := r.Update(ctx, sealed); err != nil {
				return serviceResult{}, fmt.Errorf("update store credentials %s/%s: %w", namespace, name, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	} else {
		sealed = &oramav1alpha1.LakeSealedSecret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: storeClaimLabels(lake, labels, st),
				Annotations: map[string]string{serviceCustomerAnnotation: st.CustomerID}},
			Spec: oramav1alpha1.LakeSealedSecretSpec{
				LakeRef: corev1.LocalObjectReference{Name: lake.Name}, Template: template, Source: source},
		}
		if err := controllerutil.SetControllerReference(lake, sealed, r.Scheme); err != nil {
			return serviceResult{}, err
		}
		if err := r.Create(ctx, sealed); err != nil {
			return serviceResult{}, fmt.Errorf("create store credentials %s/%s: %w", namespace, name, err)
		}
	}

	produced := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, produced); err != nil {
		if !apierrors.IsNotFound(err) {
			return serviceResult{}, err
		}
		return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: bucketDeliveringMessage(sealed)}, nil
	}
	// Only the Secret this delivery produced counts: a foreign Secret of the same name is refused by
	// the LakeSealedSecret controller and never makes the store Ready.
	if !metav1.IsControlledBy(produced, sealed) {
		return serviceResult{phase: ServicePhaseProvisioning, endpoint: endpoint, message: bucketDeliveringMessage(sealed)}, nil
	}
	credential, err := json.Marshal(struct {
		Type      string `json:"type"`
		Provider  string `json:"provider"`
		Bucket    string `json:"bucket"`
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Prefix    string `json:"prefix"`
		Secret    string `json:"secret"`
		Namespace string `json:"namespace"`
	}{st.Type, st.Provider, spec.Bucket, spec.Endpoint, spec.Region, spec.Prefix, name, namespace})
	if err != nil {
		return serviceResult{}, err
	}
	return serviceResult{phase: ServicePhaseProvisioning, message: storeBucketReadyMessage, endpoint: endpoint, resourceReady: true, credential: credential}, nil
}

// bucketDeliveringMessage is the Provisioning message, with the delivery's own reason when it has
// failed (a secret that does not exist in the mount, for one), so the person can act on it.
func bucketDeliveringMessage(sealed *oramav1alpha1.LakeSealedSecret) string {
	if sealed.Status.Phase == "Error" {
		if ready := apimeta.FindStatusCondition(sealed.Status.Conditions, "Ready"); ready != nil && ready.Message != "" {
			return storeBucketDeliveringMessage + ": " + ready.Message
		}
	}
	return storeBucketDeliveringMessage
}

// deleteBucketStore deletes an absent bucket store's delivery (its Secret goes with it, by owner
// reference) and its connection. The bucket and what is in it are the customer's and stay.
func (r *LakeReconciler) deleteBucketStore(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, st *LakeStore) (serviceResult, error) {
	name := storeResourceName(lake.Name, st.Slug)
	key := client.ObjectKey{Namespace: namespace, Name: name}
	deleted := serviceResult{phase: ServicePhaseDeleted, message: storeBucketDeletedMessage}
	sealed := &oramav1alpha1.LakeSealedSecret{}
	if err := r.Get(ctx, key, sealed); err == nil {
		if !storeOwned(lake, sealed, st) {
			deleted.message = storeBucketDeletedMessage + "; " + foreignStoreSecretMessage(namespace, name) + " and was left in place"
		} else if sealed.DeletionTimestamp == nil {
			if err := r.Delete(ctx, sealed); err != nil && !apierrors.IsNotFound(err) {
				return serviceResult{}, fmt.Errorf("delete store credentials %s: %w", key, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	}
	if err := r.removeServiceCredential(ctx, lake, namespace, st.Slug); err != nil {
		return serviceResult{}, err
	}
	remaining := &oramav1alpha1.LakeSealedSecret{}
	if err := r.Get(ctx, key, remaining); err == nil {
		if storeOwned(lake, remaining, st) {
			return serviceResult{phase: ServicePhaseDeleting, message: "removing the credentials"}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return serviceResult{}, err
	}
	return deleted, nil
}

// lakeRunStoreResult: a Lake-run store is resource-ready at once, with a connection that says only
// what it is; Lake reaches it in the workspace itself.
func lakeRunStoreResult(st *LakeStore) (serviceResult, error) {
	if raw := strings.TrimSpace(st.Spec); raw != "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return serviceResult{phase: ServicePhaseFailed, message: fmt.Sprintf("spec is not a JSON object: %v", err)}, nil
		}
	}
	credential, err := json.Marshal(struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}{st.Type, st.Provider})
	if err != nil {
		return serviceResult{}, err
	}
	return serviceResult{phase: ServicePhaseProvisioning, message: storeLakeRunReadyMessage, resourceReady: true, credential: credential}, nil
}
