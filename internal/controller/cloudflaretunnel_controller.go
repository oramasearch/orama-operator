/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

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
	// DefaultCloudflaredImage is Cloudflare's connector (Apache 2.0), pinned.
	DefaultCloudflaredImage   = "cloudflare/cloudflared:2026.10.0"
	cloudflareTunnelFinalizer = "orama.cloud/cloudflare-tunnel"
	cloudflaredMetricsPort    = 2000
	// cloudflareAccessMaxSession bounds a Cloudflare Access session, as zetta-tunnel bounds a client
	// certificate.
	cloudflareAccessMaxSession = 12 * time.Hour
)

func cloudflareTunnelName(grantID string) string { return "orama-tunnel-" + grantID }

func cloudflareTunnelTokenSecret(grantID string) string { return "tunnel-" + grantID + "-cloudflared" }

func cloudflareTunnelEgressAllowance(grantID string) string {
	return "tunnel-" + grantID + "-cloudflare"
}

func cloudflareTunnelHostname(grantID, domain string) string {
	return "tunnel-" + grantID + "." + domain
}

func cloudflaredLabels(grantID string) map[string]string {
	return map[string]string{
		zettaTunnelNameKey:      "cloudflared",
		zettaTunnelComponentKey: "connector",
		connectionTunnelIDLabel: grantID,
		workspaceManagedByLabel: zettaTunnelManagedBy,
	}
}

// CloudflareTunnelReconciler carries one grant through Cloudflare Tunnel in a customer's own
// account: a remotely managed tunnel routed to the one target, a CNAME in the customer's zone, an
// Access application admitting only the grant's person, and a cloudflared connector in the
// workspace. It never uses an Orama Cloudflare account.
type CloudflareTunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIBase is the Cloudflare API base URL; empty means the public API. Tests point it at a fake.
	APIBase string
	// Image is the cloudflared image; empty means DefaultCloudflaredImage.
	Image string
	Now   func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflaretunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflaretunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflaretunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=tunnelproviders,verbs=get;list;watch

func (r *CloudflareTunnelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *CloudflareTunnelReconciler) api() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return cfAPIBase
}

// cloudflareAccount is the provider's account and the customer's token.
type cloudflareAccount struct {
	AccountID string
	ZoneID    string
	Domain    string
	Token     string
}

func (r *CloudflareTunnelReconciler) account(ctx context.Context, tunnel *oramav1alpha1.CloudflareTunnel) (cloudflareAccount, error) {
	var provider oramav1alpha1.TunnelProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Spec.ProviderRef}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return cloudflareAccount{}, connectionPendingError{message: fmt.Sprintf("waiting for TunnelProvider %q", tunnel.Spec.ProviderRef)}
		}
		return cloudflareAccount{}, err
	}
	if message := tunnelProviderRefusal(&provider, tunnel.Spec.CustomerID); message != "" {
		return cloudflareAccount{}, zettaTunnelInvalid{message: message}
	}
	if provider.Spec.Type != oramav1alpha1.TunnelProviderCloudflare {
		return cloudflareAccount{}, zettaTunnelInvalid{message: fmt.Sprintf("TunnelProvider %q is not a cloudflare provider", provider.Name)}
	}
	cloudflare := provider.Spec.Cloudflare
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: cloudflare.APITokenSecretRef.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return cloudflareAccount{}, connectionPendingError{message: fmt.Sprintf("waiting for the provider's API token Secret %q", cloudflare.APITokenSecretRef.Name)}
		}
		return cloudflareAccount{}, err
	}
	token := string(secret.Data[cloudflare.APITokenSecretRef.Key])
	if token == "" {
		return cloudflareAccount{}, connectionPendingError{message: fmt.Sprintf("Secret %q has no key %q", cloudflare.APITokenSecretRef.Name, cloudflare.APITokenSecretRef.Key)}
	}
	return cloudflareAccount{AccountID: cloudflare.AccountID, ZoneID: cloudflare.ZoneID, Domain: cloudflare.Domain, Token: token}, nil
}

