// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha1" //nolint:gosec // mysql_native_password is defined over SHA-1.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TiDB services run on TiDB Operator v1.6.6, the stable 1.x API pingcap.com/v1alpha1. Field names
// follow that release's manifests/crd.yaml and its controller source.
var tidbClusterGVK = schema.GroupVersionKind{Group: "pingcap.com", Version: "v1alpha1", Kind: "TidbCluster"}

// configMapGVK is the ConfigMap holding a TiDB service's bootstrap SQL.
var configMapGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

const (
	// tidbVersion is the TiDB release the operator's own v1.6.6 examples run.
	tidbVersion = "v8.5.7"
	tidbPort    = 4000
	// tidbRootUser is TiDB's administrator. Only the operator signs in as it; tenants receive
	// tidbTenantUser (operator#771).
	tidbRootUser = "root"
	// tidbTenantUser is the user a tenant receives, with every privilege on its own databases and
	// none on TiDB itself (operator#771). tidbTenantDatabase is the database made for it.
	tidbTenantUser     = "app"
	tidbTenantDatabase = "app"
	// tidbTenantKey is the key of the tenant user's password in Secret <cluster>-tenant.
	tidbTenantKey = "password"
	// tidbRootKey is the key TiDB Operator reads the root password from in <cluster>-init.
	tidbRootKey = "root"
	// tidbInitSecretSuffix: with spec.tidb.initializer.createPassword, TiDB Operator sets the
	// root password from Secret <cluster>-init when it exists (and generates one only when it
	// does not), so that is the name the Orama operator creates it under.
	tidbInitSecretSuffix = "-init"
	tidbNormal           = "Normal"
	// tidbBootstrapSuffix names the ConfigMap <cluster>-bootstrap whose key tidbBootstrapKey TiDB
	// Operator mounts as TiDB's initialize-sql-file (spec.tidb.bootstrapSQLConfigMapName).
	tidbBootstrapSuffix = "-bootstrap"
	tidbBootstrapKey    = "bootstrap-sql"
	// Labels TiDB Operator puts on every PD and TiKV claim.
	tidbInstanceLabel  = "app.kubernetes.io/instance"
	tidbManagedByLabel = "app.kubernetes.io/managed-by"
	tidbManagedBy      = "tidb-operator"
)

// tidbComponents are the components the service runs, each with instances replicas.
var tidbComponents = []string{"pd", "tikv", "tidb"}

type tidbServiceKind struct{}

func (tidbServiceKind) title() string { return "TiDB" }

func (tidbServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{{gvk: tidbClusterGVK, name: in.name}, {gvk: configMapGVK, name: in.name + tidbBootstrapSuffix}}
}

// tidbNativePasswordHash is the mysql_native_password hash of password: "*" and the upper-case hex
// of SHA-1(SHA-1(password)), the form TiDB stores and accepts in IDENTIFIED WITH ... AS.
func tidbNativePasswordHash(password string) string {
	stage1 := sha1.Sum([]byte(password)) //nolint:gosec // the plugin's own definition.
	stage2 := sha1.Sum(stage1[:])        //nolint:gosec // the plugin's own definition.
	return "*" + strings.ToUpper(hex.EncodeToString(stage2[:]))
}

// tidbBootstrapSQL sets root's password while TiDB bootstraps its first start, before it accepts a
// connection (operator#774), and creates the tenant user with its database (operator#771). TiDB
// runs initialize-sql-file once, inside bootstrap, so there is no window in which root has an empty
// password and a new cluster has its tenant user from the first start. The ConfigMap holds only the
// passwords' hashes, never a password. TiDB Operator's initializer (createPassword) then finds root
// already protected ("Access denied" with an empty password) and records passwordInitialized.
// A cluster made before operator#771 gets its tenant user from the operator over SQL instead
// (tenantUsers.ensureTiDB).
func tidbBootstrapSQL(rootPassword, tenantPassword string) string {
	statements := append([]string{
		fmt.Sprintf("ALTER USER 'root'@'%%' IDENTIFIED WITH 'mysql_native_password' AS '%s'", tidbNativePasswordHash(rootPassword)),
	}, tidbTenantCreateStatements(tenantPassword, []string{tidbTenantDatabase})...)
	return strings.Join(statements, ";\n") + ";\n"
}

