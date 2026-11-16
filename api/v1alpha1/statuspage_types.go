// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// StatusPageSpec is a set of external HTTP checks, grouped into the components a customer sees,
// the alerts sent when a component changes state, and the public page they feed.
type StatusPageSpec struct {
	// Title is the page's heading, for example "example.com status".
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`

	// IntervalSeconds is how often every check runs.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:default=60
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds bounds each check's request, connection and body included.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=25
	// +kubebuilder:default=10
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is how many consecutive failed runs mark a check failing. One passing run
	// clears it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// Components are what the page shows, in order. A component is Operational when every check
	// that could run passes, Outage when every one is failing, and Degraded in between.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +listType=map
	// +listMapKey=name
	Components []StatusPageComponent `json:"components"`

	// Publish writes the public page (index.html and status.json) to an S3-compatible bucket.
	// Unset runs the checks and alerts without a page.
	// +optional
	Publish *StatusPagePublish `json:"publish,omitempty"`

	// Alerts says where a component's change of state is sent.
	// +optional
	Alerts StatusPageAlerts `json:"alerts,omitempty"`
}

// StatusPageComponent is one line on the page, backed by one or more checks.
type StatusPageComponent struct {
	// Name is the component's key in status and in status.json.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Title is what the page shows, for example "API".
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`
	// Group puts components under one heading, for example "Europe".
	// +optional
	Group string `json:"group,omitempty"`
	// Checks back the component.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=name
	Checks []StatusPageCheck `json:"checks"`
}

// StatusPageCheck is one HTTP request and what its answer must be.
// +kubebuilder:validation:XValidation:rule="!has(self.bearerTokenKey) || has(self.secretRef)",message="bearerTokenKey needs secretRef"
type StatusPageCheck struct {
	// Name identifies the check within its component.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// URL is requested as written, after $(KEY) references are replaced from secretRef.
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`
	// Method is GET or POST.
	// +kubebuilder:validation:Enum=GET;POST
	// +kubebuilder:default=GET
	// +optional
	Method string `json:"method,omitempty"`
	// Headers are sent with the request. Values may hold $(KEY) references.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
	// Body is sent with a POST. It may hold $(KEY) references.
	// +optional
	Body string `json:"body,omitempty"`
	// SecretRef names a Secret in this namespace whose keys fill $(KEY) references. A check whose
	// Secret or key is missing does not run and is reported as not checked, never as failing.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
	// BearerTokenKey is the key in secretRef sent as "Authorization: Bearer <value>".
	// +optional
	BearerTokenKey string `json:"bearerTokenKey,omitempty"`
	// Expect is what a passing answer looks like.
	// +optional
	Expect StatusPageExpect `json:"expect,omitempty"`
}

// StatusPageExpect describes a passing answer. Every field set must hold.
type StatusPageExpect struct {
	// Status is the HTTP status code.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	// +kubebuilder:default=200
	// +optional
	Status int32 `json:"status,omitempty"`
	// BodyContains are substrings the body must contain.
	// +optional
	BodyContains []string `json:"bodyContains,omitempty"`
	// JSON are values the JSON body must hold.
	// +optional
	JSON []StatusPageJSONExpect `json:"json,omitempty"`
	// MinCertificateDays fails the check when the server's TLS certificate expires sooner.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinCertificateDays int32 `json:"minCertificateDays,omitempty"`
}

// StatusPageJSONExpect is one value in a JSON body.
type StatusPageJSONExpect struct {
	// Path is a dotted path into the body; a number indexes an array, for example "value.region.name".
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
	// Equals is the value as text: a string as written, anything else as its JSON (true, 3, null).
	// Unset only requires the path to exist with a value other than null.
	// +optional
	Equals *string `json:"equals,omitempty"`
}

// StatusPagePublish is the bucket the page is written to. Serve the bucket on a public host (a
// CloudflareR2Bucket's spec.customDomain does) and set PublicURL to it.
type StatusPagePublish struct {
	// BucketSecretRef names a Secret in this namespace with S3 credentials for the bucket (keys
	// accessKeyId, secretAccessKey, endpoint, bucket, region), such as the one a
	// CloudflareR2Bucket's spec.credentials writes.
	// +kubebuilder:validation:MinLength=1
	BucketSecretRef string `json:"bucketSecretRef"`
	// PublicURL is where the page is read, for example https://status.example.com. Shown in alerts.
	// +optional
	PublicURL string `json:"publicURL,omitempty"`
}

// StatusPageAlerts are the channels a change of state is sent to. Each is optional.
type StatusPageAlerts struct {
	// Webhook posts JSON to the URL in the Secret's key "url". The body carries "text" and
	// "content", so Slack, Discord, Google Chat and Mattermost incoming webhooks read it as is.
	// +optional
	Webhook *StatusPageWebhookAlert `json:"webhook,omitempty"`
	// Email sends a message over SMTP with TLS.
	// +optional
	Email *StatusPageEmailAlert `json:"email,omitempty"`
}

// StatusPageWebhookAlert names the Secret holding the webhook URL.
type StatusPageWebhookAlert struct {
	// SecretRef names a Secret in this namespace with key "url".
	// +kubebuilder:validation:MinLength=1
	SecretRef string `json:"secretRef"`
}

// StatusPageEmailAlert names the Secret holding the SMTP account and the recipients.
type StatusPageEmailAlert struct {
	// SecretRef names a Secret in this namespace with keys host, port (465 for TLS, anything else
	// uses STARTTLS, which is required), username, password, from, and to (comma-separated).
	// +kubebuilder:validation:MinLength=1
	SecretRef string `json:"secretRef"`
}

// StatusPageComponentStatus is one component's observed state.
type StatusPageComponentStatus struct {
	Name string `json:"name"`
	// State is Operational, Degraded, Outage or Unknown (no check could run).
	State string `json:"state"`
	// Since is when the component entered State.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// FailingChecks are the checks at or over the failure threshold.
	// +optional
	FailingChecks []string `json:"failingChecks,omitempty"`
	// UncheckedChecks are the checks that could not run (a missing Secret or key).
	// +optional
	UncheckedChecks []string `json:"uncheckedChecks,omitempty"`
	// Message explains a failing or unchecked check. It is never published on the page.
	// +optional
	Message string `json:"message,omitempty"`
	// History holds the worst state of each of the last 90 days (UTC), oldest first.
	// +optional
	History []StatusPageDay `json:"history,omitempty"`
}

// StatusPageDay is the worst state a component was in on one UTC day.
type StatusPageDay struct {
	// Date is YYYY-MM-DD.
	Date  string `json:"date"`
	State string `json:"state"`
}

// StatusPageStatus is the observed state, written only when a component changes state, a day
// ends, or a condition changes; a steady page writes nothing.
type StatusPageStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the worst component state: Operational, Degraded, Outage or Unknown.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Operational counts components in state Operational, as n/total.
	// +optional
	Operational string `json:"operational,omitempty"`
	// +optional
	Components []StatusPageComponentStatus `json:"components,omitempty"`
	// Conditions: ChecksConfigured, Published, AlertsDelivered.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=spage
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Operational",type=string,JSONPath=`.status.operational`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StatusPage probes public endpoints from outside the services they check, alerts on every change
// of a component's state, and publishes a static status page to a bucket.
type StatusPage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StatusPageSpec   `json:"spec,omitempty"`
	Status StatusPageStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// StatusPageList contains a list of StatusPage.
type StatusPageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StatusPage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StatusPage{}, &StatusPageList{})
}
