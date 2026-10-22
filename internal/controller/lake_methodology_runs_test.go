/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testRunID       = "77777777-7777-4777-8777-777777777777"
	testRunSlug     = "mr-777777777777"
	testRunNS       = "lake-prod-methodology-runs"
	testRuntimeImg  = "oramasearch/methodology-runtime@sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	testBuildDigest = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

// --- the fake store's methodology runs half ------------------------------------------------------

func (f *fakeServiceStore) ListMethodologyRuns(context.Context) ([]LakeMethodologyRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runsMissing {
		return nil, errMethodologyRunsTableMissing
	}
	out := make([]LakeMethodologyRun, 0, len(f.methodologyRuns))
	for _, run := range f.methodologyRuns {
		if run.Status != nil {
			status := *run.Status
			run.Status = &status
		}
		if run.Build != nil {
			build := *run.Build
			run.Build = &build
		}
		out = append(out, run)
	}
	return out, nil
}

func (f *fakeServiceStore) UpsertMethodologyRunStatus(_ context.Context, status LakeMethodologyRunStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runStatusUpserts = append(f.runStatusUpserts, status)
	for i := range f.methodologyRuns {
		if f.methodologyRuns[i].ID == status.RunID {
			copied := status
			f.methodologyRuns[i].Status = &copied
		}
	}
	return nil
}

func (f *fakeServiceStore) runStatus(t *testing.T) LakeMethodologyRunStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.methodologyRuns) == 0 || f.methodologyRuns[0].Status == nil {
		t.Fatalf("the run has no status")
	}
	return *f.methodologyRuns[0].Status
}

func (f *fakeServiceStore) setBuild(build LakeMethodologyRunBuild) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methodologyRuns[0].Build = &build
}

func (f *fakeServiceStore) setRunDesired(desired string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methodologyRuns[0].Desired = desired
	f.methodologyRuns[0].Generation++
}

// --- fixtures ------------------------------------------------------------------------------------

func methodologyTestLake() *oramav1alpha1.Lake {
	lake := serviceTestLake()
	lake.Spec.Host = "lake.example.test"
	lake.Spec.Builds = oramav1alpha1.LakeBuildsSpec{BuilderImage: "oramasearch/lake-builder:test"}
	lake.Spec.Methodologies = oramav1alpha1.LakeMethodologiesSpec{RuntimeImage: testRuntimeImg}
	return lake
}

func testMethodologyRun() LakeMethodologyRun {
	return LakeMethodologyRun{ID: testRunID, CustomerID: testServiceCustomer, WorkspaceID: testWorkspaceID,
		MethodologyID: "W-0123abcd", Mode: methodologyModeTrain, BuildID: testBuildID,
		RuntimeImage: testRuntimeImg, CPUMillis: 2000, MemoryBytes: 4 << 30, TimeoutSeconds: 900,
		Slug: testRunSlug, Desired: serviceDesiredPresent, Generation: 1,
		Build: &LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseRunning,
			Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"}}
}

func pullSecretForTest() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oramasearch-dockerhub", Namespace: testServiceNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"https://index.docker.io/v1/":{"username":"orama","password":"hub"}}}`)},
	}
}

func (h *serviceHarness) job(name string) *batchv1.Job {
	h.t.Helper()
	job := &batchv1.Job{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: name}, job); err != nil {
		h.t.Fatalf("Job %s: %v", name, err)
	}
	return job
}

func (h *serviceHarness) finishJob(name string, succeeded bool, conditions ...batchv1.JobCondition) {
	h.t.Helper()
	job := h.job(name)
	if succeeded {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
	}
	job.Status.Conditions = conditions
	if err := h.client.Status().Update(context.Background(), job); err != nil {
		if err := h.client.Update(context.Background(), job); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *serviceHarness) sandboxPod(exitCode int32, started, finished time.Time) {
	h.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: methodologySandboxJob(testRunSlug) + "-x", Namespace: testRunNS,
			Labels: map[string]string{"job-name": methodologySandboxJob(testRunSlug)}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "methodology", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode,
				StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(finished)}}}}},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		_ = h.client.Update(context.Background(), pod)
	}
}

