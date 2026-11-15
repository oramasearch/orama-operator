/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	zettaTunnelRelayName      = "zetta-tunnel-relay"
	zettaTunnelRelayGrants    = "zetta-tunnel-relay-grants"
	zettaTunnelRelayServerTLS = "zetta-tunnel-relay-server"
	zettaTunnelRelayWorkTLS   = "zetta-tunnel-relay-workload"
	zettaTunnelRelayPull      = "zetta-tunnel-pull"
	zettaTunnelGrantsKey      = "grants.json"
	zettaTunnelClientCAKey    = "client-ca.pem"
	zettaTunnelRelayReplicas  = 2
)

func zettaTunnelRelayLabels() map[string]string {
	return map[string]string{zettaTunnelNameKey: zettaTunnelAppName, zettaTunnelComponentKey: "relay"}
}

// zettaTunnelGrant is one entry of the relay's grants file (zetta-tunnel docs/tunnel.md).
// ClientCASHA256 binds the grant to its own customer's CA (LSR-074, zetta-tunnel#35): the relay,
// from zetta-tunnel v0.2.0, refuses a client certificate for this grant unless a CA listed here
// signed it, so one customer's CA can never open another customer's grant.
type zettaTunnelGrant struct {
	ID             string   `json:"id"`
	Workspace      string   `json:"workspace"`
	Endpoint       string   `json:"endpoint"`
	ExpiresAt      string   `json:"expires_at"`
	ClientCASHA256 []string `json:"client_ca_sha256"`
}

// zettaTunnelMaxClientCAs is the relay's limit on client_ca_sha256 entries per grant.
const zettaTunnelMaxClientCAs = 16

// zettaTunnelClientCAFingerprints is the lowercase-hex SHA-256 of the DER of each distinct
// certificate in clientCAPEM, sorted. The relay refuses a grant with none, more than 16, or a
// repeated one, so an empty result, or one over the limit, is an error here.
func zettaTunnelClientCAFingerprints(clientCAPEM string) ([]string, error) {
	certs, err := pemCertificates([]byte(clientCAPEM))
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var fingerprints []string
	for _, cert := range certs {
		sum := sha256.Sum256(cert.Raw)
		fingerprint := hex.EncodeToString(sum[:])
		if _, dup := seen[fingerprint]; dup {
			continue
		}
		seen[fingerprint] = struct{}{}
		fingerprints = append(fingerprints, fingerprint)
	}
	if len(fingerprints) > zettaTunnelMaxClientCAs {
		return nil, fmt.Errorf("clientCAPEM holds %d distinct CA certificates; the relay accepts at most %d",
			len(fingerprints), zettaTunnelMaxClientCAs)
	}
	sort.Strings(fingerprints)
	return fingerprints, nil
}

type zettaTunnelGrantsDocument struct {
	Grants []zettaTunnelGrant `json:"grants"`
}

// zettaTunnelRelayReconciler is the cluster's one relay. It runs while any ZettaTunnel exists, and
// projects the live grants and the union of their client CAs into the files the relay reads.
type zettaTunnelRelayReconciler struct {
	client.Client
	// APIReader reads Namespaces uncached; the operator never lists or watches them (operator#106).
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Image     string
	Now       func() time.Time
}

func (r *zettaTunnelRelayReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

var zettaTunnelRelayRequest = reconcile.Request{NamespacedName: client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}}

