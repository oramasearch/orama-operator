/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// Zetta headless ports (source-verified — see docs/projects/one-orama/zetta-headless-reference.md).
const (
	zettaPeerPort = 7777 // peer replication + signal plane + blob transfer (always mTLS — shared join token)
	zettaAPIPort  = 4800 // MCP + REST + model/index serving (one HTTP server)
	zettaDataDir  = "/data"

	// Landing placeholder served at the host root. The real Zetta web UI is upstream work
	// (oramasearch/zetta#647); until then the operator serves a static "zetta" page at `/` from a
	// tiny nginx pod that is INDEPENDENT of Zetta's (currently flaky, zetta#700/#695) HTTP server —
	// so the host always resolves to something even when the API is down.
	zettaLandingImage = "nginx:1.27-alpine"
	zettaLandingPort  = 80
	zettaLandingText  = "zetta\n"
)

// OramaZettaClusterReconciler deploys a headless Orama Zetta cluster (StatefulSet of equal-peer CRDT
// nodes) from an OramaZettaCluster CR. Step 1 of One Orama (orama-bridge#130): it only RUNS Zetta
// headless — no operator↔Zetta symbiosis and no changes to Zetta's MCP service.
type OramaZettaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader is the manager's uncached reader (mgr.GetAPIReader()). Used for the elastic-capacity
	// node count so the operator doesn't hold a cluster-wide Node informer just for a status field. May be
	// nil under `make run`/tests — reader() then falls back to the cached client.
	APIReader client.Reader
	// stateCache is the anti-flap memory for the operator-state sync into Zetta: component key →
	// last-synced fingerprint, so a steady stack produces zero Zetta writes (see zetta_statesync.go).
	stateCache sync.Map
	// topologyCache is the anti-flap memory for the cross-cluster topology sync (operator#105): topology
	// node key → last-written fingerprint, so a steady wan slice appends no new orama_control.topology rows
	// (see zetta_topology.go).
	topologyCache sync.Map
	// operatorID{Once,Value} memoize this operator's unique identity (one per k8s cluster) so its state
	// stays distinct in a Zetta cluster shared across k8s clusters.
	operatorIDOnce  sync.Once
	operatorIDValue string
}

//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s,verbs=get;list;watch
//+kubebuilder:rbac:groups=orama.cloud,resources=oramacloudv1s/status,verbs=get
//+kubebuilder:rbac:groups=orama.cloud,resources=oramazettaclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
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
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch;create;update;patch;delete;bind;escalate

