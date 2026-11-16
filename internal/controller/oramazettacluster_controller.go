/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Zetta headless ports (source-verified — see docs/projects/one-orama/zetta-headless-reference.md).
const (
	zettaPeerPort = 7777 // peer replication + signal plane + blob transfer (always mTLS — shared join token)
	zettaAPIPort  = 4800 // MCP + REST + model/index serving (one HTTP server)
	zettaDataDir  = "/data"

	// The shared cluster join token, mounted from Secret <name>-zetta-cluster as a read-only file
	// (oramasearch/zetta#770). The engine reads ZETTA_CLUSTER_TOKEN_FILE in preference to the
	// ZETTA_CLUSTER_TOKEN variable, so the token need not sit in the process environment (readable
	// through /proc/<pid>/environ and inherited by every child the node runs).
	zettaClusterTokenVolume    = "cluster-token"
	zettaClusterTokenMountPath = "/run/secrets/zetta-cluster"
	zettaClusterTokenFile      = zettaClusterTokenMountPath + "/token"
	// 0440: owner root, group fsGroup (10001, the zetta user's group), no access for others.
	zettaClusterTokenFileMode int32 = 0o440

	// The connector-credential root secret (operator#1166, zetta#1314), mounted from Secret
	// <name>-zetta-connection-root (key root) as a read-only file outside the data volume. Zetta
	// seals every saved connection's credential under it, so a copy of /data alone opens none of
	// them. The engine refuses a file inside --data-dir, one writable by group or others, and a
	// secret shorter than 32 characters; 0440 (root, and fsGroup 10001 the zetta user) and 64 hex
	// characters satisfy all three.
	zettaConnectionRootVolume    = "connection-root"
	zettaConnectionRootMountPath = "/run/secrets/zetta-connection-root"
	zettaConnectionRootKey       = "root"
	zettaConnectionRootFile      = zettaConnectionRootMountPath + "/" + zettaConnectionRootKey
	zettaConnectionRootFileEnv   = "ZETTA_CONNECTION_ROOT_SECRET_FILE"
	zettaConnectionRootSuffix    = "-zetta-connection-root"
)

var zettaManagedEnvNames = map[string]struct{}{
	"HOST_IP":                  {},
	"POD_NAME":                 {},
	"ZETTA_ADMIN_TOKEN":        {},
	"ZETTA_ADVERTISE_MAP":      {},
	"ZETTA_CLUSTER_TOKEN":      {},
	"ZETTA_CLUSTER_TOKEN_FILE": {},
	"ZETTA_HOST_NETWORK":       {},
	zettaSeedEnv:               {},
	// The engine's own provisioning env (src/main.cpp, #749): ZETTA_SEED_PEER adds --peer seeds and
	// ZETTA_DATA_DIR moves the data dir. The operator owns both (peers are validated, /data is the PVC), so
	// spec.env may not set them (operator#279).
	"ZETTA_SEED_PEER": {},
	"ZETTA_DATA_DIR":  {},
	// The enrollment CA (operator#1174, zetta_identity.go): its key and seed are the operator's.
	zettaClusterCAEnv:         {},
	zettaClusterCASeedFileEnv: {},
	// Named egress through the workspace gateway (operator#1179, lake_zetta_egress_gateway.go).
	zettaEgressGatewayEnv:       {},
	zettaEgressGatewayRoutesEnv: {},
}

// zettaCustomEnv returns accepted spec.env entries in authored order and a stable list of names the
// operator ignored because it owns their values. The same function drives the pod and status so the two
// cannot disagree.
func zettaCustomEnv(env []corev1.EnvVar) ([]corev1.EnvVar, []string) {
	accepted := make([]corev1.EnvVar, 0, len(env))
	ignoredSet := make(map[string]struct{})
	for _, variable := range env {
		if _, reserved := zettaManagedEnvNames[variable.Name]; reserved {
			ignoredSet[variable.Name] = struct{}{}
			continue
		}
		accepted = append(accepted, variable)
	}
	ignored := make([]string, 0, len(ignoredSet))
	for name := range ignoredSet {
		ignored = append(ignored, name)
	}
	slices.Sort(ignored)
	return accepted, ignored
}

// OramaZettaClusterReconciler deploys a headless Orama Zetta cluster (StatefulSet of equal-peer CRDT
// nodes) from an OramaZettaCluster CR. Step 1 of One Orama: it only RUNS Zetta
// headless — no operator↔Zetta symbiosis and no changes to Zetta's MCP service.
type OramaZettaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader is the manager's uncached reader (mgr.GetAPIReader()). Used for the elastic-capacity
	// node count so the operator doesn't hold a cluster-wide Node informer just for a status field. May be
	// nil under `make run`/tests — reader() then falls back to the cached client.
	APIReader client.Reader
	// stateCache is the anti-flap memory for the operator-state sync into Zetta: component key →
	// last-synced fingerprint or a partial component/event append, so a steady stack produces zero
	// Zetta writes and a partial failure retries only the missing append (see zetta_statesync.go).
	stateCache sync.Map
	// topologyCache is the anti-flap memory for the cross-cluster topology sync (operator#105): topology
	// node key → last-written fingerprint, so a steady wan slice appends no new orama_control.topology rows
	// (see zetta_topology.go).
	topologyCache sync.Map
	// operatorID{Once,Value} memoize this operator's unique identity (one per k8s cluster) so its state
	// stays distinct in a Zetta cluster shared across k8s clusters.
	// droppedPeersLog remembers, per cluster, the last set of invalid topology peers that was logged, so a
	// steady hostile row is logged once per change rather than every reconcile (operator#279).
	droppedPeersLog sync.Map
	operatorIDOnce  sync.Once
	operatorIDValue string
	// OperatorNamespace is the manager pod's namespace (POD_NAMESPACE). Only an OramaZettaCluster in it
	// may own the cluster-wide Karpenter install (operator#280); a tenant's CR, in its own namespace,
	// is refused.
	OperatorNamespace string
	// Clientset reads the features Job's pod log (`zetta --help`, zetta_egress_file.go). PodLogs
	// replaces that read in tests. With neither, no image is asked and every image keeps
	// ZETTA_EGRESS_ALLOW in its environment.
	Clientset kubernetes.Interface
	PodLogs   func(ctx context.Context, namespace, pod string) (string, error)
	// ZettaNodeURL replaces node 0's in-cluster API address in tests (zetta_identity.go).
	ZettaNodeURL func(z *oramav1alpha1.OramaZettaCluster) string
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s/status,verbs=get
//+kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
//+kubebuilder:rbac:groups=orama.cloud,resources=lakeenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// The features Job's `zetta --help` is read from its pod log (zetta_egress_file.go).
//+kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// Zetta data PVCs of an absent Lake service are deleted by the services poll (lake_services.go).
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;delete
// ORAMA_OPERATOR_ID (operator#106): a single uncached GET of the kube-system Namespace for its UID — `get`
// only, never list/watch, so the operator holds no cluster-wide Namespace informer.
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=cloudflarednsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// Elastic capacity (operator#69): own the Karpenter NodePool + read the provider NodeClasses/NodeClaims
// + count provisioned nodes. karpenter.sh/karpenter.k8s.aws/karpenter.azure.com may be absent (the
// reconciler no-ops when the CRD is missing) — the grants are harmless when the groups don't exist.
//+kubebuilder:rbac:groups=karpenter.sh,resources=nodepools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.aws,resources=ec2nodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// Operator-owned Karpenter install (operator#75): server-side-apply the Karpenter CRDs + the
// controller's ServiceAccount/RBAC/Deployment/Service/PDB. Creating RBAC that grants Karpenter's (broad)
// permissions requires the operator hold `bind` + `escalate` on roles/bindings (RBAC
// privilege-escalation prevention) since the operator doesn't itself carry every node/pod grant. CRDs
// are applied but never deleted (no `delete` verb — deleting a CRD cascades to all NodePools/NodeClaims).
// Every write, bind and escalate is scoped by resourceNames to the vendored bundle's objects
// (karpenterControllerObjects, karpenterCRDNames), so the operator cannot create or bind any other role,
// binding or CRD (operator#79). Reads stay unscoped.
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=create;update;patch,resourceNames=ec2nodeclasses.karpenter.k8s.aws;nodeclaims.karpenter.sh;nodeoverlays.karpenter.sh;nodepools.karpenter.sh
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=create;update;patch;delete;bind;escalate,resourceNames=karpenter;karpenter-admin;karpenter-core
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create;update;patch;delete,resourceNames=karpenter;karpenter-core
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;update;patch;delete;bind;escalate,resourceNames=karpenter;karpenter-dns
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;update;patch;delete,resourceNames=karpenter;karpenter-dns

