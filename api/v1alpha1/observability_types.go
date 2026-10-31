/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Observability (operator#15, #456). Three abstract kinds live on the managing cluster:
//
//   - ObservabilityProvider: the vendor's release, written once (for type pixie, the pinned
//     oramasearch/pixie commit and every image by digest, #455).
//   - ObservabilityHub: where a purpose's observability lives (its UI, sign-in, cluster registry and
//     alert routing), on one cluster.
//   - ObservabilityAgent: the node agents on one cluster, reporting to one hub.
//
// The provider's type decides the concrete kinds the managing operator writes onto the clusters:
// PixieCloud on the hub's cluster and PixieVizier on each agent's. Each cluster's own operator
// reconciles them. Which hub an agent may report to is decided from the clusters' purpose tags,
// never from a field: production agents report only to the hub on the managing cluster, nonprod
// agents only to a hub on a nonprod cluster.

// ObservabilityProviderType is the vendor a provider stands for.
// +kubebuilder:validation:Enum=pixie
type ObservabilityProviderType string

const (
	// ObservabilityProviderPixie installs self-hosted Pixie Cloud and Vizier, built from oramasearch/pixie.
	ObservabilityProviderPixie ObservabilityProviderType = "pixie"
)

// Phases of the observability kinds.
const (
	ObservabilityPhasePending     = "Pending"
	ObservabilityPhaseProgressing = "Progressing"
	ObservabilityPhaseReady       = "Ready"
	ObservabilityPhaseDegraded    = "Degraded"
	ObservabilityPhaseRefused     = "Refused"
)

// PixieRelease is one build of oramasearch/pixie: the commit the operator's vendored manifests come
// from, and every image built from it, by digest.
type PixieRelease struct {
	// Commit is the oramasearch/pixie commit the images were built from. It must equal the commit
	// the operator vendors its manifests from (internal/controller/assets/pixie/COMMIT): manifests
	// and images from different commits are refused.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`

	// VizierVersion is the version the Vizier images were tagged with, reported to Pixie Cloud and
	// pinned in its artifact tracker so nothing offers another version.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	VizierVersion string `json:"vizierVersion"`

	// CLIVersion is the px CLI version the hub's artifact tracker reports.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	CLIVersion string `json:"cliVersion"`

	// Images maps each image the operator renders (see docs/infrastructure/observability.md for
	// the exact set) to its reference by digest, repository@sha256:<64 hex>. A reference by tag is
	// refused: what runs is what was built and recorded (#455).
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	Images map[string]ImageDigestReference `json:"images"`
}

// ImageDigestReference is an image by digest: repository@sha256:<64 lowercase hex>.
// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$`
// +kubebuilder:validation:MaxLength=512
type ImageDigestReference string

// ObservabilityProviderSpec is a vendor's release.
// +kubebuilder:validation:XValidation:rule="self.type != 'pixie' || has(self.pixie)",message="spec.pixie is required when type is pixie"
type ObservabilityProviderSpec struct {
	// Type is the vendor. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type ObservabilityProviderType `json:"type"`

	// Pixie is the release for type pixie.
	// +optional
	Pixie *PixieRelease `json:"pixie,omitempty"`
}

// ObservabilityProviderStatus says whether the release can be installed.
type ObservabilityProviderStatus struct {
	// Ready is true when the release names every image the operator renders, and its commit is the
	// one the operator vendors.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// Message says why it is not Ready.
	// +optional
	Message string `json:"message,omitempty"`
	// Hubs and Agents count the resources that use this provider.
	// +optional
	Hubs int32 `json:"hubs,omitempty"`
	// +optional
	Agents int32 `json:"agents,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=obsprovider
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.spec.pixie.commit`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Hubs",type=integer,JSONPath=`.status.hubs`
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agents`

// ObservabilityProvider is an observability vendor's release, written once, which hubs and agents
// point at. Reconciled on the managing cluster.
type ObservabilityProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObservabilityProviderSpec   `json:"spec,omitempty"`
	Status ObservabilityProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ObservabilityProviderList is a list of ObservabilityProvider.
type ObservabilityProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObservabilityProvider `json:"items"`
}

// ObservabilitySecretKeyRef names one key of a Secret on the managing cluster.
type ObservabilitySecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// AlertReceiver is where an alert is delivered. Exactly one of slack and webhook is set.
// +kubebuilder:validation:XValidation:rule="has(self.slack) != has(self.webhook)",message="a receiver is exactly one of slack and webhook"
type AlertReceiver struct {
	// Name is the receiver's name in Alertmanager.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// Severities are the alert severities delivered here. Empty means critical and warning.
	// +optional
	// +kubebuilder:validation:items:Enum=critical;warning;info
	Severities []string `json:"severities,omitempty"`

	// Slack posts to an incoming webhook.
	// +optional
	Slack *SlackAlertReceiver `json:"slack,omitempty"`

	// Webhook posts Alertmanager's JSON to a URL, for a pager or an on-prem system.
	// +optional
	Webhook *WebhookAlertReceiver `json:"webhook,omitempty"`
}

// SlackAlertReceiver is a Slack incoming webhook.
type SlackAlertReceiver struct {
	// URLSecretRef holds the incoming webhook URL. It is copied to each cluster that routes alerts
	// here under the production-credential rules (docs/crd-reference.md).
	URLSecretRef ObservabilitySecretKeyRef `json:"urlSecretRef"`
	// Channel overrides the webhook's default channel.
	// +optional
	// +kubebuilder:validation:MaxLength=80
	Channel string `json:"channel,omitempty"`
}

// WebhookAlertReceiver posts to a URL held in a Secret.
type WebhookAlertReceiver struct {
	URLSecretRef ObservabilitySecretKeyRef `json:"urlSecretRef"`
}

// ObservabilityAlerting is where every cluster of the hub's purpose sends its alerts.
type ObservabilityAlerting struct {
	// Receivers get every alert of their severities, from the hub's cluster and every agent's.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Receivers []AlertReceiver `json:"receivers"`
}

// ObservabilityHubStorage sizes the hub's durable stores.
type ObservabilityHubStorage struct {
	// StorageClassName for every volume the hub claims. Empty means the cluster's default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// Postgres is the hub's control database (users, organisations, clusters, deploy keys).
	// +kubebuilder:default="20Gi"
	// +optional
	Postgres *resource.Quantity `json:"postgres,omitempty"`
	// Search is the metadata search index (OpenSearch).
	// +kubebuilder:default="20Gi"
	// +optional
	Search *resource.Quantity `json:"search,omitempty"`
	// Messaging is the NATS JetStream store.
	// +kubebuilder:default="10Gi"
	// +optional
	Messaging *resource.Quantity `json:"messaging,omitempty"`
	// Metrics is the hub Prometheus' volume, which keeps every agent cluster's federated metrics
	// for metricsRetention.
	// +kubebuilder:default="100Gi"
	// +optional
	Metrics *resource.Quantity `json:"metrics,omitempty"`
}

// ObservabilityHubSpec is where one purpose's observability lives.
// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) == has(oldSelf.clusterRef)",message="clusterRef is immutable"
type ObservabilityHubSpec struct {
	// ProviderRef names the ObservabilityProvider whose release the hub runs.
	// +kubebuilder:validation:MinLength=1
	ProviderRef string `json:"providerRef"`

	// ClusterRef is the KubernetesCluster the hub runs on. Empty means the managing cluster itself,
	// which is where production's hub runs: it serves production agents. A nonprod cluster's hub
	// serves nonprod agents. A hub on a production cluster, or on one with any other purpose, is
	// refused. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// Domain is the host name people reach the hub's UI under (the UI and its sign-in use it and
	// work.<domain>). The operator does not create its DNS record or expose it publicly.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +kubebuilder:validation:MaxLength=200
	Domain string `json:"domain"`

	// Organization is the one organisation every agent's cluster is registered under, and which
	// the hub's administrator belongs to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=50
	Organization string `json:"organization"`

	// AdminEmail is the hub administrator's sign-in. The operator generates the password and keeps
	// it in Secret pixie-admin on the hub's cluster.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	// +kubebuilder:validation:MaxLength=200
	AdminEmail string `json:"adminEmail"`

	// Users are the other people who sign in to the hub's UI, by email, as members of the
	// administrator's organisation. The operator generates each one's password and keeps it in
	// Secret pixie-user-<email> on the hub's cluster. Removing an email removes that sign-in.
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:MaxItems=50
	// +listType=set
	// +optional
	Users []string `json:"users,omitempty"`

	// ImagePullSecretRef is a registry credentials Secret on the managing cluster, copied to the
	// hub's cluster under the production-credential rules. Give each cluster its own.
	ImagePullSecretRef ClusterSecretReference `json:"imagePullSecretRef"`

	// Storage sizes the hub's durable stores.
	// +optional
	Storage ObservabilityHubStorage `json:"storage,omitempty"`

	// Alerting routes the alerts of the hub's cluster and of every agent's cluster.
	// +optional
	Alerting *ObservabilityAlerting `json:"alerting,omitempty"`

	// MetricsRetention is how long the hub keeps the metrics it federates from its clusters
	// (Prometheus duration: 30d, 12w).
	// +kubebuilder:validation:Pattern=`^[0-9]+(h|d|w|y)$`
	// +kubebuilder:default="30d"
	// +optional
	MetricsRetention string `json:"metricsRetention,omitempty"`
}

// ObservabilityComponentStatus is one deployable component's readiness.
type ObservabilityComponentStatus struct {
	Name string `json:"name"`
	// Ready is true when every replica the component wants is available.
	Ready bool `json:"ready"`
	// Message says why it is not ready.
	// +optional
	Message string `json:"message,omitempty"`
}

// ObservabilityHubAgentStatus is one agent the hub accepted.
type ObservabilityHubAgentStatus struct {
	// Name is the ObservabilityAgent's name.
	Name string `json:"name"`
	// Cluster is the agent's KubernetesCluster.
	Cluster string `json:"cluster"`
	// Connected is true while the agent's cluster is registered and heartbeating.
	Connected bool `json:"connected"`
	// MetricsConnected is true while the hub's Prometheus reads the cluster's metrics.
	MetricsConnected bool `json:"metricsConnected"`
}

// ObservabilityHubStatus is what the managing cluster last read from the hub's cluster.
type ObservabilityHubStatus struct {
	// Phase is Pending, Progressing, Ready, Degraded or Refused.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Purpose is the hub cluster's purpose tag; production for the managing cluster.
	// +optional
	Purpose string `json:"purpose,omitempty"`
	// ConcreteRef is the concrete resource on the hub's cluster, Kind namespace/name.
	// +optional
	ConcreteRef string `json:"concreteRef,omitempty"`
	// Ready is "n/total" of the hub's components.
	// +optional
	Ready string `json:"ready,omitempty"`
	// +optional
	Components []ObservabilityComponentStatus `json:"components,omitempty"`
	// +optional
	Agents []ObservabilityHubAgentStatus `json:"agents,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=obshub
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.status.purpose`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// ObservabilityHub is one purpose's observability hub on one cluster: the UI, sign-in and cluster
// registry the agents report to, and the alert routing of every cluster of that purpose. Reconciled
// on the managing cluster, which writes the provider's concrete kind onto the hub's cluster.
type ObservabilityHub struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObservabilityHubSpec   `json:"spec,omitempty"`
	Status ObservabilityHubStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ObservabilityHubList is a list of ObservabilityHub.
type ObservabilityHubList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObservabilityHub `json:"items"`
}

// ObservabilityAgentSpec is the node agents of one cluster.
type ObservabilityAgentSpec struct {
	// ClusterRef is the KubernetesCluster the agents run on. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	ClusterRef string `json:"clusterRef"`

	// HubRef names the ObservabilityHub the agents report to. It is refused unless the two
	// clusters' purposes match: production to the managing cluster's hub, nonprod to a nonprod
	// cluster's hub.
	// +kubebuilder:validation:MinLength=1
	HubRef string `json:"hubRef"`

	// ImagePullSecretRef is a registry credentials Secret on the managing cluster, copied to the
	// agent's cluster under the production-credential rules. Give each cluster its own.
	ImagePullSecretRef ClusterSecretReference `json:"imagePullSecretRef"`

	// NodeMemory is the memory each node agent may use. Its in-memory data store is sized from the
	// same number, so a larger value keeps more history on each node.
	// +kubebuilder:default="2Gi"
	// +optional
	NodeMemory *resource.Quantity `json:"nodeMemory,omitempty"`
}

// ObservabilityAgentStatus is what the managing cluster last read for one agent.
type ObservabilityAgentStatus struct {
	// Phase is Pending, Progressing, Ready, Degraded or Refused.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Purpose is the agent cluster's purpose tag.
	// +optional
	Purpose string `json:"purpose,omitempty"`
	// HubPurpose is the hub cluster's purpose tag; production for the managing cluster.
	// +optional
	HubPurpose string `json:"hubPurpose,omitempty"`
	// ConcreteRef is the concrete resource on the agent's cluster, Kind namespace/name.
	// +optional
	ConcreteRef string `json:"concreteRef,omitempty"`
	// Link is how the agents reach the hub: "local" on the hub's own cluster, otherwise the
	// PeerLink that carries it.
	// +optional
	Link string `json:"link,omitempty"`
	// Ready is "n/total" of the agent's components.
	// +optional
	Ready string `json:"ready,omitempty"`
	// +optional
	Components []ObservabilityComponentStatus `json:"components,omitempty"`
	// Connected is true once the hub reports the cluster registered and heartbeating.
	// +optional
	Connected bool `json:"connected,omitempty"`
	// MetricsConnected is true while the hub's Prometheus reads the cluster's metrics.
	// +optional
	MetricsConnected bool `json:"metricsConnected,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=obsagent
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
// +kubebuilder:printcolumn:name="Hub",type=string,JSONPath=`.spec.hubRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Connected",type=boolean,JSONPath=`.status.connected`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// ObservabilityAgent is one cluster's observability agents, reporting to one hub of the matching
// purpose. Reconciled on the managing cluster, which writes the provider's concrete kind onto the
// agent's cluster, links it to the hub, and gives it its own deploy key.
type ObservabilityAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObservabilityAgentSpec   `json:"spec,omitempty"`
	Status ObservabilityAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ObservabilityAgentList is a list of ObservabilityAgent.
type ObservabilityAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObservabilityAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&ObservabilityProvider{}, &ObservabilityProviderList{},
		&ObservabilityHub{}, &ObservabilityHubList{},
		&ObservabilityAgent{}, &ObservabilityAgentList{},
	)
}
