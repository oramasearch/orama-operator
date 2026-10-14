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
	"sort"
	"strings"
	"time"

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	zettaTunnelFinalizer = "orama.cloud/zetta-tunnel"
	tunnelReadyCondition = "Ready"
	// tunnelPendingRequeue is how soon a waiting tunnel looks again.
	tunnelPendingRequeue = 5 * time.Second
	// tunnelExpiryRequeueCap bounds how long a live tunnel goes unreconciled before its expiry.
	tunnelExpiryRequeueCap = 10 * time.Minute
)

// zettaTunnelEndpointName is the endpoint's Deployment, Service and pod-name prefix in the workspace.
func zettaTunnelEndpointName(grantID string) string { return "tunnel-" + grantID }

func zettaTunnelEndpointSecret(grantID string) string { return "tunnel-" + grantID + "-endpoint" }

// tunnelConnectionName is the generated Connection from the tunnel's workload to its target. Its
// name keeps the Connection allowance naming the workspace renderer preserves.
func tunnelConnectionName(grantID string) string { return "connection-tunnel-" + grantID }

// zettaTunnelRelayAllowance is the workspace NetworkSandbox allowance that admits the relay to one
// endpoint.
func zettaTunnelRelayAllowance(grantID string) string { return "tunnel-" + grantID + "-relay" }

// ZettaTunnelReconciler runs one zetta-tunnel endpoint per grant in the workspace, fixed to the one
// target, writes the narrow Connection from the endpoint to that target, and admits the relay to the
// endpoint. The relay itself is the cluster's one zettaTunnelRelayReconciler.
type ZettaTunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Image is the zetta-tunnel image; empty means DefaultZettaTunnelImage.
	Image string
	// Now is injected for tests; nil means time.Now.
	Now func() time.Time
	// APIReader reads the manager's own pod (its pull secrets) without a pod informer.
	APIReader client.Reader
	// OperatorNamespace is the manager's namespace.
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=orama.cloud,resources=zettatunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=zettatunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=zettatunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=networksandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=lakes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *ZettaTunnelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ZettaTunnelReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultZettaTunnelImage
}

func (r *ZettaTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tunnel oramav1alpha1.ZettaTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tunnel.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&tunnel, zettaTunnelFinalizer) {
			return ctrl.Result{}, nil
		}
		done, err := r.teardown(ctx, &tunnel)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		controllerutil.RemoveFinalizer(&tunnel, zettaTunnelFinalizer)
		return ctrl.Result{}, r.Update(ctx, &tunnel)
	}
	if !controllerutil.ContainsFinalizer(&tunnel, zettaTunnelFinalizer) {
		controllerutil.AddFinalizer(&tunnel, zettaTunnelFinalizer)
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if message := validateZettaTunnel(&tunnel); message != "" {
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhaseFailed, message, "", nil, "")
	}
	now := r.now()
	if !now.Before(tunnel.Spec.ExpiresAt.Time) {
		done, err := r.teardown(ctx, &tunnel)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhaseExpired, "the grant has expired; its endpoint is removed", "", nil, "")
	}
	result, err := r.reconcileLive(ctx, &tunnel, now)
	if err != nil {
		if isPending(err) {
			statusErr := r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhasePending, err.Error(), "", nil, tunnel.Status.Target)
			return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, statusErr
		}
		var invalid zettaTunnelInvalid
		if !errors.As(err, &invalid) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhaseFailed, err.Error(), "", nil, "")
	}
	return result, nil
}

// zettaTunnelInvalid is a refusal that a retry cannot fix.
type zettaTunnelInvalid struct{ message string }

func (e zettaTunnelInvalid) Error() string { return e.message }

