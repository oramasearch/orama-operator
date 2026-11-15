/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testSQLiteID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	testSQLiteSlug = "svc-bbbbbbbbbbbb"
	testSQLiteName = "prod-svc-bbbbbbbbbbbb"
)

var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sqliteCatalogCase runs the sqlite kind through every catalog test: build, ready gating and
// credential, deletion order, foreign objects and Secrets, old volumes, and the quota.
func sqliteCatalogCase() catalogCase {
	return catalogCase{
		kind: serviceKindSQLite, id: testSQLiteID, slug: testSQLiteSlug,
		objects:    []string{"StatefulSet/" + testSQLiteName, "Service/" + testSQLiteName},
		checkBuild: checkSQLiteBuild,
		waiting:    "waiting for the lake-sqlite pod",
		markPartlyReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(1), "readyReplicas": int64(0)})
		},
		partWaiting: "waiting for the lake-sqlite pod",
		markReady: func(h *serviceHarness, c catalogCase) {
			h.setStatus(statefulSetGVK, c.name(), map[string]interface{}{"replicas": int64(1), "readyReplicas": int64(1)})
		},
		passwordSecret: testSQLiteName + "-credentials", passwordKey: "token",
		endpoint: "http://prod-svc-bbbbbbbbbbbb.prod-ws-123e4567e89b.svc.cluster.local:8080",
		credential: func(token string) string {
			return `{"kind":"sqlite","endpoint":"http://prod-svc-bbbbbbbbbbbb.prod-ws-123e4567e89b.svc.cluster.local:8080","token":"` + token + `","database":"main"}`
		},
		claimLabels: map[string]string{"app.kubernetes.io/name": "lake-sqlite", "app.kubernetes.io/instance": testSQLiteName, serviceIDLabel: testSQLiteID},
	}
}

func (h *serviceHarness) sqliteStatefulSet(name string) *appsv1.StatefulSet {
	h.t.Helper()
	statefulSet := &appsv1.StatefulSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(h.upstream(statefulSetGVK, name).Object, statefulSet); err != nil {
		h.t.Fatal(err)
	}
	return statefulSet
}

func checkSQLiteBuild(h *serviceHarness, c catalogCase) {
	t := h.t
	t.Helper()
	statefulSet := h.sqliteStatefulSet(c.name())
	spec := statefulSet.Spec
	selector := map[string]string{"app.kubernetes.io/name": "lake-sqlite", "app.kubernetes.io/instance": c.name(), serviceIDLabel: c.id}
	if spec.Replicas == nil || *spec.Replicas != 1 || spec.ServiceName != c.name() || spec.Selector == nil ||
		!mapsEqual(spec.Selector.MatchLabels, selector) || !mapsEqual(spec.Template.Labels, selector) {
		t.Fatalf("statefulset spec = %+v", spec)
	}
	if len(spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("claim templates = %+v", spec.VolumeClaimTemplates)
	}
	claim := spec.VolumeClaimTemplates[0]
	// The contract's storage_gi (the catalog tests ask for 15).
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
		security.FSGroup == nil || *security.FSGroup != 65532 {
		t.Fatalf("pod security = %+v", security)
	}
	container := pod.Containers[0]
	if container.Image != DefaultLakeSQLiteImage || len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8080 {
		t.Fatalf("container = %+v", container)
	}
	for _, probe := range []*corev1.Probe{container.ReadinessProbe, container.LivenessProbe, container.StartupProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port.StrVal != "http" {
			t.Fatalf("probe = %+v", probe)
		}
	}
	if container.Resources.Requests.Cpu().String() != "50m" || container.Resources.Requests.Memory().String() != "64Mi" ||
		container.Resources.Limits.Memory().String() != "512Mi" {
		t.Fatalf("resources = %+v", container.Resources)
	}
	containerSecurity := container.SecurityContext
	if containerSecurity == nil || containerSecurity.ReadOnlyRootFilesystem == nil || !*containerSecurity.ReadOnlyRootFilesystem ||
		containerSecurity.AllowPrivilegeEscalation == nil || *containerSecurity.AllowPrivilegeEscalation {
		t.Fatalf("container security = %+v", containerSecurity)
	}
	env := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		env[variable.Name] = variable
	}
	token := env["LAKE_SQLITE_TOKEN"].ValueFrom
	if env["LAKE_SQLITE_PATH"].Value != "/data/main.db" || token == nil || token.SecretKeyRef == nil ||
		token.SecretKeyRef.Name != c.name()+"-credentials" || token.SecretKeyRef.Key != "token" ||
		env["LAKE_SQLITE_MEMORY_LIMIT"].ValueFrom == nil || env["LAKE_SQLITE_MEMORY_LIMIT"].ValueFrom.ResourceFieldRef.Resource != "limits.memory" {
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

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestServicesSQLiteHasExactlyOneInstance(t *testing.T) {
	for _, tc := range []struct {
		spec, message string
	}{
		{`{"instances":2}`, "a sqlite service has exactly one instance, because SQLite has one writer; spec.instances is 2"},
		{`{"instances":3,"storage_gi":5}`, "a sqlite service has exactly one instance, because SQLite has one writer; spec.instances is 3"},
	} {
		store := &fakeServiceStore{services: []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, tc.spec, false)}}
		h := newServiceHarness(t, serviceTestLake(), store)
		h.poll()
		expectStatus(t, store.status(t, testSQLiteID), ServicePhaseFailed, tc.message, "")
		if h.exists(statefulSetGVK, testSQLiteName) {
			t.Fatalf("%s: a StatefulSet was created", tc.spec)
		}
	}
	// Unset instances is the one instance.
	store := &fakeServiceStore{services: []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{"storage_gi":2}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStatus(t, store.status(t, testSQLiteID), ServicePhaseProvisioning, "waiting for the lake-sqlite pod", sqliteCatalogCase().endpoint)
}

