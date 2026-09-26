/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SQLite services (liquid-bridge decision 47, amended; the contract is lake's
// docs/control-plane/services.md, the sqlite row). SQLite has no upstream operator, so the Orama
// operator runs lake-sqlite (github.com/oramasearch/lake-sqlite, docs/lake/lake-sqlite.md) itself: a StatefulSet of
// one pod over one database on a storage_gi volume, a ClusterIP Service on 8080, and a Secret
// <name>-credentials holding a generated 32-byte bearer token. They go through the catalog
// machinery like any upstream kind: the same ownership, labels, quota, refusal of foreign objects
// and deletion order. SQLite has one writer, so instances is always 1 (validateService).
var (
	statefulSetGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	serviceGVK     = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
)

// DefaultLakeSQLiteImage is the lake-sqlite image when neither the Lake nor the operator's flag
// names one. The chart passes its own default (lakeServices.sqliteImage) as the flag.
const DefaultLakeSQLiteImage = "oramasearch/lake-sqlite:0.1.1"

const (
	sqlitePort         = 8080
	sqlitePortName     = "http"
	sqliteContainer    = "lake-sqlite"
	sqliteDataVolume   = "data"
	sqliteDataDir      = "/data"
	sqliteDatabase     = "main"
	sqliteTokenKey     = "token"
	sqliteTokenBytes   = 32
	sqliteHealthPath   = "/healthz"
	sqliteTempVolume   = "tmp"
	sqliteTempDir      = "/tmp"
	sqliteRunAsUser    = int64(65532) // distroless nonroot
	sqliteGracePeriod  = int64(45)    // lake-sqlite drains for up to 30 s after SIGTERM
	sqliteNameLabel    = "app.kubernetes.io/name"
	sqliteNameValue    = "lake-sqlite"
	sqliteCPURequest   = "50m"
	sqliteMemRequest   = "64Mi"
	sqliteMemLimit     = "512Mi"
	sqliteTempSizeGi   = "1Gi"
	sqliteRevisionHash = statefulSetRevisionSuffix
)

type sqliteServiceKind struct{}

func (sqliteServiceKind) title() string { return "SQLite" }

func (sqliteServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{{gvk: statefulSetGVK, name: in.name}, {gvk: serviceGVK, name: in.name}}
}

// sqliteSelector selects the service's pod, and — the StatefulSet controller copies its selector
// onto every claim it creates — its data PVC. The service id makes it one service's alone.
func sqliteSelector(in catalogService) map[string]string {
	return map[string]string{
		sqliteNameLabel:              sqliteNameValue,
		"app.kubernetes.io/instance": in.name,
		serviceIDLabel:               in.svc.ID,
	}
}

func stringMap(labels map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

// sqlitePodTemplate is lake-sqlite's pod: non-root with a read-only root filesystem, the token from
// the Secret, the memory limit handed to the binary (which splits it between the Go heap and
// SQLite's page cache), and /healthz — which opens the database and runs SELECT 1 — behind every
// probe.
func sqlitePodTemplate(in catalogService, secretName string) corev1.PodTemplateSpec {
	var pullSecrets []corev1.LocalObjectReference
	if in.lake.Spec.PullSecretName != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: in.lake.Spec.PullSecretName}}
	}
	noToken := false
	noLinks := false
	nonRoot := true
	noEscalation := false
	readOnlyRoot := true
	user := sqliteRunAsUser
	grace := sqliteGracePeriod
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch
	tempSize := resource.MustParse(sqliteTempSizeGi)
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: sqliteHealthPath, Port: intstr.FromString(sqlitePortName), Scheme: corev1.URISchemeHTTP}}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: sqliteSelector(in)},
		Spec: corev1.PodSpec{
			ImagePullSecrets:              pullSecrets,
			AutomountServiceAccountToken:  &noToken,
			EnableServiceLinks:            &noLinks,
			TerminationGracePeriodSeconds: &grace,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        &nonRoot,
				RunAsUser:           &user,
				RunAsGroup:          &user,
				FSGroup:             &user,
				FSGroupChangePolicy: &onRootMismatch,
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            sqliteContainer,
				Image:           in.image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{Name: sqlitePortName, ContainerPort: sqlitePort, Protocol: corev1.ProtocolTCP}},
				Env: []corev1.EnvVar{
					{Name: "LAKE_SQLITE_PATH", Value: sqliteDataDir + "/" + sqliteDatabase + ".db"},
					{Name: "LAKE_SQLITE_ADDR", Value: fmt.Sprintf(":%d", sqlitePort)},
					{Name: "LAKE_SQLITE_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: sqliteTokenKey}}},
					{Name: "LAKE_SQLITE_MEMORY_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
						ContainerName: sqliteContainer, Resource: "limits.memory"}}},
					// SQLite spills large sorts and temporary tables here; the root filesystem is
					// read-only.
					{Name: "SQLITE_TMPDIR", Value: sqliteTempDir},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(sqliteCPURequest),
						corev1.ResourceMemory: resource.MustParse(sqliteMemRequest),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(sqliteMemLimit)},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: sqliteDataVolume, MountPath: sqliteDataDir},
					{Name: sqliteTempVolume, MountPath: sqliteTempDir},
				},
				StartupProbe: &corev1.Probe{ProbeHandler: health,
					PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 60},
				ReadinessProbe: &corev1.Probe{ProbeHandler: health,
					PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3},
				LivenessProbe: &corev1.Probe{ProbeHandler: health,
					PeriodSeconds: 20, TimeoutSeconds: 3, FailureThreshold: 3},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: []corev1.Volume{{Name: sqliteTempVolume, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tempSize}}}},
		},
	}
}