func newMethodologyHarness(t *testing.T, network bool) (*serviceHarness, *fakeServiceStore) {
	t.Helper()
	run := testMethodologyRun()
	run.Network = network
	store := &fakeServiceStore{methodologyRuns: []LakeMethodologyRun{run}, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	return h, store
}

// advanceToSandbox succeeds the build and the stage, returning the sandbox Job.
func advanceToSandbox(t *testing.T, h *serviceHarness, store *fakeServiceStore) *batchv1.Job {
	t.Helper()
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseStaging {
		t.Fatalf("after the build: %+v", status)
	}
	h.finishJob(methodologyStageJob(testRunSlug), true)
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseRunning {
		t.Fatalf("after the stage: %+v", status)
	}
	return h.job(methodologySandboxJob(testRunSlug))
}

// --- the tests -----------------------------------------------------------------------------------

func TestAMethodologyRunWaitsForItsBuildAndFailsWithIt(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseBuilding || status.CredentialSHA256 != "" || !h.changing {
		t.Fatalf("waiting for the build: %+v (changing %v)", status, h.changing)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: methodologyStageJob(testRunSlug)},
		&batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("nothing runs before the image exists: %v", err)
	}

	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseFailed,
		Message: "pip: hash mismatch for numpy", Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	status = store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed || status.Message != "the methodology's image build failed: pip: hash mismatch for numpy" ||
		status.StartedAt != nil || status.FinishedAt != nil {
		t.Fatalf("after a failed build: %+v", status)
	}
	// A finished run stays finished.
	h.poll()
	if got := store.runStatus(t); got.Phase != MethodologyPhaseFailed {
		t.Fatalf("terminal phase changed: %+v", got)
	}
}

func TestACancelledBuildOrNoBuildFailsTheRun(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredCancelled, Phase: BuildPhaseQueued})
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseFailed || status.Message != "the methodology's image build was cancelled" {
		t.Fatalf("cancelled build: %+v", status)
	}

	h2, store2 := newMethodologyHarness(t, false)
	store2.mu.Lock()
	store2.methodologyRuns[0].Build = nil
	store2.mu.Unlock()
	h2.poll()
	if status := store2.runStatus(t); status.Phase != MethodologyPhaseFailed || !strings.Contains(status.Message, "no image build") {
		t.Fatalf("no build: %+v", status)
	}
}

func TestRunsUseTheDefaultRuntimeImageWithoutASpecImage(t *testing.T) {
	run := testMethodologyRun()
	store := &fakeServiceStore{methodologyRuns: []LakeMethodologyRun{run}}
	lake := methodologyTestLake()
	lake.Spec.Methodologies.RuntimeImage = ""
	h := newServiceHarness(t, lake, store, pullSecretForTest())
	h.poll()
	// Every Lake runs methodologies (operator#539): a run is not refused for want of a spec image.
	if status := store.runStatus(t); status.Phase == MethodologyPhaseFailed {
		t.Fatalf("status %+v", status)
	}
	if got := envValue(t, lakeEnv(lake, lakeSealingMaterial{}), "LAKE_METHODOLOGY_RUNTIME_IMAGE").Value; got != lakeDefaultMethodologyRuntimeImage {
		t.Fatalf("a Lake without a spec image is told %q, want the default", got)
	}
	if got := envValue(t, lakeEnv(methodologyTestLake(), lakeSealingMaterial{}), "LAKE_METHODOLOGY_RUNTIME_IMAGE").Value; got != testRuntimeImg {
		t.Fatalf("LAKE_METHODOLOGY_RUNTIME_IMAGE = %q", got)
	}
	if !strings.Contains(buildArgAllow, "METHODOLOGY_RUNTIME_IMAGE") {
		t.Fatal("the builder must accept the METHODOLOGY_RUNTIME_IMAGE build argument")
	}
}

