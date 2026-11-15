/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Every build tolerates the build taint; with build nodes it requires them and one build per
// node; without, it only prefers a node with no other build (operator#759).
func TestBuildPodPlacementFollowsTheClustersBuildNodes(t *testing.T) {
	var shared corev1.PodSpec
	placeBuildPod(&shared, false)
	if len(shared.Tolerations) != 1 || shared.Tolerations[0].Key != buildNodeTaint || shared.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("tolerations = %+v", shared.Tolerations)
	}
	if shared.NodeSelector[buildNodeLabel] != "" || shared.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil ||
		len(shared.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("a cluster without build nodes must not require them: %+v", shared)
	}
	var dedicated corev1.PodSpec
	placeBuildPod(&dedicated, true)
	required := dedicated.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if dedicated.NodeSelector[buildNodeLabel] != buildNodeValue || len(required) != 1 ||
		required[0].TopologyKey != "kubernetes.io/hostname" || required[0].LabelSelector.MatchLabels["app.kubernetes.io/name"] != "lake-builder" {
		t.Fatalf("dedicated placement = %+v", dedicated)
	}

	s := newLakeScheme(t)
	ctx := context.Background()
	without := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}).Build()
	if found, err := clusterHasBuildNodes(ctx, without); err != nil || found {
		t.Fatalf("found %t, err %v: no node is labelled", found, err)
	}
	with := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b1", Labels: map[string]string{buildNodeLabel: buildNodeValue}}}).Build()
	if found, err := clusterHasBuildNodes(ctx, with); err != nil || !found {
		t.Fatalf("found %t, err %v: a build node exists", found, err)
	}
}
