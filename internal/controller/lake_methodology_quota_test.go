// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// quotaGate makes the harness's API server refuse pods in the workspace namespace the way the
// workspace's ResourceQuota admission does, while full is true.
type quotaGate struct {
	mu   sync.Mutex
	full bool
}

func (g *quotaGate) set(full bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.full = full
}

func (g *quotaGate) install(h *serviceHarness) {
	watch := h.client.(client.WithWatch)
	h.reconciler.Client = interceptor.NewClient(watch, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			g.mu.Lock()
			full := g.full
			g.mu.Unlock()
			if _, isPod := obj.(*corev1.Pod); isPod && full && obj.GetNamespace() == testWorkspaceName {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(),
					errorString(`exceeded quota: workspace-profile, requested: requests.cpu=2, used: requests.cpu=3500m, limited: requests.cpu=4`))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

type errorString string

func (e errorString) Error() string { return string(e) }

func (h *serviceHarness) reservation(slug string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testWorkspaceName, Name: methodologyReservationName(slug)}, pod)
	return pod, err
}

// orama-operator#799: a run counts against its workspace's quota. While the workspace's
// quota has no room the run waits as Queued, with nothing started and no credential minted; once
// there is room its reservation is admitted in the workspace's namespace and the run starts; the
// reservation goes when the run ends.
func TestAMethodologyRunWaitsForRoomInItsWorkspaceQuota(t *testing.T) {
	ctx := context.Background()
	h, store := newMethodologyHarness(t, false)
	gate := &quotaGate{full: true}
	gate.install(h)
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseQueued || !strings.Contains(status.Message, "the workspace's resource quota has no room for this run") ||
		!strings.Contains(status.Message, "exceeded quota") || status.CredentialSHA256 != "" || !h.changing {
		t.Fatalf("a run without room in its workspace quota: %+v", status)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyStageJob(testRunSlug)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the run's stage started without room in its workspace quota: %v", err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyCredentialSecret(testRunSlug)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a credential was minted for a run that cannot start: %v", err)
	}

	gate.set(false)
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseStaging {
		t.Fatalf("with room: %+v", status)
	}
	reservation, err := h.reservation(testRunSlug)
	if err != nil {
		t.Fatalf("no reservation in the workspace's namespace: %v", err)
	}
	spec := reservation.Spec
	if len(spec.SchedulingGates) != 1 || spec.SchedulingGates[0].Name != methodologyReservationGate || spec.NodeName != "" {
		t.Fatalf("the reservation could be scheduled: %+v", spec.SchedulingGates)
	}
	if !metav1.IsControlledBy(reservation, h.lake) || reservation.Labels[methodologyRunIDLabel] != testRunID ||
		reservation.Labels[methodologyRunRoleLabel] != methodologyRoleReservation || reservation.Labels[methodologyRunLakeLabel] != h.lake.Name {
		t.Fatalf("reservation metadata = %+v", reservation.ObjectMeta)
	}
	// The run's sandbox (2 CPU, 4Gi, requests equal to limits) is larger than its stage and collect
	// pods in everything but the transfer pods' 1 CPU limit, which is below 2: the sandbox's sizing.
	resources := spec.Containers[0].Resources
	for name, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "4Gi"} {
		if got := resources.Requests[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("reserved request %s = %s, want %s", name, got.String(), want)
		}
		if got := resources.Limits[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("reserved limit %s = %s, want %s", name, got.String(), want)
		}
	}
	if *spec.AutomountServiceAccountToken || !*spec.Containers[0].SecurityContext.RunAsNonRoot ||
		*spec.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("the reservation is not a restricted pod")
	}

	// A full quota later does not stop a run that has started.
	gate.set(true)
	h.finishJob(methodologyStageJob(testRunSlug), true)
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseRunning {
		t.Fatalf("a started run was held back: %+v", status)
	}

	// Stopped, its reservation goes with its other objects.
	store.setRunDesired(serviceDesiredAbsent)
	h.poll()
	if _, err := h.reservation(testRunSlug); !apierrors.IsNotFound(err) {
		t.Fatalf("the reservation outlived the run: %v", err)
	}
}

