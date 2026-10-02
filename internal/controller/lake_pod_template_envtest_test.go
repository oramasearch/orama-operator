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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// startLakeEnvtest runs a real API server, so defaulting and the Deployment generation behave
// as they do in a cluster: the generation moves only when the stored spec changes.
func startLakeEnvtest(t *testing.T) client.Client {
	t.Helper()
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
	kubeClient, err := client.New(config, client.Options{Scheme: newLakeScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return kubeClient
}

func lakeTemplateLabels(lake *oramav1alpha1.Lake) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lake",
		"app.kubernetes.io/instance":   lake.Name,
		"app.kubernetes.io/managed-by": "orama-operator",
	}
}

func TestLakePodTemplateEnvtest(t *testing.T) {
	kubeClient := startLakeEnvtest(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-template"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:test",
			Apps: oramav1alpha1.LakeAppsSpec{
				Enabled:      true,
				Wildcard:     "apps.example.com",
				RuntimeImage: "oramasearch/app-runtime:0.1.0",
			},
			Builds: oramav1alpha1.LakeBuildsSpec{Enabled: true},
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
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: kubeClient.Scheme()}
	labels := lakeTemplateLabels(lake)
	sealing := lakeSealingMaterial{KID: "test-kid", PublicKey: make([]byte, 32)}
	rwo := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx, lake, "lake-dev", "lake-pod-template", lake.Spec.Image, "lake-pod-template-store",
		rwo, labels, 1, lakeDefaultPort, sealing,
	); err != nil {
		t.Fatal(err)
	}

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "lake-dev", Name: "lake-pod-template"}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	// The API server accepts and keeps the surge strategy, the co-location affinity, the probes
	// and the sleep preStop action as rendered.
	assertLakeServesThroughRollout(t, deployment)
	affinity := deployment.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.PodAffinity == nil ||
		len(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 ||
		affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey != "kubernetes.io/hostname" {
		t.Fatalf("single-replica RWO Lake affinity = %+v, want the surge pod on the running pod's node", affinity)
	}
	securityContext := deployment.Spec.Template.Spec.SecurityContext
	if securityContext == nil || securityContext.FSGroupChangePolicy == nil ||
		*securityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("pod fsGroupChangePolicy = %+v, want %q", securityContext, corev1.FSGroupChangeOnRootMismatch)
	}
	assertEnvValueOnce(t, deployment, "LAKE_APPS_WILDCARD", "apps.example.com")
	assertEnvValueOnce(t, deployment, "LAKE_APPS_RUNTIME_IMAGE", "oramasearch/app-runtime:0.1.0")
	assertEnvValueOnce(t, deployment, "LAKE_INTERNAL_URL", "http://lake-pod-template.lake-dev.svc:80")

	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Apps.Wildcard = "managed.example.com"
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_APPS_WILDCARD", Value: "override.example.com"}}
	if err := kubeClient.Update(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.reconcileLakeDeployment(
		ctx, lake, "lake-dev", "lake-pod-template", lake.Spec.Image, "lake-pod-template-store",
		rwo, labels, 1, lakeDefaultPort, sealing,
	); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	assertEnvValueOnce(t, deployment, "LAKE_APPS_WILDCARD", "override.example.com")
}

// TestLakeSpecChangeRollsOnce is operator#243: patching spec.apps.runtimeImage must change the
// Deployment once. The store is created through reconcileLakeStore, so the rollout choice comes
// from the real claim, as in Reconcile.
func TestLakeSpecChangeRollsOnce(t *testing.T) {
	kubeClient := startLakeEnvtest(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "one-rollout"},
		Spec: oramav1alpha1.LakeSpec{
			Image: "oramasearch/lake:test",
			Apps: oramav1alpha1.LakeAppsSpec{
				Enabled:      true,
				Wildcard:     "apps.example.com",
				RuntimeImage: "oramasearch/app-runtime:first",
			},
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
	reconciler := &LakeReconciler{Client: kubeClient, Scheme: kubeClient.Scheme()}
	labels := lakeTemplateLabels(lake)
	sealing := lakeSealingMaterial{KID: "test-kid", PublicKey: make([]byte, 32)}
	key := client.ObjectKey{Namespace: "lake-dev", Name: "lake-one-rollout"}

	// reconcile runs the store and Deployment steps the way Reconcile does, from a fresh read of
	// the Lake, and returns the stored Deployment.
	reconcile := func() *appsv1.Deployment {
		t.Helper()
		current := &oramav1alpha1.Lake{}
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), current); err != nil {
			t.Fatal(err)
		}
		claimName, accessModes, err := reconciler.reconcileLakeStore(ctx, current, "lake-dev", labels)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.reconcileLakeDeployment(
			ctx, current, "lake-dev", key.Name, current.Spec.Image, claimName,
			accessModes, labels, 1, lakeDefaultPort, sealing,
		); err != nil {
			t.Fatal(err)
		}
		deployment := &appsv1.Deployment{}
		if err := kubeClient.Get(ctx, key, deployment); err != nil {
			t.Fatal(err)
		}
		return deployment
	}

	initial := reconcile()
	steady := reconcile()
	if steady.Generation != initial.Generation ||
		!apiequality.Semantic.DeepEqual(steady.Spec.Template, initial.Spec.Template) {
		t.Fatalf("a reconcile with no spec change moved the Deployment: generation %d -> %d",
			initial.Generation, steady.Generation)
	}

	current := &oramav1alpha1.Lake{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(lake), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Apps.RuntimeImage = "oramasearch/app-runtime:second"
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}

	changed := reconcile()
	if changed.Generation != initial.Generation+1 {
		t.Fatalf("the spec change moved the Deployment generation %d -> %d, want exactly one step",
			initial.Generation, changed.Generation)
	}
	assertEnvValueOnce(t, changed, "LAKE_APPS_RUNTIME_IMAGE", "oramasearch/app-runtime:second")
	expected := initial.Spec.Template.DeepCopy()
	for i := range expected.Spec.Containers[0].Env {
		if expected.Spec.Containers[0].Env[i].Name == "LAKE_APPS_RUNTIME_IMAGE" {
			expected.Spec.Containers[0].Env[i].Value = "oramasearch/app-runtime:second"
		}
	}
	if !apiequality.Semantic.DeepEqual(*expected, changed.Spec.Template) {
		t.Fatalf("the spec change altered more of the pod template than LAKE_APPS_RUNTIME_IMAGE:\nwant %+v\ngot  %+v",
			expected, changed.Spec.Template)
	}

	// The second and later reconciles after the change leave the template alone: one rollout.
	for attempt := 0; attempt < 3; attempt++ {
		again := reconcile()
		if again.Generation != changed.Generation ||
			!apiequality.Semantic.DeepEqual(again.Spec.Template, changed.Spec.Template) {
			t.Fatalf("reconcile %d after the change moved the Deployment: generation %d -> %d",
				attempt+2, changed.Generation, again.Generation)
		}
	}
}

func assertEnvValueOnce(t *testing.T, deployment *appsv1.Deployment, name, want string) {
	t.Helper()
	count := 0
	value := ""
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == name {
			count++
			value = variable.Value
		}
	}
	if count != 1 || value != want {
		t.Fatalf("%s count = %d, value = %q, want one value %q", name, count, value, want)
	}
}
