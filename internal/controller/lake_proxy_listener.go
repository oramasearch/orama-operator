/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lake's proxy listener (lake#1132): a second port, serving the same routes, on which lake believes
// the client address in X-Real-IP / X-Forwarded-For. Only ingress-nginx and zetta-tunnel's ingress
// gateway may reach it (the Lake's NetworkSandbox), and the Lake Ingress and LakeApp auth
// subrequests use it. Every other caller (the operator, library agents, workspaces, builds,
// methodologies, e2e) keeps the main port, where lake believes no forwarded address
// (LAKE_TRUSTED_PROXY_CIDRS=none), so a pod running customer code cannot name a new client
// address per request to escape lake's per-client limits.
const (
	lakeProxyPortName         = "proxy"
	lakeProxyServicePort      = 8080
	lakeProxyPortEnv          = "LAKE_PROXY_PORT"
	lakeTrustedProxyCIDRsEnv  = "LAKE_TRUSTED_PROXY_CIDRS"
	lakeTrustedProxyCIDRsNone = "none"
)

// lakeProxyContainerPort is the port lake's proxy listener binds: the one above the Lake's container
// port, or the one below when the container port is 65535.
func lakeProxyContainerPort(lake *oramav1alpha1.Lake) int32 {
	port := int32(lakeContainerPort(lake))
	if port >= 65535 {
		return port - 1
	}
	return port + 1
}

// lakeProxyEnv opens the proxy listener and tells lake to believe no forwarded address on the main
// port. Both are the operator's: spec.env cannot set them.
func lakeProxyEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: lakeProxyPortEnv, Value: strconv.Itoa(int(lakeProxyContainerPort(lake)))},
		{Name: lakeTrustedProxyCIDRsEnv, Value: lakeTrustedProxyCIDRsNone},
	}
}
