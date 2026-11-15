/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The concrete kinds of the pixie observability provider (operator#15, #456). The managing cluster's
// ObservabilityHub and ObservabilityAgent controllers write them onto the hub's and each agent's
// cluster; nobody writes them by hand. The cluster's own operator reconciles them from the manifests
// it vendors from oramasearch/pixie (internal/controller/assets/pixie), with no Pixie operator, no
// OLM and no Vizier updater.

const (
	// PixieCloudNamespace is where self-hosted Pixie Cloud runs. Pixie's services address each other
	// in it by name, so it is fixed, as upstream fixes it.
	PixieCloudNamespace = "plc"
	// PixieVizierNamespace is where Vizier runs.
	PixieVizierNamespace = "pl"
)

// PixieCloudAgent is one agent cluster the hub issues a deploy key for.
type PixieCloudAgent struct {
	// Name is the ObservabilityAgent's name. Its deploy key is Secret pixie-deploy-key-<name>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`
	// Cluster is the agent's cluster, the name it is registered under.
	// +kubebuilder:validation:MinLength=1
	Cluster string `json:"cluster"`
}

// PixieAlertRoute is one Alertmanager receiver, with its URL already copied to this cluster.
type PixieAlertRoute struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:items:Enum=critical;warning;info
	Severities []string `json:"severities"`
	// Kind is slack or webhook.
	// +kubebuilder:validation:Enum=slack;webhook
	Kind string `json:"kind"`
	// SecretName holds key "url" in the resource's namespace, written by the managing cluster.
	SecretName string `json:"secretName"`
	// +optional
	Channel string `json:"channel,omitempty"`
}

// PixieCloudSpec is self-hosted Pixie Cloud on one cluster.
type PixieCloudSpec struct {
	// Release is the provider's release, copied from the ObservabilityProvider.
	Release PixieRelease `json:"release"`
	// Hub is the ObservabilityHub this was written for.
	// +kubebuilder:validation:MinLength=1
	Hub string `json:"hub"`
	// Purpose is the hub cluster's purpose tag, as the managing cluster read it: production on the
	// managing cluster, nonprod on a nonprod cluster.
	// +kubebuilder:validation:Enum=nonprod;production
	Purpose string `json:"purpose"`
	// Cluster is the hub's own KubernetesCluster: the cluster label of its own metrics. Empty on the
	// managing cluster, whose metrics are labelled with the hub's name.
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// MetricsRetention is how long the hub Prometheus keeps federated metrics.
	// +kubebuilder:validation:Pattern=`^[0-9]+(h|d|w|y)$`
	// +optional
	MetricsRetention string `json:"metricsRetention,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Domain string `json:"domain"`
	// +kubebuilder:validation:MinLength=1
	Organization string `json:"organization"`
	// +kubebuilder:validation:MinLength=3
	AdminEmail string `json:"adminEmail"`
	// Users are the other people who sign in, as members of the organisation. Each gets a
	// generated password in Secret pixie-user-<email>.
	// +listType=set
	// +optional
	Users []string `json:"users,omitempty"`
	// PullSecretName is the registry credentials Secret in this namespace.
	// +kubebuilder:validation:MinLength=1
	PullSecretName string `json:"pullSecretName"`
	// +optional
	Storage ObservabilityHubStorage `json:"storage,omitempty"`
	// Agents are the clusters that get a deploy key. Only the managing cluster writes this list,
	// after checking each agent's purpose against the hub's.
	// +optional
	// +listType=map
	// +listMapKey=name
	Agents []PixieCloudAgent `json:"agents,omitempty"`
	// AlertRoutes are this cluster's alert receivers.
	// +optional
	AlertRoutes []PixieAlertRoute `json:"alertRoutes,omitempty"`
}

// PixieCloudAgentStatus is what the hub knows of one agent cluster.
type PixieCloudAgentStatus struct {
	Name string `json:"name"`
	// DeployKeySecret is the Secret in this namespace that holds the cluster's deploy key.
	// +optional
	DeployKeySecret string `json:"deployKeySecret,omitempty"`
	// Registered is true once a Vizier registered with the cluster's key.
	Registered bool `json:"registered"`
	// Connected is true while that Vizier heartbeats.
	Connected bool `json:"connected"`
	// MetricsConnected is true while the hub Prometheus reads the cluster's metrics.
	MetricsConnected bool `json:"metricsConnected"`
}

// PixieCloudStatus is the hub's readiness, per component.
type PixieCloudStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Ready string `json:"ready,omitempty"`
	// +optional
	Components []ObservabilityComponentStatus `json:"components,omitempty"`
	// +optional
	Agents []PixieCloudAgentStatus `json:"agents,omitempty"`
	// CACertificate is the hub's public trust root (PEM), which every agent verifies the hub with.
	// +optional
	CACertificate string `json:"caCertificate,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=pxcloud
// +kubebuilder:printcolumn:name="Hub",type=string,JSONPath=`.spec.hub`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// PixieCloud is self-hosted Pixie Cloud, the pixie provider's hub. Written by the managing cluster's
// ObservabilityHub controller into namespace plc of the hub's cluster.
type PixieCloud struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PixieCloudSpec   `json:"spec,omitempty"`
	Status PixieCloudStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PixieCloudList is a list of PixieCloud.
type PixieCloudList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PixieCloud `json:"items"`
}