func TestTheMethodologyRunsNamespaceDeniesEverythingButLakeForTransfers(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	advanceToSandbox(t, h, store)
	ctx := context.Background()
	var namespace corev1.Namespace
	if err := h.client.Get(ctx, client.ObjectKey{Name: testRunNS}, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" ||
		namespace.Annotations[methodologyOwnerAnnotation] != "prod" {
		t.Fatalf("namespace %+v", namespace.ObjectMeta)
	}
	var policies networkingv1.NetworkPolicyList
	if err := h.client.List(ctx, &policies, client.InNamespace(testRunNS)); err != nil {
		t.Fatal(err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, policy := range policies.Items {
		byName[policy.Name] = policy
	}
	deny := byName[methodologyDefaultDeny]
	if len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PolicyTypes) != 2 || deny.Spec.Egress != nil || deny.Spec.Ingress != nil {
		t.Fatalf("default deny %+v", deny.Spec)
	}
	transfer := byName[methodologyTransferPolicy]
	if len(transfer.Spec.Egress) != 2 || transfer.Spec.Egress[1].To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "lake" {
		t.Fatalf("transfer egress %+v", transfer.Spec.Egress)
	}
	for _, rule := range transfer.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Fatal("a transfer pod reaches lake and DNS only, never an address range")
			}
		}
	}
	if len(transfer.Spec.Ingress) != 0 || len(byName[methodologySandboxInternet].Spec.Ingress) != 0 {
		t.Fatal("no policy lets anything in")
	}
	// Which policies select which pod: the sandbox is selected only by the default deny.
	selects := func(policy networkingv1.NetworkPolicy, podLabels map[string]string) bool {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		return selector.Matches(labels.Set(podLabels))
	}
	sandbox := h.job(methodologySandboxJob(testRunSlug)).Spec.Template.Labels
	stage := h.job(methodologyStageJob(testRunSlug)).Spec.Template.Labels
	if selects(transfer, sandbox) || selects(byName[methodologySandboxInternet], sandbox) || !selects(deny, sandbox) {
		t.Fatal("a sandbox without a network grant may reach nothing at all")
	}
	if !selects(transfer, stage) || selects(byName[methodologySandboxInternet], stage) {
		t.Fatal("the stage pod reaches lake through the transfer policy only")
	}
}

func TestTheSandboxCarriesNoCredentialAndRunsLocked(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	ctx := context.Background()
	sandbox := advanceToSandbox(t, h, store)

	var credential corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testRunSlug + "-credential"}, &credential); err != nil {
		t.Fatalf("credential: %v", err)
	}
	token := string(credential.Data["token"])
	if !strings.HasPrefix(token, "lmr."+testRunID+".") || len(token) != len("lmr.")+36+1+32 {
		t.Fatalf("credential %q", token)
	}
	if status := store.runStatus(t); status.CredentialSHA256 != credentialDigest(token) {
		t.Fatal("the status carries the credential's digest")
	}

	// The stage pod: the runtime image, the credential as a file, lake's address.
	stage := h.job(methodologyStageJob(testRunSlug)).Spec.Template.Spec
	if stage.Containers[0].Image != testRuntimeImg {
		t.Fatalf("stage image %q", stage.Containers[0].Image)
	}
	if got := strings.Join(stage.Containers[0].Command, " "); got != "/opt/orama/bin/methodology stage --lake $(LAKE_URL) --run $(RUN_ID) --credential-file /var/run/orama/credential --out /work/in" {
		t.Fatalf("stage command %q", got)
	}
	if envValue(t, stage.Containers[0].Env, "LAKE_URL").Value != "http://lake-prod.lake.svc.cluster.local:80" ||
		envValue(t, stage.Containers[0].Env, "RUN_ID").Value != testRunID {
		t.Fatalf("stage env %v", stage.Containers[0].Env)
	}
	hasCredential := false
	for _, volume := range stage.Volumes {
		if volume.Secret != nil && volume.Secret.SecretName == testRunSlug+"-credential" {
			hasCredential = true
		}
	}
	if !hasCredential {
		t.Fatal("the stage pod reads the run's credential")
	}

	// The sandbox: the built image, nothing secret, locked down.
	pod := sandbox.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != "lake.example.test/acme/churn-methodology@"+testBuildDigest {
		t.Fatalf("sandbox image %q", container.Image)
	}
	if got := strings.Join(container.Command, " "); got != "/opt/orama/bin/methodology run --in /work/in --out /work/out --source /methodology" {
		t.Fatalf("sandbox command %q", got)
	}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil || volume.Projected != nil {
			t.Fatalf("the sandbox mounts a Secret: %+v", volume)
		}
	}
	for _, variable := range container.Env {
		if variable.ValueFrom != nil || strings.HasPrefix(variable.Name, "LAKE_") || strings.HasPrefix(variable.Name, "ZETTA_") ||
			strings.Contains(variable.Value, token) {
			t.Fatalf("the sandbox is told %s", variable.Name)
		}
	}
	if len(container.EnvFrom) != 0 {
		t.Fatal("the sandbox takes no environment from anything")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("the sandbox has no service account token")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Fatal("the sandbox is not told the namespace's services")
	}
	security := container.SecurityContext
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.RunAsUser != 10001 ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		!*security.ReadOnlyRootFilesystem || *security.AllowPrivilegeEscalation ||
		len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("sandbox security %+v %+v", pod.SecurityContext, security)
	}
	cpu, memory := container.Resources.Requests[corev1.ResourceCPU], container.Resources.Requests[corev1.ResourceMemory]
	if cpu.MilliValue() != 2000 || memory.Value() != 4<<30 {
		t.Fatalf("requests %v", container.Resources.Requests)
	}
	for name, request := range container.Resources.Requests {
		if limit := container.Resources.Limits[name]; limit.Cmp(request) != 0 {
			t.Fatalf("%s: request %v, limit %v", name, request, limit)
		}
	}
	if *sandbox.Spec.ActiveDeadlineSeconds != 900 || *sandbox.Spec.BackoffLimit != 0 || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("the code gets its time limit and is never retried")
	}
	if sandbox.Spec.Template.Labels[methodologyRunNetworkLabel] != methodologyNetworkDenied {
		t.Fatal("a run without a grant is labelled denied")
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != testRunSlug+"-pull" {
		t.Fatalf("pull secrets %v", pod.ImagePullSecrets)
	}
	var pull corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testRunSlug + "-pull"}, &pull); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(pull.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatal(err)
	}
	if entry := config.Auths["lake.example.test"]; entry["username"] != "methodology-run" || entry["password"] != token {
		t.Fatalf("lake registry entry %v", entry)
	}
	if _, found := config.Auths["https://index.docker.io/v1/"]; !found {
		t.Fatal("the Lake's own pull Secret entries are kept for the runtime image")
	}
	var claim corev1.PersistentVolumeClaim
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: testRunSlug + "-work"}, &claim); err != nil {
		t.Fatal(err)
	}
	if size := claim.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("work volume %v", size)
	}
}