func (r *CloudflareTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tunnel oramav1alpha1.CloudflareTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tunnel.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&tunnel, cloudflareTunnelFinalizer) {
			return ctrl.Result{}, nil
		}
		done, err := r.teardown(ctx, &tunnel)
		if err != nil {
			_ = r.setStatus(ctx, &tunnel, tunnel.Status.Phase, "cannot remove the Cloudflare objects yet: "+err.Error(), tunnel.Status.Endpoint)
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		controllerutil.RemoveFinalizer(&tunnel, cloudflareTunnelFinalizer)
		return ctrl.Result{}, r.Update(ctx, &tunnel)
	}
	if !controllerutil.ContainsFinalizer(&tunnel, cloudflareTunnelFinalizer) {
		controllerutil.AddFinalizer(&tunnel, cloudflareTunnelFinalizer)
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
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
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhaseExpired, "the grant has expired; its Cloudflare tunnel and Access application are removed", "")
	}
	result, err := r.reconcileLive(ctx, &tunnel, now)
	if err != nil {
		var invalid zettaTunnelInvalid
		switch {
		case isPending(err):
			return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhasePending, err.Error(), "")
		case errors.As(err, &invalid):
			return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhaseFailed, err.Error(), "")
		}
		var apiErr *cfAPIError
		if errors.As(err, &apiErr) {
			statusErr := r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelPhasePending,
				fmt.Sprintf("Cloudflare API %s answered %d", apiErr.Method, apiErr.StatusCode), "")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, statusErr
		}
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *CloudflareTunnelReconciler) reconcileLive(
	ctx context.Context,
	tunnel *oramav1alpha1.CloudflareTunnel,
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
	account, err := r.account(ctx, tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}
	hostname := cloudflareTunnelHostname(tunnel.Spec.GrantID, account.Domain)
	if err := r.ensureTunnel(ctx, tunnel, account); err != nil {
		return ctrl.Result{}, err
	}
	token, err := r.tunnelToken(ctx, account, tunnel.Status.TunnelID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.putTunnelConfiguration(ctx, account, tunnel.Status.TunnelID, hostname, target); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDNSRecord(ctx, tunnel, account, hostname); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureAccessApplication(ctx, tunnel, account, hostname, now); err != nil {
		return ctrl.Result{}, err
	}
	tokenHash, err := r.reconcileTokenSecret(ctx, tunnel, token)
	if err != nil {
		return ctrl.Result{}, err
	}
	deployment, err := r.reconcileConnector(ctx, tunnel, &lake, tokenHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	connection, err := reconcileTunnelConnection(ctx, r.Client, r.Scheme, tunnel, tunnel.Spec.TunnelGrantSpec, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	allowances, err := cloudflaredEgressAllowances(ctx, r.Client, tunnel.Spec.GrantID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := setTunnelSandboxAllowances(ctx, r.Client, r.Scheme, sandbox, tunnel.Spec.GrantID, allowances); err != nil {
		return ctrl.Result{}, err
	}
	requeue := min(tunnel.Spec.ExpiresAt.Time.Sub(now)+time.Second, tunnelExpiryRequeueCap)
	switch {
	case deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.AvailableReplicas < 1:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the cloudflared connector to become available", hostname)
	case connection.Status.Phase != oramav1alpha1.ConnectionPhaseReady || connection.Status.ObservedGeneration != connection.Spec.Generation:
		return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhasePending, "waiting for the connector's Connection to its target", hostname)
	}
	return ctrl.Result{RequeueAfter: requeue}, r.setStatus(ctx, tunnel, oramav1alpha1.TunnelPhaseReady,
		"Cloudflare Access admits only the grant's person; the connector reaches only its target", hostname)
}

// cfEnvelope is the Cloudflare API's response wrapper.
type cfEnvelope[T any] struct {
	Success bool `json:"success"`
	Result  T    `json:"result"`
}

type cfTunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r *CloudflareTunnelReconciler) ensureTunnel(ctx context.Context, tunnel *oramav1alpha1.CloudflareTunnel, account cloudflareAccount) error {
	if tunnel.Status.TunnelID != "" {
		return nil
	}
	name := cloudflareTunnelName(tunnel.Spec.GrantID)
	var listed cfEnvelope[[]cfTunnel]
	listURL := fmt.Sprintf("%s/accounts/%s/cfd_tunnel?is_deleted=false&name=%s", r.api(), account.AccountID, url.QueryEscape(name))
	if err := cfDo(ctx, account.Token, http.MethodGet, listURL, nil, &listed); err != nil {
		return err
	}
	id := ""
	for _, existing := range listed.Result {
		if existing.Name == name {
			id = existing.ID
			break
		}
	}
	if id == "" {
		var created cfEnvelope[cfTunnel]
		body := map[string]any{"name": name, "config_src": "cloudflare"}
		if err := cfDo(ctx, account.Token, http.MethodPost, fmt.Sprintf("%s/accounts/%s/cfd_tunnel", r.api(), account.AccountID), body, &created); err != nil {
			return err
		}
		id = created.Result.ID
	}
	if id == "" {
		return errors.New("Cloudflare returned no tunnel id")
	}
	tunnel.Status.TunnelID = id
	return r.Status().Update(ctx, tunnel)
}

func (r *CloudflareTunnelReconciler) tunnelToken(ctx context.Context, account cloudflareAccount, id string) (string, error) {
	var token cfEnvelope[string]
	if err := cfDo(ctx, account.Token, http.MethodGet, fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/token", r.api(), account.AccountID, id), nil, &token); err != nil {
		return "", err
	}
	if token.Result == "" {
		return "", errors.New("Cloudflare returned no tunnel token")
	}
	return token.Result, nil
}

func (r *CloudflareTunnelReconciler) putTunnelConfiguration(ctx context.Context, account cloudflareAccount, id, hostname string, target tunnelTarget) error {
	body := map[string]any{"config": map[string]any{"ingress": []map[string]any{
		{"hostname": hostname, "service": "tcp://" + target.Address()},
		{"service": "http_status:404"},
	}}}
	return cfDo(ctx, account.Token, http.MethodPut, fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", r.api(), account.AccountID, id), body, nil)
}

type cfDNSRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (r *CloudflareTunnelReconciler) ensureDNSRecord(ctx context.Context, tunnel *oramav1alpha1.CloudflareTunnel, account cloudflareAccount, hostname string) error {
	content := tunnel.Status.TunnelID + ".cfargotunnel.com"
	body := map[string]any{"type": "CNAME", "name": hostname, "content": content, "proxied": true,
		"comment": "Orama tunnel grant " + tunnel.Spec.GrantID}
	id := tunnel.Status.DNSRecordID
	if id == "" {
		var listed cfEnvelope[[]cfDNSRecord]
		listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=CNAME&name=%s", r.api(), account.ZoneID, url.QueryEscape(hostname))
		if err := cfDo(ctx, account.Token, http.MethodGet, listURL, nil, &listed); err != nil {
			return err
		}
		for _, record := range listed.Result {
			if record.Name == hostname {
				id = record.ID
				if record.Content != content {
					if err := cfDo(ctx, account.Token, http.MethodPut, fmt.Sprintf("%s/zones/%s/dns_records/%s", r.api(), account.ZoneID, id), body, nil); err != nil {
						return err
					}
				}
				break
			}
		}
	}
	if id == "" {
		var created cfEnvelope[cfDNSRecord]
		if err := cfDo(ctx, account.Token, http.MethodPost, fmt.Sprintf("%s/zones/%s/dns_records", r.api(), account.ZoneID), body, &created); err != nil {
			return err
		}
		id = created.Result.ID
	}
	if id == "" || id == tunnel.Status.DNSRecordID {
		return nil
	}
	tunnel.Status.DNSRecordID = id
	return r.Status().Update(ctx, tunnel)
}

type cfAccessApplication struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
}

// cloudflareSessionDuration is the Access session length: never past the grant's expiry, at most
// twelve hours, in whole minutes.
func cloudflareSessionDuration(expiresAt, now time.Time) string {
	remaining := expiresAt.Sub(now)
	if remaining > cloudflareAccessMaxSession {
		remaining = cloudflareAccessMaxSession
	}
	minutes := int(remaining / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("%dm", minutes)
}

func (r *CloudflareTunnelReconciler) ensureAccessApplication(
	ctx context.Context,
	tunnel *oramav1alpha1.CloudflareTunnel,
	account cloudflareAccount,
	hostname string,
	now time.Time,
) error {
	if tunnel.Status.AccessApplicationID != "" {
		return nil
	}
	var listed cfEnvelope[[]cfAccessApplication]
	if err := cfDo(ctx, account.Token, http.MethodGet, fmt.Sprintf("%s/accounts/%s/access/apps?per_page=1000", r.api(), account.AccountID), nil, &listed); err != nil {
		return err
	}
	id := ""
	for _, app := range listed.Result {
		if app.Domain == hostname {
			id = app.ID
			break
		}
	}
	if id == "" {
		body := map[string]any{
			"name":                 "Orama tunnel " + tunnel.Spec.GrantID,
			"domain":               hostname,
			"type":                 "self_hosted",
			"session_duration":     cloudflareSessionDuration(tunnel.Spec.ExpiresAt.Time, now),
			"app_launcher_visible": false,
			"policies": []map[string]any{{
				"name":     "grant " + tunnel.Spec.GrantID,
				"decision": "allow",
				"include":  []map[string]any{{"email": map[string]any{"email": tunnel.Spec.Subject}}},
			}},
		}
		var created cfEnvelope[cfAccessApplication]
		if err := cfDo(ctx, account.Token, http.MethodPost, fmt.Sprintf("%s/accounts/%s/access/apps", r.api(), account.AccountID), body, &created); err != nil {
			return err
		}
		id = created.Result.ID
	}
	if id == "" {
		return errors.New("Cloudflare returned no Access application id")
	}
	tunnel.Status.AccessApplicationID = id
	return r.Status().Update(ctx, tunnel)
}

func (r *CloudflareTunnelReconciler) reconcileTokenSecret(ctx context.Context, tunnel *oramav1alpha1.CloudflareTunnel, token string) (string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cloudflareTunnelTokenSecret(tunnel.Spec.GrantID), Namespace: tunnel.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, tunnel) {
			return fmt.Errorf("Secret %s/%s is not this tunnel's", secret.Namespace, secret.Name)
		}
		secret.Labels = withLabels(secret.Labels, cloudflaredLabels(tunnel.Spec.GrantID))
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{"token": []byte(token)}
		return controllerutil.SetControllerReference(tunnel, secret, r.Scheme)
	})
	return sha256Hex([]byte(token)), err
}