// liveZettaTunnelGrants is the grants file: every ZettaTunnel that is not being deleted, is valid
// and has not expired, ordered by id. clientCAs is the union of their client CAs.
func liveZettaTunnelGrants(tunnels []oramav1alpha1.ZettaTunnel, now time.Time) ([]zettaTunnelGrant, [][]byte, time.Time) {
	var grants []zettaTunnelGrant
	var cas [][]byte
	var nextExpiry time.Time
	for i := range tunnels {
		tunnel := &tunnels[i]
		if !tunnel.DeletionTimestamp.IsZero() || !now.Before(tunnel.Spec.ExpiresAt.Time) {
			continue
		}
		if validateZettaTunnel(tunnel) != "" {
			continue
		}
		fingerprints, err := zettaTunnelClientCAFingerprints(tunnel.Spec.ClientCAPEM)
		if err != nil {
			// validateZettaTunnel refuses the same input; a grant without fingerprints would make
			// the relay refuse the whole snapshot, so it is left out instead.
			continue
		}
		grants = append(grants, zettaTunnelGrant{
			ID:             tunnel.Spec.GrantID,
			Workspace:      tunnel.Namespace,
			Endpoint:       zettaTunnelEndpointAddress(tunnel),
			ExpiresAt:      tunnel.Spec.ExpiresAt.UTC().Format(time.RFC3339),
			ClientCASHA256: fingerprints,
		})
		cas = append(cas, []byte(tunnel.Spec.ClientCAPEM))
		if nextExpiry.IsZero() || tunnel.Spec.ExpiresAt.Time.Before(nextExpiry) {
			nextExpiry = tunnel.Spec.ExpiresAt.Time
		}
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
	return grants, cas, nextExpiry
}

func (r *zettaTunnelRelayReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("zetta-tunnel-relay")
	var tunnels oramav1alpha1.ZettaTunnelList
	if err := r.List(ctx, &tunnels); err != nil {
		return ctrl.Result{}, err
	}
	if len(tunnels.Items) == 0 {
		return ctrl.Result{}, r.removeRelay(ctx)
	}
	now := r.now()
	namespace, err := ensureOperatorNamespace(ctx, r.APIReader, r.Client, zettaTunnelNamespace, tunnelNamespaceLabels())
	if err != nil {
		return ctrl.Result{}, err
	}
	pki, err := ensureZettaTunnelPKI(ctx, r.APIReader, r.Client, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileSandbox(ctx, namespace); err != nil {
		return ctrl.Result{}, err
	}
	grants, clientCAs, nextExpiry := liveZettaTunnelGrants(tunnels.Items, now)
	pullSecret, err := r.reconcilePullSecret(ctx, tunnels.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileGrants(ctx, grants, append(clientCAs, pki.UnusableCA)); err != nil {
		return ctrl.Result{}, err
	}
	service, err := r.reconcileService(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDisruptionBudget(ctx); err != nil {
		return ctrl.Result{}, err
	}
	workloadHash, err := r.reconcileWorkloadCertificate(ctx, pki, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	address := zettaTunnelRelayAddress(service)
	if address == "" {
		log.Info("waiting for the relay's load balancer address")
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, nil
	}
	serverHash, err := r.reconcileServerCertificate(ctx, pki, service, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDeployment(ctx, pullSecret, serverHash+workloadHash); err != nil {
		return ctrl.Result{}, err
	}
	if !nextExpiry.IsZero() {
		return ctrl.Result{RequeueAfter: min(nextExpiry.Sub(now)+time.Second, tunnelExpiryRequeueCap)}, nil
	}
	return ctrl.Result{}, nil
}

// removeRelay deletes the relay when no tunnel needs it. The namespace, its sandbox and the CAs stay.
func (r *zettaTunnelRelayReconciler) removeRelay(ctx context.Context) error {
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayGrants}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayServerTLS}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayWorkTLS}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayPull}},
	} {
		object.SetNamespace(zettaTunnelNamespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if object.GetLabels()[workspaceManagedByLabel] != zettaTunnelManagedBy {
			continue
		}
		if err := r.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func zettaTunnelRelayObjectLabels() map[string]string {
	return withLabels(zettaTunnelRelayLabels(), map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy})
}

func (r *zettaTunnelRelayReconciler) reconcileSandbox(ctx context.Context, namespace *corev1.Namespace) error {
	relayPods := metav1.LabelSelector{MatchLabels: zettaTunnelRelayLabels()}
	public := []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}},
	}
	policies := []oramav1alpha1.NetworkSandboxPolicy{
		sandboxAllowance(
			"Developers reach the relay's public mutual-TLS listener; the relay authenticates every connection itself.",
			networkPolicy("relay-public-ingress", relayPods, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				[]networkingv1.NetworkPolicyIngressRule{{From: public, Ports: tcpPorts(zettaTunnelRelayPort)}}, nil),
		),
		sandboxAllowance(
			"The relay reaches only tunnel endpoints, on their mutual-TLS port, in workspace namespaces.",
			networkPolicy("relay-endpoint-egress", relayPods, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, nil,
				[]networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
							Key: workspaceLabel, Operator: metav1.LabelSelectorOpExists,
						}}},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{zettaTunnelComponentKey: "endpoint"},
							MatchExpressions: []metav1.LabelSelectorRequirement{{
								Key: connectionTunnelIDLabel, Operator: metav1.LabelSelectorOpExists,
							}},
						},
					}},
					Ports: tcpPorts(zettaTunnelEndpointPort),
				}}),
		),
		sandboxAllowance(
			"The relay resolves endpoint Service names through cluster DNS.",
			workspaceDNSPolicy(zettaTunnelNamespace, nil),
		),
	}
	_, err := reconcileOwnedNetworkSandbox(ctx, r.Client, r.Scheme, namespace, zettaTunnelNamespace,
		map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy}, metav1.LabelSelector{}, policies)
	return err
}