func TestAGrantedRunReachesOnlyThePublicInternet(t *testing.T) {
	h, store := newMethodologyHarness(t, true)
	sandbox := advanceToSandbox(t, h, store)
	if sandbox.Spec.Template.Labels[methodologyRunNetworkLabel] != methodologyNetworkGranted {
		t.Fatal("a granted run is labelled granted")
	}
	var policy networkingv1.NetworkPolicy
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: methodologySandboxInternet}, &policy); err != nil {
		t.Fatal(err)
	}
	selector, _ := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if !selector.Matches(labels.Set(sandbox.Spec.Template.Labels)) {
		t.Fatal("the internet policy selects a granted sandbox")
	}
	var block *networkingv1.IPBlock
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				block = peer.IPBlock
			}
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == "lake" {
				t.Fatal("a sandbox never reaches lake")
			}
		}
	}
	if block == nil || block.CIDR != "0.0.0.0/0" {
		t.Fatalf("internet block %+v", block)
	}
	for _, private := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"} {
		found := false
		for _, except := range block.Except {
			found = found || except == private
		}
		if !found {
			t.Fatalf("%s is not excepted: cluster, node and metadata addresses stay out of reach", private)
		}
	}
}

func TestARunSucceedsOnlyAfterItsResultIsCollected(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	advanceToSandbox(t, h, store)
	started := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	finished := started.Add(95 * time.Second)
	h.sandboxPod(0, started, finished)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseRunning || status.StartedAt == nil || !status.StartedAt.Equal(started) {
		t.Fatalf("running: %+v", status)
	}
	h.finishJob(methodologySandboxJob(testRunSlug), true)
	h.poll()
	status = store.runStatus(t)
	if status.Phase != MethodologyPhaseCollecting || !status.FinishedAt.Equal(finished) || status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("collecting: %+v", status)
	}
	if status.CredentialSHA256 == "" {
		t.Fatal("the collect pod still needs the credential")
	}
	row, found := store.resources[workloadKey{WorkloadKindMethodologyRun, testRunID}]
	if !found || row.Pods != 0 || row.CPUMillis != 2000 || row.MemoryBytes != 4<<30 {
		t.Fatalf("the sandbox's resources are recorded once its code finishes: %+v", row)
	}
	collect := h.job(methodologyCollectJob(testRunSlug)).Spec.Template.Spec.Containers[0]
	if got := strings.Join(collect.Command, " "); got != "/opt/orama/bin/methodology collect --lake $(LAKE_URL) --run $(RUN_ID) --credential-file /var/run/orama/credential --in /work/out --run-exit-code 0" {
		t.Fatalf("collect command %q", got)
	}
	h.finishJob(methodologyCollectJob(testRunSlug), true)
	h.poll()
	status = store.runStatus(t)
	if status.Phase != MethodologyPhaseSucceeded || status.CredentialSHA256 != "" || !status.StartedAt.Equal(started) || !status.FinishedAt.Equal(finished) {
		t.Fatalf("succeeded: %+v", status)
	}
	ctx := context.Background()
	for _, object := range []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: methodologySandboxJob(testRunSlug), Namespace: testRunNS}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testRunSlug + "-credential", Namespace: testRunNS}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testRunSlug + "-work", Namespace: testRunNS}},
	} {
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("%s is removed once the run has reported: %v", object.GetName(), err)
		}
	}
}

