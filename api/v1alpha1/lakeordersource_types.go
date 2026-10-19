/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LakeOrderDedicatedCluster says how a dedicated cluster is made for a `dedicated_cluster` order.
type LakeOrderDedicatedCluster struct {
	// ProviderRef names the KubernetesClusterProvider that creates the cluster. It is the cloud
	// identity: on a cluster where it does not exist, or exists and is refused, a
	// `dedicated_cluster` order is marked failed with that reason.
	// +kubebuilder:validation:MinLength=1
	ProviderRef string `json:"providerRef"`
	// TemplateClusterRef names the KubernetesCluster a dedicated cluster is made like: its
	// Kubernetes version, node pools and tags, every ClusterHelmRelease installed on it (the
	// platform layer) and its OramaOperator. Only the region and the name differ. So an ordered
	// cluster has the same platform and operator as every other leaf (principle 11).
	// +kubebuilder:validation:MinLength=1
	TemplateClusterRef string `json:"templateClusterRef"`
}

// LakeOrderSourceSpec names where the orders are read and how each offer is provisioned.
type LakeOrderSourceSpec struct {
	// LakeSetRef names the LakeSet whose Lakes take orders (lake.live: `lake-prod`). The orders
	// are read from every member's control-plane Postgres, in the `cloud_orders` table, through
	// the member's cluster's kubeconfig. Every ordered Lake starts from this set's template and
	// secrets, and runs on clusters of the same purpose.
	// +kubebuilder:validation:MinLength=1
	LakeSetRef string `json:"lakeSetRef"`

	// HostDomain is the DNS zone ordered Lakes are served in: each at `<lake name>.<hostDomain>`.
	// It must be in the zone of the template's `dnsSecretRef`. At most 234 characters, so a host
	// (`order-<12 hex digits>.` and the domain) fits lake's 253 for `cloud_orders.host`.
	// +kubebuilder:validation:MaxLength=234
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	HostDomain string `json:"hostDomain"`

	// Member is every ordered Lake's own settings over the set's template, as a set member's
	// `spec` (Clerk, environment). The operator sets `host` and `args` itself; a Member that sets
	// either, or LAKE_PUBLIC_HOST in `env`, is refused.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Member *apiextensionsv1.JSON `json:"member,omitempty"`

	// MemberSecrets are copied into every ordered Lake's namespace, as a set member's `secrets`.
	// +optional
	MemberSecrets []SetSecret `json:"memberSecrets,omitempty"`

	// DedicatedCluster says how `dedicated_cluster` orders are provisioned. Without it, every
	// such order is marked failed with that reason.
	// +optional
	DedicatedCluster *LakeOrderDedicatedCluster `json:"dedicatedCluster,omitempty"`

	// PollInterval is how often the orders are read. Default 30s.
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
}

// LakeOrderSourceMemberStatus is one member of the set, as an order source.
type LakeOrderSourceMemberStatus struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
	// Reachable is true when the member's `cloud_orders` table was read on the last pass.
	Reachable bool `json:"reachable"`
	// Message says why it was not read.
	// +optional
	Message string `json:"message,omitempty"`
}

// LakeOrderStatus is one order this source is provisioning.
type LakeOrderStatus struct {
	ID string `json:"id"`
	// Member is the set member whose control plane holds the order.
	Member string `json:"member"`
	Offer  string `json:"offer"`
	// +optional
	Region string `json:"region,omitempty"`
	// LakeSet is the LakeSet made for the order.
	// +optional
	LakeSet string `json:"lakeSet,omitempty"`
	// +optional
	Host string `json:"host,omitempty"`
	// Message says what the order is waiting for.
	// +optional
	Message string `json:"message,omitempty"`
}

// LakeOrderSourceStatus is what the orders are doing.
type LakeOrderSourceStatus struct {
	// Phase is Ready (every member read, no order waiting on a fix), Progressing (orders being
	// provisioned), Pending (the set or its clusters are not there yet) or Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Provisioning, ReadyOrders and Failed count this source's orders in each state.
	// +optional
	Provisioning int32 `json:"provisioning,omitempty"`
	// +optional
	ReadyOrders int32 `json:"readyOrders,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// Members reports each member read.
	// +optional
	Members []LakeOrderSourceMemberStatus `json:"members,omitempty"`
	// Orders lists the orders being provisioned.
	// +optional
	Orders []LakeOrderStatus `json:"orders,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=lorders
//+kubebuilder:printcolumn:name="Set",type=string,JSONPath=`.spec.lakeSetRef`
//+kubebuilder:printcolumn:name="Provisioning",type=integer,JSONPath=`.status.provisioning`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyOrders`
//+kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// LakeOrderSource provisions the paid orders that a LakeSet's Lakes record in their control-plane
// Postgres (lake#831, operator#634): a private warehouse is a new Lake on an existing cluster of
// the set, in the ordered region; a dedicated cluster is a new KubernetesCluster, its platform,
// its OramaOperator and the Lake on it. The orders are the source of truth for what it creates.
type LakeOrderSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LakeOrderSourceSpec   `json:"spec,omitempty"`
	Status LakeOrderSourceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LakeOrderSourceList is a list of LakeOrderSource.
type LakeOrderSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LakeOrderSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LakeOrderSource{}, &LakeOrderSourceList{})
}