func zettaLabels(z *oramav1alpha1.OramaZettaCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "orama-zetta",
		"app.kubernetes.io/instance":   z.Name,
		"app.kubernetes.io/component":  "zetta-node",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
	}
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
	//      `ZETTA_ADMIN_TOKEN` → `--admin-token` (which seeds the owner), and authenticates its OWN Zetta
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
	var derivedPeers []string
	if syncEnabled {
		derivedPeers = r.reconcileZettaTopology(ctx, &z, clientName, peerLBName, stsName)
	}

	// 3. StatefulSet — N equal-peer headless nodes, each with a per-node PVC at /data.
	// Build the POSITIONAL per-node advertise map (comma-joined; field i = node i, "" while pending) so
	// each PERMANENT wan-accessible node gossips ITS OWN routable address (--advertise) instead of its
	// unreachable bound pod IP — otherwise external peers learn a 10.244.x and dial it forever
	// (oramasearch/zetta#739). The resolved subset becomes .status.wanAddress below. Recomputed each
	// reconcile; a later LB IP just rolls the pods once.
	advertiseMap := strings.Join(r.zettaWANAddresses(ctx, &z, peerLBName, stsName), ",")
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		return r.mutateZettaStatefulSet(&z, sts, headlessName, stsName, lbls, advertiseMap, derivedPeers)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Optional external exposure — Ingress (+ proxied Cloudflare DNS) for the HTTP API/UI on 4800
	//    (e.g. zetta.oramadev.net). No-op unless spec.host is set.
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
	} else if err := r.teardownZettaControlPlane(ctx, &z); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Status — readiness roll-up + the capacity/install conditions/count and the slice's reachability
	//    type + (for a wan-accessible slice) the routable rendezvous address (write only on change; anti-flap).
	wanAddress := r.zettaWANAddress(ctx, &z, peerLBName, stsName)
	if err := r.reconcileZettaStatus(ctx, &z, stsName, capCond, installCond, provisionedNodes, reachability, wanAddress); err != nil {
		return ctrl.Result{}, err
	}
	// When acting as the datastore, requeue periodically so the coarse state sync tracks the rest of
	// the operator's CRs even without a Zetta-cluster event (anti-flap keeps the Zetta writes rare).
	if syncEnabled {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileZettaAdminToken ensures a Secret holding a 64-char (32-byte hex) ADMIN token exists for this
// cluster. UNCONDITIONAL since zetta#799 made the engine's auth mandatory: the token is injected into the
// node (ZETTA_ADMIN_TOKEN → `--admin-token`), which seeds it as the owner principal, AND it is the
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

// reconcileZettaIngress exposes the cluster at spec.host via an nginx Ingress. The host root (`/`)
// serves a static "zetta" landing placeholder from a tiny operator-owned nginx pod (see
// reconcileZettaLanding) — independent of Zetta's own HTTP server — while every other path proxies
// to the Zetta HTTP API/UI (:4800). When spec.dnsSecretRef is also set it creates an OWNED
// CloudflareDNSRecord (proxied A/CNAME → the ingress LoadBalancer), mirroring the OramaCloudV1
// dashboard-DNS pattern (the child controller owns the CF API call + finalizer, so the operator
// never mutates Cloudflare from the main loop). SAFE no-op unless spec.ingressEnabled is true AND
// spec.host is set — the bool is the explicit exposure gate, Host names the endpoint.
func (r *OramaZettaClusterReconciler) reconcileZettaIngress(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc string, lbls map[string]string) error {
	ingName := z.Name + "-zetta"
	landingName := z.Name + "-zetta-landing"
	if !z.Spec.IngressEnabled || z.Spec.Host == "" {
		// Gate off (ingressEnabled false) or no host → tear down the Ingress/DNS + landing placeholder.
		_ = deleteIfExists(ctx, r.Client, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: ingName + "-dns", Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		_ = deleteIfExists(ctx, r.Client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: landingName, Namespace: z.Namespace}})
		return nil
	}

	// Landing placeholder (nginx serving "zetta") — the backend for the host root.
	if err := r.reconcileZettaLanding(ctx, z, landingName); err != nil {
		return err
	}

	exact := networkingv1.PathTypeExact
	prefix := networkingv1.PathTypePrefix
	svcBackend := func(name string, port int32) networkingv1.IngressBackend {
		return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: name, Port: networkingv1.ServiceBackendPort{Number: port}}}
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: ingName, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = lbls
		ing.Spec.IngressClassName = strPtr("nginx")
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: z.Spec.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					// Exact `/` (nginx `location = /`) → the landing placeholder; wins for the bare root.
					{Path: "/", PathType: &exact, Backend: svcBackend(landingName, zettaLandingPort)},
					// Prefix `/` (nginx `location /`) → the Zetta HTTP API/UI for everything else.
					{Path: "/", PathType: &prefix, Backend: svcBackend(clientSvc, zettaAPIPort)},
				},
			}},
		}}
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

