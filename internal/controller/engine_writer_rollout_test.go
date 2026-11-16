// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stalledWriterPod(sts *appsv1.StatefulSet) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: sts.Name + "-0", Namespace: sts.Namespace, UID: "old-writer-pod",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: &controller}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "oramacore", Image: "old-engine@sha256:old"}}},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "oramacore", RestartCount: 4,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}},
		},
	}
}

func TestWriterImageRolloutRecoversOnlyBrokenPreviousImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod, *appsv1.StatefulSet)
		remove bool
	}{
		{"broken previous image", func(*corev1.Pod, *appsv1.StatefulSet) {}, true},
		{"current image", func(p *corev1.Pod, s *appsv1.StatefulSet) {
			p.Spec.Containers[0].Image = s.Spec.Template.Spec.Containers[0].Image
		}, false},
		{"healthy previous image", func(p *corev1.Pod, _ *appsv1.StatefulSet) { p.Status.Conditions[0].Status = corev1.ConditionTrue }, false},
		{"ready engine", func(p *corev1.Pod, _ *appsv1.StatefulSet) { p.Status.ContainerStatuses[0].Ready = true }, false},
		{"starting engine", func(p *corev1.Pod, _ *appsv1.StatefulSet) {
			p.Status.ContainerStatuses[0].State.Waiting.Reason = "ContainerCreating"
		}, false},
		{"running engine", func(p *corev1.Pod, _ *appsv1.StatefulSet) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}, false},
		{"no previous failure", func(p *corev1.Pod, _ *appsv1.StatefulSet) {
			p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
		}, false},
		{"successful prior exit", func(p *corev1.Pod, _ *appsv1.StatefulSet) {
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 0
		}, false},
		{"terminating pod", func(p *corev1.Pod, _ *appsv1.StatefulSet) {
			now := metav1.NewTime(time.Now())
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test-retention"}
		}, false},
		{"unowned pod", func(p *corev1.Pod, _ *appsv1.StatefulSet) { p.OwnerReferences[0].UID = "other-statefulset" }, false},
		{"unowned workload", func(_ *corev1.Pod, s *appsv1.StatefulSet) { s.OwnerReferences[0].UID = "other-cloud" }, false},
		{"scaled down workload", func(_ *corev1.Pod, s *appsv1.StatefulSet) { s.Spec.Replicas = int32Ptr(0) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t)
			oc := engineOC(nil)
			h.reconcile(oc)
			sts := h.writer()
			sts.UID = "writer-statefulset"
			pod := stalledWriterPod(sts)
			tc.mutate(pod, sts)
			if err := h.cl.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: sts.Namespace}}
			if err := h.cl.Create(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if err := h.r.recoverWriterImageRollout(context.Background(), oc, sts); err != nil {
				t.Fatal(err)
			}
			got := &corev1.Pod{}
			err := h.cl.Get(context.Background(), client.ObjectKeyFromObject(pod), got)
			if apierrors.IsNotFound(err) != tc.remove {
				t.Fatalf("removed=%v, want %v (error=%v)", apierrors.IsNotFound(err), tc.remove, err)
			}
			if err := h.cl.Get(context.Background(), client.ObjectKeyFromObject(claim), &corev1.PersistentVolumeClaim{}); err != nil {
				t.Fatalf("writer claim changed: %v", err)
			}
		})
	}
}

func TestWriterReconcileUsesRequestedImageAndGuardsPodReplacement(t *testing.T) {
	h := newEngineHarness(t)
	oc := engineOC(nil)
	h.reconcile(oc)
	sts := h.writer()
	sts.UID = "writer-statefulset"
	if err := h.cl.Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	pod := stalledWriterPod(sts)
	if err := h.cl.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Pod{}
	if err := h.cl.Get(context.Background(), client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	deleted := false
	h.r.Client = interceptor.NewClient(h.cl.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); !ok {
				return c.Delete(ctx, obj, opts...)
			}
			deleted = true
			options := &client.DeleteOptions{}
			options.ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != stored.UID ||
				options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != stored.ResourceVersion || options.GracePeriodSeconds != nil {
				t.Fatalf("delete must guard UID/version and preserve graceful termination: %+v", options)
			}
			current := &appsv1.StatefulSet{}
			if err := c.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, current); err != nil {
				t.Fatal(err)
			}
			if current.Spec.Template.Spec.Containers[0].Image == pod.Spec.Containers[0].Image {
				t.Fatal("old template still selects broken image")
			}
			return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, nil)
		},
	})
	if err := h.r.reconcileWriter(context.Background(), oc, "", ""); !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict to retry safely, got %v", err)
	}
	if !deleted {
		t.Fatal("writer reconcile did not attempt guarded recovery")
	}
	if err := h.cl.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("conflicting pod was removed: %v", err)
	}
}
