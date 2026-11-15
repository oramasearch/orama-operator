package controller

import (
	"encoding/json"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// #801 (LSR-139): an image a tenant chose is never pulled with the platform's registry authority.

func TestAnImageModeAppNeverGetsThePlatformPullSecret(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{Host: "lake.example.test", PullSecretName: "oramasearch-dockerhub"}}
	synced := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{
		Mode: oramav1alpha1.LakeAppModeImage, Image: "docker.io/oramasearch/lake@sha256:" + sha256Hex([]byte("x")),
		Sync: &oramav1alpha1.LakeAppSync{ID: testWorkspaceID},
	}}
	if got := lakeAppPlatformPullSecretName(lake, synced, oramav1alpha1.LakeAppModeImage); got != "" {
		t.Fatalf("a synchronized image-mode app took pull Secret %q; it must get lake's registry token alone", got)
	}
	handWritten := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{Mode: oramav1alpha1.LakeAppModeImage}}
	if got := lakeAppPlatformPullSecretName(lake, handWritten, oramav1alpha1.LakeAppModeImage); got != "" {
		t.Fatalf("an image-mode app without its own pull Secret took %q", got)
	}
	handWritten.Spec.PullSecretName = "own-registry"
	if got := lakeAppPlatformPullSecretName(lake, handWritten, oramav1alpha1.LakeAppModeImage); got != "own-registry" {
		t.Fatalf("a hand-written image-mode app keeps its own pull Secret, got %q", got)
	}
	runtime := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{Sync: &oramav1alpha1.LakeAppSync{ID: testWorkspaceID}}}
	if got := lakeAppPlatformPullSecretName(lake, runtime, oramav1alpha1.LakeAppModeRuntime); got != lake.Spec.PullSecretName {
		t.Fatalf("a runtime-mode app pulls the platform's app runtime image with the Lake's pull Secret, got %q", got)
	}
}

func TestASynchronizedImageAppPullSecretHoldsOnlyLakesRegistry(t *testing.T) {
	config, ok := mergeLakeAppDockerConfig(nil, "lake.example.test", []byte("pull-token"))
	if !ok {
		t.Fatal("encoding failed")
	}
	var decoded struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Auths) != 1 || decoded.Auths["lake.example.test"]["password"] != "pull-token" {
		t.Fatalf("auths = %v, want lake.example.test alone", decoded.Auths)
	}
}

func TestOnlyThePlatformMethodologyRuntimeGetsThePlatformPullSecret(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{PullSecretName: "oramasearch-dockerhub"}}
	if !methodologyRuntimeIsPlatform(lake, lakeDefaultMethodologyRuntimeImage) {
		t.Fatal("the default methodology runtime is the platform's")
	}
	lake.Spec.Methodologies.RuntimeImage = "oramasearch/methodology-runtime:pinned"
	if !methodologyRuntimeIsPlatform(lake, "oramasearch/methodology-runtime:pinned") {
		t.Fatal("the pinned methodology runtime is the platform's")
	}
	for _, image := range []string{"", lakeDefaultMethodologyRuntimeImage, "oramasearch/lake:v0.5.0"} {
		if methodologyRuntimeIsPlatform(lake, image) {
			t.Fatalf("%q is not the platform runtime this Lake names", image)
		}
	}
	if methodologyPullSecret("run-a") == methodologyRuntimePullSecret("run-a") {
		t.Fatal("tenant and runtime images use different pull Secrets")
	}
}