// reconcileZettaLanding creates the static "zetta" landing placeholder served at the host root — a
// ConfigMap (the page), a tiny nginx Deployment, and its Service. It is deliberately DECOUPLED from
// the Zetta node's own HTTP server (which is currently unreliable — oramasearch/zetta#700/#695), so
// visiting the host always returns something. Replace with the real Zetta web UI when zetta#647 ships.
func (r *OramaZettaClusterReconciler) reconcileZettaLanding(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, name string) error {
	lbls := map[string]string{
		"app.kubernetes.io/name":       "orama-zetta",
		"app.kubernetes.io/instance":   z.Name,
		"app.kubernetes.io/component":  "zetta-landing",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Data = map[string]string{"index.html": zettaLandingText}
		return controllerutil.SetControllerReference(z, cm, r.Scheme)
	}); err != nil {
		return err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = lbls
		svc.Spec.Selector = lbls
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: zettaLandingPort, TargetPort: intstr.FromInt32(zettaLandingPort)}}
		return controllerutil.SetControllerReference(z, svc, r.Scheme)
	}); err != nil {
		return err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = lbls
		dep.Spec.Replicas = int32Ptr(1)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbls},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "nginx",
					Image: zettaLandingImage,
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: zettaLandingPort}},
					VolumeMounts: []corev1.VolumeMount{{
						Name: "content", MountPath: "/usr/share/nginx/html", ReadOnly: true}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(zettaLandingPort)}},
						InitialDelaySeconds: 2, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					},
				}},
				Volumes: []corev1.Volume{{
					Name: "content",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: name}}},
				}},
			},
		}
		return controllerutil.SetControllerReference(z, dep, r.Scheme)
	}); err != nil {
		return err
	}
	return nil
}

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

