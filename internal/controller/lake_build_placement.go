/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Where a tenant build runs (operator#759). Rootless BuildKit needs an unconfined seccomp and
// AppArmor profile, so a kernel exploit in a Dockerfile's RUN step reaches the node. Builds are
// therefore kept off the nodes everything else runs on, as soon as a cluster has nodes for them:
//
//   - every build pod tolerates the build taint orama.cloud/builds=true:NoSchedule, which nothing
//     else tolerates, so a tainted build pool runs builds only;
//   - when any node carries the label orama.cloud/builds=true, every build pod requires it (a
//     nodeSelector) and refuses a node another build runs on (required anti-affinity on
//     app.kubernetes.io/name=lake-builder): a node escape reaches only that one build;
//   - while no node carries it, builds go where there is room, as before, and only prefer a node
//     with no other build.
//
// The choice follows the cluster's nodes, never a setting: the code is the same on every cluster,
// and a cluster gets dedicated build nodes by having a pool with that label and taint, made in its
// KubernetesCluster (and helmfile.yaml's cluster for the management cluster). Kata
// (runtimeClassName) comes later, once that pool runs a sandboxed runtime.
const (
	buildNodeLabel = "orama.cloud/builds"
	buildNodeValue = "true"
	buildNodeTaint = "orama.cloud/builds"
)

// clusterHasBuildNodes is whether any node is labelled for builds.
func clusterHasBuildNodes(ctx context.Context, reader client.Reader) (bool, error) {
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels{buildNodeLabel: buildNodeValue}, client.Limit(1)); err != nil {
		return false, fmt.Errorf("list build nodes: %w", err)
	}
	return len(nodes.Items) > 0, nil
}

// placeBuildPod sets a build pod's toleration, node selection and anti-affinity.
func placeBuildPod(pod *corev1.PodSpec, dedicatedNodes bool) {
	pod.Tolerations = append(pod.Tolerations, corev1.Toleration{
		Key: buildNodeTaint, Operator: corev1.TolerationOpEqual, Value: buildNodeValue, Effect: corev1.TaintEffectNoSchedule,
	})
	otherBuilds := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "lake-builder"}},
		TopologyKey:   "kubernetes.io/hostname",
		// Builds of every Lake and every build namespace on the node.
		NamespaceSelector: &metav1.LabelSelector{},
	}
	if pod.Affinity == nil {
		pod.Affinity = &corev1.Affinity{}
	}
	if dedicatedNodes {
		if pod.NodeSelector == nil {
			pod.NodeSelector = map[string]string{}
		}
		pod.NodeSelector[buildNodeLabel] = buildNodeValue
		pod.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{otherBuilds},
		}
		return
	}
	pod.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: otherBuilds}},
	}
}