func zettaLabels(z *oramav1alpha1.OramaZettaCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "orama-zetta",
		"app.kubernetes.io/instance":   z.Name,
		"app.kubernetes.io/component":  "zetta-node",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
	}

}

func zettaPodLabels(z *oramav1alpha1.OramaZettaCluster) map[string]string {
	labels := zettaLabels(z)
	// LakeApp egress is label-scoped to the exact managed service. Preserve only the Lake service
	// identity labels on pods. The StatefulSet selector deliberately remains zettaLabels because it
	// is immutable after cluster creation.
	for _, key := range []string{serviceLakeLabel, serviceIDLabel, serviceKindLabel, serviceCustomerLabel, workspaceIDLabel, workspaceLabel, lakeInstanceLabel} {
		if value := z.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	return labels
}

func zettaNames(z *oramav1alpha1.OramaZettaCluster) (sts, headlessSvc, clientSvc string) {
	return z.Name + "-zetta", z.Name + "-zetta-headless", z.Name + "-zetta"
}

// zettaReachability returns the EFFECTIVE node-reachability type of a slice (oramasearch/zetta#739) —
// the single source of truth the reconciler + status share. An ephemeral slice is always dial-out-only
// (burst/autoscaled capacity must never be advertised as a rendezvous — its address would flap), and an
// unset value defaults to dial-out-only (a private node is always the initiator).
func zettaReachability(z *oramav1alpha1.OramaZettaCluster) string {
	if z.Spec.Ephemeral {
		return oramav1alpha1.ReachabilityDialOutOnly
	}
	if z.Spec.Reachability == oramav1alpha1.ReachabilityWANAccessible {
		return oramav1alpha1.ReachabilityWANAccessible
	}
	return oramav1alpha1.ReachabilityDialOutOnly
}

// zettaWANAddress resolves the routable rendezvous address the operator advertises for a WAN-ACCESSIBLE
// slice — `host:port` on the peer port. This is the value siblings copy into their spec.externalPeers,
// and the Cloudflare LB origin pool (operator#73) is populated from the wan-accessible set (its HTTP
// origin is the same host at :4800). Empty for a dial-out-only slice, or until the address is known (a
// pending cloud LB IP, an unscheduled hostNetwork pod). Resolution order: an explicit spec.wanAddress
// wins (a mesh-VPN address / stable public IP / DNS name); else the peer LoadBalancer's assigned public
// address (spec.exposePeerLoadBalancer); else node-0's host IP (spec.hostNetwork).
//
// NOTE: once Zetta grows a `--advertise HOST:PORT` flag (oramasearch/zetta#739 request #1) the operator
// will pass this address as --advertise so a NAT'd/LB'd node gossips it instead of its bound address;
// until then wan-accessibility relies on hostNetwork (bound address == routable address) or the LB path.
// zettaWANAddresses resolves the routable per-node advertise addresses for a WAN-ACCESSIBLE slice — one
// entry per replica, IN ORDINAL ORDER (index i = node i) — so every PERMANENT node advertises its OWN
// address and cross-cluster sync spreads across all their uplinks (aggregate bandwidth) rather than one
// shared LB (oramasearch/zetta#739). An entry is "" while its address is still pending (LB IP unassigned).
// An explicit spec.wanAddress override applies to every node (one stable VPN/DNS rendezvous).
func (r *OramaZettaClusterReconciler) zettaWANAddresses(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, peerLBName, stsName string) []string {
	// Every resolved address is validated before it is advertised or written to the shared topology
	// (operator#279): an invalid one (a bad spec.wanAddress, an odd LB status) is treated as pending ("").
	out := r.resolveZettaWANAddresses(ctx, z, peerLBName, stsName)
	for i, a := range out {
		if a != "" && validateZettaPeerAddress(a) != nil {
			out[i] = ""
		}
	}
	return out
}

func (r *OramaZettaClusterReconciler) resolveZettaWANAddresses(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, peerLBName, stsName string) []string {
	if zettaReachability(z) != oramav1alpha1.ReachabilityWANAccessible {
		return nil
	}
	replicas := z.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	out := make([]string, replicas)
	// 1. Explicit override — authoritative; every node advertises the same stable address.
	if a := strings.TrimSpace(z.Spec.WanAddress); a != "" {
		for i := range out {
			out[i] = withPeerPort(a)
		}
		return out
	}
	// 2. Per-node peer LoadBalancers (cloud-provisioned) — one public IP per node, keyed by ordinal.
	if z.Spec.ExposePeerLoadBalancer {
		for ord := int32(0); ord < replicas; ord++ {
			var lb corev1.Service
			if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: fmt.Sprintf("%s-%d", peerLBName, ord)}, &lb); err == nil {
				for _, ing := range lb.Status.LoadBalancer.Ingress {
					if ing.Hostname != "" {
						out[ord] = withPeerPort(ing.Hostname)
						break
					}
					if ing.IP != "" {
						out[ord] = withPeerPort(ing.IP)
						break
					}
				}
			}
		}
		return out
	}
	// 3. hostNetwork — each node's own host IP (the k8s node's routable address).
	if z.Spec.HostNetwork {
		for ord := int32(0); ord < replicas; ord++ {
			var pod corev1.Pod
			if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: fmt.Sprintf("%s-%d", stsName, ord)}, &pod); err == nil && pod.Status.HostIP != "" {
				out[ord] = withPeerPort(pod.Status.HostIP)
			}
		}
		return out
	}
	return out
}

// zettaWANAddress returns the resolved wan addresses as a comma-joined list (pending entries dropped):
// this is .status.wanAddress and exactly what a SIBLING slice copies into spec.externalPeers so each of
// its nodes dials all of ours (spreading the cross-cluster load across every permanent node's uplink).
func (r *OramaZettaClusterReconciler) zettaWANAddress(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, peerLBName, stsName string) string {
	var resolved []string
	for _, a := range r.zettaWANAddresses(ctx, z, peerLBName, stsName) {
		if a != "" {
			resolved = append(resolved, a)
		}
	}
	return strings.Join(resolved, ",")
}

// withPeerPort appends the Zetta peer port to a bare host, leaving an already-`host:port` address alone
// (and bracketing a bare IPv6 literal so the result is a valid dial target).
func withPeerPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // already host:port (or [ipv6]:port)
	}
	if strings.HasPrefix(addr, "[") { // bracketed IPv6, no port
		return fmt.Sprintf("%s:%d", addr, zettaPeerPort)
	}
	if strings.Count(addr, ":") >= 2 { // bare IPv6 literal — bracket it
		return fmt.Sprintf("[%s]:%d", addr, zettaPeerPort)
	}
	return fmt.Sprintf("%s:%d", addr, zettaPeerPort)
}

