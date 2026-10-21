/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// daemonSetNodes is the nodes a rendered DaemonSet's pods can run on: those its required node
// affinity names by metadata.name, or every node when it has none.
func daemonSetNodes(daemonSet *appsv1.DaemonSet, all []string) []string {
	affinity := daemonSet.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return all
	}
	var nodes []string
	for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions) > 0 {
			return all
		}
		for _, field := range term.MatchFields {
			if field.Key != "metadata.name" || field.Operator != corev1.NodeSelectorOpIn {
				return all
			}
			nodes = append(nodes, field.Values...)
		}
	}
	return nodes
}

// TestEachMeshNodeReceivesOnlyItsOwnWorkloadKeys is LSR-039 (orama-operator#788): kubelet delivers
// every Secret a pod mounts to that pod's node, so root on one node read every node's workload
// private keys when the one DaemonSet mounted every template shard. Read from the rendered objects:
// every private key a mesh pod on a node can receive belongs to a workload on that node.
func TestEachMeshNodeReceivesOnlyItsOwnWorkloadKeys(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	app := map[string]string{lakeAppIdentityLabel: "web"}
	db := map[string]string{serviceIDLabel: testConnectionTargetID}
	sandbox := meshSandbox(true)
	nodes := []string{"node-a", "node-b", "node-c"}
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testTunnelNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}},
		sandbox,
		meshPodFor("web-1", "node-a", "10.244.0.10", app),
		meshPodFor("web-2", "node-a", "10.244.0.11", app),
		meshPodFor("db-0", "node-b", "10.244.1.20", db),
	}
	for _, node := range nodes {
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.NetworkSandbox{}, &appsv1.DaemonSet{}).
		WithObjects(objects...).Build()
	r := &zettaTunnelMeshReconciler{Client: kube, APIReader: kube, Scheme: scheme, OperatorNamespace: "orama-system",
		ProjectorImage: "oramasearch/orama-operator:test", Now: func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }}
	markRendered(t, kube, sandbox)
	if _, err := r.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{ // workload key file → the node its pod runs on
		"web-1.workspace.mesh.orama.internal.key": "node-a",
		"web-2.workspace.mesh.orama.internal.key": "node-a",
		"db-0.workspace.mesh.orama.internal.key":  "node-b",
	}

	var daemonSets appsv1.DaemonSetList
	if err := kube.List(ctx, &daemonSets, client.InNamespace(zettaTunnelMeshNamespace)); err != nil {
		t.Fatal(err)
	}
	covered := map[string]int{}
	delivered := map[string]bool{}
	for i := range daemonSets.Items {
		daemonSet := &daemonSets.Items[i]
		runsOn := daemonSetNodes(daemonSet, nodes)
		for _, node := range runsOn {
			covered[node]++
		}
		for _, volume := range daemonSet.Spec.Template.Spec.Volumes {
			if volume.Secret == nil {
				continue
			}
			var secret corev1.Secret
			if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: volume.Secret.SecretName}, &secret); err != nil {
				continue
			}
			for key, value := range secret.Data {
				if !strings.Contains(string(value), "PRIVATE KEY") {
					continue
				}
				delivered[key] = true
				for _, node := range runsOn {
					if owner[key] != node {
						t.Errorf("DaemonSet %s runs on %s and mounts Secret %s, which holds %s of a workload on %s", daemonSet.Name, node, secret.Name, key, owner[key])
					}
				}
			}
		}
	}
	for _, node := range nodes {
		if covered[node] != 1 {
			t.Errorf("node %s runs %d mesh agents, want exactly 1", node, covered[node])
		}
	}
	for key := range owner {
		if !delivered[key] {
			t.Errorf("%s reaches no node", key)
		}
	}
}

// The operator's single DaemonSet and its shard Secrets, which held every node's keys, are deleted on
// the first pass; a node that leaves the cluster takes its agent and Secret with it. Objects that are
// not the operator's are left alone.
func TestMeshRemovesTheSharedShardsAndAGoneNodesAgent(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	owned := zettaTunnelMeshObjectLabels()
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelMeshName, Namespace: zettaTunnelMeshNamespace, Labels: owned}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh-templates-0", Namespace: zettaTunnelMeshNamespace, Labels: owned}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh-templates-15", Namespace: zettaTunnelMeshNamespace, Labels: owned}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh-templates-7", Namespace: zettaTunnelMeshNamespace}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &zettaTunnelMeshReconciler{Client: kube, APIReader: kube, Scheme: scheme, OperatorNamespace: "orama-system", ProjectorImage: "oramasearch/orama-operator:test"}
	if _, err := r.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		t.Fatal(err)
	}
	exists := func(object client.Object, name string) bool {
		t.Helper()
		err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: name}, object)
		if err != nil && client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
		return err == nil
	}
	if exists(&appsv1.DaemonSet{}, zettaTunnelMeshName) || exists(&corev1.Secret{}, "mesh-templates-0") || exists(&corev1.Secret{}, "mesh-templates-15") {
		t.Fatal("the shared DaemonSet or a shard Secret is still there")
	}
	if !exists(&corev1.Secret{}, "mesh-templates-7") {
		t.Fatal("a Secret that is not the operator's was deleted")
	}
	for _, node := range []string{"node-a", "node-b"} {
		if !exists(&appsv1.DaemonSet{}, meshNodeDaemonSet(node)) || !exists(&corev1.Secret{}, meshNodeSecret(node)) {
			t.Fatalf("node %s has no agent or Secret", node)
		}
	}

	if err := kube.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		t.Fatal(err)
	}
	if exists(&appsv1.DaemonSet{}, meshNodeDaemonSet("node-b")) || exists(&corev1.Secret{}, meshNodeSecret("node-b")) {
		t.Fatal("a gone node's agent or Secret is still there")
	}
	if !exists(&appsv1.DaemonSet{}, meshNodeDaemonSet("node-a")) {
		t.Fatal("node-a's agent was deleted")
	}
}
