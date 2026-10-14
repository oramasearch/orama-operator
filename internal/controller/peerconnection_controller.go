/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	peerConnectionFinalizer = "orama.cloud/peer-connection"
	peerIDLabel             = "orama.cloud/peer-id"
	zettaTunnelPeerPort     = 7446
	// Import listeners are allocated from here, in the sorted order of the imports.
	peerImportListenerBase = 20000
	peerReplicas           = 2
	// peerNameMaxLength keeps every derived name (allowances "peer-<name>-imports-ingress") a label.
	peerNameMaxLength = 41
)

var peerServiceNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func peerGatewayName(peer *oramav1alpha1.PeerConnection) string { return peer.Name + "-gateway" }

func peerCredentialsSecret(peer *oramav1alpha1.PeerConnection) string {
	if peer.Spec.CredentialsSecretName != "" {
		return peer.Spec.CredentialsSecretName
	}
	return peer.Name + "-credentials"
}

func peerAllowancePrefix(peer *oramav1alpha1.PeerConnection) string { return "peer-" + peer.Name + "-" }

// peerImportServiceName is the readable in-namespace name workloads use for an import.
func peerImportServiceName(name string) string {
	candidate := "peer-" + strings.ReplaceAll(name, ".", "-")
	if len(candidate) <= validation.DNS1035LabelMaxLength {
		return candidate
	}
	return candidate[:50] + "-" + sha256Hex([]byte(name))[:12]
}

func peerGatewayLabels(peer *oramav1alpha1.PeerConnection) map[string]string {
	return map[string]string{
		zettaTunnelNameKey:      zettaTunnelAppName,
		zettaTunnelComponentKey: "peer-gateway",
		peerIDLabel:             peer.Spec.PeerID,
		workspaceManagedByLabel: zettaTunnelManagedBy,
	}
}

func peerGatewaySelector(peer *oramav1alpha1.PeerConnection) map[string]string {
	return map[string]string{zettaTunnelComponentKey: "peer-gateway", peerIDLabel: peer.Spec.PeerID}
}

// PeerConnectionReconciler runs one end of a peer: zetta-tunnel's peer gateway with its projected
// peer document, a LoadBalancer for the other end, one ClusterIP Service per import, and the
// sandbox allowances that let only this namespace's workloads use the imports and only the gateway
// reach the exported targets.
type PeerConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Image is the zetta-tunnel image; empty means DefaultZettaTunnelImage.
	Image string
}

// +kubebuilder:rbac:groups=orama.cloud,resources=peerconnections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=peerconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=peerconnections/finalizers,verbs=update

func (r *PeerConnectionReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultZettaTunnelImage
}

