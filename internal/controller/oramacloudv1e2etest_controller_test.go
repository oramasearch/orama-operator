/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The OramaCloudV1E2ETest runner (operator#61, #63): its config names the control plane the target
// runs, its pod sits inside the target's network sandbox, and the dashboard fixture reaches it only
// through its environment.

func e2eTestTarget(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(oc *oramav1alpha1.OramaCloudV1) {
		oc.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "v1-master", Key: "key"}
		if mut != nil {
			mut(oc)
		}
	})
}

func e2eTestResource(fixture string) *oramav1alpha1.OramaCloudV1E2ETest {
	t := &oramav1alpha1.OramaCloudV1E2ETest{
		ObjectMeta: metav1.ObjectMeta{Name: "suite", Namespace: "orama-system"},
		Spec:       oramav1alpha1.OramaCloudV1E2ETestSpec{TargetRef: "v1"},
	}
	if fixture != "" {
		t.Spec.DashboardFixtureSecretRef = &corev1.LocalObjectReference{Name: fixture}
	}
	return t
}

func reconcileE2E(t *testing.T, objs ...client.Object) (client.Client, *oramav1alpha1.OramaCloudV1E2ETest) {
	t.Helper()
	scheme := ocv1TestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1E2ETest{}).Build()
	r := &OramaCloudV1E2ETestReconciler{Client: kube, Scheme: scheme}
	key := client.ObjectKey{Namespace: "orama-system", Name: "suite"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	got := &oramav1alpha1.OramaCloudV1E2ETest{}
	if err := kube.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	return kube, got
}

func TestE2EConfigNamesTheTargetsControlPlane(t *testing.T) {
	full := e2eTestTarget(func(oc *oramav1alpha1.OramaCloudV1) {
		oc.Spec.Components.Dashboard.Enabled = true
		oc.Spec.Components.Workers.Enabled = true
		oc.Spec.Components.SearchWorkers.Enabled = true
		oc.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{Audiences: []string{"https://writer.example"}}
	})
	var cfg e2eConfig
	if err := json.Unmarshal([]byte(e2eConfigJSON(e2eTestResource(""), full)), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.WriterURL != "http://v1-writer:8080" || cfg.Cluster.ReadURL != "http://v1-reader" ||
		cfg.CollectionID != e2eCollectionID || cfg.IndexID != e2eIndexID || cfg.APIKey != e2eRWKey {
		t.Fatalf("engine config = %+v", cfg)
	}
	if cfg.DashboardURL != "http://v1-dashboard:3000" || !cfg.WriterJWT {
		t.Fatalf("dashboard = %q, writerJWT = %v", cfg.DashboardURL, cfg.WriterJWT)
	}
	want := []e2eWorker{
		{"orama-workers", "http://v1-workers:8080"},
		{"index-shared-worker", "http://v1-index-shared-worker:8080"},
		{"large-index-worker", "http://v1-large-index-worker:8080"},
		{"openai-decrypter", "http://v1-openai-decrypter:8080"},
		{"telemetry-worker", "http://v1-telemetry-worker:8080"},
	}
	if !reflect.DeepEqual(cfg.Workers, want) {
		t.Fatalf("workers = %+v", cfg.Workers)
	}

	for name, target := range map[string]*oramav1alpha1.OramaCloudV1{"nothing enabled": e2eTestTarget(nil), "no target": nil} {
		var bare e2eConfig
		if err := json.Unmarshal([]byte(e2eConfigJSON(e2eTestResource(""), target)), &bare); err != nil {
			t.Fatal(err)
		}
		if bare.DashboardURL != "" || bare.WriterJWT || bare.Workers == nil || len(bare.Workers) != 0 {
			t.Errorf("%s: config = %+v (the suite reads .workers as a list)", name, bare)
		}
	}
}

func TestE2EJobRunsInsideTheTargetsSandbox(t *testing.T) {
	kube, got := reconcileE2E(t, e2eTestTarget(nil), e2eTestResource(""))
	job := &batchv1.Job{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e"}, job); err != nil {
		t.Fatalf("no Job (status %+v): %v", got.Status, err)
	}
	pod := job.Spec.Template.Labels
	want := labelsFor(e2eTestTarget(nil), e2eComponent)
	want["orama.cloud/e2e"] = "suite"
	if !reflect.DeepEqual(pod, want) {
		t.Fatalf("runner pod labels = %v, want %v", pod, want)
	}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "DASHBOARD_FIXTURE_") {
			t.Errorf("fixture variable %s with no fixture", e.Name)
		}
	}
	cm := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e-config"}, cm); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"config.json", "functional.sh", "lib.sh", "control-plane.sh"} {
		if cm.Data[k] == "" {
			t.Errorf("ConfigMap has no %s", k)
		}
	}
}

func TestE2EFixtureReachesTheRunnerOnlyThroughItsEnvironment(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "orama-system"},
		Data: map[string][]byte{
			"collectionID": []byte("fixture-coll"), "indexID": []byte("fixture-index"), "apiKey": []byte("fixture-private-key"),
		},
	}
	kube, _ := reconcileE2E(t, e2eTestTarget(nil), e2eTestResource("fixture"), secret)
	job := &batchv1.Job{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e"}, job); err != nil {
		t.Fatal(err)
	}
	got := map[string]corev1.EnvVar{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		got[e.Name] = e
	}
	for _, k := range e2eFixtureKeys {
		e, ok := got[k.env]
		if !ok || e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s = %+v, want a secretKeyRef", k.env, e)
		}
		ref := e.ValueFrom.SecretKeyRef
		optional := ref.Optional != nil && *ref.Optional
		if ref.Name != "fixture" || ref.Key != k.key || optional != k.optional {
			t.Errorf("%s reads %+v", k.env, ref)
		}
	}
	cm := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e-config"}, cm); err != nil {
		t.Fatal(err)
	}
	for k, v := range cm.Data {
		if strings.Contains(v, "fixture-private-key") {
			t.Errorf("the fixture's private API key is in ConfigMap key %s", k)
		}
	}
}

