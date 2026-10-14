/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// zetta-tunnel's platform namespace and the operator's certificate authorities in it. The relay
// (#471) lives here; the namespace, its NetworkSandbox and the PKI Secret are created on first use
// and never deleted, so the CAs handed to developers stay valid while the relay comes and goes.
const (
	// DefaultZettaTunnelImage is the zetta-tunnel image every role runs unless the manager's
	// --zetta-tunnel-image flag says otherwise. The repository is private: pods use their owner's
	// pull secret.
	DefaultZettaTunnelImage = "oramasearch/zetta-tunnel:dev-933d6fc"

	zettaTunnelNamespace     = "zetta-tunnel"
	zettaTunnelPKISecret     = "zetta-tunnel-pki"
	zettaTunnelManagedBy     = "orama-operator"
	zettaTunnelComponentKey  = "app.kubernetes.io/component"
	zettaTunnelNameKey       = "app.kubernetes.io/name"
	zettaTunnelAppName       = "zetta-tunnel"
	zettaTunnelProbePort     = 8080
	zettaTunnelEndpointPort  = 7444
	zettaTunnelRelayPort     = 7443
	zettaTunnelRelayPublic   = 443
	zettaTunnelPKIRelayCA    = "relay-ca.crt"
	zettaTunnelPKIRelayKey   = "relay-ca.key"
	zettaTunnelPKIWorkCA     = "workload-ca.crt"
	zettaTunnelPKIWorkKey    = "workload-ca.key"
	zettaTunnelPKIUnusableCA = "unusable-ca.crt"
)

// zettaTunnelRelayIdentity is the DNS SAN of the relay's workload certificate. Every endpoint
// requires exactly this name (--peer-name) from the relay.
const zettaTunnelRelayIdentity = "zetta-tunnel-relay.zetta-tunnel.svc"

func zettaTunnelEndpointIdentity(grantID string) string { return "endpoint." + grantID }

// zettaTunnelPKI is the operator's two zetta-tunnel CAs: one signs the relay's public server
// certificate (developers verify it), the other the relay and endpoint workload certificates.
// The developer CA is Lake's and never here.
type zettaTunnelPKI struct {
	Relay      tunnelCA
	Workload   tunnelCA
	UnusableCA []byte
}

func tunnelNamespaceLabels() map[string]string {
	return map[string]string{
		workspaceManagedByLabel:              zettaTunnelManagedBy,
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
	}
}

// ensureOperatorNamespace creates namespace with labels, or confirms an existing one is the
// operator's. It never adopts a namespace someone else made.
func ensureOperatorNamespace(ctx context.Context, c client.Client, name string, labels map[string]string) (*corev1.Namespace, error) {
	var namespace corev1.Namespace
	err := c.Get(ctx, client.ObjectKey{Name: name}, &namespace)
	if apierrors.IsNotFound(err) {
		namespace = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: copyStringMap(labels)}}
		if err := c.Create(ctx, &namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create namespace %s: %w", name, err)
		}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if namespace.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
		return nil, fmt.Errorf("namespace %s exists and is not managed by the operator", name)
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return nil, connectionPendingError{message: fmt.Sprintf("namespace %s is being deleted", name)}
	}
	changed := false
	for key, value := range labels {
		if namespace.Labels[key] != value {
			changed = true
		}
	}
	if changed {
		before := namespace.DeepCopy()
		namespace.Labels = withLabels(namespace.Labels, labels)
		if err := c.Patch(ctx, &namespace, client.MergeFrom(before)); err != nil {
			return nil, err
		}
	}
	return &namespace, nil
}

