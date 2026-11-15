/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClickHouse services run on the Altinity operator 0.27.4. Field names follow that release's
// install bundle: a ClickHouseInstallation (clickhouse.altinity.com/v1) of one shard and, for more
// than one replica, a ClickHouseKeeperInstallation (clickhouse-keeper.altinity.com/v1) the
// installation points at through spec.configuration.zookeeper.keeper.
var (
	clickHouseInstallationGVK = schema.GroupVersionKind{Group: "clickhouse.altinity.com", Version: "v1", Kind: "ClickHouseInstallation"}
	clickHouseKeeperGVK       = schema.GroupVersionKind{Group: "clickhouse-keeper.altinity.com", Version: "v1", Kind: "ClickHouseKeeperInstallation"}
)

const (
	clickHouseUser = "app"
	// clickHouseDefaultUser is ClickHouse's built-in user.
	clickHouseDefaultUser = "default"
	// clickHouseProfile is the settings profile of the app user.
	clickHouseProfile     = "app"
	clickHouseDatabase    = "default"
	clickHouseHTTPPort    = 8123
	clickHouseNativePort  = 9000
	clickHouseClusterName = "main"
	clickHouseKeeperName  = "keeper"
	// clickHouseVolumeTemplate names the volume claim template on both installations.
	clickHouseVolumeTemplate = "data"
	// clickHouseKeeperStorage sizes the one Keeper node's volume: replication metadata only.
	clickHouseKeeperStorage = "1Gi"
	// clickHouseCompleted is the installation's status.status once the operator has applied it.
	clickHouseCompleted = "Completed"
	// Labels Altinity puts on every PVC of an installation.
	clickHouseInstallationLabel = "clickhouse.altinity.com/chi"
	clickHouseAppLabel          = "clickhouse.altinity.com/app"
	clickHouseKeeperLabel       = "clickhouse-keeper.altinity.com/chk"
	clickHouseKeeperAppLabel    = "clickhouse-keeper.altinity.com/app"
	clickHouseAppValue          = "chop"
)

type clickHouseServiceKind struct{}

func (clickHouseServiceKind) title() string { return "ClickHouse" }

// Both installations are named <lake>-<slug>: Altinity prefixes every object it derives with chi-
// or chk- (and names the client Services clickhouse-<name> and keeper-<name>), so they never
// collide.
func (clickHouseServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{
		{gvk: clickHouseInstallationGVK, name: in.name},
		{gvk: clickHouseKeeperGVK, name: in.name},
	}
}

func clickHouseVolumeClaimTemplate(size string) []interface{} {
	return []interface{}{map[string]interface{}{
		"name": clickHouseVolumeTemplate,
		"spec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": size}},
		},
	}}
}

const (
	clickHousePodTemplateName = "restricted"
	// The Altinity operator's default is clickhouse/clickhouse-server:latest, which changes under
	// a running service. A pod template has to name its image, so it is pinned here.
	clickHouseServerImage = "clickhouse/clickhouse-server:25.3"
	clickHouseKeeperImage = "clickhouse/clickhouse-keeper:25.3"
)

// clickHousePodTemplates is the pod template every ClickHouse and Keeper pod is built from: the
// `restricted` security context a workspace namespace requires (operator#435), running as the
// image's own clickhouse user (UID and GID 101), with stated sizes.
func clickHousePodTemplates(container, image string, resources map[string]interface{}) []interface{} {
	return []interface{}{map[string]interface{}{
		"name": clickHousePodTemplateName,
		"spec": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"runAsNonRoot": true, "runAsUser": int64(101), "runAsGroup": int64(101), "fsGroup": int64(101),
				"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
			},
			"containers": []interface{}{map[string]interface{}{
				"name": container, "image": image,
				"securityContext": restrictedContainerSecurityContext(), "resources": resources,
			}},
		},
	}}
}

