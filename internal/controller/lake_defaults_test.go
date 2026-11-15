/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The images Lake/prod ran on 2026-10-17 (operator#539). The test repeats them on purpose: moving
// a default is a deliberate change to both lake_defaults.go and this table.
var wantLakeDefaultLibrary = map[string]string{
	"agent-ask-orama":          "oramasearch/agent-ask-orama:v0.1.0",
	"agent-project":            "oramasearch/agent-project:v0.1.0",
	"agent-autosuggest":        "oramasearch/agent-autosuggest:v0.1.0",
	"agent-creator":            "oramasearch/agent-creator:v0.1.0",
	"agent-eval-generator":     "oramasearch/agent-eval-generator:v0.1.0",
	"agent-project-corpus":     "oramasearch/agent-project-corpus:v0.1.0",
	"agent-autosuggest-corpus": "oramasearch/agent-autosuggest-corpus:v0.1.0",
	"agent-application":        "oramasearch/agent-application:v0.1.0",
	"agent-experiments":        "oramasearch/agent-experiments:v0.1.0",
}

// bareLake states only what differs per environment: no feature field at all.
func bareLake() *oramav1alpha1.Lake {
	return &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "dev"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake",
			Image:     "oramasearch/lake:test",
			Host:      "lake-dev.oramadev.net",
		},
	}
}

func lakeEnvMap(lake *oramav1alpha1.Lake) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		env[variable.Name] = variable
	}
	return env
}

func assertLakeRunsEveryFeature(t *testing.T, lake *oramav1alpha1.Lake) {
	t.Helper()
	for name, enabled := range map[string]bool{
		"workspaces":   lakeWorkspacesEnabled(lake),
		"services":     lakeServicesEnabled(lake),
		"apps":         lakeAppsEnabled(lake),
		"builds":       lakeBuildsEnabled(lake),
		"snapshots":    lakeSnapshotsEnabled(lake),
		"control rows": lakeControlRowsEnabled(lake),
	} {
		if !enabled {
			t.Errorf("%s are not reconciled", name)
		}
	}
	if condition := lakeFeaturesCondition(lake); condition.Status != metav1.ConditionTrue {
		t.Errorf("FeaturesReady = %+v", condition)
	}
}