// toJSONValue turns a typed object into the JSON-shaped value an unstructured field holds.
func toJSONValue(obj interface{}) (map[string]interface{}, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

func (k sqliteServiceKind) build(in catalogService, _ string) []catalogResource {
	secretName, _ := k.passwordSecret(in.name)
	podTemplate := sqlitePodTemplate(in, secretName)
	template, err := toJSONValue(&podTemplate)
	if err != nil {
		// Converting a well-formed typed object cannot fail.
		panic(fmt.Sprintf("convert the lake-sqlite pod template: %v", err))
	}
	selector := sqliteSelector(in)
	claim := map[string]interface{}{
		"metadata": map[string]interface{}{"name": sqliteDataVolume, "labels": stringMap(selector)},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{string(corev1.ReadWriteOnce)},
			"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": serviceStorage(in.spec)}},
		},
	}
	objects := k.objects(in)
	return []catalogResource{
		{catalogObject: objects[0], fields: []serviceField{
			{path: []string{"spec", "replicas"}, value: int64(1)},
			// Selector, governing Service and claim templates are immutable once created.
			{path: []string{"spec", "selector"}, createOnly: true, value: map[string]interface{}{"matchLabels": stringMap(selector)}},
			{path: []string{"spec", "serviceName"}, createOnly: true, value: in.name},
			{path: []string{"spec", "volumeClaimTemplates"}, createOnly: true, value: []interface{}{claim}},
			{path: []string{"spec", "template"}, onChange: true, value: template},
		}},
		{catalogObject: objects[1], fields: []serviceField{
			{path: []string{"spec", "type"}, value: string(corev1.ServiceTypeClusterIP)},
			{path: []string{"spec", "selector"}, value: stringMap(selector)},
			// Every field the API server defaults on a port is written, so it reads back equal.
			{path: []string{"spec", "ports"}, value: []interface{}{map[string]interface{}{
				"name": sqlitePortName, "port": int64(sqlitePort), "targetPort": sqlitePortName, "protocol": string(corev1.ProtocolTCP),
			}}},
		}},
	}
}

func (sqliteServiceKind) passwordSecret(name string) (string, string) {
	return name + "-credentials", sqliteTokenKey
}

// passwordBytes: the bearer token is 32 random bytes, 64 hex characters.
func (sqliteServiceKind) passwordBytes() int { return sqliteTokenBytes }

func (sqliteServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", in.name, in.namespace, sqlitePort)
}

// ready: the StatefulSet's one pod is Ready, which its readiness probe on /healthz grants only once
// the database opens and answers SELECT 1.
func (sqliteServiceKind) ready(_ catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	statefulSet := found[statefulSetGVK.Kind]
	if statefulSet == nil {
		return false, "waiting for the lake-sqlite pod"
	}
	ready, _, _ := unstructured.NestedInt64(statefulSet.Object, "status", "readyReplicas")
	if ready != 1 {
		return false, "waiting for the lake-sqlite pod"
	}
	return true, ""
}

func (k sqliteServiceKind) credential(_ context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, token string) ([]byte, string, error) {
	if token == "" {
		return nil, "", nil
	}
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
		Database string `json:"database"`
	}{serviceKindSQLite, k.endpoint(in), token, sqliteDatabase})
	return credential, "", err
}

// claimSelectors: the data claim data-<name>-0 carries the StatefulSet's selector labels.
func (sqliteServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{sqliteSelector(in)}
}

// nameOverhead: the StatefulSet <name> and its revision-hash label value <name>-<hash>.
func (sqliteServiceKind) nameOverhead() int { return sqliteRevisionHash }