// clickHouseAppGrants are the app user's only privileges: everything on its own database.
func clickHouseAppGrants() []interface{} {
	return []interface{}{"GRANT ALL ON " + clickHouseDatabase + ".*"}
}

func (k clickHouseServiceKind) build(in catalogService, password string) []catalogResource {
	objects := k.objects(in)
	digest := sha256.Sum256([]byte(password))
	configuration := map[string]interface{}{
		// Only the SHA-256 reaches the spec; the password itself stays in the Secret.
		//
		// The app user is the tenant's, never an administrator (operator#771). Its grants replace
		// the implicit ALL a users.xml user has on everything: ALL on the service's database only,
		// so no SYSTEM commands (SHUTDOWN, DROP caches, RELOAD config), no access management (it can
		// create no user or role), and no SOURCES (the file, url, remote and s3 table functions,
		// which would read the server's files or reach other hosts). ClickHouse refuses grants
		// together with access_management or allow_databases, and access_management defaults off.
		// The system tables a client needs to read its own schema are open without a grant.
		// networks/ip stays open because the workspace's NetworkPolicy decides who reaches the
		// server; pod addresses are not known ahead of time.
		"users": map[string]interface{}{
			clickHouseUser + "/password_sha256_hex": hex.EncodeToString(digest[:]),
			clickHouseUser + "/networks/ip":         []interface{}{"::/0"},
			clickHouseUser + "/profile":             clickHouseProfile,
			clickHouseUser + "/quota":               "default",
			clickHouseUser + "/grants/query":        clickHouseAppGrants(),
			// The built-in default user has no password; it is pinned here to loopback (operator#772),
			// which is the Altinity operator's default today, so a later chart or operator default
			// can never widen it. The operator still adds its host_regexp, the installation's own
			// pods by reverse DNS, which replicas use for distributed queries between themselves.
			clickHouseDefaultUser + "/networks/ip": []interface{}{"127.0.0.1", "::1"},
		},
		// The app user's own profile, so nothing a later change makes to "default" reaches it:
		// reads and writes, DDL in its database, no introspection functions (addressToLine and the
		// like read the server binary's memory map).
		"profiles": map[string]interface{}{
			clickHouseProfile + "/readonly":                      int64(0),
			clickHouseProfile + "/allow_ddl":                     int64(1),
			clickHouseProfile + "/allow_introspection_functions": int64(0),
		},
		"clusters": []interface{}{map[string]interface{}{
			"name":   clickHouseClusterName,
			"layout": map[string]interface{}{"shardsCount": int64(1), "replicasCount": int64(in.spec.Instances)},
		}},
	}
	replicated := in.spec.Instances > 1
	if replicated {
		configuration["zookeeper"] = map[string]interface{}{"keeper": map[string]interface{}{"name": objects[1].name}}
	}
	resources := []catalogResource{{catalogObject: objects[0], fields: []serviceField{
		{path: []string{"spec", "configuration"}, value: configuration},
		{path: []string{"spec", "defaults", "templates", "dataVolumeClaimTemplate"}, value: clickHouseVolumeTemplate},
		{path: []string{"spec", "defaults", "templates", "podTemplate"}, value: clickHousePodTemplateName},
		{path: []string{"spec", "templates", "podTemplates"}, onChange: true, value: clickHousePodTemplates("clickhouse", clickHouseServerImage, serviceResources("100m", "512Mi", "500m", "1Gi"))},
		{path: []string{"spec", "templates", "volumeClaimTemplates"}, createOnly: true, value: clickHouseVolumeClaimTemplate(serviceStorage(in.spec))},
	}}}
	if replicated {
		// The Keeper goes first so the installation never points at a Keeper that is not there.
		keeper := catalogResource{catalogObject: objects[1], fields: []serviceField{
			{path: []string{"spec", "configuration", "clusters"}, value: []interface{}{map[string]interface{}{
				"name": clickHouseKeeperName, "layout": map[string]interface{}{"replicasCount": int64(1)},
			}}},
			{path: []string{"spec", "defaults", "templates", "dataVolumeClaimTemplate"}, value: clickHouseVolumeTemplate},
			{path: []string{"spec", "defaults", "templates", "podTemplate"}, value: clickHousePodTemplateName},
			{path: []string{"spec", "templates", "podTemplates"}, onChange: true, value: clickHousePodTemplates("clickhouse-keeper", clickHouseKeeperImage, serviceResources("50m", "128Mi", "250m", "512Mi"))},
			{path: []string{"spec", "templates", "volumeClaimTemplates"}, createOnly: true, value: clickHouseVolumeClaimTemplate(clickHouseKeeperStorage)},
		}}
		resources = append([]catalogResource{keeper}, resources...)
	}
	return resources
}