func (r *OramaZettaClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var z oramav1alpha1.OramaZettaCluster
	if err := r.Get(ctx, req.NamespacedName, &z); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: release the cluster-scoped capacity finalizers before k8s GC removes the CR + its owned
	// children — neither the Karpenter NodePool nor the operator-owned Karpenter install (CRDs,
	// ClusterRoles) can be owner-GC'd by a namespaced CR. Tear the NodePool down FIRST so Karpenter (if
	// this CR also owns the install) can still process the NodeClaim termination before its controller goes.
	if !z.DeletionTimestamp.IsZero() {
		if err := r.finalizeZettaCapacity(ctx, &z); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.finalizeKarpenterInstall(ctx, &z); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	stsName, headlessName, clientName := zettaNames(&z)
	lbls := zettaLabels(&z)

	// 0. Karpenter install (operator#75) — when spec.capacity.karpenter.install is set, OWN the Karpenter
	//    controller + CRDs (pinned upstream manifests, server-side-applied). Reconciled BEFORE the
	//    NodePool so the CRDs exist first. Cloud-gated: a clean no-op on bare-metal/k0s and on AKS (NAP
	//    manages Karpenter); on EKS it surfaces KarpenterInstalled and blocks (not crash-loops) if the
	//    cloud IAM identity is missing.
	installCond, err := r.reconcileKarpenterInstall(ctx, &z)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 0.5 Elastic capacity (operator#69) — when enabled, own a Karpenter NodePool the Zetta pods pin
	//    onto, so raising spec.replicas past node capacity provisions new nodes. Reconciled before the
	//    (possibly Pending) pods below. Graceful no-op if Karpenter is absent (RESTMapper check).
	capCond, provisionedNodes, err := r.reconcileZettaCapacity(ctx, &z)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 1. Headless Service — stable per-pod DNS for peer discovery (<sts>-0.<headless>.<ns>.svc:7777).
	headless := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, headless, func() error {
		headless.Labels = lbls
		headless.Spec.ClusterIP = corev1.ClusterIPNone
		headless.Spec.PublishNotReadyAddresses = true // peers must resolve each other before they're Ready
		headless.Spec.Selector = lbls
		headless.Spec.Ports = []corev1.ServicePort{
			{Name: "peer", Port: zettaPeerPort, TargetPort: intstr.FromInt32(zettaPeerPort)},
			{Name: "api", Port: zettaAPIPort, TargetPort: intstr.FromInt32(zettaAPIPort)},
		}
		return controllerutil.SetControllerReference(&z, headless, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. Client Service — the HTTP API (MCP + REST + serving) on 4800.
	clientSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: clientName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, clientSvc, func() error {
		clientSvc.Labels = lbls
		clientSvc.Spec.Selector = lbls
		clientSvc.Spec.Ports = []corev1.ServicePort{
			{Name: "api", Port: zettaAPIPort, TargetPort: intstr.FromInt32(zettaAPIPort)},
		}
		return controllerutil.SetControllerReference(&z, clientSvc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2.55 Admin token — UNCONDITIONAL. zetta#799 made the engine's auth MANDATORY and REMOVED the
	//      `--require-auth`/`--mcp-token`/`--cluster-plaintext` flags: every node now ALWAYS gates its
	//      HTTP/MCP/SQL surface on a bearer that resolves to a user. A fresh node auto-generates an admin
	//      credential the operator can't read, so the operator MUST seed a KNOWN one: it mints a 64-hex
	//      admin token once into Secret `<name>-zetta-admin` (key `token`), injects it as
	//      `ZETTA_ADMIN_TOKEN` (env; the engine seeds the owner from it), and authenticates its OWN Zetta
	//      SQL client with it (zetta_statesync.go / zetta_controlplane.go) — otherwise the operator's SQL
	//      sync 401s against the auth-on engine. Provisioned before the StatefulSet so its secretKeyRef
	//      resolves; minted once, never rotated. spec.requireAuth / spec.mcpTokenAuth are now deprecated
	//      no-ops (auth is always enforced — see the CRD types).
	if err := r.reconcileZettaAdminToken(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 2.57 Cluster join token — UNCONDITIONAL (the mTLS cluster identity, operator#92). Zetta derives the
	//      cluster CA deterministically from a shared join token, so every node MUST hold the SAME token or
	//      each derives a different CA and forms a cluster-of-one. The operator mints it once into
	//      `<name>-zetta-cluster` and injects it into EVERY node as ZETTA_CLUSTER_TOKEN. Provisioned before
	//      the StatefulSet so its secretKeyRef resolves. Never deleted — it IS the cluster's durable identity.
	if err := r.reconcileZettaClusterToken(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 2.58 Connector-credential root secret — UNCONDITIONAL (operator#1166, zetta#1314). Zetta seals
	//      saved connector credentials under it instead of a key derived from the join token it keeps in
	//      /data, so a copy of the data volume opens no stored source credential. Minted once into
	//      `<name>-zetta-connection-root` and mounted on every node before the StatefulSet references it.
	if err := r.reconcileZettaConnectionRoot(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 2.6 Peer LoadBalancer — OPT-IN (spec.exposePeerLoadBalancer) and ONLY for a wan-accessible slice.
	//     A cloud LB fronting the peer port makes a GENUINELY wan-accessible slice reachable from other
	//     clusters (its public IP → siblings' spec.externalPeers + this slice's .status.wanAddress).
	//     Corrected mesh model (oramasearch/zetta#739): a dial-out-only/ephemeral/private node must DIAL
	//     OUT to a rendezvous, never accept inbound — so on such a slice this is IGNORED (with a warning)
	//     and any previously-created LB is torn down. No-op unless set AND wan-accessible.
	reachability := zettaReachability(&z)
	peerLBName := z.Name + "-zetta-peer"
	wantPeerLB := z.Spec.ExposePeerLoadBalancer && reachability == oramav1alpha1.ReachabilityWANAccessible
	if z.Spec.ExposePeerLoadBalancer && !wantPeerLB {
		logf.FromContext(ctx).Info("ignoring spec.exposePeerLoadBalancer on a non-wan-accessible slice: a "+
			"dial-out-only/ephemeral node must DIAL OUT to a wan-accessible rendezvous, not accept an inbound "+
			"endpoint (node-reachability model, oramasearch/zetta#739) — set spec.reachability=wan-accessible to expose it",
			"reachability", reachability, "ephemeral", z.Spec.Ephemeral)
	}
	// Per-node peer LBs: each PERMANENT wan-accessible node gets its OWN public IP so cross-cluster CRDT
	// sync spreads across ALL their uplinks (aggregate bandwidth) instead of funnelling through one shared
	// LB (a bottleneck + SPOF). Each LB selects exactly one StatefulSet pod by its stable pod-name label,
	// and the node advertises its own LB address (--advertise, below) so external peers dial it directly
	// rather than an unreachable pod IP (oramasearch/zetta#739). A legacy single shared LB is torn down.
	peerReplicas := z.Spec.Replicas
	if peerReplicas < 1 {
		peerReplicas = 1
	}
	if wantPeerLB {
		for ord := int32(0); ord < peerReplicas; ord++ {
			lbName := fmt.Sprintf("%s-%d", peerLBName, ord)
			podSel := map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("%s-%d", stsName, ord)}
			for k, v := range lbls {
				podSel[k] = v
			}
			lb := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: lbName, Namespace: z.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, lb, func() error {
				lb.Labels = lbls
				lb.Spec.Type = corev1.ServiceTypeLoadBalancer
				lb.Spec.Selector = podSel // exactly one pod → one public IP per permanent node
				lb.Spec.Ports = []corev1.ServicePort{
					{Name: "peer", Port: zettaPeerPort, TargetPort: intstr.FromInt32(zettaPeerPort)},
				}
				return controllerutil.SetControllerReference(&z, lb, r.Scheme)
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Retire the legacy shared LB and any per-ordinal LBs above the current replica count (scale-down).
		_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: peerLBName, Namespace: z.Namespace}})
		for ord := peerReplicas; ord < peerReplicas+16; ord++ {
			_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", peerLBName, ord), Namespace: z.Namespace}})
		}
	} else {
		_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: peerLBName, Namespace: z.Namespace}})
		for ord := int32(0); ord < peerReplicas+16; ord++ {
			_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", peerLBName, ord), Namespace: z.Namespace}})
		}
	}

	// 2.9 Cross-cluster topology THROUGH Zetta (operator#105) — write this slice's wan-accessible endpoints
	//     into orama_control.topology and read the cross-cluster set back to DERIVE the sibling slices'
	//     --peer addresses, so peering self-forms instead of an admin hand-copying .status.wanAddress into
	//     each sibling's spec.externalPeers. Gated on the datastore symbiosis (same switch as the state
	//     sync); best-effort (never fails the reconcile — the node HTTP server is flaky, zetta#731). Runs
	//     AFTER the per-node peer LBs (so their addresses resolve) and BEFORE the StatefulSet (so the derived
	//     peers feed its --peer rendering). syncEnabled is reused by the control-plane/state-sync step below.
	syncEnabled := z.Spec.ProvisionOperatorSchema == nil || *z.Spec.ProvisionOperatorSchema
	var derivedPeers, droppedPeers []string
	topologyRead := true
	if syncEnabled {
		derivedPeers, droppedPeers, topologyRead = r.reconcileZettaTopology(ctx, &z, clientName, peerLBName, stsName)
	}

	// 2.95 Peer address validation (operator#279). An invalid spec.externalPeers / spec.wanAddress (a CR
	//      written before the CRD pattern, or one the pattern missed) is REFUSED: the StatefulSet below is
	//      left exactly as it was, and PeerAddressesValid=False names the bad values. Invalid topology peers
	//      were already dropped by reconcileZettaTopology and are only reported. When the topology could not
	//      be read this round the condition is left as it is, so a flaky read does not flap it (a stale
	//      InvalidSpecPeerAddress is still cleared once the spec is valid).
	specInvalid := invalidSpecPeerAddresses(&z)
	peerCond := peerAddressesConditionFor(&z, specInvalid, droppedPeers, topologyRead)
	if len(specInvalid) > 0 {
		logf.FromContext(ctx).Info("refusing invalid peer addresses; the StatefulSet is left unchanged", "invalid", specInvalid)
	}

	// 3. StatefulSet — N equal-peer headless nodes, each with a per-node PVC at /data.
	// Build the POSITIONAL per-node advertise map (comma-joined; field i = node i, "" while pending) so
	// each PERMANENT wan-accessible node gossips ITS OWN routable address (--advertise) instead of its
	// unreachable bound pod IP — otherwise external peers learn a 10.244.x and dial it forever
	// (oramasearch/zetta#739). The resolved subset becomes .status.wanAddress below. Recomputed each
	// reconcile; a later LB IP just rolls the pods once.
	//
	// The egress allowlist file (operator#724, zetta#1332): spec.egressAllowConfigMap is mounted, and
	// the image's own `zetta --help` decides whether it reads it or keeps ZETTA_EGRESS_ALLOW. While the
	// named ConfigMap does not exist yet the StatefulSet is left as it is (egress.hold).
	//
	// The image's answer is read for every cluster: the identity root rotation and the enrollment CA
	// (operator#1174, zetta_identity.go) follow it too. A new cluster waits for it (identity.hold), so
	// it is created with a CA seed when its image reads one.
	features, featuresRequeue, err := r.reconcileZettaImageFeatures(ctx, &z)
	if err != nil {
		return ctrl.Result{}, err
	}
	egress, err := r.reconcileZettaEgressFileWith(ctx, &z, features, featuresRequeue)
	if err != nil {
		return ctrl.Result{}, err
	}
	identity, err := r.reconcileZettaIdentity(ctx, &z, features, stsName, headlessName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(specInvalid) == 0 && !egress.hold && !identity.hold {
		advertiseMap := strings.Join(r.zettaWANAddresses(ctx, &z, peerLBName, stsName), ",")
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
			return r.mutateZettaStatefulSetWith(&z, sts, headlessName, stsName, lbls, advertiseMap, derivedPeers, egress.render, identity.render)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Optional external exposure — Ingress (+ proxied Cloudflare DNS) for the HTTP API/UI on 4800
	//    (e.g. zetta.example.net). No-op unless spec.host is set.
	if err := r.reconcileZettaIngress(ctx, &z, clientName, lbls); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Operator datastore (One Orama symbiosis v0) — this cluster as the operator's store. Provisions
	//    the orama_control + orama_data schema, then coarsely syncs the operator's OBSERVED state into
	//    orama_control.components (anti-flap; default on). syncEnabled was computed at step 2.9 above.
	if syncEnabled {
		if err := r.reconcileZettaControlPlane(ctx, &z, clientName); err != nil {
			return ctrl.Result{}, err
		}
		r.reconcileOperatorStateSync(ctx, &z, clientName) // best-effort; never fails the reconcile
		r.reconcileZettaDeployments(ctx, &z, clientName)  // best-effort; the periodic poll retries
	} else if err := r.teardownZettaControlPlane(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Status — readiness roll-up + the capacity/install conditions/count and the slice's reachability
	//    type + (for a wan-accessible slice) the routable rendezvous address (write only on change; anti-flap).
	wanAddress := r.zettaWANAddress(ctx, &z, peerLBName, stsName)
	if err := r.reconcileZettaStatus(ctx, &z, stsName, capCond, installCond, peerCond, provisionedNodes, reachability, wanAddress, egress.features, identity); err != nil {
		return ctrl.Result{}, err
	}
	// When acting as the datastore, poll at the shortest Lake deploySync interval in this namespace.
	// The same tick keeps coarse state sync current; both write paths are anti-flapped.
	var requeue time.Duration
	if syncEnabled {
		requeue = r.deploymentSyncInterval(ctx, z.Namespace)
	}
	for _, next := range []time.Duration{egress.requeue, identity.requeue} {
		if next > 0 && (requeue == 0 || next < requeue) {
			requeue = next
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileZettaAdminToken ensures a Secret holding a 64-char (32-byte hex) ADMIN token exists for this
// cluster. UNCONDITIONAL since zetta#799 made the engine's auth mandatory: the token is injected into the
// node (env ZETTA_ADMIN_TOKEN, read by the engine — never a flag), which seeds it as the owner principal, AND it is the
// credential the operator's OWN Zetta SQL client authenticates with (a fresh node otherwise auto-generates
// an admin credential the operator can't read, and the auth-on engine 401s an unauthenticated client).
// Idempotent — minted once and PRESERVED across reconciles (never rotated); the value is never logged
// (the entrypoint echo redacts it). Provisioned before the StatefulSet so its secretKeyRef resolves.
func (r *OramaZettaClusterReconciler) reconcileZettaAdminToken(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	name := z.Name + "-zetta-admin"
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = zettaLabels(z)
		if len(sec.Data["token"]) == 0 { // mint once; preserve on later reconciles
			token, gerr := randHex(32) // 32 bytes → 64 hex chars
			if gerr != nil {
				return gerr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data["token"] = []byte(token)
		}
		return controllerutil.SetControllerReference(z, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("provisioned admin token — the node seeds it as the owner and the operator's SQL client authenticates with it (auth is mandatory, zetta#799)",
			"component", "zetta-admin", "secret", name,
			"retrieve", fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.token}' | base64 -d", name, z.Namespace))
	}
	return nil
}

// reconcileZettaClusterToken ensures the Secret holding this cluster's shared mTLS join token exists.
// The join token IS the cluster identity: Zetta derives the cluster CA deterministically from it
// (SHA256(token ‖ "zetta-cluster-ca-v1"), see the engine's docs/cluster/security.md), so ONLY nodes
// holding the SAME token trust each other. The operator mints it ONCE (32 random bytes → 64 hex) into
// `<name>-zetta-cluster` and injects it into EVERY node as ZETTA_CLUSTER_TOKEN (the engine reads that env
// → --join-token at boot). Without it each node mints its OWN token on first boot, derives a different
// CA, and rejects every peer — so every node forms its own cluster-of-one (operator#92). Mirrors
// reconcileZettaAdminToken (also UNCONDITIONAL + minted-once), with one further difference: it is NEVER
// rotated (a new token re-derives the CA and orphans every existing node cert, splitting the cluster). The
// value is never logged. Provisioned before the StatefulSet so its secretKeyRef resolves.
func (r *OramaZettaClusterReconciler) reconcileZettaClusterToken(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	name := z.Name + "-zetta-cluster"
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = zettaLabels(z)
		if len(sec.Data["token"]) == 0 { // mint once; preserve on later reconciles (rotating orphans node certs)
			token, gerr := randHex(32) // 32 bytes → 64 hex chars
			if gerr != nil {
				return gerr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data["token"] = []byte(token)
		}
		return controllerutil.SetControllerReference(z, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("provisioned shared cluster join token — every node forms ONE mTLS cluster from this CA",
			"component", "zetta-cluster", "secret", name,
			"retrieve", fmt.Sprintf("kubectl get secret %s -n %s -o jsonpath='{.data.token}' | base64 -d", name, z.Namespace))
	}
	return nil
}

// zettaConnectionRootSecretName is the Secret holding a cluster's connector-credential root secret.
func zettaConnectionRootSecretName(z *oramav1alpha1.OramaZettaCluster) string {
	return z.Name + zettaConnectionRootSuffix
}

// reconcileZettaConnectionRoot ensures the Secret holding this cluster's connector-credential root
// secret exists (operator#1166, zetta#1314). Every node of the cluster mounts the same Secret, because
// each node opens what another sealed. The operator mints it ONCE (32 random bytes as 64 hex) and never
// writes the key again while it holds anything: a new value would orphan every sealed credential.
// Rotation is by hand and the engine's own: put the new secret on the first line and keep the old one
// below it; each node re-seals at its next start, and the old line is dropped once every node has
// restarted. The value is never logged.
func (r *OramaZettaClusterReconciler) reconcileZettaConnectionRoot(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	name := zettaConnectionRootSecretName(z)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = zettaLabels(z)
		if len(sec.Data[zettaConnectionRootKey]) == 0 { // mint once; preserve (and any rotation lines) after
			secret, gerr := randHex(32) // 32 bytes → 64 hex chars, above the engine's 32-character minimum
			if gerr != nil {
				return gerr
			}
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data[zettaConnectionRootKey] = []byte(secret)
		}
		return controllerutil.SetControllerReference(z, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("provisioned the connector-credential root secret — saved connections are sealed under it, outside the data volume (zetta#1314)",
			"component", "zetta-connection-root", "secret", name)
	}
	return nil
}

// reconcileZettaIngress exposes the cluster at spec.host via an nginx Ingress that proxies the WHOLE
// host to the Zetta node's own HTTP server on :4800 — root `/` is the node's web dashboard
// (oramasearch/zetta#647), every other path is MCP/REST/serving. When spec.dnsSecretRef is also set it
// creates an OWNED CloudflareDNSRecord (proxied A/CNAME → the ingress LoadBalancer), mirroring the
// OramaCloudV1 dashboard-DNS pattern (the child controller owns the CF API call + finalizer, so the
// operator never mutates Cloudflare from the main loop). SAFE no-op unless spec.ingressEnabled is true
// AND spec.host is set — the bool is the explicit exposure gate, Host names the endpoint.
func (r *OramaZettaClusterReconciler) reconcileZettaIngress(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc string, lbls map[string]string) error {
	ingName := z.Name + "-zetta"

	// The old static "zetta" landing placeholder is retired: it shadowed the bare root and hid the
	// node's real web dashboard, which now works. Root proxies straight to :4800; tear down any
	// placeholder Deployment/Service/ConfigMap a prior operator version created (idempotent).
	landingName := z.Name + "-zetta-landing"
	_ = deleteIfExists(ctx, r.Client, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
	_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
	_ = deleteIfExists(ctx, r.Client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})

	// Origin TLS (operator#578): one Let's Encrypt Issuer per Zetta with a DNS credential, DNS-01
	// through its dnsSecretRef's `token`, as Cloud v1 (#457) and Lake have.
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerIssuerGVK)
	issuer.SetName(zettaOriginIssuerName(z))
	issuer.SetNamespace(z.Namespace)
	if !z.Spec.IngressEnabled || z.Spec.Host == "" {
		// Gate off (ingressEnabled false) or no host → tear down the Ingress + DNS.
		_ = deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: ingName + "-dns", Namespace: z.Namespace}})
		return deleteIfKindExists(ctx, r.Client, issuer)
	}
	// No HTTP-only fallback: a public Zetta needs its DNS credential for its certificate.
	if z.Spec.DNSSecretRef == nil || z.Spec.DNSSecretRef.Name == "" {
		return fmt.Errorf("origin TLS for %s needs spec.dnsSecretRef (key \"token\") for the Let's Encrypt DNS-01 Issuer", z.Spec.Host)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(lbls)
		issuer.Object["spec"] = map[string]any{
			"acme": map[string]any{
				"server":              letsEncryptDirectory,
				"privateKeySecretRef": map[string]any{"name": zettaOriginIssuerName(z) + "-account"},
				"solvers": []any{map[string]any{
					"dns01": map[string]any{
						"cloudflare": map[string]any{
							"apiTokenSecretRef": map[string]any{"name": z.Spec.DNSSecretRef.Name, "key": "token"},
						},
					},
				}},
			},
		}
		return controllerutil.SetControllerReference(z, issuer, r.Scheme)
	}); err != nil {
		return fmt.Errorf("origin TLS issuer: %w", err)
	}

	prefix := networkingv1.PathTypePrefix
	svcBackend := func(name string, port int32) networkingv1.IngressBackend {
		return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: name, Port: networkingv1.ServiceBackendPort{Number: port}}}
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Spec.IngressClassName = strPtr(ingressClassOrDefault(z.Spec.IngressClassName))
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: z.Spec.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					// The whole host proxies to the Zetta node's HTTP server on :4800 — root `/` is the
					// node's web dashboard (zetta#647), every other path is MCP/REST/serving.
					{Path: "/", PathType: &prefix, Backend: svcBackend(clientSvc, zettaAPIPort)},
				},
			}},
		}}
		if err := applyIngressTLS(ing, zettaOriginIssuerName(z)); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(z, ing, r.Scheme)
	}); err != nil {
		return err
	}

	// Proxied Cloudflare DNS (opt-in via dnsSecretRef) → resolves once the ingress LB has an address.
	if z.Spec.DNSSecretRef != nil && z.Spec.DNSSecretRef.Name != "" {
		if target := r.zettaIngressTarget(ctx, z, ingName); target != "" {
			rec := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: ingName + "-dns", Namespace: z.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
				rec.Spec.SecretRef, rec.Spec.Name, rec.Spec.Content, rec.Spec.Proxied = z.Spec.DNSSecretRef.Name, z.Spec.Host, target, true
				return controllerutil.SetControllerReference(z, rec, r.Scheme)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// zettaOriginIssuerName is a public Zetta's cert-manager Issuer.
func zettaOriginIssuerName(z *oramav1alpha1.OramaZettaCluster) string { return z.Name + "-letsencrypt" }

// zettaIngressTarget returns the external address (hostname preferred, else IP) of the cluster's
// Ingress once the ingress controller has assigned one — the Content for the Cloudflare DNS record.
func (r *OramaZettaClusterReconciler) zettaIngressTarget(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, ingName string) string {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: ingName}, ing); err != nil {
		return ""
	}
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" {
			return lb.Hostname
		}
		if lb.IP != "" {
			return lb.IP
		}
	}
	return ""
}

