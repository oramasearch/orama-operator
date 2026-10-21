/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Node-reachability types (the corrected mesh model — oramasearch/zetta#739). A slice DECLARES how
// siblings in the shared Zetta cluster reach its nodes, and the operator wires connectivity from that
// — instead of the old anti-pattern of putting an inbound LoadBalancer on a private node.
const (
	// ReachabilityWANAccessible — the slice has a routable address (a public/LAN IP, a host whose node
	// already has a public IP, or a cloud LB). It is a RENDEZVOUS: the operator advertises that address
	// as `.status.wanAddress`, and others dial OUT to it. Siblings copy it into their `spec.externalPeers`,
	// and the Cloudflare LB origin pool (operator#73) is populated from the wan-accessible set.
	ReachabilityWANAccessible = "wan-accessible"
	// ReachabilityDialOutOnly — the slice is behind NAT / a private VNet (a home node, an AKS pod on a
	// private IP). It dials OUT only (`--peer` to the wan-accessible rendezvous listed in
	// `spec.externalPeers`) and accepts NO inbound: it never gets an inbound LoadBalancer. This is the
	// DEFAULT — a private node is always the initiator.
	ReachabilityDialOutOnly = "dial-out-only"
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

	// Env appends Zetta-specific environment variables after the operator-managed variables.
	// Operator-managed names take precedence and conflicting entries are ignored and reported in
	// status. Both literal values and Kubernetes valueFrom sources are supported.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Models are model checkpoints placed on every node's data volume before the node starts, each
	// at /data/models/<name> (lake#246). Each comes from a model image: an OCI image that holds the
	// checkpoint files under /model and a POSIX shell. An init container copies them when the
	// directory does not already hold that image's copy, so a node restart copies nothing and a new
	// image replaces the old copy. Model images are pulled with PullSecretName. See
	// docs/zetta-models.md.
	// +listType=map
	// +listMapKey=name
	// +optional
	Models []OramaZettaModel `json:"models,omitempty"`

	// NOTE: inter-node peer traffic is ALWAYS mutual-TLS. The operator mints one shared cluster join
	// token per cluster (Secret `<name>-zetta-cluster`) and injects it into every node as
	// ZETTA_CLUSTER_TOKEN; the engine derives the cluster CA from it, so all nodes form ONE cluster
	// (operator#92). There is no plaintext-peer option — an unencrypted `--cluster-plaintext` mode was
	// removed as a security hole once real mTLS clustering worked. See docs/zetta-cluster-mtls.md.

	// IngressEnabled gates external HTTP exposure of the cluster's API/UI (:4800). When true AND Host is
	// set, the operator provisions an nginx Ingress at Host (root `/` → a "zetta" landing placeholder,
	// every other path → the Zetta HTTP API/UI on :4800) and, if DNSSecretRef is also set, a proxied
	// Cloudflare DNS record (Host → the ingress LoadBalancer). When false (the default) no Ingress,
	// landing, or DNS record is created — and any previously created are torn down — even if Host is
	// still set. Host names the endpoint; this bool is the explicit on/off switch.
	// +kubebuilder:default=false
	// +optional
	IngressEnabled bool `json:"ingressEnabled,omitempty"`

	// IngressClassName is the ingress controller that serves this resource's Ingresses: "nginx"
	// (ingress-nginx, the default until a cluster's cutover) or "zetta-tunnel" (zetta-tunnel's
	// ingress gateway, docs/infrastructure/ingress.md). Changing it moves the hosts' DNS, because DNS
	// follows the Ingress's load-balancer address.
	// +kubebuilder:validation:Enum=nginx;zetta-tunnel
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// Host is the domain name the cluster's HTTP API/UI (:4800 — MCP + REST + serving) is exposed at
	// (e.g. zetta.oramadev.net) when IngressEnabled is true. Requires an ingress controller.
	// +optional
	Host string `json:"host,omitempty"`

	// DNSSecretRef, if set together with Host, creates a proxied Cloudflare DNS record (Host → the
	// ingress LoadBalancer) using the Cloudflare credentials (token/zoneID/accountID) in the named
	// Secret — normally the same `cloudflare-api-token` Secret the OramaCloudV1 uses.
	// +optional
	DNSSecretRef *corev1.LocalObjectReference `json:"dnsSecretRef,omitempty"`

	// MCPTokenAuth is DEPRECATED and a NO-OP as of zetta#799. The engine made auth MANDATORY and removed the
	// `--mcp-token` flag (passing it is now an unknown-option error that crashes the node), so the operator no
	// longer renders it or provisions a `<name>-zetta-mcp` Secret. Auth is always enforced with the seeded
	// admin token instead (see RequireAuth). Retained only for API compatibility; setting it has no effect.
	// +kubebuilder:default=false
	// +optional
	MCPTokenAuth bool `json:"mcpTokenAuth,omitempty"`

	// RequireAuth is DEPRECATED and a NO-OP as of zetta#799. The engine made auth MANDATORY and removed the
	// `--require-auth` flag — every node ALWAYS gates its HTTP/MCP/SQL surface on a bearer that resolves to a
	// user, and there is no unauthenticated/plaintext/legacy-shared-secret mode. The operator now ALWAYS mints
	// a 64-hex admin token into Secret `<name>-zetta-admin` (key `token`), injects it as `ZETTA_ADMIN_TOKEN` →
	// `--admin-token` (seeding the owner) on every node, authenticates its OWN Zetta SQL client with it, and
	// the ZettaClusterE2ETest always runs its authz assertions (`ZETTA_E2E_REQUIRE_AUTH=1`). Retained only for
	// API compatibility; setting it has no effect (auth is enforced regardless). The target's image MUST be an
	// auth-capable build (oramasearch/zetta#799) — the pre-#799 sandbox image is not.
	// +kubebuilder:default=true
	// +optional
	RequireAuth bool `json:"requireAuth,omitempty"`

	// ClusterName is the GLOBAL Zetta cluster name. A Zetta cluster can span multiple k8s clusters; each
	// k8s cluster's OramaZettaCluster CR is a SLICE of it, and CRs sharing this name are SIBLINGS — equal
	// peers in one multi-leader cluster (operator#71). Empty = a standalone cluster named after the CR.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// ExternalPeers are peer addresses (`host:port`) of sibling nodes in OTHER k8s clusters (or bare
	// hosts) — the route table (operator#70). Each is appended as a `--peer` flag so this node's nodes
	// dial them and join the shared Zetta cluster. Zetta is the mesh (gossip relays transitively), so a
	// connected dial-graph is enough; NAT'd nodes just list a reachable sibling here.
	//
	// Each entry must be exactly `host:port`: a DNS name, an IPv4 address or a bracketed IPv6 address
	// (`[fd00::1]:7777`), and a port from 1 to 65535 (operator#279). The API server refuses anything else;
	// the controller re-checks (for CRs written before this rule) and, on an invalid entry, leaves the
	// StatefulSet unchanged and sets `PeerAddressesValid=False`. Peers reach Zetta as process arguments,
	// never as shell text.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=261
	// +kubebuilder:validation:items:Pattern=`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:]+\]):([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$`
	// +optional
	ExternalPeers []string `json:"externalPeers,omitempty"`

	// Reachability declares how siblings in the shared Zetta cluster reach THIS slice's nodes, per the
	// corrected node-reachability model (oramasearch/zetta#739):
	//   - `dial-out-only` (DEFAULT): the slice is behind NAT / a private VNet — it dials OUT to the
	//     wan-accessible rendezvous(es) in `spec.externalPeers` and accepts NO inbound. The operator
	//     NEVER provisions an inbound LoadBalancer for it (any `exposePeerLoadBalancer` is ignored + a
	//     warning is logged — see that field).
	//   - `wan-accessible`: the slice has a routable address (see `spec.wanAddress` / `spec.hostNetwork`
	//     / `spec.exposePeerLoadBalancer`) — it is a RENDEZVOUS others dial OUT to. The operator
	//     advertises that address as `.status.wanAddress`, which siblings copy into their
	//     `spec.externalPeers` and the Cloudflare LB origin pool (operator#73) consumes.
	// Default `dial-out-only` (a private node is always the initiator).
	// +kubebuilder:validation:Enum=wan-accessible;dial-out-only
	// +kubebuilder:default=dial-out-only
	// +optional
	Reachability string `json:"reachability,omitempty"`

	// Ephemeral marks this slice as burst / autoscaled capacity that is EXPECTED to churn — nodes come
	// and go (e.g. the elastic-capacity autoscaler, operator#69). An ephemeral slice is ALWAYS
	// dial-out-only: it is never advertised as a rendezvous (its address would flap), so it is forced to
	// dial-out-only regardless of `spec.reachability`, and its membership blips are not treated as a
	// cluster fault. Default false.
	// +kubebuilder:default=false
	// +optional
	Ephemeral bool `json:"ephemeral,omitempty"`

	// WanAddress is the routable address (`host` or `host:port`) that siblings use to reach this
	// wan-accessible slice's peer port. If no port is given, the peer port (:7777) is assumed. When set
	// (and reachability is wan-accessible) it is authoritative — the operator advertises it verbatim as
	// `.status.wanAddress` (e.g. a Tailscale/WireGuard address, a stable public IP, or a DNS name). When
	// empty, the operator derives the address from `spec.hostNetwork` (the node's host IP) or
	// `spec.exposePeerLoadBalancer` (the cloud LB's public IP). Ignored unless reachability is
	// wan-accessible. Validated like `spec.externalPeers` (a DNS name, IPv4, `[IPv6]` or a bare IPv6 literal,
	// with an optional port 1-65535); an invalid value is refused the same way (operator#279).
	// +kubebuilder:validation:MaxLength=261
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:]+\])(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?|[0-9A-Fa-f]{0,4}(:[0-9A-Fa-f]{0,4}){2,7})$`
	// +optional
	WanAddress string `json:"wanAddress,omitempty"`

	// HostNetwork runs the node pods on the host network, so the peer (:7777) + API (:4800) ports are
	// reachable at the k8s node's own address — the simplest way to make a node dialable by siblings in
	// other clusters (esp. a single-node cluster). When replicas>1, the nodes pack onto the shared
	// host(s) with per-ordinal ports (node-n listens on 7777+n / 4800+n) and seed off node-0 on the host
	// loopback — correct for a single big host / single-node k8s cluster. Default false.
	// +kubebuilder:default=false
	// +optional
	HostNetwork bool `json:"hostNetwork,omitempty"`

	// ExposePeerLoadBalancer creates a LoadBalancer Service for the peer port (:7777) so nodes in OTHER
	// k8s clusters can dial this cluster from outside.
	//
	// ONLY valid for a `reachability: wan-accessible` slice — it makes a slice the platform intends to be
	// GENUINELY wan-accessible reachable via a cloud public IP (put `<ip>:7777` in a sibling's
	// spec.externalPeers; the operator also surfaces it as `.status.wanAddress`). Even then a cloud LB is
	// only ONE option — a node whose host already has a public IP (`spec.hostNetwork`) or a mesh-VPN
	// address (`spec.wanAddress`) needs none.
	//
	// ANTI-PATTERN — do NOT use this to "reach" a private (`dial-out-only`) node. That inverts the mesh
	// model (oramasearch/zetta#739): a NAT'd/private node must DIAL OUT to a rendezvous, not accept
	// inbound. On a `dial-out-only` slice (the default) this field is IGNORED and the operator logs a
	// warning; any previously-created peer LB is torn down.
	//
	// NOTE: peer traffic is mTLS (the shared cluster join token derives the cluster CA, operator#92), so
	// the exposed port is encrypted on the wire — keep the `<name>-zetta-cluster` Secret protected, as it
	// is the cluster's identity. Default false.
	// +kubebuilder:default=false
	// +optional
	ExposePeerLoadBalancer bool `json:"exposePeerLoadBalancer,omitempty"`

	// ProvisionOperatorSchema makes this cluster the operator's own datastore (One Orama symbiosis v0):
	// once the node is up, the operator applies its control-plane + data-plane schema to it — the
	// `orama_control` database (the operator's registry: components, reconcile_events) and the
	// `orama_data` database (the application data landing zone: documents) — via Zetta's `/v1/sql`.
	// Idempotent (tolerates "already exists"; Zetta has no IF NOT EXISTS). Default true.
	// +kubebuilder:default=true
	// +optional
	ProvisionOperatorSchema *bool `json:"provisionOperatorSchema,omitempty"`

	// Capacity opts this cluster into elastic node capacity — the first slice of Zetta-driven elastic
	// capacity (operator#69). When enabled, the operator owns a cluster-node-autoscaler NodePool
	// (Karpenter) and pins the Zetta pods onto it, so raising `spec.replicas` beyond current node
	// capacity leaves pods Pending → the autoscaler provisions matching nodes just-in-time → the new
	// pods schedule and join the CRDT cluster. Off by default (a standalone Zetta doesn't need it).
	// +optional
	Capacity *OramaZettaCapacity `json:"capacity,omitempty"`
}

// OramaZettaCapacity configures elastic node capacity for the cluster. It abstracts "provision capacity
// for Zetta" over the cluster's node autoscaler; the first slice targets Karpenter (EKS-native, and the
// engine behind AKS Node Auto Provisioning). A cluster with no node autoscaler (k0s / bare metal) is a
// graceful no-op: `spec.replicas` still packs more pods onto the fixed host until it's full.
type OramaZettaCapacity struct {
	// Karpenter drives node provisioning through a Karpenter NodePool the operator owns and reconciles.
	// +optional
	Karpenter *KarpenterCapacity `json:"karpenter,omitempty"`
}

// KarpenterCapacity is the operator-managed Karpenter NodePool for the Zetta cluster. The operator owns
// only the (cloud-neutral) NodePool — labels, taints, requirements, disruption policy, and the cost cap
// (`limits`); the provider-specific NodeClass (EC2NodeClass on EKS / AKSNodeClass on AKS) is owned by
// the platform and referenced via NodeClassRef, because it carries cluster-specific cloud details
// (AMI/subnet/IAM on AWS) the operator cannot invent. Karpenter reference model: NodePool → NodeClass →
// NodeClaim → Node (see docs/research/karpenter-manual.md).
type KarpenterCapacity struct {
	// Enabled turns on operator-managed Karpenter capacity. When true the operator reconciles the Zetta
	// NodePool and pins the Zetta pods onto it (a dedicated taint + toleration and a pool nodeSelector),
	// so the pods land only on Karpenter-provisioned Zetta nodes. Default false.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// NodeClassRef points at the provider NodeClass Karpenter uses to launch nodes: an EC2NodeClass
	// (group `karpenter.k8s.aws`, the default) on EKS, or an AKSNodeClass (group `karpenter.azure.com`)
	// on AKS Node Auto Provisioning. Name is REQUIRED when enabled — without it the operator cannot
	// build a valid NodePool and reports `CapacityReady=False, Reason=Blocked` instead of crash-looping.
	// +optional
	NodeClassRef KarpenterNodeClassRef `json:"nodeClassRef,omitempty"`

	// InstanceTypes optionally restricts the pool to specific instance types via the well-known
	// `node.kubernetes.io/instance-type` label (portable across AWS + Azure). Empty (recommended) lets
	// Karpenter choose the cheapest fitting type from the NodeClass offerings — broadest availability.
	// +optional
	InstanceTypes []string `json:"instanceTypes,omitempty"`

	// Arch is the CPU architecture the pool provisions. Default amd64.
	// +kubebuilder:validation:Enum=amd64;arm64
	// +kubebuilder:default=amd64
	// +optional
	Arch string `json:"arch,omitempty"`

	// CapacityType selects on-demand or spot nodes. Default on-demand: a Zetta node is a stateful CRDT
	// peer, so stable On-Demand capacity is the safe default — only choose spot with enough replicas to
	// tolerate an interruption (a spot reclaim evicts a peer).
	// +kubebuilder:validation:Enum=on-demand;spot
	// +kubebuilder:default=on-demand
	// +optional
	CapacityType string `json:"capacityType,omitempty"`

	// Limits caps the aggregate capacity this pool may provision — the cost/scale guardrail (operator#69).
	// Karpenter stops provisioning once any limit is hit, so a runaway replica bump can't launch unbounded
	// nodes.
	// +optional
	Limits KarpenterLimits `json:"limits,omitempty"`

	// Install makes the operator OWN the Karpenter controller install itself (operator#75) — its
	// Deployment + ServiceAccount + RBAC + the Karpenter CRDs (NodePool / NodeClaim / EC2NodeClass),
	// pinned to a vendored upstream version and reconciled as owned resources — rather than assuming
	// Karpenter is already present. Off by default (the platform may pre-install Karpenter and just use
	// `enabled`). Cloud-gated: a graceful no-op on bare-metal / k0s (no cloud provider → no VM API for
	// Karpenter to drive) and on AKS, where Node Auto Provisioning already deploys and manages Karpenter
	// for you; the self-install path targets EKS (the self-managed AWS provider). Status is surfaced with
	// the `KarpenterInstalled` condition.
	//
	// CLOUD IAM PREREQUISITE — the operator CANNOT satisfy this; it is a cloud-account prerequisite, the
	// same class as the image pull secret. Karpenter's controller needs a cloud identity permitted to
	// create/terminate VMs, and the launched nodes need their own identity:
	//   - EKS: an IAM role assumed by the controller ServiceAccount via IRSA (set ControllerRoleARN) or
	//     EKS Pod Identity, plus a node instance profile referenced by the platform-owned EC2NodeClass.
	//   - AKS: an Azure managed identity with a role assignment (handled by NAP — the operator does not
	//     self-install on AKS).
	// The in-cluster operator cannot mint these. When the controller identity is absent the reconciler
	// reports `KarpenterInstalled=False, Reason=CloudIdentityMissing` and does NOT deploy the controller
	// (which would otherwise crash-loop). See docs/karpenter-cloud-iam-prerequisite.md.
	// +kubebuilder:default=false
	// +optional
	Install bool `json:"install,omitempty"`

	// InstallClusterName is the cloud cluster name Karpenter's controller uses to discover the cluster's
	// resources (AWS `settings.clusterName` — the tagged subnets / security-groups / interruption queue).
	// REQUIRED when `install` is true on EKS; empty ⇒ `KarpenterInstalled=False,
	// Reason=InstallClusterNameRequired`. Ignored when `install` is false.
	//
	// Must be a valid EKS cluster name: 1-100 characters, a letter or digit first, then letters, digits,
	// hyphens or underscores (operator#79). The API server enforces this pattern, and the reconciler checks
	// it again (a CR written before the pattern existed is refused with `KarpenterInstalled=False,
	// Reason=InvalidInstallSpec`), because the value is written into the Karpenter controller Deployment.
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][A-Za-z0-9\-_]{0,99}$`
	// +optional
	InstallClusterName string `json:"installClusterName,omitempty"`

	// ControllerRoleARN is the AWS IAM role ARN the Karpenter CONTROLLER assumes to call EC2 — annotated
	// onto its ServiceAccount for IRSA (`eks.amazonaws.com/role-arn`). This is the cloud IAM identity the
	// operator CANNOT mint (a cloud-account prerequisite). REQUIRED when `install` is true on EKS unless
	// the controller identity is wired out-of-band (e.g. EKS Pod Identity); empty ⇒
	// `KarpenterInstalled=False, Reason=CloudIdentityMissing` and the controller is NOT deployed (no
	// crash-loop). This is the CONTROLLER identity only; the launched NODES get their own instance
	// profile via the platform-owned NodeClass (NodeClassRef). Ignored when `install` is false.
	//
	// Must be an IAM role ARN in the aws, aws-cn or aws-us-gov partition:
	// `arn:<partition>:iam::<12-digit account>:role/<path and name>`, where the path and name use only
	// the characters IAM allows (operator#79). The API server enforces this pattern, and the reconciler
	// checks it again (`KarpenterInstalled=False, Reason=InvalidInstallSpec`).
	// +kubebuilder:validation:MaxLength=550
	// +kubebuilder:validation:Pattern=`^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_\-/]{1,512}$`
	// +optional
	ControllerRoleARN string `json:"controllerRoleARN,omitempty"`
}

// KarpenterNodeClassRef references the provider NodeClass the NodePool launches nodes from (the
// Karpenter v1 `nodeClassRef` group/kind/name shape).
type KarpenterNodeClassRef struct {
	// Group of the provider NodeClass API. Default `karpenter.k8s.aws` (EKS); use `karpenter.azure.com`
	// on AKS NAP.
	// +kubebuilder:default="karpenter.k8s.aws"
	// +optional
	Group string `json:"group,omitempty"`

	// Kind of the provider NodeClass. Default `EC2NodeClass` (EKS); use `AKSNodeClass` on AKS NAP.
	// +kubebuilder:default="EC2NodeClass"
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the platform-owned NodeClass. Required when `karpenter.enabled`.
	// +optional
	Name string `json:"name,omitempty"`
}

// KarpenterLimits caps the aggregate resources the Zetta NodePool may provision.
type KarpenterLimits struct {
	// CPU cap across the pool (Karpenter DecimalSI, e.g. "16"). Default "16".
	// +kubebuilder:default="16"
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory cap across the pool (Karpenter BinarySI, e.g. "64Gi"). Default "64Gi".
	// +kubebuilder:default="64Gi"
	// +optional
	Memory string `json:"memory,omitempty"`

	// Nodes caps the node count of the pool — the hard ceiling on elastic scale-out. Default 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	Nodes int32 `json:"nodes,omitempty"`
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

// OramaZettaModel is one model checkpoint placed on each node's data volume (spec.models).
type OramaZettaModel struct {
	// Name is the checkpoint's directory under /data/models.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Image is the model image: the checkpoint files under /model, and /bin/sh. Use a tag that is
	// never moved, or a digest; the copy on the volume is replaced only when this value changes.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
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
	// ProvisionedNodes is the number of k8s nodes the operator-managed Karpenter NodePool currently
	// backs this cluster with (nodes carrying `karpenter.sh/nodepool=<pool>`). 0 when elastic capacity
	// is off or no autoscaler node has been provisioned yet. Surfaced with the `CapacityReady` condition.
	// +optional
	ProvisionedNodes int32 `json:"provisionedNodes,omitempty"`
	// Reachability echoes the EFFECTIVE reachability type of this slice (spec.reachability, forced to
	// `dial-out-only` when spec.ephemeral is set) — the node-reachability model (oramasearch/zetta#739).
	// +optional
	Reachability string `json:"reachability,omitempty"`
	// Ephemeral echoes spec.ephemeral — this slice is burst/autoscaled capacity expected to churn.
	// +optional
	Ephemeral bool `json:"ephemeral,omitempty"`
	// WanAddress is the routable rendezvous address (`host:port`) siblings dial OUT to when this slice is
	// wan-accessible. It is the address siblings put in their `spec.externalPeers`, and the Cloudflare LB
	// origin pool (operator#73) is populated from the wan-accessible set's addresses (its HTTP origin is
	// the same host at :4800). Empty for a dial-out-only slice, or until the address is known (e.g. a
	// pending cloud LB IP).
	// +optional
	WanAddress string `json:"wanAddress,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// IgnoredEnvVars lists spec.env names that conflict with operator-managed variables. The list is
	// sorted and de-duplicated so a steady collision does not cause status writes.
	// +optional
	IgnoredEnvVars []string `json:"ignoredEnvVars,omitempty"`
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
//+kubebuilder:printcolumn:name="Provisioned",type=string,JSONPath=`.status.provisionedNodes`,priority=1
//+kubebuilder:printcolumn:name="Reachability",type=string,JSONPath=`.status.reachability`
//+kubebuilder:printcolumn:name="WAN",type=string,JSONPath=`.status.wanAddress`,priority=1
//+kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`

// OramaZettaCluster deploys a headless Orama Zetta cluster (a StatefulSet of equal-peer CRDT nodes)
// from one CR. Peer replication is TCP 7777 (always mTLS — every node shares one operator-minted
// cluster join token that derives the cluster CA, operator#92); the HTTP API (MCP + REST + serving)
// is 4800; each node persists to a per-node PVC at /data. See the plan of record in
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
