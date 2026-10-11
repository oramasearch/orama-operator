/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// --- the fake store's workload resources half ----------------------------------------------------

func (f *fakeServiceStore) ListWorkloadResources(context.Context) ([]WorkloadResources, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resources == nil {
		return nil, errWorkloadResourcesTableMissing
	}
	out := make([]WorkloadResources, 0, len(f.resources))
	for _, key := range sortedWorkloadKeys(f.resources) {
		out = append(out, f.resources[key])
	}
	return out, nil
}

// ListWorkloadObjects mirrors listWorkloadObjectsSQL: the live read lists workspaces, services,
// apps and present service-mode launches; builds and job launches come back only by id.
func (f *fakeServiceStore) ListWorkloadObjects(_ context.Context, ids []string, live bool) ([]WorkloadObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []WorkloadObject
	for _, object := range f.objects {
		listed := false
		switch object.Kind {
		case WorkloadKindWorkspace, WorkloadKindService, WorkloadKindApp:
			listed = true
		case WorkloadKindAgentLaunch:
			listed = object.Subkind == agentLaunchModeService && object.Desired == serviceDesiredPresent
		}
		if wanted[object.ID] || (live && listed) {
			out = append(out, object)
		}
	}
	// Methodology runs come back by id only, as listMethodologyRunObjectsSQL returns them.
	for _, run := range f.methodologyRuns {
		if wanted[run.ID] {
			out = append(out, WorkloadObject{Kind: WorkloadKindMethodologyRun, ID: run.ID, Desired: run.Desired, Subkind: run.Mode, Slug: run.Slug})
		}
	}
	return out, nil
}

func (f *fakeServiceStore) UpsertWorkloadResources(_ context.Context, row WorkloadResources) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceUpserts = append(f.resourceUpserts, row)
	f.resources[row.key()] = row
	return nil
}

func (f *fakeServiceStore) DeleteWorkloadResources(_ context.Context, kind, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := workloadKey{kind, id}
	f.resourceDeletes = append(f.resourceDeletes, key)
	delete(f.resources, key)
	return nil
}

func (f *fakeServiceStore) removeObject(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.objects[:0]
	for _, object := range f.objects {
		if object.ID != id {
			kept = append(kept, object)
		}
	}
	f.objects = kept
}

// --- the arithmetic ------------------------------------------------------------------------------

func rl(pairs ...string) corev1.ResourceList {
	list := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		list[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return list
}

func ctr(name string, requests, limits corev1.ResourceList) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits}}
}

func sidecar(name string, requests corev1.ResourceList) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	c := ctr(name, requests, nil)
	c.RestartPolicy = &always
	return c
}

