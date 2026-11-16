// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The DuckDB and Polars kinds (lake#195): lake-duckdb and lake-polars, run by the Orama operator
// through the wrapper machinery like SQLite. They go through every catalog test (build, ready
// gating and credential, deletion order, foreign objects and Secrets, old volumes, the quota) via
// duckdbCatalogCase and polarsCatalogCase; the tests here add what is their own.

const (
	testDuckDBID   = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	testDuckDBSlug = "svc-dddddddddddd"
	testDuckDBName = "prod-svc-dddddddddddd"
	testPolarsID   = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	testPolarsSlug = "svc-eeeeeeeeeeee"
	testPolarsName = "prod-svc-eeeeeeeeeeee"
)

func duckdbCatalogCase() catalogCase {
	return catalogCase{
		kind: serviceKindDuckDB, id: testDuckDBID, slug: testDuckDBSlug,
		objects:    []string{"StatefulSet/" + testDuckDBName, "Service/" + testDuckDBName},
		checkBuild: checkDuckDBBuild,
		waiting:    "waiting for the lake-duckdb pod",
		markPartlyReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(1), "readyReplicas": int64(0)})
		},
		partWaiting: "waiting for the lake-duckdb pod",
		markReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(1), "readyReplicas": int64(1)})
		},
		passwordSecret: testDuckDBName + "-credentials", passwordKey: "token",
		endpoint: "http://prod-svc-dddddddddddd.prod-ws-123e4567e89b.svc.cluster.local:8080",
		credential: func(token string) string {
			return `{"kind":"duckdb","endpoint":"http://prod-svc-dddddddddddd.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"` + token + `","database":"main"}`
		},
		claimLabels: map[string]string{"app.kubernetes.io/name": "lake-duckdb", "app.kubernetes.io/instance": testDuckDBName, serviceIDLabel: testDuckDBID},
	}
}

// polarsCatalogCase: the catalog tests ask for three instances, so three workers.
func polarsCatalogCase() catalogCase {
	return catalogCase{
		kind: serviceKindPolars, id: testPolarsID, slug: testPolarsSlug,
		objects:    []string{"StatefulSet/" + testPolarsName, "Service/" + testPolarsName},
		checkBuild: checkPolarsBuild,
		waiting:    "waiting for the lake-polars pods",
		markPartlyReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(3), "readyReplicas": int64(2)})
		},
		partWaiting: "waiting for the lake-polars pods",
		markReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(3), "readyReplicas": int64(3)})
		},
		passwordSecret: testPolarsName + "-credentials", passwordKey: "token",
		endpoint: "http://prod-svc-eeeeeeeeeeee.prod-ws-123e4567e89b.svc.cluster.local:8080",
		credential: func(token string) string {
			return `{"kind":"polars","endpoint":"http://prod-svc-eeeeeeeeeeee.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"` + token + `"}`
		},
		claimLabels: map[string]string{"app.kubernetes.io/name": "lake-polars", "app.kubernetes.io/instance": testPolarsName, serviceIDLabel: testPolarsID},
	}
}

// wrapperExpectation is what checkWrapperBuild requires of one kind's objects.
type wrapperExpectation struct {
	app        string
	image      string
	replicas   int32
	memRequest string
	memLimit   string
	// env are plain variables that must be set to these values.
	env map[string]string
	// tokenEnv and memoryEnv are the variables fed from the Secret and the memory limit.
	tokenEnv, memoryEnv string
}