// reconcilePullSecret copies the first (by name) Lake's pull secret that the tunnels name into the
// relay's namespace, so the relay can pull the private image.
func (r *zettaTunnelRelayReconciler) reconcilePullSecret(ctx context.Context, tunnels []oramav1alpha1.ZettaTunnel) (string, error) {
	lakes := map[string]bool{}
	for i := range tunnels {
		lakes[tunnels[i].Spec.LakeRef] = true
	}
	names := make([]string, 0, len(lakes))
	for name := range lakes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var lake oramav1alpha1.Lake
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &lake); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		if lake.Spec.PullSecretName == "" {
			continue
		}
		var source corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(&lake), Name: lake.Spec.PullSecretName}, &source); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayPull, Namespace: zettaTunnelNamespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.ResourceVersion != "" && secret.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
				return fmt.Errorf("Secret %s/%s is not the operator's", zettaTunnelNamespace, zettaTunnelRelayPull)
			}
			secret.Labels = zettaTunnelRelayObjectLabels()
			secret.Type = source.Type
			secret.Data = make(map[string][]byte, len(source.Data))
			for key, value := range source.Data {
				secret.Data[key] = append([]byte(nil), value...)
			}
			return nil
		})
		return zettaTunnelRelayPull, err
	}
	return "", nil
}

func zettaTunnelGrantsData(grants []zettaTunnelGrant, clientCAs [][]byte) (map[string]string, error) {
	if grants == nil {
		grants = []zettaTunnelGrant{}
	}
	document, err := json.Marshal(zettaTunnelGrantsDocument{Grants: grants})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		zettaTunnelGrantsKey:   string(document) + "\n",
		zettaTunnelClientCAKey: string(pemBundle(clientCAs...)),
	}, nil
}

func (r *zettaTunnelRelayReconciler) reconcileGrants(ctx context.Context, grants []zettaTunnelGrant, clientCAs [][]byte) error {
	data, err := zettaTunnelGrantsData(grants, clientCAs)
	if err != nil {
		return err
	}
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayGrants, Namespace: zettaTunnelNamespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if configMap.ResourceVersion != "" && configMap.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("ConfigMap %s/%s is not the operator's", zettaTunnelNamespace, zettaTunnelRelayGrants)
		}
		configMap.Labels = zettaTunnelRelayObjectLabels()
		configMap.Data = data
		return nil
	})
	return err
}

func (r *zettaTunnelRelayReconciler) reconcileService(ctx context.Context) (*corev1.Service, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName, Namespace: zettaTunnelNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.ResourceVersion != "" && service.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Service %s/%s is not the operator's", zettaTunnelNamespace, zettaTunnelRelayName)
		}
		service.Labels = zettaTunnelRelayObjectLabels()
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		// Local keeps the developer's source address for the audit records and makes the cloud load
		// balancer probe only nodes with a ready relay.
		service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
		service.Spec.Selector = zettaTunnelRelayLabels()
		service.Spec.Ports = []corev1.ServicePort{{
			Name: "tunnel", Protocol: corev1.ProtocolTCP, Port: zettaTunnelRelayPublic, TargetPort: intstr.FromInt32(zettaTunnelRelayPort),
		}}
		return nil
	})
	return service, err
}