func TestPodSpecResources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spec     corev1.PodSpec
		defaults containerDefaults
		want     podResources
	}{
		{
			name: "requests are summed over app containers",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				ctr("a", rl("cpu", "250m", "memory", "256Mi"), rl("cpu", "1", "memory", "1Gi")),
				ctr("b", rl("cpu", "500m", "memory", "512Mi"), nil),
			}},
			want: podResources{cpuMillis: 750, memoryBytes: 768 << 20},
		},
		{
			name: "a container with no request counts its limit",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				ctr("a", rl("memory", "128Mi"), rl("cpu", "2", "memory", "1Gi")),
			}},
			want: podResources{cpuMillis: 2000, memoryBytes: 128 << 20},
		},
		{
			name: "a container with neither counts nothing without a LimitRange",
			spec: corev1.PodSpec{Containers: []corev1.Container{ctr("a", nil, nil)}},
			want: podResources{},
		},
		{
			name: "a container with neither counts the LimitRange default request, then default limit",
			spec: corev1.PodSpec{Containers: []corev1.Container{ctr("a", nil, nil), ctr("b", rl("cpu", "100m"), nil)}},
			defaults: containerDefaults{
				request: rl("cpu", "200m"),
				limit:   rl("cpu", "1", "memory", "512Mi"),
			},
			want: podResources{cpuMillis: 300, memoryBytes: 1024 << 20},
		},
		{
			name: "the largest init container wins when it exceeds the app containers",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					ctr("migrate", rl("cpu", "2", "memory", "64Mi"), nil),
					ctr("small", rl("cpu", "100m"), nil),
				},
				Containers: []corev1.Container{ctr("a", rl("cpu", "500m", "memory", "1Gi"), nil)},
			},
			want: podResources{cpuMillis: 2000, memoryBytes: 1 << 30},
		},
		{
			name: "sidecars add to the app containers and to every init container after them",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecar("proxy", rl("cpu", "100m", "memory", "100Mi")),
					ctr("seed", rl("cpu", "1", "memory", "100Mi"), nil),
				},
				Containers: []corev1.Container{ctr("a", rl("cpu", "500m", "memory", "1Gi"), nil)},
			},
			// app 500m + sidecar 100m = 600m < seed 1000m + sidecar 100m = 1100m.
			// memory: app 1Gi + 100Mi > seed 100Mi + 100Mi.
			want: podResources{cpuMillis: 1100, memoryBytes: (1 << 30) + (100 << 20)},
		},
		{
			name: "pod overhead is added",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{ctr("a", rl("cpu", "1", "memory", "1Gi"), nil)},
				Overhead:   rl("cpu", "250m", "memory", "120Mi"),
			},
			want: podResources{cpuMillis: 1250, memoryBytes: (1 << 30) + (120 << 20)},
		},
		{
			name: "GPUs count nvidia and amd, from limits",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				ctr("a", nil, rl("nvidia.com/gpu", "2")),
				ctr("b", rl("amd.com/gpu", "1"), rl("amd.com/gpu", "1")),
			}},
			want: podResources{gpus: 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podSpecResources(&tc.spec, tc.defaults); got != tc.want {
				t.Errorf("podSpecResources = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNormalizeGPUClass(t *testing.T) {
	for product, want := range map[string]string{
		"NVIDIA-L4":                        "l4",
		"NVIDIA-A10G":                      "a10g",
		"NVIDIA-A100-SXM4-80GB":            "a100",
		"NVIDIA-A100-SXM4-40GB-MIG-1g.5gb": "a100",
		"NVIDIA-H100-80GB-HBM3":            "h100",
		"Tesla-T4":                         "t4",
		"NVIDIA-RTX-A6000":                 "rtx-a6000",
		"NVIDIA-GeForce-RTX-4090":          "rtx-4090",
		"AMD_Instinct_MI300X":              "mi300x",
		"NVIDIA":                           "",
		"":                                 "",
	} {
		if got := normalizeGPUClass(product); got != want {
			t.Errorf("normalizeGPUClass(%q) = %q, want %q", product, got, want)
		}
		if got := normalizeGPUClass(product); !gpuClassPattern.MatchString(got) {
			t.Errorf("normalizeGPUClass(%q) = %q does not match the column check", product, got)
		}
	}
	if got := validGPUClass("Not A Class!"); got != "" {
		t.Errorf("validGPUClass accepted %q", got)
	}
}

func TestPodGPUInfo(t *testing.T) {
	node := func(labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labels}}
	}
	pod := &corev1.Pod{}
	selected := &corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{gpuClassLabel: "l4"}}}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		node *corev1.Node
		want gpuInfo
	}{
		{"feature discovery memory label wins", pod,
			node(map[string]string{nodeGPUProductLabel: "NVIDIA-L4", nodeGPUMemoryLabel: "23034"}),
			gpuInfo{class: "l4", vramPerGPU: 23034 * mib}},
		{"built-in table: l4", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-L4"}), gpuInfo{"l4", 24 * gib}},
		{"built-in table: a10g", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-A10G"}), gpuInfo{"a10g", 24 * gib}},
		{"built-in table: a100 80GB", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-A100-SXM4-80GB"}), gpuInfo{"a100", 80 * gib}},
		{"built-in table: a100 40GB", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-A100-PCIE-40GB"}), gpuInfo{"a100", 40 * gib}},
		{"built-in table: h100", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-H100-80GB-HBM3"}), gpuInfo{"h100", 80 * gib}},
		{"built-in table: t4", pod, node(map[string]string{nodeGPUProductLabel: "Tesla-T4"}), gpuInfo{"t4", 16 * gib}},
		{"node class label", pod, node(map[string]string{gpuClassLabel: "h100"}), gpuInfo{"h100", 80 * gib}},
		{"pod nodeSelector class", selected, nil, gpuInfo{"l4", 24 * gib}},
		{"unknown class has no memory", pod, node(map[string]string{nodeGPUProductLabel: "NVIDIA-B200"}), gpuInfo{"b200", 0}},
		{"nothing known", pod, nil, gpuInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podGPUInfo(tc.pod, tc.node); got != tc.want {
				t.Errorf("podGPUInfo = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWorkloadTotalsPicksTheClassRunningTheMostGPUs(t *testing.T) {
	totals := &workloadTotals{}
	totals.add(podResources{cpuMillis: 100, gpus: 1}, gpuInfo{"l4", 24 * gib})
	totals.add(podResources{cpuMillis: 100, gpus: 2}, gpuInfo{"h100", 80 * gib})
	totals.add(podResources{cpuMillis: 100}, gpuInfo{})
	row := totals.row(workloadKey{WorkloadKindService, "id"}, totals.pods)
	want := WorkloadResources{Kind: WorkloadKindService, ID: "id", Pods: 3, CPUMillis: 300, GPUs: 3,
		GPUClass: "h100", VRAMBytes: 24*gib + 160*gib}
	if row != want {
		t.Errorf("row = %+v, want %+v", row, want)
	}
}

func TestWorkloadResourcesClockThrottlesPerLake(t *testing.T) {
	var clock workloadResourcesClock
	now := time.Unix(1_700_000_000, 0)
	if !clock.due("a", now) || clock.due("a", now.Add(10*time.Second)) || !clock.due("b", now) {
		t.Fatal("the first pass of each Lake is due and a second one within the interval is not")
	}
	if !clock.due("a", now.Add(workloadResourcesInterval)) {
		t.Fatal("a pass is due again after the interval")
	}
}

// --- the pass ------------------------------------------------------------------------------------

const (
	wrWorkspaceNS     = "ws-ns"
	wrWorkspaceID     = "aaaaaaaa-0000-4000-8000-000000000001"
	wrWorkspaceZetta  = "aaaaaaaa-0000-4000-8000-000000000002"
	wrZettaServiceID  = "aaaaaaaa-0000-4000-8000-000000000003"
	wrPostgresID      = "aaaaaaaa-0000-4000-8000-000000000004"
	wrKafkaID         = "aaaaaaaa-0000-4000-8000-000000000005"
	wrAppID           = "aaaaaaaa-0000-4000-8000-000000000006"
	wrStoppedAppID    = "aaaaaaaa-0000-4000-8000-000000000007"
	wrServiceAgentID  = "aaaaaaaa-0000-4000-8000-000000000008"
	wrJobAgentID      = "aaaaaaaa-0000-4000-8000-000000000009"
	wrBuildID         = "aaaaaaaa-0000-4000-8000-00000000000a"
	wrForeignLaunchID = "aaaaaaaa-0000-4000-8000-00000000000b"
	wrGoneServiceID   = "aaaaaaaa-0000-4000-8000-00000000000c"
)

func wrPod(name, namespace, node string, labels map[string]string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node, Containers: containers},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func wrHarness(t *testing.T) (*serviceHarness, *oramav1alpha1.Lake) {
	t.Helper()
	lake := serviceTestLake()
	store := &fakeServiceStore{resources: map[workloadKey]WorkloadResources{
		// A row whose service lake has since removed: deleted on the first pass.
		{WorkloadKindService, wrGoneServiceID}: {Kind: WorkloadKindService, ID: wrGoneServiceID, Pods: 1, CPUMillis: 100},
	}}
	store.objects = []WorkloadObject{
		{Kind: WorkloadKindWorkspace, ID: wrWorkspaceID, Namespace: wrWorkspaceNS, Desired: "running"},
		{Kind: WorkloadKindService, ID: wrZettaServiceID, Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: serviceKindZetta, Slug: "svc-000000000003"},
		{Kind: WorkloadKindService, ID: wrPostgresID, Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: serviceKindPostgres, Slug: "svc-000000000004"},
		{Kind: WorkloadKindService, ID: wrKafkaID, Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: serviceKindKafka, Slug: "svc-000000000005"},
		{Kind: WorkloadKindApp, ID: wrAppID, Namespace: wrWorkspaceNS, Desired: "running", Subkind: "image"},
		{Kind: WorkloadKindApp, ID: wrStoppedAppID, Namespace: wrWorkspaceNS, Desired: "stopped", Subkind: "image"},
		{Kind: WorkloadKindAgentLaunch, ID: wrServiceAgentID, Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: agentLaunchModeService, Slug: "al-000000000008"},
		{Kind: WorkloadKindAgentLaunch, ID: wrJobAgentID, Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: agentLaunchModeJob, Slug: "al-000000000009"},
		{Kind: WorkloadKindAgentLaunch, ID: wrForeignLaunchID, Namespace: "other-ns", Desired: serviceDesiredPresent, Subkind: agentLaunchModeJob, Slug: "al-00000000000b"},
		{Kind: WorkloadKindBuild, ID: wrBuildID, Desired: "queued"},
	}
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: lakeNamespace(lake),
			Labels: map[string]string{serviceLakeLabel: lake.Name, lakeAppRowIDLabel: wrAppID}, OwnerReferences: lakeOwnerRef(lake)},
		Spec: oramav1alpha1.LakeAppSpec{LakeRef: lake.Name, Sync: &oramav1alpha1.LakeAppSync{ID: wrAppID, WorkspaceNamespace: wrWorkspaceNS}},
	}
	identity := lakeAppLabels(app)[lakeAppIdentityLabel]
	gpuNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node", Labels: map[string]string{nodeGPUProductLabel: "NVIDIA-L4"}}}
	buildNS := buildNamespace(lake.Name, "acme")
	pending := wrPod("pending", wrWorkspaceNS, "", map[string]string{serviceIDLabel: wrPostgresID}, ctr("c", rl("cpu", "8"), nil))
	finished := wrPod("finished", wrWorkspaceNS, "node", map[string]string{cnpgClusterLabel: serviceResourceName(lake.Name, "svc-000000000004")}, ctr("c", rl("cpu", "8"), nil))
	finished.Status.Phase = corev1.PodSucceeded
	objects := []client.Object{
		app, gpuNode,
		// The workspace engine: a Zetta pod carrying both the workspace and its service id.
		wrPod("engine-0", wrWorkspaceNS, "node", map[string]string{
			"app.kubernetes.io/name": "orama-zetta", "app.kubernetes.io/instance": workspaceZettaClusterName(lake, wrWorkspaceID),
			workspaceLabel: wrWorkspaceID, serviceIDLabel: wrWorkspaceZetta,
		}, ctr("zetta", rl("cpu", "500m", "memory", "1Gi"), nil)),
		// A managed Zetta service in the same workspace.
		wrPod("svc-zetta-0", wrWorkspaceNS, "node", map[string]string{
			"app.kubernetes.io/name": "orama-zetta", "app.kubernetes.io/instance": serviceResourceName(lake.Name, "svc-000000000003"),
			workspaceLabel: wrWorkspaceID, serviceIDLabel: wrZettaServiceID,
		}, ctr("zetta", rl("memory", "64Mi"), rl("memory", "1Gi"))),
		// CloudNativePG's two instances, matched by cnpg.io/cluster.
		wrPod("pg-1", wrWorkspaceNS, "node", map[string]string{cnpgClusterLabel: serviceResourceName(lake.Name, "svc-000000000004")}, ctr("postgres", rl("cpu", "1", "memory", "2Gi"), nil)),
		wrPod("pg-2", wrWorkspaceNS, "node", map[string]string{cnpgClusterLabel: serviceResourceName(lake.Name, "svc-000000000004")}, ctr("postgres", rl("cpu", "1", "memory", "2Gi"), nil)),
		pending, finished,
		// Strimzi's broker, matched by strimzi.io/cluster and kind.
		wrPod("kafka-0", wrWorkspaceNS, "node", map[string]string{strimziClusterLabel: serviceResourceName(lake.Name, "svc-000000000005"), strimziKindLabel: "Kafka"}, ctr("kafka", rl("cpu", "2", "memory", "4Gi"), nil)),
		wrPod("app-0", wrWorkspaceNS, "gpu-node", map[string]string{lakeAppIdentityLabel: identity}, ctr("app", rl("cpu", "250m", "memory", "512Mi"), rl("nvidia.com/gpu", "1"))),
		wrPod("agent-svc", wrWorkspaceNS, "node", map[string]string{agentLaunchIDLabel: wrServiceAgentID}, ctr("agent", rl("cpu", "100m", "memory", "128Mi"), nil)),
		wrPod("agent-job", wrWorkspaceNS, "node", map[string]string{agentLaunchIDLabel: wrJobAgentID}, ctr("agent", rl("cpu", "300m", "memory", "256Mi"), nil)),
		// A pod claiming another workspace's launch from this namespace counts for nothing.
		wrPod("spoof", wrWorkspaceNS, "node", map[string]string{agentLaunchIDLabel: wrForeignLaunchID}, ctr("x", rl("cpu", "64"), nil)),
		wrPod("build-0", buildNS, "node", map[string]string{buildIDLabel: wrBuildID, buildLakeLabel: lake.Name}, ctr("buildkit", rl("cpu", "500m", "memory", "1Gi"), rl("cpu", "2", "memory", "4Gi"))),
		// A pod nobody claims.
		wrPod("stray", wrWorkspaceNS, "node", map[string]string{"app": "stray"}, ctr("x", rl("cpu", "1"), nil)),
	}
	h := newServiceHarness(t, lake, store, objects...)
	return h, lake
}

func (h *serviceHarness) resourcesPass() {
	h.t.Helper()
	if err := h.reconciler.reconcileWorkloadResources(context.Background(), h.lake, h.store); err != nil {
		h.t.Fatalf("reconcileWorkloadResources: %v", err)
	}
}

func (h *serviceHarness) row(kind, id string) WorkloadResources {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	row, found := h.store.resources[workloadKey{kind, id}]
	if !found {
		h.t.Fatalf("no %s row for %s", kind, id)
	}
	return row
}

func TestWorkloadResourcesMapsPodsToObjects(t *testing.T) {
	h, _ := wrHarness(t)
	h.resourcesPass()
	want := map[workloadKey]WorkloadResources{
		{WorkloadKindWorkspace, wrWorkspaceID}:      {Pods: 1, CPUMillis: 500, MemoryBytes: 1 << 30},
		{WorkloadKindService, wrZettaServiceID}:     {Pods: 1, MemoryBytes: 64 << 20},
		{WorkloadKindService, wrPostgresID}:         {Pods: 2, CPUMillis: 2000, MemoryBytes: 4 << 30},
		{WorkloadKindService, wrKafkaID}:            {Pods: 1, CPUMillis: 2000, MemoryBytes: 4 << 30},
		{WorkloadKindApp, wrAppID}:                  {Pods: 1, CPUMillis: 250, MemoryBytes: 512 << 20, GPUs: 1, GPUClass: "l4", VRAMBytes: 24 * gib},
		{WorkloadKindApp, wrStoppedAppID}:           {},
		{WorkloadKindAgentLaunch, wrServiceAgentID}: {Pods: 1, CPUMillis: 100, MemoryBytes: 128 << 20},
		{WorkloadKindAgentLaunch, wrJobAgentID}:     {Pods: 1, CPUMillis: 300, MemoryBytes: 256 << 20},
		{WorkloadKindBuild, wrBuildID}:              {Pods: 1, CPUMillis: 500, MemoryBytes: 1 << 30},
	}
	h.store.mu.Lock()
	got := h.store.resources
	h.store.mu.Unlock()
	if len(got) != len(want) {
		t.Errorf("rows = %v, want %d rows", got, len(want))
	}
	for key, numbers := range want {
		numbers.Kind, numbers.ID = key.kind, key.id
		if row, found := got[key]; !found || row != numbers {
			t.Errorf("%s %s = %+v, want %+v", key.kind, key.id, row, numbers)
		}
	}
	if _, found := got[workloadKey{WorkloadKindService, wrWorkspaceZetta}]; found {
		t.Error("the workspace engine was also reported as a service")
	}
	if _, found := got[workloadKey{WorkloadKindAgentLaunch, wrForeignLaunchID}]; found {
		t.Error("a pod outside its launch's namespace was counted")
	}
	if len(h.store.resourceDeletes) != 1 || h.store.resourceDeletes[0] != (workloadKey{WorkloadKindService, wrGoneServiceID}) {
		t.Errorf("deletes = %v, want the removed service's row", h.store.resourceDeletes)
	}

	// A steady cluster writes nothing.
	upserts := len(h.store.resourceUpserts)
	h.resourcesPass()
	if len(h.store.resourceUpserts) != upserts {
		t.Errorf("a steady pass wrote %d rows", len(h.store.resourceUpserts)-upserts)
	}
}

func TestWorkloadResourcesKeepsFinishedBuildAndJobValues(t *testing.T) {
	h, _ := wrHarness(t)
	h.resourcesPass()
	for _, name := range []string{"build-0", "agent-job", "agent-svc", "kafka-0"} {
		var pod corev1.Pod
		namespace := wrWorkspaceNS
		if name == "build-0" {
			namespace = buildNamespace(h.lake.Name, "acme")
		}
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			t.Fatal(err)
		}
		if name == "agent-job" {
			// A finished pod still listed counts as gone.
			if err := h.client.Delete(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			pod.ResourceVersion = ""
			pod.Status.Phase = corev1.PodSucceeded
			h.create(&pod)
			continue
		}
		if err := h.client.Delete(context.Background(), &pod); err != nil {
			t.Fatal(err)
		}
	}
	h.resourcesPass()
	if got, want := h.row(WorkloadKindBuild, wrBuildID), (WorkloadResources{Kind: WorkloadKindBuild, ID: wrBuildID, CPUMillis: 500, MemoryBytes: 1 << 30}); got != want {
		t.Errorf("finished build = %+v, want %+v", got, want)
	}
	if got, want := h.row(WorkloadKindAgentLaunch, wrJobAgentID), (WorkloadResources{Kind: WorkloadKindAgentLaunch, ID: wrJobAgentID, CPUMillis: 300, MemoryBytes: 256 << 20}); got != want {
		t.Errorf("finished job launch = %+v, want %+v", got, want)
	}
	// A long-running object with no scheduled pods is zeros.
	if got := h.row(WorkloadKindAgentLaunch, wrServiceAgentID); got != (WorkloadResources{Kind: WorkloadKindAgentLaunch, ID: wrServiceAgentID}) {
		t.Errorf("service launch with no pods = %+v, want zeros", got)
	}
	if got := h.row(WorkloadKindService, wrKafkaID); got != (WorkloadResources{Kind: WorkloadKindService, ID: wrKafkaID}) {
		t.Errorf("service with no pods = %+v, want zeros", got)
	}

	// Once lake no longer has the build, its row goes.
	h.store.removeObject(wrBuildID)
	h.resourcesPass()
	h.store.mu.Lock()
	_, found := h.store.resources[workloadKey{WorkloadKindBuild, wrBuildID}]
	h.store.mu.Unlock()
	if found {
		t.Error("the row of a build lake no longer has was kept")
	}
}

func TestWorkloadResourcesDeletesAbsentObjectsOnceTheirPodsAreGone(t *testing.T) {
	h, _ := wrHarness(t)
	h.resourcesPass()
	h.store.mu.Lock()
	for i := range h.store.objects {
		if h.store.objects[i].ID == wrKafkaID {
			h.store.objects[i].Desired = serviceDesiredAbsent
		}
	}
	h.store.mu.Unlock()
	// Still stopping: its pod is scheduled, so it is still reported.
	h.resourcesPass()
	if got := h.row(WorkloadKindService, wrKafkaID); got.Pods != 1 || got.CPUMillis != 2000 {
		t.Errorf("absent service still running = %+v", got)
	}
	var pod corev1.Pod
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: wrWorkspaceNS, Name: "kafka-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	h.resourcesPass()
	h.store.mu.Lock()
	_, found := h.store.resources[workloadKey{WorkloadKindService, wrKafkaID}]
	h.store.mu.Unlock()
	if found {
		t.Error("an absent service with no pods kept its row")
	}
}

func TestWorkloadResourcesFromAFinishedJobTemplate(t *testing.T) {
	h, lake := wrHarness(t)
	// A job launch whose pod finished and was removed before any pass saw it.
	launchID := "aaaaaaaa-0000-4000-8000-0000000000ff"
	h.store.mu.Lock()
	h.store.objects = append(h.store.objects, WorkloadObject{Kind: WorkloadKindAgentLaunch, ID: launchID,
		Namespace: wrWorkspaceNS, Desired: serviceDesiredPresent, Subkind: agentLaunchModeJob, Slug: "al-0000000000ff"})
	h.store.mu.Unlock()
	h.create(&corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: wrWorkspaceNS},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: rl("cpu", "100m", "memory", "128Mi"),
			Default:        rl("cpu", "1", "memory", "1Gi"),
		}}},
	})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: agentLaunchName(lake.Name, "al-0000000000ff"), Namespace: wrWorkspaceNS,
			Labels: map[string]string{agentLaunchIDLabel: launchID}, OwnerReferences: lakeOwnerRef(lake)},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				ctr("agent", nil, rl("cpu", "2")), // no request: its limit; memory from the default request
				ctr("helper", nil, nil),           // neither: the default requests
			},
		}}},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	h.create(job)
	h.resourcesPass()
	want := WorkloadResources{Kind: WorkloadKindAgentLaunch, ID: launchID, CPUMillis: 2100, MemoryBytes: 256 << 20}
	if got := h.row(WorkloadKindAgentLaunch, launchID); got != want {
		t.Errorf("job template resources = %+v, want %+v", got, want)
	}
}