func (r *PeerConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var peer oramav1alpha1.PeerConnection
	if err := r.Get(ctx, req.NamespacedName, &peer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !peer.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&peer, peerConnectionFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.teardown(ctx, &peer); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&peer, peerConnectionFinalizer)
		return ctrl.Result{}, r.Update(ctx, &peer)
	}
	if !controllerutil.ContainsFinalizer(&peer, peerConnectionFinalizer) {
		controllerutil.AddFinalizer(&peer, peerConnectionFinalizer)
		if err := r.Update(ctx, &peer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if peer.Spec.Desired == oramav1alpha1.ConnectionDesiredAbsent {
		if err := r.teardown(ctx, &peer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setStatus(ctx, &peer, oramav1alpha1.PeerConnectionStatus{
			Phase: oramav1alpha1.PeerPhaseDeleted, Message: "the gateway, its Services and its policies are removed",
		})
	}
	if message := validatePeerConnection(&peer); message != "" {
		if err := r.teardown(ctx, &peer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setStatus(ctx, &peer, oramav1alpha1.PeerConnectionStatus{Phase: oramav1alpha1.PeerPhaseFailed, Message: message})
	}
	observed, err := r.reconcilePresent(ctx, &peer)
	if err != nil {
		var invalid zettaTunnelInvalid
		switch {
		case isPending(err):
			address := observed.Address
			if address == "" {
				address = r.currentGatewayAddress(ctx, &peer)
			}
			return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, &peer, oramav1alpha1.PeerConnectionStatus{
				Phase: oramav1alpha1.PeerPhasePending, Message: err.Error(), Address: address,
			})
		case errors.As(err, &invalid):
			return ctrl.Result{}, r.setStatus(ctx, &peer, oramav1alpha1.PeerConnectionStatus{Phase: oramav1alpha1.PeerPhaseFailed, Message: err.Error()})
		}
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if observed.Phase != oramav1alpha1.PeerPhaseReady {
		result.RequeueAfter = tunnelPendingRequeue
	}
	return result, r.setStatus(ctx, &peer, observed)
}

func validatePeerConnection(peer *oramav1alpha1.PeerConnection) string {
	parsed, err := uuid.Parse(peer.Spec.PeerID)
	if err != nil || parsed.String() != peer.Spec.PeerID {
		return "peerId must be a canonical lowercase UUID"
	}
	if len(peer.Name) > peerNameMaxLength {
		return fmt.Sprintf("a PeerConnection name is at most %d characters", peerNameMaxLength)
	}
	if peer.Spec.Remote != "" {
		if message := validatePeerRemote(peer.Spec.Remote); message != "" {
			return message
		}
	}
	importKeys := map[string]bool{}
	for _, item := range peer.Spec.Imports {
		if message := validatePeerServiceName(item.Name); message != "" {
			return "import " + message
		}
		key := item.Name + ":" + strconv.Itoa(int(item.Port))
		if importKeys[key] {
			return "import " + key + " is declared twice"
		}
		importKeys[key] = true
	}
	if len(peer.Spec.Imports) > 256 || len(peer.Spec.Exports) > 256 {
		return "at most 256 imports and 256 exports"
	}
	for _, item := range peer.Spec.Exports {
		if item.Name != "" {
			if message := validatePeerServiceName(item.Name); message != "" {
				return "export " + message
			}
		}
		switch item.TargetKind {
		case oramav1alpha1.PeerTargetService, oramav1alpha1.PeerTargetApp:
			if peer.Spec.LakeRef == "" {
				return "a service or app export needs a Lake (spec.lakeRef)"
			}
			if _, err := uuid.Parse(item.TargetID); err != nil {
				return "export targetId must be a UUID"
			}
		case oramav1alpha1.PeerTargetKubernetesService:
			if len(validation.IsDNS1035Label(item.ServiceName)) != 0 {
				return "export serviceName must be a Service name"
			}
		default:
			return "export targetKind must be service, app or kubernetesService"
		}
		if item.Port < 1 || item.Port > 65535 {
			return "export port must be between 1 and 65535"
		}
	}
	return ""
}

// validatePeerServiceName applies zetta-tunnel's rule: lowercase DNS-style names, no wildcard, never
// a numeric address.
func validatePeerServiceName(name string) string {
	if !peerServiceNamePattern.MatchString(name) || len(name) > 253 {
		return fmt.Sprintf("name %q must be a lowercase DNS-style name", name)
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return fmt.Sprintf("name %q is an address, not a service name", name)
	}
	return ""
}

func validatePeerRemote(remote string) string {
	host, port, err := net.SplitHostPort(remote)
	if err != nil || host == "" {
		return "remote must be host:port"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "remote port must be between 1 and 65535"
	}
	return ""
}

// peerCredentials is the gateway's credentials Secret, as the managing cluster wrote it.
type peerCredentials struct {
	Identity string
	PeerName string
	Remote   string
	Hash     string
}

func (r *PeerConnectionReconciler) credentials(ctx context.Context, peer *oramav1alpha1.PeerConnection) (peerCredentials, error) {
	name := peerCredentialsSecret(peer)
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: peer.Namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return peerCredentials{}, connectionPendingError{message: fmt.Sprintf(
				"waiting for credentials Secret %s, written by a PeerLink on the managing cluster", name)}
		}
		return peerCredentials{}, err
	}
	for _, key := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt", "peer-name"} {
		if len(secret.Data[key]) == 0 {
			return peerCredentials{}, connectionPendingError{message: fmt.Sprintf("credentials Secret %s has no %s", name, key)}
		}
	}
	certs, err := pemCertificates(secret.Data[corev1.TLSCertKey])
	if err != nil || len(certs[0].DNSNames) != 1 {
		return peerCredentials{}, zettaTunnelInvalid{message: fmt.Sprintf("credentials Secret %s must hold a certificate with exactly one DNS name", name)}
	}
	if _, err := pemCertificates(secret.Data["ca.crt"]); err != nil {
		return peerCredentials{}, zettaTunnelInvalid{message: fmt.Sprintf("credentials Secret %s ca.crt: %v", name, err)}
	}
	return peerCredentials{
		Identity: certs[0].DNSNames[0],
		PeerName: strings.TrimSpace(string(secret.Data["peer-name"])),
		Remote:   strings.TrimSpace(string(secret.Data["remote"])),
		Hash:     sha256Hex(secret.Data[corev1.TLSCertKey], secret.Data["ca.crt"], secret.Data["peer-name"]),
	}, nil
}