func TestAFailingCodeIsStillCollectedThenFails(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	advanceToSandbox(t, h, store)
	started := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.sandboxPod(3, started, started.Add(10*time.Second))
	h.finishJob(methodologySandboxJob(testRunSlug), false, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"})
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseCollecting {
		t.Fatalf("a failed run's findings are still reported: %+v", status)
	}
	if got := strings.Join(h.job(methodologyCollectJob(testRunSlug)).Spec.Template.Spec.Containers[0].Command, " "); !strings.HasSuffix(got, "--run-exit-code 3") {
		t.Fatalf("collect is told the code's exit code: %q", got)
	}
	h.finishJob(methodologyCollectJob(testRunSlug), true)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed || status.Message != "the methodology's code exited with 3" || *status.ExitCode != 3 || status.CredentialSHA256 != "" {
		t.Fatalf("failed: %+v", status)
	}
}

func TestACodeOverItsTimeLimitFails(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	advanceToSandbox(t, h, store)
	ended := metav1.NewTime(time.Date(2026, 10, 11, 12, 15, 0, 0, time.UTC))
	h.finishJob(methodologySandboxJob(testRunSlug), false, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "DeadlineExceeded", LastTransitionTime: ended})
	h.poll()
	h.finishJob(methodologyCollectJob(testRunSlug), true)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed || status.Message != "the methodology's code ran past its time limit (900 seconds)" ||
		status.FinishedAt == nil || !status.FinishedAt.Equal(ended.Time) {
		t.Fatalf("deadline: %+v", status)
	}
}

func TestAStageOrCollectFailureFailsTheRun(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	store.setBuild(LakeMethodologyRunBuild{Phase: BuildPhaseSucceeded, Digest: testBuildDigest, Target: "lake-prod.lake.svc/acme/m:b"})
	h.poll()
	h.finishJob(methodologyStageJob(testRunSlug), false, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "lake answered 403"})
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed || status.Message != "could not bring the data in: lake answered 403" || status.StartedAt != nil {
		t.Fatalf("stage failure: %+v", status)
	}

	h2, store2 := newMethodologyHarness(t, false)
	advanceToSandbox(t, h2, store2)
	h2.finishJob(methodologySandboxJob(testRunSlug), true)
	h2.poll()
	h2.finishJob(methodologyCollectJob(testRunSlug), false)
	h2.poll()
	if status := store2.runStatus(t); status.Phase != MethodologyPhaseFailed || !strings.HasPrefix(status.Message, "could not report the result to Lake") {
		t.Fatalf("collect failure: %+v", status)
	}
}

// failedPod leaves a failed pod of Job `job` that said `message` (its termination message, or its
// log's tail under TerminationMessageFallbackToLogsOnError).
func (h *serviceHarness) failedPod(job, message, reason string) {
	h.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job + "-x", Namespace: testRunNS, Labels: map[string]string{"job-name": job}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "methodology", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 4, Message: message, Reason: reason}}}}},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		_ = h.client.Update(context.Background(), pod)
	}
}