func validateZettaTunnel(tunnel *oramav1alpha1.ZettaTunnel) string {
	for name, value := range map[string]string{
		"grantId":     tunnel.Spec.GrantID,
		"customerId":  tunnel.Spec.CustomerID,
		"workspaceId": tunnel.Spec.WorkspaceID,
		"targetId":    tunnel.Spec.TargetID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return name + " must be a canonical lowercase UUID"
		}
	}
	if tunnel.Name != zettaTunnelEndpointName(tunnel.Spec.GrantID) {
		return "a ZettaTunnel is named tunnel-<grantId>"
	}
	if tunnel.Spec.Port < 1 || tunnel.Spec.Port > 65535 {
		return "port must be between 1 and 65535"
	}
	if tunnel.Spec.TargetKind != oramav1alpha1.TunnelTargetService && tunnel.Spec.TargetKind != oramav1alpha1.TunnelTargetApp {
		return "targetKind must be service or app"
	}
	certs, err := pemCertificates([]byte(tunnel.Spec.ClientCAPEM))
	if err != nil {
		return "clientCAPEM must be PEM certificates: " + err.Error()
	}
	for _, cert := range certs {
		if !cert.IsCA {
			return "clientCAPEM must contain only CA certificates"
		}
	}
	return ""
}

func (r *ZettaTunnelReconciler) reconcileLive(
	ctx context.Context,
	tunnel *oramav1alpha1.ZettaTunnel,
	now time.Time,
) (ctrl.Result, error) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: tunnel.Spec.LakeRef}, &lake); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, zettaTunnelInvalid{message: fmt.Sprintf("Lake %q does not exist", tunnel.Spec.LakeRef)}
		}
		return ctrl.Result{}, err
	}
	sandbox, err := tunnelWorkspaceSandbox(ctx, r.Client, tunnel.Namespace, &lake)
	if err != nil {
		return ctrl.Result{}, err
	}
	target, err := resolveTunnelTarget(ctx, r.Client, tunnel.Spec.LakeRef, tunnel.Namespace, tunnel.Spec.TargetKind, tunnel.Spec.TargetID, tunnel.Spec.Port)
	if err != nil {
		if isPending(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, zettaTunnelInvalid{message: err.Error()}
	}
	pki, err := ensureZettaTunnelPKI(ctx, r.Client, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	secretHash, err := r.reconcileEndpointSecret(ctx, tunnel, pki, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileEndpointService(ctx, tunnel); err != nil {
		return ctrl.Result{}, err
	}
	deployment, err := r.reconcileEndpointDeployment(ctx, tunnel, &lake, target, secretHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	connection, err := reconcileTunnelConnection(ctx, r.Client, r.Scheme, tunnel, tunnel.Spec.TunnelGrantSpec, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := setTunnelSandboxAllowances(ctx, r.Client, r.Scheme, sandbox, tunnel.Spec.GrantID, zettaTunnelEndpointAllowances(tunnel.Spec.GrantID)); err != nil {
		return ctrl.Result{}, err
	}

	requeue := time.Until(tunnel.Spec.ExpiresAt.Time)
	if r.Now != nil {
		requeue = tunnel.Spec.ExpiresAt.Time.Sub(now)
	}
	requeue = min(requeue+time.Second, tunnelExpiryRequeueCap)

	relay, err := readZettaTunnelRelay(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.AvailableReplicas < 1:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the endpoint to become available", "", nil, target.Address())
	case connection.Status.Phase != oramav1alpha1.ConnectionPhaseReady || connection.Status.ObservedGeneration != connection.Spec.Generation:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the endpoint's Connection to its target", "", nil, target.Address())
	case relay.Address == "":
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the relay's public address", "", nil, target.Address())
	case !relay.Available:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the relay to become available", "", nil, target.Address())
	case !relay.Grants[tunnel.Spec.GrantID]:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the relay's grants file to carry this grant", "", nil, target.Address())
	}
	return ctrl.Result{RequeueAfter: requeue}, r.setStatus(
		ctx, tunnel, oramav1alpha1.TunnelPhaseReady, "the relay serves this grant; the endpoint reaches only its target",
		relay.Address, pki.Relay.CertPEM, target.Address(),
	)
}

func (r *ZettaTunnelReconciler) reconcileEndpointSecret(
	ctx context.Context,
	tunnel *oramav1alpha1.ZettaTunnel,
	pki zettaTunnelPKI,
	now time.Time,
) (string, error) {
	request := tunnelLeafRequest{
		CommonName: zettaTunnelEndpointIdentity(tunnel.Spec.GrantID),
		DNSNames:   []string{zettaTunnelEndpointIdentity(tunnel.Spec.GrantID)},
		Server:     true,
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointSecret(tunnel.Spec.GrantID), Namespace: tunnel.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, tunnel) {
			return fmt.Errorf("Secret %s/%s is not this tunnel's", secret.Namespace, secret.Name)
		}
		secret.Labels = withLabels(secret.Labels, zettaTunnelEndpointLabels(tunnel.Spec.GrantID))
		secret.Type = corev1.SecretTypeTLS
		if !tunnelLeafCurrent(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], pki.Workload.CertPEM, request, now) {
			cert, key, err := issueTunnelLeaf(pki.Workload, request, now)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key}
		}
		secret.Data["ca.crt"] = append([]byte(nil), pki.Workload.CertPEM...)
		return controllerutil.SetControllerReference(tunnel, secret, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(secret.Data[corev1.TLSCertKey], secret.Data["ca.crt"]), nil
}

func zettaTunnelEndpointLabels(grantID string) map[string]string {
	return map[string]string{
		zettaTunnelNameKey:      zettaTunnelAppName,
		zettaTunnelComponentKey: "endpoint",
		connectionTunnelIDLabel: grantID,
		workspaceManagedByLabel: zettaTunnelManagedBy,
	}
}

func (r *ZettaTunnelReconciler) reconcileEndpointService(ctx context.Context, tunnel *oramav1alpha1.ZettaTunnel) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(tunnel.Spec.GrantID), Namespace: tunnel.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.ResourceVersion != "" && !metav1.IsControlledBy(service, tunnel) {
			return fmt.Errorf("Service %s/%s is not this tunnel's", service.Namespace, service.Name)
		}
		service.Labels = withLabels(service.Labels, zettaTunnelEndpointLabels(tunnel.Spec.GrantID))
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Selector = map[string]string{connectionTunnelIDLabel: tunnel.Spec.GrantID, zettaTunnelComponentKey: "endpoint"}
		service.Spec.Ports = []corev1.ServicePort{{
			Name: "tunnel", Protocol: corev1.ProtocolTCP, Port: zettaTunnelEndpointPort, TargetPort: intstr.FromInt32(zettaTunnelEndpointPort),
		}}
		return controllerutil.SetControllerReference(tunnel, service, r.Scheme)
	})
	return err
}

