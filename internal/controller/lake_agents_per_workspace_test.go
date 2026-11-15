/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testAgentWorkspaceA = "aaaaaaaa-1111-4111-8111-111111111111"
	testAgentWorkspaceB = "bbbbbbbb-2222-4222-8222-222222222222"
)

func perWorkspaceAgentsLake() *oramav1alpha1.Lake {
	lake := agentsTestLake()
	lake.Spec.Agents.Isolation = oramav1alpha1.LakeAgentIsolationPerWorkspace
	return lake
}

func boundBusManifest(image string) *oramav1alpha1.LakeImageManifest {
	return &oramav1alpha1.LakeImageManifest{Image: image, Supported: true, WorkspaceBoundAgentBus: true}
}

// workspaceZettaFor is the workspace Zetta the workspaces reconciler makes for a running workspace.
func workspaceZettaFor(t *testing.T, h *serviceHarness, workspace string) *oramav1alpha1.OramaZettaCluster {
	t.Helper()
	cluster := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{
		Name:      workspaceZettaClusterName(h.lake, workspace),
		Namespace: "ws-" + workspace[:8],
		Labels:    map[string]string{workspaceLabel: workspace, workspaceLakeLabel: h.lake.Name},
	}}
	if err := controllerutil.SetControllerReference(h.lake, cluster, h.scheme); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Create(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// operator#1169: the image says whether it binds a bus credential to a workspace, in its manifest's
// features, and only for the image it was read from.
func TestLakeManifestReportsWorkspaceBoundAgentBus(t *testing.T) {
	document := func(features string) lakeManifestDocument {
		var parsed lakeManifestDocument
		raw := `{"manifest_version":1,"name":"lake","version":"v1","revision":"abc",
			"schema":{"version":"sha256:000000000000","latest_migration":"0001_a","guard_table":"a_guard","guard_tables":["a_guard"],"migrations":["0001_a"]},
			"migrate":{"args":["migrate"],"wait_flag":"--wait-seconds","database_env":"LAKE_CONTROL_DB_URI"},
			"stripe":{"webhook_events":["invoice.paid"],"products":[{"id":"lake_seat","name":"Seat"}]},
			"environment":[{"name":"LAKE_AGENT_BUS_DIR"}]` + features + `}`
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	bound, err := lakeManifestFromDocument(document(`,"features":["agent_bus_workspace_credentials"]`))
	if err != nil || !bound.WorkspaceBoundAgentBus {
		t.Fatalf("an image listing the feature: %+v %v", bound, err)
	}
	for name, features := range map[string]string{"no features": "", "other features": `,"features":["something_else"]`} {
		older, err := lakeManifestFromDocument(document(features))
		if err != nil || older.WorkspaceBoundAgentBus {
			t.Errorf("%s: %+v %v", name, older, err)
		}
	}

	lake := perWorkspaceAgentsLake()
	if !lakeImageBindsAgentBus(lake, boundBusManifest(lake.Spec.Image)) {
		t.Fatal("the current image said so")
	}
	for name, manifest := range map[string]*oramav1alpha1.LakeImageManifest{
		"no manifest":   nil,
		"another image": boundBusManifest("oramasearch/lake:old"),
		"it does not":   {Image: lake.Spec.Image, Supported: true},
	} {
		if lakeImageBindsAgentBus(lake, manifest) {
			t.Errorf("%s: an image that does not bind credentials would run agents per workspace", name)
		}
	}
}

// The running workspaces are the workspace Zettas this Lake controls: a managed Zetta service in a
// workspace namespace, another Lake's workspace, or a cluster being deleted does not count.
func TestTheRunningWorkspacesAreTheLakesWorkspaceZettas(t *testing.T) {
	ctx := context.Background()
	h := newServiceHarness(t, perWorkspaceAgentsLake(), &fakeServiceStore{})
	workspaceZettaFor(t, h, testAgentWorkspaceB)
	workspaceZettaFor(t, h, testAgentWorkspaceA)
	service := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod-svc-0123456789ab", Namespace: "ws-aaaaaaaa",
		Labels: map[string]string{workspaceLabel: testAgentWorkspaceA, workspaceLakeLabel: "prod"}}}
	other := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "dev-ws-cccccccc3333", Namespace: "ws-cccccccc",
		Labels: map[string]string{workspaceLabel: "cccccccc-3333-4333-8333-333333333333", workspaceLakeLabel: "dev"}}}
	for _, object := range []client.Object{service, other} {
		if err := controllerutil.SetControllerReference(h.lake, object, h.scheme); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	workspaces, err := h.reconciler.lakeAgentWorkspaces(ctx, h.lake)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 2 || workspaces[0] != testAgentWorkspaceA || workspaces[1] != testAgentWorkspaceB {
		t.Fatalf("workspaces = %v", workspaces)
	}
}

