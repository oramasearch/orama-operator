// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestMethodologyReservationsAreAdmittedByTheWorkspaceQuotaEnvtest proves orama-operator#799
// against a real API server: its ResourceQuota, LimitRanger and PodSecurity admission, the same
// admission the workspace's apps and services pass. A reservation is admitted in a restricted
// workspace namespace and counted in its quota; one that does not fit in what is left waits; one
// larger than the whole quota fails; an app pod is then refused because the reservation holds the
// room; and the workspace's LimitRange refuses a reservation it would refuse for an app.
//
// envtest runs no quota controller, so the quota's status is seeded by hand as the controller
// would write it; ResourceQuota admission itself adds every admitted pod to status.used.
func TestMethodologyReservationsAreAdmittedByTheWorkspaceQuotaEnvtest(t *testing.T) {
	kube := startLakeEnvtest(t)
	ctx := context.Background()
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "quota"},
		Spec: oramav1alpha1.LakeSpec{
			Image:  "oramasearch/lake:test",
			Apps:   oramav1alpha1.LakeAppsSpec{Wildcard: "apps.example.com", RuntimeImage: "oramasearch/app-runtime:0.1.0"},
			Store:  oramav1alpha1.LakeStoreSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
			Builds: oramav1alpha1.LakeBuildsSpec{},
		},
	}
	if err := kube.Create(ctx, lake); err != nil {
		t.Fatal(err)
	}
	const namespace = "prod-ws-0123456789ab"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest",
	}}}); err != nil {
		t.Fatal(err)
	}
	plan := defaultWorkspacePlan()
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: namespace},
		Spec: corev1.ResourceQuotaSpec{Hard: plan.Quota.DeepCopy()}}
	if err := kube.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	quota.Status.Hard = plan.Quota.DeepCopy()
	quota.Status.Used = corev1.ResourceList{}
	for name := range plan.Quota {
		quota.Status.Used[name] = resource.MustParse("0")
	}
	if err := kube.Status().Update(ctx, quota); err != nil {
		t.Fatal(err)
	}
	limits := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: workspaceProfileName, Namespace: namespace},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer,
			Default: plan.Default.DeepCopy(), DefaultRequest: plan.DefaultRequest.DeepCopy()}}}}
	if err := kube.Create(ctx, limits); err != nil {
		t.Fatal(err)
	}

	r := &LakeReconciler{Client: kube, Scheme: kube.Scheme()}
	reserve := func(slug, id string, cpuMillis int32, memory int64) methodologyReservationOutcome {
		t.Helper()
		run := &LakeMethodologyRun{ID: id, Slug: slug, CPUMillis: cpuMillis, MemoryBytes: memory}
		outcome, err := r.reserveMethodologyQuota(ctx, lake, methodologyReservation{
			Namespace: namespace, Slug: slug, IDLabel: methodologyRunIDLabel, ID: id,
			Labels: methodologyLabels(lake, run, ""), Resources: methodologyRunReservation(run), What: "run",
		})
		if err != nil {
			t.Fatalf("reserve %s: %v", slug, err)
		}
		return outcome
	}

	// The default plan reserves 4 CPU of requests: a 3-CPU run fits.
	if outcome := reserve("mr-aaaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 3000, 4<<30); outcome != (methodologyReservationOutcome{}) {
		t.Fatalf("a run that fits: %+v", outcome)
	}
	var pod corev1.Pod
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "mr-aaaaaaaaaaaa-quota"}, &pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.SchedulingGates) != 1 || pod.Spec.NodeName != "" {
		t.Fatalf("the admitted reservation is not held unscheduled: %+v", pod.Spec)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if used := quota.Status.Used[corev1.ResourceRequestsCPU]; used.Cmp(resource.MustParse("3")) != 0 {
		t.Fatalf("the workspace quota does not count the reservation: used %v", quota.Status.Used)
	}
	// Reserving again changes nothing.
	if outcome := reserve("mr-aaaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 3000, 4<<30); outcome != (methodologyReservationOutcome{}) {
		t.Fatalf("an existing reservation: %+v", outcome)
	}

	// A second 2-CPU run does not fit in the 1 CPU left: it waits.
	outcome := reserve("mr-bbbbbbbbbbbb", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", 2000, 1<<30)
	if outcome.Fail != "" || !strings.Contains(outcome.Wait, "exceeded quota") {
		t.Fatalf("a run without room: %+v", outcome)
	}
	// A 6-CPU run never fits in a 4-CPU quota: it fails.
	outcome = reserve("mr-cccccccccccc", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", 6000, 1<<30)
	if outcome.Fail != "the run asks for more than the workspace's whole quota allows (requests.cpu 6 > 4)" {
		t.Fatalf("a run larger than the quota: %+v", outcome)
	}

	// The reservation holds the room against the workspace's own apps too.
	app := methodologyReservationPod(namespace, "app", map[string]string{"app": "x"}, corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	})
	app.Spec.SchedulingGates = nil
	if err := kube.Create(ctx, app); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("an app pod was admitted past the methodology's reservation: %v", err)
	}

	// The workspace's LimitRange applies as it does to an app: a per-container maximum refuses it.
	limits.Spec.Limits[0].Max = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	if err := kube.Update(ctx, limits); err != nil {
		t.Fatal(err)
	}
	outcome = reserve("mr-dddddddddddd", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", 1500, 1<<30)
	if !strings.HasPrefix(outcome.Fail, "the workspace's limits refuse this run: ") || !strings.Contains(outcome.Fail, "cpu") {
		t.Fatalf("a run above the LimitRange: %+v", outcome)
	}

	// Released, the reservation is gone.
	if err := r.releaseMethodologyQuota(ctx, lake, namespace, "mr-aaaaaaaaaaaa", methodologyRunIDLabel, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "mr-aaaaaaaaaaaa-quota"}, &pod); err == nil && pod.DeletionTimestamp.IsZero() {
		t.Fatal("the released reservation is still there")
	}
}