// mutateZettaStatefulSet renders the StatefulSet with no egress allowlist file.
func (r *OramaZettaClusterReconciler) mutateZettaStatefulSet(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet, headlessName, stsName string, lbls map[string]string, advertise string, derivedPeers []string) error {
	return r.mutateZettaStatefulSetWithEgress(z, sts, headlessName, stsName, lbls, advertise, derivedPeers, zettaEgressRender{})
}

// mutateZettaStatefulSetWithEgress renders the StatefulSet, with the egress allowlist file of
// spec.egressAllowConfigMap as reconcileZettaEgressFile decided it.
func (r *OramaZettaClusterReconciler) mutateZettaStatefulSetWithEgress(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet, headlessName, stsName string, lbls map[string]string, advertise string, derivedPeers []string, egress zettaEgressRender) error {
	return r.mutateZettaStatefulSetWith(z, sts, headlessName, stsName, lbls, advertise, derivedPeers, egress, zettaIdentityRender{})
}

// mutateZettaStatefulSetWith renders the StatefulSet with the egress allowlist file and the identity
// render (the admin token generation, ZETTA_CLUSTER_CA and the CA seed, zetta_identity.go).
func (r *OramaZettaClusterReconciler) mutateZettaStatefulSetWith(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet, headlessName, stsName string, lbls map[string]string, advertise string, derivedPeers []string, egress zettaEgressRender, identity zettaIdentityRender) error {
	replicas := z.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	image := zettaImage(z)
	storageSize := z.Spec.Storage.Size
	if storageSize == "" {
		storageSize = "10Gi"
	}

	var pull []corev1.LocalObjectReference
	if z.Spec.PullSecretName != "" {
		pull = []corev1.LocalObjectReference{{Name: z.Spec.PullSecretName}}
	}
	var resources corev1.ResourceRequirements
	if z.Spec.Resources != nil {
		resources = *z.Spec.Resources
	}

	// The seed is node-0's stable DNS. The entrypoint keys off the pod ordinal (POD_NAME = <sts>-<ord>) so
	// node-0 gets no --peer (it is the rendezvous) while peers seed off node-0. EVERY node — node-0
	// included — boots with the SAME shared cluster join token (ZETTA_CLUSTER_TOKEN, injected below): the
	// engine derives the cluster CA from it, so all nodes trust one another and form ONE mTLS cluster
	// (operator#92). Peer traffic is always mTLS — there is no plaintext mode.
	seed := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", stsName, headlessName, z.Namespace, zettaPeerPort)
	// External peers — sibling nodes in OTHER k8s clusters / bare hosts, dialed by EVERY node here so they
	// join the shared (globally-named) Zetta cluster. Zetta is the mesh. The set is the UNION of the explicit
	// spec.externalPeers (authoritative override — wins) and the topology-DERIVED wan endpoints of sibling
	// slices (operator#105, coordinated through orama_control.topology — see zetta_topology.go), deduped.
	// The derived set already excludes this operator's OWN nodes and our own advertised addresses.
	//
	// operator#279: none of these values is ever formatted into shell text. The entrypoint script is the
	// constant zettaEntrypointScript; the node flags and the external peers reach it as container args
	// (positional parameters it only moves with "$@"), and the seed as ORAMA_ZETTA_SEED. See zetta_peers.go.
	//
	// The shared cluster join token (ZETTA_CLUSTER_TOKEN) is NOT passed as a flag: the engine reads that
	// env var directly at boot and maps it to --join-token internally (src/main.cpp provisioning env). We
	// keep it env-only on purpose — an unknown env var is safely ignored by an image that predates the
	// feature, whereas an unknown FLAG would crash the node (as the sandbox image does for unknown flags).
	// The admin token is env-only too (operator#279): ZETTA_ADMIN_TOKEN, which the engine reads when
	// `--admin-token` is absent, seeds the owner. A flag would put the token in the world-readable argv
	// (/proc/<pid>/cmdline); the log line shows only a redacted "****". Tokens are injected via env from their
	// Secrets: ZETTA_CLUSTER_TOKEN (<name>-zetta-cluster, reconcileZettaClusterToken; every node) and
	// ZETTA_ADMIN_TOKEN (<name>-zetta-admin, reconcileZettaAdminToken; every node). zetta#799 made auth
	// MANDATORY and REMOVED `--require-auth`/`--mcp-token`/`--cluster-plaintext` — passing any of them is now
	// an unknown-option error that crashes the node, so the entrypoint renders NONE of them.
	//
	// Node-reachability + node-type flags (zetta#803/#810, zettaNodeFlags) — this is what makes the engine's
	// CRDT node:<id> entry (and thus `system.nodes` + the reachability-aware dialer) reflect the slice's
	// ACTUAL topology instead of the engine's permanent/wan-accessible DEFAULT. reachability and node_type are
	// INDEPENDENT axes: `--dial-out-only` iff the slice's effective reachability is dial-out-only
	// (zettaReachability — the single source of truth shared with status + topology); `--node-type
	// ephemeral` iff spec.ephemeral, else `--node-type permanent`. These are FLAGS (not env), so they require
	// the auth-capable image generation the operator already mandates.
	entryArgs := zettaEntrypointArgs(z, zettaPeerSet(z.Spec.ExternalPeers, derivedPeers))

	zettaEnv := []corev1.EnvVar{
		// The ordinal comes from the POD name — $HOSTNAME is the NODE's name under hostNetwork.
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		// node-0's in-cluster peer address, read by the entrypoint as data (never as script text).
		{Name: zettaSeedEnv, Value: seed},
		// Shared cluster join token → the mTLS cluster identity. Injected into EVERY node (incl. node-0):
		// the engine derives the cluster CA from it, so all nodes trust each other and form ONE cluster.
		// Without a SHARED token each node mints its own → different CA → cluster-of-one (operator#92).
		// oramasearch/zetta#770: the engine reads the token from this read-only file (the Secret volume
		// below) in preference to the environment, refuses to start if the two disagree, and scrubs
		// ZETTA_CLUSTER_TOKEN from its environment once read so no child process inherits it.
		{Name: "ZETTA_CLUSTER_TOKEN_FILE", Value: zettaClusterTokenFile},
		// Kept for Zetta images that predate zetta#770 and ignore ZETTA_CLUSTER_TOKEN_FILE: without it they
		// would mint their own token and split the cluster. Remove once every image in use reads the file
		// (operator#721).
		{Name: "ZETTA_CLUSTER_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-cluster"}, Key: "token"}}},
		// The connector-credential root secret as a file outside /data (operator#1166, zetta#1314). An
		// image that predates zetta#1314 ignores the variable and keeps its old sealing; one that has it
		// and finds no file refuses connector credentials, so the operator rolls before that image.
		{Name: zettaConnectionRootFileEnv, Value: zettaConnectionRootFile},
	}
	if z.Spec.HostNetwork {
		// Pack ordinals onto per-ordinal ports; seed peers off HOST_IP (the node's real address, via the
		// downward API) — NOT 127.0.0.1 — so the dialed address equals node-0's advertised address and
		// Zetta's address-keyed reconnect dedup doesn't leak duplicate links (oramasearch/zetta#739).
		zettaEnv = append(zettaEnv,
			corev1.EnvVar{Name: "ZETTA_HOST_NETWORK", Value: "1"},
			corev1.EnvVar{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		)
	}
	// Admin token → ZETTA_ADMIN_TOKEN on EVERY node (UNCONDITIONAL since zetta#799): the engine reads it from
	// its environment to seed the owner (never a flag — argv is world-readable, operator#279), and the operator's own SQL client authenticates with the same token.
	zettaEnv = append(zettaEnv, corev1.EnvVar{Name: "ZETTA_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-admin"}, Key: "token"}}})
	// Per-node WAN advertise map (positional; field ORD+1 = this node's own routable addr) — a wan node
	// gossips its OWN reachable endpoint, not its unreachable pod IP (operator#104, oramasearch/zetta#739).
	if advertise != "" {
		zettaEnv = append(zettaEnv, corev1.EnvVar{Name: "ZETTA_ADVERTISE_MAP", Value: advertise})
	}
	customEnv, _ := zettaCustomEnv(z.Spec.Env)
	zettaEnv = append(zettaEnv, customEnv...)
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch
	noToken := false
	runAsNonRoot := true
	noPrivilegeEscalation := false

	sts.Labels = lbls
	sts.Spec.ServiceName = headlessName
	sts.Spec.Replicas = &replicas
	sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
	// Bounded, ordered scale-up so node-0 is Ready before peers try to join it.
	pmp := appsv1.ParallelPodManagement
	sts.Spec.PodManagementPolicy = pmp
	sts.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: zettaPodLabels(z)},
		Spec: corev1.PodSpec{
			ImagePullSecrets:             pull,
			AutomountServiceAccountToken: &noToken,
			// The cluster join token as a read-only file (oramasearch/zetta#770), readable by the zetta
			// user through fsGroup and by no one else.
			Volumes: []corev1.Volume{{
				Name: zettaClusterTokenVolume,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  z.Name + "-zetta-cluster",
					Items:       []corev1.KeyToPath{{Key: "token", Path: "token"}},
					DefaultMode: int32Ptr(zettaClusterTokenFileMode),
				}},
			}, {
				// The connector-credential root secret, outside the data volume (operator#1166).
				Name: zettaConnectionRootVolume,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  zettaConnectionRootSecretName(z),
					Items:       []corev1.KeyToPath{{Key: zettaConnectionRootKey, Path: zettaConnectionRootKey}},
					DefaultMode: int32Ptr(zettaClusterTokenFileMode),
				}},
			}},
			// The image runs zetta as uid 10001; fsGroup makes the mounted PVC group-writable so the
			// node can create /data/security (mints its cluster cert/token) etc. Azure/most CSI disks
			// mount root-owned, so a non-root node needs this. OnRootMismatch avoids walking all data
			// after a node move when the volume root already has the expected ownership.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:           int64Ptr(10001),
				RunAsGroup:          int64Ptr(10001),
				RunAsNonRoot:        &runAsNonRoot,
				FSGroup:             int64Ptr(10001),
				FSGroupChangePolicy: &onRootMismatch,
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "zetta",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways, // dev sandbox: mutable :v1-sandbox tag — always re-pull (operator#103)
				// A constant script; every variable value is an arg or an env var (operator#279).
				Command: []string{"/bin/sh", "-c", zettaEntrypointScript},
				Args:    entryArgs,
				Env:     zettaEnv,
				Ports: []corev1.ContainerPort{
					{Name: "peer", ContainerPort: zettaPeerPort},
					{Name: "api", ContainerPort: zettaAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: zettaDataDir},
					{Name: zettaClusterTokenVolume, MountPath: zettaClusterTokenMountPath, ReadOnly: true},
					{Name: zettaConnectionRootVolume, MountPath: zettaConnectionRootMountPath, ReadOnly: true},
				},
				Resources: resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noPrivilegeEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				// Readiness probe intentionally DISABLED (operator#68): the node's HTTP server is flaky
				// (drops ~2/3 of back-to-back requests, zetta#731), so an HTTP/TCP readiness probe keeps
				// flapping the pod out of the Service — starving the schema Job + clients. Without a
				// readiness gate the pod stays a Service endpoint. Restore a real probe (GET /mcp.md)
				// once zetta#731 lands. Liveness (peer port) + startup still guard genuine hangs.
				// Liveness: the peer listener is up.
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
						Port: intstr.FromInt32(zettaPeerPort)}},
					InitialDelaySeconds: 15, PeriodSeconds: 20, TimeoutSeconds: 3, FailureThreshold: 3,
				},
				// Startup: the default preflight rehashes every large-object body — allow a long boot.
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
						Port: intstr.FromInt32(zettaPeerPort)}},
					PeriodSeconds: 10, FailureThreshold: 60,
				},
			}},
		},
	}
	// Per-node PVC at /data — the CRDT doc, node_id (stable identity), certs, and JSONL/large-object data.
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storageSize)}},
		},
	}
	if z.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = &z.Spec.Storage.StorageClassName
	}
	// A StatefulSet's claim templates are immutable: the API server refuses an update that changes
	// them. Once the StatefulSet exists its templates are kept as they are, so a later
	// spec.storage change (a workspace grown by lake's storage_gi, operator#259) never blocks the
	// rest of this reconcile. Existing claims are grown in place by whoever raised the size; a
	// replica added later starts at the template's size.
	if sts.ResourceVersion == "" || len(sts.Spec.VolumeClaimTemplates) == 0 {
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{pvc}
	}

	// The egress allowlist file and its variables (operator#724, zetta_egress_file.go).
	applyZettaEgressRender(&sts.Spec.Template.Spec, egress, z.Spec.Env)
	// The admin token generation, the enrollment CA and its seed (operator#1174, zetta_identity.go).
	applyZettaIdentityRender(z, &sts.Spec.Template, identity)

	// hostNetwork makes :7777/:4800 reachable at the k8s node's own address — the simplest way for a
	// node to be dialable by siblings in other clusters (esp. a single-node cluster).
	if z.Spec.HostNetwork {
		sts.Spec.Template.Spec.HostNetwork = true
		sts.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		// Under hostNetwork a declared containerPort is treated as a hostPort by the scheduler, so every
		// replica would reserve the SAME host ports and only one could schedule. Drop the declarations —
		// each ordinal binds its own ports from the entrypoint flags, and the Services target them numerically.
		sts.Spec.Template.Spec.Containers[0].Ports = nil
		// The liveness/startup probes TCP-check the fixed peer port (:7777), but packed ordinals bind
		// per-ordinal ports (7777+n) — and all packed pods share the host netns, so a :7777 probe would
		// false-pass on a peer or false-fail after a port drift. Drop them under hostNetwork packing (as
		// readiness already is); Zetta peers are equal CRDT nodes and a wrong-port probe does more harm
		// than good here. Restore ordinal-aware probes once the port scheme is authoritative.
		sts.Spec.Template.Spec.Containers[0].LivenessProbe = nil
		sts.Spec.Template.Spec.Containers[0].StartupProbe = nil
	}

	// Elastic capacity (operator#69): pin the Zetta pods onto the operator-owned Karpenter pool. The
	// pods tolerate the dedicated Zetta taint and nodeSelect the pool label, so a replica bump past node
	// capacity leaves them Pending → Karpenter provisions matching Zetta nodes → the pods schedule and
	// join. The do-not-disrupt annotation keeps Karpenter's disruption controller from voluntarily
	// evicting a live CRDT peer (quorum-safety). When capacity is off this is a no-op (pods schedule
	// wherever the scheduler fits them), and clearing it reverts cleanly on the next reconcile.
	if capacityEnabled(z) {
		sts.Spec.Template.Spec.NodeSelector = map[string]string{zettaPoolLabel: z.Name}
		sts.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
			Key: zettaCapacityTaintKey, Operator: corev1.TolerationOpEqual, Value: z.Name, Effect: corev1.TaintEffectNoSchedule,
		}}
		if sts.Spec.Template.Annotations == nil {
			sts.Spec.Template.Annotations = map[string]string{}
		}
		sts.Spec.Template.Annotations[doNotDisruptAnnotation] = "true"
	} else {
		// Revert if capacity was previously enabled then turned off.
		sts.Spec.Template.Spec.NodeSelector = nil
		sts.Spec.Template.Spec.Tolerations = nil
		delete(sts.Spec.Template.Annotations, doNotDisruptAnnotation)
	}

	return controllerutil.SetControllerReference(z, sts, r.Scheme)
}

