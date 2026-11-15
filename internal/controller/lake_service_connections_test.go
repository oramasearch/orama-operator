/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const testConnectionURL = "https://orama-operator-control-plane.orama-system.svc:8090"

func onDemandManifest(image string) *oramav1alpha1.LakeImageManifest {
	return &oramav1alpha1.LakeImageManifest{Image: image, Supported: true, ServiceConnectionsOnDemand: true}
}

func TestLakeManifestReportsServiceConnectionsOnDemand(t *testing.T) {
	document := func(environment string) lakeManifestDocument {
		var parsed lakeManifestDocument
		raw := `{"manifest_version":1,"name":"lake","version":"v1","revision":"abc",
			"schema":{"version":"sha256:000000000000","latest_migration":"0001_a","guard_table":"a_guard","guard_tables":["a_guard"],"migrations":["0001_a"]},
			"migrate":{"args":["migrate"],"wait_flag":"--wait-seconds","database_env":"LAKE_CONTROL_DB_URI"},
			"stripe":{"webhook_events":["invoice.paid"],"products":[{"id":"lake_seat","name":"Seat"}]},
			"environment":` + environment + `}`
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	reads, err := lakeManifestFromDocument(document(`[{"name":"LAKE_SERVICE_DIR"},{"name":"LAKE_SERVICE_CONNECTION_URL"}]`))
	if err != nil || !reads.ServiceConnectionsOnDemand {
		t.Fatalf("an image listing %s is not on demand: %+v %v", lakeServiceConnectionURLEnv, reads, err)
	}
	mounts, err := lakeManifestFromDocument(document(`[{"name":"LAKE_SERVICE_DIR"}]`))
	if err != nil || mounts.ServiceConnectionsOnDemand {
		t.Fatalf("an image that only reads the mount is on demand: %+v %v", mounts, err)
	}
}

func TestLakeServiceConnectionsOnDemandOnlyForTheCurrentImage(t *testing.T) {
	lake := &oramav1alpha1.Lake{Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:new"}}
	if !lakeServiceConnectionsOnDemand(lake, onDemandManifest("oramasearch/lake:new"), testConnectionURL) {
		t.Fatal("the current on-demand image is not on demand")
	}
	for name, manifest := range map[string]*oramav1alpha1.LakeImageManifest{
		"no manifest":         nil,
		"another image":       onDemandManifest("oramasearch/lake:old"),
		"image reads a mount": {Image: "oramasearch/lake:new", Supported: true},
		"unsupported":         {Image: "oramasearch/lake:new", ServiceConnectionsOnDemand: true},
	} {
		if lakeServiceConnectionsOnDemand(lake, manifest, testConnectionURL) {
			t.Errorf("%s: on demand", name)
		}
	}
	if lakeServiceConnectionsOnDemand(lake, onDemandManifest("oramasearch/lake:new"), "") {
		t.Error("on demand without a control plane to ask")
	}
}

func TestOnDemandLakePodMountsNoServiceCredential(t *testing.T) {
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	optional := true
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "lake"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "lake",
				Env:  []corev1.EnvVar{{Name: "LAKE_PORT", Value: "8080"}, {Name: lakeServiceDirEnv, Value: lakeServicesMountPath}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: lakeStoreVolume, MountPath: lakeStorePath},
					{Name: lakeServicesVolume, MountPath: lakeServicesMountPath, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: lakeStoreVolume},
				{Name: lakeServicesVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: lakeServicesSecret(lake.Name), Optional: &optional,
				}}},
			},
		},
	}
	applyLakeServiceConnectionsOnDemand(lake, &template, testConnectionURL)
	for _, volume := range template.Spec.Volumes {
		if volume.Secret != nil && volume.Secret.SecretName == lakeServicesSecret(lake.Name) {
			t.Fatal("the aggregate services Secret is still mounted")
		}
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil && source.Secret.Name != lakeConnectionsSecret(lake.Name) {
					t.Fatalf("the projected volume carries Secret %s", source.Secret.Name)
				}
			}
		}
	}
	env := map[string]string{}
	for _, variable := range template.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	if _, found := env[lakeServiceDirEnv]; found {
		t.Fatal("LAKE_SERVICE_DIR is still set")
	}
	if env[lakeServiceConnectionURLEnv] != testConnectionURL+lakeServiceConnectionPath ||
		env[lakeServiceConnectionTokenFileEnv] != lakeServiceConnectionMountPath+"/token" ||
		env[lakeServiceConnectionCAFileEnv] != lakeServiceConnectionMountPath+"/ca.crt" || env["LAKE_PORT"] != "8080" {
		t.Fatalf("on-demand env = %v", env)
	}
	if template.Labels[serviceConnectionClientPodLabel] != "true" || template.Labels["app.kubernetes.io/name"] != "lake" {
		t.Fatalf("pod labels = %v", template.Labels)
	}
}