func (r *CloudflareTunnelReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultCloudflaredImage
}

func (r *CloudflareTunnelReconciler) reconcileConnector(
	ctx context.Context,
	tunnel *oramav1alpha1.CloudflareTunnel,
	lake *oramav1alpha1.Lake,
	tokenHash string,
) (*appsv1.Deployment, error) {
	labels := cloudflaredLabels(tunnel.Spec.GrantID)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(tunnel.Spec.GrantID), Namespace: tunnel.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" && !metav1.IsControlledBy(deployment, tunnel) {
			return fmt.Errorf("Deployment %s/%s is not this tunnel's", deployment.Namespace, deployment.Name)
		}
		deployment.Labels = withLabels(deployment.Labels, labels)
		deployment.Spec.Replicas = int32Ptr(1)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
			connectionTunnelIDLabel: tunnel.Spec.GrantID, zettaTunnelComponentKey: "connector",
		}}
		deployment.Spec.Template.Labels = labels
		deployment.Spec.Template.Annotations = map[string]string{"orama.cloud/tunnel-token": tokenHash}
		deployment.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			ImagePullSecrets:              pullSecretsFor(lake.Spec.PullSecretName),
			SecurityContext:               tunnelPodSecurityContext(),
			TerminationGracePeriodSeconds: int64Ptr(10),
			Containers: []corev1.Container{{
				Name:            "cloudflared",
				Image:           r.image(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"tunnel", "--no-autoupdate", "--metrics", fmt.Sprintf("0.0.0.0:%d", cloudflaredMetricsPort), "run"},
				Env: []corev1.EnvVar{{
					Name: "TUNNEL_TOKEN",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: cloudflareTunnelTokenSecret(tunnel.Spec.GrantID)},
						Key:                  "token",
					}},
				}},
				Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: cloudflaredMetricsPort, Protocol: corev1.ProtocolTCP}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(cloudflaredMetricsPort)}},
					PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: tunnelContainerSecurityContext(),
			}},
		}
		return controllerutil.SetControllerReference(tunnel, deployment, r.Scheme)
	})
	return deployment, err
}

