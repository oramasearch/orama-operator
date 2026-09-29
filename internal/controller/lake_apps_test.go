/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testAppRowID = "91b78263-af2c-40eb-b09c-7c768c6d4d94"

type fakeAppsStore struct {
	rows         []LakeAppRow
	services     []LakeService
	listErr      error
	writeErr     error
	statusWrites int
	hashWrites   int
}

func (s *fakeAppsStore) ListApps(context.Context) ([]LakeAppRow, error) {
	return append([]LakeAppRow(nil), s.rows...), s.listErr
}

func (s *fakeAppsStore) ListServices(context.Context) ([]LakeService, error) {
	return append([]LakeService(nil), s.services...), nil
}

func (s *fakeAppsStore) Close(context.Context) error {
	return nil
}

func (s *fakeAppsStore) rowForUpdate(row *LakeAppRow) *LakeAppRow {
	for i := range s.rows {
		current := &s.rows[i]
		if current.ID == row.ID && current.CustomerID == row.CustomerID && current.Generation == row.Generation && current.Desired == row.Desired {
			return current
		}
	}
	return nil
}

func (s *fakeAppsStore) UpdateAppStatus(_ context.Context, row *LakeAppRow, status LakeAppRowStatus) (bool, error) {
	if s.writeErr != nil {
		return false, s.writeErr
	}
	current := s.rowForUpdate(row)
	if current == nil {
		return false, nil
	}
	s.statusWrites++
	current.Status = status
	return true, nil
}

func (s *fakeAppsStore) UpdateAppTokenHashes(_ context.Context, row *LakeAppRow, hashes *oramav1alpha1.LakeAppTokenHashes) (bool, error) {
	if s.writeErr != nil {
		return false, s.writeErr
	}
	current := s.rowForUpdate(row)
	if current == nil {
		return false, nil
	}
	s.hashWrites++
	current.TokenHashes = hashes.DeepCopy()
	return true, nil
}

func testLakeAppRow() LakeAppRow {
	return LakeAppRow{
		ID: testAppRowID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName, Name: "sales-dashboard", Mode: "runtime",
		SourceRepository: "sales", SourceRevision: testLakeAppRevisionA, SourcePath: "apps/dashboard",
		Framework: "fastapi", Port: 8080, Replicas: 1, Auth: "lake", Desired: "running", Generation: 1,
		BindingsJSON: `{"zetta":{"scopes":["read"]},"services":["` + testPostgresID + `"]}`,
		EnvJSON:      `{"GREETING":"hello"}`, SecretEnvRefs: `[]`,
	}
}

type appsSyncHarness struct {
	t      *testing.T
	ctx    context.Context
	client client.Client
	lake   *oramav1alpha1.Lake
	store  *fakeAppsStore
	sync   *LakeAppsReconciler
	apps   *LakeAppReconciler
	key    client.ObjectKey
	events int
}