func TestLakeDeploymentMountsServicesUntilTheRolloutCompletes(t *testing.T) {
	replicas := int32(2)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 3, UpdatedReplicas: 1},
	}
	if !lakeDeploymentMountsServices(deployment) {
		t.Fatal("a rollout with old pods left is treated as done")
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 2}
	if lakeDeploymentMountsServices(deployment) {
		t.Fatal("a finished on-demand rollout still counts as mounting")
	}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: lakeServicesVolume}}
	if !lakeDeploymentMountsServices(deployment) || !lakeDeploymentMountsServices(nil) {
		t.Fatal("a template that mounts the Secret, or no Deployment, is treated as not mounting")
	}
}

func serviceConnectionFixture(t *testing.T) (client.Client, *oramav1alpha1.Lake, *oramav1alpha1.Lake) {
	t.Helper()
	scheme := deploymentControlPlaneScheme(t)
	lakeA := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "eu", UID: "uid-eu"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake-eu"}}
	lakeB := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "us", UID: "uid-us"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake-us"}}
	objects := []client.Object{lakeA, lakeB}
	for _, lake := range []*oramav1alpha1.Lake{lakeA, lakeB} {
		token := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lakeConnectionsSecret(lake.Name), Namespace: lake.Spec.Namespace},
			Data:       map[string][]byte{"token": []byte(lakeServiceConnectionTokenPrefix + lake.Name)},
		}
		if err := controllerutil.SetControllerReference(lake, token, scheme); err != nil {
			t.Fatal(err)
		}
		deploy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lakeDeploySecret(lake.Name), Namespace: lake.Spec.Namespace},
			Data:       map[string][]byte{"token": []byte("lake_deploy_" + lake.Name)},
		}
		workspace := "ws-" + lake.Name
		objects = append(objects, token, deploy,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspace, Labels: map[string]string{workspaceLakeLabel: lake.Name}}},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: serviceConnectionSecret(lake.Name, testConnectionSlug(lake.Name)), Namespace: workspace,
					Labels: serviceConnectionLabels(lake, testConnectionSlug(lake.Name), "id-"+lake.Name),
				},
				Data: map[string][]byte{serviceConnectionKey: []byte(`{"kind":"postgres","password":"` + lake.Name + `-secret"}`)},
			},
		)
	}
	// A connection Secret labelled for eu in a namespace that is not one of eu's workspaces.
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: serviceConnectionSecret("eu", "svc-111111111111"), Namespace: "elsewhere",
				Labels: serviceConnectionLabels(lakeA, "svc-111111111111", "id-planted"),
			},
			Data: map[string][]byte{serviceConnectionKey: []byte(`{"kind":"postgres"}`)},
		},
	)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), lakeA, lakeB
}

