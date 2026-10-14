/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// LakePeer is one network_peers row with its current operator status.
type LakePeer struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	Name               string
	Remote             string
	// Exports and Imports are the rows' jsonb, decoded strictly by the intake.
	Exports    []byte
	Imports    []byte
	Desired    string
	Generation int64
	Status     *LakePeerStatus
}

// LakePeerStatus is written only to network_peer_status.
type LakePeerStatus struct {
	PeerID             string
	Phase              string
	Message            string
	ObservedGeneration int64
}

// LakePeerStore is the peer half of Lake's secure-networking contract (0084_network).
type LakePeerStore interface {
	// ListPeers returns errNetworkGuardMissing before 0084. Rows that are absent with Deleted
	// recorded are left out.
	ListPeers(ctx context.Context) ([]LakePeer, error)
	UpsertPeerStatus(ctx context.Context, status LakePeerStatus) error
}

func lakePeerName(id string) string { return "peer-" + id }

func (r *LakeReconciler) reconcileLakePeers(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	peers, ok := store.(LakePeerStore)
	if !ok {
		return false
	}
	rows, err := peers.ListPeers(ctx)
	if errors.Is(err, errNetworkGuardMissing) {
		log.Info("peers skipped: " + err.Error())
		return false
	}
	if err != nil {
		log.Info("peers deferred: cannot read peers", "err", err.Error())
		return false
	}
	return r.syncLakePeers(ctx, lake, peers, rows)
}

func (r *LakeReconciler) syncLakePeers(ctx context.Context, lake *oramav1alpha1.Lake, store LakePeerStore, rows []LakePeer) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	keep := map[string]bool{}
	changing := false
	for i := range rows {
		row := &rows[i]
		keep[row.WorkspaceNamespace+"/"+lakePeerName(row.ID)] = true
		status, err := r.syncLakePeer(ctx, lake, row)
		if err != nil {
			log.Error(err, "peer reconciliation deferred", "peer", row.ID)
			changing = true
			continue
		}
		if row.Status == nil || *row.Status != status {
			if err := store.UpsertPeerStatus(ctx, status); err != nil {
				log.Info("peer status write deferred", "peer", row.ID, "err", err.Error())
				changing = true
			} else {
				row.Status = &status
			}
		}
		settled := status.ObservedGeneration == row.Generation &&
			(status.Phase == oramav1alpha1.PeerPhaseReady || status.Phase == oramav1alpha1.PeerPhaseFailed || status.Phase == oramav1alpha1.PeerPhaseDeleted)
		if !settled {
			changing = true
		}
	}
	var all oramav1alpha1.PeerConnectionList
	if err := r.List(ctx, &all, client.MatchingLabels{serviceLakeLabel: lake.Name}); err != nil {
		log.Error(err, "peer orphan list deferred")
		return true
	}
	for i := range all.Items {
		peer := &all.Items[i]
		if keep[peer.Namespace+"/"+peer.Name] || !metav1.IsControlledBy(peer, lake) || !peer.DeletionTimestamp.IsZero() {
			continue
		}
		changing = true
		if err := r.Delete(ctx, peer); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "orphan peer deletion deferred", "peer", peer.Name)
		}
	}
	return changing
}

type lakePeerExportRow struct {
	TargetKind string `json:"target_kind"`
	TargetID   string `json:"target_id"`
	Port       int32  `json:"port"`
}

type lakePeerImportRow struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

// decodeStrict decodes JSON, refusing unknown fields and trailing data.
func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