func TestE2EWaitsForACompleteFixtureSecret(t *testing.T) {
	cases := map[string][]client.Object{
		"waiting for dashboard fixture Secret orama-system/fixture": nil,
		"to hold a non-empty apiKey": {&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "orama-system"},
			Data:       map[string][]byte{"collectionID": []byte("c"), "indexID": []byte("i")},
		}},
	}
	for msg, extra := range cases {
		kube, got := reconcileE2E(t, append([]client.Object{e2eTestTarget(nil), e2eTestResource("fixture")}, extra...)...)
		if got.Status.Phase != "Pending" || !strings.Contains(got.Status.Message, msg) {
			t.Errorf("status = %+v, want Pending with %q", got.Status, msg)
		}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e"}, &batchv1.Job{}); err == nil {
			t.Errorf("%s: a Job was created before the fixture was complete", msg)
		}
	}
}

// The suite's three files: valid sh, and the functional suite sources the other two from its own
// directory and calls every control-plane section.
func TestE2EScriptsParseAndWireTheControlPlane(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"functional.sh": e2eFunctionalScript, "lib.sh": e2eLibScript, "control-plane.sh": e2eControlPlaneScript} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
	for _, want := range []string{`. "$E2E_DIR/lib.sh"`, `. "$E2E_DIR/control-plane.sh"`, "\ncp_prepare\n", "\ncp_dashboard\n", "\ncp_write_tokens\n", "\ncp_workers\n", "\ncp_expiry\n"} {
		if !strings.Contains(e2eFunctionalScript, want) {
			t.Errorf("functional.sh lacks %q", want)
		}
	}
	if strings.Index(e2eFunctionalScript, "\ncp_expiry\n") > strings.Index(e2eFunctionalScript, `section "SUMMARY"`) {
		t.Error("cp_expiry runs after the summary")
	}
}

// The runner (operator#1158). Once the prebuilt oramasearch/e2e-runner is pinned, every default
// spec.image (empty, the short alpine name, the CRD default existing objects carry) runs it, a custom
// image is kept, and no command installs a package at start; the command refuses an image without
// curl or jq. Until it is pinned, the legacy alpine installs them as before.
func TestE2ERunnerPrebuiltImageRunsTheSuiteWithoutInstalling(t *testing.T) {
	const prebuilt = "docker.io/oramasearch/e2e-runner:alpine3.20-1@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	for _, image := range []string{"", "alpine:3.20", e2eRunnerImage, "custom@sha256:" + strings.Repeat("1", 64)} {
		runner, command := e2eRunner(image, prebuilt)
		want := prebuilt
		if strings.HasPrefix(image, "custom") {
			want = image
		}
		if runner != want {
			t.Errorf("spec.image %q runs %q, want %q", image, runner, want)
		}
		script := strings.Join(command, " ")
		if strings.Contains(script, "apk") || !strings.Contains(script, "command -v curl") ||
			!strings.Contains(script, "command -v jq") || !strings.HasSuffix(script, "exec sh "+e2eScriptPath) {
			t.Errorf("spec.image %q command = %q", image, script)
		}
	}

	runner, command := e2eRunner("", "")
	if runner != e2eRunnerImage || !strings.Contains(strings.Join(command, " "), "apk add --no-cache curl jq") {
		t.Errorf("without a prebuilt runner: %q %q", runner, command)
	}
	// The default the operator ships matches the Job it writes today.
	kube, _ := reconcileE2E(t, e2eTestTarget(nil), e2eTestResource(""))
	job := &batchv1.Job{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e"}, job); err != nil {
		t.Fatal(err)
	}
	wantImage, wantCommand := e2eRunner("", e2eRunnerPrebuiltImage)
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != wantImage || !reflect.DeepEqual(c.Command, wantCommand) {
		t.Errorf("Job runs %q %q, want %q %q", c.Image, c.Command, wantImage, wantCommand)
	}
}

// A test without its own pull secret pulls the runner with the target stack's, which can read the
// private prebuilt runner (operator#1158); the test's own secret wins.
func TestE2ERunnerPullsWithTheTargetsSecret(t *testing.T) {
	target := e2eTestTarget(func(oc *oramav1alpha1.OramaCloudV1) { oc.Spec.PullSecretName = "stack-pull" })
	for _, own := range []string{"", "test-pull"} {
		test := e2eTestResource("")
		test.Spec.PullSecretName = own
		kube, _ := reconcileE2E(t, target.DeepCopy(), test)
		job := &batchv1.Job{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "suite-e2e"}, job); err != nil {
			t.Fatal(err)
		}
		want := "stack-pull"
		if own != "" {
			want = own
		}
		if got := job.Spec.Template.Spec.ImagePullSecrets; len(got) != 1 || got[0].Name != want {
			t.Errorf("test pull secret %q: Job pulls with %v, want %s", own, got, want)
		}
	}
}
