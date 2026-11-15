/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Cassandra services run on K8ssandra Operator 1.33.0 (k8ssandra.io/v1alpha1), which drives
// cass-operator v1.32.0. Field names follow the 1.33.0 CRD; naming follows both operators' source.
var k8ssandraClusterGVK = schema.GroupVersionKind{Group: "k8ssandra.io", Version: "v1alpha1", Kind: "K8ssandraCluster"}

const (
	// cassandraVersion is Apache Cassandra 5.0.4, the base version K8ssandra 1.33.0 tests against.
	cassandraVersion    = "5.0.4"
	cassandraServerType = "cassandra"
	cassandraPort       = 9042
	// cassandraDatacenter is the Cassandra datacenter name clients use for local routing.
	cassandraDatacenter = "dc1"
	// cassandraClusterLabel is the label cass-operator puts on every claim of a cluster.
	cassandraClusterLabel = "cassandra.datastax.com/cluster"
	cassandraReady        = "Ready"
)

type cassandraServiceKind struct{}

func (cassandraServiceKind) title() string { return "Cassandra" }

func (cassandraServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{{gvk: k8ssandraClusterGVK, name: in.name}}
}

// cassandraSlugName names what cass-operator derives per namespace: the Cassandra cluster name
// (spec.cassandra.clusterName) and the CassandraDatacenter object (the datacenter's
// metadata.name). Both must be unique in the Lake's namespace, so neither can be "dc1", and
// cass-operator's StatefulSet <cluster>-<datacenter>-default-sts must stay under 60 characters
// (its webhook) and 52 (the pod revision-hash label). The slug is unique and keeps that name at 45
// whatever the Lake is called; the Cassandra datacenter itself is still named dc1 through
// datacenterName.
func cassandraSlugName(in catalogService) string { return in.svc.Slug }

func (k cassandraServiceKind) build(in catalogService, _ string) []catalogResource {
	slug := cassandraSlugName(in)
	datacenter := map[string]interface{}{
		"metadata":       map[string]interface{}{"name": slug},
		"datacenterName": cassandraDatacenter,
		"size":           int64(in.spec.Instances),
		"storageConfig": map[string]interface{}{"cassandraDataVolumeClaimSpec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": serviceStorage(in.spec)}},
		}},
	}
	return []catalogResource{{catalogObject: k.objects(in)[0], fields: []serviceField{
		// A workspace namespace enforces the `restricted` Pod Security Standard and the API
		// server refused cass-operator's pod, whose containers neither drop capabilities nor set
		// a seccomp profile (operator#435). The pod and each of its four containers carry the
		// security context here, and state their sizes so none takes the namespace default.
		// cass-operator runs Cassandra as UID and GID 999.
		{path: []string{"spec", "cassandra", "podSecurityContext"}, value: map[string]interface{}{
			"runAsNonRoot": true, "runAsUser": int64(999), "runAsGroup": int64(999), "fsGroup": int64(999),
			"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
		}},
		{path: []string{"spec", "cassandra", "resources"}, value: serviceResources("250m", "1Gi", "500m", "2Gi")},
		// Cassandra sizes its heap from the machine's memory, not the container's limit: on a 32GiB
		// node it asked for far more than 2Gi and was OOM-killed on start (operator#435). The heap
		// is stated, at half the container's limit, leaving the rest for off-heap memory.
		{path: []string{"spec", "cassandra", "config"}, value: map[string]interface{}{
			"jvmOptions": map[string]interface{}{"heap_initial_size": "1Gi", "heap_max_size": "1Gi"},
		}},
		{path: []string{"spec", "cassandra", "containers"}, onChange: true, value: []interface{}{
			restrictedContainer("cassandra", nil),
			restrictedContainer("server-system-logger", serviceResources("20m", "64M", "100m", "128M")),
		}},
		{path: []string{"spec", "cassandra", "initContainers"}, onChange: true, value: []interface{}{
			restrictedContainer("server-config-init-base", serviceResources("50m", "128M", "500m", "384M")),
			restrictedContainer("server-config-init", serviceResources("50m", "256M", "500m", "384M")),
		}},
		{path: []string{"spec", "cassandra", "serverType"}, value: cassandraServerType},
		{path: []string{"spec", "cassandra", "serverVersion"}, value: cassandraVersion},
		// The cluster name cannot change, and the datacenter's items carry CRD defaults, so both
		// are written once.
		{path: []string{"spec", "cassandra", "clusterName"}, createOnly: true, value: slug},
		{path: []string{"spec", "cassandra", "datacenters"}, createOnly: true, value: []interface{}{datacenter}},
	}}}
}

func (cassandraServiceKind) passwordSecret(string) (string, string) { return "", "" }

// cassandraHost is cass-operator's datacenter Service, <cluster>-<datacenter object>-service.
func cassandraHost(in catalogService) string {
	slug := cassandraSlugName(in)
	return fmt.Sprintf("%s-%s-service.%s.svc.cluster.local", slug, slug, in.namespace)
}

func (cassandraServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("%s:%d", cassandraHost(in), cassandraPort)
}

// ready: the datacenter's status (keyed by the CassandraDatacenter name) has cass-operator's
// Ready condition and progress Ready.
func (cassandraServiceKind) ready(in catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	cluster := found[k8ssandraClusterGVK.Kind]
	if cluster == nil {
		return false, "waiting for the Cassandra datacenter"
	}
	path := []string{"status", "datacenters", cassandraSlugName(in), "cassandra"}
	progress, _, _ := unstructured.NestedString(cluster.Object, append(path, "cassandraOperatorProgress")...)
	if progress != cassandraReady || !conditionTrue(cluster.Object, cassandraReady, append(path, "conditions")...) {
		return false, "waiting for the Cassandra datacenter"
	}
	return true, ""
}

