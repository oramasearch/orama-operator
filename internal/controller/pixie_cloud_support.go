// SPDX-License-Identifier: MIT

package controller

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The parts of self-hosted Pixie Cloud the operator runs itself instead of taking upstream's
// cloud_deps: the Postgres (CloudNativePG, one database and role per service), the metadata search
// index (OpenSearch, Apache-2.0, in place of Elastic's), and the PxL script bundle server.

const (
	pixieDBCluster       = "pixie-db"
	pixieDBInstances     = 2
	pixieSearchHeap      = "1g"
	pixieSearchMemory    = "2Gi"
	pixieSearchNodeCerts = "pixie-search-node"
	// pixieSearchConfigDir is where OpenSearch reads its configuration; files are mounted into it
	// by subPath so the image's own files stay (principle 8).
	pixieSearchConfigDir   = "/usr/share/opensearch/config"
	pixieSearchSecurityDir = pixieSearchConfigDir + "/opensearch-security"
)

var cnpgDatabaseGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"}

// pixieDBHost is the read-write Service of the hub's Postgres.
func pixieDBHost() string {
	return pixieDBCluster + "-rw." + oramav1alpha1.PixieCloudNamespace + ".svc.cluster.local"
}

// pixieDBClusterObject is the hub's CloudNativePG cluster: two instances, one managed login role per
// service, never a superuser, each with a connection limit and its password from its own Secret.
func pixieDBClusterObject(cloud *oramav1alpha1.PixieCloud) *unstructured.Unstructured {
	storage := quantityOr(cloud.Spec.Storage.Postgres, "20Gi")
	var roles []interface{}
	for _, db := range pixieDatabases() {
		roles = append(roles, map[string]interface{}{
			"name":            db,
			"comment":         "Pixie Cloud " + db + " service (operator#15)",
			"ensure":          "present",
			"login":           true,
			"superuser":       false,
			"createdb":        false,
			"createrole":      false,
			"replication":     false,
			"bypassrls":       false,
			"inherit":         true,
			"connectionLimit": int64(30),
			"passwordSecret":  map[string]interface{}{"name": pixieDatabaseSecret(db)},
		})
	}
	// The bootstrap's own role (pixie_cloud_bootstrap.go): it may read and insert organisations and
	// users in the profile database, granted by their owner, and nothing else.
	roles = append(roles, map[string]interface{}{
		"name":            pixieBootstrapRole,
		"comment":         "Pixie Cloud organisation and administrator bootstrap (operator#15)",
		"ensure":          "present",
		"login":           true,
		"superuser":       false,
		"createdb":        false,
		"createrole":      false,
		"replication":     false,
		"bypassrls":       false,
		"inherit":         true,
		"connectionLimit": int64(2),
		"passwordSecret":  map[string]interface{}{"name": pixieDatabaseSecret(pixieBootstrapRole)},
	})
	storageSpec := map[string]interface{}{"size": storage.String()}
	if class := cloud.Spec.Storage.StorageClassName; class != "" {
		storageSpec["storageClass"] = class
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(pixieDBCluster)
	cluster.SetNamespace(oramav1alpha1.PixieCloudNamespace)
	cluster.Object["spec"] = map[string]interface{}{
		"instances": int64(pixieDBInstances),
		"imageName": string(cloud.Spec.Release.Images["postgres"]),
		"imagePullSecrets": []interface{}{
			map[string]interface{}{"name": cloud.Spec.PullSecretName},
		},
		"storage": storageSpec,
		// shared_buffers is a quarter of the memory limit, the binary's share of the cgroup cap.
		"postgresql": map[string]interface{}{"parameters": map[string]interface{}{
			"shared_buffers":       "256MB",
			"effective_cache_size": "512MB",
			"max_connections":      "300",
		}},
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{"cpu": "100m", "memory": "512Mi"},
			"limits":   map[string]interface{}{"cpu": "1", "memory": "1Gi"},
		},
		"bootstrap": map[string]interface{}{"initdb": map[string]interface{}{"database": "postgres_init", "owner": "postgres_init"}},
		"managed":   map[string]interface{}{"roles": roles},
		"inheritedMetadata": map[string]interface{}{"labels": map[string]interface{}{
			pixieManagedLabel:   pixieManagedCloud,
			pixieComponentLabel: pixieDBCluster,
		}},
	}
	return cluster
}