func callServiceConnection(t *testing.T, handler http.Handler, method, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, lakeServiceConnectionPath, strings.NewReader(body))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestServiceConnectionAnswersOnlyTheCallersOwnWorkspaceService(t *testing.T) {
	kube, _, _ := serviceConnectionFixture(t)
	handler := (&DeploymentControlPlane{Client: kube}).Handler()

	ok := callServiceConnection(t, handler, http.MethodPost, "lake_conn_eu", `{"service":"svc-00000000000e"}`)
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "eu-secret") || ok.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("own service = %d %s", ok.Code, ok.Body.String())
	}
	for name, call := range map[string]struct {
		method, bearer, body string
		status               int
	}{
		"another Lake's service":         {http.MethodPost, "lake_conn_eu", `{"service":"svc-00000000000f"}`, http.StatusNotFound},
		"a Secret outside its workspace": {http.MethodPost, "lake_conn_eu", `{"service":"svc-111111111111"}`, http.StatusNotFound},
		"the deploy token":               {http.MethodPost, "lake_deploy_eu", `{"service":"svc-00000000000e"}`, http.StatusUnauthorized},
		"an unknown credential":          {http.MethodPost, "lake_conn_nobody", `{"service":"svc-00000000000e"}`, http.StatusUnauthorized},
		"no credential":                  {http.MethodPost, "", `{"service":"svc-00000000000e"}`, http.StatusUnauthorized},
		"a path for a slug":              {http.MethodPost, "lake_conn_eu", `{"service":"../lake-eu-services"}`, http.StatusBadRequest},
		"an extra field":                 {http.MethodPost, "lake_conn_eu", `{"service":"svc-00000000000e","lake":"us"}`, http.StatusBadRequest},
		"a GET":                          {http.MethodGet, "lake_conn_eu", ``, http.StatusMethodNotAllowed},
	} {
		response := callServiceConnection(t, handler, call.method, call.bearer, call.body)
		if response.Code != call.status || strings.Contains(response.Body.String(), "secret\"") {
			t.Errorf("%s = %d %s, want %d", name, response.Code, response.Body.String(), call.status)
		}
	}
}

func TestServiceConnectionSecretsLiveInTheirWorkspaceNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := deploymentControlPlaneScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "eu", UID: "uid-eu"}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: serviceConnectionSecret("eu", "svc-cccccccccccc"), Namespace: "ws-c"}}
	stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: serviceConnectionSecret("eu", "svc-dddddddddddd"), Namespace: "ws-d",
		Labels: serviceConnectionLabels(lake, "svc-dddddddddddd", "id-d"),
	}}
	moved := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: serviceConnectionSecret("eu", "svc-aaaaaaaaaaaa"), Namespace: "ws-old",
		Labels: serviceConnectionLabels(lake, "svc-aaaaaaaaaaaa", "id-a"),
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, foreign, stale, moved).Build()
	r := &LakeReconciler{Client: kube, Scheme: scheme}
	kept := map[string]bool{"svc-aaaaaaaaaaaa": true, "svc-bbbbbbbbbbbb": true, "svc-cccccccccccc": true}
	written, err := r.reconcileServiceConnectionSecrets(ctx, lake, func(slug string) bool { return kept[slug] },
		map[string][]byte{
			"svc-aaaaaaaaaaaa": []byte(`{"a":1}`),
			"svc-cccccccccccc": []byte(`{"c":1}`),
		},
		map[string]serviceConnectionTarget{
			"svc-aaaaaaaaaaaa": {namespace: "ws-a", id: "id-a"},
			"svc-cccccccccccc": {namespace: "ws-c", id: "id-c"},
		})
	if err == nil || !written["svc-aaaaaaaaaaaa"] || written["svc-cccccccccccc"] {
		t.Fatalf("written = %v, err %v: a foreign Secret of the same name must be refused", written, err)
	}
	got, err := serviceConnectionByID(ctx, kube, lake, "ws-a", "id-a")
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("ws-a connection = %s, %v", got, err)
	}
	if got, _ := serviceConnectionByID(ctx, kube, lake, "ws-b", "id-a"); got != nil {
		t.Fatal("a connection is found from another workspace namespace")
	}
	for _, gone := range []*corev1.Secret{stale, moved} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(gone), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s was not removed: %v", gone.Namespace, gone.Name, err)
		}
	}
	if err := removeServiceConnectionSecrets(ctx, kube, lake, "svc-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if got, _ := serviceConnectionByID(ctx, kube, lake, "ws-a", "id-a"); got != nil {
		t.Fatal("a removed service keeps its connection Secret")
	}
}

func testConnectionSlug(lake string) string {
	return map[string]string{"eu": "svc-00000000000e", "us": "svc-00000000000f"}[lake]
}