func (r *zettaTunnelRelayReconciler) reconcileDisruptionBudget(ctx context.Context) error {
	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName, Namespace: zettaTunnelNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, budget, func() error {
		if budget.ResourceVersion != "" && budget.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("PodDisruptionBudget %s/%s is not the operator's", zettaTunnelNamespace, zettaTunnelRelayName)
		}
		budget.Labels = zettaTunnelRelayObjectLabels()
		minAvailable := intstr.FromInt32(1)
		budget.Spec.MinAvailable = &minAvailable
		budget.Spec.Selector = &metav1.LabelSelector{MatchLabels: zettaTunnelRelayLabels()}
		return nil
	})
	return err
}

func zettaTunnelRelayServerRequest(service *corev1.Service) tunnelLeafRequest {
	request := tunnelLeafRequest{
		CommonName: "zetta-tunnel relay",
		DNSNames:   []string{fmt.Sprintf("%s.%s.svc", zettaTunnelRelayName, zettaTunnelNamespace)},
		Server:     true,
	}
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ip := net.ParseIP(ingress.IP); ip != nil {
			request.IPs = append(request.IPs, ip)
		}
		if ingress.Hostname != "" {
			request.DNSNames = append(request.DNSNames, ingress.Hostname)
		}
	}
	return request
}

// reconcileTLSSecret keeps a leaf Secret signed by ca for request, reissuing it when it is wrong
// or close to expiry. It returns a hash that changes when the certificate does.
func reconcileOperatorTLSSecret(
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
	labels map[string]string,
	ca tunnelCA,
	request tunnelLeafRequest,
	now time.Time,
) (string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && secret.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Secret %s/%s is not the operator's", namespace, name)
		}
		secret.Labels = withLabels(secret.Labels, labels)
		secret.Type = corev1.SecretTypeTLS
		if !tunnelLeafCurrent(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], ca.CertPEM, request, now) {
			cert, key, err := issueTunnelLeaf(ca, request, now)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key}
		}
		secret.Data["ca.crt"] = append([]byte(nil), ca.CertPEM...)
		return nil
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(secret.Data[corev1.TLSCertKey], secret.Data["ca.crt"]), nil
}

func (r *zettaTunnelRelayReconciler) reconcileServerCertificate(ctx context.Context, pki zettaTunnelPKI, service *corev1.Service, now time.Time) (string, error) {
	return reconcileOperatorTLSSecret(ctx, r.Client, zettaTunnelNamespace, zettaTunnelRelayServerTLS,
		zettaTunnelRelayObjectLabels(), pki.Relay, zettaTunnelRelayServerRequest(service), now)
}

func (r *zettaTunnelRelayReconciler) reconcileWorkloadCertificate(ctx context.Context, pki zettaTunnelPKI, now time.Time) (string, error) {
	return reconcileOperatorTLSSecret(ctx, r.Client, zettaTunnelNamespace, zettaTunnelRelayWorkTLS,
		zettaTunnelRelayObjectLabels(), pki.Workload, tunnelLeafRequest{
			CommonName: zettaTunnelRelayIdentity,
			DNSNames:   []string{zettaTunnelRelayIdentity},
			Client:     true,
		}, now)
}

func (r *zettaTunnelRelayReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultZettaTunnelImage
}

