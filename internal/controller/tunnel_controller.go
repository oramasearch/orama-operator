// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"net/mail"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	tunnelFinalizer = "orama.cloud/tunnel"
	// defaultTunnelProviderName is the TunnelProvider a workspace's Lake-granted tunnels use, when
	// the namespace has one. Without it they use ZettaTunnel.
	defaultTunnelProviderName = "default"

	tunnelProviderKindZetta      = "ZettaTunnel"
	tunnelProviderKindCloudflare = "CloudflareTunnel"
)

// TunnelReconciler is the abstract kind's controller (principle 10). It names no vendor: it picks
// the concrete kind from the TunnelProvider, keeps exactly that one child, mirrors its status, and
// ends the grant at its expiry or revocation by deleting the child before reporting Expired or
// Revoked.
type TunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=tunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=tunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=zettatunnels;cloudflaretunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=tunnelproviders,verbs=get;list;watch

func (r *TunnelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func lakeTunnelName(grantID string) string { return "tunnel-" + grantID }

func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tunnel oramav1alpha1.Tunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tunnel.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&tunnel, tunnelFinalizer) {
			return ctrl.Result{}, nil
		}
		gone, err := r.deleteChildren(ctx, &tunnel, "")
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		controllerutil.RemoveFinalizer(&tunnel, tunnelFinalizer)
		return ctrl.Result{}, r.Update(ctx, &tunnel)
	}
	if !controllerutil.ContainsFinalizer(&tunnel, tunnelFinalizer) {
		controllerutil.AddFinalizer(&tunnel, tunnelFinalizer)
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if message := validateTunnel(&tunnel); message != "" {
		if _, err := r.deleteChildren(ctx, &tunnel, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{Phase: oramav1alpha1.TunnelPhaseFailed, Message: message})
	}
	now := r.now()
	ended, phase, message := tunnelEnded(&tunnel, now)
	if ended {
		gone, err := r.deleteChildren(ctx, &tunnel, "")
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			return ctrl.Result{RequeueAfter: time.Second}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{
				Phase: oramav1alpha1.TunnelPhasePending, Message: "ending the grant: removing its provider",
			})
		}
		return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{Phase: phase, Message: message})
	}

	kind, provider, err := r.providerFor(ctx, &tunnel)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: tunnelPendingRequeue}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{
				Phase: oramav1alpha1.TunnelPhasePending, Message: fmt.Sprintf("waiting for TunnelProvider %q", tunnel.Spec.ProviderRef),
			})
		}
		return ctrl.Result{}, err
	}
	if provider != nil {
		if message := tunnelProviderRefusal(provider, tunnel.Spec.CustomerID); message != "" {
			if _, err := r.deleteChildren(ctx, &tunnel, ""); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{Phase: oramav1alpha1.TunnelPhaseFailed, Message: message})
		}
	}
	if gone, err := r.deleteChildren(ctx, &tunnel, kind); err != nil {
		return ctrl.Result{}, err
	} else if !gone {
		return ctrl.Result{RequeueAfter: time.Second}, r.setStatus(ctx, &tunnel, oramav1alpha1.TunnelStatus{
			Phase: oramav1alpha1.TunnelPhasePending, Message: "switching provider: removing the previous one", Provider: kind,
		})
	}
	var child oramav1alpha1.TunnelStatus
	switch kind {
	case tunnelProviderKindZetta:
		child, err = r.reconcileZetta(ctx, &tunnel)
	default:
		child, err = r.reconcileCloudflare(ctx, &tunnel, provider)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	child.Provider = kind
	requeue := min(tunnel.Spec.ExpiresAt.Time.Sub(now)+time.Second, tunnelExpiryRequeueCap)
	return ctrl.Result{RequeueAfter: requeue}, r.setStatus(ctx, &tunnel, child)
}

