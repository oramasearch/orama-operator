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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestWorkspaceZettaHasOneOwnerAcrossPolls(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for workspace Zetta ownership envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
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

	ctx := context.Background()
	scheme := newLakeScheme(t)
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testServiceNamespace}}); err != nil {
		t.Fatal(err)
	}
	lake := testWorkspaceLake()
	lake.Spec.PullSecretName = ""
	lake.Spec.Services.Enabled = true
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	controlDB := controlDBAppSecretForTest(lake.Name, "postgresql://unused")
	if err := kube.Create(ctx, controlDB); err != nil {
		t.Fatal(err)
	}

	service := testService(testWorkspaceZettaID, testDefaultSlug, serviceKindZetta, `{"instances":3,"storage_gi":80}`, true)
	service.WorkspaceNamespace = testWorkspaceName
	service.WorkspaceZetta = true
	service.Generation = 11
	serviceStore := &fakeServiceStore{services: []LakeService{service}}
	workspaceStore := &fakeWorkspacesStore{
		rows:         []LakeWorkspaceRow{testWorkspaceRow("running")},
		serviceStore: serviceStore,
	}
	workspaces := &LakeWorkspacesReconciler{
		Client:    kube,
		Scheme:    scheme,
		APIReader: kube,
		WorkspaceStores: func(context.Context, string) (LakeWorkspacesStore, error) {
			return workspaceStore, nil
		},
		OperatorNamespace: "orama-system",
	}
	services := &LakeReconciler{
		Client:        kube,
		Scheme:        scheme,
		ServiceStores: serviceStore.opener(),
	}
	labels := map[string]string{"app.kubernetes.io/name": "lake"}

	if changing := workspaces.pollLakeWorkspaces(ctx, lake); !changing {
		t.Fatal("first workspace poll did not report provisioning")
	}
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: testWorkspaceName}
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := kube.Get(ctx, key, cluster); err != nil {
		t.Fatal(err)
	}
	firstGeneration := cluster.Generation
	if firstGeneration == 0 {
		t.Fatal("workspace Zetta has no API-server generation")
	}
	assertWorkspaceZettaOwnedSpec(t, cluster)

	lake.Spec.Workspaces.Storage.Size = "90Gi"
	for range 3 {
		services.reconcileLakeServices(ctx, lake, testServiceNamespace, labels)
		workspaces.pollLakeWorkspaces(ctx, lake)
		if err := kube.Get(ctx, key, cluster); err != nil {
			t.Fatal(err)
		}
		if cluster.Generation != firstGeneration {
			t.Fatalf("workspace Zetta generation changed across polls: %d -> %d", firstGeneration, cluster.Generation)
		}
		assertWorkspaceZettaOwnedSpec(t, cluster)
	}

	cluster.Status.Phase = "Ready"
	cluster.Status.ReadyNodes = 1
	if err := kube.Status().Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if changing := workspaces.pollLakeWorkspaces(ctx, lake); changing {
		t.Fatal("ready workspace requested another fast poll")
	}
	status := serviceStore.status(t, testWorkspaceZettaID)
	if status.Phase != ServicePhaseReady || status.Message != "service ready" ||
		status.Endpoint != "http://prod-ws-123e4567e89b-zetta.prod-ws-123e4567e89b.svc.cluster.local:4800" ||
		status.ObservedGeneration != service.Generation {
		t.Fatalf("workspace-owned service status = %+v", status)
	}
	statusWrites := serviceStore.upsertCount()
	for range 3 {
		services.reconcileLakeServices(ctx, lake, testServiceNamespace, labels)
		workspaces.pollLakeWorkspaces(ctx, lake)
	}
	if serviceStore.upsertCount() != statusWrites {
		t.Fatalf("steady polls rewrote service status: %d -> %d", statusWrites, serviceStore.upsertCount())
	}
	if err := kube.Get(ctx, key, cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Generation != firstGeneration {
		t.Fatalf("ready workspace Zetta generation changed: %d -> %d", firstGeneration, cluster.Generation)
	}
}

func assertWorkspaceZettaOwnedSpec(t *testing.T, cluster *oramav1alpha1.OramaZettaCluster) {
	t.Helper()
	if cluster.Spec.Image != "oramasearch/zetta:service" || cluster.Spec.Replicas != workspaceZettaReplicas ||
		cluster.Spec.Storage.Size != "20Gi" || cluster.Spec.Storage.StorageClassName != "managed-csi" {
		t.Fatalf("workspace-owned Zetta spec = %+v", cluster.Spec)
	}
}