func (r *zettaTunnelRelayReconciler) reconcileDeployment(ctx context.Context, pullSecret, certificateHash string) error {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelRelayName, Namespace: zettaTunnelNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" && deployment.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("Deployment %s/%s is not the operator's", zettaTunnelNamespace, zettaTunnelRelayName)
		}
		deployment.Labels = zettaTunnelRelayObjectLabels()
		deployment.Spec.Replicas = int32Ptr(zettaTunnelRelayReplicas)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: zettaTunnelRelayLabels()}
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: intstrPtr(intstr.FromInt32(0)),
				MaxSurge:       intstrPtr(intstr.FromInt32(1)),
			},
		}
		deployment.Spec.Template.Labels = zettaTunnelRelayObjectLabels()
		// Only a certificate change restarts the relay. Grant changes reach it through the mounted
		// file, which it re-reads every two seconds, so live sessions survive them.
		deployment.Spec.Template.Annotations = map[string]string{"orama.cloud/tunnel-certificate": certificateHash}
		deployment.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			ImagePullSecrets:              pullSecretsFor(pullSecret),
			SecurityContext:               tunnelPodSecurityContext(),
			TerminationGracePeriodSeconds: int64Ptr(30),
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       "kubernetes.io/hostname",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: zettaTunnelRelayLabels()},
			}},
			Containers: []corev1.Container{{
				Name:            "relay",
				Image:           r.image(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{
					"relay",
					"--cert", "/etc/zetta-tunnel/server/tls.crt",
					"--key", "/etc/zetta-tunnel/server/tls.key",
					"--ca", "/etc/zetta-tunnel/grants/" + zettaTunnelClientCAKey,
					"--grants", "/etc/zetta-tunnel/grants/" + zettaTunnelGrantsKey,
					"--endpoint-cert", "/etc/zetta-tunnel/workload/tls.crt",
					"--endpoint-key", "/etc/zetta-tunnel/workload/tls.key",
					"--endpoint-ca", "/etc/zetta-tunnel/workload/ca.crt",
					"--listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelRelayPort),
					"--health-listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelProbePort),
					"--max-sessions", "1024",
					"--audit", "-",
				},
				Ports: []corev1.ContainerPort{
					{Name: "tunnel", ContainerPort: zettaTunnelRelayPort, Protocol: corev1.ProtocolTCP},
					{Name: "probe", ContainerPort: zettaTunnelProbePort, Protocol: corev1.ProtocolTCP},
				},
				ReadinessProbe: zettaTunnelProbe("/readyz", 5),
				LivenessProbe:  zettaTunnelProbe("/healthz", 10),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
				SecurityContext: tunnelContainerSecurityContext(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "server", MountPath: "/etc/zetta-tunnel/server", ReadOnly: true},
					{Name: "workload", MountPath: "/etc/zetta-tunnel/workload", ReadOnly: true},
					{Name: "grants", MountPath: "/etc/zetta-tunnel/grants", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "server", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: zettaTunnelRelayServerTLS, DefaultMode: int32Ptr(0o440),
				}}},
				{Name: "workload", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: zettaTunnelRelayWorkTLS, DefaultMode: int32Ptr(0o440),
				}}},
				// Mounted without subPath so kubelet's atomic ..data swap reaches the relay.
				{Name: "grants", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: zettaTunnelRelayGrants}, DefaultMode: int32Ptr(0o444),
				}}},
			},
		}
		return nil
	})
	return err
}

func (r *zettaTunnelRelayReconciler) relayRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{zettaTunnelRelayRequest}
}

func (r *zettaTunnelRelayReconciler) relayObjectRequest(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() != zettaTunnelNamespace {
		return nil
	}
	return r.relayRequest(ctx, object)
}

func (r *zettaTunnelRelayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	objects := handler.EnqueueRequestsFromMapFunc(r.relayObjectRequest)
	return ctrl.NewControllerManagedBy(mgr).
		Named("zettatunnel-relay").
		Watches(&oramav1alpha1.ZettaTunnel{}, handler.EnqueueRequestsFromMapFunc(r.relayRequest)).
		Watches(&appsv1.Deployment{}, objects).
		Watches(&corev1.Service{}, objects).
		Watches(&corev1.ConfigMap{}, objects).
		Watches(&corev1.Secret{}, objects).
		Watches(&policyv1.PodDisruptionBudget{}, objects).
		Watches(&oramav1alpha1.NetworkSandbox{}, objects).
		Complete(r)
}

// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
