/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testDeploymentID   = "88888888-8888-4888-8888-888888888888"
	testDeploymentSlug = "md-888888888888"
)

// --- the fake store's served models half ---------------------------------------------------------

func (f *fakeServiceStore) ListMethodologyDeployments(context.Context) ([]LakeMethodologyDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deploymentsMissing {
		return nil, errMethodologyDeploymentsTableMissing
	}
	out := make([]LakeMethodologyDeployment, 0, len(f.deployments))
	for _, deployment := range f.deployments {
		if deployment.Status != nil {
			status := *deployment.Status
			deployment.Status = &status
		}
		if deployment.Build != nil {
			build := *deployment.Build
			deployment.Build = &build
		}
		out = append(out, deployment)
	}
	return out, nil
}

func (f *fakeServiceStore) UpsertMethodologyDeploymentStatus(_ context.Context, status LakeMethodologyDeploymentStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploymentStatusUpserts = append(f.deploymentStatusUpserts, status)
	for i := range f.deployments {
		if f.deployments[i].ID == status.DeploymentID {
			copied := status
			f.deployments[i].Status = &copied
		}
	}
	return nil
}

func (f *fakeServiceStore) deploymentStatus(t *testing.T) LakeMethodologyDeploymentStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deployments) == 0 || f.deployments[0].Status == nil {
		t.Fatalf("the deployment has no status")
	}
	return *f.deployments[0].Status
}

// --- fixtures ------------------------------------------------------------------------------------

func testMethodologyDeployment() LakeMethodologyDeployment {
	return LakeMethodologyDeployment{ID: testDeploymentID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		MethodologyID: "W-0123abcd", ModelID: "model-1", BuildID: testBuildID, RuntimeImage: testRuntimeImg,
		CPUMillis: 1000, MemoryBytes: 2 << 30, Slug: testDeploymentSlug, Desired: serviceDesiredPresent, Generation: 1,
		Build: &LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
			Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"}}
}

func newDeploymentHarness(t *testing.T, edit func(*LakeMethodologyDeployment), objects ...client.Object) (*serviceHarness, *fakeServiceStore) {
	t.Helper()
	deployment := testMethodologyDeployment()
	if edit != nil {
		edit(&deployment)
	}
	store := &fakeServiceStore{deployments: []LakeMethodologyDeployment{deployment}, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, append([]client.Object{pullSecretForTest()}, objects...)...)
	return h, store
}

func (h *serviceHarness) servedDeployment() *appsv1.Deployment {
	h.t.Helper()
	served := &appsv1.Deployment{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug}, served); err != nil {
		h.t.Fatalf("serve Deployment: %v", err)
	}
	return served
}

func (h *serviceHarness) markServeReady() {
	h.t.Helper()
	served := h.servedDeployment()
	served.Status.ReadyReplicas, served.Status.Replicas = 1, 1
	if err := h.client.Status().Update(context.Background(), served); err != nil {
		if err := h.client.Update(context.Background(), served); err != nil {
			h.t.Fatal(err)
		}
	}
}

// advanceToReady stages the model and marks the serve pod ready.
func advanceToReady(t *testing.T, h *serviceHarness, store *fakeServiceStore) {
	t.Helper()
	h.poll()
	if status := store.deploymentStatus(t); status.Phase != DeploymentPhaseStaging || status.CredentialSHA256 == "" {
		t.Fatalf("staging: %+v", status)
	}
	h.finishJob(methodologyDeploymentStageJob(testDeploymentSlug), true)
	h.poll()
	if status := store.deploymentStatus(t); status.Phase != DeploymentPhaseStarting {
		t.Fatalf("starting: %+v", status)
	}
	h.markServeReady()
	h.poll()
	if status := store.deploymentStatus(t); status.Phase != DeploymentPhaseReady {
		t.Fatalf("ready: %+v", status)
	}
}

// --- the tests -----------------------------------------------------------------------------------