func TestALakeWithNoFeatureFieldsRunsEveryFeatureOnTheDefaultImages(t *testing.T) {
	lake := bareLake()
	assertLakeRunsEveryFeature(t, lake)

	env := lakeEnvMap(lake)
	for name, want := range map[string]string{
		"LAKE_APPS_RUNTIME_IMAGE":        "oramasearch/app-runtime:976e6a8",
		"LAKE_METHODOLOGY_RUNTIME_IMAGE": "oramasearch/methodology-runtime:ac58062",
		"LAKE_MCP_RUNNER_IMAGE":          "oramasearch/lake-mcp-runner:d98451a@sha256:b5439ce37f45d1a853ad5cd2b35dca7b6e571075816a41e287b2a42693d77892",
		"LAKE_APPS_WILDCARD":             "lake-dev.oramadev.net",
		"LAKE_AGENT_MODEL":               "gpt-6.1-sol",
		"LAKE_SERVICE_DIR":               lakeServicesMountPath,
		"LAKE_AGENT_BUS_DIR":             lakeAgentBusMountPath,
	} {
		if got := env[name].Value; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	endpoints := strings.Split(env["LAKE_AGENT_ENDPOINTS"].Value, ",")
	if len(endpoints) != len(wantLakeDefaultLibrary) {
		t.Fatalf("LAKE_AGENT_ENDPOINTS names %d agents: %v", len(endpoints), endpoints)
	}

	library := map[string]string{}
	for _, agent := range lakeAgentLibrary(lake) {
		library[agent.Key] = agent.Image
	}
	if !reflect.DeepEqual(library, wantLakeDefaultLibrary) {
		t.Fatalf("library = %v", library)
	}
	for _, image := range [][2]string{
		{lakeAgentRuntimeImage(lake), "oramasearch/agent-runtime:v0.1.0"},
		{lakeBuilderImage(lake), "oramasearch/lake-builder:31a48d9"},
		{serviceZettaImage(lake), "oramasearch/zetta:v0.3.0@sha256:0dad6a7fba1842a1406a3430394fd98a06d3241d9989a69470f6ad921a54328a"},
		{lakeDataPlaneZettaImage(lake), "oramasearch/zetta:v0.3.0@sha256:0dad6a7fba1842a1406a3430394fd98a06d3241d9989a69470f6ad921a54328a"},
		{lakeAppsRuntimeImage(lake), "oramasearch/app-runtime:976e6a8"},
		{lakeMethodologyRuntimeImage(lake), "oramasearch/methodology-runtime:ac58062"},
	} {
		if image[0] != image[1] {
			t.Errorf("image %q, want %q", image[0], image[1])
		}
	}
	wantZettaEnv := []corev1.EnvVar{
		{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "67108864"},
		{Name: "MALLOC_ARENA_MAX", Value: "2"},
		{Name: "ZETTA_EGRESS_ALLOW", Value: "api.example.com:443"},
		{Name: "ZETTA_CONNECTOR_ALLOW_DESTINATIONS", Value: "db-rw.ws.svc.cluster.local:5432"},
	}
	if got := serviceZettaEnv(lake, "api.example.com:443", "db-rw.ws.svc.cluster.local:5432"); !reflect.DeepEqual(got, wantZettaEnv) {
		t.Errorf("service and workspace Zetta env = %v", got)
	}
	// The data plane is no workspace Zetta: no connector sources live in the Lake namespace.
	wantZettaEnv = wantZettaEnv[:3]
	wantZettaEnv[2].Value = ""
	if got := lakeDataPlaneZettaEnv(lake); !reflect.DeepEqual(got, wantZettaEnv) {
		t.Errorf("data-plane Zetta env = %v", got)
	}
}

func TestASpecImageOverridesItsDefault(t *testing.T) {
	lake := bareLake()
	lake.Spec.Apps = oramav1alpha1.LakeAppsSpec{RuntimeImage: "oramasearch/app-runtime:mine", Wildcard: "*.apps.example.test"}
	lake.Spec.Builds.BuilderImage = "oramasearch/lake-builder:mine"
	lake.Spec.Methodologies.RuntimeImage = "oramasearch/methodology-runtime:mine"
	lake.Spec.Agents.RuntimeImage = "oramasearch/agent-runtime:mine"
	lake.Spec.Agents.Library = []oramav1alpha1.LakeLibraryAgent{{Key: "agent-project", Image: "oramasearch/agent-project:mine"}}
	lake.Spec.Workspaces.Image = "oramasearch/zetta:workspace"
	lake.Spec.Zetta.Image = "oramasearch/zetta:plane"
	lake.Spec.Zetta.Env = []corev1.EnvVar{{Name: "MALLOC_ARENA_MAX", Value: "4"}}
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_MCP_RUNNER_IMAGE", Value: "oramasearch/lake-mcp-runner:mine"}}

	env := lakeEnvMap(lake)
	for name, want := range map[string]string{
		"LAKE_APPS_RUNTIME_IMAGE":        "oramasearch/app-runtime:mine",
		"LAKE_METHODOLOGY_RUNTIME_IMAGE": "oramasearch/methodology-runtime:mine",
		"LAKE_MCP_RUNNER_IMAGE":          "oramasearch/lake-mcp-runner:mine",
		"LAKE_APPS_WILDCARD":             "apps.example.test",
	} {
		if got := env[name].Value; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	runners := 0
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		if variable.Name == "LAKE_MCP_RUNNER_IMAGE" {
			runners++
		}
	}
	if runners != 1 {
		t.Fatalf("LAKE_MCP_RUNNER_IMAGE appears %d times", runners)
	}
	if lakeBuilderImage(lake) != "oramasearch/lake-builder:mine" || lakeAgentRuntimeImage(lake) != "oramasearch/agent-runtime:mine" ||
		serviceZettaImage(lake) != "oramasearch/zetta:workspace" || lakeDataPlaneZettaImage(lake) != "oramasearch/zetta:plane" {
		t.Fatal("a spec image did not override its default")
	}
	lake.Spec.Services.ZettaImage = "oramasearch/zetta:service"
	if serviceZettaImage(lake) != "oramasearch/zetta:service" {
		t.Fatal("spec.services.zettaImage does not come first")
	}
	library := lakeAgentLibrary(lake)
	if len(library) != len(wantLakeDefaultLibrary) {
		t.Fatalf("an override removed agents: %v", library)
	}
	for _, agent := range library {
		want := wantLakeDefaultLibrary[agent.Key]
		if agent.Key == "agent-project" {
			want = "oramasearch/agent-project:mine"
		}
		if agent.Image != want {
			t.Errorf("%s runs %s, want %s", agent.Key, agent.Image, want)
		}
	}
	if got := lakeDataPlaneZettaEnv(lake); !reflect.DeepEqual(got, []corev1.EnvVar{
		{Name: "MALLOC_ARENA_MAX", Value: "4"},
		{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "67108864"},
		{Name: "ZETTA_EGRESS_ALLOW", Value: ""},
	}) {
		t.Fatalf("a spec Zetta variable did not override its default: %v", got)
	}
}

// Lake/prod states every image and both memory variables; the defaults change none of them and add
// only ZETTA_EGRESS_ALLOW, the engine's own egress allowlist (zetta#75, operator#724), and, on a
// service or workspace Zetta, ZETTA_CONNECTOR_ALLOW_DESTINATIONS (operator#882).
func TestLakeProdIsUnchangedByTheDefaults(t *testing.T) {
	lake := bareLake()
	zettaEnv := []corev1.EnvVar{{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "67108864"}, {Name: "MALLOC_ARENA_MAX", Value: "2"}}
	lake.Spec.Zetta.Env = zettaEnv
	lake.Spec.Services.ZettaEnv = zettaEnv
	want := append(append([]corev1.EnvVar(nil), zettaEnv...), corev1.EnvVar{Name: "ZETTA_EGRESS_ALLOW", Value: ""})
	wantService := append(append([]corev1.EnvVar(nil), want...), corev1.EnvVar{Name: "ZETTA_CONNECTOR_ALLOW_DESTINATIONS", Value: ""})
	if !reflect.DeepEqual(lakeDataPlaneZettaEnv(lake), want) || !reflect.DeepEqual(serviceZettaEnv(lake, "", ""), wantService) {
		t.Fatalf("stated Zetta env gained more than ZETTA_EGRESS_ALLOW: %v", lakeDataPlaneZettaEnv(lake))
	}
}

// Every Zetta a Lake owns carries its own egress allowlist (zetta#75, operator#724): the data plane
// denies everything (its namespace allows it nothing outside), a service or workspace Zetta gets
// the list rendered from its namespace, and a spec that names the variable keeps its own.
func TestLakeZettasCarryTheirEgressAllowlist(t *testing.T) {
	lake := bareLake()
	for name, check := range map[string]struct {
		env  []corev1.EnvVar
		want string
	}{
		"data plane":         {lakeDataPlaneZettaEnv(lake), ""},
		"service, workspace": {serviceZettaEnv(lake, "*:443", ""), "*:443"},
	} {
		if got := findEnv(check.env, "ZETTA_EGRESS_ALLOW"); got == nil || got.Value != check.want {
			t.Errorf("%s Zetta ZETTA_EGRESS_ALLOW = %+v, want %q", name, got, check.want)
		}
	}
	for _, variable := range serviceZettaEnv(lake, "", "") {
		if variable.Value == "*" {
			t.Fatalf("a Lake Zetta still turns its egress control off: %+v", variable)
		}
	}
	lake.Spec.Services.ZettaEnv = []corev1.EnvVar{{Name: "ZETTA_EGRESS_ALLOW", Value: "*.snowflakecomputing.com:443"}}
	env := serviceZettaEnv(lake, "*:443", "")
	count := 0
	for _, variable := range env {
		if variable.Name == "ZETTA_EGRESS_ALLOW" {
			count++
			if variable.Value != "*.snowflakecomputing.com:443" {
				t.Errorf("a stated allowlist was replaced: %q", variable.Value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("ZETTA_EGRESS_ALLOW appears %d times", count)
	}
}

func TestNothingCanTurnALakeFeatureOff(t *testing.T) {
	// What a Lake stored before operator#539: the switches off. The CRD prunes them; decoding
	// without them leaves every feature on.
	var spec oramav1alpha1.LakeSpec
	if err := json.Unmarshal([]byte(`{"image":"oramasearch/lake:test","host":"lake-dev.oramadev.net",
		"workspaces":{"enabled":false},"services":{"enabled":false},"apps":{"enabled":false},
		"builds":{"enabled":false},"snapshots":{"enabled":false}}`), &spec); err != nil {
		t.Fatal(err)
	}
	assertLakeRunsEveryFeature(t, &oramav1alpha1.Lake{Spec: spec})
	for _, kind := range []any{
		oramav1alpha1.LakeWorkspacesSpec{}, oramav1alpha1.LakeServicesSpec{}, oramav1alpha1.LakeAppsSpec{},
		oramav1alpha1.LakeBuildsSpec{}, oramav1alpha1.LakeSnapshotsSpec{}, oramav1alpha1.LakeAgentsSpec{},
		oramav1alpha1.LakeMethodologiesSpec{},
	} {
		if _, found := reflect.TypeOf(kind).FieldByName("Enabled"); found {
			t.Errorf("%T has an Enabled switch", kind)
		}
	}
}

// Postgres is the one prerequisite. Without it the features cannot run, and the Lake says so.
func TestALakeWithoutPostgresSaysWhichFeaturesCannotRun(t *testing.T) {
	lake := bareLake()
	lake.Spec.Postgres.Enabled = boolPtr(false)
	condition := lakeFeaturesCondition(lake)
	if condition.Status != metav1.ConditionFalse || condition.Reason != "PostgresDisabled" ||
		!strings.Contains(condition.Message, "workspaces") || !strings.Contains(condition.Message, "builds") {
		t.Fatalf("FeaturesReady = %+v", condition)
	}
}

func TestTheAppsWildcardDefaultsToTheLakeHost(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeAppScheme(t)
	lake := bareLake()
	lake.UID = "lake-uid"
	lake.Spec.DNSSecretRef = &corev1.LocalObjectReference{Name: "cloudflare"}
	if got := lakeAppsWildcard(lake); got != "lake-dev.oramadev.net" {
		t.Fatalf("wildcard = %q", got)
	}
	app := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "myapp"}}
	if got := lakeAppHost(lake, app); got != "myapp.lake-dev.oramadev.net" {
		t.Fatalf("app host = %q", got)
	}
	class := "nginx"
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-dev", Namespace: "lake"},
		Spec:       networkingv1.IngressSpec{IngressClassName: &class},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.20"}},
		}},
	}
	if err := controllerutil.SetControllerReference(lake, ingress, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, ingress).WithStatusSubresource(ingress).Build()
	if err := (&LakeAppsReconciler{Client: c, Scheme: scheme}).reconcileLakeAppsDNS(ctx, lake); err != nil {
		t.Fatal(err)
	}
	var records oramav1alpha1.CloudflareDNSRecordList
	if err := c.List(ctx, &records, client.InNamespace("lake")); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 1 || records.Items[0].Spec.Name != "*.lake-dev.oramadev.net" {
		t.Fatalf("apps DNS records: %+v", records.Items)
	}
}