func (k tidbServiceKind) build(in catalogService, password string) []catalogResource {
	replicas := int64(in.spec.Instances)
	storage := map[string]interface{}{"storage": serviceStorage(in.spec)}
	fields := []serviceField{
		// The `restricted` Pod Security Standard of a workspace namespace (operator#435): every
		// pod of the cluster runs as a non-root user with the default seccomp profile, and each
		// component's container drops its capabilities.
		{path: []string{"spec", "podSecurityContext"}, value: map[string]interface{}{
			"runAsNonRoot": true, "runAsUser": int64(1000), "runAsGroup": int64(1000), "fsGroup": int64(1000),
			"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
		}},
		{path: []string{"spec", "discovery", "securityContext"}, value: restrictedContainerSecurityContext()},
		{path: []string{"spec", "discovery", "requests"}, value: map[string]interface{}{"cpu": "20m", "memory": "64Mi"}},
		{path: []string{"spec", "discovery", "limits"}, value: map[string]interface{}{"cpu": "100m", "memory": "128Mi"}},
		{path: []string{"spec", "pd", "limits"}, value: map[string]interface{}{"cpu": "250m", "memory": "512Mi"}},
		// TiKV was OOM-killed two seconds after "ready to serve" with a 1Gi limit and its default
		// block cache (operator#435). With 2Gi and the block cache capped below, it served an empty
		// cluster and was OOM-killed within seconds of every start once TiDB began writing its
		// schema into it, in an otherwise empty workspace. It gets 4Gi.
		{path: []string{"spec", "tikv", "limits"}, value: map[string]interface{}{"cpu": "500m", "memory": "4Gi"}},
		{path: []string{"spec", "tidb", "limits"}, value: map[string]interface{}{"cpu": "250m", "memory": "512Mi"}},
		{path: []string{"spec", "tidb", "requests"}, value: map[string]interface{}{"cpu": "100m", "memory": "256Mi"}},
		// TiDB's pod has a second container, slowlog, that tails the slow-query log. Without a
		// security context of its own the API server refuses the whole pod (operator#435).
		{path: []string{"spec", "tidb", "slowLogTailer"}, value: map[string]interface{}{
			"securityContext": restrictedContainerSecurityContext(),
			"requests":        map[string]interface{}{"cpu": "10m", "memory": "32Mi"},
			"limits":          map[string]interface{}{"cpu": "50m", "memory": "64Mi"},
		}},
		{path: []string{"spec", "version"}, value: tidbVersion},
		// The PVs of deleted claims go too; Retain (the default) would keep every disk.
		{path: []string{"spec", "pvReclaimPolicy"}, value: "Delete"},
		// PD places at most as many replicas of a region as there are TiKV stores.
		{path: []string{"spec", "pd", "config"}, value: map[string]interface{}{
			"replication": map[string]interface{}{"max-replicas": replicas},
		}},
		// TiKV reserves 5GB of its volume by default, more than a small service's whole disk; a
		// tenth of the volume keeps room to recover from a full disk.
		{path: []string{"spec", "tikv", "config"}, value: map[string]interface{}{
			"storage": map[string]interface{}{
				"reserve-space": fmt.Sprintf("%dMB", int64(in.spec.StorageGi)*1024/10),
				"block-cache":   map[string]interface{}{"capacity": "512MB"},
			},
		}},
		{path: []string{"spec", "tidb", "config"}, value: map[string]interface{}{}},
		// The TiDB Service <name>-tidb exists only when spec.tidb.service is set, and the
		// password initializer waits for its endpoints.
		{path: []string{"spec", "tidb", "service"}, value: map[string]interface{}{"type": "ClusterIP"}},
		{path: []string{"spec", "tidb", "initializer"}, value: map[string]interface{}{"createPassword": true}},
		// TiDB reads the bootstrap SQL only on its first start, and TiDB Operator asks for the
		// field to be set only when the cluster is created (operator#774).
		{path: []string{"spec", "tidb", "bootstrapSQLConfigMapName"}, createOnly: true, value: in.name + tidbBootstrapSuffix},
		// Storage cannot be changed, so the requests are written once; they carry the sizes too.
		{path: []string{"spec", "pd", "requests"}, createOnly: true, value: map[string]interface{}{"storage": storage["storage"], "cpu": "50m", "memory": "256Mi"}},
		{path: []string{"spec", "tikv", "requests"}, createOnly: true, value: map[string]interface{}{"storage": storage["storage"], "cpu": "100m", "memory": "512Mi"}},
	}
	for _, component := range tidbComponents {
		fields = append(fields,
			serviceField{path: []string{"spec", component, "replicas"}, value: replicas},
			// No failover pods: the service runs exactly instances of each component.
			serviceField{path: []string{"spec", component, "maxFailoverCount"}, value: int64(0)},
			serviceField{path: []string{"spec", component, "securityContext"}, value: restrictedContainerSecurityContext()},
		)
	}
	objects := k.objects(in)
	return []catalogResource{
		{catalogObject: objects[0], fields: fields},
		{catalogObject: objects[1], fields: []serviceField{
			{path: []string{"data"}, value: map[string]interface{}{tidbBootstrapKey: tidbBootstrapSQL(password, in.tenantPassword)}},
		}},
	}
}

