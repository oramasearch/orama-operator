// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BucketDeletionPolicy decides what happens to the real bucket when its resource is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type BucketDeletionPolicy string

const (
	// BucketDeletionDelete deletes the bucket from the provider with its resource.
	BucketDeletionDelete BucketDeletionPolicy = "Delete"
	// BucketDeletionRetain keeps the bucket, and every object in it, when its resource is deleted.
	// Re-creating the resource adopts the existing bucket. Backup buckets use Retain.
	BucketDeletionRetain BucketDeletionPolicy = "Retain"
)

// CloudflareR2BucketSpec is the desired state of an R2 bucket. The API token + account come
// from the referenced Secret (keys `token` + `accountID`).
type CloudflareR2BucketSpec struct {
	// SecretRef names the Secret holding the Cloudflare credentials (`token`, `accountID`).
	SecretRef string `json:"secretRef"`
	// Name is the R2 bucket name.
	Name string `json:"name"`
	// DeletionPolicy is required and has no default: whoever writes the resource chooses whether
	// deleting it deletes the bucket and everything in it (Delete) or keeps it (Retain).
	DeletionPolicy BucketDeletionPolicy `json:"deletionPolicy"`
	// Credentials, when set, has the operator mint an API token scoped to this one bucket (object
	// read and write, nothing else) and write its S3 credentials to a Secret. The account token in
	// SecretRef never leaves this namespace. The bucket token is revoked when the resource is
	// deleted, whatever the deletion policy.
	// +optional
	Credentials *CloudflareR2BucketCredentials `json:"credentials,omitempty"`
	// CORS, when set, replaces the bucket's whole CORS configuration. Unset leaves it alone.
	// +optional
	CORS []CloudflareR2CORSRule `json:"cors,omitempty"`
	// ExpireAfterDays deletes objects this many days after they were written through the
	// operator-owned lifecycle rule "orama-expire". Unset leaves lifecycle rules alone.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ExpireAfterDays *int32 `json:"expireAfterDays,omitempty"`
	// CustomDomain, when set, serves the bucket's objects publicly on this host through
	// Cloudflare (an R2 custom domain). Everything in the bucket becomes readable by anyone, so set
	// it only on a bucket that holds public content, such as a status page. Unset removes a custom
	// domain the operator attached.
	// +optional
	CustomDomain *CloudflareR2CustomDomain `json:"customDomain,omitempty"`
}

// CloudflareR2CustomDomain is a public host for a bucket, in a zone on the same Cloudflare account.
type CloudflareR2CustomDomain struct {
	// Domain is the host, for example status.example.com. Cloudflare creates its DNS record.
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	Domain string `json:"domain"`
	// ZoneID is the Cloudflare zone the domain is in. Not a secret.
	// +kubebuilder:validation:MinLength=1
	ZoneID string `json:"zoneID"`
	// MinTLS is the lowest TLS version the host accepts.
	// +kubebuilder:validation:Enum="1.2";"1.3"
	// +kubebuilder:default="1.2"
	// +optional
	MinTLS string `json:"minTLS,omitempty"`
}

// CloudflareR2CORSRule allows browser requests from the listed origins.
type CloudflareR2CORSRule struct {
	AllowedOrigins []string `json:"allowedOrigins"`
	// +kubebuilder:validation:items:Enum=GET;PUT;POST;DELETE;HEAD
	AllowedMethods []string `json:"allowedMethods"`
	// +optional
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	// +optional
	MaxAgeSeconds int32 `json:"maxAgeSeconds,omitempty"`
}

// CloudflareR2BucketCredentials asks for S3 credentials scoped to the bucket.
type CloudflareR2BucketCredentials struct {
	// SecretName is the Secret the operator writes in this namespace, with keys `accessKeyId`,
	// `secretAccessKey`, `endpoint`, `bucket` and `region`.
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
	// AllowedNamespaces lists the other namespaces whose backups may use this bucket. The operator
	// copies the bucket-scoped Secret into a backup's namespace only when it is listed here.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// CloudflareR2BucketStatus is the observed state.
type CloudflareR2BucketStatus struct {
	// +optional
	Created bool `json:"created,omitempty"`
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// CredentialsTokenID is the id of the bucket-scoped token the operator minted; it is also the
	// S3 access key id. Not a secret.
	// +optional
	CredentialsTokenID string `json:"credentialsTokenID,omitempty"`
	// CredentialsSecret is the Secret holding the bucket-scoped S3 credentials, once written.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
	// CustomDomain is the public host the operator attached, once attached.
	// +optional
	CustomDomain string `json:"customDomain,omitempty"`
	// CustomDomainStatus is Cloudflare's ownership and certificate state for it, for example
	// "ownership=active ssl=active".
	// +optional
	CustomDomainStatus string `json:"customDomainStatus,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=cfr2
//+kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.name`
//+kubebuilder:printcolumn:name="Deletion",type=string,JSONPath=`.spec.deletionPolicy`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`

// CloudflareR2Bucket is an R2 (S3-compatible) bucket the operator provisions via the CF API,
// owned by an OramaCloudV1 CR or written for backups. On delete a finalizer revokes the bucket's
// scoped token and, with deletionPolicy Delete only, deletes the bucket from Cloudflare.
type CloudflareR2Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareR2BucketSpec   `json:"spec,omitempty"`
	Status CloudflareR2BucketStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareR2BucketList contains a list of CloudflareR2Bucket.
type CloudflareR2BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareR2Bucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareR2Bucket{}, &CloudflareR2BucketList{})
}