func newAppsSyncHarness(t *testing.T, kubeClient client.Client) *appsSyncHarness {
	t.Helper()
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "apps-test", UID: types.UID("test-lake")},
		Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:test", Namespace: "lake", Apps: oramav1alpha1.LakeAppsSpec{
			Enabled: true, Wildcard: "apps.example.test", RuntimeImage: "oramasearch/app-runtime:0.1.0",
		}},
	}
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: testWorkspaceName, Labels: map[string]string{workspaceLabel: testWorkspaceID},
		}},
		lake,
	} {
		if err := kubeClient.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB(lake.Name) + "-app", Namespace: "lake"},
		Data:       map[string][]byte{"uri": []byte("postgresql://test:private@localhost/lake")},
	}
	connections := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeServicesSecret(lake.Name), Namespace: "lake"},
		Data:       map[string][]byte{testPostgresID: []byte(`{"uri":"postgresql://user:private@db/app"}`)},
	}
	if err := controllerutil.SetControllerReference(lake, connections, kubeClient.Scheme()); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{credentials, connections} {
		if err := kubeClient.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	row := testLakeAppRow()
	store := &fakeAppsStore{rows: []LakeAppRow{row}, services: []LakeService{{
		ID: testPostgresID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testWorkspaceName, Name: "analytics", Desired: serviceDesiredPresent,
	}}}
	h := &appsSyncHarness{t: t, ctx: ctx, client: kubeClient, lake: lake, store: store,
		key: client.ObjectKey{Namespace: "lake", Name: lakeAppRowName(lake.Name, &row)}}
	h.sync = &LakeAppsReconciler{Client: kubeClient, Scheme: kubeClient.Scheme(), APIReader: kubeClient,
		AppStores: func(_ context.Context, uri string) (LakeAppsStore, error) {
			if uri != string(credentials.Data["uri"]) {
				t.Fatal("apps did not use the Services DSN Secret")
			}
			return store, nil
		},
	}
	h.apps = &LakeAppReconciler{Client: kubeClient, Scheme: kubeClient.Scheme(), APIReader: kubeClient,
		EventPublisher: func(_ context.Context, baseURL, token, topic string, event lakeAppEvent) error {
			if baseURL != workspaceLakeInternalURL(lake, testWorkspaceID) || topic != "app-events."+row.ID || len(token) != 64 {
				t.Error("incorrect scoped Lake event inputs")
			}
			h.events++
			return errors.New("bus unavailable")
		},
	}
	return h
}

func (h *appsSyncHarness) poll() {
	h.t.Helper()
	h.sync.pollLakeApps(h.ctx, h.lake)
}

func (h *appsSyncHarness) reconcileApp() {
	h.t.Helper()
	if _, err := h.apps.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *appsSyncHarness) app() *oramav1alpha1.LakeApp {
	h.t.Helper()
	var app oramav1alpha1.LakeApp
	if err := h.client.Get(h.ctx, h.key, &app); err != nil {
		h.t.Fatal(err)
	}
	return &app
}

func (h *appsSyncHarness) converge() {
	for range 4 {
		h.poll()
		h.reconcileApp()
	}
	h.poll()
}

func TestLakeAppsEnvtestLifecycle(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for apps sync envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	kubeClient, err := client.New(config, client.Options{Scheme: newLakeAppScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	h := newAppsSyncHarness(t, kubeClient)
	h.poll()
	h.reconcileApp()
	app := h.app()
	namespace := lakeAppNamespace(app)
	var deployment appsv1.Deployment
	deploymentKey := client.ObjectKey{Namespace: namespace, Name: lakeAppWorkloadObjectName(app)}
	if err := kubeClient.Get(h.ctx, deploymentKey, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("workload started before token registration: %v", err)
	}
	h.converge()
	assertSyncedAppDelivery(t, h)
	setNamedLakeAppDeploymentReady(t, h.ctx, kubeClient, namespace, lakeAppWorkloadObjectName(app), 1)
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Running" || !status.Ready || status.URL != "https://sales-dashboard.apps.example.test" {
		t.Fatalf("status did not flow back: %+v", status)
	}
	assertSyncedAppSteady(t, h)
	assertSyncedAppUpdateAndStop(t, h)
	assertSyncedAppRotation(t, h)
	h.store.rows[0].Desired = "deleted"
	h.store.rows[0].Generation++
	h.poll()
	if h.store.rows[0].Status.Phase == "Deleted" {
		t.Fatal("Deleted acknowledged before namespace teardown")
	}
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Deleted" || status.Ready || h.store.rows[0].TokenHashes != nil {
		t.Fatalf("deleted app not acknowledged and revoked: %+v", status)
	}
	if err := kubeClient.Get(h.ctx, h.key, &oramav1alpha1.LakeApp{}); !apierrors.IsNotFound(err) {
		t.Fatalf("LakeApp still exists: %v", err)
	}
	if err := kubeClient.Get(h.ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}); err != nil {
		t.Fatalf("app deletion removed the shared workspace namespace: %v", err)
	}
}