// A per-workspace Lake: one bus credential per (agent, workspace) and no unbound one; one process per
// credential, mounting only it, with no Service; no shared process; a workspace that goes takes its
// processes and credentials with it; going back to Shared restores the shared agents.
func TestPerWorkspaceAgentsHoldOnlyTheirWorkspacesCredential(t *testing.T) {
	ctx := context.Background()
	h := newServiceHarness(t, perWorkspaceAgentsLake(), &fakeServiceStore{})
	labels := map[string]string{"app.kubernetes.io/name": "lake"}
	if err := h.reconciler.reconcileLakeAgentServiceToken(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	// It starts shared, as every Lake did before.
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels, lakeAgentPlacement{}); err != nil {
		t.Fatal(err)
	}
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	workspaceZettaFor(t, h, testAgentWorkspaceA)
	workspaceZettaFor(t, h, testAgentWorkspaceB)
	manifest := lakeImageManifestState{Status: boundBusManifest(h.lake.Spec.Image), Current: true}
	placement, err := h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, manifest)
	if err != nil || !placement.perWorkspace || len(placement.workspaces) != 2 {
		t.Fatalf("placement = %+v, %v", placement, err)
	}
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels, placement); err != nil {
		t.Fatal(err)
	}
	var bus corev1.Secret
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "lake-prod-agent-bus"}, &bus); err != nil {
		t.Fatal(err)
	}
	if _, found := bus.Data["agent-ask-orama"]; found {
		t.Fatal("an unbound credential is left: a process holding it would reach every workspace")
	}
	keyA := workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceA)
	keyB := workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceB)
	tokenA := string(bus.Data[keyA])
	if len(tokenA) != 64 || len(bus.Data[keyB]) != 64 || tokenA == string(bus.Data[keyB]) {
		t.Fatalf("one distinct credential per (agent, workspace): %v", bus.Data)
	}
	if len(bus.Data) != len(lakeAgentLibrary(h.lake))*2 {
		t.Fatalf("%d credentials for %d agents and 2 workspaces", len(bus.Data), len(lakeAgentLibrary(h.lake)))
	}

	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-ask-orama"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the shared process still runs: %v", err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-ask-orama"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the shared Service is left: %v", err)
	}
	nameA := workspaceAgentName("prod", "agent-ask-orama", testAgentWorkspaceA)
	if nameA != "prod-agent-ask-orama-aaaaaaaa1111" {
		t.Fatalf("name = %q", nameA)
	}
	var agentA appsv1.Deployment
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: nameA}, &agentA); err != nil {
		t.Fatalf("workspace A's Ask Orama: %v", err)
	}
	container := agentA.Spec.Template.Spec.Containers[0]
	if ref := envValue(t, container.Env, "AGENT_BUS_TOKEN").ValueFrom.SecretKeyRef; ref.Name != "lake-prod-agent-bus" || ref.Key != keyA {
		t.Fatalf("workspace A's process reads credential %v", ref)
	}
	if envValue(t, container.Env, "AGENT_BUS_AGENT").Value != "agent-ask-orama" || container.Image != "oramasearch/agent-ask-orama:test" {
		t.Fatalf("container = %+v", container)
	}
	for _, variable := range container.Env {
		if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil &&
			variable.ValueFrom.SecretKeyRef.Name == "lake-prod-agent-bus" && variable.ValueFrom.SecretKeyRef.Key != keyA {
			t.Fatalf("workspace A's process holds another credential: %v", variable)
		}
	}
	if len(agentA.Spec.Template.Spec.Volumes) != 1 || agentA.Spec.Template.Spec.Volumes[0].Name != agentTempVolume {
		t.Fatalf("the process mounts more than its scratch space: %+v", agentA.Spec.Template.Spec.Volumes)
	}
	if agentA.Labels[workspaceLabel] != testAgentWorkspaceA || agentA.Labels[libraryAgentLabel] != "agent-ask-orama" ||
		agentA.Spec.Template.Labels["app.kubernetes.io/instance"] != nameA {
		t.Fatalf("labels = %v / %v", agentA.Labels, agentA.Spec.Template.Labels)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: nameA}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a workspace's process needs no Service: %v", err)
	}

	// Workspace B stops: its Zetta goes, and its credentials and processes follow.
	clusterB := &oramav1alpha1.OramaZettaCluster{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: "ws-bbbbbbbb", Name: workspaceZettaClusterName(h.lake, testAgentWorkspaceB)}, clusterB); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(ctx, clusterB); err != nil {
		t.Fatal(err)
	}
	placement, _ = h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, manifest)
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels, placement); err != nil {
		t.Fatal(err)
	}
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	_ = h.client.Get(ctx, client.ObjectKeyFromObject(&bus), &bus)
	if _, found := bus.Data[keyB]; found || string(bus.Data[keyA]) != tokenA {
		t.Fatalf("after B stopped: %v", bus.Data)
	}
	nameB := workspaceAgentName("prod", "agent-ask-orama", testAgentWorkspaceB)
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: nameB}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace B's process outlived its workspace: %v", err)
	}

	// Back to Shared: the shared agents and their credentials return, the workspace ones go.
	h.lake.Spec.Agents.Isolation = oramav1alpha1.LakeAgentIsolationShared
	placement, _ = h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, manifest)
	if err := h.reconciler.reconcileLakeAgentBusTokens(ctx, h.lake, testServiceNamespace, labels, placement); err != nil {
		t.Fatal(err)
	}
	if err := h.reconciler.reconcileLakeLibraryAgents(ctx, h.lake, testServiceNamespace, labels); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: "prod-agent-ask-orama"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("the shared agent did not return: %v", err)
	}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: testServiceNamespace, Name: nameA}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a workspace's process is left on a Shared Lake: %v", err)
	}
}

