// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// An App or environment is Ready only when its rollout is complete: while the old pod still serves
// and the new one starts, every count reaches desired but the new version does not answer yet
// (orama-operator#1049).
func TestADeploymentIsRolledOutOnlyWhenNoOldReplicaIsLeft(t *testing.T) {
	cases := []struct {
		name   string
		status appsv1.DeploymentStatus
		want   bool
	}{
		{"old pod serving, new pod starting", appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}, false},
		{"new pod ready, old pod terminating", appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2}, false},
		{"new pod ready, not yet available", appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 0}, false},
		{"rolled out", appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}, true},
		{"rolled out, three replicas", appsv1.DeploymentStatus{Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3}, true},
	}
	for _, c := range cases {
		deployment := &appsv1.Deployment{Status: c.status}
		desired := c.status.UpdatedReplicas
		if desired == 0 {
			desired = 1
		}
		if c.name == "rolled out, three replicas" {
			desired = 3
		}
		if got := deploymentRolledOut(deployment, desired); got != c.want {
			t.Errorf("%s: rolled out = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnAppMidRolloutIsProgressingNotReady(t *testing.T) {
	s := newLakeScheme(t)
	app := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "lake", UID: "app-uid"}}
	r := &LakeAppReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	phase, ready, _, _, err := r.lakeAppDeploymentState(context.Background(), app, deployment, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if ready || phase != oramav1alpha1.LakeAppPhaseProgressing {
		t.Fatalf("an App whose old pod still serves = %s ready=%v, want Progressing", phase, ready)
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if phase, ready, _, _, _ = r.lakeAppDeploymentState(context.Background(), app, deployment, "ws-1"); !ready || phase != oramav1alpha1.LakeAppPhaseReady {
		t.Fatalf("a rolled-out App = %s ready=%v, want Ready", phase, ready)
	}
}