// ensureZettaTunnelPKI returns the CAs, minting them once. They are never rotated by the operator;
// a rotation is a deliberate deletion of the Secret, which reissues every leaf.
func ensureZettaTunnelPKI(ctx context.Context, c client.Client, now time.Time) (zettaTunnelPKI, error) {
	if _, err := ensureOperatorNamespace(ctx, c, zettaTunnelNamespace, tunnelNamespaceLabels()); err != nil {
		return zettaTunnelPKI{}, err
	}
	key := client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelPKISecret}
	var secret corev1.Secret
	err := c.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		relay, err := newTunnelCA("zetta-tunnel relay CA", now)
		if err != nil {
			return zettaTunnelPKI{}, err
		}
		workload, err := newTunnelCA("zetta-tunnel workload CA", now)
		if err != nil {
			return zettaTunnelPKI{}, err
		}
		unusable, err := newUnusableTunnelCA("zetta-tunnel placeholder CA (no key exists)", now)
		if err != nil {
			return zettaTunnelPKI{}, err
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      zettaTunnelPKISecret,
				Namespace: zettaTunnelNamespace,
				Labels:    map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy, zettaTunnelNameKey: zettaTunnelAppName},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				zettaTunnelPKIRelayCA:    relay.CertPEM,
				zettaTunnelPKIRelayKey:   relay.KeyPEM,
				zettaTunnelPKIWorkCA:     workload.CertPEM,
				zettaTunnelPKIWorkKey:    workload.KeyPEM,
				zettaTunnelPKIUnusableCA: unusable,
			},
		}
		if err := c.Create(ctx, &secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return zettaTunnelPKI{}, fmt.Errorf("create the zetta-tunnel CAs: %w", err)
		}
		if err := c.Get(ctx, key, &secret); err != nil {
			return zettaTunnelPKI{}, err
		}
	} else if err != nil {
		return zettaTunnelPKI{}, err
	}
	pki := zettaTunnelPKI{
		Relay:      tunnelCA{CertPEM: secret.Data[zettaTunnelPKIRelayCA], KeyPEM: secret.Data[zettaTunnelPKIRelayKey]},
		Workload:   tunnelCA{CertPEM: secret.Data[zettaTunnelPKIWorkCA], KeyPEM: secret.Data[zettaTunnelPKIWorkKey]},
		UnusableCA: secret.Data[zettaTunnelPKIUnusableCA],
	}
	if _, _, err := parseTunnelCA(pki.Relay); err != nil {
		return zettaTunnelPKI{}, fmt.Errorf("Secret %s/%s relay CA: %w", zettaTunnelNamespace, zettaTunnelPKISecret, err)
	}
	if _, _, err := parseTunnelCA(pki.Workload); err != nil {
		return zettaTunnelPKI{}, fmt.Errorf("Secret %s/%s workload CA: %w", zettaTunnelNamespace, zettaTunnelPKISecret, err)
	}
	if _, err := pemCertificates(pki.UnusableCA); err != nil {
		return zettaTunnelPKI{}, fmt.Errorf("Secret %s/%s placeholder CA: %w", zettaTunnelNamespace, zettaTunnelPKISecret, err)
	}
	return pki, nil
}

// tunnelTarget is the one in-cluster destination a tunnel or gateway is fixed to.
type tunnelTarget struct {
	// Service is the Kubernetes Service name; Host its cluster DNS name.
	Service string
	Host    string
	// Port is the Service port; PodPort the port the Service sends it to.
	Port    int32
	PodPort int32
	// Selector selects the target's pods, for the generated Connection's policy.
	Selector metav1.LabelSelector
}

func (t tunnelTarget) Address() string { return fmt.Sprintf("%s:%d", t.Host, t.Port) }

// resolveTunnelTarget maps a Lake service or app id and a Service port to one Kubernetes Service in
// namespace, using the same workload identity labels as Connection. A Postgres service has several
// Services on its port; the read-write one is chosen.
func resolveTunnelTarget(
	ctx context.Context,
	c client.Client,
	lakeRef string,
	namespace string,
	kind string,
	id string,
	port int32,
) (tunnelTarget, error) {
	probe := &oramav1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
		Spec:       oramav1alpha1.ConnectionSpec{LakeRef: lakeRef},
	}
	selector, err := (&ConnectionReconciler{Client: c}).workloadSelector(ctx, probe, kind, id)
	if err != nil {
		return tunnelTarget{}, err
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return tunnelTarget{}, err
	}
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return tunnelTarget{}, err
	}
	type candidate struct {
		service *corev1.Service
		port    corev1.ServicePort
	}
	var candidates []candidate
	for i := range services.Items {
		service := &services.Items[i]
		if !service.DeletionTimestamp.IsZero() {
			continue
		}
		for _, servicePort := range service.Spec.Ports {
			protocol := servicePort.Protocol
			if protocol == "" {
				protocol = corev1.ProtocolTCP
			}
			if servicePort.Port == port && protocol == corev1.ProtocolTCP {
				candidates = append(candidates, candidate{service: service, port: servicePort})
			}
		}
	}
	if len(candidates) == 0 {
		return tunnelTarget{}, connectionPendingError{message: fmt.Sprintf("waiting for a Service of %s %s on TCP port %d", kind, id, port)}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].service.Name, candidates[j].service.Name
		aRW, bRW := strings.HasSuffix(a, "-rw"), strings.HasSuffix(b, "-rw")
		if aRW != bRW {
			return aRW
		}
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	chosen := candidates[0]
	podPort := chosen.port.Port
	switch chosen.port.TargetPort.Type {
	case intstr.Int:
		if chosen.port.TargetPort.IntVal != 0 {
			podPort = chosen.port.TargetPort.IntVal
		}
	case intstr.String:
		return tunnelTarget{}, fmt.Errorf("Service %s port %d sends to the named port %q; a tunnel needs a numbered target port",
			chosen.service.Name, port, chosen.port.TargetPort.StrVal)
	}
	return tunnelTarget{
		Service:  chosen.service.Name,
		Host:     fmt.Sprintf("%s.%s.svc.cluster.local", chosen.service.Name, namespace),
		Port:     port,
		PodPort:  podPort,
		Selector: selector,
	}, nil
}