// A LakeSet member states only host, args and env. The Lake the set applies still runs every
// feature on the operator's defaults.
func TestALakeSetMemberGetsEveryFeatureAndEveryDefault(t *testing.T) {
	set := productionLakeSet(
		oramav1alpha1.SetMember{ClusterRef: "orama-production-cluster-02", Name: "shared",
			Spec: rawJSON(`{"host":"lake-eu.oramadev.net","args":["--node","shared"],` +
				`"env":[{"name":"LAKE_PUBLIC_HOST","value":"lake-eu.oramadev.net"}]}`)},
	)
	members := &fakeMembers{ready: map[string]bool{}}
	c := setTestClient(t, append(productionClusters(), set)...)
	if _, err := reconcileLakeSet(t, c, members, "lake-prod"); err != nil {
		t.Fatal(err)
	}
	if len(members.applied) != 1 {
		t.Fatalf("applied %d members", len(members.applied))
	}
	raw, err := json.Marshal(members.applied[0].Spec)
	if err != nil {
		t.Fatal(err)
	}
	var spec oramav1alpha1.LakeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "shared"}, Spec: spec}
	assertLakeRunsEveryFeature(t, lake)
	env := lakeEnvMap(lake)
	if env["LAKE_APPS_WILDCARD"].Value != "lake-eu.oramadev.net" ||
		env["LAKE_APPS_RUNTIME_IMAGE"].Value != lakeDefaultAppsRuntimeImage ||
		env["LAKE_MCP_RUNNER_IMAGE"].Value != lakeDefaultMCPRunnerImage ||
		len(lakeAgentLibrary(lake)) != len(wantLakeDefaultLibrary) {
		t.Fatalf("set member env: %+v", env)
	}
}