// tunnelEnded reports whether the grant has ended, revocation first.
func tunnelEnded(tunnel *oramav1alpha1.Tunnel, now time.Time) (bool, string, string) {
	if tunnel.Spec.RevokedAt != nil {
		return true, oramav1alpha1.TunnelPhaseRevoked, "the grant was revoked; its provider is removed"
	}
	if !now.Before(tunnel.Spec.ExpiresAt.Time) {
		return true, oramav1alpha1.TunnelPhaseExpired, "the grant has expired; its provider is removed"
	}
	return false, "", ""
}

func validateTunnel(tunnel *oramav1alpha1.Tunnel) string {
	for name, value := range map[string]string{
		"grantId":     tunnel.Spec.GrantID,
		"customerId":  tunnel.Spec.CustomerID,
		"workspaceId": tunnel.Spec.WorkspaceID,
		"userId":      tunnel.Spec.UserID,
		"targetId":    tunnel.Spec.TargetID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return name + " must be a canonical lowercase UUID"
		}
	}
	if tunnel.Name != lakeTunnelName(tunnel.Spec.GrantID) {
		return "a Tunnel is named tunnel-<grantId>"
	}
	if tunnel.Spec.Port < 1 || tunnel.Spec.Port > 65535 {
		return "port must be between 1 and 65535"
	}
	if tunnel.Spec.TargetKind != oramav1alpha1.TunnelTargetService && tunnel.Spec.TargetKind != oramav1alpha1.TunnelTargetApp {
		return "targetKind must be service or app"
	}
	return ""
}

// tunnelProviderRefusal is why provider cannot carry a tunnel of customerID, or empty.
func tunnelProviderRefusal(provider *oramav1alpha1.TunnelProvider, customerID string) string {
	if provider.Spec.CustomerID != customerID {
		return fmt.Sprintf("TunnelProvider %q belongs to another customer", provider.Name)
	}
	if provider.Spec.Type == oramav1alpha1.TunnelProviderCloudflare && provider.Spec.Cloudflare == nil {
		return fmt.Sprintf("TunnelProvider %q has no cloudflare settings", provider.Name)
	}
	return ""
}

func (r *TunnelReconciler) providerFor(ctx context.Context, tunnel *oramav1alpha1.Tunnel) (string, *oramav1alpha1.TunnelProvider, error) {
	if tunnel.Spec.ProviderRef == "" {
		return tunnelProviderKindZetta, nil, nil
	}
	var provider oramav1alpha1.TunnelProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Spec.ProviderRef}, &provider); err != nil {
		return "", nil, err
	}
	switch provider.Spec.Type {
	case oramav1alpha1.TunnelProviderCloudflare:
		return tunnelProviderKindCloudflare, &provider, nil
	default:
		return tunnelProviderKindZetta, &provider, nil
	}
}

func (r *TunnelReconciler) grant(tunnel *oramav1alpha1.Tunnel) oramav1alpha1.TunnelGrantSpec {
	return oramav1alpha1.TunnelGrantSpec{
		LakeRef:     tunnel.Spec.LakeRef,
		GrantID:     tunnel.Spec.GrantID,
		CustomerID:  tunnel.Spec.CustomerID,
		WorkspaceID: tunnel.Spec.WorkspaceID,
		TargetKind:  tunnel.Spec.TargetKind,
		TargetID:    tunnel.Spec.TargetID,
		Port:        tunnel.Spec.Port,
		ExpiresAt:   tunnel.Spec.ExpiresAt,
	}
}