// reconcileZettaStatus rolls the StatefulSet's readiness up into the CR status, folds in the
// elastic-capacity + Karpenter-install conditions and the provisioned-node count, and records the slice's
// effective node-reachability type and (for a wan-accessible slice) the routable rendezvous address.
// Writes only on a real change (anti-flap — operator principle #7); reachability + wanAddress are computed
// by the caller (zettaReachability / zettaWANAddress) so the reconcile and status agree.
func (r *OramaZettaClusterReconciler) reconcileZettaStatus(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, stsName string, capCond, installCond, peerCond *metav1.Condition, provisionedNodes int32, reachability, wanAddress string, features *oramav1alpha1.ZettaImageFeatures, identity zettaIdentityState) error {
	desired := z.Spec.Replicas
	if desired < 1 {
		desired = 1
	}
	var sts appsv1.StatefulSet
	ready := int32(0)
	var storageCond *metav1.Condition
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: stsName}, &sts); err == nil {
		ready = sts.Status.ReadyReplicas
		storageCond = zettaStorageTemplateCondition(z, &sts)
	}

	phase := "Progressing"
	msg := fmt.Sprintf("%d/%d zetta nodes ready", ready, desired)
	if ready >= desired {
		phase, msg = "Ready", fmt.Sprintf("all %d zetta nodes ready", desired)
	} else if ready == 0 {
		phase = "Progressing"
	}
	if peerCond != nil && peerCond.Reason == "InvalidSpecPeerAddress" {
		phase, msg = "Degraded", "invalid peer address in spec; the StatefulSet is left unchanged (see condition "+peerAddressesConditionType+")"
	}
	_, ignoredEnvVars := zettaCustomEnv(z.Spec.Env)

	changed := z.Status.Phase != phase || z.Status.ReadyNodes != ready ||
		z.Status.TotalNodes != desired || z.Status.Message != msg ||
		z.Status.ProvisionedNodes != provisionedNodes ||
		z.Status.Reachability != reachability || z.Status.Ephemeral != z.Spec.Ephemeral ||
		z.Status.WanAddress != wanAddress ||
		!slices.Equal(z.Status.IgnoredEnvVars, ignoredEnvVars) ||
		!zettaImageFeaturesEqual(z.Status.ImageFeatures, features) ||
		z.Status.Identity != identity.status

	if capCond != nil {
		capCond.ObservedGeneration = z.Generation
		if meta.SetStatusCondition(&z.Status.Conditions, *capCond) {
			changed = true
		}
	} else if meta.RemoveStatusCondition(&z.Status.Conditions, capacityConditionType) {
		changed = true
	}

	if installCond != nil {
		installCond.ObservedGeneration = z.Generation
		if meta.SetStatusCondition(&z.Status.Conditions, *installCond) {
			changed = true
		}
	} else if meta.RemoveStatusCondition(&z.Status.Conditions, installConditionType) {
		changed = true
	}

	if setPeerAddressesCondition(z, peerCond) {
		changed = true
	}
	// IdentityRootRotated exists only on a cluster with spec.identity.adminTokenGeneration (operator#1174).
	if identity.condition != nil {
		if meta.SetStatusCondition(&z.Status.Conditions, *identity.condition) {
			changed = true
		}
	} else if meta.RemoveStatusCondition(&z.Status.Conditions, zettaIdentityConditionType) {
		changed = true
	}

	if storageCond != nil {
		storageCond.ObservedGeneration = z.Generation
		if meta.SetStatusCondition(&z.Status.Conditions, *storageCond) {
			changed = true
		}
	} else if meta.RemoveStatusCondition(&z.Status.Conditions, storageTemplateConditionType) {
		changed = true
	}

	if !changed {
		return nil // no change — don't touch the status subresource (anti-flap)
	}
	z.Status.Phase, z.Status.ReadyNodes, z.Status.TotalNodes, z.Status.Message = phase, ready, desired, msg
	z.Status.ProvisionedNodes = provisionedNodes
	z.Status.Reachability, z.Status.Ephemeral, z.Status.WanAddress = reachability, z.Spec.Ephemeral, wanAddress
	z.Status.IgnoredEnvVars = ignoredEnvVars
	z.Status.ImageFeatures = features
	z.Status.Identity = identity.status
	return r.Status().Update(ctx, z)
}

