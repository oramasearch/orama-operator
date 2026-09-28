/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testBuildID     = "44444444-4444-4444-8444-444444444444"
	testBuildTarget = "lake-registry.lake.svc:80/space-one/app:v1"
)

func buildTestLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-build-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace:      testServiceNamespace,
			Image:          "oramasearch/lake:test",
			PullSecretName: "oramasearch-dockerhub",
			Builds: oramav1alpha1.LakeBuildsSpec{
				Enabled:      true,
				BuilderImage: "oramasearch/lake-builder:issue-195",
			},
		},
	}
}

func testBuild(id, target string) LakeBuild {
	return LakeBuild{
		ID:          id,
		CustomerID:  testServiceCustomer,
		Repository:  "repo-one",
		Revision:    "0123456789abcdef",
		Context:     "app",
		Dockerfile:  "Dockerfile",
		Target:      target,
		BuildArgs:   `{"PYTHON_VERSION":"3.13"}`,
		Desired:     buildDesiredQueued,
		TokenHashes: `{}`,
	}
}

func buildStatus(t *testing.T, store *fakeServiceStore, id string) LakeBuild {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, build := range store.builds {
		if build.ID == id {
			return build
		}
	}
	t.Fatalf("build %s not found", id)
	return LakeBuild{}
}

func TestLakeBuildRowCreatesIsolatedJobAndReportsSuccess(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newServiceHarness(t, lake, store)
	h.reconciler.BuildLogs = func(context.Context, string, string) (string, error) {
		return "#8 pushing\n{\"digest\":\"sha256:" + strings.Repeat("a", 64) + "\",\"size\":1234}\n", nil
	}

	h.poll()
	if !h.changing {
		t.Fatal("new build must keep the Lake on the fast poll")
	}
	target, err := parseBuildTarget(testBuildTarget)
	if err != nil {
		t.Fatal(err)
	}
	namespace := buildNamespace(lake.Name, target.Space)
	var buildNamespaceObject corev1.Namespace
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: namespace}, &buildNamespaceObject); err != nil {
		t.Fatal(err)
	}
	if buildNamespaceObject.Labels["pod-security.kubernetes.io/enforce"] != "privileged" ||
		buildNamespaceObject.Labels["pod-security.kubernetes.io/audit"] != "restricted" {
		t.Fatalf("build namespace Pod Security labels = %v", buildNamespaceObject.Labels)
	}
	for _, name := range []string{buildDefaultDeny, buildNetworkPolicy} {
		var policy networkingv1.NetworkPolicy
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &policy); err != nil {
			t.Fatalf("NetworkPolicy %s: %v", name, err)
		}
	}
	var secret corev1.Secret
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: buildTokenSecretName(testBuildID)}, &secret); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{buildTokenSourceKey, buildTokenPushKey, buildTokenEventsKey} {
		if len(secret.Data[key]) != 64 {
			t.Fatalf("token %s has %d bytes", key, len(secret.Data[key]))
		}
	}
	queued := buildStatus(t, store, testBuildID)
	if queued.Phase != BuildPhaseQueued || queued.TokenHashes == "{}" {
		t.Fatalf("queued build = %+v", queued)
	}
	var hashes map[string]string
	if err := json.Unmarshal([]byte(queued.TokenHashes), &hashes); err != nil || len(hashes) != 3 {
		t.Fatalf("token hashes = %q, %v", queued.TokenHashes, err)
	}

	var job batchv1.Job
	jobKey := client.ObjectKey{Namespace: namespace, Name: buildJobName(testBuildID)}
	if err := h.client.Get(context.Background(), jobKey, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 ||
		job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != buildDeadlineSeconds ||
		job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != buildJobTTLSeconds {
		t.Fatalf("Job bounds = %+v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	container := pod.Containers[0]
	ephemeralLimit := container.Resources.Limits[corev1.ResourceEphemeralStorage]
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		container.SecurityContext == nil || container.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined ||
		container.SecurityContext.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined ||
		ephemeralLimit.String() != "20Gi" {
		t.Fatalf("builder pod is not hardened and bounded: %+v", pod)
	}
	env := map[string]corev1.EnvVar{}
	for _, item := range container.Env {
		env[item.Name] = item
	}
	for _, name := range []string{
		"LAKE_URL", "BUILD_ID", "REPOSITORY", "REVISION", "CONTEXT", "DOCKERFILE", "TARGET",
		"BUILD_ARGS", "BUILD_ARG_ALLOW", "SOURCE_TOKEN", "PUSH_TOKEN", "EVENTS_TOKEN", "CACHE_REF", "DOCKER_CONFIG",
	} {
		if _, ok := env[name]; !ok {
			t.Errorf("builder env is missing %s", name)
		}
	}
	if env["CACHE_REF"].Value != "lake-registry.lake.svc:80/space-one/cache/app" {
		t.Fatalf("CACHE_REF = %q", env["CACHE_REF"].Value)
	}
	job.Status.Succeeded = 1
	if err := h.client.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	h.poll()
	succeeded := buildStatus(t, store, testBuildID)
	if succeeded.Phase != BuildPhaseSucceeded || succeeded.Digest != "sha256:"+strings.Repeat("a", 64) ||
		succeeded.Size != 1234 || succeeded.TokenHashes != "{}" {
		t.Fatalf("succeeded build = %+v", succeeded)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: buildTokenSecretName(testBuildID)}, &corev1.Secret{}); err == nil {
		t.Fatal("terminal build kept its plaintext token Secret")
	}
	writes := len(store.buildUpdates)
	h.poll()
	if len(store.buildUpdates) != writes {
		t.Fatal("steady terminal build rewrote its row")
	}
}

func TestLakeBuildConcurrencyCapsLakeAndSpace(t *testing.T) {
	for _, test := range []struct {
		name        string
		existing    string
		perLake     int32
		perSpace    int32
		wantMessage string
	}{
		{name: "lake", existing: "other-space", perLake: 1, perSpace: 2, wantMessage: "waiting for Lake build concurrency (1)"},
		{name: "space", existing: "space-one", perLake: 2, perSpace: 1, wantMessage: "waiting for space build concurrency (1)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lake := buildTestLake()
			lake.Spec.Builds.Concurrency = oramav1alpha1.LakeBuildConcurrencySpec{PerLake: test.perLake, PerSpace: test.perSpace}
			build := testBuild(testBuildID, testBuildTarget)
			store := &fakeServiceStore{builds: []LakeBuild{build}}
			existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "existing", Namespace: "other", Labels: map[string]string{
					buildLakeLabel: lake.Name, buildSpaceLabel: buildHash(test.existing),
				},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lake, oramav1alpha1.GroupVersion.WithKind("Lake"))},
			}}
			h := newServiceHarness(t, lake, store, existing)
			h.poll()
			status := buildStatus(t, store, testBuildID)
			if status.Phase != BuildPhaseQueued || status.Message != test.wantMessage {
				t.Fatalf("status = %+v", status)
			}
			target, _ := parseBuildTarget(testBuildTarget)
			if err := h.client.Get(context.Background(), client.ObjectKey{
				Namespace: buildNamespace(lake.Name, target.Space), Name: buildJobName(testBuildID),
			}, &batchv1.Job{}); err == nil {
				t.Fatal("concurrency-capped build created a Job")
			}
		})
	}
}