func (r *TunnelReconciler) reconcileZetta(ctx context.Context, tunnel *oramav1alpha1.Tunnel) (oramav1alpha1.TunnelStatus, error) {
	if tunnel.Spec.ClientCAPEM == "" {
		return oramav1alpha1.TunnelStatus{
			Phase:   oramav1alpha1.TunnelPhaseFailed,
			Message: "the grant has no client CA (Lake migration 0085_network_tunnel_ca); revoke and grant it again",
		}, nil
	}
	child := &oramav1alpha1.ZettaTunnel{ObjectMeta: metav1.ObjectMeta{Name: tunnel.Name, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, child, func() error {
		if child.ResourceVersion != "" && !metav1.IsControlledBy(child, tunnel) {
			return fmt.Errorf("ZettaTunnel %s/%s is not this Tunnel's", child.Namespace, child.Name)
		}
		child.Labels = withLabels(child.Labels, map[string]string{connectionTunnelIDLabel: tunnel.Spec.GrantID, workspaceLabel: tunnel.Spec.WorkspaceID})
		child.Spec = oramav1alpha1.ZettaTunnelSpec{TunnelGrantSpec: r.grant(tunnel), ClientCAPEM: tunnel.Spec.ClientCAPEM}
		return controllerutil.SetControllerReference(tunnel, child, r.Scheme)
	}); err != nil {
		return oramav1alpha1.TunnelStatus{}, err
	}
	return childTunnelStatus(child.Status.Phase, child.Status.Message, child.Status.Endpoint, child.Status.RelayCAPEM), nil
}

func (r *TunnelReconciler) reconcileCloudflare(
	ctx context.Context,
	tunnel *oramav1alpha1.Tunnel,
	provider *oramav1alpha1.TunnelProvider,
) (oramav1alpha1.TunnelStatus, error) {
	if _, err := mail.ParseAddress(tunnel.Spec.Subject); err != nil || tunnel.Spec.Subject == "" {
		return oramav1alpha1.TunnelStatus{
			Phase:   oramav1alpha1.TunnelPhaseFailed,
			Message: "a Cloudflare tunnel needs the person's e-mail address in spec.subject for Cloudflare Access",
		}, nil
	}
	child := &oramav1alpha1.CloudflareTunnel{ObjectMeta: metav1.ObjectMeta{Name: tunnel.Name, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, child, func() error {
		if child.ResourceVersion != "" && !metav1.IsControlledBy(child, tunnel) {
			return fmt.Errorf("CloudflareTunnel %s/%s is not this Tunnel's", child.Namespace, child.Name)
		}
		child.Labels = withLabels(child.Labels, map[string]string{connectionTunnelIDLabel: tunnel.Spec.GrantID, workspaceLabel: tunnel.Spec.WorkspaceID})
		child.Spec = oramav1alpha1.CloudflareTunnelSpec{
			TunnelGrantSpec: r.grant(tunnel),
			ProviderRef:     provider.Name,
			Subject:         tunnel.Spec.Subject,
		}
		return controllerutil.SetControllerReference(tunnel, child, r.Scheme)
	}); err != nil {
		return oramav1alpha1.TunnelStatus{}, err
	}
	return childTunnelStatus(child.Status.Phase, child.Status.Message, child.Status.Endpoint, ""), nil
}

func childTunnelStatus(phase, message, endpoint, relayCA string) oramav1alpha1.TunnelStatus {
	switch phase {
	case oramav1alpha1.TunnelPhaseReady:
		return oramav1alpha1.TunnelStatus{Phase: phase, Message: message, Endpoint: endpoint, RelayCAPEM: relayCA}
	case oramav1alpha1.TunnelPhaseFailed:
		return oramav1alpha1.TunnelStatus{Phase: phase, Message: message}
	case "":
		return oramav1alpha1.TunnelStatus{Phase: oramav1alpha1.TunnelPhasePending, Message: "waiting for the provider"}
	default:
		// An endpoint is reported only while the provider serves the grant.
		return oramav1alpha1.TunnelStatus{Phase: oramav1alpha1.TunnelPhasePending, Message: message}
	}
}

// deleteChildren deletes every concrete child except keep, and reports whether they are all gone.
func (r *TunnelReconciler) deleteChildren(ctx context.Context, tunnel *oramav1alpha1.Tunnel, keep string) (bool, error) {
	gone := true
	children := map[string]client.Object{
		tunnelProviderKindZetta:      &oramav1alpha1.ZettaTunnel{},
		tunnelProviderKindCloudflare: &oramav1alpha1.CloudflareTunnel{},
	}
	for kind, child := range children {
		if kind == keep {
			continue
		}
		if err := r.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Name}, child); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !metav1.IsControlledBy(child, tunnel) {
			continue
		}
		gone = false
		if child.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	return gone, nil
}

func (r *TunnelReconciler) setStatus(ctx context.Context, tunnel *oramav1alpha1.Tunnel, observed oramav1alpha1.TunnelStatus) error {
	conditionStatus := metav1.ConditionFalse
	if observed.Phase == oramav1alpha1.TunnelPhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	desired := observed
	desired.ObservedGeneration = tunnel.Spec.Generation
	desired.Conditions = append([]metav1.Condition(nil), tunnel.Status.Conditions...)
	if desired.Provider == "" && (observed.Phase == oramav1alpha1.TunnelPhaseExpired || observed.Phase == oramav1alpha1.TunnelPhaseRevoked) {
		desired.Provider = tunnel.Status.Provider
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: tunnelReadyCondition, Status: conditionStatus, Reason: desired.Phase, Message: desired.Message, ObservedGeneration: tunnel.Generation,
	})
	if apiequality.Semantic.DeepEqual(tunnel.Status, desired) {
		return nil
	}
	tunnel.Status = desired
	return r.Status().Update(ctx, tunnel)
}