func TestWorkloadResourcesSkippedBeforeTheSchemaHasTheTable(t *testing.T) {
	h, _ := wrHarness(t)
	h.store.resources = nil
	err := h.reconciler.reconcileWorkloadResources(context.Background(), h.lake, h.store)
	if !errors.Is(err, errWorkloadResourcesTableMissing) {
		t.Fatalf("err = %v, want the missing-table error", err)
	}
	if len(h.store.resourceUpserts) != 0 || len(h.store.resourceDeletes) != 0 {
		t.Error("a schema without the table was written to")
	}
}

func TestServicePodSelectorsCoverEveryKind(t *testing.T) {
	lake := serviceTestLake()
	for _, kind := range serviceKindNames {
		object := WorkloadObject{Kind: WorkloadKindService, ID: wrPostgresID, Namespace: wrWorkspaceNS, Subkind: kind, Slug: "svc-000000000004"}
		selectors := servicePodSelectors(lake, object)
		switch kind {
		case serviceKindZetta, serviceKindSQLite, serviceKindDuckDB, serviceKindPolars:
			// Their pods carry orama.cloud/service-id, which classify reads first.
			if len(selectors) != 0 {
				t.Errorf("%s has selectors %v; its pods are found by orama.cloud/service-id", kind, selectors)
			}
		default:
			if len(selectors) == 0 {
				t.Errorf("%s has no pod selector", kind)
			}
		}
	}
	keys := make([]string, 0)
	for key := range servicePodSelectors(lake, WorkloadObject{Subkind: serviceKindPostgres, Slug: "svc-000000000004"})[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != cnpgClusterLabel {
		t.Errorf("postgres selector keys = %v", keys)
	}
}