// isPending reports whether err is a wait rather than a failure.
func isPending(err error) bool {
	var pending connectionPendingError
	return errors.As(err, &pending)
}

func tunnelContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                int64Ptr(65532),
		RunAsGroup:               int64Ptr(65532),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func tunnelPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(65532),
		RunAsGroup:     int64Ptr(65532),
		FSGroup:        int64Ptr(65532),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// zettaTunnelProbe is a probe on zetta-tunnel's private plain-HTTP health listener.
func zettaTunnelProbe(path string, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromInt32(zettaTunnelProbePort),
		}},
		PeriodSeconds:    period,
		TimeoutSeconds:   3,
		FailureThreshold: 3,
	}
}

// pullSecretsFor returns the image pull secret list for a Lake's pods in another namespace.
func pullSecretsFor(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

// connectionControlledByLakeTunnel accepts a Connection generated for a tunnel: its controller is a
// ZettaTunnel or CloudflareTunnel in the same namespace, for the same Lake and grant, whose source is
// that grant's workload.
func connectionControlledByLakeTunnel(ctx context.Context, c client.Client, connection *oramav1alpha1.Connection) bool {
	owner := metav1.GetControllerOf(connection)
	if owner == nil || owner.APIVersion != oramav1alpha1.GroupVersion.String() ||
		connection.Spec.SourceKind != oramav1alpha1.ConnectionSourceTunnel {
		return false
	}
	var grant oramav1alpha1.TunnelGrantSpec
	switch owner.Kind {
	case tunnelProviderKindZetta:
		var tunnel oramav1alpha1.ZettaTunnel
		if c.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: owner.Name}, &tunnel) != nil || tunnel.UID != owner.UID {
			return false
		}
		grant = tunnel.Spec.TunnelGrantSpec
	case tunnelProviderKindCloudflare:
		var tunnel oramav1alpha1.CloudflareTunnel
		if c.Get(ctx, client.ObjectKey{Namespace: connection.Namespace, Name: owner.Name}, &tunnel) != nil || tunnel.UID != owner.UID {
			return false
		}
		grant = tunnel.Spec.TunnelGrantSpec
	default:
		return false
	}
	return grant.LakeRef == connection.Spec.LakeRef &&
		grant.GrantID == connection.Spec.SourceID &&
		grant.GrantID == connection.Spec.ConnectionID &&
		grant.WorkspaceID == connection.Spec.WorkspaceID &&
		grant.TargetKind == connection.Spec.TargetKind &&
		grant.TargetID == connection.Spec.TargetID
}

// workspaceTunnelAllowanceNames lists the sandbox allowances the namespace's live tunnels own, so the
// Lake workspace renderer keeps them when it rewrites the sandbox.
func workspaceTunnelAllowanceNames(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	var names []string
	var zetta oramav1alpha1.ZettaTunnelList
	if err := c.List(ctx, &zetta, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list workspace ZettaTunnels: %w", err)
	}
	for i := range zetta.Items {
		if zetta.Items[i].DeletionTimestamp.IsZero() {
			names = append(names, zettaTunnelRelayAllowance(zetta.Items[i].Spec.GrantID))
		}
	}
	var cloudflare oramav1alpha1.CloudflareTunnelList
	if err := c.List(ctx, &cloudflare, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list workspace CloudflareTunnels: %w", err)
	}
	for i := range cloudflare.Items {
		if cloudflare.Items[i].DeletionTimestamp.IsZero() {
			names = append(names, cloudflareTunnelEgressAllowance(cloudflare.Items[i].Spec.GrantID))
		}
	}
	return names, nil
}