func (r *TunnelReconciler) tunnelsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	var tunnels oramav1alpha1.TunnelList
	if err := r.List(ctx, &tunnels, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range tunnels.Items {
		if tunnels.Items[i].Spec.ProviderRef == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
		}
	}
	return requests
}

func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Tunnel{}).
		Owns(&oramav1alpha1.ZettaTunnel{}).
		Owns(&oramav1alpha1.CloudflareTunnel{}).
		Watches(&oramav1alpha1.TunnelProvider{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsForProvider)).
		Complete(r)
}

// TunnelProviderReconciler reports whether a provider can be used: its type's settings are present
// and, for a customer's Cloudflare account, its API token Secret holds the named key. It never
// calls the vendor; the concrete tunnel controllers do.
type TunnelProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orama.cloud,resources=tunnelproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=tunnelproviders/status,verbs=get;update;patch

func (r *TunnelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var provider oramav1alpha1.TunnelProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, reason, message := metav1.ConditionTrue, "Usable", "the provider can carry tunnels"
	if provider.Spec.Type == oramav1alpha1.TunnelProviderCloudflare {
		switch {
		case provider.Spec.Cloudflare == nil:
			status, reason, message = metav1.ConditionFalse, "Invalid", "spec.cloudflare is required"
		default:
			ref := provider.Spec.Cloudflare.APITokenSecretRef
			var secret corev1.Secret
			err := r.Get(ctx, client.ObjectKey{Namespace: provider.Namespace, Name: ref.Name}, &secret)
			switch {
			case apierrors.IsNotFound(err):
				status, reason, message = metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("Secret %q does not exist", ref.Name)
			case err != nil:
				return ctrl.Result{}, err
			case len(secret.Data[ref.Key]) == 0:
				status, reason, message = metav1.ConditionFalse, "SecretMissing", fmt.Sprintf("Secret %q has no key %q", ref.Name, ref.Key)
			}
		}
	}
	desired := provider.Status.DeepCopy()
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: tunnelReadyCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: provider.Generation,
	})
	if apiequality.Semantic.DeepEqual(provider.Status, *desired) {
		return ctrl.Result{}, nil
	}
	provider.Status = *desired
	return ctrl.Result{}, r.Status().Update(ctx, &provider)
}

func (r *TunnelProviderReconciler) providersInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var providers oramav1alpha1.TunnelProviderList
	if err := r.List(ctx, &providers, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(providers.Items))
	for i := range providers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&providers.Items[i])})
	}
	return requests
}

func (r *TunnelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.TunnelProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.providersInNamespace)).
		Complete(r)
}