// pixieDatabaseObjects is one CloudNativePG Database per service, owned by that service's role.
func pixieDatabaseObjects() []*unstructured.Unstructured {
	var objects []*unstructured.Unstructured
	for _, db := range pixieDatabases() {
		database := &unstructured.Unstructured{}
		database.SetGroupVersionKind(cnpgDatabaseGVK)
		database.SetName(pixieDBCluster + "-" + db)
		database.SetNamespace(oramav1alpha1.PixieCloudNamespace)
		database.Object["spec"] = map[string]interface{}{
			"name":    db,
			"owner":   db,
			"ensure":  "present",
			"cluster": map[string]interface{}{"name": pixieDBCluster},
		}
		objects = append(objects, database)
	}
	return objects
}

// pixieSearchObjects is a single-node OpenSearch with its security plugin on: TLS on both layers
// from the hub's service CA, and two users, the indexer (writes the metadata index) and the API
// (reads it), each limited to the metadata indices.
func pixieSearchObjects(cloud *oramav1alpha1.PixieCloud, securityConfig map[string]string) []client.Object {
	labels := map[string]string{pixieManagedLabel: pixieManagedCloud, pixieComponentLabel: pixieSearchService}
	storage := quantityOr(cloud.Spec.Storage.Search, "20Gi")
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pixieSearchService + "-config"},
		Data: map[string]string{"opensearch.yml": strings.Join([]string{
			"cluster.name: pixie-search",
			"network.host: 0.0.0.0",
			"discovery.type: single-node",
			// Report Elasticsearch 7.10.2 to Pixie's olivere/elastic v7 client.
			"compatibility.override_main_response_version: true",
			"bootstrap.memory_lock: false",
			"plugins.security.ssl.transport.pemcert_filepath: certs/tls.crt",
			"plugins.security.ssl.transport.pemkey_filepath: certs/tls.key",
			"plugins.security.ssl.transport.pemtrustedcas_filepath: certs/ca.crt",
			"plugins.security.ssl.transport.enforce_hostname_verification: false",
			"plugins.security.ssl.http.enabled: true",
			"plugins.security.ssl.http.pemcert_filepath: certs/tls.crt",
			"plugins.security.ssl.http.pemkey_filepath: certs/tls.key",
			"plugins.security.ssl.http.pemtrustedcas_filepath: certs/ca.crt",
			"plugins.security.allow_default_init_securityindex: true",
			"plugins.security.nodes_dn: ['CN=pixie-search,O=Orama']",
			"plugins.security.authcz.admin_dn: ['CN=pixie-search-admin,O=Orama']",
			"plugins.security.restapi.roles_enabled: []",
			"",
		}, "\n")},
	}
	securityFiles := []string{"internal_users.yml", "roles.yml", "roles_mapping.yml", "config.yml"}
	security := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pixieSearchService + "-security"},
		StringData: securityConfig,
	}
	mounts := []corev1.VolumeMount{
		{Name: "data", MountPath: "/usr/share/opensearch/data"},
		{Name: "config", MountPath: pixieSearchConfigDir + "/opensearch.yml", SubPath: "opensearch.yml"},
		{Name: "certs", MountPath: pixieSearchConfigDir + "/certs", ReadOnly: true},
	}
	for _, file := range securityFiles {
		mounts = append(mounts, corev1.VolumeMount{Name: "security", MountPath: pixieSearchSecurityDir + "/" + file, SubPath: file, ReadOnly: true})
	}
	budget := pixieBudget{CPURequest: "200m", CPULimit: "2", MemoryRequest: pixieSearchMemory, MemoryLimit: pixieSearchMemory}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: pixieSearchService},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: pixieSearchService,
			Replicas:    int32Ptr(1),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(1000), FSGroup: int64Ptr(1000),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "opensearch",
						Image: string(cloud.Spec.Release.Images["opensearch"]),
						Env: []corev1.EnvVar{
							// The JVM heap is half the memory limit; the rest is Lucene's page cache.
							{Name: "OPENSEARCH_JAVA_OPTS", Value: "-Xms" + pixieSearchHeap + " -Xmx" + pixieSearchHeap},
							{Name: "DISABLE_INSTALL_DEMO_CONFIG", Value: "true"},
							{Name: "DISABLE_PERFORMANCE_ANALYZER_AGENT_CLI", Value: "true"},
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: pixieSearchPort},
							{Name: "transport", ContainerPort: 9300},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(pixieSearchPort)}},
							PeriodSeconds:    10,
							FailureThreshold: 6,
						},
						Resources:       budget.requirements(),
						VolumeMounts:    mounts,
						SecurityContext: hardenedSecurityContext(),
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: config.Name}}}},
						{Name: "security", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: security.Name}}},
						{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: pixieSearchNodeCerts}}},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: labels},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: storageClassOrNil(cloud.Spec.Storage.StorageClassName),
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
				},
			}},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: pixieSearchService},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: pixieSearchPort, TargetPort: intstr.FromInt(pixieSearchPort)}},
		},
	}
	return []client.Object{config, security, service, statefulSet}
}

