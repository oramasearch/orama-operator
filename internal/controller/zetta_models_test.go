/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestAZettaWithoutModelsHasNoInitContainers(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	if got := sts.Spec.Template.Spec.InitContainers; len(got) != 0 {
		t.Fatalf("init containers = %+v, want none", got)
	}
}

// lake#246: each spec.models entry is an init container that runs the model image with the data
// volume mounted, and passes the directory and the image as arguments, never as script text.
func TestEachZettaModelIsCopiedOntoTheDataVolumeBeforeTheNodeStarts(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	z := newZettaTestCluster()
	z.Spec.PullSecretName = "dockerhub"
	z.Spec.Models = []oramav1alpha1.OramaZettaModel{
		{Name: "base-agent", Image: "oramasearch/base-agent-0.6b:v16"},
		{Name: "reranker", Image: "oramasearch/reranker@sha256:abc"},
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSet(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	pod := sts.Spec.Template.Spec
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "dockerhub" {
		t.Fatalf("pull secrets = %+v: a private model image could not be pulled", pod.ImagePullSecrets)
	}
	if len(pod.InitContainers) != 2 {
		t.Fatalf("init containers = %d, want 2", len(pod.InitContainers))
	}
	for i, want := range []struct{ name, image, dir string }{
		{"model-base-agent", "oramasearch/base-agent-0.6b:v16", "/data/models/base-agent"},
		{"model-reranker", "oramasearch/reranker@sha256:abc", "/data/models/reranker"},
	} {
		c := pod.InitContainers[i]
		if c.Name != want.name || c.Image != want.image || c.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("init container %d = %s %s %s", i, c.Name, c.Image, c.ImagePullPolicy)
		}
		if !slices.Equal(c.Command, []string{"/bin/sh", "-c", zettaModelCopyScript, "zetta-model"}) {
			t.Errorf("%s command = %q", c.Name, c.Command)
		}
		if !slices.Equal(c.Args, []string{want.dir, want.image}) {
			t.Errorf("%s args = %q, want [%s %s]", c.Name, c.Args, want.dir, want.image)
		}
		if strings.Contains(c.Command[2], want.image) || strings.Contains(c.Command[2], want.dir) {
			t.Errorf("%s formats a value into its script (operator#279)", c.Name)
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != "data" || c.VolumeMounts[0].MountPath != "/data" {
			t.Errorf("%s mounts = %+v", c.Name, c.VolumeMounts)
		}
		if c.Resources.Requests.Cpu().IsZero() || c.Resources.Limits.Memory().Cmp(resource.MustParse("256Mi")) != 0 {
			t.Errorf("%s resources = %+v: a workspace quota needs explicit values", c.Name, c.Resources)
		}
		if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
			len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s security context = %+v", c.Name, c.SecurityContext)
		}
	}
}

// runModelCopy runs the init container's script exactly as Kubernetes would, with the model
// image's /model replaced by a local directory.
func runModelCopy(t *testing.T, source, dir, image string) string {
	t.Helper()
	script := strings.Replace(zettaModelCopyScript, zettaModelImageDir+"/.", source+"/.", 1)
	if script == zettaModelCopyScript {
		t.Fatal("the copy script does not read the model image's directory")
	}
	out, err := exec.Command("/bin/sh", "-c", script, "zetta-model", dir, image).CombinedOutput()
	if err != nil {
		t.Fatalf("copy script failed: %v\n%s", err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheModelCopyPlacesACheckpointOnceAndReplacesItForANewImage(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	source := filepath.Join(root, "image", "model")
	writeFile(t, filepath.Join(source, "config.json"), "v16 config")
	writeFile(t, filepath.Join(source, "tokenizer.json"), "v16 tokenizer")
	dir := filepath.Join(root, "data", "models", "base-agent")

	runModelCopy(t, source, dir, "oramasearch/base-agent-0.6b:v16")
	if got := readFile(t, filepath.Join(dir, "tokenizer.json")); got != "v16 tokenizer" {
		t.Fatalf("tokenizer.json = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, zettaModelMarker)); got != "oramasearch/base-agent-0.6b:v16\n" {
		t.Fatalf("marker = %q", got)
	}

	// A restart with the same image copies nothing: a file changed on the volume stays changed.
	writeFile(t, filepath.Join(dir, "config.json"), "untouched")
	if out := runModelCopy(t, source, dir, "oramasearch/base-agent-0.6b:v16"); !strings.Contains(out, "already holds") {
		t.Fatalf("second run output = %q", out)
	}
	if got := readFile(t, filepath.Join(dir, "config.json")); got != "untouched" {
		t.Fatalf("the same image was copied again: config.json = %q", got)
	}

	// A new image replaces the whole copy, and a file the new checkpoint lacks is gone.
	writeFile(t, filepath.Join(dir, "stale.bin"), "old")
	next := filepath.Join(root, "image17", "model")
	writeFile(t, filepath.Join(next, "config.json"), "v17 config")
	writeFile(t, filepath.Join(next, "tokenizer.json"), "v17 tokenizer")
	runModelCopy(t, next, dir, "oramasearch/base-agent-0.6b:v17")
	if got := readFile(t, filepath.Join(dir, "config.json")); got != "v17 config" {
		t.Fatalf("config.json = %q after a new image", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.bin")); !os.IsNotExist(err) {
		t.Fatalf("a file of the old copy survived: %v", err)
	}
	if _, err := os.Stat(dir + ".staging"); !os.IsNotExist(err) {
		t.Fatalf("the staging directory was left behind: %v", err)
	}
}

// A copy that cannot finish never stops the node: the script exits 0, keeps the model it had, and
// leaves no half-copied directory behind.
func TestAFailedModelCopyKeepsThePreviousCopyAndLetsTheNodeStart(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	source := filepath.Join(root, "image", "model")
	writeFile(t, filepath.Join(source, "tokenizer.json"), "v16 tokenizer")
	dir := filepath.Join(root, "data", "models", "base-agent")
	runModelCopy(t, source, dir, "oramasearch/base-agent-0.6b:v16")

	missing := filepath.Join(root, "broken-image", "model")
	out := runModelCopy(t, missing, dir, "oramasearch/base-agent-0.6b:broken")
	if !strings.Contains(out, "the node starts without it") {
		t.Fatalf("failure output = %q", out)
	}
	if got := readFile(t, filepath.Join(dir, "tokenizer.json")); got != "v16 tokenizer" {
		t.Fatalf("the previous copy was lost: tokenizer.json = %q", got)
	}
	if _, err := os.Stat(dir + ".staging"); !os.IsNotExist(err) {
		t.Fatalf("the staging directory was left behind: %v", err)
	}
}

// lake#246: every workspace Zetta gets the Orama base model, and lake is told where it is.
func TestEveryWorkspaceZettaGetsTheBaseModelAndLakeKnowsItsPath(t *testing.T) {
	lake := bareLake()
	models := workspaceZettaModels(lake)
	if len(models) != 1 || models[0].Name != "base-agent" || models[0].Image != "oramasearch/base-agent-0.6b:v16" {
		t.Fatalf("default models = %+v", models)
	}
	lake.Spec.Workspaces.BaseModelImage = "oramasearch/base-agent-0.6b:v17"
	if got := workspaceZettaModels(lake)[0].Image; got != "oramasearch/base-agent-0.6b:v17" {
		t.Fatalf("spec.workspaces.baseModelImage not used: %q", got)
	}

	// The path lake gives native agents is where the init container puts the model.
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_BASE_AGENT_MODEL", Value: "/var/lib/zetta/models/base-agent"}}
	env := lakeEnvMap(lake)
	if got := env["LAKE_BASE_AGENT_MODEL"].Value; got != "/data/models/base-agent" || got != zettaModelPath(models[0].Name) {
		t.Fatalf("LAKE_BASE_AGENT_MODEL = %q, want /data/models/base-agent (spec.env must not move it)", got)
	}
	count := 0
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		if variable.Name == "LAKE_BASE_AGENT_MODEL" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("LAKE_BASE_AGENT_MODEL appears %d times", count)
	}
}

// lake#246: the default workspace Zetta has room to load the base model (about 2.4 GiB in memory).
func TestTheDefaultWorkspaceZettaCanHoldTheBaseModelInMemory(t *testing.T) {
	resources := serviceZettaResources(bareLake())
	if limit := resources.Limits.Memory(); limit.Cmp(resource.MustParse("4Gi")) < 0 {
		t.Fatalf("default memory limit = %s, want at least 4Gi", limit)
	}
	if request := resources.Requests.Memory(); request.Cmp(resource.MustParse("64Mi")) != 0 {
		t.Fatalf("default memory request = %s, want 64Mi", request)
	}
}
