/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
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

// Lake-declared egress (operator#470, stage 3 of secure networking). Lake writes
// network_egress_rules (migration 0084_network); the operator materializes one Lake-controlled
// EgressRule per row in the workspace namespace and writes only network_egress_rule_status. The
// EgressRule controller renders the policy, and for a DNS name the workspace's egress gateway.

const egressRuleIDLabel = "orama.cloud/egress-rule-id"

// LakeNetworkEgressRule is one network_egress_rules desired row and its current status.
type LakeNetworkEgressRule struct {
	ID                 string
	CustomerID         string
	WorkspaceID        string
	WorkspaceNamespace string
	SourceKind         string
	SourceID           string
	CIDR               string
	DNSName            string
	Ports              []int32
	Desired            string
	Generation         int64
	Status             *LakeNetworkEgressRuleStatus
}

// LakeNetworkEgressRuleStatus is written only to network_egress_rule_status.
type LakeNetworkEgressRuleStatus struct {
	RuleID             string
	Phase              string
	Message            string
	ObservedGeneration int64
}

// LakeNetworkEgressSnapshot is returned only after network_guard exists.
type LakeNetworkEgressSnapshot struct {
	Workspaces []LakeNetworkWorkspace
	Rules      []LakeNetworkEgressRule
}

func lakeEgressRuleName(id string) string {
	return "egress-" + id
}

func (r *LakeReconciler) reconcileLakeEgressRules(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeServiceStore,
) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	snapshot, err := store.ListNetworkEgressRules(ctx)
	if errors.Is(err, errNetworkGuardMissing) {
		log.Info("egress rules skipped: " + err.Error())
		return false
	}
	if err != nil {
		log.Info("egress rules deferred: cannot read network egress rules", "err", err.Error())
		return false
	}
	return r.syncLakeEgressRules(ctx, lake, store, snapshot)
}

func (r *LakeReconciler) syncLakeEgressRules(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	store LakeServiceStore,
	snapshot LakeNetworkEgressSnapshot,
) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	workspaceByID := make(map[string]LakeNetworkWorkspace, len(snapshot.Workspaces))
	keep := make(map[string]map[string]bool, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		workspaceByID[workspace.ID] = workspace
		keep[workspace.Namespace] = map[string]bool{}
	}
	changing := false
	for i := range snapshot.Rules {
		row := &snapshot.Rules[i]
		workspace, found := workspaceByID[row.WorkspaceID]
		if !found || workspace.Namespace == "" || workspace.Namespace != row.WorkspaceNamespace {
			status := failedEgressRuleStatus(row, "workspace and its reconciled namespace are required")
			if writeEgressRuleStatus(ctx, store, row, status) != nil {
				changing = true
			}
			continue
		}
		keep[workspace.Namespace][lakeEgressRuleName(row.ID)] = true
		status, err := r.syncLakeEgressRule(ctx, lake, row)
		if err != nil {
			log.Error(err, "egress rule reconciliation deferred", "rule", row.ID)
			changing = true
			continue
		}
		if writeEgressRuleStatus(ctx, store, row, status) != nil {
			log.Info("egress rule status write deferred", "rule", row.ID)
			changing = true
		}
		settled := status.ObservedGeneration == row.Generation &&
			(status.Phase == oramav1alpha1.EgressRulePhaseReady || status.Phase == oramav1alpha1.EgressRulePhaseDeleted ||
				status.Phase == oramav1alpha1.EgressRulePhaseFailed)
		if !settled {
			changing = true
		}
	}
	// A hard-removed row leaves an orphan CR; its finalizer removes the allowances. The operator's
	// own internetEgress rule carries no row id and is the workspace reconciler's.
	for _, workspace := range snapshot.Workspaces {
		var rules oramav1alpha1.EgressRuleList
		if err := r.List(ctx, &rules, client.InNamespace(workspace.Namespace), client.HasLabels{egressRuleIDLabel}); err != nil {
			log.Error(err, "egress rule orphan list deferred", "workspace", workspace.ID)
			changing = true
			continue
		}
		for i := range rules.Items {
			rule := &rules.Items[i]
			if keep[workspace.Namespace][rule.Name] || !metav1.IsControlledBy(rule, lake) || !rule.DeletionTimestamp.IsZero() {
				continue
			}
			changing = true
			if err := r.Delete(ctx, rule); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "orphan egress rule deletion deferred", "rule", rule.Name)
			}
		}
	}
	return changing
}

