/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testWriterImage = "oramasearch/oramacore-writer:1.2.38"
	testReaderImage = "oramasearch/oramacore-reader:1.2.38"
)

// sideImagesOC is a full stack whose writer and readers run side-only OramaCore images.
func sideImagesOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}
		if mut != nil {
			mut(o)
		}
	})
}

// renderInit returns the render-config init container of an engine StatefulSet.
func renderInit(t *testing.T, sts *appsv1.StatefulSet) corev1.Container {
	t.Helper()
	for _, c := range sts.Spec.Template.Spec.InitContainers {
		if c.Name == "render-config" {
			return c
		}
	}
	t.Fatalf("%s has no render-config init container", sts.Name)
	return corev1.Container{}
}

// parseEngineConfig parses an OramaCore config and returns its top-level sections.
func parseEngineConfig(t *testing.T, name, cfg string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &out); err != nil {
		t.Fatalf("%s is not YAML: %v\n%s", name, err, cfg)
	}
	return out
}

func engineSidesConditionOf(t *testing.T, cl client.Client) *metav1.Condition {
	t.Helper()
	oc := getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})
	return apimeta.FindStatusCondition(oc.Status.Conditions, engineSidesConditionType)
}

func TestEngineSideImagesRenderSideOnlyConfigs(t *testing.T) {
	cl, _ := reconcileOCV1(t, sideImagesOC(nil))
	cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
	if _, ok := cm.Data[combinedConfigKey]; ok {
		t.Errorf("side-only stack still renders the combined %s", combinedConfigKey)
	}

	for _, tc := range []struct {
		sts, image, key, want, absent string
	}{
		{"v1-writer", testWriterImage, writerConfigKey, "writer_side", "reader_side"},
		{"v1-reader", testReaderImage, readerConfigKey, "reader_side", "writer_side"},
	} {
		sts := getObj(t, cl, tc.sts, &appsv1.StatefulSet{})
		if got := sts.Spec.Template.Spec.Containers[0].Image; got != tc.image {
			t.Errorf("%s image = %q, want %q", tc.sts, got, tc.image)
		}
		if cmd := renderInit(t, sts).Command[2]; !strings.Contains(cmd, " /tmpl/"+tc.key+" > /etc/oramacore/config.yaml") {
			t.Errorf("%s renders %q, want template %s", tc.sts, cmd, tc.key)
		}
		cfg := parseEngineConfig(t, tc.key, cm.Data[tc.key])
		if _, ok := cfg[tc.want]; !ok {
			t.Errorf("%s has no %s", tc.key, tc.want)
		}
		if _, ok := cfg[tc.absent]; ok {
			t.Errorf("%s must not carry %s", tc.key, tc.absent)
		}
		for _, section := range []string{"http", "log", "ai_server"} {
			if _, ok := cfg[section]; !ok {
				t.Errorf("%s has no %s", tc.key, section)
			}
		}
	}
}

func TestEngineSidesSeparatedCondition(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	cm := getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{})
	if _, ok := cm.Data[combinedConfigKey]; !ok || len(cm.Data) != 2 {
		t.Fatalf("combined stack ConfigMap keys = %v, want envoy.yaml and %s", slices.Sorted(maps.Keys(cm.Data)), combinedConfigKey)
	}
	for _, name := range []string{"v1-writer", "v1-reader"} {
		sts := getObj(t, cl, name, &appsv1.StatefulSet{})
		if got := sts.Spec.Template.Spec.Containers[0].Image; got != "oramasearch/oramacore:latest" {
			t.Errorf("%s image = %q, want the shared oramacore image", name, got)
		}
		if cmd := renderInit(t, sts).Command[2]; !strings.Contains(cmd, " /tmpl/"+combinedConfigKey+" ") {
			t.Errorf("%s renders %q, want the combined config", name, cmd)
		}
	}
	cond := engineSidesConditionOf(t, cl)
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "every reader also runs a writer") {
		t.Fatalf("combined stack condition = %+v, want %s=False saying readers run a writer", cond, engineSidesConditionType)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}
	})
	if cond := engineSidesConditionOf(t, cl); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("side-only stack condition = %+v, want %s=True", cond, engineSidesConditionType)
	}

	one := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{"oramacore-reader": testReaderImage}
	})
	if err := r.reconcileEngine(context.Background(), one); err == nil {
		t.Error("a reader-only image without a writer-only image must be refused")
	}
}

