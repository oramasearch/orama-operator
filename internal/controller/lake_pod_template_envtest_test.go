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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakePodTemplateEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for Lake pod-template envtest")
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
	scheme := newLakeScheme(t)
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-template"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:test",
			Store: oramav1alpha1.LakeStoreSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake-dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: scheme}
	labels := map[string]string{
		"app.kubernetes.io/name":       "lake",
		"app.kubernetes.io/instance":   lake.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
	sealing := lakeSealingMaterial{KID: "test-kid", PublicKey: make([]byte, 32)}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx,
		lake,
		"lake-dev",
		"lake-pod-template",
		lake.Spec.Image,
		"lake-pod-template-store",
		labels,
		1,
		lakeDefaultPort,
		sealing,
	); err != nil {
		t.Fatal(err)
	}

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "lake-dev", Name: "lake-pod-template"}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("deployment strategy = %q, want %q", deployment.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	}
	if deployment.Spec.Strategy.RollingUpdate != nil {
		t.Fatalf("Recreate deployment has rollingUpdate settings: %+v", deployment.Spec.Strategy.RollingUpdate)
	}
}