// pixieScriptsObjects serves the PxL script bundle inside the cluster: to the UI through the proxy
// (/pxl_scripts/), and to the script manager as a storage emulator. The image (#455,
// the Pixie source's hack/pixie-script-bundle) serves /pixie-scripts/bundle.json on port 8080, as a non-root user.
func pixieScriptsObjects(cloud *oramav1alpha1.PixieCloud) []client.Object {
	labels := map[string]string{pixieManagedLabel: pixieManagedCloud, pixieComponentLabel: pixieScriptsService}
	budget := pixieBudget{CPURequest: "10m", CPULimit: "200m", MemoryRequest: "32Mi", MemoryLimit: "64Mi"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: pixieScriptsService},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "scripts",
						Image:           string(cloud.Spec.Release.Images["script-bundle"]),
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: pixieScriptsPort}},
						Resources:       budget.requirements(),
						SecurityContext: hardenedSecurityContext(),
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/" + pixieScriptsBucket + "/bundle.json", Port: intstr.FromInt(pixieScriptsPort),
						}}},
					}},
				},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: pixieScriptsService},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: pixieScriptsPort, TargetPort: intstr.FromInt(pixieScriptsPort)}},
		},
	}
	return []client.Object{deployment, service}
}

func storageClassOrNil(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// pixieSearchSecurityConfig is the OpenSearch security plugin's users, roles and their mapping:
// the indexer may write the metadata indices, the API may only read them, and nobody else may sign
// in. The hashes are bcrypt hashes of the generated passwords.
func pixieSearchSecurityConfig(indexerHash, apiHash string) map[string]string {
	return map[string]string{
		"internal_users.yml": fmt.Sprintf(`_meta:
  type: "internalusers"
  config_version: 2
pixie-indexer:
  hash: %q
  reserved: true
  description: "Pixie Cloud indexer: writes the metadata indices"
pixie-api:
  hash: %q
  reserved: true
  description: "Pixie Cloud API: reads the metadata indices for autocomplete"
`, indexerHash, apiHash),
		"roles.yml": `_meta:
  type: "roles"
  config_version: 2
pixie_indexer:
  reserved: true
  cluster_permissions: ["cluster_monitor", "cluster:admin/ingest/pipeline/get", "indices:data/write/bulk"]
  index_permissions:
  - index_patterns: ["md_entities*"]
    allowed_actions: ["indices_all"]
pixie_api:
  reserved: true
  cluster_permissions: ["cluster_monitor"]
  index_permissions:
  - index_patterns: ["md_entities*"]
    allowed_actions: ["read", "indices:admin/mappings/get"]
`,
		"roles_mapping.yml": `_meta:
  type: "rolesmapping"
  config_version: 2
pixie_indexer:
  reserved: true
  users: ["pixie-indexer"]
pixie_api:
  reserved: true
  users: ["pixie-api"]
`,
		"config.yml": `_meta:
  type: "config"
  config_version: 2
config:
  dynamic:
    http:
      anonymous_auth_enabled: false
    authc:
      basic_internal_auth_domain:
        http_enabled: true
        transport_enabled: true
        order: 0
        http_authenticator:
          type: basic
          challenge: true
        authentication_backend:
          type: intern
`,
	}
}