// zettaImageFeaturesEqual compares two answers, nil included.
func zettaImageFeaturesEqual(a, b *oramav1alpha1.ZettaImageFeatures) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// storageTemplateConditionType reports a spec.storage the StatefulSet's immutable claim template
// does not carry.
const storageTemplateConditionType = "StorageTemplateCurrent"

// zettaStorageTemplateCondition is nil while the StatefulSet's claim template matches spec.storage.
// Otherwise it says the template is unchanged (it is immutable) and who grows the claims: the Lake
// workspace reconciler for a workspace cluster, nobody for any other, whose claims keep their size
// until they are grown by hand.
func zettaStorageTemplateCondition(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet) *metav1.Condition {
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}
	template := sts.Spec.VolumeClaimTemplates[0]
	size := z.Spec.Storage.Size
	if size == "" {
		size = "10Gi"
	}
	wanted, err := resource.ParseQuantity(size)
	current := template.Spec.Resources.Requests[corev1.ResourceStorage]
	class := ""
	if template.Spec.StorageClassName != nil {
		class = *template.Spec.StorageClassName
	}
	if err == nil && wanted.Cmp(current) == 0 && (z.Spec.Storage.StorageClassName == "" || z.Spec.Storage.StorageClassName == class) {
		return nil
	}
	message := fmt.Sprintf("spec.storage (size %s, class %q) differs from the StatefulSet's claim template (size %s, class %q), which is immutable and unchanged; ",
		size, z.Spec.Storage.StorageClassName, current.String(), class)
	if z.Labels[workspaceLabel] != "" {
		message += "the Lake workspace reconciler grows the existing claims in place"
	} else {
		message += "existing claims keep their size until they are grown by hand"
	}
	return &metav1.Condition{
		Type: storageTemplateConditionType, Status: metav1.ConditionFalse,
		Reason: "ClaimTemplateUnchanged", Message: message,
	}
}

func (r *OramaZettaClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.OramaZettaCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.Job{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&oramav1alpha1.CloudflareDNSRecord{}).
		Complete(r)
}