func zettaTunnelEndpointAddress(tunnel *oramav1alpha1.ZettaTunnel) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", zettaTunnelEndpointName(tunnel.Spec.GrantID), tunnel.Namespace, zettaTunnelEndpointPort)
}

func (r *ZettaTunnelReconciler) reconcileEndpointDeployment(
	ctx context.Context,
	tunnel *oramav1alpha1.ZettaTunnel,
	lake *oramav1alpha1.Lake,
	target tunnelTarget,
	secretHash string,
) (*appsv1.Deployment, error) {
	labels := zettaTunnelEndpointLabels(tunnel.Spec.GrantID)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(tunnel.Spec.GrantID), Namespace: tunnel.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" && !metav1.IsControlledBy(deployment, tunnel) {
			return fmt.Errorf("Deployment %s/%s is not this tunnel's", deployment.Namespace, deployment.Name)
		}
		deployment.Labels = withLabels(deployment.Labels, labels)
		deployment.Spec.Replicas = int32Ptr(1)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
			connectionTunnelIDLabel: tunnel.Spec.GrantID, zettaTunnelComponentKey: "endpoint",
		}}
		deployment.Spec.Template.Labels = labels
		deployment.Spec.Template.Annotations = map[string]string{"orama.cloud/tunnel-certificate": secretHash}
		deployment.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			ImagePullSecrets:              pullSecretsFor(lake.Spec.PullSecretName),
			SecurityContext:               tunnelPodSecurityContext(),
			TerminationGracePeriodSeconds: int64Ptr(10),
			Containers: []corev1.Container{{
				Name:            "endpoint",
				Image:           r.image(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{
					"endpoint",
					"--cert", "/etc/zetta-tunnel/tls/tls.crt",
					"--key", "/etc/zetta-tunnel/tls/tls.key",
					"--ca", "/etc/zetta-tunnel/tls/ca.crt",
					"--peer-name", zettaTunnelRelayIdentity,
					"--target", target.Address(),
					"--listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelEndpointPort),
					"--health-listen", fmt.Sprintf("0.0.0.0:%d", zettaTunnelProbePort),
					"--audit", "-",
				},
				Ports: []corev1.ContainerPort{
					{Name: "tunnel", ContainerPort: zettaTunnelEndpointPort, Protocol: corev1.ProtocolTCP},
					{Name: "probe", ContainerPort: zettaTunnelProbePort, Protocol: corev1.ProtocolTCP},
				},
				ReadinessProbe: zettaTunnelProbe("/readyz", 5),
				LivenessProbe:  zettaTunnelProbe("/healthz", 10),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				SecurityContext: tunnelContainerSecurityContext(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "tls", MountPath: "/etc/zetta-tunnel/tls", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name: "tls",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: zettaTunnelEndpointSecret(tunnel.Spec.GrantID), DefaultMode: int32Ptr(0o440),
				}},
			}},
		}
		return controllerutil.SetControllerReference(tunnel, deployment, r.Scheme)
	})
	return deployment, err
}

