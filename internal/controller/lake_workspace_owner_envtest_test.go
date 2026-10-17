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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// startWorkspaceEnvtest starts an API server with the operator's CRDs and creates the Lake
// namespace, a services-enabled workspace Lake, and its control DB app Secret.
func startWorkspaceEnvtest(t *testing.T) (client.Client, *runtime.Scheme, *oramav1alpha1.Lake) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the workspace envtests")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{crdPath},
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
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, controlDBAppSecretForTest(lake.Name, "postgresql://unused")); err != nil {
		t.Fatal(err)
	}
	return kube, scheme, lake
}

// markWorkspaceZettaReady stands in for the OramaZettaCluster controller, which envtest does not
// run: the cluster reports its one node Ready and its admin Secret holds a token.
func markWorkspaceZettaReady(t *testing.T, kube client.Client, key client.ObjectKey, token string) {
	t.Helper()
	ctx := context.Background()
	cluster := &oramav1alpha1.OramaZettaCluster{}
	if err := kube.Get(ctx, key, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Status.Phase = "Ready"
	cluster.Status.ReadyNodes = 1
	if err := kube.Status().Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if token == "" {
		return
	}
	if err := kube.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name + "-zetta-admin", Namespace: key.Namespace},
		Data:       map[string][]byte{"token": []byte(token)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceZettaHasOneOwnerAcrossPolls(t *testing.T) {
	ctx := context.Background()
	kube, scheme, lake := startWorkspaceEnvtest(t)

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

	markWorkspaceZettaReady(t, kube, key, "workspace-admin-token")
	services.reconcileLakeServices(ctx, lake, testServiceNamespace, labels)
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

// TestWorkspaceCreatedAfterStartGetsItsConnection is operator#264: a workspace created while the
// operator is already polling, whose Zetta service id differs from the workspace id, must have its
// connection written to the services Secret (under its slug and its service id) before the
// workspace or its Zetta service reports Ready.
func TestWorkspaceCreatedAfterStartGetsItsConnection(t *testing.T) {
	ctx := context.Background()
	kube, scheme, lake := startWorkspaceEnvtest(t)

	const (
		workspaceServiceID = "7e1f982e-2f4a-482b-93ac-4961b8248654"
		workspaceSlug      = "ws-123e4567e89b"
	)
	serviceStore := &fakeServiceStore{}
	workspaceStore := &fakeWorkspacesStore{serviceStore: serviceStore}
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
		APIReader:     kube,
		ServiceStores: serviceStore.opener(),
	}
	labels := map[string]string{"app.kubernetes.io/name": "lake"}
	poll := func() (servicesChanging, workspacesChanging bool) {
		servicesChanging = services.reconcileLakeServices(ctx, lake, testServiceNamespace, labels)
		workspacesChanging = workspaces.pollLakeWorkspaces(ctx, lake)
		return servicesChanging, workspacesChanging
	}
	servicesSecret := func() *corev1.Secret {
		t.Helper()
		secret := &corev1.Secret{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: lakeServicesSecret(lake.Name)}, secret); err != nil {
			t.Fatal(err)
		}
		return secret
	}

	// The operator is running and steady with no workspaces.
	for range 2 {
		if servicesChanging, workspacesChanging := poll(); servicesChanging || workspacesChanging {
			t.Fatalf("empty Lake is changing: services %v workspaces %v", servicesChanging, workspacesChanging)
		}
	}
	if keys := servicesSecret().Data; len(keys) != 0 {
		t.Fatalf("services Secret holds %d keys before any workspace", len(keys))
	}

	// lake's create_workspace: one workspace row and its main Zetta service row, together.
	row := testWorkspaceRow("running")
	row.ZettaServiceID = workspaceServiceID
	workspaceStore.rows = append(workspaceStore.rows, row)
	service := testService(workspaceServiceID, workspaceSlug, serviceKindZetta, `{}`, true)
	service.WorkspaceNamespace = ""
	service.WorkspaceZetta = true
	service.Generation = row.ZettaServiceGeneration
	serviceStore.services = append(serviceStore.services, service)

	if _, workspacesChanging := poll(); !workspacesChanging {
		t.Fatal("new workspace did not report provisioning")
	}
	if got := workspaceStore.rows[0]; got.Phase != WorkspacePhasePending || got.Namespace != testWorkspaceName {
		t.Fatalf("new workspace status = %+v", got)
	}
	// lake's services query joins the workspace and so now carries its reconciled namespace.
	serviceStore.services[0].WorkspaceNamespace = workspaceStore.rows[0].Namespace
	if servicesChanging, _ := poll(); !servicesChanging {
		t.Fatal("services poll did not keep polling a workspace Zetta that is still starting")
	}
	key := client.ObjectKey{Namespace: testWorkspaceName, Name: testWorkspaceName}

	// Ready nodes without an admin token: nothing to deliver yet, and not Ready.
	markWorkspaceZettaReady(t, kube, key, "")
	if servicesChanging, workspacesChanging := poll(); !servicesChanging || !workspacesChanging {
		t.Fatalf("undelivered workspace stopped polling: services %v workspaces %v", servicesChanging, workspacesChanging)
	}
	if got := workspaceStore.rows[0]; got.Phase != WorkspacePhasePending || got.Message != workspaceDeliveringMessage {
		t.Fatalf("workspace Ready before its connection was delivered: %+v", got)
	}
	if status := serviceStore.status(t, workspaceServiceID); status.Phase == ServicePhaseReady {
		t.Fatalf("workspace Zetta service Ready before its connection was delivered: %+v", status)
	}

	// The admin token appears: one pass of each delivers the connection and reports Ready.
	if err := kube.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name + "-zetta-admin", Namespace: key.Namespace},
		Data:       map[string][]byte{"token": []byte("new-workspace-token")},
	}); err != nil {
		t.Fatal(err)
	}
	if servicesChanging, workspacesChanging := poll(); servicesChanging || workspacesChanging {
		t.Fatalf("delivered workspace still changing: services %v workspaces %v", servicesChanging, workspacesChanging)
	}
	endpoint := "http://prod-ws-123e4567e89b-zetta.prod-ws-123e4567e89b.svc.cluster.local:4800"
	want := `{"kind":"zetta","endpoint":"` + endpoint + `","token":"new-workspace-token"}`
	secret := servicesSecret()
	if got := string(secret.Data[serviceCredentialKey(workspaceSlug)]); got != want {
		t.Fatalf("%s = %q, want %q", serviceCredentialKey(workspaceSlug), got, want)
	}
	if got := string(secret.Data[workspaceServiceID]); got != want {
		t.Fatalf("service-id key = %q, want %q", got, want)
	}
	if got := workspaceStore.rows[0]; got.Phase != WorkspacePhaseReady {
		t.Fatalf("delivered workspace status = %+v", got)
	}
	status := serviceStore.status(t, workspaceServiceID)
	if status.Phase != ServicePhaseReady || status.Endpoint != endpoint {
		t.Fatalf("delivered workspace Zetta service status = %+v", status)
	}

	// Steady: no Secret or status rewrites.
	version := secret.ResourceVersion
	statusWrites := serviceStore.upsertCount()
	for range 3 {
		if servicesChanging, workspacesChanging := poll(); servicesChanging || workspacesChanging {
			t.Fatalf("steady workspace changing: services %v workspaces %v", servicesChanging, workspacesChanging)
		}
	}
	if servicesSecret().ResourceVersion != version {
		t.Fatal("steady polls rewrote the services Secret")
	}
	if serviceStore.upsertCount() != statusWrites {
		t.Fatalf("steady polls rewrote service status: %d -> %d", statusWrites, serviceStore.upsertCount())
	}
}
