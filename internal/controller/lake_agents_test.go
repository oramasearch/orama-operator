// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testLaunchID   = "66666666-6666-4666-8666-666666666666"
	testLaunchSlug = "al-666666666666"
	testLaunchName = "prod-al-666666666666"
)

func agentsTestLake() *oramav1alpha1.Lake {
	lake := serviceTestLake()
	lake.Spec.Agents = oramav1alpha1.LakeAgentsSpec{
		RuntimeImage:     "oramasearch/agent-runtime:test",
		OpenAISecretName: "lake-prod-openai",
		Model:            "gpt-5.5",
		Library: []oramav1alpha1.LakeLibraryAgent{
			{Key: "agent-project", Image: "oramasearch/agent-project:test"},
			{Key: "agent-ask-orama", Image: "oramasearch/agent-ask-orama:test"},
		},
	}
	return lake
}

func testLaunch(mode string) LakeAgentLaunch {
	return LakeAgentLaunch{ID: testLaunchID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		WorkspaceNamespace: testServiceNamespace, AgentID: "a-1", Mode: mode,
		Input: `"week 39"`, Slug: testLaunchSlug, Desired: serviceDesiredPresent, Generation: 1}
}

func envValue(t *testing.T, env []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	for _, variable := range env {
		if variable.Name == name {
			return variable
		}
	}
	t.Fatalf("no %s in %v", name, env)
	return corev1.EnvVar{}
}

func (f *fakeServiceStore) launchStatus(t *testing.T) LakeAgentLaunchStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launches) == 0 || f.launches[0].Status == nil {
		t.Fatalf("the launch has no status")
	}
	return *f.launches[0].Status
}

func TestLakeTellsLakeWhereItsAgentsAre(t *testing.T) {
	lake := agentsTestLake()
	env := lakeAgentEnv(lake, "lake")
	// Every library agent, in key order: the two the spec names and the defaults (operator#539).
	want := make([]string, 0, len(wantLakeDefaultLibrary))
	for key := range wantLakeDefaultLibrary {
		want = append(want, key+"=http://prod-"+key+".lake.svc.cluster.local:8080")
	}
	sort.Strings(want)
	if got := envValue(t, env, "LAKE_AGENT_ENDPOINTS").Value; got != strings.Join(want, ",") {
		t.Fatalf("LAKE_AGENT_ENDPOINTS = %q", got)
	}
	if got := envValue(t, env, "LAKE_INTERNAL_URL").Value; got != "http://lake-prod.lake.svc.cluster.local:80" {
		t.Fatalf("LAKE_INTERNAL_URL = %q", got)
	}
	if ref := envValue(t, env, "LAKE_AGENT_SERVICE_TOKEN").ValueFrom.SecretKeyRef; ref.Name != "lake-prod-agent-service-token" {
		t.Fatalf("service token from %q", ref.Name)
	}
	if got := envValue(t, env, "LAKE_AGENT_BUS_DIR").Value; got != "/var/run/lake/agent-bus" {
		t.Fatalf("LAKE_AGENT_BUS_DIR = %q", got)
	}
	if got := envValue(t, lakeAgentEnv(serviceTestLake(), "lake"), "LAKE_AGENT_ENDPOINTS").Value; len(strings.Split(got, ",")) != len(wantLakeDefaultLibrary) {
		t.Fatalf("a Lake that states no agents runs the whole default library, got %q", got)
	}
	full := lakeEnv(lake, lakeSealingMaterial{})
	envValue(t, full, "LAKE_AGENT_ENDPOINTS")
}

