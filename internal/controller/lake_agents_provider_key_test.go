// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// orama-operator#779: Orama's provider key is a platform Secret of the Lake. It never goes into a
// workspace namespace and never reaches a pod that runs a tenant's agent; those pods call models
// through lake's model gateway with their own LAKE_TOKEN (lake docs/api/model-gateway.md).

const (
	testServiceLaunchID   = "55555555-5555-4555-8555-555555555555"
	testServiceLaunchSlug = "al-555555555555"
	testServiceLaunchName = "prod-al-555555555555"
)

// assertNoProviderKey fails when a pod reads the Lake's provider key in any way: an env var, an
// envFrom of its Secret, or a volume of its Secret.
func assertNoProviderKey(t *testing.T, what, secretName string, pod corev1.PodSpec) {
	t.Helper()
	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, env := range container.Env {
			if env.Name == openAIKeyName {
				t.Errorf("%s: container %s has %s", what, container.Name, openAIKeyName)
			}
			if ref := env.ValueFrom; ref != nil && ref.SecretKeyRef != nil && ref.SecretKeyRef.Name == secretName {
				t.Errorf("%s: container %s reads %s from the provider key Secret", what, container.Name, env.Name)
			}
		}
		for _, from := range container.EnvFrom {
			if from.SecretRef != nil && from.SecretRef.Name == secretName {
				t.Errorf("%s: container %s imports the provider key Secret", what, container.Name)
			}
		}
	}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil && volume.Secret.SecretName == secretName {
			t.Errorf("%s: volume %s mounts the provider key Secret", what, volume.Name)
		}
	}
}

func TestTenantAgentPodsNeverReceiveOramasProviderKey(t *testing.T) {
	lake := agentsTestLake()
	job := testLaunch(agentLaunchModeJob)
	job.WorkspaceNamespace = testWorkspaceName
	service := testLaunch(agentLaunchModeService)
	service.ID, service.Slug, service.WorkspaceNamespace = testServiceLaunchID, testServiceLaunchSlug, testWorkspaceName
	schedule := testSchedule(testScheduleID, testScheduleSlug, "0 9 * * 1", "")
	schedule.WorkspaceNamespace = testWorkspaceName
	store := &fakeServiceStore{
		launches:  []LakeAgentLaunch{job, service},
		schedules: []LakeAgentSchedule{schedule},
	}
	sources := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lake.Spec.PullSecretName, Namespace: testServiceNamespace},
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lake.Spec.Agents.OpenAISecretName, Namespace: testServiceNamespace},
			Data:       map[string][]byte{openAIKeyName: []byte("test-key")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lakeAgentServiceTokenSecret(lake.Name), Namespace: testServiceNamespace},
			Data:       map[string][]byte{agentServiceTokenKey: []byte("the-shared-agent-service-token")},
		},
	}
	h := newServiceHarness(t, lake, store, sources...)
	h.poll()
	ctx := context.Background()

	var jobObject batchv1.Job
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testLaunchName}, &jobObject); err != nil {
		t.Fatalf("job launch: %v", err)
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testServiceLaunchName}, &deployment); err != nil {
		t.Fatalf("service launch: %v", err)
	}
	var cron batchv1.CronJob
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testScheduleName}, &cron); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	secretName := lake.Spec.Agents.OpenAISecretName
	pods := map[string]corev1.PodSpec{
		"job launch":     jobObject.Spec.Template.Spec,
		"service launch": deployment.Spec.Template.Spec,
		"schedule":       cron.Spec.JobTemplate.Spec.Template.Spec,
	}
	for what, pod := range pods {
		assertNoProviderKey(t, what, secretName, pod)
		// The model is reached through lake, with the run's own credential.
		container := pod.Containers[0]
		if envValue(t, container.Env, "LAKE_URL").Value == "" || envValue(t, container.Env, "LAKE_TOKEN").ValueFrom == nil {
			t.Errorf("%s: no LAKE_URL and LAKE_TOKEN to call lake's model gateway with", what)
		}
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: secretName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the provider key Secret was copied into the workspace namespace: %v", err)
	}
	// The pull credential still is: the workspace's pods pull private images with it.
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: lake.Spec.PullSecretName}, &corev1.Secret{}); err != nil {
		t.Fatalf("the pull Secret is no longer copied: %v", err)
	}
}

