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

// Wrapper services (liquid-bridge decision 47, amended; the contract is lake's
// docs/control-plane/services.md, the sqlite, duckdb and polars rows). These kinds have no upstream
// operator we would use, so the Orama operator runs a small web service of ours — each in its own
// private repository and image: lake-sqlite, lake-duckdb, lake-polars — itself: a StatefulSet (one
// volume per pod, which stays with its pod), a ClusterIP Service on 8080, and a Secret
// <name>-credentials holding a generated 32-byte bearer token. They go through the catalog
// machinery like any upstream kind: the same ownership, labels, quota, refusal of foreign objects
// and deletion order, and deletion removes the StatefulSet's PVCs too. What differs per kind —
// image, environment, resources, how many pods, the credential's database field — is a
// wrapperServiceKind value (lake_service_sqlite.go, lake_service_duckdb.go, lake_service_polars.go).
var (
	statefulSetGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	serviceGVK     = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
)

const (
	wrapperPort        = 8080
	wrapperPortName    = "http"
	wrapperDataVolume  = "data"
	wrapperDataDir     = "/data"
	wrapperTokenKey    = "token"
	wrapperTokenBytes  = 32
	wrapperHealthPath  = "/healthz"
	wrapperTempVolume  = "tmp"
	wrapperTempDir     = "/tmp"
	wrapperRunAsUser   = int64(65532) // distroless nonroot
	wrapperGracePeriod = int64(45)    // each wrapper drains for up to 30 s after SIGTERM
	wrapperNameLabel   = "app.kubernetes.io/name"
	wrapperCPURequest  = "50m"
	wrapperTempSize    = "1Gi"
)

// wrapperServiceKind is one kind the Orama operator runs itself behind a web service of ours.
type wrapperServiceKind struct {
	// kind is the contract's kind ("sqlite"), written into the credential.
	kind string
	// titleText names the kind in messages: "SQLite".
	titleText string
	// app is the web service's name: the container, the app.kubernetes.io/name label, the image
	// repository ("lake-sqlite").
	app string
	// envPrefix prefixes the token and memory-limit variables: LAKE_SQLITE_TOKEN,
	// LAKE_SQLITE_MEMORY_LIMIT.
	envPrefix string
	// leadEnv come before the token (the data path and listen address); trailEnv after the
	// memory limit. The order is part of the pod template, whose hash rolls the pod when it changes.
	leadEnv, trailEnv []corev1.EnvVar
	// memRequest and memLimit are the container's memory; the limit is handed to the binary.
	memRequest, memLimit string
	// multiInstance: the StatefulSet has spec.instances pods (polars); otherwise always one.
	multiInstance bool
	// database is the credential's "database" field; "" leaves it out.
	database string
}

func (k wrapperServiceKind) title() string { return k.titleText }

func (wrapperServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{{gvk: statefulSetGVK, name: in.name}, {gvk: serviceGVK, name: in.name}}
}

// replicas is how many pods the StatefulSet runs.
func (k wrapperServiceKind) replicas(spec lakeServiceSpec) int64 {
	if k.multiInstance && spec.Instances > 1 {
		return int64(spec.Instances)
	}
	return 1
}