func TestLibraryAgentsRunAsDeploymentsAndLeaveWhenUnlisted(t *testing.T) {
	store := &fakeServiceStore{}
	h := newServiceHarness(t, agentsTestLake(), store)
	ctx := context.Background()
	labels := map[string]string{"app.kubernetes.io/name": "lake"}
	if err := h.reconciler.reconcileLakeAgentServiceToken(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	var token corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "lake-prod-agent-service-token"}, &token); err != nil {
		t.Fatalf("service token not minted: %v", err)
	}
	minted := string(token.Data["token"])
	if err := h.reconciler.reconcileLakeAgentServiceToken(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	_ = h.client.Get(ctx, client.ObjectKeyFromObject(&token), &token)
	if string(token.Data["token"]) != minted {
		t.Fatal("the service token is minted once")
	}
	// Each library agent gets its own bus credential, kept across reconciles.
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	var bus corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "lake-prod-agent-bus"}, &bus); err != nil {
		t.Fatalf("bus tokens not minted: %v", err)
	}
	askToken, projectToken := string(bus.Data["agent-ask-orama"]), string(bus.Data["agent-project"])
	if len(askToken) != 64 || len(projectToken) != 64 || askToken == projectToken || askToken == minted {
		t.Fatalf("one distinct bus token per agent, never the service token: %v", bus.Data)
	}
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	_ = h.client.Get(ctx, client.ObjectKeyFromObject(&bus), &bus)
	if string(bus.Data["agent-ask-orama"]) != askToken {
		t.Fatal("a bus token never changes")
	}
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	var ask appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-ask-orama"}, &ask); err != nil {
		t.Fatalf("Ask Orama deployment: %v", err)
	}
	container := ask.Spec.Template.Spec.Containers[0]
	if container.Image != "oramasearch/agent-ask-orama:test" {
		t.Fatalf("image %q", container.Image)
	}
	if envValue(t, container.Env, "LAKE_URL").Value != "http://lake-prod.lake.svc.cluster.local:80" {
		t.Fatal("agents reach lake in-cluster")
	}
	if ref := envValue(t, container.Env, "AGENT_BUS_TOKEN").ValueFrom.SecretKeyRef; ref.Name != "lake-prod-agent-bus" || ref.Key != "agent-ask-orama" {
		t.Fatalf("Ask Orama's bus token from %v", ref)
	}
	if envValue(t, container.Env, "AGENT_BUS_AGENT").Value != "agent-ask-orama" {
		t.Fatal("the agent takes tasks under its own key")
	}
	if ref := envValue(t, container.Env, "OPENAI_API_KEY").ValueFrom.SecretKeyRef; ref.Name != "lake-prod-openai" || ref.Key != "OPENAI_API_KEY" {
		t.Fatalf("OpenAI key from %v", ref)
	}
	if !*ask.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem ||
		*ask.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("agent pods are locked down")
	}
	var svc corev1.Service
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-project"}, &svc); err != nil {
		t.Fatalf("Project Agent service: %v", err)
	}

	// Every Lake runs every library agent (operator#539): an agent dropped from the spec goes
	// back to its default image instead of stopping.
	h.lake.Spec.Agents.Library = h.lake.Spec.Agents.Library[:1]
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-ask-orama"}, &ask); err != nil ||
		ask.Spec.Template.Spec.Containers[0].Image != wantLakeDefaultLibrary["agent-ask-orama"] {
		t.Fatalf("an agent without a spec image runs its default, got %v", err)
	}
	// A library agent the operator no longer runs (a key dropped from the library) is removed.
	retired := ask.DeepCopy()
	retired.Name, retired.ResourceVersion, retired.UID = "prod-agent-retired", "", ""
	retired.Labels[libraryAgentLabel] = "agent-retired"
	if err := h.client.Create(ctx, retired); err != nil {
		t.Fatal(err)
	}
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(ctx, client.ObjectKeyFromObject(retired), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a retired library agent is removed, got %v", err)
	}
}