// lakeControlRowsRequeue is how soon a Lake with its control Postgres is looked at again: every
// Lake polls its control rows (operator#539), at the deployment-sync interval when that is sooner
// than the status requeue.
var lakeControlRowsRequeue = min(lakeStatusRequeue, defaultDeploymentSyncInterval)

func envNamesOf(env []corev1.EnvVar) []string {
	names := make([]string, 0, len(env))
	for _, variable := range env {
		names = append(names, variable.Name)
	}
	return names
}

// wantLakeOperatorEnv is the operator's own env on a Lake that states no feature field, in order:
// the same on every Lake, plus the control Postgres's variables when it has one.
// wantLakeOperatorEnv is the operator's variables in order. deployments is whether the Lake has a
// public host, which gives it the operator Zetta pair (operator#557).
func wantLakeOperatorEnv(postgres, deployments bool) []string {
	names := []string{
		"LAKE_ADMIN_TOKEN", "LAKE_STORE_ROOT", "LAKE_SEALING_PUBLIC_KEY", "LAKE_SEALING_KID", "ZETTA_EGRESS_ALLOW",
		"LAKE_PROXY_PORT", "LAKE_TRUSTED_PROXY_CIDRS", "LAKE_APPS_WILDCARD", "LAKE_APPS_RUNTIME_IMAGE", "LAKE_METHODOLOGY_RUNTIME_IMAGE", "LAKE_MCP_RUNNER_IMAGE",
	}
	if postgres {
		names = append(names, "LAKE_INSTANCE_KEK", "LAKE_CONTROL_DB_URI", "LAKE_CONTROL_DB_PASSWORD")
	}
	names = append(names, "LAKE_DATA_PLANE_URL", "LAKE_DATA_PLANE_TOKEN")
	if deployments {
		names = append(names, "LAKE_OPERATOR_ZETTA_URL", "LAKE_OPERATOR_ZETTA_TOKEN")
	}
	if postgres {
		names = append(names, "LAKE_SERVICE_DIR")
	}
	return append(names, "LAKE_AGENT_ENDPOINTS", "LAKE_INTERNAL_URL", "LAKE_AGENT_MODEL",
		"LAKE_AGENT_SERVICE_TOKEN", "LAKE_AGENT_BUS_DIR")
}

// The MCP runner image becomes an image App's image: lake creates every stdio MCP connection as
// an App with mode image on it. lake refuses a stdio connection when it is not pinned by digest
// (lake#783), and the operator fails such an App in lakeAppRowSource and the LakeApp CRD pattern,
// so a tagged default means no stdio MCP server can start (operator#744).
func TestTheDefaultMCPRunnerImageIsPinnedByDigestSoItsAppsCanRun(t *testing.T) {
	runner, fromSpec := lakeMCPRunnerEnv(bareLake())
	if fromSpec {
		t.Fatal("a bare Lake's runner image came from its spec")
	}
	row := &LakeAppRow{Mode: oramav1alpha1.LakeAppModeImage, Image: runner.Value}
	if err := lakeAppRowSource(row, &oramav1alpha1.LakeAppSpec{}); err != nil {
		t.Errorf("an App on the default runner image %q fails: %v", runner.Value, err)
	}
	// The LakeApp CRD's spec.image pattern (api/v1alpha1/lakeapp_types.go).
	if !regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`).MatchString(runner.Value) {
		t.Errorf("the default runner image %q does not match the LakeApp image pattern", runner.Value)
	}
}