// peerExportTarget is one resolved export.
type peerExportTarget struct {
	Name   string
	Port   int32
	Target tunnelTarget
}

func (r *PeerConnectionReconciler) resolveExports(ctx context.Context, peer *oramav1alpha1.PeerConnection) ([]peerExportTarget, error) {
	var out []peerExportTarget
	seen := map[string]bool{}
	for _, item := range peer.Spec.Exports {
		var target tunnelTarget
		var err error
		if item.TargetKind == oramav1alpha1.PeerTargetKubernetesService {
			target, err = resolveKubernetesServiceTarget(ctx, r.Client, peer.Namespace, item.ServiceName, item.Port)
		} else {
			target, err = resolveTunnelTarget(ctx, r.Client, peer.Spec.LakeRef, peer.Namespace, item.TargetKind, item.TargetID, item.Port)
		}
		if err != nil {
			if isPending(err) {
				return nil, err
			}
			return nil, zettaTunnelInvalid{message: "export: " + err.Error()}
		}
		name := item.Name
		if name == "" {
			name = target.Service
		}
		key := name + ":" + strconv.Itoa(int(item.Port))
		if seen[key] {
			return nil, zettaTunnelInvalid{message: "export " + key + " is declared twice"}
		}
		seen[key] = true
		out = append(out, peerExportTarget{Name: name, Port: item.Port, Target: target})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Port < out[j].Port
	})
	return out, nil
}

// resolveKubernetesServiceTarget is a Service in namespace by name; its own selector selects its pods.
func resolveKubernetesServiceTarget(ctx context.Context, c client.Client, namespace, name string, port int32) (tunnelTarget, error) {
	var service corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return tunnelTarget{}, connectionPendingError{message: fmt.Sprintf("waiting for Service %s", name)}
		}
		return tunnelTarget{}, err
	}
	if len(service.Spec.Selector) == 0 {
		return tunnelTarget{}, fmt.Errorf("Service %s has no selector", name)
	}
	for _, servicePort := range service.Spec.Ports {
		if servicePort.Port != port || (servicePort.Protocol != "" && servicePort.Protocol != corev1.ProtocolTCP) {
			continue
		}
		podPort := port
		switch servicePort.TargetPort.Type {
		case intstr.Int:
			if servicePort.TargetPort.IntVal != 0 {
				podPort = servicePort.TargetPort.IntVal
			}
		case intstr.String:
			return tunnelTarget{}, fmt.Errorf("Service %s port %d sends to the named port %q; a numbered target port is needed", name, port, servicePort.TargetPort.StrVal)
		}
		return tunnelTarget{
			Service: name, Host: fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace), Port: port, PodPort: podPort,
			Selector: metav1.LabelSelector{MatchLabels: copyStringMap(service.Spec.Selector)},
		}, nil
	}
	return tunnelTarget{}, connectionPendingError{message: fmt.Sprintf("waiting for Service %s to expose TCP port %d", name, port)}
}

// peerImportListener is one import with its local listener port.
type peerImportListener struct {
	Name    string
	Port    int32
	Listen  int32
	Service string
}

func peerImportListeners(peer *oramav1alpha1.PeerConnection) []peerImportListener {
	imports := append([]oramav1alpha1.PeerImport(nil), peer.Spec.Imports...)
	sort.Slice(imports, func(i, j int) bool {
		if imports[i].Name != imports[j].Name {
			return imports[i].Name < imports[j].Name
		}
		return imports[i].Port < imports[j].Port
	})
	out := make([]peerImportListener, 0, len(imports))
	for i, item := range imports {
		out = append(out, peerImportListener{
			Name: item.Name, Port: item.Port, Listen: peerImportListenerBase + int32(i), Service: peerImportServiceName(item.Name),
		})
	}
	return out
}

// zettaTunnelPeerDocument is zetta-tunnel's projected peer (docs/peer.md).
type zettaTunnelPeerDocument struct {
	Peer zettaTunnelPeer `json:"peer"`
}

