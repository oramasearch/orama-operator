/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Every Lake runs every feature (operator#539): there is no per-Lake switch. This file is the one
// table of the images a Lake runs when its spec leaves one empty, so a LakeSet member states only
// what differs per environment (host, args, env, secrets). A spec image still overrides its default.
//
// The defaults are pinned to what the managing cluster's Lake/prod ran on 2026-10-17. Moving a
// version is a change to this table, rolled through the pipeline dev, testing, staging, prod.
const (
	// lakeDefaultZettaImage is the Zetta of the Lake's own data plane, of every workspace and of
	// every managed Zetta service.
	lakeDefaultZettaImage = "oramasearch/zetta:main-4015ef76"
	// lakeDefaultAppsRuntimeImage runs runtime-mode apps (spec.apps.runtimeImage).
	lakeDefaultAppsRuntimeImage = "oramasearch/app-runtime:976e6a8"
	// lakeDefaultBuilderImage runs every image build (spec.builds.builderImage).
	lakeDefaultBuilderImage = "oramasearch/lake-builder:31a48d9"
	// lakeDefaultMethodologyRuntimeImage is the base of every methodology image and runs its stage
	// and collect pods (spec.methodologies.runtimeImage).
	lakeDefaultMethodologyRuntimeImage = "oramasearch/methodology-runtime:ac58062"
	// lakeDefaultAgentRuntimeImage runs every user-created agent (spec.agents.runtimeImage).
	lakeDefaultAgentRuntimeImage = "oramasearch/agent-runtime:f8feb14"
	// lakeDefaultMCPRunnerImage is LAKE_MCP_RUNNER_IMAGE, the image lake runs MCP servers on.
	// spec.env may still set the variable, which wins.
	lakeDefaultMCPRunnerImage = "oramasearch/lake-mcp-runner:d98451a"
)

// lakeMCPRunnerImageEnv is the variable lake reads the MCP runner image from.
const lakeMCPRunnerImageEnv = "LAKE_MCP_RUNNER_IMAGE"

// lakeDefaultLibrary is the Agent Library every Lake runs: all nine specialized agents. A
// spec.agents.library entry replaces the image of its key; it cannot remove an agent.
var lakeDefaultLibrary = []oramav1alpha1.LakeLibraryAgent{
	{Key: "agent-ask-orama", Image: "oramasearch/agent-ask-orama:6baa70d"},
	{Key: "agent-project", Image: "oramasearch/agent-project:16e17d0"},
	{Key: "agent-autosuggest", Image: "oramasearch/agent-autosuggest:3d9acf7"},
	{Key: "agent-creator", Image: "oramasearch/agent-creator:6b1cc36-f8feb14"},
	{Key: "agent-eval-generator", Image: "oramasearch/agent-eval-generator:e2a683c"},
	{Key: "agent-project-corpus", Image: "oramasearch/agent-project-corpus:4537bcb"},
	{Key: "agent-autosuggest-corpus", Image: "oramasearch/agent-autosuggest-corpus:a882954"},
	{Key: "agent-application", Image: "oramasearch/agent-application:3a38da8"},
	{Key: "agent-experiments", Image: "oramasearch/agent-experiments:62a4d64"},
}

// lakeDefaultZettaEnv is added to every Zetta a Lake owns (its data plane, its workspaces and its
// managed Zetta services) unless the spec's env for that Zetta names the variable: glibc returns
// freed memory above 64 MiB and keeps two arenas, so a Zetta stays inside its memory limit.
var lakeDefaultZettaEnv = []corev1.EnvVar{
	{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "67108864"},
	{Name: "MALLOC_ARENA_MAX", Value: "2"},
}

// lakeDataPlaneZettaImage is the image of the Lake's own data-plane Zetta.
func lakeDataPlaneZettaImage(lake *oramav1alpha1.Lake) string {
	return orDefault(lake.Spec.Zetta.Image, lakeDefaultZettaImage)
}

// serviceZettaImage is the image of every managed Zetta service and every workspace Zetta:
// spec.services.zettaImage, then spec.workspaces.image, then the default.
func serviceZettaImage(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Services.ZettaImage != "" {
		return lake.Spec.Services.ZettaImage
	}
	return orDefault(lake.Spec.Workspaces.Image, lakeDefaultZettaImage)
}

// withLakeDefaultZettaEnv is spec env followed by every default it does not name.
func withLakeDefaultZettaEnv(spec []corev1.EnvVar) []corev1.EnvVar {
	env := append([]corev1.EnvVar(nil), spec...)
	named := make(map[string]struct{}, len(spec))
	for _, variable := range spec {
		named[variable.Name] = struct{}{}
	}
	for _, variable := range lakeDefaultZettaEnv {
		if _, found := named[variable.Name]; !found {
			env = append(env, variable)
		}
	}
	return env
}

// lakeDataPlaneZettaEnv is the env of the Lake's own data-plane Zetta.
func lakeDataPlaneZettaEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	return withLakeDefaultZettaEnv(lake.Spec.Zetta.Env)
}

