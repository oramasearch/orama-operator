/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Cluster purposes and the observability rule (operator#456, #1059). A KubernetesCluster's
// `purpose` tag is nonprod or production. The managing cluster has no KubernetesCluster of its
// own: an empty clusterRef names it, and it counts as production, because it already holds every
// production credential and manages the production clusters.
//
// Which hub an agent may report to is decided from the two clusters' purposes:
//
//	agent cluster   may report to a hub on
//	production      the managing cluster (a hub with no clusterRef)
//	nonprod         the managing cluster, or a nonprod cluster
//
// Nonprod may report to the managing cluster's hub so one hub shows every cluster (Steve,
// 2026-10-31: "I told you to include nonprod ... change the earlier decision", replacing #456's
// one hub per purpose). Production still never reports to a nonprod hub. A nonprod agent there
// pages through its own receivers (agentAlerting): production's are never copied to it.
//
// A hub runs only on the managing cluster or on a nonprod cluster, never on a production cluster
// beside customer workloads. There is no separate support cluster (Steve, 2026-10-29: "let's put
// the observability databases and functionality on the management cluster instead of provisioning
// a prod support cluster").

const nonprodPurpose = "nonprod"

// managingClusterName is how a refusal names the cluster an empty clusterRef points at.
const managingClusterName = "the managing cluster"

// clusterPurpose is the purpose of the cluster clusterRef names, and whether that cluster exists.
// An empty clusterRef is the managing cluster itself, which is production.
func clusterPurpose(ctx context.Context, r client.Reader, clusterRef string) (string, bool, error) {
	if clusterRef == "" {
		return productionPurpose, true, nil
	}
	var cluster oramav1alpha1.KubernetesCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterRef}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return cluster.Spec.Tags[clusterPurposeTag], true, nil
}

// hubPurposeRefusal says why a hub may not run on the cluster clusterRef names, or "".
func hubPurposeRefusal(clusterRef, purpose string) string {
	switch {
	case clusterRef == "", purpose == nonprodPurpose:
		return ""
	case purpose == productionPurpose:
		return fmt.Sprintf("KubernetesCluster %s is production: production's observability hub runs on the managing cluster (no clusterRef), never beside customer workloads", clusterRef)
	case purpose == "":
		return fmt.Sprintf("KubernetesCluster %s has no %q tag, so it is not known which agents may report to it", clusterRef, clusterPurposeTag)
	}
	return fmt.Sprintf("KubernetesCluster %s has purpose %q; a hub runs only on the managing cluster or a nonprod cluster", clusterRef, purpose)
}

// agentPurposeRefusal says why an agent on agentCluster may not report to a hub on hubCluster, or
// "". A hub serves only agents of its own purpose.
func agentPurposeRefusal(agentCluster, agentPurpose, hubCluster, hubPurpose string) string {
	switch agentPurpose {
	case productionPurpose, nonprodPurpose:
	case "":
		return fmt.Sprintf("KubernetesCluster %s has no %q tag, so it is not known which hub it may report to", agentCluster, clusterPurposeTag)
	default:
		return fmt.Sprintf("KubernetesCluster %s has purpose %q, which reports to no hub", agentCluster, agentPurpose)
	}
	if hubPurpose != agentPurpose && !(agentPurpose == nonprodPurpose && hubCluster == "") {
		return fmt.Sprintf("an agent on %s cluster %s reports only to a %s hub, and %s is %s: refused",
			agentPurpose, agentCluster, agentPurpose, firstNonEmpty(hubCluster, managingClusterName), firstNonEmpty(hubPurpose, "untagged"))
	}
	return ""
}

// agentClusterName is the name an agent's cluster is registered under in Pixie and labelled with in
// the hub's metrics: its KubernetesCluster, or, for an agent on the managing cluster (no clusterRef),
// the hub's own name, which is what the hub already calls its own cluster (hubFederatedClusters).
// An agent on the managing cluster reports only to the managing cluster's hub (the purpose rule).
func agentClusterName(agent *oramav1alpha1.ObservabilityAgent, hub *oramav1alpha1.ObservabilityHub) string {
	if agent.Spec.ClusterRef != "" {
		return agent.Spec.ClusterRef
	}
	return firstNonEmpty(hub.Spec.ClusterRef, hub.Name)
}

// agentAlerting is where an agent's cluster pages: its own receivers when it names them, otherwise
// its hub's when the two are of one purpose, otherwise nowhere (a nonprod agent on the managing
// cluster's hub naming none: production's receivers are never copied to a nonprod cluster).
func agentAlerting(agent *oramav1alpha1.ObservabilityAgentSpec, purpose, hubPurpose string, hub *oramav1alpha1.ObservabilityAlerting) *oramav1alpha1.ObservabilityAlerting {
	switch {
	case agent.Alerting != nil:
		return agent.Alerting
	case purpose == hubPurpose:
		return hub
	}
	return nil
}