func TestServicesSQLiteImage(t *testing.T) {
	for _, tc := range []struct {
		name, lakeImage, flagImage, want string
	}{
		{"default", "", "", DefaultLakeSQLiteImage},
		{"operator flag", "", "registry.example/lake-sqlite:flag", "registry.example/lake-sqlite:flag"},
		{"lake spec wins", "registry.example/lake-sqlite:lake", "registry.example/lake-sqlite:flag", "registry.example/lake-sqlite:lake"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lake := serviceTestLake()
			lake.Spec.Services.SQLiteImage = tc.lakeImage
			store := &fakeServiceStore{services: []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false)}}
			h := newServiceHarness(t, lake, store)
			h.reconciler.SQLiteImage = tc.flagImage
			h.poll()
			if got := h.sqliteStatefulSet(testSQLiteName).Spec.Template.Spec.Containers[0].Image; got != tc.want {
				t.Fatalf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServicesSQLiteImageChangeRollsThePod(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{"storage_gi":3}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	before := h.sqliteStatefulSet(testSQLiteName)

	var lake = h.lake.DeepCopy()
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(lake), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Services.SQLiteImage = "oramasearch/lake-sqlite:next"
	h.update(lake)
	store.services[0].Spec = `{"storage_gi":8}`
	*h.updates = 0
	h.poll()
	if *h.updates != 1 {
		t.Fatalf("an image change issued %d updates, want the StatefulSet's one", *h.updates)
	}
	after := h.sqliteStatefulSet(testSQLiteName)
	if after.Spec.Template.Spec.Containers[0].Image != "oramasearch/lake-sqlite:next" ||
		after.Annotations[serviceAppliedHashAnnotation] == before.Annotations[serviceAppliedHashAnnotation] {
		t.Fatalf("template not rewritten: %s, hash %s", after.Spec.Template.Spec.Containers[0].Image, after.Annotations[serviceAppliedHashAnnotation])
	}
	// The volume is sized once: storage_gi changes never touch the immutable claim template.
	if got := after.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "3Gi" {
		t.Fatalf("claim size = %s", got)
	}

	*h.updates = 0
	h.poll()
	if *h.updates != 0 {
		t.Fatalf("a steady pass after the roll issued %d updates", *h.updates)
	}
}

func TestServicesSQLiteDataClaimNeedsTheStatefulSetLabels(t *testing.T) {
	c := sqliteCatalogCase()
	h, store := readyCatalogService(t, c)
	// data-<name>-0 as the StatefulSet made it, and a claim of the same pattern without its labels.
	h.create(catalogClaim(c, "data-"+c.name()+"-0"))
	stranger := &corev1.PersistentVolumeClaim{}
	stranger.Name, stranger.Namespace = "data-"+c.name()+"-1", testServiceWorkspaceNamespace
	stranger.Labels = map[string]string{"app.kubernetes.io/instance": c.name()}
	h.create(stranger)

	store.setDesired(c.id, serviceDesiredAbsent)
	h.poll()
	h.poll()
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseDeleted, "service deleted", "")
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(stranger), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("a claim without the StatefulSet's labels was deleted: %v", err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceWorkspaceNamespace, Name: "data-" + c.name() + "-0"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the data claim survived: %v", err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceWorkspaceNamespace, Name: c.passwordSecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the credentials Secret survived: %v", err)
	}
}

func TestServicesSQLiteCountsTowardQuota(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services.MaxServices = 1
	store := &fakeServiceStore{services: []LakeService{
		testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	h.poll()
	if phase := store.status(t, testSQLiteID).Phase; phase != ServicePhaseProvisioning {
		t.Fatalf("sqlite: %s", phase)
	}
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "service quota reached (1)", "")

	// A new SQLite service over the quota writes nothing, not even its token.
	store2 := &fakeServiceStore{services: []LakeService{
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
		testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false),
	}}
	h2 := newServiceHarness(t, lake, store2)
	h2.poll()
	expectStatus(t, store2.status(t, testSQLiteID), ServicePhaseFailed, "service quota reached (1)", "")
	if h2.exists(statefulSetGVK, testSQLiteName) || h2.exists(serviceGVK, testSQLiteName) {
		t.Fatal("a service over quota was created")
	}
	if err := h2.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceWorkspaceNamespace, Name: testSQLiteName + "-credentials"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a service over quota generated a token: %v", err)
	}
}

// The Lake's own StatefulSets and Services never count as services: only objects carrying a
// service id do.
func TestServicesSQLiteQuotaIgnoresOtherStatefulSets(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services.MaxServices = 1
	own := &appsv1.StatefulSet{}
	own.Name, own.Namespace = "lake-prod-other", testServiceWorkspaceNamespace
	own.Labels = map[string]string{serviceLakeLabel: "prod"}
	own.OwnerReferences = lakeOwnerRef(lake)
	store := &fakeServiceStore{services: []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{}`, false)}}
	h := newServiceHarness(t, lake, store, own)
	h.poll()
	expectStatus(t, store.status(t, testSQLiteID), ServicePhaseProvisioning, "waiting for the lake-sqlite pod", sqliteCatalogCase().endpoint)
}