func (tidbServiceKind) passwordSecret(name string) (string, string) {
	return name + tidbInitSecretSuffix, tidbRootKey
}

func (tidbServiceKind) tenantSecret(name string) (string, string) {
	return name + serviceTenantSecretSuffix, tidbTenantKey
}

func (tidbServiceKind) adminUsers(*LakeService) []string { return []string{tidbRootUser} }

// The administrator rotation (operator#1172): root's password is the generated one in
// <cluster>-init, key root.
func (tidbServiceKind) adminSecret(in catalogService) (string, string, string) {
	return in.name + tidbInitSecretSuffix, tidbRootKey, ""
}

func (tidbServiceKind) adminUser() string { return tidbRootUser }

func (tidbServiceKind) rotateAdmin(ctx context.Context, in catalogService, _, current, next string) error {
	return tenantUsers.rotateTiDBRoot(ctx, in, current, next)
}

func (tidbServiceKind) otherUsers(ctx context.Context, in catalogService, _, password string) ([]string, error) {
	return tenantUsers.otherTiDBUsers(ctx, in, password)
}

func tidbHost(in catalogService) string {
	return fmt.Sprintf("%s-tidb.%s.svc.cluster.local", in.name, in.namespace)
}

func (tidbServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("%s:%d", tidbHost(in), tidbPort)
}

// ready: PD, TiKV and TiDB each report phase Normal with every replica ready, and the root password
// from the Secret has been applied.
func (tidbServiceKind) ready(in catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	cluster := found[tidbClusterGVK.Kind]
	if cluster == nil {
		return false, "waiting for the TiDB cluster"
	}
	for _, component := range tidbComponents {
		phase, _, _ := unstructured.NestedString(cluster.Object, "status", component, "phase")
		ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", component, "statefulSet", "readyReplicas")
		if phase != tidbNormal || ready != int64(in.spec.Instances) {
			return false, fmt.Sprintf("waiting for TiDB %s", component)
		}
	}
	if initialized, _, _ := unstructured.NestedBool(cluster.Object, "status", "tidb", "passwordInitialized"); !initialized {
		return false, "waiting for the TiDB root password"
	}
	return true, ""
}

// credential delivers the tenant user, never root (operator#771). The operator first makes sure, as
// root, that the user exists with only its grants (tenantUsers.ensureTiDB); until it has, nothing is
// delivered and the pass is retried.
func (tidbServiceKind) credential(ctx context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, password string) ([]byte, string, error) {
	if password == "" || in.tenantPassword == "" {
		return nil, "", nil
	}
	if !ensureTenantUserOnce(ctx, in, func(ctx context.Context) error {
		return tenantUsers.ensureTiDB(ctx, in, password, in.tenantPassword)
	}) {
		return nil, "", nil
	}
	address := fmt.Sprintf("%s:%d", tidbHost(in), tidbPort)
	uri := (&url.URL{Scheme: "mysql", User: url.UserPassword(tidbTenantUser, in.tenantPassword), Host: address, Path: "/" + tidbTenantDatabase}).String()
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Database string `json:"database"`
		User     string `json:"user"`
		Password string `json:"password"`
		URI      string `json:"uri"`
	}{serviceKindTiDB, address, tidbHost(in), tidbPort, tidbTenantDatabase, tidbTenantUser, in.tenantPassword, uri})
	return credential, "", err
}

// podSelector: the TiDB servers only; clients never reach PD or TiKV.
func (tidbServiceKind) podSelector(in catalogService) map[string]string {
	return map[string]string{tidbInstanceLabel: in.name, tidbManagedByLabel: tidbManagedBy, "app.kubernetes.io/component": "tidb"}
}

// claimSelectors: TiDB Operator labels PD and TiKV claims with the cluster's instance and keeps
// them after the TidbCluster is deleted.
func (tidbServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{tidbInstanceLabel: in.name, tidbManagedByLabel: tidbManagedBy}}
}

// nameOverhead: the StatefulSet <name>-tikv plus its revision-hash suffix.
func (tidbServiceKind) nameOverhead() int { return len("-tikv") + statefulSetRevisionSuffix }
