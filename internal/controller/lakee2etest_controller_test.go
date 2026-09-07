/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeE2ETestWaitsForTarget(t *testing.T) {
	scheme := newLakeE2ETestScheme(t)
	test := &oramav1alpha1.LakeE2ETest{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-target"},
		Spec: oramav1alpha1.LakeE2ETestSpec{
			TargetRef: "missing",
			Image:     "registry.example/lake-e2e:sha-1234",
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(test).
		WithStatusSubresource(test).
		Build()
	reconciler := &LakeE2ETestReconciler{Client: client, Scheme: scheme}

	result, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: test.Name}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("requeue = %s", result.RequeueAfter)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Name: test.Name}, test); err != nil {
		t.Fatal(err)
	}
	if test.Status.Phase != "Pending" || test.Status.JobName != "" {
		t.Fatalf("status = %+v", test.Status)
	}
	var jobs batchv1.JobList
	if err := client.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("created %d Jobs without a target", len(jobs.Items))
	}
}

func TestLakeE2ETestCreatesRunnerAndReportsSuccess(t *testing.T) {
	scheme := newLakeE2ETestScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      "lake-system",
			Image:          "oramasearch/lake:sha-1234",
			PullSecretName: "target-pull-secret",
		},
		Status: oramav1alpha1.LakeStatus{
			Phase:         "Ready",
			ReadyReplicas: 1,
			PostgresReady: true,
			SchemaPhase:   "Applied",
		},
	}
	test := &oramav1alpha1.LakeE2ETest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hello",
			UID:  types.UID("run-uid"),
		},
		Spec: oramav1alpha1.LakeE2ETestSpec{
			TargetRef: "hello",
			Image:     "registry.example/lake-e2e:sha-5678",
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lake, test).
		WithStatusSubresource(lake, test, &batchv1.Job{}).
		Build()
	reconciler := &LakeE2ETestReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: test.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Namespace: "lake-system", Name: "hello-e2e"}
	if err := client.Get(context.Background(), jobKey, job); err != nil {
		t.Fatal(err)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "LakeE2ETest" {
		t.Fatalf("owner references = %+v", job.OwnerReferences)
	}
	if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 ||
		job.Spec.Template.Spec.ImagePullSecrets[0].Name != "target-pull-secret" {
		t.Fatalf("image pull secrets = %+v", job.Spec.Template.Spec.ImagePullSecrets)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil ||
		*job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("automountServiceAccountToken = %v", job.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "registry.example/lake-e2e:sha-5678" {
		t.Fatalf("runner image = %q", container.Image)
	}
	wantEnv := map[string]string{
		"LAKE_E2E_BASE_URL": "http://lake-hello.lake-system.svc.cluster.local",
		"LAKE_E2E_RUN_ID":   "run-uid",
	}
	for _, env := range container.Env {
		if want, found := wantEnv[env.Name]; found {
			if env.Value != want {
				t.Errorf("%s = %q, want %q", env.Name, env.Value, want)
			}
			delete(wantEnv, env.Name)
		}
	}
	if len(wantEnv) != 0 {
		t.Fatalf("missing environment variables: %+v", wantEnv)
	}

	job.Status.Succeeded = 1
	if err := client.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Name: test.Name}, test); err != nil {
		t.Fatal(err)
	}
	if test.Status.Phase != "Succeeded" || test.Status.JobName != "hello-e2e" {
		t.Fatalf("status = %+v", test.Status)
	}
}

func newLakeE2ETestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		batchv1.AddToScheme,
		oramav1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}