// credential reads the superuser Secret K8ssandra generates, <cluster name>-superuser, keys
// username and password. K8ssandra makes it controlled by the K8ssandraCluster; one it does not
// control was not generated for this service.
func (cassandraServiceKind) credential(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured, _ string) ([]byte, string, error) {
	cluster := found[k8ssandraClusterGVK.Kind]
	if cluster == nil {
		return nil, "", nil
	}
	secretName := cassandraSlugName(in) + "-superuser"
	secret, err := readServiceSecret(ctx, c, in.namespace, secretName)
	if err != nil || secret == nil {
		return nil, "", err
	}
	if !metav1.IsControlledBy(secret, cluster) {
		return nil, foreignServiceMessage("Secret", in.namespace, secretName), nil
	}
	user, password := string(secret.Data["username"]), string(secret.Data["password"])
	if user == "" || password == "" {
		return nil, "", nil
	}
	// Cassandra's stock cassandra/cassandra superuser is dropped once the generated one exists
	// (operator#772). Best effort: a failure is logged and tried again on the next pass, and never
	// holds back the connection of a service that already works.
	if err := dropCassandraDefaultRole(ctx, in, user); err != nil {
		logf.FromContext(ctx).Error(err, "Cassandra's default superuser is not dropped yet; tried again on the next pass",
			"namespace", in.namespace, "service", in.svc.Slug)
	}
	credential, err := json.Marshal(struct {
		Kind       string `json:"kind"`
		Endpoint   string `json:"endpoint"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Datacenter string `json:"datacenter"`
		User       string `json:"user"`
		Password   string `json:"password"`
	}{serviceKindCassandra, cassandraServiceKind{}.endpoint(in), cassandraHost(in), cassandraPort, cassandraDatacenter, user, password})
	return credential, "", err
}

// podSelector: every Cassandra node of the cluster.
func (cassandraServiceKind) podSelector(in catalogService) map[string]string {
	return map[string]string{cassandraClusterLabel: cassandraSlugName(in)}
}

// claimSelectors: cass-operator keeps a datacenter's claims after it is deleted.
func (cassandraServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{cassandraClusterLabel: cassandraSlugName(in)}}
}

// nameOverhead: every name cass-operator derives comes from the slug, not <lake>-<slug>.
func (cassandraServiceKind) nameOverhead() int { return 0 }

// cassandraDefaultRole is the superuser Cassandra's CassandraRoleManager creates, with the
// well-known password "cassandra", when system_auth has no role yet. cass-operator creates the
// generated <cluster>-superuser beside it and never drops it (operator#772).
const cassandraDefaultRole = "cassandra"

// cassandraManagement is how the operator reaches a datacenter's management API (port 8080), the
// HTTP API cass-operator and k8ssandra-operator already drive every node through. The workspace
// NetworkPolicy allow-orama-operator-cassandra-management admits the operator's pods to it.
// Tests replace baseURL and client.
var cassandraManagement = struct {
	client  *http.Client
	baseURL func(in catalogService) string
}{
	client: &http.Client{Timeout: 10 * time.Second},
	baseURL: func(in catalogService) string {
		return fmt.Sprintf("http://%s:%d", cassandraHost(in), cassandraManagementPort)
	},
}

// cassandraDefaultRolesDropped remembers the datacenters whose default role is gone, so a steady
// service makes no management-API call. A restarted operator checks each one once again.
var cassandraDefaultRolesDropped sync.Map

// dropCassandraDefaultRole drops the stock `cassandra` role through the management API
// (DELETE /api/v0/ops/auth/role?username=cassandra), only once the role list shows the generated
// superuser, so the datacenter always keeps a superuser. Dropping it is permanent: with a role in
// system_auth, CassandraRoleManager never creates the default again.
func dropCassandraDefaultRole(ctx context.Context, in catalogService, superuser string) error {
	key := in.namespace + "/" + cassandraSlugName(in)
	if _, done := cassandraDefaultRolesDropped.Load(key); done {
		return nil
	}
	base := cassandraManagement.baseURL(in)
	roles, err := cassandraManagementRoles(ctx, base)
	if err != nil {
		return err
	}
	if !roles[superuser] {
		return fmt.Errorf("the generated superuser %s is not in the role list yet", superuser)
	}
	if superuser == cassandraDefaultRole {
		return fmt.Errorf("the generated superuser is named %s; it is never dropped", cassandraDefaultRole)
	}
	if roles[cassandraDefaultRole] {
		endpoint := base + "/api/v0/ops/auth/role?" + url.Values{"username": {cassandraDefaultRole}}.Encode()
		if _, err := cassandraManagementCall(ctx, http.MethodDelete, endpoint); err != nil {
			return fmt.Errorf("drop role %s: %w", cassandraDefaultRole, err)
		}
		logf.FromContext(ctx).Info("dropped Cassandra's default superuser", "namespace", in.namespace, "service", in.svc.Slug)
	}
	cassandraDefaultRolesDropped.Store(key, true)
	return nil
}

// cassandraManagementRoles lists the datacenter's roles (GET /api/v0/ops/auth/role): a JSON array
// with one object per role, its name under "name".
func cassandraManagementRoles(ctx context.Context, base string) (map[string]bool, error) {
	body, err := cassandraManagementCall(ctx, http.MethodGet, base+"/api/v0/ops/auth/role")
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	var listed []map[string]interface{}
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	roles := make(map[string]bool, len(listed))
	for _, role := range listed {
		if name, ok := role["name"].(string); ok {
			roles[name] = true
		}
	}
	return roles, nil
}

func cassandraManagementCall(ctx context.Context, method, endpoint string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := cassandraManagement.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("management API answered %s", response.Status)
	}
	return body, nil
}