func (clickHouseServiceKind) passwordSecret(name string) (string, string) {
	return name + "-credentials", "password"
}

// clickHouseHost is the installation's own Service, clickhouse-<name>.
func clickHouseHost(in catalogService) string {
	return fmt.Sprintf("clickhouse-%s.%s.svc.cluster.local", in.name, in.namespace)
}

func (clickHouseServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("%s:%d", clickHouseHost(in), clickHouseHTTPPort)
}

// ready: the installation's status is Completed, and so is the Keeper's when there is one.
func (clickHouseServiceKind) ready(in catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	installation := found[clickHouseInstallationGVK.Kind]
	if status, _, _ := unstructured.NestedString(installationObject(installation), "status", "status"); status != clickHouseCompleted {
		return false, "waiting for the ClickHouse installation"
	}
	if in.spec.Instances > 1 {
		keeper := found[clickHouseKeeperGVK.Kind]
		if status, _, _ := unstructured.NestedString(installationObject(keeper), "status", "status"); status != clickHouseCompleted {
			return false, "waiting for the ClickHouse Keeper"
		}
	}
	return true, ""
}

func installationObject(obj *unstructured.Unstructured) map[string]interface{} {
	if obj == nil {
		return nil
	}
	return obj.Object
}

func (clickHouseServiceKind) credential(_ context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, password string) ([]byte, string, error) {
	if password == "" {
		return nil, "", nil
	}
	credential, err := json.Marshal(struct {
		Kind       string `json:"kind"`
		Endpoint   string `json:"endpoint"`
		Host       string `json:"host"`
		HTTPPort   int    `json:"http_port"`
		NativePort int    `json:"native_port"`
		User       string `json:"user"`
		Password   string `json:"password"`
		Database   string `json:"database"`
	}{serviceKindClickHouse, clickHouseServiceKind{}.endpoint(in), clickHouseHost(in), clickHouseHTTPPort, clickHouseNativePort, clickHouseUser, password, clickHouseDatabase})
	return credential, "", err
}

// podSelector: the installation's server pods; clients never reach the Keeper.
func (clickHouseServiceKind) podSelector(in catalogService) map[string]string {
	return map[string]string{clickHouseInstallationLabel: in.name, clickHouseAppLabel: clickHouseAppValue}
}

// claimSelectors: the installation's claims and the Keeper's. Altinity deletes both with their
// installation (reclaimPolicy Delete is its default); deletion still waits for them.
func (clickHouseServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{
		{clickHouseInstallationLabel: in.name, clickHouseAppLabel: clickHouseAppValue},
		{clickHouseKeeperLabel: in.name, clickHouseKeeperAppLabel: clickHouseAppValue},
	}
}

// nameOverhead: the Keeper StatefulSet chk-<name>-keeper-0-0 plus its revision-hash suffix.
func (clickHouseServiceKind) nameOverhead() int {
	return len("chk-") + len("-"+clickHouseKeeperName+"-0-0") + statefulSetRevisionSuffix
}

// statefulSetRevisionSuffix is the "-<10 characters>" Kubernetes appends to a StatefulSet's name
// in the controller-revision-hash label of each pod.
const statefulSetRevisionSuffix = 11