// PixieVizierSpec is Vizier on one cluster.
type PixieVizierSpec struct {
	// Release is the provider's release, copied from the ObservabilityProvider. Empty while the
	// provider is not installable: the cluster's alert routing and rules are written anyway, and
	// Vizier waits.
	// +optional
	Release *PixieRelease `json:"release,omitempty"`
	// Agent is the ObservabilityAgent this was written for.
	// +kubebuilder:validation:MinLength=1
	Agent string `json:"agent"`
	// Purpose is the agent cluster's purpose tag, as the managing cluster read it.
	// +kubebuilder:validation:Enum=nonprod;production
	Purpose string `json:"purpose"`
	// ClusterName is the name the cluster is registered under in Pixie Cloud.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
	// CloudAddress is host:port of the hub's vzconn: the hub's Service on its own cluster, or the
	// PeerConnection import Service in this namespace.
	// +kubebuilder:validation:MinLength=1
	CloudAddress string `json:"cloudAddress"`
	// CloudCASecretName holds ca.crt, the hub's trust root, which the cloud connector verifies the
	// hub's certificate with.
	// +kubebuilder:validation:MinLength=1
	CloudCASecretName string `json:"cloudCASecretName"`
	// DeployKeySecretName holds deploy-key, this cluster's own key, copied from the hub.
	// +kubebuilder:validation:MinLength=1
	DeployKeySecretName string `json:"deployKeySecretName"`
	// PullSecretName is the registry credentials Secret in this namespace.
	// +kubebuilder:validation:MinLength=1
	PullSecretName string `json:"pullSecretName"`
	// NodeMemory caps each node agent; its data store is sized from the same number.
	NodeMemory resource.Quantity `json:"nodeMemory"`
	// AlertRoutes are this cluster's alert receivers.
	// +optional
	AlertRoutes []PixieAlertRoute `json:"alertRoutes,omitempty"`
}

// PixieVizierStatus is Vizier's readiness, per component.
type PixieVizierStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Ready string `json:"ready,omitempty"`
	// +optional
	Components []ObservabilityComponentStatus `json:"components,omitempty"`
	// ClusterID is the id Pixie Cloud registered the cluster under.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=pxvizier
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// PixieVizier is Pixie's in-cluster part, the pixie provider's agent. Written by the managing
// cluster's ObservabilityAgent controller into namespace pl of the agent's cluster.
type PixieVizier struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PixieVizierSpec   `json:"spec,omitempty"`
	Status PixieVizierStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PixieVizierList is a list of PixieVizier.
type PixieVizierList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PixieVizier `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&PixieCloud{}, &PixieCloudList{},
		&PixieVizier{}, &PixieVizierList{},
	)
}
