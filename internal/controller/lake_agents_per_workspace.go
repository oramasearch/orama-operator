/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// One library-agent process per workspace (operator#1169, lake#871, lake PR #1304).
//
// A library agent normally runs as one shared process per Lake, with one bus credential that
// reaches every workspace's tasks. Lake now accepts a LAKE_AGENT_BUS_DIR file named
// <agent>.<workspace id>: a bus credential bound to that workspace, which receives, claims, reports
// and acts only for that workspace's tasks, always by reference, so the agent process never holds
// another tenant's task credential.
//
// Which Lakes do this follows from the resource, never from a flag: spec.agents.isolation
// PerWorkspace on the Lake (an enterprise or isolated install). The workspaces it runs agents for
// are the resources that exist: every workspace Zetta (OramaZettaCluster) the Lake controls, which
// the workspaces reconciler creates for a running workspace and deletes when the workspace stops or
// is deleted. For a PerWorkspace Lake whose image supports it:
//
//   - the bus Secret lake-<lake>-agent-bus holds one credential per (agent, workspace), keyed
//     <agent>.<workspace id>, and no unbound <agent> key, so no process can take another
//     workspace's task;
//   - each (agent, workspace) runs as its own Deployment <lake>-<agent>-<12 hex of the workspace>
//     in the Lake's namespace, beside lake, mounting only its own credential. It has no Service:
//     lake dispatches library-agent tasks over the bus, and LAKE_AGENT_ENDPOINTS only lists which
//     agents the Lake runs;
//   - a workspace whose Zetta goes (stopped or deleted) loses its credentials and its processes;
//   - lake reads LAKE_AGENT_BUS_DIR when it starts, so the Lake's pod template carries a hash of the
//     credential names, and a workspace added or removed restarts lake once.
//
// The bus Secret is the record of what is in place: the library agents and the Lake pod read it,
// so the processes, the credentials and lake always agree. While a new image's manifest is read,
// what is in place is kept.
//
// The switch is what the image says: its manifest features list agent_bus_workspace_credentials
// (status.imageManifest.workspaceBoundAgentBus). Until then a PerWorkspace Lake keeps its shared
// agents and condition AgentIsolation is False with the reason.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// lakeFeatureWorkspaceBoundAgentBus is the manifest feature of a lake image that binds a bus
	// credential named <agent>.<workspace id> to that workspace (lake#871).
	lakeFeatureWorkspaceBoundAgentBus = "agent_bus_workspace_credentials"
	// lakeAgentBusKeysAnnotation on the Lake pod template is a hash of the bus credential names, so
	// lake, which reads LAKE_AGENT_BUS_DIR at start, restarts when a workspace's are added or removed.
	lakeAgentBusKeysAnnotation = "orama.cloud/agent-bus-keys"
	// lakeAgentIsolationConditionType exists only on a Lake with spec.agents.isolation PerWorkspace.
	lakeAgentIsolationConditionType = "AgentIsolation"
	// lakeAgentBusKeySeparator splits <agent>.<workspace id>, as lake's agent_bus_key does.
	lakeAgentBusKeySeparator = "."
)

// agentWorkspaceIDPattern is a workspace id the operator will put in a Secret key and a label value:
// lake's ids are UUIDs.
var agentWorkspaceIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// lakeAgentPlacement is where one reconcile runs the library agents.
type lakeAgentPlacement struct {
	// requested is spec.agents.isolation PerWorkspace.
	requested bool
	// perWorkspace is true when the agents run per workspace: requested, and the image supports it.
	perWorkspace bool
	// workspaces are the running workspaces' ids, sorted.
	workspaces []string
}

// lakeAgentsPerWorkspaceRequested reports whether the Lake asks for one agent process per workspace.
func lakeAgentsPerWorkspaceRequested(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Agents.Isolation == oramav1alpha1.LakeAgentIsolationPerWorkspace
}

// lakeImageBindsAgentBus reports whether spec.image's manifest lists the workspace-bound bus
// credential feature. A manifest of another image, or none, is false.
func lakeImageBindsAgentBus(lake *oramav1alpha1.Lake, manifest *oramav1alpha1.LakeImageManifest) bool {
	return manifest != nil && manifest.Supported && manifest.WorkspaceBoundAgentBus &&
		manifest.Image != "" && manifest.Image == lake.Spec.Image
}

// workspaceAgentBusKey is the bus credential name of agent bound to workspace.
func workspaceAgentBusKey(agent, workspace string) string {
	return agent + lakeAgentBusKeySeparator + workspace
}

// splitAgentBusKey returns the agent and the bound workspace of a bus credential name ("" for an
// unbound one).
func splitAgentBusKey(key string) (string, string) {
	agent, workspace, _ := strings.Cut(key, lakeAgentBusKeySeparator)
	return agent, workspace
}

// workspaceAgentName names the Deployment of agent for workspace: <lake>-<agent>-<12 hex>, or a
// hash of both when that would not fit a label value.
func workspaceAgentName(lake, agent, workspace string) string {
	hexID := strings.ReplaceAll(strings.ToLower(workspace), "-", "")
	if len(hexID) > 12 {
		hexID = hexID[:12]
	}
	name := lake + "-" + agent + "-" + hexID
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(agent + lakeAgentBusKeySeparator + workspace))
	return lake + "-wa-" + hex.EncodeToString(sum[:])[:16]
}

