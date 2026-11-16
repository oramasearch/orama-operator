// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func (cassandraServiceKind) tenantSecret(name string) (string, string) {
	return name + serviceTenantSecretSuffix, cassandraTenantKey
}

// adminUsers: the generated superuser, and Cassandra's stock one.
func (cassandraServiceKind) adminUsers(svc *LakeService) []string {
	return []string{cassandraSuperuserSecret(svc.Slug), "cassandra"}
}

// The administrator rotation (operator#1172): the superuser's password is in the Secret K8ssandra
// generates. K8ssandra makes that Secret only when it is missing; the operator writes the new
// password into it after changing the role, so cass-operator and the Secret agree.
func (cassandraServiceKind) adminSecret(in catalogService) (string, string, string) {
	return cassandraSuperuserSecret(cassandraSlugName(in)), "password", "username"
}

func (cassandraServiceKind) adminUser() string { return "" }

func (cassandraServiceKind) rotateAdmin(ctx context.Context, in catalogService, user, current, next string) error {
	return tenantUsers.rotateCassandraSuperuser(ctx, in, user, current, next)
}

func (cassandraServiceKind) otherUsers(ctx context.Context, in catalogService, user, password string) ([]string, error) {
	return tenantUsers.otherCassandraRoles(ctx, in, user, password)
}

// cassandraSuperuserSecret is the superuser Secret K8ssandra generates, <cluster name>-superuser;
// the role it holds has the same name.
func cassandraSuperuserSecret(slug string) string { return slug + "-superuser" }

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

// credential delivers the tenant role, never a superuser (operator#771). The operator reads the
// superuser Secret K8ssandra generates, <cluster name>-superuser, keys username and password, and
// signs in with it to keep the tenant role (tenantUsers.ensureCassandra); until that has succeeded,
// nothing is delivered and the pass is retried. K8ssandra makes the Secret controlled by the
// K8ssandraCluster; one it does not control was not generated for this service.
func (cassandraServiceKind) credential(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured, _ string) ([]byte, string, error) {
	cluster := found[k8ssandraClusterGVK.Kind]
	if cluster == nil || in.tenantPassword == "" {
		return nil, "", nil
	}
	secretName := cassandraSuperuserSecret(cassandraSlugName(in))
	secret, err := readServiceSecret(ctx, c, in.namespace, secretName)
	if err != nil || secret == nil {
		return nil, "", err
	}
	if !metav1.IsControlledBy(secret, cluster) {
		return nil, foreignServiceMessage("Secret", in.namespace, secretName), nil
	}
	superuser, superPassword := string(secret.Data["username"]), string(secret.Data["password"])
	if superuser == "" || superPassword == "" {
		return nil, "", nil
	}
	if superuser == cassandraTenantRole {
		return nil, fmt.Sprintf("Secret %s/%s names the superuser %s, the tenant role's name", in.namespace, secretName, cassandraTenantRole), nil
	}
	if !ensureTenantUserOnce(ctx, in, func(ctx context.Context) error {
		return tenantUsers.ensureCassandra(ctx, in, superuser, superPassword, in.tenantPassword)
	}) {
		return nil, "", nil
	}
	credential, err := json.Marshal(struct {
		Kind       string `json:"kind"`
		Endpoint   string `json:"endpoint"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Datacenter string `json:"datacenter"`
		Keyspace   string `json:"keyspace"`
		User       string `json:"user"`
		Password   string `json:"password"`
	}{serviceKindCassandra, cassandraServiceKind{}.endpoint(in), cassandraHost(in), cassandraPort, cassandraDatacenter,
		cassandraTenantKeyspace, cassandraTenantRole, in.tenantPassword})
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
