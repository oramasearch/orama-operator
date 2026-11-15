/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Valkey services (preview) run on valkey-io/valkey-operator 0.6.0, valkey.io/v1alpha1, which its
// README says is not ready for production. Field names follow that release's CRD and source.
var valkeyClusterGVK = schema.GroupVersionKind{Group: "valkey.io", Version: "v1alpha1", Kind: "ValkeyCluster"}

const (
	// valkeyShards: cluster mode needs three primaries.
	valkeyShards = 3
	valkeyPort   = 6379
	valkeyUser   = "app"
	// valkeyClusterLabel is the label the operator puts on every ValkeyNode claim.
	valkeyClusterLabel = "valkey.io/cluster"
	valkeyManagedBy    = "valkey-operator"
)

type valkeyServiceKind struct{}

func (valkeyServiceKind) title() string { return "Valkey" }

func (valkeyServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{{gvk: valkeyClusterGVK, name: in.name}}
}

// valkeyDeniedCategories are the command categories the tenant's app user may not run
// (operator#771): @admin (CONFIG, ACL, MODULE, DEBUG, SHUTDOWN, REPLICAOF, CLUSTER's writing
// subcommands, ...) and @dangerous (KEYS, FLUSHALL, FLUSHDB, MIGRATE, RESTORE, MONITOR, INFO, ...).
// With them the credential a tenant receives could reconfigure the server, load a module or write
// files with CONFIG SET dir. Every other command, every key and every channel stay allowed.
var valkeyDeniedCategories = []interface{}{"@admin", "@dangerous"}

// valkeyUsers: the app user with every command but the denied categories, every key and channel,
// the built-in default user switched off — unmanaged, it would accept unauthenticated clients —
// and the Orama operator's own user, which may only run ACL USERS (operator#1172). The
// valkey-operator's probes and replication use its system users. Every field the CRD defaults is
// written, so the array reads back equal and a steady cluster is never rewritten.
func valkeyUsers(secretName, operatorSecretName string) []interface{} {
	return []interface{}{
		map[string]interface{}{"name": "default", "enabled": false, "nopass": false, "resetpass": false},
		map[string]interface{}{
			"name": valkeyUser, "enabled": true, "nopass": false, "resetpass": false,
			"passwordSecret": map[string]interface{}{"name": secretName, "keys": []interface{}{"password"}},
			"commands":       map[string]interface{}{"allow": []interface{}{"@all"}, "deny": valkeyDeniedCategories},
			"keys":           map[string]interface{}{"readWrite": []interface{}{"*"}},
			"channels":       map[string]interface{}{"patterns": []interface{}{"*"}},
		},
		map[string]interface{}{
			"name": valkeyOperatorUser, "enabled": true, "nopass": false, "resetpass": false,
			"passwordSecret": map[string]interface{}{"name": operatorSecretName, "keys": []interface{}{serviceOperatorSecretKey}},
			"commands":       map[string]interface{}{"allow": []interface{}{"acl|users"}},
		},
	}
}

func (k valkeyServiceKind) build(in catalogService, _ string) []catalogResource {
	secretName, _ := k.passwordSecret(in.name)
	operatorSecretName, _ := k.operatorSecret(in.name)
	return []catalogResource{{catalogObject: k.objects(in)[0], fields: []serviceField{
		// The `restricted` Pod Security Standard of a workspace namespace (operator#435), on the
		// pod and on both of its containers, with their sizes stated.
		// The valkey image declares no numeric user, so with runAsNonRoot alone the kubelet refuses
		// to start it ("image will run as root"). 999 and 1000 are the image's own valkey user and
		// group.
		{path: []string{"spec", "podSecurityContext"}, value: map[string]interface{}{
			"runAsNonRoot": true, "runAsUser": int64(999), "runAsGroup": int64(1000), "fsGroup": int64(1000),
			"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
		}},
		{path: []string{"spec", "resources"}, value: serviceResources("50m", "128Mi", "250m", "256Mi")},
		{path: []string{"spec", "containers"}, onChange: true, value: []interface{}{restrictedContainer("server", nil)}},
		{path: []string{"spec", "exporter", "securityContext"}, value: restrictedContainerSecurityContext()},
		{path: []string{"spec", "exporter", "resources"}, value: serviceResources("10m", "32Mi", "50m", "64Mi")},
		{path: []string{"spec", "shards"}, value: int64(valkeyShards)},
		{path: []string{"spec", "replicas"}, value: int64(in.spec.Instances - 1)},
		{path: []string{"spec", "users"}, value: valkeyUsers(secretName, operatorSecretName)},
		// reclaimPolicy stays the default Retain; deletion removes the claims itself.
		{path: []string{"spec", "persistence"}, createOnly: true, value: map[string]interface{}{"size": serviceStorage(in.spec)}},
	}}}
}

func (valkeyServiceKind) passwordSecret(name string) (string, string) {
	return name + "-credentials", "password"
}

func (valkeyServiceKind) operatorSecret(name string) (string, string) {
	return name + serviceOperatorSecretSuffix, serviceOperatorSecretKey
}

// unmanagedUsers: ACL USERS on every node, as the operator's own user, without the users the
// operators make.
func (valkeyServiceKind) unmanagedUsers(ctx context.Context, _ *LakeReconciler, in catalogService, _ map[string]*unstructured.Unstructured) ([]string, error) {
	if in.operatorPassword == "" {
		return nil, fmt.Errorf("no operator password")
	}
	return serviceUserListers.valkeyACLUsers(ctx, in, valkeyOperatorUser, in.operatorPassword)
}

// valkeyHost is the cluster's client Service, valkey-<name> (headless: a cluster client reaches
// every node through it).
func valkeyHost(in catalogService) string {
	return fmt.Sprintf("valkey-%s.%s.svc.cluster.local", in.name, in.namespace)
}

func (valkeyServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("%s:%d", valkeyHost(in), valkeyPort)
}

// ready: the ValkeyCluster's Ready condition.
func (valkeyServiceKind) ready(_ catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	cluster := found[valkeyClusterGVK.Kind]
	if cluster == nil || !conditionTrue(cluster.Object, "Ready") {
		return false, "waiting for the Valkey cluster"
	}
	return true, ""
}

func (valkeyServiceKind) credential(_ context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, password string) ([]byte, string, error) {
	if password == "" {
		return nil, "", nil
	}
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Cluster  bool   `json:"cluster"`
	}{serviceKindValkey, valkeyServiceKind{}.endpoint(in), valkeyHost(in), valkeyPort, valkeyUser, password, true})
	return credential, "", err
}

// podSelector: every node of the cluster.
func (valkeyServiceKind) podSelector(in catalogService) map[string]string {
	return map[string]string{valkeyClusterLabel: in.name}
}

// claimSelectors: each ValkeyNode's claim carries the cluster label.
func (valkeyServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{valkeyClusterLabel: in.name, "app.kubernetes.io/managed-by": valkeyManagedBy}}
}

// nameOverhead: the StatefulSet valkey-<name>-<shard>-<node> plus its revision-hash suffix.
func (valkeyServiceKind) nameOverhead() int {
	return len("valkey-") + len("-2-2") + statefulSetRevisionSuffix
}