// A run larger than its workspace's whole quota can never start: it fails and says why.
func TestAMethodologyRunLargerThanItsWorkspaceQuotaFails(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	gate := &quotaGate{full: true}
	gate.install(h)
	h.create(&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: testWorkspaceName},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1"),
			corev1.ResourceRequestsMemory: resource.MustParse("8Gi")}}})
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	status := store.runStatus(t)
	if status.Phase != MethodologyPhaseFailed || status.Message != "the run asks for more than the workspace's whole quota allows (requests.cpu 2 > 1)" {
		t.Fatalf("a run larger than its workspace quota: %+v", status)
	}
}

// A run waits while its workspace has no namespace.
func TestAMethodologyRunWaitsForItsWorkspaceNamespace(t *testing.T) {
	run := testMethodologyRun()
	run.WorkspaceNamespace = ""
	run.Build = &LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"}
	store := &fakeServiceStore{methodologyRuns: []LakeMethodologyRun{run}, resources: map[workloadKey]WorkloadResources{}}
	h := newServiceHarness(t, methodologyTestLake(), store, pullSecretForTest())
	h.poll()
	if status := store.runStatus(t); status.Phase != MethodologyPhaseQueued ||
		status.Message != "Queued: the workspace has no namespace yet; the run starts when it does." {
		t.Fatalf("a run of a workspace without a namespace: %+v", status)
	}
}

// A served model counts against its workspace's quota as a run does, for as long as it is served.
func TestAServedModelWaitsForRoomInItsWorkspaceQuota(t *testing.T) {
	h, store := newDeploymentHarness(t, nil)
	gate := &quotaGate{full: true}
	gate.install(h)
	h.poll()
	status := store.deploymentStatus(t)
	if status.Phase != DeploymentPhaseQueued || !strings.Contains(status.Message, "no room for this model") || status.CredentialSHA256 != "" {
		t.Fatalf("a model without room in its workspace quota: %+v", status)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testRunNS, Name: methodologyDeploymentStageJob(testDeploymentSlug)},
		&batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the model's stage started without room: %v", err)
	}
	gate.set(false)
	advanceToReady(t, h, store)
	reservation, err := h.reservation(testDeploymentSlug)
	if err != nil {
		t.Fatalf("a served model has no reservation: %v", err)
	}
	// The serve pod (1 CPU, 2Gi, requests equal to limits) with the stage pod's larger limits.
	resources := reservation.Spec.Containers[0].Resources
	if cpu := resources.Requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("1")) != 0 ||
		reservation.Labels[methodologyDeploymentIDLabel] != testDeploymentID {
		t.Fatalf("model reservation = %+v %v", resources, reservation.Labels)
	}
	store.mu.Lock()
	store.deployments[0].Desired = serviceDesiredAbsent
	store.deployments[0].Generation++
	store.mu.Unlock()
	h.poll()
	if _, err := h.reservation(testDeploymentSlug); !apierrors.IsNotFound(err) {
		t.Fatalf("the reservation outlived the served model: %v", err)
	}
}

// A reservation whose run is no longer read (its workspace row went first, or its release failed
// before the run settled) is swept; one of a run still read is not.
func TestLeftoverMethodologyReservationsAreSwept(t *testing.T) {
	h, _ := newMethodologyHarness(t, false)
	lake := h.lake
	orphan := methodologyReservationPod("other-ws-000000000000", "mr-000000000000-quota", map[string]string{
		methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleReservation,
		methodologyRunIDLabel: "00000000-0000-4000-8000-000000000000"}, corev1.ResourceRequirements{})
	kept := methodologyReservationPod(testWorkspaceName, methodologyReservationName(testRunSlug), map[string]string{
		methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleReservation,
		methodologyRunIDLabel: testRunID}, corev1.ResourceRequirements{})
	foreign := methodologyReservationPod("other-ws-000000000000", "mr-111111111111-quota", map[string]string{
		methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleReservation,
		methodologyRunIDLabel: "11111111-1111-4111-8111-111111111111"}, corev1.ResourceRequirements{})
	for _, pod := range []*corev1.Pod{orphan, kept} {
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(lake, oramav1alpha1.GroupVersion.WithKind("Lake"))}
	}
	h.create(orphan)
	h.create(kept)
	h.create(foreign) // not controlled by the Lake: never touched
	h.poll()
	ctx := context.Background()
	if err := h.client.Get(ctx, client.ObjectKeyFromObject(orphan), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a leftover reservation was not swept: %v", err)
	}
	for _, pod := range []*corev1.Pod{kept, foreign} {
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
			t.Fatalf("%s was swept: %v", pod.Name, err)
		}
	}
}