func checkWrapperBuild(h *serviceHarness, c catalogCase, want wrapperExpectation) {
	t := h.t
	t.Helper()
	statefulSet := h.sqliteStatefulSet(c.name())
	spec := statefulSet.Spec
	selector := map[string]string{"app.kubernetes.io/name": want.app, "app.kubernetes.io/instance": c.name(), serviceIDLabel: c.id}
	if spec.Replicas == nil || *spec.Replicas != want.replicas || spec.ServiceName != c.name() || spec.Selector == nil ||
		!mapsEqual(spec.Selector.MatchLabels, selector) || !mapsEqual(spec.Template.Labels, selector) {
		t.Fatalf("statefulset spec = %+v", spec)
	}
	// One claim template: every pod gets its own volume, which stays with it.
	if len(spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("claim templates = %+v", spec.VolumeClaimTemplates)
	}
	claim := spec.VolumeClaimTemplates[0]
	if claim.Name != "data" || claim.Spec.Resources.Requests.Storage().String() != "15Gi" || !mapsEqual(claim.Labels, selector) ||
		len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("claim template = %+v", claim)
	}

	pod := spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "oramasearch-dockerhub" ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("pod spec = %+v", pod)
	}
	security := pod.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser != 65532 ||
		security.FSGroup == nil || *security.FSGroup != 65532 || security.SeccompProfile == nil ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security = %+v", security)
	}
	container := pod.Containers[0]
	if container.Name != want.app || container.Image != want.image || len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8080 {
		t.Fatalf("container = %+v", container)
	}
	for _, probe := range []*corev1.Probe{container.ReadinessProbe, container.LivenessProbe, container.StartupProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port.StrVal != "http" {
			t.Fatalf("probe = %+v", probe)
		}
	}
	if container.Resources.Requests.Cpu().String() != "50m" || container.Resources.Requests.Memory().String() != want.memRequest ||
		container.Resources.Limits.Memory().String() != want.memLimit {
		t.Fatalf("resources = %+v", container.Resources)
	}
	containerSecurity := container.SecurityContext
	if containerSecurity == nil || containerSecurity.ReadOnlyRootFilesystem == nil || !*containerSecurity.ReadOnlyRootFilesystem ||
		containerSecurity.AllowPrivilegeEscalation == nil || *containerSecurity.AllowPrivilegeEscalation ||
		containerSecurity.Capabilities == nil || len(containerSecurity.Capabilities.Drop) != 1 || containerSecurity.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security = %+v", containerSecurity)
	}
	mounts := map[string]string{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}
	if mounts["data"] != "/data" || mounts["tmp"] != "/tmp" {
		t.Fatalf("mounts = %+v", container.VolumeMounts)
	}
	env := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		env[variable.Name] = variable
	}
	for name, value := range want.env {
		if env[name].Value != value {
			t.Fatalf("env %s = %q, want %q (%+v)", name, env[name].Value, value, container.Env)
		}
	}
	token := env[want.tokenEnv].ValueFrom
	if token == nil || token.SecretKeyRef == nil || token.SecretKeyRef.Name != c.name()+"-credentials" || token.SecretKeyRef.Key != "token" ||
		env[want.memoryEnv].ValueFrom == nil || env[want.memoryEnv].ValueFrom.ResourceFieldRef.Resource != "limits.memory" ||
		env[want.memoryEnv].ValueFrom.ResourceFieldRef.ContainerName != want.app {
		t.Fatalf("env = %+v", container.Env)
	}
	if statefulSet.Annotations[serviceAppliedHashAnnotation] == "" {
		t.Fatal("the StatefulSet carries no applied-spec hash")
	}

	service := &corev1.Service{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(h.upstream(serviceGVK, c.name()).Object, service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || !mapsEqual(service.Spec.Selector, selector) || len(service.Spec.Ports) != 1 ||
		service.Spec.Ports[0].Port != 8080 || service.Spec.Ports[0].TargetPort.StrVal != "http" {
		t.Fatalf("service spec = %+v", service.Spec)
	}

	secret := h.secret(c.name() + "-credentials")
	if !hexToken.Match(secret.Data["token"]) || secret.Labels[serviceComponentLabel] != serviceCredentialsRole {
		t.Fatalf("credentials Secret = %v %q", secret.Labels, secret.Data["token"])
	}
}

func checkDuckDBBuild(h *serviceHarness, c catalogCase) {
	h.t.Helper()
	checkWrapperBuild(h, c, wrapperExpectation{
		app: "lake-duckdb", image: DefaultLakeDuckDBImage, replicas: 1, memRequest: "256Mi", memLimit: "2Gi",
		env:      map[string]string{"LAKE_DUCKDB_PATH": "/data/main.duckdb", "LAKE_DUCKDB_ADDR": ":8080", "LAKE_DUCKDB_THREADS": "2"},
		tokenEnv: "LAKE_DUCKDB_TOKEN", memoryEnv: "LAKE_DUCKDB_MEMORY_LIMIT",
	})
}

func checkPolarsBuild(h *serviceHarness, c catalogCase) {
	h.t.Helper()
	checkWrapperBuild(h, c, wrapperExpectation{
		app: "lake-polars", image: DefaultLakePolarsImage, replicas: 3, memRequest: "256Mi", memLimit: "2Gi",
		env: map[string]string{"LAKE_POLARS_DIR": "/data", "LAKE_POLARS_ADDR": "0.0.0.0:8080", "LAKE_POLARS_THREADS": "2",
			"TMPDIR": "/tmp"},
		tokenEnv: "LAKE_POLARS_TOKEN", memoryEnv: "LAKE_POLARS_MEMORY_LIMIT",
	})
}

func TestServicesDuckDBHasExactlyOneInstance(t *testing.T) {
	for _, tc := range []struct {
		spec, message string
	}{
		{`{"instances":2}`, "a duckdb service has exactly one instance, because one DuckDB process owns its database file; spec.instances is 2"},
		{`{"instances":3,"storage_gi":5}`, "a duckdb service has exactly one instance, because one DuckDB process owns its database file; spec.instances is 3"},
	} {
		store := &fakeServiceStore{services: []LakeService{testService(testDuckDBID, testDuckDBSlug, serviceKindDuckDB, tc.spec, false)}}
		h := newServiceHarness(t, serviceTestLake(), store)
		h.poll()
		expectStatus(t, store.status(t, testDuckDBID), ServicePhaseFailed, tc.message, "")
		if h.exists(statefulSetGVK, testDuckDBName) {
			t.Fatalf("%s: a StatefulSet was created", tc.spec)
		}
	}
	// Unset instances is the one instance.
	store := &fakeServiceStore{services: []LakeService{testService(testDuckDBID, testDuckDBSlug, serviceKindDuckDB, `{"storage_gi":2}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStatus(t, store.status(t, testDuckDBID), ServicePhaseProvisioning, "waiting for the lake-duckdb pod", duckdbCatalogCase().endpoint)
}

// TestServicesPolarsInstancesAreWorkers: instances is the StatefulSet's replica count, readiness
// waits for every worker, and a change of instances scales the StatefulSet in place.
func TestServicesPolarsInstancesAreWorkers(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testPolarsID, testPolarsSlug, serviceKindPolars, `{}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	if got := *h.sqliteStatefulSet(testPolarsName).Spec.Replicas; got != 1 {
		t.Fatalf("default replicas = %d, want 1", got)
	}

	store.services[0].Spec = `{"instances":2}`
	h.poll()
	if got := *h.sqliteStatefulSet(testPolarsName).Spec.Replicas; got != 2 {
		t.Fatalf("replicas after instances=2 = %d", got)
	}
	h.setStatus(statefulSetGVK, testPolarsName, map[string]interface{}{"replicas": int64(2), "readyReplicas": int64(1)})
	h.poll()
	expectStatus(t, store.status(t, testPolarsID), ServicePhaseProvisioning, "waiting for the lake-polars pods", polarsCatalogCase().endpoint)
	h.setStatus(statefulSetGVK, testPolarsName, map[string]interface{}{"replicas": int64(2), "readyReplicas": int64(2)})
	h.poll()
	if phase := store.status(t, testPolarsID).Phase; phase != ServicePhaseReady {
		t.Fatalf("phase with every worker ready = %s", phase)
	}
}

// TestServicesPolarsDeletionRemovesEveryWorkerVolume: each worker's claim goes with the service.
func TestServicesPolarsDeletionRemovesEveryWorkerVolume(t *testing.T) {
	c := polarsCatalogCase()
	h, store := readyCatalogService(t, c)
	for _, name := range []string{"data-" + c.name() + "-0", "data-" + c.name() + "-1", "data-" + c.name() + "-2"} {
		h.create(catalogClaim(c, name))
	}
	store.setDesired(c.id, serviceDesiredAbsent)
	h.poll()
	h.poll()
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseDeleted, "service deleted", "")
	for _, name := range []string{"data-" + c.name() + "-0", "data-" + c.name() + "-1", "data-" + c.name() + "-2"} {
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceWorkspaceNamespace, Name: name}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
			t.Fatalf("claim %s survived: %v", name, err)
		}
	}
}

func TestServicesAnalyticsImages(t *testing.T) {
	type imageCase struct {
		name, lakeImage, flagImage, want string
	}
	for _, kind := range []struct {
		kind, id, slug, name, fallback string
		setLake                        func(lake *oramav1alpha1.LakeServicesSpec, image string)
		setFlag                        func(r *LakeReconciler, image string)
	}{
		{serviceKindDuckDB, testDuckDBID, testDuckDBSlug, testDuckDBName, DefaultLakeDuckDBImage,
			func(s *oramav1alpha1.LakeServicesSpec, image string) { s.DuckDBImage = image },
			func(r *LakeReconciler, image string) { r.DuckDBImage = image }},
		{serviceKindPolars, testPolarsID, testPolarsSlug, testPolarsName, DefaultLakePolarsImage,
			func(s *oramav1alpha1.LakeServicesSpec, image string) { s.PolarsImage = image },
			func(r *LakeReconciler, image string) { r.PolarsImage = image }},
	} {
		for _, tc := range []imageCase{
			{"default", "", "", kind.fallback},
			{"operator flag", "", "registry.example/x:flag", "registry.example/x:flag"},
			{"lake spec wins", "registry.example/x:lake", "registry.example/x:flag", "registry.example/x:lake"},
		} {
			t.Run(kind.kind+"/"+tc.name, func(t *testing.T) {
				lake := serviceTestLake()
				kind.setLake(&lake.Spec.Services, tc.lakeImage)
				store := &fakeServiceStore{services: []LakeService{testService(kind.id, kind.slug, kind.kind, `{}`, false)}}
				h := newServiceHarness(t, lake, store)
				kind.setFlag(h.reconciler, tc.flagImage)
				h.poll()
				if got := h.sqliteStatefulSet(kind.name).Spec.Template.Spec.Containers[0].Image; got != tc.want {
					t.Fatalf("image = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// TestServicesAnalyticsImageChangeRollsThePods: a new image rewrites the pod template once, and a
// steady pass afterwards writes nothing.
func TestServicesAnalyticsImageChangeRollsThePods(t *testing.T) {
	for _, c := range []catalogCase{duckdbCatalogCase(), polarsCatalogCase()} {
		t.Run(c.kind, func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{}`, false)}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			*h.updates = 0
			h.reconciler.DuckDBImage = "oramasearch/lake-duckdb:next"
			h.reconciler.PolarsImage = "oramasearch/lake-polars:next"
			h.poll()
			if *h.updates != 1 {
				t.Fatalf("an image change issued %d updates, want the StatefulSet's one", *h.updates)
			}
			*h.updates = 0
			h.poll()
			if *h.updates != 0 {
				t.Fatalf("a steady pass after the roll issued %d updates", *h.updates)
			}
		})
	}
}