func TestCancelledLakeBuildDeletesOnlyItsJobAndTokens(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	store.mu.Lock()
	store.builds[0].Desired = buildDesiredCancelled
	store.mu.Unlock()
	h.poll()
	if !h.changing {
		t.Fatal("deleting build objects must keep the fast poll")
	}
	h.poll()
	status := buildStatus(t, store, testBuildID)
	if status.Phase != BuildPhaseCancelled || status.TokenHashes != "{}" {
		t.Fatalf("cancelled build = %+v", status)
	}
}

func TestLakeBuildDoesNotRestartWhenItsJobDisappears(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	target, _ := parseBuildTarget(testBuildTarget)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: buildJobName(testBuildID), Namespace: buildNamespace(lake.Name, target.Space),
	}}
	if err := h.client.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h.poll()
	status := buildStatus(t, store, testBuildID)
	if status.Phase != BuildPhaseFailed || status.Message != "build Job disappeared before its outcome was recorded" {
		t.Fatalf("missing Job status = %+v", status)
	}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err == nil {
		t.Fatal("missing build Job was started a second time")
	}
}

func TestInvalidLakeBuildFailsWithoutCreatingResources(t *testing.T) {
	lake := buildTestLake()
	build := testBuild(testBuildID, "public.example/Space/app:latest")
	store := &fakeServiceStore{builds: []LakeBuild{build}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	status := buildStatus(t, store, testBuildID)
	if status.Phase != BuildPhaseFailed || !strings.Contains(status.Message, "target must be") {
		t.Fatalf("invalid build status = %+v", status)
	}
	var jobs batchv1.JobList
	if err := h.client.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("invalid build Jobs = %+v, %v", jobs.Items, err)
	}
}

type buildEventTransport struct {
	mu     sync.Mutex
	events []buildEvent
}

func (t *buildEventTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var event buildEvent
	err := json.NewDecoder(request.Body).Decode(&event)
	_ = request.Body.Close()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func TestLakeBuildPublishesPodEventsOnce(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newServiceHarness(t, lake, store)
	transport := &buildEventTransport{}
	h.reconciler.BuildEventClient = &http.Client{Transport: transport}
	h.poll()
	target, _ := parseBuildTarget(testBuildTarget)
	namespace := buildNamespace(lake.Name, target.Space)
	jobKey := client.ObjectKey{Namespace: namespace, Name: buildJobName(testBuildID)}
	var job batchv1.Job
	if err := h.client.Get(context.Background(), jobKey, &job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 1
	if err := h.client.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "builder-pod", Namespace: namespace, Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
		}}},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.poll()
	h.poll()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.events) != 1 || transport.events[0].BuildID != testBuildID || transport.events[0].Message != "builder pod scheduled" {
		t.Fatalf("events = %+v", transport.events)
	}
}

func TestParseLakeBuildResultRequiresFinalJSONLine(t *testing.T) {
	valid := "progress\n{\"digest\":\"sha256:" + strings.Repeat("b", 64) + "\",\"size\":9}\n"
	result, err := parseBuildResult(valid)
	if err != nil || result.Size != 9 {
		t.Fatalf("valid result = %+v, %v", result, err)
	}
	if _, err := parseBuildResult(valid + "trailing log\n"); err == nil {
		t.Fatal("accepted result JSON before the final stdout line")
	}
}
