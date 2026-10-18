/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const historyExpiredMessage = "REPLICATION_HISTORY_EXPIRED: reader requires offset 0 but first retained offset is 81234"

// readerPod is a reader pod of oc whose oramacore container has the given state and last state.
func readerPod(oc *oramav1alpha1.OramaCloudV1, name string, state, last corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: labelsFor(oc, "reader")},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "oramacore", State: state, LastTerminationState: last},
		}},
	}
}

func terminated(code int32, message string) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: message}}
}

var crashLoop = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}

func TestReaderRefusalCause(t *testing.T) {
	oc := newOCV1(nil)
	deleted := readerPod(oc, "deleted", terminated(1, historyExpiredMessage), corev1.ContainerState{})
	deleted.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	deleted.Finalizers = []string{"test/hold"}

	for _, tc := range []struct {
		name string
		pods []client.Object
		want string
	}{
		{"history expired", []client.Object{readerPod(oc, "a", terminated(1, historyExpiredMessage), corev1.ContainerState{})}, readerHistoryExpiredCause},
		{"stream behind", []client.Object{readerPod(oc, "a", terminated(1, "REPLICATION_STREAM_BEHIND: reader requires offset 900 but stream head is 12"), corev1.ContainerState{})}, readerStreamBehindCause},
		{"waiting reads last state", []client.Object{readerPod(oc, "a", crashLoop, terminated(1, historyExpiredMessage))}, readerHistoryExpiredCause},
		{"exit code 0", []client.Object{readerPod(oc, "a", terminated(0, historyExpiredMessage), corev1.ContainerState{})}, ""},
		{"unrelated message", []client.Object{readerPod(oc, "a", crashLoop, terminated(1, "panicked at src/main.rs"))}, ""},
		{"same code twice", []client.Object{
			readerPod(oc, "a", terminated(1, historyExpiredMessage), corev1.ContainerState{}),
			readerPod(oc, "b", crashLoop, terminated(1, historyExpiredMessage)),
		}, readerHistoryExpiredCause},
		{"both codes", []client.Object{
			readerPod(oc, "a", terminated(1, historyExpiredMessage), corev1.ContainerState{}),
			readerPod(oc, "b", terminated(1, "REPLICATION_STREAM_BEHIND: ahead"), corev1.ContainerState{}),
		}, readerStreamBehindCause + "; " + readerHistoryExpiredCause},
		{"deleted pod ignored", []client.Object{deleted}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ocv1TestScheme(t)
			r := &OramaCloudV1Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(tc.pods...).Build(), Scheme: s}
			if got := r.readerRefusalCause(context.Background(), oc); got != tc.want {
				t.Errorf("readerRefusalCause = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReaderDetailCarriesRefusalOnlyWhenNotReady: the reader component's detail names the refusal
// while the reader is not Ready, and stays the plain n/total count once it is.
func TestReaderDetailCarriesRefusalOnlyWhenNotReady(t *testing.T) {
	oc := newOCV1(nil)
	for _, tc := range []struct {
		name       string
		ready      int32
		wantDetail string
	}{
		{"not ready", 0, "0/1; " + readerHistoryExpiredCause},
		{"ready", 1, "1/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: oc.Namespace, Labels: labelsFor(oc, "reader")},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: tc.ready},
			}
			pod := readerPod(oc, "a", crashLoop, terminated(1, historyExpiredMessage))
			s := ocv1TestScheme(t)
			r := &OramaCloudV1Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(dep, pod).Build(), Scheme: s}
			var reader *oramav1alpha1.ComponentStatus
			comps := r.componentReadiness(context.Background(), oc)
			for i := range comps {
				if comps[i].Name == "reader" {
					reader = &comps[i]
				}
			}
			if reader == nil {
				t.Fatalf("no reader component in %+v", comps)
			}
			if reader.Detail != tc.wantDetail {
				t.Errorf("reader detail = %q, want %q", reader.Detail, tc.wantDetail)
			}
		})
	}
}