func (r *OramaZettaClusterReconciler) mutateZettaStatefulSet(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet, headlessName, stsName string, lbls map[string]string, advertise string, derivedPeers []string) error {
	replicas := z.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	image := z.Spec.Image
	if image == "" {
		image = "oramasearch/zetta:headless-v1-sandbox"
	}
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

	// The seed is node-0's stable DNS. A tiny sh wrapper keys off the pod ordinal ($HOSTNAME = <sts>-<ord>)
	// so node-0 gets no --peer (it is the rendezvous) while peers seed off node-0. EVERY node — node-0
	// included — boots with the SAME shared cluster join token (ZETTA_CLUSTER_TOKEN, injected below): the
	// engine derives the cluster CA from it, so all nodes trust one another and form ONE mTLS cluster
	// (operator#92). Peer traffic is always mTLS — there is no plaintext mode.
	seed := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", stsName, headlessName, z.Namespace, zettaPeerPort)
	// External peers — sibling nodes in OTHER k8s clusters / bare hosts, dialed by EVERY node here so they
	// join the shared (globally-named) Zetta cluster. Zetta is the mesh. The set is the UNION of the explicit
	// spec.externalPeers (authoritative override — wins) and the topology-DERIVED wan endpoints of sibling
	// slices (operator#105, coordinated through orama_control.topology — see zetta_topology.go), deduped.
	// The derived set already excludes this operator's OWN nodes and our own advertised addresses.
	extPeers := ""
	for _, p := range zettaPeerSet(z.Spec.ExternalPeers, derivedPeers) {
		extPeers += " --peer " + p
	}
	// The shared cluster join token (ZETTA_CLUSTER_TOKEN) is NOT passed as a flag: the engine reads that
	// env var directly at boot and maps it to --join-token internally (src/main.cpp provisioning env). We
	// keep it env-only on purpose — an unknown env var is safely ignored by an image that predates the
	// feature, whereas an unknown FLAG would crash the node (as the sandbox image does for unknown flags).
	// The `--admin-token` flag is appended AFTER the echo, and the echo shows only a redacted "****", so no
	// token ever lands in the pod log. Tokens are injected via env from their Secrets: ZETTA_CLUSTER_TOKEN
	// (<name>-zetta-cluster, reconcileZettaClusterToken; every node) and ZETTA_ADMIN_TOKEN (<name>-zetta-admin,
	// reconcileZettaAdminToken; every node) which seeds the owner. zetta#799 made auth MANDATORY and REMOVED
	// `--require-auth`/`--mcp-token`/`--cluster-plaintext` — passing any of them is now an unknown-option
	// error that crashes the node, so the entrypoint renders NONE of them (only `--admin-token`).
	//
	// Node-reachability + node-type flags (zetta#803/#810) — this is what makes the engine's CRDT node:<id>
	// entry (and thus `system.nodes` + the reachability-aware dialer) reflect the slice's ACTUAL topology
	// instead of the engine's permanent/wan-accessible DEFAULT. Without these, a dial-out-only slice would
	// still advertise its bound pod IP and peers would dial it — the pod-CIDR-collision undercount #803 was
	// built to fix. reachability and node_type are INDEPENDENT axes: `--dial-out-only` is rendered iff the
	// slice's effective reachability is dial-out-only (zettaReachability — the single source of truth shared
	// with status + topology); `--node-type ephemeral` iff spec.ephemeral, else `--node-type permanent`. These
	// are FLAGS (not env), so — like `--admin-token` — they require the same auth-capable image generation the
	// operator already mandates (post-#799 travels with #803/#810); an older image would crash on them.
	nodeFlags := " --node-type permanent"
	if z.Spec.Ephemeral {
		nodeFlags = " --node-type ephemeral"
	}
	if zettaReachability(z) == oramav1alpha1.ReachabilityDialOutOnly {
		nodeFlags += " --dial-out-only"
	}
	entry := fmt.Sprintf(`set -eu
ORD="${POD_NAME##*-}"   # NOT $HOSTNAME — under hostNetwork that is the node's name, not the pod's
PEER_PORT=%[2]d
API_PORT=%[3]d
SEED="%[4]s"
if [ "${ZETTA_HOST_NETWORK:-0}" = "1" ] && [ "$ORD" != "0" ]; then
  # hostNetwork packs peers onto the shared host(s): every ordinal binds host ports, so give each its
  # own (node-n → 7777+n / 4800+n) and seed off node-0 at the HOST's real IP (HOST_IP, not 127.0.0.1).
  # The seed address must equal what node-0 advertises (its non-loopback IP) or Zetta's address-keyed
  # reconnect dedup never matches and leaks duplicate links (oramasearch/zetta#739).
  PEER_PORT=$((%[2]d + ORD))
  API_PORT=$((%[3]d + ORD))
  SEED="${HOST_IP}:%[2]d"
fi
A="--headless --data-dir %[1]s --listen 0.0.0.0:$PEER_PORT --mcp-listen 0.0.0.0:$API_PORT%[6]s"
if [ "$ORD" != "0" ]; then A="$A --peer $SEED"; fi
A="$A%[5]s"
# Per-node WAN advertise (operator#104, oramasearch/zetta#739): ZETTA_ADVERTISE_MAP is a positional
# comma-joined map — field (ORD+1) is THIS node's own routable addr; a 1-field map applies to all nodes.
# A permanent wan node gossips ITS OWN reachable address, so cross-cluster peers spread across all of them.
ADV="${ZETTA_ADVERTISE_MAP:-}"
if [ -n "$ADV" ]; then
  N=$(echo "$ADV" | awk -F, '{print NF}')
  if [ "$N" = "1" ]; then MINE="$ADV"; else MINE=$(echo "$ADV" | cut -d, -f$((ORD+1))); fi
  [ -n "$MINE" ] && A="$A --advertise $MINE"
fi
echo "zetta node $POD_NAME (ordinal $ORD): zetta $A${ZETTA_CLUSTER_TOKEN:+ [join-token via env ****]}${ZETTA_ADMIN_TOKEN:+ --admin-token ****}"
if [ -n "${ZETTA_ADMIN_TOKEN:-}" ]; then A="$A --admin-token $ZETTA_ADMIN_TOKEN"; fi
exec zetta $A`, zettaDataDir, zettaPeerPort, zettaAPIPort, seed, extPeers, nodeFlags)

	zettaEnv := []corev1.EnvVar{
		// The ordinal comes from the POD name — $HOSTNAME is the NODE's name under hostNetwork.
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		// Shared cluster join token → the mTLS cluster identity. Injected into EVERY node (incl. node-0):
		// the engine derives the cluster CA from it, so all nodes trust each other and form ONE cluster.
		// Without a SHARED token each node mints its own → different CA → cluster-of-one (operator#92).
		{Name: "ZETTA_CLUSTER_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-cluster"}, Key: "token"}}},
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
	// Admin token → ZETTA_ADMIN_TOKEN on EVERY node (UNCONDITIONAL since zetta#799): the entrypoint renders
	// `--admin-token` to seed the owner, and the operator's own SQL client authenticates with the same token.
	zettaEnv = append(zettaEnv, corev1.EnvVar{Name: "ZETTA_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-admin"}, Key: "token"}}})
	// Per-node WAN advertise map (positional; field ORD+1 = this node's own routable addr) — a wan node
	// gossips its OWN reachable endpoint, not its unreachable pod IP (operator#104, oramasearch/zetta#739).
	if advertise != "" {
		zettaEnv = append(zettaEnv, corev1.EnvVar{Name: "ZETTA_ADVERTISE_MAP", Value: advertise})
	}

	sts.Labels = lbls
	sts.Spec.ServiceName = headlessName
	sts.Spec.Replicas = &replicas
	sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: lbls}
	// Bounded, ordered scale-up so node-0 is Ready before peers try to join it.
	pmp := appsv1.ParallelPodManagement
	sts.Spec.PodManagementPolicy = pmp
	sts.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: lbls},
		Spec: corev1.PodSpec{
			ImagePullSecrets: pull,
			// The image runs zetta as uid 10001; fsGroup makes the mounted PVC group-writable so the
			// node can create /data/security (mints its cluster cert/token) etc. Azure/most CSI disks
			// mount root-owned, so a non-root node needs this.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  int64Ptr(10001),
				RunAsGroup: int64Ptr(10001),
				FSGroup:    int64Ptr(10001),
			},
			Containers: []corev1.Container{{
				Name:            "zetta",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways, // dev sandbox: mutable :v1-sandbox tag — always re-pull (operator#103)
				Command:         []string{"/bin/sh", "-c", entry},
				Env:             zettaEnv,
				Ports: []corev1.ContainerPort{
					{Name: "peer", ContainerPort: zettaPeerPort},
					{Name: "api", ContainerPort: zettaAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: zettaDataDir}},
				Resources:    resources,
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
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{pvc}

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
func (r *OramaZettaClusterReconciler) reconcileZettaStatus(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, stsName string, capCond, installCond *metav1.Condition, provisionedNodes int32, reachability, wanAddress string) error {
	desired := z.Spec.Replicas
	if desired < 1 {
		desired = 1
	}
	var sts appsv1.StatefulSet
	ready := int32(0)
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: stsName}, &sts); err == nil {
		ready = sts.Status.ReadyReplicas
	}

	phase := "Progressing"
	msg := fmt.Sprintf("%d/%d zetta nodes ready", ready, desired)
	if ready >= desired {
		phase, msg = "Ready", fmt.Sprintf("all %d zetta nodes ready", desired)
	} else if ready == 0 {
		phase = "Progressing"
	}

	changed := z.Status.Phase != phase || z.Status.ReadyNodes != ready ||
		z.Status.TotalNodes != desired || z.Status.Message != msg ||
		z.Status.ProvisionedNodes != provisionedNodes ||
		z.Status.Reachability != reachability || z.Status.Ephemeral != z.Spec.Ephemeral ||
		z.Status.WanAddress != wanAddress

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

	if !changed {
		return nil // no change — don't touch the status subresource (anti-flap)
	}
	z.Status.Phase, z.Status.ReadyNodes, z.Status.TotalNodes, z.Status.Message = phase, ready, desired, msg
	z.Status.ProvisionedNodes = provisionedNodes
	z.Status.Reachability, z.Status.Ephemeral, z.Status.WanAddress = reachability, z.Spec.Ephemeral, wanAddress
	return r.Status().Update(ctx, z)
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