func TestReaderConfigNamesStreamConnectionByPod(t *testing.T) {
	oc := sideImagesOC(nil)
	reader := engineSideFor(oc, "reader")
	for _, line := range []string{"client_provided_name: " + podNamePlaceholder, "consumer_name: " + podNamePlaceholder} {
		if !strings.Contains(reader.template, "    "+line+"\n") {
			t.Errorf("reader template lacks %q", line)
		}
	}
	writer := parseEngineConfig(t, writerConfigKey, engineSideFor(oc, "writer").template)
	output := writer["writer_side"].(map[string]any)["output"].(map[string]any)
	if output["client_provided_name"] != "v1-writer" || output["producer_name"] != "v1-writer" {
		t.Errorf("writer stream names = %v / %v, want v1-writer", output["client_provided_name"], output["producer_name"])
	}

	render := renderConfigInit(oc, reader)
	if len(render.Env) == 0 || render.Env[0].Name != "POD_NAME" || render.Env[0].ValueFrom == nil ||
		render.Env[0].ValueFrom.FieldRef == nil || render.Env[0].ValueFrom.FieldRef.FieldPath != "metadata.name" {
		t.Fatalf("render-config env = %+v, want POD_NAME from metadata.name", render.Env)
	}

	// Run the init container's real command against the template, as the pod v1-reader-2 would.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in-"+readerConfigKey), []byte(reader.template), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/tmpl/", dir+"/in-", "/etc/oramacore/", dir+"/out-").Replace(render.Command[2])
	cmd := exec.Command(render.Command[0], render.Command[1], script)
	cmd.Env = append(os.Environ(), "MASTER_API_KEY=k", "GROQ_API_KEY=g", "POD_NAME=v1-reader-2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render command: %v\n%s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "out-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseEngineConfig(t, "rendered reader config", string(rendered))
	input := cfg["reader_side"].(map[string]any)["input"].(map[string]any)
	if input["client_provided_name"] != "v1-reader-2" || input["consumer_name"] != "v1-reader-2" {
		t.Errorf("rendered reader stream names = %v / %v, want v1-reader-2", input["client_provided_name"], input["consumer_name"])
	}
}

func TestEngineConfigChangeRollsOnlyItsSide(t *testing.T) {
	cl, r := reconcileOCV1(t, sideImagesOC(nil))
	annotations := func(name string) map[string]string {
		return getObj(t, cl, name, &appsv1.StatefulSet{}).Spec.Template.Annotations
	}
	writer, reader := annotations("v1-writer"), annotations("v1-reader")
	if writer["orama.cloud/config-hash"] == "" || reader["orama.cloud/config-hash"] == "" {
		t.Fatalf("side-only pods need a config hash: writer %v, reader %v", writer, reader)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides["oramacore-reader"] = "oramasearch/oramacore-reader:1.2.39"
		o.Spec.Components.Engine.Reader.Replicas = 5
		o.Spec.Components.Engine.Reader.Resources = resourceCaps("8Gi", "4", "4Gi", "2")
	})
	if got := annotations("v1-writer"); !maps.Equal(got, writer) {
		t.Errorf("reader-only change touched the writer pod annotations: %v -> %v", writer, got)
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{JWKSURL: "https://app.example.com/jwks.json", Audience: "writer"}
	})
	if got := annotations("v1-reader"); !maps.Equal(got, reader) {
		t.Errorf("writer-only change touched the reader pod annotations: %v -> %v", reader, got)
	}
	if maps.Equal(annotations("v1-writer"), writer) {
		t.Error("writer JWT change did not roll the writer")
	}
}