// tunnelWorkspaceSandbox returns the Lake-controlled workspace NetworkSandbox of namespace.
func tunnelWorkspaceSandbox(
	ctx context.Context,
	c client.Client,
	namespace string,
	lake *oramav1alpha1.Lake,
) (*oramav1alpha1.NetworkSandbox, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, connectionPendingError{message: "waiting for the workspace NetworkSandbox"}
		}
		return nil, err
	}
	if !metav1.IsControlledBy(&sandbox, lake) {
		return nil, zettaTunnelInvalid{message: fmt.Sprintf("NetworkSandbox %s/%s is not controlled by Lake %q", namespace, networkSandboxName, lake.Name)}
	}
	return &sandbox, nil
}

// zettaTunnelEndpointAllowances admits the relay, and only the relay, to the endpoint's port.
func zettaTunnelEndpointAllowances(grantID string) []oramav1alpha1.NetworkSandboxPolicy {
	return []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(
		fmt.Sprintf("The zetta-tunnel relay reaches tunnel grant %s's endpoint, and nothing else in the workspace.", grantID),
		networkPolicy(
			zettaTunnelRelayAllowance(grantID),
			metav1.LabelSelector{MatchLabels: map[string]string{connectionTunnelIDLabel: grantID, zettaTunnelComponentKey: "endpoint"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			[]networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{namespacePodPeer(zettaTunnelNamespace, zettaTunnelRelayLabels())},
				Ports: tcpPorts(zettaTunnelEndpointPort),
			}},
			nil,
		),
	)}
}

// tunnelAllowanceNames lists every sandbox allowance a tunnel of grantID may own.
func tunnelAllowanceNames(grantID string) []string {
	return []string{zettaTunnelRelayAllowance(grantID), cloudflareTunnelEgressAllowance(grantID)}
}