func TestAServedModelStagesThenServesAndSaysWhereToCallIt(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	ctx := context.Background()
	h.poll()
	status := store.deploymentStatus(t)
	if status.Phase != DeploymentPhaseStaging || status.Endpoint != "" || !h.changing {
		t.Fatalf("first pass: %+v", status)
	}
	stage := h.job(methodologyDeploymentStageJob(testDeploymentSlug))
	command := strings.Join(stage.Spec.Template.Spec.Containers[0].Command, " ")
	if stage.Spec.Template.Spec.Containers[0].Image != testRuntimeImg || !strings.Contains(command, "stage-model") ||
		!strings.Contains(command, "--deployment $(DEPLOYMENT_ID)") || !strings.Contains(command, "--out /work/model") {
		t.Fatalf("stage: %s %s", stage.Spec.Template.Spec.Containers[0].Image, command)
	}
	if envValue(t, stage.Spec.Template.Spec.Containers[0].Env, "DEPLOYMENT_ID").Value != testDeploymentID {
		t.Fatal("the stage is told its deployment")
	}
	var credential corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug + "-credential"}, &credential); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(credential.Data[agentCredentialKey]), "lmd."+testDeploymentID+".") ||
		credentialDigest(string(credential.Data[agentCredentialKey])) != status.CredentialSHA256 {
		t.Fatal("the credential is lmd.<deployment id>.<secret> and lake holds only its digest")
	}
	var pull corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug + "-pull"}, &pull); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pull.Data[corev1.DockerConfigJsonKey]), "lake.example.test") {
		t.Fatal("the pull Secret names lake's registry")
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("nothing serves before the model is in: %v", err)
	}

	h.finishJob(methodologyDeploymentStageJob(testDeploymentSlug), true)
	h.poll()
	status = store.deploymentStatus(t)
	wantEndpoint := "http://" + testDeploymentSlug + "." + testRunNS + ".svc.cluster.local"
	if status.Phase != DeploymentPhaseStarting || status.Endpoint != wantEndpoint {
		t.Fatalf("starting: %+v", status)
	}
	var service corev1.Service
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug}, &service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.Ports[0].Port != 80 || service.Spec.Selector[methodologyDeploymentIDLabel] != testDeploymentID ||
		service.Spec.Selector[methodologyRunRoleLabel] != methodologyRoleServe {
		t.Fatalf("service %+v", service.Spec)
	}

	h.markServeReady()
	h.poll()
	status = store.deploymentStatus(t)
	if status.Phase != DeploymentPhaseReady || status.ReadyAt == nil || status.CredentialSHA256 == "" {
		t.Fatalf("ready: %+v", status)
	}
	// A steady deployment writes nothing more.
	writes := len(store.deploymentStatusUpserts)
	h.poll()
	if len(store.deploymentStatusUpserts) != writes {
		t.Fatal("a ready deployment's unchanged status is not written again")
	}
}

func TestTheServePodHoldsNoCredentialAndRunsLocked(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	advanceToReady(t, h, store)
	served := h.servedDeployment()
	pod := served.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != "lake.example.test/acme/churn-methodology@"+testBuildDigest {
		t.Fatalf("image %s", container.Image)
	}
	if !strings.Contains(strings.Join(container.Command, " "), "serve --model /work/model --source /methodology --port 8080") {
		t.Fatalf("command %v", container.Command)
	}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil || volume.Projected != nil {
			t.Fatalf("the serve pod mounts no Secret: %+v", volume)
		}
		if volume.PersistentVolumeClaim != nil && (!volume.PersistentVolumeClaim.ReadOnly || volume.PersistentVolumeClaim.ClaimName != testDeploymentSlug+"-model") {
			t.Fatalf("the model volume is read-only: %+v", volume)
		}
	}
	for _, variable := range container.Env {
		if variable.ValueFrom != nil || strings.HasPrefix(variable.Name, "LAKE_") || strings.HasPrefix(variable.Name, "ZETTA_") {
			t.Fatalf("the serve pod is told nothing secret: %+v", variable)
		}
	}
	if *pod.AutomountServiceAccountToken || *pod.EnableServiceLinks || !*container.SecurityContext.ReadOnlyRootFilesystem ||
		*container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("the serve pod runs locked down")
	}
	cpu, memory := container.Resources.Requests[corev1.ResourceCPU], container.Resources.Limits[corev1.ResourceMemory]
	if cpu.MilliValue() != 1000 || memory.Value() != 2<<30 || !container.Resources.Requests.Memory().Equal(*container.Resources.Limits.Memory()) {
		t.Fatalf("requests equal limits, from the row: %+v", container.Resources)
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Path != "/healthz" ||
		pod.RestartPolicy != corev1.RestartPolicyAlways || served.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("the serve pod is probed and restarted, one at a time")
	}
	if served.Spec.Template.Labels[methodologyRunNetworkLabel] != methodologyNetworkDenied {
		t.Fatal("no network unless granted")
	}
}

