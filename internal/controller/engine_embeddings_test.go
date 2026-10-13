/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for #305: OramaCore embeds in-process, so the operator runs no <cr>-embeddings ai-server,
// keeps the downloaded models on the data PVC, and gives a GPU writer the GPU itself.

// embeddingsObjects are a <cr>-embeddings Deployment and Service as operators before #305 left them,
// controlled by oc when owned is true.
func embeddingsObjects(oc *oramav1alpha1.OramaCloudV1, owned bool) []client.Object {
	meta := func() metav1.ObjectMeta {
		m := metav1.ObjectMeta{Name: "v1-embeddings", Namespace: "orama-system", Labels: labelsFor(oc, "embeddings")}
		if owned {
			m.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))}
		}
		return m
	}
	return []client.Object{
		&appsv1.Deployment{ObjectMeta: meta()},
		&corev1.Service{ObjectMeta: meta(), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 50051}}}},
	}
}

func TestEngineRunsNoEmbeddingsServer(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	if !absent(t, cl, "v1-embeddings", &appsv1.Deployment{}) || !absent(t, cl, "v1-embeddings", &corev1.Service{}) {
		t.Error("an engine-enabled CR must not create the v1-embeddings Deployment or Service")
	}
}

func TestEngineRetiresOwnedEmbeddingsServer(t *testing.T) {
	oc := fullStackOCV1(nil)
	cl, _ := reconcileOCV1(t, oc, embeddingsObjects(oc, true)...)
	if !absent(t, cl, "v1-embeddings", &appsv1.Deployment{}) || !absent(t, cl, "v1-embeddings", &corev1.Service{}) {
		t.Error("the CR-owned v1-embeddings Deployment and Service must be deleted")
	}
}

func TestEngineLeavesForeignEmbeddingsServer(t *testing.T) {
	oc := fullStackOCV1(nil)
	cl, _ := reconcileOCV1(t, oc, embeddingsObjects(oc, false)...)
	if absent(t, cl, "v1-embeddings", &appsv1.Deployment{}) || absent(t, cl, "v1-embeddings", &corev1.Service{}) {
		t.Error("a v1-embeddings Deployment or Service the CR does not own must be left alone")
	}
}

// embeddingsConfigOf returns ai_server and ai_server.embeddings of a rendered OramaCore config.
func embeddingsConfigOf(t *testing.T, name, cfg string) (ai, emb map[string]any) {
	t.Helper()
	ai = parseEngineConfig(t, name, cfg)["ai_server"].(map[string]any)
	return ai, ai["embeddings"].(map[string]any)
}

func TestEngineConfigKeepsModelsOnDataVolume(t *testing.T) {
	oc := fullStackOCV1(nil)
	configs := map[string]string{
		"combined": oramacoreConfigYAML(oc),
		"writer":   oramacoreWriterConfigYAML(oc),
		"reader":   oramacoreReaderConfigYAML(oc),
	}
	for name, cfg := range configs {
		ai, emb := embeddingsConfigOf(t, name, cfg)
		if ai["models_cache_dir"] != "/app/.data/models" {
			t.Errorf("%s models_cache_dir = %v, want /app/.data/models on the data PVC", name, ai["models_cache_dir"])
		}
		for _, key := range []string{"host", "port", "scheme", "api_key", "max_connections"} {
			if _, ok := ai[key]; ok {
				t.Errorf("%s ai_server.%s is rendered; OramaCore reaches no embeddings server", name, key)
			}
		}
		if emb["default_model_group"] != "small" || emb["dynamically_load_models"] != true {
			t.Errorf("%s embeddings = %v, want default_model_group small and dynamically_load_models true", name, emb)
		}
	}
	// The data PVC is mounted at /app/.data, so the cache survives a restart.
	sts := engineStatefulSets(t, oc)
	for side, s := range sts {
		c := s.Spec.Template.Spec.Containers[0]
		if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "data" && m.MountPath == "/app/.data" }) {
			t.Errorf("%s does not mount the data PVC at /app/.data", side)
		}
	}
}

func TestGPUWriterGetsTheGPUAndBothProviders(t *testing.T) {
	cuda := []any{"CUDAExecutionProvider", "CPUExecutionProvider"}
	cpu := []any{"CPUExecutionProvider"}
	for _, gpu := range []bool{true, false} {
		custom := &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}}
		oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.GPU = gpu
			o.Spec.Components.Engine.Writer.Resources = custom
		})
		sts := engineStatefulSets(t, oc)
		limit, ok := sts["writer"].Spec.Template.Spec.Containers[0].Resources.Limits[resourceNvidiaGPU]
		if gpu && (!ok || limit.Value() != 1) {
			t.Errorf("gpu writer limits nvidia.com/gpu = %v, want 1", limit.String())
		}
		if !gpu && ok {
			t.Error("a CPU writer must not request nvidia.com/gpu")
		}
		if _, ok := custom.Limits[resourceNvidiaGPU]; ok {
			t.Error("the GPU limit was written into the CR's writer.resources")
		}
		if _, ok := sts["reader"].Spec.Template.Spec.Containers[0].Resources.Limits[resourceNvidiaGPU]; ok {
			t.Error("readers must not request nvidia.com/gpu")
		}

		want := cpu
		if gpu {
			want = cuda
		}
		for name, cfg := range map[string]string{"combined": oramacoreConfigYAML(oc), "writer": oramacoreWriterConfigYAML(oc)} {
			if _, emb := embeddingsConfigOf(t, name, cfg); !slices.Equal(emb["execution_providers"].([]any), want) {
				t.Errorf("gpu=%v %s execution_providers = %v, want %v", gpu, name, emb["execution_providers"], want)
			}
		}
		if _, emb := embeddingsConfigOf(t, "reader", oramacoreReaderConfigYAML(oc)); !slices.Equal(emb["execution_providers"].([]any), cpu) {
			t.Errorf("gpu=%v reader-only execution_providers = %v, want CPU only", gpu, emb["execution_providers"])
		}
	}
}

func TestNoEmbeddingsURLsPointAtDeadPorts(t *testing.T) {
	cl, _ := reconcileOCV1(t, fullStackOCV1(nil))
	if v, ok := getObj(t, cl, "v1-config", &corev1.ConfigMap{}).Data["EMBEDDINGS_URL"]; ok {
		t.Errorf("shared ConfigMap EMBEDDINGS_URL = %q, want absent", v)
	}
	disco := getObj(t, cl, "v1-disco", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0]
	if slices.ContainsFunc(disco.Env, func(e corev1.EnvVar) bool { return e.Name == "EMBEDDINGS_ENDPOINT" }) {
		t.Error("DISCO must not be pointed at the removed embeddings ai-server")
	}

	embeddingsURL := func(cl client.Client) (string, bool) {
		c := getObj(t, cl, "v1-index-builder", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0]
		i := slices.IndexFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "ORAMA_EMBEDDINGS_URL" })
		if i < 0 {
			return "", false
		}
		return c.Env[i].Value, true
	}
	// index-builder's EnvSchema requires the variable at boot, so unset renders it empty.
	if v, ok := embeddingsURL(cl); !ok || v != "" {
		t.Errorf("index-builder ORAMA_EMBEDDINGS_URL = %q (present %v), want present and empty when ingestion.embeddingsURL is unset", v, ok)
	}
	const hosted = "https://embeddings.example.com/v1/embed"
	cl, _ = reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Ingestion.EmbeddingsURL = hosted
	}))
	if v, _ := embeddingsURL(cl); v != hosted {
		t.Errorf("index-builder ORAMA_EMBEDDINGS_URL = %q, want %q", v, hosted)
	}
}
