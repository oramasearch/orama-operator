/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Workload resources (lake#643, lake#658; lake migrations 0075_workload_resources and
// 0077_metered_jobs; this repo's docs/lake/workload-resources.md). lake prices compute by the
// resources each workload is scheduled with, as CPU, RAM, GPU and VRAM. The operator is the only
// writer of workload_resources: one row per workload of the Lake (a workspace's engine, a managed
// service, an app, an image build, an agent launch, an agent cron run) holding the totals of its
// pods that are scheduled now. A job (a build, a job launch, a cron run) has its row from its Job's
// pod template as soon as the Job exists, and one that finished without a row is backfilled from
// the pod spec the operator schedules such jobs with. It runs on the control-rows poll over the
// same connection, at most once per workloadResourcesInterval, and writes a row only when its
// numbers change.
//
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list
const (
	WorkloadKindWorkspace   = "workspace"
	WorkloadKindService     = "service"
	WorkloadKindStore       = "store"
	WorkloadKindApp         = "app"
	WorkloadKindBuild       = "build"
	WorkloadKindAgentLaunch = "agent_launch"
	// WorkloadKindMethodologyRun is the sandbox pod of a workspace methodology's run (lake#655):
	// the code's own pod, not the stage and collect pods around it.
	WorkloadKindMethodologyRun = "methodology_run"

	// WorkloadKindAgentCronRun is one Job an agent cron schedule's CronJob started (lake#658); its
	// id is the agent_cron_runs id, the Job's uid.
	WorkloadKindAgentCronRun = "agent_cron_run"

	// The labels Kubernetes puts on a Job's pods naming the Job's uid (the second is the older one).
	jobControllerUIDLabel       = "batch.kubernetes.io/controller-uid"
	legacyJobControllerUIDLabel = "controller-uid"

	// workloadResourcesInterval is how often the pass runs per Lake. The control-rows poll runs
	// every 15-20 seconds (2 while rows change); the contract asks for at least once a minute.
	workloadResourcesInterval = 30 * time.Second

	// GPU feature discovery's node labels: the product name and the memory of one GPU in MiB.
	nodeGPUProductLabel = "nvidia.com/gpu.product"
	nodeGPUMemoryLabel  = "nvidia.com/gpu.memory"
	// AMD's node labeller names the product here.
	nodeAMDGPUProductLabel = "amd.com/gpu.product-name"
	// gpuClassLabel is an explicit GPU class, on a node or in a pod's nodeSelector.
	gpuClassLabel = "orama.cloud/gpu-class"

	resourceNvidiaGPU corev1.ResourceName = "nvidia.com/gpu"
	resourceAMDGPU    corev1.ResourceName = "amd.com/gpu"

	gib = int64(1) << 30
	mib = int64(1) << 20
)

// errWorkloadResourcesTableMissing: lake's schema has not reached 0075_workload_resources yet.
var errWorkloadResourcesTableMissing = errors.New("the workload_resources table does not exist yet (lake schema before 0075_workload_resources)")

var (
	gpuClassPattern = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,31})?$`)
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// gpuBrandTokens are the words of a product name that are not its class.
	gpuBrandTokens = map[string]bool{"nvidia": true, "tesla": true, "geforce": true, "quadro": true, "amd": true, "instinct": true, "radeon": true}
)

// WorkloadResources is one workload_resources row.
type WorkloadResources struct {
	Kind        string
	ID          string
	Pods        int32
	CPUMillis   int64
	MemoryBytes int64
	GPUs        int32
	GPUClass    string
	VRAMBytes   int64
}

func (w WorkloadResources) key() workloadKey { return workloadKey{w.Kind, w.ID} }

// sameNumbers compares everything but the key.
func (w WorkloadResources) sameNumbers(other WorkloadResources) bool {
	return w.Pods == other.Pods && w.CPUMillis == other.CPUMillis && w.MemoryBytes == other.MemoryBytes &&
		w.GPUs == other.GPUs && w.GPUClass == other.GPUClass && w.VRAMBytes == other.VRAMBytes
}

// WorkloadObject is one lake row the operator runs pods for, read for this pass.
type WorkloadObject struct {
	Kind string
	ID   string
	// Namespace is the joined workspace's namespace; empty for a build, whose namespace is its
	// build space's.
	Namespace string
	Desired   string
	// Subkind is the service kind, the app mode or the agent launch mode.
	Subkind string
	Slug    string
}

type workloadKey struct {
	kind string
	id   string
}

// live reports whether lake still wants the object to exist. A stopped workspace or app is live
// with no pods.
func (o WorkloadObject) live() bool {
	switch o.Kind {
	case WorkloadKindWorkspace, WorkloadKindApp:
		return o.Desired != "deleted"
	case WorkloadKindService, WorkloadKindAgentLaunch:
		return o.Desired == serviceDesiredPresent
	}
	return true
}

// keepsFinishedValues: a build's, a job launch's and a cron run's row keeps what its pods ran with
// after they finish (pods 0), so lake can price the duration × resources.
func (o WorkloadObject) keepsFinishedValues() bool {
	return o.Kind == WorkloadKindBuild || o.Kind == WorkloadKindMethodologyRun || o.Kind == WorkloadKindAgentCronRun ||
		(o.Kind == WorkloadKindAgentLaunch && o.Subkind == agentLaunchModeJob)
}

// --- the arithmetic ------------------------------------------------------------------------------

// containerDefaults are a namespace's LimitRange container defaults. Admission applies them to
// live pods, so they matter only for a Job's pod template read after its pods are gone.
type containerDefaults struct {
	request corev1.ResourceList
	limit   corev1.ResourceList
}

// containerAmount is what one container counts for: its request, or its limit when it has no
// request, or, with neither, the LimitRange default request and then default limit, the order
// LimitRanger admission applies them in. CPU is in millicores, everything else in units.
func containerAmount(c *corev1.Container, name corev1.ResourceName, defaults containerDefaults) int64 {
	for _, list := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits, defaults.request, defaults.limit} {
		if quantity, found := list[name]; found {
			if name == corev1.ResourceCPU {
				return quantity.MilliValue()
			}
			return quantity.Value()
		}
	}
	return 0
}

// containerGPUs counts nvidia.com/gpu and amd.com/gpu. Extended resources cannot be overcommitted,
// so a request equals its limit.
func containerGPUs(c *corev1.Container, defaults containerDefaults) int64 {
	return containerAmount(c, resourceNvidiaGPU, defaults) + containerAmount(c, resourceAMDGPU, defaults)
}

// effectivePodAmount is Kubernetes' effective pod request for one resource: the app containers
// plus every restartable init container (sidecar), or the largest init container plus the
// sidecars started before it, whichever is larger.
func effectivePodAmount(spec *corev1.PodSpec, amount func(*corev1.Container) int64) int64 {
	var app int64
	for i := range spec.Containers {
		app += amount(&spec.Containers[i])
	}
	var sidecars, init int64
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		value := amount(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars += value
			init = max(init, sidecars)
			continue
		}
		init = max(init, value+sidecars)
	}
	return max(app+sidecars, init)
}

// podResources is one pod's effective CPU (millicores), memory (bytes) and GPUs, with the pod
// overhead (a RuntimeClass's) added to CPU and memory.
type podResources struct {
	cpuMillis   int64
	memoryBytes int64
	gpus        int64
}

func podSpecResources(spec *corev1.PodSpec, defaults containerDefaults) podResources {
	resources := podResources{
		cpuMillis: effectivePodAmount(spec, func(c *corev1.Container) int64 {
			return containerAmount(c, corev1.ResourceCPU, defaults)
		}),
		memoryBytes: effectivePodAmount(spec, func(c *corev1.Container) int64 {
			return containerAmount(c, corev1.ResourceMemory, defaults)
		}),
		gpus: effectivePodAmount(spec, func(c *corev1.Container) int64 { return containerGPUs(c, defaults) }),
	}
	if overhead, found := spec.Overhead[corev1.ResourceCPU]; found {
		resources.cpuMillis += overhead.MilliValue()
	}
	if overhead, found := spec.Overhead[corev1.ResourceMemory]; found {
		resources.memoryBytes += overhead.Value()
	}
	return resources
}

// podScheduled: assigned to a node and not finished.
func podScheduled(pod *corev1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// normalizeGPUClass turns a product name into a short class: NVIDIA-L4 is l4, NVIDIA-A10G a10g,
// NVIDIA-A100-SXM4-80GB a100, NVIDIA-H100-80GB-HBM3 h100, Tesla-T4 t4, NVIDIA-RTX-A6000
// rtx-a6000, AMD_Instinct_MI300X mi300x. "" when nothing usable is left.
func normalizeGPUClass(product string) string {
	tokens := strings.FieldsFunc(strings.ToLower(product), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for len(tokens) > 0 && gpuBrandTokens[tokens[0]] {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return ""
	}
	class := tokens[0]
	if class == "rtx" && len(tokens) > 1 {
		class += "-" + tokens[1]
	}
	return validGPUClass(class)
}

// validGPUClass returns class when it matches the column's check, "" otherwise.
func validGPUClass(class string) string {
	class = strings.ToLower(strings.TrimSpace(class))
	if len(class) > 32 {
		class = strings.TrimRight(class[:32], "-")
	}
	if !gpuClassPattern.MatchString(class) {
		return ""
	}
	return class
}

// builtinVRAMPerGPU is the memory of one GPU of a known class, for nodes without GPU feature
// discovery's memory label. The A100 comes in 40 and 80 GiB; the product name tells them apart
// and 40 GiB is assumed without one.
func builtinVRAMPerGPU(class, product string) int64 {
	switch class {
	case "l4", "a10g":
		return 24 * gib
	case "t4":
		return 16 * gib
	case "h100":
		return 80 * gib
	case "a100":
		if strings.Contains(strings.ToLower(product), "80gb") {
			return 80 * gib
		}
		return 40 * gib
	}
	return 0
}

// gpuInfo is the class and per-GPU memory of the GPUs a pod runs on.
type gpuInfo struct {
	class      string
	vramPerGPU int64
}

// podGPUInfo reads the node's GPU feature discovery labels, then an explicit orama.cloud/gpu-class
// on the node or in the pod's nodeSelector. node may be nil when it could not be read.
func podGPUInfo(pod *corev1.Pod, node *corev1.Node) gpuInfo {
	var nodeLabels map[string]string
	if node != nil {
		nodeLabels = node.Labels
	}
	product := nodeLabels[nodeGPUProductLabel]
	if product == "" {
		product = nodeLabels[nodeAMDGPUProductLabel]
	}
	class := normalizeGPUClass(product)
	if class == "" {
		class = validGPUClass(nodeLabels[gpuClassLabel])
	}
	if class == "" {
		class = validGPUClass(pod.Spec.NodeSelector[gpuClassLabel])
	}
	info := gpuInfo{class: class}
	if memory, err := strconv.ParseInt(strings.TrimSpace(nodeLabels[nodeGPUMemoryLabel]), 10, 64); err == nil && memory > 0 {
		info.vramPerGPU = memory * mib
	} else {
		info.vramPerGPU = builtinVRAMPerGPU(class, product)
	}
	return info
}

// workloadTotals accumulates one object's pods.
type workloadTotals struct {
	pods        int32
	cpuMillis   int64
	memoryBytes int64
	gpus        int64
	vramBytes   int64
	// gpusByClass picks the row's class: the class running the most GPUs.
	gpusByClass map[string]int64
}

func (t *workloadTotals) add(resources podResources, gpu gpuInfo) {
	t.pods++
	t.cpuMillis += resources.cpuMillis
	t.memoryBytes += resources.memoryBytes
	if resources.gpus > 0 {
		t.gpus += resources.gpus
		t.vramBytes += resources.gpus * gpu.vramPerGPU
		if t.gpusByClass == nil {
			t.gpusByClass = map[string]int64{}
		}
		t.gpusByClass[gpu.class] += resources.gpus
	}
}

func (t *workloadTotals) row(key workloadKey, pods int32) WorkloadResources {
	row := WorkloadResources{Kind: key.kind, ID: key.id, Pods: pods, CPUMillis: t.cpuMillis,
		MemoryBytes: t.memoryBytes, GPUs: int32(t.gpus), VRAMBytes: t.vramBytes}
	if t.gpus > 0 {
		classes := make([]string, 0, len(t.gpusByClass))
		for class := range t.gpusByClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		var most int64
		for _, class := range classes {
			if t.gpusByClass[class] > most && class != "" {
				most, row.GPUClass = t.gpusByClass[class], class
			}
		}
	}
	return row
}

// --- mapping pods to objects ---------------------------------------------------------------------

// workloadIndex is what a pod is matched against.
type workloadIndex struct {
	lake *oramav1alpha1.Lake
	// apps maps a LakeApp's identity label to the apps row it runs.
	apps map[string]string
	// selectors are the label selectors of the services run by an upstream operator, by
	// namespace: CloudNativePG, Strimzi, Altinity, TiDB, cass-operator and the Valkey operator
	// label the pods of the resource the Orama operator created for the service.
	selectors map[string][]serviceSelector
}

type serviceSelector struct {
	id       string
	selector labels.Selector
}

// classifyWorkloadPod names the object a pod belongs to, from the labels the operator puts on it
// or the labels an upstream operator puts on the pods of a service's resource. The first match
// wins: build, agent launch, app, workspace engine, labelled service, selected service.
func (ix *workloadIndex) classify(pod *corev1.Pod) (workloadKey, bool) {
	podLabels := pod.Labels
	if id := podLabels[buildIDLabel]; id != "" && podLabels[buildLakeLabel] == ix.lake.Name {
		return workloadKey{WorkloadKindBuild, id}, true
	}
	if id := podLabels[agentLaunchIDLabel]; id != "" {
		return workloadKey{WorkloadKindAgentLaunch, id}, true
	}
	if id := podLabels[methodologyRunIDLabel]; id != "" {
		if podLabels[methodologyRunLakeLabel] == ix.lake.Name && podLabels[methodologyRunRoleLabel] == methodologyRoleSandbox {
			return workloadKey{WorkloadKindMethodologyRun, id}, true
		}
		return workloadKey{}, false
	}
	if podLabels[agentScheduleIDLabel] != "" {
		// A cron run is its Job, which Kubernetes names on the Job's pods.
		if uid := podLabels[jobControllerUIDLabel]; uid != "" {
			return workloadKey{WorkloadKindAgentCronRun, uid}, true
		}
		if uid := podLabels[legacyJobControllerUIDLabel]; uid != "" {
			return workloadKey{WorkloadKindAgentCronRun, uid}, true
		}
		return workloadKey{}, false
	}
	if identity := podLabels[lakeAppIdentityLabel]; identity != "" {
		if id, found := ix.apps[identity]; found {
			return workloadKey{WorkloadKindApp, id}, true
		}
		return workloadKey{}, false
	}
	if podLabels["app.kubernetes.io/name"] == "orama-zetta" {
		if name := workspaceZettaClusterName(ix.lake, podLabels[workspaceLabel]); name != "" &&
			podLabels["app.kubernetes.io/instance"] == name {
			workspace := podLabels[workspaceLabel]
			return workloadKey{WorkloadKindWorkspace, workspace}, true
		}
	}
	if id := podLabels[serviceIDLabel]; id != "" {
		return workloadKey{WorkloadKindService, id}, true
	}
	set := labels.Set(podLabels)
	for _, candidate := range ix.selectors[pod.Namespace] {
		if candidate.selector.Matches(set) {
			return workloadKey{WorkloadKindService, candidate.id}, true
		}
	}
	return workloadKey{}, false
}

// servicePodSelectors are the labels on a service's pods. Zetta, SQLite, DuckDB and Polars pods
// carry orama.cloud/service-id and need none. Postgres pods carry cnpg.io/cluster; the catalog
// kinds' upstream operators label each pod with the labels they put on its claims
// (claimSelectors): Strimzi's cluster and kind, Altinity's installation and Keeper, TiDB's
// instance, cass-operator's cluster, the Valkey operator's cluster.
func servicePodSelectors(lake *oramav1alpha1.Lake, object WorkloadObject) []client.MatchingLabels {
	name := serviceResourceName(lake.Name, object.Slug)
	switch object.Subkind {
	case serviceKindPostgres:
		return []client.MatchingLabels{servicePostgresSelector(name)}
	case serviceKindZetta, serviceKindSQLite, serviceKindDuckDB, serviceKindPolars:
		return nil
	}
	kind, found := catalogKinds[object.Subkind]
	if !found {
		return nil
	}
	svc := &LakeService{ID: object.ID, Slug: object.Slug, Kind: object.Subkind}
	return kind.claimSelectors(catalogService{lake: lake, svc: svc, namespace: object.Namespace, name: name})
}

// --- the pass ------------------------------------------------------------------------------------

// workloadResourcesClock throttles the pass per Lake.
type workloadResourcesClock struct {
	mu   sync.Mutex
	last map[types.UID]time.Time
}

func (c *workloadResourcesClock) due(uid types.UID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.UID]time.Time{}
	}
	if last, found := c.last[uid]; found && now.Sub(last) < workloadResourcesInterval {
		return false
	}
	c.last[uid] = now
	return true
}

func (r *LakeReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// reconcileWorkloadResources is one pass: read the rows written so far and the objects lake has,
// list the pods of the Lake's workspace namespaces and build spaces, total each object's scheduled
// pods, and write the rows that changed and delete the rows of objects that are gone. Any failed
// read stops the pass before it writes, so a partial view never zeroes or deletes a row.
func (r *LakeReconciler) reconcileWorkloadResources(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore) error {
	existingRows, err := store.ListWorkloadResources(ctx)
	if err != nil {
		return err
	}
	existing := make(map[workloadKey]WorkloadResources, len(existingRows))
	for _, row := range existingRows {
		existing[row.key()] = row
	}
	liveObjects, err := store.ListWorkloadObjects(ctx, nil, true)
	if err != nil {
		return fmt.Errorf("read objects: %w", err)
	}
	objects := map[workloadKey]WorkloadObject{}
	namespaces := map[string]bool{}
	for _, object := range liveObjects {
		objects[workloadKey{object.Kind, object.ID}] = object
		if object.Namespace != "" && len(validation.IsDNS1123Label(object.Namespace)) == 0 {
			namespaces[object.Namespace] = true
		}
	}

	reader := r.reader()
	var pods []corev1.Pod
	for _, namespace := range sortedKeys(namespaces) {
		var list corev1.PodList
		if err := reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("list pods in %s: %w", namespace, err)
		}
		pods = append(pods, list.Items...)
	}
	var buildPods corev1.PodList
	if err := reader.List(ctx, &buildPods, client.MatchingLabels{buildLakeLabel: lake.Name}); err != nil {
		return fmt.Errorf("list build pods: %w", err)
	}
	for i := range buildPods.Items {
		if !namespaces[buildPods.Items[i].Namespace] {
			pods = append(pods, buildPods.Items[i])
		}
	}
	var runPods corev1.PodList
	if err := reader.List(ctx, &runPods, client.InNamespace(methodologyRunsNamespace(lake.Name)),
		client.MatchingLabels{methodologyRunLakeLabel: lake.Name}); err != nil {
		return fmt.Errorf("list methodology run pods: %w", err)
	}
	pods = append(pods, runPods.Items...)
	jobs, err := r.workloadJobs(ctx, lake, namespaces)
	if err != nil {
		return err
	}
	ix, err := r.workloadIndex(ctx, lake, liveObjects)
	if err != nil {
		return err
	}

	// Ids seen on pods, Jobs and rows that the live read did not return: read them by id, which
	// returns history (a finished build, an absent service still stopping) and nothing for an
	// object lake no longer has.
	podKeys := make([]workloadKey, len(pods))
	missing := map[string]bool{}
	note := func(key workloadKey) {
		if _, found := objects[key]; !found && uuidPattern.MatchString(key.id) {
			missing[key.id] = true
		}
	}
	for i := range pods {
		if key, found := ix.classify(&pods[i]); found {
			podKeys[i] = key
			note(key)
		}
	}
	for key := range jobs {
		note(key)
	}
	for key := range existing {
		note(key)
	}
	if len(missing) > 0 {
		more, err := store.ListWorkloadObjects(ctx, sortedKeys(missing), false)
		if err != nil {
			return fmt.Errorf("read objects by id: %w", err)
		}
		for _, object := range more {
			objects[workloadKey{object.Kind, object.ID}] = object
		}
	}
	// Finished runs that have no row (lake#658): their pods and Job are gone, or they ran before
	// the operator reported resources. They are backfilled from the pod spec the operator
	// schedules such runs with, unless a pod or Job of theirs is seen below.
	unmetered, err := store.ListUnmeteredJobs(ctx)
	if err != nil {
		return fmt.Errorf("read unmetered jobs: %w", err)
	}
	backfill := map[workloadKey]bool{}
	for _, object := range unmetered {
		key := workloadKey{object.Kind, object.ID}
		if _, found := objects[key]; !found {
			objects[key] = object
		}
		backfill[key] = true
	}

	totals := map[workloadKey]*workloadTotals{}
	nodes := map[string]*corev1.Node{}
	for i := range pods {
		pod := &pods[i]
		key := podKeys[i]
		object, found := objects[key]
		if key.kind == "" || !found || !podScheduled(pod) {
			continue
		}
		// A pod counts only in its object's own namespace (a build in its build space, a
		// methodology run in the Lake's methodology-runs namespace).
		if key.kind == WorkloadKindMethodologyRun && pod.Namespace != methodologyRunsNamespace(ix.lake.Name) {
			continue
		}
		if key.kind != WorkloadKindBuild && key.kind != WorkloadKindMethodologyRun && pod.Namespace != object.Namespace {
			continue
		}
		resources := podSpecResources(&pod.Spec, containerDefaults{})
		var gpu gpuInfo
		if resources.gpus > 0 {
			gpu = podGPUInfo(pod, r.workloadNode(ctx, reader, nodes, pod.Spec.NodeName))
		}
		if totals[key] == nil {
			totals[key] = &workloadTotals{}
		}
		totals[key].add(resources, gpu)
	}

	desired := map[workloadKey]WorkloadResources{}
	for key, object := range objects {
		current := totals[key]
		switch {
		case current != nil:
			desired[key] = current.row(key, current.pods)
		case object.keepsFinishedValues():
			if row, found := existing[key]; found {
				row.Pods = 0
				desired[key] = row
			} else if job, found := jobs[key]; found {
				// From the Job as soon as it exists (lake#658), so a run that finishes between two
				// passes is still priced; its scheduled pods replace these numbers once seen.
				row, err := r.jobTemplateResources(ctx, reader, key, job)
				if err != nil {
					return err
				}
				desired[key] = row
			} else if backfill[key] {
				if row, ok := scheduledJobResources(lake, key); ok {
					desired[key] = row
				}
			}
		case object.live():
			desired[key] = WorkloadResources{Kind: key.kind, ID: key.id}
		}
	}

	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	for _, key := range sortedWorkloadKeys(desired) {
		row := desired[key]
		if prior, found := existing[key]; found && prior.sameNumbers(row) {
			continue
		}
		if err := store.UpsertWorkloadResources(ctx, row); err != nil {
			return fmt.Errorf("write %s %s: %w", key.kind, key.id, err)
		}
	}
	for _, key := range sortedWorkloadKeys(existing) {
		if _, found := desired[key]; found {
			continue
		}
		if err := store.DeleteWorkloadResources(ctx, key.kind, key.id); err != nil {
			return fmt.Errorf("delete %s %s: %w", key.kind, key.id, err)
		}
		log.V(1).Info("workload resources removed", "kind", key.kind, "id", key.id)
	}
	return nil
}

// workloadJobs are the Jobs of builds and job-mode agent launches, by object, controlled by the
// Lake. A Job outlives its pods for its TTL, so a short run is still recorded from its template.
func (r *LakeReconciler) workloadJobs(ctx context.Context, lake *oramav1alpha1.Lake, namespaces map[string]bool) (map[workloadKey]*batchv1.Job, error) {
	reader := r.reader()
	out := map[workloadKey]*batchv1.Job{}
	var builds batchv1.JobList
	if err := reader.List(ctx, &builds, client.MatchingLabels{buildLakeLabel: lake.Name}); err != nil {
		return nil, fmt.Errorf("list build Jobs: %w", err)
	}
	for i := range builds.Items {
		job := &builds.Items[i]
		if id := job.Labels[buildIDLabel]; id != "" && metav1.IsControlledBy(job, lake) {
			out[workloadKey{WorkloadKindBuild, id}] = job
		}
	}
	var launches batchv1.JobList
	if err := reader.List(ctx, &launches, client.HasLabels{agentLaunchIDLabel}); err != nil {
		return nil, fmt.Errorf("list agent Jobs: %w", err)
	}
	for i := range launches.Items {
		job := &launches.Items[i]
		if id := job.Labels[agentLaunchIDLabel]; id != "" && namespaces[job.Namespace] && metav1.IsControlledBy(job, lake) {
			out[workloadKey{WorkloadKindAgentLaunch, id}] = job
		}
	}
	var methodologyRuns batchv1.JobList
	if err := reader.List(ctx, &methodologyRuns, client.InNamespace(methodologyRunsNamespace(lake.Name)),
		client.MatchingLabels{methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleSandbox}); err != nil {
		return nil, fmt.Errorf("list methodology run Jobs: %w", err)
	}
	for i := range methodologyRuns.Items {
		job := &methodologyRuns.Items[i]
		if id := job.Labels[methodologyRunIDLabel]; id != "" && metav1.IsControlledBy(job, lake) {
			out[workloadKey{WorkloadKindMethodologyRun, id}] = job
		}
	}
	// A cron schedule's Jobs belong to its CronJob, which the Lake controls; a run is its Job's
	// uid, in one of the Lake's workspace namespaces.
	var runs batchv1.JobList
	if err := reader.List(ctx, &runs, client.HasLabels{agentScheduleIDLabel}); err != nil {
		return nil, fmt.Errorf("list agent cron Jobs: %w", err)
	}
	for i := range runs.Items {
		job := &runs.Items[i]
		if job.UID != "" && namespaces[job.Namespace] {
			out[workloadKey{WorkloadKindAgentCronRun, string(job.UID)}] = job
		}
	}
	return out, nil
}

// scheduledJobResources is what the operator schedules a run of this kind with today: a build's
// pod (buildResources, spec.builds.resources) or an agent Job's pod (agentJobPod), with pods 0. It
// prices a finished run whose pods and Job were gone before any pass saw them (lake#658). Their
// requests are explicit, so no LimitRange default applies; they ask for no GPU.
func scheduledJobResources(lake *oramav1alpha1.Lake, key workloadKey) (WorkloadResources, bool) {
	var spec corev1.PodSpec
	switch key.kind {
	case WorkloadKindBuild:
		spec = buildPod(lake, &LakeBuild{ID: key.id}, buildTarget{}, nil)
	case WorkloadKindAgentLaunch, WorkloadKindAgentCronRun:
		spec = agentJobPod(lake, nil)
	default:
		return WorkloadResources{}, false
	}
	totals := &workloadTotals{}
	totals.add(podSpecResources(&spec, containerDefaults{}), gpuInfo{})
	return totals.row(key, 0), true
}

// workloadIndex reads the LakeApps (for their pods' identity label) and builds the service
// selectors of the live services.
func (r *LakeReconciler) workloadIndex(ctx context.Context, lake *oramav1alpha1.Lake, objects []WorkloadObject) (*workloadIndex, error) {
	ix := &workloadIndex{lake: lake, apps: map[string]string{}, selectors: map[string][]serviceSelector{}}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(lakeNamespace(lake)), client.MatchingLabels{serviceLakeLabel: lake.Name}); err != nil {
		if !isKindNotInstalled(err) {
			return nil, fmt.Errorf("list LakeApps: %w", err)
		}
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.Sync == nil || app.Spec.Sync.ID == "" || !metav1.IsControlledBy(app, lake) {
			continue
		}
		ix.apps[lakeAppLabels(app)[lakeAppIdentityLabel]] = app.Spec.Sync.ID
	}
	for _, object := range objects {
		if object.Kind != WorkloadKindService {
			continue
		}
		for _, selector := range servicePodSelectors(lake, object) {
			ix.selectors[object.Namespace] = append(ix.selectors[object.Namespace],
				serviceSelector{id: object.ID, selector: labels.SelectorFromSet(labels.Set(selector))})
		}
	}
	return ix, nil
}

func (r *LakeReconciler) workloadNode(ctx context.Context, reader client.Reader, nodes map[string]*corev1.Node, name string) *corev1.Node {
	if node, found := nodes[name]; found {
		return node
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Info("GPU class unknown: cannot read the node", "node", name, "err", err.Error())
		}
		node = nil
	}
	nodes[name] = node
	return node
}

// jobTemplateResources is a Job's pod template's resources, for a run none of whose pods the pass
// has seen scheduled: one just created, or one whose pods were gone before a pass saw them. A
// container without request or limit counts the namespace's LimitRange default. GPU class comes
// from the template's nodeSelector only.
func (r *LakeReconciler) jobTemplateResources(ctx context.Context, reader client.Reader, key workloadKey, job *batchv1.Job) (WorkloadResources, error) {
	defaults, err := namespaceContainerDefaults(ctx, reader, job.Namespace)
	if err != nil {
		return WorkloadResources{}, err
	}
	pod := &corev1.Pod{ObjectMeta: job.Spec.Template.ObjectMeta, Spec: job.Spec.Template.Spec}
	resources := podSpecResources(&pod.Spec, defaults)
	var gpu gpuInfo
	if resources.gpus > 0 {
		gpu = podGPUInfo(pod, nil)
	}
	totals := &workloadTotals{}
	totals.add(resources, gpu)
	return totals.row(key, 0), nil
}

// namespaceContainerDefaults merges the Container defaults of a namespace's LimitRanges.
func namespaceContainerDefaults(ctx context.Context, reader client.Reader, namespace string) (containerDefaults, error) {
	var ranges corev1.LimitRangeList
	if err := reader.List(ctx, &ranges, client.InNamespace(namespace)); err != nil {
		return containerDefaults{}, fmt.Errorf("list LimitRanges in %s: %w", namespace, err)
	}
	defaults := containerDefaults{request: corev1.ResourceList{}, limit: corev1.ResourceList{}}
	sort.Slice(ranges.Items, func(i, j int) bool { return ranges.Items[i].Name < ranges.Items[j].Name })
	for _, limitRange := range ranges.Items {
		for _, item := range limitRange.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer {
				continue
			}
			for name, quantity := range item.DefaultRequest {
				if _, set := defaults.request[name]; !set {
					defaults.request[name] = quantity
				}
			}
			for name, quantity := range item.Default {
				if _, set := defaults.limit[name]; !set {
					defaults.limit[name] = quantity
				}
			}
		}
	}
	return defaults, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedWorkloadKeys[V any](rows map[workloadKey]V) []workloadKey {
	out := make([]workloadKey, 0, len(rows))
	for key := range rows {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].id < out[j].id
	})
	return out
}
