package imagepolicy

import (
	"reflect"
	"strings"
	"testing"
)

func TestPinned(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, image := range []string{"postgres@sha256:" + digest, "registry.example:5000/team/image:tag@sha256:" + digest} {
		if !Pinned(image) {
			t.Errorf("rejected pinned image %q", image)
		}
	}
	for _, image := range []string{"", "postgres", "postgres:18-alpine", "postgres@sha256:" + digest + "x", "postgres@sha256:" + digest + "\n", " postgres@sha256:" + digest, "postgres@sha256:" + strings.Repeat("A", 64), "postgres@sha512:" + strings.Repeat("a", 128), "bad name@sha256:" + digest, "https://registry/image@sha256:" + digest, "postgres@SHA256:" + digest, "postgres@sha256:" + strings.Repeat("a", 63)} {
		if Pinned(image) {
			t.Errorf("accepted mutable or invalid image %q", image)
		}
	}
}

func TestMutableImagesIncludesEveryExecutableContainer(t *testing.T) {
	pinned := "image@sha256:" + strings.Repeat("a", 64)
	object := map[string]interface{}{"kind": "CronJob", "spec": map[string]interface{}{
		"jobTemplate": map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
			"initContainers":      []interface{}{map[string]interface{}{"name": "init", "image": "init:latest"}},
			"containers":          []interface{}{map[string]interface{}{"name": "main", "image": pinned, "env": []interface{}{map[string]interface{}{"name": "WORKER_IMAGE", "value": "worker:tag"}}}, map[string]interface{}{"name": "sidecar"}},
			"ephemeralContainers": []interface{}{map[string]interface{}{"name": "debug", "image": "debug:tag"}},
		}}}},
	}}
	want := []string{"spec.jobTemplate.spec.template.spec.containers[0].env[0].value", "spec.jobTemplate.spec.template.spec.containers[1].image", "spec.jobTemplate.spec.template.spec.ephemeralContainers[0].image", "spec.jobTemplate.spec.template.spec.initContainers[0].image"}
	if got := MutableImages(object); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}