// setTunnelSandboxAllowances replaces grantID's tunnel allowances in the workspace sandbox with
// allowances (nil removes them) and renders the sandbox.
func setTunnelSandboxAllowances(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	sandbox *oramav1alpha1.NetworkSandbox,
	grantID string,
	allowances []oramav1alpha1.NetworkSandboxPolicy,
) error {
	names := map[string]bool{}
	for _, name := range tunnelAllowanceNames(grantID) {
		names[name] = true
	}
	kept := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(sandbox.Spec.Policies)+len(allowances))
	for _, policy := range sandbox.Spec.Policies {
		if !names[policy.Name] {
			kept = append(kept, policy)
		}
	}
	kept = append(kept, allowances...)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if !apiequality.Semantic.DeepEqual(sandbox.Spec.Policies, kept) {
		sandbox.Spec.Policies = kept
		if err := c.Update(ctx, sandbox); err != nil {
			return fmt.Errorf("update NetworkSandbox tunnel allowances: %w", err)
		}
	}
	_, _, err := reconcileNetworkSandboxPolicies(ctx, c, scheme, sandbox)
	return err
}

// reconcileTunnelConnection writes the Connection from a tunnel's workload to its target. owner is
// the concrete tunnel resource; the Connection controller accepts it because owner names the same
// Lake and grant.
func reconcileTunnelConnection(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	grant oramav1alpha1.TunnelGrantSpec,
	target tunnelTarget,
) (*oramav1alpha1.Connection, error) {
	connection := &oramav1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: tunnelConnectionName(grant.GrantID), Namespace: owner.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, connection, func() error {
		if connection.ResourceVersion != "" && !metav1.IsControlledBy(connection, owner) {
			return fmt.Errorf("Connection %s/%s is not this tunnel's", connection.Namespace, connection.Name)
		}
		connection.Labels = withLabels(connection.Labels, map[string]string{
			connectionTunnelIDLabel: grant.GrantID,
			workspaceLabel:          grant.WorkspaceID,
		})
		connection.Spec = oramav1alpha1.ConnectionSpec{
			LakeRef:      grant.LakeRef,
			ConnectionID: grant.GrantID,
			CustomerID:   grant.CustomerID,
			WorkspaceID:  grant.WorkspaceID,
			SourceKind:   oramav1alpha1.ConnectionSourceTunnel,
			SourceID:     grant.GrantID,
			TargetKind:   grant.TargetKind,
			TargetID:     grant.TargetID,
			Ports:        []int32{target.PodPort},
			Desired:      oramav1alpha1.ConnectionDesiredPresent,
			Generation:   1,
		}
		return controllerutil.SetControllerReference(owner, connection, scheme)
	})
	return connection, err
}

// teardownTunnelWorkload removes, in fail-closed order, the allowances, the Connection and then
// named workload objects of a tunnel. It reports done once the Connection is gone.
func teardownTunnelWorkload(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	namespace string,
	grantID string,
	objects ...client.Object,
) (bool, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err == nil {
		if err := setTunnelSandboxAllowances(ctx, c, scheme, &sandbox, grantID, nil); err != nil {
			return false, err
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	for _, object := range objects {
		object.SetNamespace(namespace)
		if err := c.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	connection := &oramav1alpha1.Connection{}
	key := client.ObjectKey{Namespace: namespace, Name: tunnelConnectionName(grantID)}
	if err := c.Get(ctx, key, connection); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if connection.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, connection); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

func (r *ZettaTunnelReconciler) teardown(ctx context.Context, tunnel *oramav1alpha1.ZettaTunnel) (bool, error) {
	name := zettaTunnelEndpointName(tunnel.Spec.GrantID)
	return teardownTunnelWorkload(ctx, r.Client, r.Scheme, tunnel.Namespace, tunnel.Spec.GrantID,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointSecret(tunnel.Spec.GrantID)}},
	)
}

func (r *ZettaTunnelReconciler) setStatus(
	ctx context.Context,
	tunnel *oramav1alpha1.ZettaTunnel,
	phase string,
	message string,
	endpoint string,
	relayCA []byte,
	target string,
) error {
	conditionStatus := metav1.ConditionFalse
	if phase == oramav1alpha1.TunnelPhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	desired := oramav1alpha1.ZettaTunnelStatus{
		Phase:      phase,
		Message:    message,
		Endpoint:   endpoint,
		RelayCAPEM: string(relayCA),
		Target:     target,
		Conditions: append([]metav1.Condition(nil), tunnel.Status.Conditions...),
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               tunnelReadyCondition,
		Status:             conditionStatus,
		Reason:             phase,
		Message:            message,
		ObservedGeneration: tunnel.Generation,
	})
	if apiequality.Semantic.DeepEqual(tunnel.Status, desired) {
		return nil
	}
	tunnel.Status = desired
	return r.Status().Update(ctx, tunnel)
}