// An image that does not bind credentials keeps a PerWorkspace Lake shared, and says so; while a new
// image's manifest is read, what is in place is kept.
func TestAPerWorkspaceLakeStaysSharedUntilItsImageSupportsIt(t *testing.T) {
	ctx := context.Background()
	h := newServiceHarness(t, perWorkspaceAgentsLake(), &fakeServiceStore{})
	workspaceZettaFor(t, h, testAgentWorkspaceA)
	older := lakeImageManifestState{Status: &oramav1alpha1.LakeImageManifest{Image: h.lake.Spec.Image, Supported: true}, Current: true}
	placement, err := h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, older)
	if err != nil || !placement.requested || placement.perWorkspace {
		t.Fatalf("placement = %+v, %v", placement, err)
	}
	condition := lakeAgentIsolationCondition(h.lake, placement)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ImageNotSupported" {
		t.Fatalf("condition = %+v", condition)
	}
	shared := agentsTestLake()
	if lakeAgentIsolationCondition(shared, lakeAgentPlacement{}) != nil {
		t.Fatal("a Shared Lake carries no AgentIsolation condition")
	}

	// Nothing bound yet, and the new image's manifest not read: stay shared.
	reading := lakeImageManifestState{Status: boundBusManifest("oramasearch/lake:old"), Current: false}
	if placement, _ = h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, reading); placement.perWorkspace {
		t.Fatal("went per workspace on another image's manifest")
	}
	// Bound credentials in place, and a new image's manifest being read: keep them.
	bus := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-agent-bus", Namespace: testServiceNamespace},
		Data: map[string][]byte{workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceA): []byte("x")}}
	if err := h.client.Create(ctx, bus); err != nil {
		t.Fatal(err)
	}
	if placement, _ = h.reconciler.lakeAgentPlacementFor(ctx, h.lake, testServiceNamespace, reading); !placement.perWorkspace || len(placement.workspaces) != 1 {
		t.Fatalf("a manifest being read turned a running per-workspace Lake shared: %+v", placement)
	}
}

// lake reads its bus credentials at start: the pod template's hash follows the bound names and is
// absent for shared agents, so a Shared Lake's template does not change.
func TestTheLakePodRestartsWhenItsBoundCredentialsChange(t *testing.T) {
	if lakeAgentBusKeysHash([]string{"agent-ask-orama", "agent-project"}, false) != "" {
		t.Fatal("a Shared Lake's template carries the hash")
	}
	one := lakeAgentBusKeysHash([]string{workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceA)}, true)
	two := lakeAgentBusKeysHash([]string{workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceA),
		workspaceAgentBusKey("agent-ask-orama", testAgentWorkspaceB)}, true)
	if one == "" || one == two {
		t.Fatalf("hash %q then %q", one, two)
	}
	meta := metav1.ObjectMeta{}
	setLakeAgentBusKeysHash(&meta, one)
	if meta.Annotations[lakeAgentBusKeysAnnotation] != one {
		t.Fatal("annotation not set")
	}
	setLakeAgentBusKeysHash(&meta, "")
	if _, found := meta.Annotations[lakeAgentBusKeysAnnotation]; found {
		t.Fatal("annotation not cleared")
	}
	if agent, workspace := splitAgentBusKey(workspaceAgentBusKey("agent-project", testAgentWorkspaceA)); agent != "agent-project" || workspace != testAgentWorkspaceA {
		t.Fatalf("split = %q, %q", agent, workspace)
	}
	if long := workspaceAgentName("a-very-long-lake-name-indeed-xx", "agent-autosuggest-corpus", testAgentWorkspaceA); len(long) > 63 {
		t.Fatalf("name %q does not fit a label value", long)
	}
}