func lakePeerSpec(lake *oramav1alpha1.Lake, row *LakePeer) (oramav1alpha1.PeerConnectionSpec, string) {
	for name, value := range map[string]string{"peer id": row.ID, "customer id": row.CustomerID, "workspace id": row.WorkspaceID} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return oramav1alpha1.PeerConnectionSpec{}, name + " must be a canonical UUID"
		}
	}
	if row.WorkspaceNamespace == "" || len(validation.IsDNS1123Label(row.WorkspaceNamespace)) != 0 {
		return oramav1alpha1.PeerConnectionSpec{}, "workspace and its reconciled namespace are required"
	}
	if row.Desired != oramav1alpha1.ConnectionDesiredPresent && row.Desired != oramav1alpha1.ConnectionDesiredAbsent {
		return oramav1alpha1.PeerConnectionSpec{}, "desired must be present or absent"
	}
	if row.Generation < 1 {
		return oramav1alpha1.PeerConnectionSpec{}, "generation must be positive"
	}
	if message := validatePeerRemote(row.Remote); message != "" {
		return oramav1alpha1.PeerConnectionSpec{}, message
	}
	var exports []lakePeerExportRow
	if err := decodeStrict(row.Exports, &exports); err != nil {
		return oramav1alpha1.PeerConnectionSpec{}, "exports must be [{target_kind, target_id, port}]: " + err.Error()
	}
	var imports []lakePeerImportRow
	if err := decodeStrict(row.Imports, &imports); err != nil {
		return oramav1alpha1.PeerConnectionSpec{}, "imports must be [{name, port}]: " + err.Error()
	}
	spec := oramav1alpha1.PeerConnectionSpec{
		PeerID: row.ID, LakeRef: lake.Name, CustomerID: row.CustomerID, WorkspaceID: row.WorkspaceID,
		Remote: row.Remote, PullSecretName: lake.Spec.PullSecretName, Desired: row.Desired, Generation: row.Generation,
		Exports: []oramav1alpha1.PeerExport{}, Imports: []oramav1alpha1.PeerImport{},
	}
	for _, item := range exports {
		if item.TargetKind != oramav1alpha1.PeerTargetService && item.TargetKind != oramav1alpha1.PeerTargetApp {
			return oramav1alpha1.PeerConnectionSpec{}, "an export's target_kind must be service or app"
		}
		if parsed, err := uuid.Parse(item.TargetID); err != nil || parsed.String() != item.TargetID {
			return oramav1alpha1.PeerConnectionSpec{}, "an export's target_id must be a canonical UUID"
		}
		if item.Port < 1 || item.Port > 65535 {
			return oramav1alpha1.PeerConnectionSpec{}, "an export's port must be between 1 and 65535"
		}
		spec.Exports = append(spec.Exports, oramav1alpha1.PeerExport{TargetKind: item.TargetKind, TargetID: item.TargetID, Port: item.Port})
	}
	for _, item := range imports {
		if message := validatePeerServiceName(item.Name); message != "" {
			return oramav1alpha1.PeerConnectionSpec{}, "an import " + message
		}
		if item.Port < 1 || item.Port > 65535 {
			return oramav1alpha1.PeerConnectionSpec{}, "an import's port must be between 1 and 65535"
		}
		spec.Imports = append(spec.Imports, oramav1alpha1.PeerImport{Name: item.Name, Port: item.Port})
	}
	return spec, ""
}

func (r *LakeReconciler) syncLakePeer(ctx context.Context, lake *oramav1alpha1.Lake, row *LakePeer) (LakePeerStatus, error) {
	spec, message := lakePeerSpec(lake, row)
	if message != "" {
		return LakePeerStatus{PeerID: row.ID, Phase: oramav1alpha1.PeerPhaseFailed, Message: message, ObservedGeneration: row.Generation}, nil
	}
	peer := &oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: lakePeerName(row.ID), Namespace: row.WorkspaceNamespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(peer), peer)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return LakePeerStatus{}, err
	}
	if !exists && row.Desired == oramav1alpha1.ConnectionDesiredAbsent {
		return LakePeerStatus{PeerID: row.ID, Phase: oramav1alpha1.PeerPhaseDeleted, Message: "nothing was running for this peer", ObservedGeneration: row.Generation}, nil
	}
	if exists && (!metav1.IsControlledBy(peer, lake) || peer.Spec.PeerID != row.ID) {
		return LakePeerStatus{PeerID: row.ID, Phase: oramav1alpha1.PeerPhaseFailed, Message: "PeerConnection name is occupied by another owner", ObservedGeneration: row.Generation}, nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, peer, func() error {
		peer.Labels = withLabels(peer.Labels, map[string]string{
			peerIDLabel: row.ID, serviceLakeLabel: lake.Name, workspaceLabel: row.WorkspaceID,
		})
		peer.Spec = spec
		return controllerutil.SetControllerReference(lake, peer, r.Scheme)
	}); err != nil {
		return LakePeerStatus{}, err
	}
	status := LakePeerStatus{PeerID: row.ID, Phase: oramav1alpha1.PeerPhasePending, Message: "waiting for the PeerConnection controller", ObservedGeneration: row.Generation}
	if peer.Status.ObservedGeneration == row.Generation && peer.Status.Phase != "" {
		status.Phase, status.Message = peer.Status.Phase, peer.Status.Message
	}
	if row.Desired == oramav1alpha1.ConnectionDesiredAbsent && status.Phase == oramav1alpha1.PeerPhaseDeleted && peer.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, peer); err != nil && !apierrors.IsNotFound(err) {
			return LakePeerStatus{}, fmt.Errorf("delete the removed peer's PeerConnection: %w", err)
		}
	}
	return status, nil
}