func assertSyncedAppDelivery(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	app := h.app()
	var tokens corev1.Secret
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}
	if err := h.client.Get(h.ctx, key, &tokens); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.store.rows[0].TokenHashes, lakeAppTokenHashes(&tokens)) || len(tokens.Data) != 3 {
		t.Fatal("registered hashes do not match generated tokens")
	}
	encoded, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens.Data {
		if len(token) != 64 || strings.Contains(string(encoded), string(token)) {
			t.Fatal("token missing or exposed outside Secrets")
		}
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: key.Namespace, Name: lakeAppWorkloadObjectName(app)}, &deployment); err != nil {
		t.Fatal(err)
	}
	env := deployment.Spec.Template.Spec.Containers[0].Env
	assertLakeAppEnv(t, env, "APP_TOKEN", "")
	assertLakeAppEnv(t, env, "EVENTS_TOKEN", "")
	for _, variable := range env {
		if variable.Name == "APP_ENTRYPOINT" {
			t.Fatal("empty entrypoint overrides app.toml")
		}
		if variable.Name == "APP_TOKEN" && variable.ValueFrom.SecretKeyRef.Name != lakeAppTokensName(app) {
			t.Fatal("source token does not use app-tokens")
		}
	}
	found := false
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			for _, item := range volume.Secret.Items {
				found = found || item.Key == testPostgresID && item.Path == "services/analytics"
			}
		}
	}
	if !found {
		t.Fatal("service connection is not mounted by service name")
	}
	var policy networkingv1.NetworkPolicy
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: key.Namespace, Name: lakeAppAccessPolicyName(app)}, &policy); err != nil {
		t.Fatal(err)
	}
	if !lakeAppPolicyHasPodSelector(policy.Spec.Egress, map[string]string{serviceIDLabel: testPostgresID}) || !lakeAppPolicyHasPodSelector(policy.Spec.Egress, map[string]string{"app.kubernetes.io/name": "lake"}) {
		t.Fatal("scoped service or Lake egress is missing")
	}
}

func assertSyncedAppSteady(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	app := h.app()
	statusWrites, hashWrites, events := h.store.statusWrites, h.store.hashWrites, h.events
	var tokens corev1.Secret
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}
	if err := h.client.Get(h.ctx, key, &tokens); err != nil {
		t.Fatal(err)
	}
	version := tokens.ResourceVersion
	h.converge()
	if h.store.statusWrites != statusWrites || h.store.hashWrites != hashWrites || h.events != events || h.app().ResourceVersion != app.ResourceVersion {
		t.Fatal("steady app rewrote status, credentials, events or its CR")
	}
	if err := h.client.Get(h.ctx, key, &tokens); err != nil || tokens.ResourceVersion != version {
		t.Fatal("steady tokens changed")
	}
}

func assertSyncedAppUpdateAndStop(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	h.store.rows[0].SourceRevision = testLakeAppRevisionB
	h.store.rows[0].SourceEntrypoint = "main:app"
	h.store.rows[0].Generation++
	h.poll()
	if h.store.rows[0].Status.Ready {
		t.Fatal("old Ready status acknowledged a new row generation")
	}
	h.converge()
	app := h.app()
	if app.Spec.Source.Entrypoint != "main:app" || app.Status.ObservedRevision != testLakeAppRevisionB {
		t.Fatal("source update did not reach the app")
	}
	h.store.rows[0].Desired = "stopped"
	h.store.rows[0].Generation++
	h.converge()
	if h.store.rows[0].Status.Phase == "Stopped" {
		t.Fatal("Stopped acknowledged before the Deployment observed zero replicas")
	}
	setNamedLakeAppDeploymentReady(t, h.ctx, h.client, lakeAppNamespace(app), lakeAppWorkloadObjectName(app), 0)
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Stopped" || status.Ready || *h.app().Spec.Replicas != 0 {
		t.Fatalf("stopped state = %+v", status)
	}
	h.store.rows[0].Desired = "running"
	h.store.rows[0].SourceRevision = testLakeAppRevisionA
	h.store.rows[0].Generation++
	h.converge()
	if h.app().Status.ObservedRevision != testLakeAppRevisionA {
		t.Fatal("rollback did not return to the original source revision")
	}
}