func TestOnlyLakeReachesAServePodAndAGrantedOneReachesTheInternet(t *testing.T) {
	h, store := newDeploymentHarness(t, func(d *LakeMethodologyDeployment) { d.Network = true })
	advanceToReady(t, h, store)
	var policies networkingv1.NetworkPolicyList
	if err := h.client.List(context.Background(), &policies, client.InNamespace(testRunNS)); err != nil {
		t.Fatal(err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, policy := range policies.Items {
		byName[policy.Name] = policy
	}
	selects := func(policy networkingv1.NetworkPolicy, podLabels map[string]string) bool {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		return selector.Matches(labels.Set(podLabels))
	}
	serve := h.servedDeployment().Spec.Template.Labels
	stage := h.job(methodologyDeploymentStageJob(testDeploymentSlug)).Spec.Template.Labels
	ingress := byName[methodologyServeIngress]
	if !selects(ingress, serve) || selects(ingress, stage) || len(ingress.Spec.Ingress) != 1 || len(ingress.Spec.Egress) != 0 {
		t.Fatalf("serve ingress %+v", ingress.Spec)
	}
	from := ingress.Spec.Ingress[0].From[0]
	if from.PodSelector.MatchLabels["app.kubernetes.io/name"] != "lake" || from.IPBlock != nil ||
		ingress.Spec.Ingress[0].Ports[0].Port.IntValue() != 8080 {
		t.Fatalf("only lake's pods, on the serve port: %+v", ingress.Spec.Ingress)
	}
	if !selects(byName[methodologySandboxInternet], serve) || selects(byName[methodologyTransferPolicy], serve) {
		t.Fatal("a granted serve pod reaches the internet, never lake")
	}
	if !selects(byName[methodologyTransferPolicy], stage) {
		t.Fatal("the model's stage reaches lake through the transfer policy")
	}

	h2, store2 := newDeploymentHarness(t, nil)
	advanceToReady(t, h2, store2)
	if selects(byName[methodologySandboxInternet], h2.servedDeployment().Spec.Template.Labels) {
		t.Fatal("a serve pod without a grant reaches no network")
	}
}

func TestAServerThatKeepsExitingFailsWithWhatItSaid(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	h.poll()
	h.finishJob(methodologyDeploymentStageJob(testDeploymentSlug), true)
	h.poll()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-x", Namespace: testRunNS,
			Labels: map[string]string{methodologyDeploymentIDLabel: testDeploymentID, methodologyRunRoleLabel: methodologyRoleServe,
				methodologyRunLakeLabel: "prod"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "methodology", RestartCount: 3,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3,
				Message: `{"serving": false, "findings": [{"check": "sandbox.credentials", "ok": false}]}`}}}}},
	}
	h.create(pod)
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		_ = h.client.Update(context.Background(), pod)
	}
	h.poll()
	status := store.deploymentStatus(t)
	if status.Phase != DeploymentPhaseFailed || !strings.Contains(status.Message, "exited with 3") ||
		!strings.Contains(status.Message, "sandbox.credentials") || status.CredentialSHA256 != "" || status.Endpoint != "" {
		t.Fatalf("failed: %+v", status)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: testDeploymentSlug}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a failed deployment's objects go: %v", err)
	}
}

