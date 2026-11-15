/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// One tenant must not use up what every tenant of a cluster shares (operator#782, lake#983,
// lake#987): the relay's one grants projection, a namespace's NetworkSandbox, the cluster's nodes.
// lake refuses a row past these limits when it is created; the operator holds the same numbers, so
// a row lake let through anyway (an older lake, a row written by hand) is refused here too. They
// are constants, the same on every cluster, with no setting. lake gets them as its environment, so
// the two can never disagree.
const (
	// lakeConnectionsPerWorkspace caps a workspace's present network connections; each adds
	// policies to the workspace's NetworkSandbox.
	lakeConnectionsPerWorkspace = 50
	// lakeEgressRulesPerWorkspace caps a workspace's present egress rules.
	lakeEgressRulesPerWorkspace = 100
	// lakeTunnelGrantsPerWorkspace and lakeTunnelGrantsPerCustomer cap live (unrevoked, unexpired)
	// tunnel grants, which all go into the relay's one projection: lake's defaults (lake#983).
	lakeTunnelGrantsPerWorkspace = 100
	lakeTunnelGrantsPerCustomer  = 500
	// lakeWorkspacesPerCustomer caps a customer's live workspaces; each is a namespace, a Zetta
	// and a volume (lake#987).
	lakeWorkspacesPerCustomer = 10
	// lakeServicesPerCustomer caps a customer's present services, each workspace's own Zetta
	// included (lake#987).
	lakeServicesPerCustomer = 50
)

// lakeLimitEnv is the limits lake reads today (lake#1292). lake has no limit of its own yet on
// network connections or egress rules; the operator holds those alone until it
// does. They are reserved: spec.env cannot change them.
func lakeLimitEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "LAKE_TUNNEL_GRANTS_PER_WORKSPACE", Value: strconv.Itoa(lakeTunnelGrantsPerWorkspace)},
		{Name: "LAKE_TUNNEL_GRANTS_PER_CUSTOMER", Value: strconv.Itoa(lakeTunnelGrantsPerCustomer)},
		{Name: "LAKE_WORKSPACES_PER_CUSTOMER", Value: strconv.Itoa(lakeWorkspacesPerCustomer)},
		{Name: "LAKE_SERVICES_PER_CUSTOMER", Value: strconv.Itoa(lakeServicesPerCustomer)},
	}
}

// lakeLimitEnvNames are the reserved names of lakeLimitEnv.
func lakeLimitEnvNames() map[string]bool {
	names := map[string]bool{}
	for _, variable := range lakeLimitEnv() {
		names[variable.Name] = true
	}
	return names
}

// rowLimit admits rows in the order lake created them (every list is ordered by created_at, id)
// until a key (a workspace, a customer) has its limit; the rows after are refused.
type rowLimit struct {
	limit  int
	what   string
	counts map[string]int
}

func newRowLimit(limit int, what string) *rowLimit {
	return &rowLimit{limit: limit, what: what, counts: map[string]int{}}
}

// admit counts one row for key and reports whether it is within the limit.
func (l *rowLimit) admit(key string) bool {
	l.counts[key]++
	return l.counts[key] <= l.limit
}

// refusal is the status message of a refused row.
func (l *rowLimit) refusal() string {
	return fmt.Sprintf("limit reached: at most %d %s (operator#782)", l.limit, l.what)
}