func assertSyncedAppRotation(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	app := h.app()
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}
	var tokens corev1.Secret
	if err := h.client.Get(h.ctx, key, &tokens); err != nil {
		t.Fatal(err)
	}
	before := h.store.rows[0].TokenHashes.DeepCopy()
	if err := h.client.Delete(h.ctx, &tokens); err != nil {
		t.Fatal(err)
	}
	h.reconcileApp()
	h.store.writeErr = errors.New("database unavailable")
	h.poll()
	if !reflect.DeepEqual(before, h.store.rows[0].TokenHashes) {
		t.Fatal("failed registration changed hashes")
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: key.Namespace, Name: lakeAppWorkloadObjectName(app)}, &deployment); !apierrors.IsNotFound(err) {
		t.Fatal("rotation allowed a workload before registering the new hashes")
	}
	h.store.writeErr = nil
	h.converge()
	if reflect.DeepEqual(before, h.store.rows[0].TokenHashes) {
		t.Fatal("rotation did not replace the hashes")
	}
}

func TestLakeAppsOrphansAndReadFailures(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	standalone := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "lake"}, Spec: app.Spec}
	standalone.Spec.Sync = nil
	if err := c.Create(h.ctx, standalone); err != nil {
		t.Fatal(err)
	}
	h.store.rows = nil
	h.store.listErr = errors.New("partial list")
	h.poll()
	if !h.app().DeletionTimestamp.IsZero() {
		t.Fatal("a failed list deleted an app")
	}
	h.store.listErr = nil
	h.poll()
	if h.app().DeletionTimestamp.IsZero() {
		t.Fatal("orphan was not deleted")
	}
	if err := c.Get(h.ctx, client.ObjectKeyFromObject(standalone), standalone); err != nil {
		t.Fatal("standalone app was deleted")
	}
}

func TestLakeAppsRejectForeignBindingsAndNames(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Apps: oramav1alpha1.LakeAppsSpec{Wildcard: "apps.example.test"}}}
	for _, name := range []string{"foreign customer", "missing service", "unsafe filename", "outside host", "invalid env", "mutable image"} {
		t.Run(name, func(t *testing.T) {
			row := testLakeAppRow()
			services := []LakeService{{ID: testPostgresID, CustomerID: row.CustomerID, Name: "db", Desired: "present"}}
			switch name {
			case "foreign customer":
				services[0].CustomerID = "other"
			case "missing service":
				services = nil
			case "unsafe filename":
				services[0].Name = "../token"
			case "outside host":
				row.Host = "other.example.test"
			case "invalid env":
				row.EnvJSON = `{"PRIVATE":{"token":"do-not-log"}}`
			case "mutable image":
				row.Mode, row.Image = "image", "example.test/app:latest"
			}
			if _, err := lakeAppRowSpec(lake, &row, services); err == nil || strings.Contains(err.Error(), "do-not-log") {
				t.Fatal("invalid row accepted or secret echoed in validation")
			}
		})
	}
}

func TestLakeAppsRefuseForeignCRForUpdateAndDelete(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	h.poll()
	app := h.app()
	app.OwnerReferences = nil
	if err := c.Update(h.ctx, app); err != nil {
		t.Fatal(err)
	}
	version := app.ResourceVersion
	for _, desired := range []string{"running", "deleted"} {
		h.store.rows[0].Desired = desired
		h.store.rows[0].Generation++
		h.poll()
		if h.store.rows[0].Status.Phase != "Failed" || h.app().ResourceVersion != version || !h.app().DeletionTimestamp.IsZero() {
			t.Fatalf("foreign app changed during %s", desired)
		}
	}
}