// The methodology-runs namespace carries a ResourceQuota: the ceiling on every workspace's
// methodologies together, the operator's default or spec.methodologies.namespaceQuota.
func TestTheMethodologyRunsNamespaceCarriesACeilingQuota(t *testing.T) {
	ctx := context.Background()
	h, store := newMethodologyHarness(t, false)
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	var quota corev1.ResourceQuota
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyCeilingQuota}, &quota); err != nil {
		t.Fatalf("the methodology-runs namespace has no ResourceQuota: %v", err)
	}
	if cpu := quota.Spec.Hard[corev1.ResourceRequestsCPU]; cpu.Cmp(resource.MustParse("64")) != 0 || !metav1.IsControlledBy(&quota, h.lake) {
		t.Fatalf("default ceiling = %+v", quota.Spec.Hard)
	}
	if gpu := quota.Spec.Hard["requests."+resourceNvidiaGPU]; gpu.Cmp(resource.MustParse("8")) != 0 {
		t.Fatalf("default ceiling has no GPU limit: %+v", quota.Spec.Hard)
	}

	var lake oramav1alpha1.Lake
	if err := h.client.Get(ctx, client.ObjectKey{Name: h.lake.Name}, &lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Methodologies.NamespaceQuota = corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("12")}
	if err := h.client.Update(ctx, &lake); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testRunNS, Name: methodologyCeilingQuota}, &quota); err != nil {
		t.Fatal(err)
	}
	if cpu := quota.Spec.Hard[corev1.ResourceRequestsCPU]; len(quota.Spec.Hard) != 1 || cpu.Cmp(resource.MustParse("12")) != 0 {
		t.Fatalf("configured ceiling = %+v", quota.Spec.Hard)
	}
}

func TestReservationResourcesAreTheLargestOfEachPod(t *testing.T) {
	reserved := methodologyReservationResources(
		corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), resourceNvidiaGPU: resource.MustParse("1")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), resourceNvidiaGPU: resource.MustParse("1")},
		},
	)
	check := func(list corev1.ResourceList, name corev1.ResourceName, want string) {
		t.Helper()
		if got := list[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("%s = %s, want %s", name, got.String(), want)
		}
	}
	check(reserved.Requests, corev1.ResourceCPU, "500m")
	check(reserved.Requests, corev1.ResourceMemory, "512Mi")
	check(reserved.Requests, resourceNvidiaGPU, "1")
	check(reserved.Limits, corev1.ResourceCPU, "1")
	check(reserved.Limits, corev1.ResourceMemory, "1Gi")
	check(reserved.Limits, resourceNvidiaGPU, "1")
	if over := quotaNeverFits(corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("4"),
		corev1.ResourceLimitsMemory: resource.MustParse("512Mi"), "requests." + resourceNvidiaGPU: resource.MustParse("0")}, reserved); over !=
		"limits.memory 1Gi > 512Mi, requests.nvidia.com/gpu 1 > 0" {
		t.Fatalf("over = %q", over)
	}
}

// A refusal by the operator's own RBAC is an error to retry, never the run's failure.
func TestAnRBACRefusalOfTheReservationIsRetriedNotFailed(t *testing.T) {
	h, store := newMethodologyHarness(t, false)
	watch := h.client.(client.WithWatch)
	h.reconciler.Client = interceptor.NewClient(watch, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(),
					errorString(`User "system:serviceaccount:orama-system:orama-operator" cannot create resource "pods" in API group "" in the namespace "`+testWorkspaceName+`"`))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	store.setBuild(LakeMethodologyRunBuild{Desired: buildDesiredQueued, Phase: BuildPhaseSucceeded, Digest: testBuildDigest,
		Target: "lake-prod.lake.svc/acme/churn-methodology:build-1"})
	h.poll()
	store.mu.Lock()
	status := store.methodologyRuns[0].Status
	store.mu.Unlock()
	if status != nil && status.Phase == MethodologyPhaseFailed || !h.changing {
		t.Fatalf("an RBAC refusal failed the run: %+v (changing %v)", status, h.changing)
	}
}