type zettaTunnelPeer struct {
	ID        string                  `json:"id"`
	Workspace string                  `json:"workspace"`
	Remote    string                  `json:"remote"`
	Imports   []zettaTunnelPeerImport `json:"imports"`
	Exports   []zettaTunnelPeerExport `json:"exports"`
}

type zettaTunnelPeerImport struct {
	Name   string `json:"name"`
	Port   int32  `json:"port"`
	Listen string `json:"listen"`
}

type zettaTunnelPeerExport struct {
	Name   string `json:"name"`
	Port   int32  `json:"port"`
	Target string `json:"target"`
}

func peerDocument(peer *oramav1alpha1.PeerConnection, remote string, imports []peerImportListener, exports []peerExportTarget) ([]byte, error) {
	document := zettaTunnelPeerDocument{Peer: zettaTunnelPeer{
		ID: peer.Spec.PeerID, Workspace: peer.Namespace, Remote: remote,
		Imports: []zettaTunnelPeerImport{}, Exports: []zettaTunnelPeerExport{},
	}}
	for _, item := range imports {
		document.Peer.Imports = append(document.Peer.Imports, zettaTunnelPeerImport{
			Name: item.Name, Port: item.Port, Listen: fmt.Sprintf("0.0.0.0:%d", item.Listen),
		})
	}
	for _, item := range exports {
		document.Peer.Exports = append(document.Peer.Exports, zettaTunnelPeerExport{
			Name: item.Name, Port: item.Port, Target: item.Target.Address(),
		})
	}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (r *PeerConnectionReconciler) sandbox(ctx context.Context, peer *oramav1alpha1.PeerConnection) (*oramav1alpha1.NetworkSandbox, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := r.Get(ctx, client.ObjectKey{Namespace: peer.Namespace, Name: networkSandboxName}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, connectionPendingError{message: "waiting for the namespace's NetworkSandbox"}
		}
		return nil, err
	}
	if peer.Spec.LakeRef != "" {
		var lake oramav1alpha1.Lake
		if err := r.Get(ctx, client.ObjectKey{Name: peer.Spec.LakeRef}, &lake); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, zettaTunnelInvalid{message: fmt.Sprintf("Lake %q does not exist", peer.Spec.LakeRef)}
			}
			return nil, err
		}
		if !metav1.IsControlledBy(&sandbox, &lake) {
			return nil, zettaTunnelInvalid{message: fmt.Sprintf("NetworkSandbox %s/%s is not controlled by Lake %q", peer.Namespace, networkSandboxName, lake.Name)}
		}
	} else if metav1.GetControllerOf(&sandbox) == nil {
		return nil, zettaTunnelInvalid{message: "the namespace's NetworkSandbox has no owner"}
	}
	return &sandbox, nil
}

func (r *PeerConnectionReconciler) reconcilePresent(ctx context.Context, peer *oramav1alpha1.PeerConnection) (oramav1alpha1.PeerConnectionStatus, error) {
	sandbox, err := r.sandbox(ctx, peer)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	credentials, err := r.credentials(ctx, peer)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	// The public address comes first: the other end needs it before either gateway can start.
	service, err := r.reconcileGatewayService(ctx, peer)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	remote := peer.Spec.Remote
	if remote == "" {
		remote = credentials.Remote
	}
	if remote == "" {
		return oramav1alpha1.PeerConnectionStatus{Address: peerGatewayAddress(service)}, connectionPendingError{message: "waiting for the other end's address (spec.remote, or the PeerLink's remote)"}
	}
	if message := validatePeerRemote(remote); message != "" {
		return oramav1alpha1.PeerConnectionStatus{}, zettaTunnelInvalid{message: message}
	}
	exports, err := r.resolveExports(ctx, peer)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	imports := peerImportListeners(peer)
	allowances, err := peerAllowances(ctx, r.Client, peer, remote, imports, exports)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	// Policies first, so the gateway never runs without them.
	if err := setPrefixedSandboxAllowances(ctx, r.Client, r.Scheme, sandbox, peerAllowancePrefix(peer), allowances); err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	document, err := peerDocument(peer, remote, imports, exports)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	if err := r.reconcileDocument(ctx, peer, document); err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	pullSecret := peer.Spec.PullSecretName
	if pullSecret == "" && peer.Spec.LakeRef != "" {
		var lake oramav1alpha1.Lake
		if err := r.Get(ctx, client.ObjectKey{Name: peer.Spec.LakeRef}, &lake); err == nil {
			pullSecret = lake.Spec.PullSecretName
		}
	}
	deployment, err := r.reconcileGateway(ctx, peer, credentials, pullSecret, imports)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	importStatus, err := r.reconcileImportServices(ctx, peer, imports)
	if err != nil {
		return oramav1alpha1.PeerConnectionStatus{}, err
	}
	exportStatus := make([]oramav1alpha1.PeerExportStatus, 0, len(exports))
	for _, item := range exports {
		exportStatus = append(exportStatus, oramav1alpha1.PeerExportStatus{Name: item.Name, Port: item.Port, Target: item.Target.Address()})
	}
	address := peerGatewayAddress(service)
	status := oramav1alpha1.PeerConnectionStatus{
		Phase: oramav1alpha1.PeerPhasePending, Address: address, Identity: credentials.Identity,
		Imports: importStatus, Exports: exportStatus,
	}
	switch {
	case address == "":
		status.Message = "waiting for the gateway's public address"
	case deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.AvailableReplicas < 1:
		status.Message = "waiting for the gateway to become available"
	default:
		status.Phase = oramav1alpha1.PeerPhaseReady
		status.Message = peerReadyMessage(imports, peer.Namespace, len(exports))
	}
	return status, nil
}