// lake#802: a failed stage or collect says what its pod said, not only that the Job gave up; the
// pod is read before the run's objects, and so the pod, are removed.
func TestAStageOrCollectFailureCarriesThePodsOwnWords(t *testing.T) {
	backoff := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		Message: "Job has reached the specified backoff limit"}
	staged := func() (*serviceHarness, *fakeServiceStore) {
		h, store := newMethodologyHarness(t, false)
		store.setBuild(LakeMethodologyRunBuild{Phase: BuildPhaseSucceeded, Digest: testBuildDigest, Target: "lake-prod.lake.svc/acme/m:b"})
		h.poll()
		return h, store
	}

	h, store := staged()
	h.failedPod(methodologyStageJob(testRunSlug), "stage failed: methodology_run_input could not reach Lake:\n  timed out\n", "Error")
	h.finishJob(methodologyStageJob(testRunSlug), false, backoff)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed ||
		status.Message != "could not bring the data in: stage failed: methodology_run_input could not reach Lake: timed out" {
		t.Fatalf("stage failure: %+v", status)
	}

	// A pod that wrote nothing still says how it ended.
	h2, store2 := newMethodologyHarness(t, false)
	advanceToSandbox(t, h2, store2)
	h2.finishJob(methodologySandboxJob(testRunSlug), true)
	h2.poll()
	h2.failedPod(methodologyCollectJob(testRunSlug), "", "OOMKilled")
	h2.finishJob(methodologyCollectJob(testRunSlug), false, backoff)
	h2.poll()
	if status := store2.runStatus(t); status.Message != "could not report the result to Lake: OOMKilled" {
		t.Fatalf("collect failure: %+v", status)
	}

	// A long one keeps its end, cut to valid UTF-8.
	h3, store3 := staged()
	h3.failedPod(methodologyStageJob(testRunSlug), strings.Repeat("é", methodologyPodMessageLimit)+" the end", "Error")
	h3.finishJob(methodologyStageJob(testRunSlug), false, backoff)
	h3.poll()
	message := store3.runStatus(t).Message
	if !strings.HasSuffix(message, " the end") || len(message) > len("could not bring the data in: …")+methodologyPodMessageLimit ||
		!utf8.ValidString(message) {
		t.Fatalf("a long message keeps its valid end only: %d bytes", len(message))
	}

	// No pod left: the Job's own words.
	h4, store4 := staged()
	h4.finishJob(methodologyStageJob(testRunSlug), false, backoff)
	h4.poll()
	if status := store4.runStatus(t); status.Message != "could not bring the data in: Job has reached the specified backoff limit" {
		t.Fatalf("no pod: %+v", status)
	}
}

func TestStoppingARunRemovesItAndChargesWhatRan(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	advanceToSandbox(t, h, store)
	ctx := context.Background()
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-running", Namespace: testRunNS,
			Labels: map[string]string{"job-name": methodologySandboxJob(testRunSlug)}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "methodology", State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))}}}}},
	}
	if err := h.client.Create(ctx, running); err != nil {
		t.Fatal(err)
	}
	_ = h.client.Status().Update(ctx, running)
	h.poll()
	store.setRunDesired(serviceDesiredAbsent)
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseStopped || status.CredentialSHA256 != "" || status.StartedAt == nil || status.FinishedAt == nil ||
		status.ObservedGeneration != 2 {
		t.Fatalf("stopped: %+v", status)
	}
	if _, found := store.resources[workloadKey{WorkloadKindMethodologyRun, testRunID}]; !found {
		t.Fatal("a stopped run is charged for what it ran")
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologySandboxJob(testRunSlug)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the sandbox Job is removed: %v", err)
	}
	upserts := len(store.runStatusUpserts)
	h.poll()
	if len(store.runStatusUpserts) != upserts {
		t.Fatal("a stopped run writes nothing more")
	}
}

func TestMethodologyRunsSkipBeforeTheirSchema(t *testing.T) {
	store := &fakeServiceStore{runsMissing: true}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()
	if len(store.runStatusUpserts) != 0 {
		t.Fatal("nothing is written before 0078_methodology_runs")
	}
}

func TestTheWorkloadPassCountsOnlyTheSandbox(t *testing.T) {
	lake := methodologyTestLake()
	ix := &workloadIndex{lake: lake}
	run := testMethodologyRun()
	sandbox := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: methodologyLabels(lake, &run, methodologyRoleSandbox)}}
	if key, found := ix.classify(sandbox); !found || key != (workloadKey{WorkloadKindMethodologyRun, testRunID}) {
		t.Fatalf("sandbox = %v %v", key, found)
	}
	for _, role := range []string{methodologyRoleStage, methodologyRoleCollect} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: methodologyLabels(lake, &run, role)}}
		if _, found := ix.classify(pod); found {
			t.Fatalf("the %s pod is not the run's metered workload", role)
		}
	}
	if !(WorkloadObject{Kind: WorkloadKindMethodologyRun}).keepsFinishedValues() {
		t.Fatal("a run's row keeps what its sandbox ran with after it finishes")
	}
}

