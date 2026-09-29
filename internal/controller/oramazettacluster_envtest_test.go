/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestZettaEnvEnvtestUpdatesPodTemplateForRollout(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for Zetta env envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	testEnvironment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	ctx := context.Background()
	scheme := zettaTestScheme(t)
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "zetta-envtest"}}
	if err := kubeClient.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	operatorSchema := false
	cluster := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "allocator", Namespace: namespace.Name},
		Spec: oramav1alpha1.OramaZettaClusterSpec{
			Image: "oramasearch/zetta:test",
			Env: []corev1.EnvVar{
				{Name: "MALLOC_ARENA_MAX", Value: "2"},
				{Name: "ZETTA_ADMIN_TOKEN", Value: "rejected"},
			},
			ProvisionOperatorSchema: &operatorSchema,
		},
	}
	if err := kubeClient.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	statefulSet := &appsv1.StatefulSet{}
	statefulSetKey := client.ObjectKey{Namespace: namespace.Name, Name: "allocator-zetta"}
	if err := kubeClient.Get(ctx, statefulSetKey, statefulSet); err != nil {
		t.Fatal(err)
	}
	if env := findEnv(statefulSet.Spec.Template.Spec.Containers[0].Env, "MALLOC_ARENA_MAX"); env == nil || env.Value != "2" {
		t.Fatalf("initial MALLOC_ARENA_MAX = %+v", env)
	}
	securityContext := statefulSet.Spec.Template.Spec.SecurityContext
	if securityContext == nil || securityContext.FSGroupChangePolicy == nil ||
		*securityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("pod fsGroupChangePolicy = %+v, want %q", securityContext, corev1.FSGroupChangeOnRootMismatch)
	}
	admin := findEnv(statefulSet.Spec.Template.Spec.Containers[0].Env, "ZETTA_ADMIN_TOKEN")
	if admin == nil || admin.ValueFrom == nil || admin.ValueFrom.SecretKeyRef == nil || admin.ValueFrom.SecretKeyRef.Name != "allocator-zetta-admin" {
		t.Fatalf("managed ZETTA_ADMIN_TOKEN was overridden: %+v", admin)
	}
	initialGeneration := statefulSet.Generation

	if err := kubeClient.Get(ctx, request.NamespacedName, cluster); err != nil {
		t.Fatal(err)
	}
	if len(cluster.Status.IgnoredEnvVars) != 1 || cluster.Status.IgnoredEnvVars[0] != "ZETTA_ADMIN_TOKEN" {
		t.Fatalf("ignored env status = %v", cluster.Status.IgnoredEnvVars)
	}
	cluster.Spec.Env = []corev1.EnvVar{{Name: "MALLOC_ARENA_MAX", Value: "4"}}
	if err := kubeClient.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, statefulSetKey, statefulSet); err != nil {
		t.Fatal(err)
	}
	if env := findEnv(statefulSet.Spec.Template.Spec.Containers[0].Env, "MALLOC_ARENA_MAX"); env == nil || env.Value != "4" {
		t.Fatalf("updated MALLOC_ARENA_MAX = %+v", env)
	}
	if statefulSet.Generation <= initialGeneration {
		t.Fatalf("StatefulSet generation did not advance for the pod-template change: %d -> %d", initialGeneration, statefulSet.Generation)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, cluster); err != nil {
		t.Fatal(err)
	}
	if len(cluster.Status.IgnoredEnvVars) != 0 {
		t.Fatalf("ignored env status did not clear: %v", cluster.Status.IgnoredEnvVars)
	}
}