// lakeAgentPlacementFor decides where this reconcile runs the library agents. A Shared Lake needs no
// read. A PerWorkspace Lake runs per workspace when spec.image's manifest supports it; while a new
// image's manifest is being read, what is in place (the bus Secret's bound credentials) is kept.
func (r *LakeReconciler) lakeAgentPlacementFor(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	manifest lakeImageManifestState,
) (lakeAgentPlacement, error) {
	placement := lakeAgentPlacement{requested: lakeAgentsPerWorkspaceRequested(lake)}
	if !placement.requested || len(lakeAgentLibrary(lake)) == 0 {
		return placement, nil
	}
	if manifest.Current {
		placement.perWorkspace = lakeImageBindsAgentBus(lake, manifest.Status)
	} else {
		_, bound, err := r.lakeAgentBusKeys(ctx, lake, namespace)
		if err != nil {
			return placement, err
		}
		placement.perWorkspace = bound
	}
	if !placement.perWorkspace {
		return placement, nil
	}
	workspaces, err := r.lakeAgentWorkspaces(ctx, lake)
	if err != nil {
		return placement, err
	}
	placement.workspaces = workspaces
	return placement, nil
}

// lakeAgentWorkspaces are the ids of the workspaces that run now: one per workspace Zetta this Lake
// controls and is not deleting. A managed Zetta service in a workspace namespace carries the
// workspace label too, so only the cluster with the workspace Zetta's own name counts.
func (r *LakeReconciler) lakeAgentWorkspaces(ctx context.Context, lake *oramav1alpha1.Lake) ([]string, error) {
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &clusters, client.MatchingLabels{workspaceLakeLabel: lake.Name}, client.HasLabels{workspaceLabel}); err != nil {
		return nil, fmt.Errorf("list workspace Zettas: %w", err)
	}
	var workspaces []string
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		id := cluster.Labels[workspaceLabel]
		if !agentWorkspaceIDPattern.MatchString(id) || cluster.Name != workspaceZettaClusterName(lake, id) ||
			!metav1.IsControlledBy(cluster, lake) || !cluster.DeletionTimestamp.IsZero() {
			continue
		}
		workspaces = append(workspaces, strings.ToLower(id))
	}
	slices.Sort(workspaces)
	return slices.Compact(workspaces), nil
}

// lakeAgentBusWantedKeys are the bus credential names a placement needs: one per library agent, or
// for a per-workspace Lake one per (agent, workspace) and no unbound one.
func lakeAgentBusWantedKeys(lake *oramav1alpha1.Lake, placement lakeAgentPlacement) map[string]bool {
	wanted := map[string]bool{}
	for _, agent := range lakeAgentLibrary(lake) {
		if !placement.perWorkspace {
			wanted[agent.Key] = true
			continue
		}
		for _, workspace := range placement.workspaces {
			wanted[workspaceAgentBusKey(agent.Key, workspace)] = true
		}
	}
	return wanted
}

// lakeAgentBusKeys reads the bus Secret's credential names, sorted, and whether any is bound to a
// workspace. A missing Secret has none.
func (r *LakeReconciler) lakeAgentBusKeys(ctx context.Context, lake *oramav1alpha1.Lake, namespace string) ([]string, bool, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeAgentBusSecret(lake.Name)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read agent bus credentials: %w", err)
	}
	keys := make([]string, 0, len(secret.Data))
	bound := false
	for key, value := range secret.Data {
		if len(value) == 0 {
			continue
		}
		keys = append(keys, key)
		if _, workspace := splitAgentBusKey(key); workspace != "" {
			bound = true
		}
	}
	slices.Sort(keys)
	return keys, bound, nil
}

// lakeAgentBusKeysHash is the Lake pod template's annotation value: empty while no credential is
// bound to a workspace, so a Shared Lake's template is unchanged.
func lakeAgentBusKeysHash(keys []string, bound bool) string {
	if !bound {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// setLakeAgentBusKeysHash sets or clears the annotation.
func setLakeAgentBusKeysHash(template *metav1.ObjectMeta, hash string) {
	if hash == "" {
		delete(template.Annotations, lakeAgentBusKeysAnnotation)
		return
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[lakeAgentBusKeysAnnotation] = hash
}

// lakeAgentIsolationCondition is AgentIsolation, or nil for a Shared Lake.
func lakeAgentIsolationCondition(lake *oramav1alpha1.Lake, placement lakeAgentPlacement) *metav1.Condition {
	if !placement.requested {
		return nil
	}
	condition := &metav1.Condition{Type: lakeAgentIsolationConditionType, ObservedGeneration: lake.Generation}
	switch {
	case len(lakeAgentLibrary(lake)) == 0:
		condition.Status, condition.Reason = metav1.ConditionTrue, "NoLibraryAgents"
		condition.Message = "the Lake runs no library agent"
	case placement.perWorkspace:
		condition.Status, condition.Reason = metav1.ConditionTrue, "PerWorkspace"
		condition.Message = fmt.Sprintf("each library agent runs one process per running workspace (%d), each with only its workspace's bus credential",
			len(placement.workspaces))
	default:
		condition.Status, condition.Reason = metav1.ConditionFalse, "ImageNotSupported"
		condition.Message = fmt.Sprintf("spec.image's manifest does not list the %s feature, so the library agents stay shared until a lake image that binds a bus credential to a workspace rolls",
			lakeFeatureWorkspaceBoundAgentBus)
	}
	return condition
}