// cloudflaredEgressAllowances lets the connector reach Cloudflare's edge on the public internet only.
func cloudflaredEgressAllowances(ctx context.Context, reader client.Reader, grantID string) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	peers, err := publicInternetPeers(ctx, reader)
	if err != nil {
		return nil, err
	}
	return []oramav1alpha1.NetworkSandboxPolicy{sandboxAllowance(
		fmt.Sprintf("Tunnel grant %s's cloudflared connector reaches Cloudflare's edge on public addresses only.", grantID),
		networkPolicy(
			cloudflareTunnelEgressAllowance(grantID),
			metav1.LabelSelector{MatchLabels: map[string]string{connectionTunnelIDLabel: grantID, zettaTunnelComponentKey: "connector"}},
			[]networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			nil,
			[]networkingv1.NetworkPolicyEgressRule{{
				To: peers,
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(intstr.FromInt32(443))},
					{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(intstr.FromInt32(7844))},
					{Protocol: protocolPtr(corev1.ProtocolUDP), Port: intstrPtr(intstr.FromInt32(7844))},
				},
			}},
		),
	)}, nil
}

func cfNotFound(err error) bool {
	var apiErr *cfAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// teardown removes the workspace allowance and connector first, so nothing reaches the target,
// then the Access application, the DNS record and the tunnel in the customer's account.
func (r *CloudflareTunnelReconciler) teardown(ctx context.Context, tunnel *oramav1alpha1.CloudflareTunnel) (bool, error) {
	done, err := teardownTunnelWorkload(ctx, r.Client, r.Scheme, tunnel.Namespace, tunnel.Spec.GrantID,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelEndpointName(tunnel.Spec.GrantID)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cloudflareTunnelTokenSecret(tunnel.Spec.GrantID)}},
	)
	if err != nil || !done {
		return done, err
	}
	if tunnel.Status.TunnelID == "" && tunnel.Status.DNSRecordID == "" && tunnel.Status.AccessApplicationID == "" {
		return true, nil
	}
	account, err := r.account(ctx, tunnel)
	if err != nil {
		return false, fmt.Errorf("the customer's Cloudflare account is needed to remove the tunnel: %w", err)
	}
	if id := tunnel.Status.AccessApplicationID; id != "" {
		err := cfDo(ctx, account.Token, http.MethodDelete, fmt.Sprintf("%s/accounts/%s/access/apps/%s", r.api(), account.AccountID, id), nil, nil)
		if err != nil && !cfNotFound(err) {
			return false, err
		}
		tunnel.Status.AccessApplicationID = ""
		if err := r.Status().Update(ctx, tunnel); err != nil {
			return false, err
		}
	}
	if id := tunnel.Status.DNSRecordID; id != "" {
		err := cfDo(ctx, account.Token, http.MethodDelete, fmt.Sprintf("%s/zones/%s/dns_records/%s", r.api(), account.ZoneID, id), nil, nil)
		if err != nil && !cfNotFound(err) {
			return false, err
		}
		tunnel.Status.DNSRecordID = ""
		if err := r.Status().Update(ctx, tunnel); err != nil {
			return false, err
		}
	}
	if id := tunnel.Status.TunnelID; id != "" {
		base := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s", r.api(), account.AccountID, id)
		if err := cfDo(ctx, account.Token, http.MethodDelete, base+"/connections", nil, nil); err != nil && !cfNotFound(err) {
			return false, err
		}
		if err := cfDo(ctx, account.Token, http.MethodDelete, base, nil, nil); err != nil && !cfNotFound(err) {
			return false, err
		}
		tunnel.Status.TunnelID = ""
		if err := r.Status().Update(ctx, tunnel); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *CloudflareTunnelReconciler) setStatus(
	ctx context.Context,
	tunnel *oramav1alpha1.CloudflareTunnel,
	phase string,
	message string,
	endpoint string,
) error {
	conditionStatus := metav1.ConditionFalse
	if phase == oramav1alpha1.TunnelPhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	desired := tunnel.Status.DeepCopy()
	desired.Phase = phase
	desired.Message = message
	desired.Endpoint = endpoint
	reason := phase
	if reason == "" {
		reason = oramav1alpha1.TunnelPhasePending
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: tunnelReadyCondition, Status: conditionStatus, Reason: reason, Message: message, ObservedGeneration: tunnel.Generation,
	})
	if apiequality.Semantic.DeepEqual(tunnel.Status, *desired) {
		return nil
	}
	tunnel.Status = *desired
	return r.Status().Update(ctx, tunnel)
}

func (r *CloudflareTunnelReconciler) tunnelsInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var tunnels oramav1alpha1.CloudflareTunnelList
	if err := r.List(ctx, &tunnels, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
	}
	return requests
}

func (r *CloudflareTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.CloudflareTunnel{}).
		Owns(&appsv1.Deployment{}).
		Owns(&oramav1alpha1.Connection{}).
		Watches(&oramav1alpha1.TunnelProvider{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsInNamespace)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsInNamespace)).
		Watches(&oramav1alpha1.NetworkSandbox{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsInNamespace)).
		Complete(r)
}
