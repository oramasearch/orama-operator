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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	rows     []LakeAppRow
	services []LakeService
	// stores and storeMounts are the stores apps mount (lake#666).
	stores             []LakeStore
	storeMounts        []LakeStoreMount
	storeMountsMissing bool
	storeMountsErr     error
	listErr            error
	writeErr           error
	statusWrites       int
	hashWrites         int
}

func (s *fakeAppsStore) ListApps(context.Context) ([]LakeAppRow, error) {
	return append([]LakeAppRow(nil), s.rows...), s.listErr
}

func (s *fakeAppsStore) ListServices(context.Context) ([]LakeService, error) {
	return append([]LakeService(nil), s.services...), nil
}

func (s *fakeAppsStore) ListStores(context.Context) ([]LakeStore, error) {
	return append([]LakeStore(nil), s.stores...), nil
}

func (s *fakeAppsStore) ListStoreMounts(context.Context) ([]LakeStoreMount, error) {
	if s.storeMountsMissing {
		return nil, errStoreMountsTableMissing
	}
	return append([]LakeStoreMount(nil), s.storeMounts...), s.storeMountsErr
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
		EnvJSON:      `{"GREETING":"hello"}`, SecretEnvRefs: `{}`,
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
		Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:test", Namespace: "lake", Host: "lake.example.test", PullSecretName: "docker-pull", Apps: oramav1alpha1.LakeAppsSpec{
			Wildcard: "apps.example.test", RuntimeImage: "oramasearch/app-runtime:0.1.0",
		}},
	}
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: testWorkspaceName, Labels: map[string]string{workspaceLabel: testWorkspaceID, workspaceCustomerLabel: testServiceCustomer},
		}},
		lake,
	} {
		if err := kubeClient.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeRoleSecret(lake.Name, lakeOperatorRole), Namespace: "lake"},
		Data:       map[string][]byte{"uri": []byte("postgresql://test:private@localhost/lake")},
	}
	connections := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeServicesSecret(lake.Name), Namespace: "lake"},
		Data:       map[string][]byte{testPostgresID: []byte(`{"uri":"postgresql://user:private@db/app"}`)},
	}
	pullSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-pull", Namespace: "lake"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://index.docker.io/v1/":{"username":"hub","password":"hub-token"}}}`),
		},
	}
	if err := controllerutil.SetControllerReference(lake, connections, kubeClient.Scheme()); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{credentials, connections, pullSecret} {
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
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{crdPath}, CRDs: certManagerTestCRDs(), ErrorIfCRDPathMissing: true}
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
	assertSyncedAppSecretEnvironment(t, h)
	assertSyncedAppLakeSecretEnvironment(t, h)
	assertSyncedAppStoreMounts(t, h)
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

// operator#492: a row that names a Kubernetes Secret, in either the object or the legacy array form,
// is refused with a reason, and nothing from the Lake namespace reaches the app, the instance key
// included. A row with lake-secret:// references only is the one accepted form.
func assertSyncedAppSecretEnvironment(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	instanceKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-instance-kek", Namespace: h.app().Namespace},
		Data:       map[string][]byte{"kek": []byte("instance-key")},
	}
	if err := h.client.Create(h.ctx, instanceKey); err != nil {
		t.Fatal(err)
	}
	for _, refs := range []string{
		`{"KEK":"lake-prod-instance-kek/kek"}`,
		`{"KEK":"lake-prod-instance-kek"}`,
		`["lake-prod-instance-kek"]`,
	} {
		h.store.rows[0].SecretEnvRefs = refs
		h.store.rows[0].Generation++
		h.converge()
		status := h.store.rows[0].Status
		if status.Phase != "Failed" || !strings.Contains(status.Message, "operator#492") {
			t.Fatalf("%s: status = %+v, want Failed naming operator#492", refs, status)
		}
		app := h.app()
		if len(app.Spec.SecretEnv) != 0 || len(app.Spec.SecretRefs) != 0 {
			t.Fatalf("%s: refused row still reached the LakeApp: env %+v, refs %+v", refs, app.Spec.SecretEnv, app.Spec.SecretRefs)
		}
		var secrets corev1.SecretList
		if err := h.client.List(h.ctx, &secrets, client.InNamespace(lakeAppNamespace(app))); err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets.Items {
			if string(secret.Data["kek"]) == "instance-key" {
				t.Fatalf("%s: the instance key was copied into the app namespace as %s", refs, secret.Name)
			}
		}
	}
	h.store.rows[0].SecretEnvRefs = `{}`
	h.store.rows[0].Generation++
	h.converge()
	if status := h.store.rows[0].Status; status.Phase == "Failed" {
		t.Fatalf("app still failed after its references were cleared: %+v", status)
	}
}

// A lake secret reference (lake#625): the app's LakeSealedSecret in the Lake namespace, the app
// held back until lake delivers, the field copied into the app namespace, a new version rolling the
// pods, and the delivery removed with the reference.
func assertSyncedAppLakeSecretEnvironment(t *testing.T, h *appsSyncHarness) {
	t.Helper()
	const mount = "6f1c2a4e-0d3b-4c55-9a7e-1b2c3d4e5f60"
	h.store.rows[0].SecretEnvRefs = `{"DB_PASSWORD":"lake-secret://` + mount + `/payments/db#password"}`
	h.store.rows[0].Generation++
	h.converge()
	app := h.app()
	if len(app.Spec.SecretEnv) != 1 || app.Spec.SecretEnv[0].LakeSecret == nil {
		t.Fatalf("lake secret environment = %+v", app.Spec.SecretEnv)
	}
	reference := *app.Spec.SecretEnv[0].LakeSecret
	if reference.Mount != mount || reference.Name != "payments/db" || reference.Field != "password" {
		t.Fatalf("lake secret reference = %+v", reference)
	}
	produced := app.Spec.SecretEnv[0].SecretKeyRef.Name
	var sealed oramav1alpha1.LakeSealedSecret
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: app.Namespace, Name: produced}, &sealed); err != nil {
		t.Fatalf("no LakeSealedSecret for the lake secret: %v", err)
	}
	if sealed.Spec.Source.Type != "lake" || sealed.Spec.Source.Lake == nil ||
		sealed.Spec.Source.Lake.Mount != mount || sealed.Spec.Source.Lake.Name != "payments/db" ||
		sealed.Spec.Source.Lake.Version != nil || sealed.Spec.Template.Name != produced || len(sealed.Spec.Data) != 0 {
		t.Fatalf("LakeSealedSecret spec = %+v", sealed.Spec)
	}
	ownedByApp := false
	for _, owner := range sealed.OwnerReferences {
		ownedByApp = ownedByApp || (owner.Kind == "LakeApp" && owner.Name == app.Name)
	}
	if !ownedByApp {
		t.Fatalf("LakeSealedSecret owners = %+v", sealed.OwnerReferences)
	}
	if ready := apimeta.FindStatusCondition(app.Status.Conditions, "Ready"); ready == nil ||
		ready.Reason != "SecretDeliveryPending" || app.Status.Phase != oramav1alpha1.LakeAppPhaseProgressing {
		t.Fatalf("app before delivery: phase=%s conditions=%+v", app.Status.Phase, app.Status.Conditions)
	}

	// The LakeSealedSecret reconciler's produced Secret (its own tests cover the minting).
	delivered := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: produced, Namespace: app.Namespace},
		Data:       map[string][]byte{"password": []byte("v1-password"), "user": []byte("app")},
	}
	if err := h.client.Create(h.ctx, delivered); err != nil {
		t.Fatal(err)
	}
	h.converge()
	app = h.app()
	var deployment appsv1.Deployment
	deploymentKey := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppWorkloadObjectName(app)}
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	copyName := lakeAppEnvSecretName(app, produced)
	assertLakeAppSecretEnv(t, deployment.Spec.Template.Spec.Containers[0].Env, "DB_PASSWORD", copyName, "password")
	var copied corev1.Secret
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: lakeAppNamespace(app), Name: copyName}, &copied); err != nil {
		t.Fatal(err)
	}
	if len(copied.Data) != 1 || string(copied.Data["password"]) != "v1-password" {
		t.Fatalf("copied lake secret = %#v", copied.Data)
	}
	before := deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation]

	// A new version delivered: the copy and the pods' credential digest follow.
	delivered.Data["password"] = []byte("v2-password")
	if err := h.client.Update(h.ctx, delivered); err != nil {
		t.Fatal(err)
	}
	h.converge()
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: lakeAppNamespace(app), Name: copyName}, &copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["password"]) != "v2-password" || deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation] == before {
		t.Fatalf("rotation did not reach the app: copy=%q digest unchanged=%v", copied.Data["password"],
			deployment.Spec.Template.Annotations[lakeAppCredentialHashAnnotation] == before)
	}

	// The reference removed: the delivery goes with it.
	h.store.rows[0].SecretEnvRefs = `{}`
	h.store.rows[0].Generation++
	h.converge()
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: app.Namespace, Name: produced}, &sealed); !apierrors.IsNotFound(err) {
		t.Fatalf("LakeSealedSecret kept after its reference was removed: %v", err)
	}
	if err := h.client.Delete(h.ctx, delivered); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func assertLakeAppSecretEnv(t *testing.T, env []corev1.EnvVar, name, secretName, key string) {
	t.Helper()
	for _, variable := range env {
		if variable.Name != name {
			continue
		}
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s is not sourced from a Secret key", name)
		}
		selector := variable.ValueFrom.SecretKeyRef
		if selector.Name != secretName || selector.Key != key {
			t.Fatalf("%s selector = %s/%s, want %s/%s", name, selector.Name, selector.Key, secretName, key)
		}
		return
	}
	t.Fatalf("secret environment variable %s not found", name)
}

func TestLakeAppRowSecretEnvironmentContract(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantSecretEnv []oramav1alpha1.LakeAppSecretEnvironmentVariable
		wantLegacy    []string
		wantError     string
	}{
		{name: "empty"},
		{name: "null", raw: "null"},
		{name: "empty object", raw: `{}`},
		{name: "Kubernetes Secret with a key", raw: `{"API_KEY":"svc-creds/token"}`, wantError: "operator#492"},
		{name: "Kubernetes Secret by name", raw: `{"DB_URL":"db-secret"}`, wantError: "operator#492"},
		{name: "the instance key", raw: `{"KEK":"lake-prod-instance-kek/kek"}`, wantError: "names a Kubernetes Secret"},
		{
			name: "lake secret reference",
			raw:  `{"DB_PASSWORD":"lake-secret://m-1/payments/db#prod#password"}`,
			wantSecretEnv: []oramav1alpha1.LakeAppSecretEnvironmentVariable{{
				Name: "DB_PASSWORD",
				SecretKeyRef: oramav1alpha1.LakeAppSecretKeyReference{
					Name: lakeAppLakeSecretName(nil, oramav1alpha1.LakeAppLakeSecretReference{Mount: "m-1", Name: "payments/db#prod"}),
					Key:  "password",
				},
				LakeSecret: &oramav1alpha1.LakeAppLakeSecretReference{Mount: "m-1", Name: "payments/db#prod", Field: "password"},
			}},
		},
		{name: "lake secret without a field", raw: `{"GOOD":"lake-secret://m-1/db"}`, wantError: "lake-secret://"},
		{name: "lake secret with a bad field", raw: `{"GOOD":"lake-secret://m-1/db#a b"}`, wantError: "not a Secret key"},
		{name: "legacy array", raw: `["z-secret","a-secret","z-secret"]`, wantError: "legacy array"},
		{name: "invalid environment name", raw: `{"9BAD":"lake-secret://m-1/db#password"}`, wantError: "environment name"},
		{name: "nested object value", raw: `{"GOOD":{"secret":"good-secret"}}`, wantError: "must be an object"},
		{name: "invalid shape", raw: `true`, wantError: "must be an object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := oramav1alpha1.LakeAppSpec{}
			err := lakeAppRowSecretEnvironment(test.raw, &spec)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want message containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(spec.SecretEnv, test.wantSecretEnv) || !reflect.DeepEqual(spec.SecretRefs, test.wantLegacy) {
				t.Fatalf("secret environment = %+v, legacy = %+v", spec.SecretEnv, spec.SecretRefs)
			}
		})
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
	if !reflect.DeepEqual(h.store.rows[0].TokenHashes, lakeAppTokenHashes(&tokens)) || len(tokens.Data) != 4 {
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
	if len(deployment.Spec.Template.Spec.ImagePullSecrets) != 1 || deployment.Spec.Template.Spec.ImagePullSecrets[0].Name != lakeAppPullName(app) {
		t.Fatalf("app pod pull Secret = %+v", deployment.Spec.Template.Spec.ImagePullSecrets)
	}
	assertSyncedAppPullSecret(t, h, app, &tokens)
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
	var ingress networkingv1.Ingress
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: key.Namespace, Name: lakeAppWorkloadObjectName(app)}, &ingress); err != nil {
		t.Fatal(err)
	}
	wantAuthURL := "http://lake-apps-test.lake.svc.cluster.local:80/w/" + testWorkspaceID + "/v1/apps/auth"
	if ingress.Annotations["nginx.ingress.kubernetes.io/auth-url"] != wantAuthURL {
		t.Fatalf("auth-url = %q, want %q", ingress.Annotations["nginx.ingress.kubernetes.io/auth-url"], wantAuthURL)
	}
	if ingress.Annotations["nginx.ingress.kubernetes.io/auth-always-set-cookie"] != "true" {
		t.Fatalf("auth-always-set-cookie missing: %#v", ingress.Annotations)
	}
}

func assertSyncedAppPullSecret(t *testing.T, h *appsSyncHarness, app *oramav1alpha1.LakeApp, tokens *corev1.Secret) {
	t.Helper()
	var pullSecret corev1.Secret
	key := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppPullName(app)}
	if err := h.client.Get(h.ctx, key, &pullSecret); err != nil {
		t.Fatal(err)
	}
	if pullSecret.Type != corev1.SecretTypeDockerConfigJson || len(pullSecret.Data) != 1 {
		t.Fatalf("merged pull Secret type/key count = %q/%d", pullSecret.Type, len(pullSecret.Data))
	}
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(pullSecret.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatal(err)
	}
	if auth := config.Auths["https://index.docker.io/v1/"]; auth.Username != "hub" || auth.Password != "hub-token" {
		t.Fatalf("Docker Hub auth was not preserved: %+v", auth)
	}
	if auth := config.Auths[h.lake.Spec.Host]; auth.Username != "app" || auth.Password != string(tokens.Data["pull"]) {
		t.Fatal("Lake registry auth does not use the generated pull token")
	}
}

func TestLakeAppAuthSigninUsesLakePublicHost(t *testing.T) {
	app := &oramav1alpha1.LakeApp{Spec: oramav1alpha1.LakeAppSpec{Sync: &oramav1alpha1.LakeAppSync{ID: testWorkspaceID}}}
	lake := &oramav1alpha1.Lake{}
	if got := lakeAppAuthSignin(lake, app); got != "" {
		t.Fatalf("signin without public ingress = %q, want empty", got)
	}
	lake.Spec.IngressEnabled = true
	lake.Spec.Host = "lake.example.test"
	want := "https://lake.example.test/apps/authorize?app=" + testWorkspaceID + "&rd=https://$host$escaped_request_uri"
	if got := lakeAppAuthSignin(lake, app); got != want {
		t.Fatalf("signin = %q, want %q", got, want)
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
	app := h.app()
	deploymentKey := client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppWorkloadObjectName(app)}
	setNamedLakeAppDeploymentReady(t, h.ctx, h.client, deploymentKey.Namespace, deploymentKey.Name, 1)
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Running" || !status.Ready {
		t.Fatalf("app was not ready before rollout checks: %+v", status)
	}

	h.store.rows[0].Host = "sales-dashboard-v2.apps.example.test"
	h.store.rows[0].Generation++
	h.converge()
	app = h.app()
	var deployment appsv1.Deployment
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if app.Generation <= deployment.Generation {
		t.Fatalf("test setup did not separate generations: app=%d Deployment=%d", app.Generation, deployment.Generation)
	}
	if status := h.store.rows[0].Status; status.Phase != "Running" || !status.Ready || status.URL != "https://sales-dashboard-v2.apps.example.test" {
		t.Fatalf("ingress-only update lost ready status: %+v", status)
	}

	h.store.rows[0].SourceRevision = testLakeAppRevisionB
	h.store.rows[0].SourceEntrypoint = "main:app"
	h.store.rows[0].Generation++
	h.poll()
	if h.store.rows[0].Status.Ready {
		t.Fatal("old Ready status acknowledged a new row generation")
	}
	h.converge()
	app = h.app()
	if app.Spec.Source.Entrypoint != "main:app" || app.Status.ObservedRevision != testLakeAppRevisionB {
		t.Fatal("source update did not reach the app")
	}
	if err := h.client.Get(h.ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Status.ObservedGeneration >= deployment.Generation {
		t.Fatal("rolled Deployment was ready before its new generation was observed")
	}
	setNamedLakeAppDeploymentReady(t, h.ctx, h.client, deploymentKey.Namespace, deploymentKey.Name, 1)
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Running" || !status.Ready || status.URL != "https://sales-dashboard-v2.apps.example.test" {
		t.Fatalf("rolled app status = %+v", status)
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
	setNamedLakeAppDeploymentReady(t, h.ctx, h.client, deploymentKey.Namespace, deploymentKey.Name, 1)
	h.reconcileApp()
	h.poll()
	if status := h.store.rows[0].Status; status.Phase != "Running" || !status.Ready || status.URL != "https://sales-dashboard-v2.apps.example.test" {
		t.Fatalf("restarted app status = %+v", status)
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
	oldPullToken := string(tokens.Data["pull"])
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
	if err := h.client.Get(h.ctx, key, &tokens); err != nil {
		t.Fatal(err)
	}
	if string(tokens.Data["pull"]) == oldPullToken {
		t.Fatal("rotation retained the old pull token")
	}
	assertSyncedAppPullSecret(t, h, app, &tokens)
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

func TestLakeAppRowAuthNoneIsPublic(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}, Spec: oramav1alpha1.LakeSpec{Apps: oramav1alpha1.LakeAppsSpec{Wildcard: "apps.example.test"}}}
	row := testLakeAppRow()
	services := []LakeService{{ID: testPostgresID, CustomerID: row.CustomerID, WorkspaceID: row.WorkspaceID, Name: "db", Desired: "present"}}
	row.Auth = "none"
	spec, err := lakeAppRowSpec(lake, &row, services)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Auth != oramav1alpha1.LakeAppAuthPublic {
		t.Fatalf("auth = %q, want public", spec.Auth)
	}
	row.Auth = "anyone"
	if _, err := lakeAppRowSpec(lake, &row, services); err == nil {
		t.Fatal("unknown auth accepted")
	}
}