// currentGatewayAddress is the gateway Service's public address, if it has one.
func (r *PeerConnectionReconciler) currentGatewayAddress(ctx context.Context, peer *oramav1alpha1.PeerConnection) string {
	var service corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: peer.Namespace, Name: peerGatewayName(peer)}, &service); err != nil || !metav1.IsControlledBy(&service, peer) {
		return ""
	}
	return peerGatewayAddress(&service)
}

func peerReadyMessage(imports []peerImportListener, namespace string, exports int) string {
	parts := make([]string, 0, len(imports))
	for _, item := range imports {
		parts = append(parts, fmt.Sprintf("%s:%d at %s.%s.svc:%d", item.Name, item.Port, item.Service, namespace, item.Port))
	}
	message := fmt.Sprintf("gateway ready; %d export(s)", exports)
	if len(parts) > 0 {
		message += "; imports " + strings.Join(parts, ", ")
	}
	return message
}

func peerGatewayAddress(service *corev1.Service) string {
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		host := ingress.IP
		if host == "" {
			host = ingress.Hostname
		}
		if host != "" {
			return net.JoinHostPort(host, strconv.Itoa(zettaTunnelPeerPort))
		}
	}
	return ""
}

// peerAllowances are the namespace sandbox's allowances for one peer:
//   - the other end reaches the gateway's mutual-TLS listener (the gateway checks its identity);
//   - the gateway reaches the other end's address and port, and only the exported targets' pods;
//   - each exported target admits only the gateway, on its pod port;
//   - this namespace's workloads reach only the import listeners on the gateway.
func peerAllowances(
	ctx context.Context,
	reader client.Reader,
	peer *oramav1alpha1.PeerConnection,
	remote string,
	imports []peerImportListener,
	exports []peerExportTarget,
) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	prefix := peerAllowancePrefix(peer)
	gateway := metav1.LabelSelector{MatchLabels: peerGatewaySelector(peer)}
	all := []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}, {IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}}}
	host, portText, _ := net.SplitHostPort(remote)
	remotePort, _ := strconv.Atoi(portText)
	var remotePeers []networkingv1.NetworkPolicyPeer
	if address, err := netip.ParseAddr(host); err == nil {
		bits := 32
		if address.Is6() {
			bits = 128
		}
		remotePeers = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: netip.PrefixFrom(address, bits).String()}}}
	} else {
		var err error
		remotePeers, err = publicInternetPeers(ctx, reader)
		if err != nil {
			return nil, err
		}
	}
	egress := []networkingv1.NetworkPolicyEgressRule{
		{To: remotePeers, Ports: tcpPorts(int32(remotePort))},
		workspaceDNSPolicy(peer.Namespace, nil).Spec.Egress[0],
	}
	for _, item := range exports {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: item.Target.Selector.DeepCopy()}},
			Ports: tcpPorts(item.Target.PodPort),
		})
	}
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			fmt.Sprintf("Peer %s: the other end's gateway reaches this gateway's mutual-TLS listener, which checks its exact identity.", peer.Spec.PeerID),
			networkPolicy(prefix+"gateway-ingress", gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: all, Ports: tcpPorts(zettaTunnelPeerPort)}}, nil),
		),
		sandboxAllowance(
			fmt.Sprintf("Peer %s: the gateway reaches only the other end, DNS and the exported targets.", peer.Spec.PeerID),
			networkPolicy(prefix+"gateway-egress", gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil, egress),
		),
	}
	for i, item := range exports {
		policies = append(policies, sandboxAllowance(
			fmt.Sprintf("Peer %s: only the gateway reaches the exported %s on port %d.", peer.Spec.PeerID, item.Name, item.Port),
			networkPolicy(fmt.Sprintf("%sexport-%d", prefix, i), *item.Target.Selector.DeepCopy(), []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: gateway.DeepCopy()}},
					Ports: tcpPorts(item.Target.PodPort),
				}}, nil),
		))
	}
	if len(imports) > 0 {
		listeners := make([]int32, 0, len(imports))
		for _, item := range imports {
			listeners = append(listeners, item.Listen)
		}
		namespacePods := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
		policies = append(policies,
			sandboxAllowance(
				fmt.Sprintf("Peer %s: this namespace's workloads reach the import listeners, and no other gateway port.", peer.Spec.PeerID),
				networkPolicy(prefix+"imports-ingress", gateway, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					[]networkingv1.NetworkPolicyIngressRule{{From: namespacePods, Ports: tcpPorts(listeners...)}}, nil),
			),
			sandboxAllowance(
				fmt.Sprintf("Peer %s: this namespace's workloads may connect to the import listeners.", peer.Spec.PeerID),
				networkPolicy(prefix+"imports-egress", metav1.LabelSelector{}, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil,
					[]networkingv1.NetworkPolicyEgressRule{{
						To:    []networkingv1.NetworkPolicyPeer{{PodSelector: gateway.DeepCopy()}},
						Ports: tcpPorts(listeners...),
					}}),
			),
		)
	}
	return policies, nil
}

