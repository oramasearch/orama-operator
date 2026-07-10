/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OramaZettaClusterSpec describes a headless Orama Zetta cluster — N equal-peer CRDT nodes.
// Step 1 of the One Orama plan (orama-bridge#130): the operator only *runs* Zetta headless; the
// bi-directional operator↔Zetta symbiosis (Zetta as the operator's datastore) is a later stage.
type OramaZettaClusterSpec struct {
	// Replicas is the number of equal-peer headless zetta nodes.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Image is the Zetta-headless container image (built like the other components → private Docker Hub).
	// +kubebuilder:default="oramasearch/zetta:headless-v1-sandbox"
	// +optional
	Image string `json:"image,omitempty"`

	// PullSecretName lets the pods pull a private zetta image.
	// +optional
	PullSecretName string `json:"pullSecretName,omitempty"`

	// Storage sizes the per-node PVC mounted at the node's --data-dir (the CRDT doc, node_id, certs,
	// and JSONL/large-object data). Each node keeps its identity across restarts via this volume.
	// +optional
	Storage OramaZettaStorage `json:"storage,omitempty"`

	// Resources caps each node (mem/cpu) — right-sizing (see the operator's principle #8).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// ClusterPlaintext disables inter-node mTLS (the peers join without a token). Convenient inside a
	// single trusted namespace (+ a NetworkPolicy); leave false to use the generated join-token Secret.
	// +kubebuilder:default=false
	// +optional
	ClusterPlaintext bool `json:"clusterPlaintext,omitempty"`

	// Host, if set, exposes the cluster's HTTP API (:4800 — MCP + REST + serving, the Zetta UI) via an
	// Ingress at this hostname (e.g. zetta.oramadev.net). Requires an ingress controller.
	// +optional
	Host string `json:"host,omitempty"`

	// DNSSecretRef, if set together with Host, creates a proxied Cloudflare DNS record (Host → the
	// ingress LoadBalancer) using the Cloudflare credentials (token/zoneID/accountID) in the named
	// Secret — normally the same `cloudflare-api-token` Secret the OramaCloudV1 uses.
	// +optional
	DNSSecretRef *corev1.LocalObjectReference `json:"dnsSecretRef,omitempty"`

	// MCPTokenAuth provisions a 64-char bearer token (Secret `<name>-zetta-mcp`) and requires it on
	// the node's MCP + REST surface (`--mcp-token`).
	//
	// DEFAULT false — because Zetta's `--mcp-token` treats the token as an *access-key id* that must
	// already be a provisioned principal with grants. A bare operator-minted token currently passes
	// the bearer check but is then denied EVERY capability call ("unknown access key — denied"), for
	// both REST `/v1/*` and MCP `tools/call` — so turning this on makes the node authenticate but do
	// nothing. Leave it off (the node runs as the functional unauthenticated-local session) until the
	// node can seed the configured bearer as an owner key (oramasearch/zetta#733). When true, the
	// operator still won't expose a working session — only enable it once you also provision a
	// matching access key.
	// +kubebuilder:default=false
	// +optional
	MCPTokenAuth bool `json:"mcpTokenAuth,omitempty"`

	// ClusterName is the GLOBAL Zetta cluster name. A Zetta cluster can span multiple k8s clusters; each
	// k8s cluster's OramaZettaCluster CR is a SLICE of it, and CRs sharing this name are SIBLINGS — equal
	// peers in one multi-leader cluster (operator#71). Empty = a standalone cluster named after the CR.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// ExternalPeers are peer addresses (`host:port`) of sibling nodes in OTHER k8s clusters (or bare
	// hosts) — the route table (operator#70). Each is appended as a `--peer` flag so this node's nodes
	// dial them and join the shared Zetta cluster. Zetta is the mesh (gossip relays transitively), so a
	// connected dial-graph is enough; NAT'd nodes just list a reachable sibling here.
	// +optional
	ExternalPeers []string `json:"externalPeers,omitempty"`

	// HostNetwork runs the node pods on the host network, so the peer (:7777) + API (:4800) ports are
	// reachable at the k8s node's own address — the simplest way to make a node dialable by siblings in
	// other clusters (esp. a single-node cluster). Default false.
	// +kubebuilder:default=false
	// +optional
	HostNetwork bool `json:"hostNetwork,omitempty"`

	// ProvisionOperatorSchema makes this cluster the operator's own datastore (One Orama symbiosis v0):
	// once the node is up, the operator applies its control-plane + data-plane schema to it — the
	// `orama_control` database (the operator's registry: components, reconcile_events) and the
	// `orama_data` database (the application data landing zone: documents) — via Zetta's `/v1/sql`.
	// Idempotent (tolerates "already exists"; Zetta has no IF NOT EXISTS). Default true.
	// +kubebuilder:default=true
	// +optional
	ProvisionOperatorSchema *bool `json:"provisionOperatorSchema,omitempty"`
}

// OramaZettaStorage configures each node's data volume.
type OramaZettaStorage struct {
	// Size of the per-node PVC (e.g. "10Gi").
	// +kubebuilder:default="10Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClassName for the per-node PVCs (empty = the cluster default StorageClass).
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// OramaZettaClusterStatus is the observed state of the cluster.
type OramaZettaClusterStatus struct {
	// Phase: Progressing | Ready | Degraded.
	// +optional
	Phase string `json:"phase,omitempty"`
	// ReadyNodes is the number of ready zetta pods.
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`
	// TotalNodes is the desired node count.
	// +optional
	TotalNodes int32 `json:"totalNodes,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=ozc
//+kubebuilder:printcolumn:name="Nodes",type=string,JSONPath=`.status.readyNodes`
//+kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.status.totalNodes`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`

// OramaZettaCluster deploys a headless Orama Zetta cluster (a StatefulSet of equal-peer CRDT nodes)
// from one CR. Peer replication is TCP 7777 (mTLS unless clusterPlaintext); the HTTP API (MCP + REST
// + serving) is 4800; each node persists to a per-node PVC at /data. See the plan of record in
// oramasearch/orama-bridge docs/projects/one-orama.
type OramaZettaCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaZettaClusterSpec   `json:"spec,omitempty"`
	Status OramaZettaClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OramaZettaClusterList contains a list of OramaZettaCluster.
type OramaZettaClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaZettaCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OramaZettaCluster{}, &OramaZettaClusterList{})
}