func TestMethodologyRunImageIsLakesPublicReferenceByDigest(t *testing.T) {
	lake := methodologyTestLake()
	build := &LakeMethodologyRunBuild{Phase: BuildPhaseSucceeded, Digest: testBuildDigest, Target: "lake-prod.lake.svc:80/acme/m:build-1"}
	if image, err := methodologyRunImage(lake, build); err != nil || image != "lake.example.test/acme/m@"+testBuildDigest {
		t.Fatalf("image %q, %v", image, err)
	}
	lake.Spec.Env = append(lake.Spec.Env, corev1.EnvVar{Name: "LAKE_PUBLIC_HOST", Value: "lake.oramadev.net"})
	if image, _ := methodologyRunImage(lake, build); image != "lake.oramadev.net/acme/m@"+testBuildDigest {
		t.Fatalf("LAKE_PUBLIC_HOST wins: %q", image)
	}
	if _, err := methodologyRunImage(lake, &LakeMethodologyRunBuild{Target: build.Target}); err == nil {
		t.Fatal("no digest, no image")
	}
	if got := methodologyRunsNamespace(strings.Repeat("a", 60)); len(got) > 63 {
		t.Fatalf("namespace %q is too long", got)
	}
}

// A run whose sandbox pod and Job are gone before any workload pass saw them (a short run, the Job's
// TTL, an operator restart) is still charged (lake#655, as lake#658 for builds and agent jobs): its
// workload_resources row is written from the sandbox pod spec before the Job is created, with no
// pod involved, and the end of the run neither needs the pod nor loses the row.
func TestARunWhosePodsAreGoneIsStillMetered(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	key := workloadKey{WorkloadKindMethodologyRun, testRunID}
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	if _, found := store.resources[key]; found {
		t.Fatal("nothing is recorded before the code is about to run")
	}
	h.finishJob(methodologyStageJob(testRunSlug), true)
	h.poll()
	row, found := store.resources[key]
	if !found || row.Pods != 0 || row.CPUMillis != 2000 || row.MemoryBytes != 4<<30 || row.GPUs != 0 {
		t.Fatalf("the run's resources are recorded as its sandbox Job is created: %+v (found %v)", row, found)
	}
	// The sandbox finishes and its pod is never seen: no pod exists in this harness at all.
	h.finishJob(methodologySandboxJob(testRunSlug), true, batchv1.JobCondition{Type: batchv1.JobComplete,
		Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 11, 12, 1, 0, 0, time.UTC))})
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseCollecting || status.StartedAt == nil || status.FinishedAt == nil {
		t.Fatalf("a run with no observed pod still has its times: %+v", status)
	}
	// A workload pass after the Job and its pods are gone keeps the row for lake to charge.
	if err := h.client.Delete(context.Background(), h.job(methodologySandboxJob(testRunSlug))); err != nil {
		t.Fatal(err)
	}
	if err := h.reconciler.reconcileWorkloadResources(context.Background(), h.lake, h.store); err != nil {
		t.Fatalf("reconcileWorkloadResources: %v", err)
	}
	if row, found := store.resources[key]; !found || row.CPUMillis != 2000 || row.MemoryBytes != 4<<30 {
		t.Fatalf("the row survives the pass once the pod and Job are gone: %+v (found %v)", row, found)
	}
}

// A row that cannot be written holds the code back: no run starts unmetered.
func TestARunDoesNotStartUntilItsResourcesAreRecorded(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	h.finishJob(methodologyStageJob(testRunSlug), true)
	store.mu.Lock()
	store.resourceUpsertErr = errors.New("connection reset")
	store.mu.Unlock()
	h.poll()
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: methodologySandboxJob(testRunSlug)},
		&batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the sandbox waits for its row: %v", err)
	}
	store.mu.Lock()
	store.resourceUpsertErr = nil
	store.mu.Unlock()
	h.poll()
	if _, found := store.resources[workloadKey{WorkloadKindMethodologyRun, testRunID}]; !found {
		t.Fatal("recorded on the next pass")
	}
	h.job(methodologySandboxJob(testRunSlug))
}