// setPrefixedSandboxAllowances replaces every allowance whose name starts with prefix by
// allowances (nil removes them) and renders the sandbox.
func setPrefixedSandboxAllowances(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
	prefix string,
	allowances []oramav1alpha1.NetworkSandboxPolicy,
) error {
	kept := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(sandbox.Spec.Policies)+len(allowances))
	for _, policy := range sandbox.Spec.Policies {
		if !strings.HasPrefix(policy.Name, prefix) {
			kept = append(kept, policy)
		}
	}
	kept = append(kept, allowances...)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if !apiequality.Semantic.DeepEqual(sandbox.Spec.Policies, kept) {
		sandbox.Spec.Policies = kept
		if err := c.Update(ctx, sandbox); err != nil {
			return fmt.Errorf("update NetworkSandbox peer allowances: %w", err)
		}
	}
	_, _, err := reconcileNetworkSandboxPolicies(ctx, c, scheme, sandbox)
	return err
}

func (r *PeerConnectionReconciler) reconcileDocument(ctx context.Context, peer *oramav1alpha1.PeerConnection, document []byte) error {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if configMap.ResourceVersion != "" && !metav1.IsControlledBy(configMap, peer) {
			return fmt.Errorf("ConfigMap %s/%s is not this peer's", configMap.Namespace, configMap.Name)
		}
		configMap.Labels = withLabels(configMap.Labels, peerGatewayLabels(peer))
		configMap.Data = map[string]string{"peer.json": string(document)}
		return controllerutil.SetControllerReference(peer, configMap, r.Scheme)
	})
	return err
}

func (r *PeerConnectionReconciler) reconcileGatewayService(ctx context.Context, peer *oramav1alpha1.PeerConnection) (*corev1.Service, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.ResourceVersion != "" && !metav1.IsControlledBy(service, peer) {
			return fmt.Errorf("Service %s/%s is not this peer's", service.Namespace, service.Name)
		}
		service.Labels = withLabels(service.Labels, peerGatewayLabels(peer))
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
		service.Spec.Selector = peerGatewaySelector(peer)
		service.Spec.Ports = []corev1.ServicePort{{
			Name: "peer", Protocol: corev1.ProtocolTCP, Port: zettaTunnelPeerPort, TargetPort: intstr.FromInt32(zettaTunnelPeerPort),
		}}
		return controllerutil.SetControllerReference(peer, service, r.Scheme)
	})
	return service, err
}

