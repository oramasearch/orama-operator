/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// orama-operator#293: a per-workspace cap on concurrent methodology runs (phase Queued), and a GPU
// run's work volume placed in a zone where its GPU class has nodes.

const testOtherWorkspaceID = "88888888-8888-4888-8888-888888888888"

// queuedTestRun is a run whose image build has succeeded, so only the queue holds it back.
func queuedTestRun(n int, workspace string) LakeMethodologyRun {
	run := testMethodologyRun()
	run.ID = fmt.Sprintf("77777777-7777-4777-8777-%012d", n)
	run.Slug = fmt.Sprintf("mr-%012x", n)
	run.WorkspaceID = workspace
	run.Build = &LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"}
	return run
}

func (f *fakeServiceStore) statusOf(t *testing.T, id string) LakeMethodologyRunStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, run := range f.methodologyRuns {
		if run.ID == id {
			if run.Status == nil {
				t.Fatalf("run %s has no status", id)
			}
			return *run.Status
		}
	}
	t.Fatalf("no run %s", id)
	return LakeMethodologyRunStatus{}
}

func (f *fakeServiceStore) stopRun(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.methodologyRuns {
		if f.methodologyRuns[i].ID == id {
			f.methodologyRuns[i].Desired = serviceDesiredAbsent
			f.methodologyRuns[i].Generation++
		}
	}
}

func stageJobExists(t *testing.T, h *serviceHarness, slug string) bool {
	t.Helper()
	err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: methodologyStageJob(slug)}, &batchv1.Job{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestAWorkspacesRunsPastItsCapAreQueuedOldestFirst(t *testing.T) {
	runs := []LakeMethodologyRun{
		queuedTestRun(1, testWorkspaceID), queuedTestRun(2, testWorkspaceID), queuedTestRun(3, testWorkspaceID),
		queuedTestRun(4, testOtherWorkspaceID), queuedTestRun(5, testWorkspaceID),
	}
	store := &fakeServiceStore{methodologyRuns: runs, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()

	for _, n := range []int{1, 2, 4} {
		if status := store.statusOf(t, runs[n-1].ID); status.Phase != MethodologyPhaseStaging {
			t.Fatalf("run %d is within its workspace's cap of 2: %+v", n, status)
		}
	}
	third := store.statusOf(t, runs[2].ID)
	if third.Phase != MethodologyPhaseQueued || third.CredentialSHA256 != "" ||
		third.Message != "Queued: 2 of this workspace's methodology runs are running (the limit is 2); this run is next." {
		t.Fatalf("the third run of the workspace waits, with no credential: %+v", third)
	}
	fifth := store.statusOf(t, runs[4].ID)
	if fifth.Phase != MethodologyPhaseQueued || !strings.HasSuffix(fifth.Message, "1 run is ahead of it.") {
		t.Fatalf("the fifth waits behind the third: %+v", fifth)
	}
	if stageJobExists(t, h, runs[2].Slug) || stageJobExists(t, h, runs[4].Slug) {
		t.Fatal("a queued run has no stage pod")
	}
	ctx := context.Background()
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyWorkClaim(runs[2].Slug)},
		&corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a queued run has no work volume: %v", err)
	}
	if !h.changing {
		t.Fatal("a queued run keeps the pass changing")
	}

	// A steady queue writes nothing new.
	writes := len(store.runStatusUpserts)
	h.poll()
	if len(store.runStatusUpserts) != writes {
		t.Fatalf("an unchanged queue rewrote status: %d writes", len(store.runStatusUpserts)-writes)
	}

	// The first run is stopped: the third takes its place in the same pass, the fifth moves up.
	store.stopRun(runs[0].ID)
	h.poll()
	if status := store.statusOf(t, runs[2].ID); status.Phase != MethodologyPhaseStaging || status.CredentialSHA256 == "" {
		t.Fatalf("the oldest queued run starts when a place frees: %+v", status)
	}
	if status := store.statusOf(t, runs[4].ID); status.Phase != MethodologyPhaseQueued || !strings.HasSuffix(status.Message, "this run is next.") {
		t.Fatalf("the fifth is next: %+v", status)
	}
}

func TestTheLakeSetsTheCapOnConcurrentRuns(t *testing.T) {
	runs := []LakeMethodologyRun{queuedTestRun(1, testWorkspaceID), queuedTestRun(2, testWorkspaceID)}
	store := &fakeServiceStore{methodologyRuns: runs, resources: map[workloadKey]WorkloadResources{}}
	lake := methodologyTestLake()
	lake.Spec.Methodologies.MaxConcurrentRunsPerWorkspace = 1
	h := newServiceHarness(t, lake, store, pullSecretForTest())
	h.poll()
	if status := store.statusOf(t, runs[0].ID); status.Phase != MethodologyPhaseStaging {
		t.Fatalf("first: %+v", status)
	}
	if status := store.statusOf(t, runs[1].ID); status.Phase != MethodologyPhaseQueued ||
		status.Message != "Queued: 1 of this workspace's methodology runs are running (the limit is 1); this run is next." {
		t.Fatalf("second: %+v", status)
	}
	if got := methodologyConcurrentRuns(methodologyTestLake()); got != 2 {
		t.Fatalf("default cap %d", got)
	}
}

