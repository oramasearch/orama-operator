/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TiDB services run on TiDB Operator v1.6.6, the stable 1.x API pingcap.com/v1alpha1. Field names
// follow that release's manifests/crd.yaml and its controller source.
var tidbClusterGVK = schema.GroupVersionKind{Group: "pingcap.com", Version: "v1alpha1", Kind: "TidbCluster"}

const (
	// tidbVersion is the TiDB release the operator's own v1.6.6 examples run.
	tidbVersion = "v8.5.7"
	tidbPort    = 4000
	tidbUser    = "root"
	// tidbRootKey is the key TiDB Operator reads the root password from in <cluster>-init.
	tidbRootKey = "root"
	// tidbInitSecretSuffix: with spec.tidb.initializer.createPassword, TiDB Operator sets the
	// root password from Secret <cluster>-init when it exists (and generates one only when it
	// does not), so that is the name the Orama operator creates it under.
	tidbInitSecretSuffix = "-init"
	tidbNormal           = "Normal"
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
	return []catalogObject{{gvk: tidbClusterGVK, name: in.name}}
}

func (k tidbServiceKind) build(in catalogService, _ string) []catalogResource {
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
		{path: []string{"spec", "tikv", "limits"}, value: map[string]interface{}{"cpu": "500m", "memory": "1Gi"}},
		{path: []string{"spec", "tidb", "limits"}, value: map[string]interface{}{"cpu": "250m", "memory": "512Mi"}},
		{path: []string{"spec", "tidb", "requests"}, value: map[string]interface{}{"cpu": "100m", "memory": "256Mi"}},
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
			"storage": map[string]interface{}{"reserve-space": fmt.Sprintf("%dMB", int64(in.spec.StorageGi)*1024/10)},
		}},
		{path: []string{"spec", "tidb", "config"}, value: map[string]interface{}{}},
		// The TiDB Service <name>-tidb exists only when spec.tidb.service is set, and the
		// password initializer waits for its endpoints.
		{path: []string{"spec", "tidb", "service"}, value: map[string]interface{}{"type": "ClusterIP"}},
		{path: []string{"spec", "tidb", "initializer"}, value: map[string]interface{}{"createPassword": true}},
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
	return []catalogResource{{catalogObject: k.objects(in)[0], fields: fields}}
}

func (tidbServiceKind) passwordSecret(name string) (string, string) {
	return name + tidbInitSecretSuffix, tidbRootKey
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

func (tidbServiceKind) credential(_ context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, password string) ([]byte, string, error) {
	if password == "" {
		return nil, "", nil
	}
	address := fmt.Sprintf("%s:%d", tidbHost(in), tidbPort)
	uri := (&url.URL{Scheme: "mysql", User: url.UserPassword(tidbUser, password), Host: address}).String()
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		URI      string `json:"uri"`
	}{serviceKindTiDB, address, tidbHost(in), tidbPort, tidbUser, password, uri})
	return credential, "", err
}

// claimSelectors: TiDB Operator labels PD and TiKV claims with the cluster's instance and keeps
// them after the TidbCluster is deleted.
func (tidbServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{tidbInstanceLabel: in.name, tidbManagedByLabel: tidbManagedBy}}
}

// nameOverhead: the StatefulSet <name>-tikv plus its revision-hash suffix.
func (tidbServiceKind) nameOverhead() int { return len("-tikv") + statefulSetRevisionSuffix }