// agent-runtime#14: a service launch runs the person's own code, so it never gets the Lake's agent
// service token (which every library agent accepts). It gets its own token, derived from the
// Lake's and the launch id, which lake computes the same way; a job gets no service token at all,
// and a copy an earlier operator left in the workspace namespace is removed.
func TestAServiceLaunchHoldsItsOwnServiceTokenNeverTheLakes(t *testing.T) {
	lake := agentsTestLake()
	service := testLaunch(agentLaunchModeService)
	service.ID, service.Slug, service.WorkspaceNamespace = testServiceLaunchID, testServiceLaunchSlug, testWorkspaceName
	job := testLaunch(agentLaunchModeJob)
	job.WorkspaceNamespace = testWorkspaceName
	shared := lakeAgentServiceTokenSecret(lake.Name)
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: shared, Namespace: testWorkspaceName,
			Labels: map[string]string{workspaceLabel: testWorkspaceID}},
		Data: map[string][]byte{agentServiceTokenKey: []byte("the-shared-agent-service-token")},
	}
	sources := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lake.Spec.PullSecretName, Namespace: testServiceNamespace},
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: shared, Namespace: testServiceNamespace},
			Data:       map[string][]byte{agentServiceTokenKey: []byte("the-shared-agent-service-token")},
		},
	}
	h := newServiceHarness(t, lake, &fakeServiceStore{launches: []LakeAgentLaunch{service, job}}, sources...)
	ctx := context.Background()
	if err := controllerutil.SetControllerReference(h.lake, stale, h.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if err := h.reconciler.removeWorkspaceServiceTokenCopies(ctx, h.lake); err != nil {
		t.Fatal(err)
	}

	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: shared}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the Lake's agent service token is in the workspace namespace: %v", err)
	}
	var own corev1.Secret
	ownName := agentLaunchServiceTokenSecret(lake.Name, testServiceLaunchSlug)
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: ownName}, &own); err != nil {
		t.Fatalf("the launch's own service token: %v", err)
	}
	want := launchServiceToken("the-shared-agent-service-token", testServiceLaunchID)
	if got := string(own.Data[agentServiceTokenKey]); got != want || got == "the-shared-agent-service-token" {
		t.Fatalf("launch service token %q, want %q", got, want)
	}
	if own.Labels[agentLaunchIDLabel] != testServiceLaunchID || !metav1.IsControlledBy(&own, h.lake) {
		t.Fatalf("labels %v, owners %v", own.Labels, own.OwnerReferences)
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testServiceLaunchName}, &deployment); err != nil {
		t.Fatal(err)
	}
	ref := envValue(t, deployment.Spec.Template.Spec.Containers[0].Env, "AGENT_SERVICE_TOKEN").ValueFrom.SecretKeyRef
	if ref.Name != ownName || ref.Key != agentServiceTokenKey {
		t.Fatalf("the service launch reads AGENT_SERVICE_TOKEN from %s/%s", ref.Name, ref.Key)
	}
	var jobObject batchv1.Job
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testWorkspaceName, Name: testLaunchName}, &jobObject); err != nil {
		t.Fatal(err)
	}
	for _, env := range jobObject.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "AGENT_SERVICE_TOKEN" {
			t.Fatal("a job launch has a service token")
		}
	}
	// Distinct per launch.
	if launchServiceToken("k", "a") == launchServiceToken("k", "b") {
		t.Fatal("two launches share a service token")
	}
	// Lake derives the same value (lake agent_launch_service_token); the same vector is in lake's test.
	if got := launchServiceToken("k", "al-1"); got != "c5447490ca4b85399ec15e9025cdf93a5390197f3e3aea8bf66120e91411da6c" {
		t.Fatalf("launchServiceToken(k, al-1) = %s", got)
	}
}

// The library agents are Orama's own code in the Lake's namespace; they keep the key, and the run
// env of a tenant's agent is built without it.
func TestOnlyLibraryAgentsInTheLakeNamespaceHoldTheProviderKey(t *testing.T) {
	lake := agentsTestLake()
	for _, env := range agentRunEnv(lake, "credential", "", testServiceCustomer, testWorkspaceID, "a-1", agentLaunchModeJob, "null") {
		if env.Name == openAIKeyName {
			t.Fatalf("a tenant agent's run env has %s", openAIKeyName)
		}
	}
	h := newServiceHarness(t, lake, &fakeServiceStore{})
	ctx := context.Background()
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, map[string]string{"app.kubernetes.io/name": "lake"}); err != nil {
		t.Fatal(err)
	}
	var library appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-project"}, &library); err != nil {
		t.Fatal(err)
	}
	ref := envValue(t, library.Spec.Template.Spec.Containers[0].Env, openAIKeyName).ValueFrom.SecretKeyRef
	if ref.Name != lake.Spec.Agents.OpenAISecretName {
		t.Fatalf("a library agent reads %s from %q", openAIKeyName, ref.Name)
	}
}

// A copy an earlier operator made in a workspace namespace is deleted on the next Lake reconcile,
// which every operator roll runs. A Secret of the same name the Lake does not own, and the Lake's own
// Secret in its namespace, are kept.
func TestAnEarlierCopyOfTheProviderKeyLeavesWorkspaceNamespaces(t *testing.T) {
	lake := agentsTestLake()
	name := lake.Spec.Agents.OpenAISecretName
	original := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testServiceNamespace},
		Data:       map[string][]byte{openAIKeyName: []byte("test-key")},
	}
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testWorkspaceName,
			Labels: map[string]string{workspaceLabel: testWorkspaceID}},
		Data: map[string][]byte{openAIKeyName: []byte("test-key")},
	}
	if err := controllerutil.SetControllerReference(lake, stale, newLakeScheme(t)); err != nil {
		t.Fatal(err)
	}
	notOurs := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod-ws-ffffffffffff",
			Labels: map[string]string{workspaceLabel: "another-workspace"}},
		Data: map[string][]byte{openAIKeyName: []byte("someone-elses-key")},
	}
	h := newServiceHarness(t, lake, &fakeServiceStore{}, original, stale, notOurs)
	ctx := context.Background()
	if err := h.reconciler.removeWorkspaceProviderKeyCopies(ctx, h.lake); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(ctx, client.ObjectKeyFromObject(stale), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the earlier copy is still in the workspace namespace: %v", err)
	}
	for _, kept := range []*corev1.Secret{original, notOurs} {
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(kept), &corev1.Secret{}); err != nil {
			t.Fatalf("%s/%s was removed: %v", kept.Namespace, kept.Name, err)
		}
	}
}
