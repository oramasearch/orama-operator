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
// The defaults are pinned to what the managing cluster's Lake/prod ran on 2026-10-17, apart from
// Zetta, which is the v0.3.0 release pinned by digest (operator#588). Moving a version is a change
// to this table, rolled through the pipeline dev, testing, staging, prod.
const (
	// lakeDefaultZettaImage is the Zetta of the Lake's own data plane, of every workspace and of
	// every managed Zetta service.
	lakeDefaultZettaImage = "oramasearch/zetta:v0.3.0@sha256:0dad6a7fba1842a1406a3430394fd98a06d3241d9989a69470f6ad921a54328a"
	// The images that run tenant code (apps, builds, methodologies, user-created agents) are pinned
	// by digest, never a tag alone, so a re-pushed tag cannot change what every tenant runs
	// (orama-operator#765). The tag is kept for readers; the digest is what is pulled.
	//
	// lakeDefaultAppsRuntimeImage runs runtime-mode apps (spec.apps.runtimeImage).
	lakeDefaultAppsRuntimeImage = "oramasearch/app-runtime:976e6a8@sha256:ee7222fbbccdcd9dc4ec88c7ee6856b81aa876366a4abeae2bc6d13fda2146b3"
	// lakeDefaultBuilderImage runs every image build (spec.builds.builderImage).
	lakeDefaultBuilderImage = "oramasearch/lake-builder:31a48d9@sha256:35cc44ca93a191a2832d0e03e4c7c83946eb44ae917215cd7d52b8711f9d09c0"
	// lakeDefaultMethodologyRuntimeImage is the base of every methodology image and runs its stage
	// and collect pods (spec.methodologies.runtimeImage).
	lakeDefaultMethodologyRuntimeImage = "oramasearch/methodology-runtime:ac58062@sha256:ee64b81a97a46917cfb278e3db3bf4692794ff3dba47ff9e05c3e436af97d1ed"
	// lakeDefaultAgentRuntimeImage runs every user-created agent (spec.agents.runtimeImage).
	lakeDefaultAgentRuntimeImage = "oramasearch/agent-runtime:v0.1.0@sha256:f14cbc7b7f82d3a9bb54635011d9b5daf019cd89c41e38dce7b00185e297e73f"
	// lakeDefaultMCPRunnerImage is LAKE_MCP_RUNNER_IMAGE, the image lake runs MCP servers on.
	// spec.env may still set the variable, which wins. Lake runs a stdio MCP connection as an image
	// App on it, so it is pinned by digest: lake refuses a stdio connection otherwise, and
	// lakeAppRowSource and the LakeApp CRD fail an image App that is not (operator#744). The digest
	// is the one the d98451a tag names in Docker Hub.
	lakeDefaultMCPRunnerImage = "oramasearch/lake-mcp-runner:d98451a@sha256:b5439ce37f45d1a853ad5cd2b35dca7b6e571075816a41e287b2a42693d77892"
)

// lakeMCPRunnerImageEnv is the variable lake reads the MCP runner image from.
const lakeMCPRunnerImageEnv = "LAKE_MCP_RUNNER_IMAGE"

// lakeDefaultLibrary is the Agent Library every Lake runs: all nine specialized agents. A
// spec.agents.library entry replaces the image of its key; it cannot remove an agent.
var lakeDefaultLibrary = []oramav1alpha1.LakeLibraryAgent{
	{Key: "agent-ask-orama", Image: "oramasearch/agent-ask-orama:v0.1.0"},
	{Key: "agent-project", Image: "oramasearch/agent-project:v0.1.0"},
	{Key: "agent-autosuggest", Image: "oramasearch/agent-autosuggest:v0.1.0"},
	{Key: "agent-creator", Image: "oramasearch/agent-creator:v0.1.0"},
	{Key: "agent-eval-generator", Image: "oramasearch/agent-eval-generator:v0.1.0"},
	{Key: "agent-project-corpus", Image: "oramasearch/agent-project-corpus:v0.1.0"},
	{Key: "agent-autosuggest-corpus", Image: "oramasearch/agent-autosuggest-corpus:v0.1.0"},
	{Key: "agent-application", Image: "oramasearch/agent-application:v0.1.0"},
	{Key: "agent-experiments", Image: "oramasearch/agent-experiments:v0.1.0"},
}

// lakeDefaultZettaEnv is added to every Zetta a Lake owns (its data plane, its workspaces and its
// managed Zetta services) unless the spec's env for that Zetta names the variable: glibc returns
// freed memory above 64 MiB and keeps two arenas, so a Zetta stays inside its memory limit.
//
// ZETTA_EGRESS_ALLOW follows them (zetta#75, operator#724): rendered for each Zetta from what its
// namespace's NetworkSandbox allows it (lake_zetta_egress.go), unless the spec's env names it.
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

// lakeDataPlaneZettaEnv is the env of the Lake's own data-plane Zetta. Its egress allowlist is
// empty (loopback only): the Lake namespace's NetworkSandbox lets its pods reach cluster DNS and
// the other nodes of the same cluster and nothing else (reconcileLakeNetworkSandbox), and neither
// goes through the engine's HTTP client.
func lakeDataPlaneZettaEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	return withZettaEgressAllow(withLakeDefaultZettaEnv(lake.Spec.Zetta.Env), zettaEgressDeny)
}

// serviceZettaEnv is the env of every managed Zetta service and every workspace Zetta, with the
// egress allowlist rendered from its workspace namespace (workspaceZettaEgressAllow) and, last, the
// connector destinations approved from the Lake services in that namespace
// (workspaceZettaConnectorAllow, lake_zetta_connector_destinations.go), which the spec cannot set.
func serviceZettaEnv(lake *oramav1alpha1.Lake, egressAllow, connectorAllow string) []corev1.EnvVar {
	return withZettaConnectorAllow(
		withZettaEgressAllow(withLakeDefaultZettaEnv(lake.Spec.Services.ZettaEnv), egressAllow), connectorAllow)
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
	switch {
	case !lakePostgresEnabled(lake):
		condition.Status = metav1.ConditionFalse
		condition.Reason = "PostgresDisabled"
		condition.Message = "spec.postgres.enabled is false, so these features cannot run: " +
			strings.Join(lakeFeaturesNeedingPostgres, ", ")
	case !lakeZettaEnabled(lake):
		// Deployments read and write orama_control on the Lake's own Zetta (operator#557).
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ZettaDisabled"
		condition.Message = "spec.zetta.enabled is false, so deployments cannot run: they use the Lake's Zetta as their control plane"
	case !lakeHasPublicHost(lake):
		condition.Status = metav1.ConditionFalse
		condition.Reason = "NoPublicHost"
		condition.Message = "the Lake has neither spec.host nor LAKE_PUBLIC_HOST in spec.env, so deployments cannot run: " +
			"lake names itself on every deployment row with that host"
	}
	return condition
}