func TestAQueuedRunCanBeStopped(t *testing.T) {
	runs := []LakeMethodologyRun{queuedTestRun(1, testWorkspaceID), queuedTestRun(2, testWorkspaceID), queuedTestRun(3, testWorkspaceID)}
	store := &fakeServiceStore{methodologyRuns: runs, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()
	store.stopRun(runs[2].ID)
	h.poll()
	if status := store.statusOf(t, runs[2].ID); status.Phase != MethodologyPhaseStopped || status.StartedAt != nil {
		t.Fatalf("a stopped queued run never ran: %+v", status)
	}
}

func gpuNode(name, class, zone string, gpus int64) corev1.Node {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if class != "" {
		node.Labels[gpuClassLabel] = class
	}
	if zone != "" {
		node.Labels[corev1.LabelTopologyZone] = zone
	}
	if gpus > 0 {
		node.Status.Allocatable = corev1.ResourceList{resourceNvidiaGPU: *resource.NewQuantity(gpus, resource.DecimalSI)}
	}
	return node
}

func TestMethodologyRunPlacement(t *testing.T) {
	cpu := testMethodologyRun()
	gpu := testMethodologyRun()
	gpu.GPUs, gpu.GPUClass = 1, "a100"
	anyGPU := testMethodologyRun()
	anyGPU.GPUs = 2
	cordoned := gpuNode("cordoned", "a100", "eastus-3", 1)
	cordoned.Spec.Unschedulable = true

	cases := []struct {
		name      string
		run       LakeMethodologyRun
		nodes     []corev1.Node
		zones     []string
		waitsWith string
	}{
		{name: "a CPU run goes anywhere", run: cpu, nodes: nil},
		{name: "a CPU run ignores GPU nodes", run: cpu, nodes: []corev1.Node{gpuNode("g", "a100", "eastus-1", 1)}},
		{name: "no node of its class: it waits", run: gpu, nodes: []corev1.Node{gpuNode("t4", "t4", "eastus-1", 1)},
			waitsWith: "Queued: no node of GPU class a100 is in the cluster yet; the run starts when one is."},
		{name: "no nodes at all: it waits", run: gpu, waitsWith: "no node of GPU class a100"},
		{name: "an unschedulable node does not count", run: gpu, nodes: []corev1.Node{cordoned}, waitsWith: "no node of GPU class a100"},
		{name: "the zones of its class, sorted, others ignored", run: gpu, nodes: []corev1.Node{
			gpuNode("a", "a100", "eastus-3", 1), gpuNode("b", "a100", "eastus-1", 1), gpuNode("c", "a100", "eastus-3", 1),
			gpuNode("t4", "t4", "eastus-2", 1), cordoned, gpuNode("cpu", "", "eastus-2", 0)},
			zones: []string{"eastus-1", "eastus-3"}},
		{name: "nodes without zone labels: no zone constraint", run: gpu, nodes: []corev1.Node{gpuNode("a", "a100", "", 1)}},
		{name: "an unlabelled node adds no zone", run: gpu, nodes: []corev1.Node{gpuNode("a", "a100", "", 1), gpuNode("b", "a100", "eastus-2", 1)},
			zones: []string{"eastus-2"}},
		{name: "no class: nodes offering GPUs", run: anyGPU, nodes: []corev1.Node{gpuNode("cpu", "", "eastus-1", 0), gpuNode("g", "", "eastus-2", 4)},
			zones: []string{"eastus-2"}},
		{name: "no class and no GPU node: it waits", run: anyGPU, nodes: []corev1.Node{gpuNode("cpu", "", "eastus-1", 0)},
			waitsWith: "Queued: no node with GPUs is in the cluster yet; the run starts when one is."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := methodologyRunPlacement(c.nodes, &c.run)
			if !reflect.DeepEqual(got.Zones, c.zones) {
				t.Fatalf("zones %v, want %v", got.Zones, c.zones)
			}
			if c.waitsWith == "" && got.Wait != "" || c.waitsWith != "" && !strings.Contains(got.Wait, c.waitsWith) {
				t.Fatalf("wait %q, want %q", got.Wait, c.waitsWith)
			}
		})
	}
}

func TestAGPURunWaitsForItsClassThenStagesInItsZones(t *testing.T) {
	run := queuedTestRun(1, testWorkspaceID)
	run.GPUs, run.GPUClass = 1, "a100"
	store := &fakeServiceStore{methodologyRuns: []LakeMethodologyRun{run}, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()
	status := store.statusOf(t, run.ID)
	if status.Phase != MethodologyPhaseQueued || !strings.Contains(status.Message, "no node of GPU class a100") || status.CredentialSHA256 != "" {
		t.Fatalf("waits for a node of its class: %+v", status)
	}
	ctx := context.Background()
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyWorkClaim(run.Slug)},
		&corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no work volume while it waits: %v", err)
	}

	for _, node := range []corev1.Node{gpuNode("gpu-1", "a100", "eastus-2", 1), gpuNode("gpu-2", "a100", "eastus-1", 1),
		gpuNode("cpu-1", "", "eastus-3", 0)} {
		node := node
		if err := h.client.Create(ctx, &node); err != nil {
			t.Fatal(err)
		}
	}
	h.poll()
	if status := store.statusOf(t, run.ID); status.Phase != MethodologyPhaseStaging {
		t.Fatalf("stages once its class has nodes: %+v", status)
	}
	stage := h.job(methodologyStageJob(run.Slug)).Spec.Template.Spec
	want := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eastus-1", "eastus-2"}}}}}}}}
	if !reflect.DeepEqual(stage.Affinity, want) {
		t.Fatalf("the stage pod, the volume's first consumer, is pinned to the GPU zones: %+v", stage.Affinity)
	}
}

func TestACPURunsStagePodIsNotPinned(t *testing.T) {
	run := queuedTestRun(1, testWorkspaceID)
	store := &fakeServiceStore{methodologyRuns: []LakeMethodologyRun{run}, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()
	if stage := h.job(methodologyStageJob(run.Slug)).Spec.Template.Spec; stage.Affinity != nil {
		t.Fatalf("a CPU run's stage pod goes anywhere: %+v", stage.Affinity)
	}
}
