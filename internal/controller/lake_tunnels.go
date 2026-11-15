/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// errTunnelCAMissing is returned while Lake's schema has network_guard (0084) but not the tunnel
// trust columns of 0085_network_tunnel_ca: no grant can be served without its client CA.
var errTunnelCAMissing = errors.New("network_tunnel_grants.client_ca_pem is not installed yet (Lake migration 0085_network_tunnel_ca)")

// lakeTunnelClock is the intake's clock; tests replace it.
var lakeTunnelClock = time.Now

// LakeTunnelGrant is one network_tunnel_grants row with its current operator status.
type LakeTunnelGrant struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	UserID             string
	// UserEmail is read for providers that admit people themselves (Cloudflare Access) only.
	UserEmail   string
	TargetKind  string
	TargetID    string
	Port        int32
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	ClientCAPEM string
	Generation  int64
	Status      *LakeTunnelGrantStatus
}

// LakeTunnelGrantStatus is written only to network_tunnel_grant_status.
type LakeTunnelGrantStatus struct {
	GrantID            string
	Phase              string
	Message            string
	Endpoint           string
	RelayCAPEM         string
	ObservedGeneration int64
}

// LakeTunnelStore is the developer-tunnel half of Lake's secure-networking contract. The pgx
// store implements it; the Lake reconciler uses it only when its store does.
type LakeTunnelStore interface {
	// ListTunnelGrants returns errNetworkGuardMissing before 0084 and errTunnelCAMissing before
	// 0085. Grants whose terminal status (Expired or Revoked) is already recorded are left out.
	ListTunnelGrants(ctx context.Context) ([]LakeTunnelGrant, error)
	UpsertTunnelGrantStatus(ctx context.Context, status LakeTunnelGrantStatus) error
}

func (r *LakeReconciler) reconcileLakeTunnels(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	tunnels, ok := store.(LakeTunnelStore)
	if !ok {
		return false
	}
	grants, err := tunnels.ListTunnelGrants(ctx)
	if errors.Is(err, errNetworkGuardMissing) || errors.Is(err, errTunnelCAMissing) {
		log.Info("tunnels skipped: " + err.Error())
		return false
	}
	if err != nil {
		log.Info("tunnels deferred: cannot read tunnel grants", "err", err.Error())
		return false
	}
	return r.syncLakeTunnels(ctx, lake, tunnels, grants)
}

func (r *LakeReconciler) syncLakeTunnels(ctx context.Context, lake *oramav1alpha1.Lake, store LakeTunnelStore, grants []LakeTunnelGrant) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	now := lakeTunnelClock()
	keep := map[string]bool{}
	changing := false
	perWorkspace := newRowLimit(lakeTunnelGrantsPerWorkspace, "live tunnel grants per workspace")
	perCustomer := newRowLimit(lakeTunnelGrantsPerCustomer, "live tunnel grants per customer")
	for i := range grants {
		grant := &grants[i]
		// Every live grant goes into the relay's one projection, shared by every tenant of the
		// cluster: past either limit (operator#782, lake#983) a grant is refused, and its Tunnel,
		// if one was made, is removed with the orphans below.
		if live := grant.RevokedAt == nil && now.Before(grant.ExpiresAt); live {
			inWorkspace, inCustomer := perWorkspace.admit(grant.WorkspaceID), perCustomer.admit(grant.CustomerID)
			if !inWorkspace || !inCustomer {
				refusal := perWorkspace.refusal()
				if !inCustomer {
					refusal = perCustomer.refusal()
				}
				status := LakeTunnelGrantStatus{GrantID: grant.ID, Phase: oramav1alpha1.TunnelPhaseFailed, Message: refusal, ObservedGeneration: grant.Generation}
				if err := writeTunnelGrantStatus(ctx, store, grant, status); err != nil {
					changing = true
				}
				continue
			}
		}
		keep[grant.WorkspaceNamespace+"/"+lakeTunnelName(grant.ID)] = true
		status, err := r.syncLakeTunnel(ctx, lake, grant, now)
		if err != nil {
			log.Error(err, "tunnel reconciliation deferred", "grant", grant.ID)
			changing = true
			continue
		}
		if err := writeTunnelGrantStatus(ctx, store, grant, status); err != nil {
			log.Info("tunnel status write deferred", "grant", grant.ID, "err", err.Error())
			changing = true
		}
		if status.Phase != oramav1alpha1.TunnelPhaseReady && status.Phase != oramav1alpha1.TunnelPhaseFailed {
			changing = true
		}
	}
	// A Lake-controlled Tunnel whose row is gone or whose terminal status is recorded is deleted.
	var all oramav1alpha1.TunnelList
	if err := r.List(ctx, &all, client.MatchingLabels{serviceLakeLabel: lake.Name}); err != nil {
		log.Error(err, "tunnel orphan list deferred")
		return true
	}
	for i := range all.Items {
		tunnel := &all.Items[i]
		if keep[tunnel.Namespace+"/"+tunnel.Name] || !metav1.IsControlledBy(tunnel, lake) || !tunnel.DeletionTimestamp.IsZero() {
			continue
		}
		changing = true
		if err := r.Delete(ctx, tunnel); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "orphan tunnel deletion deferred", "tunnel", tunnel.Name)
		}
	}
	return changing
}

