// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A workspace Zetta's approved connector destinations (operator#882, for zetta#1469).
//
// The engine dials a private or loopback connector source only when
// ZETTA_CONNECTOR_ALLOW_DESTINATIONS names it as an exact host:port; metadata and link-local
// addresses are refused whatever the variable says. The operator renders the variable on every
// managed Zetta service and workspace Zetta (every pod of its OramaZettaCluster, so every node that
// takes part in a distributed fetch) from the Lake services it already reconciles in that Zetta's
// workspace namespace, and from nothing else:
//
//   - postgres: the CloudNativePG Cluster <name> gives <name>-rw.<namespace>.svc.cluster.local:5432;
//   - tidb (the MySQL protocol): the TidbCluster <name> gives <name>-tidb.<namespace>.svc.cluster.local:4000;
//   - sqlite, duckdb, polars: the StatefulSet <name> gives <name>.<namespace>.svc.cluster.local:8080.
//
// Each entry is the authority of the endpoint lake hands the workspace for that service (its
// credential), so the connector dials exactly what is approved. An object counts only when the Lake
// controls it, carries the Lake's service labels for that kind, and is not being deleted. There are no
// wildcards and no address ranges, and a namespace never names another namespace's services. A
// namespace with none of these services gets an empty value, which approves no private destination.
//
// The value is the operator's alone: a ZETTA_CONNECTOR_ALLOW_DESTINATIONS stated in
// spec.services.zettaEnv is dropped, so no Lake-wide or per-cluster setting can widen it. The
// namespace's NetworkSandbox still decides whether the pod may reach the service at all.

// zettaConnectorAllowEnv is the engine's connector destination approval variable.
const zettaConnectorAllowEnv = "ZETTA_CONNECTOR_ALLOW_DESTINATIONS"

// zettaConnectorAllowNone approves no private or loopback connector destination.
const zettaConnectorAllowNone = ""

// tidbClusterListGVK lists TiDB Operator clusters.
var tidbClusterListGVK = schema.GroupVersionKind{Group: tidbClusterGVK.Group, Version: tidbClusterGVK.Version, Kind: tidbClusterGVK.Kind + "List"}

// connectorSource is one service kind a workspace Zetta reads as a connector source: the object
// that marks the service in its namespace, and the host:port its endpoint gives.
type connectorSource struct {
	kind      string
	list      schema.GroupVersionKind
	authority func(name, namespace string) (string, error)
}

// connectorSources is every kind whose endpoint a workspace Zetta may approve, in a fixed order.
var connectorSources = []connectorSource{
	{kind: serviceKindPostgres, list: cnpgClusterListGVK, authority: func(name, namespace string) (string, error) {
		return cnpgReadWriteAuthority(name, namespace), nil
	}},
	{kind: serviceKindTiDB, list: tidbClusterListGVK, authority: catalogEndpointAuthority(serviceKindTiDB)},
	{kind: serviceKindSQLite, list: statefulSetListGVK(), authority: catalogEndpointAuthority(serviceKindSQLite)},
	{kind: serviceKindDuckDB, list: statefulSetListGVK(), authority: catalogEndpointAuthority(serviceKindDuckDB)},
	{kind: serviceKindPolars, list: statefulSetListGVK(), authority: catalogEndpointAuthority(serviceKindPolars)},
}

func statefulSetListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: statefulSetGVK.Group, Version: statefulSetGVK.Version, Kind: statefulSetGVK.Kind + "List"}
}

// catalogEndpointAuthority is the host:port of a catalog kind's endpoint for the service object
// <name>, with any URL scheme removed.
func catalogEndpointAuthority(kind string) func(name, namespace string) (string, error) {
	return func(name, namespace string) (string, error) {
		catalog, found := catalogKinds[kind]
		if !found {
			return "", fmt.Errorf("no catalog kind %q", kind)
		}
		endpoint := catalog.endpoint(catalogService{name: name, namespace: namespace})
		parsed, err := url.Parse(endpoint)
		if err == nil && parsed.Host != "" {
			return parsed.Host, nil
		}
		return endpoint, nil
	}
}

// withZettaConnectorAllow is env with every stated ZETTA_CONNECTOR_ALLOW_DESTINATIONS removed and
// the operator's value appended.
func withZettaConnectorAllow(env []corev1.EnvVar, allow string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+1)
	for _, variable := range env {
		if variable.Name != zettaConnectorAllowEnv {
			out = append(out, variable)
		}
	}
	return append(out, corev1.EnvVar{Name: zettaConnectorAllowEnv, Value: allow})
}

// workspaceZettaConnectorAllow is the connector approval of a Zetta in a workspace namespace, or,
// when the namespace cannot be read, the value the cluster already runs with (current), so a failed
// read never rolls the StatefulSet; the error is returned for the caller to log.
func workspaceZettaConnectorAllow(
	ctx context.Context,
	reader client.Reader,
	lake *oramav1alpha1.Lake,
	namespace string,
	current []corev1.EnvVar,
) (string, error) {
	allow, err := lakeZettaConnectorAllow(ctx, reader, lake, namespace)
	if err == nil || errors.Is(err, errZettaEgressTooLong) {
		return allow, err
	}
	if variable := findEnvVar(current, zettaConnectorAllowEnv); variable != nil {
		return variable.Value, err
	}
	return zettaConnectorAllowNone, err
}

// lakeZettaConnectorAllow renders ZETTA_CONNECTOR_ALLOW_DESTINATIONS for a Zetta in namespace from
// the Lake's postgres, tidb, sqlite, duckdb and polars services there. A kind whose CRD is not
// installed has no services. Too long a list is refused in favour of approving nothing.
func lakeZettaConnectorAllow(ctx context.Context, reader client.Reader, lake *oramav1alpha1.Lake, namespace string) (string, error) {
	var entries []string
	for _, source := range connectorSources {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(source.list)
		err := reader.List(ctx, list, client.InNamespace(namespace),
			client.MatchingLabels{serviceLakeLabel: lake.Name, serviceKindLabel: source.kind})
		if isKindNotInstalled(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("list %s services for the Zetta connector destinations: %w", source.kind, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if item.GetNamespace() != namespace || !item.GetDeletionTimestamp().IsZero() || !metav1.IsControlledBy(item, lake) {
				continue
			}
			authority, err := source.authority(item.GetName(), namespace)
			if err != nil {
				return "", err
			}
			entries = append(entries, authority)
		}
	}
	allow, err := joinZettaEgressEntries(entries)
	if err != nil {
		return zettaConnectorAllowNone, fmt.Errorf("the Zetta connector destinations: %w", err)
	}
	return allow, nil
}
