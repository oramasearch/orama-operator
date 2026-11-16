// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	PeerTargetService           = "service"
	PeerTargetApp               = "app"
	PeerTargetKubernetesService = "kubernetesService"

	PeerPhasePending = "Pending"
	PeerPhaseReady   = "Ready"
	PeerPhaseFailed  = "Failed"
	PeerPhaseDeleted = "Deleted"
)

// PeerExport is one named service this end offers the other end.
// +kubebuilder:validation:XValidation:rule="self.targetKind == 'kubernetesService' ? has(self.serviceName) : has(self.targetId)",message="a kubernetesService export names serviceName; a service or app export names targetId"
type PeerExport struct {
	// Name is what the other end imports. Empty means the target Kubernetes Service's name.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`

	// TargetKind is a Lake service or app (by Lake id), or a Kubernetes Service in this namespace
	// (Orama's own stacks).
	// +kubebuilder:validation:Enum=service;app;kubernetesService
	TargetKind string `json:"targetKind"`

	// TargetID is the Lake id of a service or app target.
	// +optional
	// +kubebuilder:validation:Format=uuid
	TargetID string `json:"targetId,omitempty"`

	// ServiceName is a kubernetesService target.
	// +optional
	// +kubebuilder:validation:MinLength=1
	ServiceName string `json:"serviceName,omitempty"`

	// Port is the target Service's TCP port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PeerImport is one named service of the other end made reachable here.
type PeerImport struct {
	// Name is the other end's export name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Port is the other end's export port, and the port of the local Service that reaches it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PeerConnectionSpec exchanges named services with another cluster or network through
// zetta-tunnel's peer gateway at each end (decision D6). No address is routed.
type PeerConnectionSpec struct {
	// PeerID is the identity both ends share: the gateways refuse a request naming another one.
	// For a Lake peer it is the network_peers row id.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="peerId is immutable"
	PeerID string `json:"peerId"`

	// LakeRef names the Lake whose control plane declared this peer. Empty for Orama's own stacks.
	// +optional
	LakeRef string `json:"lakeRef,omitempty"`

	// CustomerID and WorkspaceID are Lake's, for a Lake peer.
	// +optional
	// +kubebuilder:validation:Format=uuid
	CustomerID string `json:"customerId,omitempty"`
	// +optional
	// +kubebuilder:validation:Format=uuid
	WorkspaceID string `json:"workspaceId,omitempty"`

	// Remote is the other gateway's host:port. Empty means the address the managing cluster's
	// PeerLink wrote into the credentials Secret (key "remote").
	// +optional
	Remote string `json:"remote,omitempty"`

	// Exports are the named services the other end may reach here, and nothing else.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Exports []PeerExport `json:"exports,omitempty"`

	// Imports are the other end's named services made reachable here.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Imports []PeerImport `json:"imports,omitempty"`

	// CredentialsSecretName holds this gateway's tls.crt and tls.key, the trust root ca.crt and the
	// other end's identity peer-name (and optionally remote). The managing cluster's PeerLink writes
	// it; nobody writes it by hand. Empty means <name>-credentials.
	// +optional
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`

	// PullSecretName is the image pull secret for the gateway. A Lake peer uses the Lake's.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// Desired removes the gateway, its Services and its policies before the CR goes when absent.
	// +kubebuilder:validation:Enum=present;absent
	// +kubebuilder:default=present
	Desired string `json:"desired,omitempty"`

	// Generation is the desired-row generation, acknowledged in status.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Generation int64 `json:"generation,omitempty"`
}

// PeerImportStatus is where an import is reachable in this namespace.
type PeerImportStatus struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
	// Service is the ClusterIP Service in this namespace that reaches the import.
	Service string `json:"service"`
}

// PeerExportStatus is what an export resolved to.
type PeerExportStatus struct {
	Name   string `json:"name"`
	Port   int32  `json:"port"`
	Target string `json:"target"`
}

// PeerConnectionStatus is returned to network_peer_status for a Lake peer.
type PeerConnectionStatus struct {
	// Phase is Pending, Ready, Failed or Deleted.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Address is this gateway's public host:port, the other end's remote.
	// +optional
	Address string `json:"address,omitempty"`
	// Identity is the name in this gateway's certificate.
	// +optional
	Identity string `json:"identity,omitempty"`
	// +optional
	Imports []PeerImportStatus `json:"imports,omitempty"`
	// +optional
	Exports []PeerExportStatus `json:"exports,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=peer
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=`.spec.remote`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PeerConnection is one end of a peer: named services exchanged with another cluster or network
// through zetta-tunnel's peer gateway.
type PeerConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PeerConnectionSpec   `json:"spec"`
	Status PeerConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PeerConnectionList contains a list of PeerConnection resources.
type PeerConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PeerConnection `json:"items"`
}

// PeerLinkEnd is one PeerConnection of the pair, on some cluster.
type PeerLinkEnd struct {
	// Name is this end's name in the pair; its certificate identity is <name>.<peerId>.peer.orama.internal.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// ClusterRef is the KubernetesCluster this end runs on. Empty means this cluster.
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// Namespace and PeerConnection name the end's PeerConnection.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	PeerConnection string `json:"peerConnection"`
}

// PeerLinkSpec pairs two PeerConnections: the managing cluster mints their shared trust root and
// one certificate per end, and writes each end's credentials Secret on its cluster.
type PeerLinkSpec struct {
	// PeerID is the identity both PeerConnections carry.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="peerId is immutable"
	PeerID string `json:"peerId"`

	// Ends are the two PeerConnections.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self[0].name != self[1].name",message="the two ends need different names"
	Ends []PeerLinkEnd `json:"ends"`
}

// PeerLinkEndStatus is what the managing cluster last wrote and saw for one end.
type PeerLinkEndStatus struct {
	Name string `json:"name"`
	// Identity is the end's certificate identity.
	Identity string `json:"identity"`
	// Written is true once the end's credentials Secret is current on its cluster.
	Written bool `json:"written"`
	// Address is the end's gateway address, read from its PeerConnection.
	// +optional
	Address string `json:"address,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// PeerLinkStatus reports both ends.
type PeerLinkStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Ends []PeerLinkEndStatus `json:"ends,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=peerlink
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PeerLink is how two PeerConnections get credentials: through the managing cluster, never by hand.
type PeerLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PeerLinkSpec   `json:"spec"`
	Status PeerLinkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PeerLinkList contains a list of PeerLink resources.
type PeerLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PeerLink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PeerConnection{}, &PeerConnectionList{}, &PeerLink{}, &PeerLinkList{})
}