// zettaTunnelRelayState is what the relay currently is, read from its objects.
type zettaTunnelRelayState struct {
	Address   string
	Available bool
	Grants    map[string]bool
}

func readZettaTunnelRelay(ctx context.Context, c client.Client) (zettaTunnelRelayState, error) {
	state := zettaTunnelRelayState{Grants: map[string]bool{}}
	var service corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &service); err == nil {
		state.Address = zettaTunnelRelayAddress(&service)
	} else if !apierrors.IsNotFound(err) {
		return state, err
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayName}, &deployment); err == nil {
		state.Available = deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.UpdatedReplicas >= 1 && deployment.Status.AvailableReplicas >= 1
	} else if !apierrors.IsNotFound(err) {
		return state, err
	}
	var grants corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: zettaTunnelNamespace, Name: zettaTunnelRelayGrants}, &grants); err == nil {
		var document zettaTunnelGrantsDocument
		if json.Unmarshal([]byte(grants.Data[zettaTunnelGrantsKey]), &document) == nil {
			for _, grant := range document.Grants {
				state.Grants[grant.ID] = true
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return state, err
	}
	return state, nil
}

func zettaTunnelRelayAddress(service *corev1.Service) string {
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		host := ingress.IP
		if host == "" {
			host = ingress.Hostname
		}
		if host == "" {
			continue
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return fmt.Sprintf("%s:%d", host, zettaTunnelRelayPublic)
	}
	return ""
}

func (r *ZettaTunnelReconciler) tunnelsForRelay(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() != zettaTunnelNamespace {
		return nil
	}
	var tunnels oramav1alpha1.ZettaTunnelList
	if err := r.List(ctx, &tunnels); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
	}
	return requests
}

func (r *ZettaTunnelReconciler) tunnelsInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var tunnels oramav1alpha1.ZettaTunnelList
	if err := r.List(ctx, &tunnels, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
	}
	return requests
}

// SetupWithManager starts zetta-tunnel's controllers: one per ZettaTunnel (its endpoint), the
// cluster's one relay, which every ZettaTunnel feeds, the cluster's ingress gateway, and the
// mesh's node agent and projections.
func (r *ZettaTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	relayObjects := handler.EnqueueRequestsFromMapFunc(r.tunnelsForRelay)
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.ZettaTunnel{}).
		Owns(&appsv1.Deployment{}).
		Owns(&oramav1alpha1.Connection{}).
		Watches(&appsv1.Deployment{}, relayObjects).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			if object.GetNamespace() == zettaTunnelNamespace {
				return r.tunnelsForRelay(ctx, object)
			}
			return r.tunnelsInNamespace(ctx, object)
		})).
		Watches(&corev1.ConfigMap{}, relayObjects).
		Watches(&oramav1alpha1.NetworkSandbox{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsInNamespace)).
		Complete(r); err != nil {
		return err
	}
	relay := &zettaTunnelRelayReconciler{Client: r.Client, Scheme: r.Scheme, Image: r.Image, Now: r.Now}
	if err := relay.SetupWithManager(mgr); err != nil {
		return err
	}
	ingress := &zettaTunnelIngressReconciler{
		Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Image: r.Image, OperatorNamespace: r.OperatorNamespace,
	}
	if err := ingress.SetupWithManager(mgr); err != nil {
		return err
	}
	mesh := &zettaTunnelMeshReconciler{
		Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Image: r.Image, OperatorNamespace: r.OperatorNamespace, Now: r.Now,
	}
	return mesh.SetupWithManager(mgr)
}