func (r *LakeReconciler) syncLakeTunnel(ctx context.Context, lake *oramav1alpha1.Lake, grant *LakeTunnelGrant, now time.Time) (LakeTunnelGrantStatus, error) {
	if message := validateTunnelGrant(grant); message != "" {
		return LakeTunnelGrantStatus{GrantID: grant.ID, Phase: oramav1alpha1.TunnelPhaseFailed, Message: message, ObservedGeneration: grant.Generation}, nil
	}
	tunnel := &oramav1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: lakeTunnelName(grant.ID), Namespace: grant.WorkspaceNamespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(tunnel), tunnel)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return LakeTunnelGrantStatus{}, err
	}
	ended := grant.RevokedAt != nil || !now.Before(grant.ExpiresAt)
	if !exists && ended {
		// Nothing was ever made for it, so nothing is left to tear down.
		phase, message := oramav1alpha1.TunnelPhaseExpired, "the grant has expired"
		if grant.RevokedAt != nil {
			phase, message = oramav1alpha1.TunnelPhaseRevoked, "the grant was revoked"
		}
		return LakeTunnelGrantStatus{GrantID: grant.ID, Phase: phase, Message: message, ObservedGeneration: grant.Generation}, nil
	}
	if exists && !metav1.IsControlledBy(tunnel, lake) {
		return LakeTunnelGrantStatus{GrantID: grant.ID, Phase: oramav1alpha1.TunnelPhaseFailed,
			Message: "Tunnel name is occupied by another owner", ObservedGeneration: grant.Generation}, nil
	}
	if exists && (tunnel.Spec.GrantID != grant.ID || tunnel.Spec.LakeRef != lake.Name ||
		tunnel.Spec.CustomerID != grant.CustomerID || tunnel.Spec.WorkspaceID != grant.WorkspaceID) {
		return LakeTunnelGrantStatus{GrantID: grant.ID, Phase: oramav1alpha1.TunnelPhaseFailed,
			Message: "Tunnel immutable fields do not match the Lake row", ObservedGeneration: grant.Generation}, nil
	}
	providerRef, subject, err := r.workspaceTunnelProvider(ctx, grant)
	if err != nil {
		return LakeTunnelGrantStatus{}, err
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, tunnel, func() error {
		tunnel.Labels = withLabels(tunnel.Labels, map[string]string{
			connectionTunnelIDLabel: grant.ID,
			serviceLakeLabel:        lake.Name,
			workspaceLabel:          grant.WorkspaceID,
		})
		var revokedAt *metav1.Time
		if grant.RevokedAt != nil {
			value := metav1.NewTime(grant.RevokedAt.UTC())
			revokedAt = &value
		}
		tunnel.Spec = oramav1alpha1.TunnelSpec{
			LakeRef:     lake.Name,
			GrantID:     grant.ID,
			CustomerID:  grant.CustomerID,
			WorkspaceID: grant.WorkspaceID,
			UserID:      grant.UserID,
			TargetKind:  grant.TargetKind,
			TargetID:    grant.TargetID,
			Port:        grant.Port,
			ExpiresAt:   metav1.NewTime(grant.ExpiresAt.UTC()),
			RevokedAt:   revokedAt,
			ClientCAPEM: grant.ClientCAPEM,
			Subject:     subject,
			ProviderRef: providerRef,
			Generation:  grant.Generation,
		}
		return controllerutil.SetControllerReference(lake, tunnel, r.Scheme)
	}); err != nil {
		return LakeTunnelGrantStatus{}, err
	}
	status := LakeTunnelGrantStatus{
		GrantID:            grant.ID,
		Phase:              oramav1alpha1.TunnelPhasePending,
		Message:            "waiting for the Tunnel controller",
		ObservedGeneration: grant.Generation,
	}
	if tunnel.Status.ObservedGeneration == grant.Generation && tunnel.Status.Phase != "" {
		status.Phase = tunnel.Status.Phase
		status.Message = tunnel.Status.Message
		if status.Phase == oramav1alpha1.TunnelPhaseReady {
			status.Endpoint = tunnel.Status.Endpoint
			status.RelayCAPEM = tunnel.Status.RelayCAPEM
		}
	}
	// A recorded end needs no resource: deleting the Tunnel now leaves nothing behind, because its
	// finalizer already removed the provider before it reported Expired or Revoked.
	if (status.Phase == oramav1alpha1.TunnelPhaseExpired || status.Phase == oramav1alpha1.TunnelPhaseRevoked) &&
		grant.Status != nil && grant.Status.Phase == status.Phase && grant.Status.ObservedGeneration == grant.Generation &&
		tunnel.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, tunnel); err != nil && !apierrors.IsNotFound(err) {
			return LakeTunnelGrantStatus{}, err
		}
	}
	return status, nil
}