func TestAJobLaunchRunsOnceWithItsCredentialAndReportsItsOutcome(t *testing.T) {
	store := &fakeServiceStore{launches: []LakeAgentLaunch{testLaunch(agentLaunchModeJob)}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	ctx := context.Background()
	var credential corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName + "-credential"}, &credential); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	token := string(credential.Data["token"])
	if !strings.HasPrefix(token, "lal."+testLaunchID+".") || len(token) != len("lal.")+36+1+32 {
		t.Fatalf("credential %q", token)
	}
	status := store.launchStatus(t)
	if status.Phase != AgentPhasePending || status.CredentialSHA256 != credentialDigest(token) {
		t.Fatalf("status %+v", status)
	}
	if !h.changing {
		t.Fatal("a starting launch keeps the poll fast")
	}
	var job batchv1.Job
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName}, &job); err != nil {
		t.Fatalf("job: %v", err)
	}
	if *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("an agent run is never retried behind the person's back")
	}
	for key, lake := range map[string]string{"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": "prod"} {
		if job.Spec.Template.Labels[key] == lake {
			t.Fatalf("a job pod carries lake's selector label %s=%s and would receive lake's traffic", key, lake)
		}
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	if envValue(t, env, "AGENT_INPUT").Value != `"week 39"` || envValue(t, env, "AGENT_MODE").Value != "job" ||
		envValue(t, env, "LAKE_TOKEN").ValueFrom.SecretKeyRef.Name != testLaunchName+"-credential" {
		t.Fatalf("job env %v", env)
	}
	for _, variable := range env {
		if strings.Contains(variable.Value, token) {
			t.Fatal("the credential is never a plain env value")
		}
	}

	job.Status.Succeeded = 1
	if err := h.client.Status().Update(ctx, &job); err != nil {
		_ = h.client.Update(ctx, &job)
	}
	h.poll()
	if status := store.launchStatus(t); status.Phase != AgentPhaseRunning {
		t.Fatalf("a success whose pod outcome is not readable yet is not final: %+v", status)
	}

	if err := h.client.Status().Update(ctx, &job); err != nil {
		if err := h.client.Update(ctx, &job); err != nil {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testLaunchName + "-x", Namespace: testServiceNamespace, Labels: map[string]string{"job-name": testLaunchName}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"reply":"done","exit":0}`}}}}},
	}
	if err := h.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Status().Update(ctx, pod); err != nil {
		_ = h.client.Update(ctx, pod)
	}
	h.poll()
	status = store.launchStatus(t)
	if status.Phase != AgentPhaseSucceeded || status.ExitCode == nil || *status.ExitCode != 0 ||
		!strings.Contains(status.Result, "done") {
		t.Fatalf("finished status %+v", status)
	}
	if status.CredentialSHA256 != "" {
		t.Fatal("a finished run's credential stops resolving")
	}
}

func TestAServiceLaunchIsReadyWhenItsDeploymentIs(t *testing.T) {
	store := &fakeServiceStore{launches: []LakeAgentLaunch{testLaunch(agentLaunchModeService)}}
	h := newServiceHarness(t, agentsTestLake(), store)
	h.poll()
	if store.launchStatus(t).Phase != AgentPhasePending {
		t.Fatalf("status %+v", store.launchStatus(t))
	}
	ctx := context.Background()
	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName}, &deployment); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != "oramasearch/agent-runtime:test" {
		t.Fatal("every launch runs on agent-runtime")
	}
	deployment.Status.AvailableReplicas = 1
	if err := h.client.Status().Update(ctx, &deployment); err != nil {
		_ = h.client.Update(ctx, &deployment)
	}
	h.poll()
	status := store.launchStatus(t)
	if status.Phase != AgentPhaseReady || status.Endpoint != "http://prod-al-666666666666.lake.svc.cluster.local:8080" {
		t.Fatalf("status %+v", status)
	}

	store.mu.Lock()
	store.launches[0].Desired = serviceDesiredAbsent
	store.launches[0].Generation++
	store.mu.Unlock()
	h.poll()
	if status := store.launchStatus(t); status.Phase != AgentPhaseStopped || status.CredentialSHA256 != "" {
		t.Fatalf("stopped status %+v", status)
	}
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T left after stop: %v", object, err)
		}
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName + "-credential"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("credential left after stop: %v", err)
	}
}

func TestLaunchesRunOnTheDefaultRuntimeImageWithoutASpecImage(t *testing.T) {
	lake := agentsTestLake()
	lake.Spec.Agents.RuntimeImage = ""
	store := &fakeServiceStore{launches: []LakeAgentLaunch{testLaunch(agentLaunchModeJob)}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	if status := store.launchStatus(t); status.Phase == AgentPhaseFailed {
		t.Fatalf("status %+v", status)
	}
	var job batchv1.Job
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: testLaunchName}, &job); err != nil {
		t.Fatalf("launch job: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != lakeDefaultAgentRuntimeImage {
		t.Fatalf("launch runs %q, want the default %q", got, lakeDefaultAgentRuntimeImage)
	}
}