func (r *LakeReconciler) syncLakeEgressRule(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	row *LakeNetworkEgressRule,
) (LakeNetworkEgressRuleStatus, error) {
	if message := validateNetworkEgressRule(row); message != "" {
		return failedEgressRuleStatus(row, message), nil
	}
	rule := &oramav1alpha1.EgressRule{ObjectMeta: metav1.ObjectMeta{
		Name:      lakeEgressRuleName(row.ID),
		Namespace: row.WorkspaceNamespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rule), rule); err == nil {
		if !metav1.IsControlledBy(rule, lake) || rule.Spec.RuleID != row.ID ||
			rule.Spec.LakeRef != lake.Name || rule.Spec.WorkspaceID != row.WorkspaceID {
			return failedEgressRuleStatus(row, "EgressRule name is occupied by another owner or row"), nil
		}
	} else if !apierrors.IsNotFound(err) {
		return LakeNetworkEgressRuleStatus{}, err
	}
	dnsName, _ := normalizeEgressDNSName(row.DNSName)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rule, func() error {
		rule.Labels = withLabels(rule.Labels, map[string]string{
			egressRuleIDLabel: row.ID,
			serviceLakeLabel:  lake.Name,
			workspaceLabel:    row.WorkspaceID,
		})
		rule.Spec = oramav1alpha1.EgressRuleSpec{
			LakeRef:     lake.Name,
			RuleID:      row.ID,
			CustomerID:  row.CustomerID,
			WorkspaceID: row.WorkspaceID,
			SourceKind:  row.SourceKind,
			SourceID:    row.SourceID,
			CIDR:        row.CIDR,
			DNSName:     dnsName,
			Ports:       sortedUniqueInt32(append([]int32(nil), sortedInt32(row.Ports)...)),
			Desired:     row.Desired,
			Generation:  row.Generation,
		}
		return controllerutil.SetControllerReference(lake, rule, r.Scheme)
	})
	if err != nil {
		return LakeNetworkEgressRuleStatus{}, err
	}
	status := LakeNetworkEgressRuleStatus{
		RuleID:             row.ID,
		Phase:              oramav1alpha1.EgressRulePhasePending,
		Message:            "waiting for the EgressRule controller",
		ObservedGeneration: row.Generation,
	}
	if rule.Status.ObservedGeneration == row.Generation && rule.Status.Phase != "" {
		status.Phase = rule.Status.Phase
		status.Message = rule.Status.Message
	}
	if row.Desired == oramav1alpha1.EgressRuleDesiredAbsent &&
		status.Phase == oramav1alpha1.EgressRulePhaseDeleted && rule.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, rule); err != nil && !apierrors.IsNotFound(err) {
			return LakeNetworkEgressRuleStatus{}, err
		}
	}
	return status, nil
}

func validateNetworkEgressRule(row *LakeNetworkEgressRule) string {
	ids := map[string]string{"rule id": row.ID, "customer id": row.CustomerID, "workspace id": row.WorkspaceID}
	if row.SourceID != "" {
		ids["source id"] = row.SourceID
	}
	for name, value := range ids {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return name + " must be a canonical UUID"
		}
	}
	if len(validation.IsDNS1123Label(row.WorkspaceNamespace)) != 0 {
		return "workspace namespace is invalid"
	}
	switch row.SourceKind {
	case "":
		if row.SourceID != "" {
			return "source_kind and source_id are set together"
		}
	case oramav1alpha1.EgressRuleSourceApp, oramav1alpha1.EgressRuleSourceService, oramav1alpha1.EgressRuleSourceAgent:
		if row.SourceID == "" {
			return "source_kind and source_id are set together"
		}
	default:
		return "source kind must be app, service or agent"
	}
	if (row.CIDR == "") == (row.DNSName == "") {
		return "exactly one of cidr and dns_name is required"
	}
	if row.CIDR != "" {
		if _, err := parseEgressCIDR(row.CIDR); err != nil {
			return err.Error()
		}
	}
	if row.DNSName != "" {
		if _, err := normalizeEgressDNSName(row.DNSName); err != nil {
			return err.Error()
		}
	}
	if len(row.Ports) == 0 || len(row.Ports) > maxEgressRulePorts {
		return fmt.Sprintf("ports must list between 1 and %d ports", maxEgressRulePorts)
	}
	for _, port := range row.Ports {
		if port < 1 || port > 65535 {
			return "ports must be between 1 and 65535"
		}
	}
	if row.Desired != oramav1alpha1.EgressRuleDesiredPresent && row.Desired != oramav1alpha1.EgressRuleDesiredAbsent {
		return "desired must be present or absent"
	}
	if row.Generation < 1 {
		return "generation must be positive"
	}
	return ""
}

func failedEgressRuleStatus(row *LakeNetworkEgressRule, message string) LakeNetworkEgressRuleStatus {
	return LakeNetworkEgressRuleStatus{
		RuleID:             row.ID,
		Phase:              oramav1alpha1.EgressRulePhaseFailed,
		Message:            message,
		ObservedGeneration: row.Generation,
	}
}

func writeEgressRuleStatus(
	ctx context.Context,
	store LakeServiceStore,
	row *LakeNetworkEgressRule,
	status LakeNetworkEgressRuleStatus,
) error {
	if row.Status != nil && *row.Status == status {
		return nil
	}
	if err := store.UpsertNetworkEgressRuleStatus(ctx, status); err != nil {
		return err
	}
	row.Status = &status
	return nil
}