func (r *PeerConnectionReconciler) reconcileGateway(
	ctx context.Context,
	peer *oramav1alpha1.PeerConnection,
	credentials peerCredentials,
	pullSecret string,
	imports []peerImportListener,
) (*appsv1.Deployment, error) {
	labels := peerGatewayLabels(peer)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}}
	ports := []corev1.ContainerPort{
		{Name: "peer", ContainerPort: zettaTunnelPeerPort, Protocol: corev1.ProtocolTCP},
		{Name: "probe", ContainerPort: zettaTunnelProbePort, Protocol: corev1.ProtocolTCP},
	}
	for _, item := range imports {
		ports = append(ports, corev1.ContainerPort{ContainerPort: item.Listen, Protocol: corev1.ProtocolTCP})
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" && !metav1.IsControlledBy(deployment, peer) {
			return fmt.Errorf("Deployment %s/%s is not this peer's", deployment.Namespace, deployment.Name)
		}
		deployment.Labels = withLabels(deployment.Labels, labels)
		deployment.Spec.Replicas = int32Ptr(peerReplicas)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: peerGatewaySelector(peer)}
		deployment.Spec.Template.Labels = labels
		// A credential change restarts the gateway; a document change reaches it through the
		// mounted file, which it re-reads every two seconds.
		deployment.Spec.Template.Annotations = map[string]string{"orama.cloud/peer-credentials": credentials.Hash}
		deployment.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			ImagePullSecrets:              pullSecretsFor(pullSecret),
			SecurityContext:               tunnelPodSecurityContext(),
			TerminationGracePeriodSeconds: int64Ptr(30),
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector: &metav1.LabelSelector{MatchLabels: peerGatewaySelector(peer)},
			}},
			Containers: []corev1.Container{{
				Name:            "gateway",
				Image:           r.image(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{
					"peer",
					"--configuration", "/etc/zetta-tunnel/peer/peer.json",
					"--cert", "/etc/zetta-tunnel/tls/tls.crt",
					"--key", "/etc/zetta-tunnel/tls/tls.key",
					"--ca", "/etc/zetta-tunnel/tls/ca.crt",
					"--peer-name", credentials.PeerName,
					"--listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelPeerPort),
					"--health-listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelProbePort),
					"--max-sessions", "1024",
					"--audit", "-",
				},
				Ports:          ports,
				ReadinessProbe: zettaTunnelProbe("/readyz", 5),
				LivenessProbe:  zettaTunnelProbe("/healthz", 10),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
				SecurityContext: tunnelContainerSecurityContext(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "peer", MountPath: "/etc/zetta-tunnel/peer", ReadOnly: true},
					{Name: "tls", MountPath: "/etc/zetta-tunnel/tls", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "peer", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: peerGatewayName(peer)}, DefaultMode: int32Ptr(0o444),
				}}},
				{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: peerCredentialsSecret(peer), DefaultMode: int32Ptr(0o440),
					Items: []corev1.KeyToPath{
						{Key: corev1.TLSCertKey, Path: "tls.crt"}, {Key: corev1.TLSPrivateKeyKey, Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"},
					},
				}}},
			},
		}
		return controllerutil.SetControllerReference(peer, deployment, r.Scheme)
	})
	return deployment, err
}

func (r *PeerConnectionReconciler) reconcileImportServices(
	ctx context.Context,
	peer *oramav1alpha1.PeerConnection,
	imports []peerImportListener,
) ([]oramav1alpha1.PeerImportStatus, error) {
	wanted := map[string][]corev1.ServicePort{}
	for _, item := range imports {
		wanted[item.Service] = append(wanted[item.Service], corev1.ServicePort{
			Name: fmt.Sprintf("p%d", item.Port), Protocol: corev1.ProtocolTCP, Port: item.Port, TargetPort: intstr.FromInt32(item.Listen),
		})
	}
	for name, ports := range wanted {
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: peer.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
			if service.ResourceVersion != "" && !metav1.IsControlledBy(service, peer) {
				return zettaTunnelInvalid{message: fmt.Sprintf("Service %s/%s for an import exists and is not this peer's", service.Namespace, service.Name)}
			}
			service.Labels = withLabels(service.Labels, peerGatewayLabels(peer))
			service.Labels[zettaTunnelComponentKey] = "peer-import"
			service.Spec.Type = corev1.ServiceTypeClusterIP
			service.Spec.Selector = peerGatewaySelector(peer)
			sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
			service.Spec.Ports = ports
			return controllerutil.SetControllerReference(peer, service, r.Scheme)
		})
		if err != nil {
			return nil, err
		}
	}
	if err := r.deleteImportServices(ctx, peer, wanted); err != nil {
		return nil, err
	}
	status := make([]oramav1alpha1.PeerImportStatus, 0, len(imports))
	for _, item := range imports {
		status = append(status, oramav1alpha1.PeerImportStatus{Name: item.Name, Port: item.Port, Service: item.Service})
	}
	return status, nil
}