// selector selects the service's pods, and — the StatefulSet controller copies its selector onto
// every claim it creates — their data PVCs. The service id makes it one service's alone.
func (k wrapperServiceKind) selector(in catalogService) map[string]string {
	return map[string]string{
		wrapperNameLabel:             k.app,
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

// podTemplate is the web service's pod: non-root with a read-only root filesystem, the token from
// the Secret, the memory limit handed to the binary (which divides it between its own heap and the
// engine), a writable /tmp, and /healthz — which answers only when the engine does — behind every
// probe.
func (k wrapperServiceKind) podTemplate(in catalogService, secretName string) corev1.PodTemplateSpec {
	var pullSecrets []corev1.LocalObjectReference
	if in.lake.Spec.PullSecretName != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: in.lake.Spec.PullSecretName}}
	}
	noToken := false
	noLinks := false
	nonRoot := true
	noEscalation := false
	readOnlyRoot := true
	user := wrapperRunAsUser
	grace := wrapperGracePeriod
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch
	tempSize := resource.MustParse(wrapperTempSize)
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: wrapperHealthPath, Port: intstr.FromString(wrapperPortName), Scheme: corev1.URISchemeHTTP}}
	env := append([]corev1.EnvVar{}, k.leadEnv...)
	env = append(env,
		corev1.EnvVar{Name: k.envPrefix + "_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: wrapperTokenKey}}},
		corev1.EnvVar{Name: k.envPrefix + "_MEMORY_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			ContainerName: k.app, Resource: "limits.memory"}}},
	)
	env = append(env, k.trailEnv...)
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: k.selector(in)},
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
				Name:            k.app,
				Image:           in.image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{Name: wrapperPortName, ContainerPort: wrapperPort, Protocol: corev1.ProtocolTCP}},
				Env:             env,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(wrapperCPURequest),
						corev1.ResourceMemory: resource.MustParse(k.memRequest),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(k.memLimit)},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: wrapperDataVolume, MountPath: wrapperDataDir},
					{Name: wrapperTempVolume, MountPath: wrapperTempDir},
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
			Volumes: []corev1.Volume{{Name: wrapperTempVolume, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tempSize}}}},
		},
	}
}

// toJSONValue turns a typed object into the JSON-shaped value an unstructured field holds.
func toJSONValue(obj interface{}) (map[string]interface{}, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

func (k wrapperServiceKind) build(in catalogService, _ string) []catalogResource {
	secretName, _ := k.passwordSecret(in.name)
	podTemplate := k.podTemplate(in, secretName)
	template, err := toJSONValue(&podTemplate)
	if err != nil {
		// Converting a well-formed typed object cannot fail.
		panic(fmt.Sprintf("convert the %s pod template: %v", k.app, err))
	}
	selector := k.selector(in)
	claim := map[string]interface{}{
		"metadata": map[string]interface{}{"name": wrapperDataVolume, "labels": stringMap(selector)},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{string(corev1.ReadWriteOnce)},
			"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": serviceStorage(in.spec)}},
		},
	}
	objects := k.objects(in)
	return []catalogResource{
		{catalogObject: objects[0], fields: []serviceField{
			{path: []string{"spec", "replicas"}, value: k.replicas(in.spec)},
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
				"name": wrapperPortName, "port": int64(wrapperPort), "targetPort": wrapperPortName, "protocol": string(corev1.ProtocolTCP),
			}}},
		}},
	}
}

func (wrapperServiceKind) passwordSecret(name string) (string, string) {
	return name + "-credentials", wrapperTokenKey
}

// passwordBytes: the bearer token is 32 random bytes, 64 hex characters.
func (wrapperServiceKind) passwordBytes() int { return wrapperTokenBytes }

func (wrapperServiceKind) endpoint(in catalogService) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", in.name, in.namespace, wrapperPort)
}

// waiting is the Provisioning message while pods are not all Ready.
func (k wrapperServiceKind) waiting() string {
	if k.multiInstance {
		return fmt.Sprintf("waiting for the %s pods", k.app)
	}
	return fmt.Sprintf("waiting for the %s pod", k.app)
}

// ready: every pod of the StatefulSet is Ready, which its readiness probe on /healthz grants only
// once the engine answers.
func (k wrapperServiceKind) ready(in catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	statefulSet := found[statefulSetGVK.Kind]
	if statefulSet == nil {
		return false, k.waiting()
	}
	ready, _, _ := unstructured.NestedInt64(statefulSet.Object, "status", "readyReplicas")
	if ready != k.replicas(in.spec) {
		return false, k.waiting()
	}
	return true, ""
}

func (k wrapperServiceKind) credential(_ context.Context, _ client.Reader, in catalogService, _ map[string]*unstructured.Unstructured, token string) ([]byte, string, error) {
	if token == "" {
		return nil, "", nil
	}
	credential, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
		Database string `json:"database,omitempty"`
	}{k.kind, k.endpoint(in), token, k.database})
	return credential, "", err
}

// claimSelectors: each data claim data-<name>-<i> carries the StatefulSet's selector labels.
func (k wrapperServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{k.selector(in)}
}

// nameOverhead: the StatefulSet <name> and its revision-hash label value <name>-<hash>.
func (wrapperServiceKind) nameOverhead() int { return statefulSetRevisionSuffix }