func TestUnpublishingStopsAndRemovesEverything(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	advanceToReady(t, h, store)
	store.mu.Lock()
	store.deployments[0].Desired = serviceDesiredAbsent
	store.deployments[0].Generation++
	store.mu.Unlock()
	h.poll()
	status := store.deploymentStatus(t)
	if status.Phase != DeploymentPhaseStopped || status.CredentialSHA256 != "" || status.Endpoint != "" || status.StoppedAt == nil {
		t.Fatalf("stopped: %+v", status)
	}
	ctx := context.Background()
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-stage"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-credential"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-pull"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-model"}},
	} {
		if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: object.GetName()}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T %s remains: %v", object, object.GetName(), err)
		}
	}
	writes := len(store.deploymentStatusUpserts)
	h.poll()
	if len(store.deploymentStatusUpserts) != writes {
		t.Fatal("a stopped deployment is left alone")
	}
}

func TestAGPUModelWaitsForItsClassAndStagesInItsZones(t *testing.T) {
	gpu := func(d *LakeMethodologyDeployment) { d.GPUs, d.GPUClass = 1, "a100" }
	h, store := newDeploymentHarness(t, gpu)
	h.poll()
	if status := store.deploymentStatus(t); status.Phase != DeploymentPhaseQueued || !strings.Contains(status.Message, "a100") ||
		!strings.Contains(status.Message, "the model is served") {
		t.Fatalf("queued: %+v", status)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{
		gpuClassLabel: "a100", corev1.LabelTopologyZone: "eastus-2"}}}
	h.create(node)
	h.poll()
	if status := store.deploymentStatus(t); status.Phase != DeploymentPhaseStaging {
		t.Fatalf("staging: %+v", status)
	}
	stage := h.job(methodologyDeploymentStageJob(testDeploymentSlug)).Spec.Template.Spec
	if stage.Affinity == nil || stage.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].
		MatchExpressions[0].Values[0] != "eastus-2" {
		t.Fatalf("the model volume goes where the GPUs are: %+v", stage.Affinity)
	}
	h.finishJob(methodologyDeploymentStageJob(testDeploymentSlug), true)
	h.poll()
	served := h.servedDeployment().Spec.Template.Spec
	gpus := served.Containers[0].Resources.Limits[resourceNvidiaGPU]
	if served.NodeSelector[gpuClassLabel] != "a100" || gpus.Value() != 1 {
		t.Fatalf("serve pod GPUs %+v %v", served.NodeSelector, served.Containers[0].Resources)
	}
}

func TestAServedModelIsMeteredWhileItsPodsRun(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	advanceToReady(t, h, store)
	served := h.servedDeployment()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-abc", Namespace: testRunNS, Labels: served.Spec.Template.Labels},
		Spec:       served.Spec.Template.Spec,
	}
	pod.Spec.NodeName = "node-1"
	h.create(pod)
	h.resourcesPass()
	row := h.row(WorkloadKindMethodologyDeployment, testDeploymentID)
	if row.Pods != 1 || row.CPUMillis != 1000 || row.MemoryBytes != 2<<30 {
		t.Fatalf("serve row %+v", row)
	}
	// The stage's pod is the operator's transfer, not the workspace's workload.
	stage := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testDeploymentSlug + "-stage-x", Namespace: testRunNS,
		Labels: h.job(methodologyDeploymentStageJob(testDeploymentSlug)).Spec.Template.Labels},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "m", Image: "x",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}}}}
	h.create(stage)
	h.resourcesPass()
	if row := h.row(WorkloadKindMethodologyDeployment, testDeploymentID); row.CPUMillis != 1000 {
		t.Fatalf("the stage is not counted: %+v", row)
	}
}

func TestDeploymentsAreSkippedBeforeTheirMigration(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	store.mu.Lock()
	store.deploymentsMissing = true
	store.mu.Unlock()
	h.poll()
	if len(store.deploymentStatusUpserts) != 0 {
		t.Fatal("a schema before 0083 writes no deployment status")
	}
}