// deleteImportServices deletes this peer's import Services that are not in keep.
func (r *PeerConnectionReconciler) deleteImportServices(ctx context.Context, peer *oramav1alpha1.PeerConnection, keep map[string][]corev1.ServicePort) error {
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(peer.Namespace), client.MatchingLabels{
		peerIDLabel: peer.Spec.PeerID, zettaTunnelComponentKey: "peer-import",
	}); err != nil {
		return err
	}
	for i := range services.Items {
		service := &services.Items[i]
		if _, ok := keep[service.Name]; ok || !metav1.IsControlledBy(service, peer) {
			continue
		}
		if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// teardown withdraws the import Services and every policy first, then removes the projection and
// the gateway, as zetta-tunnel's peer contract orders it.
func (r *PeerConnectionReconciler) teardown(ctx context.Context, peer *oramav1alpha1.PeerConnection) error {
	if err := r.deleteImportServices(ctx, peer, nil); err != nil {
		return err
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := r.Get(ctx, client.ObjectKey{Namespace: peer.Namespace, Name: networkSandboxName}, &sandbox); err == nil {
		if err := setPrefixedSandboxAllowances(ctx, r.Client, r.Scheme, &sandbox, peerAllowancePrefix(peer), nil); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	for _, object := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: peerGatewayName(peer), Namespace: peer.Namespace}},
	} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(object, peer) {
			continue
		}
		if err := r.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *PeerConnectionReconciler) setStatus(ctx context.Context, peer *oramav1alpha1.PeerConnection, observed oramav1alpha1.PeerConnectionStatus) error {
	conditionStatus := metav1.ConditionFalse
	if observed.Phase == oramav1alpha1.PeerPhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	desired := observed
	desired.ObservedGeneration = peer.Spec.Generation
	desired.Conditions = append([]metav1.Condition(nil), peer.Status.Conditions...)
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: tunnelReadyCondition, Status: conditionStatus, Reason: desired.Phase, Message: desired.Message, ObservedGeneration: peer.Generation,
	})
	if apiequality.Semantic.DeepEqual(peer.Status, desired) {
		return nil
	}
	peer.Status = desired
	return r.Status().Update(ctx, peer)
}

func (r *PeerConnectionReconciler) peersInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var peers oramav1alpha1.PeerConnectionList
	if err := r.List(ctx, &peers, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(peers.Items))
	for i := range peers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&peers.Items[i])})
	}
	return requests
}

// peerAllowanceNames are the allowances a PeerConnection owns, for the workspace renderer to keep.
func peerAllowanceNames(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	var peers oramav1alpha1.PeerConnectionList
	if err := c.List(ctx, &peers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list PeerConnections: %w", err)
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	var names []string
	for i := range peers.Items {
		peer := &peers.Items[i]
		if !peer.DeletionTimestamp.IsZero() || peer.Spec.Desired == oramav1alpha1.ConnectionDesiredAbsent {
			continue
		}
		prefix := peerAllowancePrefix(peer)
		for _, policy := range sandbox.Spec.Policies {
			if strings.HasPrefix(policy.Name, prefix) {
				names = append(names, policy.Name)
			}
		}
	}
	return names, nil
}

func (r *PeerConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inNamespace := handler.EnqueueRequestsFromMapFunc(r.peersInNamespace)
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PeerConnection{}).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1.Service{}, inNamespace).
		Watches(&corev1.Secret{}, inNamespace).
		Watches(&oramav1alpha1.NetworkSandbox{}, inNamespace).
		Complete(r)
}
