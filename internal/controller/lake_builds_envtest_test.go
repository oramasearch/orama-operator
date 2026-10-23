/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestLakeBuildEnvtestRowToJobToStatus(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for Lake build envtest")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		CRDs:                  networkSandboxCiliumTestCRDs(),
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := newLakeScheme(t)
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	lake := buildTestLake()
	lake.Name = "build-envtest"
	lake.UID = ""
	if err := kube.Create(context.Background(), lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Name: lake.Name}, lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lakeNamespace(lake)}}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lake.Spec.PullSecretName, Namespace: lakeNamespace(lake)},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}); err != nil {
		t.Fatal(err)
	}
	build := testBuild("55555555-5555-4555-8555-555555555555", testBuildTarget)
	store := &fakeServiceStore{builds: []LakeBuild{build}}
	reconciler := &LakeReconciler{
		Client: kube,
		Scheme: scheme,
		BuildLogs: func(context.Context, string, string) (string, error) {
			return `{"digest":"sha256:` + strings.Repeat("d", 64) + `","size":77}` + "\n", nil
		},
	}
	if !reconciler.reconcileLakeBuilds(context.Background(), lake, store, store.builds) {
		t.Fatal("new build did not request a follow-up poll")
	}
	target, _ := parseBuildTarget(testBuildTarget)
	namespace := buildNamespace(lake.Name, target.Space)
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: buildTokenSecretName(build.ID)}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: buildJobName(build.ID)}},
		metadataDenyForTest(namespace),
	} {
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("created %T: %v", object, err)
		}
	}
	if !reconciler.reconcileLakeBuilds(context.Background(), lake, store, store.builds) {
		t.Fatal("suspended Job release did not request a follow-up poll")
	}
	job := &batchv1.Job{}
	jobKey := client.ObjectKey{Namespace: namespace, Name: buildJobName(build.ID)}
	if err := kube.Get(context.Background(), jobKey, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed",
	}}
	if err := kube.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if reconciler.reconcileLakeBuilds(context.Background(), lake, store, store.builds) {
		t.Fatal("completed build stayed on the fast poll")
	}
	status := buildStatus(t, store, build.ID)
	if status.Phase != BuildPhaseSucceeded || status.Size != 77 || status.Digest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("build status = %+v", status)
	}
}
