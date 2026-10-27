/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import "fmt"

// Cluster purposes and the observability rule (operator#456). A KubernetesCluster's `purpose` tag
// is nonprod, production, or prod-support: a cluster that runs support services for production
// (observability first) and no customer or test workload. prod-support counts as production for
// the cluster rules, and production credentials may reach it; they never reach nonprod.
//
// Which hub an agent reports to is decided from the two clusters' purposes, never from a field:
//
//	agent cluster   reports to a hub on
//	production      prod-support
//	prod-support    prod-support
//	nonprod         nonprod
//
// A hub runs only on a prod-support or a nonprod cluster.

const (
	nonprodPurpose     = "nonprod"
	prodSupportPurpose = "prod-support"
)

// productionTier is true for the purposes production credentials may reach.
func productionTier(purpose string) bool {
	return purpose == productionPurpose || purpose == prodSupportPurpose
}

// hubPurposeFor is the purpose of the hub an agent of agentPurpose reports to.
func hubPurposeFor(agentPurpose string) (string, bool) {
	switch agentPurpose {
	case productionPurpose, prodSupportPurpose:
		return prodSupportPurpose, true
	case nonprodPurpose:
		return nonprodPurpose, true
	}
	return "", false
}

// hubPurposeRefusal says why a hub may not run on a cluster of purpose, or "".
func hubPurposeRefusal(cluster, purpose string) string {
	switch purpose {
	case prodSupportPurpose, nonprodPurpose:
		return ""
	case productionPurpose:
		return fmt.Sprintf("KubernetesCluster %s is production: production's observability hub runs on a prod-support cluster, never beside customer workloads", cluster)
	case "":
		return fmt.Sprintf("KubernetesCluster %s has no %q tag, so it is not known which agents may report to it", cluster, clusterPurposeTag)
	}
	return fmt.Sprintf("KubernetesCluster %s has purpose %q; a hub runs only on a prod-support or nonprod cluster", cluster, purpose)
}

// agentPurposeRefusal says why an agent on agentCluster may not report to a hub on hubCluster, or "".
func agentPurposeRefusal(agentCluster, agentPurpose, hubCluster, hubPurpose string) string {
	want, ok := hubPurposeFor(agentPurpose)
	if !ok {
		if agentPurpose == "" {
			return fmt.Sprintf("KubernetesCluster %s has no %q tag, so it is not known which hub it may report to", agentCluster, clusterPurposeTag)
		}
		return fmt.Sprintf("KubernetesCluster %s has purpose %q, which reports to no hub", agentCluster, agentPurpose)
	}
	if hubPurpose != want {
		return fmt.Sprintf("an agent on %s cluster %s reports only to a %s hub, and %s is %s: refused", agentPurpose, agentCluster, want, hubCluster, firstNonEmpty(hubPurpose, "untagged"))
	}
	return ""
}