// serviceZettaEnv is the env of every managed Zetta service and every workspace Zetta.
func serviceZettaEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	return withLakeDefaultZettaEnv(lake.Spec.Services.ZettaEnv)
}

// lakeAppsRuntimeImage is the image runtime-mode apps run.
func lakeAppsRuntimeImage(lake *oramav1alpha1.Lake) string {
	return orDefault(lake.Spec.Apps.RuntimeImage, lakeDefaultAppsRuntimeImage)
}

// lakeAppsWildcard is the DNS suffix an app without its own host is named under, without a
// leading "*.": spec.apps.wildcard, else the Lake's own host, so an app is <app>.<lake host>.
// Empty only when the Lake has neither.
func lakeAppsWildcard(lake *oramav1alpha1.Lake) string {
	return strings.TrimPrefix(orDefault(lake.Spec.Apps.Wildcard, lake.Spec.Host), "*.")
}

// lakeBuilderImage is the image every build Job runs.
func lakeBuilderImage(lake *oramav1alpha1.Lake) string {
	return orDefault(lake.Spec.Builds.BuilderImage, lakeDefaultBuilderImage)
}

// lakeMethodologyRuntimeImage is the methodology runtime image.
func lakeMethodologyRuntimeImage(lake *oramav1alpha1.Lake) string {
	return orDefault(lake.Spec.Methodologies.RuntimeImage, lakeDefaultMethodologyRuntimeImage)
}

// lakeAgentRuntimeImage is the image user-created agents run on.
func lakeAgentRuntimeImage(lake *oramav1alpha1.Lake) string {
	return orDefault(lake.Spec.Agents.RuntimeImage, lakeDefaultAgentRuntimeImage)
}

// lakeAgentLibrary is every library agent the Lake runs: the nine defaults, each image replaced by
// a spec.agents.library entry of the same key, in the default order.
func lakeAgentLibrary(lake *oramav1alpha1.Lake) []oramav1alpha1.LakeLibraryAgent {
	images := make(map[string]string, len(lake.Spec.Agents.Library))
	for _, agent := range lake.Spec.Agents.Library {
		if agent.Image != "" {
			images[agent.Key] = agent.Image
		}
	}
	library := make([]oramav1alpha1.LakeLibraryAgent, 0, len(lakeDefaultLibrary))
	for _, agent := range lakeDefaultLibrary {
		library = append(library, oramav1alpha1.LakeLibraryAgent{Key: agent.Key, Image: orDefault(images[agent.Key], agent.Image)})
	}
	return library
}

// lakeMCPRunnerEnv is LAKE_MCP_RUNNER_IMAGE: spec.env's entry when it sets one (fromSpec), else
// the default.
func lakeMCPRunnerEnv(lake *oramav1alpha1.Lake) (variable corev1.EnvVar, fromSpec bool) {
	for i := range lake.Spec.Env {
		if lake.Spec.Env[i].Name == lakeMCPRunnerImageEnv {
			return *lake.Spec.Env[i].DeepCopy(), true
		}
	}
	return corev1.EnvVar{Name: lakeMCPRunnerImageEnv, Value: lakeDefaultMCPRunnerImage}, false
}

// lakeFeaturesNeedingPostgres are the features that read lake's control Postgres. A Lake with
// spec.postgres.enabled false cannot run them, and says so in its FeaturesReady condition.
var lakeFeaturesNeedingPostgres = []string{
	"workspaces", "services", "stores", "apps", "builds", "methodology runs", "methodology deployments",
	"agent launches", "agent schedules", "volume snapshots", "connections", "egress rules", "tunnels", "peers",
}

// lakeFeaturesCondition is FeaturesReady: True when the Lake can run every feature, False naming
// the prerequisite that is missing and the features it stops. A feature is never skipped silently.
func lakeFeaturesCondition(lake *oramav1alpha1.Lake) metav1.Condition {
	condition := metav1.Condition{
		Type:               "FeaturesReady",
		Status:             metav1.ConditionTrue,
		Reason:             "AllFeatures",
		Message:            "every Lake feature is reconciled",
		ObservedGeneration: lake.Generation,
	}
	if !lakePostgresEnabled(lake) {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "PostgresDisabled"
		condition.Message = "spec.postgres.enabled is false, so these features cannot run: " +
			strings.Join(lakeFeaturesNeedingPostgres, ", ")
	}
	return condition
}