// workspaceTunnelProvider is the workspace's default provider, if it has one. Only a cloudflare
// provider is given the person's e-mail address.
func (r *LakeReconciler) workspaceTunnelProvider(ctx context.Context, grant *LakeTunnelGrant) (string, string, error) {
	var provider oramav1alpha1.TunnelProvider
	err := r.Get(ctx, client.ObjectKey{Namespace: grant.WorkspaceNamespace, Name: defaultTunnelProviderName}, &provider)
	if apierrors.IsNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if provider.Spec.Type == oramav1alpha1.TunnelProviderCloudflare {
		return provider.Name, grant.UserEmail, nil
	}
	return provider.Name, "", nil
}

func validateTunnelGrant(grant *LakeTunnelGrant) string {
	for name, value := range map[string]string{
		"grant id":     grant.ID,
		"customer id":  grant.CustomerID,
		"workspace id": grant.WorkspaceID,
		"user id":      grant.UserID,
		"target id":    grant.TargetID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return name + " must be a canonical UUID"
		}
	}
	if grant.WorkspaceNamespace == "" || len(validation.IsDNS1123Label(grant.WorkspaceNamespace)) != 0 {
		return "workspace and its reconciled namespace are required"
	}
	if grant.TargetKind != oramav1alpha1.TunnelTargetService && grant.TargetKind != oramav1alpha1.TunnelTargetApp {
		return "target kind must be service or app"
	}
	if grant.Port < 1 || grant.Port > 65535 {
		return "port must be between 1 and 65535"
	}
	if grant.Generation < 1 {
		return "generation must be positive"
	}
	return ""
}

func writeTunnelGrantStatus(ctx context.Context, store LakeTunnelStore, grant *LakeTunnelGrant, status LakeTunnelGrantStatus) error {
	if grant.Status != nil && *grant.Status == status {
		return nil
	}
	if err := store.UpsertTunnelGrantStatus(ctx, status); err != nil {
		return fmt.Errorf("write tunnel grant status: %w", err)
	}
	grant.Status = &status
	return nil
}
